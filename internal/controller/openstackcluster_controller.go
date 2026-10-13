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
	// APIReader reads current cleanup inputs and metadata before mutations.
	APIReader            client.Reader
	Scheme               *runtime.Scheme
	DefaultVolumesPolicy string
	RetryDefaultDelay    int
	Metrics              *Metrics
	Recorder             record.EventRecorder
	// CleanupFunc cleans up OpenStack resources. It defaults to
	// openstack.PurgeResources.
	CleanupFunc func(context.Context, openstack.PurgeOptions) error
	// NewSession binds resource verification and credential deletion to one cloud.
	NewSession func(context.Context, openstack.PurgeOptions) (openstack.CleanupSession, error)
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
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;delete
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch

func (r *OpenStackClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var cluster infrav1.OpenStackCluster
	if err := r.reader().Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	clusterName := clusterNameFor(&cluster)
	logger = logger.WithValues("clusterName", clusterName)
	logger.V(1).Info("reconciling OpenStackCluster")
	paused, err := r.isPaused(ctx, &cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if paused {
		return ctrl.Result{}, nil
	}

	// Not deleting: ensure our finalizer is present.
	if cluster.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&cluster, Finalizer) {
			changed, err := r.addFinalizer(ctx, &cluster)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
			}
			if changed {
				logger.Info("Added Janitor finalizer to OpenStackCluster")
			}
		}
		return ctrl.Result{}, nil
	}

	// Deleting: only act if our finalizer is present.
	if !controllerutil.ContainsFinalizer(&cluster, Finalizer) {
		logger.Info("janitor finalizer not present, skipping cleanup")
		return ctrl.Result{}, nil
	}

	result, err := r.reconcileDelete(ctx, &cluster)
	if errors.Is(err, errPaused) || apierrors.IsNotFound(err) {
		return ctrl.Result{}, nil
	}
	if errors.Is(err, cleanup.ErrDeletePending) {
		return ctrl.Result{RequeueAfter: pendingDeleteDelay}, nil
	}
	if err != nil && !apierrors.IsConflict(err) {
		r.countCleanup("failure")
		r.recordEvent(&cluster, corev1.EventTypeWarning, "CleanupFailed", err.Error())
	}
	return result, err
}

func (r *OpenStackClusterReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *OpenStackClusterReconciler) finishCleanup(ctx context.Context, observed *infrav1.OpenStackCluster, secret *corev1.Secret, soleFinalizer bool) (ctrl.Result, error) {
	latest, err := r.checkCleanupInputs(ctx, observed, secret, soleFinalizer)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Use the resourceVersion from checkCleanupInputs to detect concurrent changes.
	base := latest.DeepCopy()
	controllerutil.RemoveFinalizer(latest, Finalizer)
	if err := r.Patch(ctx, latest, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	r.countCleanup("success")
	r.recordEvent(latest, corev1.EventTypeNormal, "CleanupSucceeded", "OpenStack resources cleaned up successfully")
	log.FromContext(ctx).Info("Removed Janitor finalizer from OpenStackCluster")
	return ctrl.Result{}, nil
}

// addFinalizer patches the latest object and checks for concurrent changes.
func (r *OpenStackClusterReconciler) addFinalizer(ctx context.Context, observed *infrav1.OpenStackCluster) (bool, error) {
	var latest infrav1.OpenStackCluster
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(observed), &latest); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if latest.UID != observed.UID {
		return false, apierrors.NewConflict(infrav1.Resource("openstackclusters"), observed.Name,
			errors.New("OpenStackCluster was replaced during reconciliation"))
	}
	if !latest.DeletionTimestamp.IsZero() || controllerutil.ContainsFinalizer(&latest, Finalizer) {
		return false, nil
	}
	paused, err := r.isPaused(ctx, &latest)
	if err != nil || paused {
		return false, err
	}
	base := latest.DeepCopy()
	controllerutil.AddFinalizer(&latest, Finalizer)
	if err := r.Patch(ctx, &latest, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return true, nil
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *OpenStackClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("capi-janitor")
	}
	if r.Metrics == nil {
		r.Metrics = NewMetrics(ctrlmetrics.Registry)
	}
	b := ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.OpenStackCluster{}).
		WithOptions(ctrlcontroller.Options{
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](
				retryBaseDelay,
				r.maxRetryDelay(),
			),
		})
	if err := r.setupLifecycleWatches(mgr, b); err != nil {
		return err
	}
	return b.Complete(r)
}

// clusterNameFor returns the cluster name to use for resource cleanup.
// It prefers the cluster.x-k8s.io/cluster-name label over metadata.name.
func clusterNameFor(cluster *infrav1.OpenStackCluster) string {
	if name, ok := cluster.Labels[ClusterNameLabel]; ok {
		return name
	}
	return cluster.Name
}

// volumesPolicyFor resolves the effective policy for both cleanup and checkpoints.
// Only "delete" enables deletion. Every other value keeps volumes.
func (r *OpenStackClusterReconciler) volumesPolicyFor(cluster *infrav1.OpenStackCluster) string {
	policy := r.DefaultVolumesPolicy
	if policy == "" {
		policy = PolicyDelete
	}
	if ann, ok := cluster.Annotations[VolumesPolicyAnnotation]; ok {
		policy = ann
	}
	if policy == PolicyDelete {
		return PolicyDelete
	}
	return "keep"
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
	if err := r.reader().Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &secret, nil
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
