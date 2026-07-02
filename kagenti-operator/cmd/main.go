/*
Copyright 2025.

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
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	cmv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/spiffe/go-spiffe/v2/workloadapi"

	agentv1alpha1 "github.com/kagenti/operator/api/v1alpha1"
	"github.com/kagenti/operator/internal/agentcard"
	"github.com/kagenti/operator/internal/bootstrap"
	"github.com/kagenti/operator/internal/controller"
	"github.com/kagenti/operator/internal/discovery"
	"github.com/kagenti/operator/internal/keycloak"
	"github.com/kagenti/operator/internal/mlflow"
	"github.com/kagenti/operator/internal/signature"
	"github.com/kagenti/operator/internal/tekton"
	webhookconfig "github.com/kagenti/operator/internal/webhook/config"
	"github.com/kagenti/operator/internal/webhook/injector"
	webhookv1alpha1 "github.com/kagenti/operator/internal/webhook/v1alpha1"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agentv1alpha1.AddToScheme(scheme))
	utilruntime.Must(mlflow.AddToScheme(scheme))
	utilruntime.Must(tekton.AddToScheme(scheme))
	utilruntime.Must(cmv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// getOperatorNamespace returns the namespace the operator is running in.
// Reads from POD_NAMESPACE environment variable (set via downward API in deployment),
// falling back to kagenti-system if not set.
func getOperatorNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	setupLog.Info("POD_NAMESPACE not set, using default", "default", "kagenti-system")
	return "kagenti-system"
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)

	var enableClientRegistration bool
	var configPath string
	var featureGatesPath string

	var requireA2ASignature bool
	var signatureAuditMode bool
	var enforceNetworkPolicies bool
	var enableMLflow bool
	var enableOtelBootstrap bool
	var mlflowWorkspace string
	var mlflowExperimentName string

	var enableCardDiscovery bool

	var enableVerifiedFetch bool
	var verifiedFetchSpiffeSocket string

	var spireTrustDomain string
	var spireTrustBundleConfigMapName string
	var spireTrustBundleConfigMapNS string
	var spireTrustBundleConfigMapKey string
	var spireTrustBundleRefreshInterval time.Duration
	var svidExpiryGracePeriod time.Duration

	var keycloakAdminSecretNamespace string
	var keycloakRealm string
	var keycloakPublicURL string
	var clientAuthType string
	var spiffeIdpAlias string
	var credentialWaitTimeout string
	var enableAuthbridgeConfig bool
	var useSpiffeAuth bool
	var jwtSVIDPath string
	var operatorClientID string

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
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.BoolVar(&enableClientRegistration, "enable-client-registration", true,
		"Enable operator-managed Keycloak client registration for agent/tool workloads")
	flag.StringVar(&configPath, "config-path", "/etc/kagenti/config.yaml", "Path to platform config file")
	flag.StringVar(&featureGatesPath, "feature-gates-path",
		"/etc/kagenti/feature-gates/feature-gates.yaml", "Path to feature gates config file")
	flag.BoolVar(&requireA2ASignature, "require-a2a-signature", false,
		"Require A2A agent cards to have a valid signature")
	flag.BoolVar(&signatureAuditMode, "signature-audit-mode", false,
		"When true, log signature verification failures but don't block (use for rollout)")
	flag.BoolVar(&enforceNetworkPolicies, "enforce-network-policies", false,
		"Create NetworkPolicies to restrict traffic for agents with unverified signatures")
	flag.BoolVar(&enableMLflow, "enable-mlflow", false,
		"Enable MLflow experiment tracking integration")
	flag.BoolVar(&enableOtelBootstrap, "enable-otel-bootstrap", false,
		"Enable OTel collector bootstrap (ingress CA trust and ConfigMap assembly) at startup")
	flag.StringVar(&mlflowWorkspace, "mlflow-workspace", "",
		"Kubernetes namespace used as the x-mlflow-workspace header value (RHOAI only)")
	flag.StringVar(&mlflowExperimentName, "mlflow-experiment-name", "kagenti-traces",
		"MLflow experiment name; created automatically if it doesn't exist")

	flag.BoolVar(&enableCardDiscovery, "enable-card-discovery", false,
		"Enable automatic agent card discovery from AgentRuntime workloads into status.card")
	flag.BoolVar(&enableVerifiedFetch, "enable-verified-fetch", false,
		"Enable mTLS-authenticated fetch of agent cards via SPIFFE identity")
	flag.StringVar(&verifiedFetchSpiffeSocket, "verified-fetch-spiffe-socket",
		"unix:///spiffe-workload-api/spire-agent.sock",
		"SPIFFE Workload API socket path for verified fetch")

	flag.StringVar(&spireTrustBundleConfigMapName, "spire-trust-bundle-configmap", "",
		"Name of the ConfigMap containing the SPIRE trust bundle (SPIFFE JSON format)")
	flag.StringVar(&spireTrustBundleConfigMapNS, "spire-trust-bundle-configmap-namespace", "",
		"Namespace of the trust bundle ConfigMap")
	flag.StringVar(&spireTrustBundleConfigMapKey, "spire-trust-bundle-configmap-key", "bundle.spiffe",
		"Key within the trust bundle ConfigMap containing SPIFFE JSON data")
	flag.DurationVar(&spireTrustBundleRefreshInterval, "spire-trust-bundle-refresh-interval", 5*time.Minute,
		"How often to re-read the trust bundle")
	flag.DurationVar(&svidExpiryGracePeriod, "svid-expiry-grace-period", 30*time.Minute,
		"How far before the signing SVID expires to trigger a proactive workload restart for re-signing")

	flag.StringVar(&keycloakAdminSecretNamespace, "keycloak-admin-secret-namespace", "keycloak",
		"Namespace where keycloak-initial-admin secret is located")
	flag.StringVar(&keycloakRealm, "keycloak-realm", "kagenti",
		"Keycloak realm for agent client registration and identity")
	flag.StringVar(&clientAuthType, "client-auth-type", "client-secret",
		"Default client authentication type: client-secret or federated-jwt")
	flag.StringVar(&spiffeIdpAlias, "spiffe-idp-alias", "spire-spiffe",
		"Keycloak SPIFFE Identity Provider alias")
	flag.StringVar(&credentialWaitTimeout, "credential-wait-timeout", "120s",
		"How long AuthBridge waits for Keycloak credentials to become available")
	flag.BoolVar(&enableAuthbridgeConfig, "enable-authbridge-config", true,
		"Reconcile authbridge-config ConfigMap in namespaces labeled kagenti-enabled=true")
	flag.BoolVar(&useSpiffeAuth, "use-spiffe-auth", false,
		"Use JWT-SVID authentication for Keycloak client registration instead of admin credentials")
	flag.StringVar(&jwtSVIDPath, "jwt-svid-path", "/opt/jwt_svid.token",
		"Path to JWT-SVID file written by spiffe-helper sidecar (used when --use-spiffe-auth=true)")
	flag.StringVar(&operatorClientID, "operator-client-id", "",
		"Operator SPIFFE ID (e.g. spiffe://<domain>/ns/<ns>/sa/<sa>), used when --use-spiffe-auth=true")

	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	ctx := ctrl.SetupSignalHandler()

	// ========================================
	// Load platform configuration (AuthBridge)
	// ========================================
	configLoader := webhookconfig.NewConfigLoader(configPath)
	if err := configLoader.Load(); err != nil {
		setupLog.Error(err, "Failed to load platform config")
		os.Exit(1)
	}
	configLoader.OnChange(func(cfg *webhookconfig.PlatformConfig) {
		setupLog.Info("Platform config updated",
			"envoyImage", cfg.Images.EnvoyProxy,
			"proxyPort", cfg.Proxy.Port)
	})
	if err := configLoader.Watch(ctx); err != nil {
		setupLog.Error(err, "Failed to start config watcher")
		// Non-fatal - continue without hot reload
	}

	// ========================================
	// Load feature gates (AuthBridge)
	// ========================================
	featureGateLoader := webhookconfig.NewFeatureGateLoader(featureGatesPath)
	if err := featureGateLoader.Load(); err != nil {
		setupLog.Error(err, "Failed to load feature gates")
		os.Exit(1)
	}
	featureGateLoader.OnChange(func(fg *webhookconfig.FeatureGates) {
		setupLog.Info("Feature gates updated",
			"globalEnabled", fg.GlobalEnabled,
			"envoyProxy", fg.EnvoyProxy,
			"injectTools", fg.InjectTools,
			"perWorkloadConfigResolution", fg.PerWorkloadConfigResolution)
	})
	if err := featureGateLoader.Watch(ctx); err != nil {
		setupLog.Error(err, "Failed to start feature gates watcher")
		// Non-fatal - continue without hot reload
	}

	// Mitigate CVE-2023-44487 (HTTP/2 Rapid Reset).
	disableHTTP2 := func(c *tls.Config) {
		c.NextProtos = []string{"http/1.1"}
	}
	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher
	webhookTLSOpts := tlsOpts

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(webhookCertPath, webhookCertName),
			filepath.Join(webhookCertPath, webhookCertKey),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize webhook certificate watcher")
			os.Exit(1)
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: webhookTLSOpts,
	})

	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(metricsCertPath, metricsCertName),
			filepath.Join(metricsCertPath, metricsCertKey),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize metrics certificate watcher")
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	cmCacheNamespaces := buildConfigMapCacheNamespaces(
		requireA2ASignature, spireTrustBundleConfigMapName, spireTrustBundleConfigMapNS,
	)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsServerOptions,
		Cache: cache.Options{
			DefaultNamespaces: getNamespacesToWatch(),
			ByObject: map[client.Object]cache.ByObject{
				&corev1.ConfigMap{}: {
					Namespaces: cmCacheNamespaces,
				},
			},
		},
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "b7c4ae34.kagenti.dev",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// ========================================
	// Auto-discover keycloakPublicURL and spireTrustDomain
	// Env vars override auto-discovery.
	// ========================================
	if keycloakPublicURL == "" {
		keycloakPublicURL = os.Getenv("KAGENTI_KEYCLOAK_PUBLIC_URL")
	}
	if keycloakPublicURL == "" {
		discovered, err := discovery.DiscoverKeycloakPublicURL(ctx, mgr.GetAPIReader(), keycloakAdminSecretNamespace)
		if err != nil {
			setupLog.Info("Keycloak public URL auto-discovery failed (will retry at reconcile time)", "error", err.Error())
		} else {
			keycloakPublicURL = discovered
			setupLog.Info("Auto-discovered Keycloak public URL", "url", keycloakPublicURL)
		}
	}

	if spireTrustDomain == "" {
		spireTrustDomain = os.Getenv("KAGENTI_SPIRE_TRUST_DOMAIN")
	}
	if spireTrustDomain == "" {
		spireNS := os.Getenv("KAGENTI_SPIRE_NAMESPACE")
		discovered, err := discovery.DiscoverSPIRETrustDomain(ctx, mgr.GetAPIReader(), spireNS)
		if err != nil {
			setupLog.Info("SPIRE trust domain auto-discovery failed (will retry at reconcile time)", "error", err.Error())
		} else {
			spireTrustDomain = discovered
			setupLog.Info("Auto-discovered SPIRE trust domain", "trustDomain", spireTrustDomain)
		}
	}

	if !requireA2ASignature && !enableVerifiedFetch {
		setupLog.Info("WARNING: Neither --require-a2a-signature nor --enable-verified-fetch is set. " +
			"Identity binding requires at least one trust mechanism to function. " +
			"AgentCards with spec.identityBinding will report NotBound.")
	}

	var sigProvider signature.Provider
	if requireA2ASignature {
		if spireTrustDomain == "" {
			setupLog.Error(errors.New("SPIRE trust domain not available"),
				"spireTrustDomain is required when --require-a2a-signature=true "+
					"(set KAGENTI_SPIRE_TRUST_DOMAIN or ensure ZTWIM/spire-bundle is accessible)")
			os.Exit(1)
		}
		if spireTrustBundleConfigMapName == "" || spireTrustBundleConfigMapNS == "" {
			setupLog.Error(errors.New("missing required flags"),
				"--spire-trust-bundle-configmap and --spire-trust-bundle-configmap-namespace "+
					"are required when --require-a2a-signature=true")
			os.Exit(1)
		}

		sigConfig := &signature.Config{
			Type:                       signature.ProviderTypeX5C,
			TrustBundleConfigMapName:   spireTrustBundleConfigMapName,
			TrustBundleConfigMapNS:     spireTrustBundleConfigMapNS,
			TrustBundleConfigMapKey:    spireTrustBundleConfigMapKey,
			TrustBundleRefreshInterval: spireTrustBundleRefreshInterval,
			Client:                     mgr.GetClient(),
		}

		var providerErr error
		sigProvider, providerErr = signature.NewProvider(sigConfig)
		if providerErr != nil {
			setupLog.Error(providerErr, "unable to create x5c signature provider")
			os.Exit(1)
		}
		setupLog.Info("Signature verification enabled",
			"provider", "x5c",
			"trustDomain", spireTrustDomain,
			"auditMode", signatureAuditMode)
	}

	agentFetcher := agentcard.NewConfigMapFetcher(mgr.GetAPIReader())

	var authenticatedFetcher agentcard.AuthenticatedFetcher
	if enableVerifiedFetch {
		fetchCtx, fetchCancel := context.WithTimeout(ctx, 30*time.Second)
		fetchX509Source, fetchSourceErr := workloadapi.NewX509Source(
			fetchCtx,
			workloadapi.WithClientOptions(workloadapi.WithAddr(verifiedFetchSpiffeSocket)),
		)
		fetchCancel()

		if fetchSourceErr != nil {
			setupLog.Info("WARNING: SPIRE unavailable for verified fetch, falling back to default fetcher",
				"error", fetchSourceErr.Error(),
				"socket", verifiedFetchSpiffeSocket)
			enableVerifiedFetch = false
		} else {
			td := spireTrustDomain
			if td == "" {
				setupLog.Error(errors.New("SPIRE trust domain not available"),
					"spireTrustDomain is required when --enable-verified-fetch=true "+
						"(set KAGENTI_SPIRE_TRUST_DOMAIN or ensure ZTWIM/spire-bundle is accessible)")
				os.Exit(1)
			}
			fetcher, fetcherErr := agentcard.NewSpiffeFetcher(fetchX509Source, td)
			if fetcherErr != nil {
				setupLog.Error(fetcherErr, "Failed to create authenticated fetcher")
				os.Exit(1)
			}
			authenticatedFetcher = fetcher
			defer fetchX509Source.Close() //nolint:errcheck
			setupLog.Info("Verified fetch enabled (mTLS via SPIFFE)",
				"socket", verifiedFetchSpiffeSocket,
				"trustDomain", td)
		}
	}

	agentCardReconciler := &controller.AgentCardReconciler{
		Client:                mgr.GetClient(),
		Scheme:                mgr.GetScheme(),
		Recorder:              mgr.GetEventRecorderFor("agentcard-controller"),
		AgentFetcher:          agentFetcher,
		AuthenticatedFetcher:  authenticatedFetcher,
		EnableVerifiedFetch:   enableVerifiedFetch,
		SignatureProvider:     sigProvider,
		RequireSignature:      requireA2ASignature,
		SignatureAuditMode:    signatureAuditMode,
		SpireTrustDomain:      spireTrustDomain,
		SVIDExpiryGracePeriod: svidExpiryGracePeriod,
	}

	if err = agentCardReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AgentCard")
		os.Exit(1)
	}

	if enforceNetworkPolicies {
		npReconciler := &controller.AgentCardNetworkPolicyReconciler{
			Client:                 mgr.GetClient(),
			Scheme:                 mgr.GetScheme(),
			EnforceNetworkPolicies: enforceNetworkPolicies,
			OperatorNamespace:      os.Getenv("POD_NAMESPACE"),
		}
		npReconciler.DiscoverKubeAPIServerCIDRs(
			context.Background(), mgr.GetAPIReader(),
		)
		if err = npReconciler.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "AgentCardNetworkPolicy")
			os.Exit(1)
		}
		setupLog.Info("Network policy enforcement enabled for identity verification")
	}

	if err = (&controller.AgentCardSyncReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		SpireTrustDomain: spireTrustDomain,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AgentCardSync")
		os.Exit(1)
	}

	if controller.NetworkOperatorCRDExists(mgr.GetConfig()) {
		if warning := controller.CheckOVNNetworkConfig(context.Background(), mgr.GetAPIReader()); warning != "" {
			setupLog.Error(fmt.Errorf("OVN network misconfiguration"), warning)
		} else {
			setupLog.Info("OVN-Kubernetes routingViaHost is correctly configured")
		}
	}

	artReconciler := &controller.AgentRuntimeReconciler{
		Client:              mgr.GetClient(),
		APIReader:           mgr.GetAPIReader(),
		Scheme:              mgr.GetScheme(),
		Recorder:            mgr.GetEventRecorderFor("agentruntime-controller"),
		EnableCardDiscovery: enableCardDiscovery,
		SpireTrustDomain:    spireTrustDomain,
		GetFeatureGates:     featureGateLoader.Get,
	}
	if enableCardDiscovery {
		artReconciler.AgentFetcher = agentFetcher
		artReconciler.SignatureProvider = sigProvider
		if authenticatedFetcher != nil {
			artReconciler.AuthenticatedFetcher = authenticatedFetcher
		}
		setupLog.Info("Card discovery enabled for AgentRuntime controller")
	}
	if err = artReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AgentRuntime")
		os.Exit(1)
	}

	if enableMLflow {
		if err = (&controller.MLflowReconciler{
			Client:   mgr.GetClient(),
			Scheme:   mgr.GetScheme(),
			Recorder: mgr.GetEventRecorderFor("mlflow-controller"),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "MLflow")
			os.Exit(1)
		}
		setupLog.Info("MLflow experiment tracking controller enabled")

		uiConfigBootstrap := &bootstrap.UIConfigBootstrapRunnable{
			Client:    mgr.GetClient(),
			APIReader: mgr.GetAPIReader(),
			Namespace: getOperatorNamespace(),
			Log:       ctrl.Log.WithName("bootstrap"),
		}
		if err := mgr.Add(uiConfigBootstrap); err != nil {
			setupLog.Error(err, "unable to add UI config bootstrap runnable")
			os.Exit(1)
		}
		setupLog.Info("MLflow UI config bootstrap enabled")

		if err = (&controller.MLflowOperandReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
			//nolint:staticcheck // consistent with existing controllers
			Recorder:          mgr.GetEventRecorderFor("mlflow-operand-controller"),
			OperatorNamespace: getOperatorNamespace(),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "MLflowOperand")
			os.Exit(1)
		}
		setupLog.Info("MLflow operand controller enabled")
	}

	if enableClientRegistration {
		operatorNS := getOperatorNamespace()
		setupLog.Info("Client registration controller enabled",
			"keycloakAdminSecretNamespace", keycloakAdminSecretNamespace,
			"operatorNamespace", operatorNS)
		if useSpiffeAuth {
			setupLog.Info("SPIFFE ID authentication enabled: using JWT-SVID for client registration",
				"spireSocket", verifiedFetchSpiffeSocket,
				"operatorSPIFFEID", operatorClientID)
		}
		if err = (&controller.ClientRegistrationReconciler{
			Client:                       mgr.GetClient(),
			APIReader:                    mgr.GetAPIReader(),
			Scheme:                       mgr.GetScheme(),
			OperatorNamespace:            operatorNS,
			KeycloakAdminSecretNamespace: keycloakAdminSecretNamespace,
			SpireTrustDomain:             spireTrustDomain,
			KeycloakAdminTokenCache:      &keycloak.CachedAdminTokenProvider{},
			UseSpiffeAuth:                useSpiffeAuth,
			JWTSVIDPath:                  jwtSVIDPath,
			OperatorClientID:             operatorClientID,
			Recorder:                     mgr.GetEventRecorderFor("clientregistration"),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "ClientRegistration")
			os.Exit(1)
		}
		setupLog.Info("Operator-managed client registration controller enabled")
	}

	if controller.TektonConfigCRDExists(mgr.GetConfig()) {
		if err = (&controller.TektonConfigReconciler{
			Client: mgr.GetClient(),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "TektonConfig")
			os.Exit(1)
		}
	}

	if controller.CertManagerCRDExists(mgr.GetConfig()) {
		if err = (&controller.SharedTrustReconciler{
			Client:   mgr.GetClient(),
			Recorder: mgr.GetEventRecorderFor("shared-trust-controller"), //nolint:staticcheck
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "SharedTrust")
			os.Exit(1)
		}
	}

	if err = webhookv1alpha1.SetupAgentCardWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "AgentCard")
		os.Exit(1)
	}
	if err = webhookv1alpha1.SetupAgentRuntimeWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "AgentRuntime")
		os.Exit(1)
	}

	// AuthBridge sidecar injection webhook
	if authBridgeWebhooksEnabled() {
		podMutator := injector.NewPodMutator(
			mgr.GetClient(),
			mgr.GetAPIReader(),
			configLoader.Get,
			featureGateLoader.Get,
		)
		if err = webhookv1alpha1.SetupAuthBridgeWebhookWithManager(mgr, podMutator); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "AuthBridge")
			os.Exit(1)
		}

		// Defaults-only config reconciler: propagates ConfigMap changes to
		// workloads that have kagenti.io/type but no AgentRuntime CR.
		if err = (&injector.DefaultsConfigReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "DefaultsConfig")
			os.Exit(1)
		}
	}
	// +kubebuilder:scaffold:builder

	if enableOtelBootstrap {
		otelBootstrap := &bootstrap.OtelBootstrapRunnable{
			Client:               mgr.GetClient(),
			APIReader:            mgr.GetAPIReader(),
			Config:               mgr.GetConfig(),
			Namespace:            getOperatorNamespace(),
			Log:                  ctrl.Log.WithName("bootstrap"),
			MLflowWorkspace:      mlflowWorkspace,
			MLflowExperimentName: mlflowExperimentName,
		}
		if err := mgr.Add(otelBootstrap); err != nil {
			setupLog.Error(err, "unable to add OTel bootstrap runnable")
			os.Exit(1)
		}
		setupLog.Info("OTel collector bootstrap enabled")
	}

	if enableAuthbridgeConfig {
		if err = (&controller.AuthbridgeConfigReconciler{
			Client: mgr.GetClient(),
			Platform: controller.AuthbridgeConfigPlatform{
				KeycloakAdminSecretNamespace: keycloakAdminSecretNamespace,
				KeycloakRealm:                keycloakRealm,
				KeycloakPublicURL:            keycloakPublicURL,
				SpireTrustDomain:             spireTrustDomain,
				ClientAuthType:               clientAuthType,
				SpiffeIdpAlias:               spiffeIdpAlias,
				CredentialWaitTimeout:        credentialWaitTimeout,
			},
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "AuthbridgeConfig")
			os.Exit(1)
		}
		setupLog.Info("authbridge-config reconciler enabled")
	}

	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			setupLog.Error(err, "unable to add metrics certificate watcher to manager")
			os.Exit(1)
		}
	}

	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(webhookCertWatcher); err != nil {
			setupLog.Error(err, "unable to add webhook certificate watcher to manager")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("webhook", webhookServer.StartedChecker()); err != nil {
		setupLog.Error(err, "unable to set up webhook ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// authBridgeWebhooksEnabled reports whether AuthBridge mutating webhooks should be registered.
// Set ENABLE_WEBHOOKS=false to skip registration (tests, minimal deployments).
func authBridgeWebhooksEnabled() bool {
	return os.Getenv("ENABLE_WEBHOOKS") != "false"
}

func getNamespacesToWatch() map[string]cache.Config {
	namespace := strings.TrimSpace(os.Getenv("NAMESPACES2WATCH"))
	if namespace == "" {
		return nil
	}

	namespaces := make(map[string]cache.Config)
	for _, ns := range strings.Split(namespace, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			namespaces[ns] = cache.Config{}
		}
	}
	if len(namespaces) == 0 {
		return nil
	}
	return namespaces
}

// buildConfigMapCacheNamespaces returns the per-namespace cache selectors for
// ConfigMaps. The scoped cache ensures only kagenti-relevant ConfigMaps are
// watched instead of every ConfigMap cluster-wide.
//
// Three categories are included:
//  1. Cluster-level defaults in kagenti-system (label selector).
//  2. Namespace-level defaults in any namespace (label selector).
//  3. SPIRE trust bundle (field selector on metadata.name), added only when
//     signature verification is enabled.
func buildConfigMapCacheNamespaces(
	requireA2ASignature bool, spireTrustBundleConfigMapName, spireTrustBundleConfigMapNS string,
) map[string]cache.Config {
	namespaces := map[string]cache.Config{
		controller.ClusterDefaultsNamespace: {
			LabelSelector: labels.SelectorFromSet(map[string]string{
				"app.kubernetes.io/name": "kagenti-operator-chart",
			}),
		},
		cache.AllNamespaces: {
			LabelSelector: labels.SelectorFromSet(map[string]string{
				controller.LabelNamespaceDefaults: "true",
			}),
		},
	}
	if requireA2ASignature && spireTrustBundleConfigMapNS != "" {
		if _, collision := namespaces[spireTrustBundleConfigMapNS]; collision {
			setupLog.Error(
				errors.New("namespace collision: --spire-trust-bundle-configmap-namespace matches "+
					"the cluster defaults namespace"),
				"SPIRE trust bundle will not be cached; signature verification may fail. "+
					"Use a different namespace for the trust bundle ConfigMap",
				"trustBundleNamespace", spireTrustBundleConfigMapNS,
				"clusterDefaultsNamespace", controller.ClusterDefaultsNamespace,
			)
		} else {
			namespaces[spireTrustBundleConfigMapNS] = cache.Config{
				FieldSelector: fields.SelectorFromSet(fields.Set{
					"metadata.name": spireTrustBundleConfigMapName,
				}),
			}
		}
	}
	return namespaces
}
