//go:build integration

// Integration tests for the builder, against a real API server via envtest.
//
// They exist for the one thing the fake client structurally cannot do: deliver WATCH events. The
// unit suite drives Reconcile by calling it, so a reconcile provoked by the controller's own writes
// is invisible there — and that blind spot produced the worst defect this controller has had. The
// failure path deleted a failed Job so the next attempt would not adopt it; deleting an owned Job
// woke the controller through its own Owns() watch, which found no Job and started another, so the
// backoff never applied and a failing build retried every few seconds forever. Every unit test
// passed throughout.
//
// So the rule for what belongs here: if the assertion is about what happens BECAUSE the controller
// wrote something, it cannot live in the unit suite.
//
// What envtest does NOT run: kube-controller-manager, the scheduler, or a kubelet. Jobs therefore
// never produce Pods and never complete on their own — tests drive Job status themselves. Anything
// needing a real build belongs in test/e2e.
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

// TestMain starts one API server for this package. The bootstrap lives in internal/testenv because
// both controller packages need it; what belongs here is WHY this package needs a real API server,
// which is the comment above.
func TestMain(m *testing.M) {
	os.Exit(testenv.Run(m, []string{
		// Both this project's CRDs and the GitRepository stand-in, because a build resolves its
		// context from a Flux source and installing Flux to test this controller would test Flux.
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
