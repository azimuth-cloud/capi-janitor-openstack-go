# Python regression ledger

## Purpose

The Python controller's issue and pull request history is part of the replacement contract.
This ledger records each relevant failure and the evidence required from the Go controller.

The compatibility baseline is Python `0.15.0` at [`f14d860`](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/tree/f14d86013d78ac3a4b07f5a2669a8f49590e13ca).
Use the [compatibility policy](python-compatibility-policy.md) for decisions, the [resource ownership matrix](resource-ownership-matrix.md) for selectors, and the [cleanup behavior matrix](cleanup-behaviour-matrix.md) for observable outcomes and tests.

## Historical regressions

| History | Failure to prevent | Required Go evidence |
| --- | --- | --- |
| [PR #21](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/21) | Cinder snapshots can remain after cluster deletion | Exact snapshot metadata fixtures and snapshot-before-volume cleanup |
| [PR #23](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/23), [PR #26](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/26) | Credential deletion is not opt-in, or runs before CAPO and other finalizers finish | Exact `credential-policy: delete` tests, another-finalizer wait, and restart injection around every credential step |
| [PR #27](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/27) | Octavia leaves child resources when a service load balancer is deleted | Cascade-delete request assertion |
| [PR #32](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/32), [PR #182](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/182) | A catalog entry without a usable endpoint causes a panic or false success | Missing endpoint and wrong interface or region fixtures that retain the finalizer |
| [PR #52](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/52) | A CAPO API version change breaks the watch or identity contract | Tested CAPO version matrix and CRD-backed controller tests for each advertised lane |
| [PR #102](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/102) | Retry backoff overflows or stalls reconciliation | Bounded `RequeueAfter` and rate-limiter tests with no in-process sleep |
| [PR #116](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/116), [PR #285](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/285) | Keystone `/v3` handling duplicates or loses an identity deployment path | Authentication fixtures for `/v3`, `/identity/v3`, and trailing slashes |
| [PR #117](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/117) | Endpoint selection ignores `region_name` | Selected-region and no-region catalog tests |
| [PR #147](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/147) | OCCM service security groups remain | Exact positive and negative description fixtures |
| [PR #165](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/165) | ClusterClass topology uses a cluster name different from `metadata.name` | Label-precedence test across every selector |
| [PR #167](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/167) | DevStack's `block-storage` catalog name is not recognized | `volumev3` and `block-storage` endpoint tests |
| [PR #176](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/176) | A user-marked Cinder volume is deleted | Case-sensitive exact `keep=true` positive and near-match tests |
| [PR #186](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/186), [PR #187](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/187) | A handler fails to persist the finalizer | Envtest creation-to-finalizer assertion and conflict retry |
| [PR #218](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/218), [PR #261](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/261) | Octavia permissions block deletion, or the CAPO status workaround leaks workload load balancers | All CAPO enabled/status combinations; absent service with no, unbound, and bound matching FIPs; unusable endpoint; fail-closed Octavia inventory; and foreign-tag shared LB plus VIP FIP preservation |
| [PR #234](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/234) | Slow OpenStack APIs exceed the client timeout | Gophercloud client timeout fixed at 60 seconds with context cancellation coverage |

## Defects found during the replacement audit

These failures do not have a single upstream pull request, but they require the same release-level evidence:

| Python failure | Replacement rule | Required Go evidence |
| --- | --- | --- |
| Missing identity Secret can lead to finalizer removal | Before the recorded Secret-delete phase, fail closed and retain the finalizer; absence after `secretDeleteStarted` is idempotent completion | Missing-Secret envtest before the phase, absence after the phase, Secret watch recovery, and break-glass runbook |
| Application credential ID is read from the hard-coded `openstack` cloud | Read the ID from the selected cloud | Multi-cloud fixture with different IDs |
| Credential or Secret deletion can leave an unrecoverable restart window | Persist resources-verified state before either delete; treat post-start `401` as unverified rather than proof of deletion | Failure injection before and after application credential, Secret, and finalizer operations; `204`, exact `404`, `403`, and post-start `401` outcomes |
| Empty or mismatched catalog can be treated as an already-deleted credential | Accept an already-absent credential only from an exact bound-resource result | Pre-checkpoint and post-checkpoint authentication outcome table tests |
| Optional Octavia and Cinder clients are created before their phase is needed | Create a service client only for a required phase | Disabled Cinder does not require an endpoint; absent Octavia capability follows the bound-FIP safety rule |
| A matching FIP can be deleted before a later shared-LB veto protects the LB | Complete the LB/tag/VIP-to-FIP ownership preflight before any mutation | A foreign-tag shared LB and attached FIP remain while unrelated owned resources are deleted; missing association facts block all mutation |
| Missing or empty cluster name can produce an unsafe selector | Reject an empty resolved name before discovery | Empty label and metadata name fixtures with no cloud request |
| Finalizer patch can overwrite concurrent metadata | Patch only Janitor-owned metadata and retry conflicts | Envtest conflict and unrelated-finalizer preservation |

## Release use

Before the replacement release, map each row to a named Go test or an explicit, reviewed deferral.
Ownership, finalizer, and restart-safety rows cannot be deferred, and aggregate coverage cannot replace this mapping.
