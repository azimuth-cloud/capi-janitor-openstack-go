# Python compatibility policy

## Baseline and purpose

The replacement contract is based on Python controller release `0.15.0` at commit [`f14d860`](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/tree/f14d86013d78ac3a4b07f5a2669a8f49590e13ca).
The released code is the observed runtime baseline, while its README and change history record intended behavior and known corrections.
When those sources disagree, this document records the replacement decision explicitly.

The first goal is a safe replacement for that controller, not a broader cleanup service.
The Janitor remains a generic CAPO and OpenStack component: one deployment is an integration target, not a permanent product-specific compatibility boundary.
Legacy finalizers, annotations, and selectors remain only where replacement compatibility requires them.

The first replacement release supports:

- CAPO `OpenStackCluster` `v1beta1` objects
- one same-namespace Secret referenced directly by `spec.identityRef`
- an explicit `v3applicationcredential` entry in the selected `clouds.yaml` cloud
- the OpenStack resource types and exact selectors used by the Python controller

All other identity sources, authentication types, API versions, and cleanup targets are outside this release.
Unsupported input must fail before any OpenStack discovery or deletion request.
Future support is a separate compatibility decision and is not constrained by a particular deployment.

The only user-selectable cleanup policies remain:

- the existing operator and cluster volume policy
- the existing direct application-credential deletion opt-in

The replacement adds no deletion target, primary Kubernetes kind, CRD, skip switch, force-finalize switch, or policy annotation.
Complete inventory, shared-resource checks, retries, and restart checkpoints are fixed safety rules rather than new user policies.

## Python policy carried into the replacement

| Area | Replacement contract |
| --- | --- |
| Watched resource | Watch `OpenStackCluster` objects in all namespaces |
| Finalizer | Keep the exact `janitor.capi.stackhpc.com` value so the Go controller can adopt Python-managed objects |
| Cleanup trigger | Start only after `deletionTimestamp` is set and the Janitor finalizer is present |
| Cluster name | Prefer `cluster.x-k8s.io/cluster-name`; otherwise use `OpenStackCluster.metadata.name` |
| Identity source | Resolve only the same-namespace direct Secret named by `spec.identityRef.name`; accept `Secret` and the legacy empty type on an already-admitted migration object, and reject every other type before a cloud call |
| Cloud selection | Use required `identityRef.cloudName`; an already-admitted migration object with an empty value may use `openstack` |
| Authentication | Require explicit `v3applicationcredential` with complete application credential fields |
| Project boundary | Require a non-empty project ID in the authenticated token and use only that project |
| Endpoint selection | Honor `interface`, defaulting to `public`, and `region_name` from the selected cloud when configured |
| TLS | Honor `verify` and optional `cacert` bytes from the Secret |
| Request timeout | Use 60 seconds |
| Cinder catalog | Try `volumev3`, then `block-storage`; fail if neither has a usable endpoint |
| Floating IP | Keep the exact Python description prefix and cluster-name suffix |
| Service load balancer | Keep the exact `kube_service_<cluster>_` name selector, remove the CAPO API server load balancer gate, and apply the OCCM shared-LB veto |
| Service security group | Keep the exact Python description prefix and cluster-name suffix |
| Volume and snapshot | Keep the exact cluster metadata, volume policy, and case-sensitive volume keep rule |
| Resource order | Complete the read-only shared-LB/FIP preflight, then process floating IP, load balancer, security group, snapshot, and volume phases |
| Credential policy | Exact `janitor.capi.stackhpc.com/credential-policy: delete` authorizes deletion of the selected direct application credential and its Secret |
| Last-finalizer rule | Begin the credential transition only when the Janitor finalizer is the only remaining finalizer |
| Missing identity | Retain the finalizer, report the blocked cleanup, and retry |
| Exact selected-resource DELETE `404` | Treat the already-selected resource as absent; inventory and endpoint `404` responses remain errors |
| Other OpenStack errors | Retain the finalizer and retry through controller-runtime |

## Cluster name ownership boundary

The effective cluster name is the primary ownership key for the legacy selectors.
Every floating IP, load balancer, security group, snapshot, and volume selector depends on this exact value.
It must match the cluster name used by OCCM and Cinder CSI when they created the resources.

Workload cluster names must be unique within one OpenStack project.
The Janitor does not use an immutable workload-cluster UID stored on each OpenStack resource, so duplicate names can make ownership ambiguous.
An empty name blocks discovery, and the replacement must not use fuzzy matching to compensate for a missing or incorrect name.
Release validation must cover the configured name used by the CAPI object, OCCM, and Cinder CSI.

## Load balancer correction and shared ownership

The released Python controller gates workload LB cleanup on CAPO API server load balancer fields.
That gate was added in [PR #218](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/218) and can leave OCCM workload LBs behind when the API server uses another exposure mechanism.
[PR #261](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/261) removed the gate, but it also swallowed Octavia list failures.

The Go controller keeps the gate removal and does not copy the fail-open error handling:

1. list every load balancer page in the authenticated project
2. select candidates by the exact Python `kube_service_<cluster>_` name prefix
3. read the complete OCCM ownership tag set and VIP port for every candidate
4. complete the LB-to-floating-IP association before the first mutation

OCCM uses `kube_service_<cluster>_<namespace>_<service>` tags to track Service ownership and shared LBs.
Pool members and member ports are not ownership evidence and must not override the OCCM tags.

The deletion rule is:

- delete a name-selected LB when every well-formed reserved ownership tag belongs to the target cluster name
- preserve the whole LB and its VIP floating IP when any reserved tag belongs to another cluster
- preserve the whole LB and its VIP floating IP when a reserved tag is malformed or cannot be classified
- keep the Python name-only behavior when tag data is read successfully and contains no reserved ownership tag
- block before mutation when LB, tag, VIP port, floating-IP association, or pagination data is incomplete

Several Services from the same workload cluster may therefore share an LB that is deleted with the cluster.
An LB shared with a different workload cluster is not deleted.
The uniqueness requirement for workload cluster names in a project is a precondition for this cluster-name-based tag rule.

## Octavia capability and error handling

A successful complete inventory with no matching LB is a normal no-op.
An inventory failure is not evidence that no LB exists.
The replacement does not add a configurable LB skip policy.

| Observation | Result |
| --- | --- |
| The catalog has no `load-balancer` service or the selected region or interface has no endpoint | Block and retain the finalizer |
| Any Octavia inventory, list, pagination, extraction, tag, or association failure | Block and retain the finalizer |
| Exact DELETE `404` for an already-selected LB | Treat the LB as absent and continue verification |
| A selected LB remains after an accepted delete | Requeue and verify again |

CAPO API server load balancer fields never stand in for these observations.

## Application credential cleanup

Application credential deletion is the final OpenStack operation because it removes the controller's ability to inspect and clean the project.
If resource cleanup fails or inventory is incomplete, the controller retains the application credential, Secret, and finalizer so that cleanup can be retried.
It must not delete the credential from a `finally`-style path after partial cleanup.

The credential transition requires all of the following:

- the direct Secret has exact `credential-policy: delete`
- the selected cloud is the same explicit `v3applicationcredential` cloud used for cleanup
- the Janitor finalizer is the only remaining finalizer
- a fresh complete inventory verifies that no owned resource remains

When the annotation is absent or is not exact `delete`, the application credential and Secret are preserved.

The controller records a versioned checkpoint on the `OpenStackCluster` before each irreversible transition:

1. after fresh complete inventory is empty and the Janitor is the sole finalizer, record `credentialDeleteStarted`, then return
2. delete the exact application credential; on exact `204` or an exact bound-credential `404`, record `secretDeleteStarted`, then return
3. delete the recorded Secret with a UID precondition, then remove the Janitor finalizer with a later conflict-safe patch

The checkpoint binds the cluster UID and effective name, normalized Keystone authority, project and region, direct Secret UID, selected cloud, exact application credential ID, and resolved cleanup policies.
It contains no Secret data.

An application credential DELETE `401`, `403`, timeout, rate limit, server error, or unclassified response does not advance the checkpoint.
The controller retains the Secret and finalizer, reports blocked cleanup, and retries the exact operation or waits for manual recovery.
An initial authentication failure, catalog failure, or Keystone base-URL `404` never proves that the credential is absent.

A missing Secret before `secretDeleteStarted` blocks cleanup.
A missing Secret after `secretDeleteStarted` is an idempotent completion of the recorded Secret deletion.
Before `credentialDeleteStarted`, a valid change within the same ownership boundary requires full verification again.
At or after `credentialDeleteStarted`, any binding change remains blocked for manual recovery.

## Retry and change control

Reconcile must never sleep, poll in a loop, or write retry annotations.
Expected waits use `RequeueAfter`; operational failures return errors for bounded per-object workqueue backoff.
No timeout or automatic force-finalize path may convert unknown cleanup into success.

The first validation lane uses a stable CAPO `v0.14.x` release with `OpenStackCluster` `v1beta1` and a direct Secret.
Exact deployment versions are test fixtures recorded with release evidence, not permanent compatibility constraints on future Janitor development.

A future scope change requires its own ownership rules, negative fixtures, migration assessment, and maintainer agreement.
