/*


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

package hostedcluster

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/blang/semver"
	"github.com/go-logr/logr"
	capiibmv1 "github.com/kubernetes-sigs/cluster-api-provider-ibmcloud/api/v1alpha4"
	configv1 "github.com/openshift/api/config/v1"
	routev1 "github.com/openshift/api/route/v1"
	"github.com/openshift/hypershift/api"
	hyperv1 "github.com/openshift/hypershift/api/v1alpha1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/kas"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/util"
	"github.com/openshift/hypershift/hypershift-operator/controllers/manifests"
	"github.com/openshift/hypershift/hypershift-operator/controllers/manifests/autoscaler"
	"github.com/openshift/hypershift/hypershift-operator/controllers/manifests/clusterapi"
	"github.com/openshift/hypershift/hypershift-operator/controllers/manifests/controlplaneoperator"
	"github.com/openshift/hypershift/hypershift-operator/controllers/manifests/ignitionserver"
	"github.com/openshift/hypershift/hypershift-operator/controllers/manifests/machineapprover"
	hyperutil "github.com/openshift/hypershift/hypershift-operator/controllers/util"
	"github.com/openshift/hypershift/support/certs"
	"github.com/openshift/hypershift/support/releaseinfo"
	"github.com/openshift/hypershift/support/upsert"
	prometheusoperatorv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/trace"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/util/workqueue"
	k8sutilspointer "k8s.io/utils/pointer"
	capiawsv1 "sigs.k8s.io/cluster-api-provider-aws/api/v1alpha4"
	capiv1alpha4 "sigs.k8s.io/cluster-api/api/v1alpha4"
	capiv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	"sigs.k8s.io/yaml"
)

const (
	finalizer                      = "hypershift.openshift.io/finalizer"
	hostedClusterAnnotation        = "hypershift.openshift.io/cluster"
	clusterDeletionRequeueDuration = time.Duration(5 * time.Second)

	// TODO (alberto): Eventually these images will be mirrored and pulled from an internal registry.
	imageClusterAutoscaler = "k8s.gcr.io/autoscaling/cluster-autoscaler:v1.21.0"
	// This comes from https://console.cloud.google.com/gcr/images/k8s-staging-cluster-api/global/
	imageCAPI = "us.gcr.io/k8s-artifacts-prod/cluster-api/cluster-api-controller:v1.0.0"
	// This comes from https://console.cloud.google.com/gcr/images/k8s-artifacts-prod
	imageCAPA = "us.gcr.io/k8s-artifacts-prod/cluster-api-aws/cluster-api-aws-controller:v0.7.0"
	// This image is built from https://github.com/openshift/cluster-machine-approver/tree/release-4.10
	imageMachineApprover = "quay.io/openshift/origin-cluster-machine-approver:4.10.0"
)

// NoopReconcile is just a default mutation function that does nothing.
var NoopReconcile controllerutil.MutateFn = func() error { return nil }

// HostedClusterReconciler reconciles a HostedCluster object
type HostedClusterReconciler struct {
	client.Client

	DiscoveryClient *discovery.DiscoveryClient

	// HypershiftOperatorImage is the image used to deploy the control plane operator if
	// 1) There is no hypershift.openshift.io/control-plane-operator-image annotation on the HostedCluster and
	// 2) The OCP version being deployed is the latest version supported by Hypershift
	HypershiftOperatorImage string

	// releaseProvider looks up the OCP version for the release images in HostedClusters
	ReleaseProvider releaseinfo.Provider

	// IgnitionServerImage is the image used to deploy the ignition server.
	IgnitionServerImage string

	// Log is a thread-safe logger.
	Log logr.Logger

	// Clock is used to determine the time in a testable way.
	Clock clock.Clock

	tracer trace.Tracer

	EnableOCPClusterMonitoring bool

	upsert.CreateOrUpdateProvider

	EnableCIDebugOutput bool
}

// +kubebuilder:rbac:groups=hypershift.openshift.io,resources=hostedclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=hypershift.openshift.io,resources=hostedclusters/status,verbs=get;update;patch

func (r *HostedClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Clock == nil {
		r.Clock = clock.RealClock{}
	}
	r.tracer = otel.Tracer("hostedcluster-controller")
	// Set up watches for resource types the controller manages. The list basically
	// tracks types of the resources in the clusterapi, controlplaneoperator, and
	// ignitionserver manifests packages. Since we're receiving watch events across
	// namespaces, the events are filtered to enqueue only those resources which
	// are annotated as being associated with a hostedcluster (using an annotation).
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&hyperv1.HostedCluster{}).
		Watches(&source.Kind{Type: &capiawsv1.AWSCluster{}}, handler.EnqueueRequestsFromMapFunc(enqueueParentHostedCluster)).
		Watches(&source.Kind{Type: &hyperv1.HostedControlPlane{}}, handler.EnqueueRequestsFromMapFunc(enqueueParentHostedCluster)).
		Watches(&source.Kind{Type: &capiv1.Cluster{}}, handler.EnqueueRequestsFromMapFunc(enqueueParentHostedCluster)).
		Watches(&source.Kind{Type: &appsv1.Deployment{}}, handler.EnqueueRequestsFromMapFunc(enqueueParentHostedCluster)).
		Watches(&source.Kind{Type: &prometheusoperatorv1.PodMonitor{}}, handler.EnqueueRequestsFromMapFunc(enqueueParentHostedCluster)).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewItemExponentialFailureRateLimiter(1*time.Second, 10*time.Second),
		})

	if routesEnabled, err := r.routesEnabled(); routesEnabled {
		builder.Watches(&source.Kind{Type: &routev1.Route{}}, handler.EnqueueRequestsFromMapFunc(enqueueParentHostedCluster))
	} else if err != nil {
		return fmt.Errorf("unable to determine if routes are registered on the cluster: %v", err)
	}

	return builder.Complete(r)
}

func (r *HostedClusterReconciler) routesEnabled() (bool, error) {
	return isGroupVersionRegistered(r.DiscoveryClient, routev1.GroupVersion)
}

// serviceFirstNodePortAvailable checks if the first port in a service has a node port available. Utilized to
// check status of the ignition service
func serviceFirstNodePortAvailable(svc *corev1.Service) bool {
	return svc != nil && len(svc.Spec.Ports) > 0 && svc.Spec.Ports[0].NodePort > 0

}

func (r *HostedClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = baggage.ContextWithValues(ctx,
		attribute.String("request", req.String()),
	)
	var span trace.Span
	ctx, span = r.tracer.Start(ctx, "reconcile")
	defer span.End()

	r.Log = ctrl.LoggerFrom(ctx)
	r.Log.Info("reconciling")

	// Look up the HostedCluster instance to reconcile
	hcluster := &hyperv1.HostedCluster{}
	isMissing := false
	err := r.Get(ctx, req.NamespacedName, hcluster)
	if err != nil {
		if apierrors.IsNotFound(err) {
			isMissing = true
		} else {
			return ctrl.Result{}, fmt.Errorf("failed to get cluster %q: %w", req.NamespacedName, err)
		}
	}

	// If deleted or missing, clean up and return early.
	// TODO: This should be incorporated with status/reconcile
	if isMissing || !hcluster.DeletionTimestamp.IsZero() {
		// Keep trying to delete until we know it's safe to finalize.
		completed, err := r.delete(ctx, req, hcluster)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to delete cluster: %w", err)
		}
		if !completed {
			r.Log.Info("hostedcluster is still deleting", "name", req.NamespacedName)
			return ctrl.Result{RequeueAfter: clusterDeletionRequeueDuration}, nil
		}
		r.Log.Info("finished deleting hostedcluster", "name", req.NamespacedName)
		// Now we can remove the finalizer.
		if controllerutil.ContainsFinalizer(hcluster, finalizer) {
			controllerutil.RemoveFinalizer(hcluster, finalizer)
			if err := r.Update(ctx, hcluster); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to remove finalizer from cluster: %w", err)
			}
			r.Log.Info("hostedcluster was finalized", "name", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, nil
	}

	// Part one: update status

	// Set kubeconfig status
	{
		kubeConfigSecret := manifests.KubeConfigSecret(hcluster.Namespace, hcluster.Name)
		err := r.Client.Get(ctx, client.ObjectKeyFromObject(kubeConfigSecret), kubeConfigSecret)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("failed to reconcile kubeconfig secret: %w", err)
			}
		} else {
			hcluster.Status.KubeConfig = &corev1.LocalObjectReference{Name: kubeConfigSecret.Name}
		}
	}

	// Set version status
	{
		controlPlaneNamespace := manifests.HostedControlPlaneNamespace(hcluster.Namespace, hcluster.Name)
		hcp := controlplaneoperator.HostedControlPlane(controlPlaneNamespace.Name, hcluster.Name)
		err := r.Client.Get(ctx, client.ObjectKeyFromObject(hcp), hcp)
		if err != nil {
			if apierrors.IsNotFound(err) {
				hcp = nil
			} else {
				return ctrl.Result{}, fmt.Errorf("failed to get hostedcontrolplane: %w", err)
			}
		}
		hcluster.Status.Version = computeClusterVersionStatus(r.Clock, hcluster, hcp)
	}

	// Reconcile unmanaged etcd client tls secret validation error status. Note only update status on validation error case to
	// provide clear status to the user on the resource without having to look at operator logs.
	{
		if hcluster.Spec.Etcd.ManagementType == hyperv1.Unmanaged {
			unmanagedEtcdTLSClientSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: hcluster.GetNamespace(),
					Name:      hcluster.Spec.Etcd.Unmanaged.TLS.ClientSecret.Name,
				},
			}
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(unmanagedEtcdTLSClientSecret), unmanagedEtcdTLSClientSecret); err != nil {
				if apierrors.IsNotFound(err) {
					unmanagedEtcdTLSClientSecret = nil
				} else {
					return ctrl.Result{}, fmt.Errorf("failed to get unmanaged etcd tls secret: %w", err)
				}
			}
			meta.SetStatusCondition(&hcluster.Status.Conditions, computeUnmanagedEtcdAvailability(hcluster, unmanagedEtcdTLSClientSecret))
		}
	}

	// Set the Available condition
	// TODO: This is really setting something that could be more granular like
	// HostedControlPlaneAvailable, and then the HostedCluster high-level Available
	// condition could be computed as a function of the granular ThingAvailable
	// conditions (so that it could incorporate e.g. HostedControlPlane and IgnitionServer
	// availability in the ultimate HostedCluster Available condition)
	{
		controlPlaneNamespace := manifests.HostedControlPlaneNamespace(hcluster.Namespace, hcluster.Name)
		hcp := controlplaneoperator.HostedControlPlane(controlPlaneNamespace.Name, hcluster.Name)
		err := r.Client.Get(ctx, client.ObjectKeyFromObject(hcp), hcp)
		if err != nil {
			if apierrors.IsNotFound(err) {
				hcp = nil
			} else {
				return ctrl.Result{}, fmt.Errorf("failed to get hostedcontrolplane: %w", err)
			}
		}
		meta.SetStatusCondition(&hcluster.Status.Conditions, computeHostedClusterAvailability(hcluster, hcp))
	}

	// Set ValidConfiguration condition
	{
		controlPlaneNamespace := manifests.HostedControlPlaneNamespace(hcluster.Namespace, hcluster.Name)
		hcp := controlplaneoperator.HostedControlPlane(controlPlaneNamespace.Name, hcluster.Name)
		err := r.Client.Get(ctx, client.ObjectKeyFromObject(hcp), hcp)
		if err != nil {
			if apierrors.IsNotFound(err) {
				hcp = nil
			} else {
				return ctrl.Result{}, fmt.Errorf("failed to get hostedcontrolplane: %w", err)
			}
		}
		condition := metav1.Condition{
			Type:   string(hyperv1.ValidHostedClusterConfiguration),
			Status: metav1.ConditionUnknown,
			Reason: "StatusUnknown",
		}
		if hcp != nil {
			validConfigHCPCondition := meta.FindStatusCondition(hcp.Status.Conditions, string(hyperv1.ValidConfiguration))
			if validConfigHCPCondition != nil {
				condition.Status = validConfigHCPCondition.Status
				condition.Message = validConfigHCPCondition.Message
				condition.Reason = validConfigHCPCondition.Reason
			}
		}
		meta.SetStatusCondition(&hcluster.Status.Conditions, condition)
	}

	// Set Ignition Server endpoint
	{
		serviceStrategy := servicePublishingStrategyByType(hcluster, hyperv1.Ignition)
		if serviceStrategy == nil {
			// We don't return the error here as reconciling won't solve the input problem.
			// An update event will trigger reconciliation.
			r.Log.Error(fmt.Errorf("ignition server service strategy not specified"), "")
			return ctrl.Result{}, nil
		}
		controlPlaneNamespace := manifests.HostedControlPlaneNamespace(hcluster.Namespace, hcluster.Name)
		switch serviceStrategy.Type {
		case hyperv1.Route:
			ignitionServerRoute := ignitionserver.Route(controlPlaneNamespace.GetName())
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(ignitionServerRoute), ignitionServerRoute); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, fmt.Errorf("failed to get ignitionServerRoute: %w", err)
				}
			}
			if err == nil && ignitionServerRoute.Spec.Host != "" {
				hcluster.Status.IgnitionEndpoint = ignitionServerRoute.Spec.Host
			}
		case hyperv1.NodePort:
			if serviceStrategy.NodePort == nil {
				// We don't return the error here as reconciling won't solve the input problem.
				// An update event will trigger reconciliation.
				r.Log.Error(fmt.Errorf("nodeport metadata not specified for ignition service"), "")
				return ctrl.Result{}, nil
			}
			ignitionService := ignitionserver.Service(controlPlaneNamespace.GetName())
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(ignitionService), ignitionService); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, fmt.Errorf("failed to get ignition service: %w", err)
				}
			}
			if err == nil && serviceFirstNodePortAvailable(ignitionService) {
				hcluster.Status.IgnitionEndpoint = fmt.Sprintf("%s:%d", serviceStrategy.NodePort.Address, ignitionService.Spec.Ports[0].NodePort)
			}
		default:
			// We don't return the error here as reconciling won't solve the input problem.
			// An update event will trigger reconciliation.
			r.Log.Error(fmt.Errorf("unknown service strategy type for ignition service: %s", serviceStrategy.Type), "")
			return ctrl.Result{}, nil
		}
	}

	// Set the ignition server availability condition by checking its deployment.
	{
		// Assume the server is unavailable unless proven otherwise.
		newCondition := metav1.Condition{
			Type:   string(hyperv1.IgnitionEndpointAvailable),
			Status: metav1.ConditionUnknown,
			Reason: hyperv1.IgnitionServerDeploymentStatusUnknownReason,
		}
		// Check to ensure the deployment exists and is available.
		controlPlaneNamespace := manifests.HostedControlPlaneNamespace(hcluster.Namespace, hcluster.Name)
		deployment := ignitionserver.Deployment(controlPlaneNamespace.Name)
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(deployment), deployment); err != nil {
			if apierrors.IsNotFound(err) {
				newCondition = metav1.Condition{
					Type:   string(hyperv1.IgnitionEndpointAvailable),
					Status: metav1.ConditionFalse,
					Reason: hyperv1.IgnitionServerDeploymentNotFoundReason,
				}
			} else {
				return ctrl.Result{}, fmt.Errorf("failed to get ignition server deployment: %w", err)
			}
		} else {
			// Assume the deployment is unavailable until proven otherwise.
			newCondition = metav1.Condition{
				Type:   string(hyperv1.IgnitionEndpointAvailable),
				Status: metav1.ConditionFalse,
				Reason: hyperv1.IgnitionServerDeploymentUnavailableReason,
			}
			for _, cond := range deployment.Status.Conditions {
				if cond.Type == appsv1.DeploymentAvailable && cond.Status == corev1.ConditionTrue {
					newCondition = metav1.Condition{
						Type:   string(hyperv1.IgnitionEndpointAvailable),
						Status: metav1.ConditionTrue,
						Reason: hyperv1.IgnitionServerDeploymentAsExpectedReason,
					}
					break
				}
			}
		}
		newCondition.ObservedGeneration = hcluster.Generation
		meta.SetStatusCondition(&hcluster.Status.Conditions, newCondition)
		span.AddEvent("updated ignition endpoint condition", trace.WithAttributes(attribute.String(newCondition.Type, string(newCondition.Status))))
	}

	// Persist status updates
	if err := r.Client.Status().Update(ctx, hcluster); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to update status: %w", err)
	}

	// Part two: reconcile the state of the world

	// Ensure the cluster has a finalizer for cleanup and update right away.
	if !controllerutil.ContainsFinalizer(hcluster, finalizer) {
		controllerutil.AddFinalizer(hcluster, finalizer)
		if err := r.Update(ctx, hcluster); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer to cluster: %w", err)
		}
	}

	// Reconcile the hosted cluster namespace
	controlPlaneNamespace := manifests.HostedControlPlaneNamespace(hcluster.Namespace, hcluster.Name)
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, controlPlaneNamespace, func() error {
		if controlPlaneNamespace.Labels == nil {
			controlPlaneNamespace.Labels = make(map[string]string)
		}
		controlPlaneNamespace.Labels["hypershift.openshift.io/hosted-control-plane"] = ""
		if r.EnableOCPClusterMonitoring {
			controlPlaneNamespace.Labels["openshift.io/cluster-monitoring"] = "true"
		}
		return nil
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile namespace: %w", err)
	}

	// Reconcile the platform provider cloud controller credentials secret by resolving
	// the reference from the HostedCluster and syncing the secret in the control
	// plane namespace.
	switch hcluster.Spec.Platform.Type {
	case hyperv1.AWSPlatform:
		var src corev1.Secret
		err = r.Client.Get(ctx, client.ObjectKey{Namespace: hcluster.GetNamespace(), Name: hcluster.Spec.Platform.AWS.KubeCloudControllerCreds.Name}, &src)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get cloud controller provider creds %s: %w", hcluster.Spec.Platform.AWS.KubeCloudControllerCreds.Name, err)
		}
		dest := manifests.AWSKubeCloudControllerCreds(controlPlaneNamespace.Name)
		_, err = r.CreateOrUpdate(ctx, r.Client, dest, func() error {
			srcData, srcHasData := src.Data["credentials"]
			if !srcHasData {
				return fmt.Errorf("hostedcluster cloud controller provider credentials secret %q must have a credentials key", src.Name)
			}
			dest.Type = corev1.SecretTypeOpaque
			if dest.Data == nil {
				dest.Data = map[string][]byte{}
			}
			dest.Data["credentials"] = srcData
			return nil
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to reconcile cloud controller provider creds: %w", err)
		}
	}

	// Reconcile the platform provider node pool management credentials secret by
	// resolving  the reference from the HostedCluster and syncing the secret in
	// the control plane namespace.
	switch hcluster.Spec.Platform.Type {
	case hyperv1.AWSPlatform:
		var src corev1.Secret
		err = r.Client.Get(ctx, client.ObjectKey{Namespace: hcluster.GetNamespace(), Name: hcluster.Spec.Platform.AWS.NodePoolManagementCreds.Name}, &src)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get node pool provider creds %s: %w", hcluster.Spec.Platform.AWS.NodePoolManagementCreds.Name, err)
		}
		dest := manifests.AWSNodePoolManagementCreds(controlPlaneNamespace.Name)
		_, err = r.CreateOrUpdate(ctx, r.Client, dest, func() error {
			srcData, srcHasData := src.Data["credentials"]
			if !srcHasData {
				return fmt.Errorf("node pool provider credentials secret %q is missing credentials key", src.Name)
			}
			dest.Type = corev1.SecretTypeOpaque
			if dest.Data == nil {
				dest.Data = map[string][]byte{}
			}
			dest.Data["credentials"] = srcData
			return nil
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to reconcile node pool provider creds: %w", err)
		}
	}

	// Reconcile the HostedControlPlane pull secret by resolving the source secret
	// reference from the HostedCluster and syncing the secret in the control plane namespace.
	{
		var src corev1.Secret
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: hcluster.GetNamespace(), Name: hcluster.Spec.PullSecret.Name}, &src); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get pull secret %s: %w", hcluster.Spec.PullSecret.Name, err)
		}
		dst := controlplaneoperator.PullSecret(controlPlaneNamespace.Name)
		_, err = r.CreateOrUpdate(ctx, r.Client, dst, func() error {
			srcData, srcHasData := src.Data[".dockerconfigjson"]
			if !srcHasData {
				return fmt.Errorf("hostedcluster pull secret %q must have a .dockerconfigjson key", src.Name)
			}
			dst.Type = corev1.SecretTypeDockerConfigJson
			if dst.Data == nil {
				dst.Data = map[string][]byte{}
			}
			dst.Data[".dockerconfigjson"] = srcData
			return nil
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to reconcile pull secret: %w", err)
		}
	}

	// Reconcile the HostedControlPlane Secret Encryption Info
	if hcluster.Spec.SecretEncryption != nil {
		r.Log.Info("Reconciling secret encryption configuration")
		switch hcluster.Spec.SecretEncryption.Type {
		case hyperv1.AESCBC:
			if hcluster.Spec.SecretEncryption.AESCBC == nil || len(hcluster.Spec.SecretEncryption.AESCBC.ActiveKey.Name) == 0 {
				r.Log.Error(fmt.Errorf("aescbc metadata  is nil"), "")
				// don't return error here as reconciling won't fix input error
				return ctrl.Result{}, nil
			}
			var src corev1.Secret
			if err := r.Client.Get(ctx, client.ObjectKey{Namespace: hcluster.GetNamespace(), Name: hcluster.Spec.SecretEncryption.AESCBC.ActiveKey.Name}, &src); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to get active aescbc secret %s: %w", hcluster.Spec.SecretEncryption.AESCBC.ActiveKey.Name, err)
			}
			if _, ok := src.Data[hyperv1.AESCBCKeySecretKey]; !ok {
				r.Log.Error(fmt.Errorf("no key field %s specified for aescbc active key secret", hyperv1.AESCBCKeySecretKey), "")
				// don't return error here as reconciling won't fix input error
				return ctrl.Result{}, nil
			}
			hostedControlPlaneActiveKeySecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: controlPlaneNamespace.Name,
					Name:      src.Name,
				},
			}
			_, err = r.CreateOrUpdate(ctx, r.Client, hostedControlPlaneActiveKeySecret, func() error {
				if hostedControlPlaneActiveKeySecret.Data == nil {
					hostedControlPlaneActiveKeySecret.Data = map[string][]byte{}
				}
				hostedControlPlaneActiveKeySecret.Data[hyperv1.AESCBCKeySecretKey] = src.Data[hyperv1.AESCBCKeySecretKey]
				hostedControlPlaneActiveKeySecret.Type = corev1.SecretTypeOpaque
				return nil
			})
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed reconciling aescbc active key: %w", err)
			}
			if hcluster.Spec.SecretEncryption.AESCBC.BackupKey != nil && len(hcluster.Spec.SecretEncryption.AESCBC.BackupKey.Name) > 0 {
				var src corev1.Secret
				if err := r.Client.Get(ctx, client.ObjectKey{Namespace: hcluster.GetNamespace(), Name: hcluster.Spec.SecretEncryption.AESCBC.BackupKey.Name}, &src); err != nil {
					return ctrl.Result{}, fmt.Errorf("failed to get backup aescbc secret %s: %w", hcluster.Spec.SecretEncryption.AESCBC.BackupKey.Name, err)
				}
				if _, ok := src.Data[hyperv1.AESCBCKeySecretKey]; !ok {
					r.Log.Error(fmt.Errorf("no key field %s specified for aescbc backup key secret", hyperv1.AESCBCKeySecretKey), "")
					// don't return error here as reconciling won't fix input error
					return ctrl.Result{}, nil
				}
				hostedControlPlaneBackupKeySecret := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: controlPlaneNamespace.Name,
						Name:      src.Name,
					},
				}
				_, err = r.CreateOrUpdate(ctx, r.Client, hostedControlPlaneBackupKeySecret, func() error {
					if hostedControlPlaneBackupKeySecret.Data == nil {
						hostedControlPlaneBackupKeySecret.Data = map[string][]byte{}
					}
					hostedControlPlaneBackupKeySecret.Data[hyperv1.AESCBCKeySecretKey] = src.Data[hyperv1.AESCBCKeySecretKey]
					hostedControlPlaneBackupKeySecret.Type = corev1.SecretTypeOpaque
					return nil
				})
				if err != nil {
					return ctrl.Result{}, fmt.Errorf("failed reconciling aescbc backup key: %w", err)
				}
			}
		case hyperv1.KMS:
			if hcluster.Spec.SecretEncryption.KMS == nil {
				r.Log.Error(fmt.Errorf("kms metadata nil"), "")
				// don't return error here as reconciling won't fix input error
				return ctrl.Result{}, nil
			}
			switch hcluster.Spec.SecretEncryption.KMS.Provider {
			case hyperv1.IBMCloud:
				if hcluster.Spec.SecretEncryption.KMS.IBMCloud == nil {
					r.Log.Error(fmt.Errorf("ibm kms metadata nil"), "")
					// don't return error here as reconciling won't fix input error
					return ctrl.Result{}, nil
				}
				if hcluster.Spec.SecretEncryption.KMS.IBMCloud.Auth.Type == hyperv1.IBMCloudKMSUnmanagedAuth {
					if hcluster.Spec.SecretEncryption.KMS.IBMCloud.Auth.Unmanaged == nil || len(hcluster.Spec.SecretEncryption.KMS.IBMCloud.Auth.Unmanaged.Credentials.Name) == 0 {
						r.Log.Error(fmt.Errorf("ibm unmanaged auth credential nil"), "")
						// don't return error here as reconciling won't fix input error
						return ctrl.Result{}, nil
					}
					var src corev1.Secret
					if err := r.Client.Get(ctx, client.ObjectKey{Namespace: hcluster.GetNamespace(), Name: hcluster.Spec.SecretEncryption.KMS.IBMCloud.Auth.Unmanaged.Credentials.Name}, &src); err != nil {
						return ctrl.Result{}, fmt.Errorf("failed to get ibmcloud kms credentials %s: %w", hcluster.Spec.SecretEncryption.KMS.IBMCloud.Auth.Unmanaged.Credentials.Name, err)
					}
					if _, ok := src.Data[hyperv1.IBMCloudIAMAPIKeySecretKey]; !ok {
						r.Log.Error(fmt.Errorf("no ibmcloud iam apikey field %s specified in auth secret", hyperv1.IBMCloudIAMAPIKeySecretKey), "")
						// don't return error here as reconciling won't fix input error
						return ctrl.Result{}, nil
					}
					hostedControlPlaneIBMCloudKMSAuthSecret := &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{
							Namespace: controlPlaneNamespace.Name,
							Name:      src.Name,
						},
					}
					_, err = r.CreateOrUpdate(ctx, r.Client, hostedControlPlaneIBMCloudKMSAuthSecret, func() error {
						if hostedControlPlaneIBMCloudKMSAuthSecret.Data == nil {
							hostedControlPlaneIBMCloudKMSAuthSecret.Data = map[string][]byte{}
						}
						hostedControlPlaneIBMCloudKMSAuthSecret.Data[hyperv1.IBMCloudIAMAPIKeySecretKey] = src.Data[hyperv1.IBMCloudIAMAPIKeySecretKey]
						hostedControlPlaneIBMCloudKMSAuthSecret.Type = corev1.SecretTypeOpaque
						return nil
					})
					if err != nil {
						return ctrl.Result{}, fmt.Errorf("failed reconciling aescbc backup key: %w", err)
					}
				}
			case hyperv1.AWS:
				if hcluster.Spec.SecretEncryption.KMS.AWS == nil || len(hcluster.Spec.SecretEncryption.KMS.AWS.Auth.Credentials.Name) == 0 {
					r.Log.Error(fmt.Errorf("aws kms metadata nil"), "")
					// don't return error here as reconciling won't fix input error
					return ctrl.Result{}, nil
				}
				var src corev1.Secret
				if err := r.Client.Get(ctx, client.ObjectKey{Namespace: hcluster.GetNamespace(), Name: hcluster.Spec.SecretEncryption.KMS.AWS.Auth.Credentials.Name}, &src); err != nil {
					return ctrl.Result{}, fmt.Errorf("failed to get ibmcloud kms credentials %s: %w", hcluster.Spec.SecretEncryption.KMS.IBMCloud.Auth.Unmanaged.Credentials.Name, err)
				}
				if _, ok := src.Data[hyperv1.AWSCredentialsFileSecretKey]; !ok {
					r.Log.Error(fmt.Errorf("aws credential key %s not present in auth secret", hyperv1.AWSCredentialsFileSecretKey), "")
					// don't return error here as reconciling won't fix input error
					return ctrl.Result{}, nil
				}
				hostedControlPlaneAWSKMSAuthSecret := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: controlPlaneNamespace.Name,
						Name:      src.Name,
					},
				}
				_, err = r.CreateOrUpdate(ctx, r.Client, hostedControlPlaneAWSKMSAuthSecret, func() error {
					if hostedControlPlaneAWSKMSAuthSecret.Data == nil {
						hostedControlPlaneAWSKMSAuthSecret.Data = map[string][]byte{}
					}
					hostedControlPlaneAWSKMSAuthSecret.Data[hyperv1.AWSCredentialsFileSecretKey] = src.Data[hyperv1.AWSCredentialsFileSecretKey]
					hostedControlPlaneAWSKMSAuthSecret.Type = corev1.SecretTypeOpaque
					return nil
				})
				if err != nil {
					return ctrl.Result{}, fmt.Errorf("failed reconciling aws kms backup key: %w", err)
				}
			default:
				r.Log.Error(fmt.Errorf("unsupported kms provider %s", hcluster.Spec.SecretEncryption.KMS.Provider), "")
				// don't return error here as reconciling won't fix input error
				return ctrl.Result{}, nil
			}
		default:
			r.Log.Error(fmt.Errorf("unsupported encryption type %s", hcluster.Spec.SecretEncryption.Type), "")
			// don't return error here as reconciling won't fix input error
			return ctrl.Result{}, nil
		}
	}

	// Reconcile the HostedControlPlane audit webhook config if specified
	// reference from the HostedCluster and syncing the secret in the control plane namespace.
	{
		if hcluster.Spec.AuditWebhook != nil && len(hcluster.Spec.AuditWebhook.Name) > 0 {
			var src corev1.Secret
			if err := r.Client.Get(ctx, client.ObjectKey{Namespace: hcluster.GetNamespace(), Name: hcluster.Spec.AuditWebhook.Name}, &src); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to get audit webhook config %s: %w", hcluster.Spec.AuditWebhook.Name, err)
			}
			configData, ok := src.Data[hyperv1.AuditWebhookKubeconfigKey]
			if !ok {
				return ctrl.Result{}, fmt.Errorf("audit webhook secret does not contain key %s", hyperv1.AuditWebhookKubeconfigKey)
			}

			hostedControlPlaneAuditWebhookSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: controlPlaneNamespace.Name,
					Name:      src.Name,
				},
			}
			_, err = r.CreateOrUpdate(ctx, r.Client, hostedControlPlaneAuditWebhookSecret, func() error {
				if hostedControlPlaneAuditWebhookSecret.Data == nil {
					hostedControlPlaneAuditWebhookSecret.Data = map[string][]byte{}
				}
				hostedControlPlaneAuditWebhookSecret.Data[hyperv1.AuditWebhookKubeconfigKey] = configData
				hostedControlPlaneAuditWebhookSecret.Type = corev1.SecretTypeOpaque
				return nil
			})
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed reconciling audit webhook secret: %w", err)
			}
		}
	}

	// Reconcile the HostedControlPlane SSH secret by resolving the source secret reference
	// from the HostedCluster and syncing the secret in the control plane namespace.
	if len(hcluster.Spec.SSHKey.Name) > 0 {
		var src corev1.Secret
		err = r.Client.Get(ctx, client.ObjectKey{Namespace: hcluster.Namespace, Name: hcluster.Spec.SSHKey.Name}, &src)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get hostedcluster SSH key secret %s: %w", hcluster.Spec.SSHKey.Name, err)
		}
		dest := controlplaneoperator.SSHKey(controlPlaneNamespace.Name)
		_, err = r.CreateOrUpdate(ctx, r.Client, dest, func() error {
			srcData, srcHasData := src.Data["id_rsa.pub"]
			if !srcHasData {
				return fmt.Errorf("hostedcluster ssh key secret %q must have a id_rsa.pub key", src.Name)
			}
			dest.Type = corev1.SecretTypeOpaque
			if dest.Data == nil {
				dest.Data = map[string][]byte{}
			}
			dest.Data["id_rsa.pub"] = srcData
			return nil
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to reconcile controlplane ssh secret: %w", err)
		}
	}

	// Reconcile etcd client MTLS secret if the control plane is using an unmanaged etcd cluster
	if hcluster.Spec.Etcd.ManagementType == hyperv1.Unmanaged {
		unmanagedEtcdTLSClientSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: hcluster.GetNamespace(),
				Name:      hcluster.Spec.Etcd.Unmanaged.TLS.ClientSecret.Name,
			},
		}
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(unmanagedEtcdTLSClientSecret), unmanagedEtcdTLSClientSecret); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get unmanaged etcd tls secret: %w", err)
		}
		hostedControlPlaneEtcdClientSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: controlPlaneNamespace.Name,
				Name:      hcluster.Spec.Etcd.Unmanaged.TLS.ClientSecret.Name,
			},
		}
		if result, err := r.CreateOrUpdate(ctx, r.Client, hostedControlPlaneEtcdClientSecret, func() error {
			if hostedControlPlaneEtcdClientSecret.Data == nil {
				hostedControlPlaneEtcdClientSecret.Data = map[string][]byte{}
			}
			hostedControlPlaneEtcdClientSecret.Data = unmanagedEtcdTLSClientSecret.Data
			hostedControlPlaneEtcdClientSecret.Type = corev1.SecretTypeOpaque
			return nil
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed reconciling etcd client secret: %w", err)
		} else {
			r.Log.Info("reconciled etcd client mtls secret to control plane namespace", "result", result)
		}
	}

	// Reconcile global config related configmaps and secrets
	{
		if hcluster.Spec.Configuration != nil {
			for _, configMapRef := range hcluster.Spec.Configuration.ConfigMapRefs {
				sourceCM := &corev1.ConfigMap{}
				if err := r.Get(ctx, client.ObjectKey{Namespace: hcluster.Namespace, Name: configMapRef.Name}, sourceCM); err != nil {
					return ctrl.Result{}, fmt.Errorf("failed to get referenced configmap %s/%s: %w", hcluster.Namespace, configMapRef.Name, err)
				}
				destCM := &corev1.ConfigMap{}
				destCM.Name = sourceCM.Name
				destCM.Namespace = controlPlaneNamespace.Name
				if _, err := r.CreateOrUpdate(ctx, r.Client, destCM, func() error {
					destCM.Annotations = sourceCM.Annotations
					destCM.Labels = sourceCM.Labels
					destCM.Data = sourceCM.Data
					destCM.BinaryData = sourceCM.BinaryData
					destCM.Immutable = sourceCM.Immutable
					return nil
				}); err != nil {
					return ctrl.Result{}, fmt.Errorf("failed to reconcile referenced config map %s/%s: %w", destCM.Namespace, destCM.Name, err)
				}
			}

			for _, secretRef := range hcluster.Spec.Configuration.SecretRefs {
				sourceSecret := &corev1.Secret{}
				if err := r.Get(ctx, client.ObjectKey{Namespace: hcluster.Namespace, Name: secretRef.Name}, sourceSecret); err != nil {
					return ctrl.Result{}, fmt.Errorf("failed to get referenced secret %s/%s: %w", hcluster.Namespace, secretRef.Name, err)
				}
				destSecret := &corev1.Secret{}
				destSecret.Name = sourceSecret.Name
				destSecret.Namespace = controlPlaneNamespace.Name
				if _, err := r.CreateOrUpdate(ctx, r.Client, destSecret, func() error {
					destSecret.Annotations = sourceSecret.Annotations
					destSecret.Labels = sourceSecret.Labels
					destSecret.Data = sourceSecret.Data
					destSecret.Immutable = sourceSecret.Immutable
					destSecret.Type = sourceSecret.Type
					return nil
				}); err != nil {
					return ctrl.Result{}, fmt.Errorf("failed to reconcile secret %s/%s: %w", destSecret.Namespace, destSecret.Name, err)
				}
			}
		}
	}

	// Reconcile the HostedControlPlane
	hcp := controlplaneoperator.HostedControlPlane(controlPlaneNamespace.Name, hcluster.Name)
	_, err = r.CreateOrUpdate(ctx, r.Client, hcp, func() error {
		return reconcileHostedControlPlane(hcp, hcluster)
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile hostedcontrolplane: %w", err)
	}

	var infraCR client.Object
	switch hcluster.Spec.Platform.Type {
	// We run the AWS controller for NonePlatform for now
	// So nodePools can be created to expose ign endpoints that can be used for byo machines to join.
	case hyperv1.AWSPlatform, hyperv1.NonePlatform:
		// Reconcile external AWSCluster
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(hcp), hcp); err != nil {
			r.Log.Error(err, "failed to get control plane ref")
			return reconcile.Result{}, err
		}

		awsCluster := controlplaneoperator.AWSCluster(controlPlaneNamespace.Name, hcluster.Name)
		_, err = controllerutil.CreateOrPatch(ctx, r.Client, awsCluster, func() error {
			return reconcileAWSCluster(awsCluster, hcluster, hcp.Status.ControlPlaneEndpoint)
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to reconcile AWSCluster: %w", err)
		}
		infraCR = awsCluster
	case hyperv1.IBMCloudPlatform:
		// Reconcile external IBM Cloud Cluster
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(hcp), hcp); err != nil {
			r.Log.Error(err, "failed to get control plane ref")
			return reconcile.Result{}, err
		}

		ibmCluster := controlplaneoperator.IBMCloudCluster(controlPlaneNamespace.Name, hcluster.Name)
		_, err = r.CreateOrUpdate(ctx, r.Client, ibmCluster, func() error {
			return reconcileIBMCloudCluster(ibmCluster, hcluster, hcp.Status.ControlPlaneEndpoint)
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to reconcile IBMCluster: %w", err)
		}
		infraCR = ibmCluster
	default:
		// TODO(alberto): for platform None implement back a "pass through" infra CR similar to externalInfraCluster.
	}

	// Reconcile cluster prometheus RBAC resources if enabled
	if r.EnableOCPClusterMonitoring {
		if err := reconcileClusterPrometheusRBAC(ctx, r.Client, hcp.Namespace); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to reconcile RBAC for OCP cluster prometheus: %w", err)
		}
	}

	// Reconcile the CAPI Cluster resource
	capiCluster := controlplaneoperator.CAPICluster(controlPlaneNamespace.Name, hcluster.Spec.InfraID)
	_, err = r.CreateOrUpdate(ctx, r.Client, capiCluster, func() error {
		return reconcileCAPICluster(capiCluster, hcluster, hcp, infraCR)
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile capi cluster: %w", err)
	}

	// Reconcile the HostedControlPlane kubeconfig if one is reported
	if hcp.Status.KubeConfig != nil {
		src := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: hcp.Namespace,
				Name:      hcp.Status.KubeConfig.Name,
			},
		}
		err := r.Client.Get(ctx, client.ObjectKeyFromObject(src), src)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get controlplane kubeconfig secret %q: %w", client.ObjectKeyFromObject(src), err)
		}
		dest := manifests.KubeConfigSecret(hcluster.Namespace, hcluster.Name)
		_, err = r.CreateOrUpdate(ctx, r.Client, dest, func() error {
			key := hcp.Status.KubeConfig.Key
			srcData, srcHasData := src.Data[key]
			if !srcHasData {
				return fmt.Errorf("controlplane kubeconfig secret %q must have a %q key", client.ObjectKeyFromObject(src), key)
			}
			dest.Type = corev1.SecretTypeOpaque
			if dest.Data == nil {
				dest.Data = map[string][]byte{}
			}
			dest.Data["kubeconfig"] = srcData
			dest.SetOwnerReferences([]metav1.OwnerReference{{
				APIVersion: hyperv1.GroupVersion.String(),
				Kind:       "HostedCluster",
				Name:       hcluster.Name,
				UID:        hcluster.UID,
			}})
			return nil
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to reconcile hostedcluster kubeconfig secret: %w", err)
		}
	}

	// Reconcile the CAPI manager components
	err = r.reconcileCAPIManager(ctx, hcluster)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile capi manager: %w", err)
	}

	switch hcluster.Spec.Platform.Type {
	case hyperv1.AWSPlatform:
		// Reconcile the CAPI AWS provider components
		err = r.reconcileCAPIAWSProvider(ctx, hcluster)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to reconcile capi aws provider: %w", err)
		}
	default:
		//TODO: add other providers
		r.Log.Info("provider specific cluster api components not specified", "provider", hcluster.Spec.Platform.Type)
	}

	// Reconcile the autoscaler
	err = r.reconcileAutoscaler(ctx, hcluster, hcp)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile autoscaler: %w", err)
	}

	// Reconcile the control plane operator
	err = r.reconcileControlPlaneOperator(ctx, hcluster, hcp)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile control plane operator: %w", err)
	}

	// Reconcile the Ignition server
	if err = r.reconcileIgnitionServer(ctx, hcluster, hcp); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile ignition server: %w", err)
	}

	// Reconcile the machine config server
	if err = r.reconcileMachineConfigServer(ctx, hcluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile machine config server: %w", err)
	}

	if err = r.reconcileMachineApprover(ctx, hcluster, hcp); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile machine approver: %w", err)
	}

	r.Log.Info("successfully reconciled")
	return ctrl.Result{}, nil
}

// reconcileHostedControlPlane reconciles the given HostedControlPlane, which
// will be mutated.
func reconcileHostedControlPlane(hcp *hyperv1.HostedControlPlane, hcluster *hyperv1.HostedCluster) error {
	// Always initialize the HostedControlPlane with an image matching
	// the HostedCluster.
	if hcp.ObjectMeta.CreationTimestamp.IsZero() {
		hcp.Spec.ReleaseImage = hcluster.Spec.Release.Image
	}

	hcp.Annotations = map[string]string{
		hostedClusterAnnotation: client.ObjectKeyFromObject(hcluster).String(),
	}
	for annotationKey := range hcluster.Annotations {
		if annotationKey == hyperv1.DisablePKIReconciliationAnnotation {
			hcp.Annotations[hyperv1.DisablePKIReconciliationAnnotation] = hcluster.Annotations[hyperv1.DisablePKIReconciliationAnnotation]
		} else if annotationKey == hyperv1.OauthLoginURLOverrideAnnotation {
			hcp.Annotations[hyperv1.OauthLoginURLOverrideAnnotation] = hcluster.Annotations[hyperv1.OauthLoginURLOverrideAnnotation]
		} else if strings.HasPrefix(annotationKey, hyperv1.IdentityProviderOverridesAnnotationPrefix) {
			hcp.Annotations[annotationKey] = hcluster.Annotations[annotationKey]
		} else if annotationKey == hyperv1.KonnectivityAgentImageAnnotation || annotationKey == hyperv1.KonnectivityServerImageAnnotation {
			hcp.Annotations[annotationKey] = hcluster.Annotations[annotationKey]
		} else if annotationKey == hyperv1.RestartDateAnnotation {
			hcp.Annotations[annotationKey] = hcluster.Annotations[annotationKey]
		} else if annotationKey == hyperv1.IBMCloudKMSProviderImage || annotationKey == hyperv1.AWSKMSProviderImage {
			hcp.Annotations[annotationKey] = hcluster.Annotations[annotationKey]
		} else if annotationKey == hyperv1.PortierisImageAnnotation {
			hcp.Annotations[hyperv1.PortierisImageAnnotation] = hcluster.Annotations[hyperv1.PortierisImageAnnotation]
		}
	}
	hcp.Spec.PullSecret = corev1.LocalObjectReference{Name: controlplaneoperator.PullSecret(hcp.Namespace).Name}
	if len(hcluster.Spec.SSHKey.Name) > 0 {
		hcp.Spec.SSHKey = corev1.LocalObjectReference{Name: controlplaneoperator.SSHKey(hcp.Namespace).Name}
	}
	if hcluster.Spec.AuditWebhook != nil && len(hcluster.Spec.AuditWebhook.Name) > 0 {
		hcp.Spec.AuditWebhook = hcluster.Spec.AuditWebhook.DeepCopy()
	}
	hcp.Spec.FIPS = hcluster.Spec.FIPS
	hcp.Spec.IssuerURL = hcluster.Spec.IssuerURL
	hcp.Spec.ServiceCIDR = hcluster.Spec.Networking.ServiceCIDR
	hcp.Spec.PodCIDR = hcluster.Spec.Networking.PodCIDR
	hcp.Spec.MachineCIDR = hcluster.Spec.Networking.MachineCIDR
	hcp.Spec.NetworkType = hcluster.Spec.Networking.NetworkType
	if hcluster.Spec.Networking.APIServer != nil {
		hcp.Spec.APIAdvertiseAddress = hcluster.Spec.Networking.APIServer.AdvertiseAddress
		hcp.Spec.APIPort = hcluster.Spec.Networking.APIServer.Port
	}

	hcp.Spec.InfraID = hcluster.Spec.InfraID
	hcp.Spec.DNS = hcluster.Spec.DNS
	hcp.Spec.Services = hcluster.Spec.Services
	hcp.Spec.ControllerAvailabilityPolicy = hcluster.Spec.ControllerAvailabilityPolicy
	hcp.Spec.InfrastructureAvailabilityPolicy = hcluster.Spec.InfrastructureAvailabilityPolicy
	hcp.Spec.Etcd.ManagementType = hcluster.Spec.Etcd.ManagementType
	if hcluster.Spec.Etcd.ManagementType == hyperv1.Unmanaged && hcluster.Spec.Etcd.Unmanaged != nil {
		hcp.Spec.Etcd.Unmanaged = hcluster.Spec.Etcd.Unmanaged.DeepCopy()
	}
	if hcluster.Spec.Etcd.ManagementType == hyperv1.Managed && hcluster.Spec.Etcd.Managed != nil {
		hcp.Spec.Etcd.Managed = hcluster.Spec.Etcd.Managed.DeepCopy()
	}
	if hcluster.Spec.ImageContentSources != nil {
		hcp.Spec.ImageContentSources = hcluster.Spec.ImageContentSources
	}
	if hcluster.Spec.SecretEncryption != nil {
		hcp.Spec.SecretEncryption = hcluster.Spec.SecretEncryption.DeepCopy()
	}
	switch hcluster.Spec.Platform.Type {
	case hyperv1.AWSPlatform:
		hcp.Spec.Platform.Type = hyperv1.AWSPlatform
		hcp.Spec.Platform.AWS = hcluster.Spec.Platform.AWS.DeepCopy()
		hcp.Spec.Platform.AWS.KubeCloudControllerCreds = corev1.LocalObjectReference{
			Name: manifests.AWSKubeCloudControllerCreds(hcp.Namespace).Name,
		}
		// TODO: Not actually used by the control plane operator...
		hcp.Spec.Platform.AWS.NodePoolManagementCreds = corev1.LocalObjectReference{
			Name: manifests.AWSNodePoolManagementCreds(hcp.Namespace).Name,
		}
	case hyperv1.NonePlatform:
		hcp.Spec.Platform.Type = hyperv1.NonePlatform
	case hyperv1.IBMCloudPlatform:
		hcp.Spec.Platform.Type = hyperv1.IBMCloudPlatform
	}

	// always reconcile the release image (facilitates rolling forward)
	hcp.Spec.ReleaseImage = hcluster.Spec.Release.Image

	hcp.Spec.Configuration = hcluster.Spec.Configuration.DeepCopy()
	return nil
}

// reconcileCAPIManager orchestrates orchestrates of  all CAPI manager components.
func (r *HostedClusterReconciler) reconcileCAPIManager(ctx context.Context, hcluster *hyperv1.HostedCluster) error {
	controlPlaneNamespace := manifests.HostedControlPlaneNamespace(hcluster.Namespace, hcluster.Name)
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(controlPlaneNamespace), controlPlaneNamespace)
	if err != nil {
		return fmt.Errorf("failed to get control plane namespace: %w", err)
	}

	// Reconcile CAPI webhooks TLS secret
	capiWebhooksTLSSecret := clusterapi.CAPIWebhooksTLSSecret(controlPlaneNamespace.Name)
	_, err = r.CreateOrUpdate(ctx, r.Client, capiWebhooksTLSSecret, func() error {
		_, hasTLSPrivateKeyKey := capiWebhooksTLSSecret.Data[corev1.TLSPrivateKeyKey]
		_, hasTLSCertKey := capiWebhooksTLSSecret.Data[corev1.TLSCertKey]
		if hasTLSPrivateKeyKey && hasTLSCertKey {
			return nil
		}

		// We currently don't expose CAPI webhooks but still they run as part of the manager
		// and it breaks without a cert https://github.com/kubernetes-sigs/cluster-api/pull/4709.
		cn := "capi-webhooks"
		ou := "openshift"
		cfg := &certs.CertCfg{
			Subject:   pkix.Name{CommonName: cn, OrganizationalUnit: []string{ou}},
			KeyUsages: x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
			Validity:  certs.ValidityTenYears,
			IsCA:      true,
		}
		key, crt, err := certs.GenerateSelfSignedCertificate(cfg)
		if err != nil {
			return fmt.Errorf("failed to generate CA (cn=%s,ou=%s): %w", cn, ou, err)
		}
		if capiWebhooksTLSSecret.Data == nil {
			capiWebhooksTLSSecret.Data = map[string][]byte{}
		}
		capiWebhooksTLSSecret.Data[corev1.TLSCertKey] = certs.CertToPem(crt)
		capiWebhooksTLSSecret.Data[corev1.TLSPrivateKeyKey] = certs.PrivateKeyToPem(key)
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile capi webhook tls secret: %w", err)
	}

	// Reconcile CAPI manager service account
	capiManagerServiceAccount := clusterapi.CAPIManagerServiceAccount(controlPlaneNamespace.Name)
	_, err = r.CreateOrUpdate(ctx, r.Client, capiManagerServiceAccount, NoopReconcile)
	if err != nil {
		return fmt.Errorf("failed to reconcile capi manager service account: %w", err)
	}

	// Reconcile CAPI manager cluster role
	capiManagerClusterRole := clusterapi.CAPIManagerClusterRole(controlPlaneNamespace.Name)
	_, err = r.CreateOrUpdate(ctx, r.Client, capiManagerClusterRole, func() error {
		return reconcileCAPIManagerClusterRole(capiManagerClusterRole)
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile capi manager cluster role: %w", err)
	}

	// Reconcile CAPI manager cluster role binding
	capiManagerClusterRoleBinding := clusterapi.CAPIManagerClusterRoleBinding(controlPlaneNamespace.Name)
	_, err = r.CreateOrUpdate(ctx, r.Client, capiManagerClusterRoleBinding, func() error {
		return reconcileCAPIManagerClusterRoleBinding(capiManagerClusterRoleBinding, capiManagerClusterRole, capiManagerServiceAccount)
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile capi manager cluster role binding: %w", err)
	}

	// Reconcile CAPI manager role
	capiManagerRole := clusterapi.CAPIManagerRole(controlPlaneNamespace.Name)
	_, err = r.CreateOrUpdate(ctx, r.Client, capiManagerRole, func() error {
		return reconcileCAPIManagerRole(capiManagerRole)
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile capi manager role: %w", err)
	}

	// Reconcile CAPI manager role binding
	capiManagerRoleBinding := clusterapi.CAPIManagerRoleBinding(controlPlaneNamespace.Name)
	_, err = r.CreateOrUpdate(ctx, r.Client, capiManagerRoleBinding, func() error {
		return reconcileCAPIManagerRoleBinding(capiManagerRoleBinding, capiManagerRole, capiManagerServiceAccount)
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile capi manager role: %w", err)
	}

	// Reconcile CAPI manager deployment
	capiImage := imageCAPI
	if _, ok := hcluster.Annotations[hyperv1.ClusterAPIManagerImage]; ok {
		capiImage = hcluster.Annotations[hyperv1.ClusterAPIManagerImage]
	}
	capiManagerDeployment := clusterapi.ClusterAPIManagerDeployment(controlPlaneNamespace.Name)
	_, err = r.CreateOrUpdate(ctx, r.Client, capiManagerDeployment, func() error {
		// TODO (alberto): This image builds from https://github.com/kubernetes-sigs/cluster-api/pull/4709
		// We need to build from main branch and push to quay.io/hypershift once this is merged or otherwise enable webhooks.
		return reconcileCAPIManagerDeployment(capiManagerDeployment, hcluster, capiManagerServiceAccount, capiImage)
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile capi manager deployment: %w", err)
	}

	return nil
}

// reconcileCAPIAWSProvider orchestrates reconciliation of the CAPI AWS provider
// components.
func (r *HostedClusterReconciler) reconcileCAPIAWSProvider(ctx context.Context, hcluster *hyperv1.HostedCluster) error {
	controlPlaneNamespace := manifests.HostedControlPlaneNamespace(hcluster.Namespace, hcluster.Name)
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(controlPlaneNamespace), controlPlaneNamespace)
	if err != nil {
		return fmt.Errorf("failed to get control plane namespace: %w", err)
	}

	// Reconcile CAPI AWS provider role
	capiAwsProviderRole := clusterapi.CAPIAWSProviderRole(controlPlaneNamespace.Name)
	_, err = r.CreateOrUpdate(ctx, r.Client, capiAwsProviderRole, func() error {
		return reconcileCAPIAWSProviderRole(capiAwsProviderRole)
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile capi aws provider role: %w", err)
	}

	// Reconcile CAPI AWS provider service account
	capiAwsProviderServiceAccount := clusterapi.CAPIAWSProviderServiceAccount(controlPlaneNamespace.Name)
	_, err = r.CreateOrUpdate(ctx, r.Client, capiAwsProviderServiceAccount, NoopReconcile)
	if err != nil {
		return fmt.Errorf("failed to reconcile capi aws provider service account: %w", err)
	}

	// Reconcile CAPI AWS provider role binding
	capiAwsProviderRoleBinding := clusterapi.CAPIAWSProviderRoleBinding(controlPlaneNamespace.Name)
	_, err = r.CreateOrUpdate(ctx, r.Client, capiAwsProviderRoleBinding, func() error {
		return reconcileCAPIAWSProviderRoleBinding(capiAwsProviderRoleBinding, capiAwsProviderRole, capiAwsProviderServiceAccount)
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile capi aws provider role binding: %w", err)
	}

	// Reconcile CAPI AWS provider deployment
	capiAwsProviderDeployment := clusterapi.CAPIAWSProviderDeployment(controlPlaneNamespace.Name)
	_, err = r.CreateOrUpdate(ctx, r.Client, capiAwsProviderDeployment, func() error {
		// TODO (alberto): This image builds from https://github.com/kubernetes-sigs/cluster-api-provider-aws/pull/2453
		// We need to build from main branch and push to quay.io/hypershift once this is merged or otherwise enable webhooks.
		return reconcileCAPIAWSProviderDeployment(capiAwsProviderDeployment, hcluster, capiAwsProviderServiceAccount)
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile capi aws provider deployment: %w", err)
	}

	return nil
}

// reconcileControlPlaneOperator orchestrates reconciliation of the control plane
// operator components.
func (r *HostedClusterReconciler) reconcileControlPlaneOperator(ctx context.Context, hcluster *hyperv1.HostedCluster, hostedControlPlane *hyperv1.HostedControlPlane) error {
	controlPlaneNamespace := manifests.HostedControlPlaneNamespace(hcluster.Namespace, hcluster.Name)
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(controlPlaneNamespace), controlPlaneNamespace)
	if err != nil {
		return fmt.Errorf("failed to get control plane namespace: %w", err)
	}

	// Reconcile operator service account
	controlPlaneOperatorServiceAccount := controlplaneoperator.OperatorServiceAccount(controlPlaneNamespace.Name)
	_, err = r.CreateOrUpdate(ctx, r.Client, controlPlaneOperatorServiceAccount, NoopReconcile)
	if err != nil {
		return fmt.Errorf("failed to reconcile controlplane operator service account: %w", err)
	}

	// Reconcile operator cluster role
	controlPlaneOperatorClusterRole := controlplaneoperator.OperatorClusterRole()
	_, err = r.CreateOrUpdate(ctx, r.Client, controlPlaneOperatorClusterRole, func() error {
		return reconcileControlPlaneOperatorClusterRole(controlPlaneOperatorClusterRole)
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile controlplane operator cluster role: %w", err)
	}

	// Reconcile operator cluster role binding
	controlPlaneOperatorClusterRoleBinding := controlplaneoperator.OperatorClusterRoleBinding(controlPlaneNamespace.Name)
	_, err = r.CreateOrUpdate(ctx, r.Client, controlPlaneOperatorClusterRoleBinding, func() error {
		return reconcileControlPlaneOperatorClusterRoleBinding(controlPlaneOperatorClusterRoleBinding, controlPlaneOperatorClusterRole, controlPlaneOperatorServiceAccount)
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile controlplane operator clusterrolebinding: %w", err)
	}

	// Reconcile operator role
	controlPlaneOperatorRole := controlplaneoperator.OperatorRole(controlPlaneNamespace.Name)
	_, err = r.CreateOrUpdate(ctx, r.Client, controlPlaneOperatorRole, func() error {
		return reconcileControlPlaneOperatorRole(controlPlaneOperatorRole)
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile controlplane operator clusterrole: %w", err)
	}

	// Reconcile operator role binding
	controlPlaneOperatorRoleBinding := controlplaneoperator.OperatorRoleBinding(controlPlaneNamespace.Name)
	_, err = r.CreateOrUpdate(ctx, r.Client, controlPlaneOperatorRoleBinding, func() error {
		return reconcileControlPlaneOperatorRoleBinding(controlPlaneOperatorRoleBinding, controlPlaneOperatorRole, controlPlaneOperatorServiceAccount)
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile controlplane operator rolebinding: %w", err)
	}

	// Reconcile operator deployment
	var pullSecret corev1.Secret
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: hcluster.Namespace, Name: hcluster.Spec.PullSecret.Name}, &pullSecret); err != nil {
		return fmt.Errorf("failed to get pull secret: %w", err)
	}
	pullSecretBytes, ok := pullSecret.Data[corev1.DockerConfigJsonKey]
	if !ok {
		return fmt.Errorf("expected %s key in pull secret", corev1.DockerConfigJsonKey)
	}
	controlPlaneOperatorImage, err := getControlPlaneOperatorImage(ctx, hcluster, r.ReleaseProvider, r.HypershiftOperatorImage, pullSecretBytes)
	if err != nil {
		return err
	}
	controlPlaneOperatorDeployment := controlplaneoperator.OperatorDeployment(controlPlaneNamespace.Name)
	_, err = r.CreateOrUpdate(ctx, r.Client, controlPlaneOperatorDeployment, func() error {
		return reconcileControlPlaneOperatorDeployment(controlPlaneOperatorDeployment, hcluster, controlPlaneOperatorImage, controlPlaneOperatorServiceAccount, r.EnableCIDebugOutput)
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile controlplane operator deployment: %w", err)
	}

	// Reconcile operator PodMonitor
	podMonitor := controlplaneoperator.PodMonitor(controlPlaneNamespace.Name, hcluster.Name)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, podMonitor, func() error {
		podMonitor.Spec.Selector = *controlPlaneOperatorDeployment.Spec.Selector
		podMonitor.Spec.PodMetricsEndpoints = []prometheusoperatorv1.PodMetricsEndpoint{{
			Interval: "15s",
			Port:     "metrics",
		}}
		podMonitor.Spec.NamespaceSelector = prometheusoperatorv1.NamespaceSelector{MatchNames: []string{controlPlaneNamespace.Name}}
		podMonitor.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: hyperv1.GroupVersion.String(),
			Kind:       "HostedControlPlane",
			Name:       hostedControlPlane.Name,
			UID:        hostedControlPlane.UID,
		}})
		if podMonitor.Annotations == nil {
			podMonitor.Annotations = map[string]string{}
		}
		podMonitor.Annotations[hostedClusterAnnotation] = client.ObjectKeyFromObject(hcluster).String()
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile controlplane operator pod monitor: %w", err)
	}

	return nil
}

func servicePublishingStrategyByType(hcp *hyperv1.HostedCluster, svcType hyperv1.ServiceType) *hyperv1.ServicePublishingStrategy {
	for _, mapping := range hcp.Spec.Services {
		if mapping.Service == svcType {
			return &mapping.ServicePublishingStrategy
		}
	}
	return nil
}

func reconcileIgnitionServerService(svc *corev1.Service, strategy *hyperv1.ServicePublishingStrategy) error {
	svc.Spec.Selector = map[string]string{
		"app": ignitionserver.ResourceName,
	}
	var portSpec corev1.ServicePort
	if len(svc.Spec.Ports) > 0 {
		portSpec = svc.Spec.Ports[0]
	} else {
		svc.Spec.Ports = []corev1.ServicePort{portSpec}
	}
	portSpec.Port = int32(443)
	portSpec.Name = "https"
	portSpec.Protocol = corev1.ProtocolTCP
	portSpec.TargetPort = intstr.FromInt(9090)
	switch strategy.Type {
	case hyperv1.NodePort:
		svc.Spec.Type = corev1.ServiceTypeNodePort
		if portSpec.NodePort == 0 && strategy.NodePort != nil {
			portSpec.NodePort = strategy.NodePort.Port
		}
	case hyperv1.Route:
		svc.Spec.Type = corev1.ServiceTypeClusterIP
	default:
		return fmt.Errorf("invalid publishing strategy for Ignition service: %s", strategy.Type)
	}
	svc.Spec.Ports[0] = portSpec
	return nil
}

func (r *HostedClusterReconciler) reconcileIgnitionServer(ctx context.Context, hcluster *hyperv1.HostedCluster, hcp *hyperv1.HostedControlPlane) error {
	var span trace.Span
	ctx, span = r.tracer.Start(ctx, "reconcile-ignition-server")
	defer span.End()

	controlPlaneNamespace := manifests.HostedControlPlaneNamespace(hcluster.Namespace, hcluster.Name)
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(controlPlaneNamespace), controlPlaneNamespace); err != nil {
		return fmt.Errorf("failed to get control plane namespace: %w", err)
	}

	serviceStrategy := servicePublishingStrategyByType(hcluster, hyperv1.Ignition)
	if serviceStrategy == nil {
		//lint:ignore ST1005 Ignition is proper name
		return fmt.Errorf("Ignition service strategy not specified")
	}
	// Reconcile service
	ignitionServerService := ignitionserver.Service(controlPlaneNamespace.Name)
	if result, err := r.CreateOrUpdate(ctx, r.Client, ignitionServerService, func() error {
		return reconcileIgnitionServerService(ignitionServerService, serviceStrategy)
	}); err != nil {
		return fmt.Errorf("failed to reconcile ignition service: %w", err)
	} else {
		span.AddEvent("reconciled ignition server service", trace.WithAttributes(attribute.String("result", string(result))))
	}
	var ignitionServerAddress string
	switch serviceStrategy.Type {
	case hyperv1.Route:
		// Reconcile route
		ignitionServerRoute := ignitionserver.Route(controlPlaneNamespace.Name)
		if result, err := r.CreateOrUpdate(ctx, r.Client, ignitionServerRoute, func() error {
			if ignitionServerRoute.Annotations == nil {
				ignitionServerRoute.Annotations = map[string]string{}
			}
			if hcluster.Spec.Platform.Type == hyperv1.AWSPlatform &&
				(hcluster.Spec.Platform.AWS.EndpointAccess == hyperv1.PublicAndPrivate ||
					hcluster.Spec.Platform.AWS.EndpointAccess == hyperv1.Private) {
				if ignitionServerRoute.Labels == nil {
					ignitionServerRoute.Labels = map[string]string{}
				}
				ignitionServerRoute.Labels[hyperutil.HypershiftRouteLabel] = controlPlaneNamespace.Name
			}
			ignitionServerRoute.Annotations[hostedClusterAnnotation] = client.ObjectKeyFromObject(hcluster).String()
			ignitionServerRoute.Spec.TLS = &routev1.TLSConfig{
				Termination: routev1.TLSTerminationPassthrough,
			}
			ignitionServerRoute.Spec.To = routev1.RouteTargetReference{
				Kind:   "Service",
				Name:   ignitionserver.ResourceName,
				Weight: k8sutilspointer.Int32Ptr(100),
			}
			return nil
		}); err != nil {
			return fmt.Errorf("failed to reconcile ignition route: %w", err)
		} else {
			span.AddEvent("reconciled ignition server route", trace.WithAttributes(attribute.String("result", string(result))))
		}

		// The route must be admitted and assigned a host before we can generate certs
		if len(ignitionServerRoute.Status.Ingress) == 0 || len(ignitionServerRoute.Status.Ingress[0].Host) == 0 {
			r.Log.Info("ignition server reconciliation waiting for ignition server route to be assigned a host value")
			return nil
		}
		ignitionServerAddress = ignitionServerRoute.Status.Ingress[0].Host
	case hyperv1.NodePort:
		if serviceStrategy.NodePort == nil {
			return fmt.Errorf("nodeport metadata not specified for ignition service")
		}
		ignitionServerAddress = serviceStrategy.NodePort.Address
	default:
		return fmt.Errorf("unknown service strategy type for ignition service: %s", serviceStrategy.Type)
	}

	// Reconcile a root CA for ignition serving certificates. We only create this
	// and don't update it for now.
	caCertSecret := ignitionserver.IgnitionCACertSecret(controlPlaneNamespace.Name)
	if result, err := r.CreateOrUpdate(ctx, r.Client, caCertSecret, func() error {
		if caCertSecret.CreationTimestamp.IsZero() {
			cfg := &certs.CertCfg{
				Subject:   pkix.Name{CommonName: "ignition-root-ca", OrganizationalUnit: []string{"openshift"}},
				KeyUsages: x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
				Validity:  certs.ValidityTenYears,
				IsCA:      true,
			}
			key, crt, err := certs.GenerateSelfSignedCertificate(cfg)
			if err != nil {
				return fmt.Errorf("failed to generate CA: %w", err)
			}
			caCertSecret.Type = corev1.SecretTypeTLS
			caCertSecret.Data = map[string][]byte{
				corev1.TLSCertKey:       certs.CertToPem(crt),
				corev1.TLSPrivateKeyKey: certs.PrivateKeyToPem(key),
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile ignition ca cert: %w", err)
	} else {
		span.AddEvent("reconciled ignition CA cert secret", trace.WithAttributes(attribute.String("result", string(result))))
		r.Log.Info("reconciled ignition CA cert secret", "result", result)
	}

	// Reconcile a ignition serving certificate issued by the generated root CA. We
	// only create this and don't update it for now.
	servingCertSecret := ignitionserver.IgnitionServingCertSecret(controlPlaneNamespace.Name)
	if result, err := r.CreateOrUpdate(ctx, r.Client, servingCertSecret, func() error {
		if servingCertSecret.CreationTimestamp.IsZero() {
			caCert, err := certs.PemToCertificate(caCertSecret.Data[corev1.TLSCertKey])
			if err != nil {
				return fmt.Errorf("couldn't get ca cert: %w", err)
			}
			caKey, err := certs.PemToPrivateKey(caCertSecret.Data[corev1.TLSPrivateKeyKey])
			if err != nil {
				return fmt.Errorf("couldn't get ca key: %w", err)
			}
			cfg := &certs.CertCfg{
				Subject:   pkix.Name{CommonName: "ignition-server", Organization: []string{"openshift"}},
				KeyUsages: x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
				Validity:  certs.ValidityOneYear,
			}
			numericIP := net.ParseIP(ignitionServerAddress)
			if numericIP == nil {
				cfg.DNSNames = []string{ignitionServerAddress}
			} else {
				cfg.IPAddresses = []net.IP{numericIP}
			}
			key, crt, err := certs.GenerateSignedCertificate(caKey, caCert, cfg)
			if err != nil {
				return fmt.Errorf("failed to generate ignition serving cert: %w", err)
			}
			servingCertSecret.Type = corev1.SecretTypeTLS
			servingCertSecret.Data = map[string][]byte{
				corev1.TLSCertKey:       certs.CertToPem(crt),
				corev1.TLSPrivateKeyKey: certs.PrivateKeyToPem(key),
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile ignition serving cert: %w", err)
	} else {
		span.AddEvent("reconciled ignition serving cert secret", trace.WithAttributes(attribute.String("result", string(result))))
		r.Log.Info("reconciled ignition serving cert secret", "result", result)
	}

	role := ignitionserver.Role(controlPlaneNamespace.Name)
	if result, err := r.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{
					"events",
					// This is needed by the tokeSecret controller to watch secrets.
					"secrets",
					// This is needed by the MCS ignitionProvider to lookup the release image and create the MCS.
					"pods/log",
					"serviceaccounts",
					"pods",
					// This is needed by the MCS ignitionProvider to create an ephemeral ConfigMap
					// with the machine config to mount it into the MCS Pod that generates the final payload.
					"configmaps",
				},
				Verbs: []string{"*"},
			},
			{
				APIGroups: []string{"rbac.authorization.k8s.io"},
				Resources: []string{"*"},
				Verbs:     []string{"*"},
			},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile ignition role: %w", err)
	} else {
		span.AddEvent("reconciled ignition server role", trace.WithAttributes(attribute.String("result", string(result))))
	}

	sa := ignitionserver.ServiceAccount(controlPlaneNamespace.Name)
	if result, err := r.CreateOrUpdate(ctx, r.Client, sa, NoopReconcile); err != nil {
		return fmt.Errorf("failed to reconcile controlplane operator service account: %w", err)
	} else {
		span.AddEvent("reconciled ignition ServiceAccount", trace.WithAttributes(attribute.String("result", string(result))))
	}

	roleBinding := ignitionserver.RoleBinding(controlPlaneNamespace.Name)
	if result, err := r.CreateOrUpdate(ctx, r.Client, roleBinding, func() error {
		roleBinding.RoleRef = rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     role.Name,
		}

		roleBinding.Subjects = []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      sa.Name,
				Namespace: sa.Namespace,
			},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile ignition RoleBinding: %w", err)
	} else {
		span.AddEvent("reconciled ignition RoleBinding", trace.WithAttributes(attribute.String("result", string(result))))
	}

	// Reconcile deployment
	ignitionServerDeployment := ignitionserver.Deployment(controlPlaneNamespace.Name)
	if result, err := r.CreateOrUpdate(ctx, r.Client, ignitionServerDeployment, func() error {
		if ignitionServerDeployment.Annotations == nil {
			ignitionServerDeployment.Annotations = map[string]string{}
		}
		ignitionServerLabels := map[string]string{
			"app":                         ignitionserver.ResourceName,
			hyperv1.ControlPlaneComponent: ignitionserver.ResourceName,
		}
		ignitionServerDeployment.Annotations[hostedClusterAnnotation] = client.ObjectKeyFromObject(hcluster).String()
		ignitionServerDeployment.Spec = appsv1.DeploymentSpec{
			Replicas: k8sutilspointer.Int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: ignitionServerLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ignitionServerLabels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:            sa.Name,
					TerminationGracePeriodSeconds: k8sutilspointer.Int64Ptr(10),
					Tolerations: []corev1.Toleration{
						{
							Key:    "node-role.kubernetes.io/master",
							Effect: corev1.TaintEffectNoSchedule,
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "serving-cert",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: servingCertSecret.Name,
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            ignitionserver.ResourceName,
							Image:           r.IgnitionServerImage,
							ImagePullPolicy: corev1.PullAlways,
							Env: []corev1.EnvVar{
								{
									Name: "MY_NAMESPACE",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "metadata.namespace",
										},
									},
								},
							},
							Command: []string{
								"/usr/bin/ignition-server",
								"start",
								"--cert-file", "/var/run/secrets/ignition/serving-cert/tls.crt",
								"--key-file", "/var/run/secrets/ignition/serving-cert/tls.key",
							},
							LivenessProbe: &corev1.Probe{
								Handler: corev1.Handler{
									TCPSocket: &corev1.TCPSocketAction{
										Port: intstr.FromInt(9090),
									},
								},
								InitialDelaySeconds: 120,
								TimeoutSeconds:      5,
								PeriodSeconds:       60,
								FailureThreshold:    6,
								SuccessThreshold:    1,
							},
							ReadinessProbe: &corev1.Probe{
								Handler: corev1.Handler{
									TCPSocket: &corev1.TCPSocketAction{
										Port: intstr.FromInt(9090),
									},
								},
								InitialDelaySeconds: 5,
								TimeoutSeconds:      5,
								PeriodSeconds:       60,
								FailureThreshold:    3,
								SuccessThreshold:    1,
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          "https",
									ContainerPort: 9090,
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("40Mi"),
									corev1.ResourceCPU:    resource.MustParse("10m"),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "serving-cert",
									MountPath: "/var/run/secrets/ignition/serving-cert",
								},
							},
						},
					},
				},
			},
		}
		hyperutil.SetColocation(hcluster, ignitionServerDeployment)
		hyperutil.SetControlPlaneIsolation(hcluster, ignitionServerDeployment)
		hyperutil.SetDefaultPriorityClass(ignitionServerDeployment)
		switch hcluster.Spec.ControllerAvailabilityPolicy {
		case hyperv1.HighlyAvailable:
			maxSurge := intstr.FromInt(1)
			maxUnavailable := intstr.FromInt(1)
			ignitionServerDeployment.Spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
			ignitionServerDeployment.Spec.Strategy.RollingUpdate = &appsv1.RollingUpdateDeployment{
				MaxSurge:       &maxSurge,
				MaxUnavailable: &maxUnavailable,
			}
			ignitionServerDeployment.Spec.Replicas = k8sutilspointer.Int32Ptr(3)
			hyperutil.SetMultizoneSpread(ignitionServerLabels, ignitionServerDeployment)
		default:
			ignitionServerDeployment.Spec.Replicas = k8sutilspointer.Int32Ptr(1)
		}

		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile ignition deployment: %w", err)
	} else {
		span.AddEvent("reconciled ignition server deployment", trace.WithAttributes(attribute.String("result", string(result))))
	}

	return nil
}

// reconcileAutoscaler orchestrates reconciliation of autoscaler components using
// both the HostedCluster and the HostedControlPlane which the autoscaler takes
// inputs from.
func (r *HostedClusterReconciler) reconcileAutoscaler(ctx context.Context, hcluster *hyperv1.HostedCluster, hcp *hyperv1.HostedControlPlane) error {
	controlPlaneNamespace := manifests.HostedControlPlaneNamespace(hcluster.Namespace, hcluster.Name)
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(controlPlaneNamespace), controlPlaneNamespace)
	if err != nil {
		return fmt.Errorf("failed to get control plane namespace: %w", err)
	}

	// Reconcile autoscaler role
	autoScalerRole := autoscaler.AutoScalerRole(controlPlaneNamespace.Name)
	_, err = r.CreateOrUpdate(ctx, r.Client, autoScalerRole, func() error {
		return reconcileAutoScalerRole(autoScalerRole)
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile autoscaler role: %w", err)
	}

	// Reconcile autoscaler service account
	autoScalerServiceAccount := autoscaler.AutoScalerServiceAccount(controlPlaneNamespace.Name)
	_, err = r.CreateOrUpdate(ctx, r.Client, autoScalerServiceAccount, NoopReconcile)
	if err != nil {
		return fmt.Errorf("failed to reconcile autoscaler service account: %w", err)
	}

	// Reconcile autoscaler role binding
	autoScalerRoleBinding := autoscaler.AutoScalerRoleBinding(controlPlaneNamespace.Name)
	_, err = r.CreateOrUpdate(ctx, r.Client, autoScalerRoleBinding, func() error {
		return reconcileAutoScalerRoleBinding(autoScalerRoleBinding, autoScalerRole, autoScalerServiceAccount)
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile autoscaler role binding: %w", err)
	}

	// The deployment depends on the kubeconfig being reported.
	if hcp.Status.KubeConfig != nil {
		// Resolve the kubeconfig secret for CAPI which the
		// autoscaler is deployed alongside of.
		capiKubeConfigSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: hcp.Namespace,
				Name:      fmt.Sprintf("%s-kubeconfig", hcluster.Spec.InfraID),
			},
		}
		err = r.Client.Get(ctx, client.ObjectKeyFromObject(capiKubeConfigSecret), capiKubeConfigSecret)
		if err != nil {
			return fmt.Errorf("failed to get hosted controlplane kubeconfig secret %q: %w", capiKubeConfigSecret.Name, err)
		}

		// Reconcile autoscaler deployment
		clusterAutoScalerImage := imageClusterAutoscaler
		if _, ok := hcluster.Annotations[hyperv1.ClusterAutoscalerImage]; ok {
			clusterAutoScalerImage = hcluster.Annotations[hyperv1.ClusterAutoscalerImage]
		}
		autoScalerDeployment := autoscaler.AutoScalerDeployment(controlPlaneNamespace.Name)
		_, err = r.CreateOrUpdate(ctx, r.Client, autoScalerDeployment, func() error {
			return reconcileAutoScalerDeployment(autoScalerDeployment, hcluster, autoScalerServiceAccount, capiKubeConfigSecret, hcluster.Spec.Autoscaling, clusterAutoScalerImage, r.HypershiftOperatorImage)
		})
		if err != nil {
			return fmt.Errorf("failed to reconcile autoscaler deployment: %w", err)
		}
	}

	return nil
}

func getControlPlaneOperatorImage(ctx context.Context, hc *hyperv1.HostedCluster, releaseProvider releaseinfo.Provider, hypershiftOperatorImage string, pullSecret []byte) (string, error) {
	if val, ok := hc.Annotations[hyperv1.ControlPlaneOperatorImageAnnotation]; ok {
		return val, nil
	}
	releaseInfo, err := releaseProvider.Lookup(ctx, hc.Spec.Release.Image, pullSecret)
	if err != nil {
		return "", err
	}
	version, err := semver.Parse(releaseInfo.Version())
	if err != nil {
		return "", err
	}
	versionMajMin := fmt.Sprintf("%d.%d", version.Major, version.Minor)
	pullSpec := "registry.ci.openshift.org/hypershift/hypershift"
	switch versionMajMin {
	case "4.9", "4.10":
		return hypershiftOperatorImage, nil
	case "4.8":
		return fmt.Sprintf("%s:%s", pullSpec, versionMajMin), nil
	default:
		return "", fmt.Errorf("unsupported release image with version %s", versionMajMin)
	}
}

func reconcileControlPlaneOperatorDeployment(deployment *appsv1.Deployment, hc *hyperv1.HostedCluster, image string, sa *corev1.ServiceAccount, enableCIDebugOutput bool) error {
	deployment.Spec = appsv1.DeploymentSpec{
		Replicas: k8sutilspointer.Int32Ptr(1),
		Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"name": "control-plane-operator",
			},
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"name":                        "control-plane-operator",
					"app":                         "control-plane-operator",
					hyperv1.ControlPlaneComponent: "control-plane-operator",
				},
			},
			Spec: corev1.PodSpec{
				ServiceAccountName: sa.Name,
				Containers: []corev1.Container{
					{
						Name:            "control-plane-operator",
						Image:           image,
						ImagePullPolicy: corev1.PullAlways,
						Env: []corev1.EnvVar{
							{
								Name: "MY_NAMESPACE",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										FieldPath: "metadata.namespace",
									},
								},
							},
						},
						// needed since control plane operator runs with anyuuid scc
						SecurityContext: &corev1.SecurityContext{
							RunAsUser: k8sutilspointer.Int64Ptr(1000),
						},
						Command: []string{"/usr/bin/control-plane-operator"},
						Args:    []string{"run", "--namespace", "$(MY_NAMESPACE)", "--deployment-name", "control-plane-operator", "--metrics-addr", "0.0.0.0:8080", fmt.Sprintf("--enable-ci-debug-output=%t", enableCIDebugOutput)},
						Ports:   []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8080}},
					},
				},
			},
		},
	}
	hyperutil.SetColocation(hc, deployment)
	hyperutil.SetRestartAnnotation(hc, deployment)
	hyperutil.SetControlPlaneIsolation(hc, deployment)
	hyperutil.SetDefaultPriorityClass(deployment)
	return nil
}

func reconcileControlPlaneOperatorClusterRole(role *rbacv1.ClusterRole) error {
	role.Rules = []rbacv1.PolicyRule{
		{
			APIGroups: []string{"apiextensions.k8s.io"},
			Resources: []string{"customresourcedefinitions"},
			Verbs:     []string{"*"},
		},
		{
			APIGroups: []string{"config.openshift.io"},
			Resources: []string{"*"},
			Verbs:     []string{"get", "list", "watch"},
		},
		{
			APIGroups: []string{"operator.openshift.io"},
			Resources: []string{"*"},
			Verbs:     []string{"*"},
		},
		{
			APIGroups: []string{"security.openshift.io"},
			Resources: []string{"securitycontextconstraints"},
			Verbs:     []string{"*"},
		},
		{
			APIGroups: []string{"rbac.authorization.k8s.io"},
			Resources: []string{"*"},
			Verbs:     []string{"*"},
		},
	}
	return nil
}

func reconcileControlPlaneOperatorClusterRoleBinding(binding *rbacv1.ClusterRoleBinding, role *rbacv1.ClusterRole, sa *corev1.ServiceAccount) error {
	binding.RoleRef = rbacv1.RoleRef{
		APIGroup: "rbac.authorization.k8s.io",
		Kind:     "ClusterRole",
		Name:     role.Name,
	}
	binding.Subjects = []rbacv1.Subject{
		{
			Kind:      "ServiceAccount",
			Name:      sa.Name,
			Namespace: sa.Namespace,
		},
	}
	return nil
}

func reconcileControlPlaneOperatorRole(role *rbacv1.Role) error {
	role.Rules = []rbacv1.PolicyRule{
		{
			APIGroups: []string{"hypershift.openshift.io"},
			Resources: []string{"*"},
			Verbs:     []string{"*"},
		},
		{
			APIGroups: []string{
				"bootstrap.cluster.x-k8s.io",
				"controlplane.cluster.x-k8s.io",
				"infrastructure.cluster.x-k8s.io",
				"machines.cluster.x-k8s.io",
				"exp.infrastructure.cluster.x-k8s.io",
				"addons.cluster.x-k8s.io",
				"exp.cluster.x-k8s.io",
				"cluster.x-k8s.io",
				"monitoring.coreos.com",
			},
			Resources: []string{"*"},
			Verbs:     []string{"*"},
		},
		{
			APIGroups: []string{"route.openshift.io"},
			Resources: []string{"*"},
			Verbs:     []string{"*"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{
				"events",
				"configmaps",
				"pods",
				"pods/log",
				"secrets",
				"nodes",
				"serviceaccounts",
				"services",
			},
			Verbs: []string{"*"},
		},
		{
			APIGroups: []string{"apps"},
			Resources: []string{"deployments", "statefulsets"},
			Verbs:     []string{"*"},
		},
		{
			APIGroups: []string{"etcd.database.coreos.com"},
			Resources: []string{"*"},
			Verbs:     []string{"*"},
		},
		{
			APIGroups: []string{"machine.openshift.io"},
			Resources: []string{"*"},
			Verbs:     []string{"*"},
		},
		{
			APIGroups: []string{"batch"},
			Resources: []string{"cronjobs", "jobs"},
			Verbs:     []string{"*"},
		},
	}
	return nil
}

func reconcileControlPlaneOperatorRoleBinding(binding *rbacv1.RoleBinding, role *rbacv1.Role, sa *corev1.ServiceAccount) error {
	binding.RoleRef = rbacv1.RoleRef{
		APIGroup: "rbac.authorization.k8s.io",
		Kind:     "Role",
		Name:     role.Name,
	}

	binding.Subjects = []rbacv1.Subject{
		{
			Kind:      "ServiceAccount",
			Name:      sa.Name,
			Namespace: sa.Namespace,
		},
	}

	return nil
}

func reconcileAWSCluster(awsCluster *capiawsv1.AWSCluster, hcluster *hyperv1.HostedCluster, apiEndpoint hyperv1.APIEndpoint) error {
	// We only create this resource once and then let CAPI own it
	awsCluster.Annotations = map[string]string{
		hostedClusterAnnotation:    client.ObjectKeyFromObject(hcluster).String(),
		capiv1.ManagedByAnnotation: "external",
	}

	awsCluster.Spec.AdditionalTags = nil
	if hcluster.Spec.Platform.AWS != nil {
		awsCluster.Spec.Region = hcluster.Spec.Platform.AWS.Region

		if hcluster.Spec.Platform.AWS.CloudProviderConfig != nil {
			awsCluster.Spec.NetworkSpec.VPC.ID = hcluster.Spec.Platform.AWS.CloudProviderConfig.VPC
		}

		if len(hcluster.Spec.Platform.AWS.ResourceTags) > 0 {
			awsCluster.Spec.AdditionalTags = capiawsv1.Tags{}
		}
		for _, entry := range hcluster.Spec.Platform.AWS.ResourceTags {
			awsCluster.Spec.AdditionalTags[entry.Key] = entry.Value
		}
	}

	// Set the values for upper level controller
	awsCluster.Status.Ready = true
	awsCluster.Spec.ControlPlaneEndpoint = capiv1alpha4.APIEndpoint{
		Host: apiEndpoint.Host,
		Port: apiEndpoint.Port,
	}
	return nil
}

func reconcileIBMCloudCluster(ibmCluster *capiibmv1.IBMVPCCluster, hcluster *hyperv1.HostedCluster, apiEndpoint hyperv1.APIEndpoint) error {
	ibmCluster.Annotations = map[string]string{
		hostedClusterAnnotation:    client.ObjectKeyFromObject(hcluster).String(),
		capiv1.ManagedByAnnotation: "external",
	}

	// Set the values for upper level controller
	ibmCluster.Status.Ready = true
	ibmCluster.Spec.ControlPlaneEndpoint = capiv1alpha4.APIEndpoint{
		Host: apiEndpoint.Host,
		Port: apiEndpoint.Port,
	}
	return nil
}

func reconcileCAPICluster(cluster *capiv1.Cluster, hcluster *hyperv1.HostedCluster, hcp *hyperv1.HostedControlPlane, infraCR client.Object) error {
	// We only create this resource once and then let CAPI own it
	if !cluster.CreationTimestamp.IsZero() {
		return nil
	}

	cluster.Annotations = map[string]string{
		hostedClusterAnnotation: client.ObjectKeyFromObject(hcluster).String(),
	}
	gvk, err := apiutil.GVKForObject(infraCR, api.Scheme)
	if err != nil {
		return err
	}
	cluster.Spec = capiv1.ClusterSpec{
		ControlPlaneEndpoint: capiv1.APIEndpoint{},
		ControlPlaneRef: &corev1.ObjectReference{
			APIVersion: "hypershift.openshift.io/v1alpha1",
			Kind:       "HostedControlPlane",
			Namespace:  hcp.Namespace,
			Name:       hcp.Name,
		},
		InfrastructureRef: &corev1.ObjectReference{
			APIVersion: gvk.GroupVersion().String(),
			Kind:       gvk.Kind,
			Namespace:  infraCR.GetNamespace(),
			Name:       infraCR.GetName(),
		},
	}

	return nil
}

func reconcileCAPIManagerDeployment(deployment *appsv1.Deployment, hc *hyperv1.HostedCluster, sa *corev1.ServiceAccount, capiManagerImage string) error {
	defaultMode := int32(420)
	capiManagerLabels := map[string]string{
		"name":                        "cluster-api",
		"app":                         "cluster-api",
		hyperv1.ControlPlaneComponent: "cluster-api",
	}
	deployment.Spec = appsv1.DeploymentSpec{
		Replicas: k8sutilspointer.Int32Ptr(1),
		Selector: &metav1.LabelSelector{
			MatchLabels: capiManagerLabels,
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: capiManagerLabels,
			},
			Spec: corev1.PodSpec{
				ServiceAccountName: sa.Name,
				Volumes: []corev1.Volume{
					{
						Name: "capi-webhooks-tls",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								DefaultMode: &defaultMode,
								SecretName:  "capi-webhooks-tls",
							},
						},
					},
				},
				Containers: []corev1.Container{
					{
						Name:            "manager",
						Image:           capiManagerImage,
						ImagePullPolicy: corev1.PullAlways,
						Env: []corev1.EnvVar{
							{
								Name: "MY_NAMESPACE",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										FieldPath: "metadata.namespace",
									},
								},
							},
						},
						Command: []string{"/manager"},
						Args: []string{"--namespace", "$(MY_NAMESPACE)",
							"--alsologtostderr",
							"--v=4",
							"--leader-elect=true",
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("20Mi"),
								corev1.ResourceCPU:    resource.MustParse("10m"),
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "capi-webhooks-tls",
								ReadOnly:  true,
								MountPath: "/tmp/k8s-webhook-server/serving-certs",
							},
						},
					},
				},
			},
		},
	}
	hyperutil.SetColocation(hc, deployment)
	// TODO (alberto): Reconsider enable this back when we face a real need
	// with no better solution.
	// hyperutil.SetRestartAnnotation(hc, deployment)
	hyperutil.SetControlPlaneIsolation(hc, deployment)
	hyperutil.SetDefaultPriorityClass(deployment)
	switch hc.Spec.ControllerAvailabilityPolicy {
	case hyperv1.HighlyAvailable:
		maxSurge := intstr.FromInt(1)
		maxUnavailable := intstr.FromInt(1)
		deployment.Spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
		deployment.Spec.Strategy.RollingUpdate = &appsv1.RollingUpdateDeployment{
			MaxSurge:       &maxSurge,
			MaxUnavailable: &maxUnavailable,
		}
		deployment.Spec.Replicas = k8sutilspointer.Int32Ptr(3)
		hyperutil.SetMultizoneSpread(capiManagerLabels, deployment)
	default:
		deployment.Spec.Replicas = k8sutilspointer.Int32Ptr(1)
	}

	return nil
}

func reconcileCAPIManagerClusterRole(role *rbacv1.ClusterRole) error {
	role.Rules = []rbacv1.PolicyRule{
		{
			APIGroups: []string{"apiextensions.k8s.io"},
			Resources: []string{"customresourcedefinitions"},
			Verbs:     []string{"get", "list", "watch"},
		},
	}
	return nil
}

func reconcileCAPIManagerClusterRoleBinding(binding *rbacv1.ClusterRoleBinding, role *rbacv1.ClusterRole, sa *corev1.ServiceAccount) error {
	binding.RoleRef = rbacv1.RoleRef{
		APIGroup: "rbac.authorization.k8s.io",
		Kind:     "ClusterRole",
		Name:     role.Name,
	}

	binding.Subjects = []rbacv1.Subject{
		{
			Kind:      "ServiceAccount",
			Name:      sa.Name,
			Namespace: sa.Namespace,
		},
	}
	return nil
}

func reconcileCAPIManagerRole(role *rbacv1.Role) error {
	role.Rules = []rbacv1.PolicyRule{
		{
			APIGroups: []string{
				"bootstrap.cluster.x-k8s.io",
				"controlplane.cluster.x-k8s.io",
				"infrastructure.cluster.x-k8s.io",
				"machines.cluster.x-k8s.io",
				"exp.infrastructure.cluster.x-k8s.io",
				"addons.cluster.x-k8s.io",
				"exp.cluster.x-k8s.io",
				"cluster.x-k8s.io",
			},
			Resources: []string{"*"},
			Verbs:     []string{"*"},
		},
		{
			APIGroups: []string{"hypershift.openshift.io"},
			Resources: []string{
				"hostedcontrolplanes",
				"hostedcontrolplanes/status",
			},
			Verbs: []string{"*"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{
				"configmaps",
				"events",
				"nodes",
				"secrets",
			},
			Verbs: []string{"*"},
		},
		{
			APIGroups: []string{"coordination.k8s.io"},
			Resources: []string{
				"leases",
			},
			Verbs: []string{"*"},
		},
	}
	return nil
}

func reconcileCAPIManagerRoleBinding(binding *rbacv1.RoleBinding, role *rbacv1.Role, sa *corev1.ServiceAccount) error {
	binding.RoleRef = rbacv1.RoleRef{
		APIGroup: "rbac.authorization.k8s.io",
		Kind:     "Role",
		Name:     role.Name,
	}

	binding.Subjects = []rbacv1.Subject{
		{
			Kind:      "ServiceAccount",
			Name:      sa.Name,
			Namespace: sa.Namespace,
		},
	}

	return nil
}

func reconcileCAPIAWSProviderDeployment(deployment *appsv1.Deployment, hc *hyperv1.HostedCluster, sa *corev1.ServiceAccount) error {
	defaultMode := int32(420)
	capaLabels := map[string]string{
		"control-plane":               "capa-controller-manager",
		"app":                         "capa-controller-manager",
		hyperv1.ControlPlaneComponent: "capa-controller-manager",
	}
	deployment.Spec = appsv1.DeploymentSpec{
		Replicas: k8sutilspointer.Int32Ptr(1),
		Selector: &metav1.LabelSelector{
			MatchLabels: capaLabels,
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: capaLabels,
			},
			Spec: corev1.PodSpec{
				ServiceAccountName:            sa.Name,
				TerminationGracePeriodSeconds: k8sutilspointer.Int64Ptr(10),
				Tolerations: []corev1.Toleration{
					{
						Key:    "node-role.kubernetes.io/master",
						Effect: corev1.TaintEffectNoSchedule,
					},
				},
				Volumes: []corev1.Volume{
					{
						Name: "capi-webhooks-tls",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								DefaultMode: &defaultMode,
								SecretName:  "capi-webhooks-tls",
							},
						},
					},
					{
						Name: "credentials",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName: manifests.AWSNodePoolManagementCreds(deployment.Namespace).Name,
							},
						},
					},
				},
				Containers: []corev1.Container{
					{
						Name:            "manager",
						Image:           imageCAPA,
						ImagePullPolicy: corev1.PullAlways,
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "credentials",
								MountPath: "/home/.aws",
							},
							{
								Name:      "capi-webhooks-tls",
								ReadOnly:  true,
								MountPath: "/tmp/k8s-webhook-server/serving-certs",
							},
						},
						Env: []corev1.EnvVar{
							{
								Name: "MY_NAMESPACE",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										FieldPath: "metadata.namespace",
									},
								},
							},
							{
								Name:  "AWS_SHARED_CREDENTIALS_FILE",
								Value: "/home/.aws/credentials",
							},
						},
						Command: []string{"/manager"},
						Args: []string{"--namespace", "$(MY_NAMESPACE)",
							"--alsologtostderr",
							"--v=4",
							"--leader-elect=true",
							"--feature-gates=EKS=false",
						},
						Ports: []corev1.ContainerPort{
							{
								Name:          "healthz",
								ContainerPort: 9440,
								Protocol:      corev1.ProtocolTCP,
							},
						},
						LivenessProbe: &corev1.Probe{
							Handler: corev1.Handler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.FromString("healthz"),
								},
							},
						},
						ReadinessProbe: &corev1.Probe{
							Handler: corev1.Handler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/readyz",
									Port: intstr.FromString("healthz"),
								},
							},
						},
					},
				},
			},
		},
	}
	hyperutil.SetColocation(hc, deployment)
	// TODO (alberto): Reconsider enable this back when we face a real need
	// with no better solution.
	// hyperutil.SetRestartAnnotation(hc, deployment)
	hyperutil.SetControlPlaneIsolation(hc, deployment)
	hyperutil.SetDefaultPriorityClass(deployment)
	switch hc.Spec.ControllerAvailabilityPolicy {
	case hyperv1.HighlyAvailable:
		maxSurge := intstr.FromInt(1)
		maxUnavailable := intstr.FromInt(1)
		deployment.Spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
		deployment.Spec.Strategy.RollingUpdate = &appsv1.RollingUpdateDeployment{
			MaxSurge:       &maxSurge,
			MaxUnavailable: &maxUnavailable,
		}
		deployment.Spec.Replicas = k8sutilspointer.Int32Ptr(3)
		hyperutil.SetMultizoneSpread(capaLabels, deployment)
	default:
		deployment.Spec.Replicas = k8sutilspointer.Int32Ptr(1)
	}

	return nil
}

func reconcileCAPIAWSProviderRole(role *rbacv1.Role) error {
	role.Rules = []rbacv1.PolicyRule{
		{
			APIGroups: []string{""},
			Resources: []string{
				"events",
				"secrets",
				"configmaps",
			},
			Verbs: []string{"*"},
		},
		{
			APIGroups: []string{
				"bootstrap.cluster.x-k8s.io",
				"controlplane.cluster.x-k8s.io",
				"infrastructure.cluster.x-k8s.io",
				"machines.cluster.x-k8s.io",
				"exp.infrastructure.cluster.x-k8s.io",
				"addons.cluster.x-k8s.io",
				"exp.cluster.x-k8s.io",
				"cluster.x-k8s.io",
			},
			Resources: []string{"*"},
			Verbs:     []string{"*"},
		},
		{
			APIGroups: []string{"hypershift.openshift.io"},
			Resources: []string{"*"},
			Verbs:     []string{"*"},
		},
		{
			APIGroups: []string{"coordination.k8s.io"},
			Resources: []string{
				"leases",
			},
			Verbs: []string{"*"},
		},
	}
	return nil
}

func reconcileCAPIAWSProviderRoleBinding(binding *rbacv1.RoleBinding, role *rbacv1.Role, sa *corev1.ServiceAccount) error {
	binding.RoleRef = rbacv1.RoleRef{
		APIGroup: "rbac.authorization.k8s.io",
		Kind:     "Role",
		Name:     role.Name,
	}

	binding.Subjects = []rbacv1.Subject{
		{
			Kind:      "ServiceAccount",
			Name:      sa.Name,
			Namespace: sa.Namespace,
		},
	}
	return nil
}

func reconcileAutoScalerDeployment(deployment *appsv1.Deployment, hc *hyperv1.HostedCluster, sa *corev1.ServiceAccount, kubeConfigSecret *corev1.Secret, options hyperv1.ClusterAutoscaling, clusterAutoScalerImage string, availabilityProberImage string) error {
	args := []string{
		"--cloud-provider=clusterapi",
		"--node-group-auto-discovery=clusterapi:namespace=$(MY_NAMESPACE)",
		"--kubeconfig=/mnt/kubeconfig/target-kubeconfig",
		"--clusterapi-cloud-config-authoritative",
		// TODO (alberto): Is this a fair assumption?
		// There's currently pods with local storage e.g grafana and image-registry.
		// Without this option after after a scaling out operation and an “unfortunate” reschedule
		// we might end up locked with three nodes.
		"--skip-nodes-with-local-storage=false",
		"--alsologtostderr",
		"--v=4",
	}

	// TODO if the options for the cluster autoscaler continues to grow, we should take inspiration
	// from the cluster-autoscaler-operator and create some utility functions for these assignments.
	if options.MaxNodesTotal != nil {
		arg := fmt.Sprintf("%s=%d", "--max-nodes-total", *options.MaxNodesTotal)
		args = append(args, arg)
	}

	if options.MaxPodGracePeriod != nil {
		arg := fmt.Sprintf("%s=%d", "--max-graceful-termination-sec", *options.MaxPodGracePeriod)
		args = append(args, arg)
	}

	if options.MaxNodeProvisionTime != "" {
		arg := fmt.Sprintf("%s=%s", "--max-node-provision-time", options.MaxNodeProvisionTime)
		args = append(args, arg)
	}

	if options.PodPriorityThreshold != nil {
		arg := fmt.Sprintf("%s=%d", "--expendable-pods-priority-cutoff", *options.PodPriorityThreshold)
		args = append(args, arg)
	}

	deployment.Spec = appsv1.DeploymentSpec{
		Replicas: k8sutilspointer.Int32Ptr(1),
		Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"app": "cluster-autoscaler",
			},
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"app":                         "cluster-autoscaler",
					hyperv1.ControlPlaneComponent: "cluster-autoscaler",
				},
			},
			Spec: corev1.PodSpec{
				ServiceAccountName:            sa.Name,
				TerminationGracePeriodSeconds: k8sutilspointer.Int64Ptr(10),
				Tolerations: []corev1.Toleration{
					{
						Key:    "node-role.kubernetes.io/master",
						Effect: corev1.TaintEffectNoSchedule,
					},
				},
				Volumes: []corev1.Volume{
					{
						Name: "target-kubeconfig",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName: kubeConfigSecret.Name,
								Items: []corev1.KeyToPath{
									{
										// TODO: should the key be published on status?
										Key:  "value",
										Path: "target-kubeconfig",
									},
								},
							},
						},
					},
				},
				Containers: []corev1.Container{
					{
						Name:            "cluster-autoscaler",
						Image:           clusterAutoScalerImage,
						ImagePullPolicy: corev1.PullAlways,
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "target-kubeconfig",
								MountPath: "/mnt/kubeconfig",
							},
						},
						Env: []corev1.EnvVar{
							{
								Name: "MY_NAMESPACE",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										FieldPath: "metadata.namespace",
									},
								},
							},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("35Mi"),
								corev1.ResourceCPU:    resource.MustParse("10m"),
							},
						},
						Command: []string{"/cluster-autoscaler"},
						Args:    args,
					},
				},
			},
		},
	}

	util.AvailabilityProber(kas.InClusterKASReadyURL(deployment.Namespace), availabilityProberImage, &deployment.Spec.Template.Spec)

	hyperutil.SetColocation(hc, deployment)
	hyperutil.SetRestartAnnotation(hc, deployment)
	hyperutil.SetControlPlaneIsolation(hc, deployment)
	hyperutil.SetDefaultPriorityClass(deployment)
	return nil
}

func reconcileAutoScalerRole(role *rbacv1.Role) error {
	role.Rules = []rbacv1.PolicyRule{
		{
			APIGroups: []string{"apiextensions.k8s.io"},
			Resources: []string{"customresourcedefinitions"},
			Verbs:     []string{"get", "list", "watch"},
		},
		{
			APIGroups: []string{"cluster.x-k8s.io"},
			Resources: []string{
				"machinedeployments",
				"machinedeployments/scale",
				"machines",
				"machinesets",
				"machinesets/scale",
			},
			Verbs: []string{"*"},
		},
	}
	return nil
}

func reconcileAutoScalerRoleBinding(binding *rbacv1.RoleBinding, role *rbacv1.Role, sa *corev1.ServiceAccount) error {
	binding.RoleRef = rbacv1.RoleRef{
		APIGroup: "rbac.authorization.k8s.io",
		Kind:     "Role",
		Name:     role.Name,
	}

	binding.Subjects = []rbacv1.Subject{
		{
			Kind:      "ServiceAccount",
			Name:      sa.Name,
			Namespace: sa.Namespace,
		},
	}

	return nil
}

// computeClusterVersionStatus determines the ClusterVersionStatus of the
// given HostedCluster and returns it.
func computeClusterVersionStatus(clock clock.Clock, hcluster *hyperv1.HostedCluster, hcp *hyperv1.HostedControlPlane) *hyperv1.ClusterVersionStatus {
	// If there's no history, rebuild it from scratch.
	if hcluster.Status.Version == nil || len(hcluster.Status.Version.History) == 0 {
		return &hyperv1.ClusterVersionStatus{
			Desired:            hcluster.Spec.Release,
			ObservedGeneration: hcluster.Generation,
			History: []configv1.UpdateHistory{
				{
					State:       configv1.PartialUpdate,
					Image:       hcluster.Spec.Release.Image,
					StartedTime: metav1.NewTime(clock.Now()),
				},
			},
		}
	}

	// Reconcile the current version with the latest resource states.
	version := hcluster.Status.Version.DeepCopy()

	// If the hosted control plane doesn't exist, there's no way to assess the
	// rollout so return early.
	if hcp == nil {
		return version
	}

	// If a rollout is in progress, we need to wait before updating.
	// TODO: This is a potentially weak check. Conditions checks don't seem
	// quite right because the intent here is to identify a terminal rollout
	// state. For now it assumes when status.releaseImage matches, that rollout
	// is definitely done.
	hcpRolloutComplete := (hcp.Spec.ReleaseImage == hcp.Status.ReleaseImage) && (version.Desired.Image == hcp.Status.ReleaseImage)
	if !hcpRolloutComplete {
		return version
	}

	// The rollout is complete, so update the current history entry
	version.History[0].State = configv1.CompletedUpdate
	version.History[0].Version = hcp.Status.Version
	if hcp.Status.LastReleaseImageTransitionTime != nil {
		version.History[0].CompletionTime = hcp.Status.LastReleaseImageTransitionTime.DeepCopy()
	}

	// If a new rollout is needed, update the desired version and prepend a new
	// partial history entry to unblock rollouts.
	rolloutNeeded := hcluster.Spec.Release.Image != hcluster.Status.Version.Desired.Image
	if rolloutNeeded {
		version.Desired.Image = hcluster.Spec.Release.Image
		version.ObservedGeneration = hcluster.Generation
		// TODO: leaky
		version.History = append([]configv1.UpdateHistory{
			{
				State:       configv1.PartialUpdate,
				Image:       hcluster.Spec.Release.Image,
				StartedTime: metav1.NewTime(clock.Now()),
			},
		}, version.History...)
	}

	return version
}

// computeHostedClusterAvailability determines the Available condition for the
// given HostedCluster and returns it.
func computeHostedClusterAvailability(hcluster *hyperv1.HostedCluster, hcp *hyperv1.HostedControlPlane) metav1.Condition {
	// Determine whether the hosted control plane is available.
	hcpAvailable := false
	if hcp != nil {
		hcpAvailable = meta.IsStatusConditionTrue(hcp.Status.Conditions, string(hyperv1.HostedControlPlaneAvailable))
	}

	// Determine whether the kubeconfig is available.
	// TODO: is it a good idea to compute hc status based on other field within
	// the same resource like this? does it imply an ordering requirement that
	// kubeconfig status must come before availability status? would extracting
	// the kubeconfig as an argument help by making that dependency explicit?
	kubeConfigAvailable := hcluster.Status.KubeConfig != nil

	// Managed etcd availability isn't reported at this granularity yet, so always
	// assume managed etcd is available. If etcd is configured as unmanaged, consider
	// etcd available once the unmanaged available condition is true.
	etcdAvailable := hcluster.Spec.Etcd.ManagementType == hyperv1.Managed ||
		meta.IsStatusConditionTrue(hcluster.Status.Conditions, string(hyperv1.UnmanagedEtcdAvailable))

	switch {
	case hcpAvailable && kubeConfigAvailable && etcdAvailable:
		return metav1.Condition{
			Type:               string(hyperv1.HostedClusterAvailable),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: hcluster.Generation,
			Reason:             hyperv1.HostedClusterAsExpectedReason,
		}
	default:
		var messages []string
		if !hcpAvailable {
			messages = append(messages, "the hosted control plane is unavailable")
		}
		if !kubeConfigAvailable {
			messages = append(messages, "the hosted control plane kubeconfig is unavailable")
		}
		if !etcdAvailable {
			messages = append(messages, "etcd is unavailable")
		}
		return metav1.Condition{
			Type:               string(hyperv1.HostedClusterAvailable),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: hcluster.Generation,
			Reason:             hyperv1.HostedClusterUnhealthyComponentsReason,
			Message:            strings.Join(messages, "; "),
		}
	}
}

// computeUnmanagedEtcdAvailability calculates the current status of unmanaged etcd.
func computeUnmanagedEtcdAvailability(hcluster *hyperv1.HostedCluster, unmanagedEtcdTLSClientSecret *corev1.Secret) metav1.Condition {
	if unmanagedEtcdTLSClientSecret == nil {
		return metav1.Condition{
			Type:    string(hyperv1.UnmanagedEtcdAvailable),
			Status:  metav1.ConditionFalse,
			Reason:  hyperv1.UnmanagedEtcdMisconfiguredReason,
			Message: fmt.Sprintf("missing TLS client secret %s", hcluster.Spec.Etcd.Unmanaged.TLS.ClientSecret.Name),
		}
	}
	if hcluster.Spec.Etcd.Unmanaged == nil || len(hcluster.Spec.Etcd.Unmanaged.TLS.ClientSecret.Name) == 0 || len(hcluster.Spec.Etcd.Unmanaged.Endpoint) == 0 {
		return metav1.Condition{
			Type:    string(hyperv1.UnmanagedEtcdAvailable),
			Status:  metav1.ConditionFalse,
			Reason:  hyperv1.UnmanagedEtcdMisconfiguredReason,
			Message: "etcd metadata not specified for unmanaged deployment",
		}
	}
	if _, ok := unmanagedEtcdTLSClientSecret.Data["etcd-client.crt"]; !ok {
		return metav1.Condition{
			Type:    string(hyperv1.UnmanagedEtcdAvailable),
			Status:  metav1.ConditionFalse,
			Reason:  hyperv1.UnmanagedEtcdMisconfiguredReason,
			Message: fmt.Sprintf("etcd secret %s does not have client cert", hcluster.Spec.Etcd.Unmanaged.TLS.ClientSecret.Name),
		}
	}
	if _, ok := unmanagedEtcdTLSClientSecret.Data["etcd-client.key"]; !ok {
		return metav1.Condition{
			Type:    string(hyperv1.UnmanagedEtcdAvailable),
			Status:  metav1.ConditionFalse,
			Reason:  hyperv1.UnmanagedEtcdMisconfiguredReason,
			Message: fmt.Sprintf("etcd secret %s does not have client key", hcluster.Spec.Etcd.Unmanaged.TLS.ClientSecret.Name),
		}
	}
	if _, ok := unmanagedEtcdTLSClientSecret.Data["etcd-client-ca.crt"]; !ok {
		return metav1.Condition{
			Type:    string(hyperv1.UnmanagedEtcdAvailable),
			Status:  metav1.ConditionFalse,
			Reason:  hyperv1.UnmanagedEtcdMisconfiguredReason,
			Message: fmt.Sprintf("etcd secret %s does not have client ca", hcluster.Spec.Etcd.Unmanaged.TLS.ClientSecret.Name),
		}
	}
	return metav1.Condition{
		Type:   string(hyperv1.UnmanagedEtcdAvailable),
		Status: metav1.ConditionTrue,
		Reason: hyperv1.UnmanagedEtcdAsExpected,
	}
}

func (r *HostedClusterReconciler) listNodePools(clusterNamespace, clusterName string) ([]hyperv1.NodePool, error) {
	nodePoolList := &hyperv1.NodePoolList{}
	if err := r.Client.List(
		context.TODO(),
		nodePoolList,
	); err != nil {
		return nil, fmt.Errorf("failed getting nodePool list: %v", err)
	}
	// TODO: do a label association or something
	filtered := []hyperv1.NodePool{}
	for i, nodePool := range nodePoolList.Items {
		if nodePool.Namespace == clusterNamespace && nodePool.Spec.ClusterName == clusterName {
			filtered = append(filtered, nodePoolList.Items[i])
		}
	}
	return filtered, nil
}

func (r *HostedClusterReconciler) delete(ctx context.Context, req ctrl.Request, hc *hyperv1.HostedCluster) (bool, error) {
	controlPlaneNamespace := manifests.HostedControlPlaneNamespace(req.Namespace, req.Name).Name

	nodePools, err := r.listNodePools(req.Namespace, req.Name)
	if err != nil {
		return false, fmt.Errorf("failed to get nodePools by cluster name for cluster %q: %w", req.Name, err)
	}

	for key := range nodePools {
		if err := r.Delete(ctx, &nodePools[key]); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("failed to delete nodePool %q for cluster %q: %w", nodePools[key].GetName(), req.Name, err)
		}
	}

	if hc != nil && len(hc.Spec.InfraID) > 0 {
		r.Log.Info("Deleting Cluster", "clusterName", hc.Spec.InfraID, "clusterNamespace", controlPlaneNamespace)
		cluster := &capiv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      hc.Spec.InfraID,
				Namespace: controlPlaneNamespace,
			},
		}

		if err := r.Delete(ctx, cluster); err != nil {
			if !apierrors.IsNotFound(err) {
				return false, fmt.Errorf("error deleting Cluster: %w", err)
			}
			// The advancing case is when Delete() returns an error that the cluster is not found
		} else {
			r.Log.Info("Waiting for Cluster deletion", "clusterName", hc.Spec.InfraID, "clusterNamespace", controlPlaneNamespace)
			return false, nil
		}
	}

	r.Log.Info("Deleting controlplane namespace", "namespace", controlPlaneNamespace)
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: controlPlaneNamespace},
	}
	if err := r.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("failed to delete namespace: %w", err)
	}
	return true, nil
}

func enqueueParentHostedCluster(obj client.Object) []reconcile.Request {
	var hostedClusterName string
	if obj.GetAnnotations() != nil {
		hostedClusterName = obj.GetAnnotations()[hostedClusterAnnotation]
	}
	if hostedClusterName == "" {
		return []reconcile.Request{}
	}
	return []reconcile.Request{
		{NamespacedName: hyperutil.ParseNamespacedName(hostedClusterName)},
	}
}

func (r *HostedClusterReconciler) reconcileMachineConfigServer(ctx context.Context, hcluster *hyperv1.HostedCluster) error {
	var span trace.Span
	ctx, span = r.tracer.Start(ctx, "reconcile-machine-config-server")
	defer span.End()

	controlPlaneNamespace := manifests.HostedControlPlaneNamespace(hcluster.Namespace, hcluster.Name)
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(controlPlaneNamespace), controlPlaneNamespace); err != nil {
		return fmt.Errorf("failed to get control plane namespace: %w", err)
	}

	// Reconcile service
	mcsService := ignitionserver.MCSService(controlPlaneNamespace.Name)
	if result, err := r.CreateOrUpdate(ctx, r.Client, mcsService, func() error {
		return reconcileMachineConfigServerService(mcsService)
	}); err != nil {
		return fmt.Errorf("failed to reconcile machine config server service: %w", err)
	} else {
		span.AddEvent("reconciled machine config server service", trace.WithAttributes(attribute.String("result", string(result))))
	}

	return nil
}

func reconcileMachineConfigServerService(svc *corev1.Service) error {
	svc.Spec.Selector = map[string]string{
		"app": "machine-config-server",
	}
	var portSpec corev1.ServicePort
	if len(svc.Spec.Ports) > 0 {
		portSpec = svc.Spec.Ports[0]
	} else {
		svc.Spec.Ports = []corev1.ServicePort{portSpec}
	}
	portSpec.Port = int32(8443)
	portSpec.Name = "https"
	portSpec.Protocol = corev1.ProtocolTCP
	portSpec.TargetPort = intstr.FromInt(8443)
	svc.Spec.Ports[0] = portSpec
	svc.Spec.Type = corev1.ServiceTypeClusterIP
	svc.Spec.ClusterIP = corev1.ClusterIPNone
	return nil
}

func reconcileClusterPrometheusRBAC(ctx context.Context, c client.Client, namespace string) error {
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "openshift-prometheus"}}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, role, func() error {
		role.Rules = []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{
				"services",
				"endpoints",
				"pods",
			},
			Verbs: []string{
				"get",
				"list",
				"watch",
			},
		}}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to ensure the %s role: %w", role.Name, err)
	}

	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "openshift-prometheus"}}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, binding, func() error {
		binding.RoleRef.APIGroup = "rbac.authorization.k8s.io"
		binding.RoleRef.Kind = "Role"
		binding.RoleRef.Name = role.Name
		binding.Subjects = []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      "prometheus-k8s",
			Namespace: "openshift-monitoring",
		}}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to ensure the %s rolebinding: %w", binding.Name, err)
	}

	return nil
}

func (r *HostedClusterReconciler) reconcileMachineApprover(ctx context.Context, hcluster *hyperv1.HostedCluster, hcp *hyperv1.HostedControlPlane) error {
	controlPlaneNamespaceName := manifests.HostedControlPlaneNamespace(hcluster.Namespace, hcluster.Name).Name

	// Reconcile machine-approver role
	role := machineapprover.Role(controlPlaneNamespaceName)
	if _, err := r.CreateOrUpdate(ctx, r.Client, role, func() error {
		return reconcileMachineApproverRole(role)
	}); err != nil {
		return fmt.Errorf("failed to reconcile machine-approver role: %w", err)
	}

	// Reconcile machine-approver service account
	sa := machineapprover.ServiceAccount(controlPlaneNamespaceName)
	if _, err := r.CreateOrUpdate(ctx, r.Client, sa, NoopReconcile); err != nil {
		return fmt.Errorf("failed to reconcile machine-approver service account: %w", err)
	}

	// Reconcile machine-approver role binding
	rolebinding := machineapprover.RoleBinding(controlPlaneNamespaceName)
	if _, err := r.CreateOrUpdate(ctx, r.Client, rolebinding, func() error {
		return reconcileMachineApproverRoleBinding(rolebinding, role, sa)
	}); err != nil {
		return fmt.Errorf("failed to reconcile machine-approver role binding: %w", err)
	}
	config := machineapprover.ConfigMap(controlPlaneNamespaceName)
	if _, err := r.CreateOrUpdate(ctx, r.Client, config, func() error {
		return reconcileMachineApproverConfig(config)
	}); err != nil {
		return fmt.Errorf("failed to reconcile machine-approver config: %w", err)
	}

	// The deployment depends on the kubeconfig being reported.
	if hcp.Status.KubeConfig != nil {
		// Resolve the kubeconfig secret for machine-approver
		kubeconfigSecretName := machineapprover.KASServiceKubeconfigSecret(controlPlaneNamespaceName).Name

		// Reconcile machine-approver deployment
		image := imageMachineApprover
		if _, ok := hcluster.Annotations[hyperv1.MachineApproverImage]; ok {
			image = hcluster.Annotations[hyperv1.MachineApproverImage]
		}
		deployment := machineapprover.Deployment(controlPlaneNamespaceName)
		if _, err := r.CreateOrUpdate(ctx, r.Client, deployment, func() error {
			return reconcileMachineApproverDeployment(deployment, hcluster, sa, kubeconfigSecretName, config, image, r.HypershiftOperatorImage)
		}); err != nil {
			return fmt.Errorf("failed to reconcile machine-approver deployment: %w", err)
		}
	}

	return nil
}

type ClusterMachineApproverConfig struct {
	NodeClientCert NodeClientCert `json:"nodeClientCert,omitempty"`
}
type NodeClientCert struct {
	Disabled bool `json:"disabled,omitempty"`
}

func reconcileMachineApproverConfig(cm *corev1.ConfigMap) error {
	// Enable the client cert csr approval
	cfg := ClusterMachineApproverConfig{
		NodeClientCert: NodeClientCert{
			Disabled: false,
		},
	}
	if b, err := yaml.Marshal(cfg); err != nil {
		return err
	} else {
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data["config.yaml"] = string(b)
	}

	return nil
}

func reconcileMachineApproverRole(role *rbacv1.Role) error {
	role.Rules = []rbacv1.PolicyRule{
		{
			APIGroups: []string{"cluster.x-k8s.io"},
			Resources: []string{"machines", "machines/status"},
			Verbs:     []string{"get", "list", "watch"},
		},
	}
	return nil
}

func reconcileMachineApproverRoleBinding(binding *rbacv1.RoleBinding, role *rbacv1.Role, sa *corev1.ServiceAccount) error {
	binding.RoleRef = rbacv1.RoleRef{
		APIGroup: "rbac.authorization.k8s.io",
		Kind:     "Role",
		Name:     role.Name,
	}

	binding.Subjects = []rbacv1.Subject{
		{
			Kind:      "ServiceAccount",
			Name:      sa.Name,
			Namespace: sa.Namespace,
		},
	}
	return nil
}

func reconcileMachineApproverDeployment(deployment *appsv1.Deployment, hc *hyperv1.HostedCluster, sa *corev1.ServiceAccount, kubeconfigSecretName string, config *corev1.ConfigMap, machineApproverImage, availabilityProberImage string) error {
	// TODO: enable leader election when the flag is added in machine-approver
	args := []string{
		"--config=/var/run/configmaps/config/config.yaml",
		"-v=3",
		"--logtostderr",
		"--apigroup=cluster.x-k8s.io",
		"--workload-cluster-kubeconfig=/etc/kubernetes/kubeconfig/kubeconfig",
		"--machine-namespace=" + deployment.Namespace,
		"--disable-status-controller",
	}

	deployment.Spec = appsv1.DeploymentSpec{
		Replicas: k8sutilspointer.Int32Ptr(1),
		Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"app": "machine-approver",
			},
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"app": "machine-approver",
				},
				Name: "machine-approver",
			},
			Spec: corev1.PodSpec{
				ServiceAccountName: sa.Name,
				Tolerations: []corev1.Toleration{
					{
						Key:    "node-role.kubernetes.io/master",
						Effect: corev1.TaintEffectNoSchedule,
					},
				},
				Volumes: []corev1.Volume{
					{
						Name: "kubeconfig",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName: kubeconfigSecretName,
							},
						},
					},
					{
						Name: "config",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: config.Name,
								},
								Optional:    k8sutilspointer.BoolPtr(true),
								DefaultMode: k8sutilspointer.Int32Ptr(440),
							},
						},
					},
				},
				Containers: []corev1.Container{
					{
						Name:            "machine-approver-controller",
						Image:           machineApproverImage,
						ImagePullPolicy: corev1.PullAlways,
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "kubeconfig",
								MountPath: "/etc/kubernetes/kubeconfig",
							},
							{
								Name:      "config",
								MountPath: "/var/run/configmaps/config",
							},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("50Mi"),
								corev1.ResourceCPU:    resource.MustParse("10m"),
							},
						},
						Command: []string{"/usr/bin/machine-approver"},
						Args:    args,
					},
				},
			},
		},
	}
	util.AvailabilityProber(kas.InClusterKASReadyURL(deployment.Namespace), availabilityProberImage, &deployment.Spec.Template.Spec)

	hyperutil.SetColocation(hc, deployment)
	hyperutil.SetRestartAnnotation(hc, deployment)
	hyperutil.SetControlPlaneIsolation(hc, deployment)
	hyperutil.SetDefaultPriorityClass(deployment)
	return nil
}

// isGroupVersionRegistered determines if a specified groupVersion is registered on the cluster
func isGroupVersionRegistered(client discovery.ServerResourcesInterface, groupVersion schema.GroupVersion) (bool, error) {
	_, apis, err := client.ServerGroupsAndResources()
	if err != nil {
		if discovery.IsGroupDiscoveryFailedError(err) {
			// If the group we are looking for can't be fully discovered,
			// that does still mean that it exists.
			// Continue with the search in the discovered groups if not present here.
			e := err.(*discovery.ErrGroupDiscoveryFailed)
			if _, exists := e.Groups[groupVersion]; exists {
				return true, nil
			}
		} else {
			return false, err
		}
	}

	for _, api := range apis {
		if api.GroupVersion == groupVersion.String() {
			return true, nil
		}
	}

	return false, nil
}
