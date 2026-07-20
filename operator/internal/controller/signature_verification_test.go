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

package controller

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	"github.com/rossoctl/operator/internal/agentcard"
	"github.com/rossoctl/operator/internal/signature"
)

var _ = Describe("Signature Verification", func() {
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	Context("Signed AgentCard — Valid JWS Signature", func() {
		const (
			deploymentName = "sig-valid-agent"
			agentCardName  = "sig-valid-card"
			namespace      = "default"
		)

		var (
			rsaPrivKey *rsa.PrivateKey
			pubKeyPEM  []byte
		)

		ctx := context.Background()

		BeforeEach(func() {
			By("generating an RSA key pair")
			rsaPrivKey, pubKeyPEM = generateTestRSAKeyPair()
		})

		AfterEach(func() {
			By("cleaning up test resources")
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should set validSignature=true and SignatureVerified condition for a correctly signed card", func() {
			By("creating Deployment and Service")
			createDeploymentWithService(ctx, deploymentName, namespace)

			By("creating a signed agent card (JWS format)")
			cardData := &agentv1alpha1.AgentCardData{
				Name:    "Valid Signed Agent",
				Version: "1.0.0",
				URL:     "http://localhost:8000",
			}
			jwsSig := buildTestJWS(cardData, rsaPrivKey, "my-signing-key", "")
			cardData.Signatures = []agentv1alpha1.AgentCardSignature{jwsSig}

			By("creating an AgentCard CR with targetRef")
			agentCard := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
				Spec: agentv1alpha1.AgentCardSpec{
					SyncPeriod: "30s",
					TargetRef: &agentv1alpha1.TargetRef{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       deploymentName,
					},
				},
			}
			Expect(k8sClient.Create(ctx, agentCard)).To(Succeed())

			By("configuring a reconciler with signature verification enabled")
			provider := &mockSignatureProvider{pubKeyPEM: pubKeyPEM}

			reconciler := &AgentCardReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				AgentFetcher:       &mockFetcher{cardData: cardData},
				RequireSignature:   true,
				SignatureProvider:  provider,
				SignatureAuditMode: false,
			}

			// First reconcile adds finalizer
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: agentCardName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile performs verification
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: agentCardName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying validSignature=true")
			Eventually(func() bool {
				card := &agentv1alpha1.AgentCard{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: agentCardName, Namespace: namespace}, card); err != nil {
					return false
				}
				return card.Status.ValidSignature != nil && *card.Status.ValidSignature
			}, timeout, interval).Should(BeTrue())

			By("verifying SignatureVerified condition is True")
			card := &agentv1alpha1.AgentCard{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: agentCardName, Namespace: namespace}, card)).To(Succeed())
			sigCond := findCondition(card.Status.Conditions, "SignatureVerified")
			Expect(sigCond).NotTo(BeNil())
			Expect(sigCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(sigCond.Reason).To(Equal(ReasonSignatureValid))

			By("verifying signatureKeyId is set")
			Expect(card.Status.SignatureKeyID).To(Equal("my-signing-key"))

			By("verifying Synced condition is True")
			syncedCond := findCondition(card.Status.Conditions, "Synced")
			Expect(syncedCond).NotTo(BeNil())
			Expect(syncedCond.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	Context("Unsigned AgentCard — Rejected", func() {
		const (
			deploymentName = "sig-unsigned-agent"
			agentCardName  = "sig-unsigned-card"
			namespace      = "default"
		)

		ctx := context.Background()

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should set validSignature=false for an unsigned card", func() {
			By("creating Deployment and Service")
			createDeploymentWithService(ctx, deploymentName, namespace)

			agentCard := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
				Spec: agentv1alpha1.AgentCardSpec{
					SyncPeriod: "30s",
					TargetRef: &agentv1alpha1.TargetRef{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       deploymentName,
					},
				},
			}
			Expect(k8sClient.Create(ctx, agentCard)).To(Succeed())

			By("setting up reconciler with unsigned card data")
			cardData := &agentv1alpha1.AgentCardData{
				Name:    "Unsigned Agent",
				Version: "1.0.0",
				URL:     "http://localhost:8000",
				// No Signatures field
			}

			provider := &mockSignatureProvider{}

			reconciler := &AgentCardReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				AgentFetcher:       &mockFetcher{cardData: cardData},
				RequireSignature:   true,
				SignatureProvider:  provider,
				SignatureAuditMode: false,
			}

			// Reconcile twice (finalizer + verify)
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: agentCardName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: agentCardName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying validSignature=false")
			Eventually(func() bool {
				card := &agentv1alpha1.AgentCard{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: agentCardName, Namespace: namespace}, card); err != nil {
					return false
				}
				return card.Status.ValidSignature != nil && !*card.Status.ValidSignature
			}, timeout, interval).Should(BeTrue())

			By("verifying SignatureVerified condition is False")
			card := &agentv1alpha1.AgentCard{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: agentCardName, Namespace: namespace}, card)).To(Succeed())
			sigCond := findCondition(card.Status.Conditions, "SignatureVerified")
			Expect(sigCond).NotTo(BeNil())
			Expect(sigCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(sigCond.Reason).To(Equal(ReasonSignatureInvalid))
		})
	})

	Context("Wrong-Key JWS Signature — Rejected", func() {
		const (
			deploymentName = "sig-wrongkey-agent"
			agentCardName  = "sig-wrongkey-card"
			namespace      = "default"
		)

		ctx := context.Background()

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should set validSignature=false when card is signed with wrong key", func() {
			By("generating two different key pairs")
			signingKey, _ := generateTestRSAKeyPair()
			_, wrongPubPEM := generateTestRSAKeyPair()

			By("creating Deployment and Service")
			createDeploymentWithService(ctx, deploymentName, namespace)

			cardData := &agentv1alpha1.AgentCardData{
				Name:    "Wrong Key Agent",
				Version: "1.0.0",
				URL:     "http://localhost:8000",
			}
			jwsSig := buildTestJWS(cardData, signingKey, "key-1", "")
			cardData.Signatures = []agentv1alpha1.AgentCardSignature{jwsSig}

			agentCard := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
				Spec: agentv1alpha1.AgentCardSpec{
					SyncPeriod: "30s",
					TargetRef: &agentv1alpha1.TargetRef{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       deploymentName,
					},
				},
			}
			Expect(k8sClient.Create(ctx, agentCard)).To(Succeed())

			By("reconciling with signature verification (wrong key)")
			provider := &mockSignatureProvider{pubKeyPEM: wrongPubPEM}

			reconciler := &AgentCardReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				AgentFetcher:       &mockFetcher{cardData: cardData},
				RequireSignature:   true,
				SignatureProvider:  provider,
				SignatureAuditMode: false,
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: agentCardName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: agentCardName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying validSignature=false")
			card := &agentv1alpha1.AgentCard{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: agentCardName, Namespace: namespace}, card)).To(Succeed())
			Expect(card.Status.ValidSignature).NotTo(BeNil())
			Expect(*card.Status.ValidSignature).To(BeFalse())

			By("verifying Synced condition is False with InvalidSignature reason")
			syncedCond := findCondition(card.Status.Conditions, "Synced")
			Expect(syncedCond).NotTo(BeNil())
			Expect(syncedCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(syncedCond.Reason).To(Equal(ReasonSignatureInvalid))
		})
	})

	Context("Audit Mode — Accept with Warning", func() {
		const (
			deploymentName = "sig-audit-agent"
			agentCardName  = "sig-audit-card"
			namespace      = "default"
		)

		ctx := context.Background()

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should allow unsigned card in audit mode and set Synced=True", func() {
			_, pubPEM := generateTestRSAKeyPair()

			By("creating Deployment and Service")
			createDeploymentWithService(ctx, deploymentName, namespace)

			cardData := &agentv1alpha1.AgentCardData{
				Name:    "Audit Agent",
				Version: "1.0.0",
				URL:     "http://localhost:8000",
				// No Signatures
			}

			agentCard := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
				Spec: agentv1alpha1.AgentCardSpec{
					SyncPeriod: "30s",
					TargetRef: &agentv1alpha1.TargetRef{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       deploymentName,
					},
				},
			}
			Expect(k8sClient.Create(ctx, agentCard)).To(Succeed())

			By("reconciling with audit mode enabled")
			provider := &mockSignatureProvider{pubKeyPEM: pubPEM}

			reconciler := &AgentCardReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				AgentFetcher:       &mockFetcher{cardData: cardData},
				RequireSignature:   true,
				SignatureProvider:  provider,
				SignatureAuditMode: true,
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: agentCardName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: agentCardName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying card is synced (audit mode allows it)")
			Eventually(func() bool {
				card := &agentv1alpha1.AgentCard{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: agentCardName, Namespace: namespace}, card); err != nil {
					return false
				}
				syncedCond := findCondition(card.Status.Conditions, "Synced")
				return syncedCond != nil && syncedCond.Status == metav1.ConditionTrue
			}, timeout, interval).Should(BeTrue())

			By("verifying SignatureVerified condition is False with audit reason")
			card := &agentv1alpha1.AgentCard{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: agentCardName, Namespace: namespace}, card)).To(Succeed())
			sigCond := findCondition(card.Status.Conditions, "SignatureVerified")
			Expect(sigCond).NotTo(BeNil())
			Expect(sigCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(sigCond.Reason).To(Equal(ReasonSignatureInvalidAudit))
		})
	})

	Context("No Signature Required — Verification Skipped", func() {
		const (
			deploymentName = "sig-none-agent"
			agentCardName  = "sig-none-card"
			namespace      = "default"
		)

		ctx := context.Background()

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should sync card without checking signature when RequireSignature=false", func() {
			By("creating Deployment and Service")
			createDeploymentWithService(ctx, deploymentName, namespace)

			cardData := &agentv1alpha1.AgentCardData{
				Name:    "No Sig Agent",
				Version: "1.0.0",
				URL:     "http://localhost:8000",
			}

			agentCard := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
				Spec: agentv1alpha1.AgentCardSpec{
					SyncPeriod: "30s",
					TargetRef: &agentv1alpha1.TargetRef{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       deploymentName,
					},
				},
			}
			Expect(k8sClient.Create(ctx, agentCard)).To(Succeed())

			By("reconciling WITHOUT signature verification")
			reconciler := &AgentCardReconciler{
				Client:           k8sClient,
				Scheme:           k8sClient.Scheme(),
				AgentFetcher:     &mockFetcher{cardData: cardData},
				RequireSignature: false,
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: agentCardName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: agentCardName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying card is synced and validSignature is nil (not evaluated)")
			card := &agentv1alpha1.AgentCard{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: agentCardName, Namespace: namespace}, card)).To(Succeed())
			Expect(card.Status.ValidSignature).To(BeNil())

			syncedCond := findCondition(card.Status.Conditions, "Synced")
			Expect(syncedCond).NotTo(BeNil())
			Expect(syncedCond.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	Context("Signature Identity Match", func() {
		const (
			deploymentName = "sig-identity-agent"
			agentCardName  = "sig-identity-card"
			namespace      = "default"
			trustDomain    = "test.local"
		)

		ctx := context.Background()

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should set signatureIdentityMatch=true when both signature and binding pass", func() {
			By("generating key pair")
			privKey, pubPEM := generateTestRSAKeyPair()

			By("creating Deployment and Service")
			createDeploymentWithService(ctx, deploymentName, namespace)

			By("creating signed card data")
			expectedSpiffeID := "spiffe://" + trustDomain + "/ns/" + namespace + "/sa/test-sa"
			cardData := &agentv1alpha1.AgentCardData{
				Name:    "Identity Agent",
				Version: "1.0.0",
				URL:     "http://localhost:8000",
			}
			jwsSig := buildTestJWS(cardData, privKey, "key-1", "")
			cardData.Signatures = []agentv1alpha1.AgentCardSignature{jwsSig}

			By("creating AgentCard with both signature verification and identity binding")
			agentCard := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
				Spec: agentv1alpha1.AgentCardSpec{
					SyncPeriod: "30s",
					TargetRef: &agentv1alpha1.TargetRef{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       deploymentName,
					},
					IdentityBinding: &agentv1alpha1.IdentityBinding{
						TrustDomain: trustDomain,
					},
				},
			}
			Expect(k8sClient.Create(ctx, agentCard)).To(Succeed())

			By("reconciling with both signature and identity binding")
			provider := &mockSignatureProvider{pubKeyPEM: pubPEM, spiffeID: expectedSpiffeID}

			reconciler := &AgentCardReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				AgentFetcher:       &mockFetcher{cardData: cardData},
				RequireSignature:   true,
				SignatureProvider:  provider,
				SignatureAuditMode: false,
			}

			// First reconcile adds the finalizer and returns early.
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: agentCardName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile performs verification, binding, and status update.
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: agentCardName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying signatureIdentityMatch=true")
			Eventually(func() bool {
				card := &agentv1alpha1.AgentCard{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: agentCardName, Namespace: namespace}, card); err != nil {
					return false
				}
				return card.Status.SignatureIdentityMatch != nil && *card.Status.SignatureIdentityMatch
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("Label Propagation — Valid Signature with targetRef Deployment", func() {
		const (
			deploymentName = "sig-label-agent"
			agentCardName  = "sig-label-card"
			namespace      = "default"
		)

		ctx := context.Background()

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should propagate signature-verified=true label to Deployment pod template on valid signature", func() {
			By("generating key pair")
			privKey, pubPEM := generateTestRSAKeyPair()

			By("creating a Deployment directly (not via Agent CRD)")
			replicas := int32(1)
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: namespace,
					Labels: map[string]string{
						"app":                       deploymentName,
						LabelAgentType:              LabelValueAgent,
						ProtocolLabelPrefix + "a2a": "",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": deploymentName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": deploymentName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "agent", Image: "test-image:latest"},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			By("marking the Deployment as available (simulating real controller)")
			Eventually(func() error {
				d := &appsv1.Deployment{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, d); err != nil {
					return err
				}
				d.Status.Conditions = []appsv1.DeploymentCondition{
					{
						Type:   appsv1.DeploymentAvailable,
						Status: corev1.ConditionTrue,
					},
				}
				d.Status.Replicas = 1
				d.Status.ReadyReplicas = 1
				return k8sClient.Status().Update(ctx, d)
			}).Should(Succeed())

			By("creating a Service for the Deployment")
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: deploymentName, Namespace: namespace},
				Spec: corev1.ServiceSpec{
					Ports:    []corev1.ServicePort{{Name: "http", Port: 8000, Protocol: corev1.ProtocolTCP}},
					Selector: map[string]string{"app": deploymentName},
				},
			}
			Expect(k8sClient.Create(ctx, service)).To(Succeed())

			By("creating signed card data (JWS format)")
			cardData := &agentv1alpha1.AgentCardData{
				Name:    "Label Test Agent",
				Version: "1.0.0",
				URL:     "http://localhost:8000",
			}
			jwsSig := buildTestJWS(cardData, privKey, "key-1", "")
			cardData.Signatures = []agentv1alpha1.AgentCardSignature{jwsSig}

			By("creating AgentCard with targetRef pointing to the Deployment")
			agentCard := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
				Spec: agentv1alpha1.AgentCardSpec{
					SyncPeriod: "30s",
					TargetRef: &agentv1alpha1.TargetRef{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       deploymentName,
					},
				},
			}
			Expect(k8sClient.Create(ctx, agentCard)).To(Succeed())

			By("reconciling with signature verification enabled")
			provider := &mockSignatureProvider{pubKeyPEM: pubPEM}

			reconciler := &AgentCardReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				AgentFetcher:       &mockFetcher{cardData: cardData},
				RequireSignature:   true,
				SignatureProvider:  provider,
				SignatureAuditMode: false,
			}

			// First reconcile adds finalizer
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: agentCardName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile performs verification + label propagation
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: agentCardName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the signature-verified label is set on the Deployment pod template")
			Eventually(func() string {
				d := &appsv1.Deployment{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, d); err != nil {
					return ""
				}
				return d.Spec.Template.Labels[LabelSignatureVerified]
			}, timeout, interval).Should(Equal("true"))

			By("verifying the per-card annotation is set on the Deployment")
			d := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, d)).To(Succeed())
			Expect(d.Annotations[AnnotationVerifiedStatePrefix+agentCardName]).To(Equal("true"))
		})
	})

	Context("Label Propagation — Repeated reconciles are idempotent (no loop)", func() {
		const (
			deploymentName = "sig-label-idem-agent"
			agentCardName  = "sig-label-idem-card"
			namespace      = "default"
		)

		ctx := context.Background()

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should not update the Deployment on a second reconcile when label is already correct", func() {
			By("generating key pair")
			privKey, pubPEM := generateTestRSAKeyPair()

			By("creating Deployment and Service")
			createDeploymentWithService(ctx, deploymentName, namespace)

			By("creating signed card data")
			makeCardData := func() *agentv1alpha1.AgentCardData {
				cd := &agentv1alpha1.AgentCardData{
					Name:    "Idempotent Label Agent",
					Version: "1.0.0",
					URL:     "http://localhost:8000",
				}
				jwsSig := buildTestJWS(cd, privKey, "key-1", "")
				cd.Signatures = []agentv1alpha1.AgentCardSignature{jwsSig}
				return cd
			}

			By("creating AgentCard with targetRef")
			agentCard := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
				Spec: agentv1alpha1.AgentCardSpec{
					SyncPeriod: "30s",
					TargetRef: &agentv1alpha1.TargetRef{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       deploymentName,
					},
				},
			}
			Expect(k8sClient.Create(ctx, agentCard)).To(Succeed())

			provider := &mockSignatureProvider{pubKeyPEM: pubPEM}
			reconciler := &AgentCardReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				AgentFetcher:       &mockFetcherFunc{fn: func() *agentv1alpha1.AgentCardData { return makeCardData() }},
				RequireSignature:   true,
				SignatureProvider:  provider,
				SignatureAuditMode: false,
			}

			By("first reconcile: adds finalizer")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: agentCardName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("second reconcile: sets label + annotation")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: agentCardName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("capturing the Deployment resourceVersion after label propagation")
			d := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, d)).To(Succeed())
			Expect(d.Spec.Template.Labels[LabelSignatureVerified]).To(Equal("true"))
			Expect(d.Annotations[AnnotationVerifiedStatePrefix+agentCardName]).To(Equal("true"))
			rvAfterFirstPropagation := d.ResourceVersion

			By("third reconcile: should be a no-op for the Deployment")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: agentCardName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the Deployment was NOT updated (resourceVersion unchanged)")
			d2 := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, d2)).To(Succeed())
			Expect(d2.ResourceVersion).To(Equal(rvAfterFirstPropagation))
		})
	})

	Context("Label Propagation — Invalid Signature removes label from Deployment", func() {
		const (
			deploymentName = "sig-label-rm-agent"
			agentCardName  = "sig-label-rm-card"
			namespace      = "default"
		)

		ctx := context.Background()

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, agentCardName, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should remove signature-verified label when signature becomes invalid", func() {
			By("generating two key pairs — signing key and wrong verification key")
			signingKey, _ := generateTestRSAKeyPair()
			_, wrongPubPEM := generateTestRSAKeyPair()

			By("creating a Deployment with the signature-verified label already set (simulating previous valid state)")
			replicas := int32(1)
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: namespace,
					Labels: map[string]string{
						"app":                       deploymentName,
						LabelAgentType:              LabelValueAgent,
						ProtocolLabelPrefix + "a2a": "",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": deploymentName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app":                  deploymentName,
								LabelSignatureVerified: "true", // pre-existing from previous valid state
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "agent", Image: "test-image:latest"},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			By("marking the Deployment as available")
			Eventually(func() error {
				d := &appsv1.Deployment{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, d); err != nil {
					return err
				}
				d.Status.Conditions = []appsv1.DeploymentCondition{
					{
						Type:   appsv1.DeploymentAvailable,
						Status: corev1.ConditionTrue,
					},
				}
				d.Status.Replicas = 1
				d.Status.ReadyReplicas = 1
				return k8sClient.Status().Update(ctx, d)
			}).Should(Succeed())

			By("verifying label is initially present")
			d := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, d)).To(Succeed())
			Expect(d.Spec.Template.Labels[LabelSignatureVerified]).To(Equal("true"))

			By("creating a Service for the Deployment")
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: deploymentName, Namespace: namespace},
				Spec: corev1.ServiceSpec{
					Ports:    []corev1.ServicePort{{Name: "http", Port: 8000, Protocol: corev1.ProtocolTCP}},
					Selector: map[string]string{"app": deploymentName},
				},
			}
			Expect(k8sClient.Create(ctx, service)).To(Succeed())

			By("creating card signed with wrong key (JWS format)")
			cardData := &agentv1alpha1.AgentCardData{
				Name:    "Label Removal Agent",
				Version: "1.0.0",
				URL:     "http://localhost:8000",
			}
			jwsSig := buildTestJWS(cardData, signingKey, "key-1", "")
			cardData.Signatures = []agentv1alpha1.AgentCardSignature{jwsSig}

			By("creating AgentCard with targetRef")
			agentCard := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Name: agentCardName, Namespace: namespace},
				Spec: agentv1alpha1.AgentCardSpec{
					SyncPeriod: "30s",
					TargetRef: &agentv1alpha1.TargetRef{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       deploymentName,
					},
				},
			}
			Expect(k8sClient.Create(ctx, agentCard)).To(Succeed())

			By("reconciling with signature verification enabled (wrong key)")
			provider := &mockSignatureProvider{pubKeyPEM: wrongPubPEM}

			reconciler := &AgentCardReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				AgentFetcher:       &mockFetcher{cardData: cardData},
				RequireSignature:   true,
				SignatureProvider:  provider,
				SignatureAuditMode: false,
			}

			// First reconcile adds finalizer
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: agentCardName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile performs verification — fails — should remove label
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: agentCardName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the signature-verified label was REMOVED from the Deployment pod template")
			Eventually(func() string {
				d := &appsv1.Deployment{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, d); err != nil {
					return "error"
				}
				return d.Spec.Template.Labels[LabelSignatureVerified]
			}, timeout, interval).Should(BeEmpty())

			By("verifying the per-card annotation records false")
			d2 := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, d2)).To(Succeed())
			Expect(d2.Annotations[AnnotationVerifiedStatePrefix+agentCardName]).To(Equal("false"))

			By("verifying validSignature=false")
			card := &agentv1alpha1.AgentCard{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: agentCardName, Namespace: namespace}, card)).To(Succeed())
			Expect(card.Status.ValidSignature).NotTo(BeNil())
			Expect(*card.Status.ValidSignature).To(BeFalse())
		})
	})

	Context("Label Propagation — Multi-card AND aggregation", func() {
		const (
			deploymentName = "sig-multi-agent"
			cardNameA      = "sig-multi-card-a"
			cardNameB      = "sig-multi-card-b"
			namespace      = "default"
		)

		ctx := context.Background()

		AfterEach(func() {
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, cardNameA, namespace)
			cleanupResource(ctx, &agentv1alpha1.AgentCard{}, cardNameB, namespace)
			cleanupResource(ctx, &appsv1.Deployment{}, deploymentName, namespace)
			cleanupResource(ctx, &corev1.Service{}, deploymentName, namespace)
		})

		It("should set label=false when one card says false even if the other says true", func() {
			By("generating key pair")
			privKey, pubPEM := generateTestRSAKeyPair()

			By("creating Deployment and Service")
			createDeploymentWithService(ctx, deploymentName, namespace)

			makeSignedCard := func() *agentv1alpha1.AgentCardData {
				cd := &agentv1alpha1.AgentCardData{
					Name: "Multi Card Agent", Version: "1.0.0", URL: "http://localhost:8000",
				}
				jwsSig := buildTestJWS(cd, privKey, "key-1", "")
				cd.Signatures = []agentv1alpha1.AgentCardSignature{jwsSig}
				return cd
			}

			makeUnsignedCard := func() *agentv1alpha1.AgentCardData {
				return &agentv1alpha1.AgentCardData{
					Name: "Multi Card Agent", Version: "1.0.0", URL: "http://localhost:8000",
				}
			}

			By("creating two AgentCards targeting the same Deployment")
			cardA := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Name: cardNameA, Namespace: namespace},
				Spec: agentv1alpha1.AgentCardSpec{
					SyncPeriod: "30s",
					TargetRef:  &agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: deploymentName},
				},
			}
			Expect(k8sClient.Create(ctx, cardA)).To(Succeed())

			cardB := &agentv1alpha1.AgentCard{
				ObjectMeta: metav1.ObjectMeta{Name: cardNameB, Namespace: namespace},
				Spec: agentv1alpha1.AgentCardSpec{
					SyncPeriod: "30s",
					TargetRef:  &agentv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: deploymentName},
				},
			}
			Expect(k8sClient.Create(ctx, cardB)).To(Succeed())

			By("reconciling card A with valid signature (verified=true)")
			providerA := &mockSignatureProvider{pubKeyPEM: pubPEM}
			reconcilerA := &AgentCardReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				AgentFetcher:       &mockFetcherFunc{fn: makeSignedCard},
				RequireSignature:   true,
				SignatureProvider:  providerA,
				SignatureAuditMode: false,
			}
			_, err := reconcilerA.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: cardNameA, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconcilerA.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: cardNameA, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying label is true after card A (only card so far)")
			d := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, d)).To(Succeed())
			Expect(d.Spec.Template.Labels[LabelSignatureVerified]).To(Equal("true"))
			Expect(d.Annotations[AnnotationVerifiedStatePrefix+cardNameA]).To(Equal("true"))

			By("reconciling card B with unsigned card (verified=false)")
			reconcilerB := &AgentCardReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				AgentFetcher:       &mockFetcherFunc{fn: makeUnsignedCard},
				RequireSignature:   true,
				SignatureProvider:  providerA,
				SignatureAuditMode: false,
			}
			_, err = reconcilerB.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: cardNameB, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconcilerB.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: cardNameB, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying label is now empty (card B says false, AND aggregation)")
			Eventually(func() string {
				d := &appsv1.Deployment{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, d); err != nil {
					return "error"
				}
				return d.Spec.Template.Labels[LabelSignatureVerified]
			}, timeout, interval).Should(BeEmpty())

			By("verifying per-card annotations are correct")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, d)).To(Succeed())
			Expect(d.Annotations[AnnotationVerifiedStatePrefix+cardNameA]).To(Equal("true"))
			Expect(d.Annotations[AnnotationVerifiedStatePrefix+cardNameB]).To(Equal("false"))

			By("reconciling card B again with valid signature (both cards now true)")
			reconcilerB.AgentFetcher = &mockFetcherFunc{fn: makeSignedCard}
			_, err = reconcilerB.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: cardNameB, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying label is now true (both cards agree)")
			Eventually(func() string {
				d := &appsv1.Deployment{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, d); err != nil {
					return ""
				}
				return d.Spec.Template.Labels[LabelSignatureVerified]
			}, timeout, interval).Should(Equal("true"))
		})
	})
})

// --- Test helpers ---

// generateTestRSAKeyPair generates an RSA key pair for testing.
func generateTestRSAKeyPair() (*rsa.PrivateKey, []byte) {
	privKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	pubDER, _ := x509.MarshalPKIXPublicKey(&privKey.PublicKey)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	return privKey, pubPEM
}

// buildTestJWS creates a JWS signature for integration testing.
func buildTestJWS(cardData *agentv1alpha1.AgentCardData, privKey *rsa.PrivateKey, kid, _ string) agentv1alpha1.AgentCardSignature {
	header := map[string]string{"alg": "RS256", "typ": "JOSE", "kid": kid}
	headerJSON, _ := json.Marshal(header)
	protectedB64 := base64.RawURLEncoding.EncodeToString(headerJSON)

	payload, _ := signature.CreateCanonicalCardJSON(cardData)

	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := []byte(protectedB64 + "." + payloadB64)

	hash := sha256.Sum256(signingInput)
	sig, _ := rsa.SignPKCS1v15(rand.Reader, privKey, crypto.SHA256, hash[:])

	return agentv1alpha1.AgentCardSignature{
		Protected: protectedB64,
		Signature: base64.RawURLEncoding.EncodeToString(sig),
	}
}

// mockFetcherFunc returns a fresh AgentCardData on each call, avoiding the
// mutation issue where the reconciler overwrites cardData.URL in place.
type mockFetcherFunc struct {
	fn func() *agentv1alpha1.AgentCardData
}

func (m *mockFetcherFunc) Fetch(
	_ context.Context, _, _, _, _ string,
) (*agentcard.FetchResult, error) {
	return &agentcard.FetchResult{CardData: m.fn()}, nil
}

// mockSignatureProvider wraps VerifyJWS with a fixed public key for tests.
// This replaces the deleted SecretProvider in test code.
type mockSignatureProvider struct {
	pubKeyPEM []byte
	spiffeID  string // returned in result when verification succeeds
}

func (m *mockSignatureProvider) Name() string       { return "mock" }
func (m *mockSignatureProvider) BundleHash() string { return "mock-hash" }

func (m *mockSignatureProvider) VerifySignature(ctx context.Context, cardData *agentv1alpha1.AgentCardData,
	signatures []agentv1alpha1.AgentCardSignature) (*signature.VerificationResult, error) {
	for i := range signatures {
		result, err := signature.VerifyJWS(cardData, &signatures[i], m.pubKeyPEM)
		if err == nil && result != nil && result.Verified {
			result.SpiffeID = m.spiffeID
			return result, nil
		}
	}
	return &signature.VerificationResult{Verified: false, Details: "no valid signature"}, nil
}

// createDeploymentWithService creates a Deployment (with Available status) and a Service for testing.
func createDeploymentWithService(ctx context.Context, name, namespace string) {
	labels := map[string]string{
		"app":                       name,
		LabelAgentType:              LabelValueAgent,
		ProtocolLabelPrefix + "a2a": "",
	}
	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "agent", Image: "test-image:latest"},
					},
				},
			},
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, deployment)).To(Succeed())

	Eventually(func() error {
		d := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, d); err != nil {
			return err
		}
		d.Status.Conditions = []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
		}
		d.Status.Replicas = 1
		d.Status.ReadyReplicas = 1
		return k8sClient.Status().Update(ctx, d)
	}).Should(Succeed())

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Ports:    []corev1.ServicePort{{Name: "http", Port: 8000, Protocol: corev1.ProtocolTCP}},
			Selector: map[string]string{"app": name},
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, service)).To(Succeed())
}
