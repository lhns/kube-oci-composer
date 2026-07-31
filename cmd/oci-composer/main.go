// Command oci-composer runs the ImageComposition controller and its built-in OCI endpoint.
//
// One binary, no privileges, no daemon: assembly happens in-process. The serving endpoint runs
// alongside the manager so a cluster with no registry needs nothing else installed.
package main

import (
	"flag"
	"os"

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
	"github.com/lhns/kube-oci-composer/internal/controller"
	"github.com/lhns/kube-oci-composer/internal/serve"
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
		metricsAddr          string
		probeAddr            string
		enableLeaderElection bool
		servingHost          string
		servingAddr          string
		storageDir           string
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
	flag.StringVar(&storageDir, "storage-dir", "/var/lib/oci-composer",
		"Directory backing the served blobs. An emptyDir is fine: composition is deterministic, so "+
			"anything lost is rebuilt by the reconcile that runs at startup. Note that rebuilding "+
			"re-fetches every layer from upstream, and the pod stays unready until it has.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

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
		server, err = serve.New(servingHost, servingAddr, storageDir)
		if err != nil {
			setupLog.Error(err, "unable to set up the serving endpoint")
			os.Exit(1)
		}
		// Runnable rather than a bare goroutine, so the manager owns its lifecycle and a
		// listener failure takes the process down instead of leaving a controller that
		// reports Ready for artifacts nothing can pull.
		if err := mgr.Add(server); err != nil {
			setupLog.Error(err, "unable to register the serving endpoint")
			os.Exit(1)
		}
		setupLog.Info("serving endpoint enabled", "host", servingHost, "addr", servingAddr)
	} else {
		setupLog.Info("no serving host configured; only ImageCompositions with spec.push will reconcile")
	}

	readiness := &controller.Readiness{Client: mgr.GetClient()}

	if err := (&controller.ImageCompositionReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor("imagecomposition-controller"),
		Server:    server,
		Readiness: readiness,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ImageComposition")
		os.Exit(1)
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
