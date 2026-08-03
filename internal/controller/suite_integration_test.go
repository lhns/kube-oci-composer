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
	"path/filepath"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
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
		_ = testEnv.Stop()
		os.Exit(1)
	}

	code := m.Run()

	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stopping envtest: %v\n", err)
	}
	os.Exit(code)
}

func integrationCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}
