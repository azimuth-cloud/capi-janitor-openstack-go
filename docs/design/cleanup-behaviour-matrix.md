# Cleanup behavior matrix

## How to read this document

This matrix records the behavior of the Python controller at
[`cluster-api-janitor-openstack`](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/tree/44a89539cc902192cce46b93c7b05e20d127dc12)
and the contract for the replacement release.

The Go controller preserves the Python controller's cleanup scope. It may
replace framework-specific mechanisms and must not preserve a failure mode that
removes the finalizer before cleanup is verified. The matrix labels those cases
as safety or correctness corrections. The [roadmap](../../ROADMAP.md) records
what is implemented today.

The decision labels used below are:

- **Keep** means the observable behavior remains the same
- **Native implementation** means the outcome remains the same but the mechanism
  changes to controller-runtime or Gophercloud
- **Safety correction** means the Go controller behaves more conservatively
  without adding a deletion target
- **Correctness correction** fixes a known wrong target or outcome within the
  established resource scope
- **Approved extension** means a compatibility feature has been accepted
  explicitly without adding a deletion target
- **Open decision** means the contract is not settled and blocks the replacement
  release
- **Defer** means the behavior belongs after the replacement release

## Lifecycle and Kubernetes behavior

| Area | Python behavior | Replacement release | Required test |
| --- | --- | --- | --- |
| Watched object | Watches CAPO `OpenStackCluster` events | Native implementation. Preserve the watch through controller-runtime | Creating and updating an `OpenStackCluster` produces the expected reconcile request |
| Finalizer value | Uses `janitor.capi.stackhpc.com` | Keep | An existing Python finalizer is recognized and a new object receives the same value |
| Normal reconcile | Adds the Janitor finalizer and does no OpenStack cleanup | Native implementation. Preserve the outcome | A non-deleting object is patched once and no cloud client is created |
| Deletion trigger | Cleanup starts only when `deletionTimestamp` is set and the Janitor finalizer is present | Keep | No deletion runs before deletion starts or when the finalizer is absent |
| Finalizer update | Patches the finalizer list and retries selected API errors | Native implementation. Use a conflict-safe metadata patch | A concurrent metadata update is not lost |
| Cluster name | Uses `cluster.x-k8s.io/cluster-name`, falling back to `OpenStackCluster.metadata.name` | Keep | Label present, label absent, and empty or near-match names are covered |
| Missing Secret | Logs the error, skips OpenStack cleanup, and can remove the finalizer | Safety correction. Keep the finalizer and report a blocked cleanup | A missing Secret never causes cleanup success or finalizer removal |
| Paused objects | No explicit CAPI pause handling | Safety correction. Honor pause on the Cluster or OpenStackCluster | A paused object performs no cleanup and retains the finalizer |
| Watch filter | No standard CAPI watch filter | Safety correction. Add the standard filter without changing ownership rules | When configured, only labeled objects are reconciled |
| Retry mechanism | Sleeps and writes a random retry annotation | Native implementation. Use returned errors and `RequeueAfter` | Expected waiting returns `RequeueAfter` and failures return an error without annotation churn |
| Status | Does not own a Janitor status API | Keep. Do not write CAPO-owned status or conditions | Reconcile leaves `OpenStackCluster.status` unchanged |

## Identity and OpenStack client behavior

| Area | Python behavior | Replacement release | Required test |
| --- | --- | --- | --- |
| Identity source | Reads the same-namespace Secret named by `spec.identityRef.name` | Keep | A Secret in the object namespace is read and a same-named Secret elsewhere is ignored |
| Secret data | Decodes the Kubernetes API representation of `clouds.yaml` and optional `cacert` | Native implementation. Pass the already-decoded `Secret.Data` bytes directly | Raw Secret bytes reach the parser exactly once |
| Cloud name | Uses `identityRef.cloudName`, then legacy `spec.cloudName`, then `openstack` | Keep `identityRef.cloudName` with `openstack` default for the selected CAPO API. Legacy API support depends on the supported-version matrix | Explicit and default cloud names select the expected entry |
| Authentication | Supports only `v3applicationcredential` | Approved extension. Support `v3applicationcredential` and `v3password`; reject every other auth type | Both supported methods authenticate through Gophercloud. All other auth types are rejected without making a deletion request |
| Password credential cleanup | Not supported | Approved extension. Password authentication has no application credential ID, so the Keystone credential deletion stage does not apply | Resource cleanup completes without creating an Identity client or attempting application credential deletion |
| Region | Uses `region_name` from the selected `clouds.yaml` entry | Keep | Only the configured region endpoint is selected |
| Interface | Uses the configured interface and defaults to `public` | Keep | Explicit and default interface selection are covered |
| TLS verification | Uses `verify` from `clouds.yaml` and loads optional CA data from the Secret | Keep through Gophercloud transport configuration | Default verification, custom CA, and invalid CA cases are covered |
| Token handling | Uses a hand-written token and catalog client | Native implementation. Use Gophercloud authentication and reauthentication | An expired token is reauthenticated and the request is retried through Gophercloud |
| Pagination | Iterates resource lists across returned pages | Keep through Gophercloud pagers | A matching resource on a later page is found |
| `ClusterIdentity` | Not supported by the Python controller | Defer | The replacement controller rejects or reports the unsupported type without deletion |
| IdentityRef region override | Not used by the Python controller | Defer until a separate compatibility decision | Setting only the IdentityRef region does not silently widen the replacement contract |

## Resource cleanup

| Area | Python behavior | Replacement release | Required test |
| --- | --- | --- | --- |
| Floating IPs | Always selects matching OCCM service floating IPs and requests deletion | Keep | Matching descriptions are deleted and near-matches remain |
| Service load balancer gate | Deletes matching service load balancers only when `spec.apiServerLoadBalancer.enabled` is true and `status.apiServerLoadBalancer.id` is non-empty | [Open decision](#open-replacement-decisions). API server load balancer status is not ownership proof, and incomplete Octavia inventory is never success | Cover all four enabled and ID combinations, conflicting shared-LB tags, and a credential without Octavia list permission |
| Service load balancers | Selects names beginning with `kube_service_<cluster>_` and requests cascade deletion when the gate is true | Keep the legacy prefix. Allow cascade only under the replacement policy and when supported ownership data has no conflict | Matching names are deleted, while near-matches, API server load balancers, and conflicting shared load balancers remain |
| Service security groups | Always selects matching service load balancer security groups | Keep | Matching descriptions are deleted and near-matches remain |
| Default volume policy | Defaults to `delete` through `CAPI_JANITOR_DEFAULT_VOLUMES_POLICY` | Keep | Default and configured operator policy are covered |
| Per-cluster volume policy | `janitor.capi.stackhpc.com/volumes-policy` overrides the default. Only the exact value `delete` enables deletion | Keep | `delete`, `keep`, empty, and unknown values are covered |
| Snapshots | Deletes snapshots with exact Cinder cluster metadata when volume deletion is enabled | Keep | Matching snapshots are deleted only under the delete policy |
| Volumes | Deletes volumes with exact Cinder cluster metadata when volume deletion is enabled | Keep | Matching volumes are deleted only under the delete policy |
| Per-volume keep | Keeps a volume only when `janitor.capi.azimuth-cloud.com/keep` is exactly `true` | Keep | Exact `true` is kept. Missing, differently cased, and other values follow existing delete behavior |
| Snapshot keep | No independent snapshot keep property is implemented. A related volume keep value does not protect the snapshot | Keep. Do not invent a snapshot keep rule during replacement work | A matching snapshot is still selected when volume policy is `delete`, including when a related volume is kept |
| Resource order | Requests FIP, load balancer, security group, snapshot, and volume deletion in that order, then verifies the selected kinds | Safety correction. Keep the order but wait for a dependency to disappear before a dependent phase runs | A later dependent phase does not run before the earlier dependency is absent |
| Verification | Re-lists resource kinds for which at least one delete was attempted and retries while matches remain | Safety correction. Verify all required phases through level-based observation | Finalizer removal is blocked while any required match remains |
| Empty inventory | Completes a resource phase when no matching candidate exists | Keep | A cluster with no candidates progresses without delete requests |

## Application credential and Secret cleanup

| Area | Python behavior | Replacement release | Required test |
| --- | --- | --- | --- |
| Credential policy source | Reads `janitor.capi.stackhpc.com/credential-policy` from the credential Secret | Keep | Missing, `delete`, and other values are covered |
| Last-finalizer rule | Attempts credential and Secret deletion only when the policy is `delete` and the Janitor finalizer is the only finalizer | Keep | Other finalizers cause waiting and do not delete the credential or Secret |
| Credential ID | Reads the application credential ID from the hard-coded `openstack` entry even when another cloud was selected | Correctness correction. Read the ID from the selected cloud entry | A non-default selected cloud uses its own application credential ID |
| Credential order | Attempts application credential deletion after other resources have been checked | Safety correction. Add a persisted resources-verified checkpoint | A restart before and after the credential request converges safely |
| Credential `403` | Warns and continues because a restricted credential may not delete itself | Keep as a narrow exception and report that the credential may remain | `403` records a Warning Event or equivalent outcome and does not turn other resource failures into success |
| Other credential errors | Retries and retains the finalizer | Native implementation. Preserve the outcome | Non-`403` errors block Secret and finalizer removal |
| Secret deletion | Deletes the Secret after the credential phase when policy and finalizer rules permit it | Keep | Secret deletion occurs only after the resources-verified checkpoint |
| Authentication failure before verification | Can be treated as an already-deleted credential when credential deletion was requested | Safety correction. Only accept missing credentials after a persisted resources-verified checkpoint | Invalid credentials before the checkpoint retain the finalizer |
| Authentication failure after verification | The Python controller intends this to represent an already-deleted credential | Keep the intended outcome using the checkpoint | A restart after credential deletion can remove the Secret and finalizer safely |

## Error handling

| Area | Python behavior | Replacement release | Required test |
| --- | --- | --- | --- |
| Delete response `400` or `409` | Logs and retries after checking whether the resource remains | Native implementation. Classify through Gophercloud and requeue | Conflict or transient delete state does not remove the finalizer |
| Resource still present | Retries after a short delay | Native implementation. Return `RequeueAfter` | No sleep occurs inside reconcile |
| Other OpenStack error | Retries after the configured default delay | Native implementation. Return the error for workqueue backoff | Error is returned and finalizer remains |
| Kubernetes finalizer patch conflict | Retries | Native implementation. Preserve the outcome through patch conflict handling | Reconcile succeeds after a simulated conflict |
| Kubernetes object not found | Treats the object as already gone | Keep | NotFound returns success |
| Optional service clients | Creates load balancer and volume clients even when their policy gate is false | Safety correction. Create a service client only when that cleanup phase needs it | A disabled phase does not require an unrelated service endpoint |
| Missing required service | Raises a catalog error | Keep the outcome when the corresponding cleanup phase is enabled | A missing required service blocks completion |
| Partial inventory failure | The successful path assumes listing completed | Safety correction. Any inventory failure blocks completion | A later-page or service list failure retains the finalizer |
| Request timeout | Uses a 60-second client timeout after [PR #234](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/234) | [Open decision](#open-replacement-decisions) | A slow request on each side of the supported threshold has a deterministic outcome |

## Explicitly deferred work

The following may be useful later, but adding them to the replacement release
would make its role larger than the Python controller's:

- deleting networks, subnets, routers, ports, servers, or keypairs
- deleting infrastructure managed by CAPO
- supporting additional OCCM or Azimuth naming conventions
- supporting `ClusterIdentity`
- supporting token, federation, or other authentication methods beyond application credential and password
- replacing the existing ownership selectors with tag-based discovery
- adding a Janitor status CRD
- changing the finalizer value
- taking responsibility for general CI project cleanup

After the replacement release is stable, consider each item separately. It must
not enter the replacement contract as an undocumented convenience.

## Open replacement decisions

- **Service load balancers:** Python
  [PR #261](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/261)
  shows that the API server load balancer gate can leak OCCM resources. The
  replacement must remove that coupling, fail closed on Octavia inventory
  errors, and block cascade deletion when shared load balancer tags conflict.
  Any permission-only skip requires an explicit policy and Warning Event.
- **Request timeout:** define and test one supported threshold. The Python fix
  uses 60 seconds; the current implementation difference is tracked in the
  roadmap.

Implementation gaps for decisions that are already settled, including
credential `403` reporting and auth-type enforcement, remain in the
[roadmap](../../ROADMAP.md#1-close-behavior-gaps).
