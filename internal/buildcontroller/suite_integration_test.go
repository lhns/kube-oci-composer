//go:build integration

// Integration tests for the builder, against a real API server via envtest.
//
// They cover what the fake client cannot: reconciles triggered by WATCH events from the
// controller's own writes (e.g. deleting an owned Job re-triggering a build in a hot loop). If an
// assertion is about what happens because the controller wrote something, it belongs here.
//
// envtest runs no controller-manager, scheduler or kubelet: Jobs never produce Pods or complete on
// their own, so tests drive Job status themselves. Real builds belong in test/e2e.
package buildcontroller

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

var cfg *rest.Config

// TestMain starts one API server for this package (bootstrap in internal/testenv).
func TestMain(m *testing.M) {
	os.Exit(testenv.Run(m, []string{
		// Our CRDs plus a GitRepository stand-in, so Flux need not be installed.
		filepath.Join("..", "..", "config", "crd", "bases"),
		filepath.Join("..", "..", "test", "crds"),
	}, &cfg))
}

func integrationCtx(t *testing.T) (context.Context, client.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	k8s, err := client.New(cfg, client.Options{Scheme: testScheme(t)})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}
	return ctx, k8s
}
