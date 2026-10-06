//go:build envtest

package controller_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	infrav1 "sigs.k8s.io/cluster-api-provider-openstack/api/v1beta1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/cleanup"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/controller"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack"
)

func testEnvtestLifecycle(t *testing.T, config *rest.Config, apiClient client.Client) {
	t.Helper()
	namespace := envtestNamespace(t, apiClient)
	var cleanups atomic.Int32
	var lastCA atomic.Value
	mgr, ctx := startEnvtestManager(t, config, namespace, func(_ context.Context, options openstack.PurgeOptions) error {
		cleanups.Add(1)
		lastCA.Store(options.CACert)
		return cleanup.ErrDeletePending
	})
	owner := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "paused-owner", Namespace: namespace},
		Spec:       clusterv1.ClusterSpec{Paused: true},
	}
	if err := apiClient.Create(ctx, owner); err != nil {
		t.Fatalf("creating CAPI Cluster: %v", err)
	}
	infra := newCluster("paused-infra", namespace)
	infra.OwnerReferences = []metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "Cluster", Name: owner.Name, UID: owner.UID}}
	if err := apiClient.Create(ctx, infra); err != nil {
		t.Fatal(err)
	}
	// Wait for the manager to observe the object before checking pause behavior.
	envtestEventually(t, ctx, "infrastructure cache population", func(ctx context.Context) (bool, error) {
		return mgr.GetClient().Get(ctx, client.ObjectKeyFromObject(infra), &infrav1.OpenStackCluster{}) == nil, nil
	})
	assertNoFinalizer := func() {
		t.Helper()
		var got infrav1.OpenStackCluster
		if err := apiClient.Get(ctx, client.ObjectKeyFromObject(infra), &got); err != nil {
			t.Fatal(err)
		}
		if controllerutil.ContainsFinalizer(&got, controller.Finalizer) {
			t.Fatal("manager added finalizer while owner was paused")
		}
	}
	for range 5 {
		assertNoFinalizer()
		time.Sleep(50 * time.Millisecond)
	}
	owner.Spec.Paused = false
	if err := apiClient.Update(ctx, owner); err != nil {
		t.Fatal(err)
	}
	envtestEventually(t, ctx, "owner resume event adds finalizer", func(ctx context.Context) (bool, error) {
		var got infrav1.OpenStackCluster
		if err := apiClient.Get(ctx, client.ObjectKeyFromObject(infra), &got); err != nil {
			return false, err
		}
		return controllerutil.ContainsFinalizer(&got, controller.Finalizer), nil
	})
	// Pause the OpenStackCluster before deletion. Cleanup must wait for resume.
	if err := apiClient.Get(ctx, client.ObjectKeyFromObject(infra), infra); err != nil {
		t.Fatal(err)
	}
	infra.Annotations = map[string]string{clusterv1.PausedAnnotation: ""}
	if err := apiClient.Update(ctx, infra); err != nil {
		t.Fatal(err)
	}
	if err := apiClient.Delete(ctx, infra); err != nil {
		t.Fatal(err)
	}
	if err := apiClient.Get(ctx, client.ObjectKeyFromObject(infra), infra); err != nil {
		t.Fatal(err)
	}
	delete(infra.Annotations, clusterv1.PausedAnnotation)
	if err := apiClient.Update(ctx, infra); err != nil {
		t.Fatal(err)
	}
	// Creating the missing Secret should restart cleanup.
	if cleanups.Load() != 0 {
		t.Fatal("cleanup ran without credentials")
	}
	secret := newSecret("cloud-credentials", namespace)
	if err := apiClient.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	envtestEventually(t, ctx, "restored Secret resumes cleanup", func(context.Context) (bool, error) {
		return cleanups.Load() > 0, nil
	})
	// Secret updates should trigger reconciliation before the five-second retry.
	secret.Data["cacert"] = []byte("updated-ca")
	if err := apiClient.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(ctx, 20*time.Millisecond, 2*time.Second, true, func(context.Context) (bool, error) {
		return lastCA.Load() == "updated-ca", nil
	}); err != nil {
		t.Fatalf("Secret data update did not enqueue cleanup before the retry timer: %v", err)
	}
}

func envtestEventually(t *testing.T, ctx context.Context, description string, condition wait.ConditionWithContextFunc) {
	t.Helper()
	if err := wait.PollUntilContextTimeout(ctx, 20*time.Millisecond, 10*time.Second, true, condition); err != nil {
		t.Fatalf("waiting for %s: %v", description, err)
	}
}
