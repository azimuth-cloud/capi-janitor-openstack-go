# Go rewrite guidelines

## Purpose

This repository contains the Go replacement for the Python
`cluster-api-janitor-openstack` controller. This document defines what the
replacement preserves and where it deliberately uses safer controller
mechanics.

The goal is a safe operational replacement. The replacement release keeps the
same resource deletion scope. It may fix a failure mode or add an explicitly
approved compatibility feature, but it is not a broader OpenStack cleanup
controller.

Additional cleanup responsibilities require separate design proposals after
the replacement release has proved reliable in a representative environment.

The Python baseline for this work is
[`cluster-api-janitor-openstack`](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/tree/44a89539cc902192cce46b93c7b05e20d127dc12).
The [cleanup behavior matrix](cleanup-behaviour-matrix.md) and
[resource ownership matrix](resource-ownership-matrix.md) record the detailed
compatibility contract. The [CAPO integration boundary](capo-integration-boundary.md)
separates CAPO-owned infrastructure from workload resources.

> [!IMPORTANT]
> The repository already contains a Go implementation.
> These documents define the **target behavior and design boundaries** used to
> review that implementation and future changes.
> They do not imply that every requirement is currently unimplemented.
>
> The [roadmap](../../ROADMAP.md) tracks implementation progress.

## Controller framework

The project retains the existing Kubebuilder v4 scaffold and uses
controller-runtime as the controller framework.

Use lower-level client-go only when controller-runtime cannot reasonably provide
the required functionality.

Evaluating or migrating to another controller framework is outside the scope of
the replacement release.

## What compatibility means

Compatibility does not mean translating each Python function line by line. It
means keeping the same external purpose, deletion scope, configuration, and
successful outcomes while applying the safety corrections in the behavior
matrix.

The replacement release covers:

- watching CAPO `OpenStackCluster` objects
- adding and removing the existing Janitor finalizer
- identifying the cluster name in the same way as the Python controller
- removing the same OCCM floating IPs, service load balancers, and service load
  balancer security groups, subject to the
  [load balancer release decision](cleanup-behaviour-matrix.md#open-replacement-decisions)
- removing the same Cinder snapshots and volumes when volume policy is `delete`
- keeping volumes marked with the existing keep property
- attempting application credential and Secret cleanup under the existing policy

The following are not part of the replacement release:

- networks, subnets, routers, ports, security groups, or bastions managed by CAPO
- CAPO API server load balancers
- `OpenStackMachine` servers
- arbitrary project cleanup
- keypair cleanup
- new naming conventions that the Python controller does not recognize
- additional authentication methods beyond application credential and password
- `ClusterIdentity` support
- new resource types from open or unmerged changes in the Python repository

The Go implementation may correct unsafe failure handling without widening the
set of resources it can delete. Record each intentional difference in the
behavior matrix and cover it with a test.

## When the scope can grow

The cleanup scope remains frozen until the
[replacement release criteria](#acceptance-criteria-for-the-replacement-release)
are complete, the release has run in a representative environment, and no
critical or high-severity cleanup safety issue remains.

Passing those gates does not add new responsibilities. A proposed extension
still needs its own design, ownership rules, negative fixtures, rollout and
rollback plan, and maintainer agreement.

## Design boundaries

The target code has three parts with clear responsibilities.

### Controller

The controller handles Kubernetes concerns:

- reading `OpenStackCluster` and related Kubernetes objects
- honoring pause and watch-filter behavior
- managing the Janitor finalizer
- resolving configuration and policy
- calling one cleanup iteration
- returning a result or error to controller-runtime
- publishing Events, metrics, and structured logs

It must not know OpenStack URL formats, response bodies, or pagination details.

### Cleanup logic

The cleanup package decides:

- which candidates match the Python ownership rules
- which policy applies
- which deletion phase should run next
- whether cleanup is complete, waiting, or blocked

This package uses small domain types and ordinary Go interfaces. It must not
depend on controller-runtime or concrete Gophercloud service clients.

### OpenStack resource services

The OpenStack resource services handle:

- authentication and service client creation
- listing every page of candidate resources
- converting Gophercloud results into the domain types
- making delete requests
- classifying OpenStack errors
- propagating the reconcile context

The services provide facts. They do not decide whether a resource belongs to a
cluster.

## Controller conventions

Reconciliation must be level based and idempotent. The controller must be able
to repeat any step after a restart without deleting a different resource or
incorrectly completing cleanup.

A reconcile should follow this general shape:

1. Read the `OpenStackCluster`
2. Resolve the owning CAPI `Cluster` where it is needed for pause handling
3. Add the legacy Janitor finalizer on a non-deleting object
4. Return without cleanup if deletion has not started
5. Return if no recognized Janitor finalizer is present
6. Resolve the existing Secret-based identity and cleanup policy
7. Run one bounded cleanup iteration
8. Return `RequeueAfter` when OpenStack is still completing an accepted delete
9. Return an error when an operation has failed
10. Remove the finalizer only after required cleanup has been verified

The controller must not call `time.Sleep` or poll in a loop during reconcile.
Expected waiting uses `RequeueAfter`. Failures are returned so that the
controller-runtime workqueue can apply backoff.

Change finalizers and Janitor-owned annotations with a patch rather than a
full-object update.
The controller must not write Janitor conditions into
`OpenStackCluster.status`, because that status is owned by CAPO. Events,
metrics, and logs are sufficient for the replacement release.

## Finalizer compatibility

The Python controller uses `janitor.capi.stackhpc.com`. Existing
`OpenStackCluster` objects may already carry it, so the replacement release must
continue to use and recognize that value.

The replacement migration has three rules:

- do not run the Python and Go controllers against the same objects at the same
  time
- allow the Go controller to adopt objects that already have the legacy
  finalizer
- do not rename the finalizer as part of the rewrite

A qualified replacement finalizer can be considered later. It needs a separate
migration design and must account for objects that are already deleting and for
rollback to an older controller.

## Gophercloud usage

Gophercloud `v2` and Gophercloud `utils` are direct dependencies. Do not
maintain a separate implementation of Keystone authentication, service catalog
parsing, endpoint construction, token refresh, pagination, or service-specific
response models.

The replacement contract accepts the following identity configuration:

- a same namespace Secret referenced by `OpenStackCluster.spec.identityRef`
- a `clouds.yaml` entry selected by `cloudName`
- `v3applicationcredential` or `v3password` authentication
- interface and region values from the selected `clouds.yaml` entry
- the optional CA certificate stored in the Secret

Gophercloud may support more configurations, but the Janitor must validate this
set rather than enabling new behavior accidentally. `ClusterIdentity`, token or
federation authentication, and different identity sources are post-replacement
features.

> [!IMPORTANT]
> CAPO API types are part of the watched contract. CAPO internal packages and
> broad implementation interfaces must not become Janitor dependencies.

## Cleanup progression

The target Go cleanup path uses this order. The load balancer phase follows the
[open release decision](cleanup-behaviour-matrix.md#open-replacement-decisions):

1. floating IPs
2. service load balancers when the replacement policy and ownership checks
   enable cascade deletion; CAPO API server load balancer status alone is not
   ownership proof
3. service load balancer security groups
4. snapshots when volume policy is `delete`
5. volumes when volume policy is `delete`
6. application credential when credential policy permits it
7. credential Secret when credential policy permits it
8. Janitor finalizer

The Go controller may spread this work across several reconciliations. An
accepted OpenStack delete request is not proof that the resource is gone.
Before moving past a dependency or removing the finalizer, the controller must
observe the required resource as absent.

> The important dependency pairs are floating IP before load balancer, load
> balancer before its service security group, and snapshot before volume.

Independent errors may be collected when that is safe, but an incomplete
inventory must never be treated as successful cleanup.

## Credential cleanup

Application credential deletion is the last OpenStack operation because it
removes the controller's ability to inspect the project.

Before attempting it, the controller must persist a **Janitor-owned checkpoint**
showing that all other resources have been verified absent.
This closes the restart window between application credential deletion, Secret
deletion, and finalizer removal.

The checkpoint does not add a new cleanup responsibility. It makes the existing
credential policy safe to implement in a level-based controller.

The Python controller treats a `403` while deleting an application credential
as a warning and continues because a restricted application credential may not
be allowed to delete itself. The replacement contract retains this narrow
exception. It must report that the credential may remain instead of emitting an
unqualified cleanup success.
The exception is specific to application credential self-deletion and must not
be applied to other resource deletion failures.

Missing or invalid credentials before the resources-verified checkpoint must
block finalizer removal.
The Python implementation can remove the finalizer when the Secret is missing,
but carrying that behavior forward could leak resources.
This is recorded as an intentional safety correction rather than a scope change.

## Dependencies

Select Kubernetes, controller-runtime, CAPI, CAPO, Gophercloud, and Go as one
tested dependency train. The
[version strategy](capo-integration-boundary.md#version-strategy) defines the
stable and active compatibility lanes.

Dependencies should not be upgraded independently unless the combination is
covered by the controller and OpenStack test suites. Imports from CAPI and CAPO
must use public packages. Internal packages are not an integration contract.

## Testing

Organize the test suite around the boundaries above.

Pure unit tests must cover policy, ownership filters, negative near-matches,
ordering, and credential checkpoint transitions.

Gophercloud service tests must use HTTP fixtures to cover authentication,
endpoint selection, pagination, delete options, response classification, and
context cancellation.

Controller tests must use envtest with the required CAPI and CAPO CRDs. They
must cover finalizers, pause, watch filters, Secret changes, retries, conflicts,
and restarts between cleanup phases.

A real OpenStack end-to-end test is required before the replacement release.
It must prove both sides of the deletion boundary by deleting owned fixtures
and keeping similar non-owned fixtures.

Test count and statement coverage are useful signals, but neither is a
substitute for destructive-path tests.

## Review expectations

Follow the [destructive change checklist](../../CONTRIBUTING.md#destructive-change-checklist).
Keep each pull request small enough for a reviewer to follow the deletion
decision. Record unresolved behavior in the design or roadmap before writing
code.

## Acceptance criteria for the replacement release

The replacement release is ready when the following criteria are met. These are
acceptance criteria, not an implementation progress checklist.

- the design documents match the shipped behavior
- every ownership rule has positive and negative tests
- resource ownership selectors are no broader than the Python baseline
- any intentional difference from the Python baseline is agreed, documented in
  the behavior matrix, and tested
- Gophercloud is used for OpenStack API operations
- reconcile contains no blocking sleeps or in-process polling
- finalizer updates are conflict-safe
- cleanup can resume after a process restart
- envtest covers Kubernetes controller behavior
- a real OpenStack test covers both cleanup and non-deletion
- migration from the Python controller and rollback have been tested in a
  representative environment

The [roadmap](../../ROADMAP.md#replacement-release-criteria) records the status
and evidence for each criterion.
