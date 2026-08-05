# Resource ownership matrix


## Purpose

The Janitor makes destructive API calls, so the rule for selecting a resource is part of its public behaviour. 
This document records the ownership rules used by the Python controller and fixes them as the boundary for the first Go release.

The Go rewrite must not broaden these selectors. 
A stronger ownership model may be proposed after the rewrite is stable, but it needs separate compatibility and rollout work.

The Python baseline is
[`capi_janitor/openstack/operator.py`](https://github.com/azimuth-cloud/cluster-api-janitor-openstack/blob/44a89539cc902192cce46b93c7b05e20d127dc12/capi_janitor/openstack/operator.py).

## Outer boundary

All discovery and deletion happens through the credential selected from the `OpenStackCluster` identity Secret.

For initial parity:

- the Secret is in the same namespace as the `OpenStackCluster`
- the cloud is selected by `identityRef.cloudName`, with `openstack` as the default
- the OpenStack project is the project scoped by the selected credential and authenticated token
- the interface and region come from the selected `clouds.yaml` entry
- the cluster name comes from `cluster.x-k8s.io/cluster-name`, falling back to `OpenStackCluster.metadata.name`

Project and region scoping reduce the candidate inventory, but they do not replace the resource specific selectors below.

## Ownership rules

| Resource | Policy gate | Python selector | Initial Go selector | Delete request |
| --- | --- | --- | --- | --- |
| Neutron floating IP | Always considered during deletion | `description` starts with `Floating IP for Kubernetes external service` and ends with `from cluster <cluster-name>` | Exact same start and end checks | Delete by floating IP ID |
| Octavia service load balancer | Considered only when CAPO API server load balancer is enabled and `status.apiServerLoadBalancer.id` is not empty | `name` starts with `kube_service_<cluster-name>_` | Exact same prefix check | Cascade delete by load balancer ID |
| Neutron service security group | Always considered during deletion | `description` starts with `Security Group for` and ends with `Service LoadBalancer in cluster <cluster-name>` | Exact same start and end checks | Delete by security group ID |
| Cinder snapshot | Considered only when resolved volume policy is `delete` | Metadata key `cinder.csi.openstack.org/cluster` exactly equals the cluster name | Exact same metadata check | Delete by snapshot ID |
| Cinder volume | Considered only when resolved volume policy is `delete` | Metadata key `cinder.csi.openstack.org/cluster` exactly equals the cluster name and keep property is not exactly `true` | Exact same metadata and keep checks | Delete by volume ID |
| Keystone application credential | Considered only when Secret credential policy is `delete` and the Janitor finalizer is the only finalizer | Application credential ID read from the fixed `openstack` entry | Exact ID from the selected cloud entry. This corrects the cloud selection bug without broadening the resource type | Delete for the authenticated user by application credential ID |
| Kubernetes credential Secret | Same gate as application credential cleanup | Exact Secret referenced by `spec.identityRef.name` in the `OpenStackCluster` namespace | Exact same object reference | Delete by namespace and name |

Clouds that use password authentication have no application credential ID. They
use the same project resource ownership rules, but the Keystone application
credential deletion row is not applicable.

The candidate selectors and the verification selectors must be the same. 
A controller must not request deletion using one rule and later declare success using a weaker rule.

## Resources that are not owned by the Janitor

The first Go release must not select any of the following:

- CAPO API server load balancers
- CAPO networks, subnets, routers, router interfaces, ports, or security groups
- CAPO bastions
- servers represented by `OpenStackMachine`
- Cinder resources without the exact cluster metadata
- volumes carrying the exact keep value `true`
- load balancers that do not use the Python `kube_service_<cluster>_` prefix
- floating IPs or security groups with only a partial description match
- resources from another OpenStack project or selected region
- SSH keypairs
- general CI leftovers

The Janitor and any project-wide CI cleanup job have different responsibilities.
Rules from one must not be copied into the other without a separate design decision.

## Selector details and fixtures

The examples below use `demo` as the resolved cluster name. Each positive fixture should result in a deletion candidate. Each negative fixture should remain untouched.

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
The Go selector should preserve that behaviour and should not replace it with a general substring search.

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

The positive fixture is a candidate only when the existing load balancer gate is true. 
A CAPO API server load balancer is not selected merely because CAPO status contains its ID.

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

Missing metadata is also negative. Snapshot selection has no independent keep property in the Python implementation. 
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

The keep comparison is case-sensitive. Values such as `True`, `1`, or `yes` do not have the same meaning in the Python implementation. 
That behaviour is kept for parity and should be documented for users.

### Application credential

The candidate is not found by name or by listing all user credentials. 
The Go controller uses the exact `application_credential_id` from the selected `clouds.yaml` entry. 

The Python code reads the hard-coded `openstack` entry at this point, even if a different cloud was selected earlier. 
Using the selected entry is an intentional correctness fix, not a wider ownership rule.

Positive conditions:

- the Secret annotation `janitor.capi.stackhpc.com/credential-policy` is `delete`
- the Janitor finalizer is the only remaining finalizer
- all other required resources have been verified absent

Any missing condition means that the credential and Secret are not deleted in that reconcile.

## Discovery and deletion rules

Every resource service must list all pages before cleanup can be declared complete.
A pagination or extraction error makes inventory incomplete and must retain the finalizer.

Selection should be implemented as pure functions over small domain types.
Deletion code should receive already-authorised IDs. This keeps API transport details separate from ownership decisions and makes negative fixture tests easy to review.

Delete requests are idempotent. A resource that was selected in an earlier reconcile and is now absent can be treated as complete. 
A `NotFound` response must not be used to authorise a resource that never passed the selector.

## Required test shape

Each selector needs:

- one straightforward positive fixture
- a positive fixture found on a later pagination page
- a different-cluster negative fixture
- a partial-match negative fixture
- an empty or missing-field negative fixture
- a candidate in an unselected project or region at the service boundary

The composed OpenStack end-to-end test should create at least one owned resource and one similar non owned resource. 
The test passes only when the owned resource is removed and the non owned resource remains.

## Changing an ownership rule

Ownership rules are frozen for the parity release. A later change needs:

- a written reason for changing the selector
- examples of resources newly included and newly excluded
- evidence that the new selector identifies ownership reliably
- positive and negative fixtures
- an upgrade and rollback assessment
- maintainer agreement before implementation

This process applies even when a broader selector appears to fix an obvious leftover. 
The rewrite should first establish a dependable replacement for the Python controller, then consider additional cleanup coverage.
