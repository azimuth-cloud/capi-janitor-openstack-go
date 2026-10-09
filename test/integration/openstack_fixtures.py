"""OpenStack fixtures tracked by ID for Janitor acceptance tests."""

from dataclasses import dataclass, field
from functools import partial
import time
from typing import Any, Callable

from robot.api import logger


_ABSENT = object()


class FixtureError(RuntimeError):
    """A fixture operation failed without exposing an SDK response body."""


def _status_code(error):
    status = getattr(error, "status_code", None)
    if status is None:
        status = getattr(error, "http_status", None)
    return status if isinstance(status, int) else None


def _error_summary(error):
    if isinstance(error, FixtureError):
        return str(error)
    status = _status_code(error)
    return type(error).__name__ + (f" (HTTP {status})" if status else "")


def cloud_call(action, function, *args, missing_ok=False, **kwargs):
    try:
        return function(*args, **kwargs)
    except Exception as error:
        if missing_ok and _status_code(error) == 404:
            return _ABSENT
        raise FixtureError(f"{action}: {_error_summary(error)}") from None


@dataclass(frozen=True)
class ResourceRef:
    kind: str
    id: str
    getter: Callable[[str], Any] = field(repr=False, compare=False)

    def get(self):
        """Fetch this exact ID. Only an HTTP 404 represents absence."""
        resource = cloud_call(
            f"Get {self.kind} {self.id}", self.getter, self.id, missing_ok=True
        )
        if resource is _ABSENT:
            return None
        if resource is None:
            raise FixtureError(f"Get {self.kind} {self.id} returned no resource without 404")
        return resource


def assert_resources(absent=(), present=()):
    """Check exact GET results before any teardown can remove the fixtures."""
    failures = []
    for ref in absent:
        if ref.get() is not None:
            failures.append(f"{ref.kind} {ref.id} still exists")
    for ref in present:
        if ref.get() is None:
            failures.append(f"{ref.kind} {ref.id} is missing")
    assert not failures, ", ".join(failures)


class OpenStackFixtures:
    def __init__(self, connection, name, external_network_id):
        if not name or not external_network_id:
            raise ValueError("Fixture name and external network ID are required")
        self.connection = connection
        self.name = name
        self.external_network_id = external_network_id
        self.timeout = 600
        self.interval = 2
        self._cleanup = []
        self._subnet = None
        self._load_balancers = []

    def _create(self, kind, create, get, delete=None, **attributes):
        resource = cloud_call(f"Create {kind}", create, **attributes)
        if not resource.id:
            raise FixtureError(f"Created {kind} has no ID")
        ref = ResourceRef(kind, resource.id, get)
        if delete is not None:
            self._cleanup.append(
                (f"Delete {kind} {ref.id}", partial(self._delete, ref, delete))
            )
        logger.info(f"Created OpenStack {kind} {ref.id}")
        return ref

    def _wait_status(self, ref, attribute, wanted, *, missing_ok=False):
        deadline = time.monotonic() + self.timeout
        while True:
            resource = ref.get()
            if resource is None:
                if missing_ok:
                    return None
                raise FixtureError(f"{ref.kind} {ref.id} disappeared while waiting")
            status = getattr(resource, attribute, None)
            if status in wanted:
                return resource
            if isinstance(status, str) and status.upper().startswith("ERROR"):
                raise FixtureError(f"{ref.kind} {ref.id} entered an error state")
            if time.monotonic() >= deadline:
                raise FixtureError(f"Timed out waiting for {ref.kind} {ref.id}")
            time.sleep(self.interval)

    def _wait_absent(self, ref):
        deadline = time.monotonic() + self.timeout
        while ref.get() is not None:
            if time.monotonic() >= deadline:
                raise FixtureError(f"Timed out deleting {ref.kind} {ref.id}")
            time.sleep(self.interval)

    def _delete(self, ref, delete):
        if ref.kind == "load_balancer":
            current = self._wait_status(
                ref, "provisioning_status", {"ACTIVE", "ERROR"}, missing_ok=True
            )
        else:
            current = ref.get()
        if current is None:
            return
        cloud_call(
            f"Delete {ref.kind} {ref.id}",
            delete, ref.id, ignore_missing=False, missing_ok=True,
        )
        self._wait_absent(ref)

    def _ensure_network(self):
        if self._subnet is not None:
            return
        network = self.connection.network
        net = self._create(
            "network", network.create_network, network.get_network,
            network.delete_network, name=f"{self.name}-network",
        )
        subnet = self._create(
            "subnet", network.create_subnet, network.get_subnet,
            network.delete_subnet, name=f"{self.name}-subnet", network_id=net.id,
            ip_version=4, cidr="192.0.2.0/24", is_dhcp_enabled=True,
        )
        router = self._create(
            "router", network.create_router, network.get_router,
            network.delete_router, name=f"{self.name}-router",
            external_gateway_info={"network_id": self.external_network_id},
        )
        cloud_call(
            "Add fixture router interface", network.add_interface_to_router,
            router.id, subnet.id,
        )
        self._cleanup.append((
            f"Remove interface from router {router.id}",
            partial(
                cloud_call, "Remove fixture router interface",
                network.remove_interface_from_router, router.id, subnet.id,
                missing_ok=True,
            ),
        ))
        self._subnet = subnet

    def create_load_balancer(self, tags):
        """Create an LB, listener, pool and VIP FIP for a deletion scenario."""
        self._ensure_network()
        service = f"web-{len(self._load_balancers) + 1}"
        octavia = self.connection.load_balancer
        lb = self._create(
            "load_balancer", octavia.create_load_balancer, octavia.get_load_balancer,
            partial(octavia.delete_load_balancer, cascade=True),
            name=f"kube_service_{self.name}_default_{service}",
            vip_subnet_id=self._subnet.id, tags=list(tags),
        )
        self._load_balancers.append(lb)
        self._wait_status(lb, "provisioning_status", {"ACTIVE"})
        listener = self._create(
            "listener", octavia.create_listener, octavia.get_listener,
            name=f"{self.name}-{service}-listener",
            load_balancer_id=lb.id, protocol="TCP", protocol_port=80,
        )
        self._wait_status(lb, "provisioning_status", {"ACTIVE"})
        pool = self._create(
            "pool", octavia.create_pool, octavia.get_pool,
            name=f"{self.name}-{service}-pool", listener_id=listener.id,
            protocol="TCP", lb_algorithm="ROUND_ROBIN",
        )
        active = self._wait_status(lb, "provisioning_status", {"ACTIVE"})
        if not active.vip_port_id:
            raise FixtureError(f"Load balancer {lb.id} has no VIP port")
        network = self.connection.network
        floating_ip = self._create(
            "floating_ip", network.create_ip, network.get_ip, network.delete_ip,
            floating_network_id=self.external_network_id, port_id=active.vip_port_id,
            description=(
                f"Floating IP for Kubernetes external service default/{service} "
                f"from cluster {self.name}"
            ),
        )
        return {
            "load_balancer": lb, "listener": listener, "pool": pool,
            "floating_ip": floating_ip,
        }

    def set_load_balancer_tags(self, lb, tags):
        if lb not in self._load_balancers:
            raise ValueError("Load balancer was not created by this fixture")
        self._wait_status(lb, "provisioning_status", {"ACTIVE"})
        cloud_call(
            f"Update tags on load balancer {lb.id}",
            self.connection.load_balancer.update_load_balancer, lb.id, tags=list(tags),
        )
        self._wait_status(lb, "provisioning_status", {"ACTIVE"})

    def create_storage(self):
        """Create an owned volume and snapshot, plus a volume marked to keep."""
        storage = self.connection.block_storage
        metadata = {"cinder.csi.openstack.org/cluster": self.name}
        volume = self._create(
            "volume", storage.create_volume, storage.get_volume, storage.delete_volume,
            name=f"{self.name}-volume", size=1, metadata=metadata,
        )
        self._wait_status(volume, "status", {"available"})
        snapshot = self._create(
            "snapshot", storage.create_snapshot, storage.get_snapshot,
            storage.delete_snapshot, name=f"{self.name}-snapshot",
            volume_id=volume.id, metadata=metadata,
        )
        self._wait_status(snapshot, "status", {"available"})
        kept_volume = self._create(
            "volume", storage.create_volume, storage.get_volume, storage.delete_volume,
            name=f"{self.name}-kept-volume", size=1,
            metadata={**metadata, "janitor.capi.azimuth-cloud.com/keep": "true"},
        )
        self._wait_status(kept_volume, "status", {"available"})
        return {"volume": volume, "snapshot": snapshot, "kept_volume": kept_volume}

    def create_security_group(self):
        network = self.connection.network
        return self._create(
            "security_group", network.create_security_group, network.get_security_group,
            network.delete_security_group, name=f"{self.name}-security-group",
            description=f"Security Group for Service LoadBalancer in cluster {self.name}",
        )

    def cleanup(self):
        """Delete only recorded IDs, continuing after failures and reporting all of them."""
        errors = []
        while self._cleanup:
            label, action = self._cleanup.pop()
            try:
                action()
            except Exception as error:
                errors.append(f"{label}: {_error_summary(error)}")
        if errors:
            raise FixtureError("Fixture cleanup failed: " + ", ".join(errors)) from None
