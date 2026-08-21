# Cleanup behaviour matrix

## How to read this document

This matrix records the behaviour of the Python controller at
[`cluster-api-janitor-openstack` `0.15.0`](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/tree/f14d86013d78ac3a4b07f5a2669a8f49590e13ca)
and the target behavior for the Python replacement release.

The rewrite keeps the same cleanup role.
It replaces mechanisms specific to Kopf and fixes cases where the finalizer could be removed before cleanup was verified.
The matrix states each changed outcome directly.

The replacement column states the target behavior directly:

- **Keep** means the observable behavior remains the same
- **Defer** means the behavior is outside the replacement release
- other rows describe an intentional bug fix or implementation change directly

The Required test column describes the evidence needed before release, not necessarily one Go unit test.
Most rows should use automated Go unit tests, tests against a fake HTTP server, or envtest.
Use real OpenStack for cloud behavior that these tests cannot prove.
Use a representative Azimuth environment for full replacement and migration tests.

## Lifecycle and Kubernetes behaviour

| Area | Python behaviour | Replacement release | Required test |
| --- | --- | --- | --- |
| Watched object | Watches CAPO `OpenStackCluster` events | Keep. Use a watch from the controller framework | Creating and updating an `OpenStackCluster` produces the expected reconcile request |
| Finalizer value | Uses `janitor.capi.stackhpc.com` | Keep | An existing Python finalizer is recognised and a new object receives the same value |
| Normal reconcile | Adds the Janitor finalizer and does no OpenStack cleanup | Keep. Use the controller framework | An object without a deletion timestamp is patched once and no cloud client is created |
| Deletion trigger | Cleanup starts only when `deletionTimestamp` is set and the Janitor finalizer is present | Keep | No deletion runs before deletion starts or when the finalizer is absent |
| Finalizer update | Patches the finalizer list and retries selected API errors | Patch the latest object metadata and retry conflicts | A concurrent metadata update is not lost |
| Cluster name | Uses `cluster.x-k8s.io/cluster-name`, falling back to `OpenStackCluster.metadata.name` | Keep the resolution order. Require the same value in OCCM and Cinder CSI. Workload cluster names must be unique within each OpenStack project | Label present, label absent, mismatch, and duplicate name risks are covered |
| Missing Secret | Logs the error, skips OpenStack cleanup, and can remove the finalizer | Before the recorded Secret deletion phase, retain the finalizer and report blocked cleanup | A missing Secret before `secretDeleteStarted` retains the finalizer and reports blocked cleanup |
| Paused objects | No explicit CAPI pause handling | Honor pause on the Cluster or OpenStackCluster | A paused object performs no cleanup and retains the finalizer |
| Watch filter | No standard CAPI watch filter | Defer. The replacement release watches all namespaces | The replacement release does not filter objects by label |
| Retry mechanism | Sleeps and writes a random retry annotation | Return `RequeueAfter` for expected waits and return errors for workqueue backoff | An expected wait returns `RequeueAfter`. A failure returns an error without annotation churn |
| Status | Does not own a Janitor status API | Keep. Do not write status or conditions owned by CAPO | Reconcile leaves `OpenStackCluster.status` unchanged |

## Identity and OpenStack client behaviour

| Area | Python behaviour | Replacement release | Required test |
| --- | --- | --- | --- |
| Identity source | Reads the Secret named by `spec.identityRef.name` from the same namespace | Keep | A Secret in the object namespace is read and a Secret with the same name elsewhere is ignored |
| Secret data | Decodes the Kubernetes API representation of `clouds.yaml` and optional `cacert` | Keep the resulting bytes. In Go, `Secret.Data` is already decoded | Raw Secret bytes reach the parser exactly once |
| Cloud name | Uses `identityRef.cloudName`, then legacy `spec.cloudName`, then `openstack` | Use `identityRef.cloudName`. For an object admitted before `cloudName` became required, an empty value falls back to `openstack` for migration compatibility. `spec.cloudName` is outside the `v1beta1` contract | Explicit cloud names and the migration fallback select the expected entry |
| Authentication | Supports only `v3applicationcredential` | Keep | Other auth types are rejected without making a deletion request |
| Region | Uses `region_name` from the selected `clouds.yaml` entry | Keep | Only the configured region endpoint is selected |
| Interface | Uses the configured interface and defaults to `public` | Keep | Explicit and default interface selection are covered |
| TLS verification | Uses `verify` from `clouds.yaml` and loads optional CA data from the Secret | Keep through Gophercloud transport configuration | Default verification, custom CA, and invalid CA cases are covered |
| Token handling | Uses a custom token and catalog client | Use Gophercloud authentication and reauthentication | An expired token is reauthenticated and the request is retried through Gophercloud |
| Pagination | Iterates resource lists across returned pages | Keep through Gophercloud pagers | A matching resource on a later page is found |
| Unsupported identity types | Not supported by the Python controller | Defer | The replacement release rejects every unsupported identity type without discovery or deletion |
| Additional auth methods | Not supported | Defer | Only explicit `v3applicationcredential` enables cleanup |
| IdentityRef region override | Not used by the Python controller | Defer until a separate compatibility decision | Setting only the IdentityRef region does not silently widen the replacement release scope |

## Resource cleanup

| Area | Python behaviour | Replacement release | Required test |
| --- | --- | --- | --- |
| Floating IPs | Always selects matching OCCM service floating IPs and requests deletion | Keep | Matching descriptions are deleted. Descriptions that do not match exactly remain |
| Service load balancer gate | Deletes matching service load balancers only when `spec.apiServerLoadBalancer.enabled` is true and `status.apiServerLoadBalancer.id` is not empty | Remove the broken gate and inspect workload LBs independently of CAPO API server LB state | All enabled and ID combinations trigger the same complete workload LB inventory |
| Service load balancers | Selects names beginning with `kube_service_<cluster>_` and requests cascade deletion | Keep the name selector. Use the complete set of reserved OCCM tags to decide whether a matching LB must be preserved | A matching LB remains eligible for cascade deletion when its reserved tags refer only to Services in the target cluster. A foreign or malformed reserved tag preserves the LB and its VIP FIP |
| Shared LB evidence | Does not protect a shared LB used by another cluster | For each LB selected by name, use its complete set of OCCM `kube_service_<cluster>_<namespace>_<service>` tags to detect sharing across clusters | Incomplete tag data, a missing VIP port, or an unknown FIP association blocks mutation. A complete tag result with no reserved OCCM tags uses the legacy name selector |
| Service security groups | Always selects matching service load balancer security groups | Keep | Matching descriptions are deleted. Descriptions that do not match exactly remain |
| Default volume policy | Defaults to `delete` through `CAPI_JANITOR_DEFAULT_VOLUMES_POLICY` | Keep | Default and configured operator policy are covered |
| Cluster volume policy | `janitor.capi.stackhpc.com/volumes-policy` overrides the default. Only the exact value `delete` enables deletion | Keep | `delete`, `keep`, empty, and unknown values are covered |
| Snapshots | Deletes snapshots with exact Cinder cluster metadata when volume deletion is enabled | Keep | Matching snapshots are deleted only under the delete policy |
| Volumes | Deletes volumes with exact Cinder cluster metadata when volume deletion is enabled | Keep | Matching volumes are deleted only under the delete policy |
| Volume keep setting | Keeps a volume only when `janitor.capi.azimuth-cloud.com/keep` is exactly `true` | Keep | Exact `true` is kept. A missing value, differently cased values, and all other values follow existing delete behaviour |
| Snapshot keep | No independent snapshot keep property is implemented. A related volume keep value does not protect the snapshot | Keep. Do not invent a snapshot keep rule for the replacement release | A matching snapshot is still selected when volume policy is `delete`, including when a related volume is kept |
| Resource order | Requests FIP, load balancer, security group, snapshot, and volume deletion in that order, then verifies the selected kinds | Keep the order and wait for each dependency to disappear before the next phase | A later dependent phase does not run before the earlier dependency is absent |
| Verification | Lists resource kinds again after attempting at least one deletion and retries while matches remain | List each required resource type again and advance only after no matching resource remains | Finalizer removal is blocked while any required match remains |
| Empty inventory | Completes a resource phase when no matching candidate exists | Keep | A cluster with no candidates progresses without delete requests |

## Application credential and Secret cleanup

| Area | Python behaviour | Replacement release | Required test |
| --- | --- | --- | --- |
| Credential policy source | Reads `janitor.capi.stackhpc.com/credential-policy` from the credential Secret | Keep | Missing, `delete`, and other values are covered |
| Finalizer condition | Attempts credential and Secret deletion only when the policy is `delete` and the Janitor finalizer is the only finalizer | Keep | Other finalizers cause waiting and do not delete the credential or Secret |
| Credential ID | Reads the application credential ID from the fixed `openstack` entry even when another cloud was selected | Read the application credential ID from the selected cloud entry | A selected cloud other than `openstack` uses its own application credential ID |
| Credential order | Attempts application credential deletion after other resources have been checked | Run credential deletion last, after a fresh and complete verification, the finalizer check, and a persisted deletion checkpoint | A restart before or after the credential request resumes the recorded phase or blocks without removing the Secret or finalizer |
| Credential `403` | Warns and continues because a restricted credential may not delete itself | A `403` does not prove deletion. Retain the Secret and finalizer | The checkpoint does not advance and cleanup remains blocked |
| Other credential errors | Retries and retains the finalizer | Keep. Return an error so the work queue retries | All unconfirmed outcomes block Secret and finalizer removal |
| Secret deletion | Deletes the Secret after the credential phase when policy and finalizer rules permit it | Use a UID precondition. After `secretDeleteStarted`, treat a Secret that is already missing as complete | Secret deletion occurs only after confirmed credential absence and targets the recorded Secret UID |
| Authentication failure before verification | Can be treated as an already deleted credential when credential deletion was requested | Authentication failure is not proof that the credential is absent | Invalid credentials retain the finalizer. Only a `404` returned while deleting the recorded credential by its exact ID after `credentialDeleteStarted` proves absence |
| Authentication failure after verification | The Python controller intends this to represent a credential that was already deleted | Authentication failure remains unverified | Only a `204` or a `404` from deleting the recorded credential by its exact ID permits Secret deletion |

## Error handling

| Area | Python behaviour | Replacement release | Required test |
| --- | --- | --- | --- |
| Delete response `400` or `409` | Logs and retries after checking whether the resource remains | Classify the response through Gophercloud, list the resource again, and requeue while it remains | Conflict or transient delete state does not remove the finalizer |
| Resource still present | Retries after a short delay | Return `RequeueAfter` | No sleep occurs inside reconcile |
| Other OpenStack error | Retries after the configured default delay | Return the error and use workqueue backoff | Error is returned and finalizer remains |
| Kubernetes finalizer patch conflict | Retries | Read the latest object and retry the metadata patch | Reconcile succeeds after a simulated conflict |
| Kubernetes object not found | Treats the object as already gone | Keep | NotFound returns success |
| Optional service clients | Creates load balancer and volume clients even when their policy gate is false | Create a service client only when its cleanup phase needs it | A disabled phase does not require an unrelated service endpoint |
| Missing required service | Raises a catalog error | Block completion when Neutron, enabled Cinder, or Octavia is missing or unusable | A missing required service must not be treated as an empty inventory |
| Partial inventory failure | The successful path assumes listing completed | Any inventory failure blocks completion | A failure on a later page or a service list failure retains the finalizer |

## Explicitly deferred work

The following may be useful later, but adding them to the replacement release
would make the rewrite larger than the Python role:

- deleting networks, subnets, routers, ports, servers, or keypairs
- deleting infrastructure managed by CAPO
- supporting additional naming conventions
- supporting other identity sources or authentication methods
- replacing the existing ownership selectors with discovery based on tags
- adding a Janitor status CRD
- changing the finalizer value
- taking responsibility for general CI project cleanup

After the Go rewrite is stable, each item can be considered separately. It must
not enter the replacement release as an undocumented convenience.

## Known Python issues fixed in the replacement

The replacement does not copy the Python CAPO API server LB gate or the Octavia behavior that treats list errors as success.
A complete inventory with no matching resources finishes normally.
Endpoint, list, pagination, tag, and association failures retain the finalizer.

The replacement also does not finalize after an unconfirmed application credential deletion.
Resource cleanup must be verified first.
Only a `204` or `404` response from deleting the recorded credential by its exact ID may authorize Secret deletion.
