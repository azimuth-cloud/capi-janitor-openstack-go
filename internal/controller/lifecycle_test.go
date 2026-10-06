package controller

import (
	"context"
	"errors"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	infrav1 "sigs.k8s.io/cluster-api-provider-openstack/api/v1beta1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack"
)

func lifecycleScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, infrav1.AddToScheme, clusterv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func lifecycleOwnerRef(uid types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: clusterv1.GroupVersion.String(), Kind: "Cluster", Name: "owner", UID: uid}
}

func TestPauseHonorsObjectAndOwner(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*infrav1.OpenStackCluster, *clusterv1.Cluster)
		paused bool
	}{
		{"unpaused", func(*infrav1.OpenStackCluster, *clusterv1.Cluster) {}, false},
		{"infrastructure annotation presence", func(infra *infrav1.OpenStackCluster, _ *clusterv1.Cluster) {
			infra.Annotations = map[string]string{clusterv1.PausedAnnotation: "false"}
		}, true},
		{"owner spec", func(_ *infrav1.OpenStackCluster, owner *clusterv1.Cluster) { owner.Spec.Paused = true }, true},
		{"owner annotation", func(_ *infrav1.OpenStackCluster, owner *clusterv1.Cluster) {
			owner.Annotations = map[string]string{clusterv1.PausedAnnotation: ""}
		}, true},
		{"owner v1beta2 reference", func(infra *infrav1.OpenStackCluster, owner *clusterv1.Cluster) {
			infra.OwnerReferences[0].APIVersion = "cluster.x-k8s.io/v1beta2"
			owner.Spec.Paused = true
		}, true},
		{"replacement Cluster is unrelated", func(_ *infrav1.OpenStackCluster, owner *clusterv1.Cluster) {
			owner.UID, owner.Spec.Paused = "replacement", true
		}, false},
		{"different namespace is unrelated", func(_ *infrav1.OpenStackCluster, owner *clusterv1.Cluster) {
			owner.Namespace, owner.Spec.Paused = "elsewhere", true
		}, false},
		{"different API group is unrelated", func(infra *infrav1.OpenStackCluster, owner *clusterv1.Cluster) {
			infra.OwnerReferences[0].APIVersion = "other.example/v1"
			owner.Spec.Paused = true
		}, false},
		{"label alone is not ownership", func(infra *infrav1.OpenStackCluster, owner *clusterv1.Cluster) {
			infra.OwnerReferences = nil
			infra.Labels = map[string]string{ClusterNameLabel: "owner"}
			owner.Spec.Paused = true
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			infra := &infrav1.OpenStackCluster{ObjectMeta: metav1.ObjectMeta{
				Namespace: "test", Name: "infra", OwnerReferences: []metav1.OwnerReference{lifecycleOwnerRef("owner-uid")},
			}}
			owner := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "owner", UID: "owner-uid"}}
			tc.modify(infra, owner)
			r := &OpenStackClusterReconciler{Client: fake.NewClientBuilder().WithScheme(lifecycleScheme(t)).WithObjects(owner).Build()}
			paused, err := r.isPaused(t.Context(), infra)
			if err != nil || paused != tc.paused {
				t.Fatalf("isPaused = %v, %v, want %v, nil", paused, err, tc.paused)
			}
		})
	}
}

func TestPauseOwnerReadUsesAPIReaderAndPropagatesFailure(t *testing.T) {
	scheme := lifecycleScheme(t)
	infra := &infrav1.OpenStackCluster{ObjectMeta: metav1.ObjectMeta{
		Namespace: "test", Name: "infra", OwnerReferences: []metav1.OwnerReference{lifecycleOwnerRef("owner-uid")},
	}}
	owner := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "owner", UID: "owner-uid"}}
	stale := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner).Build()
	owner.Spec.Paused = true
	fresh := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner).Build()
	r := &OpenStackClusterReconciler{Client: stale, APIReader: fresh}
	if paused, err := r.isPaused(t.Context(), infra); !paused || err != nil {
		t.Fatalf("fresh pause = %v, %v, want true, nil", paused, err)
	}
	denied := errors.New("owner read denied")
	r.APIReader = interceptor.NewClient(fresh, interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return denied
	}})
	if _, err := r.isPaused(t.Context(), infra); !errors.Is(err, denied) {
		t.Fatalf("owner read error = %v, want %v", err, denied)
	}
}

func TestPausedReconcileDoesNotMutateOrClean(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		for _, ownerPause := range []bool{false, true} {
			t.Run(map[bool]string{true: "deleting", false: "active"}[deleting]+map[bool]string{true: " owner", false: " infra"}[ownerPause], func(t *testing.T) {
				infra := &infrav1.OpenStackCluster{ObjectMeta: metav1.ObjectMeta{
					Namespace: "test", Name: "infra", OwnerReferences: []metav1.OwnerReference{lifecycleOwnerRef("owner-uid")},
				}}
				owner := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "owner", UID: "owner-uid"}}
				if ownerPause {
					owner.Spec.Paused = true
				} else {
					infra.Annotations = map[string]string{clusterv1.PausedAnnotation: ""}
				}
				if deleting {
					now := metav1.Now()
					infra.DeletionTimestamp, infra.Finalizers = &now, []string{Finalizer}
				}
				c := fake.NewClientBuilder().WithScheme(lifecycleScheme(t)).WithObjects(infra, owner).Build()
				r := &OpenStackClusterReconciler{Client: c, CleanupFunc: func(context.Context, openstack.PurgeOptions) error {
					t.Fatal("cleanup ran while paused")
					return nil
				}}
				result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(infra)})
				if err != nil || !result.IsZero() {
					t.Fatalf("paused reconcile = %+v, %v, want no retry or error", result, err)
				}
				var got infrav1.OpenStackCluster
				if err := c.Get(t.Context(), client.ObjectKeyFromObject(infra), &got); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(got.Finalizers, infra.Finalizers) {
					t.Fatalf("finalizers changed while paused: %v", got.Finalizers)
				}
			})
		}
	}
}

func TestSecondaryWatchesSelectRelatedObjects(t *testing.T) {
	makeInfra := func(name, namespace, secret string, ownerUID types.UID) *infrav1.OpenStackCluster {
		return &infrav1.OpenStackCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, OwnerReferences: []metav1.OwnerReference{lifecycleOwnerRef(ownerUID)}},
			Spec:       infrav1.OpenStackClusterSpec{IdentityRef: infrav1.OpenStackIdentityReference{Name: secret}},
		}
	}
	objects := []client.Object{
		makeInfra("related", "test", "credentials", "owner-uid"),
		makeInfra("other-secret", "test", "other", "other-uid"),
		makeInfra("elsewhere", "other", "credentials", "owner-uid"),
		makeInfra("old-owner", "test", "old-secret", "old-uid"),
	}
	unsupported := makeInfra("unsupported-identity", "test", "credentials", "other-uid")
	unsupported.Spec.IdentityRef.Type = "Other"
	wrongOwnerName := makeInfra("wrong-owner-name", "test", "other", "owner-uid")
	wrongOwnerName.OwnerReferences[0].Name = "other"
	objects = append(objects, unsupported, wrongOwnerName)
	c := fake.NewClientBuilder().WithScheme(lifecycleScheme(t)).WithObjects(objects...).
		WithIndex(&infrav1.OpenStackCluster{}, identitySecretIndex, identitySecretKeys).
		WithIndex(&infrav1.OpenStackCluster{}, ownerClusterIndex, ownerClusterKeys).Build()
	r := &OpenStackClusterReconciler{Client: c}
	secret := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "credentials"}}
	owner := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "owner", UID: "owner-uid"}}
	for name, requests := range map[string][]ctrl.Request{
		"Secret":  r.requestsForSecret(t.Context(), secret),
		"Cluster": r.requestsForCluster(t.Context(), owner),
	} {
		if len(requests) != 1 || requests[0].Name != "related" || requests[0].Namespace != "test" {
			t.Errorf("%s watch requests = %v, want only test/related", name, requests)
		}
	}
}

func TestClusterWatchOnlyRequeuesPauseAndIdentityChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*clusterv1.Cluster)
		want   bool
	}{
		{"pause spec", func(cluster *clusterv1.Cluster) { cluster.Spec.Paused = true }, true},
		{"pause annotation", func(cluster *clusterv1.Cluster) {
			cluster.Annotations = map[string]string{clusterv1.PausedAnnotation: ""}
		}, true},
		{"UID", func(cluster *clusterv1.Cluster) { cluster.UID = "replacement-uid" }, true},
		{"status", func(cluster *clusterv1.Cluster) { cluster.Status.InfrastructureReady = true }, false},
		{"unrelated annotation", func(cluster *clusterv1.Cluster) {
			cluster.Annotations = map[string]string{"example.com/note": "changed"}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{UID: "owner-uid"}}
			updated := original.DeepCopy()
			tc.change(updated)
			for _, update := range []event.UpdateEvent{
				{ObjectOld: original, ObjectNew: updated},
				{ObjectOld: updated, ObjectNew: original},
			} {
				if got := clusterPauseChanged(update); got != tc.want {
					t.Errorf("Cluster update selected = %v, want %v", got, tc.want)
				}
			}
		})
	}
	t.Run("pause annotation value", func(t *testing.T) {
		original := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{clusterv1.PausedAnnotation: ""}}}
		updated := original.DeepCopy()
		updated.Annotations[clusterv1.PausedAnnotation] = "false"
		if clusterPauseChanged(event.UpdateEvent{ObjectOld: original, ObjectNew: updated}) {
			t.Fatal("annotation presence controls pause, its value must not enqueue work")
		}
	})
}
