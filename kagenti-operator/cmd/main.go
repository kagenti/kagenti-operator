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
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
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

	agentv1alpha1 "github.com/kagenti/operator/api/v1alpha1"
	"github.com/kagenti/operator/internal/agentcard"
	"github.com/kagenti/operator/internal/controller"
	"github.com/kagenti/operator/internal/keycloak"
	"github.com/kagenti/operator/internal/signature"
	"github.com/kagenti/operator/internal/tekton"
	webhookv1alpha1 "github.com/kagenti/operator/internal/webhook/v1alpha1"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agentv1alpha1.AddToScheme(scheme))
	utilruntime.Must(tekton.AddToScheme(scheme))
	utilruntime.Must(gwapiv1.Install(scheme))
	// +kubebuilder:scaffold:scheme
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

	var requireA2ASignature bool
	var signatureAuditMode bool
	var enforceNetworkPolicies bool
	var enableOperatorClientRegistration bool
	var enableWaypointProvisioning bool

	var operatorNamespace string
	var spireTrustDomain string
	var spireTrustBundleConfigMapName string
	var spireTrustBundleConfigMapNS string
	var spireTrustBundleConfigMapKey string
	var spireTrustBundleRefreshInterval time.Duration
	var svidExpiryGracePeriod time.Duration

	flag.StringVar(&operatorNamespace, "operator-namespace", os.Getenv("POD_NAMESPACE"),
		"Namespace where the operator is running (default: POD_NAMESPACE env var, fallback: 'kagenti-system')")
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
	flag.BoolVar(&requireA2ASignature, "require-a2a-signature", false,
		"Require A2A agent cards to have a valid signature")
	flag.BoolVar(&signatureAuditMode, "signature-audit-mode", false,
		"When true, log signature verification failures but don't block (use for rollout)")
	flag.BoolVar(&enforceNetworkPolicies, "enforce-network-policies", false,
		"Create NetworkPolicies to restrict traffic for agents with unverified signatures")
	flag.BoolVar(&enableOperatorClientRegistration, "enable-operator-client-registration", false,
		"Reconcile Keycloak client registration for agent/tool workloads unless "+
			"kagenti.io/client-registration-inject=true (legacy sidecar)")
	flag.BoolVar(&enableWaypointProvisioning, "enable-waypoint-provisioning", true,
		"Automatically provision Istio waypoint gateways for namespaces with Kagenti workloads")

	flag.StringVar(&spireTrustDomain, "spire-trust-domain", "",
		"SPIRE trust domain for identity binding (e.g. 'example.org')")
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

	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Default operator namespace if not set
	if operatorNamespace == "" {
		operatorNamespace = "kagenti-system"
		setupLog.Info("operator-namespace not set, using default", "namespace", operatorNamespace)
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

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsServerOptions,
		Cache: cache.Options{
			// Note: DefaultNamespaces is intentionally not set (removed getNamespacesToWatch()).
			// When not set, the cache defaults to cluster-wide for all resources except those
			// explicitly scoped in ByObject below.
			//
			// Scope the ConfigMap informer to only kagenti-relevant ConfigMaps.
			// Without this, the controller would cache ALL ConfigMaps cluster-wide.
			//
			// Two types of ConfigMaps are relevant:
			// 1. Cluster-level defaults in kagenti-webhook-system:
			//    - kagenti-webhook-defaults (platform-wide sidecar config)
			//    - kagenti-webhook-feature-gates (which AuthBridge components are enabled)
			//    Both are deployed by the kagenti-webhook Helm chart and share the
			//    label app.kubernetes.io/name=kagenti-webhook.
			//
			// 2. Namespace-level defaults in agent namespaces:
			//    ConfigMaps labeled kagenti.io/defaults=true, deployed by platform
			//    engineers via Helm/Kustomize to override cluster defaults per namespace.
			ByObject: map[client.Object]cache.ByObject{
				&corev1.ConfigMap{}: {
					Namespaces: map[string]cache.Config{
						controller.ClusterDefaultsNamespace: {
							LabelSelector: labels.SelectorFromSet(map[string]string{
								"app.kubernetes.io/name": "kagenti-webhook",
							}),
						},
						cache.AllNamespaces: {
							LabelSelector: labels.SelectorFromSet(map[string]string{
								controller.LabelNamespaceDefaults: "true",
							}),
						},
					},
				},
				// NOTE: All other resources (Namespace, Pod, Deployment, StatefulSet, Gateway)
				// are intentionally NOT in ByObject. With DefaultNamespaces not set, they will
				// automatically use the default cluster-wide cache, which is what we want.
				// Explicitly adding them to ByObject was preventing controllers from starting.
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

	if !requireA2ASignature {
		setupLog.Info("WARNING: --require-a2a-signature is false. Identity binding requires " +
			"--require-a2a-signature=true to function. AgentCards with spec.identityBinding " +
			"will always report NotBound.")
	}

	var sigProvider signature.Provider
	if requireA2ASignature {
		if spireTrustDomain == "" {
			setupLog.Error(errors.New("missing required flag"), "--spire-trust-domain is required when --require-a2a-signature=true")
			os.Exit(1)
		}
		if spireTrustBundleConfigMapName == "" || spireTrustBundleConfigMapNS == "" {
			setupLog.Error(errors.New("missing required flags"),
				"--spire-trust-bundle-configmap and --spire-trust-bundle-configmap-namespace are required when --require-a2a-signature=true")
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

	if err = (&controller.AgentCardReconciler{
		Client:                mgr.GetClient(),
		Scheme:                mgr.GetScheme(),
		Recorder:              mgr.GetEventRecorderFor("agentcard-controller"),
		AgentFetcher:          agentcard.NewConfigMapFetcher(mgr.GetAPIReader()),
		SignatureProvider:     sigProvider,
		RequireSignature:      requireA2ASignature,
		SignatureAuditMode:    signatureAuditMode,
		SpireTrustDomain:      spireTrustDomain,
		SVIDExpiryGracePeriod: svidExpiryGracePeriod,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AgentCard")
		os.Exit(1)
	}

	if enforceNetworkPolicies {
		npReconciler := &controller.AgentCardNetworkPolicyReconciler{
			Client:                 mgr.GetClient(),
			Scheme:                 mgr.GetScheme(),
			EnforceNetworkPolicies: enforceNetworkPolicies,
		}
		npReconciler.DiscoverKubeAPIServerCIDRs(
			context.Background(), mgr.GetAPIReader(),
		)
		if err = npReconciler.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "AgentCardNetworkPolicy")
			os.Exit(1)
		}
		setupLog.Info("Network policy enforcement enabled for signature verification")
	}

	if err = (&controller.AgentCardSyncReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		SpireTrustDomain: spireTrustDomain,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AgentCardSync")
		os.Exit(1)
	}

	if err = (&controller.AgentRuntimeReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("agentruntime-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AgentRuntime")
		os.Exit(1)
	}

	if enableOperatorClientRegistration {
		if err = (&controller.ClientRegistrationReconciler{
			Client:                  mgr.GetClient(),
			APIReader:               mgr.GetAPIReader(),
			Scheme:                  mgr.GetScheme(),
			OperatorNamespace:       operatorNamespace,
			SpireTrustDomain:        spireTrustDomain,
			KeycloakAdminTokenCache: &keycloak.CachedAdminTokenProvider{},
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "ClientRegistration")
			os.Exit(1)
		}
		setupLog.Info("Operator-managed client registration controller enabled",
			"operatorNamespace", operatorNamespace)
	}

	if controller.TektonConfigCRDExists(mgr.GetConfig()) {
		if err = (&controller.TektonConfigReconciler{
			Client: mgr.GetClient(),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "TektonConfig")
			os.Exit(1)
		}
	}

	if enableWaypointProvisioning {
		if err = (&controller.NamespaceWaypointReconciler{
			Client:                     mgr.GetClient(),
			Scheme:                     mgr.GetScheme(),
			EnableWaypointProvisioning: enableWaypointProvisioning,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "NamespaceWaypoint")
			os.Exit(1)
		}
		setupLog.Info("Waypoint provisioning controller enabled")
	}

	if err = webhookv1alpha1.SetupAgentCardWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "AgentCard")
		os.Exit(1)
	}
	if err = webhookv1alpha1.SetupAgentRuntimeWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "AgentRuntime")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

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

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
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
