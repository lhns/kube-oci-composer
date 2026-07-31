package controller

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// Readiness gates the pod's readiness probe on the served blob store being warm.
//
// The serving endpoint has no durable state: after a restart it is empty, and it is refilled by
// the reconcile that controller-runtime fires for every object once the cache syncs. Without
// this gate the pod would join the Service immediately and answer 404 to every pull in the
// meantime, putting workloads into ImagePullBackOff for no reason. Failing readiness instead
// keeps the pod out of Endpoints until there is something to serve.
//
// It also makes multiple replicas behave sensibly. The endpoint runs under leader election, so a
// standby replica neither reconciles nor listens; because it never observes anything it never
// reports ready, and therefore never receives traffic. Active/standby falls out of the same
// mechanism rather than needing a second one.
type Readiness struct {
	// Client lists the objects that must be accounted for. The manager's cached client is
	// correct here: before the cache syncs the list call fails or blocks, and "not synced" is
	// genuinely not ready.
	Client client.Client

	// Timeout bounds the list so a wedged cache surfaces as unready rather than as a probe that
	// never answers.
	Timeout time.Duration

	mu   sync.Mutex
	seen map[types.NamespacedName]struct{}
}

// Observe records that an object has been through a reconcile.
//
// Deliberately recorded on ATTEMPT, not on success. A single ImageComposition with a bad digest
// is permanently Stalled, and gating readiness on success would let it hold the entire endpoint
// out of the Service — one broken object taking down every unrelated artifact. The gate exists
// to cover the startup window, not to assert that everything is healthy; conditions do that.
func (r *Readiness) Observe(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = make(map[types.NamespacedName]struct{})
	}
	r.seen[key] = struct{}{}
}

// Forget drops an object, so a deleted one cannot keep the tracker growing.
func (r *Readiness) Forget(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.seen, key)
}

func (r *Readiness) observed(key types.NamespacedName) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.seen[key]
	return ok
}

// Pending returns the locally served objects that have not yet been through a reconcile.
//
// Also the safety gate for garbage collection. The collector decides what to delete by marking
// what every object references, so an object it has not seen contributes nothing to the live set
// and its content looks like garbage. An empty result means the controller's view is complete
// and marking can be trusted.
func (r *Readiness) Pending(ctx context.Context) ([]string, error) {
	var list ociv1alpha1.ImageCompositionList
	if err := r.Client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing ImageCompositions: %w", err)
	}

	var pending []string
	for i := range list.Items {
		obj := &list.Items[i]
		// Objects that push to an external registry are not served from here, so they cannot
		// hold readiness back — the registry serves them whether this pod is up or not.
		if obj.Spec.Push != nil {
			continue
		}
		if !obj.DeletionTimestamp.IsZero() {
			continue
		}
		key := types.NamespacedName{Namespace: obj.Namespace, Name: obj.Name}
		if !r.observed(key) {
			pending = append(pending, key.String())
		}
	}
	sort.Strings(pending)
	return pending, nil
}

// Check is a healthz.Checker reporting whether every locally served artifact has been built.
func (r *Readiness) Check(req *http.Request) error {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(req.Context(), timeout)
	defer cancel()

	pending, err := r.Pending(ctx)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return fmt.Errorf("blob store is still warming up; %d artifact(s) not built yet: %s",
			len(pending), strings.Join(pending, ", "))
	}
	return nil
}
