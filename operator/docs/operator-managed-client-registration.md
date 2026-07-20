# Operator-managed Keycloak client registration

This document describes the split responsibility between the **operator controllers** and the **AuthBridge mutating webhook** for registering agent workloads as OAuth clients in Keycloak and delivering credentials to AuthBridge sidecars.

Both the operator controllers and the webhook now live in a single binary in this repository (`rossoctl-operator`).

---

## 1. Why this change

### 1.1 Problem

By default, the mutating webhook injects a **`rossoctl-client-registration`** sidecar (or embeds equivalent behavior inside the combined **authbridge** container). That sidecar:

- Runs **inside every pod**, uses workload identity (SPIFFE when SPIRE is enabled), and talks to Keycloak to register or refresh the OAuth client.
- Competes for startup ordering and resources with the application and other sidecars.

Some deployments want **Envoy / SPIFFE / AuthBridge** injection to stay **pod-local**, but prefer **client lifecycle and secrets** to be handled **centrally** by the platform: one registration path, predictable secret names, and no client-registration container in the pod.

### 1.2 Approach

**By default**, agent and tool workloads use **operator-managed** Keycloak registration: the webhook does **not** inject the legacy **`rossoctl-client-registration`** sidecar (and disables the registration slice in combined **authbridge**). The **operator** then:

1. Registers the workload as a Keycloak client using the **Keycloak admin API** (same conceptual contract as the sidecar).
2. Creates a **Secret** in the workload namespace with `client-id.txt` and `client-secret.txt`.
3. Annotates the pod template so the **webhook** can mount that Secret into every container that already uses the **`shared-data`** volume, at the **same paths** the sidecar used (`/shared/client-id.txt`, `/shared/client-secret.txt`).

Workloads that need the **legacy** in-pod registration path **opt in** with `rossoctl.io/client-registration-inject: "true"`; the operator skips those workloads.

The webhook continues to inject **proxy-init**, **envoy** / **authbridge**, and **spiffe-helper** according to existing precedence and feature gates.

### 1.3 Benefits

- **Fewer containers** when the sidecar path is not desired.
- **Centralized registration** using namespace `keycloak-admin-secret` (already provisioned for the sidecar contract).
- **Deterministic secret naming** derived from namespace and workload name (`rossoctl-keycloak-client-credentials-<hash>`), with **owner references** to the Deployment or StatefulSet.
- **Safe ordering**: the operator creates the Secret **before** setting the pod-template annotation, so new Pods do not reference a missing Secret.
- **Admission reinvocation**: the webhook uses `reinvocationPolicy: IfNeeded` so a second pass can add Secret volume mounts if the operator annotates the template **after** the first injection.

---

## 2. How it works

### 2.1 Contract (labels and annotations)

| Key | Value | Meaning |
|-----|--------|---------|
| `rossoctl.io/client-registration-inject` | absent, `"false"`, or any value other than `"true"` | **Default**: operator-managed registration; webhook does **not** inject the legacy client-registration sidecar / combined registration slice. |
| `rossoctl.io/client-registration-inject` | `"true"` | **Legacy**: webhook injects the client-registration sidecar (or enables registration in combined authbridge); operator **does not** manage registration for this workload. |
| `rossoctl.io/keycloak-client-credentials-secret-name` | Secret name | Set by the operator on the **pod template**; webhook reads it from **Pod** annotations at admission time and mounts the Secret. |

The label and annotation key constants are shared within the repository:

- Controller: `LabelClientRegistrationInject`, `AnnotationKeycloakClientSecretName` in `clientregistration_controller.go`.
- Webhook: `LabelClientRegistrationInject` in `constants.go`, `AnnotationKeycloakClientSecretName` in `keycloak_client_credentials.go`.

### 2.2 Which workloads the operator reconciles

The **ClientRegistration** controller watches **Deployments** and **StatefulSets** whose pod template labels satisfy:

- `rossoctl.io/client-registration-inject` is **not** `"true"` (legacy sidecar opt-in disables the operator for that workload).
- `rossoctl.io/type` is **`agent`**, or **`tool`** when the cluster feature gate **`injectTools`** is true (tools are skipped if `injectTools` is false).

Other workloads are ignored by this controller.

### 2.3 Webhook behavior

1. **Precedence**: `rossoctl.io/client-registration-inject=true` **enables** injection of the client-registration sidecar / registration slice in combined authbridge; otherwise that path is off (`precedence.go`).
2. **After** sidecars and volumes are applied, **`ApplyKeycloakClientCredentialsSecretVolumes`** runs for **every** mutation:
   - If the pod (template) annotation `rossoctl.io/keycloak-client-credentials-secret-name` is set, the webhook adds a **Secret volume** named like the Secret (`rossoctl-keycloak-client-credentials-<uniq-id>`) and **subPath mounts** for `client-id.txt` and `client-secret.txt` into **each container that already has a `shared-data` volume mount**.
3. **Reinvocation**: if the pod is already considered “injected” (e.g. envoy or proxy-init present) but operator mounts are still missing, **`NeedsKeycloakClientCredentialsVolumePatch`** returns true and the webhook applies **only** the operator Secret mounts (`authbridge_webhook.go`).

### 2.4 Operator reconcile flow (simplified)

1. Read **cluster feature gates** (`rossoctl-webhook` ConfigMap in the cluster defaults namespace). If `globalEnabled` or `clientRegistration` is false, skip.
2. Read **`authbridge-config`** in the workload namespace (`KEYCLOAK_URL`, `KEYCLOAK_REALM`, etc.).
3. Read **`keycloak-admin-secret`** (admin username/password).
4. Compute **Keycloak client ID**:
   - If `--spire-trust-domain` is not set: `namespace/workloadName`.
   - If `--spire-trust-domain` is set: `spiffe://<trust-domain>/ns/<namespace>/sa/<serviceAccount>` (requires a **non-default** `serviceAccountName`).
5. **Register or fetch** the client via Keycloak admin API (`internal/keycloak`).
6. **Create or update** the credentials Secret; set **owner** to the Deployment/StatefulSet.
7. **Patch** the pod template annotation `rossoctl.io/keycloak-client-credentials-secret-name` to the deterministic secret name.

### 2.5 Feature flags

| Component | Flag / gate | Role |
|-----------|-------------|------|
| Operator (controller) | `--enable-operator-client-registration` (default **false**) | Master switch for the ClientRegistration controller. |
| Operator (controller) | `--spire-trust-domain` | Required for SPIFFE-shaped client IDs; presence of this flag enables SPIRE-based registration. |
| Operator (webhook) | `--enable-client-registration` | Cluster-wide gate for client-registration **injection** (precedence still applies). |
| Operator (webhook) | Feature gates file (`/etc/rossoctl/feature-gates/feature-gates.yaml`) | `clientRegistration`, `injectTools`, `globalEnabled`, etc., same as for injected sidecars. |

---

## 3. Requirements

### 3.1 Platform / namespace

- **`authbridge-config`** ConfigMap in the workload namespace with at least `KEYCLOAK_URL` and `KEYCLOAK_REALM`.
- **`keycloak-admin-secret`** in the same namespace with `KEYCLOAK_ADMIN_USERNAME` and `KEYCLOAK_ADMIN_PASSWORD`.
- **rossoctl-operator** deployed with webhook enabled (`webhook.enable: true` in Helm values).

### 3.2 Workload

- **Deployment** or **StatefulSet** (not bare Pods for operator ownership of Secrets).
- An **AgentRuntime** CR targeting the workload — the operator applies `rossoctl.io/type` to the Deployment/StatefulSet and its PodTemplateSpec (a `ValidatingAdmissionPolicy` prevents manual application). Do **not** set `rossoctl.io/client-registration-inject: "true"` unless you require the legacy sidecar.
- For **SPIRE-enabled** namespaces: `spec.template.spec.serviceAccountName` must be a **dedicated** ServiceAccount (not `default`).

### 3.3 Operator configuration

- Configure **`--spire-trust-domain`** to match the SPIRE server trust domain when SPIRE is deployed (same value as used for workload SPIFFE IDs).
- Ensure the operator can read **`authbridge-config`** and **`keycloak-admin-secret`** in agent namespaces, and create/update **`rossoctl-keycloak-client-credentials-*`** Secrets there (see RBAC below).

### 3.4 RBAC: why Secret rules are cluster-wide

The operator is installed with a **ClusterRole** bound to its ServiceAccount via a **ClusterRoleBinding** (the default kubebuilder layout in `config/rbac/role.yaml`). That pattern grants the listed verbs on **Secrets in every namespace**, which looks broad compared to “only agent namespaces.”

That shape is intentional for this controller:

1. **RBAC expressiveness** — A `PolicyRule` on `secrets` under a **ClusterRole** does not take a namespace allowlist. You cannot say “get/list/watch Secrets only in namespaces A, B, and C” in one cluster-scoped role. Namespace scoping is achieved by using **Roles** plus **RoleBindings** in each namespace (or by separate automation that maintains bindings), not by a single static manifest for a cluster-wide operator.

2. **Unknown agent namespaces at install time** — **ClientRegistration** reconciles **Deployments** and **StatefulSets** in **any** namespace where they match the label predicate. Platform teams add agent workloads and namespaces over time; the operator is not tied to a fixed list of namespaces configured when the ClusterRole is applied.

3. **Data plane placement** — **`authbridge-config`** and **`keycloak-admin-secret`** live in the **workload namespace** (same contract as the webhook-injected sidecar). The controller must **Get** those Secrets (and **Create**/**Patch**/**Update** the derived credentials Secret) in that namespace on every reconcile. Without cluster-wide Secret permissions, every new agent namespace would require a coordinated RBAC update before reconciliation could succeed.

4. **`list` / `watch`** — The kubebuilder marker generates **list** and **watch** alongside **get** for Secrets, consistent with other reconcilers in this project and with controller-runtime’s usual expectation that the delegating client can sync or fall back to the API without ad-hoc verb subsets per resource.

**Tighter models (not used here)** include: a dedicated **ClusterRole** that only contains Secret rules, bound only where needed via **RoleBinding** in each agent namespace (GitOps or a separate controller maintains those bindings); or policy engines that block the operator from namespaces that are not agent workloads. Those reduce blast radius but add operational steps. This project keeps a **single binding** and documents the trade-off.

**Optional pattern** — `config/rbac/manager_agent_secrets_clusterrole.yaml` plus per-namespace **RoleBinding** (`agent_namespace_secrets_role_binding.example.yaml`) scopes Secret access to namespaces where you install the binding; the main **manager-role** would then omit Secret verbs if you split the operator into two service accounts or otherwise separate concerns. The default install keeps Secrets on **manager-role** for simplicity.

### 3.5 Webhook configuration

- **`reinvocationPolicy: IfNeeded`** on the mutating webhook so late annotations still get mounts.
- Pod template must eventually carry **`rossoctl.io/keycloak-client-credentials-secret-name`** once the operator has reconciled; until then, auth consumers on `shared-data` may not see credentials (operator retries with backoff).

---

## 4. Migration strategy

### 4.1 Recommended rollout order

1. **Upgrade rossoctl-operator** to a version that includes the AuthBridge webhook and ClientRegistration controller.
2. **Enable the webhook** via Helm values (`webhook.enable: true`) and optionally `--enable-operator-client-registration`.
3. **Configure** `--spire-trust-domain` if agent namespaces use SPIRE.
4. **Remove the rossocortex webhook** chart dependency from the umbrella chart once the operator webhook is verified.

Since both the webhook and controller ship in the same binary, there is no ordering concern between them.

### 4.2 Operator-managed registration (default)

1. Ensure the namespace has `authbridge-config` and `keycloak-admin-secret`.
2. Use normal agent/tool labels; **omit** `rossoctl.io/client-registration-inject: "true"` unless you need the legacy sidecar.
3. If SPIRE is on, set a **dedicated** `serviceAccountName`.
4. **Restart** or roll the workload so the operator reconciles and the webhook applies Secret mounts (including on reinvocation).

The operator will create or reuse the Keycloak client and Secret; the webhook will inject mounts on create or on reinvocation.

### 4.3 Rollback to legacy sidecar registration

- Set **`rossoctl.io/client-registration-inject: "true"`** on the pod template and **remove** the operator annotation `rossoctl.io/keycloak-client-credentials-secret-name` if present.
- Roll pods so the **client-registration sidecar** (or combined authbridge with registration) runs again.
- Optionally delete operator-created Secrets named `rossoctl-keycloak-client-credentials-*` after confirming Keycloak clients are recreated by the sidecar path if needed.

Disabling **`--enable-operator-client-registration`** stops new reconciliation but does not remove existing annotations or Secrets; clean those up if you need a full rollback.

### 4.4 Keycloak client identity

Switching from **sidecar** to **operator** registration may change the **client ID** string (e.g. from SPIFFE-based to `namespace/name` when SPIRE is off, or same SPIFFE shape when SPIRE is on). Plan for **one-time** Keycloak client cleanup or renamed clients if both paths ran for the same logical workload.

### 4.5 Consolidation status

The webhook and operator now live in one repository (`rossoctl-operator`). This document is the single **source of truth** for the contract. Constants are co-located in the `internal/webhook/injector` package to avoid drift between annotation/label keys.

The webhook was previously part of `rossocortex`; migration tracking is in [rossocortex#266](https://github.com/rossoctl/rossocortex/issues/266).

---

## 5. Related code

| Area | Location |
|------|-----------|
| Operator reconciler | `internal/controller/clientregistration_controller.go` |
| Keycloak admin client | `internal/keycloak/` |
| Operator entrypoint / flags | `cmd/main.go` |
| Webhook mounts + reinvocation | `internal/webhook/injector/keycloak_client_credentials.go`, `pod_mutator.go`, `internal/webhook/v1alpha1/authbridge_webhook.go` |
| Injection precedence | `internal/webhook/injector/precedence.go` |

---

## 6. Operational notes

- If logs show **`cannot resolve Keycloak client id yet`** with reason **`--spire-trust-domain is required`**, configure the operator trust domain to match SPIRE (see platform docs / `rossoctl-deps` `spire.trustDomain` on Kind installs).
- Operator reads **`authbridge-config`** via an **uncached API reader** because ConfigMaps may be excluded from the controller-runtime cache for scalability; this matches how the webhook resolves namespace config.
