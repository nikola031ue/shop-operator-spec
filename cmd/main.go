/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	shophubv1alpha1 "github.com/nikola031ue/shop-operator/api/v1alpha1"
	"github.com/nikola031ue/shop-operator/internal/chain"
	"github.com/nikola031ue/shop-operator/internal/controller"
	"github.com/nikola031ue/shop-operator/internal/shopapi"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(shophubv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var webhookPort int
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Port the webhook server listens on. "+
		"Defaults to 9443. Set -1 to disable the webhook server.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	// Platform configuration (data-model.md "Platform configuration"; set by the Helm chart).
	var cfg controller.Config
	flag.StringVar(&cfg.ShopImage, "shop-image", "nginx", "Placeholder Shop application image repository.")
	flag.StringVar(&cfg.ShopImageTag, "shop-image-tag", "1.27.3",
		"Placeholder Shop application image tag; must be pinned, never 'latest'.")
	flag.StringVar(&cfg.DiscordGuildID, "discord-guild-id", "", "Discord server (guild) ID.")
	flag.StringVar(&cfg.DiscordTokenSecret.Name, "discord-token-secret", "shop-operator-discord",
		"Secret in the operator namespace holding the Discord bot token under key 'token'.")
	// Spec 004 (operator 0.3.0): namespace scoping and per-Shop routing.
	flag.StringVar(&cfg.WatchNamespace, "watch-namespace", "shops",
		"The only namespace whose Shops and Wallets are reconciled and cached. DiscordChannels are "+
			"also reconciled in the operator's own namespace (platform alerts channel).")
	flag.StringVar(&cfg.ShopDomain, "shop-domain", "shops.localhost",
		"Host suffix: a Shop is served at <shop>.<shop-domain>.")
	flag.StringVar(&cfg.IngressClass, "ingress-class", "traefik", "spec.ingressClassName of every Shop Ingress.")
	flag.StringVar(&cfg.IngressNamespace, "ingress-namespace", "traefik",
		"Namespace of the entry point; the Shop NetworkPolicy admits it.")
	flag.StringVar(&cfg.ShopHubNamespace, "shophub-namespace", "shophub",
		"Namespace of ShopHub; the Shop NetworkPolicy admits the ShopHub app pods there.")
	flag.StringVar(&cfg.ShopHubPodSelector, "shophub-pod-selector",
		"app.kubernetes.io/name=shophub,app.kubernetes.io/instance=shophub",
		"Labels (k=v,...) of the ShopHub app pods; only they are admitted from --shophub-namespace.")
	flag.StringVar(&cfg.TrustedProxyHost, "trusted-proxy-host", "traefik-proxies.traefik.svc.cluster.local",
		"DNS name whose addresses are the only proxies the shop app trusts (TRUSTED_PROXY_HOST).")
	// Spec 005 (operator 0.4.0): payments and the chain watcher.
	flag.StringVar(&cfg.OperatorPodSelector, "operator-pod-selector",
		"app.kubernetes.io/name=shop-operator,app.kubernetes.io/instance=shop-operator",
		"Labels (k=v,...) of the operator's pods; only they are admitted on a shop's internal port.")
	payments := controller.DefaultPayments("")
	flag.StringVar(&payments.TokenAddress, "payments-token-address", "",
		"mUSDT token contract address; empty turns payments and the watcher off.")
	flag.Int64Var(&payments.ChainID, "payments-chain-id", payments.ChainID, "Chain id of the token (Sepolia 11155111).")
	flag.Uint64Var(&payments.Confirmations, "payments-confirmations", payments.Confirmations,
		"Blocks that must follow a transfer's block before it counts.")
	flag.DurationVar(&payments.PollInterval, "payments-poll-interval", payments.PollInterval,
		"How often the watcher reads the chain head.")
	flag.Uint64Var(&payments.LogsBlockRange, "payments-logs-block-range", payments.LogsBlockRange,
		"Most blocks one log query covers (free provider plans limit it).")
	flag.IntVar(&payments.OrderPaymentMinutes, "payments-order-minutes", payments.OrderPaymentMinutes,
		"Shop payment window in minutes (ORDER_PAYMENT_MINUTES).")
	flag.StringVar(&payments.ExplorerTxURL, "payments-explorer-tx-url", payments.ExplorerTxURL,
		"Explorer URL prefix for a transaction hash (EXPLORER_TX_URL).")
	flag.StringVar(&payments.RPCSecret.Name, "payments-rpc-secret", "shop-operator-rpc",
		"Hand-made Secret in the operator namespace whose key 'url' is the provider endpoint (it holds an API key).")
	flag.DurationVar(&payments.StallAfter, "payments-stall-after", payments.StallAfter,
		"A Wallet whose chain reading is further behind than this reports PaymentsWatching=False/Stalled.")
	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	cfg.DiscordTokenSecret.Namespace = os.Getenv("POD_NAMESPACE")
	if cfg.DiscordTokenSecret.Namespace == "" {
		cfg.DiscordTokenSecret.Namespace = "shop-operator-system"
	}
	payments.RPCSecret.Namespace = cfg.DiscordTokenSecret.Namespace
	cfg.Payments = payments

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
		Port:    webhookPort,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	if err := cfg.Validate(); err != nil {
		setupLog.Error(err, "Invalid configuration")
		os.Exit(1)
	}

	restCfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "shop-operator.shophub.io",
		// Cache only Secrets the operator created (research R14). The hand-created Discord
		// token Secret is read with mgr.GetAPIReader() instead.
		// With --watch-namespace, every kind is cached in that namespace only (spec 004 FR-026).
		Cache: cfg.CacheOptions(),
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// Only watch database kinds the cluster serves (research R5): the operator must start on a
	// cluster without CNPG or the Redis operator.
	dbKinds, err := controller.DetectDatabaseKinds(restCfg)
	if err != nil {
		setupLog.Error(err, "Failed to discover database kinds")
		os.Exit(1)
	}
	setupLog.Info("Database kinds", "postgres", dbKinds.Postgres, "redis", dbKinds.Redis)
	setupLog.Info("Scope", "watchNamespace", cfg.WatchNamespace, "operatorNamespace", cfg.OperatorNamespace())

	if err := controller.SetupIndexes(context.Background(), mgr); err != nil {
		setupLog.Error(err, "Failed to set up field indexes")
		os.Exit(1)
	}

	if err := (&controller.ShopReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Config:    cfg,
		DBKinds:   dbKinds,
		APIReader: mgr.GetAPIReader(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "shop")
		os.Exit(1)
	}
	shops := shopapi.NewHTTPClient()
	if err := (&controller.WalletReconciler{
		Config:    cfg,
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		APIReader: mgr.GetAPIReader(),
		Shops:     shops,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "wallet")
		os.Exit(1)
	}
	if err := (&controller.DiscordChannelReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		APIReader: mgr.GetAPIReader(),
		Config:    cfg,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "discordchannel")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	// Spec 005: the single chain watcher (leader-elected, so one active replica).
	if err := mgr.Add(&chain.Watcher{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Config: cfg, Shops: shops,
		Dial: chain.DialEthclient, Log: ctrl.Log.WithName("payment-watcher"),
	}); err != nil {
		setupLog.Error(err, "Failed to add the payment watcher")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
