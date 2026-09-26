// Package reconciler holds the parts of a reconcile loop that both kinds need identically: the
// error triage that decides Stalled from Reconciling, condition writing, and history rotation.
//
// A shared library does not weaken ADR 0004's separation of components. Which failures count as
// terminal or pending is NOT shared; each controller documents its own bar.
package reconciler

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// Object is what these helpers need of a reconciled object: its generation, and its conditions.
type Object interface {
	GetGeneration() int64
	GetConditions() []metav1.Condition
	SetConditions([]metav1.Condition)
}

// TerminalError marks a failure that retrying cannot fix, and maps to Stalled rather than a backoff
// loop. The bar is narrow: editing THIS object's spec must be what fixes it, because the resulting
// generation change is the wake-up. A failure fixed by changing anything else raises no event here,
// so stalling would wait for something that never arrives — use Pending for those.
type TerminalError struct{ err error }

func (t *TerminalError) Error() string { return t.err.Error() }
func (t *TerminalError) Unwrap() error { return t.err }

func Terminal(format string, a ...any) error {
	return &TerminalError{err: fmt.Errorf(format, a...)}
}

func IsTerminal(err error) bool {
	var t *TerminalError
	return errors.As(err, &t)
}

// PendingError marks a dependency that is absent or not ready yet. It is fixed by changing a
// DIFFERENT object (no generation bump), and is a normal step in converging rather than an error,
// so it reports Reconciling and retries on a short fixed interval instead of backing off.
type PendingError struct{ err error }

func (p *PendingError) Error() string { return p.err.Error() }
func (p *PendingError) Unwrap() error { return p.err }

func Pending(format string, a ...any) error {
	return &PendingError{err: fmt.Errorf(format, a...)}
}

func IsPending(err error) bool {
	var p *PendingError
	return errors.As(err, &p)
}

// PendingRetryInterval is how often an object waiting on a dependency checks again. Short, because
// the dependency is usually another object in the same commit, which raises no event here.
const PendingRetryInterval = 30 * time.Second

// Event records one, if a recorder was wired (nil is normal in tests). The message is truncated
// because the API server rejects an over-long one outright, losing the event.
func Event(rec record.EventRecorder, obj runtime.Object, eventType, reason, msg string) {
	if rec == nil {
		return
	}
	rec.Event(obj, eventType, reason, Truncate(msg, 1024))
}

// SetCondition writes one condition, stamped with the generation it was observed at.
func SetCondition(o Object, condType string, status metav1.ConditionStatus, reason, msg string) {
	conds := o.GetConditions()
	meta.SetStatusCondition(&conds, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            Truncate(msg, 32768),
		ObservedGeneration: o.GetGeneration(),
	})
	o.SetConditions(conds)
}

// SetProgressing marks work in flight (ADR 0061). published is what is still served, if anything.
func SetProgressing(o Object, what string, published *ociv1alpha1.ArtifactStatus) {
	msg := what
	if published != nil {
		msg += "; " + published.Ref + " is still the published image"
	}
	SetCondition(o, ociv1alpha1.ReadyCondition, metav1.ConditionUnknown, ociv1alpha1.ReasonProgressing, msg)
	SetCondition(o, ociv1alpha1.ReconcilingCondition, metav1.ConditionTrue, ociv1alpha1.ReasonProgressing, msg)
	RemoveCondition(o, ociv1alpha1.StalledCondition)
}

func RemoveCondition(o Object, condType string) {
	conds := o.GetConditions()
	meta.RemoveStatusCondition(&conds, condType)
	o.SetConditions(conds)
}

// Truncate keeps a message inside the API server's per-condition limit, from the START.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return validUTF8(s[:n])
}

// TruncateTail keeps the END instead, for anything derived from a log: a build's failure is in
// its last lines.
func TruncateTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const marker = "...(truncated)...\n"
	if n <= len(marker) {
		return validUTF8(s[len(s)-n:])
	}
	return marker + validUTF8(s[len(s)-(n-len(marker)):])
}

// validUTF8 drops a rune fragment left by slicing bytes; the JSON encoder would otherwise mangle
// it silently into the message.
func validUTF8(s string) string { return strings.ToValidUTF8(s, "") }

// Interval is spec.interval, or an hour. The CRD defaults it, so the fallback covers an object
// created before the default existed and a deliberate zero.
func Interval(d *metav1.Duration) time.Duration {
	if d != nil && d.Duration > 0 {
		return d.Duration
	}
	return time.Hour
}

// RecordHistory prepends a build and trims to the limit.
//
// A nil record means the reconcile converged without publishing and leaves history alone, so
// interval reconciles do not evict distinct older builds. A rebuild that reproduces an earlier
// digest (ADR 0027) moves that entry to the front instead of duplicating it.
func RecordHistory(history []ociv1alpha1.BuildRecord, record *ociv1alpha1.BuildRecord, limit int) []ociv1alpha1.BuildRecord {
	if record == nil {
		return history
	}
	if limit < 1 {
		limit = 1
	}

	out := make([]ociv1alpha1.BuildRecord, 0, limit)
	out = append(out, *record)
	for _, h := range history {
		if h.Digest == record.Digest {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, h)
	}
	return out
}

// tagPattern is the CRD's own constraint on a tag, applied here because a tag arriving via a ref
// never passed through that validation. Shared so publish.ref and push.ref cannot diverge.
var tagPattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]*$`)

// TagFromRef extracts only the tag from a full image reference.
//
// Hand-parsed because name.ParseReference would default a bare "my-artifact" to ":latest" and
// publish a moving tag nobody asked for. No tag in, no tag out.
func TagFromRef(ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	if strings.ContainsRune(ref, '@') {
		return "", Terminal("the ref %q carries a digest; it must name a tag, since the digest is an output rather than an input", ref)
	}
	// A colon before the last slash is a port, not a tag: "registry:5000/repo".
	colon := strings.LastIndexByte(ref, ':')
	if colon <= strings.LastIndexByte(ref, '/') {
		return "", nil
	}
	tag := ref[colon+1:]
	if !tagPattern.MatchString(tag) {
		return "", Terminal("the ref %q has an invalid tag %q", ref, tag)
	}
	return tag, nil
}

// EffectiveTags is the explicit list plus whatever ref carries, in order and without duplicates.
func EffectiveTags(tags []string, ref string) ([]string, error) {
	fromRef, err := TagFromRef(ref)
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, len(tags)+1)
	seen := make(map[string]struct{}, len(tags)+1)
	for _, t := range append(append([]string(nil), tags...), fromRef) {
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out, nil
}
