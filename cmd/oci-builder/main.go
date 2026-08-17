// Command oci-builder runs the DockerBuild controller.
//
// A SECOND binary, deliberately. ADR 0004 rejected one binary with a flag — "a flag set to false
// is a weaker guarantee than a component that does not exist" — and the RBAC makes the point
// concrete: the composer's role cannot create a single object, while this one creates Jobs, which
// is the ability to run arbitrary containers. Bundling would put that in every composer install.
//
// This binary itself runs no builds. It creates a Job per build and observes it, so its own pod
// keeps the same posture the composer has: distroless, non-root, read-only root filesystem, no
// privileges. The code from a git repository runs in a different pod, under a different service
// account. See ADR 0025.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/buildcontroller"
)

// Stamped at build time via -ldflags, matching the composer.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(ociv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr     string
		probeAddr       string
		enableLeader    bool
		builderImage    string
		frontendImage   string
		sourceDateEpoch string
		historyLimit    int
		showVersion     bool
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the probe endpoint binds to.")
	flag.BoolVar(&enableLeader, "leader-elect", false, "Enable leader election.")
	flag.StringVar(&builderImage, "buildkit-image", "",
		"Rootless BuildKit image, PINNED BY DIGEST. Required.")
	flag.StringVar(&frontendImage, "dockerfile-frontend", "",
		"Dockerfile frontend image, PINNED BY DIGEST. Required.")
	flag.StringVar(&sourceDateEpoch, "source-date-epoch", "0",
		"SOURCE_DATE_EPOCH stamped into builds. Fixed rather than the wall clock, matching the composer's epoch.")
	flag.IntVar(&historyLimit, "keep-builds", 10, "How many past builds to retain in status.")
	flag.BoolVar(&showVersion, "version", false, "Print the version and exit.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if showVersion {
		fmt.Printf("oci-builder %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Both images must be pinned, and this is refused at startup rather than warned about.
	//
	// Their digests are in the input hash, playing the role oci.AssemblyVersion plays for the
	// composer: without them, an upgraded BuildKit would produce different output under an
	// unchanged hash and the controller would keep serving the old artifact forever (ADR 0002).
	// A floating tag would make that hash a claim the controller cannot honour, so it is better to
	// fail to start than to start and be quietly wrong.
	for _, img := range []struct{ flag, value string }{
		{"--buildkit-image", builderImage},
		{"--dockerfile-frontend", frontendImage},
	} {
		if img.value == "" {
			setupLog.Error(nil, "required flag is not set", "flag", img.flag)
			os.Exit(1)
		}
		if !strings.Contains(img.value, "@sha256:") {
			setupLog.Error(nil,
				"image must be pinned by digest: its digest is part of the build input hash, "+
					"so a floating tag would let an upgraded builder change output without changing the hash",
				"flag", img.flag, "value", img.value)
			os.Exit(1)
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeader,
		LeaderElectionID:       "oci-builder.lhns.de",
	})
	if err != nil {
		setupLog.Error(err, "unable to start the manager")
		os.Exit(1)
	}

	if err := (&buildcontroller.DockerBuildReconciler{
		Client: mgr.GetClient(),
		JobConfig: buildcontroller.JobConfig{
			BuilderImage:    builderImage,
			FrontendImage:   frontendImage,
			SourceDateEpoch: sourceDateEpoch,
		},
		HistoryLimit: historyLimit,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up the DockerBuild controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up the health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up the readiness check")
		os.Exit(1)
	}

	setupLog.Info("starting oci-builder",
		"version", version, "builder", builderImage, "frontend", frontendImage)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with an error")
		os.Exit(1)
	}
}
