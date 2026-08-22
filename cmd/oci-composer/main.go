// Command oci-composer runs the ImageComposition controller.
//
// One binary, no privileges, no daemon: assembly happens in-process, and the result is pushed to a
// registry -- the one the chart bundles, or one the operator supplies (ADR 0035).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/cache"
	"github.com/lhns/kube-oci-composer/internal/controller"
	"github.com/lhns/kube-oci-composer/internal/oci"
	"github.com/lhns/kube-oci-composer/internal/opts"
	"github.com/lhns/kube-oci-composer/internal/retention"
	"github.com/lhns/kube-oci-composer/internal/store"
)

// Stamped at build time via -ldflags, matching the sibling operators.
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
	// Everything both controllers share about publishing, trust and supply chain.
	var registry opts.Registry
	registry.Bind(flag.CommandLine)

	var (
		metricsAddr          string
		probeAddr            string
		enableLeaderElection bool
		showVersion          bool
		cacheDir             string
		s3Endpoint           string
		s3Bucket             string
		s3Prefix             string
		s3Region             string
		s3PathStyle          bool
		refreshInterval      time.Duration
		fetchDenyPrivate     bool
		requirePinnedSources bool
		keepBuilds           int
		gcKeepBuilds         int
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election, ensuring only one active controller manager.")
	flag.StringVar(&cacheDir, "cache-dir", "/var/cache/oci-composer",
		"Directory holding fetched layer sources, keyed by digest. Assembly reads from here, so a "+
			"local directory is always used even when object storage is configured behind it.")

	flag.StringVar(&s3Endpoint, "s3-endpoint", "",
		"S3 endpoint backing the layer cache, e.g. https://s3.example.com. A scheme is required: "+
			"without one there is no way to tell whether TLS was intended, and guessing wrong would "+
			"ship credentials in plaintext. Leave empty to keep the cache local to the pod, in which "+
			"case a restart re-fetches every layer from upstream.")
	flag.StringVar(&s3Bucket, "s3-bucket", "", "Bucket for the layer cache. Must already exist.")
	flag.StringVar(&s3Prefix, "s3-prefix", "", "Key prefix, so one bucket can be shared.")
	flag.StringVar(&s3Region, "s3-region", "default",
		"S3 region. Most self-hosted gateways ignore it but require a value; Ceph RGW expects "+
			`"default" rather than an AWS region name.`)
	flag.BoolVar(&s3PathStyle, "s3-path-style", true,
		"Use path-style addressing (host/bucket/key) instead of virtual-host style. Required by "+
			"most self-hosted gateways, whose certificate does not cover per-bucket subdomains.")

	flag.DurationVar(&refreshInterval, "retention-refresh-interval", retention.DefaultInterval,
		"How often to re-pull the images every live object still references, so that a registry "+
			"with an expiry policy does not reclaim them. Zero disables it.\n"+
			"This must stay MUCH shorter than the registry's retention window -- the ratio is the "+
			"guarantee, not either number, and the default assumes a window of 30 days. Refreshing "+
			"only reads: it needs no write or delete permission and can destroy nothing. See "+
			"ADR 0031.")
	flag.BoolVar(&fetchDenyPrivate, "fetch-deny-private", false,
		"Refuse `fetch` URLs that resolve to private, loopback or CGNAT addresses. Cloud metadata "+
			"endpoints on link-local addresses are ALWAYS refused, flag or no flag, because they "+
			"hand out credentials and no layer source lives there. The rest is opt-in: an artifact "+
			"server on a private address is this project's most ordinary source, so refusing those "+
			"by default would be a guard people turn off. See ADR 0036 (threat I6).")
	flag.BoolVar(&requirePinnedSources, "require-pinned-sources", false,
		"Refuse any sourceRef (or ImageBuild context) that names no revision. Pinning is optional by design -- a composition that tracks a branch is a legitimate thing to want (ADR 0026) -- so this is how an operator decides otherwise for a whole cluster. Objects that omit `revision:` go Stalled with a message saying so.")
	flag.IntVar(&keepBuilds, "keep-builds", ociv1alpha1.DefaultHistoryLimit,
		"How many past builds to retain per object, unless the object overrides it. Retention is "+
			"the ONLY thing keeping an old digest pullable, so reverting a commit or rescheduling a "+
			"pod pinned to an older build both depend on it. Layers are shared between builds, so a "+
			"generous value costs far less than the count suggests. Same name and meaning on both "+
			"controllers.")
	// Deprecated alias. The gc- prefix was misleading: this caps status.history, and collection
	// merely honours that cap. Kept because silently dropping a flag someone set in a values file
	// becomes a crash-loop on an unknown flag, which is a worse upgrade than a rename.
	flag.IntVar(&gcKeepBuilds, "gc-keep-builds", 0, "Deprecated alias for --keep-builds.")

	flag.BoolVar(&showVersion, "version", false, "Print version information and exit.")

	zapOpts := zap.Options{Development: false}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	keepBuilds = effectiveKeepBuilds(keepBuilds, gcKeepBuilds)

	if showVersion {
		fmt.Printf("kube-oci-composer %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	// Credentials come from the environment, never from a flag: flags show up in `ps`, in the
	// pod spec, and in every `kubectl describe`. This matches how the rest of this estate injects
	// S3 credentials, via secretKeyRef into the standard AWS variable names.
	s3Config := store.S3Config{
		Endpoint:        s3Endpoint,
		Bucket:          s3Bucket,
		Prefix:          s3Prefix,
		Region:          s3Region,
		PathStyle:       s3PathStyle,
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}

	var remote store.Store
	if s3Endpoint != "" {
		s3, err := store.NewS3(s3Config)
		if err != nil {
			setupLog.Error(err, "invalid S3 configuration")
			os.Exit(1)
		}
		remote = s3
		setupLog.Info("layer cache backed by S3",
			"endpoint", s3Endpoint, "bucket", s3Bucket, "prefix", s3Prefix, "pathStyle", s3PathStyle)
	} else if s3Bucket != "" {
		setupLog.Error(nil, "--s3-bucket is set but --s3-endpoint is not; the cache would stay local")
		os.Exit(1)
	}

	layerCache, err := cache.New(cacheDir, remote)
	if err != nil {
		setupLog.Error(err, "unable to set up the layer cache")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "oci-composer.lhns.de",
		Client: client.Options{
			Cache: &client.CacheOptions{
				// Secrets are read by name and read rarely. Caching them would mean watching
				// EVERY Secret in the cluster and holding them all in memory — a blast radius
				// wildly out of proportion to reading one referenced push credential. Reads go
				// straight to the API server instead.
				DisableFor: []client.Object{&corev1.Secret{}},
			},
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	readiness := &controller.Readiness{Client: mgr.GetClient()}

	// Everything the two controllers share about publishing, trust and supply chain. Built here so
	// a CA that cannot be read or a key that cannot sign fails the process rather than the first
	// artifact -- the same reasoning the chart applies to an unpinned builder image.
	// The CA bytes themselves are discarded: only the builder passes them on, into each build
	// pod. This controller pushes from its own process.
	registryTransport, _, err := registry.Transport()
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

	if err := (&controller.ImageCompositionReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		// Deprecated in favour of GetEventRecorder, which returns the NEW events API. That is a
		// real migration rather than a rename: events.EventRecorder has no Event method, only
		// Eventf(regarding, related, eventtype, reason, action, note, ...), so every call site
		// and the FakeRecorder the tests rely on change with it. Worth doing deliberately rather
		// than as a drive-by while repairing CI.
		//nolint:staticcheck // SA1019: deliberate; see above.
		Recorder:             mgr.GetEventRecorderFor("imagecomposition-controller"),
		Readiness:            readiness,
		Default:              defaults,
		Cache:                layerCache,
		HistoryLimit:         keepBuilds,
		Fetcher:              oci.NewFetcherWithGuard(oci.DialGuard{DenyPrivate: fetchDenyPrivate}),
		RequirePinnedSources: requirePinnedSources,
		InsecureRegistries:   registry.Insecure(),
		Attestor:             attestor,
		Transport:            registryTransport,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ImageComposition")
		os.Exit(1)
	}

	if refreshInterval > 0 {
		refresher := &retention.Refresher{
			Client:   mgr.GetClient(),
			Source:   retention.CompositionSource{Client: mgr.GetClient()},
			Pending:  readiness,
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
		setupLog.Info("retention refresh DISABLED; a registry with an expiry policy will delete " +
			"images this operator's objects still reference (ADR 0031)")
	}

	// Liveness stays a bare ping. A replica that has not won the lease is alive and must not be
	// restarted just because it is not the leader.
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}

	// A bare ping. Readiness used to gate on the served store being warm so the pod would not join
	// the Service and 404 pulls while rebuilding; there is no store and no Service to join now.
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// effectiveKeepBuilds resolves --keep-builds against its deprecated alias --gc-keep-builds.
//
// The old name wins only when the new one was left at its default, so a deployment that sets both
// gets the one it most likely meant, and one that sets only the old name keeps working. Dropping the
// alias outright would turn a values file written against the previous release into a crash-loop on
// an unknown flag, which is a worse upgrade than a rename.
func effectiveKeepBuilds(keepBuilds, deprecated int) int {
	if deprecated > 0 && keepBuilds == ociv1alpha1.DefaultHistoryLimit {
		return deprecated
	}
	return keepBuilds
}
