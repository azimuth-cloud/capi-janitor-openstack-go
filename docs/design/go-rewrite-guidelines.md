# Go rewrite guidelines

## Why this document exists

`cluster-api-janitor-openstack` is being rewritten in Go in a separate repository.
This document defines the behavior the rewrite preserves and the implementation boundaries it changes.

The first goal is a safe replacement for the existing Python controller, not a broader OpenStack cleanup controller.
Additional cleanup responsibilities require separate design proposals after the replacement has been released and validated in a representative environment.

The Python baseline is release `0.15.0` at [`f14d860`](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/tree/f14d86013d78ac3a4b07f5a2669a8f49590e13ca).
The [Python compatibility policy](python-compatibility-policy.md), [cleanup behavior matrix](cleanup-behaviour-matrix.md), and [resource ownership matrix](resource-ownership-matrix.md) record the target contract.

> [!IMPORTANT]
> The repository already contains an initial Go implementation.
> These documents define the **target behavior and design boundaries** used to review that implementation and future changes.
> They do not imply that every requirement is currently unimplemented.
>
> Implementation progress should be tracked separately from these design documents.

## Controller framework

The project will retain the existing Kubebuilder v4 scaffold and use controller-runtime as the controller framework.

Direct use of lower level client-go should be limited to cases where the required functionality is not reasonably available through controller-runtime.

Evaluating or migrating to another controller framework is outside the scope of the replacement release.

## What feature parity means

Feature parity does not mean translating each Python function line by line.
It means keeping the same external purpose, cleanup scope, configuration, and successful outcomes.

For the replacement release, compatibility covers:

- watching CAPO `OpenStackCluster` objects
- adding and removing the existing Janitor finalizer
- identifying the cluster name in the same way as the Python controller
- removing the same OCCM floating IPs, service load balancers, and service security groups under the replacement ownership policy
- removing the same Cinder snapshots and volumes when volume policy is `delete`
- keeping volumes marked with the existing keep property
- attempting application credential and Secret cleanup under the existing policy

The following are not part of the replacement release:

- CAPO-managed networks, subnets, routers, ports, security groups, and bastions
- CAPO API server load balancers
- `OpenStackMachine` servers
- arbitrary project cleanup
- keypair cleanup
- new naming conventions that the Python controller does not recognize
- authentication methods other than application credential and password
- new resource types from open or unmerged changes in the Python repository

The Go implementation may correct unsafe failure handling and behavior defects approved in the compatibility policy.
Resource types and name or metadata selectors do not expand in the replacement release.
The [compatibility policy](python-compatibility-policy.md#policy-surface) defines the direct-Secret replacement profile and the separate `OpenStackClusterIdentity` community profile.

## Public policy surface

Only the volume policy and opt-in deletion of a direct application credential and Secret are user choices.
All other decisions are fixed safety rules defined by the [compatibility policy](python-compatibility-policy.md#policy-surface).
The replacement adds no cleanup target, load balancer skip setting, password Secret deletion, force-finalize mode, or Janitor CRD.

## When the scope can grow

The cleanup scope remains frozen until all of the following are true:

- the behavior and ownership matrices are implemented as tests
- the manual OpenStack client has been replaced by Gophercloud
- controller behavior is covered by envtest with CAPI and CAPO CRDs
- destructive behavior is covered by a real OpenStack end-to-end test
- migration from the Python controller, deployment rollback with no active deletion, and audited in-flight handback before the destructive credential transition have been tested
- at least one Go release has been deployed in a representative environment
- there are no unresolved critical or high-severity cleanup safety issues

Reaching this point does not automatically add new responsibilities.
A proposed extension still needs its own design document, ownership rules, negative test fixtures, rollout plan, and maintainer agreement.

## Design boundaries

The code should be split into three parts with clear responsibilities.

### Controller

The controller handles Kubernetes concerns:

- reading `OpenStackCluster` and related Kubernetes objects
- honoring pause and watch-filter behavior
- managing the Janitor finalizer
- resolving configuration and policy
- calling one cleanup iteration
- returning a result or error to controller-runtime
- publishing Events, metrics, and structured logs

It should not know OpenStack URL formats, response bodies, or pagination details.

### Cleanup logic

The cleanup package decides:

- which candidates match the Python ownership rules
- which policy applies
- which deletion phase should run next
- whether cleanup is complete, waiting, or blocked

This package should use small domain types and ordinary Go interfaces.
It should not depend on controller-runtime or concrete Gophercloud service clients.

### OpenStack resource services

The OpenStack resource services handle:

- authentication and service client creation
- listing every page of candidate resources
- converting Gophercloud results into the domain types
- making delete requests
- classifying OpenStack errors
- propagating the reconcile context

The services provide facts. They do not decide whether a resource belongs to a cluster.

## Controller conventions

Reconciliation must be level-based and idempotent. The controller should be able to repeat any step after a restart without deleting a different resource or incorrectly completing cleanup.

A reconcile should follow this general shape:

1. Read the `OpenStackCluster`
2. Resolve the owning CAPI `Cluster` where it is needed for pause handling
3. Return without mutation when the `Cluster` or `OpenStackCluster` is paused
4. Add the legacy Janitor finalizer on a non-deleting object
5. Return without cleanup if deletion has not started
6. Return if no recognized Janitor finalizer is present
7. Read and validate the Janitor checkpoint before deciding whether identity data is still required
8. Resolve the direct Secret when the recorded phase requires OpenStack access. The community profile may instead resolve an authorized `OpenStackClusterIdentity`
9. Run one bounded cleanup iteration or one recorded credential transition
10. Return `RequeueAfter` when OpenStack is still completing an accepted delete
11. Return an error when an operation has failed
12. Remove the finalizer only after required cleanup has been verified

Reconcile must never sleep, poll in a loop, or write retry annotations.
Expected waits use `RequeueAfter`; failures use the configured per-object workqueue rate limiter defined by the [compatibility policy](python-compatibility-policy.md#retry-and-recovery).

Finalizers and Janitor-owned annotations should be changed with a patch rather than a full-object update.
The controller must not write Janitor conditions into `OpenStackCluster.status`, because that status is owned by CAPO.
Events, metrics, and logs are sufficient for the replacement release.

## Finalizer compatibility

The Python controller uses `janitor.capi.stackhpc.com`.
Existing `OpenStackCluster` objects may already carry it, so the replacement release must continue to use and recognize that value.

Migration has three rules:

- do not run the Python and Go controllers against the same objects at the same time
- allow the Go controller to adopt objects that already have the legacy finalizer
- do not rename the finalizer as part of the rewrite

A qualified replacement finalizer can be considered later.
It needs a separate migration design and must account for objects that are already deleting and for rollback to an older controller.

## Gophercloud usage

Gophercloud `v2` and Gophercloud `utils` should be direct dependencies.
The project should not maintain its own implementation of Keystone authentication, service catalog parsing, endpoint construction, token refresh, pagination, or service-specific response models.

The replacement OpenStack client keeps the Python identity surface and the approved password extension:

- a same-namespace Secret referenced by `OpenStackCluster.spec.identityRef`
- an authorized `OpenStackClusterIdentity` and its backing Secret in the later community profile
- a `clouds.yaml` entry selected by `cloudName`
- `v3applicationcredential` authentication
- project-scoped password authentication in the forms defined by the compatibility policy
- the identity endpoint path, including a deployment prefix such as `/identity`
- interface and optional region values from the selected `clouds.yaml` entry, with `identityRef.region` taking precedence
- both Python-compatible Cinder catalog names, `volumev3` and `block-storage`
- the optional CA certificate stored in the Secret
- a 60-second request timeout, matching the Python controller

Gophercloud may support more configurations, but the Janitor validates only the [documented authentication and identity profiles](python-compatibility-policy.md#policy-surface).

> [!IMPORTANT]
> CAPO API types are part of the watched contract, but CAPO internal packages and broad implementation interfaces should not become dependencies of the Janitor.

## Cleanup progression

Before its first mutation, the runner completes the LB/FIP safety preflight defined by the [ownership matrix](resource-ownership-matrix.md#shared-load-balancer-floating-ip).
Incomplete inventory or association facts block mutation, and CAPO API server load balancer status is never a cleanup gate.

After the preflight, the successful dependency order is:

1. floating IPs that are not attached to a protected shared LB
2. service load balancers that pass the legacy name selector and reserved-tag veto
3. service load balancer security groups
4. snapshots when volume policy is `delete`
5. volumes when volume policy is `delete`
6. direct application credential when its direct Secret policy permits it and the selected cloud uses application-credential authentication
7. direct credential Secret when that policy and confirmed credential outcome permit it
8. Janitor finalizer

The Go controller may spread this work across several reconciliations. An accepted OpenStack delete request is not proof that the resource is gone.
Before moving past a dependency or removing the finalizer, the controller must observe the required resource as absent.

> The important dependency pairs are floating IP before load balancer, load balancer before its service security group, and snapshot before volume.

Independent errors may be collected when that is safe, but an incomplete inventory must never be treated as successful cleanup.

## Credential cleanup

Application credential deletion is the last OpenStack operation because it removes the controller's ability to inspect the project.
Password and community identity profiles have no credential deletion phase, and their Secrets are always retained.

Before deleting an eligible direct application credential or Secret, the controller persists the versioned `janitor.capi.stackhpc.com/cleanup-state` annotation and returns after each state change.
The write-ahead phases are `resourcesVerified`, `credentialDeleteStarted`, `credentialFinalized`, and `secretDeleteStarted`.
Only the exact `deleted` and `absent` outcomes authorize Secret deletion; `retainedForbidden` keeps the Secret, and unverified outcomes remain blocked.

The [compatibility policy](python-compatibility-policy.md#restart-checkpoint) defines the binding, transition order, response classification, and recovery boundary.
The controller must fail closed on missing identity before the recorded Secret deletion phase, malformed state, incomplete inventory, or an unauthorized binding change.

## Dependencies

Kubernetes, controller-runtime, CAPI, CAPO, Gophercloud, and the Go version should be selected as one tested dependency train.
The [supported CAPO lanes](python-compatibility-policy.md#supported-capo-lanes) define the replacement target, community baseline, and preview gate.

Dependencies should not be upgraded independently unless the combination is
covered by the controller and OpenStack test suites. Imports from CAPI and CAPO
must use public packages. Internal packages are not an integration contract.

## Testing

The test suite should be organized around the boundaries above.

Pure unit tests cover policy, ownership filters, negative near-matches, ordering, and credential checkpoint transitions.

Gophercloud service tests use HTTP fixtures to cover authentication, endpoint selection, pagination, delete options, response classification, and context cancellation.

Controller tests use envtest with the required CAPI and CAPO CRDs.
They cover finalizers, pause, watch filters, Secret changes, retries, conflicts, and restarts between cleanup phases.

A real OpenStack end-to-end test is required before release.
It must prove both sides of the deletion boundary by deleting owned fixtures and keeping similar but not owned fixtures.

Test count and statement coverage are useful signals, but neither is a substitute for destructive-path tests.

## Review expectations

Pull requests should be small enough for a reviewer to follow the deletion decision. A change that can affect cleanup should explain:

- which Python behavior it preserves or intentionally changes
- which resource types it can affect
- how ownership is established
- what happens after a partial failure or process restart
- which positive and negative tests cover the change
- whether it is safe to deploy before the remaining rewrite work is complete

Unresolved behavior should be marked as deferred and discussed before implementation.
It should not be guessed in code.

## Acceptance criteria for the replacement release

The replacement release is ready when the following criteria are met.
These are release acceptance criteria rather than an implementation progress checklist.

- the compatibility policy and design documents match the shipped behavior
- every ownership rule has positive and negative tests
- resource ownership selectors are no broader than the Python baseline
- any intentional difference from the Python baseline is agreed, documented in the behavior matrix, and tested
- Gophercloud is used for OpenStack API operations
- reconcile contains no blocking sleeps or in-process polling
- finalizer updates are conflict-safe
- cleanup can resume after a process restart
- envtest covers Kubernetes controller behavior
- a real OpenStack test covers both cleanup and non-deletion
- migration from the Python controller, deployment rollback with no active deletion, and audited in-flight handback before `credentialDeleteStarted` have been tested in a representative environment; excluded states use Go forward recovery or break-glass
