// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"

	corev1 "k8s.io/api/core/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	kruntimeutil "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/controller"
	"github.com/netbirdio/kubernetes-operator/internal/version"
	nbwebhookv1 "github.com/netbirdio/kubernetes-operator/internal/webhook/v1"
)

var (
	scheme   = kruntime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	kruntimeutil.Must(clientgoscheme.AddToScheme(scheme))

	kruntimeutil.Must(corev1.AddToScheme(scheme))
	kruntimeutil.Must(gwv1.Install(scheme))
	kruntimeutil.Must(gwv1alpha2.Install(scheme))
	kruntimeutil.Must(nbv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	// NB Specific flags
	var (
		runtimeNamespace   string
		managementURL      string
		netbirdClientImage string
		netbirdAPIKey      string
		gatewayAPIEnabled  bool
	)
	flag.StringVar(&runtimeNamespace, "runtime-namespace", "", "Namespace the controller is running in")
	flag.StringVar(&managementURL, "netbird-management-url", "https://api.netbird.io", "Management service URL")
	flag.StringVar(&netbirdClientImage, "netbird-client-image", "", "Image for netbird client container")
	flag.StringVar(&netbirdAPIKey, "netbird-api-key", "", "API key for NetBird API operations")
	flag.BoolVar(&gatewayAPIEnabled, "gateway-api-enabled", false, "When true Gateway API resources will be reconciled.")

	// Controller generic flags
	var (
		metricsAddr          string
		webhookCertPath      string
		webhookCertName      string
		webhookCertKey       string
		enableLeaderElection bool
		probeAddr            string
		enableWebhooks       bool
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.BoolVar(&enableWebhooks, "enable-webhooks", true, "If set, enable Mutating and Validating webhooks.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	_, err := url.Parse(managementURL)
	if err != nil {
		setupLog.Error(err, "invalid management url")
		os.Exit(1)
	}
	runtimeNamespace, err = getRuntimeNamespace(runtimeNamespace)
	if err != nil {
		setupLog.Error(err, "unable to get runtime namespace")
		os.Exit(1)
	}
	if netbirdClientImage == "" {
		netbirdClientImage = version.NetbirdClientImage
	}

	// Setup webhook server.
	type TLSOption = func(*tls.Config)
	certWatcher, tlsOpt, err := func() (*certwatcher.CertWatcher, TLSOption, error) {
		if webhookCertPath == "" {
			return nil, nil, nil
		}

		certWatcher, err := certwatcher.New(
			filepath.Join(webhookCertPath, webhookCertName),
			filepath.Join(webhookCertPath, webhookCertKey),
		)
		if err != nil {
			return nil, nil, err
		}

		tlsOpt := func(config *tls.Config) {
			config.GetCertificate = certWatcher.GetCertificate
		}

		return certWatcher, tlsOpt, nil
	}()
	if err != nil {
		setupLog.Error(err, "Failed to initialize webhook certificate watcher")
		os.Exit(1)
	}
	webhookServer := webhook.NewServer(webhook.Options{TLSOpts: []TLSOption{tlsOpt}})

	// Setup controller manager.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		Client: client.Options{
			FieldOwner: "netbird-operator",
		},
		WebhookServer:           webhookServer,
		HealthProbeBindAddress:  probeAddr,
		LeaderElectionNamespace: runtimeNamespace,
		LeaderElection:          enableLeaderElection,
		LeaderElectionID:        "operator.netbird.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if enableWebhooks {
		if err = nbwebhookv1.SetupPodWebhookWithManager(mgr, managementURL, netbirdClientImage); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "Pod")
			os.Exit(1)
		}
	}

	if err := setupControllers(mgr, netbirdAPIKey, managementURL, netbirdClientImage, gatewayAPIEnabled); err != nil {
		setupLog.Error(err, "unable to set up controllers")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if certWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(certWatcher); err != nil {
			setupLog.Error(err, "unable to add webhook certificate watcher to manager")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	readyChecker := healthz.Ping
	if certWatcher != nil {
		readyChecker = mgr.GetWebhookServer().StartedChecker()
	}
	if err := mgr.AddReadyzCheck("readyz", readyChecker); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// setupControllers registers the NetBird controllers that require API access.
// It is a no-op (with a log line) when no API key is configured.
func setupControllers(mgr ctrl.Manager, netbirdAPIKey, managementURL, netbirdClientImage string, gatewayAPIEnabled bool) error {
	if len(netbirdAPIKey) == 0 {
		setupLog.Info("netbird API key not provided, ingress capabilities disabled")
		return nil
	}

	nbClient := netbird.NewWithOptions(
		netbird.WithManagementURL(managementURL),
		netbird.WithBearerToken(netbirdAPIKey),
		netbird.WithUserAgent(fmt.Sprintf("netbird-operator/%s (%s/%s)", version.BuildVersion(), runtime.GOOS, runtime.GOARCH)),
	)

	if err := (&controller.SetupKeyReconciler{
		Client:  mgr.GetClient(),
		Netbird: nbClient,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup SetupKey controller: %w", err)
	}
	if err := (&controller.GroupReconciler{
		Client:  mgr.GetClient(),
		Netbird: nbClient,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup Group controller: %w", err)
	}
	if err := (&controller.NetworkRouterReconciler{
		Client:        mgr.GetClient(),
		Netbird:       nbClient,
		ClientImage:   netbirdClientImage,
		ManagementURL: managementURL,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup NetworkRouter controller: %w", err)
	}
	if err := (&controller.NetworkResourceReconciler{
		Client:  mgr.GetClient(),
		Netbird: nbClient,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup NetworkResource controller: %w", err)
	}
	if err := (&controller.ClusterProxyReconciler{
		Client:        mgr.GetClient(),
		ApiKey:        netbirdAPIKey,
		ManagementURL: managementURL,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup ClusterProxy controller: %w", err)
	}

	if !gatewayAPIEnabled {
		return nil
	}
	if err := (&controller.GatewayClassReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup GatewayClass controller: %w", err)
	}
	if err := (&controller.GatewayReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup Gateway controller: %w", err)
	}
	if err := (&controller.HTTPRouteReconciler{
		Client:  mgr.GetClient(),
		Netbird: nbClient,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup HTTPRoute controller: %w", err)
	}
	if err := (&controller.TCPRouteReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup TCPRoute controller: %w", err)
	}
	if err := (&controller.NBServicePolicyReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup NBServicePolicy controller: %w", err)
	}
	return nil
}

func getRuntimeNamespace(runtimeNamespace string) (string, error) {
	if runtimeNamespace != "" {
		return runtimeNamespace, nil
	}
	inClusterNamespacePath := "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	b, err := os.ReadFile(inClusterNamespacePath)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("not running in-cluster, runtime namespace needs to be set")
	}
	if err != nil {
		return "", fmt.Errorf("error reading namespace file: %w", err)
	}
	return string(b), nil
}
