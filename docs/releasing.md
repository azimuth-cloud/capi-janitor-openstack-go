# Releasing and versioning

Git tags are the source of truth for released versions. Do not update a version
file or ask GoReleaser to calculate the next version.

> [!WARNING]
> The Go rewrite is still stabilising throughout the `v0.x.y` series and is not
> yet considered production-stable. Test migration from the Python controller
> and rollback to it before deploying a release.

## Version tags

Release tags must use OCI-compatible Semantic Versioning:

```text
vMAJOR.MINOR.PATCH[-PRERELEASE]
```

For example, `v0.2.0` and `v0.2.0-rc.1` are valid. Build metadata such as
`v0.2.0+build.1` is not allowed because `+` is not valid in an OCI image tag.
Never move or reuse a published tag.

A tag maps to the artifacts as follows:

| Artifact | Version for tag `v0.2.0` |
|---|---|
| GitHub Release | `v0.2.0` |
| OCI image | `ghcr.io/azimuth-cloud/capi-janitor-openstack-go:v0.2.0` |
| Helm chart | chart version `0.2.0`, app version `v0.2.0` |
| Binary archives | `capi-janitor-openstack-go_v0.2.0_linux_<arch>.tar.gz` |
| Kubernetes bundle | `install.yaml`, referencing the `v0.2.0` OCI image |

The OCI workflow also publishes a commit-SHA tag. Branch builds continue to use
branch and commit-SHA tags, but they do not create GitHub Releases.

## What builds each artifact

- Nix remains the only OCI image builder. GoReleaser does not build or publish
  container images.
- The existing Helm publisher packages and publishes the chart after both Nix
  image architectures have been pushed.
- GoReleaser builds the Linux `amd64` and `arm64` `manager` binaries, archives
  them with the README and licence, creates SHA-256 checksums, and attaches the
  standalone Kubernetes bundle to the GitHub Release.

The Nix image and GoReleaser archives are separate builds from the same tagged
commit; they are not expected to contain byte-identical binaries.

## Before tagging

1. Confirm the release acceptance criteria in
   [the Go rewrite guidelines](design/go-rewrite-guidelines.md) are satisfied.
2. Confirm CI is green, including the `GoReleaser snapshot` job. This snapshot
   builds archives, checksums, and the Kubernetes bundle without publishing
   anything.
3. Review the proposed tag and release notes.
4. Create and push the tag from the intended commit:

   ```sh
   git tag -a v0.2.0 -m "v0.2.0"
   git push origin v0.2.0
   ```

## Publication order

The tag workflow deliberately publishes in this order:

1. Build and push the multi-architecture OCI image with Nix.
2. Package and publish the Helm chart.
3. Build the binary archives and Kubernetes bundle and upload them to a draft
   GitHub Release.
4. Make the GitHub Release public only after every preceding step succeeds.

If an image or chart job fails, no GitHub Release is created. If GoReleaser
fails after creating its draft, the draft remains private and can be reused by a
rerun. Fix the failure and rerun the failed job; do not create or move another
tag for the same version.

## Local snapshot

With GoReleaser v2 installed, the same packaging validation can be run locally:

```sh
goreleaser release --snapshot --clean
```

Snapshot artifacts are written to `.release/dist/`; the generated release
bundle is staged at `.release/install.yaml`. The `.release/` directory is not
committed.
