package controller

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"sigs.k8s.io/yaml"
)

// registryPodSpec returns the registry StatefulSet's pod spec from a render.
func registryPodSpec(t *testing.T, out string) corev1.PodSpec {
	t.Helper()
	for _, doc := range strings.Split(out, "\n---") {
		var sts appsv1.StatefulSet
		if err := yaml.Unmarshal([]byte(doc), &sts); err != nil || sts.Kind != "StatefulSet" {
			continue
		}
		if strings.HasSuffix(sts.Name, "-registry") {
			return sts.Spec.Template.Spec
		}
	}
	t.Fatal("no registry StatefulSet rendered")
	return corev1.PodSpec{}
}

// TestTheRegistryCanBeSteeredAwayFromDrainedNodes.
//
// The defect this closes: the registry pod spec had no placement fields at all, so there was no
// supported way to keep it off the nodes being cordoned. It was rescheduled alongside the very
// pods that needed to pull from it, and they sat in ErrImagePull while it moved.
func TestTheRegistryCanBeSteeredAwayFromDrainedNodes(t *testing.T) {
	out := render(t,
		"--set", "registry.nodeSelector.storage=yes",
		"--set", "registry.priorityClassName=infra",
		"--set", "registry.tolerations[0].key=dedicated",
		"--set", "registry.tolerations[0].operator=Exists",
		"--set", "registry.topologySpreadConstraints[0].topologyKey=kubernetes.io/hostname",
		"--set", "registry.topologySpreadConstraints[0].maxSkew=1",
		"--set", "registry.topologySpreadConstraints[0].whenUnsatisfiable=ScheduleAnyway",
		"--set", "registry.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].key=zone",
		"--set", "registry.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].operator=Exists",
	)
	spec := registryPodSpec(t, out)

	if spec.NodeSelector["storage"] != "yes" {
		t.Error("registry.nodeSelector does not reach the registry pod")
	}
	if spec.PriorityClassName != "infra" {
		t.Errorf("registry.priorityClassName does not reach the registry pod, got %q", spec.PriorityClassName)
	}
	if len(spec.Tolerations) == 0 {
		t.Error("registry.tolerations does not reach the registry pod")
	}
	if len(spec.TopologySpreadConstraints) == 0 {
		t.Error("registry.topologySpreadConstraints does not reach the registry pod")
	}
	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil {
		t.Error("registry.affinity does not reach the registry pod")
	}
	if spec.TerminationGracePeriodSeconds == nil || *spec.TerminationGracePeriodSeconds != 30 {
		t.Error("the registry has no termination grace period, so in-flight pulls are cut off")
	}
}

// TestTheRegistrysPlacementIsNotTheControllers.
//
// The asymmetry that caused the incident: the controllers honoured the top-level scheduling values
// and the registry honoured nothing. Fixing it by pointing the registry at the SAME values would
// be the other half of the same mistake — the usual reason to steer the registry is that it should
// be somewhere the controllers are not.
func TestTheRegistrysPlacementIsNotTheControllers(t *testing.T) {
	out := render(t, "--set", "nodeSelector.role=controllers", "--set", "priorityClassName=controllers")
	spec := registryPodSpec(t, out)

	if spec.NodeSelector["role"] == "controllers" {
		t.Error("the top-level nodeSelector reached the registry; the registry must have its own")
	}
	if spec.PriorityClassName == "controllers" {
		t.Error("the top-level priorityClassName reached the registry; the registry must have its own")
	}
}

// TestABudgetThatCannotBeMetIsNeverRendered.
//
// The assertion that matters is the SECOND one. minAvailable 1 against a single replica can never
// be satisfied, so it blocks every drain forever: the node never cordons, a cluster upgrade stalls,
// and nothing in the events names a PodDisruptionBudget as the cause.
func TestABudgetThatCannotBeMetIsNeverRendered(t *testing.T) {
	if pdb, ok := registryPDB(t, render(t)); ok {
		t.Errorf("a PodDisruptionBudget rendered at one replica (%s); it would block every drain", pdb.Name)
	}

	out := render(t,
		"--set", "registry.cluster.enabled=true",
		"--set", "registry.cluster.replicaCount=3",
		"--set", "registry.storage.driver=s3",
		"--set", "registry.storage.s3.bucket=zot",
		"--set", "registry.cache.driver=redis",
		"--set", "registry.cache.redis.url=redis://redis:6379",
		"--set", "registry.persistence.enabled=false",
		"--set", "registry.tls.enabled=true",
	)
	pdb, ok := registryPDB(t, out)
	if !ok {
		t.Fatal("no PodDisruptionBudget at more than one replica, so a drain can evict them all at once")
	}
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Error("the budget must guarantee a floor of at least one serving registry")
	}

	// A budget selecting nothing renders, validates, and protects nothing.
	for k, v := range pdb.Spec.Selector.MatchLabels {
		if !strings.Contains(out, k+": "+v) {
			t.Errorf("the budget selects %s=%s, which nothing in the render carries", k, v)
		}
	}
}

func registryPDB(t *testing.T, out string) (policyv1.PodDisruptionBudget, bool) {
	t.Helper()
	for _, doc := range strings.Split(out, "\n---") {
		var pdb policyv1.PodDisruptionBudget
		if err := yaml.Unmarshal([]byte(doc), &pdb); err != nil || pdb.Kind != "PodDisruptionBudget" {
			continue
		}
		return pdb, true
	}
	return policyv1.PodDisruptionBudget{}, false
}

// TestEveryWorkloadCanUseAPullSecret — builder-deployment.yaml read `.Values.imagePullSecrets`,
// which is not a key this chart has, so the block rendered nothing and the builder could never pull
// from a private registry. The registry pod had no block at all.
func TestEveryWorkloadCanUseAPullSecret(t *testing.T) {
	out := render(t, "--set", "image.pullSecrets[0].name=regcred")

	for _, doc := range strings.Split(out, "\n---") {
		var probe struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Template struct {
					Spec corev1.PodSpec `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &probe); err != nil {
			continue
		}
		if probe.Kind != "Deployment" && probe.Kind != "StatefulSet" {
			continue
		}
		if len(probe.Spec.Template.Spec.ImagePullSecrets) == 0 {
			t.Errorf("%s %s ignores image.pullSecrets, so it cannot pull from a private registry",
				probe.Kind, probe.Metadata.Name)
		}
	}
}
