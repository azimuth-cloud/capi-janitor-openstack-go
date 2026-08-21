# Go rewrite guidelines


## Why this document exists

`cluster-api-janitor-openstack` is being rewritten in Go in a separate repository. 
Before more implementation work is merged, we need a shared view of what the rewrite is expected to preserve and which parts of the implementation are expected to change.

The first goal is a safe replacement for the existing Python controller. 
It is not a broader OpenStack cleanup controller. The initial Go release will cover the same cleanup responsibilities as the Python implementation and no more.

Once the Go controller has been released, deployed, and shown to behave reliably, we can discuss additional cleanup responsibilities in separate design proposals.

The Python baseline for this work is
[`cluster-api-janitor-openstack` `0.15.0`](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/tree/f14d86013d78ac3a4b07f5a2669a8f49590e13ca).
The companion [cleanup behaviour matrix](cleanup-behaviour-matrix.md) and [resource ownership matrix](resource-ownership-matrix.md) record the details
that need to be carried across.

> [!IMPORTANT]
> The repository already contains an initial Go implementation. 
> These documents define the **target behaviour and design boundaries** used to review that implementation and future changes. 
> They do not imply that every requirement is currently unimplemented.
>
> Implementation progress should be tracked separately from these design documents.


## Controller framework

The project will retrain the existing Kubebuilder v4 scaffold and use controller-runtime as the controller framework. 

Direct use of lower level client-go should be limited to cases where the required functionality is not reasonably available through controller-runtime.

Evaluating or migrating to another controller framework is outside the scope of the initial rewrite.

## What feature parity means

Feature parity does not mean translating each Python function line by line. 
It means keeping the same external purpose, cleanup scope, configuration, and successful outcomes.

For the first release, parity covers:

- watching CAPO `OpenStackCluster` objects
- adding and removing the existing Janitor finalizer
- identifying the cluster name in the same way as the Python controller, with unique workload cluster names in each OpenStack project
- removing the same OCCM floating IPs, service load balancers, and service load balancer security groups
- removing the same Cinder snapshots and volumes when volume policy is `delete`
- keeping volumes marked with the existing keep property
- attempting application credential and Secret cleanup under the existing policy

The following are not part of initial parity:

- managed by CAPO: networks, subnets, routers, ports, security groups, or bastions
- CAPO API server load balancers
- `OpenStackMachine` servers
- arbitrary project cleanup
- keypair cleanup
- new naming conventions that the Python controller does not recognise
- additional authentication methods
- other identity sources
- new resource types from open or unmerged changes in the Python repository

The Go implementation may correct unsafe failure handling without widening the set of resources it can delete. 
Each intentional difference from the Python controller must be recorded in the behaviour matrix and covered by a test.

## When the scope can grow

The cleanup scope remains frozen until all of the following are true:

- the behaviour and ownership matrices are implemented as tests
- the manual OpenStack client has been replaced by Gophercloud
- controller behaviour is covered by envtest with CAPI and CAPO CRDs
- destructive behaviour is covered by a real OpenStack end-to-end test
- migration from the Python controller and rollback to it have both been tested
- at least one Go release has been deployed in a representative environment
- there are no unresolved critical or high-severity cleanup safety issues

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

Reconciliation must be level-based and idempotent. The controller should be able to repeat any step after a restart without deleting a different resource or incorrectly completing cleanup.

A reconcile should follow this general shape:

1. Read the `OpenStackCluster`
2. Resolve the owning CAPI `Cluster` where it is needed for pause handling
3. Add the legacy Janitor finalizer on a non-deleting object
4. Return without cleanup if deletion has not started
5. Return if no recognised Janitor finalizer is present
6. Resolve the existing Secret-based identity and cleanup policy
7. Reject unsupported identity or authentication input before any cloud request
8. Run one bounded cleanup iteration
9. Return `RequeueAfter` when OpenStack is still completing an accepted delete
10. Return an error when an operation has failed
11. Remove the finalizer only after required cleanup has been verified

The controller must not call `time.Sleep` or poll in a loop during reconcile.
Expected waiting uses `RequeueAfter`. Failures are returned so that the controller-runtime workqueue can apply backoff.

Finalizers and Janitor-owned annotations should be changed with a patch rather than a full-object update. 
The controller must not write Janitor conditions into `OpenStackCluster.status`, because that status is owned by CAPO. Events, metrics, and logs are sufficient for the initial release.

## Finalizer compatibility

The Python controller uses `janitor.capi.stackhpc.com`. Existing `OpenStackCluster` objects may already carry it, so the first Go release must continue to use and recognise that value.

The initial migration has three rules:

- do not run the Python and Go controllers against the same objects at the same time
- allow the Go controller to adopt objects that already have the legacy finalizer
- do not rename the finalizer as part of the rewrite

A qualified replacement finalizer can be considered later. 
It needs a separate migration design and must account for objects that are already deleting and for rollback to an older controller.

## Gophercloud usage

Gophercloud `v2` and Gophercloud `utils` should be direct dependencies. 
The project should not maintain its own implementation of Keystone authentication, service catalog parsing, endpoint construction, token refresh, pagination, or
service-specific response models.

The initial OpenStack client should deliberately retain the Python controller's supported identity surface:

- a same namespace Secret referenced by `OpenStackCluster.spec.identityRef`
- a `clouds.yaml` entry selected by `cloudName`
- `v3applicationcredential` authentication
- interface and region values from the selected `clouds.yaml` entry
- the optional CA certificate stored in the Secret

Gophercloud may support more configurations, but the Janitor should validate
the initial supported set rather than enabling new behaviour accidentally.
Other identity sources and authentication methods require separate compatibility work.

> [!IMPORTANT]
>CAPO API types are part of the watched contract, but CAPO internal packages and broad implementation interfaces should not become dependencies of the Janitor.

## Cleanup progression

The successful Python baseline path establishes the initial order:

1. complete the shared-LB and floating-IP ownership preflight
2. floating IPs
3. service load balancers, independently of CAPO API server LB state
4. service load balancer security groups
5. snapshots when volume policy is `delete`
6. volumes when volume policy is `delete`
7. application credential when credential policy permits it
8. credential Secret when credential policy permits it
9. Janitor finalizer

The Go controller may spread this work across several reconciliations. An accepted OpenStack delete request is not proof that the resource is gone.
Before moving past a dependency or removing the finalizer, the controller must observe the required resource as absent.

> The important dependency pairs are floating IP before load balancer, loadbalancer before its service security group, and snapshot before volume.

Independent errors may be collected when that is safe, but an incomplete inventory must never be treated as successful cleanup.
The LB preflight uses complete OCCM reserved ownership tags and VIP/FIP association facts.
Pool member names and ports are not ownership evidence.

## Credential cleanup

Application credential deletion is the last OpenStack operation because it removes the controller's ability to inspect the project.
Any earlier cleanup or inventory failure retains the credential, Secret, and finalizer so the controller can retry.

Before attempting it, the controller should persist a **Janitor owned checkpoint** showing that all other resources have been verified absent. 
The checkpoint records the restart state and prevents unsafe Secret or finalizer removal after a process failure.

The checkpoint does not add a new cleanup responsibility. It makes the existing credential policy safe to implement in a level-based controller.

The Go controller does not copy the Python behavior that continues after application credential self-deletion returns `403`.
Only exact credential DELETE `204` or exact bound-credential `404` authorizes deletion of the recorded Secret.
A `401`, `403`, transport failure, or unclassified response retains the Secret and finalizer and does not advance the checkpoint.

Missing or invalid credentials before `credentialDeleteStarted` must block finalizer removal.
The Python implementation can remove the finalizer when the Secret is missing, but carrying that behaviour forward could leak resources. 
This is recorded as an intentional safety correction rather than a scope change.

## Dependencies

Kubernetes, controller-runtime, CAPI, CAPO, Gophercloud, and the Go version should be selected as one tested dependency train. 
The project should start with a stable CAPO release and align the related versions with that release's `go.mod`.

Dependencies should not be upgraded independently unless the combination is
covered by the controller and OpenStack test suites. Imports from CAPI and CAPO
must use public packages. Internal packages are not an integration contract.

## Testing

The test suite should be organised around the boundaries above.

Pure unit tests cover policy, ownership filters, negative near-matches, ordering, and credential checkpoint transitions.

Gophercloud service tests use HTTP fixtures to cover authentication, endpoint selection, pagination, delete options, response classification, and context cancellation.

Controller tests use envtest with the required CAPI and CAPO CRDs. 
They cover finalizers, pause, Secret changes, retries, conflicts, and restarts between cleanup phases.

A real OpenStack end-to-end test is required before release. 
It must prove both sides of the deletion boundary by deleting owned fixtures and keeping similar but not owned fixtures.

Test count and statement coverage are useful signals, but neither is a substitute for destructive-path tests.

## Review expectations

Pull requests should be small enough for a reviewer to follow the deletion decision. A change that can affect cleanup should explain:

- which Python behaviour it preserves or intentionally changes
- which resource types it can affect
- how ownership is established
- what happens after a partial failure or process restart
- which positive and negative tests cover the change
- whether it is safe to deploy before the remaining rewrite work is complete

Unresolved behaviour should be marked as deferred and discussed before implementation. It should not be guessed in code.

## Acceptance criteria for the initial release

The initial Go release is ready when the following criteria are met. 
These are release acceptance criteria rather than an implementation progress checklist.

- the linked design documents match the shipped behaviour
- every ownership rule has positive and negative tests
- resource ownership selectors are no broader than the Python baseline
- any intentional difference from the Python baseline is agreed, documented in the behaviour matrix, and tested
- Gophercloud is used for OpenStack API operations
- reconcile contains no blocking sleeps or in-process polling
- finalizer updates are conflict-safe
- cleanup can resume after a process restart
- envtest covers Kubernetes controller behaviour
- a real OpenStack test covers both cleanup and non-deletion
- migration from the Python controller and rollback have been tested in a representative environment
