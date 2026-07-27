# Cleanup behaviour matrix


## How to read this document

This matrix records the behaviour of the Python controller at
[`cluster-api-janitor-openstack`](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/tree/44a89539cc902192cce46b93c7b05e20d127dc12)
and the decision for the first Go release.

The rewrite keeps the same cleanup role. 
It does not have to keep framework specific mechanisms such as `Kopf` retry annotations, and it should not preserve a failure mode that can remove the finalizer before cleanup has been checked. 
Those cases are called out as intentional safety corrections.

The decisions used below are:

- **Keep** means the observable behaviour remains the same
- **Keep, native implementation** means the outcome remains the same but the mechanism changes to controller-runtime or Gophercloud
- **Safety correction** means the Go controller behaves more conservatively without adding a new deletion target
- **Defer** means the behaviour is not part of the initial rewrite

## Lifecycle and Kubernetes behaviour

| Area | Python behaviour | First Go release | Required test |
| --- | --- | --- | --- |
| Watched object | Watches CAPO `OpenStackCluster` events | Keep, using a controller-runtime watch | Creating and updating an `OpenStackCluster` produces the expected reconcile request |
| Finalizer value | Uses `janitor.capi.stackhpc.com` | Keep | An existing Python finalizer is recognised and a new object receives the same value |
| Normal reconcile | Adds the Janitor finalizer and does no OpenStack cleanup | Keep, native implementation | A non-deleting object is patched once and no cloud client is created |
| Deletion trigger | Cleanup starts only when `deletionTimestamp` is set and the Janitor finalizer is present | Keep | No deletion runs before deletion starts or when the finalizer is absent |
| Finalizer update | Patches the finalizer list and retries selected API errors | Keep outcome, use a conflict-safe metadata patch | A concurrent metadata update is not lost |
| Cluster name | Uses `cluster.x-k8s.io/cluster-name`, falling back to `OpenStackCluster.metadata.name` | Keep | Label present, label absent, and empty or near-match names are covered |
| Missing Secret | Logs the error, skips OpenStack cleanup, and can remove the finalizer | Safety correction. Keep the finalizer and report a blocked cleanup | A missing Secret never causes cleanup success or finalizer removal |
| Paused objects | No explicit CAPI pause handling | Safety correction. Honour pause on the Cluster or OpenStackCluster | A paused object performs no cleanup and retains the finalizer |
| Watch filter | No standard CAPI watch filter | Add the standard filter as an operational safeguard without changing ownership rules | When configured, only labelled objects are reconciled |
| Retry mechanism | Sleeps and writes a random retry annotation | Keep outcome, use returned errors and `RequeueAfter` | Expected waiting returns `RequeueAfter` and failures return an error without annotation churn |
| Status | Does not own a Janitor status API | Keep. Do not write CAPO-owned status or conditions | Reconcile leaves `OpenStackCluster.status` unchanged |

## Identity and OpenStack client behaviour

| Area | Python behaviour | First Go release | Required test |
| --- | --- | --- | --- |
| Identity source | Reads the same-namespace Secret named by `spec.identityRef.name` | Keep | A Secret in the object namespace is read and a same-named Secret elsewhere is ignored |
| Secret data | Decodes the Kubernetes API representation of `clouds.yaml` and optional `cacert` | Keep the resulting bytes. In Go, `Secret.Data` is already decoded | Raw Secret bytes reach the parser exactly once |
| Cloud name | Uses `identityRef.cloudName`, then legacy `spec.cloudName`, then `openstack` | Keep `identityRef.cloudName` with `openstack` default for the selected CAPO API. Legacy API support depends on the supported-version matrix | Explicit and default cloud names select the expected entry |
| Authentication | Supports only `v3applicationcredential` | Keep | Other auth types are rejected without making a deletion request |
| Region | Uses `region_name` from the selected `clouds.yaml` entry | Keep | Only the configured region endpoint is selected |
| Interface | Uses the configured interface and defaults to `public` | Keep | Explicit and default interface selection are covered |
| TLS verification | Uses `verify` from `clouds.yaml` and loads optional CA data from the Secret | Keep through Gophercloud transport configuration | Default verification, custom CA, and invalid CA cases are covered |
| Token handling | Uses a hand-written token and catalog client | Keep outcome, replace with Gophercloud authentication and reauthentication | An expired token is reauthenticated and the request is retried through Gophercloud |
| Pagination | Iterates resource lists across returned pages | Keep through Gophercloud pagers | A matching resource on a later page is found |
| `ClusterIdentity` | Not supported by the Python controller | Defer | The initial controller rejects or reports the unsupported type without deletion |
| Additional auth methods | Not supported | Defer | Password, token, and other auth types do not enable cleanup |
| IdentityRef region override | Not used by the Python controller | Defer until a separate compatibility decision | Setting only the IdentityRef region does not silently widen the initial support contract |

## Resource cleanup

| Area | Python behaviour | First Go release | Required test |
| --- | --- | --- | --- |
| Floating IPs | Always selects matching OCCM service floating IPs and requests deletion | Keep | Matching descriptions are deleted and near-matches remain |
| Service load balancer gate | Deletes matching service load balancers only when `spec.apiServerLoadBalancer.enabled` is true and `status.apiServerLoadBalancer.id` is non-empty | Keep for parity | All four enabled and ID combinations are covered |
| Service load balancers | Selects names beginning with `kube_service_<cluster>_` and requests cascade deletion when the gate is true | Keep | Matching names are deleted, near-matches and API server LBs remain |
| Service security groups | Always selects matching service load balancer security groups | Keep | Matching descriptions are deleted and near-matches remain |
| Default volume policy | Defaults to `delete` through `CAPI_JANITOR_DEFAULT_VOLUMES_POLICY` | Keep | Default and configured operator policy are covered |
| Per-cluster volume policy | `janitor.capi.stackhpc.com/volumes-policy` overrides the default. Only the exact value `delete` enables deletion | Keep | `delete`, `keep`, empty, and unknown values are covered |
| Snapshots | Deletes snapshots with exact Cinder cluster metadata when volume deletion is enabled | Keep | Matching snapshots are deleted only under the delete policy |
| Volumes | Deletes volumes with exact Cinder cluster metadata when volume deletion is enabled | Keep | Matching volumes are deleted only under the delete policy |
| Per-volume keep | Keeps a volume only when `janitor.capi.azimuth-cloud.com/keep` is exactly `true` | Keep | Exact `true` is kept. Missing, differently cased, and other values follow existing delete behaviour |
| Snapshot keep | No independent snapshot keep property is implemented. A related volume keep value does not protect the snapshot | Keep. Do not invent a snapshot keep rule during parity work | A matching snapshot is still selected when volume policy is `delete`, including when a related volume is kept |
| Resource order | Requests FIP, load balancer, security group, snapshot, and volume deletion in that order, then verifies the selected kinds | Safety correction. Keep the order but wait for a dependency to disappear before a dependent phase runs | A later dependent phase does not run before the earlier dependency is absent |
| Verification | Re-lists resource kinds for which at least one delete was attempted and retries while matches remain | Keep and strengthen through level-based observation | Finalizer removal is blocked while any required match remains |
| Empty inventory | Completes a resource phase when no matching candidate exists | Keep | A cluster with no candidates progresses without delete requests |

## Application credential and Secret cleanup

| Area | Python behaviour | First Go release | Required test |
| --- | --- | --- | --- |
| Credential policy source | Reads `janitor.capi.stackhpc.com/credential-policy` from the credential Secret | Keep | Missing, `delete`, and other values are covered |
| Last-finalizer rule | Attempts credential and Secret deletion only when the policy is `delete` and the Janitor finalizer is the only finalizer | Keep | Other finalizers cause waiting and do not delete the credential or Secret |
| Credential ID | Reads the application credential ID from the hard-coded `openstack` entry even when another cloud was selected | Correctness correction. Read the ID from the selected cloud entry | A non-default selected cloud uses its own application credential ID |
| Credential order | Attempts application credential deletion after other resources have been checked | Keep and add a persisted resources-verified checkpoint | A restart before and after the credential request converges safely |
| Credential `403` | Warns and continues because a restricted credential may not delete itself | Keep as a narrow, documented exception | `403` records a warning and does not turn other resource failures into success |
| Other credential errors | Retries and retains the finalizer | Keep, native implementation | Non-`403` errors block Secret and finalizer removal |
| Secret deletion | Deletes the Secret after the credential phase when policy and finalizer rules permit it | Keep | Secret deletion occurs only after the resources-verified checkpoint |
| Authentication failure before verification | Can be treated as an already-deleted credential when credential deletion was requested | Safety correction. Only accept missing credentials after a persisted resources-verified checkpoint | Invalid credentials before the checkpoint retain the finalizer |
| Authentication failure after verification | The Python controller intends this to represent an already-deleted credential | Keep the intended outcome using the checkpoint | A restart after credential deletion can remove the Secret and finalizer safely |

## Error handling

| Area | Python behaviour | First Go release | Required test |
| --- | --- | --- | --- |
| Delete response `400` or `409` | Logs and retries after checking whether the resource remains | Keep outcome, classify through Gophercloud and requeue | Conflict or transient delete state does not remove the finalizer |
| Resource still present | Retries after a short delay | Keep outcome with `RequeueAfter` | No sleep occurs inside reconcile |
| Other OpenStack error | Retries after the configured default delay | Keep outcome through returned error and workqueue backoff | Error is returned and finalizer remains |
| Kubernetes finalizer patch conflict | Retries | Keep outcome through patch conflict handling | Reconcile succeeds after a simulated conflict |
| Kubernetes object not found | Treats the object as already gone | Keep | NotFound returns success |
| Optional service clients | Creates load balancer and volume clients even when their policy gate is false | Correctness correction. Create a service client only when that cleanup phase needs it | A disabled phase does not require an unrelated service endpoint |
| Missing required service | Raises a catalog error | Keep the outcome when the corresponding cleanup phase is enabled | A missing required service blocks completion |
| Partial inventory failure | The successful path assumes listing completed | Make the requirement explicit. Any inventory failure blocks completion | A later-page or service list failure retains the finalizer |

## Explicitly deferred work

The following may be useful later, but adding them to the initial Go controller
would make the rewrite larger than the Python role:

- deleting networks, subnets, routers, ports, servers, or keypairs
- deleting infrastructure managed by CAPO
- supporting additional OCCM or Azimuth naming conventions
- supporting `ClusterIdentity`
- supporting password, token, federation, or other authentication methods
- replacing the existing ownership selectors with tag-based discovery
- adding a Janitor status CRD
- changing the finalizer value
- taking responsibility for general CI project cleanup

After the Go rewrite is stable, each item can be considered separately. It must
not enter the parity release as an undocumented convenience.

## Open questions to resolve in this PR

The Python README says that Octavia load balancers are always cleaned up, while
the implementation gates their deletion on the CAPO API server load balancer
state. This matrix follows the implementation for initial parity. The
maintainers should confirm that choice before implementation begins.

The Python implementation deliberately continues after a `403` from
application credential self deletion. This matrix retains that behaviour for
compatibility. The event wording and operator documentation should make it
clear that the credential may remain in OpenStack.
