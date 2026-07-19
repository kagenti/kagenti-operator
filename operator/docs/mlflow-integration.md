# MLflow Tracing Integration

The rossoctl-operator automatically integrates deployed agents with MLflow on OpenShift, using the RHOAI-managed MLflow instance (DSC `mlflowoperator`). When enabled, the operator's `MLflowReconciler` watches every Deployment labelled `rossoctl.io/type=agent` and automatically discovers the MLflow tracking server, creates an experiment per agent, injects environment variables, and creates a RoleBinding for authentication. No manual environment variable configuration is required — agents that emit traces via the MLflow SDK will see them appear in MLflow automatically after deployment.

## Requirements

### Platform (cluster admin)

1. **RHOAI 3.4 operator** installed with the `mlflowoperator` component set to `Managed` in the DataScienceCluster:

   ```bash
   kubectl patch datasciencecluster default-dsc --type=merge \
     -p '{"spec":{"components":{"mlflowoperator":{"managementState":"Managed"}}}}'
   ```

2. **An MLflow CR** created in the desired namespace:

   ```yaml
   apiVersion: mlflow.opendatahub.io/v1
   kind: MLflow
   metadata:
     name: mlflow
     namespace: <mlflow-namespace>
   spec:
     storage:
       accessModes:
         - ReadWriteOnce
       resources:
         requests:
           storage: 10Gi
     backendStoreUri: "sqlite:////mlflow/mlflow.db"
     artifactsDestination: "file:///mlflow/artifacts"
     serveArtifacts: true
   ```

3. **The operator deployed with MLflow enabled** — see [Enabling MLflow Integration](#enabling-mlflow-integration) below.

### Agent (AI engineer)

The agent code must emit traces using `mlflow[kubernetes]>=3.11.1`. The `kubernetes` extra is required for SA-token-based authentication (`MLFLOW_TRACKING_AUTH=kubernetes-namespaced`). Agents that do not instrument tracing are unaffected — no errors, no side effects.

## Enabling MLflow Integration

### Via Helm

Set `mlflow.enable` to `true` in the operator Helm chart values:

```bash
helm upgrade rossoctl-operator ./charts/operator \
  --set mlflow.enable=true
```

This adds the `--enable-mlflow=true` flag to the operator manager container (see `charts/operator/templates/manager/manager.yaml`).

### Via operator binary flag

If running the operator outside Helm (e.g. during development):

```bash
./manager --enable-mlflow=true
```

## Injected Environment Variables

The controller injects the following env vars into every container in the agent Deployment:

| Environment Variable     | Value                                       | Description                               |
|--------------------------|---------------------------------------------|-------------------------------------------|
| `MLFLOW_TRACKING_URI`    | Auto-discovered from MLflow CR `status.url` | MLflow server gateway URL                 |
| `MLFLOW_TRACKING_AUTH`   | `kubernetes-namespaced`                     | Auth method (SA token + workspace header) |
| `MLFLOW_EXPERIMENT_ID`   | Created via MLflow REST API                 | Numeric experiment ID for this agent      |
| `MLFLOW_EXPERIMENT_NAME` | Same as the Deployment name                 | Human-readable experiment name            |

The following annotations are set on the Deployment's PodTemplateSpec:

| Annotation                          | Value                   |
|--------------------------------------|-------------------------|
| `mlflow.rossoctl.io/experiment-id`    | Experiment ID           |
| `mlflow.rossoctl.io/experiment-name`  | Experiment name         |
| `mlflow.rossoctl.io/tracking-uri`     | MLflow tracking URI     |
| `mlflow.rossoctl.io/tracking-auth`    | `kubernetes-namespaced` |

## Authentication & RBAC

The MLflow controller uses Kubernetes namespace-scoped authentication with least-privilege agent access:

1. **Operator access**: The controller-manager's ServiceAccount is bound (via a ClusterRoleBinding shipped in the Helm chart / kustomize) to the broad `mlflow-operator-mlflow-integration` ClusterRole. This lets the operator call the MLflow REST API to create experiments. The `X-MLFLOW-WORKSPACE` header is set to the agent's namespace, scoping the experiment to that workspace.

2. **Agent access (scoped)**: For each agent Deployment the controller creates:
   - A **Role** named `rossoctl-mlflow-<deployment-name>` with `get` and `update` on the `experiments` resource (API group `mlflow.kubeflow.org`), scoped to the agent's own experiment via `resourceNames`. This means agents **cannot** create/delete experiments or access `registeredmodels` or `gatewayendpoints`.
   - A **RoleBinding** with the same name that binds the agent's ServiceAccount to the scoped Role.

   The agent authenticates to MLflow using its projected SA token at `/var/run/secrets/kubernetes.io/serviceaccount/token`.

3. Both the Role and RoleBinding are owned by the Deployment — deleting the Deployment garbage-collects them automatically.

## Verification

Once enabled, the operator registers the `mlflow` controller. Check the operator logs for:

```
Starting Controller    {"controller": "mlflow"}
```

After deploying an agent, verify the MLflow configuration:

```bash
# Check annotations on the Deployment
kubectl get deployment <agent-name> -n <namespace> \
  -o jsonpath='{.spec.template.metadata.annotations}' | jq .

# Check env vars on the agent container
kubectl get deployment <agent-name> -n <namespace> \
  -o jsonpath='{.spec.template.spec.containers[0].env[*].name}' | tr ' ' '\n' | grep MLFLOW

# Check the RoleBinding
kubectl get rolebinding rossoctl-mlflow-<agent-name> -n <namespace>

# Check operator events
kubectl get events -n <namespace> --field-selector reason=MLflowConfigured
```
