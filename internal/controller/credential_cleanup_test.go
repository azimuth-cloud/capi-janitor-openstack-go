package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	infrav1 "sigs.k8s.io/cluster-api-provider-openstack/api/v1beta1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/cleanup"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/controller"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack"
)

const credentialYAML = `clouds:
  openstack:
    auth_type: v3applicationcredential
    region_name: region-1
    auth:
      auth_url: https://keystone.example/v3
      application_credential_id: credential-1
      application_credential_secret: test-secret-value
      project_id: project-1
      user_id: user-1
`

type checkpointSession struct {
	binding               openstack.CredentialBinding
	cleanups, deletes     int
	cleanupErr, deleteErr error
	onCleanup, onDelete   func()
}

func (s *checkpointSession) Cleanup(context.Context) error {
	s.cleanups++
	if s.onCleanup != nil {
		s.onCleanup()
	}
	return s.cleanupErr
}

func (s *checkpointSession) Binding() openstack.CredentialBinding { return s.binding }

func (s *checkpointSession) DeleteApplicationCredential(_ context.Context, id string) error {
	if id != s.binding.CredentialID {
		return errors.New("incorrect credential DELETE target")
	}
	s.deletes++
	if s.onDelete != nil {
		s.onDelete()
	}
	return s.deleteErr
}

func credentialFixture() (*infrav1.OpenStackCluster, *corev1.Secret, *checkpointSession) {
	cluster := newCluster("mycluster", "default", withFinalizer, withDeletionTimestamp)
	cluster.UID = "cluster-1"
	secret := newSecret("cloud-credentials", "default")
	secret.UID = "secret-1"
	secret.Annotations = map[string]string{controller.CredentialPolicyAnnotation: controller.PolicyDelete}
	secret.Data["clouds.yaml"] = []byte(credentialYAML)
	session := &checkpointSession{binding: openstack.CredentialBinding{
		Authority: "https://keystone.example/v3", ProjectID: "project-1", UserID: "user-1",
		Region: "region-1", Interface: "public", CloudName: "openstack", CredentialID: "credential-1",
	}}
	return cluster, secret, session
}

func credentialReconciler(c client.Client, session *checkpointSession) *controller.OpenStackClusterReconciler {
	return &controller.OpenStackClusterReconciler{
		Client: c, APIReader: c, Scheme: testScheme,
		NewSession: func(context.Context, openstack.PurgeOptions) (openstack.CleanupSession, error) { return session, nil },
	}
}

func checkpointPhase(t *testing.T, c client.Client) string {
	t.Helper()
	cluster := getClusterOrNil(t, c, "mycluster", "default")
	if cluster == nil || !controllerutil.ContainsFinalizer(cluster, controller.Finalizer) {
		t.Fatal("cleanup released the finalizer prematurely")
	}
	raw := cluster.Annotations[controller.CredentialCheckpointAnnotation]
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "test-secret-value") || strings.Contains(raw, "clouds.yaml") {
		t.Fatal("checkpoint contains secret data")
	}
	var checkpoint struct{ Phase string }
	if err := json.Unmarshal([]byte(raw), &checkpoint); err != nil {
		t.Fatal(err)
	}
	return checkpoint.Phase
}

func requireCredentialPhase(t *testing.T, c client.Client, want string) {
	t.Helper()
	if got := checkpointPhase(t, c); got != want {
		t.Fatalf("phase = %q, want %q", got, want)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "cloud-credentials", Namespace: "default"}, &corev1.Secret{}); err != nil {
		t.Fatalf("credential Secret must remain: %v", err)
	}
}

func advanceCredential(t *testing.T, r *controller.OpenStackClusterReconciler) {
	t.Helper()
	result, err := r.Reconcile(t.Context(), reconcileRequest("mycluster", "default"))
	if err != nil || result.RequeueAfter != 5*time.Second {
		t.Fatalf("advancing checkpoint: result=%+v, error=%v", result, err)
	}
}

func TestCredentialCleanupRequiresCompleteInventoryAndSoleFinalizer(t *testing.T) {
	for _, scenario := range []string{"pending inventory", "failed inventory", "other finalizer", "checkpoint patch failure", "Secret changed during cleanup"} {
		t.Run(scenario, func(t *testing.T) {
			cluster, secret, session := credentialFixture()
			if scenario == "other finalizer" {
				cluster.Finalizers = append(cluster.Finalizers, "capo.example/finalizer")
			}
			c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(cluster, secret).Build()
			var operationClient client.Client = c
			switch scenario {
			case "pending inventory":
				session.cleanupErr = cleanup.ErrDeletePending
			case "failed inventory":
				session.cleanupErr = errors.New("inventory pagination failed")
			case "checkpoint patch failure":
				operationClient = interceptor.NewClient(c, interceptor.Funcs{
					Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
						return errors.New("checkpoint write failed")
					},
				})
			case "Secret changed during cleanup":
				session.onCleanup = func() {
					if err := c.Get(t.Context(), client.ObjectKeyFromObject(secret), secret); err != nil {
						t.Fatal(err)
					}
					secret.Annotations[controller.CredentialPolicyAnnotation] = "keep"
					if err := c.Update(t.Context(), secret); err != nil {
						t.Fatal(err)
					}
				}
			}
			r := credentialReconciler(operationClient, session)
			result, err := r.Reconcile(t.Context(), reconcileRequest(cluster.Name, cluster.Namespace))
			if scenario == "pending inventory" || scenario == "other finalizer" {
				if err != nil || result.RequeueAfter != 5*time.Second {
					t.Fatalf("expected pending cleanup, got %+v, %v", result, err)
				}
			} else if err == nil {
				t.Fatal("failed verification was accepted")
			}
			requireCredentialPhase(t, c, "")
			if session.cleanups != 1 || session.deletes != 0 {
				t.Fatalf("cleanup=%d, DELETE=%d", session.cleanups, session.deletes)
			}
		})
	}
}

func TestCredentialDeleteFailuresRetainCheckpoint(t *testing.T) {
	for _, scenario := range []string{"401", "403", "timeout", "429", "500", "authentication failed", "authenticated scope changed", "lost checkpoint write"} {
		t.Run(scenario, func(t *testing.T) {
			cluster, secret, session := credentialFixture()
			c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(cluster, secret).Build()
			r := credentialReconciler(c, session)
			advanceCredential(t, r)
			r = credentialReconciler(c, session)
			switch scenario {
			case "authentication failed":
				r.NewSession = func(context.Context, openstack.PurgeOptions) (openstack.CleanupSession, error) {
					return nil, errors.New("authentication failed")
				}
			case "authenticated scope changed":
				session.binding.ProjectID = "different-project"
			case "lost checkpoint write":
				r.Client = interceptor.NewClient(c, interceptor.Funcs{
					Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
						return errors.New("checkpoint write failed")
					},
				})
			default:
				session.deleteErr = errors.New(scenario)
			}
			if _, err := r.Reconcile(t.Context(), reconcileRequest(cluster.Name, cluster.Namespace)); err == nil {
				t.Fatal("uncertain credential deletion was accepted")
			}
			requireCredentialPhase(t, c, "credentialDeleteStarted")
			if session.cleanups != 1 {
				t.Fatal("checkpoint replay repeated resource cleanup")
			}
		})
	}
}

func TestCredentialDeleteRechecksInputsBeforeSavingCheckpoint(t *testing.T) {
	for _, scenario := range []string{"Secret policy", "Secret credential data", "pause"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			cluster, secret, session := credentialFixture()
			c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(cluster, secret).Build()
			r := credentialReconciler(c, session)
			advanceCredential(t, r)
			checkpoint := getClusterOrNil(t, c, cluster.Name, cluster.Namespace).Annotations[controller.CredentialCheckpointAnnotation]
			session.onDelete = func() {
				if scenario == "pause" {
					if err := c.Get(ctx, client.ObjectKeyFromObject(cluster), cluster); err != nil {
						t.Fatal(err)
					}
					cluster.Annotations[clusterv1.PausedAnnotation] = ""
					if err := c.Update(ctx, cluster); err != nil {
						t.Fatal(err)
					}
					return
				}
				if err := c.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil {
					t.Fatal(err)
				}
				if scenario == "Secret policy" {
					secret.Annotations[controller.CredentialPolicyAnnotation] = "keep"
				} else {
					secret.Data["clouds.yaml"] = []byte(strings.ReplaceAll(credentialYAML, "credential-1", "credential-2"))
				}
				if err := c.Update(ctx, secret); err != nil {
					t.Fatal(err)
				}
			}

			result, err := r.Reconcile(ctx, reconcileRequest(cluster.Name, cluster.Namespace))
			if scenario == "pause" {
				if err != nil {
					t.Fatalf("pause returned an error: %v", err)
				}
			} else if !apierrors.IsConflict(err) {
				t.Fatalf("changed Secret error = %v, want conflict", err)
			}
			if !result.IsZero() {
				t.Fatalf("checkpoint transition requested a requeue: %+v", result)
			}
			requireCredentialPhase(t, c, "credentialDeleteStarted")
			if got := getClusterOrNil(t, c, cluster.Name, cluster.Namespace).Annotations[controller.CredentialCheckpointAnnotation]; got != checkpoint {
				t.Fatal("checkpoint changed after cleanup inputs changed during credential DELETE")
			}
			if session.cleanups != 1 || session.deletes != 1 {
				t.Fatalf("cleanup=%d, DELETE=%d, want one of each", session.cleanups, session.deletes)
			}
		})
	}
}

func TestCredentialCheckpointRejectsBindingChanges(t *testing.T) {
	for _, phase := range []string{"credentialDeleteStarted", "secretDeleteStarted"} {
		for _, change := range []string{"cluster UID", "cluster name", "Secret UID", "policy", "volumes", "cloud", "authority", "region", "interface", "credential ID", "project", "user"} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				cluster, secret, session := credentialFixture()
				c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(cluster, secret).Build()
				r := credentialReconciler(c, session)
				advanceCredential(t, r)
				if phase == "secretDeleteStarted" {
					advanceCredential(t, r)
				}
				if err := c.Get(t.Context(), client.ObjectKeyFromObject(cluster), cluster); err != nil {
					t.Fatal(err)
				}
				if err := c.Get(t.Context(), client.ObjectKeyFromObject(secret), secret); err != nil {
					t.Fatal(err)
				}
				switch change {
				case "cluster UID":
					cluster.UID = "replacement-cluster"
				case "cluster name":
					cluster.Labels = map[string]string{controller.ClusterNameLabel: "other-cluster"}
				case "Secret UID":
					secret.UID = "replacement-secret"
				case "policy":
					secret.Annotations[controller.CredentialPolicyAnnotation] = "keep"
				case "volumes":
					cluster.Annotations[controller.VolumesPolicyAnnotation] = "keep"
				case "cloud":
					cluster.Spec.IdentityRef.CloudName = "other-cloud"
				default:
					replacement := map[string][2]string{
						"authority": {"keystone.example", "other.example"}, "region": {"region-1", "region-2"},
						"interface": {"auth_type:", "interface: internal\n    auth_type:"}, "credential ID": {"credential-1", "credential-2"},
						"project": {"project-1", "project-2"}, "user": {"user-1", "user-2"},
					}[change]
					secret.Data["clouds.yaml"] = []byte(strings.ReplaceAll(credentialYAML, replacement[0], replacement[1]))
				}
				if err := c.Update(t.Context(), cluster); err != nil {
					t.Fatal(err)
				}
				if err := c.Update(t.Context(), secret); err != nil {
					t.Fatal(err)
				}
				deletes := session.deletes
				if _, err := r.Reconcile(t.Context(), reconcileRequest(cluster.Name, cluster.Namespace)); err == nil {
					t.Fatal("changed binding was accepted")
				}
				requireCredentialPhase(t, c, phase)
				if session.deletes != deletes {
					t.Fatal("changed binding reached credential DELETE")
				}
			})
		}
	}
}

func TestCredentialSecretDeleteAndFinalizerFailuresResumeWithoutAuth(t *testing.T) {
	cluster, secret, session := credentialFixture()
	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(cluster, secret).Build()
	r := credentialReconciler(c, session)
	advanceCredential(t, r)
	advanceCredential(t, r)
	r.NewSession = func(context.Context, openstack.PurgeOptions) (openstack.CleanupSession, error) {
		t.Fatal("Secret phase authenticated")
		return nil, nil
	}
	r.Client = interceptor.NewClient(c, interceptor.Funcs{
		Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			options := (&client.DeleteOptions{}).ApplyOptions(opts)
			if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != secret.UID || options.Preconditions.ResourceVersion == nil {
				t.Fatal("Secret deletion lacks UID/resourceVersion preconditions")
			}
			return errors.New("Secret DELETE failed")
		},
	})
	if _, err := r.Reconcile(t.Context(), reconcileRequest(cluster.Name, cluster.Namespace)); err == nil {
		t.Fatal("Secret DELETE failure was ignored")
	}
	requireCredentialPhase(t, c, "secretDeleteStarted")
	r.Client = c
	advanceCredential(t, r)
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(secret), &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Secret still exists: %v", err)
	}
	r.Client = interceptor.NewClient(c, interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return errors.New("finalizer patch failed")
		},
	})
	if _, err := r.Reconcile(t.Context(), reconcileRequest(cluster.Name, cluster.Namespace)); err == nil {
		t.Fatal("finalizer failure was ignored")
	}
	if got := checkpointPhase(t, c); got != "secretDeleteStarted" {
		t.Fatalf("lost checkpoint: %s", got)
	}
	r.Client = c
	if _, err := r.Reconcile(t.Context(), reconcileRequest(cluster.Name, cluster.Namespace)); err != nil {
		t.Fatal(err)
	}
	if getClusterOrNil(t, c, cluster.Name, cluster.Namespace) != nil {
		t.Fatal("finalizer was not removed after Secret absence")
	}
}

func TestMissingSecretBeforeConfirmedCredentialDeleteBlocks(t *testing.T) {
	cluster, secret, session := credentialFixture()
	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(cluster, secret).Build()
	r := credentialReconciler(c, session)
	advanceCredential(t, r)
	if err := c.Delete(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), reconcileRequest(cluster.Name, cluster.Namespace)); err == nil {
		t.Fatal("missing Secret was treated as successful credential deletion")
	}
	if got := checkpointPhase(t, c); got != "credentialDeleteStarted" {
		t.Fatalf("phase changed to %q", got)
	}
	if session.deletes != 0 {
		t.Fatal("missing Secret reached credential DELETE")
	}
}

func TestEmptyEffectiveNameBlocksBeforeAuthentication(t *testing.T) {
	for _, name := range []string{"", "   "} {
		cluster, secret, session := credentialFixture()
		cluster.Labels = map[string]string{controller.ClusterNameLabel: name}
		c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(cluster, secret).Build()
		r := credentialReconciler(c, session)
		r.NewSession = func(context.Context, openstack.PurgeOptions) (openstack.CleanupSession, error) {
			t.Fatal("empty cluster name reached authentication")
			return nil, nil
		}
		if _, err := r.Reconcile(t.Context(), reconcileRequest(cluster.Name, cluster.Namespace)); err == nil {
			t.Fatal("empty cluster name was accepted")
		}
		requireCredentialPhase(t, c, "")
	}
}

func TestCredentialDeletionRequiresExactOptIn(t *testing.T) {
	for _, value := range []string{"", "keep", "Delete", "delete "} {
		t.Run(value, func(t *testing.T) {
			cluster, secret, session := credentialFixture()
			secret.Annotations[controller.CredentialPolicyAnnotation] = value
			c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(cluster, secret).Build()
			r := credentialReconciler(c, session)
			r.NewSession = func(context.Context, openstack.PurgeOptions) (openstack.CleanupSession, error) {
				t.Fatal("credential deletion started without exact opt-in")
				return nil, nil
			}
			cleanups := 0
			r.CleanupFunc = func(context.Context, openstack.PurgeOptions) error { cleanups++; return nil }
			if _, err := r.Reconcile(t.Context(), reconcileRequest(cluster.Name, cluster.Namespace)); err != nil {
				t.Fatal(err)
			}
			if cleanups != 1 || getClusterOrNil(t, c, cluster.Name, cluster.Namespace) != nil {
				t.Fatal("ordinary cleanup did not finish")
			}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(secret), &corev1.Secret{}); err != nil {
				t.Fatalf("Secret removed without opt-in: %v", err)
			}
		})
	}
}

func TestCredentialCheckpointMalformedStateBlocksWithMissingSecret(t *testing.T) {
	for _, scenario := range []string{"invalid JSON", "unknown version", "unknown phase", "unknown field", "trailing data", "missing binding", "invalid authority", "invalid interface"} {
		t.Run(scenario, func(t *testing.T) {
			cluster, secret, session := credentialFixture()
			c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(cluster, secret).Build()
			r := credentialReconciler(c, session)
			advanceCredential(t, r)
			advanceCredential(t, r)
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(cluster), cluster); err != nil {
				t.Fatal(err)
			}
			raw := cluster.Annotations[controller.CredentialCheckpointAnnotation]
			switch scenario {
			case "invalid JSON":
				raw = "{"
			case "unknown version":
				raw = strings.Replace(raw, `"version":1`, `"version":2`, 1)
			case "unknown phase":
				raw = strings.Replace(raw, "secretDeleteStarted", "complete", 1)
			case "unknown field":
				raw = strings.Replace(raw, `"version":1`, `"extra":true,"version":1`, 1)
			case "trailing data":
				raw += "{}"
			case "missing binding":
				raw = strings.Replace(raw, `"projectID":"project-1"`, `"projectID":""`, 1)
			case "invalid authority":
				raw = strings.Replace(raw, "https://keystone.example/v3", "not-a-url", 1)
			case "invalid interface":
				raw = strings.Replace(raw, `"interface":"public"`, `"interface":"unknown"`, 1)
			}
			cluster.Annotations[controller.CredentialCheckpointAnnotation] = raw
			if err := c.Update(t.Context(), cluster); err != nil {
				t.Fatal(err)
			}
			if err := c.Delete(t.Context(), secret); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(t.Context(), reconcileRequest(cluster.Name, cluster.Namespace)); err == nil {
				t.Fatal("malformed checkpoint released the finalizer")
			}
			got := getClusterOrNil(t, c, cluster.Name, cluster.Namespace)
			if got == nil || !controllerutil.ContainsFinalizer(got, controller.Finalizer) {
				t.Fatal("malformed checkpoint released the finalizer")
			}
		})
	}
}

func TestCredentialCleanupWaitsForSecretFinalizers(t *testing.T) {
	cluster, secret, session := credentialFixture()
	secret.Finalizers = []string{"example.com/secret-protection"}
	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(cluster, secret).Build()
	r := credentialReconciler(c, session)
	advanceCredential(t, r)
	advanceCredential(t, r)
	advanceCredential(t, r)
	advanceCredential(t, r)
	requireCredentialPhase(t, c, "secretDeleteStarted")
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(secret), secret); err != nil {
		t.Fatal(err)
	}
	if secret.DeletionTimestamp.IsZero() {
		t.Fatal("Secret deletion was not requested")
	}
	if session.deletes != 1 || session.cleanups != 1 {
		t.Fatal("waiting for Secret deletion repeated OpenStack operations")
	}
}
