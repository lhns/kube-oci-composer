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
	k8s     client.Client
)

func TestMain(m *testing.M) {
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
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

	k8s, err = client.New(cfg, client.Options{Scheme: integrationScheme()})
	if err != nil {
		fmt.Fprintf(os.Stderr, "building client: %v\n", err)
		stopEnv()
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

// stopEnv tears down the API server, then makes sure it is actually gone.
//
// Stop() reports success on Linux and fails on Windows with "not supported by windows" -- it
// signals its children, and Windows has no such signal -- so the reap is what closes the gap
// there. Safe to call more than once.
func stopEnv() {
	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stopping envtest: %v\n", err)
	}
	testenv.ReapChildren()
}

func integrationCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}
