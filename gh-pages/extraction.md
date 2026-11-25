# Stage 1 — Read-Only Extraction (`facts.json` v3)

Stage 1 (`gcp-arch extract`) transforms raw, noisy, multi-project Google Cloud Platform inventories into a compact, deterministic, credential-scrubbed architectural fact graph (`facts.json`, schema `v3`).

Implemented in `internal/extract/`, Stage 1 runs strictly with `cloud-platform.read-only` credentials, collapses thousands of ephemeral plumbing resources (historical Cloud Build runs, Docker images, Kubernetes Pods/ReplicaSets, Load Balancer intermediates) into clean parent metrics and direct architectural edges, enriches structural topology with 7-day runtime telemetry at zero SQL slot cost, and writes atomically with `0600` permissions.

---

## 8-Phase Execution Pipeline

`extract.Extract` (`internal/extract/cai.go:314-354`) executes eight deterministic phases in strict order so that every downstream resolver operates over canonical project IDs, complete network indices, and deduplicated assets:

```mermaid
flowchart TD
    P0["Initialization: newExtractState (scopes, --project-alias, --cloudbuild-runs)"] --> P1["Phase 1: ReadTime-Pinned CAI Streaming (RESOURCE + 20-type IAM_POLICY)"]
    P1 --> P2["Phase 2: Post-CAI Reconciliation (re-canonicalize with full projIdx, dedup, fold counts, active Run/AE revisions)"]
    P2 --> P3["Phase 3: GKE Node Suppression and Network / Private IP / Cloud Run Host Indexing"]
    P3 --> P4["Phase 4: GKE Resolution (ConfigMaps, Workload Identity, Service->Workload, Ingress, LB->NEG/L4)"]
    P4 --> P5["Phase 5: Cloud Build Resolution (BuildTrigger GitHub/GitLab repos, inline steps, min-heap top-N builds)"]
    P5 --> P6["Phase 6: Structural, Absorbed Bridge, Env/Secret, and IAM Binding Resolution"]
    P6 --> P7["Phase 7: Concurrent 7-Day Enrichers (BigQuery, Dataflow, Cloud Monitoring; sequential deterministic apply)"]
    P7 --> P8["Phase 8: Default Sandbox Pruning, Canonical Sorting, and Atomic 0600 Write (facts.json v3)"]
```

---

## Phase 1: `ReadTime`-Pinned CAI Streaming, Rate Limiters & Retries

### Point-in-Time Snapshot Pinning

To eliminate mid-scan drift across paginated multi-project inventories:

1. `collectCAIResources` streams `AssetService.ListAssets` with `ContentType: RESOURCE` and `PageSize: 1000` (`caiPageSize`) across every sorted target scope (`organizations/...`, `folders/...`, `projects/...`).
2. The first page response sets `*pinnedReadTime = resp.GetReadTime()` (and records `FactsFile.ReadTime` in UTC RFC3339).
3. Every subsequent `RESOURCE` page—and every `IAM_POLICY` page across all scopes—passes that exact `ReadTime` timestamp so resources and IAM bindings represent a single consistent point in time.
4. gRPC channels dial with a `32 MiB` max receive message size (`maxGRPCRecvMsgBytes = 32 << 20`) to handle large Kubernetes or IAM policy pages cleanly.

### 20-Type Scoped `IAM_POLICY` Listing

Instead of scanning IAM policies on every disk, route, or historical build, `collectCAIIAMPolicies` restricts `AssetTypes` to **20** architecturally significant resource types (`iamPolicyAssetTypes` in `internal/extract/cai.go:32-53`):

| Category | Scoped `IAM_POLICY` Asset Types (20 Total) |
| :--- | :--- |
| **Hierarchy & Identity (4)** | `cloudresourcemanager.googleapis.com/Organization`, `cloudresourcemanager.googleapis.com/Folder`, `cloudresourcemanager.googleapis.com/Project`, `iam.googleapis.com/ServiceAccount` |
| **Networking (1)** | `compute.googleapis.com/Subnetwork` *(filtered in-stream to attachment roles only)* |
| **Storage & Data (6)** | `storage.googleapis.com/Bucket`, `bigquery.googleapis.com/Dataset`, `spanner.googleapis.com/Instance`, `bigtableadmin.googleapis.com/Instance`, `artifactregistry.googleapis.com/Repository`, `cloudkms.googleapis.com/CryptoKey` |
| **Messaging & Orchestration (4)** | `pubsub.googleapis.com/Topic`, `pubsub.googleapis.com/Subscription`, `cloudtasks.googleapis.com/Queue`, `workflows.googleapis.com/Workflow` |
| **Compute, AI & Secrets (5)** | `run.googleapis.com/Service`, `cloudfunctions.googleapis.com/CloudFunction`, `aiplatform.googleapis.com/Endpoint`, `aiplatform.googleapis.com/ReasoningEngine`, `secretmanager.googleapis.com/Secret` |

!!! note "Subnetwork `networkUser` In-Stream Filtering"
    For `compute.googleapis.com/Subnetwork` IAM policies, `ingestIAMPolicyAsset` retains only subnet-attachment bindings (`roles/compute.networkUser`, roles containing `networkUser`, or `compute.subnetworks.use`) to surface Shared VPC host-subnet grants without cluttering `facts.json` with administrative bindings.

### Rate Limiters & Exponential Backoff

All discovery clients share token-bucket rate limiters (`golang.org/x/time/rate`) and exponential retry logic (`internal/extract/client.go:35-50, 299-346`):

| Client / Enricher | Default Rate Limit | Burst | Retry Trigger | Retry Backoff |
| :--- | :--- | :---: | :--- | :--- |
| **Cloud Asset Inventory (`CAILimiter`)** | `80` RPM (`DefaultCAIRPM`, safely below GCP's 100 RPM ceiling) | `1` | gRPC `ResourceExhausted`, `RATE_LIMIT_EXCEEDED`, HTTP `429`, REST `rateLimitExceeded` | `maxRateLimitRetries = 3` retries (`100ms -> 200ms -> 400ms`, context-cancelable) |
| **Cloud Monitoring (`MonitoringLimiter`)** | `10` QPS (`DefaultMonitoringQPS`) | `1` | Same | Same (`100ms -> 200ms -> 400ms`) |
| **BigQuery (`BQLimiter`)** | `10` QPS (`DefaultEnricherQPS`) | `10` | Same | Same (`100ms -> 200ms -> 400ms`) |
| **Dataflow (`DataflowLimiter`)** | `10` QPS (`DefaultEnricherQPS`) | `10` | Same | Same (`100ms -> 200ms -> 400ms`) |

---

## Hierarchy Discovery & Enabled-API Gating

Four control-plane asset types are intercepted during CAI streaming by `ingestHierarchyAsset` (`internal/extract/cai.go:621-635`) and never emitted as active workload resources:

* **`Organization`, `Folder`, `Project`:** Populate `FactsFile.Hierarchy` (`OrgNode`, `FolderNode`, `ProjectNode`) and register discovered `ProjectNumber -> ProjectID` mappings in `projIdx` (overlaid with `--project-alias` / `config.yaml` `project_aliases`).
* **`serviceusage.googleapis.com/Service`:** Records every enabled API (`state == "ENABLED"` or empty) per project in `enabledServices`. Phase 7 enrichers consult `isServiceMaybeEnabled` to skip BigQuery, Dataflow, Cloud Monitoring, and Vertex AI `PublisherModel` calls on projects where those APIs are confirmed disabled.
* **PSA Tenant Project Detection (`IsPSATenantProject`):** Projects matching `^p[a-z0-9]+-tp$` (Google's Private Services Access managed tenant naming convention) or discovered via `servicenetworking` VPC peerings are flagged `ProjectNode.IsPSATenant = true`, excluded from out-of-scope target warnings, and skipped by enrichers.
* **Shared VPC Host Tracking (`recordSharedVPCHost`):** Whenever an active resource in project `A` references a VPC belonging to a different non-PSA project `B`, `ProjectNode.SharedVPCHostProject` is populated with `B`. If `B` lies outside the scanned scopes, an external project stub is registered and linked via `CONFIG_REF` (`detail: "shared_vpc"`).

---

## Deterministic Plumbing Suppression & Child Folding

Real-world GCP projects routinely hold thousands of ephemeral or low-level resources that destroy diagram clarity and bloat synthesis prompts. Stage 1 deterministically filters them into `suppressed_plumbing_counts` while preserving their architectural signals via in-memory folding and bridge tables.

### Suppression Lists (`internal/extract/sanitize.go`)

1. **`92` explicit `plumbingAssetTypes`:**
   * **Compute / VPC / LB intermediates (23):** `Route`, `Firewall`, `Policy`, `compute/Project`, `InstanceTemplate`, `Autoscaler`, `HealthCheck`, `Disk`, `Address`, `GlobalAddress`, `TargetHttpProxy`, `TargetHttpsProxy`, `TargetGrpcProxy`, `TargetTcpProxy`, `TargetSslProxy`, `SslCertificate`, `UrlMap`, `BackendService`, `BackendBucket`, `SecurityPolicy`, `NetworkEndpointGroup`, `ExternalVpnGateway`, `VpnTunnel`.
   * **Cloud Build history (1):** `cloudbuild.googleapis.com/Build` *(folded via min-heap before discard)*.
   * **GKE / Kubernetes plumbing (16):** `container/NodePool`, `gkehub/Membership`, `k8s.io/Namespace`, `ServiceAccount`, `Secret`, `ConfigMap`, `Endpoints`, `discovery.k8s.io/EndpointSlice`, `Pod`, `apps.k8s.io/ReplicaSet`, `Event`, `Node`, `ResourceQuota`, `PersistentVolume`, `PersistentVolumeClaim`, `storage.k8s.io/StorageClass`.
   * **Database, Storage, Analytics & AI absorbed children / backups (44):** Cloud SQL `Backup`/`BackupRun`, AlloyDB `Instance`, Spanner `Database`, Bigtable `Cluster`/`AppProfile`/`Table`, NetApp `Volume`, BigQuery `Table`/`Routine`/`Model`/`RowAccessPolicy`, Artifact Registry `DockerImage`/`Package`/`Version`, Cloud Run `Revision`, App Engine `Application`/`Version`, Cloud DNS `ResourceRecordSet`, NCC `Spoke`, Certificate Manager `Certificate`/`CertificateMap`, API Gateway `Api`/`ApiConfig`, Apigee `Instance`/`EnvironmentGroup`/`Environment`/`EndpointAttachment`/`ApiProxy`, Datastream `ConnectionProfile`/`PrivateConnection`, Dataproc `AutoscalingPolicy`, Dataform `Workspace`/`CompilationResult`/`WorkflowInvocation`/`WorkflowConfig`, Dataplex `Zone`/`Asset`, Analytics Hub `Listing`, Vertex AI `Model`/`Index`/`FeatureGroup`/`FeatureView`, Document AI `ProcessorVersion`.
   * **Control plane & security metadata (8):** CRM `Organization`/`Folder`/`Project`, `serviceusage/Service`, Access Context Manager `AccessPolicy`/`ServicePerimeter`, `servicenetworking/Connection`, `containeranalysis/Occurrence`.
2. **`5` `plumbingAssetPrefixes`:**
   * `admissionregistration.k8s.io/`
   * `rbac.authorization.k8s.io/`
   * `gateway.networking.k8s.io/`
   * `autoscaling.k8s.io/`
   * `servicedirectory.googleapis.com/`
3. **`16` `suppressedPlumbingServices`:** Control-plane APIs suppressed from active assets (except retained types under `compute.googleapis.com`, `container.googleapis.com`, and `iam.googleapis.com`) and excluded from Cloud Monitoring attribution:
   `sts`, `iamcredentials`, `iam`, `oauth2`, `oslogin`, `serviceusage`, `servicemanagement`, `servicecontrol`, `cloudresourcemanager`, `compute`, `container`, `cloudasset`, `orgpolicy`, `cloudbilling`, `recommender`, and `cloudapis` (all `.googleapis.com`).

### Child-to-Parent Folding

Before a suppressed child asset is discarded, its count and structural links are folded onto its parent resource (`internal/extract/cai.go:637-748`):

| Child Asset Type | Parent Resource | Folded Attribute Key & Additional Folding |
| :--- | :--- | :--- |
| `bigquery.googleapis.com/Table` | BigQuery `Dataset` | `table_count` + `externalDataConfiguration` (`sourceUris`, `connectionId`) folded for edge resolution |
| `bigquery.googleapis.com/Routine` | BigQuery `Dataset` | `routine_count` + `remoteFunctionOptions` (`connection`, `endpoint`) |
| `bigquery.googleapis.com/Model` | BigQuery `Dataset` | `model_count` + `remoteModelInfo` (`connection`, `endpoint`) |
| `artifactregistry.googleapis.com/DockerImage` | Artifact Registry `Repository` | `image_count` |
| `artifactregistry.googleapis.com/Package` | Artifact Registry `Repository` | `package_count` |
| `artifactregistry.googleapis.com/Version` | Artifact Registry `Repository` | `version_count` |
| `spanner.googleapis.com/Database` | Spanner `Instance` | `database_count` |
| `bigtableadmin.googleapis.com/Cluster` | Bigtable `Instance` | `cluster_count` |
| `bigtableadmin.googleapis.com/Table` | Bigtable `Instance` | `table_count` |
| `container.googleapis.com/NodePool` | GKE `Cluster` | `node_pool_count` + captures `instanceGroupUrls` for GKE worker suppression |
| `k8s.io/Namespace` | GKE `Cluster` | `namespace_count` |
| `cloudkms.googleapis.com/CryptoKey` *(retained)* | KMS `KeyRing` | `key_count` (also retained as an active `CryptoKey` resource) |
| `networkconnectivity.googleapis.com/Spoke` | NCC `Hub` | `spoke_count` + bridged to linked VPCs / VPN Gateways / Interconnects |
| `run.googleapis.com/Revision` | Cloud Run `Service` | Active traffic-split revisions (or `latestReadyRevisionName`) merge `service_account`, `vpc_connector`, `vpc`, `subnet`, Cloud SQL instances, and sanitized env/secret refs onto the parent Service |
| `appengine.googleapis.com/Version` | App Engine `Service` | Versions receiving `split.allocations > 0` merge `service_account`, `vpc_connector`, and sanitized env/secret refs onto the parent Service |

### System Namespaces, GKE Worker Infrastructure & Default Sandbox Pruning

* **System K8s Namespaces (`isSystemK8sNamespace`):** Retained Kubernetes workloads, Services, and Ingresses in `kube-system`, `gmp-system`, or any `gke-managed-*` namespace are dropped into `suppressed_plumbing_counts`.
* **GKE Worker MIG & VM Suppression (`suppressGKENodeInfra`):** Suppresses `compute.googleapis.com/InstanceGroupManager` assets matching cluster `instanceGroupUrls` or `gke-<cluster>-` / `gk3-<cluster>-` prefixes, and suppresses `compute.googleapis.com/Instance` worker VMs matching `created-by` MIG, `cluster-name` metadata, `goog-gke-cluster-name` / `goog-k8s-cluster-name` labels, or `gke-<cluster>-` / `gk3-<cluster>-` name prefixes.
* **Default Sandbox Pruning (`pruneDefaultSandboxAssets`):** Executed in Phase 8 after all edges and enrichers complete:
  * **Default VPC (`default`), Default Subnetworks (`default` on VPC `default`), and Default Service Accounts** (`*-compute@developer.gserviceaccount.com`, `*@appspot.gserviceaccount.com`, `*@cloudservices.gserviceaccount.com`, `*@cloudbuild.gserviceaccount.com`, and `service-<num>@*.iam.gserviceaccount.com`) are suppressed **only if unreferenced** by any retained asset or edge.
  * **`logging.googleapis.com/LogBucket` named `_Default` or `_Required`** is **unconditionally suppressed**.

---

## In-Stream Cloud Build Resolution

Cloud Build projects can accumulate thousands of historical `cloudbuild.googleapis.com/Build` assets. Stage 1 extracts CI/CD deployment graphs in streaming memory with bounded heap size (`internal/extract/cai.go:792-839, 2244-2326`):

1. **External Git Repository Synthesis (`BuildTrigger`):** Parses `github`, `sourceToBuild`, `triggerTemplate` (including the `github_<owner>_<repo>` Cloud Source mirror convention), and `repositoryEventConfig` to synthesize external `external.github.com/Repository` or `external.gitlab.com/Repository` resources (`project_id: "external"`, `is_external: true`) connected to the trigger via `CLOUDBUILD_TRIGGER_REPO` with `detail` capturing the trigger filter (`push:<branch>`, `tag:<tag>`, or `pr:<branch>`). Inline `build` definitions on triggers are scanned immediately.
2. **Bounded Min-Heap Folding (`cbBuildMinHeap`):** As historical `cloudbuild.googleapis.com/Build` records stream past:
   * Only `status == "SUCCESS"` builds are considered; every `Build` asset increments `suppressed_plumbing_counts["cloudbuild.googleapis.com/Build"]`.
   * Builds are keyed by `{projectID, triggerID}` into a min-heap ordered by `(createTime, uri)`.
   * Triggered builds retain only the **latest `N` succeeded runs** (`--cloudbuild-runs`, default `3`).
   * Triggerless manual/ad-hoc builds (`buildTriggerId == ""`) are capped at `maxTriggerlessCloudBuildRuns = 5` per project and attributed to the project URI (`//cloudresourcemanager.googleapis.com/projects/<proj>`).
3. **Step & Artifact Scanning (`extractCloudBuildPayloadRefs`):** Scans `steps[].name`, `entrypoint`, `script`, `args` (including space-joined `args` prefixed with `entrypoint` or `gcloud` when the step image contains `gcloud` or `cloud-sdk`), `images[]`, and `artifacts.objects.location` via `ExtractSanitizedRefs`:
   * Extracts Artifact Registry repos, GCR projects, GCS buckets, Secret Manager refs, `--project=<id>`, `--cluster=<name>`, and `gcloud run deploy <svc>`.
   * Resolves `--cluster` and `run-deploy:` against unique matching in-scope GKE Clusters or Cloud Run Services (scoped to explicit `--project` targets when present).
4. **Out-of-Scope Deployment Target Hints:** References to projects outside the scanned scopes (via `--project`, `gcr.io/<proj>`, or cross-project URIs) synthesize external `cloudresourcemanager.googleapis.com/Project` stubs, emit `CLOUDBUILD_STEP_REF` edges, and populate `out_of_scope_targets` (`source_kind: "cloudbuild_deploy_flag"`). Built-in public builder mirrors (13 namespaces such as `cloud-builders`, `google-containers`, `distroless`, `gke-release`) and PSA tenant projects are automatically excluded.

---

## GKE Resolution (`internal/extract/gke.go`)

Stage 1 retains **7** Kubernetes asset types (non-system namespaces only) and sets `parent_uri` to their owning GKE cluster URI:

* **Workloads (5):** `apps.k8s.io/Deployment`, `apps.k8s.io/StatefulSet`, `apps.k8s.io/DaemonSet`, `batch.k8s.io/CronJob`, `batch.k8s.io/Job`
* **Networking (2):** `k8s.io/Service`, `networking.k8s.io/Ingress`

`resolveGKE()` links the complete Kubernetes traffic and identity stack across four deterministic joins:

```mermaid
flowchart LR
    FR["ForwardingRule / GlobalForwardingRule"] -- "K8S_NEG_BACKEND (via Proxy -> UrlMap -> BackendService -> NEG)" --> SVC["k8s.io/Service"]
    FR -- "K8S_INGRESS_BACKEND (L4 IP match)" --> SVC
    ING["networking.k8s.io/Ingress"] -- "K8S_INGRESS_BACKEND (host + path)" --> SVC
    SVC -- "K8S_SERVICE_SELECTOR (non-empty label match)" --> WL["K8s Workload (Deployment / StatefulSet / DaemonSet / CronJob / Job)"]
    WL -- "WORKLOAD_IDENTITY (iam.gke.io/gcp-service-account)" --> GSA["GCP ServiceAccount"]
```

1. **ConfigMap Reference Folding:** Suppressed `k8s.io/ConfigMap` `data` maps are scanned in memory with `ExtractSanitizedRefs`. Workloads referencing ConfigMaps via `envFrom.configMapRef`, `valueFrom.configMapKeyRef`, or `volumes[].configMap` inherit those sanitized env and secret references (scanning both `containers[]` and `initContainers[]`).
2. **Workload Identity (`WORKLOAD_IDENTITY`):** Suppressed `k8s.io/ServiceAccount` annotations (`iam.gke.io/gcp-service-account`) map `(clusterURI, namespace, serviceAccountName)` (defaulting empty `serviceAccountName` to `"default"`) to the GCP Service Account email, setting `attributes.service_account` and emitting `WORKLOAD_IDENTITY` (`detail: "ksa:<ns>/<ksa>"`). Cross-project GSAs synthesize out-of-scope project stubs (`source_kind: "iam_cross_project"`).
3. **`k8s.io/Service` Resolution (`K8S_SERVICE_SELECTOR`):**
   * Parses the `cloud.google.com/neg-status` JSON annotation (`network_endpoint_groups: {port: negName}`) and extracts `status.loadBalancer.ingress[].ip` into `attributes.load_balancer_ips`.
   * Matches `spec.selector` against pod template labels on workloads in the same cluster and namespace. **Explicitly requires a non-empty selector** so headless or `ExternalName` Services never match every workload in the namespace.
4. **`Ingress` & `ForwardingRule` Chains (`K8S_INGRESS_BACKEND`, `K8S_NEG_BACKEND`):**
   * Extracts `spec.tls[].hosts`, `spec.rules[].host`, `http.paths[].path`, `defaultBackend`/`backend`, and `ingress.kubernetes.io/{,https-}forwarding-rule` annotations, populating `ingress_hosts` and `ingress_paths` on both the Ingress and linked ForwardingRules.
   * `traceForwardingRule` walks suppressed intermediates (`ForwardingRule -> Target{Http,Https,Grpc,Tcp,Ssl}Proxy -> UrlMap -> BackendService -> NEG`), resolving GKE NEGs by `(project, negName)` (`K8S_NEG_BACKEND`, `detail: "<paths> port:<p>"`) and L4 Service IPs (`K8S_INGRESS_BACKEND`, `detail: "L4:<ip>"`), while propagating `tls: true` for HTTPS/SSL proxies.

---

## Network, PSA/PSC, Absorbed Bridges & IAM Resolution

Implemented in `internal/extract/edges.go`, Phase 3 and Phase 6 resolve networking topology, absorbed resource bridges, sanitized references, and IAM relationships:

### VPC Peerings vs. PSA / PSC Connectivity

* **Active VPC Peerings (`indexVPCPeerings`):** Inspects `ACTIVE` peerings on `compute.googleapis.com/Network`.
  * **PSA Peerings (`PSA_PEERING`):** Peerings whose target project matches `^p[a-z0-9]+-tp$` or whose peering name contains `servicenetworking` record `psaTenants[vpcURI] = tenantProj`. PSA-managed services (`sqladmin/Instance`, `alloydb/Cluster`, `redis/Instance`, `memcache/Instance`, `file/Instance`) receive `PSA_PEERING` edges from their consumer VPC (`detail: "servicenetworking-googleapis-com (<tenant>)"`) and attribute `psa_tenant_project`.
  * **Standard VPC Peerings (`VPC_PEERING`):** In-scope VPC pairs emit a single canonically ordered edge (`src < dst`, `detail: <peeringName>`); out-of-scope peer projects emit an edge to an external project stub.
  * **PSC Redis Clusters:** `redis.googleapis.com/Cluster` emits `CONFIG_REF` (`detail: "psc_network"`) from its consumer VPC.

### Private IPv4 & Serverless HTTP Matching

* **Private IPv4 Matching (`parsePrivateIPv4`, `matchPrivateIP`):** Recognizes RFC 1918 (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`) and CGNAT (`100.64.0.0/10`) addresses. Indexes private IPs of Compute Instances, internal ForwardingRules, Cloud SQL, AlloyDB (folded from absorbed `alloydb.googleapis.com/Instance`), Redis Instance/Cluster, Memcache, and Filestore by `(networkURI, ip)`. Env vars, ConfigMaps, and Datastream CDC profiles resolve private IPs across the source asset's VPC and directly peered VPCs (`reachableNetworks`)—emitting an edge **only when unambiguous (`len(matches) == 1`)**.
* **Cloud Run / Cloud Functions HTTP Matching (`indexCloudRunHosts`, `resolveHTTPTarget`):** Indexes sanitized URLs, hostnames, and short service names for retained `run.googleapis.com/Service` and `cloudfunctions.googleapis.com/CloudFunction` assets. Matches exact URLs, `*.run.app` hosts (including `<service>-<hash>-<region>.a.run.app` fallback when unique), `*.cloudfunctions.net/<fnName>`, and `.googleapis.com/` URLs across Pub/Sub push endpoints, Cloud Tasks, Cloud Scheduler, Dialogflow webhooks, BigQuery remote functions, and env vars.
* **Non-GKE Load Balancer Backends:** Resolves `BackendBucket` -> GCS Bucket, Serverless NEGs (Cloud Run, Cloud Functions, App Engine), PSC NEGs (`pscTargetService`), and MIG / InstanceGroup backends into `CONFIG_REF` edges.

### Absorbed Bridge Resolution

Suppressed intermediate resources are stored in `absorbed` and transparently bridged into direct `CONFIG_REF` edges (`internal/extract/edges.go:773-896`):

| Absorbed Resource / Folded Source | Emitted Direct Edge (`CONFIG_REF`) | `detail` |
| :--- | :--- | :--- |
| `compute.googleapis.com/VpnTunnel` | `Cloud Router -> VpnGateway` | `vpn_tunnel` |
| `networkconnectivity.googleapis.com/Spoke` | `NCC Hub -> linked VPC / VpnGateway / InterconnectAttachment` | `ncc_spoke` |
| `aiplatform.googleapis.com/FeatureView` | `BigQuery Dataset -> FeatureOnlineStore` | `feature_sync` |
| `analyticshub.googleapis.com/Listing` | `BigQuery Dataset -> DataExchange` | `analytics_hub_listing` |
| `dataplex.googleapis.com/Asset` | `Dataplex Lake -> resourceSpec.name` | `dataplex_asset` |
| `datastream.googleapis.com/ConnectionProfile` & `PrivateConnection` | `Datastream Stream -> GCS Bucket` and `Private-IP DB -> Datastream Stream` *(matched via `PrivateConnection` VPC)* | `datastream.gcs`, `datastream.cdc:<ip>` |
| `alloydb.googleapis.com/Instance` | Folds `private_ip` onto parent `AlloyDB Cluster` for private-IP resolution | *(attribute folding)* |
| Folded BigQuery external URIs (`Table` / `Routine` / `Model`) | `GCS Bucket -> BigQuery Dataset`, `BigQuery Dataset -> Run/Fn`, `BigQuery Dataset -> BQ Connection` | `biglake`, `bq_remote_fn`, `bq_external` |

### IAM Binding Resolution (`resolveIAMBindings`)

Stage 1 processes `serviceAccount:<email>` bindings (excluding GKE Workload Identity Federation pool principals containing `[`) with strict anti-noise heuristics (`internal/extract/edges.go:1086-1240`):

1. **Broad Ancestor Roles -> `ProjectCapabilities`:** `roles/owner`, `roles/editor`, `roles/viewer`, and `roles/browser` bound on an Organization, Folder, or Project are **never** expanded into noisy edges—they are folded into `FactResource.ProjectCapabilities` on the Service Account.
2. **`roles/iam.workloadIdentityUser`:** Skipped (already captured cleanly via `WORKLOAD_IDENTITY`).
3. **Direct Resource-Level Bindings:** Policies attached directly to a retained resource (Bucket, Dataset, Topic, Secret, Cloud Run Service, etc.) emit `IAM_RESOURCE_BINDING` (`SA -> resource`, `detail: <role>`).
4. **CEL Condition-Pinned Project Bindings:** Project-level bindings with IAM CEL conditions matching `resource.name == "..."`, `resource.name.startsWith("...")`, or sanitized AIP-122 URIs emit `IAM_RESOURCE_BINDING` to those exact pinned resources.
5. **Single-Target 22-Service Heuristic (`serviceRoleTargetTypes`):** For non-default Service Accounts granted a service-specific project-level role across **22** role prefixes (`roles/cloudsql.`, `roles/alloydb.`, `roles/spanner.`, `roles/datastore.`, `roles/firestore.`, `roles/bigtable.`, `roles/redis.`, `roles/memcache.`, `roles/file.`, `roles/storage.`, `roles/bigquery.`, `roles/pubsub.`, `roles/cloudtasks.`, `roles/workflows.`, `roles/secretmanager.`, `roles/artifactregistry.`, `roles/cloudkms.`, `roles/run.`, `roles/cloudfunctions.`, `roles/discoveryengine.`, `roles/dialogflow.`, `roles/documentai.`), an `IAM_RESOURCE_BINDING` edge is emitted **if and only if exactly 1** active resource of that target type exists in the project.

---

## Phase 7: Concurrent 7-Day Enrichers (`internal/extract/enrichers.go`)

All three enrichers fetch data in parallel via `errgroup.WithContext` over a `7`-day lookback window anchored to `ReadTime` (`enrichmentLookback = 7 * 24h`). Each per-project fan-out bounds concurrency to `maxEnrichWorkers = 8`, sorts its collected records deterministically, and **applies mutations sequentially on the main goroutine** (`Dataflow -> BigQuery -> Cloud Monitoring`) for reproducible output and zero data races.

### 1. Cloud Monitoring Enricher (`monitoring`)

Runs across non-empty, non-PSA projects where `monitoring.googleapis.com` is enabled (or unknown), aggregating with `ALIGN_SUM` / `REDUCE_SUM` on an `86400s` (daily) alignment period:

* **`consumed_api` Traffic (`serviceruntime.googleapis.com/api/request_count`):**
  * Grouped by `resource.labels.project_id`, `resource.labels.service`, `resource.labels.credential_id`, and `resource.labels.method`.
  * **Read-Only Discovery Filtering (`IsReadOnlyDiscoveryMethod`):** Excludes read-only control/refresh methods (`get*`, `list*`, `aggregatedlist*`, `batchget*`, `*testiampermissions`, `watch*`, `poll*`, `listen`) so Terraform refreshes and monitoring scrapers do not create false runtime dependencies.
  * **6 Companion API Aliases (`companionAPIHostMap`):** Maps `storage-component` / `storage-api` -> `storage.googleapis.com`, `bigquerystorage` / `bigquerymigration` -> `bigquery.googleapis.com`, `containerfilesystem` -> `container.googleapis.com`, and `bigtable` -> `bigtableadmin.googleapis.com`.
  * **Exclusion Lists:** Drops all **16** `suppressedPlumbingServices` and **10** `transversalControlAPIs` (`modelarmor`, `cloudtrace`, `logging`, `monitoring`, `secretmanager`, `cloudkms`, `dlp`, `artifactregistry`, `cloudbuild`, `clouddeploy` `.googleapis.com`).
  * **Unambiguous Attribution:** Resolves `serviceaccount:<uniqueId>` callers to their unique workload (if exactly 1 workload runs with that SA) or the SA itself (if 0 workloads; skipped if `> 1` workloads share the SA). Emits `MONITORING_TRAFFIC` (`detail: "<host> (<N> reqs/7d)"`) when exactly 1 active target resource exists in the project for any of the **17** `consumedAPITargetTypes`.
* **Vertex AI `PublisherModel` Metrics** *(gated on `aiplatform.googleapis.com` enabled)*:
  * Queries `aiplatform.googleapis.com/publisher/online_serving/model_invocation_count` and (when invocations `> 0`) `aiplatform.googleapis.com/publisher/online_serving/consumed_token_throughput`, grouped by `project_id`, `location`, `publisher`, `model_user_id`, and `metric.labels.request_type`.
  * Synthesizes `aiplatform.googleapis.com/PublisherModel` resources (`//aiplatform.googleapis.com/projects/<proj>/locations/<loc>/publishers/<pub>/models/<model>`) with `publisher`, `model_id`, `invocations`, and optional `dedicated_throughput: true`.
  * Attributes `generateContent` / `embedContent` `consumed_api` SA calls to the project's `PublisherModel` via `MONITORING_TRAFFIC` when a single `PublisherModel` exists in that project and no non-SA GenAI calls occurred.

### 2. BigQuery Job Lineage Enricher (`bq_jobs`)

Runs across all discovered non-PSA projects where `bigquery.googleapis.com` is enabled:

* Calls the metadata-only REST API `bigquery.jobs.list` with `AllUsers(true)`, `Projection("full")`, `MinCreationTime(readTime - 7d)`, `MaxResults(1000)`, and a strict partial `Fields` selector—incurring **USD 0 BigQuery SQL slot cost** (never executes SQL or `INFORMATION_SCHEMA` queries).
* Capped at `maxBQJobPages = 20` pages (`20,000` jobs/project); hitting the cap records `TRUNCATING_CAP` in `coverage_ledger` and retains all jobs collected so far.
* Extracts `statistics.query.referencedTables`, `configuration.query.destinationTable`, `configuration.load.destinationTable`, and `configuration.copy` source/destination tables.
* Filters out hidden/temporary artifacts: datasets prefixed with `_`, tables prefixed with `_script`, and anonymous temp tables matching `^anon(?:_|[0-9a-f]{8,})`.
* Emits two `BQ_LINEAGE` edge patterns:
  * **Dataset-to-Dataset ELT (`detail: "sql_elt"`):** `referencedDataset -> destinationDataset` whenever they differ.
  * **Principal-to-Dataset (`detail: "bq_job"`):** Resolves `principal_subject` (`serviceAccount:<email>`) or `user_email` to a unique workload or Service Account -> touched Dataset.

### 3. Active Dataflow Enricher (`dataflow_jobs`)

Runs across all discovered non-PSA projects where `dataflow.googleapis.com` is enabled:

* Lists active streaming/batch pipelines across all locations (`Projects.Locations.Jobs.List(proj, "-").Filter("ACTIVE")`) and retrieves each job with `Jobs.Get(proj, loc, id).View("JOB_VIEW_DESCRIPTION")`.
* Ensures a `dataflow.googleapis.com/Job` resource exists with `job_type`, `state`, `service_account`, `vpc`, and `subnet` (setting `parent_uri` to subnet or VPC).
* Scans `environment.tempStoragePrefix`, `environment.dataset`, `pipelineDescription.displayData`, and `originalPipelineTransform[].displayData` (`strValue` and `url`) via `ExtractSanitizedRefs` to emit `CONFIG_REF` edges (and `service_account` links).

---

## Complete `FactEdgeKind` Reference (12 Values)

Every edge in `facts.json` is classified by one of **12** `FactEdgeKind` constants (`internal/extract/schema.go:22-35`):

| `FactEdgeKind` | Direction (`from_uri -> to_uri`) | Typical `detail` Format | Producer Phase |
| :--- | :--- | :--- | :--- |
| `CONFIG_REF` | Source resource -> referenced resource *(or inverted for inbound triggers like `Topic -> Subscription`)* | `cmek`, `vpc_connector`, `subscription`, `secret_ref`, `service_account`, `vpn_tunnel`, `ncc_spoke`, `biglake`, `bq_remote_fn`, `datastream.cdc:<ip>`, private IP, `gs://<bucket>`, etc. | Phase 6 & Phase 7 (Dataflow) |
| `K8S_INGRESS_BACKEND` | `Ingress` or `ForwardingRule` (L4) -> `k8s.io/Service` | `<host><path>` (or `/*`) or `L4:<ip>` | Phase 4 (GKE) |
| `K8S_NEG_BACKEND` | `ForwardingRule` -> `k8s.io/Service` (via Proxy -> UrlMap -> BackendService -> NEG) | `<paths> port:<port>` or `neg` | Phase 4 (GKE) |
| `K8S_SERVICE_SELECTOR` | `k8s.io/Service` -> K8s Workload | Sorted selector `k1=v1,k2=v2` | Phase 4 (GKE) |
| `WORKLOAD_IDENTITY` | K8s Workload -> GCP `ServiceAccount` *(or out-of-scope project stub)* | `ksa:<namespace>/<ksa>` *(or `ksa:<ns>/<ksa>-><email>`)* | Phase 4 (GKE) |
| `CLOUDBUILD_TRIGGER_REPO` | External GitHub/GitLab `Repository` -> `BuildTrigger` | `push:<branch>`, `tag:<tag>`, `pr:<branch>` | Phase 5 (Cloud Build) |
| `CLOUDBUILD_STEP_REF` | `BuildTrigger` *(or Project for triggerless builds)* -> target repo / bucket / secret / cluster / Cloud Run / out-of-scope project | `--project=<id>`, `--cluster=<name>`, `run-deploy:<svc>`, `secret_ref`, `gcr.io/<proj>`, Artifact Registry path | Phase 5 (Cloud Build) |
| `IAM_RESOURCE_BINDING` | `ServiceAccount` *(or out-of-scope project stub)* -> target resource | IAM role (e.g., `roles/storage.objectAdmin`, `roles/cloudsql.client`) | Phase 6 (IAM) |
| `PSA_PEERING` | Consumer `VPC Network` -> PSA-managed service (`Cloud SQL`, `AlloyDB`, `Redis`, `Memcache`, `Filestore`) | `servicenetworking-googleapis-com (<tenant_project>)` | Phase 3 (VPC/PSA) |
| `VPC_PEERING` | Lexicographically smaller `VPC Network` -> peer `VPC Network` *(or out-of-scope stub)* | Peering name | Phase 3 (VPC/PSA) |
| `BQ_LINEAGE` | Source `Dataset` -> dest `Dataset`, or Workload/SA -> `Dataset` | `sql_elt` or `bq_job` | Phase 7 (BigQuery) |
| `MONITORING_TRAFFIC` | Workload or `ServiceAccount` -> single-target service resource or `PublisherModel` | `<service_host> (<N> reqs/7d)` | Phase 7 (Monitoring) |

---

## Complete `CoverageState` Reference (5 Values)

Every API scope queried during extraction is logged in `coverage_ledger` with one of **5** `CoverageState` constants (`internal/extract/schema.go:41-47`):

| `CoverageState` | Meaning | Trigger Condition | `extract` CLI Behavior |
| :--- | :--- | :--- | :--- |
| `OK` | Complete discovery for `(scope, source)`. | All pages succeeded without truncation or errors. | No warning printed. |
| `IAM_DENIED` | Caller lacked target IAM permission on this scope/source. | `IAM_PERMISSION_DENIED`, gRPC `PermissionDenied`, or HTTP `403`. | Printed under `Coverage warnings` on `stderr` (even with `-q`); exit `0`. |
| `VPC_SC_BLOCKED` | Blocked by a VPC Service Controls perimeter. | `VPC_SERVICE_CONTROLS`, `SECURITY_POLICY_VIOLATED`, or `vpcServiceControlsUniqueIdentifier`. | Printed under `Coverage warnings` on `stderr`; exit `0`. |
| `SERVICE_DISABLED` | API disabled on a distinct target child project. | `SERVICE_DISABLED` / `BILLING_DISABLED` whose `consumer` matches the target child project (and differs from caller quota project). | Printed under `Coverage warnings` on `stderr`; exit `0`. |
| `TRUNCATING_CAP` | Safety pagination cap reached before exhausting records. | BigQuery `jobs.list` hit `maxBQJobPages = 20` (`20,000` jobs) within the 7-day window. | Printed under `Coverage warnings` on `stderr`; exit `0`. |

---

## AIP-122 Canonicalization & `facts.json` `v3` Schema

### AIP-122 Normalization (`CanonicalizeAIP122`)

Because GCP APIs emit inconsistent resource identifiers (full REST `selfLink` URLs, `www.googleapis.com/<svc>/v1/...`, regional `us-central1-aiplatform.googleapis.com`, `gs://bucket`, numeric project IDs, and bare relative `projects/...` paths), every resource URI and edge endpoint passes through `CanonicalizeAIP122` / `CanonicalizeAIP122WithService` (`internal/extract/canonical.go`):

| Input Format | Canonical AIP-122 Output |
| :--- | :--- |
| `https://www.googleapis.com/compute/v1/projects/100200300401/locations/europe-southwest1/subnetworks/app` | `//compute.googleapis.com/projects/acme-cicd/regions/europe-southwest1/subnetworks/app` |
| `https://europe-southwest1-run.googleapis.com/v2/projects/100200300401/locations/europe-southwest1/services/api` | `//run.googleapis.com/projects/acme-cicd/locations/europe-southwest1/services/api` |
| `https://www.googleapis.com/sql/v1beta4/projects/100200300401/instances/sonarqube` | `//sqladmin.googleapis.com/projects/acme-cicd/instances/sonarqube` |
| `gs://acme-tfstate/prod/default.tfstate` | `//storage.googleapis.com/acme-tfstate` |
| `//container.googleapis.com/projects/acme-cicd/zones/europe-southwest1-a/clusters/ops` | `//container.googleapis.com/projects/acme-cicd/locations/europe-southwest1-a/clusters/ops` |
| `projects/100200300401/topics/events` | `//pubsub.googleapis.com/projects/acme-cicd/topics/events` |

Key normalization rules:
* Strips query strings (`?...`), fragments (`#...`), trailing slashes, regional host prefixes (`<region>-`, `global-`, `us-`, `eu-`), and API version path segments (`v1`, `v1beta1`, `v2alpha1`, `v1p1beta1`, etc.).
* Maps Compute Engine `/locations/global/...`, `/locations/<zone>/...`, and `/locations/<region>/...` into `/global/`, `/zones/`, and `/regions/`; maps GKE `/zones/.../clusters/` into `/locations/.../clusters/`.
* Substitutes every numeric project segment after `projects/` or `apps/` using `projIdx` (discovered `ProjectNode`s + `--project-alias`), then re-runs canonicalization across all maps and slices in Phase 2 (`reconcilePostCAI`) once every project number in scope has been discovered.

### Deterministic `EdgeID` Hashing & Canonical Sorting

Every `FactEdge.ID` is computed deterministically by `extract.EdgeID` (`internal/extract/schema.go:131-134`):

```text
EdgeID = hex(sha256(kind + ":" + fromURI + ":" + toURI + ":" + detail))[:16]
```

Before writing to disk, `WriteFacts` runs `normalizedFactsCopy` to canonically sort `scopes`, `hierarchy.{organizations,folders,projects}`, `resources` (and each resource's `project_capabilities`), `edges` (backfilling any empty `id`), `out_of_scope_targets` (and each hint's `referenced_by_uris`), and `coverage_ledger`, then writes 2-space indented UTF-8 JSON atomically (`0600` file / `0700` dir).

### `FactsFile` (`v3`) Field Reference

| Top-Level Key | Type | Description |
| :--- | :--- | :--- |
| `schema_version` | `string` | Always `"v3"` (`extract.FactsSchemaVersion`). `LoadFacts` rejects any other version. |
| `read_time` | `string` | RFC3339 UTC timestamp pinned from the first CAI `ListAssets` response. |
| `scopes` | `[]string` | Sorted canonical target scopes scanned. |
| `hierarchy` | `Hierarchy` | Discovered `organizations[]`, `folders[]`, and `projects[]` (including `project_number`, `parent`, `is_psa_tenant`, `shared_vpc_host_project`). |
| `resources` | `[]FactResource` | Active retained & external resources (`uri`, `asset_type`, `project_id`, `location`, `display_name`, `parent_uri`, `is_external`, allowlisted `attributes`, and `project_capabilities`). |
| `edges` | `[]FactEdge` | Deduplicated directed edges (`id`, `from_uri`, `to_uri`, `kind`, `detail`), filtered so both endpoints exist in `resources`. |
| `suppressed_plumbing_counts` | `map[string]int` | Exact count of suppressed plumbing, folded child, system-namespace, GKE-worker, and pruned default assets grouped by `asset_type`. |
| `out_of_scope_targets` | `[]OutOfScopeHint` | *(omitempty)* Cross-project targets referenced outside scanned scopes (`target_project_id`, `synthetic_uri`, `referenced_by_uris`, `source_kind`). |
| `coverage_ledger` | `[]CoverageEntry` | Complete audit log of every `(scope, source)` API query and its `CoverageState`. |

### Annotated `facts.json` (`v3`) Example

```json
{
  "schema_version": "v3",
  "read_time": "2026-11-23T02:00:00Z",
  "scopes": [
    "projects/acme-cicd"
  ],
  "hierarchy": {
    "projects": [
      {
        "project_id": "acme-cicd",
        "project_number": "100200300401",
        "display_name": "Acme Platform CICD"
      },
      {
        "project_id": "p1234567890abcdef-tp",
        "display_name": "p1234567890abcdef-tp",
        "is_psa_tenant": true
      }
    ]
  },
  "resources": [
    {
      "uri": "//artifactregistry.googleapis.com/projects/acme-cicd/locations/europe-southwest1/repositories/acme-apps",
      "asset_type": "artifactregistry.googleapis.com/Repository",
      "project_id": "acme-cicd",
      "location": "europe-southwest1",
      "display_name": "acme-apps",
      "attributes": {
        "format": "DOCKER",
        "image_count": 643,
        "mode": "STANDARD_REPOSITORY"
      }
    },
    {
      "uri": "//github.com/acme-corp/platform-services",
      "asset_type": "external.github.com/Repository",
      "project_id": "external",
      "display_name": "platform-services",
      "is_external": true
    }
  ],
  "edges": [
    {
      "id": "eeb34650563cea4f",
      "from_uri": "//iam.googleapis.com/projects/acme-cicd/serviceAccounts/cloudbuild-runner@acme-cicd.iam.gserviceaccount.com",
      "to_uri": "//artifactregistry.googleapis.com/projects/acme-cicd/locations/europe-southwest1/repositories/acme-apps",
      "kind": "IAM_RESOURCE_BINDING",
      "detail": "roles/artifactregistry.writer"
    }
  ],
  "suppressed_plumbing_counts": {
    "artifactregistry.googleapis.com/DockerImage": 643,
    "cloudbuild.googleapis.com/Build": 819,
    "k8s.io/ConfigMap": 14,
    "logging.googleapis.com/LogBucket": 2
  },
  "out_of_scope_targets": [
    {
      "target_project_id": "acme-prod",
      "synthetic_uri": "//cloudresourcemanager.googleapis.com/projects/acme-prod",
      "referenced_by_uris": [
        "//cloudbuild.googleapis.com/projects/acme-cicd/locations/europe-southwest1/triggers/deploy-api-prod"
      ],
      "source_kind": "cloudbuild_deploy_flag"
    }
  ],
  "coverage_ledger": [
    {
      "scope": "projects/acme-cicd",
      "source": "bq_jobs",
      "state": "OK"
    },
    {
      "scope": "projects/acme-cicd",
      "source": "cai_iam_policy",
      "state": "OK"
    },
    {
      "scope": "projects/acme-cicd",
      "source": "cai_resource",
      "state": "OK"
    },
    {
      "scope": "projects/acme-cicd",
      "source": "dataflow_jobs",
      "state": "OK"
    },
    {
      "scope": "projects/acme-cicd",
      "source": "monitoring",
      "state": "OK"
    }
  ]
}
```

---

## Related Documentation

* [CLI & Configuration](cli_configuration.md) — `gcp-arch extract` flags, `--project-alias`, `--cloudbuild-runs`, and `config.yaml` defaults.
* [Security, IAM & Preflight](security_iam.md) — Required IAM roles, `preflight` checks, per-asset attribute allowlist, and credential scrubbing.
* [Stage 2 — Gemini 4 Synthesis & Day-2](synthesis.md) — How `facts.json` is short-ID interned and synthesized into `architecture.yaml`.
* [Manifest Schema & Validation](manifest_schema.md) — How `gcp-arch validate --facts` checks 100% resource accounting and directed-walk `FactEdge` provenance.
