# Cleanup behaviour matrix


## How to read this document

This matrix records the behaviour of the Python controller at
[`cluster-api-janitor-openstack` `0.15.0`](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/tree/f14d86013d78ac3a4b07f5a2669a8f49590e13ca)
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
| Cluster name | Uses `cluster.x-k8s.io/cluster-name`, falling back to `OpenStackCluster.metadata.name` | Keep. Require the same value in OCCM and Cinder CSI, and unique workload cluster names within each OpenStack project | Label present, label absent, mismatch, and duplicate-name risks are covered |
| Missing Secret | Logs the error, skips OpenStack cleanup, and can remove the finalizer | Safety correction. Before the recorded Secret-deletion phase, keep the finalizer and report a blocked cleanup | A missing Secret before `secretDeleteStarted` never causes cleanup success or finalizer removal |
| Paused objects | No explicit CAPI pause handling | Safety correction. Honour pause on the Cluster or OpenStackCluster | A paused object performs no cleanup and retains the finalizer |
| Watch filter | No standard CAPI watch filter | Defer. The initial replacement keeps the all-namespace watch | The initial release has no label-based filtering behavior |
| Retry mechanism | Sleeps and writes a random retry annotation | Keep outcome, use returned errors and `RequeueAfter` | Expected waiting returns `RequeueAfter` and failures return an error without annotation churn |
| Status | Does not own a Janitor status API | Keep. Do not write CAPO-owned status or conditions | Reconcile leaves `OpenStackCluster.status` unchanged |

## Identity and OpenStack client behaviour

| Area | Python behaviour | First Go release | Required test |
| --- | --- | --- | --- |
| Identity source | Reads the same-namespace Secret named by `spec.identityRef.name` | Keep | A Secret in the object namespace is read and a same-named Secret elsewhere is ignored |
| Secret data | Decodes the Kubernetes API representation of `clouds.yaml` and optional `cacert` | Keep the resulting bytes. In Go, `Secret.Data` is already decoded | Raw Secret bytes reach the parser exactly once |
| Cloud name | Uses `identityRef.cloudName`, then legacy `spec.cloudName`, then `openstack` | Use `identityRef.cloudName`; allow `openstack` only for an already-admitted migration object with an empty value. `spec.cloudName` is outside the `v1beta1` contract | Explicit and migration-default cloud names select the expected entry |
| Authentication | Supports only `v3applicationcredential` | Keep | Other auth types are rejected without making a deletion request |
| Region | Uses `region_name` from the selected `clouds.yaml` entry | Keep | Only the configured region endpoint is selected |
| Interface | Uses the configured interface and defaults to `public` | Keep | Explicit and default interface selection are covered |
| TLS verification | Uses `verify` from `clouds.yaml` and loads optional CA data from the Secret | Keep through Gophercloud transport configuration | Default verification, custom CA, and invalid CA cases are covered |
| Token handling | Uses a hand-written token and catalog client | Keep outcome, replace with Gophercloud authentication and reauthentication | An expired token is reauthenticated and the request is retried through Gophercloud |
| Pagination | Iterates resource lists across returned pages | Keep through Gophercloud pagers | A matching resource on a later page is found |
| Unsupported identity types | Not supported by the Python controller | Defer | The initial controller rejects every unsupported identity type without discovery or deletion |
| Additional auth methods | Not supported | Defer | Only explicit `v3applicationcredential` enables cleanup |
| IdentityRef region override | Not used by the Python controller | Defer until a separate compatibility decision | Setting only the IdentityRef region does not silently widen the initial support contract |

## Resource cleanup

| Area | Python behaviour | First Go release | Required test |
| --- | --- | --- | --- |
| Floating IPs | Always selects matching OCCM service floating IPs and requests deletion | Keep | Matching descriptions are deleted and near-matches remain |
| Service load balancer gate | Deletes matching service load balancers only when `spec.apiServerLoadBalancer.enabled` is true and `status.apiServerLoadBalancer.id` is non-empty | Correctness correction. Do not use CAPO API server LB state to gate workload LB cleanup | All enabled and ID combinations produce the same exhaustive workload-LB inventory |
| Service load balancers | Selects names beginning with `kube_service_<cluster>_` and requests cascade deletion | Keep the name selector and use complete OCCM reserved tags as the shared-LB veto | Same-cluster Service tags allow cascade deletion; a foreign or malformed reserved tag preserves the LB and its VIP FIP |
| Shared LB evidence | Does not protect a cross-cluster shared LB | Safety correction. Use OCCM `kube_service_<cluster>_<namespace>_<service>` tags, never pool member names or ports | Incomplete tag or VIP/FIP facts block all mutation; a successfully read empty tag set uses the legacy name selector |
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
| Credential order | Attempts application credential deletion after other resources have been checked | Keep and require fresh complete verification, the last-finalizer rule, and a persisted deletion checkpoint | A restart before or after the credential request resumes the recorded phase or blocks without removing the Secret or finalizer |
| Credential `403` | Warns and continues because a restricted credential may not delete itself | Safety correction. A `403` does not prove deletion; retain the Secret and finalizer | The checkpoint does not advance and cleanup remains blocked |
| Other credential errors | Retries and retains the finalizer | Keep, native implementation | All unconfirmed outcomes block Secret and finalizer removal |
| Secret deletion | Deletes the Secret after the credential phase when policy and finalizer rules permit it | Keep with a UID precondition; absence after `secretDeleteStarted` is idempotent completion | Secret deletion occurs only after confirmed credential absence and targets the recorded Secret UID |
| Authentication failure before verification | Can be treated as an already-deleted credential when credential deletion was requested | Safety correction. Never infer credential absence from an authentication failure | Invalid credentials retain the finalizer; only an exact bound-credential DELETE `404` after `credentialDeleteStarted` proves absence |
| Authentication failure after verification | The Python controller intends this to represent an already-deleted credential | Safety correction. Authentication failure does not prove deletion | Only exact `204` or exact bound-credential DELETE `404` permits Secret deletion |

## Error handling

| Area | Python behaviour | First Go release | Required test |
| --- | --- | --- | --- |
| Delete response `400` or `409` | Logs and retries after checking whether the resource remains | Keep outcome, classify through Gophercloud and requeue | Conflict or transient delete state does not remove the finalizer |
| Resource still present | Retries after a short delay | Keep outcome with `RequeueAfter` | No sleep occurs inside reconcile |
| Other OpenStack error | Retries after the configured default delay | Keep outcome through returned error and workqueue backoff | Error is returned and finalizer remains |
| Kubernetes finalizer patch conflict | Retries | Keep outcome through patch conflict handling | Reconcile succeeds after a simulated conflict |
| Kubernetes object not found | Treats the object as already gone | Keep | NotFound returns success |
| Optional service clients | Creates load balancer and volume clients even when their policy gate is false | Correctness correction. Create a service client only when that cleanup phase needs it | A disabled phase does not require an unrelated service endpoint |
| Missing required service | Raises a catalog error | Keep. A missing or unusable Neutron, enabled Cinder, or Octavia service blocks completion | No missing required service is interpreted as an empty inventory |
| Partial inventory failure | The successful path assumes listing completed | Make the requirement explicit. Any inventory failure blocks completion | A later-page or service list failure retains the finalizer |

## Explicitly deferred work

The following may be useful later, but adding them to the initial Go controller
would make the rewrite larger than the Python role:

- deleting networks, subnets, routers, ports, servers, or keypairs
- deleting infrastructure managed by CAPO
- supporting additional naming conventions
- supporting other identity sources or authentication methods
- replacing the existing ownership selectors with tag-based discovery
- adding a Janitor status CRD
- changing the finalizer value
- taking responsibility for general CI project cleanup

After the Go rewrite is stable, each item can be considered separately. It must
not enter the parity release as an undocumented convenience.

## Resolved safety corrections

The replacement does not copy the Python CAPO API server LB gate or its fail-open Octavia list handling.
A complete empty inventory is a no-op, while endpoint, list, pagination, tag, and association failures retain the finalizer.

The replacement also does not finalize after an unconfirmed application credential deletion.
Resource cleanup must be verified first, and only exact credential DELETE `204` or exact bound-credential `404` may authorize Secret deletion.
