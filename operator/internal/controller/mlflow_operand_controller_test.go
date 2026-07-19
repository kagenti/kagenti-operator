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

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/rossoctl/operator/internal/mlflow"
)

func newMLflowTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = discoveryv1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	_ = rbacv1.AddToScheme(s)
	_ = mlflow.AddToScheme(s)
	return s
}

func readyEndpointSlice() *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      MLflowOperandName + "-abc",
			Namespace: MLflowOperandNamespace,
			Labels:    map[string]string{"kubernetes.io/service-name": MLflowOperandName},
		},
		Endpoints: []discoveryv1.Endpoint{
			{Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}},
		},
	}
}

func notReadyEndpointSlice() *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      MLflowOperandName + "-abc",
			Namespace: MLflowOperandNamespace,
			Labels:    map[string]string{"kubernetes.io/service-name": MLflowOperandName},
		},
		Endpoints: []discoveryv1.Endpoint{
			{Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(false)}},
		},
	}
}

func newDSC(managementState string) *mlflow.DataScienceCluster {
	return &mlflow.DataScienceCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: DSCName,
		},
		Spec: mlflow.DSCSpec{
			Components: mlflow.DSCComponents{
				MLflowOperator: mlflow.DSCComponentState{
					ManagementState: managementState,
				},
			},
		},
	}
}

func newOperandReconciler(cl client.Client, scheme *runtime.Scheme) *MLflowOperandReconciler {
	return &MLflowOperandReconciler{
		Client:            cl,
		Scheme:            scheme,
		OperatorNamespace: "rossoctl-system",
		ResolveMLflowClusterRole: func(_ context.Context) string {
			return MLflowClusterRoleIntegration
		},
	}
}

var _ = Describe("MLflow Operand Controller", func() {
	ctx := context.Background()

	reconcileRequest := func() reconcile.Request {
		return reconcile.Request{
			NamespacedName: types.NamespacedName{Name: DSCName},
		}
	}

	Context("When DataScienceCluster does not exist", func() {
		It("should return without error (RHOAI absent)", func() {
			s := newMLflowTestScheme()
			cl := fake.NewClientBuilder().WithScheme(s).Build()
			r := newOperandReconciler(cl, s)

			result, err := r.Reconcile(ctx, reconcileRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	Context("When DSC mlflowoperator is not Managed", func() {
		It("should skip when managementState is Removed", func() {
			s := newMLflowTestScheme()
			dsc := newDSC("Removed")
			cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dsc).Build()
			r := newOperandReconciler(cl, s)

			result, err := r.Reconcile(ctx, reconcileRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			// MLflow CR should NOT be created
			mlflowCR := &mlflow.MLflow{}
			err = cl.Get(ctx, types.NamespacedName{
				Name: MLflowOperandName, Namespace: MLflowOperandNamespace,
			}, mlflowCR)
			Expect(err).To(HaveOccurred())
		})

		It("should skip when managementState is empty", func() {
			s := newMLflowTestScheme()
			dsc := newDSC("")
			cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dsc).Build()
			r := newOperandReconciler(cl, s)

			result, err := r.Reconcile(ctx, reconcileRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	Context("When DSC mlflowoperator is Managed", func() {
		It("should create the MLflow CR and requeue", func() {
			s := newMLflowTestScheme()
			dsc := newDSC("Managed")
			// The namespace must exist for the fake client.
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: MLflowOperandNamespace}}
			cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dsc, ns).Build()
			r := newOperandReconciler(cl, s)

			result, err := r.Reconcile(ctx, reconcileRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueMLflowNotReady))

			// Verify MLflow CR was created with correct spec
			mlflowCR := &mlflow.MLflow{}
			Expect(cl.Get(ctx, types.NamespacedName{
				Name: MLflowOperandName, Namespace: MLflowOperandNamespace,
			}, mlflowCR)).To(Succeed())

			Expect(mlflowCR.Labels[LabelManagedBy]).To(Equal(LabelManagedByValue))
			Expect(mlflowCR.Spec.BackendStoreURI).To(Equal("sqlite:////mlflow/mlflow.db"))
			Expect(mlflowCR.Spec.ArtifactsDestination).To(Equal("file:///mlflow/artifacts"))
			Expect(mlflowCR.Spec.ServeArtifacts).To(BeTrue())
			Expect(mlflowCR.Spec.Storage).NotTo(BeNil())
			Expect(mlflowCR.Spec.Storage.AccessModes).To(Equal([]string{"ReadWriteOnce"}))
			Expect(mlflowCR.Spec.Storage.Resources.Requests["storage"]).To(Equal("10Gi"))
		})

		It("should skip creation when MLflow CR already exists", func() {
			s := newMLflowTestScheme()
			dsc := newDSC("Managed")
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: MLflowOperandNamespace}}
			existingMLflow := &mlflow.MLflow{
				ObjectMeta: metav1.ObjectMeta{
					Name:      MLflowOperandName,
					Namespace: MLflowOperandNamespace,
				},
			}
			cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dsc, ns, existingMLflow).Build()
			r := newOperandReconciler(cl, s)

			// MLflow CR exists but Service does not — should requeue waiting for readiness
			result, err := r.Reconcile(ctx, reconcileRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueMLflowNotReady))
		})

		It("should wait when MLflow Service has no ready endpoints", func() {
			s := newMLflowTestScheme()
			dsc := newDSC("Managed")
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: MLflowOperandNamespace}}
			existingMLflow := &mlflow.MLflow{
				ObjectMeta: metav1.ObjectMeta{
					Name:      MLflowOperandName,
					Namespace: MLflowOperandNamespace,
				},
			}
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      MLflowOperandName,
					Namespace: MLflowOperandNamespace,
				},
			}
			cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dsc, ns, existingMLflow, svc, notReadyEndpointSlice()).Build()
			r := newOperandReconciler(cl, s)

			result, err := r.Reconcile(ctx, reconcileRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueMLflowNotReady))
		})

		It("should proceed to OTEL RoleBindings when MLflow is ready", func() {
			s := newMLflowTestScheme()
			dsc := newDSC("Managed")
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: MLflowOperandNamespace}}
			agentNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team1"}}
			existingMLflow := &mlflow.MLflow{
				ObjectMeta: metav1.ObjectMeta{
					Name:      MLflowOperandName,
					Namespace: MLflowOperandNamespace,
				},
			}
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      MLflowOperandName,
					Namespace: MLflowOperandNamespace,
				},
			}
			agentDep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-agent",
					Namespace: "team1",
					Labels: map[string]string{
						LabelAgentType: LabelValueAgent,
					},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To(int32(1)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "my-agent"},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "my-agent"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "agent", Image: "img:latest"}},
						},
					},
				},
			}

			cl := fake.NewClientBuilder().WithScheme(s).
				WithObjects(dsc, ns, agentNS, existingMLflow, svc, readyEndpointSlice(), agentDep).Build()
			r := newOperandReconciler(cl, s)

			result, err := r.Reconcile(ctx, reconcileRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			// Verify OTEL RoleBinding in agent namespace
			rb := &rbacv1.RoleBinding{}
			Expect(cl.Get(ctx, types.NamespacedName{
				Name: OTELCollectorRoleBindingName, Namespace: "team1",
			}, rb)).To(Succeed())
			Expect(rb.RoleRef.Kind).To(Equal("ClusterRole"))
			Expect(rb.RoleRef.Name).To(Equal(MLflowClusterRoleIntegration))
			Expect(rb.Subjects).To(HaveLen(1))
			Expect(rb.Subjects[0].Kind).To(Equal("ServiceAccount"))
			Expect(rb.Subjects[0].Name).To(Equal(OTELCollectorSAName))
			Expect(rb.Subjects[0].Namespace).To(Equal("rossoctl-system"))
			Expect(rb.Labels[LabelManagedBy]).To(Equal(LabelManagedByValue))

			// Verify OTEL RoleBinding in MLflow namespace
			rb2 := &rbacv1.RoleBinding{}
			Expect(cl.Get(ctx, types.NamespacedName{
				Name: OTELCollectorRoleBindingName, Namespace: MLflowOperandNamespace,
			}, rb2)).To(Succeed())
			Expect(rb2.RoleRef.Name).To(Equal(MLflowClusterRoleIntegration))
		})

		It("should create OTEL RoleBindings in multiple agent namespaces", func() {
			s := newMLflowTestScheme()
			dsc := newDSC("Managed")
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: MLflowOperandNamespace}}
			ns1 := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team1"}}
			ns2 := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team2"}}
			existingMLflow := &mlflow.MLflow{
				ObjectMeta: metav1.ObjectMeta{
					Name: MLflowOperandName, Namespace: MLflowOperandNamespace,
				},
			}
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: MLflowOperandName, Namespace: MLflowOperandNamespace,
				},
			}
			dep1 := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name: "agent-1", Namespace: "team1",
					Labels: map[string]string{LabelAgentType: LabelValueAgent},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To(int32(1)),
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "a1"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "a1"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "i:l"}}},
					},
				},
			}
			dep2 := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name: "agent-2", Namespace: "team2",
					Labels: map[string]string{LabelAgentType: LabelValueAgent},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To(int32(1)),
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "a2"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "a2"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "i:l"}}},
					},
				},
			}

			cl := fake.NewClientBuilder().WithScheme(s).
				WithObjects(dsc, ns, ns1, ns2, existingMLflow, svc, readyEndpointSlice(), dep1, dep2).Build()
			r := newOperandReconciler(cl, s)

			result, err := r.Reconcile(ctx, reconcileRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			for _, namespace := range []string{"team1", "team2", MLflowOperandNamespace} {
				rb := &rbacv1.RoleBinding{}
				Expect(cl.Get(ctx, types.NamespacedName{
					Name: OTELCollectorRoleBindingName, Namespace: namespace,
				}, rb)).To(Succeed(), "expected RoleBinding in namespace %s", namespace)
			}
		})

		It("should skip OTEL RoleBindings when no ClusterRole exists", func() {
			s := newMLflowTestScheme()
			dsc := newDSC("Managed")
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: MLflowOperandNamespace}}
			existingMLflow := &mlflow.MLflow{
				ObjectMeta: metav1.ObjectMeta{
					Name: MLflowOperandName, Namespace: MLflowOperandNamespace,
				},
			}
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: MLflowOperandName, Namespace: MLflowOperandNamespace,
				},
			}
			cl := fake.NewClientBuilder().WithScheme(s).
				WithObjects(dsc, ns, existingMLflow, svc, readyEndpointSlice()).Build()
			r := &MLflowOperandReconciler{
				Client:            cl,
				Scheme:            s,
				OperatorNamespace: "rossoctl-system",
				ResolveMLflowClusterRole: func(_ context.Context) string {
					return "" // no ClusterRole found
				},
			}

			result, err := r.Reconcile(ctx, reconcileRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})

		It("should recreate RoleBinding when roleRef changes (upgrade path)", func() {
			s := newMLflowTestScheme()
			dsc := newDSC("Managed")
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: MLflowOperandNamespace}}
			agentNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team1"}}
			existingMLflow := &mlflow.MLflow{
				ObjectMeta: metav1.ObjectMeta{
					Name: MLflowOperandName, Namespace: MLflowOperandNamespace,
				},
			}
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: MLflowOperandName, Namespace: MLflowOperandNamespace,
				},
			}
			agentDep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name: "my-agent", Namespace: "team1",
					Labels: map[string]string{LabelAgentType: LabelValueAgent},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To(int32(1)),
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "a"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "a"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "i:l"}}},
					},
				},
			}
			staleRB := &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:      OTELCollectorRoleBindingName,
					Namespace: "team1",
					Labels:    map[string]string{LabelManagedBy: LabelManagedByValue},
				},
				RoleRef: rbacv1.RoleRef{
					APIGroup: rbacv1.GroupName,
					Kind:     "ClusterRole",
					Name:     MLflowClusterRoleEdit,
				},
				Subjects: []rbacv1.Subject{
					{Kind: rbacv1.ServiceAccountKind, Name: OTELCollectorSAName, Namespace: "rossoctl-system"},
				},
			}

			cl := fake.NewClientBuilder().WithScheme(s).
				WithObjects(dsc, ns, agentNS, existingMLflow, svc, readyEndpointSlice(), agentDep, staleRB).Build()
			r := newOperandReconciler(cl, s)

			result, err := r.Reconcile(ctx, reconcileRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			rb := &rbacv1.RoleBinding{}
			Expect(cl.Get(ctx, types.NamespacedName{
				Name: OTELCollectorRoleBindingName, Namespace: "team1",
			}, rb)).To(Succeed())
			Expect(rb.RoleRef.Name).To(Equal(MLflowClusterRoleIntegration))
		})

		It("should be idempotent on repeated reconciliation", func() {
			s := newMLflowTestScheme()
			dsc := newDSC("Managed")
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: MLflowOperandNamespace}}
			agentNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team1"}}
			existingMLflow := &mlflow.MLflow{
				ObjectMeta: metav1.ObjectMeta{
					Name: MLflowOperandName, Namespace: MLflowOperandNamespace,
				},
			}
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: MLflowOperandName, Namespace: MLflowOperandNamespace,
				},
			}
			agentDep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name: "my-agent", Namespace: "team1",
					Labels: map[string]string{LabelAgentType: LabelValueAgent},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To(int32(1)),
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "a"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "a"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "i:l"}}},
					},
				},
			}

			cl := fake.NewClientBuilder().WithScheme(s).
				WithObjects(dsc, ns, agentNS, existingMLflow, svc, readyEndpointSlice(), agentDep).Build()
			r := newOperandReconciler(cl, s)

			// First reconcile
			result, err := r.Reconcile(ctx, reconcileRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			// Second reconcile — should succeed identically
			result, err = r.Reconcile(ctx, reconcileRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			// RoleBinding should still exist and be correct
			rb := &rbacv1.RoleBinding{}
			Expect(cl.Get(ctx, types.NamespacedName{
				Name: OTELCollectorRoleBindingName, Namespace: "team1",
			}, rb)).To(Succeed())
			Expect(rb.RoleRef.Name).To(Equal(MLflowClusterRoleIntegration))
		})
	})

	Context("DSC state change from Managed to Removed", func() {
		It("should be a no-op when state changes to Removed (existing resources stay)", func() {
			s := newMLflowTestScheme()
			dsc := newDSC("Removed")
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: MLflowOperandNamespace}}
			// Existing MLflow CR and RoleBinding from a previous reconcile
			existingMLflow := &mlflow.MLflow{
				ObjectMeta: metav1.ObjectMeta{
					Name: MLflowOperandName, Namespace: MLflowOperandNamespace,
				},
			}
			existingRB := &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name: OTELCollectorRoleBindingName, Namespace: "team1",
				},
				RoleRef: rbacv1.RoleRef{
					APIGroup: rbacv1.GroupName,
					Kind:     "ClusterRole",
					Name:     MLflowClusterRoleIntegration,
				},
			}

			cl := fake.NewClientBuilder().WithScheme(s).
				WithObjects(dsc, ns, existingMLflow, existingRB).Build()
			r := newOperandReconciler(cl, s)

			result, err := r.Reconcile(ctx, reconcileRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			// MLflow CR should still exist (we don't delete on state change)
			Expect(cl.Get(ctx, types.NamespacedName{
				Name: MLflowOperandName, Namespace: MLflowOperandNamespace,
			}, &mlflow.MLflow{})).To(Succeed())
		})
	})
})
