# Migration Plan: AgentCard Controller to Standard Kubernetes Workloads

**Status:** Draft
**Created:** 2026-01-30
**Target:** rossoctl-operator v2.x

## Executive Summary

This document outlines the migration of the AgentCard controller from watching and reconciling based on the custom `Agent` CRD to watching standard Kubernetes workloads (Deployments and StatefulSets) that are identified by specific labels.

This migration aligns with the Rossoctl UI migration documented in [rossoctl/docs/plans/migrate-agent-crd-to-workloads.md](https://github.com/rossoctl/rossoctl/blob/main/docs/plans/migrate-agent-crd-to-workloads.md), where agents are now deployed directly as Deployments/StatefulSets rather than through the Agent CRD.

**Goals:**
- Support agents deployed as Deployments and StatefulSets
- Use duck typing with `targetRef` for explicit workload references
- Maintain backward compatibility with Agent CRD during transition
- Enable automatic AgentCard creation for labeled workloads

---

## Table of Contents

1. [Current State Analysis](#1-current-state-analysis)
2. [Target State](#2-target-state)
3. [Label Standards](#3-label-standards)
4. [Migration Phases](#4-migration-phases)
5. [Detailed Implementation](#5-detailed-implementation)
6. [RBAC Requirements](#6-rbac-requirements)
7. [Testing Strategy](#7-testing-strategy)
8. [Backward Compatibility](#8-backward-compatibility)
9. [Rollback Plan](#9-rollback-plan)

---

## 1. Current State Analysis

### Current Architecture

The AgentCard system consists of two controllers:

```
┌─────────────────────────────────────────────────────────────────────┐
│                        rossoctl-operator                              │
│                                                                      │
│  ┌─────────────────────────┐    ┌─────────────────────────────┐    │
│  │ AgentCardSyncReconciler │    │    AgentCardReconciler      │    │
│  │                         │    │                             │    │
│  │ Watches: Agent CRD      │    │ Watches: AgentCard CRD      │    │
│  │ Creates: AgentCard      │───▶│ Reads: Agent CRD, Service   │    │
│  │                         │    │ Updates: AgentCard status   │    │
│  └───────────┬─────────────┘    └──────────────┬──────────────┘    │
│              │                                  │                   │
└──────────────│──────────────────────────────────│───────────────────┘
               │                                  │
               ▼                                  ▼
        ┌─────────────┐                   ┌─────────────────┐
        │  Agent CRD  │                   │ /.well-known/   │
        │             │◀──────────────────│  agent.json     │
        └──────┬──────┘   (lookup)        └────────▲────────┘
               │                                   │
               │ (creates)                         │ (HTTP fetch)
               ▼                                   │
        ┌─────────────┐      ┌──────────┐         │
        │  Deployment │──────│ Service  │─────────┘
        └─────────────┘      └──────────┘
```

### Current Controller Files

| File | Purpose |
|------|---------|
| `internal/controller/agentcard_controller.go` | Main AgentCard reconciler |
| `internal/controller/agentcardsync_controller.go` | Auto-creates AgentCard for Agent CRDs |
| `internal/agentcard/fetcher.go` | Fetches agent card from service endpoint |
| `api/v1alpha1/agentcard_types.go` | AgentCard CRD types |

### Current Agent CRD Dependencies

**AgentCardReconciler** depends on Agent CRD for:
1. **Agent Discovery**: Uses `AgentCard.Spec.Selector.MatchLabels` to find Agent
2. **Readiness Check**: Checks `Agent.Status.DeploymentStatus.Phase == PhaseReady`
3. **Protocol Detection**: Reads `rossoctl.io/agent-protocol` label from Agent
4. **Service Discovery**: Uses Agent name to find corresponding Service

**AgentCardSyncReconciler** depends on Agent CRD for:
1. **Watch Source**: Watches Agent CRD events
2. **Label Check**: Requires `rossoctl.io/type=agent` and `rossoctl.io/agent-protocol` labels
3. **Owner Reference**: Sets Agent as owner of AgentCard for garbage collection

### Current Label Requirements on Agent CRD

| Label | Purpose |
|-------|---------|
| `rossoctl.io/type=agent` | Identifies resource as an agent |
| `rossoctl.io/agent-protocol` | Protocol for fetching card (e.g., "a2a") |
| `app.kubernetes.io/name` | Used for service discovery and selector |

---

## 2. Target State

### Target Architecture

```
┌─────────────────────────────────────────────────────────────────────────┐
│                          rossoctl-operator                                │
│                                                                          │
│  ┌────────────────────────────┐    ┌─────────────────────────────┐      │
│  │ AgentCardSyncReconciler    │    │    AgentCardReconciler      │      │
│  │                            │    │                             │      │
│  │ Watches: Deployment,       │    │ Watches: AgentCard CRD      │      │
│  │          StatefulSet       │    │ Reads: Deployment/SS,       │      │
│  │ Creates: AgentCard         │───▶│        Service              │      │
│  │                            │    │ Updates: AgentCard status   │      │
│  └───────────┬────────────────┘    └──────────────┬──────────────┘      │
│              │                                     │                     │
└──────────────│─────────────────────────────────────│─────────────────────┘
               │                                     │
               ▼                                     ▼
        ┌─────────────┐                      ┌─────────────────┐
        │ Deployment  │    (or)              │ /.well-known/   │
        │ StatefulSet │◀─────────────────────│  agent.json     │
        └──────┬──────┘     (lookup)         └────────▲────────┘
               │                                      │
               │ (label selector)                     │ (HTTP fetch)
               ▼                                      │
        ┌─────────────┐                              │
        │  Service    │──────────────────────────────┘
        └─────────────┘
```

### Key Changes

1. **Watch Deployments and StatefulSets** instead of Agent CRD
2. **Use `targetRef` with duck typing** for explicit workload references
3. **Determine readiness** from Deployment/StatefulSet status conditions
4. **Owner references** point to Deployment/StatefulSet instead of Agent
5. **AgentCard spec** uses `targetRef` to directly reference the backing workload

### Benefits of targetRef with Duck Typing

Using `targetRef` instead of label selectors provides several advantages:

| Aspect | Label Selector | targetRef (Duck Typing) |
|--------|----------------|------------------------|
| **Explicitness** | Implicit matching via labels | Explicit reference by name |
| **Ambiguity** | Multiple workloads could match | Exactly one workload referenced |
| **Extensibility** | Must add code for new types | Any workload type supported |
| **Debugging** | Hard to trace which workload matched | Clear reference in spec |
| **K8s Convention** | Custom approach | Follows Gateway API pattern |

**Duck Typing Pattern:**

The controller uses `unstructured.Unstructured` to fetch any workload type without
needing compile-time knowledge of the specific resource type. This enables:

```yaml
# Any of these work without code changes:
targetRef:
  apiVersion: apps/v1
  kind: Deployment
  name: my-agent

targetRef:
  apiVersion: apps/v1
  kind: StatefulSet
  name: my-agent

targetRef:
  apiVersion: batch/v1
  kind: Job
  name: my-agent

targetRef:
  apiVersion: agent.rossoctl.dev/v1alpha1
  kind: Agent  # Legacy
  name: my-agent
```

---

## 3. Label Standards

As defined in the Rossoctl UI migration plan, all agent workloads must have these labels:

### Required Labels (All Workloads)

| Label | Value | Purpose |
|-------|-------|---------|
| `rossoctl.io/type` | `agent` | Identifies resource as a Rossoctl agent |
| `app.kubernetes.io/name` | `<agent-name>` | Standard K8s app name label |
| `protocol.rossoctl.io/<name>` | `""` (e.g., `protocol.rossoctl.io/a2a`) | Protocol(s) the agent speaks |

### Recommended Labels

| Label | Value | Purpose |
|-------|-------|---------|
| `app.kubernetes.io/managed-by` | `rossoctl-ui` | Resource manager |
| `app.kubernetes.io/component` | `agent` | Component type |
| `rossoctl.io/framework` | `<framework>` | Framework (LangGraph, CrewAI, etc.) |
| `rossoctl.io/workload-type` | `deployment` or `statefulset` | Workload type |

### Label Selector for Agents

To discover all agents in a namespace:

```
rossoctl.io/type=agent
```

To discover agents with a specific protocol:

```
rossoctl.io/type=agent,protocol.rossoctl.io/a2a
```

### Label Changes from Agent CRD

| Old Label (Agent CRD) | New Label (Workloads) |
|-----------------------|-----------------------|
| `rossoctl.io/agent-protocol: a2a` | `protocol.rossoctl.io/a2a: ""` |
| `rossoctl.io/protocol: a2a` | `protocol.rossoctl.io/a2a: ""` |

---

## 4. Migration Phases

### Phase 1: Update AgentCard CRD

Extend the AgentCard spec to support workload references using duck typing.

**Scope:**
- Add `targetRef` field with `apiVersion`, `kind`, and `name`
- Deprecate label-based `selector` field (keep for backward compatibility)
- Update validation to require either `targetRef` or `selector`
- Add print columns for workload type and name

### Phase 2: Update AgentCardReconciler

Update the main reconciler to support both Agent CRD and Deployments/StatefulSets.

**Scope:**
- Add methods to fetch workloads by `targetRef` (duck typing)
- Add readiness detection for Deployments and StatefulSets
- Update protocol label lookup to support both old and new labels
- Fall back to `selector` if `targetRef` is not specified

### Phase 3: Update AgentCardSyncReconciler

Update the sync reconciler to watch and create AgentCards for Deployments/StatefulSets.

**Scope:**
- Add watches for Deployments and StatefulSets
- Create AgentCards with `targetRef` instead of `selector`
- Update owner reference handling
- Handle both Agent CRD and workload sources

### Phase 4: Backward Compatibility Period

Run in hybrid mode supporting both Agent CRD and workloads.

**Scope:**
- Feature flag for legacy Agent CRD support
- Documentation for migration path
- Deprecation warnings

### Phase 5: Deprecate Agent CRD Support

Remove Agent CRD dependency after transition period.

**Scope:**
- Remove Agent CRD watches
- Clean up legacy code
- Update documentation

---

## 5. Detailed Implementation

### 5.1 AgentCard CRD Changes

**File:** `api/v1alpha1/agentcard_types.go`

Add `targetRef` using duck typing pattern (similar to Gateway API's PolicyTargetReference):

```go
// TargetRef identifies the workload that backs this agent using duck typing.
// This allows referencing any workload type (Deployment, StatefulSet, Job, etc.)
// without the controller needing explicit knowledge of each type.
type TargetRef struct {
    // APIVersion is the API version of the target resource (e.g., "apps/v1")
    // +kubebuilder:validation:Required
    APIVersion string `json:"apiVersion"`

    // Kind is the kind of the target resource (e.g., "Deployment", "StatefulSet")
    // +kubebuilder:validation:Required
    Kind string `json:"kind"`

    // Name is the name of the target resource
    // +kubebuilder:validation:Required
    Name string `json:"name"`
}

type AgentCardSpec struct {
    // SyncPeriod specifies how often to re-fetch the agent card.
    // Defaults to "30s".
    // +optional
    SyncPeriod string `json:"syncPeriod,omitempty"`

    // TargetRef identifies the workload backing this agent using duck typing.
    // The referenced workload must have the required Rossoctl labels.
    // +optional
    TargetRef *TargetRef `json:"targetRef,omitempty"`

    // Selector is DEPRECATED. Use TargetRef instead.
    // Kept for backward compatibility with existing AgentCards.
    // If both TargetRef and Selector are specified, TargetRef takes precedence.
    // +optional
    Selector *AgentSelector `json:"selector,omitempty"`
}

// AgentSelector is DEPRECATED. Use TargetRef instead.
type AgentSelector struct {
    // MatchLabels is a map of {key,value} pairs to match against
    // the labels of agent workloads (Deployment, StatefulSet, or Agent CRD)
    MatchLabels map[string]string `json:"matchLabels"`
}
```

Update AgentCardStatus to include workload information:

```go
type AgentCardStatus struct {
    // ... existing fields ...

    // TargetRef contains the resolved reference to the backing workload
    // +optional
    TargetRef *TargetRef `json:"targetRef,omitempty"`
}
```

**Example AgentCard with targetRef:**

```yaml
apiVersion: agent.rossoctl.dev/v1alpha1
kind: AgentCard
metadata:
  name: my-agent-card
  namespace: default
spec:
  syncPeriod: "30s"
  targetRef:
    apiVersion: apps/v1
    kind: Deployment       # or StatefulSet, Job, CronJob, etc.
    name: my-agent
```

**Validation:**

```go
// +kubebuilder:validation:XValidation:rule="has(self.targetRef) || has(self.selector)",message="either targetRef or selector must be specified"

func (r *AgentCard) ValidateCreate() (admission.Warnings, error) {
    if r.Spec.TargetRef == nil && r.Spec.Selector == nil {
        return nil, fmt.Errorf("either targetRef or selector must be specified")
    }
    if r.Spec.TargetRef != nil {
        if r.Spec.TargetRef.APIVersion == "" || r.Spec.TargetRef.Kind == "" || r.Spec.TargetRef.Name == "" {
            return nil, fmt.Errorf("targetRef requires apiVersion, kind, and name")
        }
    }
    return nil, nil
}
```

### 5.2 AgentCardReconciler Changes

**File:** `internal/controller/agentcard_controller.go`

#### 5.2.1 New Workload Fetcher Using Duck Typing

The key insight is that we use `unstructured.Unstructured` to fetch any workload type
without needing explicit type knowledge. This enables true duck typing.

```go
// WorkloadInfo contains information about a discovered agent workload
type WorkloadInfo struct {
    Name         string
    Namespace    string
    APIVersion   string
    Kind         string
    Labels       map[string]string
    Ready        bool
    ServiceName  string
    Object       *unstructured.Unstructured
}

// getWorkloadByTargetRef fetches the workload referenced by targetRef using duck typing
func (r *AgentCardReconciler) getWorkloadByTargetRef(
    ctx context.Context,
    namespace string,
    targetRef *v1alpha1.TargetRef,
) (*WorkloadInfo, error) {
    log := log.FromContext(ctx)

    // Create an unstructured object to fetch any resource type
    gvk := schema.FromAPIVersionAndKind(targetRef.APIVersion, targetRef.Kind)
    obj := &unstructured.Unstructured{}
    obj.SetGroupVersionKind(gvk)

    // Fetch the workload
    key := client.ObjectKey{Namespace: namespace, Name: targetRef.Name}
    if err := r.Get(ctx, key, obj); err != nil {
        if apierrors.IsNotFound(err) {
            return nil, fmt.Errorf("%w: %s/%s %s not found",
                ErrWorkloadNotFound, targetRef.Kind, namespace, targetRef.Name)
        }
        return nil, err
    }

    labels := obj.GetLabels()

    // Validate it's a Rossoctl agent
    if !isAgentWorkload(labels) {
        return nil, fmt.Errorf("%w: %s/%s does not have rossoctl.io/type=agent label",
            ErrNotAgentWorkload, targetRef.Kind, targetRef.Name)
    }

    // Determine readiness based on workload type
    ready, err := r.isWorkloadReady(ctx, obj, targetRef.Kind)
    if err != nil {
        log.Error(err, "Failed to determine workload readiness", "kind", targetRef.Kind)
        ready = false
    }

    return &WorkloadInfo{
        Name:        targetRef.Name,
        Namespace:   namespace,
        APIVersion:  targetRef.APIVersion,
        Kind:        targetRef.Kind,
        Labels:      labels,
        Ready:       ready,
        ServiceName: targetRef.Name,  // Convention: service has same name as workload
        Object:      obj,
    }, nil
}

// isWorkloadReady determines if a workload is ready to serve traffic
// Uses duck typing to check common status patterns
func (r *AgentCardReconciler) isWorkloadReady(
    ctx context.Context,
    obj *unstructured.Unstructured,
    kind string,
) (bool, error) {
    switch kind {
    case "Deployment":
        return r.isDeploymentReadyFromUnstructured(obj)
    case "StatefulSet":
        return r.isStatefulSetReadyFromUnstructured(obj)
    case "Agent":
        return r.isAgentReadyFromUnstructured(obj)
    default:
        // For unknown types, check for common ready conditions
        return r.hasReadyCondition(obj)
    }
}

// isDeploymentReadyFromUnstructured checks Deployment readiness
func (r *AgentCardReconciler) isDeploymentReadyFromUnstructured(obj *unstructured.Unstructured) (bool, error) {
    conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
    if err != nil || !found {
        return false, err
    }

    for _, c := range conditions {
        condition, ok := c.(map[string]interface{})
        if !ok {
            continue
        }
        if condition["type"] == "Available" && condition["status"] == "True" {
            return true, nil
        }
    }
    return false, nil
}

// isStatefulSetReadyFromUnstructured checks StatefulSet readiness
func (r *AgentCardReconciler) isStatefulSetReadyFromUnstructured(obj *unstructured.Unstructured) (bool, error) {
    readyReplicas, _, err := unstructured.NestedInt64(obj.Object, "status", "readyReplicas")
    if err != nil {
        return false, err
    }
    replicas, _, err := unstructured.NestedInt64(obj.Object, "status", "replicas")
    if err != nil {
        return false, err
    }
    return readyReplicas > 0 && readyReplicas == replicas, nil
}

// isAgentReadyFromUnstructured checks legacy Agent CRD readiness
func (r *AgentCardReconciler) isAgentReadyFromUnstructured(obj *unstructured.Unstructured) (bool, error) {
    phase, found, err := unstructured.NestedString(obj.Object, "status", "deploymentStatus", "phase")
    if err != nil || !found {
        return false, err
    }
    return phase == "Ready", nil
}

// hasReadyCondition is a generic check for workloads with standard conditions
func (r *AgentCardReconciler) hasReadyCondition(obj *unstructured.Unstructured) (bool, error) {
    conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
    if err != nil || !found {
        return false, nil  // No conditions = not ready
    }

    for _, c := range conditions {
        condition, ok := c.(map[string]interface{})
        if !ok {
            continue
        }
        condType, _ := condition["type"].(string)
        status, _ := condition["status"].(string)
        // Check for common "Ready" or "Available" conditions
        if (condType == "Ready" || condType == "Available") && status == "True" {
            return true, nil
        }
    }
    return false, nil
}

// isAgentWorkload checks if labels indicate this is a Rossoctl agent
func isAgentWorkload(labels map[string]string) bool {
    return labels[LabelRossoctlType] == ValueTypeAgent
}
```

#### 5.2.2 Fallback to Legacy Selector

For backward compatibility, support the deprecated `selector` field:

```go
// getWorkload fetches the workload using targetRef or falls back to selector
func (r *AgentCardReconciler) getWorkload(
    ctx context.Context,
    agentCard *v1alpha1.AgentCard,
) (*WorkloadInfo, error) {
    // Prefer targetRef if specified
    if agentCard.Spec.TargetRef != nil {
        return r.getWorkloadByTargetRef(ctx, agentCard.Namespace, agentCard.Spec.TargetRef)
    }

    // Fall back to deprecated selector
    if agentCard.Spec.Selector != nil {
        return r.findMatchingWorkloadBySelector(ctx, agentCard.Namespace, agentCard.Spec.Selector)
    }

    return nil, fmt.Errorf("neither targetRef nor selector specified")
}

// findMatchingWorkloadBySelector is the legacy method for selector-based lookup
func (r *AgentCardReconciler) findMatchingWorkloadBySelector(
    ctx context.Context,
    namespace string,
    selector *v1alpha1.AgentSelector,
) (*WorkloadInfo, error) {
    // ... existing label-based lookup logic ...
    // Search Deployments, StatefulSets, and optionally Agent CRDs
}
```

#### 5.2.3 Updated Protocol Detection

```go
// getWorkloadProtocol extracts the protocol from workload labels
// Supports both old (rossoctl.io/agent-protocol) and new (rossoctl.io/protocol) labels
func getWorkloadProtocol(labels map[string]string) string {
    // Try new label first
    if protocol := labels[LabelRossoctlProtocol]; protocol != "" {
        return protocol
    }
    // Fall back to old label
    return labels[LabelRossoctlAgentProtocol]
}
```

#### 5.2.4 Updated Reconcile Method

Update the main `Reconcile()` method to use `getWorkload()` which handles both `targetRef` and `selector`:

```go
func (r *AgentCardReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    log := log.FromContext(ctx)

    // Fetch AgentCard
    agentCard := &v1alpha1.AgentCard{}
    if err := r.Get(ctx, req.NamespacedName, agentCard); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // Handle deletion
    if !agentCard.DeletionTimestamp.IsZero() {
        return r.handleDeletion(ctx, agentCard)
    }

    // Ensure finalizer
    if err := r.ensureFinalizer(ctx, agentCard); err != nil {
        return ctrl.Result{}, err
    }

    // Get workload using targetRef (preferred) or selector (legacy)
    workload, err := r.getWorkload(ctx, agentCard)
    if err != nil {
        if errors.Is(err, ErrWorkloadNotFound) {
            log.Info("Workload not found", "targetRef", agentCard.Spec.TargetRef)
            r.updateCondition(ctx, agentCard, ConditionSynced, metav1.ConditionFalse,
                "WorkloadNotFound", err.Error())
            return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
        }
        if errors.Is(err, ErrNotAgentWorkload) {
            log.Info("Referenced resource is not an agent", "targetRef", agentCard.Spec.TargetRef)
            r.updateCondition(ctx, agentCard, ConditionSynced, metav1.ConditionFalse,
                "NotAgentWorkload", err.Error())
            return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
        }
        return ctrl.Result{}, err
    }

    // Check workload readiness
    if !workload.Ready {
        log.Info("Workload not ready", "name", workload.Name, "kind", workload.Kind)
        r.updateCondition(ctx, agentCard, ConditionSynced, metav1.ConditionFalse,
            "WorkloadNotReady", fmt.Sprintf("%s %s is not ready", workload.Kind, workload.Name))
        return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
    }

    // Get protocol
    protocol := getWorkloadProtocol(workload.Labels)
    if protocol == "" {
        log.Info("Workload has no protocol label", "name", workload.Name)
        r.updateCondition(ctx, agentCard, ConditionSynced, metav1.ConditionFalse,
            "NoProtocol", "Workload does not have rossoctl.io/protocol label")
        return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
    }

    // Get Service
    service, err := r.getService(ctx, agentCard.Namespace, workload.ServiceName)
    if err != nil {
        return r.handleServiceError(ctx, agentCard, err)
    }

    // Build service URL and fetch agent card
    serviceURL := r.buildServiceURL(workload.ServiceName, agentCard.Namespace, service)
    card, err := r.Fetcher.Fetch(ctx, serviceURL, protocol)
    if err != nil {
        return r.handleFetchError(ctx, agentCard, err)
    }

    // Override URL with cluster service URL
    card.URL = serviceURL

    // Update status with resolved targetRef
    agentCard.Status.Card = *card
    agentCard.Status.Protocol = protocol
    agentCard.Status.TargetRef = &v1alpha1.TargetRef{
        APIVersion: workload.APIVersion,
        Kind:       workload.Kind,
        Name:       workload.Name,
    }
    agentCard.Status.LastSyncTime = &metav1.Time{Time: time.Now()}

    r.updateCondition(ctx, agentCard, ConditionSynced, metav1.ConditionTrue,
        "Synced", "Successfully fetched agent card")
    r.updateCondition(ctx, agentCard, ConditionReady, metav1.ConditionTrue,
        "Ready", "Agent card is ready")

    if err := r.Status().Update(ctx, agentCard); err != nil {
        return ctrl.Result{}, err
    }

    // Requeue based on sync period
    syncPeriod := r.getSyncPeriod(agentCard)
    return ctrl.Result{RequeueAfter: syncPeriod}, nil
}
```

#### 5.2.5 Updated SetupWithManager

Add watches for Deployments and StatefulSets. When a workload changes, find AgentCards that reference it via `targetRef`:

```go
func (r *AgentCardReconciler) SetupWithManager(mgr ctrl.Manager) error {
    builder := ctrl.NewControllerManagedBy(mgr).
        For(&v1alpha1.AgentCard{})

    // Watch Deployments with agent labels
    builder = builder.Watches(
        &appsv1.Deployment{},
        handler.EnqueueRequestsFromMapFunc(r.mapWorkloadToAgentCard),
        builder.WithPredicates(agentLabelPredicate()),
    )

    // Watch StatefulSets with agent labels
    builder = builder.Watches(
        &appsv1.StatefulSet{},
        handler.EnqueueRequestsFromMapFunc(r.mapWorkloadToAgentCard),
        builder.WithPredicates(agentLabelPredicate()),
    )

    // Optionally watch legacy Agent CRD
    if r.EnableLegacyAgentCRD {
        builder = builder.Watches(
            &v1alpha1.Agent{},
            handler.EnqueueRequestsFromMapFunc(r.mapWorkloadToAgentCard),
            builder.WithPredicates(agentLabelPredicate()),
        )
    }

    return builder.Complete(r)
}

// agentLabelPredicate filters for resources with rossoctl.io/type=agent
func agentLabelPredicate() predicate.Predicate {
    return predicate.NewPredicateFuncs(func(obj client.Object) bool {
        labels := obj.GetLabels()
        return labels[LabelRossoctlType] == ValueTypeAgent
    })
}

// mapWorkloadToAgentCard maps workload events to AgentCard reconcile requests
// It finds AgentCards that reference this workload via targetRef
func (r *AgentCardReconciler) mapWorkloadToAgentCard(ctx context.Context, obj client.Object) []reconcile.Request {
    log := log.FromContext(ctx)

    // List all AgentCards in the namespace
    agentCards := &v1alpha1.AgentCardList{}
    if err := r.List(ctx, agentCards, client.InNamespace(obj.GetNamespace())); err != nil {
        log.Error(err, "Failed to list AgentCards")
        return nil
    }

    gvk := obj.GetObjectKind().GroupVersionKind()
    apiVersion := gvk.GroupVersion().String()
    kind := gvk.Kind

    var requests []reconcile.Request
    for _, card := range agentCards.Items {
        // Check if this AgentCard references this workload via targetRef
        if card.Spec.TargetRef != nil {
            if card.Spec.TargetRef.Name == obj.GetName() &&
               card.Spec.TargetRef.Kind == kind &&
               card.Spec.TargetRef.APIVersion == apiVersion {
                requests = append(requests, reconcile.Request{
                    NamespacedName: types.NamespacedName{
                        Name:      card.Name,
                        Namespace: card.Namespace,
                    },
                })
                continue
            }
        }

        // Fall back to checking selector (legacy)
        if card.Spec.Selector != nil && r.selectorMatchesWorkload(card.Spec.Selector, obj) {
            requests = append(requests, reconcile.Request{
                NamespacedName: types.NamespacedName{
                    Name:      card.Name,
                    Namespace: card.Namespace,
                },
            })
        }
    }

    return requests
}
```

### 5.3 AgentCardSyncReconciler Changes

**File:** `internal/controller/agentcardsync_controller.go`

#### 5.3.1 Updated Reconciler Structure

```go
type AgentCardSyncReconciler struct {
    client.Client
    Scheme               *runtime.Scheme
    EnableLegacyAgentCRD bool
}
```

#### 5.3.2 New Deployment/StatefulSet Reconcile Methods

```go
// ReconcileDeployment handles Deployment events
func (r *AgentCardSyncReconciler) ReconcileDeployment(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    log := log.FromContext(ctx)

    deployment := &appsv1.Deployment{}
    if err := r.Get(ctx, req.NamespacedName, deployment); err != nil {
        if apierrors.IsNotFound(err) {
            // Deployment deleted - AgentCard cleanup handled by owner references
            return ctrl.Result{}, nil
        }
        return ctrl.Result{}, err
    }

    // Check if this is an agent deployment
    if !r.shouldSyncWorkload(deployment.Labels) {
        return ctrl.Result{}, nil
    }

    // Create or update AgentCard with targetRef
    gvk := appsv1.SchemeGroupVersion.WithKind("Deployment")
    return r.ensureAgentCard(ctx, deployment, gvk)
}

// ReconcileStatefulSet handles StatefulSet events
func (r *AgentCardSyncReconciler) ReconcileStatefulSet(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    log := log.FromContext(ctx)

    statefulset := &appsv1.StatefulSet{}
    if err := r.Get(ctx, req.NamespacedName, statefulset); err != nil {
        if apierrors.IsNotFound(err) {
            return ctrl.Result{}, nil
        }
        return ctrl.Result{}, err
    }

    if !r.shouldSyncWorkload(statefulset.Labels) {
        return ctrl.Result{}, nil
    }

    gvk := appsv1.SchemeGroupVersion.WithKind("StatefulSet")
    return r.ensureAgentCard(ctx, statefulset, gvk)
}

// ReconcileAgent handles legacy Agent CRD events (backward compatibility)
func (r *AgentCardSyncReconciler) ReconcileAgent(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    log := log.FromContext(ctx)

    agent := &v1alpha1.Agent{}
    if err := r.Get(ctx, req.NamespacedName, agent); err != nil {
        if apierrors.IsNotFound(err) {
            return ctrl.Result{}, nil
        }
        return ctrl.Result{}, err
    }

    if !r.shouldSyncWorkload(agent.Labels) {
        return ctrl.Result{}, nil
    }

    gvk := v1alpha1.GroupVersion.WithKind("Agent")
    return r.ensureAgentCard(ctx, agent, gvk)
}

// shouldSyncWorkload checks if a workload should have an AgentCard created
func (r *AgentCardSyncReconciler) shouldSyncWorkload(labels map[string]string) bool {
    // Must have rossoctl.io/type=agent
    if labels[LabelRossoctlType] != ValueTypeAgent {
        return false
    }

    // Must have protocol label (new or old format)
    protocol := labels[LabelRossoctlProtocol]
    if protocol == "" {
        protocol = labels[LabelRossoctlAgentProtocol]
    }

    return protocol != ""
}

// ensureAgentCard creates or updates an AgentCard for a workload using targetRef
func (r *AgentCardSyncReconciler) ensureAgentCard(
    ctx context.Context,
    obj client.Object,
    gvk schema.GroupVersionKind,
) (ctrl.Result, error) {
    log := log.FromContext(ctx)

    cardName := r.getAgentCardName(obj.GetName())

    // Check if AgentCard already exists
    existingCard := &v1alpha1.AgentCard{}
    err := r.Get(ctx, types.NamespacedName{
        Name:      cardName,
        Namespace: obj.GetNamespace(),
    }, existingCard)

    if err == nil {
        // Card exists - ensure owner reference is set and targetRef is correct
        needsUpdate := false

        if !r.hasOwnerReference(existingCard, obj) {
            log.Info("Adding owner reference to existing AgentCard",
                "agentCard", cardName, "owner", obj.GetName())
            needsUpdate = true
        }

        // Update targetRef if it's still using the old selector format
        if existingCard.Spec.TargetRef == nil && existingCard.Spec.Selector != nil {
            log.Info("Migrating AgentCard from selector to targetRef",
                "agentCard", cardName)
            needsUpdate = true
        }

        if needsUpdate {
            return r.updateAgentCard(ctx, existingCard, obj, gvk)
        }
        return ctrl.Result{}, nil
    }

    if !apierrors.IsNotFound(err) {
        return ctrl.Result{}, err
    }

    // Create new AgentCard with targetRef
    log.Info("Creating AgentCard for workload",
        "agentCard", cardName,
        "kind", gvk.Kind,
        "workload", obj.GetName())

    return r.createAgentCard(ctx, obj, gvk, cardName)
}

// createAgentCard creates a new AgentCard for a workload using targetRef
func (r *AgentCardSyncReconciler) createAgentCard(
    ctx context.Context,
    obj client.Object,
    gvk schema.GroupVersionKind,
    cardName string,
) (ctrl.Result, error) {
    labels := obj.GetLabels()
    appName := labels[LabelAppName]
    if appName == "" {
        appName = obj.GetName()
    }

    agentCard := &v1alpha1.AgentCard{
        ObjectMeta: metav1.ObjectMeta{
            Name:      cardName,
            Namespace: obj.GetNamespace(),
            Labels: map[string]string{
                LabelAppName:   appName,
                LabelManagedBy: "rossoctl-operator",
            },
            OwnerReferences: []metav1.OwnerReference{
                *metav1.NewControllerRef(obj, gvk),
            },
        },
        Spec: v1alpha1.AgentCardSpec{
            SyncPeriod: "30s",
            TargetRef: &v1alpha1.TargetRef{
                APIVersion: gvk.GroupVersion().String(),
                Kind:       gvk.Kind,
                Name:       obj.GetName(),
            },
        },
    }

    if err := r.Create(ctx, agentCard); err != nil {
        return ctrl.Result{}, err
    }

    return ctrl.Result{}, nil
}

// updateAgentCard updates an existing AgentCard to use targetRef
func (r *AgentCardSyncReconciler) updateAgentCard(
    ctx context.Context,
    card *v1alpha1.AgentCard,
    obj client.Object,
    gvk schema.GroupVersionKind,
) (ctrl.Result, error) {
    // Set owner reference if missing
    if !r.hasOwnerReference(card, obj) {
        card.OwnerReferences = append(card.OwnerReferences,
            *metav1.NewControllerRef(obj, gvk))
    }

    // Set targetRef (migrate from selector)
    card.Spec.TargetRef = &v1alpha1.TargetRef{
        APIVersion: gvk.GroupVersion().String(),
        Kind:       gvk.Kind,
        Name:       obj.GetName(),
    }

    if err := r.Update(ctx, card); err != nil {
        return ctrl.Result{}, err
    }

    return ctrl.Result{}, nil
}

// getAgentCardName generates the AgentCard name from workload name
func (r *AgentCardSyncReconciler) getAgentCardName(workloadName string) string {
    return workloadName + "-card"
}
```

#### 5.3.3 Updated SetupWithManager

```go
func (r *AgentCardSyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
    // Watch Deployments
    if err := ctrl.NewControllerManagedBy(mgr).
        Named("agentcardsync-deployment").
        For(&appsv1.Deployment{}).
        WithEventFilter(agentLabelPredicate()).
        Complete(reconcile.Func(r.ReconcileDeployment)); err != nil {
        return err
    }

    // Watch StatefulSets
    if err := ctrl.NewControllerManagedBy(mgr).
        Named("agentcardsync-statefulset").
        For(&appsv1.StatefulSet{}).
        WithEventFilter(agentLabelPredicate()).
        Complete(reconcile.Func(r.ReconcileStatefulSet)); err != nil {
        return err
    }

    // Optionally watch legacy Agent CRD
    if r.EnableLegacyAgentCRD {
        if err := ctrl.NewControllerManagedBy(mgr).
            Named("agentcardsync-agent").
            For(&v1alpha1.Agent{}).
            WithEventFilter(agentLabelPredicate()).
            Complete(reconcile.Func(r.ReconcileAgent)); err != nil {
            return err
        }
    }

    return nil
}
```

### 5.4 Constants and Labels

**File:** `internal/controller/constants.go` (new file)

```go
package controller

import "errors"

const (
    // Label keys
    LabelRossoctlType          = "rossoctl.io/type"
    LabelRossoctlProtocol      = "rossoctl.io/protocol"
    LabelRossoctlAgentProtocol = "rossoctl.io/agent-protocol"  // Legacy
    LabelRossoctlWorkloadType  = "rossoctl.io/workload-type"
    LabelAppName              = "app.kubernetes.io/name"
    LabelManagedBy            = "app.kubernetes.io/managed-by"

    // Label values
    ValueTypeAgent         = "agent"
    ValueManagedByOperator = "rossoctl-operator"
)

var (
    // ErrWorkloadNotFound indicates the referenced workload does not exist
    ErrWorkloadNotFound = errors.New("workload not found")

    // ErrNotAgentWorkload indicates the workload doesn't have required agent labels
    ErrNotAgentWorkload = errors.New("resource is not a Rossoctl agent")
)
```

### 5.5 Main.go Updates

**File:** `cmd/main.go`

Add feature flag and update controller registration:

```go
func main() {
    // ... existing flags ...

    var enableLegacyAgentCRD bool
    flag.BoolVar(&enableLegacyAgentCRD, "enable-legacy-agent-crd", true,
        "Enable support for legacy Agent CRD (set to false after full migration)")

    // ... manager setup ...

    // Setup AgentCardReconciler
    if err = (&controller.AgentCardReconciler{
        Client:               mgr.GetClient(),
        Scheme:               mgr.GetScheme(),
        Fetcher:              agentcard.NewDefaultFetcher(),
        EnableLegacyAgentCRD: enableLegacyAgentCRD,
    }).SetupWithManager(mgr); err != nil {
        setupLog.Error(err, "unable to create controller", "controller", "AgentCard")
        os.Exit(1)
    }

    // Setup AgentCardSyncReconciler
    if err = (&controller.AgentCardSyncReconciler{
        Client:               mgr.GetClient(),
        Scheme:               mgr.GetScheme(),
        EnableLegacyAgentCRD: enableLegacyAgentCRD,
    }).SetupWithManager(mgr); err != nil {
        setupLog.Error(err, "unable to create controller", "controller", "AgentCardSync")
        os.Exit(1)
    }
}
```

---

## 6. RBAC Requirements

### Updated ClusterRole

**File:** `config/rbac/role.yaml`

Add permissions for Deployments and StatefulSets:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: rossoctl-operator-manager-role
rules:
  # Existing AgentCard permissions
  - apiGroups: ["agent.rossoctl.dev"]
    resources: ["agentcards"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["agent.rossoctl.dev"]
    resources: ["agentcards/status"]
    verbs: ["get", "update", "patch"]
  - apiGroups: ["agent.rossoctl.dev"]
    resources: ["agentcards/finalizers"]
    verbs: ["update"]

  # Legacy Agent CRD permissions (can be removed after migration)
  - apiGroups: ["agent.rossoctl.dev"]
    resources: ["agents"]
    verbs: ["get", "list", "watch"]

  # NEW: Deployment permissions
  - apiGroups: ["apps"]
    resources: ["deployments"]
    verbs: ["get", "list", "watch"]

  # NEW: StatefulSet permissions
  - apiGroups: ["apps"]
    resources: ["statefulsets"]
    verbs: ["get", "list", "watch"]

  # Service permissions (existing)
  - apiGroups: [""]
    resources: ["services"]
    verbs: ["get", "list", "watch"]
```

---

## 7. Testing Strategy

### 7.1 Unit Tests

**File:** `internal/controller/agentcard_controller_test.go`

Add test cases for:

- `getWorkloadByTargetRef()` fetches Deployment correctly
- `getWorkloadByTargetRef()` fetches StatefulSet correctly
- `getWorkloadByTargetRef()` fetches legacy Agent CRD correctly
- `getWorkloadByTargetRef()` returns error for non-existent workload
- `getWorkloadByTargetRef()` returns error for workload without agent labels
- `isWorkloadReady()` correctly evaluates Deployment conditions
- `isWorkloadReady()` correctly evaluates StatefulSet replica counts
- `isWorkloadReady()` correctly evaluates Agent CRD phase
- `getWorkloadProtocol()` supports both old and new labels
- `mapWorkloadToAgentCard()` finds AgentCards by targetRef
- `mapWorkloadToAgentCard()` falls back to selector matching
- `getWorkload()` prefers targetRef over selector

**File:** `internal/controller/agentcardsync_controller_test.go`

Add test cases for:

- `ReconcileDeployment()` creates AgentCard with targetRef
- `ReconcileStatefulSet()` creates AgentCard with targetRef
- `createAgentCard()` sets correct apiVersion/kind/name in targetRef
- `updateAgentCard()` migrates selector-based AgentCard to targetRef
- `shouldSyncWorkload()` requires correct labels
- Owner references are correctly set
- AgentCard cleanup on workload deletion

### 7.2 Integration Tests

**File:** `internal/controller/integration_test.go`

Test scenarios:

1. **Deployment-based agent flow with targetRef:**
   - Create Deployment with agent labels
   - Verify AgentCard is automatically created with correct targetRef
   - Verify targetRef contains `apiVersion: apps/v1`, `kind: Deployment`
   - Verify agent card is fetched when Deployment is ready
   - Delete Deployment, verify AgentCard is garbage collected

2. **StatefulSet-based agent flow with targetRef:**
   - Similar to above but with StatefulSet
   - Verify targetRef contains `apiVersion: apps/v1`, `kind: StatefulSet`

3. **Legacy selector migration:**
   - Create AgentCard with selector (simulating pre-migration state)
   - Create matching Deployment
   - Verify AgentCard is updated to include targetRef
   - Verify AgentCard continues to work correctly

4. **Mixed mode (backward compatibility):**
   - Create Agent CRD
   - Verify AgentCard is created with targetRef to Agent
   - Create Deployment with same name
   - Verify separate AgentCards exist (no conflict due to explicit targetRef)

### 7.3 E2E Tests

Test full workflow with real cluster:

- Deploy sample agent as Deployment
- Verify AgentCard is created with correct targetRef
- Verify AgentCard status shows correct protocol and URL
- Verify agent card data is fetched correctly
- Test sync period functionality
- Create manual AgentCard with targetRef pointing to existing Deployment
- Verify manual AgentCard works correctly

### 7.4 Manual Testing Checklist

- [ ] Deploy agent as Deployment with required labels
- [ ] Verify AgentCard is automatically created with targetRef
- [ ] Verify AgentCard.spec.targetRef has correct apiVersion, kind, name
- [ ] Verify AgentCard.status.targetRef is populated after sync
- [ ] Verify AgentCard status shows "Synced" condition
- [ ] Verify agent card data is populated
- [ ] Deploy agent as StatefulSet
- [ ] Verify AgentCard works with StatefulSet
- [ ] Create manual AgentCard with targetRef to existing Deployment
- [ ] Test with legacy selector-based AgentCard (migration)
- [ ] Test with legacy Agent CRD (backward compatibility)
- [ ] Test with `--enable-legacy-agent-crd=false`
- [ ] Verify owner reference cleanup on workload deletion

---

## 8. Backward Compatibility

### 8.1 Hybrid Mode

During the migration period, the controller supports both:
- Legacy Agent CRD
- Deployments and StatefulSets with agent labels

This is controlled by the `--enable-legacy-agent-crd` flag (default: `true`).

### 8.2 Label Compatibility

The controller supports both label formats:
- New: `rossoctl.io/protocol`
- Old: `rossoctl.io/agent-protocol`

### 8.3 AgentCard Spec Compatibility

**targetRef vs selector:**

The new `targetRef` field is the preferred way to reference workloads. The legacy `selector` field is deprecated but still supported for backward compatibility.

| Spec Field | Status | Behavior |
|------------|--------|----------|
| `targetRef` only | Recommended | Direct lookup by apiVersion/kind/name |
| `selector` only | Deprecated | Label-based search across workload types |
| Both specified | Allowed | `targetRef` takes precedence |
| Neither specified | Invalid | Validation error |

**Automatic Migration:**

The `AgentCardSyncReconciler` will automatically update existing AgentCards:
1. When it reconciles a workload with an existing AgentCard using `selector`
2. It will add `targetRef` to the AgentCard spec
3. The `selector` field is preserved but no longer used

**Example migration:**

Before (legacy):
```yaml
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: my-agent
      rossoctl.io/type: agent
```

After (migrated):
```yaml
spec:
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: my-agent
  selector:  # Preserved for reference, but not used
    matchLabels:
      app.kubernetes.io/name: my-agent
      rossoctl.io/type: agent
```

### 8.4 Migration Path

1. **Phase 1:** Deploy updated operator (hybrid mode)
2. **Phase 2:** Create new agents as Deployments/StatefulSets (AgentCards created with `targetRef`)
3. **Phase 3:** Existing AgentCards with `selector` are automatically migrated to use `targetRef`
4. **Phase 4:** Migrate existing Agent CRDs using rossoctl migration tool
5. **Phase 5:** Set `--enable-legacy-agent-crd=false`
6. **Phase 6:** Remove Agent CRD from cluster

---

## 9. Rollback Plan

### 9.1 Quick Rollback

If issues are discovered after deployment:

1. Set `--enable-legacy-agent-crd=true` (if it was disabled)
2. Restart the operator
3. Existing Agent CRDs will continue to work

### 9.2 Full Rollback

To revert to the previous version:

1. Deploy previous operator version
2. AgentCards for Deployments/StatefulSets will stop syncing
3. AgentCards for Agent CRDs will continue working
4. Manually delete orphaned AgentCards if needed

### 9.3 Data Preservation

- AgentCard resources remain in cluster during rollback
- Agent card data is preserved in status
- No data loss expected

---

## Open Questions

1. **Should AgentCards created for Deployments/StatefulSets have different naming?**
   - Current: `<workload-name>-card`
   - Alternative: `<workload-name>-<workload-type>-card`
   - With `targetRef`, naming is less important since the reference is explicit

2. **How to handle agents that exist as both Agent CRD and Deployment?**
   - With `targetRef`, this is no longer ambiguous - each AgentCard explicitly references one workload
   - If AgentCard uses legacy `selector`, prefer Deployment, log warning

3. **Should we support Jobs/CronJobs?**
   - Jobs are typically short-lived, may not need AgentCards
   - With duck typing, support can be added without code changes
   - Need to define readiness detection for Jobs (Succeeded condition)

4. **Should we remove the deprecated `selector` field in a future version?**
   - Recommendation: Keep for at least 2 minor versions, then remove
   - Document deprecation in release notes

5. **Should `targetRef` support cross-namespace references?**
   - Current design: Same namespace only (consistent with owner references)
   - Future consideration: Add optional `namespace` field to `targetRef`

---

## References

- [Rossoctl UI Migration Plan](https://github.com/rossoctl/rossoctl/blob/main/docs/plans/migrate-agent-crd-to-workloads.md)
- [Kubernetes Deployments](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/)
- [Kubernetes StatefulSets](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/)
- [Controller Runtime Watches](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/builder)
