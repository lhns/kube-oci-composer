// Command oci-composer runs the ImageComposition controller and its built-in OCI endpoint.
//
// One binary, no privileges, no daemon: assembly happens in-process. The serving endpoint runs
// alongside the manager so a cluster with no registry needs nothing else installed.
package main

import (
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
	gcpkg "github.com/lhns/kube-oci-composer/internal/gc"
	"github.com/lhns/kube-oci-composer/internal/serve"
	"github.com/lhns/kube-oci-composer/internal/store"
)

// Stamped at build time via -ldflags, matching the sibling operators.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// Storage backend names, kept as constants so the flag validation and the selection cannot drift.
const (
	backendDisk = "disk"
	backendS3   = "s3"
)

// newBlobStore picks where served blobs live.
func newBlobStore(backend, dir string, s3 store.Store) (store.Store, error) {
	switch backend {
	case backendS3:
		if s3 == nil {
			return nil, fmt.Errorf("storage backend %q requires S3 to be configured", backend)
		}
		return s3, nil
	case backendDisk:
		return store.NewDisk(dir)
	default:
		return nil, fmt.Errorf("unknown storage backend %q", backend)
	}
}

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
		metricsAddr           string
		probeAddr             string
		enableLeaderElection  bool
		showVersion           bool
		servingHost           string
		servingAddr           string
		storageBackend        string
		storageDir            string
		cacheDir              string
		s3Endpoint            string
		s3Bucket              string
		s3Prefix              string
		s3Region              string
		s3PathStyle           bool
		s3Presign             bool
		gcInterval            time.Duration
		gcGrace               time.Duration
		keepBuilds            int
		gcKeepBuilds          int
		gcDryRun              bool
		sharedStorage         bool
		standbyReplayInterval time.Duration
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election, ensuring only one active controller manager.")
	flag.StringVar(&servingHost, "serving-host", "",
		"Externally reachable host workloads pull artifacts from, e.g. oci.example.com. "+
			"Required unless every ImageComposition sets spec.push, because it is what "+
			"status.artifact.ref is built from.")
	flag.StringVar(&servingAddr, "serving-bind-address", ":5000", "Address the OCI endpoint binds to.")
	flag.StringVar(&storageBackend, "storage-backend", backendDisk,
		"Where served blobs live: \"disk\" or \"s3\". S3 requires --s3-endpoint. Note this only "+
			"moves the blobs; manifests are held in memory by the registry implementation and are "+
			"rebuilt at startup either way, so S3 here saves re-uploading layers after a restart "+
			"rather than removing the rebuild.")
	flag.StringVar(&storageDir, "storage-dir", "/var/lib/oci-composer",
		"Directory backing the served blobs. An emptyDir is fine: composition is deterministic, so "+
			"anything lost is rebuilt by the reconcile that runs at startup. Note that rebuilding "+
			"re-fetches every layer from upstream unless a cache is configured, and the pod stays "+
			"unready until it has.")
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

	flag.BoolVar(&s3Presign, "s3-presign-blobs", false,
		"Redirect blob pulls to a presigned S3 URL so the bytes do not stream through the "+
			"controller. Off by default because it exposes the object-store endpoint to every "+
			"pulling client, which on a private gateway may not be reachable from every node.")

	flag.DurationVar(&gcInterval, "gc-interval", gcpkg.DefaultInterval,
		"How often to reclaim blobs and cache entries nothing references. Zero disables collection.")
	flag.DurationVar(&gcGrace, "gc-grace", gcpkg.DefaultGrace,
		"Never reclaim anything written more recently than this. A build writes its blobs before "+
			"recording them in status, so a sweep landing in that window would delete content that "+
			"is moments from being referenced.")
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
	flag.BoolVar(&gcDryRun, "gc-dry-run", false,
		"Log what garbage collection would reclaim without deleting anything.")

	flag.BoolVar(&sharedStorage, "shared-storage", false,
		"Assert that --storage-dir is reachable by every replica (an RWX volume), which lets "+
			"NON-LEADER replicas serve pulls instead of standing by. Implied by --storage-backend=s3, "+
			"which is shared by construction. Serving is read-only, so this is safe; publishing, "+
			"garbage collection and status writes stay on the leader either way. Set it wrongly, with "+
			"a node-local directory and more than one replica, and standbys will answer 404 for "+
			"artifacts they do not have.")
	flag.DurationVar(&standbyReplayInterval, "standby-replay-interval", time.Minute,
		"How often a replica refreshes its manifest map from the shared store, picking up builds "+
			"the leader published since the last pass. Only used with shared storage. Bounds how "+
			"long a non-leader can 404 an artifact that was just built.")

	flag.BoolVar(&showVersion, "version", false, "Print version information and exit.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	keepBuilds = effectiveKeepBuilds(keepBuilds, gcKeepBuilds)

	if showVersion {
		fmt.Printf("kube-oci-composer %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

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

	// Validate before building the manager, so a typo in chart values fails immediately rather
	// than producing a controller that reports Ready and cannot cache anything.
	if storageBackend != backendDisk && storageBackend != backendS3 {
		setupLog.Error(nil, "invalid --storage-backend", "value", storageBackend,
			"expected", []string{backendDisk, backendS3})
		os.Exit(1)
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

	if storageBackend == backendS3 && remote == nil {
		setupLog.Error(nil, "--storage-backend=s3 needs --s3-endpoint")
		os.Exit(1)
	}
	if s3Presign && remote == nil {
		setupLog.Error(nil, "--s3-presign-blobs needs an S3 backend")
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

	var server *serve.Server
	if servingHost != "" {
		blobStore, err := newBlobStore(storageBackend, storageDir, remote)
		if err != nil {
			setupLog.Error(err, "unable to set up blob storage")
			os.Exit(1)
		}
		server, err = serve.New(servingHost, servingAddr, blobStore, s3Presign)
		if err != nil {
			setupLog.Error(err, "unable to set up the serving endpoint")
			os.Exit(1)
		}
		// S3 is shared by construction, so making the operator assert it again would be noise.
		// A directory is ambiguous — node-local and an RWX mount look identical from in here —
		// so that case has to be asserted.
		server.SharedStorage = sharedStorage || storageBackend == backendS3
		// Runnable rather than a bare goroutine, so the manager owns its lifecycle and a
		// listener failure takes the process down instead of leaving a controller that
		// reports Ready for artifacts nothing can pull.
		if err := mgr.Add(server); err != nil {
			setupLog.Error(err, "unable to register the serving endpoint")
			os.Exit(1)
		}
		setupLog.Info("serving endpoint enabled",
			"host", servingHost, "addr", servingAddr, "sharedStorage", server.SharedStorage)
	} else {
		setupLog.Info("no serving host configured; only ImageCompositions with spec.push will reconcile")
	}

	readiness := &controller.Readiness{Client: mgr.GetClient()}

	// With shared storage every replica serves, so every replica needs the manifests that the
	// leader published — they live in an in-memory map, not in the store. Registered without
	// leader election, which is the entire point.
	if server != nil && server.SharedStorage {
		if err := mgr.Add(&controller.StandbyReplay{
			Client:    mgr.GetClient(),
			Server:    server,
			Readiness: readiness,
			Interval:  standbyReplayInterval,
		}); err != nil {
			setupLog.Error(err, "unable to register standby replay")
			os.Exit(1)
		}
		setupLog.Info("standby replay enabled; non-leader replicas will serve pulls")
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
		Recorder:     mgr.GetEventRecorderFor("imagecomposition-controller"),
		Server:       server,
		Readiness:    readiness,
		Cache:        layerCache,
		HistoryLimit: keepBuilds,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ImageComposition")
		os.Exit(1)
	}

	if gcInterval > 0 {
		collector := &gcpkg.Collector{
			Client:   mgr.GetClient(),
			Cache:    layerCache.Local,
			Pending:  readiness,
			Interval: gcInterval,
			Grace:    gcGrace,
			DryRun:   gcDryRun,
		}
		if server != nil {
			collector.Blobs = server.Blobs
		}
		if err := collector.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to set up garbage collection")
			os.Exit(1)
		}
		setupLog.Info("garbage collection enabled",
			"interval", gcInterval, "grace", gcGrace, "keepBuilds", keepBuilds, "dryRun", gcDryRun)
	} else {
		setupLog.Info("garbage collection disabled; blobs and cache entries will accumulate")
	}

	// Liveness stays a bare ping. A standby replica is alive and must not be restarted just
	// because it is not the leader.
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}

	// Readiness gates on the served store being warm, so the pod does not join the Service and
	// answer 404 to pulls while it is still rebuilding after a restart.
	readyCheck := healthz.Ping
	if server != nil {
		readyCheck = readiness.Check
	}
	if err := mgr.AddReadyzCheck("readyz", readyCheck); err != nil {
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
