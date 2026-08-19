# Cluster API Janitor for OpenStack agent guide

## Project purpose

This repository contains the Go replacement for the Python
`cluster-api-janitor-openstack` controller. It removes workload OpenStack
resources that can block deletion of a CAPO cluster. It is not a general project
cleaner and must not take ownership of CAPO-managed infrastructure.

The replacement release keeps the legacy finalizer and the Python resource
ownership boundary. It may correct unsafe failure handling and add an approved
compatibility feature, but it must not broaden a deletion selector without a
separate design review.

## Start with the design

Read these files before changing cleanup behavior:

- `docs/design/go-rewrite-guidelines.md` defines the target controller design
- `docs/design/cleanup-behaviour-matrix.md` records compatibility decisions
- `docs/design/resource-ownership-matrix.md` defines destructive selectors
- `docs/design/capo-integration-boundary.md` separates CAPO and Janitor ownership
- `ROADMAP.md` records current implementation status and release blockers

The design documents are normative. The roadmap is the progress report. If code
and design differ, report the difference instead of silently treating current
code as the intended behavior.

## Repository layout

```text
cmd/main.go                     manager entry point
internal/controller/            Kubernetes reconciliation and policy resolution
internal/cleanup/               target domain types, ports, and cleanup runner boundary
internal/openstack/             authentication and active purge integration
internal/openstack/client/      Gophercloud provider and service client creation
internal/openstack/network/     typed Neutron operations
internal/openstack/loadbalancer/ typed Octavia operations
internal/openstack/volume/      typed Cinder operations
internal/openstack/identity/    typed Keystone operations
chart/                          Helm chart and helm-unittest snapshots
config/                         Kustomize deployment, RBAC, metrics, and network policy
nix/                            manager, OCI image, test, and SBOM derivations
test/e2e/                       Kind manager and metrics smoke scaffold
docs/design/                    behavior and ownership contracts
```

There is no project-owned API or webhook. CAPO's public API types are the
watched contract. Do not scaffold a CRD, status API, or webhook unless an
approved design requires one.

`internal/openstack/gophercloud_resources.go` is the active typed purge path.
`internal/openstack/resources.go` is a legacy manual HTTP implementation. Do not
add new behavior to the legacy path. Remove it only after equivalent regressions
cover the active path.

## Safety invariants

These rules apply to every cleanup change:

- Scope discovery to the authenticated project and selected region.
- Apply the exact selector from the ownership matrix before passing an ID to a
  delete method.
- Treat the service load balancer prefix as necessary but not sufficient until
  the shared load balancer and reserved-tag release decision is complete.
- Use the same selector for deletion and later verification.
- List every page. Treat an incomplete inventory as a failure.
- Keep close negative fixtures for a different cluster, partial match, empty
  field, project, and region.
- Treat accepted deletion as pending until a later observation proves absence.
- Keep the finalizer when identity, discovery, deletion, or verification is
  incomplete.
- Delete an application credential last, after persisting the
  resources-verified checkpoint.
- Do not delete a credential or Secret while another finalizer remains.
- Do not delete CAPO-owned networks, subnets, routers, ports, security groups,
  API server load balancers, bastions, or `OpenStackMachine` servers.
- Do not turn `NotFound` into authorization for an object that never passed the
  selector.

Any proposed selector change must follow the
[ownership change rules](docs/design/resource-ownership-matrix.md#changing-an-ownership-rule).

## Controller rules

- Reconciliation is level based and idempotent.
- One reconcile runs one bounded cleanup iteration.
- Use `RequeueAfter` for expected OpenStack convergence. Return operational
  errors for controller-runtime backoff.
- Do not call `time.Sleep`, poll in a loop, or mutate an annotation only to
  trigger another reconcile.
- Honor CAPI pause and the standard watch filter.
- Watch referenced Secrets so that a repaired identity can unblock deletion.
- Validate `identityRef.type` before reading a Secret or contacting OpenStack.
- Kubernetes `Secret.Data` already contains decoded bytes. Do not base64-decode
  it again.
- Patch Janitor-owned metadata without replacing concurrent updates.
- Do not write Janitor conditions into CAPO-owned status.
- Preserve `janitor.capi.stackhpc.com` until a separate migration design changes
  it.
- Never run the Python and Go controllers against the same objects.

Events and metrics must describe the lifecycle point accurately. A successful
resource purge is not a completed cleanup if Secret deletion or finalizer
removal can still fail. An application credential self-deletion `403` needs a
visible credential-may-remain outcome.

## OpenStack rules

Use Gophercloud for authentication, endpoint selection, reauthentication,
pagination, request models, and response extraction. Do not add a second service
catalog or token implementation.

Keep transport and policy separate:

- resource services list facts and delete already-authorized IDs
- `internal/cleanup` decides ownership, policy, phase, and outcome
- the controller resolves Kubernetes objects and translates cleanup outcomes
  into reconcile results

Create optional service clients lazily. A disabled cleanup phase must not
require an endpoint or permission for that service. Propagate the reconcile
context through every OpenStack call.

The replacement identity contract is a same-namespace Secret with a selected
`clouds.yaml` entry, optional `cacert`, and explicitly supported application
credential or password authentication. Do not enable another Gophercloud auth
method by accident.

## Tests

Use the smallest test that proves the boundary:

- pure Go tests for selectors, policy, ordering, outcomes, and checkpoint state
- HTTP fixtures for Gophercloud authentication, pagination, request options,
  error classification, and context cancellation
- envtest with CAPI and CAPO CRDs for watches, pause, filters, Secrets,
  conflicts, finalizers, and restarts
- Kind for manager, packaging, and cluster integration smoke tests
- a dedicated OpenStack project for destructive owned and non-owned fixtures

Do not infer coverage from a Make target name. The Kind suite is intended as
packaging smoke coverage, but its current image reference mismatch must be
fixed before it provides that evidence. Check the
[roadmap](ROADMAP.md#4-prove-the-controller-boundary) for current suite
limitations.

Never run destructive cleanup tests against a development or production
project with unrelated resources. Use an isolated project, explicit fixture
IDs, and teardown that preserves evidence after a failed boundary assertion.

## Development commands

```sh
make help
make fmt
make vet
make test
make build
make run
make test-e2e
make build-installer IMG=<registry>/<image>:<tag>
```

`make test-e2e` creates or reuses the dedicated
`cluster-api-janitor-openstack-test-e2e` Kind cluster. A successful run deletes
it; a failed run may leave it for inspection. Do not point the suite at a real
cluster.

After a Go change, run at least `make fmt`, `make vet`, and the affected tests.
Run `make test` before handing off a complete change when dependency downloads
are available. For a documentation-only change, verify links, headings,
whitespace, and references to actual files and targets.

## Generated and packaged files

- `config/rbac/role.yaml` is generated from Kubebuilder RBAC markers by
  `make manifests`. Edit the markers, then regenerate the file.
- `PROJECT` is Kubebuilder metadata. Do not hand-edit it for an ordinary code
  change.
- Helm snapshots under `chart/tests/__snapshot__/` are generated by
  helm-unittest. Update them only with the chart change they represent.
- `.release/install.yaml` and `.release/dist/` are local release staging output
  and are not committed.

This repository has no generated project API types or CRD bases today. Do not
copy generic Kubebuilder instructions that assume they exist.

## Documentation style

Write for Kubernetes and OpenStack contributors who need to review a destructive
decision quickly.

- Use present tense, active voice, and U.S. English.
- Prefer short sentences and concrete nouns.
- State current behavior, target behavior, and open work separately.
- Avoid generated-sounding epics, user stories, repeated Gherkin, ceremonial
  summaries, and unverified progress percentages.
- Do not use a static test count or coverage percentage as proof of safety.
- Use `LoadBalancer` for the Kubernetes API value and “load balancer” for the
  general resource.
- Keep commands, flags, fields, URLs, and resource names exact.
- Link an upstream issue or code path when it is the reason for a compatibility
  rule.

For Go declaration comments, start with the exported name. Keep error strings
lowercase so callers can wrap them.

Follow these references for API, documentation, Event, and logging changes:

- [Kubernetes API conventions](https://github.com/kubernetes/community/blob/main/contributors/devel/sig-architecture/api-conventions.md)
- [Kubernetes documentation style](https://kubernetes.io/docs/contribute/style/style-guide/)
- [Kubernetes logging](https://github.com/kubernetes/community/blob/main/contributors/devel/sig-instrumentation/logging.md)

## Logging and review

Kubernetes log messages start with a capital letter, use active voice, name the
object type, and do not end with a period. Use balanced structured key-value
pairs. Do not log an error and return the same error unless the log adds useful
context at a deliberate verbosity level.

A pull request that can change cleanup must follow the
[destructive change checklist](CONTRIBUTING.md#destructive-change-checklist). It
must also state which Python behavior it preserves or corrects and whether the
change is safe before the remaining roadmap work is complete.

Unresolved destructive behavior belongs in a design decision or the roadmap,
not in guessed code.
