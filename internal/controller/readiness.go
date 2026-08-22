package controller

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// Readiness tracks which objects this process has reconciled.
//
// It used to gate the pod's readiness probe on the served blob store being warm, so the pod stayed
// out of the Service until it had something to serve. There is no store and no Service now
// (ADR 0035), so that role is gone and readyz is a bare ping.
//
// What remains is the completeness question, which retention needs: an object this process has not
// observed contributes nothing to the live set, and refreshing on a partial view under-protects the
// objects missing from it -- invisibly, with the symptom arriving one retention window later.
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
		// No push-mode exemption any more, and its removal is load-bearing rather than tidying.
		// It existed because an object pushing to an external registry was not served from here, so
		// it could not hold READINESS back. Every object publishes to a registry now, so keeping it
		// would have made Pending return nothing, always -- and the retention refresher would read
		// an empty list as "the view is complete" while having observed nothing at all. Exactly the
		// under-refresh ADR 0031 calls out, arriving as missing images a window later.
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
