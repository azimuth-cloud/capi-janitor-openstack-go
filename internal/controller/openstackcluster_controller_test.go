package controller_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	infrav1 "sigs.k8s.io/cluster-api-provider-openstack/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/cleanup"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/controller"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack"
)

var testScheme *runtime.Scheme

func init() {
	testScheme = runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(testScheme)
	_ = infrav1.AddToScheme(testScheme)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func reconcileRequest(name, namespace string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}
}

func newReconciler(cleanupFunc func(context.Context, openstack.PurgeOptions) error, objects ...client.Object) (*controller.OpenStackClusterReconciler, client.Client) {
	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(objects...).Build()
	r := &controller.OpenStackClusterReconciler{
		Client:      c,
		Scheme:      testScheme,
		CleanupFunc: cleanupFunc,
	}
	return r, c
}

func newReconcilerWithInterceptors(
	cleanupFunc func(context.Context, openstack.PurgeOptions) error,
	interceptors interceptor.Funcs,
	objects ...client.Object,
) (*controller.OpenStackClusterReconciler, client.Client) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptors).
		Build()
	r := &controller.OpenStackClusterReconciler{
		Client:      c,
		Scheme:      testScheme,
		CleanupFunc: cleanupFunc,
	}
	return r, c
}

func newCluster(name, namespace string, opts ...func(*infrav1.OpenStackCluster)) *infrav1.OpenStackCluster {
	c := &infrav1.OpenStackCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: infrav1.OpenStackClusterSpec{
			IdentityRef: infrav1.OpenStackIdentityReference{
				Name:      "cloud-credentials",
				CloudName: "openstack",
			},
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func withFinalizer(c *infrav1.OpenStackCluster) {
	controllerutil.AddFinalizer(c, controller.Finalizer)
}

func withDeletionTimestamp(c *infrav1.OpenStackCluster) {
	now := metav1.Now()
	c.DeletionTimestamp = &now
}

func withClusterLabel(value string) func(*infrav1.OpenStackCluster) {
	return func(c *infrav1.OpenStackCluster) {
		if c.Labels == nil {
			c.Labels = make(map[string]string)
		}
		c.Labels[controller.ClusterNameLabel] = value
	}
}

func newSecret(name, namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string][]byte{"clouds.yaml": []byte("clouds: {}")},
	}
}

func getClusterOrNil(t *testing.T, c client.Client, name, namespace string) *infrav1.OpenStackCluster {
	t.Helper()
	var cluster infrav1.OpenStackCluster
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, &cluster)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("getting cluster: %v", err)
	}
	return &cluster
}

// ── US8.1: Add a finalizer ───────────────────────────────────────────────────

// Scenario: Cluster without deletionTimestamp and without finalizer → finalizer added
func TestReconcile_AddsFinalizer_WhenNotPresent(t *testing.T) {
	cluster := newCluster("mycluster", "default")
	r, c := newReconciler(nil, cluster)

	if _, err := r.Reconcile(context.Background(), reconcileRequest("mycluster", "default")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getClusterOrNil(t, c, "mycluster", "default")
	if got == nil {
		t.Fatal("cluster not found after reconcile")
	}
	if !controllerutil.ContainsFinalizer(got, controller.Finalizer) {
		t.Errorf("expected finalizer %q to be added, got finalizers: %v", controller.Finalizer, got.Finalizers)
	}
}

// Scenario: Cluster with finalizer already present → no state change
func TestReconcile_FinalizerAlreadyPresent_Idempotent(t *testing.T) {
	cluster := newCluster("mycluster", "default", withFinalizer)
	r, c := newReconciler(nil, cluster)

	if _, err := r.Reconcile(context.Background(), reconcileRequest("mycluster", "default")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getClusterOrNil(t, c, "mycluster", "default")
	if got == nil {
		t.Fatal("cluster not found after reconcile")
	}
	if !controllerutil.ContainsFinalizer(got, controller.Finalizer) {
		t.Errorf("expected finalizer %q to still be present", controller.Finalizer)
	}
}

// ── US8.2: Cluster name from label or metadata.name ──────────────────────────

// Scenario: Label cluster.x-k8s.io/cluster-name present → label name used
func TestReconcile_ClusterName_FromLabel(t *testing.T) {
	cluster := newCluster("mycluster-openstack", "default",
		withFinalizer,
		withDeletionTimestamp,
		withClusterLabel("mycluster"),
	)
	secret := newSecret("cloud-credentials", "default")

	var capturedName string
	r, _ := newReconciler(func(_ context.Context, opts openstack.PurgeOptions) error {
		capturedName = opts.ClusterName
		return nil
	}, cluster, secret)

	if _, err := r.Reconcile(context.Background(), reconcileRequest("mycluster-openstack", "default")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedName != "mycluster" {
		t.Errorf("expected ClusterName %q (from label), got %q", "mycluster", capturedName)
	}
}

// Scenario: Label absent → metadata.name used
func TestReconcile_ClusterName_FallsBackToMetadataName(t *testing.T) {
	cluster := newCluster("mycluster", "default", withFinalizer, withDeletionTimestamp)
	secret := newSecret("cloud-credentials", "default")

	var capturedName string
	r, _ := newReconciler(func(_ context.Context, opts openstack.PurgeOptions) error {
		capturedName = opts.ClusterName
		return nil
	}, cluster, secret)

	if _, err := r.Reconcile(context.Background(), reconcileRequest("mycluster", "default")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedName != "mycluster" {
		t.Errorf("expected ClusterName %q (from metadata.name), got %q", "mycluster", capturedName)
	}
}

// ── US8.3: Remove the finalizer after successful cleanup ─────────────────────

// Scenario: Successful cleanup → finalizer removed
func TestReconcile_RemovesFinalizer_AfterCleanup(t *testing.T) {
	cluster := newCluster("mycluster", "default", withFinalizer, withDeletionTimestamp)
	secret := newSecret("cloud-credentials", "default")

	r, c := newReconciler(func(_ context.Context, _ openstack.PurgeOptions) error { return nil }, cluster, secret)

	if _, err := r.Reconcile(context.Background(), reconcileRequest("mycluster", "default")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// After removing the last finalizer with DeletionTimestamp set, the fake client may GC the object.
	got := getClusterOrNil(t, c, "mycluster", "default")
	if got != nil && controllerutil.ContainsFinalizer(got, controller.Finalizer) {
		t.Errorf("expected finalizer %q to be removed after cleanup", controller.Finalizer)
	}
}

// Scenario: Finalizer absent at deletion time → no cleanup
func TestReconcile_SkipsCleanup_WhenFinalizerAbsent(t *testing.T) {
	// Use a non-janitor finalizer to keep the cluster alive in the fake client
	// when we call Delete (which sets DeletionTimestamp instead of deleting immediately).
	cluster := newCluster("mycluster", "default")
	controllerutil.AddFinalizer(cluster, "other.finalizer.example.com")

	cleanupCalled := false
	r, c := newReconciler(func(_ context.Context, _ openstack.PurgeOptions) error {
		cleanupCalled = true
		return nil
	}, cluster)

	// Trigger deletion — fake client sets DeletionTimestamp (kept alive by other finalizer).
	if err := c.Delete(context.Background(), cluster); err != nil {
		t.Fatalf("marking cluster for deletion: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), reconcileRequest("mycluster", "default")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cleanupCalled {
		t.Error("expected cleanup NOT to be called when janitor finalizer is absent")
	}
}

// ── US8.4: Retry mechanism through controller runtime ────────────────────────

// Scenario: Error during purge is returned for workqueue retry
func TestReconcile_ReturnsCleanupError(t *testing.T) {
	cluster := newCluster("mycluster", "default", withFinalizer, withDeletionTimestamp)
	secret := newSecret("cloud-credentials", "default")

	r, c := newReconciler(func(_ context.Context, _ openstack.PurgeOptions) error {
		return errors.New("purge failed")
	}, cluster, secret)

	if _, err := r.Reconcile(context.Background(), reconcileRequest("mycluster", "default")); err == nil {
		t.Fatal("expected purge error to be returned")
	}

	got := getClusterOrNil(t, c, "mycluster", "default")
	if got == nil {
		t.Fatal("cluster not found after reconcile")
	}
	if len(got.Annotations) != 0 {
		t.Errorf("expected no retry annotation changes, got %v", got.Annotations)
	}
}

// Scenario: An accepted delete is observed again after a short fixed delay.
func TestReconcile_RequeuesPendingDelete(t *testing.T) {
	cluster := newCluster("mycluster", "default", withFinalizer, withDeletionTimestamp)
	secret := newSecret("cloud-credentials", "default")
	r, _ := newReconciler(func(context.Context, openstack.PurgeOptions) error {
		return cleanup.ErrDeletePending
	}, cluster, secret)

	result, err := r.Reconcile(context.Background(), reconcileRequest("mycluster", "default"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Fatalf("expected a 5s requeue, got %s", result.RequeueAfter)
	}
}

// ── Additional error-path and branch coverage ────────────────────────────────

// Scenario: fetching the cluster fails with a non-NotFound error → propagated
func TestReconcile_GetError_NonNotFound_Propagates(t *testing.T) {
	r, _ := newReconcilerWithInterceptors(nil, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key types.NamespacedName, obj client.Object, opts ...client.GetOption) error {
			return errors.New("boom")
		},
	})

	if _, err := r.Reconcile(context.Background(), reconcileRequest("mycluster", "default")); err == nil {
		t.Fatal("expected error to be propagated, got nil")
	}
}

// Scenario: adding the finalizer fails on Update → propagated
func TestReconcile_AddFinalizer_UpdateError_Propagates(t *testing.T) {
	cluster := newCluster("mycluster", "default")
	r, _ := newReconcilerWithInterceptors(nil, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			return errors.New("update failed")
		},
	}, cluster)

	_, err := r.Reconcile(context.Background(), reconcileRequest("mycluster", "default"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "adding finalizer:") {
		t.Errorf("expected error to wrap %q, got: %v", "adding finalizer:", err)
	}
}

// Scenario: fetching the identity secret fails with a non-NotFound error → propagated
func TestReconcile_GetSecret_NonNotFoundError_Propagates(t *testing.T) {
	cluster := newCluster("mycluster", "default", withFinalizer, withDeletionTimestamp)
	r, _ := newReconcilerWithInterceptors(nil, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key types.NamespacedName, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				return errors.New("secret get failed")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}, cluster)

	_, err := r.Reconcile(context.Background(), reconcileRequest("mycluster", "default"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "fetching identity secret:") {
		t.Errorf("expected error to wrap %q, got: %v", "fetching identity secret:", err)
	}
}

// Scenario: identity secret does not exist and cleanup remains blocked
func TestReconcile_ReturnsMissingSecretError(t *testing.T) {
	cluster := newCluster("mycluster", "default", withFinalizer, withDeletionTimestamp)
	cleanupCalled := false
	r, _ := newReconciler(func(context.Context, openstack.PurgeOptions) error {
		cleanupCalled = true
		return nil
	}, cluster) // no secret created

	_, err := r.Reconcile(context.Background(), reconcileRequest("mycluster", "default"))
	if err == nil || !strings.Contains(err.Error(), "identity Secret") {
		t.Fatalf("expected missing Secret error, got %v", err)
	}
	if cleanupCalled {
		t.Error("expected purge NOT to be called when identity secret is absent")
	}
}

// Scenario: IdentityRef.CloudName empty → defaults to "openstack"
func TestReconcile_CloudName_DefaultsToOpenstack_WhenEmpty(t *testing.T) {
	cluster := newCluster("mycluster", "default", withFinalizer, withDeletionTimestamp,
		func(c *infrav1.OpenStackCluster) { c.Spec.IdentityRef.CloudName = "" })
	secret := newSecret("cloud-credentials", "default")

	var capturedCloudName string
	r, _ := newReconciler(func(_ context.Context, opts openstack.PurgeOptions) error {
		capturedCloudName = opts.CloudName
		return nil
	}, cluster, secret)

	if _, err := r.Reconcile(context.Background(), reconcileRequest("mycluster", "default")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedCloudName != "openstack" {
		t.Errorf("expected CloudName to default to %q, got %q", "openstack", capturedCloudName)
	}
}

func TestReconcile_ForwardsSecretData(t *testing.T) {
	cluster := newCluster("mycluster", "default", withFinalizer, withDeletionTimestamp)
	secret := newSecret("cloud-credentials", "default")
	secret.Data["clouds.yaml"] = []byte("Y2xvdWRzOiB7fQ==")
	secret.Data["cacert"] = []byte("Y2VydA==")

	var capturedOptions openstack.PurgeOptions
	r, _ := newReconciler(func(_ context.Context, options openstack.PurgeOptions) error {
		capturedOptions = options
		return nil
	}, cluster, secret)

	if _, err := r.Reconcile(context.Background(), reconcileRequest("mycluster", "default")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedOptions.CloudsYAML != "Y2xvdWRzOiB7fQ==" {
		t.Fatalf("clouds.yaml bytes changed to %q", capturedOptions.CloudsYAML)
	}
	if capturedOptions.CACert != "Y2VydA==" {
		t.Fatalf("cacert bytes changed to %q", capturedOptions.CACert)
	}
}

func TestReconcile_RejectsUnsupportedIdentity(t *testing.T) {
	cluster := newCluster("mycluster", "default", withFinalizer, withDeletionTimestamp)
	cluster.Spec.IdentityRef.Type = "ClusterIdentity"
	cleanupCalled := false
	r, _ := newReconciler(func(context.Context, openstack.PurgeOptions) error {
		cleanupCalled = true
		return nil
	}, cluster)

	_, err := r.Reconcile(context.Background(), reconcileRequest("mycluster", "default"))
	if err == nil || !strings.Contains(err.Error(), "unsupported identity reference type") {
		t.Fatalf("expected unsupported identity error, got %v", err)
	}
	if cleanupCalled {
		t.Fatal("cleanup ran for an unsupported identity type")
	}
}

// Scenario: credential deletion is requested before its checkpoint is
// implemented. The Secret and finalizer remain.
func TestReconcile_BlocksCredentialDelete(t *testing.T) {
	cluster := newCluster("mycluster", "default", withFinalizer, withDeletionTimestamp)
	secret := newSecret("cloud-credentials", "default")
	secret.Annotations = map[string]string{controller.CredentialPolicyAnnotation: controller.PolicyDelete}

	r, c := newReconciler(func(context.Context, openstack.PurgeOptions) error { return nil }, cluster, secret)

	_, err := r.Reconcile(context.Background(), reconcileRequest("mycluster", "default"))
	if err == nil || !strings.Contains(err.Error(), "credential cleanup checkpoint is not implemented") {
		t.Fatalf("expected credential checkpoint error, got %v", err)
	}

	var gotSecret corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cloud-credentials", Namespace: "default"}, &gotSecret); err != nil {
		t.Errorf("expected credential Secret to remain, got %v", err)
	}

	got := getClusterOrNil(t, c, "mycluster", "default")
	if got == nil || !controllerutil.ContainsFinalizer(got, controller.Finalizer) {
		t.Error("expected janitor finalizer to remain")
	}
}

// Scenario: credential policy "delete" but other finalizers remain
func TestReconcile_RequeuesForFinalizers(t *testing.T) {
	cluster := newCluster("mycluster", "default", withFinalizer, withDeletionTimestamp)
	controllerutil.AddFinalizer(cluster, "other.finalizer.example.com")
	secret := newSecret("cloud-credentials", "default")
	secret.Annotations = map[string]string{controller.CredentialPolicyAnnotation: controller.PolicyDelete}

	r, c := newReconciler(func(context.Context, openstack.PurgeOptions) error { return nil }, cluster, secret)

	result, err := r.Reconcile(context.Background(), reconcileRequest("mycluster", "default"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Fatalf("expected a 5s requeue, got %s", result.RequeueAfter)
	}

	var gotSecret corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cloud-credentials", Namespace: "default"}, &gotSecret); err != nil {
		t.Errorf("expected credential secret to still exist, got err: %v", err)
	}

	got := getClusterOrNil(t, c, "mycluster", "default")
	if got == nil {
		t.Fatal("expected cluster to still exist")
	}
	if !controllerutil.ContainsFinalizer(got, controller.Finalizer) {
		t.Error("expected janitor finalizer to still be present")
	}
}

// Scenario: removing the janitor finalizer fails on Update → propagated
func TestReconcile_RemoveFinalizer_UpdateError_Propagates(t *testing.T) {
	cluster := newCluster("mycluster", "default", withFinalizer, withDeletionTimestamp)
	secret := newSecret("cloud-credentials", "default") // no credential-policy-delete annotation

	r, _ := newReconcilerWithInterceptors(
		func(context.Context, openstack.PurgeOptions) error { return nil },
		interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				return errors.New("update failed")
			},
		},
		cluster, secret,
	)

	_, err := r.Reconcile(context.Background(), reconcileRequest("mycluster", "default"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "removing finalizer:") {
		t.Errorf("expected error to wrap %q, got: %v", "removing finalizer:", err)
	}
}

// Scenario: CleanupFunc is nil and the real typed path returns its configuration error.
func TestReconcile_UsesDefaultCleanup(t *testing.T) {
	cluster := newCluster("mycluster", "default", withFinalizer, withDeletionTimestamp)
	secret := newSecret("cloud-credentials", "default") // clouds.yaml: "clouds: {}" — no "openstack" entry
	r, c := newReconciler(nil, cluster, secret)         // CleanupFunc left nil

	if _, err := r.Reconcile(context.Background(), reconcileRequest("mycluster", "default")); err == nil {
		t.Fatal("expected typed client configuration error")
	}

	got := getClusterOrNil(t, c, "mycluster", "default")
	if got == nil {
		t.Fatal("cluster not found after reconcile")
	}
	if !controllerutil.ContainsFinalizer(got, controller.Finalizer) {
		t.Error("expected janitor finalizer to remain after cleanup failure")
	}
}
