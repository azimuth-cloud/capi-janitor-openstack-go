# Releasing and versioning

Git tags are the source of truth for released versions. Do not update a version
file or ask GoReleaser to calculate the next version.

> [!WARNING]
> The Go rewrite is still stabilizing throughout the `v0.x.y` series and is not yet considered stable for production.
> Test migration from the Python controller before deploying a release.
> Stop the Python controller before starting the Go controller.
> Roll back only when no managed object is actively deleting.
> The Go controller remains responsible for recovery after deletion starts.

## Version tags

Release tags must use Semantic Versioning that is valid for OCI:

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

The image workflow also publishes a tag for the commit SHA.
Branch builds continue to use branch tags and commit SHA tags, but they do not create GitHub Releases.
For PR and main builds, the chart version is `<base>-dev.<run-id>.<attempt>` and the app version is the short commit SHA.
The base is the SemVer action's major, minor and patch version, with any prerelease or build suffix removed.
The run ID and attempt come from `GITHUB_RUN_ID` and `GITHUB_RUN_ATTEMPT`.
A PR or main build of a tagged commit still receives a development version, so it does not overwrite the released chart.
Development charts are excluded from Artifact Hub, including builds made after an RC tag.

## What builds each artifact

- Nix remains the only OCI image builder. GoReleaser does not build or publish
  container images.
- The Helm OCI publisher packages, publishes and signs the chart after both Nix image architectures have been pushed, then publishes the Artifact Hub repository metadata.
- GoReleaser builds the Linux `amd64` and `arm64` `manager` binaries, archives them with the README and license, creates SHA-256 checksums, and attaches the standalone Kubernetes bundle to the GitHub Release.

The Nix image and GoReleaser archives are separate builds from the same tagged commit.
They are not expected to contain identical binaries.

## Helm chart distribution

The chart is published to `oci://quay.io/azimuth/cluster-api-janitor-openstack-charts/cluster-api-janitor-openstack`.
Controller images remain at `ghcr.io/azimuth-cloud/capi-janitor-openstack-go`.
Charts are no longer published to GitHub Pages.

The publisher uses `QUAY_IO_USER` and `QUAY_IO_TOKEN` to upload the chart and metadata.
It signs and verifies the chart with Cosign, using `id-token: write` for keyless signing.
Image publication continues to use `GITHUB_TOKEN` with `packages: write`.

The existing Artifact Hub registration is recorded in [`chart/artifacthub-repo.yaml`](../chart/artifacthub-repo.yaml).
Artifact Hub indexes the chart and repository metadata from Quay.
No manual upload or Artifact Hub token is needed.

### Publication setup

Configure `QUAY_IO_USER` and `QUAY_IO_TOKEN` as both Actions and Dependabot secrets, with write access to the Quay chart repository.
Keep the existing `OS_CLOUDS` secret in both stores for cloud tests.
The reusable publication workflow inherits the caller's secrets.

The Quay chart repository and GHCR controller image must both be public so CI and deployments can pull them anonymously.
Before running cloud integration tests or publishing a release, verify access without registry credentials.
Use an already published chart version without the release tag's leading `v`, and confirm that its app version identifies the expected image:

```sh
helm show chart \
  oci://quay.io/azimuth/cluster-api-janitor-openstack-charts/cluster-api-janitor-openstack \
  --version <version>
```

Also verify that `ghcr.io/azimuth-cloud/capi-janitor-openstack-go:<app-version>` can be pulled without credentials.

## Before tagging

1. For the first Python replacement release, confirm the [replacement work list](../ROADMAP.md#replacement-release-work), including a passing [Azimuth integration run and migration check](integration-testing.md).
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

1. Build and push the OCI image for both architectures with Nix.
2. Package, publish and sign the Helm chart, then publish the Artifact Hub repository metadata.
3. Build the binary archives and Kubernetes bundle and upload them to a draft
   GitHub Release.
4. Make the GitHub Release public only after every preceding step succeeds.

Uploads are separate operations. A later failure does not remove images, charts or metadata already published.
If an image or chart job fails, no GitHub Release is created. If GoReleaser
fails after creating its draft, the draft remains private and can be reused by a
rerun.
Fix the failure and rerun the failed job.
Do not create or move another tag for the same version.

## Local snapshot

With GoReleaser v2 installed, the same packaging validation can be run locally:

```sh
goreleaser release --snapshot --clean
```

Snapshot artifacts are written to `.release/dist/`.
The generated release bundle is staged at `.release/install.yaml`.
The `.release/` directory is not committed.
