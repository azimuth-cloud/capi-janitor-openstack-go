"""Robot keywords for the Janitor deployed by the Azimuth integration workflow."""

import datetime
from functools import partial
import json
import logging
import os
from pathlib import Path
import shlex
import subprocess
import tempfile
import time
import uuid

import openstack
from robot.api import logger
from robot.api.deco import keyword, library
import yaml

from openstack_fixtures import FixtureError, OpenStackFixtures, ResourceRef, assert_resources, cloud_call


FINALIZER = "janitor.capi.stackhpc.com"
PAUSED = "cluster.x-k8s.io/paused"
VOLUMES_POLICY = f"{FINALIZER}/volumes-policy"
CREDENTIAL_POLICY = f"{FINALIZER}/credential-policy"
CLUSTER_LABEL = "cluster.x-k8s.io/cluster-name"
OSC = "openstackclusters.infrastructure.cluster.x-k8s.io"
SECRET_NAME = "cloud"
CLOUD_NAME = "janitor-test"


def credential_cloud(connection, credential):
    verify, certificate = connection.config.get_requests_verify_args()
    if certificate:
        raise AssertionError("The Janitor does not support OpenStack client certificates")
    cloud = {
        "auth_type": "v3applicationcredential",
        "auth": {
            "auth_url": connection.config.get_auth_args()["auth_url"],
            "application_credential_id": credential.id,
            "application_credential_secret": credential.secret,
        },
        "region_name": connection.config.get_region_name(),
        "interface": connection.config.get_interface(),
        "verify": verify is not False,
    }
    data = {"clouds.yaml": yaml.safe_dump({"clouds": {CLOUD_NAME: cloud}})}
    if isinstance(verify, str):
        data["cacert"] = Path(verify).read_text()
    return data


@library(scope="SUITE")
class Janitor:
    def __init__(self):
        self.name = f"janitor-test-{uuid.uuid4().hex[:12]}"
        self.connection = None
        self.credential = None
        self.fixtures = None
        self.kubeconfig = None
        self.cluster_uids = {}
        self.namespace_uid = None
        self.connect_script = None

    @keyword
    def prepare_janitor_fixtures(self):
        for variable in ("OS_CLOUD", "OS_CLIENT_CONFIG_FILE", "AZIMUTH_CONFIG_ROOT", "AZIMUTH_ENVIRONMENT"):
            if not os.environ.get(variable):
                raise AssertionError(f"Activate the dedicated Azimuth test environment ({variable} is missing)")
        for name in ("openstack", "keystoneauth", "urllib3"):
            logging.getLogger(name).setLevel(logging.WARNING)
        self._connect_management_cluster()
        self._check_controller()
        self.connection = cloud_call("Connect to test cloud", openstack.connect,
                                     cloud=os.environ["OS_CLOUD"], api_timeout=60)
        cloud_call("Authenticate test observer", self.connection.authorize)
        self.user_id = self.connection.session.get_user_id()
        networks = cloud_call("List external networks", lambda: list(
            self.connection.network.networks(is_router_external=True)
        ))
        external_id = os.environ.get("JANITOR_TEST_EXTERNAL_NETWORK_ID")
        if external_id:
            if external_id not in {network.id for network in networks}:
                raise AssertionError("JANITOR_TEST_EXTERNAL_NETWORK_ID is not an accessible external network")
        elif len(networks) == 1:
            external_id = networks[0].id
        else:
            raise AssertionError("Set JANITOR_TEST_EXTERNAL_NETWORK_ID when the cloud has multiple external networks")

        namespace = self._create({"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": self.name}})
        self.namespace_uid = namespace["metadata"]["uid"]
        logger.info(f"Created test namespace {self.name} with UID {self.namespace_uid}")
        expires = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(days=1)
        self.credential = cloud_call(
            "Create test application credential", self.connection.identity.create_application_credential,
            self.user_id, self.name, unrestricted=True, expires_at=expires.strftime("%Y-%m-%dT%H:%M:%SZ"),
        )
        logger.info(f"Created test application credential {self.credential.id}")
        self._create({
            "apiVersion": "v1", "kind": "Secret", "metadata": {"name": SECRET_NAME, "namespace": self.name},
            "stringData": credential_cloud(self.connection, self.credential),
        })
        self.fixtures = OpenStackFixtures(self.connection, self.name, external_id)
        self.storage = self.fixtures.create_storage()
        self.security_group = self.fixtures.create_security_group()
        self.shared = self.fixtures.create_load_balancer([
            f"kube_service_{self.name}_default_one", "kube_service_another-cluster_default_two",
        ])
        logger.info(f"Created Janitor fixtures for {self.name}")

    @keyword
    def preserve_shared_load_balancers_and_volumes(self):
        protected = [*self.shared.values(), *self.storage.values()]
        self._delete_cluster("foreign", "keep", None, [self.security_group], protected)
        self.fixtures.set_load_balancer_tags(self.shared["load_balancer"], ["kube_service_invalid"])
        self._delete_cluster("malformed", "keep", "Delete", [], protected)

    @keyword
    def delete_owned_resources_and_credential(self):
        self.fixtures.set_load_balancer_tags(self.shared["load_balancer"], [
            f"kube_service_{self.name}_default_one", f"kube_service_{self.name}_default_two",
        ])
        tagless = self.fixtures.create_load_balancer([])
        deleted = [*self.shared.values(), *tagless.values(), self.storage["snapshot"], self.storage["volume"]]
        self._delete_cluster("owned", "delete", "delete", deleted, [self.storage["kept_volume"]])

    def _delete_cluster(self, suffix, volume_policy, credential_policy, absent, present):
        name = f"{self.name}-{suffix}"
        self._kubectl("patch", "secret", SECRET_NAME, "-n", self.name, "--type=merge", "-p", json.dumps({
            "metadata": {"annotations": {CREDENTIAL_POLICY: credential_policy}},
        }))
        cluster = self._create({
            "apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": "OpenStackCluster",
            "metadata": {
                "namespace": self.name, "name": name, "finalizers": [FINALIZER],
                "labels": {CLUSTER_LABEL: self.name},
                "annotations": {PAUSED: "", VOLUMES_POLICY: volume_policy},
            },
            # Without a CAPI owner, CAPO does not provision or delete infrastructure.
            "spec": {"identityRef": {"type": "Secret", "name": SECRET_NAME, "cloudName": CLOUD_NAME}},
        })
        self.cluster_uids[name] = cluster["metadata"]["uid"]
        assert_resources(present=[*absent, *present])
        self._kubectl("delete", OSC, name, "-n", self.name, "--wait=false")
        paused = self._get(OSC, name)
        if not paused or FINALIZER not in paused["metadata"].get("finalizers", []):
            raise AssertionError("The inherited Janitor finalizer disappeared while cleanup was paused")
        self._kubectl("annotate", OSC, name, "-n", self.name, f"{PAUSED}-")

        deadline = time.monotonic() + 900
        while time.monotonic() < deadline:
            cluster = self._get(OSC, name)
            if cluster is not None and cluster["metadata"]["uid"] != self.cluster_uids[name]:
                raise AssertionError("The test OpenStackCluster was replaced")
            if cluster is None or FINALIZER not in cluster["metadata"].get("finalizers", []):
                # Check immediately so teardown cannot hide resources left by Janitor.
                assert_resources(absent, present)
                self._check_credential_and_secret(present=credential_policy != "delete")
                logger.info(f"Verified {suffix} cleanup, volume policy {volume_policy}, credential policy {credential_policy}")
                return
            time.sleep(5)
        raise AssertionError(f"Janitor did not finish {name} within 15 minutes")

    def _check_credential_and_secret(self, *, present):
        credential = ResourceRef(
            "application_credential", self.credential.id,
            partial(self.connection.identity.get_application_credential, self.user_id),
        )
        expected = "present" if present else "absent"
        if (credential.get() is not None) != present:
            raise AssertionError(f"Application credential should be {expected}")
        if (self._get("secret", SECRET_NAME) is not None) != present:
            raise AssertionError(f"Secret should be {expected}")

    def _connect_management_cluster(self):
        self.connect_script = str(Path(os.environ["AZIMUTH_CONFIG_ROOT"]) / "bin/kube-connect")
        # The SSH proxy inherits output descriptors. Files let the parent shell exit normally.
        with tempfile.TemporaryFile(mode="w+") as output, tempfile.TemporaryFile(mode="w+") as errors:
            result = subprocess.run([self.connect_script], stdout=output, stderr=errors, timeout=180, check=False)
            if result.returncode:
                raise AssertionError("Could not connect to the Azimuth management cluster")
            output.seek(0)
            assignments = shlex.split(output.read())
        if len(assignments) != 2 or assignments[0] != "export" or not assignments[1].startswith("KUBECONFIG="):
            raise AssertionError("kube-connect did not return a kubeconfig path")
        self.kubeconfig = assignments[1].removeprefix("KUBECONFIG=")

    def _check_controller(self):
        deployments = self._kubectl("get", "deployments", "-n", "capi-janitor-system", "-o", "json")
        for deployment in deployments["items"]:
            images = [container["image"] for container in deployment["spec"]["template"]["spec"]["containers"]]
            if any(image.startswith("ghcr.io/azimuth-cloud/capi-janitor-openstack-go:") for image in images):
                if deployment.get("status", {}).get("availableReplicas", 0) > 0:
                    logger.info(f"Testing Go Janitor deployment {deployment['metadata']['name']} with images {images}")
                    return
        raise AssertionError("No available Go Janitor deployment found in capi-janitor-system")

    def _kubectl(self, *args, body=None):
        result = subprocess.run(
            ["kubectl", "--kubeconfig", self.kubeconfig, "--request-timeout=30s", *args],
            input=json.dumps(body) if body is not None else None, text=True,
            capture_output=True, timeout=60, check=False,
        )
        if result.returncode:
            # kubectl may repeat a Secret's submitted data in an error.
            raise AssertionError(f"kubectl {args[0]} {args[1]} failed (exit {result.returncode})")
        if "-o" in args and "json" in args:
            return json.loads(result.stdout) if result.stdout.strip() else None
        return None

    def _create(self, obj):
        return self._kubectl("create", "-f", "-", "-o", "json", body=obj)

    def _get(self, kind, name):
        return self._kubectl("get", kind, name, "-n", self.name, "--ignore-not-found", "-o", "json")

    @keyword
    def remove_janitor_fixtures(self):
        errors = []

        def attempt(description, action):
            try:
                action()
            except Exception as exc:
                detail = str(exc) if isinstance(exc, FixtureError) else type(exc).__name__
                errors.append(f"{description}: {detail}")

        for name in self.cluster_uids:
            attempt(f"Pause {name}", partial(self._patch_owned_cluster, name, pause=True))
        if self.fixtures is not None:
            attempt("Remove OpenStack fixtures", self.fixtures.cleanup)
        for name in self.cluster_uids:
            attempt(f"Remove finalizer from {name}", partial(self._patch_owned_cluster, name, remove_finalizer=True))
        if self.namespace_uid is not None:
            attempt("Remove test namespace", self._remove_namespace)
        if self.credential is not None:
            attempt("Remove test credential", lambda: cloud_call(
                "Delete test application credential", self.connection.identity.delete_application_credential,
                self.user_id, self.credential.id, ignore_missing=True,
            ))
        if self.connection is not None:
            attempt("Close test connection", self.connection.close)
        if self.connect_script is not None:
            attempt("Stop management proxy", lambda: subprocess.run(
                [self.connect_script, "--terminate"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                timeout=120, check=True,
            ))
        if errors:
            raise AssertionError(f"Fixture cleanup failed for {self.name}: " + ", ".join(errors))

    def _patch_owned_cluster(self, name, *, pause=False, remove_finalizer=False):
        cluster = self._get(OSC, name)
        if cluster is None:
            return
        if cluster["metadata"]["uid"] != self.cluster_uids[name]:
            raise AssertionError("The test OpenStackCluster was replaced")
        metadata = {"resourceVersion": cluster["metadata"]["resourceVersion"]}
        if pause:
            metadata["annotations"] = {PAUSED: ""}
        if remove_finalizer:
            metadata["finalizers"] = [
                item for item in cluster["metadata"].get("finalizers", []) if item != FINALIZER
            ]
        self._kubectl("patch", OSC, name, "-n", self.name, "--type=merge", "-p", json.dumps({"metadata": metadata}))

    def _remove_namespace(self):
        namespace = self._get("namespace", self.name)
        if namespace is None:
            return
        if namespace["metadata"]["uid"] != self.namespace_uid:
            raise AssertionError("The test namespace was replaced")
        self._kubectl("delete", f"--raw=/api/v1/namespaces/{self.name}", "-f", "-", body={
            "apiVersion": "v1", "kind": "DeleteOptions", "preconditions": {"uid": self.namespace_uid},
        })
