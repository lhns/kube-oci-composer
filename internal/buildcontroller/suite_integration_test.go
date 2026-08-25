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
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/lhns/kube-oci-composer/internal/testenv"
)

var (
	testEnv *envtest.Environment
	cfg     *rest.Config
)

func TestMain(m *testing.M) {
	testEnv = &envtest.Environment{
		// Both this project's CRDs and the GitRepository stand-in, because a build resolves its
		// context from a Flux source and installing Flux to test this controller would test Flux.
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join("..", "..", "test", "crds"),
		},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"failed to start envtest: %v\n\nSet KUBEBUILDER_ASSETS, e.g.\n"+
				"  export KUBEBUILDER_ASSETS=$(setup-envtest use 1.33.0 -p path)\n"+
				"or run `make integration-test`, which does it for you.\n", err)
		os.Exit(1)
	}

	// Ctrl+C has to reach the same teardown, or an interrupted run leaks the pair.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		stopEnv()
		os.Exit(130)
	}()

	// Deferred inside a wrapper rather than written after m.Run(): a panic in any test would
	// otherwise skip the stop entirely, which is how this leaked in the first place.
	code := func() (rc int) {
		defer stopEnv()
		return m.Run()
	}()
	os.Exit(code)
}

// stopEnv tears down the API server, then makes sure it is actually gone. See the twin in
// internal/controller: Stop() cannot signal its children on Windows, so the reap closes that gap.
func stopEnv() {
	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stopping envtest: %v\n", err)
	}
	testenv.ReapChildren()
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
