package controller

import (
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"
)

// registryNetworkPolicy returns the policy in front of the registry, or nil.
func registryNetworkPolicy(t *testing.T, args ...string) *networkingv1.NetworkPolicy {
	t.Helper()
	return networkPolicyNamed(t, "-registry", args...)
}

// builderContextPolicy returns the policy in front of the builder's context endpoint, or nil.
func builderContextPolicy(t *testing.T, args ...string) *networkingv1.NetworkPolicy {
	t.Helper()
	return networkPolicyNamed(t, "-builder-context", args...)
}

// networkPolicyNamed returns the rendered NetworkPolicy whose name ends in suffix, or nil. Selected
// by name because the chart renders more than one.
func networkPolicyNamed(t *testing.T, suffix string, args ...string) *networkingv1.NetworkPolicy {
	t.Helper()
	for _, doc := range splitDocs(render(t, args...)) {
		var probe struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &probe); err != nil || probe.Kind != "NetworkPolicy" {
			continue
		}
		if !strings.HasSuffix(probe.Metadata.Name, suffix) {
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

// TestTheRegistryPolicyAdmitsEveryNamespaceByDefault: build Jobs run in their object's namespace,
// so every push crosses a namespace boundary. The policy is a connectivity guarantee for
// default-deny clusters; authority comes from the registry's own auth, not the namespace.
func TestTheRegistryPolicyAdmitsEveryNamespaceByDefault(t *testing.T) {
	np := registryNetworkPolicy(t, "--set", "registry.publish.mode=internalOnly")
	if np == nil {
		t.Fatal("no NetworkPolicy rendered; builds in other namespaces would be blocked in a default-deny cluster")
	}

	// Every pod serving the registry API (writer and read replicas) and nothing else: missing a
	// replica blackholes a share of pulls in a default-deny cluster.
	if got := np.Spec.PodSelector.MatchLabels["oci-composer.lhns.de/registry-role"]; got != "serve" {
		t.Fatalf("the policy selects registry-role %q; it must cover every pod serving the API", got)
	}
	if _, ok := np.Spec.PodSelector.MatchLabels["app.kubernetes.io/component"]; ok {
		t.Error("the policy selects a single component, so read replicas would be left unprotected " +
			"and unreachable in a default-deny cluster")
	}

	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("expected one ingress rule, got %d", len(np.Spec.Ingress))
	}
	rule := np.Spec.Ingress[0]

	var open bool
	for _, peer := range rule.From {
		// An empty namespaceSelector matches every namespace; a nil one means "this namespace only".
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

// TestNarrowingThePolicyAlwaysKeepsTheReleaseNamespace: the controllers live there; losing access
// stops publishing and the retention refresh, which means deletions one window later (ADR 0031).
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

// TestNodeCIDRsBecomeAnIpBlock: kubelet pulls come from the node's network, which no podSelector
// matches; an ipBlock is the only way to admit them.
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

// TestThePolicyCanBeTurnedOffEntirely: for CNIs that ignore NetworkPolicy or central policy
// management.
func TestThePolicyCanBeTurnedOffEntirely(t *testing.T) {
	np := registryNetworkPolicy(t,
		"--set", "registry.publish.mode=internalOnly",
		"--set", "registry.networkPolicy.enabled=false",
	)
	if np != nil {
		t.Fatal("networkPolicy.enabled=false must render no policy at all")
	}
}

// TestTheContextPolicyAdmitsEveryNamespaceByDefault is the fetch-side counterpart of the registry
// policy: build Jobs cross a namespace boundary to fetch their source from the builder.
func TestTheContextPolicyAdmitsEveryNamespaceByDefault(t *testing.T) {
	np := builderContextPolicy(t, "--set", "registry.publish.mode=internalOnly")
	if np == nil {
		t.Fatal("no context NetworkPolicy rendered; builds in other namespaces cannot fetch their source")
	}
	if got := np.Spec.PodSelector.MatchLabels["app.kubernetes.io/component"]; got != "builder" {
		t.Errorf("the policy selects component %q; it must select the builder", got)
	}
	if len(np.Spec.Ingress) != 1 || len(np.Spec.Ingress[0].From) != 1 {
		t.Fatalf("want one ingress rule from one source, got %+v", np.Spec.Ingress)
	}
	sel := np.Spec.Ingress[0].From[0].NamespaceSelector
	if sel == nil || len(sel.MatchLabels) != 0 {
		t.Errorf("the default policy must admit every namespace, got %+v", sel)
	}
}

// TestNarrowingTheContextPolicyKeepsTheReleaseNamespace: ImageBuilds in the release namespace build
// there too.
func TestNarrowingTheContextPolicyKeepsTheReleaseNamespace(t *testing.T) {
	np := builderContextPolicy(t,
		"--set", "registry.publish.mode=internalOnly",
		"--set", "imageBuild.networkPolicy.allowedNamespaces={team-a}")
	if np == nil {
		t.Fatal("no context NetworkPolicy rendered")
	}

	admitted := map[string]bool{}
	for _, from := range np.Spec.Ingress[0].From {
		if from.NamespaceSelector == nil {
			continue
		}
		if len(from.NamespaceSelector.MatchLabels) == 0 {
			t.Error("a narrowed policy must not still admit every namespace")
		}
		admitted[from.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]] = true
	}
	for _, want := range []string{"team-a", "oci-composer"} {
		if !admitted[want] {
			t.Errorf("namespace %q is not admitted; got %v", want, admitted)
		}
	}
}

// TestBuildPodsAreToldTheServiceAddress: build pods in other namespaces use the URL, so it must not
// be loopback.
func TestBuildPodsAreToldTheServiceAddress(t *testing.T) {
	out := render(t, "--set", "registry.publish.mode=internalOnly")
	if !strings.Contains(out, "--context-base-url=http://test-release-kube-oci-composer-builder-context.oci-composer.svc.") {
		t.Error("the builder is not given a cluster-resolvable context URL")
	}
	if strings.Contains(out, "--context-base-url=http://localhost") ||
		strings.Contains(out, "--context-base-url=http://127.0.0.1") {
		t.Error("the context URL is loopback; build pods in other namespaces cannot reach it")
	}
}
