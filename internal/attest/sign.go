package attest

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/secure-systems-lab/go-securesystemslib/encrypted"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/sigstore/sigstore/pkg/signature/payload"
	corev1 "k8s.io/api/core/v1"
)

// Signatures use cosign's sha256-<hex>.sig TAG convention, not the Referrers API (amending ADR
// 0008): the verifiers that exist (policy-controller, Kyverno, Connaisseur) read the .sig tag by
// default, and a signature nothing checks is theatre (ADR 0020). As a tag, it is also covered by
// the registry's keepTags policy. Attestations have no verifier constraint, so they use referrers.

const (
	// SignatureLayerMediaType is cosign's SimpleSigning payload type.
	SignatureLayerMediaType = "application/vnd.dev.cosign.simplesigning.v1+json"
	// SignatureAnnotation carries the base64 signature over the payload bytes.
	SignatureAnnotation = "dev.cosignproject.cosign/signature"

	// The field names `cosign generate-key-pair` writes, so an operator's Secret works unmodified.
	KeySecretKey      = "cosign.key"
	PasswordSecretKey = "cosign.password"
	PublicSecretKey   = "cosign.pub"
)

// Key signs artifacts. Loaded once at startup, in the CONTROLLER's process: the key is never
// projected into a build pod, so built code never runs alongside it.
type Key struct {
	signer signature.SignerVerifier
}

// LoadKey reads a cosign key pair from a Secret.
//
// The passphrase sits in the same Secret as the key, so it adds no protection against a reader of
// the Secret; it exists for compatibility with `cosign generate-key-pair k8s://<ns>/<name>`.
func LoadKey(secret *corev1.Secret) (*Key, error) {
	raw, ok := secret.Data[KeySecretKey]
	if !ok {
		return nil, fmt.Errorf("secret has no %q; create it with `cosign generate-key-pair k8s://<namespace>/<name>`", KeySecretKey)
	}

	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("cosign.key is not a PEM block")
	}
	plain, err := encrypted.Decrypt(block.Bytes, secret.Data[PasswordSecretKey])
	if err != nil {
		return nil, fmt.Errorf("decrypting the signing key (wrong %s?): %w", PasswordSecretKey, err)
	}
	priv, err := cryptoutils.UnmarshalPEMToPrivateKey(plain, cryptoutils.SkipPassword)
	if err != nil {
		// cosign stores the decrypted key as raw PKCS#8 DER rather than PEM in some versions.
		priv, err = cryptoutils.UnmarshalPEMToPrivateKey(pem.EncodeToMemory(&pem.Block{
			Type: "PRIVATE KEY", Bytes: plain,
		}), cryptoutils.SkipPassword)
		if err != nil {
			return nil, fmt.Errorf("parsing the signing key: %w", err)
		}
	}
	ecKey, ok := priv.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("the signing key is %T; cosign uses ECDSA-P256", priv)
	}
	sv, err := signature.LoadECDSASignerVerifier(ecKey, crypto.SHA256)
	if err != nil {
		return nil, fmt.Errorf("building the signer: %w", err)
	}
	pub, err := cryptoutils.MarshalPublicKeyToPEM(ecKey.Public())
	if err != nil {
		return nil, fmt.Errorf("marshalling the public key: %w", err)
	}

	k := &Key{signer: sv}
	// Fail at boot, not at the first artifact.
	if err := k.selfCheck(); err != nil {
		return nil, err
	}
	if declared, ok := secret.Data[PublicSecretKey]; ok &&
		strings.TrimSpace(string(declared)) != strings.TrimSpace(string(pub)) {
		return nil, fmt.Errorf("%s does not match the public half of %s; the Secret holds a mismatched pair",
			PublicSecretKey, KeySecretKey)
	}
	return k, nil
}

func (k *Key) selfCheck() error {
	const probe = "kube-oci-composer signing self-check"
	sig, err := k.signer.SignMessage(strings.NewReader(probe))
	if err != nil {
		return fmt.Errorf("the signing key cannot sign: %w", err)
	}
	if err := k.signer.VerifySignature(strings.NewReader(string(sig)), strings.NewReader(probe)); err != nil {
		return fmt.Errorf("the signing key produced a signature it cannot verify: %w", err)
	}
	return nil
}

// Sign returns a detached signature over an arbitrary payload, used for DSSE envelopes.
func (k *Key) Sign(payload []byte) ([]byte, error) {
	return k.signer.SignMessage(strings.NewReader(string(payload)))
}

// SignArtifact writes a cosign-compatible signature for one digest.
//
// Only the TOP-LEVEL digest (the index for a multi-platform artifact) is signed: that is what
// status.artifact.digest reports and consumers pin, and no verifier asks for per-child signatures.
func (k *Key) SignArtifact(ctx context.Context, repo name.Repository, digest v1.Hash, opts []remote.Option) (v1.Hash, error) {
	body, err := k.signedPayload(repo, digest)
	if err != nil {
		return v1.Hash{}, err
	}
	sig, err := k.Sign(body)
	if err != nil {
		return v1.Hash{}, fmt.Errorf("signing: %w", err)
	}

	layer := static.NewLayer(body, SignatureLayerMediaType)
	layerDigest, err := layer.Digest()
	if err != nil {
		return v1.Hash{}, err
	}
	layerSize, err := layer.Size()
	if err != nil {
		return v1.Hash{}, err
	}
	config, err := emptyConfig()
	if err != nil {
		return v1.Hash{}, err
	}

	mf := artifactManifest{
		SchemaVersion: 2,
		MediaType:     string(types.OCIManifestSchema1),
		Config:        config,
		Layers: []v1.Descriptor{{
			MediaType:   SignatureLayerMediaType,
			Digest:      layerDigest,
			Size:        layerSize,
			Annotations: map[string]string{SignatureAnnotation: base64.StdEncoding.EncodeToString(sig)},
		}},
	}
	rawManifest, err := json.Marshal(mf)
	if err != nil {
		return v1.Hash{}, err
	}

	if err := remote.WriteLayer(repo, layer, opts...); err != nil {
		return v1.Hash{}, fmt.Errorf("pushing the signature payload: %w", err)
	}
	if err := writeEmptyConfig(repo, opts); err != nil {
		return v1.Hash{}, fmt.Errorf("pushing the empty config: %w", err)
	}
	if err := remote.Put(repo.Tag(SignatureTag(digest)), taggable{raw: rawManifest, mediaType: types.OCIManifestSchema1}, opts...); err != nil {
		return v1.Hash{}, fmt.Errorf("pushing the signature: %w", err)
	}

	h, _, err := v1.SHA256(strings.NewReader(string(rawManifest)))
	return h, err
}

// signedPayload is the canonical cosign SimpleSigning body, built with sigstore's own marshaller.
func (k *Key) signedPayload(repo name.Repository, digest v1.Hash) ([]byte, error) {
	ref, err := name.NewDigest(repo.Name() + "@" + digest.String())
	if err != nil {
		return nil, fmt.Errorf("building the signed reference: %w", err)
	}
	return payload.Cosign{Image: ref}.MarshalJSON()
}

// SignatureTag is cosign's convention for where a signature lives.
func SignatureTag(digest v1.Hash) string {
	return strings.Replace(digest.String(), ":", "-", 1) + ".sig"
}

// OwnTag names a manifest after its own digest: "digest-<hex>". The same string as
// reconciler.DigestTag, repeated rather than imported so this package stays free of the
// controller's dependencies; see there for why it is not "sha256-<hex>". ADR 0060.
func OwnTag(digest v1.Hash) string {
	return "digest-" + digest.Hex
}

// VerifiedSignatureExists reports whether a signature THIS KEY made is already attached.
//
// Verification rather than comparison, because ECDSA signatures are randomised. After a key
// rotation, the old key's signature reads as absent, so a new one gets written.
func (k *Key) VerifiedSignatureExists(ctx context.Context, repo name.Repository, digest v1.Hash, opts []remote.Option) (bool, v1.Hash, error) {
	desc, err := remote.Get(repo.Tag(SignatureTag(digest)), opts...)
	if err != nil {
		// Absent is the ordinary case: it means "sign it".
		return false, v1.Hash{}, nil
	}

	var mf struct {
		Layers []struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(desc.Manifest, &mf); err != nil {
		return false, v1.Hash{}, fmt.Errorf("parsing the existing signature: %w", err)
	}

	want, err := k.signedPayload(repo, digest)
	if err != nil {
		return false, v1.Hash{}, err
	}
	for _, l := range mf.Layers {
		raw, ok := l.Annotations[SignatureAnnotation]
		if !ok {
			continue
		}
		sig, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			continue
		}
		if err := k.signer.VerifySignature(strings.NewReader(string(sig)), strings.NewReader(string(want))); err == nil {
			return true, desc.Digest, nil
		}
	}
	return false, desc.Digest, nil
}
