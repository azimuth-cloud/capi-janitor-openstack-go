# Contributing

We welcome contributions and suggestions for improvements to this code base.
Please check for relevant issues and PRs before opening a new one of your own.

## Making a contribution

Fork PRs run the ordinary CI checks. Artifact publication and cloud integration tests are skipped because fork workflows cannot access the required credentials.

To run the full integration suite, open your PR against a feature branch in the organisation's repository. A maintainer will review the changes and merge them into that branch. A second PR from the internal branch to the default branch will run the integration tests.

Dependabot PRs use the `OS_CLOUDS` Dependabot secret and run the same integration tests as other internal PRs when relevant files change.

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
