package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	infrav1 "sigs.k8s.io/cluster-api-provider-openstack/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/cleanup"
	"github.com/azimuth-cloud/capi-janitor-openstack-go/internal/openstack"
)

// CredentialCheckpointAnnotation records credential and Secret deletion progress.
const CredentialCheckpointAnnotation = "janitor.capi.stackhpc.com/credential-cleanup"

const (
	credentialDeleteStarted = "credentialDeleteStarted"
	secretDeleteStarted     = "secretDeleteStarted"
)

var errPaused = errors.New("cleanup is paused")

// The checkpoint contains identifiers and policies. Credentials stay in the Secret.
type credentialCheckpoint struct {
	Version          int                         `json:"version"`
	Phase            string                      `json:"phase"`
	ClusterUID       types.UID                   `json:"clusterUID"`
	ClusterName      string                      `json:"clusterName"`
	SecretName       string                      `json:"secretName"`
	SecretUID        types.UID                   `json:"secretUID"`
	Cloud            openstack.CredentialBinding `json:"cloud"`
	VolumesPolicy    string                      `json:"volumesPolicy"`
	CredentialPolicy string                      `json:"credentialPolicy"`
}

func readCredentialCheckpoint(cluster *infrav1.OpenStackCluster) (*credentialCheckpoint, error) {
	data, exists := cluster.Annotations[CredentialCheckpointAnnotation]
	if !exists {
		return nil, nil
	}
	var state credentialCheckpoint
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return nil, errors.New("credential cleanup checkpoint is invalid")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("credential cleanup checkpoint contains trailing data")
	}
	switch {
	case state.Version != 1:
		return nil, errors.New("unsupported credential checkpoint version")
	case state.Phase != credentialDeleteStarted && state.Phase != secretDeleteStarted:
		return nil, errors.New("unsupported credential checkpoint phase")
	case state.ClusterUID == "" || state.ClusterName == "":
		return nil, errors.New("credential checkpoint is missing cluster identity")
	case state.SecretUID == "" || state.SecretName == "":
		return nil, errors.New("credential checkpoint is missing Secret identity")
	case state.CredentialPolicy != PolicyDelete:
		return nil, errors.New("credential checkpoint requires delete policy")
	case state.VolumesPolicy != PolicyDelete && state.VolumesPolicy != "keep":
		return nil, errors.New("unsupported checkpoint volumes policy")
	}
	if err := openstack.ValidateCredentialBinding(state.Cloud); err != nil {
		return nil, fmt.Errorf("invalid credential checkpoint cloud binding: %w", err)
	}
	return &state, nil
}

func (r *OpenStackClusterReconciler) reconcileDelete(ctx context.Context, cluster *infrav1.OpenStackCluster) (ctrl.Result, error) {
	if strings.TrimSpace(clusterNameFor(cluster)) == "" {
		return ctrl.Result{}, errors.New("cluster name is empty")
	}
	if identityType := cluster.Spec.IdentityRef.Type; identityType != "" && identityType != "Secret" {
		return ctrl.Result{}, fmt.Errorf("unsupported identity reference type %q", identityType)
	}
	if cluster.Spec.IdentityRef.Name == "" {
		return ctrl.Result{}, errors.New("identity Secret name is empty")
	}
	state, err := readCredentialCheckpoint(cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	secret, err := r.findSecret(ctx, cluster.Spec.IdentityRef.Name, cluster.Namespace)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("fetching identity secret: %w", err)
	}
	if state != nil {
		return r.resumeCredentialCleanup(ctx, cluster, secret, state)
	}
	if secret == nil {
		return ctrl.Result{}, fmt.Errorf("identity Secret %q not found", cluster.Spec.IdentityRef.Name)
	}
	options := r.purgeOptions(cluster, secret)
	if secret.Annotations[CredentialPolicyAnnotation] != PolicyDelete {
		if err := r.cleanResources(ctx, options); err != nil {
			return ctrl.Result{}, fmt.Errorf("cleaning OpenStack resources: %w", err)
		}
		return r.finishCleanup(ctx, cluster, secret, false)
	}

	session, err := r.newCleanupSession(ctx, options)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := session.Cleanup(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("verifying resources before credential deletion: %w", err)
	}
	state = &credentialCheckpoint{
		Version: 1, Phase: credentialDeleteStarted,
		ClusterUID: cluster.UID, ClusterName: clusterNameFor(cluster),
		SecretName: secret.Name, SecretUID: secret.UID, Cloud: session.Binding(),
		VolumesPolicy: r.volumesPolicyFor(cluster), CredentialPolicy: PolicyDelete,
	}
	if state.ClusterUID == "" || state.SecretUID == "" || openstack.ValidateCredentialBinding(state.Cloud) != nil {
		return ctrl.Result{}, errors.New("cannot bind credential cleanup to an incomplete identity")
	}
	if err := r.checkCheckpointBinding(cluster, secret, state); err != nil {
		return ctrl.Result{}, err
	}
	return r.saveCredentialCheckpoint(ctx, cluster, secret, state)
}

func (r *OpenStackClusterReconciler) resumeCredentialCleanup(ctx context.Context, cluster *infrav1.OpenStackCluster, secret *corev1.Secret, state *credentialCheckpoint) (ctrl.Result, error) {
	if err := r.checkCheckpointBinding(cluster, secret, state); err != nil {
		return ctrl.Result{}, err
	}
	if len(cluster.Finalizers) != 1 {
		return ctrl.Result{}, cleanup.ErrDeletePending
	}
	if state.Phase == credentialDeleteStarted {
		session, err := r.newCleanupSession(ctx, r.purgeOptions(cluster, secret))
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("resuming credential deletion: %w", err)
		}
		if session.Binding() != state.Cloud {
			return ctrl.Result{}, errors.New("authenticated credential binding changed after deletion started")
		}
		if _, err := r.checkCleanupInputs(ctx, cluster, secret, true); err != nil {
			return ctrl.Result{}, err
		}
		// Advance only after DELETE returns 204 or 404 for the recorded credential.
		// If the response or checkpoint update is lost, retry may fail to authenticate.
		// That failure leaves the checkpoint unchanged.
		if err := session.DeleteApplicationCredential(ctx, state.Cloud.CredentialID); err != nil {
			return ctrl.Result{}, fmt.Errorf("confirming application credential deletion: %w", err)
		}
		state.Phase = secretDeleteStarted
		return r.saveCredentialCheckpoint(ctx, cluster, secret, state)
	}

	// Credential deletion is confirmed. Resume without authenticating to OpenStack.
	if secret == nil {
		return r.finishCleanup(ctx, cluster, nil, true)
	}
	if !secret.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, cleanup.ErrDeletePending
	}
	if _, err := r.checkCleanupInputs(ctx, cluster, secret, true); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Delete(ctx, secret, client.Preconditions{
		UID: &secret.UID, ResourceVersion: &secret.ResourceVersion,
	}); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("deleting recorded identity Secret: %w", err)
	}
	// Observe absence and patch the finalizer in a later reconciliation.
	return ctrl.Result{RequeueAfter: pendingDeleteDelay}, nil
}

func (r *OpenStackClusterReconciler) checkCheckpointBinding(cluster *infrav1.OpenStackCluster, secret *corev1.Secret, state *credentialCheckpoint) error {
	if cluster.UID != state.ClusterUID || clusterNameFor(cluster) != state.ClusterName ||
		cluster.Spec.IdentityRef.Name != state.SecretName || selectedCloudName(cluster) != state.Cloud.CloudName ||
		r.volumesPolicyFor(cluster) != state.VolumesPolicy {
		return errors.New("cluster binding changed after credential deletion started")
	}
	if secret == nil {
		if state.Phase != secretDeleteStarted {
			return errors.New("identity Secret is missing before its recorded deletion phase")
		}
		return nil
	}
	if secret.UID != state.SecretUID || secret.Annotations[CredentialPolicyAnnotation] != state.CredentialPolicy {
		return errors.New("identity Secret binding or credential policy changed after deletion started")
	}
	config, err := openstack.DescribeCredential(r.purgeOptions(cluster, secret))
	if err != nil {
		return fmt.Errorf("checking recorded credential configuration: %w", err)
	}
	if config.Authority != state.Cloud.Authority || config.Region != state.Cloud.Region ||
		config.Interface != state.Cloud.Interface || config.CloudName != state.Cloud.CloudName ||
		config.CredentialID != state.Cloud.CredentialID ||
		(config.ProjectID != "" && config.ProjectID != state.Cloud.ProjectID) ||
		(config.UserID != "" && config.UserID != state.Cloud.UserID) {
		return errors.New("cloud binding changed after credential deletion started")
	}
	return nil
}

func (r *OpenStackClusterReconciler) saveCredentialCheckpoint(ctx context.Context, observed *infrav1.OpenStackCluster, secret *corev1.Secret, state *credentialCheckpoint) (ctrl.Result, error) {
	latest, err := r.checkCleanupInputs(ctx, observed, secret, true)
	if err != nil {
		return ctrl.Result{}, err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("encoding credential cleanup checkpoint: %w", err)
	}
	base := latest.DeepCopy()
	if latest.Annotations == nil {
		latest.Annotations = make(map[string]string)
	}
	latest.Annotations[CredentialCheckpointAnnotation] = string(data)
	if err := r.Patch(ctx, latest, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return ctrl.Result{}, fmt.Errorf("saving credential cleanup checkpoint: %w", err)
	}
	return ctrl.Result{RequeueAfter: pendingDeleteDelay}, nil
}

// checkCleanupInputs detects input changes before a DELETE or checkpoint update.
func (r *OpenStackClusterReconciler) checkCleanupInputs(ctx context.Context, observed *infrav1.OpenStackCluster, secret *corev1.Secret, soleFinalizer bool) (*infrav1.OpenStackCluster, error) {
	// Read the Secret before checking the latest cluster scope and pause state.
	currentSecret, err := r.findSecret(ctx, observed.Spec.IdentityRef.Name, observed.Namespace)
	if err != nil {
		return nil, err
	}
	if (secret == nil) != (currentSecret == nil) ||
		(secret != nil && (currentSecret.UID != secret.UID || currentSecret.ResourceVersion != secret.ResourceVersion)) {
		return nil, apierrors.NewConflict(corev1.Resource("secrets"), observed.Spec.IdentityRef.Name, errors.New("identity Secret changed during reconciliation"))
	}

	var latest infrav1.OpenStackCluster
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(observed), &latest); err != nil {
		return nil, err
	}
	checkpoint, hasCheckpoint := latest.Annotations[CredentialCheckpointAnnotation]
	observedCheckpoint, hadCheckpoint := observed.Annotations[CredentialCheckpointAnnotation]
	if latest.UID != observed.UID || latest.Generation != observed.Generation ||
		clusterNameFor(&latest) != clusterNameFor(observed) ||
		r.volumesPolicyFor(&latest) != r.volumesPolicyFor(observed) ||
		checkpoint != observedCheckpoint || hasCheckpoint != hadCheckpoint ||
		latest.DeletionTimestamp.IsZero() || !controllerutil.ContainsFinalizer(&latest, Finalizer) {
		return nil, apierrors.NewConflict(infrav1.Resource("openstackclusters"), observed.Name, errors.New("cleanup inputs changed during reconciliation"))
	}
	paused, err := r.isPaused(ctx, &latest)
	if err != nil {
		return nil, err
	}
	if paused {
		return nil, errPaused
	}
	if soleFinalizer && len(latest.Finalizers) != 1 {
		return nil, cleanup.ErrDeletePending
	}

	return &latest, nil
}

func (r *OpenStackClusterReconciler) newCleanupSession(ctx context.Context, options openstack.PurgeOptions) (openstack.CleanupSession, error) {
	if r.NewSession != nil {
		return r.NewSession(ctx, options)
	}
	return openstack.NewCleanupSession(ctx, options)
}

func selectedCloudName(cluster *infrav1.OpenStackCluster) string {
	if cluster.Spec.IdentityRef.CloudName != "" {
		return cluster.Spec.IdentityRef.CloudName
	}
	return "openstack"
}

func (r *OpenStackClusterReconciler) purgeOptions(cluster *infrav1.OpenStackCluster, secret *corev1.Secret) openstack.PurgeOptions {
	return openstack.PurgeOptions{
		CloudsYAML: string(secret.Data["clouds.yaml"]), CACert: string(secret.Data["cacert"]),
		CloudName: selectedCloudName(cluster), ClusterName: clusterNameFor(cluster),
		DeleteVolumes: r.volumesPolicyFor(cluster) == PolicyDelete,
	}
}
