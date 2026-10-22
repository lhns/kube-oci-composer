//go:build integration

// Integration tests run against a real API server via envtest, chiefly because the fake client
// does not evaluate CEL, leaving every `+kubebuilder:validation:XValidation` rule unverified.
// envtest runs no controller-manager, scheduler or kubelet; anything needing those belongs in
// test/e2e.
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

// TestMain starts one API server for this package (bootstrap shared via internal/testenv). One
// client is shared: these tests only submit objects for validation, so nothing is per-test.
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
