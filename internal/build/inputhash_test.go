package build

import "testing"

func sampleInputs() Inputs {
	return Inputs{
		BuilderDigest:    "sha256:aaaa",
		FrontendDigest:   "sha256:bbbb",
		ContextDigest:    "sha256:cccc",
		ContextSubpath:   "src",
		Dockerfile:       "Dockerfile",
		Target:           "runtime",
		Network:          "Sandbox",
		CacheMode:        "Auto",
		CacheRef:         "ghcr.io/me/app-buildcache",
		SourceDateEpoch:  "0",
		Platforms:        []string{"linux/amd64", "linux/arm64"},
		Args:             map[string]string{"VERSION": "1.2.3", "COMMIT": "abc"},
		SecretIdentities: []string{"npmrc/1234"},
	}
}

// TestHashIsStable — the whole short-circuit rests on this: if the hash moved between calls, every
// reconcile would be a build.
func TestHashIsStable(t *testing.T) {
	want := sampleInputs().Hash()
	for i := range 5 {
		if got := sampleInputs().Hash(); got != want {
			t.Fatalf("call %d gave %s, want %s", i, got, want)
		}
	}
}

// TestEveryFieldMovesTheHash — a field that does not move the hash is a field that can change the
// output without triggering a rebuild, which is the failure mode ADR 0002 describes for
// AssemblyVersion: "keep serving artifacts built by the old algorithm, forever".
func TestEveryFieldMovesTheHash(t *testing.T) {
	base := sampleInputs().Hash()

	mutations := map[string]func(*Inputs){
		"builder digest":  func(in *Inputs) { in.BuilderDigest = "sha256:changed" },
		"frontend digest": func(in *Inputs) { in.FrontendDigest = "sha256:changed" },
		"context digest":  func(in *Inputs) { in.ContextDigest = "sha256:changed" },
		"context subpath": func(in *Inputs) { in.ContextSubpath = "other" },
		"dockerfile":      func(in *Inputs) { in.Dockerfile = "build/Dockerfile" },
		"target":          func(in *Inputs) { in.Target = "debug" },
		"network":         func(in *Inputs) { in.Network = "None" },
		"cache mode":      func(in *Inputs) { in.CacheMode = "Disabled" },
		"cache ref":       func(in *Inputs) { in.CacheRef = "elsewhere" },
		"epoch":           func(in *Inputs) { in.SourceDateEpoch = "1700000000" },
		"platform added":  func(in *Inputs) { in.Platforms = append(in.Platforms, "linux/arm/v7") },
		"platform order":  func(in *Inputs) { in.Platforms = []string{"linux/arm64", "linux/amd64"} },
		"arg value":       func(in *Inputs) { in.Args["VERSION"] = "9.9.9" },
		"arg renamed":     func(in *Inputs) { delete(in.Args, "VERSION"); in.Args["RELEASE"] = "1.2.3" },
		"arg added":       func(in *Inputs) { in.Args["X"] = "y" },
		"secret rotated":  func(in *Inputs) { in.SecretIdentities = []string{"npmrc/5678"} },
		"secret added": func(in *Inputs) {
			in.SecretIdentities = append(in.SecretIdentities, "other/1")
		},
	}

	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			in := sampleInputs()
			mutate(&in)
			if in.Hash() == base {
				t.Error("the hash did not move, so this change would not trigger a rebuild")
			}
		})
	}
}

// TestArgOrderDoesNotMatter — the opposite case. ARG order in a spec does not change what the
// build sees, so reordering the list must not rebuild.
func TestArgOrderDoesNotMatter(t *testing.T) {
	a, b := sampleInputs(), sampleInputs()
	b.Args = map[string]string{"COMMIT": "abc", "VERSION": "1.2.3"}
	if a.Hash() != b.Hash() {
		t.Error("reordering args rebuilt; the spec means the same thing either way")
	}
}

// TestSecretValuesAreNotHashed is a security property, not a correctness one.
//
// status.inputHash is readable by anyone with get on the object. If the value were hashed, that
// field would be an offline oracle against a low-entropy secret. Identity plus resourceVersion
// gives the rebuild-on-rotation behaviour without the oracle.
func TestSecretValuesAreNotHashed(t *testing.T) {
	// The struct offers nowhere to put a value, which is the real enforcement — a field carrying
	// one would not compile here. What is asserted is the behaviour that depends on it.
	rotated := sampleInputs()
	rotated.SecretIdentities = []string{"npmrc/1235"}
	if rotated.Hash() == sampleInputs().Hash() {
		t.Error("a rotated secret did not rebuild")
	}
}
