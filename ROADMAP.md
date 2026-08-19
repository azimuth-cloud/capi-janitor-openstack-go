# Roadmap

## Goal

This project must replace the Python
[`cluster-api-janitor-openstack`](https://github.com/azimuth-cloud/cluster-api-janitor-openstack)
controller in production. The replacement release keeps the Python controller's
cleanup scope and legacy finalizer. It also fixes failure modes that can leak
resources, lose credentials, or remove the finalizer too early.

Replacement does not mean that the two implementations use the same internal
mechanisms. It means that operators can move to the Go controller, preserve the
same ownership boundary, complete deletion safely after failures or restarts,
and roll back without manual object repair.

The Go controller may support an explicitly approved compatibility extension,
such as password authentication. It must not add a deletion target as an
incidental part of the rewrite.

This roadmap uses three release stages:

- a **development release** is available for controlled evaluation but does
  not meet the replacement criteria
- the **replacement release** is the first release approved to replace the
  Python controller in production
- a **post-replacement extension** adds scope or integrations after that
  boundary is proven

Implementation behavior was audited at commit `d9451d9` on 2026-08-18. Refresh
this assessment after implementation changes.

## Current assessment

Track implementation coverage separately from replacement readiness.

| Measure | Assessment | What it means |
| --- | --- | --- |
| OpenStack cleanup data path | Advanced | Authentication, typed Gophercloud services, pagination, selectors, project and region boundaries, exact-ID deletion, and most error classification are implemented and unit tested. |
| Replacement release criteria | 2 complete, 3 partial, 6 open | The table below records the evidence for all 11 criteria. These counts measure acceptance, not engineering effort. |
| Safe replacement readiness | Not ready | The controller still blocks workers with polling and sleeps. Restart-safe credential cleanup, conflict-safe finalizers, envtest, destructive boundary tests, and migration and rollback evidence are missing. |

Status terms used below:

- **Complete**: the active runtime path and relevant tests provide evidence
- **Partial**: useful code exists, but the release criterion is not yet met
- **Open**: required work or a maintainer decision remains
- **Deferred**: post-replacement work that does not block replacement

## Replacement release criteria

The normative requirements are in the
[Go rewrite guidelines](docs/design/go-rewrite-guidelines.md),
[cleanup behavior matrix](docs/design/cleanup-behaviour-matrix.md), and
[resource ownership matrix](docs/design/resource-ownership-matrix.md).

| Release criterion | Status | Evidence and remaining work |
| --- | --- | --- |
| Design documents match shipped behavior | Partial | The documents now separate the target from current progress. The service load balancer policy and request timeout still need decisions; credential `403` reporting and auth-type enforcement still need implementation. |
| Every ownership rule has positive and negative tests | Partial | Unit and HTTP fixtures cover the exact selectors, including composed owned and near-match resources. A real OpenStack project and region boundary test and restart-safe credential lifecycle fixtures are still missing. |
| Selectors are no broader than the Python baseline | Complete | The active path uses exact description boundaries, the `kube_service_<cluster>_` prefix, exact Cinder metadata, the exact keep value, and the selected application credential ID. |
| Intentional differences are agreed, documented, and tested | Partial | Selected-cloud credential IDs, lazy optional clients, password authentication, and fail-closed inventory are documented. The unresolved load balancer and timeout decisions prevent completion. |
| Gophercloud performs OpenStack API operations | Complete | The active purge path uses typed Neutron, Octavia, Cinder, and Keystone services. The unused manual HTTP implementation should be removed after equivalent regression coverage is confirmed. |
| Reconcile has no blocking sleeps or in-process polling | Open | Resource deletion polls up to six times with five-second waits. The controller also sleeps after failure and while another finalizer remains. |
| Finalizer updates are conflict-safe | Open | The controller still uses full-object `Update` calls for finalizer changes. |
| Cleanup resumes after a process restart | Open | There is no persisted resources-verified checkpoint. Deleting the application credential or Secret before a failed finalizer update can leave deletion permanently blocked. |
| Envtest covers controller behavior | Open | Controller tests use a fake client. There is no envtest coverage for CRD watches, pause, filters, Secret events, conflicts, or restarts. |
| A real OpenStack test proves deletion and non-deletion | Open | The privileged Azimuth workflow exercises a real deployment, but it does not create owned and near-match fixtures and assert both sides of the ownership boundary. |
| Python-to-Go migration and rollback are tested | Open | No repeatable migration or rollback scenario is recorded in this repository. |

The replacement release is not ready while any criterion is
Partial or Open.

## What is implemented

The following work is on the active Go path at the reviewed commit:

- Gophercloud authentication for application credentials and password
  credentials, including selected cloud, region, interface, custom CA data, and
  reauthentication
- lazy service client creation, so a disabled cleanup phase does not require an
  unrelated OpenStack endpoint
- typed Neutron, Octavia, Cinder, and Keystone list and delete operations
- complete pagination and fail-closed handling of incomplete inventory
- exact ownership selectors and exact-ID deletion
- Octavia cascade deletion and resource-specific delete error classification
- cluster name label fallback, volume policy, the current load balancer policy
  gate, legacy finalizer, Events, and cleanup metrics
- Helm, Kustomize, Nix, GoReleaser, and privileged Azimuth integration plumbing

The local audit passed the internal Go tests and `go vet`. Test count and
statement coverage are not release gates; they do not cover the restart and
destructive boundaries listed above.

## Work to reach safe replacement

### 1. Close behavior gaps

Resolve these before reshaping the controller. Each contract change needs a
behavior matrix entry and a positive and negative test.

1. Replace the CAPO API server load balancer status gate. It reintroduces the
   leak reported in Python
   [PR #261](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/261),
   but removing it before defining shared load balancer and reserved-tag safety
   can make cascade deletion too broad. Decide the replacement policy and close
   negative fixtures first. List failures remain fatal. Any permission-only
   escape hatch must be explicit and emit a Warning Event.
2. Implement a distinct application-credential-may-remain outcome and Warning
   Event after the narrow self-deletion `403` exception. Do not emit an
   unqualified cleanup success.
3. Enforce the documented `v3applicationcredential` and `v3password`
   `auth_type` values. The current inference for empty and alias values must not
   become public compatibility by accident.
4. Reconcile the current 30-second HTTP request timeout with the Python
   controller's 60-second timeout fix. Prefer an explicit, tested setting.

### 2. Make reconciliation level based

- implement the `internal/cleanup.Runner` as one bounded iteration
- move selector and phase decisions out of concrete OpenStack clients
- return `OutcomeWaiting` with `RequeueAfter` while OpenStack completes a delete
- return operational failures to controller-runtime for workqueue backoff
- remove polling loops, `time.Sleep`, and random retry annotation writes
- watch the referenced Secret and the owning CAPI Cluster where required
- honor standard CAPI pause and watch-filter behavior
- reject unsupported `identityRef.type` values before making cloud requests
- pass `Secret.Data` bytes to the parser without a second base64 decode

### 3. Make finalization restart safe

- patch Janitor-owned metadata without replacing concurrent updates
- persist a Janitor-owned resources-verified checkpoint before deleting an
  application credential
- make the checkpoint state machine converge when the application credential
  or Secret has already disappeared
- remove the checkpoint and finalizer only after all required work is verified
- emit Events and metrics for complete, waiting, blocked, and
  credential-may-remain outcomes at the correct lifecycle point
- add failure injection around checkpoint, credential, Secret, and finalizer
  operations

### 4. Prove the controller boundary

- add pure cleanup runner tests for ordering and checkpoint transitions
- add envtest with the CAPI and CAPO CRDs
- cover pause, watch filters, Secret changes, API conflicts, process restarts,
  and deletion with another finalizer
- make the Kind suite exercise a real `OpenStackCluster` watch instead of only
  manager and metrics smoke checks
- align the Kind manager image reference with
  `ghcr.io/azimuth-cloud/capi-janitor-openstack-go`
- add a real OpenStack test that deletes owned fixtures and preserves similar
  non-owned fixtures in the same run
- add named regressions for every item in the Python defect ledger below

### 5. Validate migration and release

- document installation, canary selection, migration, rollback, and recovery
  from a stuck finalizer
- verify adoption of `janitor.capi.stackhpc.com` on existing objects
- prove that Python and Go controllers are never active on the same objects
- test migration and rollback with deletion in progress
- expose metrics through the Helm chart and verify its RBAC and restricted Pod
  Security admission behavior
- test the stable CAPO `v0.14.7` and `v1beta1` lane and the active CAPO `v0.15`
  and CAPI `v1beta2` lane defined by the
  [version strategy](docs/design/capo-integration-boundary.md#version-strategy)
- publish the supported dependency matrix and an operator troubleshooting guide
- run a representative soak before declaring the Python controller superseded

## Python defect ledger

"Fix all known bugs" is only verifiable when each report has a disposition and
a regression test. This ledger covers the public runtime reports and fixes
found during the review. Add private incident references before the replacement
release if they exist.

| Source | Failure or compatibility concern | Go disposition | Status |
| --- | --- | --- | --- |
| [Issue #1](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/issues/1), [PR #165](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/165) | ClusterClass deployments can use a cluster name different from `OpenStackCluster.metadata.name`. | Prefer the CAPI cluster-name label and test the fallback. | Complete |
| [Issue #187](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/issues/187), [PR #186](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/186) | A missing `await` prevented finalizer installation; CRD RBAC was also incomplete. | Go removes the coroutine failure class and has basic finalizer and RBAC tests. Add envtest and Helm installation coverage. | Partial |
| [Issue #200](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/issues/200) | OCCM used a different cluster name, so its load balancers were not selected. | Document the shared cluster-name requirement. Add startup or deletion diagnostics where the value can be checked safely. | Open |
| [PR #21](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/21), [PR #26](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/26) | Snapshots need cleanup, and application credentials must remain until CAPO has finished. | Snapshot deletion is implemented. Credential and other-finalizer ordering still needs restart-safe controller tests. | Partial |
| [PR #27](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/27) | Octavia child resources can block load balancer deletion. | Use cascade deletion. | Complete |
| [PR #32](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/32) | Catalog entries without usable endpoints can block unrelated cleanup. | Gophercloud and lazy clients replace the hand-written catalog path. Keep a named regression fixture. | Partial |
| [PR #52](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/52) | The watched CAPO API moved to `v1beta1`. | The controller imports CAPO `v1beta1`. Add a supported-version matrix and track `v1beta2`. | Partial |
| [PR #102](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/102) | Retry backoff overflowed during long failures. | Use controller-runtime requeue and backoff. The current sleep and annotation mechanism still needs removal. | Open |
| [PR #116](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/116), [PR #167](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/167), [PR #285](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/285) | Identity and service catalog URLs may contain versioned or deployment-specific prefixes and service aliases. | Delegate endpoint discovery to Gophercloud. Add named fixtures for the reported catalog shapes. | Partial |
| [PR #117](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/117) | Cleanup could use an endpoint from the wrong region. | Select the configured region and test it. | Complete |
| [PR #147](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/147) | OCCM service security groups remained. | Preserve the exact description selector and delete by ID. | Complete |
| [PR #176](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/176) | Volumes explicitly marked to keep could be removed. | Preserve the exact `keep=true` rule and its negative fixtures. | Complete |
| [PR #182](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/182) | Missing optional service endpoints blocked cleanup even when that phase was disabled. | Create optional clients lazily. | Complete |
| [PR #218](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/218), [PR #261](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/261) | Skipping Octavia avoids permission failures, but the API server load balancer gate can leave OCCM service load balancers behind. | Replace the status gate with a reviewed policy that keeps list errors fatal and protects shared load balancers. Design any explicit skip policy separately. | Open |
| [PR #234](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/pull/234) | Slow OpenStack calls exceeded the old client timeout. | The Go client currently uses 30 seconds while the Python fix used 60 seconds. Define and test the supported timeout. | Open |

The selected-cloud application credential ID bug, missing Secret safety issue,
annotation churn, optional endpoint coupling, and credential restart window were
also found during implementation review. The replacement work above covers them
even where no standalone Python issue exists.

## CAPO and OpenStack adoption track

Upstream reports and integrations justify evaluating this independent
controller with CAPO and OpenStack operators. They do not yet establish broad
adoption or a support commitment. The evidence, responsibility split, and its
limits are recorded in the
[CAPO integration boundary](docs/design/capo-integration-boundary.md).

After the replacement criteria are complete:

1. publish the real OpenStack owned and non-owned fixture results
2. ask CAPO and cloud-provider-openstack maintainers to review lifecycle timing,
   ownership evidence, and permissions
3. publish an environment-neutral deployment guide and invite a second
   OpenStack operator to run the migration and rollback test
4. review any new integration or cleanup scope as a post-replacement extension

## Definition of replacement complete

The Python controller is superseded only when all of the following are true:

- every replacement release criterion above is complete
- every known Python defect has a complete, deferred, or explicitly rejected
  disposition with a reason
- a supported-version matrix and operator runbook are published
- migration and rollback pass in a representative environment
- a Go release completes a soak with no unresolved high-severity cleanup issue
- maintainers can archive or clearly deprecate the Python repository without
  leaving users on an unsupported path

Post-replacement CAPO integrations remain subject to the same ownership and
deletion-safety requirements.
