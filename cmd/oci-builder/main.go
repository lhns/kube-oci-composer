// Command oci-builder runs the ImageBuild controller.
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
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/buildcontroller"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
	"github.com/lhns/kube-oci-composer/internal/retention"
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

// splitList turns a comma-separated flag into a slice, dropping blanks.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func main() {
	var (
		metricsAddr       string
		probeAddr         string
		enableLeader      bool
		builderImage      string
		frontendImage     string
		sourceDateEpoch   string
		insecureRegs      string
		refreshInterval   time.Duration
		defaultRegistry   string
		defaultPushSecret string
		historyLimit      int
		showVersion       bool
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
	flag.DurationVar(&refreshInterval, "retention-refresh-interval", retention.DefaultInterval,
		"How often to re-pull the images every live ImageBuild still references, so that a registry "+
			"with an expiry policy does not reclaim them. Zero disables it.\n"+
			"Same flag name and meaning on both controllers. It must stay MUCH shorter than the "+
			"registry's retention window -- the ratio is the guarantee, not either number. It matters "+
			"more here than on the composer: a build cannot be reproduced from its spec, so a reclaimed "+
			"image is gone rather than rebuildable. See ADR 0031.")
	flag.StringVar(&defaultRegistry, "default-registry", "",
		"Registry to publish to when an object names no repository of its own, as "+
			"\"registry.example:5000\" or \"registry.example:5000/prefix\". Objects then publish "+
			"to <default-registry>/<namespace>/<name>.\n"+
			"Namespace-qualified deliberately: one registry is shared by the whole cluster, so a "+
			"bare object name would collide the moment two namespaces both have an \"app\".")
	flag.StringVar(&defaultPushSecret, "default-push-secret", "",
		"dockerconfigjson Secret authenticating pushes to --default-registry. Read from THIS "+
			"controller's namespace (POD_NAMESPACE), not the object's: it is the operator's "+
			"credential, not a tenant's.\n"+
			"Used ONLY for objects that named no repository. An object that chooses its own "+
			"registry authenticates with its own secretRef or not at all -- otherwise anyone able "+
			"to create an object could point it at a host they control and be handed this password.")
	flag.StringVar(&insecureRegs, "insecure-registry", "",
		"Comma-separated registry hosts to push to over plain HTTP. Opt-in per host, for an "+
			"internal or air-gapped registry without TLS.")
	flag.IntVar(&historyLimit, "keep-builds", ociv1alpha1.DefaultHistoryLimit, "How many past builds to retain in status.")
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

	defaults := recon.DefaultRegistry{
		Host:       defaultRegistry,
		SecretName: defaultPushSecret,
		Namespace:  os.Getenv("POD_NAMESPACE"),
	}
	if defaults.SecretName != "" && defaults.Namespace == "" {
		setupLog.Error(nil, "POD_NAMESPACE is unset, so the default push credential cannot be "+
			"read; set it from the downward API")
		os.Exit(1)
	}

	if err := (&buildcontroller.ImageBuildReconciler{
		Client:  mgr.GetClient(),
		Default: defaults,
		//nolint:staticcheck // SA1019: the new events API has no Event method; see the composer.
		Recorder: mgr.GetEventRecorderFor("imagebuild-controller"),
		JobConfig: buildcontroller.JobConfig{
			BuilderImage:       builderImage,
			FrontendImage:      frontendImage,
			SourceDateEpoch:    sourceDateEpoch,
			InsecureRegistries: splitList(insecureRegs),
		},
		HistoryLimit: historyLimit,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up the ImageBuild controller")
		os.Exit(1)
	}

	if refreshInterval > 0 {
		source := retention.BuildSource{Client: mgr.GetClient()}
		refresher := &retention.Refresher{
			Client: mgr.GetClient(),
			Source: source,
			// The builder has no Readiness to borrow -- it serves nothing -- so completeness is
			// answered from generation versus observedGeneration by the source itself.
			Pending:  source,
			Interval: refreshInterval,
			//nolint:staticcheck // SA1019: the new events API has no Event method; same as above.
			Recorder:           mgr.GetEventRecorderFor("retention"),
			InsecureRegistries: splitList(insecureRegs),
			Default:            defaults,
		}
		if err := refresher.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to set up retention refresh")
			os.Exit(1)
		}
		setupLog.Info("retention refresh enabled", "interval", refreshInterval)
	} else {
		// Worth saying out loud on this kind in particular. A build cannot be reproduced from its
		// spec (ADR 0025), so an image a registry reclaims here is gone rather than rebuildable.
		setupLog.Info("retention refresh DISABLED; a registry with an expiry policy will delete " +
			"images this operator's builds still reference, and a build cannot be reproduced")
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
