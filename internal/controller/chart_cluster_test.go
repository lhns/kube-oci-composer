package controller

import (
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// clusterArgs is a complete, valid clustered configuration. Every guard test below removes exactly
// one thing from it, so a failure names the thing that was removed rather than the first thing
// missing.
var clusterArgs = []string{
	"--set", "registry.publish.mode=internalOnly",
	"--set", "registry.cluster.enabled=true",
	"--set", "registry.storage.driver=s3",
	"--set", "registry.storage.s3.bucket=zot",
	"--set", "registry.cache.driver=redis",
	"--set", "registry.cache.redis.url=redis://redis:6379",
	"--set", "registry.persistence.enabled=false",
	"--set", "registry.tls.enabled=true",
}

func withoutArg(t *testing.T, key string) []string {
	t.Helper()
	out := make([]string, 0, len(clusterArgs))
	for i := 0; i < len(clusterArgs); i += 2 {
		if strings.HasPrefix(clusterArgs[i+1], key+"=") {
			continue
		}
		out = append(out, clusterArgs[i], clusterArgs[i+1])
	}
	return out
}

// TestTheRegistryIsAlwaysAStatefulSet.
//
// Not conditional on clustering, and deliberately so: switching kinds on `cluster.enabled=true`
// would have Helm create a StatefulSet with the same name while the Deployment's ReplicaSet still
// owned pods matching the same selector — two controllers fighting over one pod, on the day the
// operator is already changing storage, cache and TLS at once.
func TestTheRegistryIsAlwaysAStatefulSet(t *testing.T) {
	out := render(t, "--set", "registry.publish.mode=internalOnly")

	var found bool
	for _, doc := range strings.Split(out, "\n---") {
		var probe struct {
			Kind     string                `json:"kind"`
			Metadata struct{ Name string } `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &probe); err != nil {
			continue
		}
		if probe.Kind == "Deployment" && strings.HasSuffix(probe.Metadata.Name, "-registry") {
			t.Error("the registry must not be a Deployment; the kind switch is the migration landmine")
		}
		if probe.Kind == "StatefulSet" {
			found = true
			var sts appsv1.StatefulSet
			if err := yaml.Unmarshal([]byte(doc), &sts); err != nil {
				t.Fatalf("parsing the StatefulSet: %v", err)
			}
			if sts.Spec.ServiceName == "" {
				t.Error("a StatefulSet without serviceName gives its pods no stable DNS name")
			}
			// The PVC must stay a plain volume. volumeClaimTemplates would create data-<sts>-0 and
			// orphan the existing claim — which for ImageBuild holds the only copy of its output.
			if len(sts.Spec.VolumeClaimTemplates) != 0 {
				t.Error("volumeClaimTemplates would orphan the existing PVC, which is ImageBuild's system of record")
			}
		}
	}
	if !found {
		t.Fatal("no StatefulSet rendered")
	}
}

// TestTheHeadlessServicePublishesNotReadyAddresses — members must resolve each other DURING
// startup, before any of them is ready. Without this a cold cluster never forms: every member waits
// for peers DNS refuses to name until they are ready, and none of them ever is.
func TestTheHeadlessServicePublishesNotReadyAddresses(t *testing.T) {
	out := render(t, "--set", "registry.publish.mode=internalOnly")
	for _, doc := range strings.Split(out, "\n---") {
		var svc corev1.Service
		if err := yaml.Unmarshal([]byte(doc), &svc); err != nil || svc.Kind != "Service" {
			continue
		}
		if !strings.HasSuffix(svc.Name, "-registry-headless") {
			continue
		}
		if svc.Spec.ClusterIP != "None" {
			t.Errorf("the headless Service must have clusterIP None, got %q", svc.Spec.ClusterIP)
		}
		if !svc.Spec.PublishNotReadyAddresses {
			t.Error("without publishNotReadyAddresses a cold cluster cannot form")
		}
		return
	}
	t.Fatal("no headless Service rendered; the StatefulSet names one")
}

// TestTheClusterConfigNamesEveryMember — one entry per ordinal, on the CONTAINER port. Members
// address pods directly, so registry.service.port is not involved.
func TestTheClusterConfigNamesEveryMember(t *testing.T) {
	out := render(t, append(clusterArgs, "--set", "registry.cluster.replicaCount=3")...)

	var cfg struct {
		Cluster struct {
			Members []string `json:"members"`
			HashKey string   `json:"hashKey"`
			TLS     struct {
				CACert string `json:"cacert"`
			} `json:"tls"`
		} `json:"cluster"`
	}
	raw := zotConfig(t, out)
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("zot config is not valid JSON: %v\n%s", err, raw)
	}

	if len(cfg.Cluster.Members) != 3 {
		t.Fatalf("expected 3 members, got %v", cfg.Cluster.Members)
	}
	for i, m := range cfg.Cluster.Members {
		if !strings.Contains(m, "-registry-headless.") {
			t.Errorf("member %d must be addressed through the headless Service: %q", i, m)
		}
		if !strings.HasSuffix(m, ":5000") {
			t.Errorf("member %d must use the container port: %q", i, m)
		}
	}
	if len(cfg.Cluster.HashKey) != 16 {
		t.Errorf("hashKey must be 16 characters for siphash-2-4, got %d", len(cfg.Cluster.HashKey))
	}
	if cfg.Cluster.TLS.CACert == "" {
		t.Error("members need a CA to verify each other with")
	}
}

// TestTheClusterCAIsNotTheClientCA is one word apart from a cluster-wide outage.
//
// `cluster.tls.cacert` is how members verify peers. `http.tls.cacert` makes zot demand a client
// certificate from EVERY caller, including the kubelet, which has none — every pull stops.
func TestTheClusterCAIsNotTheClientCA(t *testing.T) {
	var cfg struct {
		HTTP map[string]any `json:"http"`
	}
	if err := json.Unmarshal([]byte(zotConfig(t, render(t, clusterArgs...))), &cfg); err != nil {
		t.Fatalf("parsing the zot config: %v", err)
	}
	tlsBlock, ok := cfg.HTTP["tls"].(map[string]any)
	if !ok {
		t.Fatal("http.tls should be set when TLS is on")
	}
	if _, bad := tlsBlock["cacert"]; bad {
		t.Fatal("http.tls.cacert turns on mutual TLS for every caller, including the kubelet — every pull in the cluster would stop")
	}
}

// TestClusteringRefusesWhatItCannotDo. Each case removes exactly one prerequisite from a
// configuration that otherwise works.
func TestClusteringRefusesWhatItCannotDo(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"without S3", withoutArg(t, "registry.storage.driver"), "storage.driver=s3"},
		{"without a shared cache", withoutArg(t, "registry.cache.driver"), "cache.driver"},
		{
			// Refused, not silently dropped: that PVC may hold the only copy of an ImageBuild's
			// output, and its output cannot be rebuilt from its spec.
			"with the RWO volume still enabled",
			withoutArg(t, "registry.persistence.enabled"),
			"persistence.enabled=false",
		},
		{
			// Members proxy authenticated writes to each other; without TLS that is threat I7
			// reopened on the inside of the thing that closed it.
			"without TLS",
			withoutArg(t, "registry.tls.enabled"),
			"tls.enabled=true",
		},
		{
			"with a hash key of the wrong length",
			append(append([]string{}, clusterArgs...), "--set", "registry.cluster.hashKey=tooshort"),
			"16 characters",
		},
		{
			"S3 with no bucket",
			[]string{
				"--set", "registry.publish.mode=internalOnly",
				"--set", "registry.storage.driver=s3",
			},
			"s3.bucket",
		},
		{
			"redis with no url",
			[]string{
				"--set", "registry.publish.mode=internalOnly",
				"--set", "registry.cache.driver=redis",
			},
			"redis.url",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := renderExpectingFailure(t, tc.args...)
			if !strings.Contains(out, tc.want) {
				t.Fatalf("the render failed, but not for this reason — wanted %q in:\n%s", tc.want, out)
			}
		})
	}
}

// TestAValidClusterRenders is the control. Without it, an always-firing guard would pass every
// case above.
func TestAValidClusterRenders(t *testing.T) {
	out := render(t, clusterArgs...)
	if !strings.Contains(out, `"cluster"`) {
		t.Error("a valid clustered configuration must actually render the cluster block")
	}
}

// zotConfig pulls the rendered config.json out of a render.
func zotConfig(t *testing.T, out string) string {
	t.Helper()
	for _, doc := range strings.Split(out, "\n---") {
		var cm corev1.ConfigMap
		if err := yaml.Unmarshal([]byte(doc), &cm); err != nil || cm.Kind != "ConfigMap" {
			continue
		}
		if raw, ok := cm.Data["config.json"]; ok {
			return raw
		}
	}
	t.Fatal("no zot config rendered")
	return ""
}
