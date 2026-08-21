# cluster-api-janitor-openstack

`cluster-api-janitor-openstack` is a Kubernetes operator that cleans up resources
created in [OpenStack](https://www.openstack.org/) by the
[OpenStack Cloud Controller Manager (OCCM)](https://github.com/kubernetes/cloud-provider-openstack/blob/master/docs/openstack-cloud-controller-manager/using-openstack-cloud-controller-manager.md)
and the
[Cinder CSI plugin](https://github.com/kubernetes/cloud-provider-openstack/blob/master/docs/cinder-csi-plugin/using-cinder-csi-plugin.md)
for Kubernetes clusters created with the
[Cluster API OpenStack infrastructure provider (CAPO)](https://github.com/kubernetes-sigs/cluster-api-provider-openstack).

The operator watches `OpenStackCluster` resources and, upon deletion, removes any dangling OpenStack resources (floating IPs, load balancers, security groups, and Cinder volumes and snapshots) that would otherwise be left behind after the CAPI cluster is gone.
It can also remove a cluster application credential when the identity and cleanup policy permit it.

> [!IMPORTANT]
> This repository is still working toward the replacement release criteria.
> The [roadmap](ROADMAP.md) tracks implementation status.
> The [Python compatibility policy](docs/design/python-compatibility-policy.md) defines the settled target contract.
> A documented target is not a claim that the current runtime path already implements it.

## Current build inputs

The Kubernetes, CAPI, and CAPO support matrix has not yet been validated for a replacement release.
The current source builds against these versions:

| Dependency | Current version |
|---|---|
| Go | 1.26 |
| Kubernetes Go modules | 0.36.2 |
| CAPO API module | 0.14.6 |
| controller-runtime | 0.24.1 |
| Helm | 3.x |

## How it works

This is the target replacement lifecycle.
The roadmap lists the steps that are not yet active in the production path.

1. When an `OpenStackCluster` is created, the operator adds its finalizer
   (`janitor.capi.stackhpc.com`) to the resource.
2. When the `OpenStackCluster` is marked for deletion (`deletionTimestamp` set), the operator authenticates to OpenStack using the credential referenced by `spec.identityRef` and selects owned resources using the documented name, description, and metadata rules.
3. The cluster name is taken from the `cluster.x-k8s.io/cluster-name` label if
   present, falling back to `metadata.name`.
4. After the required OpenStack resources are verified absent, the operator applies the configured application-credential and Secret policy.
5. The finalizer is removed only after every required phase completes. Expected OpenStack waits are requeued; failures retain the finalizer.

> **Why a finalizer instead of a post-delete job?**
>
> Some OCCM-created load balancers hold references to the cluster network, which
> prevents the Cluster API OpenStack provider from deleting that network. Running
> cleanup *before* the network is torn down (but *after* all machines are gone)
> avoids this deadlock and eliminates any race with a still-running OCCM.

## Resources cleaned up

| OpenStack service | Resources |
|---|---|
| Neutron | Floating IPs associated with Kubernetes Services of type `LoadBalancer` |
| Octavia | Load balancers with name prefix `kube_service_<cluster>_`, except LBs shared with another cluster |
| Neutron | Security groups matching the OCCM naming convention |
| Cinder | Volumes provisioned by the Cinder CSI (configurable — see below) |
| Cinder | Snapshots carrying the matching Cinder cluster metadata |
| Keystone | The application credential used by the cluster (if authorized) |

## Configuration

### Volume deletion policy

Cinder volumes are deleted by default. This can be changed at two levels:

**Operator-wide default** (via Helm):

```sh
helm upgrade ... --set defaultVolumesPolicy=keep
```

**Per-cluster override** (annotation on `OpenStackCluster`):

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: OpenStackCluster
metadata:
  name: my-cluster
  annotations:
    janitor.capi.stackhpc.com/volumes-policy: "keep"   # or "delete"
```

> Any value other than `delete` means volumes will be kept.

**Per-volume override** (set directly on the OpenStack volume):

```sh
openstack volume set --property janitor.capi.azimuth-cloud.com/keep=true <volume>
```

Any value other than `true` does not opt the volume out.
It remains eligible when the cluster volume policy is `delete` and the ownership metadata matches.
This property does not protect snapshots with matching cluster metadata.

### Retry delay

Operational failures use per-object exponential backoff starting at one second.
The configurable value is the maximum delay and defaults to 60 seconds.
Expected deletion waits use a short requeue and do not block a controller worker.
The configured value must be a positive integer number of seconds.

```sh
helm upgrade ... --set retryDefaultDelay=120
```

### Environment variables

| Variable | Default | Description |
|---|---|---|
| `CAPI_JANITOR_DEFAULT_VOLUMES_POLICY` | `delete` | Operator-wide volume policy |
| `CAPI_JANITOR_RETRY_DEFAULT_DELAY` | `60` | Maximum error-backoff delay in seconds |

## Installation

### OpenStack credentials

The release bundle does not include OpenStack credentials.
The replacement profile reads the direct Secret referenced by `spec.identityRef` in the `OpenStackCluster` namespace; the Secret contains `clouds.yaml` and may contain a custom `cacert`.
`spec.identityRef.cloudName` selects the cloud entry, while the `openstack` default is limited to the Python migration path.

The target contract supports `v3applicationcredential` and project-scoped password authentication.
The [compatibility policy](docs/design/python-compatibility-policy.md#approved-password-extension) defines the accepted password forms and validation rules.

For a direct application-credential identity, the Secret annotation `janitor.capi.stackhpc.com/credential-policy: delete` opts into deletion of the application credential and referenced Secret after other resources are gone and the Janitor finalizer is last.
Missing or any other value keeps both.

The identity source and authentication type determine the lifecycle:

| Identity | OpenStack resource cleanup | Credential and Secret cleanup |
| --- | --- | --- |
| Direct application-credential Secret | Supported | Exact `credential-policy: delete` opts in after restart-safe verification |
| Direct password Secret | Supported | Keystone user, password, and Secret are always retained |
| `identityRef.type: ClusterIdentity` community profile | Available only after its separate compatibility gate passes | Identity, backing Secret, and credential are always retained |

The delete annotation declares ownership of a direct application-credential Secret and must not be set on a shared Secret.
It is ignored with a warning for password and community identity inputs.

Shared load balancer protection is defined by the [ownership matrix](docs/design/resource-ownership-matrix.md#shared-load-balancer-floating-ip).
The compatibility policy defines [Octavia outcomes](docs/design/python-compatibility-policy.md#octavia-capability-and-error-policy) and [credential recovery](docs/design/python-compatibility-policy.md#retry-and-recovery).

Do not add credentials to the release bundle or commit them to the repository.

```sh
helm repo add \
  cluster-api-janitor-openstack \
  https://azimuth-cloud.github.io/capi-janitor-openstack-go

helm upgrade \
  cluster-api-janitor-openstack \
  cluster-api-janitor-openstack/cluster-api-janitor-openstack \
  --install
```

## Development

Release tags, artifact version mapping, the pre-`v1` stability notice, and the
publication workflow are documented in [Releasing and versioning](docs/releasing.md).

### Build

```sh
go build ./...
```

### Run tests

```sh
go test ./...
```

Current test evidence and the gaps that aggregate coverage cannot close are tracked in the [roadmap](ROADMAP.md#final-result).

### Lint and format

```sh
go fmt ./...
go vet ./...
```

### Makefile targets

```sh
make help          # list all targets
make generate      # regenerate DeepCopy methods
make manifests     # regenerate CRD/RBAC YAML
make fmt           # go fmt
make vet           # go vet
make test          # go test (excludes e2e)
make build         # go build ./cmd/main.go
```

## Building the OCI image

### Nix (multi-arch + SBOM)

CI uses `nix-build` for Nix-based builds.
The `tests` derivation runs `go fmt`, `go vet`, and the full unit-test suite inside the Nix sandbox — no external toolchain needed:

```sh
# CI check: go fmt + go vet + unit tests
nix-build nix -A tests

# Build the manager binary only
nix-build nix -A manager

# Build the amd64 OCI image
nix-build nix -A image

# Build the arm64 OCI image (cross-compiled from amd64)
nix-build nix -A image-arm64

# Generate the CycloneDX SBOM
nix-build nix -A sbom
```

> **`nix/nixpkgs.nix`** currently follows the moving `nixos-26.05` branch; an immutable revision and hash are a release-hardening task.
>
> **`vendorHash`** in `nix/default.nix` pins the Go dependency source (run `nix-build nix -A manager` after any `go.mod` change — the build will fail and print the new hash to substitute).

## Observability

The manager exposes metrics and emits Kubernetes Events; deployment-path parity remains a [release task](ROADMAP.md#replacement-release-work).

### Prometheus metrics

| Metric | Labels | Description |
|---|---|---|
| `capi_janitor_cleanups_total` | `result="success\|failure"` | Total cleanup attempts |

### Kubernetes events

| Reason | Type | Emitted when |
|---|---|---|
| `CleanupSucceeded` | Normal | OpenStack purge completed successfully |
| `CleanupFailed` | Warning | OpenStack purge returned an error |

## Project layout

```
cmd/                        # Operator entry point
internal/
  cleanup/                  # Cleanup policy, outcomes, and service interfaces
  controller/               # Reconciler, metrics, config
  openstack/                # Authentication and legacy cleanup path
    network/                # Typed Neutron service
    loadbalancer/           # Typed Octavia service
    volume/                 # Typed Cinder service
    identity/               # Typed Keystone service
chart/                      # Helm chart
  templates/
  tests/                    # helm-unittest tests
nix/                        # Nix-based OCI build + SBOM (no Flake)
config/                     # Kustomize bases (RBAC, manager, Prometheus)
test/e2e/                   # End-to-end test suite (Ginkgo)
```
