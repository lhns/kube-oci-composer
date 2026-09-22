//go:build e2e

// What the registry was actually deployed with, read from the cluster.
//
// The retention tests derive their timings from the deployed config and check their own
// preconditions against it, instead of trusting constants kept in step with up.sh by hand: a
// changed setting (e.g. deleteUntagged off, or a long gcDelay) can otherwise leave a test passing
// while measuring nothing.
package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// zotRetention is the part of the registry's config these tests reason about.
type zotRetention struct {
	Storage struct {
		GCDelay             string `json:"gcDelay"`
		GCInterval          string `json:"gcInterval"`
		GCMaxSchedulerDelay string `json:"gcMaxSchedulerDelay"`
		Retention           struct {
			Policies []struct {
				Repositories   []string        `json:"repositories"`
				DeleteUntagged *bool           `json:"deleteUntagged"`
				KeepUntagged   json.RawMessage `json:"keepUntagged"`
			} `json:"policies"`
		} `json:"retention"`
	} `json:"storage"`
}

// deployedRetention reads the registry's configuration from its ConfigMap -- what the running
// registry was handed, even if the release was changed after up.sh.
func deployedRetention(t *testing.T) zotRetention {
	t.Helper()
	raw := mustKubectl(t, "-n", operatorNamespace, "get", "configmap",
		"kube-oci-composer-registry", "-o", "jsonpath={.data.config\\.json}")

	var cfg zotRetention
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("parsing the deployed registry config: %v\n%s", err, raw)
	}
	return cfg
}

func parseDeployedDuration(t *testing.T, field, raw string) time.Duration {
	t.Helper()
	d, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("the deployed %s %q is not a duration: %v", field, raw, err)
	}
	return d
}

// deployedGCDelay: nothing younger is ever a collection candidate, so a test finishing inside it
// proves nothing about retention.
func deployedGCDelay(t *testing.T) time.Duration {
	t.Helper()
	return parseDeployedDuration(t, "gcDelay", deployedRetention(t).Storage.GCDelay)
}

// deployedGCInterval is how often a sweep is scheduled.
func deployedGCInterval(t *testing.T) time.Duration {
	t.Helper()
	return parseDeployedDuration(t, "gcInterval", deployedRetention(t).Storage.GCInterval)
}

// deployedRotation estimates how long the collector takes to come back to any ONE repository:
// zot's GCTaskGenerator issues one task per repository and restarts only after all were processed,
// so about N x gcInterval, with N counted from the catalog (build caches included).
func deployedRotation(t *testing.T) time.Duration {
	t.Helper()
	out := registryRequest(t, "GET", "/v2/_catalog?n=1000", "", "")

	var catalog struct {
		Repositories []string `json:"repositories"`
	}
	if i := strings.LastIndex(out, "{"); i >= 0 {
		if err := json.Unmarshal([]byte(out[i:]), &catalog); err != nil {
			t.Fatalf("parsing the registry catalog: %v\n%s", err, out)
		}
	}
	// At least one, or every derived deadline collapses to zero before anything is pushed.
	n := max(len(catalog.Repositories), 1)
	return time.Duration(n) * deployedGCInterval(t)
}

// requireCollectionPossible fails when a survival test's watch does not outlast gcDelay: the
// subject could not have been collected, so "still there" would say nothing.
func requireCollectionPossible(t *testing.T, within time.Duration, what string) {
	t.Helper()
	delay := deployedGCDelay(t)
	if within <= delay {
		t.Fatalf("this test watches %s for %s, but the registry will not collect anything younger "+
			"than gcDelay=%s -- so it would pass even with retention entirely broken. Lower "+
			"imageBuild.buildPollInterval (which floors gcDelay) or watch for longer.",
			what, within, delay)
	}
}

// requireUntaggedCollection fails when deleteUntagged is off for the repository, which would remove
// the mechanism the digest-only test measures.
func requireUntaggedCollection(t *testing.T, repository string) {
	t.Helper()
	for _, p := range deployedRetention(t).Storage.Retention.Policies {
		for _, glob := range p.Repositories {
			if !strings.HasPrefix(repository, strings.TrimSuffix(glob, "*")) {
				continue
			}
			if p.DeleteUntagged != nil && !*p.DeleteUntagged {
				t.Fatalf("deleteUntagged is false for %q, so an untagged manifest here is never "+
					"collected and this test would pass without measuring anything. It exists to "+
					"show that PULLING keeps one alive.", repository)
			}
		}
	}
}

// requireKeepUntaggedOff fails when keepUntagged is configured: zot then keeps every manifest whose
// last tag expired, and a control waiting for one to go would time out (ADR 0060).
func requireKeepUntaggedOff(t *testing.T) {
	t.Helper()
	for _, p := range deployedRetention(t).Storage.Retention.Policies {
		if len(p.KeepUntagged) > 0 {
			t.Fatalf("the registry configures keepUntagged (%s). With it, zot keeps every manifest "+
				"whose last tag expired, without evaluating it, so nothing this test publishes can be "+
				"reclaimed and its control can never fire. Set registry.retention.keepUntagged=false.",
				p.KeepUntagged)
		}
	}
}

// TestTheSchedulerDelayReachesTheRegistry -- gcMaxSchedulerDelay decides how soon anything is
// collected (a pass takes about repositories x delay / 2; ~500s at zot's 30s default). zot hides the
// field and it is reachable only through mapstructure, so this proves it was DELIVERED; the negative
// controls' latency is the evidence zot honours it.
func TestTheSchedulerDelayReachesTheRegistry(t *testing.T) {
	got := deployedRetention(t).Storage.GCMaxSchedulerDelay
	if got == "" {
		t.Fatal("the registry was deployed without gcMaxSchedulerDelay, so every collection task " +
			"waits up to zot's default 30s and a pass over this suite's repositories takes minutes")
	}
	d, err := time.ParseDuration(got)
	if err != nil {
		t.Fatalf("gcMaxSchedulerDelay %q is not a duration: %v", got, err)
	}
	if d > 5*time.Second {
		t.Errorf("gcMaxSchedulerDelay is %s; every retention wait in this suite scales with it", d)
	}
}
