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

// ErrNotReady signals a source that exists but whose status must not be believed yet: it describes
// an older generation, or the source is not Ready. The fix lives in another object, so the caller
// waits rather than stalls. ADR 0026.
type ErrNotReady struct {
	// What identifies the source, e.g. "GitRepository default/app".
	What string
	// Why is the specific reason, phrased for a status message a human will read.
	Why string
}

func (e *ErrNotReady) Error() string { return e.What + " is not ready: " + e.Why }

// fluxGroupVersions are tried in order. Sources are read unstructured, so this controller has no
// dependency on source-controller's types (ADR 0009).
var fluxGroupVersions = []string{"source.toolkit.fluxcd.io/v1", "source.toolkit.fluxcd.io/v1beta2"}

// FluxSource reads a Flux source's status.artifact. The digest comes from source-controller, which
// has already content-addressed the revision.
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

		// No-match: this API version does not serve the kind, so try the next. NotFound: right
		// version, and the source is absent.
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

	// status.artifact describes a past reconcile: right after a spec change it still names the
	// previous revision, which could publish yesterday's content under today's tag. ADR 0026.
	if err := checkCurrent(obj, ref); err != nil {
		return FluxArtifact{}, err
	}

	url, found, err := unstructured.NestedString(obj.Object, "status", "artifact", "url")
	if err != nil || !found || url == "" {
		// Ordinary right after the source is created.
		return FluxArtifact{}, fmt.Errorf("%s has no artifact yet", ref)
	}
	digest, found, err := unstructured.NestedString(obj.Object, "status", "artifact", "digest")
	if err != nil || !found || digest == "" {
		return FluxArtifact{}, fmt.Errorf("%s published an artifact with no digest", ref)
	}
	revision, _, _ := unstructured.NestedString(obj.Object, "status", "artifact", "revision")

	return FluxArtifact{URL: url, Digest: digest, Revision: revision}, nil
}

// checkCurrent reports whether the source's status can be believed about its current spec: the
// controller has observed the current generation, and the source is Ready (a failed fetch keeps
// the last good artifact in status).
//
// Absent fields are not failures: other implementations of the API may not set them, and refusing
// would wedge every composition referencing them.
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
// Walked by hand so one malformed conditions entry does not stop the others being read.
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
