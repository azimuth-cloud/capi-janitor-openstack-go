# Python compatibility policy

## Baseline

The replacement contract is based on the Python controller at commit [`f14d860`](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/tree/f14d86013d78ac3a4b07f5a2669a8f49590e13ca).
That commit is both the current `main` commit and the `0.15.0` release as of 20 August 2026.

The runtime source, chart, and README are unchanged from the earlier design baseline at `44a8953`.
Changes between the two commits only update dependencies and CI workflows.
Pinning the current commit here makes later policy reviews repeatable.

The released implementation and its tests define the observed runtime baseline.
The README and change history also record operator expectations and later fixes.
When those sources disagree, this document records the conflict and classifies the replacement decision instead of letting one source silently override another.

The Go replacement keeps the Python cleanup scope and policy.
It does not copy framework-specific retry machinery or known unsafe failure behavior.
The [resource ownership matrix](resource-ownership-matrix.md) defines the destructive boundary for these decisions.

## Policy surface

The replacement does not turn every failure case into an operator choice.
The only cleanup eligibility settings in the replacement release are the existing volume policy and the existing application-credential deletion opt-in.
The selected CAPO identity remains input, not a Janitor policy.

The other decisions in this document are fixed rules:

- project scope and credential validation are authentication safety checks
- the shared-load-balancer veto and complete-inventory requirement are ownership checks
- retry, checkpoint, and post-checkpoint error handling are internal convergence rules
- CAPO versions and identity forms are tested compatibility claims

The replacement release adds no OpenStack deletion target, primary Kubernetes kind, CRD, or operator-selectable cleanup policy.
It adds one internal checkpoint annotation.
A new deletion target, skip switch, force-finalize switch, or policy annotation requires a separate design proposal.

Validation is split into two support profiles:

- **Azimuth replacement** covers CAPO `v0.14.7`, OpenStackCluster `v1beta1`, and a direct per-cluster Secret. This profile must pass before the Python controller is deprecated.
- **CAPO community compatibility** adds support for `identityRef.type: ClusterIdentity`, which resolves an `OpenStackClusterIdentity` and applies its namespace authorization rules. It does not give the Janitor ownership of the shared credential or backing Secret. This profile has a separate gate and does not block the Azimuth cutover.

Python rollback is an operational fallback, not a general per-object safety guarantee.
Standard deployment rollback requires no active deletion and Python-compatible direct application-credential bindings.
An in-flight handback is eligible only before `credentialDeleteStarted` and after the audit defined in [Retry and recovery](#retry-and-recovery); all other states use Go forward recovery or break-glass.
Password and community identity paths have no Python rollback target.

## Python policy carried into the replacement

| Area | Python `0.15.0` policy | Go replacement contract |
| --- | --- | --- |
| Watched resource | Watch CAPO `OpenStackCluster` objects in all namespaces | Keep through controller-runtime |
| Active controller | Helm deploys one replica with `Recreate` to avoid races | Keep one active leader. Additional replicas require working leader election and conflict-safe reconciliation |
| Finalizer | Use `janitor.capi.stackhpc.com` | Keep the exact value for migration and eligible Python rollback |
| Cleanup trigger | Run only after `deletionTimestamp` is set and the Janitor finalizer is present | Keep |
| Cluster name | Prefer `cluster.x-k8s.io/cluster-name`; otherwise use `metadata.name` | Keep |
| Identity source | Read the Secret named by `spec.identityRef.name` in the `OpenStackCluster` namespace; the Python code does not inspect `identityRef.type` | In the replacement profile, accept `Secret` and the legacy empty value and resolve only a same-namespace Secret. Reject `ClusterIdentity` as unsupported, without falling through to a same-named Secret. Add authorized `OpenStackClusterIdentity` resolution under the community gate. Reject every unknown value before a cloud call |
| Cloud selection | Use `identityRef.cloudName`, then the legacy `spec.cloudName`, then `openstack` | Use required `identityRef.cloudName` for current CAPO APIs. An empty value on an already-admitted migration object may use `openstack`. The target `v1beta1` Go type does not expose `spec.cloudName`, so that older field is not supported without a separate legacy API lane |
| Authentication | Accept only `v3applicationcredential` | Keep application credentials. Accept `v3password`, `password`, and an omitted `auth_type` only when the selected cloud is an unambiguous, complete password configuration |
| Identity URL | Remove a trailing `/v3` for authentication while preserving any deployment path prefix such as `/identity` | Keep through Gophercloud and cover the path-prefix case |
| Endpoint selection | Use `interface`, defaulting to `public`. If `region_name` is set, require that region; otherwise do not add a region filter | Keep through Gophercloud |
| TLS | Honor `verify` and optional `cacert` data from the Secret | Keep through the Gophercloud transport |
| Request timeout | Use 60 seconds | Keep 60 seconds as the replacement target |
| Cinder service type | Try `volumev3`, then `block-storage`; fail if neither has a usable endpoint | Keep both Python-compatible catalog names |
| Floating IP | Match descriptions starting with `Floating IP for Kubernetes external service` and ending with `from cluster <cluster>` | Keep the exact two-sided selector |
| Service load balancer | Match `kube_service_<cluster>_`; the released code adds a CAPO status gate that conflicts with its README and can leak workload LBs | Correctness correction. Always consider matching workload LBs; do not use API server LB status as the gate |
| Service security group | Match descriptions starting with `Security Group for` and ending with `Service LoadBalancer in cluster <cluster>` | Keep the exact two-sided selector |
| Default volume policy | Read `CAPI_JANITOR_DEFAULT_VOLUMES_POLICY`, defaulting to `delete` | Keep |
| Cluster volume policy | Let `janitor.capi.stackhpc.com/volumes-policy` override the default; only exact `delete` enables cleanup | Keep |
| Volume ownership | Require exact `cinder.csi.openstack.org/cluster=<cluster>` metadata | Keep |
| Volume keep property | Keep a volume only when `janitor.capi.azimuth-cloud.com/keep` is exactly `true` | Keep the case-sensitive comparison |
| Snapshot ownership | Require exact `cinder.csi.openstack.org/cluster=<cluster>` metadata when volume cleanup is enabled | Keep; there is no independent snapshot keep rule |
| OpenStack cleanup order | Floating IP, load balancer, security group, snapshot, volume, application credential | Run a read-only load-balancer and floating-IP safety preflight first, then keep the dependency order and make each phase level based |
| Credential policy | Read `janitor.capi.stackhpc.com/credential-policy` from the identity Secret; exact `delete` enables credential cleanup | Apply only to a direct application-credential Secret. Always retain password credentials and Secrets. In the community profile, ignore the annotation and always retain the `OpenStackClusterIdentity`, backing Secret, and credential |
| Last-finalizer rule | Delete the application credential and Secret only when the Janitor finalizer is the last finalizer | Keep |
| Other finalizers | Clean external resources while other finalizers remain, but defer application credential and Secret deletion | Keep without blocking a reconcile worker |
| Exact application-credential self-DELETE `403` | Warn and continue when a restricted credential cannot delete itself | Safety correction while keeping the Python non-blocking finalizer outcome. Record `retainedForbidden` and retain the direct Secret so the surviving credential is not orphaned |
| Delete `400` or `409` | Treat as pending and verify whether the selected resource remains | Keep the outcome through Gophercloud classification and requeue |
| Exact DELETE of an already-selected OpenStack resource returns `404` | The Python request raises an error | Idempotency correction. Treat it as complete; `NotFound` cannot authorize an unselected resource and does not cover inventory or base-URL failures |
| Other OpenStack error | Retry after the configured default delay | Keep the retry outcome using controller-runtime |
| Missing required service | Fail cleanup | Keep for Neutron, enabled Cinder, and an Octavia service that exists but has no usable selected endpoint. Absence of the Octavia service type makes the LB phase not applicable, but a matching bound FIP still blocks because shared-LB ownership cannot be verified |

All discovery remains limited to the project in the authenticated token and, when configured, the region selected from `clouds.yaml`.
An omitted `region_name` does not add a region filter.
Project and region scope do not replace the resource selectors.

For application-credential authentication, `credential-policy: delete` is a destructive ownership declaration for both the credential and its Secret.
A Secret shared by multiple clusters must not carry that annotation.

## Approved password extension

Password authentication is part of the replacement release.
This is an explicit compatibility extension from the earlier Go rewrite, not Python parity.
It does not add a resource type or broaden an ownership selector.
The implementation first appeared in the reverted Go rewrite in [`a681ba7`](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/commit/a681ba7) and is retained here by an explicit project decision.
Only its password authentication work is retained; the same commit's wider floating IP prefix is rejected in favor of the Python selector.

The approved part of the contract is:

- accept `v3password`, `password`, and an omitted `auth_type` when the selected cloud contains a complete and unambiguous password credential family
- require `auth_url` and `password`; resolve the user by `user_id` or by `username` plus a user domain
- require project scope by `project_id` or by `project_name` plus a project domain; common `domain_id`, `domain_name`, and `default_domain` fallbacks are accepted when they resolve the required user and project domains
- authenticate before discovery and require a non-empty project ID in the issued token; an explicitly configured project ID must match it
- reject mixed application-credential, password, and token families, and reject incomplete or contradictory input before any resource list request
- reject simultaneous `user_id` and `username`, simultaneous `project_id` and `project_name`, and simultaneous ID and name values for the same domain. A user- or project-specific domain overrides a generic domain fallback; two conflicting fallback values are rejected
- use the same configured region, interface, TLS, and resource ownership rules as application-credential authentication
- never attempt a Keystone application-credential delete for a password-authenticated cloud
- always retain a password identity Secret. An exact `credential-policy: delete` is ignored with a warning because it does not establish ownership of a reusable user password
- treat a password authentication failure as an ordinary authentication error; it is not evidence that an application credential was already deleted
- continue to reject token, federation, and other authentication methods

These accepted forms follow CAPO's Gophercloud client configuration and common `clouds.yaml` input while narrowing authentication to the project boundary that authorizes deletion.
Domain-only, system-scoped, and unscoped password tokens are not supported.
CAPO's own [password example](https://github.com/kubernetes-sigs/cluster-api-provider-openstack/blob/v0.14.7/docs/book/src/development/development.md#L171-L190) omits `auth_type`, and its [provider scope](https://github.com/kubernetes-sigs/cluster-api-provider-openstack/blob/v0.14.7/pkg/scope/provider.go#L227-L357) extracts the authenticated project ID.

## CAPO identity compatibility

CAPO `v0.14.7` supports both direct Secrets and `identityRef.type: ClusterIdentity`.
The latter refers to an `OpenStackClusterIdentity` object.
The replacement profile resolves only a direct Secret in the `OpenStackCluster` namespace and treats the legacy empty identity type as `Secret`.
It rejects `ClusterIdentity` as an unsupported profile before a cloud call.
The community profile may resolve an `OpenStackClusterIdentity` only when its namespace selector authorizes the `OpenStackCluster` namespace; unknown identity types are always rejected.
The sharing and authorization model is described in the [CAPO ClusterIdentity documentation](https://github.com/kubernetes-sigs/cluster-api-provider-openstack/blob/v0.14.7/docs/book/src/topics/openstack-cluster-identity.md).

`identityRef.cloudName` selects the cloud entry.
`identityRef.region`, when set, overrides the region in that entry.
Current CAPO `v1beta1` requires `identityRef.cloudName`.
The `openstack` default is only for an already-admitted migration object with an empty value.
The older `spec.cloudName` field is not available in the target Go type and is not part of the replacement profile.

The Janitor never owns or deletes an `OpenStackClusterIdentity`, its backing Secret, or the credential stored in it.
This remains true for an application credential and even if the backing Secret contains `credential-policy: delete`.

## Deliberate Go corrections

The replacement corrects unsafe Python failure handling without broadening a resource selector or adding a deletion target.
Each correction remains within the destructive boundary defined by the [resource ownership matrix](resource-ownership-matrix.md).
Typed Kubernetes Events and Prometheus metrics are observability extensions and do not change cleanup eligibility.

## Load balancer gate correction

The Python README says that OCCM service load balancers are always cleaned up.
The released controller does something narrower: it runs Octavia cleanup only when `spec.apiServerLoadBalancer.enabled` is true and `status.apiServerLoadBalancer.id` is non-empty.
The gate was introduced in [PR #218](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/218) to avoid an Octavia permission failure.
It can leave workload load balancers behind when a cluster does not use a CAPO API server load balancer.
The later [PR #261](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/261) removed the gate and was closed after the maintainer said the change had been integrated into the Go rewrite.

The replacement follows the documented cleanup policy and that later intent: matching workload load balancers are considered independently of CAPO API server load balancer state.
The Python name remains the positive selector; OCCM reserved tags only veto a name-selected candidate.
The [shared load balancer ownership rules](resource-ownership-matrix.md#shared-load-balancer-floating-ip) define the tag grammar, VIP floating IP protection, and fail-closed preflight.

## Octavia capability and error policy

The replacement does not add an operator-selectable load balancer skip policy.
It classifies the authenticated service catalog and API result:

| Observation | Result |
| --- | --- |
| The raw authenticated catalog has no `load-balancer` service type and no matching FIP is bound to a port | Treat the Octavia phase as not applicable, emit a warning and metric, and continue. A matching FIP with a successfully parsed null or empty-string `port_id` is unbound and may follow the legacy selector. [CAPO supports clouds without LBaaS](https://github.com/kubernetes-sigs/cluster-api-provider-openstack/blob/v0.14.7/README.md#features) |
| The catalog has no `load-balancer` service type and a matching FIP has a non-empty `port_id` | Block before mutation. Without Octavia inventory, the controller cannot prove that the bound FIP is not the VIP of a shared load balancer |
| The service type exists but the selected region or interface has no endpoint | Block cleanup and retain the finalizer |
| Any Octavia inventory or list request failure, including `401`, `403`, `404`, `429`, or `5xx` | Return the classified error and retain the finalizer. Exact DELETE `404` for an already-selected LB follows the general idempotency rule |
| Pagination, extraction, tag, or LB-to-FIP association is incomplete | Block before the first mutation |
| A name-selected LB has a foreign or malformed reserved tag | Preserve the LB and its attached floating IP, emit a warning, and continue with other owned resources |
| Tags are read successfully and contain no foreign reserved value | Apply the legacy name selector and normal cascade-delete policy |

CAPO API server load balancer fields never stand in for these observations.
A future explicit skip option requires separate evidence and design because it would knowingly permit workload resources to remain.

The implementation must inspect the raw authenticated catalog to distinguish an absent service type from a present service with no selected endpoint.
A generic endpoint-not-found error is not enough.
A successfully parsed null or empty-string FIP `port_id` is unbound; a missing field, type mismatch, or extraction failure makes the association unknown and blocks mutation.

## Retry and recovery

`CAPI_JANITOR_RETRY_DEFAULT_DELAY` remains a positive integer number of seconds with a default of `60`.
It is the maximum delay for operational errors, not the first exponential delay.
Per-object error backoff starts at one second and is capped by the configured value.
A malformed, zero, negative, or overflowing value prevents manager startup.
Expected states, such as an accepted delete that is still pending or another finalizer that remains, use a short `RequeueAfter`, normally five seconds.
Reconcile never sleeps or writes a retry annotation.
A successful reconcile resets that object's error backoff.

This separates operational errors from expected waits in the same way as the [controller-runtime reconcile contract](https://github.com/kubernetes-sigs/controller-runtime/blob/23d764b4479e3caf971380ba523f2c5dd4c175a8/pkg/reconcile/reconcile.go#L117-L127).

A missing or invalid direct Secret, `OpenStackClusterIdentity`, or backing Secret retains the finalizer before the recorded credential transition.
The controller watches the referenced identity objects, publishes a warning, and retries.
It does not remove the finalizer after a timeout and does not expose a force-finalize annotation.

Before `credentialDeleteStarted`, recovery may restore the referenced project-scoped credential or use a temporary identity only within the same ownership boundary.
The controller invalidates the pre-start checkpoint and repeats full inventory verification.
A change of authority, project, region, cluster UID, or effective cluster name fails closed.

An actively deleting object may be handed back to Python before `credentialDeleteStarted` only when an audit confirms:

- a valid same-namespace direct Secret using `v3applicationcredential`
- identical effective cloud and region selection without `identityRef.region`
- the same exact application credential ID in the Python `openstack` entry and the Go selected-cloud entry when deletion is enabled
- complete inventory without a foreign-tag shared LB, protected VIP FIP, or workload LB skipped by the Python CAPO status gate
- no state whose safe outcome depends on a Go-only correction in the regression ledger

Even an eligible handback returns to the Python controller's known restart and failure behavior.

At or after `credentialDeleteStarted`, changing the bound identity does not authorize automatic resume.
The break-glass procedure is out of band:

1. use an independent privileged credential to inspect the documented owned resources and the exact bound application credential
2. remove any remaining owned resources and record whether the application credential and direct Secret remain
3. complete or remove the checkpoint and finalizer manually only after that audit is complete

Manual finalizer removal can leave OpenStack resources and is not an automatic success path.
CAPI's [InfraCluster contract](https://github.com/kubernetes-sigs/cluster-api/blob/2250be7f46b9c1aadd968bdda0f92f6fb3e907a9/docs/book/src/developer/providers/contracts/infra-cluster.md#L544-L576) also requires cleanup to succeed before finalizer removal.

## Restart checkpoint

The controller stores restart state in the `janitor.capi.stackhpc.com/cleanup-state` annotation on the `OpenStackCluster`.
The value is versioned JSON.

The multi-phase checkpoint is required only when a direct application credential and Secret are eligible for deletion.
Password, retained direct application credentials, and `OpenStackClusterIdentity` paths have no destructive identity transition and can remove the finalizer after fresh resource verification.
When required, the checkpoint uses these phases:

| Phase | Meaning |
| --- | --- |
| `resourcesVerified` | A fresh, complete inventory found no remaining owned resource after the Janitor became the only finalizer |
| `credentialDeleteStarted` | The exact application credential delete is authorized and is the next or current external side effect |
| `credentialFinalized` | The credential outcome is recorded as `deleted`, `absent`, or `retainedForbidden` |
| `secretDeleteStarted` | Deletion of the exact direct Secret has been authorized |

The binding contains the checkpoint version, OpenStackCluster UID and generation, effective cluster name, identity type and reference, direct Secret UID and resource version when applicable, selected cloud name, auth type, authenticated project and user IDs, a SHA-256 digest of the normalized Keystone authority URL, region, interface, and exact application credential ID, plus the resolved volume and credential policies.
It never contains Secret data or the raw authority URL.

Finalizer order is not guaranteed.
`resourcesVerified` is therefore written only after the Janitor is the sole finalizer and a new full inventory succeeds.
The transition sequence is exact:

1. verify resources, patch `resourcesVerified`, and return
2. on the next reconcile, validate the binding and repeat a fresh full inventory. If a candidate appears, clear the pre-start checkpoint and resume cleanup. If inventory remains empty, patch `credentialDeleteStarted` and return before contacting Keystone
3. issue the exact credential delete; only exact `204`, bound-resource `404`, or self-delete `403` patches `credentialFinalized` with its classified outcome. A `401` or other failure remains at `credentialDeleteStarted`
4. for `deleted` or `absent`, patch `secretDeleteStarted` and return before the Secret request; for `retainedForbidden`, retain the Secret
5. delete the Secret with the recorded UID precondition, then remove the Janitor finalizer in a later conflict-safe patch

An unknown checkpoint version or malformed value always fails closed.
Before `credentialDeleteStarted`, a resolved policy change or valid identity change within the same ownership boundary invalidates the checkpoint and requires fresh verification.
The ownership boundary is the cluster UID, effective cluster name, normalized Keystone authority digest, authenticated project ID, and effective region.
A change to any of those fields fails closed.
At or after `credentialDeleteStarted`, every binding change fails closed.

A metadata or operator-default change that alters the effective volume policy invalidates a pre-`credentialDeleteStarted` checkpoint and requires fresh full inventory.
The resolved credential policy is also bound explicitly, in addition to the direct Secret resource version.
A replacement identity can re-enter verification only when it authenticates to the same authority, project, region, cluster UID, and effective cluster name.
Other pre-start boundary changes and all changes at or after `credentialDeleteStarted` require break-glass recovery.

Only `deleted` and `absent` authorize `secretDeleteStarted`.
A `retainedForbidden` result preserves the direct Secret and may then allow the Janitor finalizer to be removed with a warning.
An unverified result is not a finalized outcome and remains blocked at `credentialDeleteStarted`.

An audited, Python-equivalent direct application-credential object may be handed back before `credentialDeleteStarted`; later phases require the Go controller or break-glass procedure to finish.

## Post-checkpoint authentication outcomes

| Observation | Result |
| --- | --- |
| Any authentication failure before `resourcesVerified` | Block and retry |
| `401` at `resourcesVerified` | Block; no credential delete attempt is recorded |
| `401` after `credentialDeleteStarted` for the same bound application credential | Record `ApplicationCredentialDeletionUnverified`, emit a warning and metric, and retain the Secret and finalizer. Retry the exact delete if the same binding becomes valid; otherwise require the out-of-band break-glass audit. The write-ahead phase proves intent, not that the delete request ran |
| Exact application credential DELETE `204` | Record `credentialFinalized: deleted`; Secret deletion may proceed |
| Exact application credential DELETE `404` for the bound ID | Record `credentialFinalized: absent`; Secret deletion may proceed |
| Exact application credential DELETE `403` | Record `credentialFinalized: retainedForbidden`, retain the direct Secret, emit a warning, and allow cluster finalization. The credential may remain |
| Empty or mismatched catalog, service or endpoint lookup failure, or a Keystone base URL `404` | Block; these do not prove that the credential is absent |
| TLS, DNS, timeout, `429`, or `5xx` | Return an error and retry |
| Password authentication `401` | Always block; password never uses the application credential replay exception |
| Direct Secret is absent after `secretDeleteStarted` | Mark the Secret phase complete |
| Direct Secret is absent before `secretDeleteStarted` | Block and use the recovery procedure |

`credentialDeleteStarted` proves authorization, not that the process reached Keystone, so a later `401` cannot advance the state automatically.

## Supported CAPO lanes

The first replacement evidence targets the Azimuth chart `0.28.0` release train: CAPI `v1.14.0`, CAPO `v0.14.7`, OpenStackCluster `v1beta1`, and a direct Secret.
The versions come from the [Azimuth CAPI defaults](https://github.com/azimuth-cloud/ansible-collection-azimuth-ops/blob/711d8dbe44be93be7e709c1eff2472faf54cc2a0/roles/clusterapi/defaults/main.yml#L3-L11).
CAPO `v0.14.7` itself depends on CAPI `v1.12.10`.
The community suite must cover that upstream baseline and `OpenStackClusterIdentity` resolution before the project advertises the wider `v0.14.x` profile.
This does not gate the Azimuth CAPI `v1.14.0` tuple.

CAPO `v0.15.0-beta.0` is a planned preview lane.
It must cover native `v1beta2` and a converted `v1beta1` object.
The project does not claim stable `v1beta2` support until CAPO `v0.15` is stable and the preview, conversion, and migration tests pass.
CAPI `v1.16` support must pass the native `v1beta2` lane rather than relying on served `v1beta1` conversion.
Move the implementation dependency from CAPO `v0.14.6` to `v0.14.7` before collecting replacement evidence.
Current release status is tracked in the [CAPO releases](https://github.com/kubernetes-sigs/cluster-api-provider-openstack/releases).
