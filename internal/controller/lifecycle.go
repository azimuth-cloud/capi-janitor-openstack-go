package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	infrav1 "sigs.k8s.io/cluster-api-provider-openstack/api/v1beta1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	identitySecretIndex = "janitor.identitySecret"
	ownerClusterIndex   = "janitor.ownerCluster"
)

func (r *OpenStackClusterReconciler) isPaused(ctx context.Context, cluster *infrav1.OpenStackCluster) (bool, error) {
	if hasPauseAnnotation(cluster) {
		return true, nil
	}
	for _, ref := range cluster.OwnerReferences {
		if !isClusterOwner(ref) {
			continue
		}
		var owner clusterv1.Cluster
		key := client.ObjectKey{Namespace: cluster.Namespace, Name: ref.Name}
		if err := r.reader().Get(ctx, key, &owner); err != nil {
			// The owning Cluster may already be deleted.
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, fmt.Errorf("reading owning Cluster: %w", err)
		}
		if owner.UID == ref.UID && (owner.Spec.Paused || hasPauseAnnotation(&owner)) {
			return true, nil
		}
	}
	return false, nil
}

func hasPauseAnnotation(obj client.Object) bool {
	_, paused := obj.GetAnnotations()[clusterv1.PausedAnnotation]
	return paused
}

func isClusterOwner(ref metav1.OwnerReference) bool {
	groupVersion, err := schema.ParseGroupVersion(ref.APIVersion)
	return err == nil && groupVersion.Group == clusterv1.GroupVersion.Group &&
		ref.Kind == "Cluster" && ref.Name != "" && ref.UID != ""
}

func identitySecretKeys(obj client.Object) []string {
	cluster := obj.(*infrav1.OpenStackCluster)
	if cluster.Spec.IdentityRef.Name == "" ||
		(cluster.Spec.IdentityRef.Type != "" && cluster.Spec.IdentityRef.Type != "Secret") {
		return nil
	}
	return []string{cluster.Spec.IdentityRef.Name}
}

func ownerClusterKeys(obj client.Object) []string {
	var keys []string
	for _, ref := range obj.GetOwnerReferences() {
		if isClusterOwner(ref) {
			keys = append(keys, clusterOwnerIndexKey(ref.Name, ref.UID))
		}
	}
	return keys
}

func clusterOwnerIndexKey(name string, uid types.UID) string {
	return name + "/" + string(uid)
}

func clusterPauseChanged(e event.UpdateEvent) bool {
	previous, previousOK := e.ObjectOld.(*clusterv1.Cluster)
	current, currentOK := e.ObjectNew.(*clusterv1.Cluster)
	if !previousOK || !currentOK {
		return false
	}
	return previous.UID != current.UID || previous.Spec.Paused != current.Spec.Paused ||
		hasPauseAnnotation(previous) != hasPauseAnnotation(current)
}

func (r *OpenStackClusterReconciler) setupLifecycleWatches(mgr ctrl.Manager, b *builder.Builder) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &infrav1.OpenStackCluster{},
		identitySecretIndex, identitySecretKeys); err != nil {
		return fmt.Errorf("indexing identity Secrets: %w", err)
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &infrav1.OpenStackCluster{},
		ownerClusterIndex, ownerClusterKeys); err != nil {
		return fmt.Errorf("indexing owning Clusters: %w", err)
	}
	// Watch Secret metadata and read credential data directly from the API server.
	b.WatchesMetadata(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.requestsForSecret)).
		Watches(&clusterv1.Cluster{}, handler.EnqueueRequestsFromMapFunc(r.requestsForCluster),
			builder.WithPredicates(predicate.Funcs{UpdateFunc: clusterPauseChanged}))
	return nil
}

func (r *OpenStackClusterReconciler) requestsForSecret(ctx context.Context, secret client.Object) []reconcile.Request {
	return r.requestsForReference(ctx, secret, identitySecretIndex, secret.GetName())
}

func (r *OpenStackClusterReconciler) requestsForCluster(ctx context.Context, cluster client.Object) []reconcile.Request {
	return r.requestsForReference(ctx, cluster, ownerClusterIndex, clusterOwnerIndexKey(cluster.GetName(), cluster.GetUID()))
}

func (r *OpenStackClusterReconciler) requestsForReference(ctx context.Context, obj client.Object, index, key string) []reconcile.Request {
	var clusters infrav1.OpenStackClusterList
	if err := r.List(ctx, &clusters, client.InNamespace(obj.GetNamespace()), client.MatchingFields{index: key}); err != nil {
		log.FromContext(ctx).Error(err, "Could not list OpenStackClusters for referenced object", "object", client.ObjectKeyFromObject(obj))
		return nil
	}
	requests := make([]reconcile.Request, 0, len(clusters.Items))
	for _, cluster := range clusters.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&cluster)})
	}
	return requests
}
