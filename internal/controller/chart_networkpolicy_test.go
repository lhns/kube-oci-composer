package controller

import (
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"
)

func registryNetworkPolicy(t *testing.T, args ...string) *networkingv1.NetworkPolicy {
	t.Helper()
	out := render(t, args...)
	for _, doc := range strings.Split(out, "\n---") {
		var probe struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &probe); err != nil || probe.Kind != "NetworkPolicy" {
			continue
		}
		var np networkingv1.NetworkPolicy
		if err := yaml.Unmarshal([]byte(doc), &np); err != nil {
			t.Fatalf("parsing NetworkPolicy: %v", err)
		}
		return &np
	}
	return nil
}

// TestTheRegistryPolicyAdmitsEveryNamespaceByDefault is the property the policy exists for.
//
// A build Job runs in its OBJECT's namespace, not the release's, so every build crosses a namespace
// boundary to push. In a default-deny cluster nothing lets it through, and the build fails in a way
// that looks like a registry fault. Defaulting to every namespace is deliberate: this is a
// connectivity guarantee, and reads are anonymous by design while writes need the password, so a
// namespace boundary in front of that adds no authority to either rule.
func TestTheRegistryPolicyAdmitsEveryNamespaceByDefault(t *testing.T) {
	np := registryNetworkPolicy(t, "--set", "registry.publish.mode=internalOnly")
	if np == nil {
		t.Fatal("no NetworkPolicy rendered; builds in other namespaces would be blocked in a default-deny cluster")
	}

	// It must select the registry and nothing else. A policy selecting more pods than it means to
	// would silently restrict the controllers as well.
	if got := np.Spec.PodSelector.MatchLabels["app.kubernetes.io/component"]; got != "registry" {
		t.Fatalf("the policy selects component %q, not the registry", got)
	}

	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("expected one ingress rule, got %d", len(np.Spec.Ingress))
	}
	rule := np.Spec.Ingress[0]

	var open bool
	for _, peer := range rule.From {
		// An empty namespaceSelector matches every namespace. A nil one does not — it would mean
		// "this namespace only", which is exactly the bug this test exists to catch.
		if peer.NamespaceSelector != nil &&
			len(peer.NamespaceSelector.MatchLabels) == 0 &&
			len(peer.NamespaceSelector.MatchExpressions) == 0 {
			open = true
		}
	}
	if !open {
		t.Errorf("the default policy must admit every namespace, or builds elsewhere cannot push: %+v", rule.From)
	}

	if len(rule.Ports) != 1 || rule.Ports[0].Port.IntValue() != 5000 {
		t.Errorf("the rule must name the registry port: %+v", rule.Ports)
	}
}

// TestNarrowingThePolicyAlwaysKeepsTheReleaseNamespace.
//
// Both controllers live in the release namespace and talk to the registry constantly. Narrowing the
// list and forgetting it would stop publishing — and, worse, stop the retention refresh, whose
// silence is not an outage but a deletion one window later (ADR 0031).
func TestNarrowingThePolicyAlwaysKeepsTheReleaseNamespace(t *testing.T) {
	np := registryNetworkPolicy(t,
		"--set", "registry.publish.mode=internalOnly",
		"--set", "registry.networkPolicy.allowedNamespaces={team-a,team-b}",
	)
	if np == nil {
		t.Fatal("no NetworkPolicy rendered")
	}

	seen := map[string]bool{}
	for _, peer := range np.Spec.Ingress[0].From {
		if peer.NamespaceSelector != nil {
			if n, ok := peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; ok {
				seen[n] = true
			}
			if len(peer.NamespaceSelector.MatchLabels) == 0 {
				t.Error("a narrowed policy must not still admit every namespace")
			}
		}
	}
	for _, want := range []string{"team-a", "team-b", "oci-composer"} {
		if !seen[want] {
			t.Errorf("namespace %q is not admitted; got %v", want, seen)
		}
	}
}

// TestNodeCIDRsBecomeAnIpBlock — kubelet image pulls arrive from the NODE's network namespace and
// no podSelector can ever match them. If node traffic is not otherwise permitted, this is the only
// way to admit it, and the symptom of getting it wrong is a pull that hangs.
func TestNodeCIDRsBecomeAnIpBlock(t *testing.T) {
	np := registryNetworkPolicy(t,
		"--set", "registry.publish.mode=internalOnly",
		"--set", "registry.networkPolicy.nodeCIDRs={10.0.0.0/24,10.0.1.0/24}",
	)
	if np == nil {
		t.Fatal("no NetworkPolicy rendered")
	}
	var cidrs []string
	for _, peer := range np.Spec.Ingress[0].From {
		if peer.IPBlock != nil {
			cidrs = append(cidrs, peer.IPBlock.CIDR)
		}
	}
	if len(cidrs) != 2 || cidrs[0] != "10.0.0.0/24" || cidrs[1] != "10.0.1.0/24" {
		t.Fatalf("node CIDRs must render as ipBlock peers; got %v", cidrs)
	}
}

// TestThePolicyCanBeTurnedOffEntirely. Anyone whose CNI ignores NetworkPolicy, or who manages
// policy centrally, should be able to render nothing rather than a resource that misleads.
func TestThePolicyCanBeTurnedOffEntirely(t *testing.T) {
	np := registryNetworkPolicy(t,
		"--set", "registry.publish.mode=internalOnly",
		"--set", "registry.networkPolicy.enabled=false",
	)
	if np != nil {
		t.Fatal("networkPolicy.enabled=false must render no policy at all")
	}
}
