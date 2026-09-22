package attest

import (
	"os"
	"strings"
	"testing"
)

// TestTheSigstoreTreeStaysSmall is a drift guard: signing needs only a signer, a PEM decoder and
// cosign's payload marshaller, but reasonable-looking imports (cosign itself, sigstore's kms or
// oauthflow subpackages, keyless rekor/fulcio/go-tuf, rejected by ADR 0008) drag in hundreds of
// modules. The reasons are in the table below.
func TestTheSigstoreTreeStaysSmall(t *testing.T) {
	raw, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	mod := string(raw)

	banned := []struct{ module, why string }{
		{"sigstore/cosign", "150-250 modules for ~75 lines of code; the format is small and fixed"},
		{"sigstore/rekor", "keyless infrastructure; ADR 0008 rejected keyless because it publishes private image names to a public log"},
		{"sigstore/fulcio", "same as rekor"},
		{"theupdateframework/go-tuf", "keyless root distribution, not needed for key-based signing"},
		{"go-rod/rod", "a headless-Chrome driver, arriving through sigstore's oauthflow package"},
		{"aws/aws-sdk-go", "a cloud KMS SDK, arriving through sigstore's kms subpackages"},
		{"cloud.google.com/go/kms", "same, for GCP"},
		{"Azure/azure-sdk-for-go", "same, for Azure"},
		{"spdx/tools-golang", "its struct tags decide our payload bytes, so a minor bump would re-attest every artifact in the cluster"},
		{"CycloneDX/cyclonedx-go", "same, and we emit SPDX to match BuildKit"},
		{"in-toto/in-toto-golang", "same; the statement is twenty lines of structs whose stability should be ours"},
	}

	for _, b := range banned {
		if strings.Contains(mod, b.module) {
			t.Errorf("go.mod now depends on %s.\nWhy that matters: %s", b.module, b.why)
		}
	}

	// The control: without these, every case above passes vacuously.
	for _, want := range []string{
		"github.com/sigstore/sigstore ",
		"github.com/secure-systems-lab/go-securesystemslib",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("%s is missing; this guard would pass while protecting nothing", want)
		}
	}
}
