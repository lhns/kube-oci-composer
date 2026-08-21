package attest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func samplePayloads() Payloads {
	return Payloads{
		BuildType: BuildTypeComposition,
		External:  map[string]any{"layers": []string{"config"}, "platforms": []string{"linux/amd64"}},
		Internal:  map[string]any{"assemblyVersion": 2},
		Sources: []Source{
			{Name: "config", Digest: "sha256:1111", Target: "/config"},
			{Name: "plugins", Digest: "sha256:2222", Version: "main@sha1:abcd", Target: "/plugins", URI: "https://example/p.tgz"},
		},
	}
}

// TestTheAttestationPayloadIsAPureFunctionOfItsInputs is the same constraint
// TestProvenanceIsDeterministic enforces one layer in, and it is worth restating why it outranks
// the feature: if the payload varied, "does this already exist" would stop being a comparison, the
// controller would re-push an attestation on every reconcile, and every artifact in the cluster
// would accumulate them.
func TestTheAttestationPayloadIsAPureFunctionOfItsInputs(t *testing.T) {
	p := samplePayloads()

	first, err := json.Marshal(SPDXStatement("registry/app", "sha256:aaaa", nil, p.Sources))
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(SPDXStatement("registry/app", "sha256:aaaa", nil, p.Sources))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("two SBOMs from identical inputs differ")
	}

	provA, err := json.Marshal(SLSAStatement("registry/app", "sha256:aaaa", p.BuildType, p.External, p.Internal, p.Sources))
	if err != nil {
		t.Fatal(err)
	}
	provB, err := json.Marshal(SLSAStatement("registry/app", "sha256:aaaa", p.BuildType, p.External, p.Internal, p.Sources))
	if err != nil {
		t.Fatal(err)
	}
	if string(provA) != string(provB) {
		t.Fatal("two provenance statements from identical inputs differ")
	}

	// The specific things that would make it vary, named so a future addition trips here.
	body := string(first) + string(provA)
	for _, forbidden := range []string{
		"invocationId", "startedOn", "finishedOn", // observations of the run, not of the artifact
		"buildStartedOn", "buildFinishedOn",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("%q is an observation of the run; it would make the payload differ every reconcile", forbidden)
		}
	}
	// The SBOM's namespace must be derived from the digest, not a UUID.
	if !strings.Contains(string(first), "sha256:aaaa") {
		t.Error("documentNamespace must be derived from the output digest")
	}
	if strings.Contains(string(first), "urn:uuid") {
		t.Error("a UUID namespace would change on every render")
	}
	// The epoch, for the same reason internal/oci stamps it.
	if !strings.Contains(string(first), "1970-01-01T00:00:00Z") {
		t.Error("creationInfo.created must be the epoch, not now")
	}
}

// TestTheSBOMRecordsTheRevisionRatherThanTheTarball — source-controller re-packs on restart, so a
// Flux tarball's digest moves while the revision it describes does not. The revision answers "what
// produced this"; the tarball digest does not. Same rule InputHash and the OCI annotations follow.
func TestTheSBOMRecordsTheRevisionRatherThanTheTarball(t *testing.T) {
	doc := SPDXDocument("registry/app", "sha256:aaaa", nil, samplePayloads().Sources)

	var found bool
	for _, p := range doc.Packages {
		if p.Name == "plugins" {
			found = true
			if p.VersionInfo != "main@sha1:abcd" {
				t.Errorf("versionInfo = %q, want the revision", p.VersionInfo)
			}
		}
	}
	if !found {
		t.Fatal("the layer is missing from the SBOM entirely")
	}
}

// TestTheSBOMKeepsSpecOrder — a later layer overwrites an earlier one, so the order carries
// meaning and sorting would discard it.
func TestTheSBOMKeepsSpecOrder(t *testing.T) {
	doc := SPDXDocument("registry/app", "sha256:aaaa", nil, []Source{
		{Name: "zzz", Digest: "sha256:1111"},
		{Name: "aaa", Digest: "sha256:2222"},
	})
	var layers []string
	for _, p := range doc.Packages {
		if strings.HasPrefix(p.SPDXID, "SPDXRef-Layer-") {
			layers = append(layers, p.Name)
		}
	}
	if len(layers) != 2 || layers[0] != "zzz" || layers[1] != "aaa" {
		t.Fatalf("layers must stay in spec order, got %v", layers)
	}
}

// TestASecondEnsureWritesNothing is the end-to-end statement of the idempotence design, and it
// counts REQUESTS rather than trusting that nothing looked different.
func TestASecondEnsureWritesNothing(t *testing.T) {
	repo, subject := pushArtifact(t)
	key, err := LoadKey(cosignKeySecret(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	a := &Attestor{SBOM: true, Provenance: true, Key: key}

	first, err := a.Ensure(context.Background(), repo, subject, samplePayloads(), nil)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	if first.SBOM == "" || first.Provenance == "" || first.Signature == "" {
		t.Fatalf("the first pass must attach all three, got %+v", first)
	}

	counter := &countingTransport{inner: remote.DefaultTransport}
	opts := []remote.Option{remote.WithTransport(counter)}

	second, err := a.Ensure(context.Background(), repo, subject, samplePayloads(), opts)
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}

	if counter.writes > 0 {
		t.Errorf("the second pass wrote %d times; it must attach nothing", counter.writes)
	}
	if second.SBOM != first.SBOM || second.Provenance != first.Provenance {
		t.Errorf("the referrer digests moved between passes:\n%+v\n%+v", first, second)
	}
	// cosign appends a layer per signature, so a re-sign would show as a changed manifest digest.
	if second.Signature != first.Signature {
		t.Errorf("the artifact was signed again: %q -> %q", first.Signature, second.Signature)
	}
}

// TestCompleteAnswersWithoutTouchingTheRegistry — layer one of the idempotence design. A converged
// reconcile must cost ZERO extra requests, which is only true if the status record alone can say
// "nothing to do".
func TestCompleteAnswersWithoutTouchingTheRegistry(t *testing.T) {
	full := &Attestor{SBOM: true, Provenance: true, Key: &Key{}}
	rec := &Record{Subject: "sha256:aaaa", SBOM: "sha256:1", Provenance: "sha256:2", Signature: "sha256:3"}

	if !full.Complete(rec, "sha256:aaaa") {
		t.Error("a complete record must satisfy the check")
	}
	// A different artifact invalidates the whole record.
	if full.Complete(rec, "sha256:bbbb") {
		t.Error("a record for another subject must not count")
	}
	// A newly enabled feature must be noticed.
	if full.Complete(&Record{Subject: "sha256:aaaa", SBOM: "sha256:1"}, "sha256:aaaa") {
		t.Error("a record missing the provenance and signature must not count as complete")
	}
	// And a disabled Attestor is trivially complete, so nothing changes for anyone not using this.
	if !(&Attestor{}).Complete(nil, "sha256:aaaa") {
		t.Error("with nothing enabled there is nothing to do")
	}
}

// TestSigningTurnsTheAttestationIntoAnEnvelope — the two switches stay independent: with a key the
// statement carries its own signature, without one it is a bare statement, which is the honest
// shape for unsigned facts.
func TestSigningTurnsTheAttestationIntoAnEnvelope(t *testing.T) {
	key, err := LoadKey(cosignKeySecret(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := json.Marshal(SPDXStatement("registry/app", "sha256:aaaa", nil, nil))
	if err != nil {
		t.Fatal(err)
	}

	env, err := Envelope(key, stmt)
	if err != nil {
		t.Fatalf("wrapping: %v", err)
	}
	var e struct {
		Payload     string `json:"payload"`
		PayloadType string `json:"payloadType"`
		Signatures  []struct {
			Sig string `json:"sig"`
		} `json:"signatures"`
	}
	if err := json.Unmarshal(env, &e); err != nil {
		t.Fatalf("the envelope is not DSSE: %v", err)
	}
	if e.PayloadType != MediaTypeInToto {
		t.Errorf("payloadType = %q", e.PayloadType)
	}
	if len(e.Signatures) != 1 || e.Signatures[0].Sig == "" {
		t.Error("the envelope must carry a signature")
	}
}
