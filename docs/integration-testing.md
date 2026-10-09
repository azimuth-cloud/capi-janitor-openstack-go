# Azimuth integration tests

The first release replaces the Python Janitor's existing cleanup scope.
The `Integration tests` workflow deploys the candidate Go chart into the existing Azimuth CI environment and runs its normal Kubernetes cluster tests.
The suite template in `test/integration/janitor_suite.robot` includes the upstream suite and adds one Janitor acceptance test.
The Python files use Azimuth's existing Robot Framework environment and remain as regression tests after replacement.
They are not included in the Go controller's runtime image.

## What the additional test checks

The test creates a unique namespace, a disposable application credential, and OpenStack resources with the existing OCCM and Cinder ownership fields.
It creates ownerless `OpenStackCluster` objects so CAPO does not provision or delete their infrastructure.
There are no workload Services or PVCs managing these fixtures, so OCCM and CSI cannot remove them during the test.

The same fixtures are used in three deletion rounds. Each round creates a new `OpenStackCluster` with the shared cleanup name in its cluster label.

| Round | Expected result |
| --- | --- |
| Foreign cluster LB tag, volume policy `keep`, credential annotation absent | Keep the LB, listener, pool, VIP FIP, volumes, snapshot, credential, and Secret. Delete the matching service security group |
| Malformed reserved LB tag, volume policy `keep`, credential policy `Delete` | Keep the protected resources and credential. Only the exact lowercase `delete` value opts into credential deletion |
| Same cluster LB tags and a second tagless LB, volume policy `delete`, credential policy `delete` | Delete both LBs and their listeners, pools, and VIP FIPs. Delete the owned snapshot and volume, retain the volume marked `keep=true`, then delete the test credential and Secret |

The `OpenStackCluster` objects have no API server load balancer configuration or status.
This checks that Janitor can remove workload load balancers without that CAPO status gate.
Each object starts paused with the Python finalizer `janitor.capi.stackhpc.com` already present, then deletion is requested and cleanup is unpaused.
This checks compatibility with the existing finalizer.

The test waits for Janitor to finish, then immediately reads each recorded resource ID before fixture teardown starts.
Only an exact resource `404` means it is absent. Authentication failures and other API errors fail the test.
Credential existence is checked with the independent CI credential, which remains valid after the test credential is deleted.
The fixture cleanup code removes resources left by failed tests and resources that Janitor must preserve.
It only uses IDs created by this test and reports cleanup failures.

## Running in CI

The workflow runs on `pull_request` so it uses the workflow changes in that PR.
PRs can target `main` or another development branch, including a branch under review.
Related source, packaging, workflow, or integration test changes trigger the cloud run.
Artifact publication and cloud tests run only for PRs from the same repository, excluding Dependabot authors and actors.
Fork and Dependabot PRs still run the ordinary CI checks, including the offline Python tests.

Until the trigger change is merged into `main`, a PR targeting `main` may show both the old `pull_request_target` run and the new `pull_request` run.
The event types have separate concurrency groups so the old run cannot cancel the new one.
Check the `pull_request` run for the new tests.
Changing a PR's base alone does not trigger this workflow. Push a new commit or reopen the PR to start a fresh run against the new base.

The workflow uses the existing `OS_CLOUDS` secret and `TARGET_CLOUD` variable.
The selected credential must be able to create application credentials, as required by the Azimuth platform tests.
The disposable credential is unrestricted so it can delete itself, and expires after one day if cleanup is interrupted.

The cloud needs Octavia, Neutron, and Cinder, including LB tags and snapshots.
Allow quota for two LBs with listeners and pools, two floating IPs, two 1 GiB volumes, one snapshot, one security group, and a temporary network, subnet, and router.
Octavia may also consume provider resources such as Amphora instances.
If more than one external network is visible, set the repository variable `JANITOR_TEST_EXTERNAL_NETWORK_ID` to the test network ID.

The Janitor source is checked out under `janitor-source` because the Azimuth setup action uses the workspace root.
The keywords call Azimuth's `bin/kube-connect` to reach the management cluster and stop that proxy during teardown.
They do not use the developer's default kubeconfig.
Results appear in the existing Robot report and debug bundle artifacts.

To rerun only the Janitor case in an already provisioned, dedicated Azimuth CI environment, run from its azimuth-config checkout:

```sh
source ./ci.env
source ./bin/activate "$AZIMUTH_CONFIG_ENVIRONMENT" "$AZIMUTH_ENVIRONMENT"
export JANITOR_TEST_SOURCE=/absolute/path/to/capi-janitor-openstack-go
ansible-playbook azimuth_cloud.azimuth_ops.generate_tests -e @extra-vars.yml \
  -e "generate_tests_kubernetes_suite_template=$JANITOR_TEST_SOURCE/test/integration/janitor_suite.robot"
./bin/run-tests --include janitor
```

This creates and deletes real cloud resources. Use the isolated CI environment.
If the runner is forcibly stopped before teardown finishes, use the fixture IDs in the Robot log to remove remaining test resources.
Do not use name prefixes to sweep the project.

## Offline checks and remaining validation

The helper checks run without OpenStack or Kubernetes access:

```sh
python3 -m venv .venv
source .venv/bin/activate
pip install -r test/integration/requirements.txt
python -m unittest discover -s test/integration -v
```

These checks validate the test assertions and cleanup guards. They are not evidence that a deployed controller passed the cloud scenarios.
The live workflow must pass for the candidate commit before release.
Checkpoint failures, restart recovery, and API conflicts remain covered by the existing Go unit tests and envtest instead of being repeated here.

The pre-existing finalizer case does not perform a Python to Go Helm upgrade.
In the representative migration check, create a cluster under Python, stop Python, deploy the candidate Go chart, and delete that cluster.
Confirm that its existing finalizer is released after cleanup and that the expected resources are absent.
Record that result alongside the live workflow run before declaring the replacement validated.
