//go:build envtest

package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	infrav1 "sigs.k8s.io/cluster-api-provider-openstack/api/v1beta1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/controller"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack"
)

const envtestCredentialYAML = `clouds:
  openstack:
    auth_type: v3applicationcredential
    auth:
      auth_url: https://keystone.example/v3
      application_credential_id: credential-1
      application_credential_secret: test-only-secret
      project_id: project-1
      user_id: user-1
`

type envtestCredentialSession struct {
	cleanups *int
	deletes  *[]string
}

func (s *envtestCredentialSession) Cleanup(context.Context) error {
	*s.cleanups += 1
	return nil
}

func (s *envtestCredentialSession) Binding() openstack.CredentialBinding {
	return openstack.CredentialBinding{
		Authority: "https://keystone.example/v3", ProjectID: "project-1", UserID: "user-1",
		Interface: "public", CloudName: "openstack", CredentialID: "credential-1",
	}
}

func (s *envtestCredentialSession) DeleteApplicationCredential(_ context.Context, id string) error {
	*s.deletes = append(*s.deletes, id)
	return nil
}

type envtestBeforeDeleteClient struct {
	client.Client
	beforeDelete func(context.Context) error
}

func (c *envtestBeforeDeleteClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if c.beforeDelete != nil {
		beforeDelete := c.beforeDelete
		c.beforeDelete = nil
		if err := beforeDelete(ctx); err != nil {
			return err
		}
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func testEnvtestCredentialCleanup(t *testing.T, apiClient client.Client) {
	for _, scenario := range []string{
		"restart at each phase", "checkpoint write fails after DELETE", "Secret replacement",
		"Secret replacement during DELETE", "Secret update during DELETE",
	} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			namespace := envtestNamespace(t, apiClient)
			cluster := newCluster("credential-cleanup", namespace, withFinalizer)
			secret := newSecret("cloud-credentials", namespace)
			secret.Annotations = map[string]string{controller.CredentialPolicyAnnotation: controller.PolicyDelete}
			secret.Data["clouds.yaml"] = []byte(envtestCredentialYAML)
			if err := apiClient.Create(ctx, secret); err != nil {
				t.Fatal(err)
			}
			if err := apiClient.Create(ctx, cluster); err != nil {
				t.Fatal(err)
			}
			if err := apiClient.Delete(ctx, cluster); err != nil {
				t.Fatal(err)
			}
			key := client.ObjectKeyFromObject(cluster)
			secretKey := client.ObjectKeyFromObject(secret)
			var cleanups, sessions int
			var deletes []string
			authFailure := errors.New("application credential no longer authenticates")
			authBroken := false
			// Recreate the reconciler on every call to test recovery from the saved checkpoint.
			reconcile := func(c client.Client) (ctrl.Result, error) {
				r := &controller.OpenStackClusterReconciler{
					Client: c, APIReader: apiClient, Scheme: testScheme,
					NewSession: func(context.Context, openstack.PurgeOptions) (openstack.CleanupSession, error) {
						sessions++
						if authBroken {
							return nil, authFailure
						}
						return &envtestCredentialSession{cleanups: &cleanups, deletes: &deletes}, nil
					},
				}
				return r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			}
			assertPhase := func(want string) {
				t.Helper()
				var current infrav1.OpenStackCluster
				if err := apiClient.Get(ctx, key, &current); err != nil {
					t.Fatal(err)
				}
				if !controllerutil.ContainsFinalizer(&current, controller.Finalizer) {
					t.Fatal("Janitor finalizer was removed before completion")
				}
				checkpoint := current.Annotations[controller.CredentialCheckpointAnnotation]
				var state struct{ Phase string }
				if err := json.Unmarshal([]byte(checkpoint), &state); err != nil {
					t.Fatalf("reading checkpoint: %v", err)
				}
				if state.Phase != want {
					t.Fatalf("checkpoint phase = %q, want %q", state.Phase, want)
				}
				if strings.Contains(checkpoint, "test-only-secret") || strings.Contains(checkpoint, "clouds.yaml") {
					t.Fatal("checkpoint contains credential data")
				}
			}
			if _, err := reconcile(apiClient); err != nil {
				t.Fatalf("saving initial checkpoint: %v", err)
			}
			assertPhase("credentialDeleteStarted")
			if cleanups != 1 || len(deletes) != 0 {
				t.Fatalf("checkpoint was not saved before DELETE: cleanups=%d, deletes=%v", cleanups, deletes)
			}
			if scenario == "checkpoint write fails after DELETE" {
				conflictingClient := &envtestConflictingClient{Client: apiClient, beforePatch: func(ctx context.Context) error {
					var current infrav1.OpenStackCluster
					if err := apiClient.Get(ctx, key, &current); err != nil {
						return err
					}
					current.Labels = map[string]string{"example.com/concurrent": "preserved"}
					return apiClient.Update(ctx, &current)
				}}
				if _, err := reconcile(conflictingClient); !apierrors.IsConflict(err) {
					t.Fatalf("checkpoint write error = %v, want actual API conflict", err)
				}
				assertPhase("credentialDeleteStarted")
				authBroken = true
				if _, err := reconcile(apiClient); !errors.Is(err, authFailure) {
					t.Fatalf("restart error = %v, want authentication failure", err)
				}
				assertPhase("credentialDeleteStarted")
				if err := apiClient.Get(ctx, secretKey, &corev1.Secret{}); err != nil {
					t.Fatalf("Secret removed after unrecorded credential deletion: %v", err)
				}
				if len(deletes) != 1 || deletes[0] != "credential-1" {
					t.Fatalf("credential DELETE requests = %v", deletes)
				}
				return
			}
			if _, err := reconcile(apiClient); err != nil {
				t.Fatalf("deleting credential: %v", err)
			}
			assertPhase("secretDeleteStarted")
			if err := apiClient.Get(ctx, secretKey, &corev1.Secret{}); err != nil {
				t.Fatalf("Secret removed before recorded phase: %v", err)
			}
			if len(deletes) != 1 || deletes[0] != "credential-1" || sessions != 2 {
				t.Fatalf("credential requests: deletes=%v sessions=%d", deletes, sessions)
			}
			authBroken = true
			if scenario == "Secret replacement during DELETE" || scenario == "Secret update during DELETE" {
				var concurrentSecret corev1.Secret
				racingClient := &envtestBeforeDeleteClient{Client: apiClient, beforeDelete: func(ctx context.Context) error {
					if err := apiClient.Get(ctx, secretKey, &concurrentSecret); err != nil {
						return err
					}
					if scenario == "Secret replacement during DELETE" {
						if err := apiClient.Delete(ctx, &concurrentSecret); err != nil {
							return err
						}
						concurrentSecret.UID = ""
						concurrentSecret.ResourceVersion = ""
						return apiClient.Create(ctx, &concurrentSecret)
					}
					concurrentSecret.Annotations["example.com/concurrent"] = "preserved"
					return apiClient.Update(ctx, &concurrentSecret)
				}}
				if _, err := reconcile(racingClient); !apierrors.IsConflict(err) {
					t.Fatalf("Secret DELETE error = %v, want API precondition conflict", err)
				}
				var current corev1.Secret
				if err := apiClient.Get(ctx, secretKey, &current); err != nil {
					t.Fatalf("concurrently changed Secret was removed: %v", err)
				}
				if current.UID != concurrentSecret.UID || current.ResourceVersion != concurrentSecret.ResourceVersion {
					t.Fatal("Secret DELETE changed the concurrently modified Secret")
				}
				if scenario == "Secret replacement during DELETE" && current.UID == secret.UID {
					t.Fatal("race did not create a replacement Secret UID")
				}
				assertPhase("secretDeleteStarted")
			} else if scenario == "Secret replacement" {
				oldUID := secret.UID
				if err := apiClient.Delete(ctx, secret); err != nil {
					t.Fatal(err)
				}
				replacement := newSecret(secret.Name, namespace)
				replacement.Annotations = secret.Annotations
				replacement.Data = secret.Data
				if err := apiClient.Create(ctx, replacement); err != nil {
					t.Fatal(err)
				}
				if replacement.UID == oldUID {
					t.Fatal("replacement Secret reused the previous UID")
				}
				if _, err := reconcile(apiClient); err == nil {
					t.Fatal("replacement Secret did not block cleanup")
				}
				var current corev1.Secret
				if err := apiClient.Get(ctx, secretKey, &current); err != nil || current.UID != replacement.UID {
					t.Fatalf("replacement Secret was modified: UID=%s, err=%v", current.UID, err)
				}
				assertPhase("secretDeleteStarted")
			} else {
				if _, err := reconcile(apiClient); err != nil {
					t.Fatalf("deleting recorded Secret after restart: %v", err)
				}
				if err := apiClient.Get(ctx, secretKey, &corev1.Secret{}); !apierrors.IsNotFound(err) {
					t.Fatalf("Secret GET = %v, want NotFound", err)
				}
				assertPhase("secretDeleteStarted")
				if err := apiClient.Get(ctx, key, cluster); err != nil {
					t.Fatal(err)
				}
				cluster.Annotations[clusterv1.PausedAnnotation] = ""
				if err := apiClient.Update(ctx, cluster); err != nil {
					t.Fatal(err)
				}
				if _, err := reconcile(apiClient); err != nil {
					t.Fatal(err)
				}
				assertPhase("secretDeleteStarted")
				delete(cluster.Annotations, clusterv1.PausedAnnotation)
				if err := apiClient.Update(ctx, cluster); err != nil {
					t.Fatal(err)
				}
				if _, err := reconcile(apiClient); err != nil {
					t.Fatalf("finishing cleanup after restart: %v", err)
				}
				if err := apiClient.Get(ctx, key, &infrav1.OpenStackCluster{}); !apierrors.IsNotFound(err) {
					t.Fatalf("OpenStackCluster GET = %v, want NotFound after finalizer removal", err)
				}
			}
			if sessions != 2 || len(deletes) != 1 || cleanups != 1 {
				t.Fatalf("Secret/finalizer phase contacted OpenStack: sessions=%d deletes=%v cleanups=%d", sessions, deletes, cleanups)
			}
		})
	}
}
