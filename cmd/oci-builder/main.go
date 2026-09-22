// Command oci-builder runs the ImageBuild controller.
//
// A separate binary from the composer (ADR 0004): creating Jobs means running arbitrary
// containers, which a composer install must not be able to do. This process runs no builds
// itself; it creates and observes a Job per build, and keeps the composer's locked-down pod
// posture. ADR 0025.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/buildcontroller"
	"github.com/lhns/kube-oci-composer/internal/opts"
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

func main() {
	// `oci-builder fetch-context` is the build pod's init container: no flags, manager or
	// kubeconfig of the controller apply.
	if len(os.Args) > 1 && os.Args[1] == "fetch-context" {
		runFetchContext(os.Args[2:])
		return
	}

	// Everything both controllers share about publishing, trust and supply chain.
	var registry opts.Registry
	registry.Bind(flag.CommandLine)

	var (
		metricsAddr          string
		probeAddr            string
		contextAddr          string
		contextBaseURL       string
		enableLeader         bool
		builderImage         string
		frontendImage        string
		fetcherImage         string
		fetchDenyPrivate     bool
		sourceDateEpoch      string
		refreshInterval      time.Duration
		buildPollInterval    time.Duration
		historyLimit         int
		requirePinnedSources bool
		showVersion          bool
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the probe endpoint binds to.")
	flag.StringVar(&contextAddr, "context-bind-address", ":8090",
		"Address the build-context endpoint binds to. Build pods fetch their Flux source through "+
			"it, so they never reach source-controller.")
	flag.StringVar(&contextBaseURL, "context-base-url", "",
		"URL build pods use to reach the context endpoint, e.g. "+
			"http://oci-builder-context.oci-composer.svc:8090. Unset makes build pods fetch "+
			"source-controller directly, which lets any build read any namespace's source.")
	flag.BoolVar(&enableLeader, "leader-elect", false, "Enable leader election.")
	flag.StringVar(&builderImage, "buildkit-image", "",
		"Rootless BuildKit image, PINNED BY DIGEST. Required.")
	flag.StringVar(&frontendImage, "dockerfile-frontend", "",
		"Dockerfile frontend image, PINNED BY DIGEST. Required.")
	flag.StringVar(&fetcherImage, "fetcher-image", "",
		"Image running `oci-builder fetch-context` as each build's init container. Required, and "+
			"normally this operator's own image, which the chart fills in. "+
			"It is part of the build input hash: the fetcher decides how an archive becomes a directory "+
			"tree, so a fixed unpack bug would otherwise change that tree under an unchanged hash. "+
			"Pinning it by digest is recommended but not required -- see the note at startup.")
	flag.BoolVar(&fetchDenyPrivate, "fetch-deny-private", false,
		"Refuse a spec.context.fetch URL resolving to a private, loopback or CGNAT address. "+
			"Link-local is refused whatever this says: that is where cloud metadata endpoints hand "+
			"out credentials. See ADR 0036. Off by default for the composer's reason -- an artifact "+
			"server on a private address is an ordinary source, and a guard people disable is no guard.")
	flag.StringVar(&sourceDateEpoch, "source-date-epoch", "0",
		"SOURCE_DATE_EPOCH stamped into builds. Fixed rather than the wall clock, matching the composer's epoch.")
	var exportFlags opts.ExportFlags
	exportFlags.Register(flag.CommandLine)
	flag.DurationVar(&buildPollInterval, "build-poll-interval", 0,
		"How often a running build Job is re-observed. Zero uses the built-in default. "+
			"Also bounds how long a build's image is UNTAGGED: the Job pushes by digest and this "+
			"controller names it when it next looks, and untagged is what a registry's collector "+
			"reclaims. The chart derives the registry's gcDelay from this.")
	flag.DurationVar(&refreshInterval, "retention-refresh-interval", retention.DefaultInterval,
		"How often to re-pull the images every live ImageBuild still references, so that a registry "+
			"with an expiry policy does not reclaim them. Zero disables it.\n"+
			"Same flag name and meaning on both controllers. It must stay MUCH shorter than the "+
			"registry's retention window -- the ratio is the guarantee, not either number. It matters "+
			"more here than on the composer: a build cannot be reproduced from its spec, so a reclaimed "+
			"image is gone rather than rebuildable. See ADR 0031.")
	flag.BoolVar(&requirePinnedSources, "require-pinned-sources", false,
		"Refuse an ImageBuild whose spec.context names no revision. Pinning is optional by design (ADR 0026), so this is how an operator decides otherwise for a whole cluster. It matters more here than on the composer: an unpinned context builds whatever the branch is at now, and a build's output cannot be reproduced from its spec (ADR 0025).")
	flag.IntVar(&historyLimit, "keep-builds", ociv1alpha1.DefaultHistoryLimit, "How many past builds to retain in status.")
	flag.BoolVar(&showVersion, "version", false, "Print the version and exit.")

	zapOpts := opts.Zap()
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	if showVersion {
		fmt.Printf("oci-builder %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	// Both images must be pinned, refused at startup: their digests are in the input hash, which
	// a floating tag would make meaningless (ADR 0002).
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

	// The fetcher is required but only warned about when unpinned: it is normally this operator's
	// own image, already chosen by the deployment, and a tag still moves with each release.
	if fetcherImage == "" {
		setupLog.Error(nil, "required flag is not set", "flag", "--fetcher-image")
		os.Exit(1)
	}
	if !strings.Contains(fetcherImage, "@sha256:") {
		setupLog.Info("the fetcher image is not pinned by digest: it decides how an archive becomes "+
			"a build context, so republishing this tag with different content would change what "+
			"builds see without moving any input hash",
			"flag", "--fetcher-image", "value", fetcherImage)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeader,
		LeaderElectionID:       "oci-builder.lhns.de",
		Client: client.Options{
			Cache: &client.CacheOptions{
					// Read by name only: RBAC grants get, not list/watch, so a Secret cache could
					// not even start.
				DisableFor: []client.Object{&corev1.Secret{}},
			},
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start the manager")
		os.Exit(1)
	}

	// Built at startup so an unreadable CA or unusable signing key fails the process, not the
	// first artifact.
	registryTransport, registryCA, err := registry.Transport()
	if err != nil {
		setupLog.Error(err, "unable to trust the registry CA", "caFile", registry.CAFile)
		os.Exit(1)
	}
	attestor, err := registry.Attestor(context.Background(), os.Getenv("POD_NAMESPACE"))
	if err != nil {
		setupLog.Error(err, "unable to set up supply-chain signing", "secret", registry.SigningKeySecret)
		os.Exit(1)
	}
	if attestor.Key != nil {
		setupLog.Info("signing enabled; artifacts will carry a cosign signature",
			"note", "a signature changes nothing until something verifies it at admission")
	}

	defaults := registry.Default(os.Getenv("POD_NAMESPACE"))
	if defaults.SecretName != "" && defaults.Namespace == "" {
		setupLog.Error(nil, "POD_NAMESPACE is unset, so the default push credential cannot be "+
			"read; set it from the downward API")
		os.Exit(1)
	}

	var refresher *retention.Refresher

	if refreshInterval > 0 {
		source := retention.BuildSource{Client: mgr.GetClient()}
		refresher = &retention.Refresher{
			Client: mgr.GetClient(),
			Source: source,
			// The builder serves nothing, so the source answers pending from generations itself.
			Pending:  source,
			Interval: refreshInterval,
			//nolint:staticcheck // SA1019: the new events API has no Event method; same as above.
			Recorder:           mgr.GetEventRecorderFor("retention"),
			InsecureRegistries: registry.Insecure(),
			Transport:          registryTransport,
			Default:            defaults,
		}
		if err := refresher.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to set up retention refresh")
			os.Exit(1)
		}
		setupLog.Info("retention refresh enabled", "interval", refreshInterval)
	} else {
		// Especially worth saying here: a build cannot be reproduced from its spec (ADR 0025).
		setupLog.Info("retention refresh DISABLED; a registry with an expiry policy will delete " +
			"images this operator's builds still reference, and a build cannot be reproduced")
	}

	if err := (&buildcontroller.ImageBuildReconciler{
		Client:    mgr.GetClient(),
		Default:   defaults,
		Transport: registryTransport,
		Attestor:  attestor,
		Refresher: refresher,
		Export:    exportFlags.Options(),
		//nolint:staticcheck // SA1019: the new events API has no Event method; see the composer.
		Recorder: mgr.GetEventRecorderFor("imagebuild-controller"),
		// Guarded, because a fetch context makes the controller GET a user-supplied URL (threat
		// I6). The fetch inside the build pod is deliberately unguarded.
		HTTPClient: guardedClient(fetchDenyPrivate),
		JobConfig: newJobConfig(jobFlags{
			BuilderImage:       builderImage,
			FrontendImage:      frontendImage,
			FetcherImage:       fetcherImage,
			ContextBaseURL:     contextBaseURL,
			SourceDateEpoch:    sourceDateEpoch,
			InsecureRegistries: registry.Insecure(),
			RegistryCA:         registryCA,
			SBOM:               registry.SBOM,
			Provenance:         registry.Provenance,
		}),
		HistoryLimit:         historyLimit,
		RequirePinnedSources: requirePinnedSources,
		BuildPollInterval:    buildPollInterval,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up the ImageBuild controller")
		os.Exit(1)
	}

	// The context endpoint runs on every replica, not only the leader: build pods started under a
	// previous leader still need their source.
	if contextBaseURL == "" {
		setupLog.Info("WARNING: --context-base-url is unset, so build pods will fetch " +
			"source-controller directly. source-controller serves artifacts unauthenticated, so " +
			"every build pod can then read every namespace's source. See ADR 0044.")
	} else {
		proxy := &buildcontroller.ContextProxy{
			Client: mgr.GetClient(),
			HTTP:   guardedClient(fetchDenyPrivate),
		}
		srv := buildcontroller.ContextServer(contextAddr, proxy)
		if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			go func() {
				<-ctx.Done()
				_ = srv.Close()
			}()
			setupLog.Info("serving build contexts", "addr", contextAddr, "url", contextBaseURL)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})); err != nil {
			setupLog.Error(err, "unable to start the context endpoint")
			os.Exit(1)
		}
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

// jobFlags is every operator-supplied value a build Job needs, as a type so newJobConfig can be
// tested (TestEveryJobFlagIsWired).
type jobFlags struct {
	BuilderImage       string
	FrontendImage      string
	FetcherImage       string
	ContextBaseURL     string
	SourceDateEpoch    string
	InsecureRegistries []string
	RegistryCA         []byte
	SBOM               bool
	Provenance         bool
}

// newJobConfig copies the flags across. Every field, every time -- see TestEveryJobFlagIsWired.
func newJobConfig(f jobFlags) buildcontroller.JobConfig {
	return buildcontroller.JobConfig{
		BuilderImage:       f.BuilderImage,
		FrontendImage:      f.FrontendImage,
		FetcherImage:       f.FetcherImage,
		ContextBaseURL:     f.ContextBaseURL,
		SourceDateEpoch:    f.SourceDateEpoch,
		InsecureRegistries: f.InsecureRegistries,
		RegistryCA:         f.RegistryCA,
		SBOM:               f.SBOM,
		Provenance:         f.Provenance,
	}
}
