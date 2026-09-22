//go:build integration

// Integration tests run against a real API server via envtest.
//
// They exist for the things a fake client cannot check. Most importantly: the fake client does not
// evaluate CEL, so every `+kubebuilder:validation:XValidation` rule in the API is completely
// unverified by the unit tests. Those rules are the only thing stopping an incoherent spec from
// being accepted and then failing at reconcile time, so testing them against something that
// actually runs them matters.
//
// What envtest does NOT run: kube-controller-manager, the scheduler, or a kubelet. Anything
// depending on those belongs in test/e2e.
package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lhns/kube-oci-composer/internal/testenv"
)

var (
	cfg *rest.Config
	k8s client.Client
)

// TestMain starts one API server for this package. The bootstrap lives in internal/testenv because
// both controller packages need it; what belongs here is WHY this package needs a real API server,
// which is the comment above.
//
// The client is built once and shared, unlike the builder's, which builds one per test: these
// tests only submit objects for the API server to validate, so there is nothing per-test to scope.
func TestMain(m *testing.M) {
	os.Exit(testenv.Run(m, []string{
		filepath.Join("..", "..", "config", "crd", "bases"),
	}, &cfg, func() error {
		var err error
		k8s, err = client.New(cfg, client.Options{Scheme: integrationScheme()})
		return err
	}))
}

func integrationCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}
