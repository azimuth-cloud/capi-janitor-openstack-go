# Go rewrite guidelines

## Why this document exists

`cluster-api-janitor-openstack` is being rewritten in Go in a separate repository.
This document defines the behavior the rewrite preserves and the implementation boundaries it changes.

The first goal is a safe replacement for the existing Python controller, not a broader OpenStack cleanup controller.
Additional cleanup responsibilities require separate design proposals after the replacement has been released and validated in a representative environment.

The Python baseline is release `0.15.0` at [`f14d860`](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/tree/f14d86013d78ac3a4b07f5a2669a8f49590e13ca).
The [Python compatibility policy](python-compatibility-policy.md) records the replacement decisions.
The [cleanup behavior matrix](cleanup-behaviour-matrix.md) records observable outcomes and required evidence.
The [resource ownership matrix](resource-ownership-matrix.md) records destructive selectors and preservation rules.
The [regression ledger](python-regression-ledger.md) connects known defects to tests or tracked work.

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
- identifying the cluster name in the same way as the Python controller, with unique workload cluster names in each OpenStack project
- removing the same OCCM floating IPs, service load balancers, and service load balancer security groups
- removing the same Cinder snapshots and volumes when volume policy is `delete`
- keeping volumes marked with the existing keep property
- attempting application credential and Secret cleanup under the existing policy

The following are not part of the replacement release:

- networks, subnets, routers, ports, security groups, and bastions managed by CAPO
- CAPO API server load balancers
- `OpenStackMachine` servers
- arbitrary project cleanup
- keypair cleanup
- new naming conventions that the Python controller does not recognize
- additional authentication methods
- other identity sources
- new resource types from open or unmerged changes in the Python repository

The Go implementation may correct unsafe failure handling and behavior defects approved in the compatibility policy.
Resource types and name or metadata selectors do not expand in the replacement release.
The [compatibility policy](python-compatibility-policy.md#baseline-and-purpose) defines the initial application credential scope and deferred work.

## Public policy surface

Only the volume policy and explicit deletion of a direct application credential and Secret are user choices.
All other decisions are fixed safety rules defined by the [compatibility policy](python-compatibility-policy.md#baseline-and-purpose).
The replacement adds no cleanup target, load balancer skip setting, option to remove the finalizer before cleanup is verified, or Janitor CRD.

## When the scope can grow

The cleanup scope remains frozen until all of the following are true:

- the behavior and ownership matrices are implemented as tests
- the manual OpenStack client has been replaced by Gophercloud
- controller behavior is covered by envtest with CAPI and CAPO CRDs
- destructive behavior is covered by a full workflow test on real OpenStack
- migration from the Python controller and deployment rollback with no active deletion have been tested
- at least one Go release has been deployed in a representative environment
- there are no unresolved cleanup safety issues with critical or high severity

Reaching this point does not automatically add new responsibilities.
A proposed extension still needs its own design document, ownership rules, negative test fixtures, rollout plan, and maintainer agreement.

## Design boundaries

The code should be split into three parts with clear responsibilities.

### Controller

The controller handles Kubernetes concerns:

- reading `OpenStackCluster` and related Kubernetes objects
- honoring pause behavior
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

Reconciliation must follow observed state and remain idempotent.
The controller should be able to repeat any step after a restart without deleting a different resource or incorrectly completing cleanup.

A reconcile should follow this general shape:

1. Read the `OpenStackCluster`
2. Resolve the owning CAPI `Cluster` where it is needed for pause handling
3. Return without mutation when the `Cluster` or `OpenStackCluster` is paused
4. Add the legacy Janitor finalizer on an object that is not deleting
5. Return without cleanup if deletion has not started
6. Return if no recognized Janitor finalizer is present
7. Validate any recorded credential cleanup state and resolve the direct Secret when it is required
8. Reject unsupported identity or authentication input before any cloud request
9. Run one bounded resource cleanup iteration or one recorded credential transition
10. Return `RequeueAfter` when OpenStack is still completing an accepted delete
11. Return an error when an operation has failed
12. Remove the finalizer only after required cleanup has been verified

Reconcile must never sleep, poll in a loop, or write retry annotations.
Expected waits return `RequeueAfter`.
Failures return errors so the workqueue can apply backoff.

Finalizers and annotations owned by Janitor should be changed with a patch rather than a full object update.
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
The project should not maintain its own implementation of Keystone authentication, service catalog parsing, endpoint construction, token refresh, pagination, or response models for individual services.

The replacement OpenStack client keeps the Python identity surface:

- a Secret in the same namespace referenced by `OpenStackCluster.spec.identityRef`
- a `clouds.yaml` entry selected by `cloudName`
- explicit `v3applicationcredential` authentication
- the identity endpoint path, including a deployment prefix such as `/identity`
- interface and optional region values from the selected `clouds.yaml` entry
- both Cinder catalog names used by Python, `volumev3` and `block-storage`
- the optional CA certificate stored in the Secret
- a request timeout of 60 seconds, matching the Python controller

Gophercloud may support more configurations, but the Janitor should validate
the replacement scope rather than enabling new behavior accidentally.
Other identity sources and authentication methods require separate compatibility work.

> [!IMPORTANT]
> CAPO API types are part of the watched contract, but CAPO internal packages and broad implementation interfaces should not become dependencies of the Janitor.

## Cleanup progression

Before its first mutation, the runner completes the load balancer and floating IP safety preflight defined by the [ownership matrix](resource-ownership-matrix.md#shared-load-balancer-floating-ip).
Incomplete inventory or association facts block mutation, and CAPO API server load balancer status is never a cleanup gate.

After the preflight, the successful dependency order is:

1. floating IPs that are not attached to a protected shared LB
2. service load balancers that pass the legacy name selector and reserved tag check
3. service load balancer security groups
4. snapshots when volume policy is `delete`
5. volumes when volume policy is `delete`
6. direct application credential when its Secret policy permits deletion
7. direct credential Secret when that policy and confirmed credential outcome permit it
8. Janitor finalizer

The Go controller may spread this work across several reconciliations. An accepted OpenStack delete request is not proof that the resource is gone.
Before moving past a dependency or removing the finalizer, the controller must observe the required resource as absent.

> The important dependency pairs are floating IP before load balancer, load balancer before its service security group, and snapshot before volume.

Independent errors may be collected when that is safe, but an incomplete inventory must never be treated as successful cleanup.
The load balancer preflight uses complete OCCM reserved ownership tags and associations between VIP ports and floating IPs.

## Credential cleanup

Application credential deletion is the last OpenStack operation because it removes the controller's ability to inspect the project.
Any earlier cleanup or inventory failure retains the credential, Secret, and finalizer so the controller can retry.

Before attempting it, the controller completes a fresh inventory and verifies that no owned resource remains.
It then records `credentialDeleteStarted` for the exact credential and returns.

Only a `204` or `404` response from deleting that recorded credential permits the controller to record `secretDeleteStarted`.
A `401`, `403`, timeout, or unclassified response retains the Secret and finalizer.

After recording `secretDeleteStarted`, the controller deletes the recorded Secret with a UID precondition.
It removes the Janitor finalizer only after that deletion is complete.

Missing or invalid credentials before `credentialDeleteStarted` must block finalizer removal.
The Python implementation can remove the finalizer when the Secret is missing, but carrying that behavior forward could leak resources.
This changes unsafe failure handling without adding a cleanup responsibility.

The [compatibility policy](python-compatibility-policy.md#application-credential-cleanup) defines the checkpoint binding and exact response handling.

## Dependencies

Kubernetes, controller-runtime, CAPI, CAPO, Gophercloud, and the Go version should be selected as one tested dependency train.
The first replacement release validates one stable CAPO `v0.14.x` fixture with `OpenStackCluster` `v1beta1` and a direct Secret.
The tested versions are release evidence, not a permanent compatibility boundary.

Dependencies should not be upgraded independently unless the combination is
covered by the controller and OpenStack test suites. Imports from CAPI and CAPO
must use public packages. Internal packages are not an integration contract.

## Testing

The test suite should be organized around the boundaries above.

Pure unit tests cover policy, ownership filters, cases that must not match, ordering, and credential checkpoint transitions.

Gophercloud service tests use HTTP fixtures to cover authentication, endpoint selection, pagination, delete options, response classification, and context cancellation.

Controller tests use envtest with the required CAPI and CAPO CRDs. 
They cover finalizers, pause, Secret changes, retries, conflicts, and restarts between cleanup phases.

A full workflow test on real OpenStack is required before release.
It must prove both sides of the deletion boundary by deleting owned fixtures and keeping similar but not owned fixtures.

Test count and statement coverage are useful signals, but neither is a substitute for destructive cleanup tests.

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
- reconcile contains no blocking sleeps or polling loops
- finalizer patches preserve concurrent changes
- cleanup can resume after a process restart
- envtest covers Kubernetes controller behavior
- a real OpenStack test covers both cleanup and preservation
- migration from the Python controller and deployment rollback with no active deletion have been tested in a representative environment
