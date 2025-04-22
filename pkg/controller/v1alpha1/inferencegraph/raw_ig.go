/*
Copyright 2023 The KServe Authors.

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

package inferencegraph

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1cfg "k8s.io/client-go/applyconfigurations/core/v1"
	metav1cfg "k8s.io/client-go/applyconfigurations/meta/v1"
	rbacv1cfg "k8s.io/client-go/applyconfigurations/rbac/v1"
	"k8s.io/client-go/kubernetes"
	"knative.dev/pkg/apis"
	knapis "knative.dev/pkg/apis"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1api "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	"github.com/kserve/kserve/pkg/apis/serving/v1beta1"
	"github.com/kserve/kserve/pkg/constants"
	"github.com/kserve/kserve/pkg/controller/v1beta1/inferenceservice/reconcilers/raw"
)

var logger = logf.Log.WithName("InferenceGraphRawDeployer")

/*
This function helps to create core podspec for a given inference graph spec and router configuration
Also propagates headers onto podspec container environment variables.

This function makes sense to be used in raw k8s deployment mode
*/
func createInferenceGraphPodSpec(graph *v1alpha1api.InferenceGraph, config *RouterConfig) *v1.PodSpec {
	bytes, err := json.Marshal(graph.Spec)
	if err != nil {
		return nil
	}

	// Pod spec with 'router container with resource requirements' and 'affinity' as well
	podSpec := &v1.PodSpec{
		Containers: []v1.Container{
			{
				Name:  graph.ObjectMeta.Name,
				Image: config.Image,
				Args: []string{
					"--enable-tls",
					"--graph-json",
					string(bytes),
				},
				Resources:      constructResourceRequirements(*graph, *config),
				ReadinessProbe: constants.GetRouterReadinessProbe(),
				SecurityContext: &v1.SecurityContext{
					Privileged:               proto.Bool(false),
					RunAsNonRoot:             proto.Bool(true),
					ReadOnlyRootFilesystem:   proto.Bool(true),
					AllowPrivilegeEscalation: proto.Bool(false),
					Capabilities: &v1.Capabilities{
						Drop: []v1.Capability{v1.Capability("ALL")},
					},
				},
			},
		},
		Affinity:                     graph.Spec.Affinity,
		ServiceAccountName:           "default",
		AutomountServiceAccountToken: proto.Bool(false), // Inference graph does not need access to api server
	}

	// Only adding this env variable "PROPAGATE_HEADERS" if router's headers config has the key "propagate"
	value, exists := config.Headers["propagate"]
	if exists {
		podSpec.Containers[0].Env = []v1.EnvVar{
			{
				Name:  constants.RouterHeadersPropagateEnvVar,
				Value: strings.Join(value, ","),
			},
		}
	}

	// If auth is enabled for the InferenceGraph:
	// * Add --enable-auth argument, to properly secure kserve-router
	// * Add the --inferencegraph-name argument, so that the router is aware of its name
	// * Enable auto-mount of the ServiceAccount, because it is required for validating tokens
	// * Set a non-default ServiceAccount with enough privileges to verify auth
	if graph.GetAnnotations()[constants.ODHKserveRawAuth] == "true" {
		podSpec.Containers[0].Args = append(podSpec.Containers[0].Args, "--enable-auth")

		podSpec.Containers[0].Args = append(podSpec.Containers[0].Args, "--inferencegraph-name")
		podSpec.Containers[0].Args = append(podSpec.Containers[0].Args, graph.GetName())

		podSpec.AutomountServiceAccountToken = proto.Bool(true)

		// In ODH, when auth is enabled, it is required to have the InferenceGraph running
		// with a ServiceAccount that can query the Kubernetes API to validate tokens
		// and privileges.
		// In KServe v0.14 there is no way for users to set the ServiceAccount for an
		// InferenceGraph. In ODH this is used at our advantage to set a non-default SA
		// and bind needed privileges for the auth verification.
		podSpec.ServiceAccountName = fmt.Sprintf("%s-auth-verifier", graph.GetName())
	}

	return podSpec
}

/*
A simple utility to create a basic meta object given name and namespace;  Can be extended to accept labels, annotations as well
*/
func constructForRawDeployment(graph *v1alpha1api.InferenceGraph) (metav1.ObjectMeta, v1beta1.ComponentExtensionSpec) {
	name := graph.ObjectMeta.Name
	namespace := graph.ObjectMeta.Namespace
	annotations := graph.ObjectMeta.Annotations
	labels := graph.ObjectMeta.Labels

	if annotations == nil {
		annotations = make(map[string]string)
	}

	if labels == nil {
		labels = make(map[string]string)
	}

	labels[constants.InferenceGraphLabel] = name

	objectMeta := metav1.ObjectMeta{
		Name:        name,
		Namespace:   namespace,
		Labels:      labels,
		Annotations: annotations,
	}

	componentExtensionSpec := v1beta1.ComponentExtensionSpec{
		MaxReplicas: graph.Spec.MaxReplicas,
		MinReplicas: graph.Spec.MinReplicas,
		ScaleMetric: (*v1beta1.ScaleMetric)(graph.Spec.ScaleMetric),
		ScaleTarget: graph.Spec.ScaleTarget,
	}

	return objectMeta, componentExtensionSpec
}

/*
Handles bulk of raw deployment logic for Inference graph controller
1. Constructs PodSpec
2. Constructs Meta and Extensionspec
3. Creates a reconciler
4. Set controller references
5. Finally reconcile
*/
func handleInferenceGraphRawDeployment(cl client.Client, clientset kubernetes.Interface, scheme *runtime.Scheme,
	graph *v1alpha1api.InferenceGraph, routerConfig *RouterConfig) (*appsv1.Deployment, *knapis.URL, error) {
	// create desired service object.
	desiredSvc := createInferenceGraphPodSpec(graph, routerConfig)

	objectMeta, componentExtSpec := constructForRawDeployment(graph)

	// create the reconciler
	reconciler, err := raw.NewRawKubeReconciler(cl, clientset, scheme, constants.InferenceGraphResource, objectMeta, metav1.ObjectMeta{}, &componentExtSpec, desiredSvc, nil)

	if err != nil {
		return nil, reconciler.URL, errors.Wrapf(err, "fails to create NewRawKubeReconciler for inference graph")
	}
	// set Deployment Controller
	for _, deployments := range reconciler.Deployment.DeploymentList {
		if err := controllerutil.SetControllerReference(graph, deployments, scheme); err != nil {
			return nil, reconciler.URL, errors.Wrapf(err, "fails to set deployment owner reference for inference graph")
		}
	}
	// set Service Controller
	for _, svc := range reconciler.Service.ServiceList {
		svc.ObjectMeta.Annotations[constants.OpenshiftServingCertAnnotation] = graph.Name + constants.ServingCertSecretSuffix
		if err := controllerutil.SetControllerReference(graph, svc, scheme); err != nil {
			return nil, reconciler.URL, errors.Wrapf(err, "fails to set service owner reference for inference graph")
		}
	}

	// set autoscaler Controller
	if err := reconciler.Scaler.Autoscaler.SetControllerReferences(graph, scheme); err != nil {
		return nil, reconciler.URL, errors.Wrapf(err, "fails to set autoscaler owner references for inference graph")
	}

	// reconcile
	deployment, err := reconciler.Reconcile()
	logger.Info("Result of inference graph raw reconcile", "deployment", deployment[0]) // only 1 deployment exist (default deployment)
	logger.Info("Result of reconcile", "err", err)

	if err != nil {
		return deployment[0], reconciler.URL, errors.Wrapf(err, "fails to reconcile inference graph raw")
	}

	return deployment[0], reconciler.URL, nil
}

func handleInferenceGraphRawAuthResources(ctx context.Context, clientset kubernetes.Interface, scheme *runtime.Scheme, graph *v1alpha1api.InferenceGraph) error {
	saName := getServiceAccountNameForGraph(graph)

	if graph.GetAnnotations()[constants.ODHKserveRawAuth] == "true" {
		graphGVK, err := apiutil.GVKForObject(graph, scheme)
		if err != nil {
			return errors.Wrapf(err, "fails get GVK for inference graph")
		}
		ownerReference := metav1cfg.OwnerReference().
			WithKind(graphGVK.Kind).
			WithAPIVersion(graphGVK.GroupVersion().String()).
			WithName(graph.GetName()).
			WithUID(graph.UID).
			WithBlockOwnerDeletion(true).
			WithController(true)

		// Create a Service Account that can be used to check auth
		saAuthVerifier := corev1cfg.ServiceAccount(saName, graph.GetNamespace()).
			WithOwnerReferences(ownerReference)
		_, err = clientset.CoreV1().ServiceAccounts(graph.GetNamespace()).Apply(ctx, saAuthVerifier, metav1.ApplyOptions{FieldManager: InferenceGraphControllerName})
		if err != nil {
			return errors.Wrapf(err, "fails to apply auth-verifier service account for inference graph")
		}

		// Bind the required privileges to the Service Account
		err = addAuthPrivilegesToGraphServiceAccount(ctx, clientset, graph)
		if err != nil {
			return err
		}
	} else {
		err := removeAuthPrivilegesFromGraphServiceAccount(ctx, clientset, graph)
		if err != nil {
			return err
		}

		err = deleteGraphServiceAccount(ctx, clientset, graph)
		if err != nil {
			return err
		}
	}

	return nil
}

func addAuthPrivilegesToGraphServiceAccount(ctx context.Context, clientset kubernetes.Interface, graph *v1alpha1api.InferenceGraph) error {
	clusterRoleBinding, err := clientset.RbacV1().ClusterRoleBindings().Get(ctx, constants.InferenceGraphAuthCRBName, metav1.GetOptions{})
	if client.IgnoreNotFound(err) != nil {
		return errors.Wrapf(err, "fails to get cluster role binding kserve-inferencegraph-auth-verifiers while configuring inference graph auth")
	}

	saName := getServiceAccountNameForGraph(graph)
	if apierrors.IsNotFound(err) {
		clusterRoleAuxiliary := rbacv1.ClusterRole{}
		rbRoleRef := rbacv1cfg.RoleRef().
			WithKind("ClusterRole").
			WithName("system:auth-delegator").
			WithAPIGroup(clusterRoleAuxiliary.GroupVersionKind().Group)
		rbSubject := rbacv1cfg.Subject().
			WithKind("ServiceAccount").
			WithNamespace(graph.GetNamespace()).
			WithName(saName)
		crbApply := rbacv1cfg.ClusterRoleBinding(constants.InferenceGraphAuthCRBName).
			WithRoleRef(rbRoleRef).
			WithSubjects(rbSubject)

		_, err = clientset.RbacV1().ClusterRoleBindings().Apply(ctx, crbApply, metav1.ApplyOptions{FieldManager: InferenceGraphControllerName})
		if err != nil {
			return errors.Wrapf(err, "fails to apply kserve-inferencegraph-auth-verifiers ClusterRoleBinding for inference graph")
		}
	} else {
		isPresent := false
		for _, subject := range clusterRoleBinding.Subjects {
			if subject.Kind == "ServiceAccount" && subject.Name == saName && subject.Namespace == graph.GetNamespace() {
				isPresent = true
				break
			}
		}
		if !isPresent {
			clusterRoleBinding.Subjects = append(clusterRoleBinding.Subjects, rbacv1.Subject{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: graph.GetNamespace(),
			})
			_, err = clientset.RbacV1().ClusterRoleBindings().Update(ctx, clusterRoleBinding, metav1.UpdateOptions{FieldManager: InferenceGraphControllerName})
			if err != nil {
				return errors.Wrapf(err, "fails to bind privileges for auth verification to inference graph")
			}
		}
	}

	return nil
}

func removeAuthPrivilegesFromGraphServiceAccount(ctx context.Context, clientset kubernetes.Interface, graph *v1alpha1api.InferenceGraph) error {
	clusterRole, err := clientset.RbacV1().ClusterRoleBindings().Get(ctx, constants.InferenceGraphAuthCRBName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return errors.Wrapf(err, "fails to get cluster role binding kserve-inferencegraph-auth-verifiers while deconfiguring inference graph auth")
	}

	isPresent := false
	saName := getServiceAccountNameForGraph(graph)
	for idx, subject := range clusterRole.Subjects {
		if subject.Kind == "ServiceAccount" && subject.Name == saName && subject.Namespace == graph.GetNamespace() {
			isPresent = true

			// Remove the no longer needed entry
			clusterRole.Subjects[idx] = clusterRole.Subjects[len(clusterRole.Subjects)-1]
			clusterRole.Subjects = clusterRole.Subjects[:len(clusterRole.Subjects)-1]
			break
		}
	}

	if isPresent {
		_, err = clientset.RbacV1().ClusterRoleBindings().Update(ctx, clusterRole, metav1.UpdateOptions{FieldManager: InferenceGraphControllerName})
		if err != nil {
			return errors.Wrapf(err, "fails to remove privileges for auth verification from inference graph")
		}
	}

	return nil
}

func deleteGraphServiceAccount(ctx context.Context, clientset kubernetes.Interface, graph *v1alpha1api.InferenceGraph) error {
	saName := getServiceAccountNameForGraph(graph)
	err := clientset.CoreV1().ServiceAccounts(graph.GetNamespace()).Delete(ctx, saName, metav1.DeleteOptions{})
	if client.IgnoreNotFound(err) != nil {
		return errors.Wrapf(err, "fails to delete service account for inference graph while deconfiguring auth")
	}
	return nil
}

func getServiceAccountNameForGraph(graph *v1alpha1api.InferenceGraph) string {
	return fmt.Sprintf("%s-auth-verifier", graph.GetName())
}

/*
PropagateRawStatus Propagates deployment status onto Inference graph status.
In raw deployment mode, deployment available denotes the ready status for IG
*/
func PropagateRawStatus(graphStatus *v1alpha1api.InferenceGraphStatus, deployment *appsv1.Deployment,
	url *apis.URL) {
	for _, con := range deployment.Status.Conditions {
		if con.Type == appsv1.DeploymentAvailable {
			graphStatus.URL = url

			conditions := []apis.Condition{
				{
					Type:   apis.ConditionReady,
					Status: v1.ConditionTrue,
				},
			}
			graphStatus.SetConditions(conditions)
			logger.Info("status propagated:")
			break
		}
	}
	graphStatus.ObservedGeneration = deployment.Status.ObservedGeneration
}
