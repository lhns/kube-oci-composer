//go:build integration

package testenv

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"testing"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Run starts an API server, runs the package's tests against it, and tears it down.
//
// Both controller packages need one, and their bootstraps were substantially identical -- the
// builder's carried a comment saying "See the twin in internal/controller", so the duplication was
// known rather than accidental. What legitimately differs is the CRD directories and the reason
// each package needs a real API server at all; the first is a parameter and the second stays in
// each package's own file, where it is the most useful thing written there.
//
// Returns the exit code for the caller to pass to os.Exit, rather than exiting itself, so a caller
// can still do work either side of it.
//
// after runs once the API server is up and before any test does, for a package that wants a shared
// client. An error from it fails the run with the environment torn down -- the case both files
// used to spell out by hand, and the one where forgetting the teardown leaks an API server.
func Run(m *testing.M, crdPaths []string, store **rest.Config, after ...func() error) int {
	env := &envtest.Environment{
		CRDDirectoryPaths:     crdPaths,
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"failed to start envtest: %v\n\nSet KUBEBUILDER_ASSETS, e.g.\n"+
				"  export KUBEBUILDER_ASSETS=$(setup-envtest use 1.33.0 -p path)\n"+
				"or run `make integration-test`, which does it for you.\n", err)
		return 1
	}
	*store = cfg

	stop := func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stopping envtest: %v\n", err)
		}
		// Stop() cannot signal its children on Windows, so the reap closes that gap.
		ReapChildren()
	}

	for _, fn := range after {
		if fn == nil {
			continue
		}
		if err := fn(); err != nil {
			fmt.Fprintf(os.Stderr, "preparing the test environment: %v\n", err)
			stop()
			return 1
		}
	}

	// Ctrl+C has to reach the same teardown, or an interrupted run leaks the pair.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		stop()
		os.Exit(130)
	}()

	// Deferred inside a wrapper rather than written after m.Run(): a panic in any test would
	// otherwise skip the stop entirely, which is how this leaked in the first place.
	return func() (rc int) {
		defer stop()
		return m.Run()
	}()
}
