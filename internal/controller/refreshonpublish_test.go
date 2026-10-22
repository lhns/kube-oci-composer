package controller

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lhns/kube-oci-composer/internal/oci"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
	"github.com/lhns/kube-oci-composer/internal/retention"
)

// countingRegistry records every manifest GET, which is what registers a lease. A HEAD does not
// count: a registry need not treat an existence check as a pull.
type countingRegistry struct {
	*httptest.Server
	mu   sync.Mutex
	gets []string
}

func (c *countingRegistry) manifestGets() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.gets...)
}

func newCountingRegistry(t *testing.T) *countingRegistry {
	t.Helper()
	c := &countingRegistry{}
	inner := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/manifests/") {
			c.mu.Lock()
			c.gets = append(c.gets, r.URL.Path)
			c.mu.Unlock()
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(c.Close)
	return c
}

// TestAPublishIsProtectedImmediately — a publish must pull its artifact at once rather than wait
// for the refresher's next tick (up to an hour). Until then it has no lease: a pull-recency
// registry has no record of the push, and zot reuses an OLD push timestamp for a known digest.
func TestAPublishIsProtectedImmediately(t *testing.T) {
	reg := newCountingRegistry(t)
	host := strings.TrimPrefix(reg.URL, "http://")

	origin := newCountingOrigin(t, map[string]string{"app/config.yaml": "x"})
	obj := composition("protected", urlLayer("cfg", origin.url, origin.digest, "/cfg"))

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(obj).WithStatusSubresource(obj).Build()

	r := &ImageCompositionReconciler{
		Client:   c,
		Scheme:   testScheme(t),
		Recorder: record.NewFakeRecorder(64),
		Default:  recon.DefaultRegistry{Host: host},
		Fetcher:  oci.NewFetcher(),
	}
	r.Refresher = &retention.Refresher{
		Client:             c,
		Source:             retention.CompositionSource{Client: c},
		Pending:            everythingReconciled{},
		Recorder:           record.NewFakeRecorder(16),
		InsecureRegistries: []string{host},
		Default:            r.Default,
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: obj.Namespace, Name: obj.Name},
	}); err != nil {
		t.Fatalf("reconciling: %v", err)
	}

	got := reload(t, r, obj)
	if got.Status.Artifact == nil {
		t.Fatal("nothing was published")
	}

	digest := got.Status.Artifact.Digest
	var pulled bool
	for _, path := range reg.manifestGets() {
		if strings.HasSuffix(path, digest) {
			pulled = true
		}
	}
	if !pulled {
		t.Errorf("the artifact was published and never pulled, so it carries no lease and a "+
			"collection pass before the next cycle would reclaim it\nmanifest GETs: %v",
			reg.manifestGets())
	}
}

// everythingReconciled is a readiness gate that reports nothing pending.
type everythingReconciled struct{}

func (everythingReconciled) Pending(context.Context) ([]string, error) { return nil, nil }
