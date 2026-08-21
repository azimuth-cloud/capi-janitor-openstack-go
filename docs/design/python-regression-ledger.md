# Python regression ledger

## Purpose

The Python controller's issue and pull request history is part of the replacement contract.
This ledger records each relevant failure and the evidence required from the Go controller.

The compatibility baseline is Python `0.15.0` at [`f14d860`](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/tree/f14d86013d78ac3a4b07f5a2669a8f49590e13ca).
Use the [compatibility policy](python-compatibility-policy.md) for decisions, the [resource ownership matrix](resource-ownership-matrix.md) for selectors, and the [cleanup behavior matrix](cleanup-behaviour-matrix.md) for observable outcomes and tests.

## Historical regressions

| History | Failure to prevent | Required Go evidence |
| --- | --- | --- |
| [PR #21](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/21) | Cinder snapshots can remain after cluster deletion | Exact snapshot metadata fixtures and snapshot cleanup before volume cleanup |
| [PR #23](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/23), [PR #26](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/26) | Credential deletion can run without explicit approval, or before CAPO and other finalizers finish | Exact `credential-policy: delete` tests, waiting when another finalizer remains, and restart injection around every credential step |
| [PR #27](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/27) | Octavia leaves child resources when a service load balancer is deleted | Assertion that the delete request enables cascade deletion |
| [PR #32](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/32), [PR #182](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/182) | A catalog entry without a usable endpoint causes a panic or false success | Missing endpoint and wrong interface or region fixtures that retain the finalizer |
| [PR #52](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/52) | A CAPO API version change breaks the watch or identity contract | A controller test with installed `v1beta1` CRDs and release evidence that records the tested CAPO fixture version |
| [PR #102](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/102) | Retry backoff overflows or stalls reconciliation | Bounded `RequeueAfter` and workqueue rate limiter tests with no sleep inside reconcile |
| [PR #116](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/116), [PR #285](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/285) | Keystone `/v3` handling duplicates or loses an identity deployment path | Authentication fixtures for `/v3`, `/identity/v3`, and trailing slashes |
| [PR #117](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/117) | Endpoint selection ignores `region_name` | Catalog tests with and without a selected region |
| [PR #147](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/147) | OCCM service security groups remain | Exact positive and negative description fixtures |
| [PR #165](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/165) | ClusterClass topology uses a cluster name different from `metadata.name` | Label precedence across every selector, plus release validation that workload cluster names are unique within each OpenStack project |
| [PR #167](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/167) | DevStack's `block-storage` catalog name is not recognized | `volumev3` and `block-storage` endpoint tests |
| [PR #176](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/176) | A Cinder volume marked to be kept is deleted | An exact lowercase `keep=true` test and similar values that must not match |
| [PR #186](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/186), [PR #187](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/187) | A handler fails to persist the finalizer | Envtest that creates an object and observes its finalizer, plus a conflict retry test |
| [PR #218](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/218), [PR #261](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/261) | The CAPO status workaround leaks workload load balancers, while an Octavia error can be mistaken for an empty inventory | Complete the inventory regardless of CAPO API server load balancer status. A complete inventory with no matches finishes normally. Endpoint, list, pagination, tag, and association errors block cleanup. Tags for Services in the target cluster allow deletion. Foreign or malformed reserved tags preserve the LB and VIP FIP. A successfully read complete tag result with no reserved tags uses the legacy selector |
| [PR #234](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/234) | Slow OpenStack APIs exceed the client timeout | Gophercloud client timeout fixed at 60 seconds with context cancellation coverage |

## Defects found during the replacement audit

These failures do not have a single upstream pull request, but they require the same release evidence:

| Python failure | Replacement rule | Required Go evidence |
| --- | --- | --- |
| Missing identity Secret can lead to finalizer removal | Before the recorded Secret deletion phase, fail closed and retain the finalizer. Absence after `secretDeleteStarted` is idempotent completion | Envtest for a missing Secret before `secretDeleteStarted`, absence after that phase, and Secret watch recovery |
| Python always reads the application credential ID from the `openstack` cloud, even when another cloud is selected | Read the ID from the selected cloud | A fixture with several clouds and different IDs |
| Credential or Secret deletion can leave an unsafe restart ambiguity | After a fresh complete inventory with no matches, record `credentialDeleteStarted` before deleting the exact credential. Record `secretDeleteStarted` before deleting the Secret. Treat authentication failure as unverified | Failure injection before and after application credential, Secret, and finalizer operations. Cover `204`, `404` for the recorded credential ID, `403`, and `401` after deletion starts |
| An empty or mismatched catalog can be treated as proof that the credential was deleted | Accept an absent credential only when an exact DELETE of the recorded credential ID returns `404` | Authentication and catalog failures retain the finalizer. Only `204` or `404` from an exact DELETE of the recorded credential ID permits Secret deletion |
| Optional Octavia and Cinder clients are created before their phase is needed | Create a service client only for a required phase | Disabled Cinder does not require an endpoint. A missing or unusable Octavia service blocks cleanup instead of becoming an empty inventory |
| A matching FIP can be deleted before a later shared LB check protects the LB | Complete the ownership preflight for OCCM reserved tags and associations from LB VIP ports to FIPs before any mutation | Tags for Services in the target cluster permit deletion. Foreign or malformed reserved tags preserve the LB and attached FIP. A successfully read complete result with no reserved tags uses the legacy selector. Missing association facts block all mutation |
| A missing, incorrect, or duplicate cluster name can make ownership unsafe | Require a name that is not empty, matches OCCM and Cinder CSI, and is unique among workload clusters in each OpenStack project | Tests for label precedence and an empty name make no unsafe cloud request. Release validation records the shared name and the requirement for unique names within the project |
| Finalizer patch can overwrite concurrent metadata | Patch only metadata owned by Janitor and retry conflicts | Envtest covers a conflict and preserves an unrelated finalizer |

## Release use

Before the replacement release, map each row to a named Go test or a tracked issue.
Required replacement behavior must be implemented and verified before release, and aggregate coverage cannot replace this mapping.
