# Cluster API Janitor for OpenStack

Cluster API Janitor for OpenStack is a Kubernetes controller that removes
OpenStack resources left by workload cloud controllers during deletion of a
Cluster API Provider OpenStack (CAPO) cluster.

It cleans resources created by the
[OpenStack Cloud Controller Manager (OCCM)](https://github.com/kubernetes/cloud-provider-openstack)
and Cinder CSI. Workload-created resources can block deletion of a CAPO-managed
network after the workload controllers stop.

> [!WARNING]
> The Go controller is a pre-v1 rewrite intended to replace the
> [Python controller](https://github.com/azimuth-cloud/cluster-api-janitor-openstack).
> The active path uses Gophercloud, but restart-safe credential cleanup,
> level-based reconciliation, envtest, destructive boundary tests, and
> migration and rollback evidence remain release blockers. Do not treat the
> current build as a production drop-in replacement. See the
> [roadmap](ROADMAP.md).

The legacy chart and finalizer names are retained so that an eventual migration
does not require rewriting existing `OpenStackCluster` objects.

## Scope

The Janitor watches CAPO `OpenStackCluster` objects. During deletion it selects
only workload resources that match the documented cluster ownership rules.

The cleanup scope consists of OCCM service floating IPs, load balancers, and
security groups; Cinder CSI snapshots and volumes; and, when policy permits,
the cluster application credential and its Secret. Project and region scoping
limit discovery but do not replace the resource selectors. The exact rules,
policy gates, and negative examples are in the
[resource ownership matrix](docs/design/resource-ownership-matrix.md).

CAPO-managed infrastructure remains outside the Janitor's scope. The Janitor is
not a general OpenStack project cleaner. See the
[CAPO integration boundary](docs/design/capo-integration-boundary.md).

## Current behavior

1. The controller adds `janitor.capi.stackhpc.com` to a non-deleting
   `OpenStackCluster`.
2. After deletion starts, it resolves the cluster name from the
   `cluster.x-k8s.io/cluster-name` label, falling back to
   `OpenStackCluster.metadata.name`.
3. It reads the same-namespace Secret referenced by `spec.identityRef` and
   selects the requested `clouds.yaml` entry.
4. It discovers and deletes matching floating IPs, service load balancers when
   the current gate permits, service security groups, Cinder snapshots and
   volumes, and optionally the application credential and Secret.
5. It keeps the finalizer while a required cleanup operation fails or a
   selected resource remains.
6. It removes the finalizer after the current cleanup path reports success.

The current controller still polls and sleeps inside reconcile, uses a retry
annotation, and lacks the persisted credential checkpoint. These are
[replacement blockers](ROADMAP.md#work-to-reach-safe-replacement), not supported
long-term behavior.

### Load balancer limitation

The current code cleans service load balancers only when the CAPO API server
load balancer is enabled and its status ID is present. That gate can leave OCCM
service load balancers behind on clusters that do not use a CAPO API server load
balancer. Removing it without accounting for OCCM shared load balancers and
reserved tags can make cascade deletion too broad. The replacement release must
resolve both sides and keep Octavia inventory errors fatal. See the
[load balancer release decision](docs/design/cleanup-behaviour-matrix.md#open-replacement-decisions).

## Identity and cluster name

Each `OpenStackCluster` must reference a Secret in the same namespace. The
Secret must contain `clouds.yaml` and may contain `cacert` for a custom CA.
`spec.identityRef.cloudName` selects an entry; the default is `openstack`.

The replacement contract covers Secret-based `v3applicationcredential` and
`v3password` authentication. `ClusterIdentity`, token, federation, and other
identity sources are not supported. Validation of `identityRef.type` is still a
release blocker, so do not rely on the current pre-release code to reject an
unsupported type safely.

OCCM and Cinder CSI must use the same cluster identifier that the Janitor
resolves. A mismatch leaves resources outside the selector. For ClusterClass
deployments, verify the `cluster.x-k8s.io/cluster-name` label and pass the same
cluster name to OCCM and Cinder CSI.

Do not add credentials to a release bundle or commit them to this repository.

## Cleanup policy

### Volumes

Cinder volume and snapshot deletion is enabled by default. Set an operator-wide
default through Helm:

```sh
helm upgrade cluster-api-janitor-openstack \
  cluster-api-janitor-openstack/cluster-api-janitor-openstack \
  --set defaultVolumesPolicy=keep
```

Override the policy on one `OpenStackCluster`:

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: OpenStackCluster
metadata:
  name: example
  annotations:
    janitor.capi.stackhpc.com/volumes-policy: keep
```

Only the exact cluster policy value `delete` enables Cinder cleanup. When it is
enabled, a volume is preserved only when this metadata value is exactly `true`:

```sh
openstack volume set \
  --property janitor.capi.azimuth-cloud.com/keep=true \
  <volume-id>
```

There is no separate snapshot keep property in the Python compatibility
contract.

### Application credential and Secret

Credential cleanup is opt-in. Annotate the referenced Secret:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: example-cloud-credentials
  annotations:
    janitor.capi.stackhpc.com/credential-policy: delete
```

The controller attempts application credential and Secret deletion only when
the Janitor finalizer is the last finalizer. Password authentication has no
application credential to delete. A restricted application credential may
receive `403` when it tries to delete itself; the compatibility behavior can
leave that credential in Keystone. Clear reporting of that outcome is still an
open release item.

### Current retry setting

The pre-release Helm chart exposes `retryDefaultDelay`, with a default of 60
seconds, through `CAPI_JANITOR_RETRY_DEFAULT_DELAY`. The target controller uses
`RequeueAfter` and controller-runtime backoff instead of blocking sleeps and
random retry annotations. Treat this setting as transitional.

| Environment variable | Default | Current use |
| --- | --- | --- |
| `CAPI_JANITOR_DEFAULT_VOLUMES_POLICY` | `delete` | Operator-wide volume policy |
| `CAPI_JANITOR_RETRY_DEFAULT_DELAY` | `60` | Delay used by the pre-release retry implementation |

## Compatibility

The repository currently builds against Go 1.26, CAPO `v0.14.6` and its
`v1beta1` API, CAPI `v1.12.8`, Kubernetes libraries `v0.36.2`, and
controller-runtime `v0.24.1`. This is a dependency snapshot, not a tested
cluster compatibility matrix. The intended lanes are defined by the
[version strategy](docs/design/capo-integration-boundary.md#version-strategy),
and the work needed to claim support is tracked in the [roadmap](ROADMAP.md).

## Installation

Use the published Helm repository only for development and controlled
pre-release evaluation at this stage:

```sh
helm repo add \
  cluster-api-janitor-openstack \
  https://azimuth-cloud.github.io/capi-janitor-openstack-go

helm repo update

helm upgrade cluster-api-janitor-openstack \
  cluster-api-janitor-openstack/cluster-api-janitor-openstack \
  --install
```

Do not run the Python and Go controllers against the same objects. A tested
migration and rollback runbook is a release gate and is not available yet.

## Observability

The controller registers these signals:

| Signal | Labels or type | Current meaning |
| --- | --- | --- |
| `capi_janitor_cleanups_total` | `result="success|failure"` | Cleanup calls reported by the current controller |
| `CleanupSucceeded` | Normal Event | The purge returned success |
| `CleanupFailed` | Warning Event | The purge returned an error |

The Helm chart does not currently enable or expose the metrics endpoint. The
Kustomize deployment has metrics configuration. Lifecycle-safe metric and
Event timing, including a credential-may-remain outcome, is tracked in the
roadmap.

## Development

The design documents are normative. The [roadmap](ROADMAP.md) records current
implementation status, and [CONTRIBUTING.md](CONTRIBUTING.md) explains the
destructive-change review rules.

```sh
go build ./...
go test ./...
go vet ./...
```

Useful Make targets:

```sh
make help
make fmt
make vet
make test
make build
make test-e2e
make build-installer IMG=<registry>/<image>:<tag>
```

`make test` runs unit tests, HTTP fixtures, and fake-client controller tests.
`make test-e2e` is intended as a Kind manager and metrics smoke suite, but its
current image reference mismatch must be fixed first. Neither command meets the
envtest or real OpenStack evidence required for the replacement release; the
exact gaps are tracked in the
[roadmap](ROADMAP.md#4-prove-the-controller-boundary).

### Nix build

CI builds the manager, multi-architecture OCI image, tests, and CycloneDX SBOM
with Nix:

```sh
nix-build nix -A tests
nix-build nix -A manager
nix-build nix -A image
nix-build nix -A image-arm64
nix-build nix -A sbom
```

`vendorHash` pins Go module input. `nix/nixpkgs.nix` currently fetches the
mutable `nixos-26.05` branch, so the complete build input is not pinned to one
nixpkgs revision. The image build sets `CGO_ENABLED=0`; it does not depend on the
glibc linkage described in older versions of this README.

## Repository layout

```text
cmd/                         manager entry point
internal/controller/         Kubernetes reconciliation and policy resolution
internal/cleanup/            target controller-independent cleanup boundary
internal/openstack/          auth, active purge path, and legacy HTTP path
internal/openstack/*/         typed Gophercloud resource services
chart/                       Helm chart and snapshot tests
config/                      Kustomize manager, RBAC, metrics, and network policy
nix/                         manager, OCI image, test, and SBOM builds
test/e2e/                    Kind manager and metrics smoke scaffold
docs/design/                 behavior, ownership, controller, and CAPO boundaries
```

Release tags, artifacts, and publication order are documented in
[Releasing and versioning](docs/releasing.md).
