package oci

import (
	"fmt"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// Threat-model gap R1: an artifact exists and nobody can say what produced it.
//
// `status.history[].sources` already records each layer's name, resolved digest and revision --
// which is what ADR 0026's incident needed and did not have, since it had to be diagnosed by
// extracting a layer and reading its payload. What was still missing is that the record lives in
// the OBJECT, so deleting the ImageComposition takes it with it, while the image it produced is
// still running somewhere.
//
// Annotations put it in the artifact. They are the right carrier rather than config labels for one
// specific reason: labels are part of the image CONFIG, so writing them changes the config digest
// and therefore what every consumer's `docker inspect` reports as the image's own labels --
// provenance masquerading as application metadata. Manifest annotations are metadata about the
// manifest, which is exactly what this is.
//
// Everything written here is a pure function of the resolved inputs, because determinism is the
// project's core invariant (ADR 0016) and an annotation carrying a timestamp or a hostname would
// end it. That is also why `org.opencontainers.image.created` is NOT set: the config already
// carries the epoch, and a real build time would make two identical specs produce two digests.
const (
	// AnnotationSources lists each layer as `name=digest` (or `name=revision` where the revision
	// is what identifies the content), separated by spaces, in spec order. Spec order rather than
	// sorted: the order layers are applied is semantically meaningful -- a later layer overwrites
	// an earlier one -- so re-ordering it would be discarding information.
	AnnotationSources = "de.lhns.oci-composer.sources"

	// AnnotationAssemblyVersion records the algorithm that produced these bytes, so an artifact
	// found in a registry can be matched against the code that made it.
	AnnotationAssemblyVersion = "de.lhns.oci-composer.assembly-version"

	// AnnotationBase names the base image's digest, or is absent for a scratch artifact. It is not
	// derivable from the layers: base layers are reused verbatim, so nothing in the output says
	// which image they came from.
	AnnotationBase = "de.lhns.oci-composer.base"
)

// provenanceAnnotations describes the inputs an assembly consumed.
func provenanceAnnotations(base v1.Image, inputs []LayerInput) map[string]string {
	ann := map[string]string{
		AnnotationAssemblyVersion: fmt.Sprintf("%d", AssemblyVersion),
	}

	parts := make([]string, 0, len(inputs))
	for _, in := range inputs {
		// Identity when it is set, for the same reason InputHash prefers it: a Flux artifact's
		// tarball digest changes when source-controller re-packs, while the revision it describes
		// does not. The revision is the answer to "what produced this"; the tarball digest is not.
		id := in.Identity
		if id == "" {
			id = in.Digest
		}
		if id == "" {
			continue
		}
		parts = append(parts, in.Name+"="+id)
	}
	if len(parts) > 0 {
		ann[AnnotationSources] = strings.Join(parts, " ")
	}

	if base != nil {
		if d, err := base.Digest(); err == nil {
			ann[AnnotationBase] = d.String()
		}
		// A base whose digest cannot be read is not worth failing an assembly over: the artifact
		// is correct, and one absent annotation is a smaller loss than a build that does not
		// happen. It cannot go unnoticed either -- the base digest is in status.
	}
	return ann
}

// withProvenance stamps the annotations onto the manifest.
func withProvenance(img v1.Image, base v1.Image, inputs []LayerInput) v1.Image {
	ann := provenanceAnnotations(base, inputs)
	if len(ann) == 0 {
		return img
	}
	// mutate.Annotations returns a v1.Image for a v1.Image input; the assertion is safe and the
	// alternative is threading an error through every caller for a case that cannot occur.
	out, ok := mutate.Annotations(img, ann).(v1.Image)
	if !ok {
		return img
	}
	return out
}
