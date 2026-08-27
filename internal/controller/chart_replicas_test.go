package controller

import (
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// replicaArgs is a complete, valid replicated configuration. Each guard test removes exactly one
// thing from it, so a failure names the thing that was removed rather than the first thing missing.
var replicaArgs = []string{
	"--set", "registry.publish.mode=internalOnly",
	"--set", "registry.readReplicas=2",
	"--set", "registry.persistence.accessMode=ReadWriteMany",
	"--set", "registry.cache.driver=redis",
	"--set", "registry.cache.redis.url=redis://redis:6379",
}

func withoutReplicaArg(t *testing.T, key string) []string {
	t.Helper()
	out := make([]string, 0, len(replicaArgs))
	for i := 0; i < len(replicaArgs); i += 2 {
		if strings.HasPrefix(replicaArgs[i+1], key+"=") {
			continue
		}
		out = append(out, replicaArgs[i], replicaArgs[i+1])
	}
	return out
}

// docs parses a helm render into one map per document.
//
// A parse failure fails the test rather than skipping the document, which is what it did before.
// Every assertion here reads "the document with property X also has Y", so an unparseable document
// vanishes from the search and the test passes having examined nothing.
//
// Narrow in practice -- helm rejects a syntax error before this sees it -- but "found nothing,
// therefore fine" is the failure mode worth removing.
//
// A chunk that parses to nothing (comments, trailing whitespace) is still skipped: that is absence
// of content, not failure to read it.
func docs(t *testing.T, out string) []map[string]any {
	t.Helper()
	var all []map[string]any
	for _, doc := range splitDocs(out) {
		var d map[string]any
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
			t.Fatalf("%s does not parse: %v", describeDoc(doc), err)
		}
		if d == nil {
			continue
		}
		all = append(all, d)
	}
	return all
}

// TestOnlyOneRegistryPodEverWrites is the invariant the whole design rests on.
//
// zot serialises repository writes with an in-process mutex, so two instances writing one repository
// lose tags that returned 201 — measured at 2–4% in test/spike. The chart's job is to make a second
// writer unreachable: the writer StatefulSet is pinned to one replica, and the Service the
// controllers push to selects it alone and never a read replica.
func TestOnlyOneRegistryPodEverWrites(t *testing.T) {
	out := render(t, replicaArgs[2:]...)

	for _, d := range docs(t, out) {
		if d["kind"] != "StatefulSet" {
			continue
		}
		var sts appsv1.StatefulSet
		b, _ := json.Marshal(d)
		if err := yaml.Unmarshal(b, &sts); err != nil {
			t.Fatalf("parsing the StatefulSet: %v", err)
		}
		if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 1 {
			t.Errorf("the writer must be exactly one pod, got %v — a second writer silently loses tags",
				sts.Spec.Replicas)
		}
	}

	// The write Service must not reach a read replica.
	var found bool
	for _, d := range docs(t, out) {
		if d["kind"] != "Service" {
			continue
		}
		var svc corev1.Service
		b, _ := json.Marshal(d)
		if err := yaml.Unmarshal(b, &svc); err != nil {
			continue
		}
		if !strings.HasSuffix(svc.Name, "-registry") {
			continue
		}
		found = true
		if svc.Spec.Selector["app.kubernetes.io/component"] != "registry" {
			t.Errorf("the write Service selects %v; it must select the writer alone", svc.Spec.Selector)
		}
		if _, ok := svc.Spec.Selector["oci-composer.lhns.de/registry-role"]; ok {
			t.Error("the write Service selects the serve role, so pushes could reach a read replica")
		}
		if svc.Spec.Type != corev1.ServiceTypeClusterIP {
			t.Errorf("the write Service is %s; nothing outside the cluster should be able to push", svc.Spec.Type)
		}
	}
	if !found {
		t.Fatal("no write Service rendered")
	}
}

// TestReadReplicasNeverCollect — garbage collection rewrites index.json, so a replica that collects
// is a second writer wearing a different name. The refresh probe in test/spike lost content to
// exactly this, while it was being actively pulled.
func TestReadReplicasNeverCollect(t *testing.T) {
	var writer, reader map[string]any
	for name, cfg := range registryConfigs(t, replicaArgs[2:]...) {
		storage, _ := cfg["storage"].(map[string]any)
		switch {
		case strings.HasSuffix(name, "-registry-reader"):
			reader = storage
		case strings.HasSuffix(name, "-registry"):
			writer = storage
		}
	}

	if writer == nil {
		t.Fatal("no writer config rendered")
	}
	if reader == nil {
		t.Fatal("no reader config rendered at readReplicas=2")
	}
	if writer["gc"] != true {
		t.Error("the writer must collect; nothing else will, and the store would grow without bound")
	}
	if _, ok := writer["retention"]; !ok {
		t.Error("the writer carries the retention policy")
	}
	if reader["gc"] != false {
		t.Errorf("a read replica has gc=%v; collecting makes it a second writer and it will lose content",
			reader["gc"])
	}
	if _, ok := reader["retention"]; ok {
		t.Error("a read replica must carry no retention policy")
	}
	// Both must reach the same metadata database, or a pull recorded by one does not protect
	// content from the other's collector.
	if reader["remoteCache"] != true || writer["remoteCache"] != true {
		t.Error("both roles must use the shared metadata database, or pulls do not count across pods")
	}
}

// TestPullsFanOutAndPushesDoNot — the read Service is what a workload, an Ingress or a NodePort
// reaches, and it must include every pod serving the API.
func TestPullsFanOutAndPushesDoNot(t *testing.T) {
	out := render(t, replicaArgs[2:]...)

	var readSvc *corev1.Service
	for _, d := range docs(t, out) {
		if d["kind"] != "Service" {
			continue
		}
		var svc corev1.Service
		b, _ := json.Marshal(d)
		if err := yaml.Unmarshal(b, &svc); err != nil {
			continue
		}
		if strings.HasSuffix(svc.Name, "-registry-read") {
			readSvc = &svc
		}
	}
	if readSvc == nil {
		t.Fatal("no read Service rendered")
	}
	if readSvc.Spec.Selector["oci-composer.lhns.de/registry-role"] != "serve" {
		t.Errorf("the read Service selects %v; it must select every pod serving the API", readSvc.Spec.Selector)
	}

	// Both the writer's pod template and the reader's must carry the serve label, or the Service
	// silently covers only one of them.
	var writerServes, readerServes bool
	for _, d := range docs(t, out) {
		kind, _ := d["kind"].(string)
		if kind != "StatefulSet" && kind != "Deployment" {
			continue
		}
		meta, _ := d["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		spec, _ := d["spec"].(map[string]any)
		tmpl, _ := spec["template"].(map[string]any)
		tmeta, _ := tmpl["metadata"].(map[string]any)
		labels, _ := tmeta["labels"].(map[string]any)
		if labels["oci-composer.lhns.de/registry-role"] != "serve" {
			continue
		}
		switch {
		case strings.HasSuffix(name, "-registry-reader"):
			readerServes = true
		case strings.HasSuffix(name, "-registry"):
			writerServes = true
		}
	}
	if !writerServes {
		t.Error("the writer is not in the read Service, so it serves no pulls")
	}
	if !readerServes {
		t.Error("the read replicas are not in the read Service, so they serve nothing at all")
	}
}

// TestTheWriterSelectorIsNeverTouched.
//
// A StatefulSet's spec.selector is immutable. Adding the serve label there instead of only to the
// pod template would make every existing install fail its upgrade with an API error naming a field
// rather than a problem.
func TestTheWriterSelectorIsNeverTouched(t *testing.T) {
	for _, args := range [][]string{{}, replicaArgs[2:]} {
		out := render(t, args...)
		for _, d := range docs(t, out) {
			if d["kind"] != "StatefulSet" {
				continue
			}
			var sts appsv1.StatefulSet
			b, _ := json.Marshal(d)
			if err := yaml.Unmarshal(b, &sts); err != nil {
				continue
			}
			if _, ok := sts.Spec.Selector.MatchLabels["oci-composer.lhns.de/registry-role"]; ok {
				t.Error("the serve label is in the StatefulSet selector, which is immutable; " +
					"every existing install would fail to upgrade")
			}
		}
	}
}

// TestNothingReplicatedRendersByDefault — the default is one pod, and a chart that quietly started
// running three would be changing the storage requirements of every existing install.
func TestNothingReplicatedRendersByDefault(t *testing.T) {
	out := render(t)
	for _, d := range docs(t, out) {
		meta, _ := d["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if strings.HasSuffix(name, "-registry-reader") {
			t.Errorf("%s %s rendered at readReplicas=0", d["kind"], name)
		}
		if d["kind"] == "PodDisruptionBudget" {
			t.Error("a PodDisruptionBudget rendered with a single pod; it would block every drain")
		}
		if d["kind"] == "PersistentVolumeClaim" && strings.HasSuffix(name, "-registry") {
			spec, _ := d["spec"].(map[string]any)
			modes, _ := spec["accessModes"].([]any)
			if len(modes) != 1 || modes[0] != "ReadWriteOnce" {
				t.Errorf("the default access mode changed to %v; that is immutable on a bound claim "+
					"and would break every upgrade", modes)
			}
		}
	}
}

// TestReplicationRefusesWhatItCannotDo. Each case removes one prerequisite from a valid set.
func TestReplicationRefusesWhatItCannotDo(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"no shared metadata database": {
			withoutReplicaArg(t, "registry.cache.driver"),
			"requires registry.cache.driver",
		},
		"a ReadWriteOnce volume": {
			withoutReplicaArg(t, "registry.persistence.accessMode"),
			"ReadWriteMany",
		},
		"an emptyDir": {
			append(append([]string{}, replicaArgs...), "--set", "registry.persistence.enabled=false"),
			"persistence.enabled=true",
		},
		"a negative count": {
			[]string{"--set", "registry.publish.mode=internalOnly", "--set", "registry.readReplicas=-1"},
			"smallest meaningful value is 0",
		},
		"the removed clustering value": {
			[]string{"--set", "registry.publish.mode=internalOnly", "--set", "registry.cluster.enabled=true"},
			"registry.cluster no longer exists",
		},
		"dynamodb without S3": {
			[]string{"--set", "registry.publish.mode=internalOnly", "--set", "registry.cache.driver=dynamodb"},
			"only supported with registry.storage.driver=s3",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out := renderRawExpectingFailure(t, tc.args...)
			if !strings.Contains(out, tc.want) {
				t.Fatalf("error does not mention %q:\n%s", tc.want, out)
			}
		})
	}
}

// TestAValidReplicatedSetRenders is the control. Without it, a guard that fired unconditionally
// would pass every case above while making the feature unusable.
func TestAValidReplicatedSetRenders(t *testing.T) {
	out := renderRaw(t, replicaArgs...)
	for _, want := range []string{"-registry-reader", "-registry-read", "kind: PodDisruptionBudget"} {
		if !strings.Contains(out, want) {
			t.Errorf("a valid replicated configuration did not render %s", want)
		}
	}
}

// TestACredentialedCacheURLNeverLandsInAConfigMap.
//
// zot's redis driver takes credentials only inside the URL, so `redis://user:pass@host` puts a
// password wherever the config is rendered. A ConfigMap is readable in every `kubectl describe`.
//
// This is a regression test for a claim that was false: the threat model asserted the config became
// a Secret when clustering was on, while the template rendered a ConfigMap unconditionally. Nothing
// checked it, so nothing caught it — and read replicas made redis mandatory rather than exotic.
func TestACredentialedCacheURLNeverLandsInAConfigMap(t *testing.T) {
	const password = "hunter2"
	out := render(t,
		"--set", "registry.cache.driver=redis",
		"--set", "registry.cache.redis.url=redis://user:"+password+"@redis:6379",
	)

	var sawSecret bool
	for _, d := range docs(t, out) {
		meta, _ := d["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		switch d["kind"] {
		case "ConfigMap":
			if strings.Contains(strings.ToLower(toJSON(t, d["data"])), password) {
				t.Errorf("ConfigMap %s carries the cache password; it is readable in every "+
					"kubectl describe", name)
			}
		case "Secret":
			if strings.Contains(toJSON(t, d["stringData"]), password) {
				sawSecret = true
			}
		}
	}
	if !sawSecret {
		t.Error("the config did not render as a Secret, so the credentialed URL went somewhere unchecked")
	}

	// And the pod must actually mount it, or the registry starts with no configuration at all.
	if !strings.Contains(out, "secretName: test-release-kube-oci-composer-registry\n") {
		t.Error("the registry does not mount the config Secret it was given")
	}
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	return string(b)
}

// TestAnOrdinaryInstallKeepsAnInspectableConfig — the Secret path is conditional on there being
// something to hide. Hiding zot's configuration unconditionally would cost every operator the
// ability to read it for no benefit.
func TestAnOrdinaryInstallKeepsAnInspectableConfig(t *testing.T) {
	out := render(t)
	for _, d := range docs(t, out) {
		meta, _ := d["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if d["kind"] == "ConfigMap" && strings.HasSuffix(name, "-registry") {
			return
		}
	}
	t.Error("the registry config is not a ConfigMap on a default install")
}
