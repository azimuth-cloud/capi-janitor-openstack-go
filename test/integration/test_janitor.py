"""Offline checks for the acceptance test's assertions and cleanup boundaries."""

import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from openstack import exceptions
from requests import Response
import yaml

import Janitor as integration
from openstack_fixtures import FixtureError, ResourceRef


def http_error(status):
    response = Response()
    response.status_code = status
    error_type = exceptions.NotFoundException if status == 404 else exceptions.HttpException
    return error_type("parent-secret child-secret", response=response)


class JanitorTests(unittest.TestCase):
    def setUp(self):
        self.janitor = integration.Janitor()
        self.janitor.name = "janitor-test-owned"
        self.janitor.user_id = "test-user"
        self.janitor.credential = SimpleNamespace(id="child-id", secret="child-secret")
        self.janitor.connection = Mock()
        self.janitor.fixtures = Mock()
        self.cluster = {"metadata": {
            "uid": "cluster-uid", "resourceVersion": "7",
            "finalizers": [integration.FINALIZER],
        }}

    def prepare_delete(self, observations):
        self.janitor._create = Mock(return_value=self.cluster)
        self.janitor._kubectl = Mock()
        self.janitor._get = Mock(side_effect=observations)
        self.janitor._check_credential_and_secret = Mock()

    def test_preservation_does_not_pass_before_controller_finishes(self):
        protected = ResourceRef("volume", "kept", Mock(return_value=object()))
        self.prepare_delete([self.cluster, self.cluster])
        with patch.object(integration.time, "monotonic", side_effect=[0, 1, 901]), \
                patch.object(integration.time, "sleep"):
            with self.assertRaisesRegex(AssertionError, "did not finish"):
                self.janitor._delete_cluster("keep", "keep", None, [], [protected])
        self.janitor._check_credential_and_secret.assert_not_called()
        self.janitor.fixtures.cleanup.assert_not_called()

    def test_leftover_resource_fails_before_fallback_cleanup(self):
        leftover = ResourceRef("floating_ip", "fip-id", Mock(return_value=object()))
        for finished in (None, {"metadata": {"uid": "cluster-uid", "finalizers": []}}):
            with self.subTest(finished=finished):
                self.prepare_delete([self.cluster, finished])
                with self.assertRaisesRegex(AssertionError, "fip-id still exists"):
                    self.janitor._delete_cluster("owned", "delete", "delete", [leftover], [])
                self.janitor.fixtures.cleanup.assert_not_called()
                self.janitor._check_credential_and_secret.assert_not_called()

    def test_resource_checks_run_after_controller_finishes(self):
        self.prepare_delete([self.cluster, self.cluster, None])
        observed_after_finish = []

        def get_kept(_):
            observed_after_finish.append(self.janitor._get.call_count)
            return object()

        kept = ResourceRef("volume", "kept", get_kept)
        removed = ResourceRef("volume", "deleted", Mock(side_effect=[
            object(), http_error(404),
        ]))
        with patch.object(integration.time, "sleep"):
            self.janitor._delete_cluster("owned", "delete", "delete", [removed], [kept])
        self.assertEqual(observed_after_finish, [0, 3])
        self.janitor._check_credential_and_secret.assert_called_once_with(present=False)

    def test_credential_absence_requires_exact_404(self):
        self.janitor._get = Mock(return_value=None)
        getter = self.janitor.connection.identity.get_application_credential
        for status in (404, 401, 403):
            with self.subTest(status=status):
                getter.reset_mock()
                getter.side_effect = http_error(status)
                if status == 404:
                    self.janitor._check_credential_and_secret(present=False)
                else:
                    with self.assertRaises(FixtureError) as raised:
                        self.janitor._check_credential_and_secret(present=False)
                    self.assertNotIn("secret", str(raised.exception))
                getter.assert_called_once_with("test-user", "child-id")

    def test_keep_requires_both_credential_and_secret(self):
        self.janitor._get = Mock(return_value={"metadata": {"name": "cloud"}})
        self.janitor._check_credential_and_secret(present=True)
        self.janitor._get.return_value = None
        with self.assertRaisesRegex(AssertionError, "Secret should be present"):
            self.janitor._check_credential_and_secret(present=True)

    def test_resource_replacement_cannot_count_as_cleanup(self):
        self.prepare_delete([self.cluster, {"metadata": {"uid": "foreign-uid", "finalizers": []}}])
        with self.assertRaisesRegex(AssertionError, "replaced"):
            self.janitor._delete_cluster("owned", "delete", "delete", [], [])
        self.janitor._check_credential_and_secret.assert_not_called()

    def test_cloud_uses_child_auth_and_embeds_custom_ca(self):
        config = self.janitor.connection.config
        config.get_auth_args.return_value = {
            "auth_url": "https://keystone.example/v3",
            "application_credential_id": "parent-id", "password": "parent-secret",
        }
        config.get_region_name.return_value = "RegionTwo"
        config.get_interface.return_value = "internal"
        with tempfile.TemporaryDirectory() as directory:
            ca = Path(directory) / "ca.pem"
            ca.write_text("test CA PEM")
            for verify in (True, False, str(ca)):
                with self.subTest(verify=verify):
                    config.get_requests_verify_args.return_value = (verify, None)
                    data = integration.credential_cloud(self.janitor.connection, self.janitor.credential)
                    cloud = yaml.safe_load(data["clouds.yaml"])["clouds"][integration.CLOUD_NAME]
                    self.assertEqual(cloud["auth"], {
                        "auth_url": "https://keystone.example/v3",
                        "application_credential_id": "child-id",
                        "application_credential_secret": "child-secret",
                    })
                    self.assertEqual((cloud["region_name"], cloud["interface"]), ("RegionTwo", "internal"))
                    self.assertIs(cloud["verify"], verify is not False)
                    self.assertEqual(data.get("cacert"), "test CA PEM" if isinstance(verify, str) else None)
                    self.assertNotIn("parent", data["clouds.yaml"])

    def test_client_certificate_is_not_silently_dropped(self):
        self.janitor.connection.config.get_requests_verify_args.return_value = (True, "/client.pem")
        with self.assertRaisesRegex(AssertionError, "client certificates"):
            integration.credential_cloud(self.janitor.connection, self.janitor.credential)

    def test_cluster_cleanup_refuses_replacement_and_locks_patch(self):
        self.janitor.cluster_uids["cluster"] = "expected-uid"
        self.janitor._get = Mock(return_value=self.cluster)
        self.janitor._kubectl = Mock()
        with self.assertRaisesRegex(AssertionError, "replaced"):
            self.janitor._patch_owned_cluster("cluster", remove_finalizer=True)
        self.janitor._kubectl.assert_not_called()
        self.janitor.cluster_uids["cluster"] = "cluster-uid"
        self.janitor._patch_owned_cluster("cluster", pause=True)
        args = self.janitor._kubectl.call_args.args
        metadata = json.loads(args[args.index("-p") + 1])["metadata"]
        self.assertEqual(metadata["resourceVersion"], "7")
        self.cluster["metadata"]["finalizers"].append("example.com/other")
        self.janitor._patch_owned_cluster("cluster", remove_finalizer=True)
        patch_data = json.loads(self.janitor._kubectl.call_args.args[-1])
        self.assertEqual(patch_data["metadata"]["finalizers"], ["example.com/other"])

    def test_namespace_cleanup_refuses_replacement(self):
        self.janitor.namespace_uid = "expected-uid"
        self.janitor._get = Mock(return_value={"metadata": {"uid": "foreign-uid"}})
        self.janitor._kubectl = Mock()
        with self.assertRaisesRegex(AssertionError, "replaced"):
            self.janitor._remove_namespace()
        self.janitor._kubectl.assert_not_called()
        self.janitor._get.return_value = {"metadata": {"uid": "expected-uid"}}
        self.janitor._remove_namespace()
        request = self.janitor._kubectl.call_args
        self.assertIn(f"--raw=/api/v1/namespaces/{self.janitor.name}", request.args)
        self.assertEqual(request.kwargs["body"]["preconditions"], {"uid": "expected-uid"})

    def test_teardown_cleans_partial_setup(self):
        janitor = integration.Janitor()
        connection = self.janitor.connection
        connection.session.get_user_id.return_value = "test-user"
        connection.network.networks.return_value = [SimpleNamespace(id="external-id")]
        connection.identity.create_application_credential.return_value = self.janitor.credential
        fixture = Mock()
        fixture.create_storage.side_effect = AssertionError("setup failed")
        janitor._connect_management_cluster = Mock()
        janitor._check_controller = Mock()
        janitor._create = Mock(return_value={"metadata": {"uid": "namespace-uid"}})
        janitor._remove_namespace = Mock()
        environment = {key: "test" for key in (
            "OS_CLOUD", "OS_CLIENT_CONFIG_FILE", "AZIMUTH_CONFIG_ROOT", "AZIMUTH_ENVIRONMENT",
        )}
        with patch.dict(integration.os.environ, environment, clear=True), \
                patch.object(integration.openstack, "connect", return_value=connection), \
                patch.object(integration, "credential_cloud", return_value={}), \
                patch.object(integration, "OpenStackFixtures", return_value=fixture):
            with self.assertRaisesRegex(AssertionError, "setup failed"):
                janitor.prepare_janitor_fixtures()
        janitor.remove_janitor_fixtures()
        fixture.cleanup.assert_called_once_with()
        janitor._remove_namespace.assert_called_once_with()
        connection.identity.delete_application_credential.assert_called_once_with(
            "test-user", "child-id", ignore_missing=True,
        )
        connection.close.assert_called_once_with()

    def test_teardown_stops_proxy_even_if_connection_close_fails(self):
        self.janitor.connect_script = "/test/kube-connect"
        self.janitor.connection.close.side_effect = RuntimeError("private error detail")
        with patch.object(integration.subprocess, "run") as run:
            with self.assertRaises(AssertionError) as raised:
                self.janitor.remove_janitor_fixtures()
        self.assertEqual(run.call_args.args[0], ["/test/kube-connect", "--terminate"])
        self.assertNotIn("private error detail", str(raised.exception))


if __name__ == "__main__":
    unittest.main()
