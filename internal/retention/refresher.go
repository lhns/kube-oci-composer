// Package retention keeps the images of live objects from being reclaimed by a registry.
//
// The guarantee, in full, is ADR 0031: an image named by the retained status.history of any live
// ImageComposition or ImageBuild is never deleted, by anything. Expiry beyond that is best-effort —
// leaking bytes is acceptable, losing live content is not.
//
// The mechanism is a lease rather than a scan. Every live object periodically PULLS the manifests
// its history names, under both their digests and their tags, and the registry keeps whatever has
// been pulled recently. "Still referenced" becomes a positive, continuously renewed assertion
// instead of something inferred from the absence of a reference.
//
// Three properties follow, and they are why this shape was chosen over deleting on eviction:
//
//   - This package needs no write and no delete permission. There is no call it makes that can
//     destroy an image, so no bug in it can.
//   - Two objects publishing the same digest need no coordination. Both refresh it; it survives
//     while either lives.
//   - Eviction needs no action. A record falling out of history simply stops being refreshed, and
//     the expiry window doubles as an undo period.
//
// The cost is that it fails UNSAFE: if refreshing stops for longer than the registry's window, live
// content is deleted. That is what makes the failure reporting here load-bearing rather than
// courteous, and why the margin between the interval and the window has to be large.
package retention

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// DefaultInterval is how often every live object's images are refreshed.
//
// One hour against a registry window of 30 days is a margin of 720. That ratio is the guarantee: it
// means refreshing has to fail continuously for weeks, not hours, before anything is at risk, and it
// leaves room for an outage, a rollout, or a slow registry without consequence.
//
// Anyone lowering the registry's window has to lower this with it. The relationship is what holds,
// not either number.
const DefaultInterval = time.Hour

// DegradedAfter is how many consecutive failed cycles for one object raise a condition on it.
//
// Not one: a single failed cycle is an unreachable registry or a rolling restart, and reporting that
// as degraded would train operators to ignore the signal. Three consecutive failures against an
// hourly interval is three hours of a 30-day window — early enough to be a warning rather than a
// post-mortem.
const DegradedAfter = 3

// PendingLister reports objects the controller has not yet reconciled.
//
// Required, for a reason that inverts the collector's. There, an incomplete view risked SWEEPING
// something live. Here it risks under-REFRESHING: an object missing from the view is an object
// whose images stop being kept alive, and the symptom arrives one retention window later with
// nothing to connect it back. Refusing to run on a partial view is the only safe answer.
type PendingLister interface {
	Pending(ctx context.Context) ([]string, error)
}

// Target is one object whose images must be kept alive.
//
// The Refresher takes these rather than listing kinds itself, and that is ADR 0004 showing through:
// the two kinds are separate COMPONENTS with separate RBAC, so the composer has no access to
// ImageBuild and the builder none to ImageComposition. A refresher that listed both would need one
// of them to hold permissions it was deliberately not given.
type Target struct {
	// Object is what an Event is recorded against.
	Object client.Object
	// Push describes the registry. Nil means the object is served from the embedded endpoint, whose
	// content this controller owns outright.
	Push *ociv1alpha1.Push
	// Artifact is the current publication, which may not be in History yet.
	Artifact *ociv1alpha1.ArtifactStatus
	// History is the retention record, and the authority on what must stay alive.
	History []ociv1alpha1.BuildRecord
}

// Source yields the objects one component owns.
type Source interface {
	Targets(ctx context.Context) ([]Target, error)
}

// CompositionSource lists ImageCompositions.
type CompositionSource struct{ client.Client }

func (s CompositionSource) Targets(ctx context.Context) ([]Target, error) {
	var list ociv1alpha1.ImageCompositionList
	if err := s.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing compositions: %w", err)
	}
	out := make([]Target, 0, len(list.Items))
	for i := range list.Items {
		obj := &list.Items[i]
		out = append(out, Target{Object: obj, Push: obj.Spec.Push,
			Artifact: obj.Status.Artifact, History: obj.Status.History})
	}
	return out, nil
}

// BuildSource lists ImageBuilds.
type BuildSource struct{ client.Client }

func (s BuildSource) Targets(ctx context.Context) ([]Target, error) {
	var list ociv1alpha1.ImageBuildList
	if err := s.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing builds: %w", err)
	}
	out := make([]Target, 0, len(list.Items))
	for i := range list.Items {
		obj := &list.Items[i]
		out = append(out, Target{Object: obj, Push: obj.Spec.Push,
			Artifact: obj.Status.Artifact, History: obj.Status.History})
	}
	return out, nil
}

// Pending reports builds the controller has not yet observed.
//
// The builder has no equivalent of the composer's Readiness — it serves nothing, so it has no store
// to warm and nothing to gate a Service on. The completeness question still has to be answered
// though, and generation versus observedGeneration is the whole of it here.
func (s BuildSource) Pending(ctx context.Context) ([]string, error) {
	var list ociv1alpha1.ImageBuildList
	if err := s.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing builds: %w", err)
	}
	var pending []string
	for i := range list.Items {
		obj := &list.Items[i]
		if obj.Status.ObservedGeneration != obj.Generation {
			pending = append(pending, obj.Namespace+"/"+obj.Name)
		}
	}
	return pending, nil
}

// Refresher renews the lease on every live object's images.
type Refresher struct {
	client.Client

	// Source yields the objects to refresh. Required.
	Source Source

	// Interval between cycles.
	Interval time.Duration

	// Pending gates a cycle. See PendingLister.
	Pending PendingLister

	// Recorder surfaces sustained failure. The failure mode of this component is silence followed
	// by deletion, so an Event is not decoration.
	Recorder record.EventRecorder

	// Default is the operator's registry and credential. See recon.DefaultRegistry.
	Default recon.DefaultRegistry

	// InsecureRegistries are hosts that may be reached over plain HTTP, matched on host exactly as
	// the builder matches them, so a host that can be pushed to can also be refreshed.
	InsecureRegistries []string

	// failures counts consecutive failed cycles per object, keyed by namespace/name.
	failures map[string]int
}

// Result summarises one cycle.
type Result struct {
	Skipped     bool
	SkipReason  string
	Objects     int
	References  int
	Refreshed   int
	Failed      int
	NotFound    int
	Unsupported int
}

// NeedLeaderElection keeps refreshing on the leader.
//
// Not for safety — concurrent refreshes are harmless, since a pull is idempotent and cannot corrupt
// anything — but because N replicas would multiply the request volume against the registry for no
// added protection.
func (r *Refresher) NeedLeaderElection() bool { return true }

// Start refreshes on an interval until ctx is cancelled.
func (r *Refresher) Start(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	logger := log.FromContext(ctx).WithName("retention")

	// Run once at startup, unlike the collector, and the difference is deliberate. The collector
	// waits because acting on an unsettled view could DELETE something; refreshing early can only
	// keep something alive that was going to be kept anyway. After a restart, an early cycle is also
	// exactly what a long outage needs.
	r.cycle(ctx, logger)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.cycle(ctx, logger)
		}
	}
}

func (r *Refresher) cycle(ctx context.Context, logger interface {
	Info(string, ...any)
	Error(error, string, ...any)
}) {
	result, err := r.RefreshOnce(ctx)
	switch {
	case err != nil:
		// Loud, and not merely logged at info. A failed cycle is a step toward deletion.
		logger.Error(err, "RETENTION REFRESH FAILED; live images lose their protection if this "+
			"continues for the registry's retention window")
	case result.Skipped:
		logger.Info("retention refresh skipped", "reason", result.SkipReason)
	default:
		logger.Info("retention refresh complete",
			"objects", result.Objects, "references", result.References,
			"refreshed", result.Refreshed, "failed", result.Failed,
			"notFound", result.NotFound, "unsupported", result.Unsupported)
	}
}

// RefreshOnce runs one cycle over every object the Source yields.
func (r *Refresher) RefreshOnce(ctx context.Context) (Result, error) {
	if r.Pending == nil {
		return Result{}, fmt.Errorf("no pending lister configured; refusing to refresh on a view " +
			"that may be incomplete")
	}
	if r.Source == nil {
		return Result{}, fmt.Errorf("no source configured; nothing would be refreshed and every " +
			"live image would silently lose its protection")
	}
	pending, err := r.Pending.Pending(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("checking for unreconciled objects: %w", err)
	}
	if len(pending) > 0 {
		// Under-refreshing is invisible until the window elapses, so a partial view is a reason to
		// do nothing rather than to do most of it.
		return Result{Skipped: true, SkipReason: fmt.Sprintf(
			"%d objects not yet reconciled (%v); a partial view would under-refresh",
			len(pending), pending)}, nil
	}

	targets, err := r.Source.Targets(ctx)
	if err != nil {
		return Result{}, err
	}

	var out Result
	for _, target := range targets {
		r.refreshObject(ctx, target, &out)
	}
	return out, nil
}

// refreshObject pulls every reference one object still names.
//
// Deliberately driven by status alone, and never by whether the object reconciled successfully. An
// object Stalled on a spec error must keep refreshing what it already published — those images may
// be running right now, and stalling is precisely when nobody is watching. ADR 0031 names this as
// the most likely implementation mistake.
func (r *Refresher) refreshObject(ctx context.Context, target Target, out *Result) {
	obj := target.Object
	namespace, objName := obj.GetNamespace(), obj.GetName()
	push := target.Push
	// Resolved the same way the publish path resolves it, so an object using the default registry
	// is refreshed rather than silently skipped -- which would look like nothing at all until its
	// images expired.
	repo := ""
	switch {
	case push != nil && push.Repository != "":
		repo = push.Repository
	case r.Default.Configured():
		repo = r.Default.RepositoryFor(namespace, objName)
	}

	if repo == "" {
		// Nothing external to convince: served from the embedded endpoint, whose content this
		// controller owns outright.
		//
		// Keyed on the resolved REPOSITORY, not on push being nil. An object with no push block
		// still publishes -- to the operator's default registry -- and skipping those would stop
		// refreshing exactly the objects a default install creates, silently, with the symptom
		// arriving one retention window later. That is condition 2's failure wearing a different
		// hat.
		out.Unsupported++
		return
	}

	refs := refsOf(repo, target.Artifact, target.History)
	if len(refs) == 0 {
		return
	}
	out.Objects++
	out.References += len(refs)

	opts, err := r.remoteOptions(ctx, namespace, repo, push)
	if err != nil {
		out.Failed += len(refs)
		r.noteFailure(ctx, obj, namespace, objName, fmt.Sprintf("registry credentials: %v", err))
		return
	}
	var refOpts []name.Option
	if recon.InsecureHost(repo, r.InsecureRegistries) {
		refOpts = append(refOpts, name.Insecure)
	}

	var failed int
	var lastErr error
	for _, ref := range refs {
		parsed, err := name.ParseReference(ref, refOpts...)
		if err != nil {
			failed++
			lastErr = err
			continue
		}

		// A GET, not a HEAD. The registry renews recency for a PULL, and an existence check is not
		// obliged to count as one — the saving would be a few KB on the one request the whole
		// guarantee depends on. remote.Image fetches the manifest and nothing else; layers are
		// lazy, so no blob moves.
		img, err := remote.Image(parsed, opts...)
		if err == nil {
			_, err = img.Manifest()
		}
		switch {
		case err == nil:
			out.Refreshed++
		case isNotFound(err):
			// The guarantee has ALREADY been broken by something else, and quietly. Counted
			// separately because it is a different alarm from a registry that is merely unreachable:
			// one says the protection failed, the other says it might.
			out.NotFound++
			failed++
			lastErr = fmt.Errorf("%s is gone: %w", ref, err)
		default:
			out.Failed++
			failed++
			lastErr = err
		}
	}

	if failed > 0 {
		r.noteFailure(ctx, obj, namespace, objName,
			fmt.Sprintf("%d of %d references: %v", failed, len(refs), lastErr))
		return
	}
	r.clearFailure(namespace, objName)
}

// noteFailure counts consecutive failures and gets loud once they persist.
func (r *Refresher) noteFailure(ctx context.Context, obj client.Object, namespace, objName, detail string) {
	key := namespace + "/" + objName
	if r.failures == nil {
		r.failures = map[string]int{}
	}
	r.failures[key]++
	n := r.failures[key]

	log.FromContext(ctx).WithName("retention").Error(fmt.Errorf("%s", detail),
		"refresh failed", "object", key, "consecutiveFailures", n)

	if n < DegradedAfter {
		return
	}
	// An Event rather than a status condition, deliberately: the reconciler owns this object's
	// conditions, and a second writer racing it would produce a status that disagrees with itself
	// depending on which patch landed last. An Event is additive and cannot lose a reconcile's work.
	recon.Event(r.Recorder, obj, corev1.EventTypeWarning, ociv1alpha1.ReasonRetentionDegraded,
		fmt.Sprintf("Retention refresh has failed %d times in a row (%s). Images this object "+
			"published are protected only while they are refreshed; if this continues for the "+
			"registry's retention window they will be deleted.", n, detail))
}

func (r *Refresher) clearFailure(namespace, objName string) {
	if r.failures != nil {
		delete(r.failures, namespace+"/"+objName)
	}
}

// refsOf lists every reference an object still needs kept alive.
//
// BOTH the digest and each tag, for every retained record. Measured, not assumed: a registry can
// govern tagged and untagged manifests by different rules, so pulling only the digest keeps the
// content alive and lets the tag be collected. A refresh keeps alive exactly what it asks for.
//
// Sourced from status.history plus status.artifact, because history is the retention record and the
// current artifact may not be in it yet.
func refsOf(repository string, artifact *ociv1alpha1.ArtifactStatus,
	history []ociv1alpha1.BuildRecord) []string {

	repo := repository
	if repo == "" {
		return nil
	}

	seen := map[string]struct{}{}
	var refs []string
	add := func(ref string) {
		if ref == "" {
			return
		}
		if _, dup := seen[ref]; dup {
			return
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}

	if artifact != nil {
		add(digestRef(repo, artifact.Digest))
		for _, tag := range artifact.Tags {
			add(qualify(repo, tag))
		}
	}
	for _, rec := range history {
		add(digestRef(repo, rec.Digest))
		for _, tag := range rec.Tags {
			add(qualify(repo, tag))
		}
	}
	return refs
}

func digestRef(repo, digest string) string {
	if digest == "" {
		return ""
	}
	return repo + "@" + digest
}

// qualify accepts a tag recorded either bare or already fully qualified.
//
// status.Tags is written as "repo:tag" by both controllers, but a hand-edited or older object may
// hold a bare tag, and a refresh that silently skipped those would under-protect exactly the objects
// least likely to be noticed.
func qualify(repo, tag string) string {
	if tag == "" {
		return ""
	}
	if strings.Contains(tag, "/") || strings.Contains(tag, "@") {
		return tag
	}
	return repo + ":" + tag
}

// remoteOptions reads the push credential, and nothing else.
//
// The same Secret the object already uses to publish. This package never needs more authority than
// reading, so a credential scoped to pull is enough for it — which is worth knowing when deciding
// what to put in that Secret.
func (r *Refresher) remoteOptions(ctx context.Context, namespace, repository string, push *ociv1alpha1.Push) ([]remote.Option, error) {
	opts := []remote.Option{remote.WithContext(ctx)}

	var ownRef string
	if push != nil && push.SecretRef != nil {
		ownRef = push.SecretRef.Name
	}
	// Same rule as the publish path: the operator's credential only ever reaches the operator's own
	// registry. Refreshing reads, so the blast radius is smaller -- but a credential sent to a host
	// a tenant chose is exfiltrated whether the request that carries it reads or writes.
	name, ns := r.Default.CredentialFor(namespace, ownRef, repository)
	if name == "" {
		return append(opts, remote.WithAuth(authn.Anonymous)), nil
	}

	var secret corev1.Secret
	key := types.NamespacedName{Namespace: ns, Name: name}
	if err := r.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("secret %s not found", key)
		}
		return nil, fmt.Errorf("reading secret %s: %w", key, err)
	}
	kc, err := recon.KeychainFromSecret(&secret)
	if err != nil {
		return nil, fmt.Errorf("secret %s is unusable: %w", key, err)
	}
	return append(opts, remote.WithAuthFromKeychain(kc)), nil
}

// SetupWithManager registers the refresher as a leader-elected runnable.
func (r *Refresher) SetupWithManager(mgr ctrl.Manager) error {
	return mgr.Add(r)
}

// insecureHost reports whether a repository's host may be reached over plain HTTP.
//
// Matched on host rather than applied globally, exactly as the builder matches it, so naming one
// internal registry does not quietly downgrade every other request this controller makes.
// isNotFound distinguishes "the registry says this is gone" from "the registry did not answer".
//
// The difference is the difference between an alarm and a warning: a 404 means the guarantee has
// ALREADY been broken by something, while a timeout means it might be about to be.
func isNotFound(err error) bool {
	var terr *transport.Error
	if errors.As(err, &terr) {
		return terr.StatusCode == http.StatusNotFound
	}
	return false
}
