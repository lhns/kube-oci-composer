package build

import (
	"reflect"
	"testing"
)

func sampleInputs() Inputs {
	return Inputs{
		BuilderDigest:    "sha256:aaaa",
		FrontendDigest:   "sha256:bbbb",
		FetcherDigest:    "sha256:ffff",
		ContextKind:      "sourceRef",
		ContextUnpack:    "tar.gz",
		ContextDigest:    "sha256:cccc",
		ContextSubpath:   "src",
		ContextStrip:     2,
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

// TestHashIsStable: the short-circuit depends on it.
func TestHashIsStable(t *testing.T) {
	want := sampleInputs().Hash()
	for i := range 5 {
		if got := sampleInputs().Hash(); got != want {
			t.Fatalf("call %d gave %s, want %s", i, got, want)
		}
	}
}

// hashMutation is one "change this and the hash must move" case; TestEveryFieldIsAccountedFor
// checks the list covers every field.
type hashMutation struct {
	// field names the Inputs field this case exercises.
	field  string
	name   string
	mutate func(*Inputs)
}

func hashMutations() []hashMutation {
	return []hashMutation{
		{"BuilderDigest", "builder digest", func(in *Inputs) { in.BuilderDigest = "sha256:changed" }},
		{"FrontendDigest", "frontend digest", func(in *Inputs) { in.FrontendDigest = "sha256:changed" }},
		{"FetcherDigest", "fetcher digest", func(in *Inputs) { in.FetcherDigest = "sha256:changed" }},
		{"ContextKind", "context kind", func(in *Inputs) { in.ContextKind = "fetch" }},
		{"ContextUnpack", "context unpack", func(in *Inputs) { in.ContextUnpack = "tar" }},
		{"ContextDigest", "context digest", func(in *Inputs) { in.ContextDigest = "sha256:changed" }},
		{"DockerfileKind", "dockerfile kind", func(in *Inputs) { in.DockerfileKind = "inline" }},
		{"DockerfileDigest", "dockerfile bytes", func(in *Inputs) {
			in.DockerfileDigest = "sha256:dddd"
		}},
		{"ContextSubpath", "context subpath", func(in *Inputs) { in.ContextSubpath = "other" }},
		{"ContextStrip", "context strip depth", func(in *Inputs) { in.ContextStrip = 1 }},
		{"Dockerfile", "dockerfile", func(in *Inputs) { in.Dockerfile = "build/Dockerfile" }},
		{"Target", "target", func(in *Inputs) { in.Target = "debug" }},
		{"Network", "network", func(in *Inputs) { in.Network = "None" }},
		{"CacheMode", "cache mode", func(in *Inputs) { in.CacheMode = "Disabled" }},
		{"CacheRef", "cache ref", func(in *Inputs) { in.CacheRef = "elsewhere" }},
		{"SourceDateEpoch", "epoch", func(in *Inputs) { in.SourceDateEpoch = "1700000000" }},
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

// TestEveryFieldMovesTheHash: a field that does not move the hash can change the output without a
// rebuild.
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

// TestArgOrderDoesNotMatter: ARG order does not change what the build sees.
func TestArgOrderDoesNotMatter(t *testing.T) {
	a, b := sampleInputs(), sampleInputs()
	b.Args = map[string]string{"COMMIT": "abc", "VERSION": "1.2.3"}
	if a.Hash() != b.Hash() {
		t.Error("reordering args rebuilt; the spec means the same thing either way")
	}
}

// TestSecretValuesAreNotHashed: identity plus resourceVersion rebuilds on rotation without making
// status.inputHash an oracle for the value.
func TestSecretValuesAreNotHashed(t *testing.T) {
	// The struct has nowhere to put a value; this asserts the behaviour that depends on that.
	rotated := sampleInputs()
	rotated.SecretIdentities = []string{"npmrc/1235"}
	if rotated.Hash() == sampleInputs().Hash() {
		t.Error("a rotated secret did not rebuild")
	}
}

// TestEveryFieldIsAccountedFor fails when a field is added to Inputs until it has a hashMutation
// case or a recorded reason in notHashed. For this kind the input hash is the identity.
func TestEveryFieldIsAccountedFor(t *testing.T) {
	// Deliberately not hashed; the reason is also on the field.
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
