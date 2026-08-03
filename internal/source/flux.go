package source

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// FluxArtifact is what source-controller publishes about a source's current revision.
type FluxArtifact struct {
	// URL is the in-cluster address of the artifact tarball.
	URL string
	// Digest of the tarball, in "<algo>:<hex>" form.
	Digest string
	// Revision is the human-facing revision string, e.g. "main@sha1:abcd".
	Revision string
}

// fluxGroupVersions are tried in order. Flux moves source kinds between API versions over time,
// and reading the object unstructured means this controller does not need a dependency on
// source-controller's types — nor to be rebuilt when they change. See ADR 0009.
var fluxGroupVersions = []string{"source.toolkit.fluxcd.io/v1", "source.toolkit.fluxcd.io/v1beta2"}

// FluxSource reads a Flux source's status.artifact.
//
// The digest comes from the source rather than from our spec: source-controller has already
// cloned, verified and content-addressed the revision, and duplicating that would be both work and
// a second opinion about what the repository contains.
func FluxSource(ctx context.Context, c client.Client, kind, namespace, name string) (FluxArtifact, error) {
	ref := fmt.Sprintf("%s %s/%s", kind, namespace, name)

	var obj *unstructured.Unstructured
	var lastErr error
	for _, gv := range fluxGroupVersions {
		parsed, err := schema.ParseGroupVersion(gv)
		if err != nil {
			return FluxArtifact{}, err
		}
		candidate := &unstructured.Unstructured{}
		candidate.SetGroupVersionKind(parsed.WithKind(kind))

		err = c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, candidate)
		if err == nil {
			obj = candidate
			break
		}
		lastErr = err

		// A no-match error means this API version does not serve the kind, so trying the next one
		// is the whole point of the loop. A plain NotFound means we found the right version and
		// the source genuinely is not there — retrying other versions would only obscure that.
		if meta.IsNoMatchError(err) {
			continue
		}
		if apierrors.IsNotFound(err) {
			return FluxArtifact{}, &ErrNotFound{What: ref}
		}
	}
	if obj == nil {
		return FluxArtifact{}, fmt.Errorf("reading %s: %w", ref, lastErr)
	}

	url, found, err := unstructured.NestedString(obj.Object, "status", "artifact", "url")
	if err != nil || !found || url == "" {
		// Not an error worth stalling on: source-controller has not published an artifact yet,
		// which is an ordinary state right after the source is created.
		return FluxArtifact{}, fmt.Errorf("%s has no artifact yet", ref)
	}
	digest, found, err := unstructured.NestedString(obj.Object, "status", "artifact", "digest")
	if err != nil || !found || digest == "" {
		return FluxArtifact{}, fmt.Errorf("%s published an artifact with no digest", ref)
	}
	revision, _, _ := unstructured.NestedString(obj.Object, "status", "artifact", "revision")

	return FluxArtifact{URL: url, Digest: digest, Revision: revision}, nil
}
