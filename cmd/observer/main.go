// Command observer runs the kargo-argocd-observer controller manager.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/mkutlak/kargo-argocd-observer/internal/controller"
	"github.com/mkutlak/kargo-argocd-observer/internal/version"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr string
		probeAddr   string
		leaderElect bool
		dryRun      bool
		observeMode string
		syncPeriod  time.Duration
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the health probe endpoint binds to.")
	flag.BoolVar(&leaderElect, "leader-elect", false,
		"Enable leader election for the controller manager.")
	flag.BoolVar(&dryRun, "dry-run", false,
		"Log and emit events instead of creating Promotions.")
	flag.StringVar(&observeMode, "observe-mode", controller.ObserveModeOptOut,
		"Which annotated Applications to act on: 'opt-out' observes all unless ignored; "+
			"'opt-in' additionally requires the kargo-observer.kutlak.cc/observe=\"true\" annotation.")
	flag.DurationVar(&syncPeriod, "sync-period", 10*time.Minute,
		"How often to re-reconcile all Applications as a backstop for missed events "+
			"(e.g. Freight produced after a FreightMissing verdict).")
	zapOpts := zap.Options{}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	setupLog := ctrl.Log.WithName("setup")

	observeMode, err := controller.ParseObserveMode(observeMode)
	if err != nil {
		setupLog.Error(err, "invalid --observe-mode")
		os.Exit(1)
	}

	if err := validateSyncPeriod(syncPeriod); err != nil {
		setupLog.Error(err, "invalid --sync-period")
		os.Exit(1)
	}

	setupLog.Info("starting kargo-argocd-observer",
		"version", version.Version,
		"buildDate", version.BuildDate,
		"buildRef", version.BuildRef,
		"observeMode", observeMode,
		"syncPeriod", syncPeriod,
	)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "kargo-argocd-observer.kutlak.cc",
		Cache:                  cache.Options{SyncPeriod: &syncPeriod},
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	reconciler := &controller.ApplicationReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		// The corev1 events API keeps the reconciler on record.EventRecorder,
		// which record.NewFakeRecorder can stand in for in tests.
		Recorder:    mgr.GetEventRecorderFor("kargo-argocd-observer"), //nolint:staticcheck // SA1019: deliberate, see above
		DryRun:      dryRun,
		ObserveMode: observeMode,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up controller", "controller", "Application")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}

// validateSyncPeriod rejects a non-positive resync interval, which would
// disable or thrash the periodic reconcile backstop instead of pacing it.
func validateSyncPeriod(d time.Duration) error {
	if d <= 0 {
		return fmt.Errorf("sync-period must be positive, got %s", d)
	}
	return nil
}
