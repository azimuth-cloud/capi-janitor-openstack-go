//go:build envtest

package controller_test

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	infrav1 "sigs.k8s.io/cluster-api-provider-openstack/api/v1beta1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/controller"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack"
)

func TestEnvtestFinalizers(t *testing.T) {
	if err := clusterv1.AddToScheme(testScheme); err != nil {
		t.Fatal(err)
	}
	// Resolve the unmodified CRD from the same CAPO module as the Go types.
	// go test without the envtest build tag remains usable in offline/Nix builds.
	moduleDir, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}",
		"sigs.k8s.io/cluster-api-provider-openstack").Output()
	if err != nil {
		t.Fatalf("locating CAPO CRD: %v", err)
	}
	capiModuleDir, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}",
		"sigs.k8s.io/cluster-api").Output()
	if err != nil {
		t.Fatalf("locating CAPI CRD: %v", err)
	}
	useExistingCluster := false
	testEnv := &envtest.Environment{
		// Always start an isolated control plane, even if the caller has set
		// USE_EXISTING_CLUSTER or has a production kubeconfig configured.
		UseExistingCluster: &useExistingCluster,
		CRDDirectoryPaths: []string{
			filepath.Join(strings.TrimSpace(string(moduleDir)), "config", "crd", "bases", "infrastructure.cluster.x-k8s.io_openstackclusters.yaml"),
			filepath.Join(strings.TrimSpace(string(capiModuleDir)), "config", "crd", "bases", "cluster.x-k8s.io_clusters.yaml"),
		},
		ErrorIfCRDPathMissing: true,
	}
	config, err := testEnv.Start()
	if err != nil {
		t.Fatalf("starting envtest (run make test-envtest to install API binaries): %v", err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("stopping envtest: %v", err)
		}
	})
	apiClient, err := client.New(config, client.Options{Scheme: testScheme})
	if err != nil {
		t.Fatalf("creating API client: %v", err)
	}

	for _, removing := range []bool{false, true} {
		name := "add"
		if removing {
			name = "remove"
		}
		t.Run(name+" preserves concurrent changes", func(t *testing.T) {
			testEnvtestFinalizerConflict(t, apiClient, removing)
		})
	}
	t.Run("manager observes creation", func(t *testing.T) {
		testEnvtestManager(t, config, apiClient)
	})
	t.Run("manager observes pause and Secret recovery", func(t *testing.T) {
		testEnvtestLifecycle(t, config, apiClient)
	})
}

// envtestConflictingClient lets a second controller write after the janitor's
// read, so the conflict is produced by the real API server rather than a mock.
type envtestConflictingClient struct {
	client.Client
	beforePatch func(context.Context) error
	conflicts   int
}

func (c *envtestConflictingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if c.beforePatch != nil {
		beforePatch := c.beforePatch
		c.beforePatch = nil
		if err := beforePatch(ctx); err != nil {
			return err
		}
	}
	err := c.Client.Patch(ctx, obj, patch, opts...)
	if apierrors.IsConflict(err) {
		c.conflicts++
	}
	return err
}

func testEnvtestFinalizerConflict(t *testing.T, apiClient client.Client, removing bool) {
	t.Helper()
	ctx := t.Context()
	namespace := envtestNamespace(t, apiClient)
	const keptFinalizer = "example.com/keep"
	const otherFinalizer = "example.com/other"
	cluster := newCluster("conflict", namespace)
	cluster.Finalizers = []string{keptFinalizer}
	if removing {
		cluster.Finalizers = append(cluster.Finalizers, otherFinalizer, controller.Finalizer)
	}
	if err := apiClient.Create(ctx, cluster); err != nil {
		t.Fatalf("creating OpenStackCluster with real CAPO schema: %v", err)
	}
	if removing {
		if err := apiClient.Create(ctx, newSecret("cloud-credentials", namespace)); err != nil {
			t.Fatalf("creating credentials: %v", err)
		}
		if err := apiClient.Delete(ctx, cluster); err != nil {
			t.Fatalf("deleting OpenStackCluster: %v", err)
		}
	}
	key := client.ObjectKeyFromObject(cluster)
	conflictingClient := &envtestConflictingClient{Client: apiClient}
	conflictingClient.beforePatch = func(ctx context.Context) error {
		var current infrav1.OpenStackCluster
		if err := apiClient.Get(ctx, key, &current); err != nil {
			return err
		}
		current.Annotations = map[string]string{"example.com/concurrent": "preserved"}
		current.Labels = map[string]string{"example.com/concurrent": "preserved"}
		if removing {
			// A deleting object permits removal of existing finalizers only.
			controllerutil.RemoveFinalizer(&current, otherFinalizer)
		} else {
			controllerutil.AddFinalizer(&current, otherFinalizer)
		}
		if err := apiClient.Update(ctx, &current); err != nil {
			return fmt.Errorf("concurrent metadata update: %w", err)
		}
		current.Status.Ready = true
		if err := apiClient.Status().Update(ctx, &current); err != nil {
			return fmt.Errorf("concurrent status update: %w", err)
		}
		return nil
	}
	r := &controller.OpenStackClusterReconciler{
		Client:    conflictingClient,
		APIReader: apiClient,
		Scheme:    testScheme,
		CleanupFunc: func(context.Context, openstack.PurgeOptions) error {
			return nil
		},
	}
	request := ctrl.Request{NamespacedName: key}
	if _, err := r.Reconcile(ctx, request); !apierrors.IsConflict(err) {
		t.Fatalf("first reconcile error = %v, want API resourceVersion conflict", err)
	}
	if conflictingClient.conflicts != 1 {
		t.Fatalf("API conflicts = %d, want 1", conflictingClient.conflicts)
	}
	// Reconciliation errors are retried by the controller workqueue.
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("retrying reconcile: %v", err)
	}
	var got infrav1.OpenStackCluster
	if err := apiClient.Get(ctx, key, &got); err != nil {
		t.Fatalf("reading OpenStackCluster after retry: %v", err)
	}
	wantFinalizers := []string{keptFinalizer}
	if !removing {
		wantFinalizers = append(wantFinalizers, otherFinalizer, controller.Finalizer)
	}
	slices.Sort(got.Finalizers)
	slices.Sort(wantFinalizers)
	if !slices.Equal(got.Finalizers, wantFinalizers) {
		t.Errorf("finalizers = %v, want %v", got.Finalizers, wantFinalizers)
	}
	if got.Annotations["example.com/concurrent"] != "preserved" || got.Labels["example.com/concurrent"] != "preserved" {
		t.Errorf("lost concurrent metadata: annotations=%v labels=%v", got.Annotations, got.Labels)
	}
	if !got.Status.Ready {
		t.Error("lost concurrent status update")
	}
	if got.Spec.IdentityRef != cluster.Spec.IdentityRef {
		t.Errorf("identity changed: got %+v, want %+v", got.Spec.IdentityRef, cluster.Spec.IdentityRef)
	}
}

func testEnvtestManager(t *testing.T, config *rest.Config, apiClient client.Client) {
	t.Helper()
	namespace := envtestNamespace(t, apiClient)
	_, ctx := startEnvtestManager(t, config, namespace, func(context.Context, openstack.PurgeOptions) error {
		return fmt.Errorf("cleanup must not run for an active cluster")
	})
	cluster := newCluster("watched", namespace)
	if err := apiClient.Create(ctx, cluster); err != nil {
		t.Fatalf("creating watched OpenStackCluster: %v", err)
	}
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		var got infrav1.OpenStackCluster
		if err := apiClient.Get(ctx, client.ObjectKeyFromObject(cluster), &got); err != nil {
			return false, err
		}
		return controllerutil.ContainsFinalizer(&got, controller.Finalizer), nil
	}); err != nil {
		t.Fatalf("waiting for manager to add finalizer from creation event: %v", err)
	}
}

func startEnvtestManager(t *testing.T, config *rest.Config, namespace string, cleanupFunc func(context.Context, openstack.PurgeOptions) error) (ctrl.Manager, context.Context) {
	t.Helper()
	// Sequential subtests reuse the controller's process-wide name.
	skipNameValidation := true
	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme: testScheme,
		Client: client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}},
		Cache: cache.Options{DefaultNamespaces: map[string]cache.Config{
			namespace: {},
		}},
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             controllerconfig.Controller{SkipNameValidation: &skipNameValidation},
	})
	if err != nil {
		t.Fatalf("creating manager: %v", err)
	}
	r := &controller.OpenStackClusterReconciler{
		Client:      mgr.GetClient(),
		Scheme:      testScheme,
		Metrics:     controller.NewMetrics(prometheus.NewRegistry()),
		Recorder:    record.NewFakeRecorder(20),
		CleanupFunc: cleanupFunc,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("registering controller: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("manager stopped with an error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("manager did not stop")
		}
	})
	syncCtx, syncCancel := context.WithTimeout(ctx, 10*time.Second)
	defer syncCancel()
	if !mgr.GetCache().WaitForCacheSync(syncCtx) {
		t.Fatal("manager cache did not sync")
	}
	return mgr, ctx
}

func envtestNamespace(t *testing.T, apiClient client.Client) string {
	t.Helper()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "janitor-envtest-"}}
	if err := apiClient.Create(t.Context(), namespace); err != nil {
		t.Fatalf("creating test namespace: %v", err)
	}
	return namespace.Name
}
