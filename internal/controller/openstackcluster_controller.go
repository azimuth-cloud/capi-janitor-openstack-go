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
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	infrav1 "sigs.k8s.io/cluster-api-provider-openstack/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/cleanup"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack"
)

const (
	Finalizer = "janitor.capi.stackhpc.com"

	VolumesPolicyAnnotation    = "janitor.capi.stackhpc.com/volumes-policy"
	CredentialPolicyAnnotation = "janitor.capi.stackhpc.com/credential-policy"
	ClusterNameLabel           = "cluster.x-k8s.io/cluster-name"

	PolicyDelete = "delete"

	defaultRetryDelay  = 60 // seconds
	retryBaseDelay     = time.Second
	pendingDeleteDelay = 5 * time.Second
)

// OpenStackClusterReconciler reconciles OpenStackCluster objects from CAPO.
type OpenStackClusterReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	DefaultVolumesPolicy string
	RetryDefaultDelay    int
	Metrics              *Metrics
	Recorder             record.EventRecorder
	// CleanupFunc cleans up OpenStack resources. It defaults to
	// openstack.PurgeResources.
	CleanupFunc func(context.Context, openstack.PurgeOptions) error
}

func (r *OpenStackClusterReconciler) cleanResources(ctx context.Context, options openstack.PurgeOptions) error {
	if r.CleanupFunc != nil {
		return r.CleanupFunc(ctx, options)
	}
	return openstack.PurgeResources(ctx, options)
}

func (r *OpenStackClusterReconciler) countCleanup(outcome string) {
	if r.Metrics != nil {
		r.Metrics.CleanupsTotal.WithLabelValues(outcome).Inc()
	}
}

func (r *OpenStackClusterReconciler) recordEvent(obj client.Object, eventType, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(obj, eventType, reason, msg)
	}
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=openstackclusters,verbs=get;list;watch;patch;update
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;delete
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create
//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch

func (r *OpenStackClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var cluster infrav1.OpenStackCluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	clusterName := clusterNameFor(&cluster)
	logger = logger.WithValues("clusterName", clusterName)
	logger.V(1).Info("reconciling OpenStackCluster")

	// Not deleting: ensure our finalizer is present.
	if cluster.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&cluster, Finalizer) {
			controllerutil.AddFinalizer(&cluster, Finalizer)
			if err := r.Update(ctx, &cluster); err != nil {
				return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
			}
			logger.Info("added janitor finalizer to cluster")
		}
		return ctrl.Result{}, nil
	}

	// Deleting: only act if our finalizer is present.
	if !controllerutil.ContainsFinalizer(&cluster, Finalizer) {
		logger.Info("janitor finalizer not present, skipping cleanup")
		return ctrl.Result{}, nil
	}

	if identityType := cluster.Spec.IdentityRef.Type; identityType != "" && identityType != "Secret" {
		return ctrl.Result{}, fmt.Errorf("unsupported identity reference type %q", identityType)
	}
	if cluster.Spec.IdentityRef.Name == "" {
		return ctrl.Result{}, errors.New("identity Secret name is empty")
	}

	// Fetch the cloud credential secret.
	secret, err := r.findSecret(ctx, cluster.Spec.IdentityRef.Name, req.Namespace)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("fetching identity secret: %w", err)
	}
	if secret == nil {
		return ctrl.Result{}, fmt.Errorf("identity Secret %q not found", cluster.Spec.IdentityRef.Name)
	}

	cloudsYAML := string(secret.Data["clouds.yaml"])
	caCert := string(secret.Data["cacert"])

	cloudName := cluster.Spec.IdentityRef.CloudName
	if cloudName == "" {
		cloudName = "openstack"
	}

	deleteVolumes := r.volumesPolicyFor(&cluster) == PolicyDelete

	credentialPolicy := secret.Annotations[CredentialPolicyAnnotation]

	cleanupErr := r.cleanResources(ctx, openstack.PurgeOptions{
		CloudsYAML:     cloudsYAML,
		CloudName:      cloudName,
		CACert:         caCert,
		ClusterName:    clusterName,
		IncludeVolumes: deleteVolumes,
	})
	if cleanupErr != nil {
		if errors.Is(cleanupErr, cleanup.ErrDeletePending) {
			logger.Info("OpenStack resource deletion is still in progress")
			return ctrl.Result{RequeueAfter: pendingDeleteDelay}, nil
		}
		r.countCleanup("failure")
		r.recordEvent(&cluster, corev1.EventTypeWarning, "CleanupFailed", cleanupErr.Error())
		return ctrl.Result{}, fmt.Errorf("cleaning OpenStack resources: %w", cleanupErr)
	}

	// Credential deletion is the next implementation phase. Keep the Secret and
	// finalizer until that transition has its persistent checkpoint.
	if credentialPolicy == PolicyDelete {
		if len(cluster.Finalizers) > 1 {
			blockingFinalizer := findOtherFinalizer(cluster.Finalizers, Finalizer)
			logger.Info("Waiting for another finalizer before deleting the application credential", "otherFinalizer", blockingFinalizer)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, errors.New("application credential cleanup checkpoint is not implemented")
	}

	r.countCleanup("success")
	r.recordEvent(&cluster, corev1.EventTypeNormal, "CleanupSucceeded", "OpenStack resources cleaned up successfully")

	// Remove our finalizer.
	controllerutil.RemoveFinalizer(&cluster, Finalizer)
	if err := r.Update(ctx, &cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	logger.Info("removed janitor finalizer from cluster")
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *OpenStackClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("capi-janitor")
	}
	if r.Metrics == nil {
		r.Metrics = NewMetrics(ctrlmetrics.Registry)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.OpenStackCluster{}).
		WithOptions(ctrlcontroller.Options{
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](
				retryBaseDelay,
				r.maxRetryDelay(),
			),
		}).
		Complete(r)
}

// clusterNameFor returns the cluster name to use for resource cleanup.
// It prefers the cluster.x-k8s.io/cluster-name label over metadata.name.
func clusterNameFor(cluster *infrav1.OpenStackCluster) string {
	if name, ok := cluster.Labels[ClusterNameLabel]; ok {
		return name
	}
	return cluster.Name
}

func (r *OpenStackClusterReconciler) volumesPolicyFor(cluster *infrav1.OpenStackCluster) string {
	if ann, ok := cluster.Annotations[VolumesPolicyAnnotation]; ok {
		return ann
	}
	if r.DefaultVolumesPolicy != "" {
		return r.DefaultVolumesPolicy
	}
	return PolicyDelete
}

func (r *OpenStackClusterReconciler) maxRetryDelay() time.Duration {
	seconds := r.RetryDefaultDelay
	if seconds <= 0 {
		seconds = defaultRetryDelay
	}
	return time.Duration(seconds) * time.Second
}

func (r *OpenStackClusterReconciler) findSecret(ctx context.Context, name, namespace string) (*corev1.Secret, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &secret, nil
}

func findOtherFinalizer(finalizers []string, excluded string) string {
	for _, finalizer := range finalizers {
		if finalizer != excluded {
			return finalizer
		}
	}
	return ""
}

// DefaultVolumesFromEnv reads CAPI_JANITOR_DEFAULT_VOLUMES_POLICY from environment.
func DefaultVolumesFromEnv() string {
	if v := os.Getenv("CAPI_JANITOR_DEFAULT_VOLUMES_POLICY"); v != "" {
		return v
	}
	return PolicyDelete
}

// RetryDelayFromEnv reads CAPI_JANITOR_RETRY_DEFAULT_DELAY from environment.
func RetryDelayFromEnv() int {
	if v := os.Getenv("CAPI_JANITOR_RETRY_DEFAULT_DELAY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultRetryDelay
}
