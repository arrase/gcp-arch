# GCP Architecture Topological Compiler (`gcp-arch`)

*   **Author:** Juan Ezquerro LLanes
*   **Status:** Superseded
*   **Date:** 2026-11-18

> [!WARNING]
> **Superseded Design (v1):** This v1 Draw.io topological compiler architecture has been superseded and removed from the codebase. See [semantic-multi-view-pipeline.md](semantic-multi-view-pipeline.md) for the active, implemented v3 semantic multi-view pipeline. The historical content below is preserved for reference only.

--------------------------------------------------------------------------------

## Overview

`gcp-arch` is a standalone command-line tool that inspects a live Google Cloud Platform (GCP) environment and compiles an editable architecture diagram baseline in Draw.io (`.drawio`) format. A project engineer or cloud consultant runs the single static binary locally using existing `gcloud` credentials (`Application Default Credentials`, or ADC) without installing external runtimes, agents, or paid cloud add-ons.

Rather than emitting one box per raw cloud API object, `gcp-arch` folds low-level sub-resources into logical service cards, restricts automatic arrows to verified structural pointers, SQL job execution history, and unambiguous `1:1` in-scope API correlations, records multi-target (`1:N`) candidates on node metadata for human confirmation in Draw.io, and updates existing `.drawio` files on subsequent runs without overwriting manual card coordinates, custom titles, notes, or hand-drawn arrows.

## The Problem

### Objectives

Cloud engineers and consultants responsible for GCP projects need to discover deployed cloud systems and deliver `As-Built` (deployed state) architecture diagrams to clients as part of project documentation. `gcp-arch` automates the inventory collection, semantic grouping, initial layout, and non-destructive incremental update of `.drawio` diagrams from live, read-only GCP APIs.

### State of the Art and Current Limits

Teams currently rely on four approaches to document GCP architectures:

1.  **Manual diagramming in Draw.io or Lucidchart:** Engineers inspect the Google Cloud Console by hand to draw boxes and connectors. Manual diagrams omit uninspected resources, reflect design intent rather than deployed state, and go stale after subsequent deployments.
2.  **Infrastructure-as-Code (IaC) visualizers:** Tools that parse Terraform or deployment manifests produce incomplete or stale diagrams whenever resources are modified outside IaC (`drift`), plans remain unapplied, or external consultants lack repository access.
3.  **Direct Cloud Asset Inventory (CAI) exporters:** Scripts that map CAI records 1:1 onto diagram nodes emit cluttered graphs because one logical GCP service spans many underlying API objects (seven networking resources for one HTTP Load Balancer, an auto-created Google Kubernetes Engine cluster inside one Cloud Composer environment, or tens of thousands of tables inside one BigQuery dataset).
4.  **Commercial discovery SaaS platforms and permission-based mappers:** SaaS platforms require organization-wide credentials, external metadata egress that conflicts with client confidentiality policies, or paid Google Security Command Center Premium licenses. Mappers that draw arrows from Identity and Access Management (IAM) roles produce false connections whenever one service account has access to multiple buckets or datasets, and regenerating outputs overwrites manual layout edits.

### New Approach

`gcp-arch` combines deterministic read-only cloud compilation with stateful `.drawio` XML updates:

1.  **Two-stage resource reduction:** During paginated ingestion, the collector folds high-cardinality child records (tables, container images, pods, serverless revisions) into compact summaries; after ingestion, the compiler executes 15 multi-resource graph compaction passes to assemble logical service cards.
2.  **Three-tier edge precision:** Solid arrows represent verified structural pointers, VPC-scoped endpoint matches, and BigQuery SQL job lineage; dashed arrows represent active API traffic where exactly one in-scope source workload and one in-scope target resource match (`1:1`). Ambiguous multi-target (`1:N`) or Secret Manager links emit zero automatic arrows and attach candidate lists to Draw.io node metadata (`Ctrl+M`).
3.  **Stateful 3-way `.drawio` updates:** Re-running `gcp-arch generate` against an existing `.drawio` file preserves existing node coordinates, user-edited titles/styles, manual arrows, user notes, and intentional card deletions, placing newly discovered cloud services into a `New Resources (Drag to Place)` staging container while refreshing live counter badges via Draw.io attribute placeholders.
4.  **Explicit handling of unknown services and partial permissions:** Uncataloged GCP asset types render as grouped `Unclassified Service` warning cards configurable via local YAML files without recompiling, and unreadable cross-project networks in restricted Landing Zones render as dashed `External Referenced` containers paired with a structured coverage report.

### Impact

Engineers generate an initial two-tab editable `.drawio` baseline (`L1-Platform-Network` and `L2-Architecture`) directly from `gcloud` credentials, curate ambiguous links and presentation details in Draw.io before exporting client PDF/SVG/PNG deliverables from Draw.io, and refresh the diagram across subsequent project sprints without losing manual layout work.

### Cost and Timeline

*   **Infrastructure and Licensing Cost:** USD 0 in additional GCP licensing or paid cloud tiers. Discovery uses free-tier GCP read quotas (`Cloud Asset Inventory`, `Cloud Monitoring` system metrics, `BigQuery` job metadata, `Dataflow` job descriptions, and `Vertex AI` RAG corpus listings) on the engineer's workstation (`< 512 MB` Go live heap `HeapAlloc`, zero generative AI token spend).
*   **Implementation Cost and Timeline:** One software engineer across four sequential phases (~4–5 weeks), matching the implementation breakdown:
    *   *Phase 1 (Week 1):* Domain model, embedded YAML catalog and icon loader, ADC preflight, rate-limited CAI streaming with child projection/sanitization, enrichers, and gzip snapshot persistence.
    *   *Phase 2 (Week 2):* 15 native Go multi-resource compaction passes, unclassified/external-reference node synthesis, and the three-tier edge resolution engine.
    *   *Phase 3 (Weeks 3–4):* Two-tab view projector, pure-Go Eclipse Layout Kernel (ELK) integration, `.drawio` serializer, and lossless 3-way XML merger.
    *   *Phase 4 (Week 5):* CLI subcommands (`preflight`, `generate`, `inspect`), user documentation, and end-to-end enterprise fixture verification.

### Metrics and Verification Exams

*   **Mid-Term Exam (Stream Folding & Compaction):** Process a synthetic snapshot representing 150,000 raw sub-resources (BigQuery tables, Kubernetes pods, Dataflow worker VMs, Cloud Composer GKE nodes, and 7-part Load Balancers) within `< 512 MB` Go live `HeapAlloc`, folding 100% of managed child resources into parent cards with zero leaked worker VMs or raw table nodes and byte-identical IR across repeated runs.
*   **Final Exam (End-to-End Compilation & Day-2 Update):** Compile the multi-project enterprise reference fixture into a `< 2 MB` `.drawio` file achieving: (1) zero unexpected edges against the reference topology, (2) byte-identical XML output across repeated runs, (3) preservation of simulated user Draw.io card coordinates, edited titles, manual arrows, text notes, and deleted cards during a Day-2 update run that stages newly added cloud resources into `New Resources (Drag to Place)` while refreshing live badge counts, and (4) runtime `.gcp-arch/catalog/*.yaml` reload resolving a synthetic unknown GCP asset type without recompilation.

## Risks and Mitigations

*   **Missing First-Run Arrows on Secret-Mediated or Shared-SA Links:** Refusing to read Secret Manager payloads or guess across `1:N` / `M:1` shared Service Accounts leaves some database or bucket connections unlinked on initial generation. *Mitigation:* Attach `Unresolved Links: N` badges and candidate lists to `<object>` metadata (`Ctrl+M`), clear the badge once the user draws a manual arrow in Draw.io, and preserve manual arrows across updates.
*   **Partial Permissions in Corporate Landing Zones:** Consultants restricted to workload projects receive `403` errors on Organization hierarchies, VPC Service Controls (VPC-SC) policies, Shared VPC Host Projects, or optional enrichers. *Mitigation:* `gcp-arch preflight` probes permissions upfront; `generate` records gaps in `CoverageLedger` (`IAM_DENIED`, `VPC_SC_BLOCKED`, `UNKNOWN_ORG_REQUIRED`), renders unreadable targets as dashed `External Referenced` boxes, and unifies split external project numbers/IDs via `--project-alias`.
*   **Enterprise Asset Volume, API Rate Ceilings, and `--org` Fan-Out:** Unfiltered CAI `ListAssets` enforces a 100 requests/minute ceiling per quota project, high-QPS BigQuery projects can exceed 20 job-history pages within 7 days, and `--org` scopes with hundreds of projects multiply per-project Monitoring/BigQuery calls. *Mitigation:* Throttle CAI to `<= 80` RPM (`page_size = 1000`), project/strip child structs on stream, bound `bigquery.jobs.list` (`<= 20` pages/project) and regional `RagCorpora` (`<= 50` calls) with explicit `CoverageLedger` truncation warnings, skip empty projects in Monitoring (`<= 10` QPS), and scope interactive consultant runs primarily to project lists or workload folders (`<= 50` active projects) backed by `--save-snapshot`.
*   **Credential Exposure in Cloud Metadata and HTML Injection in Draw.io Labels:** Workloads occasionally leak credentials inside plaintext `env` URLs, VM `startup-script` metadata, or SQL query text, while cloud resource names may contain `<`, `>`, or `"`. *Mitigation:* Strip URL `userinfo`/query strings, raw `env` values, `startup-script` bodies, and raw SQL text during ingestion before populating `RawInventory` or `.snap` files; HTML-escape all strings rendered into Draw.io labels; and reject `<script>`, `<foreignObject>`, event attributes, and external URLs in custom catalog SVGs.
*   **Draw.io Save Rewriting & Uncataloged GCP Releases:** Draw.io compresses `<diagram>` payloads by default, strips `<mxStylesheet>` blocks, and rewrites `<UserObject>` to `<object>` on save, while GCP periodically adds new resource types. *Mitigation:* Inline icon data URIs/stencils directly in vertex `style`, emit `<mxfile compressed="false">`, decode compressed `<diagram>` bodies on read, accept `<object>` and `<UserObject>` via lossless DOM parsing, and group unknown asset types into `Unclassified Service` cards extensible via `.gcp-arch/catalog/*.yaml`.

## Goals

*   **Zero-Dependency Single Binary:** Ship one cross-platform static Go executable (`CGO_ENABLED=0`) requiring no local Java, Node.js, Python, or Graphviz runtime.
*   **Live Read-Only GCP Discovery Within Free Quotas:** Discover compute, container, networking, perimeter, database, data/streaming, and Vertex AI resources via local `gcloud` ADC within `<= 80` RPM on CAI and `< 512 MB` Go live `HeapAlloc` (tested up to 150,000 raw sub-resources).
*   **Sub-Second Snapshot Reloads:** Save sanitized compressed inventory snapshots locally (`0600` permissions) and recompile diagrams from snapshots in `< 500 ms` for project graphs up to 40 composite nodes per tab.
*   **Complete Service Visibility Without Silent Omission:** Collapse multi-resource GCP services into logical cards, suppress control-plane plumbing APIs, and render uncataloged workload resources as aggregated `Unclassified Service` cards configurable via `.gcp-arch/catalog/*.yaml`.
*   **Conservative Three-Tier Edge Inference:** Emit solid edges for verified structural pointers, VPC-scoped endpoint matches, and BigQuery SQL lineage, emit dashed edges only on bidirectional `1:1` in-scope empirical matches, and omit `1:N` / `M:1` ambiguous edges while recording candidates in Draw.io `Ctrl+M` metadata and `gcp-arch inspect`.
*   **Non-Destructive Day-2 `.drawio` Updates:** Preserve existing card coordinates, user-edited titles/styles, manual arrows, notes, and user card deletions on subsequent runs, updating live badges via Draw.io placeholders and staging newly added cloud resources in `New Resources (Drag to Place)` unless `--relayout` is passed.
*   **Explicit Partial-Permission Operation:** Degrade predictably on project-scoped credentials by rendering unreadable cross-project dependencies as dashed `External Referenced` containers and emitting a structured Coverage & Degradation Ledger.

## Non-Goals

*   **Generative AI or Runtime Expression Engines:** `gcp-arch` will not invoke LLMs, embed `cel-go`, or execute dynamically generated REST crawlers.
*   **Additional Diagram Exporters or Parallel Override Files:** `gcp-arch` will not emit `.d2`, standalone `.svg`, `.c4`, or `.html` files, nor maintain a separate `arch.yaml` override DSL alongside `.drawio`.
*   **Secret Payload Reads or Paid GCP Tiers:** `gcp-arch` will not call `secretmanager.versions.access`, require Security Command Center Premium `RELATIONSHIP` APIs, or require VPC Flow Logs.
*   **Infrastructure-as-Code Parsing:** `gcp-arch` will not read Terraform state, HCL, Pulumi, or Helm files.
*   **In-Cluster Kubernetes or Firewall Rule Graphs:** `gcp-arch` will not expand Kubernetes `Deployment`/`Pod` topologies or individual firewall rules onto diagram canvases.

## Open Questions

*   None.

## Critical User Journeys (CUJs)

### CUJ 1: First-Time Architecture Discovery on a Corporate Client Project

*   **Given:** A consultant is authenticated via `gcloud auth application-default login` with `roles/cloudasset.viewer`, `roles/monitoring.viewer`, `roles/bigquery.resourceViewer`, `roles/dataflow.viewer`, and `roles/aiplatform.viewer` on two client workload projects (`app-prod`, `data-prod`) inside a Shared VPC Landing Zone, without access to the Organization node or Shared VPC Host Project.
*   **When:** The consultant runs `gcp-arch preflight --project app-prod,data-prod` followed by `gcp-arch generate --project app-prod,data-prod --save-snapshot .gcp-arch/prod.snap -o client-arch.drawio`.
*   **Then:**
    *   `preflight` reports granted workload permissions and marks Organization/VPC-SC and Shared VPC Host Project scopes as unreadable without aborting.
    *   `generate` writes `client-arch.drawio` with two tabs (`L1-Platform-Network` and `L2-Architecture`), an on-canvas Legend & Coverage Box, and dashed `External Referenced` boxes for the Shared VPC Host.
    *   Verified structural, SQL-lineage, and bidirectional `1:1` empirical edges are drawn; multi-target Secret Manager links display `Unresolved Links: N` badges with candidates listed in `Ctrl+M` metadata; and `stderr` prints the Coverage & Degradation Ledger.

### CUJ 2: Human Curation in Draw.io Followed by Non-Destructive Day-2 Update

*   **Given:** The consultant opens `client-arch.drawio` in Draw.io, repositions cards, renames two card titles (keeping their `%gcpBadges%` placeholder), deletes a test bucket card, adds a text note, draws two manual arrows to confirm `Unresolved Links` candidates, and saves the file. One week later, the client adds a `Cloud Run` worker, a `Pub/Sub` topic, and five tables to an existing 50-table `BigQuery Dataset`, and deletes an unused `Memorystore` instance.
*   **When:** The consultant runs `gcp-arch generate --project app-prod,data-prod -o client-arch.drawio`.
*   **Then:**
    *   All existing cards retain their exact `(X, Y, W, H)` coordinates; the two renamed titles, text note, and two manual arrows remain intact; the deleted test bucket stays suppressed; and `Unresolved Links` badges clear for the manually connected targets.
    *   The `BigQuery Dataset` card refreshes its `gcpBadges` placeholder to `Tables: 55`, the deleted `Memorystore` card is removed (or badged `REMOVED IN GCP` if linked by a manual arrow), and the new `Cloud Run` worker and `Pub/Sub` topic appear inside `New Resources (Drag to Place)` with orthogonal connectors attached.

### CUJ 3: Adding Support for a Newly Released GCP Service Without Recompiling

*   **Given:** A captured snapshot contains two instances of a newly launched GCP resource type (`newservice.googleapis.com/Cluster`) absent from the embedded catalog.
*   **When:** The consultant runs `gcp-arch generate --from-snapshot .gcp-arch/prod.snap -o client-arch.drawio`, observes an aggregated `Unclassified Service (newservice.googleapis.com/Cluster ×2)` card, writes a static YAML rule in `.gcp-arch/catalog/newservice.yaml`, and re-runs `gcp-arch generate --from-snapshot .gcp-arch/prod.snap -o client-arch.drawio`.
*   **Then:** On the first run, the uncataloged resource is surfaced as an unclassified summary card rather than omitted. On the second run (`< 500 ms` for `<= 40` composite nodes/tab), `gcp-arch` loads `.gcp-arch/catalog/newservice.yaml` at runtime and stages the two typed cluster cards with their configured display type, icon, layer role, and outbound URI edges.

## Technical Plan (Architecture)

### 1. System Pipeline and Package Boundaries

`gcp-arch` is structured as a four-stage deterministic pipeline compiled into a single static Go executable (`CGO_ENABLED=0`, Go 1.24+):

```text
+-----------------------------------------------------------------------------------+
| 1. Ingest (`internal/ingest`)                                                     |
|  - ADC Auth + Quota-Project Guard + Global Token Bucket (<= 80 RPM / quota proj)  |
|  - CAI ListAssets(RESOURCE, opt ACCESS_POLICY, scoped IAM_POLICY) @ server time   |
|  - Single-pass child projection/folding + URL/env/SQL credential sanitization     |
|  - Read-Only Enrichers: BigQuery jobs.list, Dataflow jobs.get, Monitoring, RAG    |
|  - Gzip/JSON Snapshot Serializer & Loader (--save-snapshot / --from-snapshot)     |
+-----------------------------------------+-----------------------------------------+
                                          | RawInventory + CoverageLedger
                                          v
+-----------------------------------------------------------------------------------+
| 2. Catalog & Compaction (`internal/catalog`, `internal/compiler`)                 |
|  - Two-tier static catalog: embedded YAML base + `.gcp-arch/catalog/*.yaml`       |
|  - AIP-122 URI canonicalizer + ProjectID <-> ProjectNumber index + --project-alias|
|  - 15 native Go multi-resource graph compaction passes                            |
|  - Plumbing API filter + aggregated `Unclassified Service (<type> xN)` cards      |
|  - Three-tier edge engine: Tier 1 Structural/SQL, Tier 2 1:1 Empirical, Tier 3 1:N|
+-----------------------------------------+-----------------------------------------+
                                          | ir.Graph
                                          v
+-----------------------------------------------------------------------------------+
| 3. View Projection & Pure-Go ELK Layout (`internal/layout`)                       |
|  - Fixed 2-tab deliverable: `L1-Platform-Network` and `L2-Architecture`           |
|  - Pre-ELK deterministic box sizer + Tarjan/DFS cycle routing onto `SOUTH` ports  |
|  - Native Go ELK (`github.com/d2lang/elk-go` v0.2.0, `layered`, `randomSeed = 1`) |
+-----------------------------------------+-----------------------------------------+
                                          | []LayoutedPage
                                          v
+-----------------------------------------------------------------------------------+
| 4. Draw.io Serializer & Lossless 3-Way XML Merger (`internal/drawio`)             |
|  - Uncompressed `<mxfile compressed="false">` + inline `style` SVG/GCP2 icons     |
|  - `<object placeholders="1" gcpBadges="...">` cards + Legend & Coverage Box      |
|  - `beevik/etree` 3-way merge preserving coordinates, edits, manual arrows, and   |
|    deletions while staging newly added cloud nodes in `New Resources`             |
+-----------------------------------------------------------------------------------+
```

### 2. Command-Line Interface Contract (`cmd/gcp-arch`)

Built on `github.com/spf13/cobra`, the CLI exposes three subcommands:

1.  **`gcp-arch preflight`**: Validates local ADC credentials, checks quota-project API readiness, executes lightweight read probes across target scopes (`Cloud Asset Inventory`, `Cloud Monitoring`, `BigQuery Jobs`, `Dataflow`, `Vertex AI RAG`, `Access Context Manager`), and outputs granted vs. missing roles with remediation `gcloud` commands (`--json` optional).
2.  **`gcp-arch generate`**: Collects live inventory (when `--org`, `--folder`, or `--project` is supplied) or loads `--from-snapshot <FILE>`, optionally writes `--save-snapshot <FILE>`, compiles the IR, and writes or updates `-o / --out <FILE.drawio>`. Accepts `--view <all|l1-platform-network|l2-architecture>` (default `all`), repeatable `--project-alias <NUMBER=ID>`, `--catalog-dir <DIR>` (default `.gcp-arch/catalog`), `--relayout`, `--restore-deleted`, and `--no-backup`.
3.  **`gcp-arch inspect`**: Compiles the IR from live scopes or `--from-snapshot <FILE>` and outputs (table or `--json`) compacted nodes, synthesized foundation models, drawn edges with evidence, `1:N` / `M:1` unresolved candidates, suppressed plumbing APIs, `Unclassified Service` counts, and `CoverageLedger` entries.

Standardized POSIX exit codes across all subcommands: `0` on full or partial-permission target completion (printing `CoverageLedger` warnings to `stderr`), `1` on ADC, `quota_project` (`SERVICE_DISABLED`, `BILLING_DISABLED`, `USER_PROJECT_DENIED`), network I/O, or filesystem failure, and `2` on invalid CLI flag combinations or invalid `.gcp-arch/catalog/*.yaml` schema.

### 3. Live Ingestion, Stream-Time Projection, and Sanitization (`internal/ingest`)

*   **ADC & Quota-Project Guard:** Preserves the active ADC `quota_project_id` unless `--quota-project` is explicitly passed (never falling back to a target workload project). Distinguishes fatal caller quota-project errors (exit `1`) from target-scope `IAM_PERMISSION_DENIED` or `VPC-SC` blocks (`SECURITY_POLICY_VIOLATED` carrying `vpcServiceControlsUniqueIdentifier`), which record degraded scope entries in `CoverageLedger`.
*   **Server-Pinned CAI Pagination (`<= 80` RPM):** Calls `ListAssets` (`page_size = 1000`) omitting `readTime` on the initial page to avoid workstation clock-skew rejections, then pins the server-returned `ListAssetsResponse.ReadTime` across all subsequent `RESOURCE`, organization-level `ACCESS_POLICY` (marking VPC-SC `UNKNOWN_ORG_REQUIRED` when organization access is unavailable), and asset-type-filtered `IAM_POLICY` streams.
*   **Order-Independent Child Projection & Credential Stripping (`< 512 MB` `HeapAlloc`):** Because CAI does not order parent assets before child assets, `internal/ingest` accumulates child counts, BigQuery `Table`/`Routine`/`Model` external connection pointers (`BigLake`, federated SQL, remote `BQML`), and minimal `Revision`/`Version` projections in parent-URI maps reconciled after streaming completes. During this single pass, URLs have `userinfo`, query strings, and fragments stripped, and raw `env` values, VM `startup-script` bodies, and raw SQL text are discarded so sensitive strings never enter `RawInventory` or `.snap` files.
*   **Bounded Read-Only Enrichers:** Executed concurrently across active projects/regions observed in CAI: (1) BigQuery `jobs.list` (7-day window, `<= 20` pages/project, zero SQL slot cost, logging a `CoverageLedger` truncation warning if page 20 is reached before 7 days), (2) Dataflow `jobs.get` (`JOB_VIEW_DESCRIPTION` on running jobs), (3) Cloud Monitoring `ListTimeSeries` (`86400s` server-side `ALIGN_SUM` at `<= 10` QPS for `consumed_api` including `method` on `aiplatform.googleapis.com`, plus `PublisherModel` invocations and `request_type="dedicated"` throughput), and (4) regional Vertex AI `RagCorpora` listings (`<= 50` calls cap across observed active regions, logging truncation if hit).

### 4. Static Catalog and 15 Native Go Compaction Passes (`internal/catalog`, `internal/compiler`)

*   **Two-Tier Static Catalog:** Base YAML definitions and icons are embedded in the binary and overlaid at runtime by `.gcp-arch/catalog/*.yaml` (`schema_version: v1`). The catalog maps Google API Improvement Proposal 122 (`AIP-122`) `asset_type` strings and `*.googleapis.com` service hosts into four dispositions: `ARCHITECTURAL_NODE`, `ABSORBED_CHILD`, `TRANSVERSAL_COUNTER`, and `IGNORED_PLUMBING` (routing `modelarmor.googleapis.com`, `Secret Manager`, `Cloud KMS`, `DLP`, CI/CD, and observability APIs to the detached transversal band while suppressing plumbing APIs such as `sts` and `iamcredentials`).
*   **15 Native Go Multi-Resource Compaction Passes:** Run in fixed order over $O(N)$ URI and label indexes:
    1.  *Cloud Load Balancing:* Collapses forwarding rules, addresses, target proxies, TLS certificates, `UrlMap`, backend services/buckets, Cloud Armor policies, and network endpoint groups into one `Cloud Load Balancer` card forwarding path rules onto backend edges.
    2.  *Apigee & API Gateway:* Consolidates Apigee organization/instance/environment/proxy hierarchies and API Gateway `Api`/`ApiConfig`/`Gateway` triplets.
    3.  *Hybrid Connectivity & Network Connectivity Center (NCC):* Consolidates NCC `Hub` + `Spoke`s, `VpnGateway` + `VpnTunnel` + `Cloud Router` BGP sessions, and `InterconnectAttachment`s.
    4.  *Compute MIGs & Cloud Batch:* Absorbs templates, autoscalers, health checks, and member VMs into `InstanceGroupManager`; absorbs Batch worker VMs into `Batch Job`.
    5.  *GKE Cluster Consolidation:* Absorbs `NodePool`s, MIGs, `gke-*` VMs, Fleet memberships, and `k8s.io` workloads into one `GKE Cluster` card on `L2` (`Autopilot/Standard`, `Namespaces: N`, `Workloads: M`), retaining classic 3-hop GSA Workload Identity, direct Workload Identity Federation (`principal://...svc.id.goog/...`), and `NodePool` SA fallback for outbound cluster edge attribution.
    6.  *Cloud Run, Cloud Run Functions v2 & App Engine:* Merges `CloudFunction` v2 + underlying `run.googleapis.com/Service` + `Eventarc Trigger` into `Cloud Run Function`; joins retained active `Revision`/`Version` metadata via `Service.traffic[]`.
    7.  *Managed Databases & Storage Pools:* Consolidates primary + read replicas/pools in `Cloud SQL` and `AlloyDB`, `Instance` + `Database` in `Spanner`, `Instance` + `Cluster` + `AppProfile` + table counts in `Bigtable`, and `StoragePool` + `Volume` in `NetApp Volumes`, adding a Customer-Managed Encryption Key (`CMEK`) badge when `kmsKeyName` is set.
    8.  *Cloud Composer Absorption:* Absorbs Composer's GKE cluster (`goog-composer-environment`), `NodePool`s, VMs, and staging/DAG GCS bucket (`config.dagGcsPrefix`) into one `Cloud Composer` card.
    9.  *Dataproc Clusters & Batches:* Absorbs master/worker VMs, internal MIGs, and staging buckets into `Dataproc Cluster`; groups recurring Serverless `Batch` runs by normalized prefix.
    10. *Dataflow Pipelines:* Suppresses `goog-dataflow-job-id` worker VMs; retains active streaming jobs and groups recurring completed batch jobs by normalized pipeline name.
    11. *Datastream CDC:* Absorbs `ConnectionProfile` and `PrivateConnection` into `Stream`, emitting `CDC_REPLICATION` edges.
    12. *BigQuery, BigLake, Dataform & Dataplex:* Uses `Dataset` as the canonical card (`Tables: N`, `BQML: K`); collapses `Dataform` workspaces/invocations into `Repository`; represents `Dataplex` zone membership as dataset/bucket badges.
    13. *Pub/Sub & Managed Kafka:* Keeps `Topic` cards, collapses 1:1 `Subscription`s onto consumer edges while retaining 1:N fan-out `Subscription`s or export sinks (Pub/Sub -> BigQuery/GCS), and rolls Kafka topics into `Managed Kafka Cluster`.
    14. *Vertex AI Agents, Search & Custom Endpoints:* Consolidates `ReasoningEngine` (`Vertex AI Agent Engine`), `DiscoveryEngine` `Engine` + `DataStore`, `Dialogflow CX` `Agent`, `Vertex AI Endpoint` + `DeployedModel` + `Model`, `Vector Search` (`IndexEndpoint` + `DeployedIndex` + `Index`), `RagCorpus`, `FeatureOnlineStore`, and `Document AI Processor`.
    15. *Foundation Model (`PublisherModel`) Synthesis:* Synthesizes one `Vertex AI Foundation Model` card per active `(project, location, model_user_id)` with `model_invocation_count > 0`, attaching `Provisioned Throughput` only when `request_type == "dedicated"`.
*   **Unclassified & External Fallback:** Uncataloged CAI `asset_type`s and active non-plumbing workload APIs lacking CAI assets group into one `Unclassified Service (<name> ×N)` card per project; out-of-scope AIP-122 targets group into dashed `External Referenced` containers (unified via `--project-alias` when configured).

### 5. Three-Tier Edge Resolution Engine (`internal/compiler`)

1.  **Tier 1 — Verified Structural, Network-Scoped Endpoint, and SQL Lineage (`SOLID` edge):**
    *   Typed pointers from CAI, projected BigQuery `Table`/`Routine`/`Model` external configs, Dataflow `displayData`, Eventarc, Push/Export Pub/Sub, Cloud Scheduler, Datastream, Dialogflow webhooks, DiscoveryEngine, and `RagCorpus`.
    *   Sanitized canonical identifiers in workload `env`/args: full `AIP-122` URIs, `gs://<bucket>`, `https://<service>-*.run.app`, `<project>.<dataset>` where `<project>` matches a known project ID, and private endpoint IPs (`AlloyDB`, `Cloud SQL`, `Memorystore`, Private Service Connect [`PSC`]) matched strictly within the source workload's VPC / Shared VPC / Peering network domain.
    *   BigQuery `jobs.list` lineage: emits `Dataset A -> Dataset B` ELT edges directly, and emits `Workload -> Dataset` solid edges when `user_email` / `principal_subject` resolves to **exactly one** in-scope workload (`source_workloads == 1`).
2.  **Tier 2 — Bidirectional `1:1` In-Scope Empirical Correlation (`DASHED` edge):**
    *   Evaluated **before** child bucket/resource absorption on raw in-scope candidate targets: emits a dashed edge if and only if active 7-day `consumed_api` traffic (`request_count > 0`) maps **one unique in-scope source workload** (`source_workloads_using_SA == 1`) to **one unique raw in-scope candidate target** (`raw_candidate_targets == 1`).
    *   For synthesized `PublisherModel` cards, requires `aiplatform.googleapis.com` `consumed_api` traffic on generative/embedding methods (`*GenerateContent*` / `*EmbedContent*`) from exactly one in-scope workload SA, zero non-SA callers, and `1` synthesized model in that project.
3.  **Tier 3 — Ambiguous `1:N` / `M:1` Candidates (`NO` edge drawn):**
    *   Whenever `source_workloads > 1` or `raw_candidate_targets > 1` (or when a workload mounts `Secret Manager` references alongside unlinked private datastores), zero arrows are drawn; candidate target URIs and metric evidence are recorded in `Node.UnresolvedCandidates` (`Ctrl+M` metadata and `gcp-arch inspect`) and surfaced via an `Unresolved Links: N` badge.

### 6. Fixed Two-Tab Projection, Pure-Go ELK Layout, and 3-Way `.drawio` Merge (`internal/layout`, `internal/drawio`)

#### A. Fixed Two-Tab Hierarchy and Cycle Routing

*   **Deterministic Two-Tab Structure:** Always emits `L1-Platform-Network` and `L2-Architecture` (unless narrowed via `--view`), eliminating node-count threshold tab splitting:
    1.  `L1-Platform-Network`: Nested `Organization` -> `Folder` -> `Project` -> `VPC` -> `Region` -> `Subnet` containers, Shared VPC links, `NCC`, `HA VPN` / `Interconnect`, `Cloud Router` / `NAT`, `PSC` endpoints, and dashed `External Referenced` boxes. `VPC-SC` membership is rendered directly on each protected `Project` container via a red dashed perimeter border and `VPC-SC: <perimeter>` header badge.
    2.  `L2-Architecture`: Flat service cards on root layer `parent="1"` carrying color-coded `ProjectID`, `Shared VPC`, and `VPC-SC` header pills, plus a detached zero-edge **Bottom Transversal Platform Band** (Security/Identity including `Model Armor`, CI/CD & MLOps, and Governance/Observability).
*   **Pure-Go ELK Layout (`github.com/d2lang/elk-go` v0.2.0):** Runs `layered` (`RIGHT`, `ORTHOGONAL`, `randomSeed = 1`) without rigid product-column partitioning, pinning `FIRST` layer only on external/ingress entry roots. Cycles (including Agent-to-Agent [`A2A`] and event loops) are detected via Tarjan/DFS pre-pass, temporarily reversed onto `FIXED_SIDE` `SOUTH` ports (`WEST`/`EAST` explicit ports on forward edges), and restored post-layout, with Lowest Common Ancestor (`LCA`) compound coordinates translated to root canvas space.

#### B. Lossless Draw.io (`mxGraphModel`) Contract and 3-Way XML Merge

*   **Self-Contained Uncompressed XML (`< 2 MB`):** Emits `<mxfile compressed="false">` with inline icon styles on each vertex (avoiding `<mxStylesheet>`, which Draw.io strips on `Ctrl+S`), HTML-escaped `<object placeholders="1" label="<b>Title</b><br>%gcpBadges%" gcpBadges="...">` wrappers so live badges refresh without touching user-edited titles, an on-canvas **Legend & Coverage Box**, and per-tab `gcpArchManifest` JSON on root `<object id="0">`.
*   **Lossless `beevik/etree` 3-Way Update (Default when target `.drawio` exists and `--relayout` is false):** Inflates compressed `<diagram>` payloads if re-saved compressed in Draw.io, accepts both `<object>` and `<UserObject>`, preserves all unknown XML attributes verbatim, and reconciles vertices and edges per tab:

| Element State (`Prior Tab Manifest` × `Existing Tab XML` × `Current Tab IR`) | Deterministic Update Action |
| :--- | :--- |
| Vertex or edge lacks `gcpArchManaged="1"` (user arrow, note, callout, layer) | **Preserve verbatim** (exact DOM element, attributes, geometry, and waypoints kept). |
| Vertex ID in Prior Manifest, **deleted** in XML, still in Current IR | **Suppress** (keep hidden unless `--restore-deleted` or `--relayout` is set). |
| Surviving managed vertex (untouched or user-edited title/style) | **Freeze `(X, Y, W, H)`**; refresh `gcpBadges` and metadata attributes (subtracting `Unresolved Links` targets connected by unmanaged user edges); update `label`/`style` only if normalized XML still matches prior compiler baseline. |
| Vertex ID in XML, **absent** from Current IR (deleted in GCP) | **Delete** if unreferenced; if linked by an unmanaged user edge, retain at `(X, Y)` with `REMOVED IN GCP` badge. |
| Vertex ID **new** in Current IR (not in Prior Manifest) | **Stage at `(maxX + 80, minY)`** inside `New Resources (Drag to Place)` without moving existing canvas cards. |
| Managed edge (`gcpArchManaged="1"`) | **Drop** if deleted by user in XML, absent from Current IR, or touching a suppressed vertex; **preserve** user-edited style/waypoints if modified; **strip** Day-1 ELK `<Array as="points">` if either endpoint moved; **emit** newly discovered edges with normalized ports and no frozen waypoints. |

## Alternatives Considered

### Alternative A: Heuristic `IAM + Service-Level Cloud Monitoring (consumed_api)` Multi-Target Edge Promotion

*   **Pros:** Connects workloads to buckets, datasets, and queues automatically when addresses live in Secret Manager or private DNS.
*   **Cons:** `serviceruntime.googleapis.com/api/request_count` (`consumed_api`) aggregates by API host (`storage.googleapis.com`, `bigquery.googleapis.com`), not by target resource, producing false arrows whenever a Service Account holds access to multiple resources or is shared across multiple workloads.
*   **Reason for Rejection:** False connections in client documentation are harder to spot and verify than missing arrows; multi-target (`1:N` / `M:1`) candidates are instead surfaced as `Unresolved Links` metadata while dashed empirical arrows require bidirectional `1:1` in-scope cardinality.

### Alternative B: Parsing Terraform State or Infrastructure-as-Code Alongside Live GCP APIs

*   **Pros:** Extracts explicit module wiring such as secret-injected database hosts without manual Draw.io confirmation.
*   **Cons:** IaC files diverge from deployed cloud state whenever console changes occur, plans are unapplied, or consultants lack repository access.
*   **Reason for Rejection:** Mixing IaC with live APIs masks configuration drift and can render unprovisioned resources in `As-Built` deliverables.

### Alternative C: Runtime Self-Mutating Imperative Code or Runtime LLM Inference During `generate`

*   **Pros:** Attempts to interpret newly launched GCP resource schemas on the fly.
*   **Cons:** Executing LLM-generated Go plugins or Python scripts built from cloud metadata introduces prompt-injection code execution risks alongside active ADC credentials, breaks static single-binary distribution, and forfeits deterministic output.
*   **Reason for Rejection:** Runtime code generation and runtime model calls violate security, portability, and determinism requirements.

### Alternative D: Offline LLM Rule Synthesis (`gcp-arch learn`), `cel-go` Graph DSL, and Dynamic `GET *.googleapis.com` Crawlers

*   **Pros:** Automates catalog rule authoring from Google API Discovery Documents and Vertex AI.
*   **Cons:** Multi-resource compactions (7-part Load Balancers, Cloud Composer GKE absorption) require relational joins rather than single-record CEL predicates ($O(N^2)$ nested CEL comprehension evaluation and sparse-protobuf `no_such_key` errors). Calling Vertex AI against an audited client project increments `PublisherModel` metrics and injects a phantom Gemini card into the client diagram; raw Discovery Documents exceed 5–11 MB; map keys (`labels`, `annotations`) carry untrusted strings; and generic `GET *.googleapis.com` templates expose secret/data endpoints (`secretmanager...:access`, `bigquery.../data`) and 40-region `429` rate-limit failures.
*   **Reason for Rejection:** Native Go compaction passes paired with `.gcp-arch/catalog/*.yaml` static files and `Unclassified Service` summary cards handle new GCP services without LLM billing, quota, or secret-access risks.

### Alternative E: Executing `elkjs` Inside `wazero` WebAssembly + `QuickJS.wasm` (or Importing `d2lib`)

*   **Pros:** Avoids external Node.js or Java installations while reusing the JavaScript build of ELK.
*   **Cons:** `elkjs` is a ~2.4 MB JavaScript bundle rather than `.wasm` bytecode, requiring an embedded C-to-WASI JavaScript engine (`QuickJS.wasm`) with custom stack limits and Promise queue pumping, whereas full `d2lib` couples layout to D2's internal graph and SVG packages.
*   **Reason for Rejection:** `github.com/d2lang/elk-go` (`v0.2.0`) executes ELK natively in pure Go (`CGO_ENABLED=0`) without WASI/JS virtualization or D2 renderer dependencies.

### Alternative F: Maintaining Multiple Diagram Exporters (`.d2`, `.svg`, `.drawio`, `.c4`, `.html`) and Parallel `arch.yaml` Overrides

*   **Pros:** Emits standalone web previews from the CLI and stores text overrides in Git alongside `.drawio`.
*   **Cons:** Multiplies exporter maintenance fivefold and creates dual-source-of-truth precedence conflicts when `arch.yaml` renames, ignores, or manual edges disagree with edits saved in `.drawio`.
*   **Reason for Rejection:** `.drawio` with `<object>` metadata and 3-way XML merge serves as the single interactive editing, inspection, and PDF/SVG/PNG export artifact.

### Alternative G: Full Overwrite or Global ELK `semiInteractive` Re-Layout on Day-2 `.drawio` Updates

*   **Pros:** Avoids 3-way XML merge logic (overwrite) or clears mid-canvas space automatically for new nodes (`semiInteractive`).
*   **Cons:** Overwriting destroys manual card alignments, titles, notes, and arrows, while `semiInteractive` global ELK re-layout recalculates `(X, Y)` coordinates across the entire tab whenever one node is added.
*   **Reason for Rejection:** Freezing existing card coordinates and staging newly discovered cloud nodes in `New Resources (Drag to Place)` preserves manual layout positioning (`--relayout` remains opt-in).

### Alternative H: Rigid 6-Column Product Partitioning, Dynamic `> 30` Tab Splitting, and Nested `L2` Project Boxes

*   **Pros:** Assigns fixed horizontal columns per product family and splits large graphs into extra tabs automatically.
*   **Cons:** Event loops (`Cloud Run -> Pub/Sub -> Cloud Run`) and >6-hop pipelines break fixed product columns; nested `Project` rectangles in `L2` overlap when cross-project flows interleave; and crossing `30 <-> 31` nodes between runs creates or deletes tabs during Day-2 merges.
*   **Reason for Rejection:** Native ELK `layered` ranking with `SOUTH`-port cycle routing, flat `L2` cards with project header pills, and a fixed 2-tab structure keep layout and Day-2 merges predictable.

--------------------------------------------------------------------------------

## Non-Functional Requirements

### Security and Client Data Confidentiality

*   Authenticates exclusively via local read-only ADC scopes; never calls write APIs or secret/data-plane read endpoints (`secretmanager.versions.access`, GCS object reads, BigQuery table row reads), and makes zero network calls outside Google Cloud's official read-only APIs.
*   Strips URL `userinfo`/query strings, raw `env` values, VM `startup-script` bodies, and raw SQL text during ingestion; writes `--save-snapshot` files with `0600` permissions and performs zero implicit disk caching when `--save-snapshot` is omitted.
*   HTML-escapes all strings interpolated into Draw.io `html="1"` labels and rejects `<script>`, `<foreignObject>`, `on*` handlers, and external URLs in custom catalog SVGs.

### Performance, Memory, and Quota Ceilings

*   Caps CAI `ListAssets` at `<= 80` RPM per `quota_project` (`page_size = 1000`) and Cloud Monitoring `ListTimeSeries` at `<= 10` QPS (`86400s` `ALIGN_SUM`).
*   Maintains Go live heap `HeapAlloc < 512 MB` on scopes up to 150,000 raw sub-resources via stream-time child projection and schema stripping.
*   Completes `--from-snapshot` compilation, layout, and `.drawio` output in `< 500 ms` for graphs up to 40 composite nodes per tab, keeping `.drawio` files `< 2 MB`.

### Determinism, Schema Compatibility, and Atomic Updates

*   Produces byte-identical `.drawio` XML for identical `RawInventory`, catalog, flags, and prior `.drawio` inputs (`elk.randomSeed = 1`, sorted slices, canonicalized DOM output).
*   Validates `schema_version: v1` on `.gcp-arch/catalog/*.yaml` files at startup (failing with exit code `2` on invalid schemas).
*   Writes updated `.drawio` files to a temporary sibling path, backs up the prior target to `<target>.bak` (unless `--no-backup` is set), and renames atomically.

## Future Improvements

*   **Opt-In VPC Flow Logs / Cloud Trace Enricher:** Resolve ambiguous `1:N` private-IP database links automatically in environments that already export Flow Logs or Cloud Trace spans.
*   **Snapshot Diff Command (`gcp-arch diff`):** Compare two `.snap` files across dates and output a Markdown topology diff alongside the updated `.drawio` staging tray.

--------------------------------------------------------------------------------

## Detailed Implementation Plan

### Incremental Delivery Phases

Each phase leaves the repository buildable (`CGO_ENABLED=0 go build ./...`) and passing race-enabled tests (`CGO_ENABLED=1 go test -race ./...`). Third-party dependencies are added to `go.mod` in the phase that first imports them so `go mod tidy` succeeds at every checkpoint:

*   **Phase 1 — Domain Model, Catalog & Streaming Ingestion:** Create `go.mod`, `internal/ir/model.go`, `internal/catalog/*`, and `internal/ingest/*` (ADC preflight, `80 RPM` CAI stream folder/sanitizer, read-only enrichers, and gzip/JSON snapshot persistence).
*   **Phase 2 — Multi-Resource Compaction & Three-Tier Edge Engine:** Create `internal/compiler/compact.go`, `internal/compiler/edges.go`, and `internal/compiler/compiler_test.go`.
*   **Phase 3 — Two-Tab Projection, Pure-Go ELK Layout & Lossless Draw.io Merger:** Add `github.com/d2lang/elk-go v0.2.0` and `github.com/beevik/etree`, and create `internal/layout/*` and `internal/drawio/*`.
*   **Phase 4 — CLI Subcommands, Documentation & End-to-End Suite:** Add `github.com/spf13/cobra`, create `cmd/gcp-arch/main.go`, `cmd/gcp-arch/main_test.go`, `cmd/gcp-arch/e2e_test.go`, `testdata/enterprise_reference.snap.json`, and `README.md`.

### Target Files to Create/Modify

All paths below are relative to the repository root:

#### 1. `go.mod` and `go.sum` (Create)

*   **Type / Scope:** Go Module Manifest (`go 1.24`, module `gcp-arch`).
*   **Proposed Changes:** Pin direct dependencies incrementally across Phases 1–4 (`go mod tidy` clean at each phase):
    *   *Phase 1:* `cloud.google.com/go/asset`, `cloud.google.com/go/monitoring`, `cloud.google.com/go/accesscontextmanager`, `google.golang.org/api`, `google.golang.org/genproto/googleapis/rpc`, `google.golang.org/grpc`, `google.golang.org/protobuf`, `golang.org/x/oauth2`, `golang.org/x/sync`, `golang.org/x/time`, `github.com/tidwall/gjson`, `gopkg.in/yaml.v3`.
    *   *Phase 3:* `github.com/d2lang/elk-go v0.2.0`, `github.com/beevik/etree`.
    *   *Phase 4:* `github.com/spf13/cobra`.

#### 2. `README.md` (Create)

*   **Type / Scope:** User & Operations Documentation.
*   **Proposed Changes:** Document binary build/install, minimum read-only IAM roles (`roles/cloudasset.viewer`, `roles/monitoring.viewer`, `roles/bigquery.resourceViewer`, optional `roles/dataflow.viewer` and `roles/aiplatform.viewer` on target projects; optional `roles/accesscontextmanager.policyReader` at Org; `roles/serviceusage.serviceUsageConsumer` on `--quota-project`), CLI flags and exit codes (`0`, `1`, `2`), Day-2 Draw.io merge behavior (`%gcpBadges%` placeholder and `New Resources` tray), and the `.gcp-arch/catalog/*.yaml` `v1` extension schema.

#### 3. `internal/ir/model.go` and `internal/ir/model_test.go` (Create)

*   **Type / Scope:** Go Intermediate Representation & Canonical URI Helpers.
*   **Proposed Changes:** Define complete cross-package structs and closed enum constants:
    *   Enums: `Family` (`COMPUTE`, `CONTAINER`, `NETWORK`, `SECURITY`, `DATABASE`, `STORAGE`, `ANALYTICS`, `MESSAGING`, `AI_AGENT`, `AI_DATA`, `AI_MODEL`, `INTEGRATION`, `TRANSVERSAL`), `LayerRole` (`EXTERNAL_SOURCE`, `INGRESS_EDGE`, `COMPUTE_ORCHESTRATION`, `MESSAGING_PIPELINE`, `STATEFUL_STORAGE`, `CONSUMPTION_INFERENCE`, `TRANSVERSAL`), `EdgeTier` (`TIER1_VERIFIED`, `TIER2_EMPIRICAL_1TO1`), `EdgeCategory` (`INGRESS`, `SYNC_CALL`, `ASYNC_EVENT`, `DATA_FLOW`, `CDC_REPLICATION`, `SQL_LINEAGE`, `AI_INFERENCE`, `AI_GROUNDING`, `A2A`, `NETWORK_LINK`), and `CoverageState` (`OK`, `TRUNCATING_CAP`, `IAM_DENIED`, `VPC_SC_BLOCKED`, `UNKNOWN_ORG_REQUIRED`).
    *   `RawInventory`: `ReadTime time.Time`, `Scopes []string`, `Projects map[string]ProjectMeta` (`ProjectID`, `ProjectNumber`, `DisplayName`, `FolderIDs []string`, `OrgID`, `VPCSCPerimeter`), `Assets []*AssetRecord` (`URI`, `AssetType`, `ProjectID`, `Location`, `DisplayName`, `Labels map[string]string`, `Ancestors []string`, `ProjectedFields map[string]any`, `SanitizedEnvRefs []string`, `SecretRefNames []string`), `EnabledServices map[string][]string`, `IAMBindings []*ScopedBinding`, `Perimeters []*PerimeterRecord`, `FoldedChildren map[string]*ChildFoldSummary` (`Counts map[string]int`, `ExternalURIs []string`), `Revisions map[string]*RevisionSummary`, `DNSRecords []*DNSRecord`, `BQJobLinks []*BQJobLink`, `DataflowLinks []*DataflowLink`, `ConsumedAPIs []*ConsumedAPIStat` (`ProjectID`, `ServiceAccountUniqueID`, `ServiceHost`, `Method`, `CallerKind`, `RequestCount int64`), `PublisherModels []*PublisherModelStat` (`ProjectID`, `Location`, `Publisher`, `ModelID`, `Invocations int64`, `DedicatedThroughput bool`), and `RagCorpora []*RagCorpusRecord`.
    *   `Graph`, `Node` (`ID`, `DisplayName`, `DisplayType`, `PrimaryAssetType`, `Family`, `LayerRole`, `IconKey`, `ProjectID`, `Location`, `Ancestors []string`, `NetworkURI`, `SubnetURI`, `SharedVPCHost`, `VPCSCPerimeter`, `IsSynthesized`, `IsExternalRef`, `IsUnclassified`, `CollapsedAssets []CollapsedAsset`, `Badges []Badge`, `UnresolvedCandidates []CandidateTarget`), and `Edge` (`ID`, `SourceID`, `TargetID`, `Tier`, `Category`, `Label`, `IsCycleBackEdge`, `Evidence []string`).
    *   `CanonicalizeAIP122(raw string, projIdx map[string]string) string`: converts Compute `https://www.googleapis.com/compute/v1/projects/...` `selfLink`s and relative `projects/...` names to `//<service>/projects/<PROJECT_ID>/...`, mapping known `PROJECT_NUMBER` segments (plus `--project-alias` overrides) to `PROJECT_ID`.
    *   `DeterministicCellID(tabID, kind, canonicalURI string) string`: returns `kind + "-" + hex.EncodeToString(sha256.Sum256([]byte(tabID + ":" + kind + ":" + canonicalURI)))[:16]` (`kind` in `"v"`, `"e"`, `"c"`). `model_test.go` tests `selfLink` and project-number canonicalization plus ID stability.

#### 4. `internal/catalog/assets/base_catalog.yaml` (Create)

*   **Type / Scope:** Embedded Declarative GCP Catalog (`schema_version: v1`).
*   **Proposed Changes:** Author the `v1` YAML schema (`schema_version: "v1"`, `assets: []AssetSpec`, `services: map[string]ServiceSpec`):
    *   `AssetSpec` fields: `asset_type`, `disposition` (`ARCHITECTURAL_NODE` | `ABSORBED_CHILD` | `TRANSVERSAL_COUNTER` | `IGNORED_PLUMBING`), `display_type`, `family`, `icon_key`, `default_layer_role`, `badges` (`[]BadgeRule` with `label`, `gjson_path`, `mode`: `"presence"` | `"value"` | `"count"`), and `outbound_ref_paths` (`[]string`).
    *   Populate all `asset_type` entries required by the 15 compaction passes and transversal/plumbing dispositions: Compute/Containers (`compute.googleapis.com/{Instance,InstanceGroupManager,InstanceTemplate,Autoscaler,HealthCheck,Disk,Route,Firewall,Policy,Network,Subnetwork,Address,GlobalAddress,ForwardingRule,GlobalForwardingRule,TargetHttpProxy,TargetHttpsProxy,TargetGrpcProxy,TargetTcpProxy,SslCertificate,UrlMap,BackendService,BackendBucket,SecurityPolicy,NetworkEndpointGroup,Router,VpnGateway,ExternalVpnGateway,VpnTunnel,InterconnectAttachment,ServiceAttachment}`, `container.googleapis.com/{Cluster,NodePool}`, `gkehub.googleapis.com/Membership`, `k8s.io/{Namespace,Service,Node,Pod,Event}`, `networking.k8s.io/Ingress`, `apps.k8s.io/{Deployment,StatefulSet,DaemonSet,ReplicaSet}`, `batch.k8s.io/{Job,CronJob}`, `discovery.k8s.io/EndpointSlice`, `rbac.authorization.k8s.io/{Role,ClusterRole,RoleBinding,ClusterRoleBinding}`, `run.googleapis.com/{Service,Job,Revision}`, `cloudfunctions.googleapis.com/CloudFunction`, `appengine.googleapis.com/{Application,Service,Version}`, `batch.googleapis.com/Job`), Networking/APIs (`dns.googleapis.com/{ManagedZone,ResourceRecordSet}`, `vpcaccess.googleapis.com/Connector`, `networkconnectivity.googleapis.com/{Hub,Spoke}`, `certificatemanager.googleapis.com/{Certificate,CertificateMap}`, `apigateway.googleapis.com/{Api,ApiConfig,Gateway}`, `apigee.googleapis.com/{Organization,Instance,EnvironmentGroup,Environment,EndpointAttachment,ApiProxy}`, `iap.googleapis.com/TunnelDestGroup`), Databases/Storage (`storage.googleapis.com/Bucket`, `file.googleapis.com/Instance`, `netapp.googleapis.com/{StoragePool,Volume}`, `sqladmin.googleapis.com/Instance`, `alloydb.googleapis.com/{Cluster,Instance}`, `spanner.googleapis.com/{Instance,Database}`, `firestore.googleapis.com/Database`, `bigtableadmin.googleapis.com/{Instance,Cluster,AppProfile,Table}`, `redis.googleapis.com/{Instance,Cluster}`, `memcache.googleapis.com/Instance`), Data/Messaging (`pubsub.googleapis.com/{Topic,Subscription}`, `managedkafka.googleapis.com/{Cluster,ConnectCluster}`, `eventarc.googleapis.com/Trigger`, `cloudtasks.googleapis.com/Queue`, `cloudscheduler.googleapis.com/Job`, `workflows.googleapis.com/Workflow`, `connectors.googleapis.com/Connection`, `bigquery.googleapis.com/{Dataset,Table,Routine,Model,RowAccessPolicy}`, `bigqueryconnection.googleapis.com/Connection`, `bigquerydatatransfer.googleapis.com/TransferConfig`, `storagetransfer.googleapis.com/TransferJob`, `dataflow.googleapis.com/Job`, `datastream.googleapis.com/{Stream,ConnectionProfile,PrivateConnection}`, `dataproc.googleapis.com/{Cluster,Batch,AutoscalingPolicy}`, `metastore.googleapis.com/Service`, `composer.googleapis.com/Environment`, `dataform.googleapis.com/{Repository,Workspace,CompilationResult,WorkflowInvocation,WorkflowConfig}`, `dataplex.googleapis.com/{Lake,Zone,Asset,AspectType,EntryGroup}`, `datafusion.googleapis.com/Instance`, `analyticshub.googleapis.com/{DataExchange,Listing}`, `looker.googleapis.com/Instance`), AI/Agents (`aiplatform.googleapis.com/{ReasoningEngine,Endpoint,Model,Index,IndexEndpoint,FeatureOnlineStore,FeatureGroup,FeatureView,PipelineJob,TrainingPipeline,CustomJob,NotebookRuntime,Tensorboard,MetadataStore}`, `discoveryengine.googleapis.com/{Engine,DataStore}`, `dialogflow.googleapis.com/Agent`, `documentai.googleapis.com/{Processor,ProcessorVersion}`), and Transversal (`iam.googleapis.com/{ServiceAccount,Role}`, `secretmanager.googleapis.com/Secret`, `cloudkms.googleapis.com/{KeyRing,CryptoKey}`, `dlp.googleapis.com/{InspectTemplate,DeidentifyTemplate}`, `artifactregistry.googleapis.com/{Repository,DockerImage,Package,Version}`, `cloudbuild.googleapis.com/Trigger`, `clouddeploy.googleapis.com/DeliveryPipeline`, `backupdr.googleapis.com/ManagementServer`, `logging.googleapis.com/LogBucket`, `monitoring.googleapis.com/AlertPolicy`).
    *   Populate `services` classifying default control-plane/companion APIs (`sts`, `iamcredentials`, `oauth2`, `oslogin`, `serviceusage`, `servicemanagement`, `servicecontrol`, `cloudresourcemanager`, `storage-component`, `storage-api`, `bigquerystorage`, `bigquerymigration`, `compute`, `container`, `containerfilesystem`, `Cloud Trace`/`Logging`/`Monitoring`, and `modelarmor.googleapis.com` as `TRANSVERSAL_CONTROL_API`).

#### 5. `internal/catalog/assets/icons.yaml` (Create)

*   **Type / Scope:** Embedded Draw.io Stencil & Inline SVG Map.
*   **Proposed Changes:** Map each `icon_key` in `base_catalog.yaml` either to Draw.io's built-in Google Cloud stencil path (`stencil: "img/lib/gcp2/<name>.svg"`) for standard GCP services or to a self-contained `<svg viewBox="0 0 32 32">...</svg>` string (`svg: "..."`, sanitized with zero semicolons, scripts, or external links) for newer Vertex AI / Agent Engine / MCP / Foundation Model cards plus the 8 generic family fallbacks (`family_compute`, `family_network`, `family_storage`, `family_data`, `family_ai`, `family_integration`, `family_transversal`, `unclassified_warning`).

#### 6. `internal/catalog/catalog.go` and `internal/catalog/catalog_test.go` (Create)

*   **Type / Scope:** Go Catalog Loader, SVG Sanitizer & Overlay Merger.
*   **Proposed Changes:** Embed package-relative `//go:embed assets/base_catalog.yaml assets/icons.yaml`, expose `LoadCatalog(overlayDir string) (*Catalog, error)` validating `schema_version == "v1"` and sanitizing custom overlay SVGs (rejecting `<script>`, `<foreignObject>`, `on*` attributes, and `http://`/`https://` hrefs), and expose `ExtractBadgesAndRefs(spec AssetSpec, projectedJSON []byte)` via `tidwall/gjson`. `catalog_test.go` verifies every `icon_key` resolves, custom SVG sanitization blocks unsafe tags, overlay files merge deterministically, invalid schema versions return an error mapped to exit code `2`, and plumbing APIs never classify as `Unclassified Service`.

#### 7. `internal/ingest/client.go` (Create)

*   **Type / Scope:** Go ADC Client Factory, Error Classifier & Preflight Runner.
*   **Proposed Changes:** Use `golang.org/x/oauth2/google.FindDefaultCredentials` to inspect and preserve ADC `QuotaProjectID` unless `--quota-project` is explicit; implement `ClassifyGCPError(err error)` unpacking `*errdetails.ErrorInfo` (`google.golang.org/genproto/googleapis/rpc/errdetails`) from both gRPC `status.FromError` and REST `*googleapi.Error` (`SERVICE_DISABLED`, `BILLING_DISABLED`, `USER_PROJECT_DENIED`, `IAM_PERMISSION_DENIED`, `SECURITY_POLICY_VIOLATED` + `Metadata["vpcServiceControlsUniqueIdentifier"]`); configure shared `rate.Limiter`s (`80` RPM CAI, `10` QPS Monitoring); and implement `RunPreflight(ctx, clients, scopes)` executing `pageSize=1` list probes on CAI, Monitoring, BigQuery `jobs.list`, Dataflow `jobs.list`, Vertex AI `ragCorpora`, and Org `AccessPolicies`.

#### 8. `internal/ingest/cai.go` (Create)

*   **Type / Scope:** Go CAI Paginated Collector, Child Projector & Sanitizer.
*   **Proposed Changes:** Implement `CollectCAI(ctx, clients, cat, scopes) (*ir.RawInventory, *ir.CoverageLedger, error)`:
    *   Fetch page 1 of `ListAssets(RESOURCE, page_size=1000)` with unset `ReadTime`, capture `resp.ReadTime`, and pass that exact timestamp to all remaining `RESOURCE`, Org `ACCESS_POLICY` (`asset.Ancestors` org extraction; `UNKNOWN_ORG_REQUIRED` on `403` or project-only scope without org visibility), and asset-type-filtered `IAM_POLICY` calls.
    *   Single-pass `ProjectAndSanitizeAsset`: for `bigquery.googleapis.com/{Table,Routine,Model}`, extract `externalDataConfiguration.sourceUris`, `externalDataConfiguration.connectionId`, `remoteFunctionOptions.connection`, and `remoteModelInfo.connection` into `FoldedChildren[datasetURI]` before discarding schemas; for `k8s.io/Namespace` increment `Namespaces` and for `apps.k8s.io/{Deployment,StatefulSet,DaemonSet}` + `batch.k8s.io/{Job,CronJob}` increment `Workloads` on `FoldedChildren[clusterURI]` while dropping `Pod`/`ReplicaSet`/`EndpointSlice`/RBAC payloads; for `run.googleapis.com/Revision` and `appengine.googleapis.com/Version`, store `RevisionSummary` (`ServiceAccount`, `SanitizedEnvRefs`, `SecretRefNames`) keyed by revision URI and join against `Service.traffic[]` after `ListAssets` completes; strip `userinfo`, `?query`, and `#fragment` from extracted URLs and drop raw `env` string values and VM `startup-script` metadata.

#### 9. `internal/ingest/enrichers.go` (Create)

*   **Type / Scope:** Go Read-Only Enrichers.
*   **Proposed Changes:** Implement concurrent enrichers over active projects/regions:
    *   `EnrichBigQueryJobs`: REST `bigquery/v2` `jobs.list` (`allUsers=true`, `projection=full`, `minCreationTime = readTime - 7d`, `maxResults=1000`, `<= 20` pages/project; record `TRUNCATING_CAP` in `CoverageLedger` if `nextPageToken != ""` at page 20), extracting `user_email`, `principal_subject`, `referencedTables`, and `destinationTable` (skipping `_script*` / anon tables and discarding `configuration.query.query` SQL text).
    *   `EnrichDataflowJobs`: REST `dataflow/v1b3` `projects.locations.jobs.list` (`filter=ACTIVE`) + `jobs.get` (`view=JOB_VIEW_DESCRIPTION`) extracting `displayData` resource URIs.
    *   `EnrichMonitoring`: `ListTimeSeries` (`alignmentPeriod=86400s`, `ALIGN_SUM`, `REDUCE_SUM`) querying (a) `serviceruntime.googleapis.com/api/request_count` on `consumed_api` grouped by `resource.labels.project_id`, `resource.labels.service`, `resource.labels.credential_id`, and `metric.labels.method` (distinguishing `serviceaccount:<uniqueId>` vs non-SA callers), and (b) `aiplatform.googleapis.com/publisher/online_serving/model_invocation_count` and `aiplatform.googleapis.com/publisher/online_serving/consumed_token_throughput` on `aiplatform.googleapis.com/PublisherModel` grouped by `project_id`, `location`, `publisher`, `model_user_id`, and `metric.labels.request_type`.
    *   `EnrichRagCorpora`: `oauth2`-authenticated HTTP `GET https://{location}-aiplatform.googleapis.com/v1beta1/projects/{p}/locations/{l}/ragCorpora` strictly for `(project, location)` pairs observed on regional CAI assets or `PublisherModel` series (`<= 50` calls cap, logging `TRUNCATING_CAP` if exceeded).

#### 10. `internal/ingest/snapshot.go` and `internal/ingest/ingest_test.go` (Create)

*   **Type / Scope:** Gzip/JSON Snapshot Persistence & Ingestion Test Suite.
*   **Proposed Changes:** Define `type SnapshotFile struct { SchemaVersion string; Inventory *ir.RawInventory; Ledger *ir.CoverageLedger }`; `SaveSnapshot` writes `compress/gzip` JSON with `0600` permissions, and `LoadSnapshot` peeks the first 2 bytes (`0x1f, 0x8b` -> `gzip.NewReader`; otherwise plain UTF-8 JSON decoder) so plain `.snap.json` fixtures load directly. `ingest_test.go` (`bufconn` + `httptest.Server`) tests `<= 80` RPM pacing, server `ReadTime` pinning, out-of-order `Revision`-before-`Service` and `Table` `BigLake` projection, credential stripping (`postgres://user:pass@10.8.0.5/db?sslmode=require` -> `10.8.0.5`), `150,000`-asset folding memory bounds, `quota_project` fail-fast vs target `403`/`VPC-SC` degradation, and gzip/plain snapshot round-trips.

#### 11. `internal/compiler/compact.go` (Create)

*   **Type / Scope:** Go 15-Pass Multi-Resource Compaction Engine.
*   **Proposed Changes:** Implement `CompileNodes(inv *ir.RawInventory, cat *catalog.Catalog, aliases map[string]string) (*ir.Graph, error)` executing Passes 1–15:
    *   Batch/serverless job suffix normalization regex: strip trailing `[-_](?:\d{8,14}|[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}|\d{4}-\d{2}-\d{2}[tT_-]\d{2}[-:]\d{2}).*$`.
    *   Synthetic canonical URIs: `//aiplatform.googleapis.com/projects/{p}/locations/{l}/publisherModels/{publisher}/{model}` (`PublisherModel`), `//gcp-arch.internal/projects/{p}/unclassified/{service_or_type}` (`Unclassified Service`), and `//gcp-arch.internal/externalProjects/{project_id_or_number}` (`External Referenced`).
    *   Pass 13 (`Pub/Sub`): retain `Topic` as a node, collapse `1:1` `Subscription` onto the `Topic -> Consumer` edge, and retain `1:N` fan-out or BigQuery/GCS export `Subscription`s.

#### 12. `internal/compiler/edges.go` and `internal/compiler/compiler_test.go` (Create)

*   **Type / Scope:** Go Three-Tier Edge Resolver & Compiler Unit Tests.
*   **Proposed Changes:** Implement `ResolveEdges(g *ir.Graph, inv *ir.RawInventory, cat *catalog.Catalog) error`:
    *   Tier 1: structural URIs, projected `BigLake`/federated/`BQML` links, VPC-domain-scoped `(networkURI, privateIP)` matches, known-project `<project>.<dataset>` refs, `Dataset -> Dataset` SQL lineage, and `Workload -> Dataset` `jobs.list` lineage when `source_workloads_for_principal == 1` (supporting GSA email, direct GKE WIF `principal://iam.googleapis.com/projects/{num}/locations/global/workloadIdentityPools/{proj}.svc.id.goog/subject/ns/{ns}/sa/{ksa}`, and `NodePool` VM SA).
    *   Tier 2: evaluate target candidate cardinality against **raw CAI assets in `inv.Assets` before Pass 1–15 absorption**; emit `TIER2_EMPIRICAL_1TO1` dashed edge iff `in_scope_workloads_using_SA == 1 && raw_in_scope_targets == 1` (and for `PublisherModel`, `*GenerateContent*`/`*EmbedContent*` methods with `0` non-SA callers and `1` model in project), then map raw target URI to its enclosing compacted `Node.ID`.
    *   Tier 3: populate `Node.UnresolvedCandidates` (`TargetNodeID`, `TargetURI`, `Reason`) and `Unresolved Links: N` badge whenever `source_workloads > 1` or `raw_in_scope_targets > 1`. `compiler_test.go` tests all 15 passes, `M:1` shared-SA blocking, raw-bucket vs Composer-bucket `1:2` blocking, VPC-isolated IP disambiguation, and `1:1` Cloud SQL promotion.

#### 13. `internal/layout/views.go`, `internal/layout/elk.go`, and `internal/layout/layout_test.go` (Create)

*   **Type / Scope:** Go Fixed Two-Tab Projector & Pure-Go `elk-go` Runner.
*   **Proposed Changes:** Define `ViewGraph` and `LayoutedPage` (`TabID`, `TabName`, `Containers []ContainerBox`, `Cards []CardBox`, `Edges []RoutedEdge`, `TransversalGroups []CounterGroup`, `Legend LegendBox`):
    *   `ProjectViews(g *ir.Graph, viewFlag string)` builds `L1-Platform-Network` (nested admin/VPC/subnet containers; protected `Project` containers flagged `HasVPCSC = true` with perimeter title) and `L2-Architecture` (flat root cards with `ProjectID`/`SharedVPCHost`/`VPCSCPerimeter` header pills + detached `Bottom Transversal Platform Band` placed at `Y = maxFlowY + 80`).
    *   Deterministic box sizing constants in `elk.go`: `CardWidth = 220`, `BaseCardHeight = 64`, `HeaderPillHeight = 18`, `BadgeRowHeight = 22` (`2` badges/row), `ContainerPadding = {Top: 44, Left: 24, Right: 24, Bottom: 28}`, `EdgeLabelCharWidth = 7`.
    *   Cycle & `elk-go` contract: run Tarjan/DFS cycle detection on sorted adjacency lists; for nodes incident to feedback back-edges, set `elk.portConstraints = "FIXED_SIDE"`, assign explicit `WEST` (`in`), `EAST` (`out`), and `SOUTH` (`cycle`) `ELKPort`s, reverse feedback edges prior to calling `github.com/d2lang/elk-go`'s `layered` layout engine (`elk.direction = "RIGHT"`, `elk.edgeRouting = "ORTHOGONAL"`, `elk.randomSeed = "1"`, `elk.layered.layering.layerConstraint = "FIRST"` on ingress roots), restore true edge direction post-layout, and translate compound LCA coordinates to root `(0, 0)` canvas space. `layout_test.go` verifies 10-run byte-identical geometry, zero sibling card overlaps, and `SOUTH`-port feedback routing.

#### 14. `internal/drawio/render.go`, `internal/drawio/merge.go`, and `internal/drawio/drawio_test.go` (Create)

*   **Type / Scope:** Lossless `mxGraphModel` Renderer & `beevik/etree` 3-Way Merger.
*   **Proposed Changes:**
    *   `RenderDrawioXML`: emit `<mxfile compressed="false" version="24.0.0">` with `<diagram id="<tabID>" name="<tabName>"><mxGraphModel><root><object id="0" gcpArchVersion="1" gcpArchManifest="<json>"/><mxCell id="1" parent="0"/>...</root></mxGraphModel></diagram>`. Inline icon style per card (`shape=label;html=1;image=img/lib/gcp2/...` or `image=data:image/svg+xml,<base64>` without `;base64`), escape all interpolated text via `html.EscapeString`, emit `<object id="<cellID>" placeholders="1" label="<b>Title</b><br>%gcpBadges%" gcpBadges="<badges-html>" gcpArchManaged="1" aip122="..." assetType="..." collapsedAssets="..." unresolvedCandidates="...">`, omit `<Array as="points">` on adjacent-rank forward edges, and place the `Legend & Coverage Box` at `(0, maxCanvasY + 40)`.
    *   `MergeAndWriteDrawio`: parse existing target via `github.com/beevik/etree`; if `<diagram>` has text rather than `<mxGraphModel>` child elements, base64-decode -> raw `flate.NewReader` -> `url.PathUnescape` before parsing DOM; treat `<object>` and `<UserObject>` identically; store canonicalized `{generated_title, sorted_style_map_hash, kind}` in per-tab `gcpArchManifest`; subtract any `UnresolvedCandidates` target already connected by an unmanaged `<mxCell edge="1" source="<cardID>" target="<candID>">` before formatting `gcpBadges`; apply the 6-row vertex+edge merge table placing newly discovered vertices inside `New Resources (Drag to Place)` at `(maxExistingX + 80, minExistingY)` (`240px` vertical pitch); write `<target>.bak` (unless `NoBackup`) and atomic temp-file rename. `drawio_test.go` tests compressed-save round-trips, `<object>` rewrite survival, Run-3 preservation of Run-2 user title/color edits alongside live `Tables: 55` badge updates, manual arrow badge clearing, managed-edge waypoint stripping on moved endpoints, and staging coordinates.

#### 15. `cmd/gcp-arch/main.go`, `cmd/gcp-arch/main_test.go`, `cmd/gcp-arch/e2e_test.go`, and `testdata/enterprise_reference.snap.json` (Create)

*   **Type / Scope:** CLI Entrypoint, Diagnostic JSON Schemas, Reference Snapshot & E2E Suite.
*   **Proposed Changes:**
    *   `main.go`: wire `preflight`, `generate`, and `inspect` on `spf13/cobra`; require one of `--org` / `--folder` / `--project` when `--from-snapshot` is unset (optional when `--from-snapshot` is set); compute default `-o` filename as `<org|folder|p1>.drawio`, `<p1>-plus-<N-1>.drawio` for multi-project `--project`, or `<snapshot-stem>.drawio` when `--from-snapshot` is used without scope flags; define `PreflightReport` and `InspectReport` JSON structs for `--json`; map errors to POSIX exit codes `0`, `1`, and `2`.
    *   `testdata/enterprise_reference.snap.json` & `cmd/gcp-arch/e2e_test.go`: plain-JSON `SnapshotFile` fixture modeling the multi-project enterprise scenario (Shared VPC + VPC-SC, 7-part LB + Cloud Run MCP + GKE + AlloyDB, Datastream + Dataflow + Composer + BigQuery 50-table dataset + `jobs.list` lineage + Looker, Dialogflow CX + 2 `ReasoningEngine` A2A agents + `RagCorpus` + `Vector Search` + `Endpoint` + `gemini-2.5-pro` `PublisherModel`, and 2 `newservice.googleapis.com/Cluster` assets). `e2e_test.go` tests initial generation, simulated Draw.io compressed save with dragged coordinates + renamed title + manual arrow + deleted card, Day-2 cloud delta update (`+1` Cloud Run, `+1` Pub/Sub, `+5` tables -> `Tables: 55`, `-1` Memorystore), and `.gcp-arch/catalog/` YAML reload.

### Verification Plan

*   **Compilation/Build:** `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/gcp-arch ./cmd/gcp-arch` followed by `go version -m bin/gcp-arch` (asserting `CGO_ENABLED=0` static build metadata).
*   **Unit & Race Tests:** `CGO_ENABLED=1 go test -race -count=1 ./internal/... ./cmd/...`
*   **Memory Ceiling Test (Non-Race):** `CGO_ENABLED=0 go test -count=1 -run TestStreamFolding150kHeapCeiling ./internal/ingest` (invoking `runtime.GC()` and asserting peak live `runtime.MemStats.HeapAlloc < 512<<20` across 150,000 synthetic sub-resources).
*   **Static Analysis & Linting:** `go vet ./...` and `golangci-lint run ./...` (zero warnings or errors).
*   **Test Coverage Requirement:** `go test -coverprofile=coverage.out ./internal/... ./cmd/... && go tool cover -func=coverage.out` achieving `>= 85%` statement coverage across `./internal/...` and `./cmd/...`.
*   **End-to-End Verification:** `CGO_ENABLED=1 go test -race -v -run TestEnterpriseEndToEnd ./cmd/gcp-arch` asserting: (1) zero unexpected edges and zero leaked worker VMs/raw tables against the reference fixture, (2) byte-identical `.drawio` XML across repeated runs, (3) `< 2 MB` `.drawio` output with valid uncompressed XML and inline icon URIs, (4) preservation of simulated user Draw.io coordinates, custom titles, manual arrows, notes, and deletions after a compressed Draw.io save and Day-2 delta update with live `%gcpBadges%` refresh and `New Resources (Drag to Place)` staging, and (5) runtime `.gcp-arch/catalog/*.yaml` overlay resolution without recompilation.
