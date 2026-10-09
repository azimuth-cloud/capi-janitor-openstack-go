# Contributing

We welcome contributions and suggestions for improvements to this code base.
Please check for relevant issues and PRs before opening a new one of your own.

## Making a contribution

When raising a PR from a forked repo the integration CI tests will fail due to
missing secrets needed to push artifacts and access the cloud.
Contributors should raise a PR against a feature branch in the organisation's
repository. A maintainer will review and, once approved, merge the fork branch
into an internal branch. From there a second PR will be raised to merge to the
default branch and that will run the full integration test suite.

### Helm template snapshots

The CI in this repository uses the Helm
[unittest](https://github.com/helm-unittest/helm-unittest) plugin's
snapshotting functionality to check PRs for changes to the templated manifests.
Therefore, if your PR makes changes to the manifest templates or values, you
will need to update the saved snapshots to allow your changes to pass the
automated tests. The easiest way to do this is to run the `helm unittest` command
inside a docker container from the repo root.

```
docker run -i --rm -v $(pwd):/apps helmunittest/helm-unittest chart -u
```

where the `-u` option is used to update the existing snapshots.
