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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rossoctl/operator/internal/mlflow"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("MLflow Controller", func() {
	const namespace = "default"

	ctx := context.Background()

	newAgentDeployment := func(name, ns string) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
				Labels: map[string]string{
					LabelAgentType: LabelValueAgent,
					LabelManagedBy: LabelManagedByValue,
				},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To(int32(1)),
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": name},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app":          name,
							LabelAgentType: LabelValueAgent,
						},
					},
					Spec: corev1.PodSpec{
						ServiceAccountName: "test-sa",
						Containers: []corev1.Container{
							{Name: "agent", Image: "test-image:latest"},
						},
					},
				},
			},
		}
	}

	newReconciler := func() *MLflowReconciler {
		return &MLflowReconciler{
			Client: k8sClient,
			Scheme: scheme.Scheme,
		}
	}

	reconcileAndExpectNoOp := func(r *MLflowReconciler, name, ns string) {
		result, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))
	}

	// newTempTokenPath creates a temporary SA token file and returns its path
	// along with a cleanup function.
	newTempTokenPath := func() (string, func()) {
		tmpDir, err := os.MkdirTemp("", "mlflow-test-*")
		Expect(err).NotTo(HaveOccurred())
		tokenPath := filepath.Join(tmpDir, "token")
		err = os.WriteFile(tokenPath, []byte("test-token"), 0600)
		Expect(err).NotTo(HaveOccurred())
		return tokenPath, func() { _ = os.RemoveAll(tmpDir) }
	}

	// newMLflowTestServer creates an httptest server that responds to MLflow API calls.
	// It also creates a temporary SA token file and returns a cleanup function.
	newMLflowTestServer := func(experimentID string) (*httptest.Server, string, func()) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/2.0/mlflow/experiments/create":
				w.Header().Set("Content-Type", "application/json")
				resp := map[string]string{"experiment_id": experimentID}
				_ = json.NewEncoder(w).Encode(resp)
			case "/api/2.0/mlflow/experiments/get-by-name":
				w.Header().Set("Content-Type", "application/json")
				resp := map[string]interface{}{
					"experiment": map[string]interface{}{
						"experiment_id":   experimentID,
						"lifecycle_stage": "active",
					},
				}
				_ = json.NewEncoder(w).Encode(resp)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))

		tokenPath, cleanupToken := newTempTokenPath()
		cleanup := func() {
			server.Close()
			cleanupToken()
		}
		return server, tokenPath, cleanup
	}

	// newMLflowFailServer creates an httptest server that always returns 500.
	newMLflowFailServer := func() (*httptest.Server, string, func()) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}))

		tokenPath, cleanupToken := newTempTokenPath()
		cleanup := func() {
			server.Close()
			cleanupToken()
		}
		return server, tokenPath, cleanup
	}

	// newReconcilerWithServer creates a reconciler with an injectable MLflow client
	// and tracking URI. This avoids depending on CRD auto-discovery and the default
	// SA token path (/var/run/secrets/...) during tests.
	newReconcilerWithServer := func(serverURL, tokenPath string) *MLflowReconciler {
		return &MLflowReconciler{
			Client: k8sClient,
			Scheme: scheme.Scheme,
			ResolveTrackingURI: func(_ context.Context) string {
				return serverURL
			},
			NewMLflowClient: func(baseURL string) *mlflow.Client {
				return &mlflow.Client{BaseURL: baseURL, TokenPath: tokenPath, MaxRetries: -1}
			},
		}
	}

	Context("When the Deployment does not exist", func() {
		It("should return without error", func() {
			reconcileAndExpectNoOp(newReconciler(), "nonexistent-mlflow-dep", namespace)
		})
	})

	Context("When the Deployment is not an agent", func() {
		It("should skip non-agent deployments", func() {
			dep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "mlflow-non-agent",
					Namespace: namespace,
					Labels:    map[string]string{"app": "something-else"},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To(int32(1)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "mlflow-non-agent"},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "mlflow-non-agent"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "app", Image: "test:latest"},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, dep) }()

			reconcileAndExpectNoOp(newReconciler(), "mlflow-non-agent", namespace)
		})
	})

	Context("When MLflow is not available", func() {
		It("should skip when no tracking URI is configured", func() {
			dep := newAgentDeployment("mlflow-no-uri", namespace)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, dep) }()

			reconcileAndExpectNoOp(newReconciler(), "mlflow-no-uri", namespace)
		})
	})

	Context("When MLflow API is reachable", func() {
		var (
			server    *httptest.Server
			tokenPath string
			cleanup   func()
		)

		BeforeEach(func() {
			server, tokenPath, cleanup = newMLflowTestServer("exp-123")
		})

		AfterEach(func() {
			cleanup()
		})

		It("should create scoped Role, RoleBinding, and inject env vars", func() {
			dep := newAgentDeployment("mlflow-full", namespace)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, dep) }()

			r := newReconcilerWithServer(server.URL, tokenPath)
			reconcileAndExpectNoOp(r, "mlflow-full", namespace)

			role := &rbacv1.Role{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "rossoctl-mlflow-mlflow-full", Namespace: namespace,
			}, role)).To(Succeed())
			Expect(role.Rules).To(HaveLen(1))
			Expect(role.Rules[0].APIGroups).To(Equal([]string{MLflowExperimentsAPIGroup}))
			Expect(role.Rules[0].Resources).To(Equal([]string{MLflowExperimentsResource}))
			Expect(role.Rules[0].ResourceNames).To(Equal([]string{"mlflow-full"}))
			Expect(role.Rules[0].Verbs).To(Equal([]string{"get", "update"}))

			rb := &rbacv1.RoleBinding{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "rossoctl-mlflow-mlflow-full", Namespace: namespace,
			}, rb)).To(Succeed())
			Expect(rb.Subjects).To(HaveLen(1))
			Expect(rb.Subjects[0].Name).To(Equal("test-sa"))
			Expect(rb.RoleRef.Kind).To(Equal("Role"))
			Expect(rb.RoleRef.Name).To(Equal("rossoctl-mlflow-mlflow-full"))

			updated := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mlflow-full", Namespace: namespace}, updated)).To(Succeed())
			envMap := make(map[string]string)
			for _, e := range updated.Spec.Template.Spec.Containers[0].Env {
				envMap[e.Name] = e.Value
			}
			Expect(envMap["MLFLOW_TRACKING_URI"]).To(Equal(server.URL))
			Expect(envMap["MLFLOW_TRACKING_AUTH"]).To(Equal("kubernetes-namespaced"))
			Expect(envMap["MLFLOW_EXPERIMENT_ID"]).To(Equal("exp-123"))
			Expect(envMap["MLFLOW_EXPERIMENT_NAME"]).To(Equal("mlflow-full"))

			Expect(updated.Spec.Template.Annotations[AnnotationMLflowExperimentID]).To(Equal("exp-123"))
			Expect(updated.Spec.Template.Annotations[AnnotationMLflowExperimentName]).To(Equal("mlflow-full"))
			Expect(updated.Spec.Template.Annotations[AnnotationMLflowTrackingURI]).To(Equal(server.URL))
			Expect(updated.Spec.Template.Annotations[AnnotationMLflowTrackingAuth]).To(Equal("kubernetes-namespaced"))
		})

		It("should use default SA name when none is specified", func() {
			dep := newAgentDeployment("mlflow-default-sa", namespace)
			dep.Spec.Template.Spec.ServiceAccountName = ""
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, dep) }()

			r := newReconcilerWithServer(server.URL, tokenPath)
			reconcileAndExpectNoOp(r, "mlflow-default-sa", namespace)

			role := &rbacv1.Role{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "rossoctl-mlflow-mlflow-default-sa", Namespace: namespace,
			}, role)).To(Succeed())
			Expect(role.Rules[0].ResourceNames).To(Equal([]string{"mlflow-default-sa"}))

			rb := &rbacv1.RoleBinding{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "rossoctl-mlflow-mlflow-default-sa", Namespace: namespace,
			}, rb)).To(Succeed())
			Expect(rb.Subjects[0].Name).To(Equal("default"))
			Expect(rb.RoleRef.Kind).To(Equal("Role"))
		})
	})

	Context("When MLflow API returns an error", func() {
		It("should requeue after 30s", func() {
			server, tokenPath, cleanup := newMLflowFailServer()
			defer cleanup()

			dep := newAgentDeployment("mlflow-fail", namespace)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, dep) }()

			r := newReconcilerWithServer(server.URL, tokenPath)
			result, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "mlflow-fail", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
		})
	})

	Context("When the Deployment is already configured", func() {
		It("should be idempotent and not call the MLflow API", func() {
			var apiCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				apiCalls.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			dep := newAgentDeployment("mlflow-idempotent", namespace)
			dep.Spec.Template.Annotations = map[string]string{
				AnnotationMLflowExperimentID:   "exp-100",
				AnnotationMLflowExperimentName: "mlflow-idempotent",
				AnnotationMLflowTrackingURI:    server.URL,
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, dep) }()

			r := &MLflowReconciler{
				Client:             k8sClient,
				Scheme:             scheme.Scheme,
				ResolveTrackingURI: func(_ context.Context) string { return server.URL },
			}
			reconcileAndExpectNoOp(r, "mlflow-idempotent", namespace)
			Expect(apiCalls.Load()).To(Equal(int32(0)))
		})
	})

})

var _ = Describe("MLflow Controller helpers", func() {
	Describe("setEnvVar", func() {
		It("should add a new env var", func() {
			container := &corev1.Container{Name: "test"}
			changed := setEnvVar(container, "FOO", "bar")
			Expect(changed).To(BeTrue())
			Expect(container.Env).To(HaveLen(1))
			Expect(container.Env[0].Name).To(Equal("FOO"))
			Expect(container.Env[0].Value).To(Equal("bar"))
		})

		It("should update an existing env var", func() {
			container := &corev1.Container{
				Name: "test",
				Env: []corev1.EnvVar{
					{Name: "FOO", Value: "old"},
				},
			}
			changed := setEnvVar(container, "FOO", "new")
			Expect(changed).To(BeTrue())
			Expect(container.Env).To(HaveLen(1))
			Expect(container.Env[0].Value).To(Equal("new"))
		})

		It("should clear ValueFrom when updating", func() {
			container := &corev1.Container{
				Name: "test",
				Env: []corev1.EnvVar{
					{
						Name: "FOO",
						ValueFrom: &corev1.EnvVarSource{
							ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
								Key: "key",
							},
						},
					},
				},
			}
			changed := setEnvVar(container, "FOO", "direct-value")
			Expect(changed).To(BeTrue())
			Expect(container.Env[0].Value).To(Equal("direct-value"))
			Expect(container.Env[0].ValueFrom).To(BeNil())
		})
	})

})
