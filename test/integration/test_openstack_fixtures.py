"""Offline checks for fixture ownership, assertions and teardown."""

from types import SimpleNamespace
import unittest
from unittest.mock import Mock, call, patch

from openstack_fixtures import FixtureError, OpenStackFixtures, ResourceRef, assert_resources


class HTTPError(Exception):
    def __init__(self, status):
        super().__init__("private-credential-value secret-token")
        self.status_code = status


class ResourceAssertionsTest(unittest.TestCase):
    def test_presence_uses_an_exact_get(self):
        getter = Mock(return_value=SimpleNamespace(id="volume-id"))
        ref = ResourceRef("volume", "volume-id", getter)
        assert_resources(present=[ref])
        getter.assert_called_once_with("volume-id")
        with self.assertRaises(AssertionError):
            assert_resources(absent=[ref])

    def test_only_http_404_proves_absence(self):
        getter = Mock(side_effect=HTTPError(404))
        ref = ResourceRef("volume", "volume-id", getter)
        assert_resources(absent=[ref])
        getter.assert_called_once_with("volume-id")
        with self.assertRaises(AssertionError):
            assert_resources(present=[ref])
        for status in (None, 401, 403, 500):
            with self.subTest(status=status):
                getter = Mock(side_effect=HTTPError(status))
                with self.assertRaises(FixtureError):
                    assert_resources(absent=[ResourceRef("volume", "volume-id", getter)])

    def test_none_without_404_fails(self):
        ref = ResourceRef("volume", "volume-id", Mock(return_value=None))
        with self.assertRaisesRegex(FixtureError, "without 404"):
            assert_resources(absent=[ref])

    def test_forbidden_error_omits_credentials(self):
        ref = ResourceRef("volume", "volume-id", Mock(side_effect=HTTPError(403)))
        with self.assertRaises(FixtureError) as raised:
            ref.get()
        self.assertIn("HTTP 403", str(raised.exception))
        self.assertNotIn("private-credential-value", str(raised.exception))
        self.assertNotIn("secret-token", str(raised.exception))
        self.assertTrue(raised.exception.__suppress_context__)


class PartialCreationTest(unittest.TestCase):
    def test_teardown_uses_created_ids_in_reverse_order_and_reports_failures(self):
        for delete_fails in (False, True):
            with self.subTest(delete_fails=delete_fails):
                network = SimpleNamespace(id="network-id")
                subnet = SimpleNamespace(id="subnet-id")
                deleted = []

                def delete(identifier, *, ignore_missing):
                    self.assertFalse(ignore_missing)
                    deleted.append(identifier)
                    if delete_fails:
                        raise HTTPError(403 if identifier == subnet.id else 409)

                proxy = SimpleNamespace(
                    create_network=Mock(return_value=network),
                    get_network=Mock(side_effect=[network, HTTPError(404)]),
                    delete_network=Mock(side_effect=delete),
                    create_subnet=Mock(return_value=subnet),
                    get_subnet=Mock(side_effect=[subnet, HTTPError(404)]),
                    delete_subnet=Mock(side_effect=delete),
                    create_router=Mock(side_effect=HTTPError(500)),
                    get_router=Mock(),
                    delete_router=Mock(),
                )
                fixtures = OpenStackFixtures(
                    SimpleNamespace(network=proxy), "fixture-name", "external-network-id"
                )
                with patch("openstack_fixtures.logger.info") as log:
                    with self.assertRaisesRegex(FixtureError, "Create router: HTTPError"):
                        fixtures.create_load_balancer([])
                self.assertEqual(log.call_args_list, [
                    call("Created OpenStack network network-id"),
                    call("Created OpenStack subnet subnet-id"),
                ])

                if delete_fails:
                    with self.assertRaises(FixtureError) as raised:
                        fixtures.cleanup()
                    message = str(raised.exception)
                    self.assertIn("HTTP 403", message)
                    self.assertIn("HTTP 409", message)
                    self.assertNotIn("private-credential-value", message)
                    self.assertNotIn("secret-token", message)
                else:
                    fixtures.cleanup()
                self.assertEqual(deleted, [subnet.id, network.id])
                proxy.delete_subnet.assert_called_once_with(subnet.id, ignore_missing=False)
                proxy.delete_network.assert_called_once_with(network.id, ignore_missing=False)
                proxy.delete_router.assert_not_called()

    def test_cascade_cleanup_survives_partial_load_balancer_setup(self):
        for failed_step in ("create_listener", "create_pool", "create_ip"):
            with self.subTest(failed_step=failed_step):
                resources = {"unrelated-id": SimpleNamespace(id="unrelated-id")}

                def create(identifier):
                    resource = SimpleNamespace(
                        id=identifier, provisioning_status="ACTIVE", vip_port_id="vip-port"
                    )
                    resources[identifier] = resource
                    return resource

                def get(identifier):
                    if identifier not in resources:
                        raise HTTPError(404)
                    return resources[identifier]

                def delete_lb(identifier, *, cascade, ignore_missing):
                    self.assertEqual(identifier, "lb-id")
                    self.assertTrue(cascade)
                    self.assertFalse(ignore_missing)
                    for child in ("lb-id", "listener-id", "pool-id"):
                        resources.pop(child, None)

                octavia = SimpleNamespace(
                    create_load_balancer=Mock(side_effect=lambda **_: create("lb-id")),
                    get_load_balancer=Mock(side_effect=get),
                    delete_load_balancer=Mock(side_effect=delete_lb),
                    create_listener=Mock(side_effect=lambda **_: create("listener-id")),
                    get_listener=Mock(side_effect=get),
                    create_pool=Mock(side_effect=lambda **_: create("pool-id")),
                    get_pool=Mock(side_effect=get),
                )
                network = SimpleNamespace(create_ip=Mock(), get_ip=Mock(), delete_ip=Mock())
                target = network if failed_step == "create_ip" else octavia
                getattr(target, failed_step).side_effect = HTTPError(500)
                fixtures = OpenStackFixtures(
                    SimpleNamespace(load_balancer=octavia, network=network),
                    "fixture-name", "external-network-id",
                )
                fixtures._subnet = SimpleNamespace(id="subnet-id")
                with self.assertRaises(FixtureError):
                    fixtures.create_load_balancer([])
                fixtures.cleanup()
                octavia.delete_load_balancer.assert_called_once_with(
                    "lb-id", cascade=True, ignore_missing=False
                )
                network.delete_ip.assert_not_called()
                self.assertEqual(set(resources), {"unrelated-id"})


if __name__ == "__main__":
    unittest.main()
