// Package reconciler holds the parts of a reconcile loop that both kinds need identically: the
// error triage that decides Stalled from Reconciling, condition writing, and history rotation.
//
// Shared deliberately, and it does not weaken ADR 0004's separation. That ADR separates
// COMPONENTS — separate binaries, charts and RBAC, and it rejected a feature flag because "a flag
// set to false is a weaker guarantee than a component that does not exist". A shared library is not
// a shared controller; api/v1alpha1 is already imported by both.
//
// What is NOT shared is which failures qualify as terminal or pending. That is a property of the
// call sites, and each controller documents its own bar.
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

// PendingError marks a dependency that is absent or not ready yet.
//
// Neither terminal nor an ordinary transient failure: it is fixed by changing a DIFFERENT object,
// which does not bump this generation, and "the GitRepository applied one second after me does not
// exist yet" is a normal step in converging a commit rather than something to log as an error and
// back off exponentially over. It reports Reconciling and retries on a short fixed interval.
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

// Event records one, if a recorder was wired. Nil is normal in tests that do not care.
//
// Truncated because the API server rejects an over-long event message outright, and the cases that
// produce one — a build's stderr, a list of every unpinned FROM — are exactly the failures worth
// seeing. Losing the whole event to keep the tail is the wrong trade.
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

func RemoveCondition(o Object, condType string) {
	conds := o.GetConditions()
	meta.RemoveStatusCondition(&conds, condType)
	o.SetConditions(conds)
}

// Truncate keeps a message inside the API server's per-condition limit.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

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
// A nil record means the reconcile converged without publishing and must not touch history:
// appending on every interval would fill the list with duplicates of the current build and evict
// genuinely distinct older ones within hours.
//
// A rebuild that reproduces an earlier digest MOVES that entry to the front rather than duplicating
// it. Reverting a change and reverting it back is ordinary, and each round trip would otherwise burn
// two retention slots on one artifact — which matters more now that rebuilds are known to reproduce
// (ADR 0027).
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

// tagPattern is the CRD's own constraint on a tag, applied here too because a tag arriving via
// a ref never passed through that validation.
//
// Shared by both kinds: publish.ref and push.ref mean the same thing, and a second copy is how the
// two would stop meaning the same thing.
var tagPattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]*$`)

// tagFromRef extracts the tag from a full image reference, and NOTHING else — the host and
// repository are the caller's business, not this field's.
//
// Deliberately hand-parsed rather than handed to name.ParseReference, which would default a bare
// "my-artifact" to "index.docker.io/library/my-artifact:latest". Inventing a `latest` out of an
// untemplated placeholder is exactly the wrong answer: it would publish a moving tag nobody asked
// for. No tag in, no tag out.
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

// effectiveTags is the explicit list plus whatever ref carries, in order and without duplicates.
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
