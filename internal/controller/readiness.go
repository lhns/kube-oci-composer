package controller

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// Readiness tracks which objects this process has reconciled, so retention can tell whether its
// view is complete: an unobserved object contributes nothing to the live set, and refreshing on a
// partial view silently under-protects it. (It no longer gates readyz; ADR 0035.)
type Readiness struct {
	// Client lists the objects that must be accounted for. The manager's cached client is correct:
	// before the cache syncs the list fails or blocks, and "not synced" is genuinely not ready.
	Client client.Client

	mu   sync.Mutex
	seen map[types.NamespacedName]struct{}
}

// Observe records that an object has been through a reconcile.
//
// Recorded on ATTEMPT, not success: a permanently Stalled object must not hold the gate closed.
// The gate covers the startup window; conditions report health.
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

// Pending returns the ImageCompositions that have not yet been through a reconcile. An empty
// result means the controller's view is complete and a live set marked from it can be trusted.
func (r *Readiness) Pending(ctx context.Context) ([]string, error) {
	var list ociv1alpha1.ImageCompositionList
	if err := r.Client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing ImageCompositions: %w", err)
	}

	var pending []string
	for i := range list.Items {
		obj := &list.Items[i]
		// Every object counts, whatever registry it pushes to: exempting any would let retention
		// read a partial view as complete (ADR 0031).
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
