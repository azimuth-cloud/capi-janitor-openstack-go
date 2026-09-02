package controller_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/cleanup"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/controller"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack"
)

func newObservedReconciler(
	registry *prometheus.Registry,
	cleanupFunc func(context.Context, openstack.PurgeOptions) error,
	objects ...client.Object,
) (*controller.OpenStackClusterReconciler, *record.FakeRecorder) {
	reconciler, _ := newReconciler(cleanupFunc, objects...)
	reconciler.Metrics = controller.NewMetrics(registry)
	recorder := record.NewFakeRecorder(10)
	reconciler.Recorder = recorder
	return reconciler, recorder
}

// ── US11.1: Prometheus Metrics ────────────────────────────────────────────

// Scenario: successful cleanup → capi_janitor_cleanups_total{result="success"} += 1
func TestMetrics_IncrementsSuccess_OnCleanup(t *testing.T) {
	cluster := newCluster("c", "default", withFinalizer, withDeletionTimestamp)
	secret := newSecret("cloud-credentials", "default")

	registry := prometheus.NewRegistry()
	reconciler, _ := newObservedReconciler(registry,
		func(_ context.Context, _ openstack.PurgeOptions) error { return nil },
		cluster, secret,
	)

	if _, err := reconciler.Reconcile(context.Background(), reconcileRequest("c", "default")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := testutil.ToFloat64(reconciler.Metrics.CleanupsTotal.WithLabelValues("success")); got != 1 {
		t.Errorf("expected cleanupsTotal{result=success}=1, got %v", got)
	}
}

// Scenario: failed purge → capi_janitor_cleanups_total{result="failure"} += 1
func TestMetrics_IncrementsFailure_OnCleanupError(t *testing.T) {
	cluster := newCluster("c", "default", withFinalizer, withDeletionTimestamp)
	secret := newSecret("cloud-credentials", "default")

	registry := prometheus.NewRegistry()
	reconciler, _ := newObservedReconciler(registry,
		func(_ context.Context, _ openstack.PurgeOptions) error { return errors.New("purge failed") },
		cluster, secret,
	)

	if _, err := reconciler.Reconcile(context.Background(), reconcileRequest("c", "default")); err == nil {
		t.Fatal("expected cleanup error")
	}

	if got := testutil.ToFloat64(reconciler.Metrics.CleanupsTotal.WithLabelValues("failure")); got != 1 {
		t.Errorf("expected cleanupsTotal{result=failure}=1, got %v", got)
	}
}

func TestPendingCleanupSkipsMetricsAndEvents(t *testing.T) {
	cluster := newCluster("c", "default", withFinalizer, withDeletionTimestamp)
	secret := newSecret("cloud-credentials", "default")

	registry := prometheus.NewRegistry()
	reconciler, recorder := newObservedReconciler(registry,
		func(_ context.Context, _ openstack.PurgeOptions) error { return cleanup.ErrDeletePending },
		cluster, secret,
	)

	result, err := reconciler.Reconcile(context.Background(), reconcileRequest("c", "default"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Fatalf("expected a 5s requeue, got %s", result.RequeueAfter)
	}
	if got := testutil.ToFloat64(reconciler.Metrics.CleanupsTotal.WithLabelValues("success")); got != 0 {
		t.Errorf("expected no success metric, got %v", got)
	}
	if got := testutil.ToFloat64(reconciler.Metrics.CleanupsTotal.WithLabelValues("failure")); got != 0 {
		t.Errorf("expected no failure metric, got %v", got)
	}
	select {
	case event := <-recorder.Events:
		t.Fatalf("expected no terminal event, got %q", event)
	default:
	}
}

// ── US11.2: Kubernetes Events ───────────────────────────────────────────────

// Scenario: successful cleanup → Normal "CleanupSucceeded" event
func TestEvents_EmitsNormal_OnCleanupSuccess(t *testing.T) {
	cluster := newCluster("c", "default", withFinalizer, withDeletionTimestamp)
	secret := newSecret("cloud-credentials", "default")

	registry := prometheus.NewRegistry()
	reconciler, recorder := newObservedReconciler(registry,
		func(_ context.Context, _ openstack.PurgeOptions) error { return nil },
		cluster, secret,
	)

	if _, err := reconciler.Reconcile(context.Background(), reconcileRequest("c", "default")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "Normal") || !strings.Contains(event, "CleanupSucceeded") {
			t.Errorf("expected Normal/CleanupSucceeded event, got: %q", event)
		}
	default:
		t.Error("no event was recorded")
	}
}

// Scenario: failed purge → Warning "CleanupFailed" event
func TestEvents_EmitsWarning_OnCleanupFailure(t *testing.T) {
	cluster := newCluster("c", "default", withFinalizer, withDeletionTimestamp)
	secret := newSecret("cloud-credentials", "default")

	registry := prometheus.NewRegistry()
	reconciler, recorder := newObservedReconciler(registry,
		func(_ context.Context, _ openstack.PurgeOptions) error { return errors.New("purge failed") },
		cluster, secret,
	)

	if _, err := reconciler.Reconcile(context.Background(), reconcileRequest("c", "default")); err == nil {
		t.Fatal("expected cleanup error")
	}

	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "Warning") || !strings.Contains(event, "CleanupFailed") {
			t.Errorf("expected Warning/CleanupFailed event, got: %q", event)
		}
	default:
		t.Error("no event was recorded")
	}
}
