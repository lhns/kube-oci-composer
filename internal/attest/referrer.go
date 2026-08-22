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

// The empty config every OCI artifact manifest points at. Fixed bytes, fixed digest, defined by the
// image specification -- so it is a constant rather than something computed.
const (
	emptyConfigMediaType = "application/vnd.oci.empty.v1+json"
	emptyConfigDigest    = "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
)

var emptyConfigBody = []byte("{}")

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
// go-containerregistry does the awkward part: `commitManifest` reads the `subject` out of the raw
// manifest and maintains the referrers fallback tag itself, so this works against a registry with
// the Referrers API and one without. zot has it (dist-spec 1.1), which is what the chart declares.
//
// Returns the referrer's own digest, which the caller records in status so the next reconcile can
// tell there is nothing to do without asking the registry.
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

	configDigest, err := v1.NewHash(emptyConfigDigest)
	if err != nil {
		return v1.Hash{}, err
	}

	mf := artifactManifest{
		SchemaVersion: 2,
		MediaType:     string(types.OCIManifestSchema1),
		ArtifactType:  MediaTypeInToto,
		Config: v1.Descriptor{
			MediaType: types.MediaType(emptyConfigMediaType),
			Digest:    configDigest,
			Size:      int64(len(emptyConfigBody)),
		},
		Layers: []v1.Descriptor{{
			MediaType:   types.MediaType(layerMediaType),
			Digest:      layerDigest,
			Size:        layerSize,
			Annotations: map[string]string{AnnotationPredicateType: predicateType},
		}},
		Subject: &subject,
		// The predicate type appears on the MANIFEST as well as on its layer, and that is not
		// duplication. A referrers index lists descriptors carrying the referring manifest's
		// artifactType and annotations -- not its layers -- so a consumer filtering by predicate
		// (including Existing below) sees only what is here. The layer annotation is for anyone
		// who has already fetched the manifest.
		Annotations: map[string]string{AnnotationPredicateType: predicateType},
	}

	raw, err := json.Marshal(mf)
	if err != nil {
		return v1.Hash{}, fmt.Errorf("encoding the attestation manifest: %w", err)
	}

	// Blobs first: a manifest referencing a layer the registry does not have is rejected, and the
	// order matters more here than usual because a partial push would leave a referrer that
	// resolves to nothing.
	if err := remote.WriteLayer(repo, layer, opts...); err != nil {
		return v1.Hash{}, fmt.Errorf("pushing the attestation payload: %w", err)
	}
	if err := remote.WriteLayer(repo, static.NewLayer(emptyConfigBody, types.MediaType(emptyConfigMediaType)), opts...); err != nil {
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
	return digest, nil
}

// Existing lists the predicate types already attached to an artifact, and the manifest digest of
// each.
//
// ONE request for every predicate, which is what keeps the reconciliation path cheap when the
// status record cannot answer. Works whether the registry implements the Referrers API or only the
// fallback tag -- ggcr decides.
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
		// The index descriptor did not carry the annotation, so fetch the manifest and read it
		// there.
		//
		// Not a hypothetical: registries differ about what they copy into a referrers listing.
		// go-containerregistry's own in-memory registry propagates `artifactType` and DROPS
		// `annotations`, which is how this was found -- the first version filtered on the
		// descriptor alone, found nothing, and would have re-pushed both predicates on every
		// reconcile forever while looking like it was working.
		//
		// One extra GET per referrer, on a path that runs only when the status record cannot
		// answer. See Attestor.Ensure for why that is rare.
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
