package controller

import (
	"encoding/json"
	"strings"
	"testing"
)

// registryConfig returns the zot config the chart renders.
func registryConfig(t *testing.T, args ...string) map[string]any {
	t.Helper()
	for _, d := range docs(t, render(t, args...)) {
		if d["kind"] != "ConfigMap" {
			continue
		}
		meta, _ := d["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if !strings.HasSuffix(name, "-registry") {
			continue
		}
		data, _ := d["data"].(map[string]any)
		raw, ok := data["config.json"].(string)
		if !ok {
			continue
		}
		var cfg map[string]any
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			t.Fatalf("%s config.json is not valid JSON: %v", name, err)
		}
		return cfg
	}
	t.Fatal("no registry config rendered")
	return nil
}

// TestTheRegistryAlwaysStatesItsReadTimeout is the regression that would silently restore the bug.
//
// zot's CLI injects defaultReadTimeout = 60s when this key is absent, and Go's ReadTimeout bounds
// the WHOLE request including the body -- so it counts time queued behind zot's registry-wide
// upload lock. Measured: twenty concurrent 1MB pushes take 3.9-6.1s each. A large layer on slower
// storage passes 60s having transferred nothing, and the retry queues behind the next one.
//
// Absent this key the chart looks fine and the registry cannot accept a large push. ADR 0047.
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

// TestDedupeIsStatedAndSettable — the lever for lock contention, and it must reach the config.
//
// Default true, which is what zot does anyway: turning it off trades disk for a shorter critical
// section, and that is the operator's call.
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

// TestAnEmptyReadTimeoutIsRefused — rendering no key silently restores zot's 60s, which is the
// failure being fixed. A plausible edit must not reintroduce it quietly.
func TestAnEmptyReadTimeoutIsRefused(t *testing.T) {
	out := renderExpectingFailure(t, "--set", "registry.readTimeout=")
	if !strings.Contains(out, "registry.readTimeout must be set") {
		t.Errorf("an empty readTimeout rendered instead of failing:\n%s", out)
	}
}
