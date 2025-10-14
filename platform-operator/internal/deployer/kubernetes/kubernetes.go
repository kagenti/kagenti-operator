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

package kubernetes

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-github/v63/github"
	platformv1alpha1 "github.com/kagenti/operator/platform/api/v1alpha1"
	"github.com/kagenti/operator/platform/internal/deployer/types"
	"golang.org/x/oauth2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

var _ types.ComponentDeployer = (*KubernetesDeployer)(nil)

type KubernetesDeployer struct {
	client.Client
	Scheme                   *runtime.Scheme
	Log                      logr.Logger
	StatusChecker            StatusChecker
	EnableClientRegistration bool
}

func NewKubernetesDeployer(client client.Client, log logr.Logger, scheme *runtime.Scheme, enableClientRegistration bool) *KubernetesDeployer {
	log.Info("NewKubernetesDeployer -------------- ")
	return &KubernetesDeployer{
		Client:                   client,
		Log:                      log,
		Scheme:                   scheme,
		StatusChecker:            *NewStatusChecker(client, log),
		EnableClientRegistration: enableClientRegistration,
	}
}
func (b *KubernetesDeployer) GetName() string {
	return "kubernetes"
}
func (d *KubernetesDeployer) Deploy(ctx context.Context, component *platformv1alpha1.Component) error {

	logger := d.Log.WithValues("deployer", component.Name, "Namespace", component.Namespace)
	logger.Info("Deploying component with Kubernetes resources ***")

	if component.Status.DeploymentStatus.Phase == "Ready" {
		logger.Info("Component", "Name", component.Name, "Namespace", component.Namespace, "Phase", "Ready")
		return nil
	}
	namespace := component.Namespace
	if component.Spec.Deployer.Namespace != "" {
		namespace = component.Spec.Deployer.Namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		if err := d.Client.Create(ctx, ns); err != nil {
			if !errors.IsAlreadyExists(err) {
				logger.Error(err, "failed to create Namespace")
				return fmt.Errorf("failed to create namespace: %w", err)
			}
		}
	}

	kubeSpec := component.Spec.Deployer.Kubernetes
	if kubeSpec == nil {
		return fmt.Errorf("missing expected Spec.Deployer.Kubernetes in the CR for component %s", component.Name)
	}
	labels := map[string]string{
		"app.kubernetes.io/name":   component.Labels["app.kubernetes.io/name"],
		"app.kubernetes.io/partOf": component.Name,
	}
	rbacConfig := GetComponentRBACConfig(namespace, component.Name, labels)
	rbacManager := NewRBACManager(d.Client, d.Scheme)
	if err := rbacManager.CreateRBACObjects(ctx, rbacConfig, component); err != nil {
		logger.Error(err, "failed to create RBAC objects")
		return fmt.Errorf("failed to create RBAC objects: %w", err)
	}

	// Determine deployment strategy based on PodTemplateSpec, ImageSpec or ManifestSpec
	if kubeSpec.PodTemplateSpec != nil {
		logger.Info("Deploying component from PodTemplateSpec")
		if err := d.createDeployment(ctx, component, namespace); err != nil {
			logger.Error(err, "Failed to create Deployment from PodTemplateSpec")
			return fmt.Errorf("failed to create deployment from PodTemplateSpec: %w", err)
		}
		if err := d.createService(ctx, component, namespace); err != nil {
			logger.Error(err, "Failed to create Service from PodTemplateSpec")
			return fmt.Errorf("failed to create service from PodTemplateSpec: %w", err)
		}
	} else if kubeSpec.ImageSpec != nil {
		logger.Info("Deploying component from ImageSpec")
		if err := d.createDeployment(ctx, component, namespace); err != nil {
			logger.Error(err, "Failed to create Deployment from ImageSpec")
			return fmt.Errorf("failed to create deployment from ImageSpec: %w", err)
		}
		if err := d.createService(ctx, component, namespace); err != nil {
			logger.Error(err, "Failed to create Service from ImageSpec")
			return fmt.Errorf("failed to create service from ImageSpec: %w", err)
		}
	} else if kubeSpec.Manifest != nil {
		if kubeSpec.Manifest.GitHub != nil {
			logger.Info("Deploying component from GitHub manifest", "repository", kubeSpec.Manifest.GitHub.Repository, "path", kubeSpec.Manifest.GitHub.Path)
			if err := d.fetchAndApplyManifestsFromGithub(ctx, component); err != nil {
				logger.Error(err, "Failed to fetch and apply manifests from GitHub")
				return fmt.Errorf("failed to fetch and apply manifests from GitHub: %w", err)
			}
		} else if kubeSpec.Manifest.URL != "" {
			logger.Info("Deploying component from URL manifest", "URL", kubeSpec.Manifest.URL)
			if err := d.fetchAndApplyManifestsFromURL(ctx, kubeSpec.Manifest.URL, component); err != nil {
				logger.Error(err, "Failed to fetch and apply manifests from URL")
				return fmt.Errorf("failed to fetch and apply manifests from URL: %w", err)

			}
		} else {
			// Neither Manifest.URL nor Manifest.Github is provided
			return fmt.Errorf("invalid KubernetesSpec for component %s: either imageSpec or manifestSpec must be provided", component.Name)
		}
	} else {
		// Neither ImageSpec nor Manifest is provided
		return fmt.Errorf("invalid KubernetesSpec for component %s: either imageSpec or manifestSpec must be provided", component.Name)
	}

	logger.Info("Successfully deployed component", "component", component.Name)

	return nil
}

// Update existing component
func (d *KubernetesDeployer) Update(ctx context.Context, component *platformv1alpha1.Component) error {

	return nil
}

// Delete existing component
func (d *KubernetesDeployer) Delete(ctx context.Context, component *platformv1alpha1.Component) error {

	logger := d.Log.WithValues("component", component.Name, "Namespace", component.Namespace)
	logger.Info("Deleting component's Kubernetes resources")

	namespace := component.Namespace
	if component.Spec.Deployer.Namespace != "" {
		namespace = component.Spec.Deployer.Namespace
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      component.Name,
			Namespace: namespace,
		},
	}
	if err := d.Client.Delete(ctx, service); err != nil {
		if !errors.IsNotFound(err) {
			logger.Error(err, "Failed to delete service")
			return err
		}
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      component.Name,
			Namespace: namespace,
		},
	}
	if err := d.Client.Delete(ctx, deployment); err != nil {
		if !errors.IsNotFound(err) {
			logger.Error(err, "Failed to delete deployment")
			return err
		}
	}
	logger.Info("Component's Kubernetes resources deleted successfully")

	return nil
}

func (d *KubernetesDeployer) GetStatus(ctx context.Context, component *platformv1alpha1.Component) (platformv1alpha1.ComponentDeploymentStatus, error) {
	return *component.Status.DeploymentStatus, nil
}

func (d *KubernetesDeployer) createDeployment(ctx context.Context, component *platformv1alpha1.Component, namespace string) error {

	kubeSpec := component.Spec.Deployer.Kubernetes

	if kubeSpec == nil {
		return fmt.Errorf("failed to create deployment - missing expected Spec.Deployer.Kubernetes in the CR")
	}
	containerPorts := []corev1.ContainerPort{}

	if component.Spec.Deployer.Kubernetes.ContainerPorts != nil {
		for _, port := range component.Spec.Deployer.Kubernetes.ContainerPorts {
			containerPorts = append(containerPorts, corev1.ContainerPort{
				Name:          port.Name,
				ContainerPort: port.ContainerPort,
				Protocol:      port.Protocol,
			})
		}
	} else {
		containerPorts = append(containerPorts, corev1.ContainerPort{
			Name:          "http",
			ContainerPort: 8080,
			Protocol:      corev1.ProtocolSCTP,
		})

	}
	labels := map[string]string{
		"app.kubernetes.io/name":      component.Name,
		"app.kubernetes.io/component": getComponentType(component),
	}
	for k, v := range component.Labels {
		if _, exists := labels[k]; !exists {
			labels[k] = v
		}
	}
	imagePullPolicy := "IfNotPresent"
	if kubeSpec.ImageSpec != nil {
		imagePullPolicy = kubeSpec.ImageSpec.ImagePullPolicy
	}

	image := ""
	if kubeSpec.ImageSpec != nil {
		image = fmt.Sprintf("%s/%s:%s",
			kubeSpec.ImageSpec.ImageRegistry,
			kubeSpec.ImageSpec.Image,
			kubeSpec.ImageSpec.ImageTag,
		)
	}
	gracePeriodSeconds := int64(300)

	clientId := namespace + "/" + component.Name
	mainEnvs := component.Spec.Deployer.Env
	mainEnvs = append(mainEnvs, []corev1.EnvVar{
		{
			Name:  "CLIENT_NAME",
			Value: clientId,
		},
		{
			Name:  "CLIENT_ID",
			Value: "spiffe://localtest.me/sa/" + component.Name,
		},
		{
			Name:  "NAMESPACE",
			Value: namespace,
		},
	}...)

	sharedVolume := corev1.Volume{
		Name: "shared-data",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}
	sharedMount := corev1.VolumeMount{
		Name:      "shared-data",
		MountPath: "/shared",
	}

	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: labels,
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: component.Name,
			Containers: []corev1.Container{
				{
					Name:            component.Name,
					Image:           image,
					ImagePullPolicy: corev1.PullPolicy(imagePullPolicy),
					Resources:       component.Spec.Deployer.Kubernetes.Resources,
					Env:             mainEnvs,
					Ports:           containerPorts,
					VolumeMounts: append(
						component.Spec.Deployer.Kubernetes.VolumeMounts,
						sharedMount,
					),
				},
				{
					Name:  "spiffe-helper",
					Image: "ghcr.io/spiffe/spiffe-helper:nightly",
					Command: []string{
						"/spiffe-helper",
						"-config=/etc/spiffe-helper/helper.conf",
						"run",
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "spiffe-helper-config",
							MountPath: "/etc/spiffe-helper",
						},
						{
							Name:      "spire-agent-socket",
							MountPath: "/spiffe-workload-api",
						},
						{
							Name:      "svid-output",
							MountPath: "/opt",
						},
					},
				},
				{
					Name:            "kagenti-client-registration",
					Image:           "ghcr.io/kagenti/kagenti/client-registration:latest",
					ImagePullPolicy: corev1.PullPolicy(imagePullPolicy),
					// Wait until /opt/jwt_svid.token appears, then exec
					Command: []string{
						"/bin/sh",
						"-c",
						// TODO: tail -f /dev/null allows the container to stay alive. Change this to be a job.
						"while [ ! -f /opt/jwt_svid.token ]; do echo waiting for SVID; sleep 1; done; python client_registration.py; tail -f /dev/null",
					},
					Env: []corev1.EnvVar{
						{
							Name: "KEYCLOAK_URL",
							ValueFrom: &corev1.EnvVarSource{
								ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "environments",
									},
									Key:      "KEYCLOAK_URL",
									Optional: ptr.To(true),
								},
							},
						},
						{
							Name: "KEYCLOAK_REALM",
							ValueFrom: &corev1.EnvVarSource{
								ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "environments",
									},
									Key: "KEYCLOAK_REALM",
								},
							},
						},
						{
							Name: "KEYCLOAK_ADMIN_USERNAME",
							ValueFrom: &corev1.EnvVarSource{
								ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "environments",
									},
									Key: "KEYCLOAK_ADMIN_USERNAME",
								},
							},
						},
						{
							Name: "KEYCLOAK_ADMIN_PASSWORD",
							ValueFrom: &corev1.EnvVarSource{
								ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "environments",
									},
									Key: "KEYCLOAK_ADMIN_PASSWORD",
								},
							},
						},
						{
							Name:  "CLIENT_NAME",
							Value: component.Name,
						},
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							// This is how client registration accesses the SVID
							Name:      "svid-output",
							MountPath: "/opt",
						},
						sharedMount,
					},
				},
			},
			TerminationGracePeriodSeconds: &gracePeriodSeconds,
			ImagePullSecrets: []corev1.LocalObjectReference{
				{
					Name: "ghcr-secret",
				},
			},
			Volumes: append(
				component.Spec.Deployer.Kubernetes.Volumes,
				[]corev1.Volume{
					sharedVolume,
					{
						Name: "spiffe-helper-config",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: "spiffe-helper-config",
								},
							},
						},
					},
					{
						Name: "spire-agent-socket",
						VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: "/run/spire/agent-sockets",
							},
						},
					},
					{
						Name: "svid-output",
						VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						},
					},
				}...,
			),
		},
	}

	// if user provided PodTemplateSpec, use it instead of the default one
	if component.Spec.Deployer.Kubernetes.PodTemplateSpec != nil {
		template = *component.Spec.Deployer.Kubernetes.PodTemplateSpec
		template.ObjectMeta = metav1.ObjectMeta{
			Labels: labels,
		}

		// Merge mainEnvs into each container's Env
		for containerInx := range template.Spec.Containers {
			if len(mainEnvs) > 0 {
				template.Spec.Containers[containerInx].Env = append(mainEnvs, template.Spec.Containers[containerInx].Env...)
			} else {
				template.Spec.Containers[containerInx].Env = mainEnvs
			}

			// ensure sharedMount is applied even if user overrides PodTemplateSpec
			template.Spec.Containers[containerInx].VolumeMounts = append(
				template.Spec.Containers[containerInx].VolumeMounts,
				sharedMount,
			)
		}

		// ensure sharedVolume is added
		template.Spec.Volumes = append(template.Spec.Volumes, sharedVolume)
	}

	deployment := &appsv1.Deployment{

		ObjectMeta: metav1.ObjectMeta{
			Name:      component.Name,
			Namespace: namespace,
			Labels:    labels,
		},

		Spec: appsv1.DeploymentSpec{
			Replicas: getReplicaCount(component),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name": component.Name,
				},
			},
			Template: template,
		},
	}

	if component.Annotations != nil {
		deployment.ObjectMeta.Annotations = component.Annotations
	}
	if component.Namespace == deployment.Namespace {
		if err := controllerutil.SetControllerReference(component, deployment, d.Client.Scheme()); err != nil {
			return err
		}
	}

	existingDeployment := &appsv1.Deployment{}
	err := d.Client.Get(ctx, k8stypes.NamespacedName{Name: deployment.Name, Namespace: namespace}, existingDeployment)
	if err != nil {
		if errors.IsNotFound(err) {
			return d.Client.Create(ctx, deployment)
		}
		return err
	}
	return nil

}

func (d *KubernetesDeployer) createService(ctx context.Context, component *platformv1alpha1.Component, namespace string) error {

	kubeSpec := component.Spec.Deployer.Kubernetes

	if kubeSpec == nil {
		return fmt.Errorf("failed to create service - missing expected Spec.Deployer.Kubernetes in the CR")
	}
	labels := map[string]string{
		"app.kubernetes.io/name":      component.Name,
		"app.kubernetes.io/component": getComponentType(component),
	}
	for k, v := range component.Labels {
		if _, exists := labels[k]; !exists {
			labels[k] = v
		}
	}
	servicePorts := []corev1.ServicePort{}

	if component.Spec.Deployer.Kubernetes.ServicePorts != nil {
		for _, port := range component.Spec.Deployer.Kubernetes.ServicePorts {
			servicePorts = append(servicePorts, corev1.ServicePort{
				Name:       port.Name,
				Port:       port.Port,
				TargetPort: port.TargetPort,
				Protocol:   corev1.ProtocolTCP,
			})
		}
	} else {
		servicePorts = append(servicePorts, corev1.ServicePort{
			Name:       "http",
			Port:       8000,
			TargetPort: intstr.FromInt(8080),
			Protocol:   corev1.ProtocolTCP,
		})

	}
	service := &corev1.Service{

		ObjectMeta: metav1.ObjectMeta{
			Name:      component.Name,
			Namespace: component.Namespace,
			Labels:    labels,
		},

		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app.kubernetes.io/name": component.Name,
			},
			Ports: servicePorts,
			Type:  corev1.ServiceType(kubeSpec.ServiceType),
		},
	}
	if component.Annotations != nil {
		service.ObjectMeta.Annotations = component.Annotations
	}

	if err := controllerutil.SetControllerReference(component, service, d.Client.Scheme()); err != nil {
		return err
	}

	existigService := &corev1.Service{}
	err := d.Client.Get(ctx, k8stypes.NamespacedName{Name: service.Name, Namespace: service.Namespace}, existigService)
	if err != nil {
		if errors.IsNotFound(err) {
			d.Log.Info("createService()-", "component", component.Name, "Namespace", component.Namespace)
			return d.Client.Create(ctx, service)
		}
		return err
	}
	return nil
}
func getComponentType(component *platformv1alpha1.Component) string {
	if component.Spec.Agent != nil {
		return "agent"
	}
	if component.Spec.Tool != nil {
		return "tool"
	}
	if component.Spec.Infra != nil {
		return "infra"
	}
	return "unknown"
}
func getReplicaCount(component *platformv1alpha1.Component) *int32 {
	var count int32 = 1

	if component.Annotations != nil {
		if replicaStr, ok := component.Annotations["platform.operator.io/replicates"]; ok {
			if replicas, err := strconv.ParseInt(replicaStr, 10, 32); err == nil {
				count = int32(replicas)
			}
		}
	}
	return &count
}
func (d *KubernetesDeployer) CheckComponentStatus(ctx context.Context, component *platformv1alpha1.Component) (bool, string, error) {

	logger := d.Log.WithValues("Kubernetes Deployer", component.Name, "namespace", component.Namespace)
	logger.Info("CheckComponentStatus")

	if component.Spec.Deployer.Kubernetes.Manifest != nil {
		// Hardcoded status for now Components deployed from a manifest. Need more work here
		// to process status for each object deployed from a manifest. For now just return ready
		return true, "Component is ready", nil
	}
	namespace := component.Namespace
	if component.Spec.Deployer.Namespace != "" {
		namespace = component.Spec.Deployer.Namespace
	}
	deployment := &appsv1.Deployment{}
	err := d.Client.Get(ctx, k8stypes.NamespacedName{Name: component.Name, Namespace: namespace}, deployment)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, "Deployment not found", nil
		}
		d.Log.Error(err, "Failed to get deployment object")
		return false, fmt.Sprintf("Error checking deployment: %v", err), err
	}
	logger.Info("CheckComponentStatus", "status", deployment.Status)
	if deployment.Status.ReadyReplicas < 1 {
		message := fmt.Sprintf("Deployment not ready: %d/%d replicas available",
			deployment.Status.ReadyReplicas, *deployment.Spec.Replicas)
		for _, condition := range deployment.Status.Conditions {
			message = fmt.Sprintf("%s, Reason: %s, Message: %s", message, condition.Reason, condition.Message)
			break
		}
		return false, message, nil
	}
	kubeSpec := component.Spec.Deployer.Kubernetes
	if kubeSpec.ServiceType != "" {
		service := &corev1.Service{}
		err := d.Client.Get(ctx, k8stypes.NamespacedName{Name: component.Name, Namespace: namespace}, service)
		if err != nil {
			if errors.IsNotFound(err) {
				d.Log.Error(err, "CheckComponentStatus() -  Service Not Found")
				return false, "Service not found", nil
			}
			d.Log.Error(err, "Failed to get service")
			return false, fmt.Sprintf("Error checking service: %v", err), err
		}
	}
	logger.Info("CheckComponentStatus", "Comoponent is ready", component.Name, "namespace", component.Namespace)
	return true, "Component is ready", nil
}

func (d *KubernetesDeployer) fetchAndApplyManifestsFromURL(ctx context.Context, manifestURL string, component *platformv1alpha1.Component) error {
	content, err := d.getManifestFromURL(ctx, manifestURL)
	if err != nil {
		d.Log.Error(err, "Error while fetching manifest from a URL")
		return fmt.Errorf("Error: Unable to fetch manifest %s: %w", manifestURL, err)
	}
	// Parse the YAML content into multiple Kubernetes objects
	objects, err := d.parseKubernetesManifest(string(content))
	if err != nil {
		return fmt.Errorf("failed to parse Kubernetes manifest from %s: %w", manifestURL, err)

	}
	// Apply each object
	for _, obj := range objects {
		rs, err := d.StatusChecker.CheckResourceStatus(ctx, obj)
		if err != nil {
			d.Log.Error(err, "StatusChecked failed to check object status")
		}
		d.Log.Info("StatusChecker", "Object", rs.Name, "Namespace", rs.Namespace, "phase", rs.Phase, "ready", rs.Ready)
		if rs.Phase == "NotFound" {
			mergedAnnotations := make(map[string]string)

			// merge annotations
			if obj.GetAnnotations() == nil {
				for k, v := range component.Annotations {
					mergedAnnotations[k] = v
				}
				obj.SetAnnotations(mergedAnnotations)
			} else {
				for k, v := range component.Annotations {
					obj.GetAnnotations()[k] = v
				}

			}
			mergedLabels := make(map[string]string)
			// merge labels
			if obj.GetLabels() == nil {
				for k, v := range component.Labels {
					mergedLabels[k] = v
				}
				obj.SetLabels(mergedLabels)
			} else {
				for k, v := range component.Labels {
					obj.GetLabels()[k] = v
				}
			}
			d.Log.Info("Applying Ownership to manifest object", "kind", obj.GetKind(), "name", obj.GetName(), "namespace", obj.GetNamespace())
			if obj.GetKind() != "CustomResourceDefinition" {
				// (namespaced) Component cannot own a CRD
				if err := controllerutil.SetControllerReference(component, obj, d.Client.Scheme()); err != nil {
					return err
				}
			}
			d.Log.Info("Applying manifest object", "kind", obj.GetKind(), "name", obj.GetName(), "namespace", obj.GetNamespace(), "annotations", obj.GetAnnotations())

			if err := d.Client.Create(ctx, obj); err != nil {
				if errors.IsAlreadyExists(err) {
					if err := d.Client.Update(ctx, obj); err != nil {
						return fmt.Errorf("failed to update %s/%s: %w", obj.GetKind(), obj.GetName(), err)
					}
				} else {
					return fmt.Errorf("failed to create %s/%s: %w", obj.GetKind(), obj.GetName(), err)
				}
			}

		}
	}
	d.Log.Info("Successfully applied all manifests from URL", "URL", manifestURL)
	return nil
}

func (d *KubernetesDeployer) fetchAndApplyManifestsFromGithub(ctx context.Context, component *platformv1alpha1.Component) error {
	manifestSpec := component.Spec.Deployer.Kubernetes.Manifest.GitHub
	if manifestSpec == nil {
		return fmt.Errorf("manifestSpec is nil in fetchAndApplyManifestsFromGithub for component %s", component.Name)
	}

	// Create GitHub client
	ghClient, err := d.getGitHubClient(ctx, component, manifestSpec.AuthSecretRef)
	if err != nil {
		return fmt.Errorf("failed to get GitHub client: %w", err)
	}

	owner, repoName, err := parseGitHubRepoURL(manifestSpec.Repository)
	if err != nil {
		return fmt.Errorf("invalid GitHub repository URL %s: %w", manifestSpec.Repository, err)
	}
	fileContent, err := d.fetchContentWithRetry(ctx, ghClient, owner, repoName, manifestSpec, 100)
	if err != nil {
		return fmt.Errorf("failed to get content for %s/%s at path %s (revision %s): %w",
			owner, repoName, manifestSpec.Path, manifestSpec.Revision, err)

	}

	if fileContent.Content == nil {
		return fmt.Errorf("GitHub returned empty content for %s/%s at path %s", owner, repoName, manifestSpec.Path)
	}

	// Decode the base64 content
	decodedContent, err := base64.StdEncoding.DecodeString(*fileContent.Content)
	if err != nil {
		return fmt.Errorf("failed to base64 decode manifest content: %w", err)
	}

	// Parse the YAML content into multiple Kubernetes objects
	objects, err := d.parseKubernetesManifest(string(decodedContent))
	if err != nil {
		return fmt.Errorf("failed to parse Kubernetes manifest from %s: %w", manifestSpec.Path, err)
	}

	// Apply each object
	for _, obj := range objects {

		if obj.GetNamespace() == "" {
			if component.Spec.Deployer.Namespace != "" {
				obj.SetNamespace(component.Spec.Deployer.Namespace)
			}
		}

		rs, err := d.StatusChecker.CheckResourceStatus(ctx, obj)
		if err != nil {
			d.Log.Error(err, "StatusChecked failed to check object status")
		}
		d.Log.Info("StatusChecker", "Object", rs.Name, "Namespace", rs.Namespace, "phase", rs.Phase, "ready", rs.Ready)

		if rs.Phase == "NotFound" {
			mergedAnnotations := make(map[string]string)
			d.Log.Info("Merging Annotations")
			// merge annotations
			if obj.GetAnnotations() == nil {
				for k, v := range component.Annotations {
					mergedAnnotations[k] = v
				}
				obj.SetAnnotations(mergedAnnotations)
			} else {
				for k, v := range component.Annotations {
					obj.GetAnnotations()[k] = v
				}

			}
			d.Log.Info("Merging Labels")
			mergedLabels := make(map[string]string)
			// merge labels
			if obj.GetLabels() == nil {
				for k, v := range component.Labels {
					mergedLabels[k] = v
				}
				obj.SetLabels(mergedLabels)
			} else {
				for k, v := range component.Labels {
					obj.GetLabels()[k] = v
				}
			}
			if obj.GetKind() != "CustomResourceDefinition" {
				if err := controllerutil.SetControllerReference(component, obj, d.Client.Scheme()); err != nil {
					return err
				}
			}
			d.Log.Info("Applying manifest object", "kind", obj.GetKind(), "name", obj.GetName(), "namespace", obj.GetNamespace(), "annotations", obj.GetAnnotations())

			if err := d.Client.Create(ctx, obj); err != nil {
				if errors.IsAlreadyExists(err) {
					if err := d.Client.Update(ctx, obj); err != nil {
						return fmt.Errorf("failed to update %s/%s: %w", obj.GetKind(), obj.GetName(), err)
					}
				} else {
					return fmt.Errorf("failed to create %s/%s: %w", obj.GetKind(), obj.GetName(), err)
				}
			}
		}

	}

	d.Log.Info("Successfully applied all manifests from GitHub", "repository", manifestSpec.Repository, "path", manifestSpec.Path)
	return nil
}
func (d *KubernetesDeployer) fetchContentWithRetry(ctx context.Context, ghClient *github.Client, owner, repoName string, manifestSpec *platformv1alpha1.GitHubSource, maxRetries int) (*github.RepositoryContent, error) {
	for i := 0; i < maxRetries; i++ {
		// Fetch manifest content from GitHub
		fileContent, _, response, err := ghClient.Repositories.GetContents(
			ctx,
			owner,
			repoName,
			manifestSpec.Path,
			&github.RepositoryContentGetOptions{
				Ref: manifestSpec.Revision,
			},
		)
		if err != nil {
			if response.StatusCode != 503 {
				return nil, fmt.Errorf("failed to get content for %s/%s at path %s (revision %s): %w",
					owner, repoName, manifestSpec.Path, manifestSpec.Revision, err)

			}
			backoff := time.Duration(i+1) * 2 * time.Second
			time.Sleep(backoff)
		}
		return fileContent, nil
	}
	return nil, fmt.Errorf("max retries exceeded while trying to fetch content from Github repo")
}

func (d *KubernetesDeployer) getGitHubClient(ctx context.Context, component *platformv1alpha1.Component, secretRef *corev1.LocalObjectReference) (*github.Client, error) {
	if secretRef != nil {
		secretData, err := d.getSecretData(ctx, component.GetNamespace(), secretRef.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to get GitHub credentials secret %s: %w", secretRef.Name, err)
		}
		token := string(secretData["token"]) // Assuming the token key in the secret is "token"
		if token == "" {
			return nil, fmt.Errorf("secret %s does not contain a 'token' key", secretRef.Name)
		}
		ts := oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: token},
		)
		tc := oauth2.NewClient(ctx, ts)
		d.Log.Info("getGitHubClient", "Github Client Authenticated", "true")
		return github.NewClient(tc), nil
	}
	// Return unauthenticated client if no secret is provided (for public repos)
	return github.NewClient(nil), nil
}

func (d *KubernetesDeployer) getSecretData(ctx context.Context, namespace, name string) (map[string][]byte, error) {
	secret := &corev1.Secret{}
	err := d.Client.Get(ctx, k8stypes.NamespacedName{Name: name, Namespace: namespace}, secret)
	if err != nil {
		return nil, fmt.Errorf("failed to get secret %s/%s: %w", namespace, name, err)
	}
	return secret.Data, nil
}

// parse YAML manifest string into Kubernetes (unstructured) objects
func (d *KubernetesDeployer) parseKubernetesManifest(manifestContent string) ([]*unstructured.Unstructured, error) {
	var objects []*unstructured.Unstructured
	//d.Log.Info("parseKubernetesManifest", "raw manifest", manifestContent)

	cleanedContent := d.cleanRawManifest(manifestContent)
	//d.Log.Info("parseKubernetesManifest", "manifest", cleanedContent)

	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(cleanedContent), 4096)
	docIndex := 0

	for {
		var obj map[string]interface{}
		err := decoder.Decode(&obj)

		if err != nil {
			if err == io.EOF {
				break // End of documents
			}
			d.Log.Error(err, "Failed to decode YAML document", "documentIndex", docIndex)
			docIndex++
			continue // Skip invalid document
		}

		// Skip empty objects
		if len(obj) == 0 {
			docIndex++
			continue
		}

		// Convert to unstructured
		unstructuredObj := &unstructured.Unstructured{Object: obj}

		// Validate that this is a valid Kubernetes object
		if unstructuredObj.GetAPIVersion() == "" || unstructuredObj.GetKind() == "" {
			d.Log.Info("Skipping document without apiVersion or kind", "documentIndex", docIndex)
			docIndex++
			continue
		}

		objects = append(objects, unstructuredObj)
		d.Log.Info("Parsed Kubernetes object",
			"kind", unstructuredObj.GetKind(),
			"apiVersion", unstructuredObj.GetAPIVersion(),
			"name", unstructuredObj.GetName(),
			"namespace", unstructuredObj.GetNamespace())

		docIndex++
	}

	if len(objects) == 0 {
		return nil, fmt.Errorf("no valid Kubernetes objects found in manifest")
	}

	return objects, nil
}

func parseGitHubRepoURL(repoURL string) (string, string, error) {
	// Trim .git suffix if present
	if len(repoURL) > 4 && repoURL[len(repoURL)-4:] == ".git" {
		repoURL = repoURL[:len(repoURL)-4]
	}

	// Simple split for "github.com/owner/repo"
	parts := bytes.Split([]byte(repoURL), []byte("/"))
	if len(parts) < 3 {
		return "", "", fmt.Errorf("invalid GitHub repository URL format")
	}

	owner := string(parts[len(parts)-2])
	repoName := string(parts[len(parts)-1])

	if owner == "" || repoName == "" {
		return "", "", fmt.Errorf("could not extract owner and repo name from URL: %s", repoURL)
	}
	return owner, repoName, nil
}

func (d *KubernetesDeployer) getManifestFromURL(ctx context.Context, manifestURL string) ([]byte, error) {
	d.Log.Info("getManifestFromURL", "URL", manifestURL)
	req, err := http.NewRequestWithContext(ctx, "GET", manifestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request for URL %s: %w", manifestURL, err)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch manifest from URL %s: %w", manifestURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("URL %s returned non-200 status: %d %s", manifestURL, resp.StatusCode, resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body from URL %s: %w", manifestURL, err)
	}

	return body, nil
}

// This function processes given raw manifest content to extract only the valid YAML
// documents, removing headers, comments, and other non-YAML content that might cause parsing issues.
func (d *KubernetesDeployer) cleanRawManifest(content string) string {
	d.Log.Info("cleanRawManifest")
	lines := strings.Split(content, "\n")
	var cleanedLines []string
	foundYamlDocument := false

	// processes raw manifest line at a time
	for i, line := range lines {
		trimmedLine := strings.TrimSpace(line)

		// move through the raw content until the YAML document is found
		if !foundYamlDocument {
			// Look for start of YAML documents - either --- or apiVersion/kind
			if trimmedLine == "---" ||
				strings.HasPrefix(trimmedLine, "apiVersion:") ||
				strings.HasPrefix(trimmedLine, "kind:") {
				foundYamlDocument = true
				// skip the first --- separator if found
				if trimmedLine != "---" {
					cleanedLines = append(cleanedLines, line)
				}
				continue
			}
			// Skip this line since we haven't found YAML content yet
			continue
		}

		//filter standalone comments but keep inline comments (eg. key: value # comment)
		if strings.HasPrefix(trimmedLine, "#") && !strings.Contains(trimmedLine, ":") {
			continue
		}

		// Skip empty lines
		if trimmedLine == "" {
			// Keep empty lines that might be separating YAML sections
			if i > 0 && i < len(lines)-1 {
				nextNonEmpty := ""
				for j := i + 1; j < len(lines); j++ {
					if strings.TrimSpace(lines[j]) != "" {
						nextNonEmpty = strings.TrimSpace(lines[j])
						break
					}
				}
				// Keep empty line if next non-empty line looks like YAML
				if nextNonEmpty == "---" ||
					strings.HasPrefix(nextNonEmpty, "apiVersion:") ||
					strings.HasPrefix(nextNonEmpty, "kind:") {
					cleanedLines = append(cleanedLines, line)
				}
			}
			continue
		}

		cleanedLines = append(cleanedLines, line)
	}

	return strings.Join(cleanedLines, "\n")
}
