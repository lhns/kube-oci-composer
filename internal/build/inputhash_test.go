package build

import (
	"reflect"
	"testing"
)

func sampleInputs() Inputs {
	return Inputs{
		BuilderDigest:    "sha256:aaaa",
		FrontendDigest:   "sha256:bbbb",
		ContextKind:      "sourceRef",
		ContextDigest:    "sha256:cccc",
		ContextSubpath:   "src",
		DockerfileKind:   "path",
		Dockerfile:       "Dockerfile",
		DockerfileDigest: "",
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
// hashMutations is the one list of "change this, and the hash must move".
//
// Extracted so TestEveryFieldIsAccountedFor can check it covers every field, rather than the two
// drifting apart — which is the failure a hand-maintained list always eventually has.
type hashMutation struct {
	// field names the Inputs field this case exercises. Explicit rather than parsed out of the
	// description: TestEveryFieldIsAccountedFor matches on it, and a heuristic over prose is a
	// guard that silently stops covering things.
	field  string
	name   string
	mutate func(*Inputs)
}

func hashMutations() []hashMutation {
	return []hashMutation{
		{"BuilderDigest", "builder digest", func(in *Inputs) { in.BuilderDigest = "sha256:changed" }},
		{"FrontendDigest", "frontend digest", func(in *Inputs) { in.FrontendDigest = "sha256:changed" }},
		{"ContextKind", "context kind", func(in *Inputs) { in.ContextKind = "" }},
		{"ContextDigest", "context digest", func(in *Inputs) { in.ContextDigest = "sha256:changed" }},
		{"DockerfileKind", "dockerfile kind", func(in *Inputs) { in.DockerfileKind = "inline" }},
		{"DockerfileDigest", "dockerfile bytes", func(in *Inputs) {
			in.DockerfileDigest = "sha256:dddd"
		}},
		{"ContextSubpath", "context subpath", func(in *Inputs) { in.ContextSubpath = "other" }},
		{"Dockerfile", "dockerfile", func(in *Inputs) { in.Dockerfile = "build/Dockerfile" }},
		{"Target", "target", func(in *Inputs) { in.Target = "debug" }},
		{"Network", "network", func(in *Inputs) { in.Network = "None" }},
		{"CacheMode", "cache mode", func(in *Inputs) { in.CacheMode = "Disabled" }},
		{"CacheRef", "cache ref", func(in *Inputs) { in.CacheRef = "elsewhere" }},
		{"SourceDateEpoch", "epoch", func(in *Inputs) { in.SourceDateEpoch = "1700000000" }},
		// Missing until TestEveryFieldIsAccountedFor was written, which is the point of that test:
		// Attestations reached the hash and nothing proved it, so a refactor dropping it would have
		// left every existing object converged at a digest with no attestations and no record why.
		{"Attestations", "attestations", func(in *Inputs) { in.Attestations = "sbom+provenance" }},
		{"Platforms", "platform added", func(in *Inputs) { in.Platforms = append(in.Platforms, "linux/arm/v7") }},
		{"Platforms", "platform order", func(in *Inputs) { in.Platforms = []string{"linux/arm64", "linux/amd64"} }},
		{"Args", "arg value", func(in *Inputs) { in.Args["VERSION"] = "9.9.9" }},
		{"Args", "arg renamed", func(in *Inputs) { delete(in.Args, "VERSION"); in.Args["RELEASE"] = "1.2.3" }},
		{"Args", "arg added", func(in *Inputs) { in.Args["X"] = "y" }},
		{"SecretIdentities", "secret rotated", func(in *Inputs) { in.SecretIdentities = []string{"npmrc/5678"} }},
		{"SecretIdentities", "secret added", func(in *Inputs) {
			in.SecretIdentities = append(in.SecretIdentities, "other/1")
		}},
	}
}

func TestEveryFieldMovesTheHash(t *testing.T) {
	base := sampleInputs().Hash()

	for _, m := range hashMutations() {
		t.Run(m.name, func(t *testing.T) {
			in := sampleInputs()
			m.mutate(&in)
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

// TestEveryFieldIsAccountedFor closes the hole the list above leaves open.
//
// TestEveryFieldMovesTheHash enumerates cases by hand, so a field added to Inputs and never hashed
// still passes it — the test says nothing about fields nobody thought to list. That is exactly the
// failure this type cannot afford: for this kind the input hash IS the identity, so an unhashed
// input means a changed build quietly reusing an old artifact forever.
//
// Reflection over the struct instead. Adding a field to Inputs now fails here until someone decides,
// in writing, whether it belongs in the hash.
func TestEveryFieldIsAccountedFor(t *testing.T) {
	// Deliberately not hashed, each with the reason recorded on the field itself.
	notHashed := map[string]string{
		"ContextRevision": "the digest already identifies the content, so hashing both would " +
			"rebuild on a repack that changed nothing",
	}

	covered := map[string]bool{}
	for _, m := range hashMutations() {
		covered[m.field] = true
	}

	typ := reflect.TypeOf(Inputs{})
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		if _, ok := notHashed[name]; ok {
			if covered[name] {
				t.Errorf("Inputs.%s is listed as not hashed but has a case asserting it moves the "+
					"hash; one of the two is wrong", name)
			}
			continue
		}
		if !covered[name] {
			t.Errorf("Inputs.%s has no case in hashMutations, so nothing proves it reaches the "+
				"hash. Add one, or record it in notHashed with the reason.", name)
		}
	}
}
