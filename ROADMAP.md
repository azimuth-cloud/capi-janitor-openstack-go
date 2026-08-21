# Go Rewrite

## Context

This repository is the Go replacement for the Python [`cluster-api-janitor-openstack`](https://github.com/azimuth-cloud/cluster-api-janitor-openstack) controller.
The compatibility baseline is Python `0.15.0` at `f14d860`; the replacement keeps its finalizer and deletion scope, fixes unsafe failure behavior, and adds project-scoped password authentication.

The rewrite follows TDD: the user stories below describe observable outcomes, and the implementation must provide named tests for them.

Scaffolding tool: **kubebuilder**.

Detailed decisions, selectors, observable outcomes, and regression evidence are recorded in the [compatibility policy](docs/design/python-compatibility-policy.md), [ownership matrix](docs/design/resource-ownership-matrix.md), [behavior matrix](docs/design/cleanup-behaviour-matrix.md), and [regression ledger](docs/design/python-regression-ledger.md).
A settled story is a target contract, not a claim that the active runtime implements it.
The user-visible cleanup choices remain the existing volume policy and direct application-credential deletion opt-in.

---

## Audit of the Existing Python Code

### Main Modules

| File | Role |
|---|---|
| `capi_janitor/openstack/openstack.py` | OpenStack client: authentication, service catalog, paginated REST resources |
| `capi_janitor/openstack/operator.py` | Operator logic: kopf handlers, resource filters, OpenStack purge |

### Covered Features

**OpenStack Authentication**
- Only `v3applicationcredential`
- X-Auth-Token management (refresh with asyncio mutex)
- Custom CA certificate support (cacert from K8s secret)
- Service catalog filtered by interface (public/internal/admin) and region

**Resource Filtering**
- Floating IPs: description `"Floating IP for Kubernetes external service … from cluster <name>"`
- Octavia Load Balancers: name `kube_service_<cluster>_*`
- Security Groups: description `"Security Group for Service LoadBalancer in cluster <name>"`
- Cinder Volumes: metadata `cinder.csi.openstack.org/cluster == <name>`, unless property `janitor.capi.azimuth-cloud.com/keep == true`
- Cinder Snapshots: same cluster metadata

**Deletion Policy**
- Volumes: configurable via env var `CAPI_JANITOR_DEFAULT_VOLUMES_POLICY` (default `delete`) and annotation `janitor.capi.stackhpc.com/volumes-policy` per cluster
- Application Credential: deleted if annotation `janitor.capi.stackhpc.com/credential-policy: delete` on the secret AND it is the last finalizer

**Kubernetes Lifecycle**
- Finalizer `janitor.capi.stackhpc.com` on `OpenStackCluster`
- Cluster name: label `cluster.x-k8s.io/cluster-name` takes priority, otherwise `metadata.name`
- Retry via random annotation `janitor.capi.stackhpc.com/retry` (triggers a new event)
- Configurable backoff `CAPI_JANITOR_RETRY_DEFAULT_DELAY` (default 60s)

**Error Handling**
- HTTP 400/409 during deletion: silent retry
- HTTP 404 during catalog fetch: authentication considered failed (no fatal error)
- HTTP 422 during finalizer patch: kopf `TemporaryError`
- Catalog error `volumev3` → fallback to `block-storage`

### Existing Tests

| File | What is tested |
|---|---|
| `test_openstack.py` | Successful auth, 404, missing interface, missing region, multiple services |
| `test_operator.py` | FIP/LB/SG/volume/snapshot filtering; `empty()`; `try_delete()`; event handler (add finalizer, skip, purge); auth error in purge |

**Notable gap**: `test_purge_openstack_resources_success` is commented out (mock complexity).

### Helm Chart

- `ClusterRole`: namespaces (list/watch), events (create), secrets (get/delete), openstackclusters (list/get/watch/patch), CRDs (list/get/watch)
- Value `defaultVolumesPolicy: delete`
- Image: `ghcr.io/azimuth-cloud/cluster-api-janitor-openstack`

### Pending PRs to Integrate

| PR | Title | Impact |
|---|---|---|
| #261 | Fix leaving Azimuth cluster loadbalancers behind | Remove the CAPO status gate; keep the exact workload selector and fail-closed inventory |

---

## Agile Roadmap

---

### Epic 1 — OpenStack Authentication

#### US1.1 — Authentication via Application Credential v3

```gherkin
Feature: OpenStack Authentication via Application Credential
  In order to access OpenStack APIs
  As an operator
  I want to authenticate using a v3 Application Credential

  Scenario: Successful authentication
    Given a clouds.yaml with auth_type "v3applicationcredential"
    And a valid application_credential_id and application_credential_secret
    When the operator initialises the OpenStack connection
    Then an X-Auth-Token is obtained from Keystone
    And the service catalog is loaded

  Scenario: Token refresh on expiry
    Given an expired X-Auth-Token
    When the operator makes an API call
    Then a new token is requested from Keystone
    And the original call is replayed with the new token

  Scenario: Authentication with unsupported type
    Given a clouds.yaml with token, federation, or another unsupported auth_type
    When the operator attempts to create a Cloud client
    Then an UnsupportedAuthenticationError is raised

  Scenario: Token response is not valid JSON
    Given Keystone returns HTTP 201 with a malformed JSON body
    When the operator requests a token
    Then an error is returned
```

#### US1.2 — Service Catalog Filtering by Interface and Region

```gherkin
Feature: OpenStack Service Catalog
  Scenario: Endpoint selected by configured interface
    Given a catalog with "public" and "internal" endpoints
    And the configured interface is "public"
    When the catalog is loaded
    Then only "public" endpoints are retained

  Scenario: Endpoint selected by configured region
    Given a catalog with endpoints for "RegionOne" and "RegionTwo"
    And the configured region is "RegionOne"
    When the catalog is loaded
    Then only "RegionOne" endpoints are retained

  Scenario: No region configured
    Given a catalog with endpoints in multiple regions
    And no region is configured
    When the catalog is loaded
    Then the first endpoint matching the interface is retained for each service

  Scenario: No interface configured
    Given a clouds.yaml entry without an "interface" value
    When the catalog is loaded
    Then the "public" interface is used by default

  Scenario: Catalog request fails with a non-404 error
    Given the catalog endpoint returns HTTP 500
    When the operator loads the catalog
    Then an error is returned

  Scenario: Catalog response is not valid JSON
    Given the catalog endpoint returns a malformed JSON body
    When the operator loads the catalog
    Then an error is returned

```

Region precedence and unusable selected endpoints are covered by the [identity and client behavior matrix](docs/design/cleanup-behaviour-matrix.md#identity-and-openstack-client-behavior).

#### US1.3 — Revoked or Invalid Credential Handling

```gherkin
Feature: Invalid OpenStack Credential
  Scenario: Authentication fails before resource verification
    Given the referenced credential cannot authenticate
    When cleanup starts
    Then cleanup is blocked
    And the credential Secret and finalizer remain

  Scenario: Authentication failure does not prove credential deletion
    Given authentication, catalog, or the Keystone base URL returns 401 or 404
    When no exact DELETE result for the bound application credential is known
    Then cleanup remains blocked
    And US7.3 controls any post-checkpoint recovery
```

#### US1.4 — Custom CA Certificate Support

```gherkin
Feature: Custom CA Certificate
  Scenario: CA provided in the Kubernetes secret
    Given a Kubernetes secret containing a "cacert" entry
    When the operator initialises the TLS transport
    Then the CA is loaded into the SSL context
    And HTTPS calls to OpenStack use this CA for verification

  Scenario: No CA provided
    Given a Kubernetes secret without a "cacert" entry
    When the operator initialises the TLS transport
    Then the system CA is used for TLS verification

  Scenario: CA certificate content is not valid PEM
    Given a Kubernetes secret with a "cacert" entry that is not valid PEM data
    When the operator initialises the TLS transport
    Then an error is returned
```

#### US1.5 — Authentication via Username/Password (v3password)

```gherkin
Feature: OpenStack Authentication via Username/Password
  Scenario: Supported password input
    Given auth_type is "v3password", "password", or omitted for a complete password credential
    And the user and project are identified unambiguously by ID or name and domain
    When authentication succeeds
    Then the token contains a non-empty project ID
    And an explicitly configured project ID matches it

  Scenario: Password scope is unsafe or ambiguous
    Given the input is unscoped, domain-scoped, system-scoped, or incomplete
    Or it mixes credential families or contains contradictory fields
    When the selected cloud is validated
    Then validation fails before any resource list request

  Scenario: Password Secret lifecycle
    Given the password Secret has credential-policy "delete"
    When resource cleanup completes
    Then no application credential delete is attempted
    And the password Secret is retained with a warning
```

The [compatibility policy](docs/design/python-compatibility-policy.md#approved-password-extension) defines the accepted ID, name, and domain combinations.

#### US1.6 — Identity Reference Resolution and Compatibility

```gherkin
Feature: CAPO Identity Reference Resolution
  Scenario: Direct Secret in the replacement profile
    Given identityRef.type is "Secret" or the admitted legacy empty value
    When the identity is resolved
    Then the same-namespace Secret named by identityRef.name is used
    And Secret.Data bytes are parsed without another base64 decode

  Scenario: Unsupported identity type in the replacement profile
    Given identityRef.type is "ClusterIdentity" or an unknown value
    When reconciliation starts in the Azimuth replacement profile
    Then no same-named Secret fallback occurs
    And cleanup is blocked before any OpenStack request
```

> The direct-Secret scenario is required for replacement.
> `OpenStackClusterIdentity` resolution remains behind a separate community gate and never makes the shared identity, Secret, or credential a deletion candidate.

---

### Epic 2 — Floating IP Cleanup

#### US2.1 — Identify Floating IPs of a Cluster

```gherkin
Feature: Identifying Floating IPs of a Cluster
  Scenario: FIP belonging to the cluster
    Given a list of OpenStack Floating IPs
    And a FIP with description "Floating IP for Kubernetes external service from cluster mycluster"
    When the FIPs of cluster "mycluster" are listed
    Then this FIP is included in the result

  Scenario: FIP from another cluster
    Given a FIP with description "Floating IP for Kubernetes external service from cluster othercluster"
    When the FIPs of cluster "mycluster" are listed
    Then this FIP is excluded from the result

  Scenario: FIP without a Kubernetes description
    Given a FIP with description "Some other description"
    When the FIPs of cluster "mycluster" are listed
    Then this FIP is excluded from the result

  Scenario: FIP with a wider or partial prefix
    Given a FIP with description "Floating IP for Kubernetes worker from cluster mycluster"
    When the FIPs of cluster "mycluster" are listed
    Then this FIP is excluded from the result
```

#### US2.2 — Delete Floating IPs

```gherkin
Feature: Floating IP Deletion
  Scenario: Successful deletion
    Given a FIP belonging to cluster "mycluster"
    When the FIP purge is triggered
    Then the FIP is deleted via the Neutron API
    And an INFO log is emitted

  Scenario: HTTP 400 error during deletion
    Given a FIP deletion returns HTTP 400
    When the purge attempts to delete the FIP
    Then a warning is emitted
    And deletion continues for other FIPs
    And the phase returns a waiting outcome for later verification

  Scenario: HTTP 500 error during deletion
    Given a FIP deletion returns HTTP 500
    When the purge attempts to delete the FIP
    Then an exception is propagated

  Scenario: FIP deletion remains pending
    Given a selected FIP still appears after OpenStack accepts deletion
    When the current cleanup iteration verifies the phase
    Then the controller returns RequeueAfter
    And no polling loop or sleep runs inside reconciliation

  Scenario: Floating IP already deleted
    Given a FIP deletion returns HTTP 404
    When the purge attempts to delete the FIP
    Then the deletion is treated as successful, not as an error
```

> Deletion verification for FIPs, LBs, security groups, volumes, and snapshots is level based.
> One bounded iteration observes or mutates one dependency phase.
> Resources that remain are observed again through `RequeueAfter`; the controller does not poll or sleep inside reconciliation.

#### US2.3 — Defensive Pagination Parsing

```gherkin
Feature: Paginated Listing Robustness
  In order to avoid authorizing deletion from incomplete inventory
  As an operator
  I want paginated list requests to degrade gracefully on malformed data

  Scenario: Malformed top-level JSON in a list page
    Given an OpenStack list endpoint returns a body that is not valid JSON
    When the page is parsed
    Then an error is returned

  Scenario: Pagination links field absent
    Given a list response without a "<resource>_links" field
    When the next page URL is resolved
    Then pagination stops after the current page (no error)

  Scenario: Pagination links field malformed
    Given a "<resource>_links" field that is not an array of link objects
    When the next page URL is resolved
    Then an error is returned
    And no deletion is authorized from the partial inventory

  Scenario: No "next" relation present
    Given a "<resource>_links" array without a "next" entry
    When the next page URL is resolved
    Then pagination stops after the current page (no error)

```

> `nextPageURL` and `listPages` are shared by FIPs, load balancers and security groups; the scenarios above were validated against the FIP listing endpoint but apply identically to the other two.
> A later-page failure discards the inventory as defined by [error handling](docs/design/cleanup-behaviour-matrix.md#error-handling).

---

### Epic 3 — Octavia Load Balancer Cleanup

#### US3.1 — Identify Kubernetes Load Balancers of a Cluster

```gherkin
Feature: Identifying Kubernetes Load Balancers
  Scenario: LB belonging to the cluster
    Given an LB with name "kube_service_mycluster_api"
    When the LBs of cluster "mycluster" are listed
    Then this LB is included in the result

  Scenario: LB from another cluster
    Given an LB with name "kube_service_othercluster_api"
    When the LBs of cluster "mycluster" are listed
    Then this LB is excluded from the result

  Scenario: LB without kube_service prefix
    Given an LB with name "fake_service_mycluster_api"
    When the LBs of cluster "mycluster" are listed
    Then this LB is excluded from the result

```

#### US3.2 — Identify Azimuth Load Balancers (PR #261)

```gherkin
Feature: Identifying OCCM Workload Load Balancers
  Scenario: Cluster does not use a CAPO API server load balancer
    Given spec.apiServerLoadBalancer.enabled is false
    And an LB is named "kube_service_mycluster_default_web"
    When the LBs of cluster "mycluster" are listed
    Then this workload LB is included in the result

  Scenario: CAPO status ID is empty
    Given status.apiServerLoadBalancer.id is empty
    And an LB is named "kube_service_mycluster_default_web"
    When the LBs of cluster "mycluster" are listed
    Then this workload LB is included in the result
```

> The CAPO API server load balancer fields are not an ownership signal for OCCM workload load balancers.
> US3.4 defines the fail-closed inventory behavior.

#### US3.3 — Delete Load Balancers with Cascade

```gherkin
Feature: Cascaded Load Balancer Deletion
  Scenario: Successful deletion with cascade
    Given an LB belonging to cluster "mycluster"
    When the LB purge is triggered
    Then the LB is deleted with the cascade=true parameter
    And associated Octavia resources (listeners, pools, members) are deleted
```

#### US3.4 — Protect Shared Load Balancers and Handle Octavia Gaps

```gherkin
Feature: Shared Load Balancer and Octavia Safety
  Scenario: A reserved tag is foreign or malformed
    Given an LB passes the Python name selector
    And a kube_service_ tag belongs to another cluster or is malformed
    When ownership is evaluated
    Then the LB and its attached VIP floating IP are preserved
    And unrelated owned resources may continue

  Scenario: Ownership facts are incomplete
    Given an LB tag, VIP port ID, FIP port ID, or inventory page is unknown
    When cleanup runs
    Then no OpenStack mutation is made

  Scenario: The catalog has no load-balancer service
    Given no FIP matches, or every matching FIP association is known and unbound
    When the Octavia phase is evaluated
    Then the LB phase is NotApplicable and cleanup may continue
    And a warning and metric report that Octavia was unavailable

  Scenario: Octavia cannot prove the boundary
    Given an absent service has a bound or unknown matching FIP
    Or an existing service has no selected endpoint or complete inventory
    When the Octavia phase is evaluated
    Then cleanup is blocked and the finalizer remains
```

Tag vetoes, FIP association states, and the complete preflight are defined in the [shared load balancer ownership rules](docs/design/resource-ownership-matrix.md#shared-load-balancer-floating-ip).

---

### Epic 4 — Security Group Cleanup

#### US4.1 — Identify Security Groups of a Cluster

```gherkin
Feature: Identifying Security Groups of a Cluster
  Scenario: SG belonging to the cluster
    Given an SG with description "Security Group for Service LoadBalancer in cluster mycluster"
    When the SGs of cluster "mycluster" are listed
    Then this SG is included in the result

  Scenario: SG from another cluster
    Given an SG with description "Security Group for Service LoadBalancer in cluster othercluster"
    When the SGs of cluster "mycluster" are listed
    Then this SG is excluded from the result
```

#### US4.2 — Delete Security Groups

```gherkin
Feature: Security Group Deletion
  Scenario: Successful deletion
    Given an SG belonging to cluster "mycluster"
    When the SG purge is triggered
    Then the SG is deleted via the Neutron API

  Scenario: SG still in use (HTTP 409)
    Given an SG deletion returns HTTP 409
    When the purge attempts to delete the SG
    Then a warning is emitted
    And the phase returns a waiting outcome for later verification
    And no in-process polling occurs
```

---

### Epic 5 — Cinder Volume Management

#### US5.1 — Identify Volumes of a Cluster

```gherkin
Feature: Identifying Cinder Volumes of a Cluster
  Scenario: Volume belonging to the cluster without keep flag
    Given a volume with metadata "cinder.csi.openstack.org/cluster" = "mycluster"
    And the property "janitor.capi.azimuth-cloud.com/keep" is absent or != "true"
    When the volumes of cluster "mycluster" are listed
    Then this volume is included in the result

  Scenario: Volume flagged keep by the user
    Given a volume with metadata "cinder.csi.openstack.org/cluster" = "mycluster"
    And the property "janitor.capi.azimuth-cloud.com/keep" = "true"
    When the volumes of cluster "mycluster" are listed
    Then this volume is excluded from the result

  Scenario: Volume from another cluster
    Given a volume with metadata "cinder.csi.openstack.org/cluster" = "othercluster"
    When the volumes of cluster "mycluster" are listed
    Then this volume is excluded from the result

  Scenario: Volume without CSI metadata
    Given a volume without metadata "cinder.csi.openstack.org/cluster"
    When the volumes of cluster "mycluster" are listed
    Then this volume is excluded from the result
```

#### US5.2 — Volume Deletion Policy

```gherkin
Feature: Volume Deletion Policy
  Scenario: Global policy "delete" (default)
    Given the environment variable CAPI_JANITOR_DEFAULT_VOLUMES_POLICY is not set
    When a cluster is deleted without a volumes annotation
    Then the cluster's volumes are deleted

  Scenario: Global policy "keep"
    Given CAPI_JANITOR_DEFAULT_VOLUMES_POLICY = "keep"
    When a cluster is deleted without a volumes annotation
    Then the cluster's volumes are kept

  Scenario: Annotation "delete" on the cluster (overrides global keep)
    Given CAPI_JANITOR_DEFAULT_VOLUMES_POLICY = "keep"
    And the annotation "janitor.capi.stackhpc.com/volumes-policy" = "delete" on the OpenStackCluster
    When the cluster is deleted
    Then the cluster's volumes are deleted

  Scenario: Annotation "keep" on the cluster (overrides global delete)
    Given CAPI_JANITOR_DEFAULT_VOLUMES_POLICY = "delete"
    And the annotation "janitor.capi.stackhpc.com/volumes-policy" = "keep" on the OpenStackCluster
    When the cluster is deleted
    Then the cluster's volumes are kept
```

#### US5.3 — Defensive Handling of Malformed Cinder Responses

```gherkin
Feature: Cinder Response Robustness
  Scenario: Top-level volume/snapshot list response is not valid JSON
    Given the Cinder API returns a malformed JSON body for a list request
    When the volumes or snapshots of a cluster are listed
    Then an error is returned

  Scenario: "volumes"/"snapshots" key is not an array
    Given the Cinder API returns a list response where the items key is not an array
    When the volumes or snapshots of a cluster are listed
    Then an error is returned
```

---

### Epic 6 — Cinder Snapshot Management

#### US6.1 — Identify and Delete Snapshots of a Cluster

```gherkin
Feature: Cinder Snapshots of a Cluster
  Scenario: Snapshot belonging to the cluster
    Given a snapshot with metadata "cinder.csi.openstack.org/cluster" = "mycluster"
    When the snapshots of cluster "mycluster" are listed
    Then this snapshot is included in the result

  Scenario: Snapshot from another cluster
    Given a snapshot with metadata "cinder.csi.openstack.org/cluster" = "othercluster"
    When the snapshots of cluster "mycluster" are listed
    Then this snapshot is excluded from the result

  Scenario: Snapshots deleted before volumes
    Given snapshots and volumes belonging to cluster "mycluster"
    When the purge is triggered with include_volumes = true
    Then snapshots are deleted first
    And volumes are deleted afterwards
```

---

### Epic 7 — Application Credential Management

#### US7.1 — Delete the OpenStack Application Credential

```gherkin
Feature: Application Credential Deletion
  Scenario: Deletion authorised (last finalizer)
    Given the annotation "janitor.capi.stackhpc.com/credential-policy" = "delete" on the secret
    And the operator's finalizer is the only finalizer present
    And restart-safe resource verification is complete
    When the credential transition reaches its exact delete phase
    Then the Application Credential is deleted via the Identity API
    And only a confirmed deleted or absent result can authorize Secret deletion

  Scenario: Other finalizers still present
    Given the annotation "credential-policy" = "delete" on the secret
    And other finalizers are still present on the OpenStackCluster
    When the purge is complete
    Then the credential secret is not deleted
    And the janitor finalizer is not removed
    And RequeueAfter requests a later reconcile

  Scenario: Application Credential cannot be deleted (403)
    Given the Application Credential is restricted (no unrestricted flag)
    When the appcred deletion is attempted
    Then credentialFinalized records retainedForbidden
    And a warning is emitted that the credential may remain
    And the Kubernetes secret is retained
    And cluster finalization may continue

  Scenario: clouds.yaml cannot be parsed while resolving the credential ID
    Given a malformed clouds.yaml
    When the application credential ID is extracted for deletion
    Then an error is returned
```

Exact responses, selected-cloud credential resolution, and post-start authentication outcomes are defined in the [credential cleanup matrix](docs/design/cleanup-behaviour-matrix.md#application-credential-and-secret-cleanup).

#### US7.2 — Purge Orchestration Across Resource Types

```gherkin
Feature: OpenStack Resource Purge Orchestration
  In order to guarantee a consistent, predictable cleanup sequence
  As an operator
  I want bounded cleanup phases in a fixed dependency order

  Scenario: Authentication fails
    Given an invalid clouds.yaml
    When the purge is triggered
    Then the authentication error is returned immediately and no resources are touched

  Scenario: Floating IP deletion fails
    Given the Floating IP listing or deletion fails
    When the purge is triggered
    Then the error is returned immediately
    And load balancers, security groups, volumes and the application credential are not touched

  Scenario: Volume deletion fails
    Given include_volumes is true and volume listing or deletion fails
    When the purge is triggered
    Then the error is returned immediately
    And the application credential is not deleted, even if include_appcred is true

  Scenario: Volumes policy disabled
    Given include_volumes is false
    When the purge is triggered
    Then snapshots and volumes are not listed or deleted

  Scenario: One dependency phase per iteration
    Given multiple resource kinds remain
    When the cleanup runner executes once
    Then it processes only the earliest required dependency phase
    And later phases wait for a future reconcile
```

> The [cleanup progression](docs/design/go-rewrite-guidelines.md#cleanup-progression) defines preflight, dependency order, optional clients, and the typed-runner cutover gate.

#### US7.3 — Restart-Safe Credential and Secret Cleanup

```gherkin
Feature: Persisted Credential Cleanup State
  Scenario: Cleanup crosses a destructive boundary
    Given the Janitor is the only finalizer
    And fresh complete inventory contains no owned resource
    When direct application-credential deletion is enabled
    Then reconciliation records resourcesVerified, credentialDeleteStarted, credentialFinalized, and secretDeleteStarted in order
    And each external delete runs only after its write-ahead phase

  Scenario: Resource verification remains fresh
    Given resourcesVerified is persisted
    When the next reconcile runs or a valid same-boundary identity or policy changes
    Then it repeats the complete inventory before credentialDeleteStarted
    And a new candidate invalidates the pre-start checkpoint

  Scenario: State cannot be replayed safely
    Given the checkpoint is malformed, crosses an ownership boundary, or changed after credentialDeleteStarted
    When reconciliation resumes
    Then cleanup fails closed and requires the documented recovery path

  Scenario: The manager stops during the transition
    Given the manager stops after a checkpoint patch or external request
    When the same object reconciles after restart
    Then the recorded phase resumes
    And the Secret and finalizer are not removed early
```

US7.1 lists the external outcomes; the [compatibility policy](docs/design/python-compatibility-policy.md#restart-checkpoint) defines the checkpoint binding and transition sequence.

---

### Epic 8 — Kubernetes Lifecycle (Finalizer Pattern)

#### US8.1 — Add a Finalizer on Creation

```gherkin
Feature: Adding the Janitor Finalizer to OpenStackCluster
  Scenario: Cluster without deletionTimestamp and without janitor finalizer
    Given an OpenStackCluster without deletionTimestamp
    And without finalizer "janitor.capi.stackhpc.com"
    When an event is received for this cluster
    Then the finalizer "janitor.capi.stackhpc.com" is added via a conflict-safe metadata patch
    And an INFO log confirms the addition

  Scenario: Cluster with finalizer already present
    Given an OpenStackCluster without deletionTimestamp
    And with the finalizer "janitor.capi.stackhpc.com" already present
    When an event is received
    Then no patch is made
```

#### US8.2 — Cluster Name from Label or metadata.name

```gherkin
Feature: Cluster Name Resolution
  Scenario: Label cluster.x-k8s.io/cluster-name present
    Given an OpenStackCluster with label "cluster.x-k8s.io/cluster-name" = "myapp"
    And metadata.name = "myapp-openstack"
    When the operator resolves the cluster name for cleanup
    Then the name "myapp" is used

  Scenario: Label absent
    Given an OpenStackCluster without label "cluster.x-k8s.io/cluster-name"
    And metadata.name = "mycluster"
    When the operator resolves the cluster name
    Then the name "mycluster" is used

```

#### US8.3 — Remove the Finalizer after Successful Cleanup

```gherkin
Feature: Finalizer Removal after Purge
  Scenario: Successful purge
    Given an OpenStackCluster being deleted
    And all OpenStack resources have been deleted
    And every required credential and Secret phase is complete
    When the purge completes without error
    Then the finalizer "janitor.capi.stackhpc.com" is removed via a conflict-safe metadata patch
    And an INFO log confirms the finalizer removal

  Scenario: Finalizer absent at removal time
    Given an OpenStackCluster with deletionTimestamp
    And without the finalizer "janitor.capi.stackhpc.com"
    When an event is received
    Then no purge is triggered
    And an INFO log indicates the finalizer is absent
```

#### US8.4 — Retry Mechanism

```gherkin
Feature: Bounded Reconciliation Retry
  Scenario: Expected OpenStack wait
    Given deletion is accepted but the selected resource remains
    When the cleanup outcome is Waiting
    Then Reconcile returns RequeueAfter, normally five seconds
    And no sleep or retry annotation is used

  Scenario: Operational cleanup error
    Given cleanup returns an operational error
    When Reconcile returns the error
    Then the per-object workqueue backoff starts at one second
    And it is capped by CAPI_JANITOR_RETRY_DEFAULT_DELAY, default 60 seconds

  Scenario: Reconciliation later succeeds
    Given an object has accumulated error backoff
    When reconciliation succeeds
    Then that object's error backoff is reset
```

#### US8.5 — Robust Reconcile Error Handling

```gherkin
Feature: Reconcile Resilience to Kubernetes API Errors
  In order to avoid silently losing track of clusters
  As an operator
  I want Kubernetes API errors during reconciliation to be surfaced or handled explicitly

  Scenario: Fetching the OpenStackCluster fails with a non-NotFound error
    Given the Kubernetes API returns an error other than NotFound when fetching the cluster
    When Reconcile runs
    Then the error is propagated

  Scenario: Adding the finalizer fails
    Given the metadata patch to the OpenStackCluster fails
    When Reconcile attempts to add the janitor finalizer
    Then the error is propagated, wrapped as "adding finalizer"

  Scenario: Fetching the identity secret fails with a non-NotFound error
    Given the Kubernetes API returns an error other than NotFound when fetching the credential secret
    When Reconcile runs during deletion
    Then the error is propagated, wrapped as "fetching identity secret"

  Scenario: Identity secret does not exist
    Given the secret referenced by spec.identityRef does not exist
    And secretDeleteStarted has not been recorded
    When Reconcile runs during deletion
    Then cleanup is reported as blocked
    And the finalizer remains
    And a Secret watch or requeue can recover when the Secret is restored

  Scenario: CloudName not specified
    Given spec.identityRef.cloudName is empty
    When Reconcile runs during deletion
    Then the cloud name "openstack" is used only for an already-admitted Python migration object

  Scenario: Deleting the credential secret fails with a non-NotFound error
    Given credential-policy is "delete", this is the last finalizer, and the secret deletion fails
    When Reconcile completes a successful purge
    Then the error is propagated, wrapped as "deleting credential secret"

  Scenario: Removing the finalizer fails
    Given a successful purge and no pending credential-policy deletion
    When Reconcile attempts to remove the janitor finalizer
    Then the error is propagated, wrapped as "removing finalizer"

  Scenario: Concurrent metadata changes
    Given another controller changes labels, annotations, or finalizers
    When the Janitor patches its own metadata
    Then unrelated changes are preserved
    And a conflict is retried from a fresh object
```

#### US8.6 — Pause, Secondary Watches, and Controller Wiring

```gherkin
Feature: Controller Runtime Safeguards
  Scenario: Reconciliation is paused
    Given the CAPI Cluster or OpenStackCluster is paused
    When reconciliation runs
    Then no OpenStack request is made and the finalizer remains

  Scenario: A referenced object changes
    Given the identity Secret or owning Cluster changes
    When the secondary watch maps the event
    Then the related OpenStackCluster is reconciled
```

Indexes, watches, RBAC, leader election, and envtest evidence are defined in the [lifecycle matrix](docs/design/cleanup-behaviour-matrix.md#lifecycle-and-kubernetes-behavior).

---

### Epic 9 — Operator Configuration

#### US9.1 — Configuration via Environment Variables

```gherkin
Feature: Configuration via Environment Variables
  Scenario: Default volumes policy configured
    Given CAPI_JANITOR_DEFAULT_VOLUMES_POLICY = "keep"
    When the operator starts
    Then the default policy for all clusters is "keep"

  Scenario: Configurable retry delay
    Given CAPI_JANITOR_RETRY_DEFAULT_DELAY = "120"
    When repeated operational errors occur
    Then per-object backoff starts at one second and never exceeds 120 seconds

  Scenario: Invalid retry delay
    Given CAPI_JANITOR_RETRY_DEFAULT_DELAY is malformed, zero, negative, or overflows
    When the manager starts
    Then startup fails with a configuration error
```

---

### Epic 10 — Packaging and Deployment

#### US10.1 — Secure Image (Nix Build)

```gherkin
Feature: Secure OCI Image for the Go Operator
  Scenario: Reproducible Nix build
    Given the Go operator source code
    And nixpkgs is pinned to an immutable revision and hash
    When `nix-build nix -A image` is run
    Then the manager binary is built with buildGoModule and CGO_ENABLED=0
    And the image contains only the Nix closure required to run the binary

  Scenario: Image security
    Given the built image
    Then the process runs as non-root (UID 65532)
    And the root filesystem is read-only
    And all Linux capabilities are dropped
```

#### US10.2 — Helm Chart

```gherkin
Feature: Deployment via Helm Chart
  Scenario: Installation with default values
    Given the cluster-api-janitor-openstack Helm chart
    When helm install is executed
    Then a Deployment, ServiceAccount, ClusterRole, and ClusterRoleBinding are created
    And the default volumes policy is "delete"
    And the default retry delay is 60 seconds

  Scenario: Override volumes policy
    Given helm install with --set defaultVolumesPolicy=keep
    When the chart is deployed
    Then the variable CAPI_JANITOR_DEFAULT_VOLUMES_POLICY = "keep" is injected into the pod

  Scenario: Health probes active
    Given the deployed Deployment
    Then a livenessProbe on /healthz:8081 is configured
    And a readinessProbe on /readyz:8081 is configured

  Scenario: Complete RBAC
    Given the deployed ClusterRole
    Then OpenStackCluster metadata can be patched
    And referenced Secrets and CAPI Clusters can be listed and watched
    And Events can be created and patched

  Scenario: Single active reconciler
    Given the chart is installed with more than one replica
    Then leader election is enabled

```

#### US10.3 — OCI Build via Nix (without Flake) and SBOM

```gherkin
Feature: Reproducible OCI Build via Nix and SBOM Generation
  Scenario: Build the amd64 image with Nix
    Given the nix/default.nix file
    When nix-build nix -A image is executed
    Then an amd64 OCI image is produced (dockerTools.buildLayeredImage)
    And the image runs as User 65532:65532

  Scenario: Build the arm64 image by cross-compilation
    Given the nix/default.nix file
    When nix-build nix -A image-arm64 is executed
    Then an arm64 OCI image is produced via pkgsCross.aarch64-multiplatform
    And both images are combined into a multi-arch manifest via skopeo + docker manifest

  Scenario: CycloneDX SBOM generation
    Given the compiled Go binary
    When nix-build nix -A sbom is executed
    Then an sbom.cdx.json file in CycloneDX format is produced
    And it lists all Go modules (extracted from the buildinfo embedded in the binary)
    And it is retained as a release artifact with the source revision
```

#### US10.4 — Release Artifacts and Publication Safety

```gherkin
Feature: Verified Replacement Release
  Scenario: Pull request and release candidate validation
    Given source, dependency, packaging, or release configuration changes
    When CI builds without publishing
    Then GoReleaser, Nix, Helm, Kustomize, SBOM, checksum, and version checks pass

  Scenario: A tag is published
    Given the exact candidate commit passed all release gates
    When publication runs
    Then public artifacts are traceable to that commit
    And a published tag or asset cannot be replaced by a rerun
```

Artifact responsibilities are documented in [Releasing and versioning](docs/releasing.md#what-builds-each-artifact).
Upgrade the CAPO dependency from `v0.14.6` to the `v0.14.7` Azimuth replacement lane before collecting release evidence; the [compatibility policy](docs/design/python-compatibility-policy.md#supported-capo-lanes) defines the other supported and preview lanes.

#### US10.5 — Migration, Recovery, and Representative Validation

```gherkin
Feature: Python-to-Go Replacement Validation
  Scenario: Controller deployment changes
    Given the object is a CAPO v1beta1 OpenStackCluster using a direct Secret
    And the Python controller is stopped before the Go controller starts
    When an eligible object moves to Go
    Then the legacy finalizer is recognized and cleanup converges

  Scenario: Standard deployment rollback
    Given no managed object is actively deleting
    And every managed object uses a Python-compatible direct application-credential binding
    When the deployment rolls back to Python
    Then no in-flight checkpoint is handed to the older controller

  Scenario: A deleting object needs recovery
    Given its state is not eligible for audited handback before credentialDeleteStarted
    When recovery begins
    Then Go completes forward or the documented break-glass audit is used

  Scenario: A deleting object is eligible for handback
    Given the compatibility audit passes before credentialDeleteStarted
    When the runbook hands the object back to Python
    Then the representative rollback test converges within Python's known risk envelope

  Scenario: Real OpenStack exercises the ownership boundary
    Given owned and near-match fixtures share a test project
    When cleanup runs
    Then owned resources are deleted and protected fixtures remain
```

Required evidence is listed in the [replacement acceptance criteria](docs/design/go-rewrite-guidelines.md#acceptance-criteria-for-the-replacement-release).

---

### Epic 11 — Observability

#### US11.1 — Prometheus Metrics

```gherkin
Feature: Prometheus Metrics
  Scenario: Successful purge → success counter incremented
    Given a cluster being deleted
    When the OpenStack purge succeeds
    Then capi_janitor_cleanups_total{result="success"} is incremented by 1

  Scenario: Failed purge → failure counter incremented
    Given a cluster being deleted
    When the OpenStack purge fails
    Then capi_janitor_cleanups_total{result="failure"} is incremented by 1
```

> Partially implemented: `CounterVec` is registered through `ctrlmetrics.Registry`, and `Metrics *Metrics` is injectable on the reconciler for tests.
> Metric exposure in packaged deployments and blocked-outcome evidence remain open.

#### US11.2 — Kubernetes Events

```gherkin
Feature: Kubernetes Events on OpenStackCluster
  Scenario: Successful purge → Normal "CleanupSucceeded" event
    Given a cluster being deleted
    When the OpenStack purge succeeds
    Then a Normal event with reason "CleanupSucceeded" is emitted on the OpenStackCluster

  Scenario: Failed purge → Warning "CleanupFailed" event
    Given a cluster being deleted
    When the OpenStack purge fails
    Then a Warning event with reason "CleanupFailed" and the error message is emitted
```

> Implemented: `record.EventRecorder` is injectable on the reconciler and initialized from the manager when nil.
> Blocked and preserved outcomes require a specific Warning reason.

---

### Epic 12 — Robustness

#### US12.1 — HTTP Client Timeout

```gherkin
Feature: HTTP Timeout on the OpenStack Client
  Scenario: Context cancelled before the call
    Given an already-cancelled context
    When Authenticate is called
    Then an error is returned immediately

  Scenario: Safety net on the http.Client
    Given no context with a deadline provided by the caller
    Then the http.Client has a Timeout of 60 seconds
    (prevents calls from blocking indefinitely when OpenStack is unreachable)

```

Keystone path-prefix coverage is tracked in the [regression ledger](docs/design/python-regression-ledger.md).

#### US12.2 — Cinder Service Legacy Aliases

```gherkin
Feature: Cinder Service Detection with Aliases
  Scenario: Catalog with "volumev3" (standard >= Stein)
    Given an OpenStack catalog with service type "volumev3"
    When the operator looks up the Cinder client
    Then the "volumev3" client is used

  Scenario: Catalog with "block-storage" only
    Given an OpenStack catalog without "volumev3" but with "block-storage"
    When the operator looks up the Cinder client
    Then the "block-storage" client is used

  Scenario: Catalog without a Cinder service
    Given a catalog without "volumev3" or "block-storage"
    When the operator looks up the Cinder client
    Then a CatalogError is raised with the appropriate message
```

#### US12.3 — Transient Error Classification

```gherkin
Feature: Distinguishing Transient from Fatal Deletion Errors
  Scenario: HTTP 400 or 409 represents a pending delete
    Given deletion of an already-selected resource returns HTTP 400 or 409
    When the operator classifies the response
    Then the phase waits and verifies the selected resource again later

  Scenario: Exact selected-resource DELETE returns 404
    Given a resource passed its ownership selector
    And its exact DELETE returns HTTP 404
    When the operator classifies the response
    Then the delete is idempotently complete

  Scenario: Inventory, endpoint, or base URL returns 404
    Given a 404 is not the exact DELETE result for an already-selected resource
    When the operator classifies the response
    Then cleanup remains blocked

  Scenario: Operational or transport failure
    Given a request fails through DNS, TLS, timeout, network, 429, or 5xx
    When the operator classifies the error
    Then the error is returned for workqueue retry
    And the finalizer remains
```

#### US12.4 — HTTP Response Body Read Failures

```gherkin
Feature: Robustness to Interrupted HTTP Responses
  Scenario: Response body cannot be fully read
    Given an OpenStack API call returns a successful status code
    And the response body fails while being read (e.g. connection interrupted mid-transfer)
    When the operator processes the response
    Then an error is returned
```

---

## Actions

### Initial Go Rewrite (Historical Record)

This checked list records the initial implementation pass, not replacement readiness.

1. [x] Audit the existing Python code
2. [x] Write the original Epic and User Story roadmap
3. [x] Scaffold the Go project with kubebuilder
4. [x] Write the initial Go unit tests
5. [x] Implement the initial manual HTTP cleanup path
6. [x] Migrate the Helm chart for the Go image
7. [x] Add the first metrics and Kubernetes Events
8. [x] Add the Nix OCI build and CycloneDX SBOM target
9. [x] Raise coverage for the original four-package implementation

### Replacement Release Work

1. [x] Settle the Python `0.15.0` compatibility policy, ownership matrix, behavior matrix, and regression ledger
2. [ ] Build and connect the non-credential typed Gophercloud runner after US2.1–US7.2 and US12.1–US12.4 pass
3. [ ] Make reconciliation bounded and level based, with conflict-safe patches, secondary watches, pause, and leader election under US8.1–US9.1
4. [ ] Implement and failure-inject US7.3, connect the Keystone transition, cut production over, and remove the legacy manual HTTP resource path
5. [ ] Pass envtest, Kind, real OpenStack ownership, migration, handback, and break-glass scenarios in US8.6 and US10.5
6. [ ] Close release publication, Helm/Kustomize parity, immutable input, provenance, RC, and soak work in US10.1–US10.4

## Final Result

An initial Go implementation exists, but it does not yet meet the safe replacement release criteria.
This snapshot was reviewed at `main@02e3491` on 20 August 2026.

| Layer | Current state |
|---|---|
| OpenStack client | Gophercloud authentication and typed Neutron, Octavia, Cinder, and Keystone adapters exist; target validation remains incomplete |
| Active cleanup path | `purge.go` still calls legacy `Session` methods and manual HTTP models |
| Controller | Core scaffolding exists; level-based reconcile, watches, safe patches, and checkpoint remain |
| Packaging | Nix, SBOM, Helm, Kustomize, and GoReleaser exist; cross-artifact gates remain |
| End-to-end evidence | Kind has an image-reference gap; real ownership and migration tests remain |
| Replacement readiness | Not ready; open work is tracked in Actions and the [acceptance criteria](docs/design/go-rewrite-guidelines.md#acceptance-criteria-for-the-replacement-release) |

### Test Coverage (`internal/`)

| Snapshot | Packages | Top-level tests | Aggregate statement coverage |
|---|---:|---:|---:|
| Initial rewrite review | 4 | 168 | 93.0% |
| `main@02e3491` review | 9 | 181 | 84.4% |

`go test ./...` and `go vet ./...` pass.
The snapshots are not directly comparable because the later tree contains more packages and typed adapters.
Neither percentage is a release gate; required evidence is tied to the User Stories and regression ledger.

## Implementation Order

```text
Typed replacement data plane (required stories in Epics 1–7 and 12)
  → bounded controller (Epic 8, US9.1)
  → checkpoint and production cutover (US7.3)
  → boundary, migration, and recovery evidence (US8.6, US10.5)
  → release acceptance (Epic 10, Epic 11)
  → deferred community identity and CAPO v0.15 preview validation
```

## Deferred Work

The replacement adds no cleanup target or authentication family beyond application credentials and the approved password path.
It keeps the Python all-namespace watch and uses a deployment-level controller handoff during migration.
`OpenStackClusterIdentity`, `v1beta2`, new cleanup settings, and a watch-filter setting remain deferred; every extension follows [When the scope can grow](docs/design/go-rewrite-guidelines.md#when-the-scope-can-grow).

## Definition of Replacement Complete

The Python controller can be deprecated only after replacement work items 2–6 and the [acceptance criteria](docs/design/go-rewrite-guidelines.md#acceptance-criteria-for-the-replacement-release) are complete.
Deferred community identity and preview `v1beta2` lanes require their own gates.
