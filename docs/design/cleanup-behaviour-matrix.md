# Cleanup behavior matrix

## How to read this document

This matrix records the behavior of Python release `0.15.0` at [`f14d860`](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/tree/f14d86013d78ac3a4b07f5a2669a8f49590e13ca) and the decision for the replacement Go release.
The [Python compatibility policy](python-compatibility-policy.md) explains how the baseline was selected and records decisions that the Python controller could not answer.

The rewrite keeps the cleanup role and successful outcomes while replacing Kopf-specific mechanisms and correcting failure modes that could remove the finalizer before cleanup is verified.

The decisions used below are:

- **Keep** means the observable behavior remains the same
- **Keep, native implementation** means the outcome remains the same but the mechanism changes to controller-runtime or Gophercloud
- **Safety correction** means the Go controller behaves more conservatively without adding a new deletion target
- **Correctness correction** fixes a wrong target or outcome. Any change to deletion eligibility is called out and bounded separately
- **Approved extension** means a compatibility feature has been accepted explicitly without adding a deletion target
- **Defer** means the behavior is outside the replacement release

## Lifecycle and Kubernetes behavior

| Area | Python behavior | Replacement release | Required test |
| --- | --- | --- | --- |
| Watched object | Watches CAPO `OpenStackCluster` events | Keep, using a controller-runtime watch | Creating and updating an `OpenStackCluster` produces the expected reconcile request |
| Watch scope | Watches all namespaces | Keep | Objects in two namespaces reconcile without an operator-wide credential |
| Active controller | Helm runs one replica with `Recreate` to avoid races | Keep the single-active guarantee through leader election and conflict-safe reconciliation | Packaging enables leader election consistently; a failover does not duplicate a destructive transition |
| Finalizer value | Uses `janitor.capi.stackhpc.com` | Keep | An existing Python finalizer is recognized and a new object receives the same value |
| Normal reconcile | Adds the Janitor finalizer and does no OpenStack cleanup | Keep, native implementation | A non-deleting object is patched once and no cloud client is created |
| Deletion trigger | Cleanup starts only when `deletionTimestamp` is set and the Janitor finalizer is present | Keep | No deletion runs before deletion starts or when the finalizer is absent |
| Finalizer update | Patches the finalizer list and retries selected API errors | Keep outcome, use a conflict-safe metadata patch | A concurrent metadata update is not lost |
| Cluster name | Uses `cluster.x-k8s.io/cluster-name`, falling back to `OpenStackCluster.metadata.name` | Keep the precedence. As a safety correction, block an empty resolved name before discovery | Label present and absent resolve correctly; an empty label or name makes no cloud call |
| Missing required identity Secret | Logs the error, skips OpenStack cleanup, and can remove the finalizer | Before the recorded Secret-delete phase, keep the finalizer and report a blocked cleanup. Absence after `secretDeleteStarted` is idempotent completion | A missing Secret before the write-ahead delete phase never causes success; absence after `secretDeleteStarted` completes the exact recorded deletion |
| Paused objects | No explicit CAPI pause handling | Safety correction. Honor pause on the Cluster or OpenStackCluster | A paused object performs no cleanup and retains the finalizer |
| Watch filter | No standard CAPI watch filter | Add the standard filter as an operational safeguard without changing ownership rules | When configured, only labeled objects are reconciled |
| Retry mechanism | Sleeps and writes a random retry annotation | Keep outcome, use returned errors and `RequeueAfter` | Expected waiting returns `RequeueAfter` and failures return an error without annotation churn |
| Status | Does not own a Janitor status API | Keep. Do not write CAPO-owned status or conditions | Reconcile leaves `OpenStackCluster.status` unchanged |

## Identity and OpenStack client behavior

| Area | Python behavior | Replacement release | Required test |
| --- | --- | --- | --- |
| Identity source and type | Reads the same-namespace Secret named by `spec.identityRef.name` without inspecting `identityRef.type` | The replacement profile accepts `Secret` and the legacy empty value, resolves only a same-namespace Secret, blocks `ClusterIdentity` as unsupported, and rejects unknown values. The community profile may resolve `ClusterIdentity` only after namespace authorization | Replacement tests cover each accepted and rejected value without same-named Secret fallback. Community tests cover authorization, bypass attempts, and reconciliation after identity, backing Secret, or Namespace label changes |
| Secret data | Decodes the Kubernetes API representation of `clouds.yaml` and optional `cacert` | Keep the resulting bytes. In Go, `Secret.Data` is already decoded | Raw Secret bytes reach the parser exactly once |
| Cloud name | Uses `identityRef.cloudName`, then legacy `spec.cloudName`, then `openstack` | Use required `identityRef.cloudName` for current CAPO APIs. An empty value on an already-admitted migration object may use `openstack`. The target Go type does not expose legacy `spec.cloudName` | Current and migrated empty inputs select the expected entry. No unsupported legacy field is silently claimed |
| Authentication | Supports only `v3applicationcredential` | Approved extension. Support explicit `v3applicationcredential` plus the fixed password forms below; reject token, federation, and other methods | Every accepted method authenticates through Gophercloud. Unsupported methods make no discovery or deletion request |
| Password `auth_type` | Not supported | Accept `v3password`, `password`, and omission only for a complete, unambiguous password family. Keep application credential explicit | Every accepted spelling, omitted complete input, mixed family, and rejected near-match is covered |
| Password scope | Not supported | Require project scope and a non-empty project ID in the issued token; reject domain-only, system, and unscoped authentication | Project ID and project name plus domain forms succeed; every non-project scope fails before discovery |
| Password fields | Not supported | Require `auth_url`, `password`, exactly one of user ID or username, and exactly one of project ID or project name. A name requires one resolved domain. Reject simultaneous ID/name values, mixed credential families, incomplete input, and conflicting domain fallbacks before discovery | Missing, mixed, duplicate, and contradictory fields fail without a resource list or delete request; configured and token project IDs must agree |
| Password credential cleanup | Not supported | Never delete a Keystone identity or password Secret. Ignore `credential-policy: delete` with a warning | Password cleanup does not create the Keystone application-credential deletion service and never deletes the referenced Secret |
| Application-credential fields | Requires `auth_url`, application credential ID, and secret | Keep and fail closed before discovery when a required field is missing | Every missing-field case retains the finalizer without a resource list or delete request |
| Identity URL | Removes a trailing `/v3` but preserves a deployment path prefix | Keep through Gophercloud | `/v3`, `/identity/v3`, and trailing-slash forms resolve without losing or duplicating path segments |
| Region | Uses `region_name` when present; without it, does not filter the catalog by region | Keep | The configured region is selected; the no-region case remains unfiltered |
| Interface | Uses the configured interface and defaults to `public` | Keep | Explicit and default interface selection are covered |
| TLS verification | Uses `verify` from `clouds.yaml` and loads optional CA data from the Secret | Keep through Gophercloud transport configuration | Default verification, custom CA, and invalid CA cases are covered |
| Token handling | Uses a hand-written token and catalog client | Keep outcome, replace with Gophercloud authentication and reauthentication | An expired token is reauthenticated and the request is retried through Gophercloud |
| Pagination | Iterates resource lists across returned pages | Keep through Gophercloud pagers | A matching resource on a later page is found |
| Additional auth methods | Not supported | Defer beyond the approved password extension | Token, federation, and other auth types do not enable cleanup |
| IdentityRef region override | Not used by the Python controller | Approved CAPO compatibility extension. `identityRef.region` overrides the selected cloud region | Direct-Secret and community identity tests prove the override, no-region behavior, and endpoint boundary |

## Resource cleanup

| Area | Python behavior | Replacement release | Required test |
| --- | --- | --- | --- |
| Floating IPs | Always selects matching OCCM service floating IPs and requests deletion | Keep | Matching descriptions are deleted and near-matches remain |
| Service load balancer gate | The released code requires `spec.apiServerLoadBalancer.enabled` and a non-empty `status.apiServerLoadBalancer.id`; its README says cleanup is unconditional | Correctness correction. Remove the CAPO status gate because it can leak workload LBs; keep inventory fail-closed | Matching workload LBs are considered under all four enabled and ID combinations. An Octavia list failure retains the finalizer |
| Service load balancers | Selects names beginning with `kube_service_<cluster>_` and requests cascade deletion when the implementation gate is true | Keep the exact prefix and cascade delete independently of CAPO API server LB status. Preserve a name-selected LB when any `kube_service_` tag is malformed or belongs to another cluster | Matching names with only target-cluster tags are deleted. Near-matches, API server LBs, and foreign-tag shared LBs remain |
| Shared LB floating IP | Python does not inspect shared-LB tags before deleting matching floating IPs | Safety correction. Before mutation, map protected LB VIP port IDs to FIP port IDs and preserve each attached FIP even when its description matches | A foreign-tag shared LB and attached FIP remain while unrelated owned resources are deleted; incomplete association data blocks all mutation |
| Service security groups | Always selects matching service load balancer security groups | Keep | Matching descriptions are deleted and near-matches remain |
| Default volume policy | Defaults to `delete` through `CAPI_JANITOR_DEFAULT_VOLUMES_POLICY` | Keep | Default and configured operator policy are covered |
| Per-cluster volume policy | `janitor.capi.stackhpc.com/volumes-policy` overrides the default. Only the exact value `delete` enables deletion | Keep | `delete`, `keep`, empty, and unknown values are covered |
| Snapshots | Deletes snapshots with exact Cinder cluster metadata when volume deletion is enabled | Keep | Matching snapshots are deleted only under the delete policy |
| Volumes | Deletes volumes with exact Cinder cluster metadata when volume deletion is enabled | Keep | Matching volumes are deleted only under the delete policy |
| Per-volume keep | Keeps a volume only when `janitor.capi.azimuth-cloud.com/keep` is exactly `true` | Keep | Exact `true` is kept. Missing, differently cased, and other values follow existing delete behavior |
| Snapshot keep | No independent snapshot keep property is implemented. A related volume keep value does not protect the snapshot | Keep. Do not invent a snapshot keep rule during parity work | A matching snapshot is still selected when volume policy is `delete`, including when a related volume is kept |
| Resource order | Requests FIP, load balancer, security group, snapshot, and volume deletion in that order, then verifies the selected kinds | Run a complete read-only LB/FIP ownership preflight, then keep FIP, LB, SG, snapshot, and volume as level-based dependency phases | No mutation occurs before preflight completes; a later phase does not run before the earlier dependency is absent |
| Verification | Re-lists resource kinds for which at least one delete was attempted and retries while matches remain | Keep and strengthen through level-based observation | Finalizer removal is blocked while any required match remains |
| Empty inventory | Completes a resource phase when no matching candidate exists | Keep | A cluster with no candidates progresses without delete requests |

## Application credential and Secret cleanup

| Area | Python behavior | Replacement release | Required test |
| --- | --- | --- | --- |
| Credential policy source | Reads `janitor.capi.stackhpc.com/credential-policy` from the credential Secret | Apply exact `delete` only to the direct application-credential Secret profile. Ignore it with a warning for password and community identities | Missing, `delete`, and other values are covered for each identity profile |
| Last-finalizer rule | Attempts credential and Secret deletion only when the policy is `delete` and the Janitor finalizer is the only finalizer | Keep for the direct application-credential Secret profile | Other finalizers cause waiting and do not delete the credential or Secret |
| Credential ID | Reads the application credential ID from the hard-coded `openstack` entry even when another cloud was selected | Correctness correction. Read the ID from the selected cloud entry | A non-default selected cloud uses its own application credential ID |
| Credential order | Attempts application credential deletion after other resources have been checked | Keep and add the versioned `janitor.capi.stackhpc.com/cleanup-state` write-ahead checkpoint. Repeat full inventory after `resourcesVerified` and before `credentialDeleteStarted` | Restarts at every phase converge safely; a candidate that appears after `resourcesVerified` clears the pre-start checkpoint and resumes cleanup |
| Checkpoint binding and freshness | Has no persisted binding | Bind the cluster and Secret identities, normalized Keystone authority digest, authenticated project and user IDs, region, exact credential ID, and resolved volume and credential policies without Secret data or the raw URL. Repeat full inventory between `resourcesVerified` and `credentialDeleteStarted` | A new candidate resumes cleanup. Valid same-boundary identity or policy changes invalidate a pre-start checkpoint and require fresh inventory. Authority, project, region, cluster UID, or effective cluster-name changes and all post-start changes fail closed |
| Exact application-credential self-DELETE `403` | Warns and continues because a restricted credential may not delete itself | Safety correction while preserving the Python non-blocking finalizer outcome. Record `retainedForbidden`, retain the direct Secret, warn that the credential may remain, and allow cluster finalization | The exact self-DELETE `403` does not delete the Secret or turn authentication, catalog, or other resource failures into success |
| Other credential errors | Retries and retains the finalizer | Except for separately classified exact credential DELETE `404` and self-delete `403`, errors block Secret and finalizer removal | Authentication, catalog, endpoint, transport, rate-limit, and server failures retain the finalizer |
| Application-credential Secret deletion | Deletes the Secret after the credential phase when policy and finalizer rules permit it | Delete only after the bound credential result is `deleted` or `absent`; retain it for `retainedForbidden` or unverified outcomes | Secret deletion occurs only after the resources-verified checkpoint and a qualifying exact credential result |
| Password Secret deletion | Not applicable because Python rejects password authentication | Always keep. The existing annotation does not authorize deletion of a reusable password Secret | Missing, `delete`, and other annotation values all retain the Secret; exact `delete` emits a warning |
| ClusterIdentity credential cleanup | Not supported | In the community profile, always keep the `OpenStackClusterIdentity`, backing Secret, and credential, regardless of auth type or annotation | Resource cleanup can use an authorized identity without any delete request for the identity or backing Secret |
| Application-credential authentication failure before verification | Can be treated as an already-deleted credential when credential deletion was requested | Safety correction. Do not accept missing credentials before a persisted resources-verified checkpoint | Invalid credentials before the checkpoint retain the finalizer |
| Application-credential authentication failure after verification | The Python controller intends this to represent an already-deleted credential | A `401` after `credentialDeleteStarted` remains `ApplicationCredentialDeletionUnverified` and blocks. The write-ahead phase proves intent, not execution. Exact credential DELETE `204` is `deleted`, exact bound-resource `404` is `absent`, and exact `403` is `retainedForbidden` | Tests cover pre-start and post-start `401`, each exact credential result, base-URL `404`, empty catalog, endpoint failure, and changed binding |

## Error handling

| Area | Python behavior | Replacement release | Required test |
| --- | --- | --- | --- |
| Delete response `400` or `409` | Logs and retries after checking whether the resource remains | Keep outcome, classify through Gophercloud and requeue | Conflict or transient delete state does not remove the finalizer |
| Resource still present | Retries after a short delay | Keep outcome with `RequeueAfter` | No sleep occurs inside reconcile |
| Other OpenStack error | Retries after the configured default delay | Keep outcome through returned error and workqueue backoff | Error is returned and finalizer remains |
| Retry configuration | Parses `CAPI_JANITOR_RETRY_DEFAULT_DELAY` as integer seconds; malformed input prevents startup | Use a positive integer as the maximum operational-error delay, default `60`. Configure per-object exponential backoff from one second to that maximum and reset it after success. Fail startup for malformed, zero, negative, or overflowing input | Default, override, malformed, zero, negative, overflow, first-delay, configured-cap, and reset behavior are covered |
| Kubernetes finalizer patch conflict | Retries | Keep outcome through patch conflict handling | Reconcile succeeds after a simulated conflict |
| Exact DELETE of an already-selected OpenStack resource returns `404` | Propagates the delete error | Idempotency correction. `NotFound` is complete only for the selected resource; inventory, endpoint, and base-URL `404` remain errors | A selected resource that disappears converges; an unselected ID is never authorized by `NotFound` |
| OpenStackCluster not found | Treats the object as already gone | Keep | A reconcile for an absent `OpenStackCluster` returns success |
| Optional service clients | Creates load balancer and volume clients even when their policy gate is false | Correctness correction. Create a service client only when that cleanup phase needs it | A disabled phase does not require an unrelated service endpoint |
| Octavia service absent | The API-server-LB gate can avoid creating the client | Inspect the raw catalog. Absence of `load-balancer` makes the LB phase not applicable. Continue only when no matching FIP is bound to a port; a successfully parsed null or empty-string `port_id` is unbound, while a bound FIP or unknown association blocks | Catalog absence with no matching or only unbound FIPs completes without an Octavia request. A bound matching FIP, missing field, type mismatch, or extraction failure retains the finalizer |
| Octavia service unusable | Raises a catalog or request error when the gated phase runs | If the service type exists, a missing selected endpoint or any inventory or list failure, including `401`, `403`, `404`, `429`, `5xx`, incomplete pagination, or extraction failure, blocks cleanup. Exact DELETE `404` for an already-selected LB is idempotent completion | Each inventory outcome retains the finalizer and no incomplete inventory authorizes mutation; the selected-delete case converges |
| Other missing required service | Raises a catalog error | Missing Neutron, or missing Cinder when volume cleanup is enabled, blocks completion | Each required-service failure retains the finalizer; disabled Cinder cleanup does not create its client |
| Cinder service type | Tries `volumev3`, then `block-storage` | Keep both catalog names | Each name selects a usable endpoint; neither present blocks volume cleanup |
| Partial inventory failure | The successful path assumes listing completed | Make the requirement explicit. Any inventory failure blocks completion | A later-page or service list failure retains the finalizer |
| Request timeout | Uses a 60-second HTTP client timeout | Keep 60 seconds through the Gophercloud transport | The configured client timeout is 60 seconds and context cancellation remains effective |

## Scope boundary

The direct-Secret profile is required for the replacement release; community identity and `v1beta2` support have separate gates in the [compatibility policy](python-compatibility-policy.md#supported-capo-lanes).
This matrix records observable outcomes and required tests; deferred scope and compatibility gates remain in the compatibility policy.
