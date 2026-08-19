# CAPO integration boundary

## Purpose

This document defines an independent controller that complements Cluster API
Provider OpenStack (CAPO). The Janitor is not part of CAPO and is not endorsed
by CAPO. This document records the external evidence for the role and the
conditions for recommending it to CAPO and OpenStack operators.

Similar cleanup failures have been reported outside Azimuth, but demand and
support commitments are not yet established. The current Go controller does
not meet the recommendation gates in the [roadmap](../../ROADMAP.md).

This review reflects upstream state on 2026-08-18.

## Responsibility split

CAPO and the Janitor act at different ownership layers. The Janitor must not
take over a resource merely because that resource blocks cluster deletion.

| Resource or action | Primary owner | Janitor role |
| --- | --- | --- |
| `OpenStackMachine` servers | CAPO | None |
| CAPO-managed network, subnet, router, router interface, port, and security group | CAPO | None; report a blocker without deleting it |
| CAPO API server load balancer and bastion | CAPO | None |
| OCCM `Service` load balancer | Workload OCCM while the cluster runs | Delete only after the workload controller can no longer do so and the Janitor's ownership rule matches |
| OCCM service floating IP and service security group | Workload OCCM while the cluster runs | Delete only with the exact documented selector |
| Cinder CSI volume and snapshot | Workload CSI while the cluster runs | Apply the cluster and per-volume policies, then use the exact Cinder metadata selector |
| Cluster application credential and Kubernetes Secret | Cluster operator and credential policy | Delete last, after a persisted resources-verified checkpoint |
| Arbitrary project leftovers | Project operator | None |

In CAPO `v0.14.7`, the
[`OpenStackCluster` deletion controller](https://github.com/kubernetes-sigs/cluster-api-provider-openstack/blob/v0.14.7/controllers/openstackcluster_controller.go)
waits for machines and then removes infrastructure that CAPO owns. It does not
reconstruct deleted workload `Service` or PVC objects to clean resources that
OCCM or Cinder CSI created. The Janitor is intended to cover that gap while the
`OpenStackCluster` still provides an identity and a finalizer anchor.

This timing is important. An OCCM load balancer can keep ports attached to the
cluster network and prevent CAPO from deleting that network. Cleanup after the
`OpenStackCluster` disappears is too late; cleanup while workload controllers
are still active can race them.

## Evidence and its limits

The cleanup boundary appears outside this repository:

- CAPO [issue #842](https://github.com/kubernetes-sigs/cluster-api-provider-openstack/issues/842)
  describes CCM load balancers that remain after CCM has stopped and then block
  network deletion. The issue closed through inactivity, not through a merged
  CAPO cleanup implementation. A maintainer also explains the boundary between
  [CAPO resources and workload service or volume resources](https://github.com/kubernetes-sigs/cluster-api-provider-openstack/issues/842#issuecomment-919723016).
- CAPO [PR #990](https://github.com/kubernetes-sigs/cluster-api-provider-openstack/pull/990)
  attempted orphan load balancer cleanup but did not merge.
- CAPO [PR #2629](https://github.com/kubernetes-sigs/cluster-api-provider-openstack/pull/2629)
  records API server load balancer status and permission tradeoffs. It informs
  failure policy; it is not evidence of Janitor adoption.
- Cluster API Provider AWS documents
  [external resource garbage collection](https://cluster-api-aws.sigs.k8s.io/topics/external-resource-gc)
  for resources created by workload cloud controllers. This is a useful
  lifecycle precedent, not a selector design that can be copied to OpenStack.
- The OpenStack governance project
  [`magnum-capi-helm`](https://opendev.org/openstack/magnum-capi-helm/src/commit/3301c01240e606d70d7191a75c89db0c660a5ff9/devstack/plugin.sh)
  installs the Python predecessor in its development environment. Its
  [user documentation](https://opendev.org/openstack/magnum-capi-helm/src/commit/3301c01240e606d70d7191a75c89db0c660a5ff9/doc/source/user_docs/index.rst)
  describes the same cleanup role. This shows integration interest, not
  production use of the Go controller.
- The archived
  [`cluster-api-cleaner-openstack`](https://github.com/giantswarm/cluster-api-cleaner-openstack)
  shows that an independent OpenStack CAPI cleaner existed for load balancers
  and Cinder volumes. Its archived state is not evidence of current use.

These sources justify evaluating the controller with teams that operate
ephemeral or tenant CAPI clusters on OpenStack. They do not establish current
adoption, maintainer support, or safe defaults for every deployment.

## Recommendation contract

Before maintainers recommend the controller beyond Azimuth,
it must meet the [replacement release criteria](go-rewrite-guidelines.md#acceptance-criteria-for-the-replacement-release)
and publish:

- the tested CAPO, CAPI, Kubernetes, OCCM, Cinder CSI, and OpenStack matrix
- migration, rollback, blocked-deletion recovery, and observability guidance
- a clear statement that it is independent of CAPO and is not a project-wide
  sweeper

The [behavior matrix](cleanup-behaviour-matrix.md) and
[ownership matrix](resource-ownership-matrix.md) define the destructive
guarantees. A replacement release may remain pre-v1, but its release notes must
state the support boundary. One Azimuth integration job is not evidence of
general CAPO support.

## Version strategy

The reviewed repository imports CAPO `v0.14.6` and its `v1beta1`
`OpenStackCluster` API. CAPO
[`v0.14.7`](https://github.com/kubernetes-sigs/cluster-api-provider-openstack/releases/tag/v0.14.7)
is the current stable patch release in the same train. Update that lane and test
the dependency versions as a set rather than upgrading Kubernetes modules
independently.

CAPO [`v0.15.0-beta.0`](https://github.com/kubernetes-sigs/cluster-api-provider-openstack/releases/tag/v0.15.0-beta.0)
adds the Cluster API `v1beta2` contract and changes parts of the
`OpenStackCluster` API. Treat this as an active compatibility lane:

1. keep a stable CAPO `v0.14.x` and `v1beta1` test lane
2. add CAPO `v0.15.x` and `v1beta2` compilation, conversion, and envtest coverage
3. document which watched versions can adopt the legacy Janitor finalizer
4. test migration across the API conversion boundary before dropping
   `v1beta1`

`ClusterIdentity` support is a separate feature. Until it is implemented, the
controller must reject that identity type before making an OpenStack request.

## Current OCCM and Cinder considerations

The Python selectors still match important current conventions: OCCM
[load balancer names](https://github.com/kubernetes/cloud-provider-openstack/blob/ff04631653b624a75c811a0836f2ba4a0395d04e/pkg/openstack/loadbalancer.go#L416)
use `kube_service_<cluster>_`, and Cinder resources use the
`cinder.csi.openstack.org/cluster` metadata key. Current OCCM also uses
[reserved tags and shared load balancers](https://github.com/kubernetes/cloud-provider-openstack/blob/master/docs/openstack-cloud-controller-manager/expose-applications-using-loadbalancer-type-service.md).
The legacy name alone does not prove ownership when shared load balancers are
enabled.

Before the replacement release, test the legacy selectors against supported
OCCM versions, satisfy the
[load balancer release decision](cleanup-behaviour-matrix.md#open-replacement-decisions),
and document that the OCCM and Cinder cluster identifier must match the resolved
CAPI cluster name. Using tags to reject conflicting shared ownership is part of
the replacement decision. Using tags to discover additional candidates is a
post-replacement ownership change with explicit rollout and rollback behavior.

The replacement release must not broaden a selector to account for a new
provider option. A narrower compatibility declaration is safer than guessing
ownership.

## Upstream posture

After the replacement criteria are complete, the
[roadmap](../../ROADMAP.md#capo-and-openstack-adoption-track)
tracks publication of boundary results, upstream review, and validation by a
second operator. An upstream discussion requests review; it does not imply
endorsement.

A CAPI Runtime SDK
[`BeforeClusterDelete` hook](https://cluster-api.sigs.k8s.io/tasks/experimental-features/runtime-sdk/implement-lifecycle-hooks)
may be evaluated later as an optional adapter. The API remains experimental and
lifecycle hooks are limited to ClusterClass topologies, so the
`OpenStackCluster` finalizer remains the broadly compatible path.

## Rules for future scope

Future additions follow the
[ownership change rules](resource-ownership-matrix.md#changing-an-ownership-rule).
A new target also needs a named lifecycle owner and evidence that the existing
owner cannot complete cleanup at the required point.

The responsibility split above remains authoritative for future scope. Diagnose
CAPO-owned resource failures in CAPO rather than deleting those resources from
the Janitor.
