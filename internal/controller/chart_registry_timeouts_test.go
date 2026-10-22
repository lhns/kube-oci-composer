package controller

import (
	"encoding/json"
	"strings"
	"testing"
)

// registryConfigs returns every zot config the chart renders (writer and read replicas), keyed by
// ConfigMap name.
func registryConfigs(t *testing.T, args ...string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, d := range docs(t, render(t, args...)) {
		if d["kind"] != "ConfigMap" {
			continue
		}
		meta, _ := d["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		data, _ := d["data"].(map[string]any)
		raw, ok := data["config.json"].(string)
		if !ok {
			continue
		}
		var cfg map[string]any
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			t.Fatalf("%s config.json is not valid JSON: %v", name, err)
		}
		out[name] = cfg
	}
	return out
}

// registryConfig returns the writer's config, which is the one every non-replica test means.
func registryConfig(t *testing.T, args ...string) map[string]any {
	t.Helper()
	for name, cfg := range registryConfigs(t, args...) {
		if strings.HasSuffix(name, "-registry") {
			return cfg
		}
	}
	t.Fatal("no registry config rendered")
	return nil
}

// TestTheRegistryAlwaysStatesItsReadTimeout: when the key is absent zot defaults to 60s, and Go's
// ReadTimeout bounds the whole request including time queued behind zot's upload lock, so large
// pushes cannot finish. ADR 0047.
func TestTheRegistryAlwaysStatesItsReadTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"defaults", nil},
		{"tls", []string{"--set", "registry.tls.enabled=true"}},
		{"s3", []string{"--set", "registry.storage.driver=s3",
			"--set", "registry.storage.s3.bucket=b", "--set", "registry.storage.s3.region=r"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			http, _ := registryConfig(t, tc.args...)["http"].(map[string]any)
			if got, _ := http["readTimeout"].(string); got == "" {
				t.Errorf("no readTimeout: zot falls back to 60s and large pushes cannot finish, got %v", http)
			}
		})
	}
}

// TestDedupeIsStatedAndSettable: dedupe (default true, as in zot) trades disk for lock contention,
// and turning it off must reach the config.
func TestDedupeIsStatedAndSettable(t *testing.T) {
	storage, _ := registryConfig(t)["storage"].(map[string]any)
	if got, ok := storage["dedupe"].(bool); !ok || !got {
		t.Errorf("dedupe = %v; the default should state zot's own behaviour rather than omit it", storage["dedupe"])
	}

	storage, _ = registryConfig(t, "--set", "registry.storage.dedupe=false")["storage"].(map[string]any)
	if got, _ := storage["dedupe"].(bool); got {
		t.Error("registry.storage.dedupe=false did not reach the rendered config")
	}
}

// TestAnEmptyReadTimeoutIsRefused: an empty value would silently restore zot's 60s default.
func TestAnEmptyReadTimeoutIsRefused(t *testing.T) {
	out := renderExpectingFailure(t, "--set", "registry.readTimeout=")
	if !strings.Contains(out, "registry.readTimeout must be set") {
		t.Errorf("an empty readTimeout rendered instead of failing:\n%s", out)
	}
}

// TestKeepTagsAlsoKeysOnPushRecency: with `pulledWithin` alone, a freshly built image is unprotected
// until the refresher runs. The rules are OR'ed, so pushedWithin is a floor, not a second window
// (zot records the push time only when a digest is new).
func TestKeepTagsAlsoKeysOnPushRecency(t *testing.T) {
	storage, _ := registryConfig(t)["storage"].(map[string]any)
	retention, _ := storage["retention"].(map[string]any)
	policies, _ := retention["policies"].([]any)
	if len(policies) == 0 {
		t.Fatalf("no retention policy rendered: %v", retention)
	}
	policy, _ := policies[0].(map[string]any)
	keepTags, _ := policy["keepTags"].([]any)

	// Exactly ONE entry carrying both rules: zot's getTagPolicy stops at the first matching pattern,
	// so a second ".*" entry is never evaluated.
	if len(keepTags) != 1 {
		t.Fatalf("keepTags has %d entries; it must have exactly ONE carrying both rules, because "+
			"zot matches the first pattern and stops. A second .* entry is dead configuration that "+
			"reads as protection: %v", len(keepTags), keepTags)
	}
	entry, _ := keepTags[0].(map[string]any)
	if patterns, _ := entry["patterns"].([]any); len(patterns) == 0 {
		t.Errorf("the keepTags entry has no patterns, so it protects no tag: %v", entry)
	}
	if entry["pulledWithin"] == nil {
		t.Errorf("keepTags does not key on pulledWithin, so refreshing protects nothing: %v", entry)
	}
	if got := entry["pushedWithin"]; got == nil {
		t.Errorf("keepTags does not key on pushedWithin, so an image built between refreshes is a "+
			"deletion candidate: %v", entry)
	} else if got != "720h" {
		t.Errorf("pushedWithin = %v; it tracks retention.window", got)
	}

	storage, _ = registryConfig(t, "--set", "retention.window=48h",
		"--set", "operator.retention.refreshInterval=1h",
		"--set", "imageBuild.retention.refreshInterval=1h")["storage"].(map[string]any)
	raw, _ := json.Marshal(storage["retention"])
	if !strings.Contains(string(raw), `"pushedWithin":"48h"`) {
		t.Errorf("retention.window did not reach pushedWithin: %s", raw)
	}
}
