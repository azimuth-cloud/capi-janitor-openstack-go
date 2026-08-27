# cluster-api-janitor-openstack

`cluster-api-janitor-openstack` is a Kubernetes operator that cleans up resources
created in [OpenStack](https://www.openstack.org/) by the
[OpenStack Cloud Controller Manager (OCCM)](https://github.com/kubernetes/cloud-provider-openstack/blob/master/docs/openstack-cloud-controller-manager/using-openstack-cloud-controller-manager.md)
and the
[Cinder CSI plugin](https://github.com/kubernetes/cloud-provider-openstack/blob/master/docs/cinder-csi-plugin/using-cinder-csi-plugin.md)
for Kubernetes clusters created with the
[Cluster API OpenStack infrastructure provider (CAPO)](https://github.com/kubernetes-sigs/cluster-api-provider-openstack).

The operator watches `OpenStackCluster` resources in every namespace and removes dangling OpenStack resources when deletion starts.
These resources include floating IPs, load balancers, security groups, Cinder volumes, and Cinder snapshots.
It can also remove the cluster application credential when the referenced Secret opts into credential deletion.

> [!IMPORTANT]
> This repository is still working toward the replacement release criteria.
> The [roadmap](ROADMAP.md) tracks implementation status.
> The [Python compatibility policy](docs/design/python-compatibility-policy.md) defines the settled target contract.
> A documented target is not a claim that the current runtime path already implements it.

## Current build inputs

The Kubernetes, CAPI, and CAPO support matrix has not yet been validated for a replacement release.
The current source builds against these versions:

| Dependency            | Current version |
| --------------------- | --------------- |
| Go                    | 1.26            |
| Kubernetes Go modules | 0.36.2          |
| CAPO API module       | 0.14.6          |
| controller-runtime    | 0.24.1          |
| Helm                  | 3.x             |

## How it works

This is the target replacement lifecycle.
The roadmap lists the steps that are not yet active in the production path.

1. When an `OpenStackCluster` is created, the operator adds its finalizer
   (`janitor.capi.stackhpc.com`) to the resource.
2. When the `OpenStackCluster` is marked for deletion (`deletionTimestamp` set), the operator authenticates to OpenStack using the credential referenced by `spec.identityRef` and selects owned resources using the documented name, description, and metadata rules.
3. The cluster name is taken from the `cluster.x-k8s.io/cluster-name` label if
   present, falling back to `metadata.name`.
4. After the required OpenStack resources are verified absent, the operator applies the configured application credential and Secret policy.
5. The finalizer is removed only after every required phase completes.
   Expected OpenStack waits are requeued.
   Failures retain the finalizer.

> **Why a finalizer instead of a cleanup job?**
>
> Some load balancers created by OCCM hold references to the cluster network, which prevents the Cluster API OpenStack provider from deleting that network.
> Cleanup runs _before_ the network is torn down and _after_ all machines are gone.
> This avoids the deadlock and eliminates any race with OCCM while it is still running.

## Resources cleaned up

| OpenStack service | Resources                                                                                         |
| ----------------- | ------------------------------------------------------------------------------------------------- |
| Neutron           | Floating IPs associated with Kubernetes Services of type `LoadBalancer`                           |
| Octavia           | Load balancers with name prefix `kube_service_<cluster>_`, except LBs shared with another cluster |
| Neutron           | Security groups matching the OCCM naming convention                                               |
| Cinder            | Volumes provisioned by the Cinder CSI (configurable, see below)                                   |
| Cinder            | Snapshots carrying the matching Cinder cluster metadata                                           |
| Keystone          | The application credential used by the cluster (if authorized)                                    |

A matching load balancer with one or more reserved OCCM tags remains eligible for deletion when every reserved tag belongs to the target cluster, including when several Services in that cluster share it.
A foreign or malformed reserved tag preserves the load balancer and its VIP floating IP.
When a complete tag result has no reserved OCCM tag, the legacy name selector still determines eligibility.
An unavailable Octavia endpoint or an incomplete inventory blocks cleanup and retains the finalizer.
A complete inventory with no matching load balancer completes normally.
See the [shared load balancer ownership rules](docs/design/resource-ownership-matrix.md#shared-load-balancer-floating-ip) and [Octavia error handling](docs/design/python-compatibility-policy.md#octavia-capability-and-error-handling).

## Configuration

### Volume deletion policy

Cinder volumes are deleted by default. This can be changed at two levels:

**Operator default** (via Helm):

```sh
helm upgrade ... --set defaultVolumesPolicy=keep
```

**Cluster override** (annotation on `OpenStackCluster`):

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: OpenStackCluster
metadata:
  name: my-cluster
  annotations:
    janitor.capi.stackhpc.com/volumes-policy: 'keep' # or "delete"
```

> Any value other than `delete` means volumes will be kept.

**Volume override** (set directly on the OpenStack volume):

```sh
openstack volume set --property janitor.capi.azimuth-cloud.com/keep=true <volume>
```

Any value other than `true` does not opt the volume out.
It remains eligible when the cluster volume policy is `delete` and the ownership metadata matches.
This property does not protect snapshots with matching cluster metadata.

### Retry delay

Operational failures use exponential workqueue backoff for each object, starting at one second.
The configurable value is the maximum delay and defaults to 60 seconds.
Expected deletion waits use a short requeue and do not block a controller worker.
The configured value must be a positive integer number of seconds.

```sh
helm upgrade ... --set retryDefaultDelay=120
```

### Environment variables

| Variable                              | Default  | Description                                |
| ------------------------------------- | -------- | ------------------------------------------ |
| `CAPI_JANITOR_DEFAULT_VOLUMES_POLICY` | `delete` | Default volume policy for the operator     |
| `CAPI_JANITOR_RETRY_DEFAULT_DELAY`    | `60`     | Maximum workqueue backoff delay in seconds |

## Installation

### OpenStack credentials

The release bundle does not include OpenStack credentials.
The replacement release supports CAPO `OpenStackCluster` `v1beta1` objects that reference a direct Secret in the same namespace through `spec.identityRef`.
The Secret must contain `clouds.yaml` and may contain a custom `cacert`.
`spec.identityRef.cloudName` selects the cloud entry.
For an existing object that Kubernetes accepted before `cloudName` became required, an empty value uses `openstack` for migration compatibility.
The selected cloud must use explicit `v3applicationcredential` authentication.
Other identity sources, authentication types, and API versions are outside the replacement release.

The Secret annotation `janitor.capi.stackhpc.com/credential-policy: delete` opts into deletion of the selected application credential and the referenced Secret.
Deletion starts only after a fresh, complete inventory shows that the other owned resources are absent and the Janitor finalizer is the only finalizer.
A missing annotation or any value other than the exact value `delete` keeps the application credential and Secret.
The annotation declares ownership of the direct Secret and must not be set on a shared Secret.
If the controller cannot confirm credential deletion, it keeps the Secret and finalizer for recovery.
See the [application credential cleanup policy](docs/design/python-compatibility-policy.md#application-credential-cleanup) for the full deletion and recovery rules.

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

Release tags, artifact version mapping, the stability notice before `v1`, and the publication workflow are documented in [Releasing and versioning](docs/releasing.md).

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

### Nix builds for multiple architectures and SBOM

CI uses `nix-build` for Nix builds.
The `tests` derivation runs `go fmt`, `go vet`, and the full unit test suite inside the Nix sandbox.
It does not require an external toolchain.

```sh
# CI check: go fmt + go vet + unit tests
nix-build nix -A tests

# Build the manager binary only
nix-build nix -A manager          # amd64
nix-build nix -A manager-arm64    # arm64 (cross-compiled from amd64)

# Build the OCI images
nix-build nix -A image            # amd64
nix-build nix -A image-arm64      # arm64 (cross-compiled from amd64)

# Build the arm64 OCI image from amd64
nix-build nix -A image-arm64

# Generate the CycloneDX SBOM
nix-build nix -A sbom
nix-build nix -A sbom-arm64
```

Each image is a `docker-archive` tarball tagged with its own `architecture`. CI
pushes both under temporary `<sha>-<arch>` tags and joins them into a
`linux/amd64` + `linux/arm64` manifest list. To check a build locally:

```sh
skopeo inspect --config docker-archive:image-arm64.tar.gz | jq .architecture
```

> **`nix/nixpkgs.nix`** currently follows the moving `nixos-26.05` branch.
> Pinning an immutable revision and hash remains release work.
>
> **`vendorHash`** in `nix/default.nix` pins the Go dependency source.
> Run `nix-build nix -A manager` after a `go.mod` change.
> The build will fail and print the new hash to use.

Both binaries are static (`CGO_ENABLED=0`), so the images contain no libc. The
arm64 build overrides `GOARCH` instead of using `pkgsCross`, which would link
against the target glibc and add ~50 MB to the image.

## Observability

The manager exposes metrics and emits Kubernetes Events.
Deployment parity remains a [release task](ROADMAP.md#replacement-release-work).

### Prometheus metrics

| Metric                        | Labels                      | Description            |
| ----------------------------- | --------------------------- | ---------------------- |
| `capi_janitor_cleanups_total` | `result="success\|failure"` | Total cleanup attempts |

### Kubernetes events

| Reason             | Type    | Emitted when                           |
| ------------------ | ------- | -------------------------------------- |
| `CleanupSucceeded` | Normal  | OpenStack purge completed successfully |
| `CleanupFailed`    | Warning | OpenStack purge returned an error      |

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
nix/                        # Nix OCI build + SBOM (no Flake)
config/                     # Kustomize bases (RBAC, manager, Prometheus)
test/e2e/                   # Full workflow test suite (Ginkgo)
```
