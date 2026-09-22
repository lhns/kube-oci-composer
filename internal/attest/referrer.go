package attest

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// The empty config every OCI artifact manifest points at, as the image specification defines it.
const (
	emptyConfigMediaType = "application/vnd.oci.empty.v1+json"
	emptyConfigDigest    = "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
)

var emptyConfigBody = []byte("{}")

// emptyConfig is the descriptor of the empty config.
func emptyConfig() (v1.Descriptor, error) {
	digest, err := v1.NewHash(emptyConfigDigest)
	if err != nil {
		return v1.Descriptor{}, err
	}
	return v1.Descriptor{
		MediaType: types.MediaType(emptyConfigMediaType),
		Digest:    digest,
		Size:      int64(len(emptyConfigBody)),
	}, nil
}

// writeEmptyConfig uploads the empty config blob.
func writeEmptyConfig(repo name.Repository, opts []remote.Option) error {
	return remote.WriteLayer(repo, static.NewLayer(emptyConfigBody, types.MediaType(emptyConfigMediaType)), opts...)
}

// artifactManifest is what an attestation looks like on the wire: an OCI image manifest whose
// `subject` names the artifact it describes.
type artifactManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType"`
	Config        v1.Descriptor     `json:"config"`
	Layers        []v1.Descriptor   `json:"layers"`
	Subject       *v1.Descriptor    `json:"subject,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// taggable adapts raw manifest bytes to what remote.Put wants.
type taggable struct {
	raw       []byte
	mediaType types.MediaType
}

func (t taggable) RawManifest() ([]byte, error) { return t.raw, nil }
func (t taggable) MediaType() (types.MediaType, error) {
	return t.mediaType, nil
}

// Push attaches one predicate to an artifact as an OCI referrer.
//
// go-containerregistry reads the manifest's subject and maintains the referrers fallback tag
// itself, so this works with or without the Referrers API. Returns the referrer's own digest, for
// the caller's status record.
func Push(repo name.Repository, subject v1.Descriptor, predicateType string, payload []byte, signed bool, opts []remote.Option) (v1.Hash, error) {
	layerMediaType := MediaTypeInToto
	if signed {
		layerMediaType = MediaTypeDSSE
	}

	layer := static.NewLayer(payload, types.MediaType(layerMediaType))
	layerDigest, err := layer.Digest()
	if err != nil {
		return v1.Hash{}, fmt.Errorf("digesting the attestation: %w", err)
	}
	layerSize, err := layer.Size()
	if err != nil {
		return v1.Hash{}, fmt.Errorf("sizing the attestation: %w", err)
	}

	config, err := emptyConfig()
	if err != nil {
		return v1.Hash{}, err
	}

	mf := artifactManifest{
		SchemaVersion: 2,
		MediaType:     string(types.OCIManifestSchema1),
		ArtifactType:  MediaTypeInToto,
		Config:        config,
		Layers: []v1.Descriptor{{
			MediaType:   types.MediaType(layerMediaType),
			Digest:      layerDigest,
			Size:        layerSize,
			Annotations: map[string]string{AnnotationPredicateType: predicateType},
		}},
		Subject: &subject,
		// On the manifest as well as the layer: a referrers index copies the manifest's
		// annotations, not its layers', so this is what Existing and other consumers filter on.
		Annotations: map[string]string{AnnotationPredicateType: predicateType},
	}

	raw, err := json.Marshal(mf)
	if err != nil {
		return v1.Hash{}, fmt.Errorf("encoding the attestation manifest: %w", err)
	}

	// Blobs first: a registry rejects a manifest whose blobs it lacks.
	if err := remote.WriteLayer(repo, layer, opts...); err != nil {
		return v1.Hash{}, fmt.Errorf("pushing the attestation payload: %w", err)
	}
	if err := writeEmptyConfig(repo, opts); err != nil {
		return v1.Hash{}, fmt.Errorf("pushing the empty config: %w", err)
	}

	digest, _, err := v1.SHA256(bytes.NewReader(raw))
	if err != nil {
		return v1.Hash{}, err
	}
	ref := repo.Digest(digest.String())
	if err := remote.Put(ref, taggable{raw: raw, mediaType: types.OCIManifestSchema1}, opts...); err != nil {
		return v1.Hash{}, fmt.Errorf("pushing the attestation manifest: %w", err)
	}
	// Also tagged after its own digest (ADR 0060): untagged content is reclaimed by age once
	// keepUntagged is off, however often the refresher pulls it.
	own := repo.Tag(OwnTag(digest))
	if err := remote.Put(own, taggable{raw: raw, mediaType: types.OCIManifestSchema1}, opts...); err != nil {
		return v1.Hash{}, fmt.Errorf("naming the attestation manifest: %w", err)
	}
	return digest, nil
}

// Existing lists the predicate types already attached to an artifact, and the manifest digest of
// each.
//
// One request covers every predicate, with or without the Referrers API.
func Existing(repo name.Repository, subject v1.Hash, opts []remote.Option) (map[string]v1.Hash, error) {
	idx, err := remote.Referrers(repo.Digest(subject.String()), opts...)
	if err != nil {
		return nil, fmt.Errorf("listing referrers: %w", err)
	}
	mf, err := idx.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("reading the referrers index: %w", err)
	}

	out := map[string]v1.Hash{}
	for _, d := range mf.Manifests {
		if pt := d.Annotations[AnnotationPredicateType]; pt != "" {
			out[pt] = d.Digest
			continue
		}
		// Some registries (go-containerregistry's own) drop annotations from the referrers
		// listing, so read them from the manifest. One extra GET, on a rarely taken path.
		if d.ArtifactType != MediaTypeInToto {
			continue
		}
		desc, err := remote.Get(repo.Digest(d.Digest.String()), opts...)
		if err != nil {
			return nil, fmt.Errorf("reading referrer %s: %w", d.Digest, err)
		}
		var m struct {
			Annotations map[string]string `json:"annotations"`
		}
		if err := json.Unmarshal(desc.Manifest, &m); err != nil {
			return nil, fmt.Errorf("parsing referrer %s: %w", d.Digest, err)
		}
		if pt := m.Annotations[AnnotationPredicateType]; pt != "" {
			out[pt] = d.Digest
		}
	}
	return out, nil
}
