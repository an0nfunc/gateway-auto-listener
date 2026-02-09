package main

import (
	"flag"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/an0nfunc/gateway-auto-listener/internal/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
	version  = "dev"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
}

func main() {
	var (
		metricsAddr                string
		probeAddr                  string
		gatewayName                string
		gatewayNamespace           string
		allowedDomainSuffix        string
		validatedNSPrefix          string
		allowedHostnamesAnnotation string
		showVersion                bool
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&gatewayName, "gateway-name", "default", "Name of the Gateway to manage listeners on.")
	flag.StringVar(&gatewayNamespace, "gateway-namespace", "nginx-gateway", "Namespace of the Gateway.")
	flag.StringVar(&allowedDomainSuffix, "allowed-domain-suffix", "", "Domain suffix for tenant hostnames (e.g., example.com). Empty disables suffix validation.")
	flag.StringVar(&validatedNSPrefix, "validated-ns-prefix", "", "Namespace prefix triggering hostname validation. Empty disables validation entirely.")
	flag.StringVar(&allowedHostnamesAnnotation, "allowed-hostnames-annotation", "gateway-auto-listener/allowed-hostnames", "Namespace annotation key for allowed custom hostnames.")
	flag.BoolVar(&showVersion, "version", false, "Print version and exit.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         true,
		LeaderElectionID:       "gateway-auto-listener.an0nfunc.github.io",
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.HTTPRouteReconciler{
		Client:                     mgr.GetClient(),
		Scheme:                     mgr.GetScheme(),
		Recorder:                   mgr.GetEventRecorderFor("gateway-auto-listener"),
		GatewayName:                gatewayName,
		GatewayNamespace:           gatewayNamespace,
		AllowedDomainSuffix:        allowedDomainSuffix,
		ValidatedNSPrefix:          validatedNSPrefix,
		AllowedHostnamesAnnotation: allowedHostnamesAnnotation,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "HTTPRoute")
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

	setupLog.Info("starting manager", "version", version)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
