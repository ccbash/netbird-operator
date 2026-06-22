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
	"strings"

	corev1 "k8s.io/api/core/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	kruntimeutil "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/controller"
	"github.com/netbirdio/kubernetes-operator/internal/logging"
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
	kruntimeutil.Must(nbv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	// NB Specific flags
	var (
		runtimeNamespace      string
		managementURL         string
		netbirdClientImage    string
		netbirdAPIKey         string
		advertiseLBs          bool
		defaultResourceGroups string
	)
	flag.StringVar(&runtimeNamespace, "runtime-namespace", "", "Namespace the controller is running in")
	flag.StringVar(&managementURL, "netbird-management-url", "https://api.netbird.io", "Management service URL")
	flag.StringVar(&netbirdClientImage, "netbird-client-image", "", "Image for netbird client container")
	flag.StringVar(&netbirdAPIKey, "netbird-api-key", "", "API key for NetBird API operations")
	flag.BoolVar(&advertiseLBs, "advertise-loadbalancers", true,
		"When true, Service type=LoadBalancer are advertised into NetBird by default (namespace/Service annotation netbird.io/advertise overrides).")
	flag.StringVar(&defaultResourceGroups, "default-resource-groups", "",
		"Comma-separated NetBird groups that advertised LoadBalancer resources join by default, so access policies can target them (netbird.io/groups annotation overrides).")

	// Controller generic flags
	var (
		metricsAddr          string
		metricsSecure        bool
		webhookCertPath      string
		webhookCertName      string
		webhookCertKey       string
		enableLeaderElection bool
		probeAddr            string
		enableWebhooks       bool
		logLevel             string
		logFormat            string
	)
	flag.StringVar(&logLevel, "log-level", "info",
		"Log verbosity: debug, info, warn, error, or a non-negative integer for higher debug verbosity.")
	flag.StringVar(&logFormat, "log-format", "json",
		"Log output format: json (structured) or console (human-readable).")

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.BoolVar(&metricsSecure, "metrics-secure", true,
		"Serve metrics over HTTPS and require authentication/authorization (TokenReview/SubjectAccessReview). "+
			"Set false only for trusted-network HTTP scraping.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.BoolVar(&enableWebhooks, "enable-webhooks", true, "If set, enable Mutating and Validating webhooks.")
	flag.Parse()

	zapOpts, err := logging.Options(logLevel, logFormat)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	_, err = url.Parse(managementURL)
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

	// Authenticate/authorize metrics scrapers (and serve over HTTPS) unless
	// explicitly disabled for a trusted-network HTTP setup.
	metricsOpts := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: metricsSecure,
	}
	if metricsSecure {
		metricsOpts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// Setup controller manager.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsOpts,
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

	if err := setupControllers(mgr, netbirdAPIKey, managementURL, netbirdClientImage, advertiseLBs, defaultResourceGroups); err != nil {
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
func setupControllers(mgr ctrl.Manager, netbirdAPIKey, managementURL, netbirdClientImage string, advertiseLBs bool, defaultResourceGroups string) error {
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
		Client:   mgr.GetClient(),
		Netbird:  nbClient,
		Recorder: mgr.GetEventRecorderFor("setupkey"),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup SetupKey controller: %w", err)
	}
	if err := (&controller.GroupReconciler{
		Client:   mgr.GetClient(),
		Netbird:  nbClient,
		Recorder: mgr.GetEventRecorderFor("group"),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup Group controller: %w", err)
	}
	if err := (&controller.ClusterProxyReconciler{
		Client:        mgr.GetClient(),
		ApiKey:        netbirdAPIKey,
		ManagementURL: managementURL,
		Recorder:      mgr.GetEventRecorderFor("clusterproxy"),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup ClusterProxy controller: %w", err)
	}

	// Layer-1 NetBird-mirror controllers.
	if err := controller.NewNetworkReconciler(mgr.GetClient(), nbClient, mgr.GetEventRecorderFor("network")).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup Network controller: %w", err)
	}
	if err := controller.NewNetworkResourceReconciler(mgr.GetClient(), nbClient, mgr.GetEventRecorderFor("networkresource")).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup NetworkResource controller: %w", err)
	}
	if err := controller.NewDNSZoneReconciler(mgr.GetClient(), nbClient, mgr.GetEventRecorderFor("dnszone")).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup DNSZone controller: %w", err)
	}
	if err := controller.NewDNSRecordReconciler(mgr.GetClient(), nbClient, mgr.GetEventRecorderFor("dnsrecord")).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup DNSRecord controller: %w", err)
	}

	if err := (&controller.NetworkRouterReconciler{
		Client:        mgr.GetClient(),
		Netbird:       nbClient,
		ClientImage:   netbirdClientImage,
		ManagementURL: managementURL,
		Recorder:      mgr.GetEventRecorderFor("networkrouter"),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup NetworkRouter controller: %w", err)
	}

	// Translation + exposure.
	if err := (&controller.LoadBalancerReconciler{
		Client:           mgr.GetClient(),
		DefaultAdvertise: advertiseLBs,
		DefaultGroups:    splitGroups(defaultResourceGroups),
		Recorder:         mgr.GetEventRecorderFor("loadbalancer"),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup LoadBalancer controller: %w", err)
	}
	if err := controller.NewReverseProxyServiceReconciler(mgr.GetClient(), nbClient, mgr.GetEventRecorderFor("reverseproxyservice")).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup ReverseProxyService controller: %w", err)
	}
	return nil
}

// splitGroups parses a comma-separated group list, trimming spaces and dropping
// empty entries.
func splitGroups(s string) []string {
	var out []string
	for _, g := range strings.Split(s, ",") {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return out
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
