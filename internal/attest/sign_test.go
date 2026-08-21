package attest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/secure-systems-lab/go-securesystemslib/encrypted"
)

// cosignKeySecret builds a Secret in exactly the shape `cosign generate-key-pair k8s://ns/name`
// produces: an encrypted PKCS#8 key in a PEM block, its passphrase, and the public half.
func cosignKeySecret(t *testing.T, password string) *corev1.Secret {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshalling the key: %v", err)
	}
	sealed, err := encrypted.Encrypt(der, []byte(password))
	if err != nil {
		t.Fatalf("encrypting the key: %v", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		t.Fatalf("marshalling the public key: %v", err)
	}

	return &corev1.Secret{
		Data: map[string][]byte{
			KeySecretKey:      pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED SIGSTORE PRIVATE KEY", Bytes: sealed}),
			PasswordSecretKey: []byte(password),
			PublicSecretKey:   pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}),
		},
	}
}

// TestASignatureLandsWhereAVerifierLooks. The tag convention is the whole reason signatures do not
// use referrers: policy-controller, Kyverno and Connaisseur read `sha256-<hex>.sig` by default, and
// a signature on the elegant rail is a signature nothing checks — the exact failure ADR 0020 named.
func TestASignatureLandsWhereAVerifierLooks(t *testing.T) {
	repo, subject := pushArtifact(t)
	key, err := LoadKey(cosignKeySecret(t, "hunter2"))
	if err != nil {
		t.Fatalf("loading the key: %v", err)
	}

	if _, err := key.SignArtifact(context.Background(), repo, subject.Digest, nil); err != nil {
		t.Fatalf("signing: %v", err)
	}

	tag := SignatureTag(subject.Digest)
	if !strings.HasSuffix(tag, ".sig") || !strings.HasPrefix(tag, "sha256-") {
		t.Fatalf("the signature tag must follow cosign's convention, got %q", tag)
	}

	ok, _, err := key.VerifiedSignatureExists(context.Background(), repo, subject.Digest, nil)
	if err != nil {
		t.Fatalf("looking for the signature: %v", err)
	}
	if !ok {
		t.Fatal("the signature this key just wrote does not verify under the same key")
	}
}

// TestTheSignedPayloadIsTheCanonicalCosignOne. Built with sigstore's own marshaller rather than by
// hand, which is the difference between "compatible" and "compatible as far as we could tell from
// the docs". This asserts the shape a verifier actually parses.
func TestTheSignedPayloadIsTheCanonicalCosignOne(t *testing.T) {
	repo, subject := pushArtifact(t)
	key, err := LoadKey(cosignKeySecret(t, ""))
	if err != nil {
		t.Fatalf("loading the key: %v", err)
	}

	body, err := key.signedPayload(repo, subject.Digest)
	if err != nil {
		t.Fatalf("building the payload: %v", err)
	}

	var p struct {
		Critical struct {
			Identity struct {
				DockerReference string `json:"docker-reference"`
			} `json:"identity"`
			Image struct {
				DockerManifestDigest string `json:"docker-manifest-digest"`
			} `json:"image"`
			Type string `json:"type"`
		} `json:"critical"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("the payload is not the SimpleSigning shape: %v\n%s", err, body)
	}
	if p.Critical.Type != "cosign container image signature" {
		t.Errorf("critical.type = %q", p.Critical.Type)
	}
	if p.Critical.Image.DockerManifestDigest != subject.Digest.String() {
		t.Errorf("the payload names %q, not the artifact digest %q",
			p.Critical.Image.DockerManifestDigest, subject.Digest)
	}
	if !strings.Contains(p.Critical.Identity.DockerReference, "team-a/app") {
		t.Errorf("the payload must name the repository, got %q", p.Critical.Identity.DockerReference)
	}
}

// TestAnEmptyPassphraseWorks — cosign supports one, and refusing it would mean rejecting keys the
// documented command can produce.
func TestAnEmptyPassphraseWorks(t *testing.T) {
	if _, err := LoadKey(cosignKeySecret(t, "")); err != nil {
		t.Fatalf("an empty passphrase must be accepted: %v", err)
	}
}

// TestAMismatchedKeyPairIsRefusedAtLoad. A Secret holding one key's private half and another's
// public half would sign happily and produce signatures nothing could verify with the public key
// the operator handed their admission policy. Caught at startup, not at the first artifact.
func TestAMismatchedKeyPairIsRefusedAtLoad(t *testing.T) {
	secret := cosignKeySecret(t, "hunter2")
	other := cosignKeySecret(t, "hunter2")
	secret.Data[PublicSecretKey] = other.Data[PublicSecretKey]

	if _, err := LoadKey(secret); err == nil {
		t.Fatal("a mismatched pair must be refused")
	} else if !strings.Contains(err.Error(), "mismatched") {
		t.Errorf("the error should say what is wrong: %v", err)
	}
}

// TestTheWrongPassphraseIsRefused — the failure must name the field, because the alternative is an
// operator staring at a decryption error with no idea which of two Secrets is wrong.
func TestTheWrongPassphraseIsRefused(t *testing.T) {
	secret := cosignKeySecret(t, "hunter2")
	secret.Data[PasswordSecretKey] = []byte("wrong")

	_, err := LoadKey(secret)
	if err == nil {
		t.Fatal("the wrong passphrase must be refused")
	}
	if !strings.Contains(err.Error(), PasswordSecretKey) {
		t.Errorf("the error should name %s: %v", PasswordSecretKey, err)
	}
}

// TestASignatureFromAnotherKeyReadsAsAbsent is what makes key rotation work.
//
// Verification rather than comparison, because ECDSA is randomised. The useful consequence: after a
// rotation, the old key's signature does not satisfy the new key, so a new signature gets written
// instead of the old one being silently accepted forever.
func TestASignatureFromAnotherKeyReadsAsAbsent(t *testing.T) {
	repo, subject := pushArtifact(t)

	old, err := LoadKey(cosignKeySecret(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.SignArtifact(context.Background(), repo, subject.Digest, nil); err != nil {
		t.Fatalf("signing with the old key: %v", err)
	}

	rotated, err := LoadKey(cosignKeySecret(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	ok, _, err := rotated.VerifiedSignatureExists(context.Background(), repo, subject.Digest, nil)
	if err != nil {
		t.Fatalf("checking: %v", err)
	}
	if ok {
		t.Fatal("a signature from a different key must not count as this key's signature")
	}
}
