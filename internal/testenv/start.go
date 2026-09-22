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

// Run starts an API server with the given CRDs, stores its config in *store, runs the package's
// tests against it, and tears it down. It returns the exit code for os.Exit.
//
// after runs once the API server is up and before any test, e.g. to build a shared client; an
// error from it fails the run with the environment torn down.
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

	// An interrupted run must tear down too, or it leaks the processes.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		stop()
		os.Exit(130)
	}()

	// Deferred, so a panicking test still stops the environment.
	return func() (rc int) {
		defer stop()
		return m.Run()
	}()
}
