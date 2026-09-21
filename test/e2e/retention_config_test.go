//go:build e2e

// What the registry was actually deployed with, read from the cluster.
//
// These tests used to hardcode durations chosen against the values up.sh happened to set, and kept
// in step by hand -- which held right up until someone changed one. Two ways that went wrong in a
// single sitting:
//
//   - deleteUntagged was switched off to dodge an unrelated race, and
//     TestPullingByDigestKeepsAnUntaggedImageAlive went on passing while measuring nothing: the
//     manifest survived because collection was disabled, not because pulling protected it.
//   - a gcDelay floor was very nearly set to 10m, which would have made EVERY retention test
//     vacuous, because nothing younger than gcDelay is ever collected and they all refresh for 90s.
//
// Neither is visible in a passing run. So the tests read the deployed policy and check their own
// preconditions against it, rather than trusting that a constant here still matches a flag there.
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
		GCDelay    string `json:"gcDelay"`
		GCInterval string `json:"gcInterval"`
		Retention  struct {
			Policies []struct {
				Repositories   []string `json:"repositories"`
				DeleteUntagged *bool    `json:"deleteUntagged"`
			} `json:"policies"`
		} `json:"retention"`
	} `json:"storage"`
}

// deployedRetention reads the registry's own configuration out of the cluster.
//
// The ConfigMap rather than the values file: this is what the running registry was handed, so it
// stays true even if someone upgraded the release by hand between `up.sh` and `go test`.
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

// deployedGCDelay is the floor on how long anything survives regardless of policy.
//
// It is the number every duration in these tests has to clear: nothing younger than this is ever a
// collection candidate, so a test that finishes inside it has proven nothing about retention.
func deployedGCDelay(t *testing.T) time.Duration {
	t.Helper()
	raw := deployedRetention(t).Storage.GCDelay
	d, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("the deployed gcDelay %q is not a duration: %v", raw, err)
	}
	return d
}

// deployedGCInterval is how often a sweep is scheduled. Clearing gcDelay only makes content
// ELIGIBLE; a sweep still has to run before anything actually goes.
func deployedGCInterval(t *testing.T) time.Duration {
	t.Helper()
	raw := deployedRetention(t).Storage.GCInterval
	d, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("the deployed gcInterval %q is not a duration: %v", raw, err)
	}
	return d
}

// requireCollectionPossible fails when the test could not observe a collection even if retention
// were completely broken.
//
// The assertion these tests were missing. A survival test is only evidence if the thing could have
// died: `within` has to outlast gcDelay, or "it is still there" says nothing at all.
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

// requireUntaggedCollection fails when untagged collection is switched off, which silently removes
// the mechanism the untagged test exists to measure.
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
