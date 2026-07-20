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

package bootstrap

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	"github.com/rossoctl/operator/internal/mlflow"
)

const (
	ingressCAConfigMap     = "otel-ingress-ca"
	ingressCertConfigMap   = "default-ingress-cert"
	ingressCertNamespace   = "openshift-config-managed"
	rootCAConfigMap        = "kube-root-ca.crt"
	rootCANamespace        = "openshift-config"
	collectorConfigMapName = "otel-collector-config"
	collectorDeployment    = "otel-collector"
	phoenixServiceName     = "phoenix"

	configMapDataKey  = "base.yaml"
	caBundleKey       = "ca-bundle.crt"
	rootCAKey         = "ca.crt"
	restartAnnotation = "rossoctl.io/otel-config-hash"
	ocpAPIGroup       = "config.openshift.io"
	mlflowCRDGroup    = "mlflow.opendatahub.io"
	mlflowCRDVersion  = "v1"
	mlflowCRDResource = "mlflows"

	defaultBackoffInitial = 5 * time.Second
	defaultBackoffMax     = 30 * time.Second
	defaultBackoffTimeout = 5 * time.Minute
)

// OtelBootstrapRunnable implements manager.Runnable. It runs once at operator
// startup to bootstrap the OTel collector infrastructure:
//   - Step 1: Project the OpenShift ingress CA into the operator namespace (OCP only).
//   - Step 2: Assemble the OTel collector ConfigMap from component-specific presets.
type OtelBootstrapRunnable struct {
	Client    client.Client
	APIReader client.Reader
	Config    *rest.Config
	Scheme    *runtime.Scheme
	Namespace string
	Log       logr.Logger

	// MLflowWorkspace is the Kubernetes namespace used as the x-mlflow-workspace
	// header value (RHOAI only). When empty, workspace/experiment headers are skipped.
	MLflowWorkspace string
	// MLflowExperimentName is the MLflow experiment name to create/lookup.
	MLflowExperimentName string

	// IsOpenShift overrides OCP detection when non-nil (for testing).
	IsOpenShift func(ctx context.Context) (bool, error)
	// MLflowCRDExists overrides CRD discovery when non-nil (for testing).
	MLflowCRDExists func(ctx context.Context) (bool, error)
	// EnsureExperiment overrides MLflow experiment creation when non-nil (for testing).
	EnsureExperiment func(ctx context.Context, baseURL, workspace string) (string, error)
}

// operatorDeploymentNames lists possible Deployment names for the operator itself,
// used to set OwnerReferences on bootstrap-created resources.
var operatorDeploymentNames = []string{
	"rossoctl-controller-manager",
	"controller-manager",
}

// getOperatorOwner looks up the operator's own Deployment to use as an OwnerReference.
// Returns nil if the deployment cannot be found (best-effort).
func (r *OtelBootstrapRunnable) getOperatorOwner(ctx context.Context, log logr.Logger) *appsv1.Deployment {
	for _, name := range operatorDeploymentNames {
		deploy := &appsv1.Deployment{}
		key := types.NamespacedName{Name: name, Namespace: r.Namespace}
		if err := r.Client.Get(ctx, key, deploy); err == nil {
			return deploy
		}
	}
	log.Info("Could not find operator Deployment for OwnerReference, ConfigMaps will be unowned")
	return nil
}

// setOwnerIfAvailable sets an OwnerReference on the given object if an owner is available.
func (r *OtelBootstrapRunnable) setOwnerIfAvailable(owner *appsv1.Deployment, obj client.Object, log logr.Logger) {
	if owner == nil || r.Scheme == nil {
		return
	}
	if err := controllerutil.SetOwnerReference(owner, obj, r.Scheme); err != nil {
		log.Error(err, "Failed to set OwnerReference on resource", "name", obj.GetName())
	}
}

// Start runs the bootstrap sequence. Called by the manager after leader election
// and cache sync, before controllers start processing events.
func (r *OtelBootstrapRunnable) Start(ctx context.Context) error {
	log := r.Log.WithName("otel-bootstrap")
	log.Info("Starting OTel collector bootstrap")

	// Look up operator Deployment once for OwnerReference on created resources.
	owner := r.getOperatorOwner(ctx, log)

	isOCP, err := r.detectOpenShift(ctx)
	if err != nil {
		return fmt.Errorf("detecting OpenShift: %w", err)
	}

	if isOCP {
		log.Info("OpenShift detected, reconciling ingress CA trust")
		if err := r.reconcileIngressCA(ctx, log, owner); err != nil {
			return fmt.Errorf("ingress CA bootstrap: %w", err)
		}
	} else {
		log.Info("Not running on OpenShift, skipping ingress CA trust")
	}

	if err := r.reconcileCollectorConfig(ctx, log, isOCP, owner); err != nil {
		return fmt.Errorf("collector config bootstrap: %w", err)
	}

	log.Info("OTel collector bootstrap complete")
	return nil
}

// NeedLeaderElection returns true so the bootstrap only runs on the leader.
func (r *OtelBootstrapRunnable) NeedLeaderElection() bool {
	return true
}

// detectOpenShift checks for the config.openshift.io API group.
func (r *OtelBootstrapRunnable) detectOpenShift(ctx context.Context) (bool, error) {
	if r.IsOpenShift != nil {
		return r.IsOpenShift(ctx)
	}

	dc, err := discovery.NewDiscoveryClientForConfig(r.Config)
	if err != nil {
		return false, fmt.Errorf("creating discovery client: %w", err)
	}

	_, apiLists, err := dc.ServerGroupsAndResources()
	if err != nil && !discovery.IsGroupDiscoveryFailedError(err) {
		return false, fmt.Errorf("discovering API groups: %w", err)
	}

	for _, list := range apiLists {
		gv, parseErr := parseGroupVersion(list.GroupVersion)
		if parseErr != nil {
			continue
		}
		if gv == ocpAPIGroup {
			return true, nil
		}
	}
	return false, nil
}

// reconcileIngressCA reads the OpenShift ingress CA and root CA, then creates
// or updates the otel-ingress-ca ConfigMap in the operator namespace.
func (r *OtelBootstrapRunnable) reconcileIngressCA(ctx context.Context, log logr.Logger, owner *appsv1.Deployment) error {
	ingressCert := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: ingressCertConfigMap, Namespace: ingressCertNamespace}
	if err := r.APIReader.Get(ctx, key, ingressCert); err != nil {
		return fmt.Errorf("reading %s/%s: %w", ingressCertNamespace, ingressCertConfigMap, err)
	}

	caBundle, ok := ingressCert.Data[caBundleKey]
	if !ok || caBundle == "" {
		return fmt.Errorf("ConfigMap %s/%s has no %q key", ingressCertNamespace, ingressCertConfigMap, caBundleKey)
	}

	rootCA := &corev1.ConfigMap{}
	rootKey := types.NamespacedName{Name: rootCAConfigMap, Namespace: rootCANamespace}
	if err := r.APIReader.Get(ctx, rootKey, rootCA); err != nil {
		if !errors.IsNotFound(err) {
			log.Info("Could not read root CA ConfigMap, continuing with ingress CA only",
				"error", err, "configmap", rootCAConfigMap, "namespace", rootCANamespace)
		}
	} else if rootCert, ok := rootCA.Data[rootCAKey]; ok && rootCert != "" {
		log.Info("Adding root CA to bundle for full certificate chain")
		caBundle = caBundle + "\n" + rootCert
	}

	existing := &corev1.ConfigMap{}
	existingKey := types.NamespacedName{Name: ingressCAConfigMap, Namespace: r.Namespace}
	if err := r.APIReader.Get(ctx, existingKey, existing); err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("checking existing %s ConfigMap: %w", ingressCAConfigMap, err)
		}
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ingressCAConfigMap,
				Namespace: r.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "rossoctl-operator",
					"app.kubernetes.io/component":  "otel-bootstrap",
				},
			},
			Data: map[string]string{caBundleKey: caBundle},
		}
		r.setOwnerIfAvailable(owner, cm, log)
		if err := r.Client.Create(ctx, cm); err != nil {
			if !errors.IsAlreadyExists(err) {
				return fmt.Errorf("creating %s ConfigMap: %w", ingressCAConfigMap, err)
			}
			log.Info("ConfigMap appeared between check and create, will update", "name", ingressCAConfigMap)
			if err := r.APIReader.Get(ctx, existingKey, existing); err != nil {
				return fmt.Errorf("re-reading %s ConfigMap: %w", ingressCAConfigMap, err)
			}
		} else {
			log.Info("Created ingress CA ConfigMap", "name", ingressCAConfigMap)
			return nil
		}
	}

	if existing.Data[caBundleKey] == caBundle {
		log.Info("Ingress CA ConfigMap already up-to-date, skipping")
		return nil
	}

	existing.Data = map[string]string{caBundleKey: caBundle}
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	existing.Labels["app.kubernetes.io/managed-by"] = "rossoctl-operator"
	existing.Labels["app.kubernetes.io/component"] = "otel-bootstrap"
	if err := r.Client.Update(ctx, existing); err != nil {
		return fmt.Errorf("updating %s ConfigMap: %w", ingressCAConfigMap, err)
	}
	log.Info("Updated ingress CA ConfigMap", "name", ingressCAConfigMap)
	return nil
}

// reconcileCollectorConfig discovers available components and assembles the
// OTel collector ConfigMap from preset configurations.
func (r *OtelBootstrapRunnable) reconcileCollectorConfig(ctx context.Context, log logr.Logger, isOCP bool, owner *appsv1.Deployment) error {
	mf, err := r.discoverMLflow(ctx, log)
	if err != nil {
		return err
	}

	if mf.available && isOCP && r.MLflowWorkspace != "" {
		mf.workspaceNS = r.MLflowWorkspace
		baseURL := strings.TrimSuffix(mf.tracesURL, "/v1/traces")
		if baseURL != "" && baseURL != mf.tracesURL {
			expID, expErr := r.ensureMLflowExperiment(ctx, log, baseURL, mf.workspaceNS)
			if expErr != nil {
				log.Error(expErr, "Could not create MLflow experiment, traces may not be routed correctly",
					"workspace", mf.workspaceNS)
			} else {
				mf.experimentID = expID
				log.Info("MLflow workspace and experiment ready",
					"workspace", mf.workspaceNS, "experimentID", expID)
			}
		}
	} else if mf.available && isOCP && r.MLflowWorkspace == "" {
		log.Info("mlflow.workspace not configured, skipping workspace and experiment headers. " +
			"Set mlflow.workspace in Helm values for RHOAI deployments.")
	}

	phoenixAvailable := r.discoverPhoenix(ctx, log)

	config, err := assembleCollectorConfig(isOCP, mf, phoenixAvailable)
	if err != nil {
		return fmt.Errorf("assembling collector config: %w", err)
	}

	configYAML, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("marshalling collector config: %w", err)
	}

	configStr := string(configYAML)
	configHash := fmt.Sprintf("%x", sha256.Sum256(configYAML))

	existing := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: collectorConfigMapName, Namespace: r.Namespace}
	if err := r.APIReader.Get(ctx, key, existing); err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("checking existing collector ConfigMap: %w", err)
		}
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      collectorConfigMapName,
				Namespace: r.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "rossoctl-operator",
					"app.kubernetes.io/component":  "otel-bootstrap",
				},
				Annotations: map[string]string{
					restartAnnotation: configHash,
				},
			},
			Data: map[string]string{configMapDataKey: configStr},
		}
		r.setOwnerIfAvailable(owner, cm, log)
		if err := r.Client.Create(ctx, cm); err != nil {
			if !errors.IsAlreadyExists(err) {
				return fmt.Errorf("creating collector ConfigMap: %w", err)
			}
			log.Info("ConfigMap appeared between check and create, will update", "name", collectorConfigMapName)
			if err := r.APIReader.Get(ctx, key, existing); err != nil {
				return fmt.Errorf("re-reading collector ConfigMap: %w", err)
			}
		} else {
			log.Info("Created OTel collector ConfigMap", "components",
				componentSummary(mf.available, phoenixAvailable))
			return r.rolloutRestartCollector(ctx, log, configHash)
		}
	}

	existingHash := existing.Annotations[restartAnnotation]
	if existingHash == configHash {
		log.Info("OTel collector ConfigMap unchanged, skipping restart")
		return nil
	}

	existing.Data = map[string]string{configMapDataKey: configStr}
	if existing.Annotations == nil {
		existing.Annotations = make(map[string]string)
	}
	existing.Annotations[restartAnnotation] = configHash
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	existing.Labels["app.kubernetes.io/managed-by"] = "rossoctl-operator"
	existing.Labels["app.kubernetes.io/component"] = "otel-bootstrap"
	if err := r.Client.Update(ctx, existing); err != nil {
		return fmt.Errorf("updating collector ConfigMap: %w", err)
	}
	log.Info("Updated OTel collector ConfigMap", "components",
		componentSummary(mf.available, phoenixAvailable))

	return r.rolloutRestartCollector(ctx, log, configHash)
}

// mlflowInfo holds the discovered MLflow endpoint and workspace config.
type mlflowInfo struct {
	available    bool
	tracesURL    string // in-cluster traces endpoint (e.g. https://mlflow.ns.svc:8443/v1/traces)
	workspaceNS  string // agent namespace for x-mlflow-workspace header (RHOAI only)
	experimentID string // MLflow experiment ID for x-mlflow-experiment-id header
}

// discoverMLflow checks for the MLflow CRD and, if present, discovers the
// MLflow CR to derive the in-cluster endpoint and workspace namespace.
func (r *OtelBootstrapRunnable) discoverMLflow(ctx context.Context, log logr.Logger) (*mlflowInfo, error) {
	crdExists, err := r.mlflowCRDPresent(ctx)
	if err != nil {
		return nil, fmt.Errorf("checking MLflow CRD: %w", err)
	}
	if !crdExists {
		log.Info("MLflow CRD (mlflows.mlflow.opendatahub.io) not found. " +
			"MLflow presets will be skipped. If the MLflow operator is installed later, " +
			"restart the rossoctl-operator pod to pick up MLflow configuration.")
		return &mlflowInfo{}, nil
	}

	log.Info("MLflow CRD detected, discovering MLflow CR")

	list := &mlflow.MLflowList{}
	if err := r.APIReader.List(ctx, list); err != nil {
		log.Info("Could not list MLflow CRs, skipping MLflow presets", "error", err)
		return &mlflowInfo{}, nil
	}

	for i := range list.Items {
		cr := &list.Items[i]
		if meta.IsStatusConditionTrue(cr.Status.Conditions, "Available") {
			info := mlflowInfoFromCR(cr, log)
			return info, nil
		}
	}

	log.Info("MLflow CRD present but no Available MLflow CR found, waiting for service readiness")
	info, err := r.waitForMLflowService(ctx, log)
	if err != nil {
		log.Info("MLflow service did not become ready within timeout, skipping MLflow presets",
			"error", err)
		return &mlflowInfo{}, nil
	}
	return info, nil
}

// mlflowInfoFromCR extracts the in-cluster traces endpoint from an MLflow CR.
func mlflowInfoFromCR(cr *mlflow.MLflow, log logr.Logger) *mlflowInfo {
	info := &mlflowInfo{available: true}

	if cr.Status.Address != nil && cr.Status.Address.URL != "" {
		parsed, err := url.Parse(cr.Status.Address.URL)
		if err == nil && parsed.Scheme != "" && parsed.Host != "" {
			info.tracesURL = fmt.Sprintf("%s://%s/v1/traces", parsed.Scheme, parsed.Host)
			log.Info("Found available MLflow CR",
				"name", cr.Name, "addressURL", cr.Status.Address.URL,
				"tracesURL", info.tracesURL)
			return info
		}
		log.Info("MLflow address URL missing scheme or host, falling back",
			"url", cr.Status.Address.URL)
	}

	if cr.Status.URL != "" {
		info.tracesURL = strings.TrimRight(cr.Status.URL, "/") + "/v1/traces"
		log.Info("Found available MLflow CR (using external URL)",
			"name", cr.Name, "url", cr.Status.URL, "tracesURL", info.tracesURL)
	} else {
		log.Info("Found available MLflow CR but no endpoint URL in status",
			"name", cr.Name)
	}
	return info
}

// waitForMLflowService retries with backoff until an MLflow CR becomes Available.
func (r *OtelBootstrapRunnable) waitForMLflowService(ctx context.Context, log logr.Logger) (*mlflowInfo, error) {
	var result *mlflowInfo

	backoff := wait.Backoff{
		Duration: defaultBackoffInitial,
		Factor:   2.0,
		Cap:      defaultBackoffMax,
		Steps:    20,
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, defaultBackoffTimeout)
	defer cancel()

	err := wait.ExponentialBackoffWithContext(timeoutCtx, backoff, func(ctx context.Context) (bool, error) {
		list := &mlflow.MLflowList{}
		if err := r.APIReader.List(ctx, list); err != nil {
			log.V(1).Info("Retrying MLflow CR list", "error", err)
			return false, nil
		}
		for i := range list.Items {
			cr := &list.Items[i]
			if meta.IsStatusConditionTrue(cr.Status.Conditions, "Available") {
				result = mlflowInfoFromCR(cr, log)
				return true, nil
			}
		}
		log.V(1).Info("MLflow CR not yet Available, retrying")
		return false, nil
	})

	if result == nil {
		result = &mlflowInfo{}
	}
	return result, err
}

// mlflowCRDPresent checks if the mlflows.mlflow.opendatahub.io CRD is installed.
func (r *OtelBootstrapRunnable) mlflowCRDPresent(ctx context.Context) (bool, error) {
	if r.MLflowCRDExists != nil {
		return r.MLflowCRDExists(ctx)
	}

	dc, err := discovery.NewDiscoveryClientForConfig(r.Config)
	if err != nil {
		return false, fmt.Errorf("creating discovery client: %w", err)
	}

	resources, err := dc.ServerResourcesForGroupVersion(mlflowCRDGroup + "/" + mlflowCRDVersion)
	if err != nil {
		return false, nil //nolint:nilerr // CRD not installed is not an error
	}
	for _, res := range resources.APIResources {
		if res.Name == mlflowCRDResource {
			return true, nil
		}
	}
	return false, nil
}

// discoverPhoenix checks if the Phoenix service exists in the operator namespace.
func (r *OtelBootstrapRunnable) discoverPhoenix(ctx context.Context, log logr.Logger) bool {
	svc := &corev1.Service{}
	key := types.NamespacedName{Name: phoenixServiceName, Namespace: r.Namespace}
	if err := r.APIReader.Get(ctx, key, svc); err != nil {
		log.V(1).Info("Phoenix service not found, skipping Phoenix preset",
			"namespace", r.Namespace)
		return false
	}
	log.Info("Phoenix service detected", "namespace", r.Namespace)
	return true
}

// ensureMLflowExperiment creates or retrieves the configured experiment in the
// given workspace and returns the experiment ID.
func (r *OtelBootstrapRunnable) ensureMLflowExperiment(ctx context.Context, log logr.Logger, baseURL, workspace string) (string, error) {
	if r.EnsureExperiment != nil {
		return r.EnsureExperiment(ctx, baseURL, workspace)
	}

	httpClient, err := newServiceCAHTTPClient()
	if err != nil {
		return "", fmt.Errorf("creating TLS HTTP client: %w", err)
	}

	c := &mlflow.Client{
		BaseURL:    baseURL,
		HTTPClient: httpClient,
	}

	expName := r.MLflowExperimentName
	if expName == "" {
		expName = "rossoctl-traces"
	}

	expID, err := c.CreateExperiment(ctx, expName, workspace)
	if err != nil {
		return "", fmt.Errorf("creating MLflow experiment %q: %w", expName, err)
	}

	log.Info("Created/found MLflow experiment", "name", expName, "workspace", workspace, "experimentID", expID)
	return expID, nil
}

// newServiceCAHTTPClient returns an HTTP client configured to trust the
// OpenShift service-serving CA certificate.
func newServiceCAHTTPClient() (*http.Client, error) {
	caCert, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt")
	if err != nil {
		return nil, fmt.Errorf("reading service-ca.crt: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caCert)
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: pool,
			},
		},
	}, nil
}

// rolloutRestartCollector patches the OTel collector Deployment's pod template
// annotation to trigger a rollout restart.
func (r *OtelBootstrapRunnable) rolloutRestartCollector(ctx context.Context, log logr.Logger, configHash string) error {
	dep := &appsv1.Deployment{}
	key := types.NamespacedName{Name: collectorDeployment, Namespace: r.Namespace}
	if err := r.APIReader.Get(ctx, key, dep); err != nil {
		if errors.IsNotFound(err) {
			log.Info("OTel collector Deployment not found, skipping rollout restart")
			return nil
		}
		return fmt.Errorf("reading OTel collector Deployment: %w", err)
	}

	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = make(map[string]string)
	}

	if dep.Spec.Template.Annotations[restartAnnotation] == configHash {
		log.Info("OTel collector Deployment already has current config hash, skipping restart")
		return nil
	}

	dep.Spec.Template.Annotations[restartAnnotation] = configHash
	if err := r.Client.Update(ctx, dep); err != nil {
		return fmt.Errorf("rollout-restarting OTel collector: %w", err)
	}
	log.Info("Triggered OTel collector rollout restart", "configHash", configHash)
	return nil
}

// assembleCollectorConfig builds the complete OTel collector YAML config by
// merging the base config with component-specific presets.
func assembleCollectorConfig(isOCP bool, mf *mlflowInfo, phoenixAvailable bool) (map[string]any, error) {
	config, err := parsePreset(baseConfig)
	if err != nil {
		return nil, fmt.Errorf("parsing base config: %w", err)
	}

	hasComponentPipeline := false

	if phoenixAvailable {
		phoenix, err := parsePreset(phoenixPreset)
		if err != nil {
			return nil, fmt.Errorf("parsing phoenix preset: %w", err)
		}
		mergeDeep(config, phoenix)
		hasComponentPipeline = true
	}

	if mf.available {
		mlflowCfg, err := parsePreset(mlflowPreset)
		if err != nil {
			return nil, fmt.Errorf("parsing mlflow preset: %w", err)
		}
		mergeDeep(config, mlflowCfg)
		hasComponentPipeline = true

		if mf.tracesURL != "" {
			setMLflowTracesEndpoint(config, mf.tracesURL)
		}

		if isOCP {
			rhoaiAuth, err := parsePreset(rhoaiMlflowAuthPreset)
			if err != nil {
				return nil, fmt.Errorf("parsing rhoai mlflow auth preset: %w", err)
			}
			mergeDeep(config, rhoaiAuth)

			setMLflowExporterServiceCATLS(config)
			setMLflowBearerTokenAuth(config)

			if mf.workspaceNS != "" {
				setMLflowHeaders(config, mf.workspaceNS, mf.experimentID)
			}
		} else {
			mlflowAuth, err := parsePreset(mlflowAuthPreset)
			if err != nil {
				return nil, fmt.Errorf("parsing mlflow auth preset: %w", err)
			}
			mergeDeep(config, mlflowAuth)

			setMLflowOAuthAuth(config)
		}
	}

	if !hasComponentPipeline {
		defaultCfg, err := parsePreset(defaultPreset)
		if err != nil {
			return nil, fmt.Errorf("parsing default preset: %w", err)
		}
		mergeDeep(config, defaultCfg)
	}

	if isOCP && mf.available {
		setIngressCATLS(config)
	}

	return config, nil
}

// setMLflowTracesEndpoint overrides the hardcoded preset endpoint with the
// dynamically discovered in-cluster URL from the MLflow CR.
func setMLflowTracesEndpoint(config map[string]any, tracesURL string) {
	exporters, ok := config["exporters"].(map[string]any)
	if !ok {
		return
	}
	mlflowExp, ok := exporters["otlphttp/mlflow"].(map[string]any)
	if !ok {
		return
	}
	mlflowExp["traces_endpoint"] = tracesURL
}

// parsePreset unmarshals a YAML string into a map.
func parsePreset(yamlStr string) (map[string]any, error) {
	var result map[string]any
	if err := yaml.Unmarshal([]byte(yamlStr), &result); err != nil {
		return nil, err
	}
	return result, nil
}

// mergeDeep recursively merges src into dst. Maps are merged recursively;
// all other values (including slices) in src overwrite dst.
func mergeDeep(dst, src map[string]any) {
	for key, srcVal := range src {
		dstVal, exists := dst[key]
		if !exists {
			dst[key] = srcVal
			continue
		}

		dstMap, dstOk := dstVal.(map[string]any)
		srcMap, srcOk := srcVal.(map[string]any)
		if dstOk && srcOk {
			mergeDeep(dstMap, srcMap)
		} else {
			dst[key] = srcVal
		}
	}
}

// setMLflowExporterServiceCATLS replaces the mlflow preset's tls.insecure config
// with the OpenShift service-serving CA cert used to verify the MLflow TLS
// certificate. Without this, the collector rejects the MLflow cert as
// "x509: certificate signed by unknown authority."
func setMLflowExporterServiceCATLS(config map[string]any) {
	exporters, ok := config["exporters"].(map[string]any)
	if !ok {
		return
	}
	mlflowExp, ok := exporters["otlphttp/mlflow"].(map[string]any)
	if !ok {
		return
	}
	mlflowExp["tls"] = map[string]any{
		"ca_file": "/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt",
	}
}

// setMLflowHeaders sets the workspace and experiment-id headers on the MLflow
// exporter. RHOAI requires both: x-mlflow-workspace scopes to a namespace, and
// x-mlflow-experiment-id routes traces to a specific experiment.
func setMLflowHeaders(config map[string]any, workspace, experimentID string) {
	exporters, ok := config["exporters"].(map[string]any)
	if !ok {
		return
	}
	mlflowExp, ok := exporters["otlphttp/mlflow"].(map[string]any)
	if !ok {
		return
	}
	headers, ok := mlflowExp["headers"].(map[string]any)
	if !ok {
		headers = map[string]any{}
		mlflowExp["headers"] = headers
	}
	headers["x-mlflow-workspace"] = workspace
	if experimentID != "" {
		headers["x-mlflow-experiment-id"] = experimentID
	}
}

// setMLflowBearerTokenAuth sets bearer token auth on the MLflow exporter for
// RHOAI deployments.
func setMLflowBearerTokenAuth(config map[string]any) {
	exporters, ok := config["exporters"].(map[string]any)
	if !ok {
		return
	}
	mlflowExp, ok := exporters["otlphttp/mlflow"].(map[string]any)
	if !ok {
		return
	}
	mlflowExp["auth"] = map[string]any{"authenticator": "bearertokenauth/mlflow"}
}

// setMLflowOAuthAuth sets OAuth2 client authentication on the MLflow exporter
// for self-managed MLflow deployments.
func setMLflowOAuthAuth(config map[string]any) {
	exporters, ok := config["exporters"].(map[string]any)
	if !ok {
		return
	}
	mlflowExp, ok := exporters["otlphttp/mlflow"].(map[string]any)
	if !ok {
		return
	}
	mlflowExp["auth"] = map[string]any{"authenticator": "oauth2client/mlflow"}
}

// setIngressCATLS sets the ingress CA file path on the OAuth2 client extension
// for OpenShift deployments.
func setIngressCATLS(config map[string]any) {
	extensions, ok := config["extensions"].(map[string]any)
	if !ok {
		return
	}
	oauth, ok := extensions["oauth2client/mlflow"].(map[string]any)
	if !ok {
		return
	}
	oauth["tls"] = map[string]any{"ca_file": "/etc/pki/ingress-ca/ingress-ca.pem"}
}

// componentSummary returns a human-readable summary of detected components.
func componentSummary(mlflow, phoenix bool) string {
	s := ""
	if mlflow {
		s += "mlflow "
	}
	if phoenix {
		s += "phoenix "
	}
	if s == "" {
		s = "default (no component pipelines)"
	}
	return s
}

// parseGroupVersion extracts the group from a "group/version" string.
func parseGroupVersion(gv string) (string, error) {
	for i := len(gv) - 1; i >= 0; i-- {
		if gv[i] == '/' {
			return gv[:i], nil
		}
	}
	return "", fmt.Errorf("no slash in group/version %q", gv)
}
