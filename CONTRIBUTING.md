# Contributing

Contributions are welcome. This controller makes destructive cloud API calls,
so cleanup changes need stronger ownership and failure evidence than ordinary
controller changes.

Check the [roadmap](ROADMAP.md) and existing issues before starting work. For a
new resource type or selector, open a design discussion first.

## Development

The common local checks are:

```sh
make fmt
make vet
make test
make build
```

`make test` runs Go unit tests, HTTP fixtures, and fake-client controller tests.
`make test-e2e` is intended as an isolated Kind smoke suite, but its current
image reference mismatch must be fixed first. Neither command meets the
replacement evidence required for Kubernetes API interactions or the
destructive OpenStack boundary; see the
[current test gaps](ROADMAP.md#4-prove-the-controller-boundary).

Read the design contract before changing cleanup:

- [Go rewrite guidelines](docs/design/go-rewrite-guidelines.md)
- [Cleanup behavior matrix](docs/design/cleanup-behaviour-matrix.md)
- [Resource ownership matrix](docs/design/resource-ownership-matrix.md)
- [CAPO integration boundary](docs/design/capo-integration-boundary.md)

## Destructive change checklist

A pull request that can select, delete, or verify an OpenStack resource must
follow the [ownership change rules](docs/design/resource-ownership-matrix.md#changing-an-ownership-rule).
Its description must identify the affected rule, failure and restart behavior,
test evidence, and rollout and rollback impact. A replacement release also
requires a real OpenStack owned and non-owned assertion.

Do not broaden an ownership selector to fix one leftover without showing what
new resources become eligible. Follow the
[CAPO integration boundary](docs/design/capo-integration-boundary.md) instead of
taking ownership of CAPO-managed infrastructure.

Run destructive tests only in a dedicated OpenStack project. Do not use a
development or production project with unrelated resources.

## Reporting a bug

Include enough information to add a named regression:

- Janitor, CAPO, CAPI, Kubernetes, OCCM, Cinder CSI, and OpenStack versions
- the `OpenStackCluster` generation and relevant policy fields or annotations
- the resolved cluster name, region, interface, and identity type, without
  credential values
- the selected resource type, ID, name or description, and ownership metadata
- the finalizer state and controller Events and logs
- whether the failure survived a controller restart
- the smallest safe reproduction and the expected non-owned resources

Never attach `clouds.yaml`, tokens, passwords, application credential secrets,
or CA private keys.

## Generated files

`config/rbac/role.yaml` is generated from RBAC markers. Change the markers and
run `make manifests` instead of editing the generated file. `PROJECT` is
Kubebuilder metadata and normally remains unchanged.

### Helm snapshots

Helm template and value changes must update the helm-unittest snapshots used by
CI. From the repository root:

```sh
docker run -i --rm \
  -v "$(pwd):/apps" \
  helmunittest/helm-unittest chart -u
```

Review the snapshot diff. Generated output must reflect the intended manifest
change; snapshot regeneration is not a substitute for that review.
