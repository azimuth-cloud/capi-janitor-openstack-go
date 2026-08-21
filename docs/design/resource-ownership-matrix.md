# Resource ownership matrix

## Purpose

The Janitor makes destructive API calls, so the rule for selecting a resource is part of its public behavior.
This document records the ownership rules used by the Python controller and fixes them as the boundary for the replacement release.

The Go rewrite must not broaden these selectors.
A stronger ownership model may be proposed after the rewrite is stable, but it needs separate compatibility and rollout work.

The Python baseline is release `0.15.0` at [`capi_janitor/openstack/operator.py`](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/blob/f14d86013d78ac3a4b07f5a2669a8f49590e13ca/capi_janitor/openstack/operator.py).
The [Python compatibility policy](python-compatibility-policy.md) records the baseline and decisions that are not ownership selectors.

## Outer boundary

All discovery and deletion happens through the credential resolved from the `OpenStackCluster` identity reference.

The [compatibility policy](python-compatibility-policy.md#capo-identity-compatibility) defines identity type, Secret resolution, cloud selection, and the separate community profile.
After that resolution:

- the OpenStack project is the project carried by the authenticated token for the selected credential
- the interface and optional region come from the selected `clouds.yaml` entry; `identityRef.region` overrides that region when set
- the cluster name comes from `cluster.x-k8s.io/cluster-name`, falling back to `OpenStackCluster.metadata.name`

An empty resolved cluster name blocks cleanup before discovery.
Project scope and any configured region reduce the candidate inventory, but they do not replace the resource-specific selectors below.

## Ownership rules

| Resource | Policy gate | Python selector | Replacement selector | Delete request |
| --- | --- | --- | --- | --- |
| Neutron floating IP | Always considered during deletion | `description` starts with `Floating IP for Kubernetes external service` and ends with `from cluster <cluster-name>` | Keep the exact checks, then protect a FIP attached to a shared LB VIP port | Delete by floating IP ID after the LB/FIP preflight |
| Octavia service load balancer | The released code checks CAPO API server LB state | `name` starts with `kube_service_<cluster-name>_` | Remove the CAPO status gate, keep the exact prefix, and veto a foreign or malformed reserved tag | Cascade delete by load balancer ID after preflight |
| Neutron service security group | Always considered during deletion | `description` starts with `Security Group for` and ends with `Service LoadBalancer in cluster <cluster-name>` | Exact same start and end checks | Delete by security group ID |
| Cinder snapshot | Resolved volume policy is `delete` | Metadata key `cinder.csi.openstack.org/cluster` exactly equals the cluster name | Exact same metadata check | Delete by snapshot ID |
| Cinder volume | Resolved volume policy is `delete` | Exact cluster metadata matches and the keep property is not exactly `true` | Exact same metadata and keep checks | Delete by volume ID |
| Keystone application credential | Direct Secret policy is `delete` and the Janitor finalizer is the only finalizer | ID is read from the hard-coded `openstack` entry | Exact ID from the selected `v3applicationcredential` cloud entry | Delete for the authenticated user by application credential ID |
| Kubernetes credential Secret | Same gate as application credential cleanup | Exact referenced Secret in the `OpenStackCluster` namespace | Only the exact direct application-credential Secret is owned; password and community identity Secrets are not | Delete the recorded Secret with a UID precondition after the qualifying checkpoint outcome |

The candidate selectors and the verification selectors must be the same.
A controller must not request deletion using one rule and later declare success using a weaker rule.

## Resources that are not owned by the Janitor

The replacement release must not select any of the following:

- CAPO API server load balancers
- CAPO networks, subnets, routers, router interfaces, ports, or security groups
- CAPO bastions
- servers represented by `OpenStackMachine`
- Cinder resources without the exact cluster metadata
- volumes carrying the exact keep value `true`
- load balancers that do not use the Python `kube_service_<cluster>_` prefix
- floating IPs or security groups with only a partial description match
- a shared load balancer carrying a foreign or malformed reserved `kube_service_` tag, and the floating IP attached to its VIP port
- resources from another OpenStack project or, when configured, region
- password identity Secrets and their external Keystone users
- `OpenStackClusterIdentity` objects, their backing Secrets, and the credentials stored in them
- SSH keypairs
- general CI leftovers

The Janitor and any project-wide CI cleanup job have different responsibilities.
Rules from one must not be copied into the other without a separate design decision.

## Selector details and fixtures

The examples below use `demo` as the resolved cluster name.
Each positive fixture passes its resource selector; a separate policy gate may still block deletion.
Each negative fixture should remain untouched.

### Floating IP

Positive:

```text
Floating IP for Kubernetes external service default/web from cluster demo
```

Negative:

```text
Floating IP for Kubernetes external service default/web from cluster demo-2
Floating IP for Kubernetes API from cluster demo
Floating IP for Kubernetes external service default/web for cluster demo
```

The Python selector allows text between the fixed start and end.
The Go selector should preserve that behavior and should not replace it with a general substring search.

### Service load balancer

Positive:

```text
kube_service_demo_default_web
```

Negative:

```text
kube_service_demo2_default_web
kube_service_demo
demo-kube-service-default-web
demo-control-plane
```

The positive fixture passes the legacy name selector independently of CAPO API server load balancer state.
A CAPO API server load balancer is not selected merely because CAPO status contains its ID.

After the name matches, tags act only as a deletion veto.
They do not discover an LB whose name failed the selector.
A target tag must have the complete OCCM form `kube_service_<cluster>_<namespace>_<service>` with non-empty namespace and Service components; a reserved tag that cannot be classified is protective.

Allowed tags for the target cluster:

```text
kube_service_demo_default_web
kube_service_demo_monitoring_ingress
environment=production
```

Any of the following reserved tags protects the whole LB:

```text
kube_service_demo-2_default_web
kube_service_
kube_service_malformed
kube_service_demo_
```

A successfully read empty tag list keeps legacy name-only behavior.
Failure to read every load balancer page and its tag data is not the same as an empty list and blocks mutation.

### Shared load balancer floating IP

A shared LB exposes one VIP and floating IP to every attached Service.
The FIP description can retain the cluster that first created it.
A matching Python FIP description is therefore insufficient when that FIP is attached to a protected shared LB.

Before deleting any FIP or LB, the controller must complete this read-only preflight:

```text
complete LB and tag inventory
  -> identify protected shared LBs
  -> collect their VIP port IDs
  -> complete FIP inventory and map FIP port IDs
  -> remove protected LB and FIP IDs from owned candidates
  -> begin the normal FIP, LB, and SG phases
```

If a protected LB lacks its VIP port ID, a FIP association cannot be extracted, or either inventory is incomplete, the controller makes no mutation.
A successfully parsed JSON null or empty-string FIP `port_id` means that the FIP is unbound; it is not an extraction failure.
A missing field or type mismatch is unknown and blocks.
The cleanup domain facts therefore carry LB tags, LB VIP port ID, and a tri-state FIP association: unknown, unbound, or bound to an exact port ID.

The [Octavia capability policy](python-compatibility-policy.md#octavia-capability-and-error-policy) defines when missing Octavia inventory is not applicable and when it blocks this preflight.

The tag and VIP rules follow the [OCCM shared-load-balancer contract](https://github.com/kubernetes/cloud-provider-openstack/blob/openstack-cloud-controller-manager-2.36.1/docs/openstack-cloud-controller-manager/expose-applications-using-loadbalancer-type-service.md#sharing-load-balancer-with-multiple-services).

OCCM service security groups are Service-specific rather than shared with the whole LB.
A target-cluster SG that passes the existing exact description rule remains eligible even when the shared LB and VIP FIP are preserved.

### Service security group

Positive:

```text
Security Group for Service LoadBalancer in cluster demo
```

Negative:

```text
Security Group for Service LoadBalancer in cluster demo-2
Security Group for worker nodes in cluster demo
Group for Service LoadBalancer in cluster demo
Security Group for Service LoadBalancer for cluster demo
```

As with floating IPs, the Python selector checks a fixed start and end. The Go rewrite should keep those checks exactly.

### Snapshot

Positive metadata:

```yaml
cinder.csi.openstack.org/cluster: demo
```

Negative metadata:

```yaml
cinder.csi.openstack.org/cluster: demo-2
```

Missing metadata is also negative.
Snapshot selection has no independent keep property in the Python implementation.
A keep property on a related volume does not remove a matching snapshot from the candidate set.

### Volume

Positive metadata:

```yaml
cinder.csi.openstack.org/cluster: demo
```

Negative because it belongs to another cluster:

```yaml
cinder.csi.openstack.org/cluster: demo-2
```

Negative because it is explicitly kept:

```yaml
cinder.csi.openstack.org/cluster: demo
janitor.capi.azimuth-cloud.com/keep: "true"
```

The keep comparison is case-sensitive.
Values such as `True`, `1`, or `yes` do not have the same meaning in the Python implementation.
That behavior is kept for parity and should be documented for users.

### Application credential

The candidate is not found by name or by listing all user credentials.
The Go controller uses the exact `application_credential_id` from the selected `clouds.yaml` entry.

The Python code reads the hard-coded `openstack` entry at this point, even if a different cloud was selected earlier.
Using the selected entry is an intentional correctness fix, not a wider ownership rule.

Positive conditions:

- the identity source is a direct Secret
- the selected cloud uses `v3applicationcredential`
- the Secret annotation `janitor.capi.stackhpc.com/credential-policy` is `delete`
- the Janitor finalizer is the only remaining finalizer
- all other required resources have been verified absent

Any missing condition means that the credential and Secret are not deleted in that reconcile.

A password-authenticated cloud has no application credential candidate.
It uses the same project and resource selectors, but it must not create the Keystone application-credential deletion service or interpret an empty application credential ID as permission to delete the Secret.
The [password policy](python-compatibility-policy.md#approved-password-extension) and [community identity policy](python-compatibility-policy.md#capo-identity-compatibility) define their lifecycles; neither identity profile creates a credential or Secret deletion candidate.

## Discovery and deletion rules

Every resource service must list all pages before cleanup can be declared complete.
A pagination or extraction error makes inventory incomplete and must retain the finalizer.
For FIP and LB cleanup, the cross-service preflight above must complete before the first mutation.

Selection should be implemented as pure functions over small domain types.
Deletion code should receive already-authorized IDs.
This keeps API transport details separate from ownership decisions and makes negative fixture tests easy to review.

Delete requests are idempotent.
A resource that was selected in an earlier reconcile and is now absent can be treated as complete.
A `NotFound` response must not be used to authorize a resource that never passed the selector.

## Required test shape

Each selector needs:

- one straightforward positive fixture
- a positive fixture found on a later pagination page
- a different-cluster negative fixture
- a partial-match negative fixture
- an empty or missing-field negative fixture
- a candidate in an unselected project or region at the service boundary

Load balancer coverage additionally needs:

- target-cluster tags that allow deletion
- a foreign reserved tag that preserves both the LB and its VIP FIP
- an empty tag list that uses the legacy name selector
- malformed reserved tags
- missing protected-LB VIP facts, FIP association extraction errors, and later-page failures that prevent all mutation

The composed OpenStack end-to-end test should create at least one owned resource and one similar non-owned resource.
The test passes only when the owned resource is removed and the non-owned resource remains.

## Changing an ownership rule

Ownership rules are frozen for the replacement release.
A later change needs:

- a written reason for changing the selector
- examples of resources newly included and newly excluded
- evidence that the new selector identifies ownership reliably
- positive and negative fixtures
- an upgrade and rollback assessment
- maintainer agreement before implementation

This process applies even when a broader selector appears to fix an obvious leftover.
The rewrite should first establish a dependable replacement for the Python controller, then consider additional cleanup coverage.
