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

// ErrNotReady signals a source that exists but whose status must not be believed yet: either it
// describes an older generation than the spec it is attached to, or the source itself says it is
// not Ready.
//
// Distinct from ErrNotFound because the object IS there — and distinct from an ordinary error
// because the fix lives in another object, so the caller must wait rather than stall. See
// ADR 0026 for the incident that made this a typed condition rather than an assumption.
type ErrNotReady struct {
	// What identifies the source, e.g. "GitRepository default/app".
	What string
	// Why is the specific reason, phrased for a status message a human will read.
	Why string
}

func (e *ErrNotReady) Error() string { return e.What + " is not ready: " + e.Why }

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

	// Before anything is read out of status: does status describe the spec that is there NOW?
	//
	// status.artifact is a statement about a PAST reconcile of the source, and a source whose spec
	// has just moved to a new ref.tag keeps reporting the previous revision's artifact — Ready=True,
	// URL and digest all intact — until source-controller catches up. Consuming that is how a
	// composition publishes yesterday's content under today's tag, which no later reconcile can
	// undo because a tag's FIRST publish has nothing to conflict with. See ADR 0026.
	if err := checkCurrent(obj, ref); err != nil {
		return FluxArtifact{}, err
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

// checkCurrent reports whether the source's status can be believed about its current spec.
//
// Two questions, and both are answered from fields every Flux source publishes:
//
//   - has the source-controller observed the spec that exists now? metadata.generation is bumped by
//     the API server on every spec write; status.observedGeneration is what the controller has
//     acted on. While they differ, everything in status — including status.artifact — describes the
//     PREVIOUS spec.
//   - does the source itself claim to be Ready? A source that failed to fetch keeps its last good
//     artifact in status, so "Ready=False with a usable-looking artifact" is an ordinary state, and
//     one where that artifact is by definition not what the spec now asks for.
//
// Absent fields are deliberately NOT treated as failures. These objects are read unstructured
// precisely so this controller carries no dependency on source-controller's types (ADR 0009), and
// refusing on a field some other implementation of the same API does not set would wedge every
// composition referencing it — a worse failure than the one being prevented, and one this code
// could not diagnose. The check tightens what is knowable; it does not demand it.
func checkCurrent(obj *unstructured.Unstructured, ref string) error {
	observed, found, err := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
	if err == nil && found && observed != obj.GetGeneration() {
		return &ErrNotReady{What: ref, Why: fmt.Sprintf(
			"status describes generation %d but the spec is at generation %d, so status.artifact still names the previous revision",
			observed, obj.GetGeneration())}
	}

	if status, reason, found := readyCondition(obj); found && status != "True" {
		return &ErrNotReady{What: ref, Why: fmt.Sprintf(
			"Ready=%s (%s), so status.artifact is whatever it last managed to fetch rather than what the spec asks for",
			status, reason)}
	}
	return nil
}

// readyCondition returns the status and reason of the Ready condition, and whether one was found.
//
// Hand-walked rather than converted into metav1.Condition: an unstructured status may hold a
// conditions entry that does not round-trip into the typed shape, and one malformed entry must not
// stop the others being read.
func readyCondition(obj *unstructured.Unstructured) (status, reason string, found bool) {
	conditions, ok, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !ok {
		return "", "", false
	}
	for _, entry := range conditions {
		c, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := c["type"].(string); t != "Ready" {
			continue
		}
		status, _ = c["status"].(string)
		reason, _ = c["reason"].(string)
		if reason == "" {
			reason = "no reason given"
		}
		return status, reason, true
	}
	return "", "", false
}
