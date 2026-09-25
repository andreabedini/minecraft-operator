// Command operator runs the MinecraftInstance controller.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/andreabedini/minecraft-operator/api/v1alpha1"
	"github.com/andreabedini/minecraft-operator/internal/controller"
	"github.com/andreabedini/minecraft-operator/internal/plan"
	"github.com/andreabedini/minecraft-operator/internal/upstream"
)

// version is set with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		metricsAddr       = flag.String("metrics-bind-address", ":8080", "metrics endpoint address")
		probeAddr         = flag.String("health-probe-bind-address", ":8081", "health probe address")
		leaderElect       = flag.Bool("leader-elect", false, "enable leader election")
		supervisorImage   = flag.String("supervisor-image", envOr("SUPERVISOR_IMAGE", "ghcr.io/andreabedini/minecraft-operator/supervisor:"+version), "supervisor image copied into server pods")
		javaImageTemplate = flag.String("java-image-template", envOr("JAVA_IMAGE_TEMPLATE", plan.DefaultJavaImageTemplate), "JRE image template; %d is the Java major")
	)
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	logger := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *probeAddr,
		LeaderElection:         *leaderElect,
		LeaderElectionID:       "minecraft-operator.minecraft.bedini.au",
	})
	if err != nil {
		logger.Error(err, "unable to create manager")
		os.Exit(1)
	}

	reconciler := &controller.MinecraftInstanceReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Recorder:          mgr.GetEventRecorderFor("minecraft-operator"),
		Resolver:          upstream.NewResolver("minecraft-operator/" + version),
		SupervisorImage:   *supervisorImage,
		JavaImageTemplate: *javaImageTemplate,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to create controller")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		logger.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		logger.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
	logger.Info("starting", "version", version, "supervisorImage", *supervisorImage)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "manager exited")
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
