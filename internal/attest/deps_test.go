package attest

import (
	"os"
	"strings"
	"testing"
)

// TestTheSigstoreTreeStaysSmall is a drift guard, in the shape of
// TestBuilderChartNeverGrantsSecretListOrWatch and for the same reason: the property it protects is
// invisible until someone imports the convenient helper.
//
// Signing needs about seventy-five lines of sigstore — a signer, a PEM decoder, and cosign's
// SimpleSigning payload marshaller. Each of the modules below would arrive by importing something
// that looks reasonable:
//
//   - sigstore/cosign/v2: the obvious import for "sign a container image". Drags rekor, fulcio,
//     go-tuf, go-jose, coreos/go-oidc, grpc and cloud SDKs — on the order of 150-250 modules, into
//     two binaries that run in-cluster.
//   - sigstore/sigstore/pkg/signature/kms/{aws,gcp,azure,hashivault}: subdirectories of a module
//     we already use, each pulling a whole cloud SDK.
//   - sigstore/sigstore/pkg/oauthflow: pulls go-rod, a headless-Chrome driver. Into a Kubernetes
//     controller. This is the most surprising one in the tree and the reason this test names
//     specific modules rather than counting them.
//   - rekor / fulcio / go-tuf: keyless infrastructure, which ADR 0008 rejected outright because it
//     would publish private image names and digests to a public transparency log.
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

	// The control. If the tree the guard protects is not present at all, every case above passes
	// vacuously and the test says nothing.
	for _, want := range []string{
		"github.com/sigstore/sigstore ",
		"github.com/secure-systems-lab/go-securesystemslib",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("%s is missing; this guard would pass while protecting nothing", want)
		}
	}
}
