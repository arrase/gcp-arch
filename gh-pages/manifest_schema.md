# Manifest Schema & Validation Reference

`architecture.yaml` (`schema_version: v1`) is the **Git-tracked source of truth** of the `gcp-arch` pipeline. It forms a clean contract between cloud-connected synthesis ([Stage 2 — Gemini 4 Synthesis & Day-2 Reconciliation](synthesis.md)) and 100% offline verification and diagram generation ([Stage 3 — Offline Rendering & Routing](rendering.md)).

Every `backed_by` entry (on components and containers) and every `excluded_resources` entry embeds its full `{uri, asset_type, project_id}` tuple directly inside `architecture.yaml`. Because of this self-contained design, `gcp-arch validate` and `gcp-arch render` operate completely offline in clean Git checkouts and pull-request CI pipelines—requiring **no `facts.json`**, **no GCP credentials**, and **zero network access**.

---

## Complete Annotated `architecture.yaml` Example

The example below illustrates every top-level section and feature supported by `schema_version: v1`: `metadata`, `components` (with `notes`, folded `backed_by`, and `inline_meta`), `containers` (with depth-2 `parent_container_id` and container `backed_by`), `excluded_resources`, `rejected_edges`, `acknowledged_edges`, `view_0_system_index`, and detail views featuring `off_page_refs`, `VERIFIED` and `INFERRED` edges, and shared `via_bus_id` bus spines.

```yaml
schema_version: v1

metadata:
  generated_at: "2026-11-23T02:00:00Z"
  scopes:
    - projects/acme-cicd

components:
  - id: acme_tfstate
    title: Terraform State Bucket
    subtitle: gs://acme-tfstate
    icon_key: cloud_storage
    component_kind: DATASTORE
    project_id: acme-cicd
    backed_by:
      - uri: //storage.googleapis.com/acme-tfstate
        asset_type: storage.googleapis.com/Bucket
        project_id: acme-cicd
    inline_meta:
      - label: Region
        value: europe-southwest1

  - id: atlantis
    title: Atlantis PR Automation
    subtitle: gke:cicd / ns:atlantis
    icon_key: k8s_workload
    component_kind: WORKLOAD
    project_id: acme-cicd
    # Human curation notes preserved across Day-2 --reconcile
    notes: Curated Terraform PR runner boundary; reviewed in ADR-014.
    # 1:1 folded Deployment + ServiceAccount + Secret under one workload card
    backed_by:
      - uri: //container.googleapis.com/projects/acme-cicd/locations/europe-southwest1-b/clusters/cicd/k8s/namespaces/atlantis/apps/deployments/atlantis
        asset_type: apps.k8s.io/Deployment
        project_id: acme-cicd
      - uri: //iam.googleapis.com/projects/acme-cicd/serviceAccounts/atlantis@acme-cicd.iam.gserviceaccount.com
        asset_type: iam.googleapis.com/ServiceAccount
        project_id: acme-cicd
      - uri: //secretmanager.googleapis.com/projects/acme-cicd/secrets/github-webhook-secret
        asset_type: secretmanager.googleapis.com/Secret
        project_id: acme-cicd
    # Up to 4 rows rendered inside the card in exact YAML order
    inline_meta:
      - label: Service Account
        value: atlantis@acme-cicd.iam
      - label: Secret
        value: github-webhook-secret

  - id: atlantis_https_lb
    title: Atlantis HTTPS Ingress
    subtitle: atlantis.cicd.example.com/events
    icon_key: cloud_load_balancing
    component_kind: INGRESS
    project_id: acme-cicd
    backed_by:
      - uri: //compute.googleapis.com/projects/acme-cicd/global/forwardingRules/k8s2-fs-demo1234-atlantis-ingress-0001
        asset_type: compute.googleapis.com/GlobalForwardingRule
        project_id: acme-cicd
      - uri: //container.googleapis.com/projects/acme-cicd/locations/europe-southwest1-b/clusters/cicd/k8s/namespaces/atlantis/networking.k8s.io/ingresses/atlantis
        asset_type: networking.k8s.io/Ingress
        project_id: acme-cicd
      - uri: //container.googleapis.com/projects/acme-cicd/locations/europe-southwest1-b/clusters/cicd/k8s/namespaces/atlantis/services/atlantis
        asset_type: k8s.io/Service
        project_id: acme-cicd
    inline_meta:
      - label: Public IP
        value: 203.0.113.41:443

  - id: github_acme
    title: GitHub (github-acme)
    subtitle: acme-corp/platform-services
    icon_key: github
    component_kind: EXTERNAL_CALLER
    project_id: external
    backed_by:
      - uri: //github.com/acme-corp/platform-services
        asset_type: external.github.com/Repository
        project_id: external

  - id: sonarqube_db
    title: Cloud SQL (sonarqube)
    subtitle: POSTGRES_16 / 10.100.0.5
    icon_key: cloud_sql
    component_kind: DATASTORE
    project_id: acme-cicd
    backed_by:
      - uri: //sqladmin.googleapis.com/projects/acme-cicd/instances/sonarqube
        asset_type: sqladmin.googleapis.com/Instance
        project_id: acme-cicd

  - id: vpc_cicd_net
    title: "VPC Network: cicd"
    subtitle: Subnet cicd-k8s (10.10.0.0/20)
    icon_key: virtual_private_cloud
    component_kind: COHORT
    project_id: acme-cicd
    backed_by:
      - uri: //compute.googleapis.com/projects/acme-cicd/global/networks/cicd
        asset_type: compute.googleapis.com/Network
        project_id: acme-cicd

containers:
  - id: ext_source
    title: External Source Control
    subtitle: GitHub SaaS
    kind: EXTERNAL_ZONE

  # Depth-2 nested container: gke_cicd -> parent proj_acme_cicd
  - id: gke_cicd
    title: "GKE Cluster: cicd"
    subtitle: europe-southwest1-b (Standard)
    kind: GKE_CLUSTER
    parent_container_id: proj_acme_cicd
    backed_by:
      - uri: //container.googleapis.com/projects/acme-cicd/locations/europe-southwest1-b/clusters/cicd
        asset_type: container.googleapis.com/Cluster
        project_id: acme-cicd

  - id: proj_acme_cicd
    title: "Project: acme-cicd"
    subtitle: europe-southwest1
    kind: PROJECT

  - id: psa_tenant
    title: Google PSA Backing Zone
    subtitle: p1234567890abcdef-tp
    kind: PSA_ZONE

# Active resources deliberately excluded from visual cards with audit rationale
excluded_resources:
  - uri: //compute.googleapis.com/projects/acme-cicd/global/networks/default
    asset_type: compute.googleapis.com/Network
    project_id: acme-cicd
    reason: Unused default GCP VPC network suppressed as boilerplate

# Curated tombstones forbidding specific directed edges across all views & Day-2 reconcile
rejected_edges:
  - from: atlantis_https_lb
    to: acme_tfstate
    reason: Ingress never accesses Terraform state directly; traffic terminates at Atlantis

# Non-heuristic FactEdge component pairs already offered to synthesis and intentionally left undrawn
acknowledged_edges:
  - from: atlantis
    to: vpc_cicd_net

views:
  # Required at views[0] whenever >= 2 detail views exist; projected deterministically by Go
  - id: view_0_system_index
    title: System Index Map
    subtitle: 2 logical views across 6 architecture components
    archetype: landscape_matrix
    accent_mode: MIXED
    placements:
      - component_id: github_acme
        column: 0
      - component_id: gke_developer_platform
        column: 2
      - component_id: shared_network_psa
        column: 3
    edges:
      - from: github_acme
        to: gke_developer_platform
        verb: calls ingress
        flow_kind: RUNTIME
        provenance: VERIFIED
      - from: gke_developer_platform
        to: shared_network_psa
        verb: PSA topology
        flow_kind: RUNTIME
        provenance: VERIFIED

  - id: gke_developer_platform
    title: GKE Developer Platform & Ingress
    subtitle: External HTTPS Load Balancer routing to Atlantis and Terraform state
    archetype: landscape_matrix
    accent_mode: RUNTIME
    notes: Primary runtime entry point for PR automation webhooks.
    container_ids:
      - ext_source
      - gke_cicd
      - proj_acme_cicd
      - psa_tenant
    # Placement order governs Stage 3 container band packing (allocateContainerBands) and barycenter tie-breaking
    placements:
      - component_id: github_acme
        container_id: ext_source
        column: 0
      - component_id: atlantis_https_lb
        container_id: proj_acme_cicd
        column: 1
      - component_id: atlantis
        container_id: gke_cicd
        column: 2
      - component_id: acme_tfstate
        container_id: proj_acme_cicd
        column: 3
      - component_id: sonarqube_db
        container_id: psa_tenant
        column: 3
        off_page_refs:
          - target_view_id: shared_network_psa
            label: PSA topology
            direction: OUT
    edges:
      - from: atlantis
        to: acme_tfstate
        verb: locks tfstate
        flow_kind: CICD
        provenance: VERIFIED
        evidence_edge_ids:
          - c21876e6fb659711
        via_bus_id: atlantis_egress
      - from: atlantis
        to: sonarqube_db
        verb: schema check
        flow_kind: ADMIN
        provenance: INFERRED
        inference_rationale: Migration verification hooks query Postgres metadata during release dry-runs.
        via_bus_id: atlantis_egress
      - from: atlantis_https_lb
        to: atlantis
        verb: HTTPS /events
        flow_kind: RUNTIME
        provenance: VERIFIED
        # Multi-hop directed walk: ForwardingRule -> Ingress -> Service -> Deployment
        evidence_edge_ids:
          - bb189d721e6415c1
          - 2d646bade955f34c
      - from: github_acme
        to: atlantis_https_lb
        verb: webhook POST
        flow_kind: RUNTIME
        provenance: VERIFIED
        evidence_edge_ids:
          - 190f6dfb3f1fa702

  - id: shared_network_psa
    title: Shared Network & PSA Topology
    subtitle: VPC cicd private peering to Google-managed PSA tenant
    archetype: hub_spoke
    accent_mode: RUNTIME
    container_ids:
      - gke_cicd
      - proj_acme_cicd
      - psa_tenant
    placements:
      - component_id: atlantis
        container_id: gke_cicd
        column: 0
      - component_id: vpc_cicd_net
        container_id: proj_acme_cicd
        column: 1
      - component_id: sonarqube_db
        container_id: psa_tenant
        column: 2
    edges:
      - from: vpc_cicd_net
        to: sonarqube_db
        verb: PSA peering
        flow_kind: RUNTIME
        provenance: VERIFIED
        evidence_edge_ids:
          - 44e5d4dcd096c550
```

---

## Strict YAML Codec & Canonical Serialization

[`internal/manifest/types.go`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/types.go) implements strict decoding and deterministic serialization so human edits in Git stay clean, minimal, and free of silent typos.

### Strict Decoding (`manifest.Load`)

1. **`KnownFields(true)`:** [`manifest.Load`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/types.go#L257-L274) decodes YAML with `gopkg.in/yaml.v3` and `dec.KnownFields(true)`. Any unknown or misspelled field name (such as `icon:` instead of `icon_key:`) fails immediately rather than being silently ignored.
2. **Single-Document Guard:** After decoding `Manifest`, `manifest.Load` attempts a second decode and asserts `errors.Is(err, io.EOF)`. Multi-document YAML streams (`---`) are rejected with `unexpected trailing YAML document`.
3. **CLI Error Handling:** Under `gcp-arch validate --json` ([`cmd/gcp-arch/validate.go`](https://github.com/arrase/gcp-arch/blob/main/cmd/gcp-arch/validate.go#L86-L102)), decode errors emit a `ValidationReport` containing code `YAML_DECODE_ERROR` and exit with code **`2`**. Filesystem read errors (`*os.PathError`, such as a missing file) return CLI exit code **`1`**.

### Canonical Serialization (`manifest.Marshal` & `manifest.Write`)

[`manifest.Marshal`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/types.go#L350-L367) clones `m` (never mutating the caller's struct) and encodes deterministic **2-space indented UTF-8 YAML**. [`manifest.Write`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/types.go#L370-L376) persists the output atomically ([`atomicfile.Write`](https://github.com/arrase/gcp-arch/blob/main/internal/atomicfile/atomicfile.go#L21-L26)) with mode **`0644`** (creating parent directories with **`0755`**).

To produce stable Git diffs while preserving meaningful visual and cryptographic ordering, [`normalizedManifestCopy`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/types.go#L307-L334) sorts structural sets deterministically and explicitly preserves three ordered slices:

| Slice Path | Behavior on `Marshal` | Sort Key / Preservation Reason |
| :--- | :--- | :--- |
| `metadata.scopes` | **Sorted** | Lexicographical ascending. |
| `components` | **Sorted** | By `id`. |
| `components[].backed_by` | **Sorted** | By `(uri, asset_type, project_id)`. |
| `containers` | **Sorted** | By `id`. |
| `containers[].backed_by` | **Sorted** | By `(uri, asset_type, project_id)`. |
| `excluded_resources` | **Sorted** | By `(uri, asset_type, project_id, reason)`. |
| `rejected_edges` | **Sorted** | By `(from, to, reason)`. |
| `acknowledged_edges` | **Sorted & Deduplicated** | By `(from, to)`, with adjacent duplicates collapsed via `slices.Compact` ([`normalizeComponentPairs`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/types.go#L325-L334)). |
| `views` | **Sorted** | `view_0_system_index` pinned at `views[0]`; all other views sorted by `id`. |
| `views[].container_ids` | **Sorted** | Lexicographical ascending. |
| `views[].placements[].off_page_refs` | **Sorted** | By `(target_view_id, direction, label)`. |
| `views[].edges` | **Sorted** | By `(from, to, flow_kind, verb, provenance, via_bus_id, inference_rationale, evidence_edge_ids)`. |
| **`components[].inline_meta`** | **Order Preserved** | Controls top-to-bottom metadata row order inside rendered component cards. |
| **`views[].placements`** | **Order Preserved** | Governs first-appearance row-band packing in [`allocateContainerBands`](https://github.com/arrase/gcp-arch/blob/main/internal/render/prepare.go#L298-L331) and acts as the deterministic **barycenter tie-breaker** (`yamlIdx` in [`internal/render/prepare.go`](https://github.com/arrase/gcp-arch/blob/main/internal/render/prepare.go#L396-L422)) during Stage 3 rendering. |
| **`views[].edges[].evidence_edge_ids`** | **Order Preserved** | Preserves the human-readable directed-walk hop sequence from source to target. |

---

## Exhaustive Schema & Enum Reference

### Struct Field Reference

#### Root `Manifest`

| YAML Key | Type | Required | Description |
| :--- | :--- | :---: | :--- |
| `schema_version` | `string` | **Yes** | Must equal `"v1"` ([`manifest.SchemaVersion`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/types.go#L20)). |
| `metadata` | `Metadata` | Optional | Generation timestamp and scanned GCP scopes. Always serialized by `manifest.Marshal` (no `omitempty`), but optional in `ValidateStructure` (omitting `metadata` passes both `manifest.Load` and `manifest.ValidateStructure`). |
| `components` | `[]Component` | **Yes** | Global logical architecture blocks referenced by detail views. |
| `containers` | `[]Container` | Optional | Global visual grouping frames (`omitempty`). |
| `excluded_resources` | `[]ExcludedResource` | Optional | Active resources intentionally omitted from cards with audit reasons (`omitempty`). |
| `rejected_edges` | `[]RejectedEdge` | Optional | Curated tombstones suppressing directed `(from, to)` component edges (`omitempty`). |
| `acknowledged_edges` | `[]ComponentPair` | Optional | Directed `(from, to)` component pairs projected from non-heuristic `FactEdge`s that were already offered to synthesis and intentionally left undrawn, preventing repeated Day-2 reconciliation churn (`omitempty`, [`internal/manifest/types.go`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/types.go#L99)). |
| `views` | `[]View` | **Yes** | One or more diagram views (`views[0]` must be `view_0_system_index` when `>= 2` detail views exist; max `16` detail views). |

#### `Metadata`

| YAML Key | Type | Required | Description |
| :--- | :--- | :---: | :--- |
| `generated_at` | `string` | Optional | RFC 3339 timestamp (populated from `facts.ReadTime` or UTC clock during Day-1 synthesis). |
| `scopes` | `[]string` | Optional | Canonical GCP scopes scanned (e.g., `projects/acme-cicd`, `folders/12345`, `organizations/67890`). |

#### `Component`

| YAML Key | Type | Required | Description |
| :--- | :--- | :---: | :--- |
| `id` | `string` | **Yes** | Unique, non-empty canonical slug (conventionally `snake_case`). |
| `title` | `string` | **Yes** | Non-empty human-readable card title. |
| `subtitle` | `string` | Optional | Concise technical sublabel (`omitempty`). |
| `icon_key` | `string` | **Yes** | Registered icon slug present in the 85-icon embedded registry ([`internal/icons/assets/icons.yaml`](https://github.com/arrase/gcp-arch/blob/main/internal/icons/assets/icons.yaml)). |
| `component_kind` | `ComponentKind` | **Yes** | Architectural role enum (8 allowed values). |
| `project_id` | `string` | **Yes** | Non-empty owning GCP project ID (or `"external"` for external actors/targets). |
| `notes` | `string` | Optional | Free-form human curation notes stored in Git and preserved across Day-2 `--reconcile` (`omitempty`). |
| `backed_by` | `[]BackedResource` | **Yes** | At least 1 resource (`len >= 1`); must satisfy stateful datastore homogeneity. |
| `inline_meta` | `[]InlineMeta` | Optional | `0..4` key-value detail rows rendered inside the card in exact YAML order (`omitempty`). |

#### `BackedResource`

| YAML Key | Type | Required | Description |
| :--- | :--- | :---: | :--- |
| `uri` | `string` | **Yes** | Non-empty canonical AIP-122 GCP resource URI (or synthetic external URI). |
| `asset_type` | `string` | **Yes** | Non-empty GCP CAI asset type (e.g., `run.googleapis.com/Service`, `sqladmin.googleapis.com/Instance`). |
| `project_id` | `string` | **Yes** | Non-empty owning GCP project ID (or `"external"`). |

#### `InlineMeta`

| YAML Key | Type | Required | Description |
| :--- | :--- | :---: | :--- |
| `label` | `string` | **Yes** | Non-empty metadata row key (e.g., `Public IP`, `Service Account`). |
| `value` | `string` | **Yes** | Non-empty metadata row value. |

#### `Container`

| YAML Key | Type | Required | Description |
| :--- | :--- | :---: | :--- |
| `id` | `string` | **Yes** | Unique, non-empty container slug. |
| `title` | `string` | **Yes** | Non-empty container frame title. |
| `subtitle` | `string` | Optional | Region, network, or scope subtitle (`omitempty`). |
| `kind` | `ContainerKind` | **Yes** | Grouping boundary kind enum (6 allowed values). |
| `parent_container_id` | `string` | Optional | ID of an existing parent container; acyclic; active nesting depth within any view must be `<= 2` (`omitempty`). |
| `backed_by` | `[]BackedResource` | Optional | Optional resources accounted for by this container (e.g., `container.googleapis.com/Cluster`, `compute.googleapis.com/Network`). Every entry requires non-empty `uri`, `asset_type`, and `project_id` (`omitempty`). |

#### `ExcludedResource`

| YAML Key | Type | Required | Description |
| :--- | :--- | :---: | :--- |
| `uri` | `string` | **Yes** | Non-empty canonical resource URI. Must be unique across `components[].backed_by`, `containers[].backed_by`, and `excluded_resources` (`DUPLICATE_ACCOUNTED_URI`). |
| `asset_type` | `string` | **Yes** | Non-empty CAI asset type. Must **not** be one of the 15 stateful datastore asset types (`EXCLUDED_DATASTORE`). |
| `project_id` | `string` | **Yes** | Non-empty owning project ID. |
| `reason` | `string` | **Yes** | Non-empty audit justification explaining why the resource is excluded from cards. |

#### `RejectedEdge`

| YAML Key | Type | Required | Description |
| :--- | :--- | :---: | :--- |
| `from` | `string` | **Yes** | Non-empty source ID referencing a declared top-level `components[].id` ([`validateRejectedEdges`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/validate.go#L529-L541) -> [`validatePairComponents`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/validate.go#L370-L376)). Naming a detail `view.id` fails validation (`UNKNOWN_PAIR_COMPONENT`, because `VIEW_COMPONENT_ID_COLLISION` forbids sharing IDs between views and components). |
| `to` | `string` | **Yes** | Non-empty target ID referencing a declared top-level `components[].id` (`UNKNOWN_PAIR_COMPONENT` otherwise). |
| `reason` | `string` | **Yes** | Non-empty rationale explaining why this directed `(from, to)` edge is suppressed across all views and Day-2 `--reconcile`. |

#### `ComponentPair` (`acknowledged_edges[]`)

| YAML Key | Type | Required | Description |
| :--- | :--- | :---: | :--- |
| `from` | `string` | **Yes** | Source ID referencing a declared top-level `components[].id` ([`internal/manifest/types.go`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/types.go#L164-L167), [`validateAcknowledgedEdges`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/validate.go#L364-L376)). |
| `to` | `string` | **Yes** | Target ID referencing a declared top-level `components[].id` (`UNKNOWN_PAIR_COMPONENT` otherwise). |

#### `View`

| YAML Key | Type | Required | Description |
| :--- | :--- | :---: | :--- |
| `id` | `string` | **Yes** | Unique, non-empty view slug (`view_0_system_index` reserved for `views[0]` when `>= 2` detail views exist). |
| `title` | `string` | **Yes** | Non-empty diagram page title. |
| `subtitle` | `string` | Optional | One-line diagram summary (`omitempty`). |
| `archetype` | `Archetype` | **Yes** | Visual layout archetype enum (`layered_flow`, `landscape_matrix`, `stage_pipeline`, or `hub_spoke`). |
| `accent_mode` | `AccentMode` | **Yes** | Color palette emphasis enum (`RUNTIME`, `CICD`, or `MIXED`). |
| `notes` | `string` | Optional | Human curation notes for the view (`omitempty`). |
| `container_ids` | `[]string` | Optional | Active global container IDs drawn in this view; max active nesting depth `2` (`omitempty`). |
| `placements` | `[]Placement` | **Yes** | `1..12` placements on detail views; `1..16` placements on `view_0_system_index`. Order preserved. |
| `edges` | `[]Edge` | Optional | Directed edges between components placed in this view (`omitempty`). |

#### `Placement`

| YAML Key | Type | Required | Description |
| :--- | :--- | :---: | :--- |
| `component_id` | `string` | **Yes** | References a global `Component.ID` (on detail views) or a `Component.ID` / detail `View.ID` (on `view_0_system_index`). Unique per view. |
| `container_id` | `string` | Optional | Active container ID; if set, MUST appear in the enclosing `view.container_ids` (`omitempty`). |
| `column` | `int` | **Yes** | 0-based stage/column index: topological rank `0..N` for `layered_flow`; `0..4` for `landscape_matrix`/`stage_pipeline`; `0..2` for `hub_spoke`. |
| `off_page_refs` | `[]OffPageRef` | Optional | Cross-view navigation badges displayed on the card (`omitempty`). |

#### `OffPageRef`

| YAML Key | Type | Required | Description |
| :--- | :--- | :---: | :--- |
| `target_view_id` | `string` | **Yes** | ID of another existing view in `manifest.views` (must not equal the current view's ID). |
| `label` | `string` | **Yes** | Non-empty concise badge label populated symmetrically across detail views by [`internal/synth/annotate.go`](https://github.com/arrase/gcp-arch/blob/main/internal/synth/annotate.go). |
| `direction` | `string` | **Yes** | `"IN"` or `"OUT"`. |

#### `Edge`

| YAML Key | Type | Required | Description |
| :--- | :--- | :---: | :--- |
| `from` | `string` | **Yes** | Placed source `component_id` in this view. |
| `to` | `string` | **Yes** | Placed target `component_id` in this view. |
| `verb` | `string` | **Yes** | Non-empty action verb, maximum **`16` UTF-8 runes** (`maxEdgeVerbRunes = 16`). |
| `flow_kind` | `FlowKind` | **Yes** | Operational flow category (`RUNTIME`, `CICD`, or `ADMIN`). |
| `provenance` | `Provenance` | **Yes** | Evidence classification (`VERIFIED` or `INFERRED`). |
| `evidence_edge_ids` | `[]string` | Conditional | Required non-empty list of non-blank `FactEdge.ID` hashes for detail-view `VERIFIED` edges (exempt on `view_0_system_index`). Order preserved. |
| `inference_rationale` | `string` | Conditional | Required non-empty explanation when `provenance == INFERRED` (`omitempty`). |
| `via_bus_id` | `string` | Optional | Shared orthogonal bus spine group ID. All edges sharing one `via_bus_id` in a view must share either one common `from` (1-to-N) or one common `to` (N-to-1) (`omitempty`). |

---

### Enumerations Reference

#### `ComponentKind` (8 Values)

| Value | Architectural Role |
| :--- | :--- |
| `EXTERNAL_CALLER` | External user, client, SaaS webhook, or upstream repository initiating flows (placed in Column `0` on `View 0`). |
| `INGRESS` | Load balancers, Cloud Armor/WAF frontends, API Gateways, Ingress/NEG front doors, and Cloud NAT egress gateways. |
| `WORKLOAD` | Application runtime services (GKE Deployments/StatefulSets, Cloud Run services, Compute Engine workloads, Cloud Functions). |
| `PIPELINE` | Batch/streaming data pipelines and CI/CD execution runners (Cloud Build triggers, Dataflow jobs, Cloud Composer). |
| `MESSAGING` | Asynchronous brokers and event buses (Pub/Sub topics/subscriptions, Eventarc triggers, Cloud Tasks). |
| `DATASTORE` | Persistent databases, object storage, caches, warehouses, and artifact registries. |
| `COHORT` | Grouped infrastructure or shared network/project cohorts (VPC network hubs, subnetwork cohorts). |
| `EXTERNAL_TARGET` | Downstream out-of-scope GCP projects, external payment PSPs, BI SaaS endpoints, or third-party sinks (placed in Column `3` or `4` on `View 0`). |

#### `ContainerKind` (12 Values)

| Value | Grouping Boundary |
| :--- | :--- |
| `ORG` | GCP Organization root boundary. |
| `FOLDER` | GCP Resource Manager Folder boundary. |
| `PROJECT` | GCP Project boundary. |
| `VPC` | VPC Network boundary (including single-host Shared VPCs). |
| `REGION` | GCP Region boundary nested inside a VPC or Project. |
| `SUBNET` | VPC Subnetwork boundary (with CIDR subtitle). |
| `EXPOSURE_ZONE` | Public/Private/Internal network exposure grouping. |
| `GKE_CLUSTER` | Google Kubernetes Engine cluster boundary. |
| `PSA_ZONE` | Google-managed Private Service Access (PSA) tenant or Private Service Connect (PSC) zone. |
| `EXTERNAL_ZONE` | Boundary grouping external SaaS, public internet, or out-of-scope GCP targets. |
| `SHARED_HUB` | Shared VPC host project or centralized networking hub boundary. |
| `HYBRID_ZONE` | On-premises / hybrid connectivity zone (Dedicated Interconnect, HA VPN, NCC transit). |

#### `Archetype` (4 Values)

| Value | Valid `column` Bounds | Layout Semantics |
| :--- | :---: | :--- |
| `layered_flow` | `0..N` *(topological ranks)* | Unified nested Flexbox containment tree with left-to-right topological flow, stage banners, transversal governance panel, and DOM-measured Hanan-grid A* orthogonal routing (default emitted by Stage 2 synthesis). |
| `landscape_matrix` | `0..4` *(max 5 columns)* | Legacy application/ingress matrix archetype. |
| `stage_pipeline` | `0..4` *(max 5 columns)* | Legacy sequential left-to-right pipeline archetype. |
| `hub_spoke` | `0..2` *(fixed 3 columns)* | Legacy 3-column networking archetype (`spoke` col `0` -> `hub` col `1` -> `peered zone` col `2`). |

#### `AccentMode` (3 Values)

| Value | Visual Emphasis |
| :--- | :--- |
| `RUNTIME` | Production request/data plane emphasis (default runtime visual palette). |
| `CICD` | Delivery pipeline / automation emphasis. |
| `MIXED` | Views combining runtime and CI/CD or cross-cutting flows. |

#### `FlowKind`, `Provenance` & `OffPageRef.Direction`

| Enum | Allowed Values | Notes |
| :--- | :--- | :--- |
| **`FlowKind`** (3) | `RUNTIME`, `CICD`, `ADMIN` | Drives edge styling and semantic palette in Stage 3. |
| **`Provenance`** (2) | `VERIFIED`, `INFERRED` | `VERIFIED` renders as a solid evidence-backed connector; `INFERRED` renders dashed with a numbered footnote quoting `inference_rationale`. |
| **`OffPageRef.Direction`** (2) | `IN`, `OUT` | Symmetric cross-view navigation badges derived deterministically in [`internal/synth/annotate.go`](https://github.com/arrase/gcp-arch/blob/main/internal/synth/annotate.go) linking components across stage-partitioned detail views. |

---

## Stateful Datastore Homogeneity

To prevent architectural diagrams from collapsing fundamentally different persistence technologies (for example, Cloud SQL + Cloud Storage + BigQuery) into a single ambiguous "Databases" box, both Stage 2 ([`internal/synth/skeleton.go`](https://github.com/arrase/gcp-arch/blob/main/internal/synth/skeleton.go)) and offline validation ([`internal/manifest/validate.go`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/validate.go)) enforce **stateful datastore homogeneity** across the following **15** exact GCP CAI asset types (`statefulDatastoreAssetTypes`):

| # | Exact GCP CAI Asset Type | GCP Product |
| :---: | :--- | :--- |
| 1 | `sqladmin.googleapis.com/Instance` | Cloud SQL Instance |
| 2 | `alloydb.googleapis.com/Cluster` | AlloyDB Cluster |
| 3 | `alloydb.googleapis.com/Instance` | AlloyDB Instance |
| 4 | `spanner.googleapis.com/Instance` | Cloud Spanner Instance |
| 5 | `spanner.googleapis.com/Database` | Cloud Spanner Database |
| 6 | `bigtableadmin.googleapis.com/Instance` | Cloud Bigtable Instance |
| 7 | `firestore.googleapis.com/Database` | Firestore Database |
| 8 | `redis.googleapis.com/Instance` | Memorystore for Redis Instance |
| 9 | `redis.googleapis.com/Cluster` | Memorystore for Redis Cluster |
| 10 | `memcache.googleapis.com/Instance` | Memorystore for Memcached Instance |
| 11 | `storage.googleapis.com/Bucket` | Cloud Storage Bucket |
| 12 | `bigquery.googleapis.com/Dataset` | BigQuery Dataset |
| 13 | `artifactregistry.googleapis.com/Repository` | Artifact Registry Repository |
| 14 | `file.googleapis.com/Instance` | Cloud Filestore Instance |
| 15 | `pubsub.googleapis.com/Topic` | Pub/Sub Topic |

**How the checks work:**

* **Component Homogeneity (`HETEROGENEOUS_DATASTORE`):** For each component, [`validateComponentBackedBy`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/validate.go#L413-L436) collects the set of distinct `br.AssetType` values in `component.backed_by` that belong to the 15-entry map above. If `len(datastoreTypes) > 1`, validation emits **`HETEROGENEOUS_DATASTORE`**.
  * **Allowed:** One stateful datastore type folded with non-datastore helper resources (e.g., `pubsub.googleapis.com/Topic` + `pubsub.googleapis.com/Subscription`, `sqladmin.googleapis.com/Instance` + `kms.googleapis.com/CryptoKey`, or `storage.googleapis.com/Bucket` + `iam.googleapis.com/ServiceAccount`), or multiple resources of the *same* datastore asset type inside one component (e.g., two `storage.googleapis.com/Bucket` resources grouped into a `COHORT`), because `len(datastoreTypes) == 1`.
  * **Rejected (`HETEROGENEOUS_DATASTORE`):** Mixing two distinct asset types from the 15-entry table inside one `backed_by` list—such as `sqladmin.googleapis.com/Instance` + `storage.googleapis.com/Bucket`, or `alloydb.googleapis.com/Cluster` + `alloydb.googleapis.com/Instance`, or `spanner.googleapis.com/Instance` + `spanner.googleapis.com/Database`.
* **No Excluded Datastores (`EXCLUDED_DATASTORE`):** [`validateExcludedResources`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/validate.go#L511-L527) rejects placing any resource whose `asset_type` is in the 15-type set into `excluded_resources`. Stateful datastores must be diagrammed in a component (folding several of the same asset type into a `COHORT` if needed), never suppressed.

---

## Offline Structural Validation (`ValidateStructure`)

Running `gcp-arch validate <architecture.yaml>` executes strict YAML decoding followed by [`manifest.ValidateStructure`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/validate.go#L105-L113) ([`internal/manifest/validate.go`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/validate.go)) completely offline with zero network calls.

### Hard Structural Limits

| Constant ([`internal/manifest/validate.go`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/validate.go#L78-L85)) | Limit | Scope |
| :--- | :---: | :--- |
| `maxInlineMetaRows` | **`4`** | Maximum `inline_meta` key-value rows per component card. |
| `maxDetailViewPlacements` | **`12`** | Maximum component placements (`1..12`) per detail view. |
| `maxSystemIndexPlacements` | **`16`** | Maximum placements (`1..16`) on `view_0_system_index`. |
| `maxDetailViews` | **`16`** | Maximum number of detail views (equals `maxSystemIndexPlacements` so `view_0_system_index` can index every detail view). |
| `maxContainerNestingDepth` | **`2`** | Maximum active container parent-child nesting depth within any view. |
| `maxEdgeVerbRunes` | **`16`** | Maximum UTF-8 rune count (`1..16`) for `edge.verb`. |

### Exhaustive Offline Validation Error Codes

All **54** offline structural error codes in [`internal/manifest/validate.go`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/validate.go#L17-L75) (**59** total including the 5 `--facts` codes below, plus `YAML_DECODE_ERROR` from [`cmd/gcp-arch/validate.go`](https://github.com/arrase/gcp-arch/blob/main/cmd/gcp-arch/validate.go#L95)) are listed below:

#### YAML Decode & Schema

| Error Code | Trigger Condition | `view_id` / `subject` |
| :--- | :--- | :--- |
| `YAML_DECODE_ERROR` | *(CLI `validate --json`)* Invalid YAML syntax, unknown field rejected by `KnownFields(true)`, or trailing multi-document YAML stream. | `view_id: ""`, `subject: <manifestPath>` |
| `INVALID_SCHEMA_VERSION` | Manifest is `nil` or `schema_version != "v1"`. | `view_id: ""`, `subject: <schema_version>` |

#### Components

| Error Code | Trigger Condition | `view_id` / `subject` |
| :--- | :--- | :--- |
| `EMPTY_COMPONENT_ID` | `component.id` is empty or whitespace. | `subject: components[i]` |
| `DUPLICATE_COMPONENT_ID` | Two components share the same `id`. | `subject: <component.id>` |
| `EMPTY_COMPONENT_TITLE` | `component.title` is empty or whitespace. | `subject: <component.id>` |
| `INVALID_COMPONENT_KIND` | `component.component_kind` is not one of the 8 valid `ComponentKind` values. | `subject: <component.id>` |
| `EMPTY_PROJECT_ID` | `component.project_id` is empty or whitespace. | `subject: <component.id>` |
| `INVALID_ICON_KEY` | `component.icon_key` is blank or not registered in the 85-icon embedded `icons.Registry`. | `subject: <component.id>` |
| `EMPTY_BACKED_BY` | `component.backed_by` has `0` entries. | `subject: <component.id>` |
| `INVALID_BACKED_RESOURCE` | A `backed_by` entry (on a component **or** container) has an empty `uri`, `asset_type`, or `project_id`. | `subject: <component.id \| container.id>` |
| `INLINE_META_LIMIT` | `len(component.inline_meta) > 4`. | `subject: <component.id>` |
| `INVALID_INLINE_META` | An `inline_meta` row has an empty `label` or `value`. | `subject: <component.id>` |
| `HETEROGENEOUS_DATASTORE` | `component.backed_by` mixes `> 1` distinct asset types from the 15-type `statefulDatastoreAssetTypes` set. | `subject: <component.id>` |
| `UNPLACED_COMPONENT` | A global component is not placed in at least one **detail view** (placement solely on `view_0_system_index` does not count). | `subject: <component.id>` |

#### Containers

| Error Code | Trigger Condition | `view_id` / `subject` |
| :--- | :--- | :--- |
| `EMPTY_CONTAINER_ID` | `container.id` is empty or whitespace. | `subject: containers[i]` |
| `DUPLICATE_CONTAINER_ID` | Two containers share the same `id`. | `subject: <container.id>` |
| `EMPTY_CONTAINER_TITLE` | `container.title` is empty or whitespace. | `subject: <container.id>` |
| `INVALID_CONTAINER_KIND` | `container.kind` is not one of the 6 valid `ContainerKind` values. | `subject: <container.id>` |
| `UNKNOWN_PARENT_CONTAINER` | `container.parent_container_id` does not match any defined container ID. | `subject: <container.id>` |
| `CONTAINER_CYCLE` | Walking `parent_container_id` links forms a cycle. | `subject: <container.id>` |
| `CONTAINER_DEPTH_EXCEEDED` | Active nesting depth of a container within `view.container_ids` exceeds `2`. | `view_id: <view.id>`, `subject: <container.id>` |

#### Exclusions, Resource Accounting & Component Pairs

| Error Code | Trigger Condition | `view_id` / `subject` |
| :--- | :--- | :--- |
| `INVALID_EXCLUDED_RESOURCE` | An `excluded_resources` entry has an empty `uri`, `asset_type`, or `project_id`. | `subject: <uri \| excluded_resources[i]>` |
| `MISSING_EXCLUSION_REASON` | An `excluded_resources` entry has an empty `reason`. | `subject: <uri \| excluded_resources[i]>` |
| `EXCLUDED_DATASTORE` | An `excluded_resources` entry has an `asset_type` in the 15-type `statefulDatastoreAssetTypes` set ([`validateExcludedResources`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/validate.go#L523-L525)). | `subject: <uri \| excluded_resources[i]>` |
| `DUPLICATE_ACCOUNTED_URI` | The same non-empty resource URI appears more than once across `components[].backed_by`, `containers[].backed_by`, and `excluded_resources` ([`validateAccountedURIUniqueness`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/validate.go#L342-L362)). | `view_id: ""`, `subject: <uri>` |
| `INVALID_REJECTED_EDGE` | A `rejected_edges` entry has an empty `from`, `to`, or `reason`. | `subject: rejected_edges[i]` |
| `UNKNOWN_PAIR_COMPONENT` | A `rejected_edges[i]` or `acknowledged_edges[i]` entry references a `from` or `to` ID not declared in top-level `components` ([`validateAcknowledgedEdges`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/validate.go#L364-L376), [`validateRejectedEdges`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/validate.go#L529-L541)). | `view_id: ""`, `subject: rejected_edges[i] \| acknowledged_edges[i]` |

#### Views & System Index (`View 0`)

| Error Code | Trigger Condition | `view_id` / `subject` |
| :--- | :--- | :--- |
| `NO_VIEWS` | `manifest.views` is empty (`0` views). | `view_id: ""`, `subject: ""` |
| `EMPTY_VIEW_ID` | `view.id` is empty or whitespace. | `subject: views[i]` |
| `DUPLICATE_VIEW_ID` | Two views share the same `id`. | `view_id: <view.id>`, `subject: <view.id>` |
| `EMPTY_VIEW_TITLE` | `view.title` is empty or whitespace. | `view_id: <view.id>`, `subject: <view.id>` |
| `VIEW_COMPONENT_ID_COLLISION` | A `view.id` collides with a top-level `component.id` (disallowed because `view_0_system_index` placements reference both namespaces; [`validateViewIndex`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/validate.go#L564-L566)). | `view_id: <view.id>`, `subject: <view.id>` |
| `TOO_MANY_DETAIL_VIEWS` | Manifest defines more than `16` (`maxDetailViews`) detail views, exceeding `view_0_system_index` capacity ([`validateViewIndex`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/validate.go#L568-L570)). | `view_id: ""`, `subject: ""` |
| `MISSING_SYSTEM_INDEX_VIEW` | `>= 2` detail views exist, but `view_0_system_index` is absent. | `view_id: ""`, `subject: view_0_system_index` |
| `UNEXPECTED_SYSTEM_INDEX_VIEW` | `< 2` detail views exist, yet `view_0_system_index` is present. | `view_id: view_0_system_index`, `subject: view_0_system_index` |
| `MISPLACED_SYSTEM_INDEX_VIEW` | `view_0_system_index` exists at an index `> 0` instead of `views[0]`. | `view_id: view_0_system_index`, `subject: view_0_system_index` |
| `INVALID_ARCHETYPE` | `view.archetype` is not `landscape_matrix`, `stage_pipeline`, or `hub_spoke`. | `view_id: <view.id>`, `subject: <archetype>` |
| `INVALID_ACCENT_MODE` | `view.accent_mode` is not `RUNTIME`, `CICD`, or `MIXED`. | `view_id: <view.id>`, `subject: <accent_mode>` |
| `UNKNOWN_CONTAINER_REF` | `view.container_ids` lists an ID not declared in top-level `containers`. | `view_id: <view.id>`, `subject: <container_id>` |

#### Placements & Off-Page References

| Error Code | Trigger Condition | `view_id` / `subject` |
| :--- | :--- | :--- |
| `VIEW_PLACEMENT_COUNT` | Detail view has `< 1` or `> 12` placements; `view_0_system_index` has `< 1` or `> 16` placements. | `view_id: <view.id>`, `subject: <view.id>` |
| `DUPLICATE_PLACEMENT` | The same `component_id` appears more than once in `view.placements`. | `view_id: <view.id>`, `subject: <component_id>` |
| `UNKNOWN_COMPONENT_REF` | On a detail view: `component_id` not found in `components`. On `view_0_system_index`: `component_id` matches neither a `component.id` nor a detail `view.id`. | `view_id: <view.id>`, `subject: <component_id>` |
| `PLACEMENT_CONTAINER_NOT_IN_VIEW` | `placement.container_id` is non-empty but absent from `view.container_ids`. | `view_id: <view.id>`, `subject: <component_id>` |
| `COLUMN_OUT_OF_BOUNDS` | `placement.column` is `< 0` or `> 4` (`landscape_matrix`/`stage_pipeline`), or `< 0` or `> 2` (`hub_spoke`). | `view_id: <view.id>`, `subject: <component_id>` |
| `INVALID_OFF_PAGE_REF` | `target_view_id` does not exist or equals the current view ID; `direction` is not `"IN"` or `"OUT"`; or `label` is blank. | `view_id: <view.id>`, `subject: <component_id>` |

#### Edges & Shared Bus Groups

| Error Code | Trigger Condition | `view_id` / `subject` |
| :--- | :--- | :--- |
| `EDGE_ENDPOINT_NOT_PLACED` | `edge.from` or `edge.to` is not present in `view.placements`. | `view_id: <view.id>`, `subject: "<from> -> <to>"` |
| `REJECTED_EDGE_PRESENT` | `(edge.from, edge.to)` matches an entry in top-level `rejected_edges`. | `view_id: <view.id>`, `subject: "<from> -> <to>"` |
| `EMPTY_EDGE_VERB` | `edge.verb` is empty or whitespace. | `view_id: <view.id>`, `subject: "<from> -> <to>"` |
| `EDGE_VERB_TOO_LONG` | `utf8.RuneCountInString(edge.verb) > 16`. | `view_id: <view.id>`, `subject: "<from> -> <to>"` |
| `INVALID_FLOW_KIND` | `edge.flow_kind` is not `RUNTIME`, `CICD`, or `ADMIN`. | `view_id: <view.id>`, `subject: "<from> -> <to>"` |
| `INVALID_PROVENANCE` | `edge.provenance` is not `VERIFIED` or `INFERRED`. | `view_id: <view.id>`, `subject: "<from> -> <to>"` |
| `MISSING_EVIDENCE_EDGE_IDS` | Detail-view `VERIFIED` edge has empty `evidence_edge_ids` or contains a blank ID string *(exempt on `view_0_system_index`)*. | `view_id: <view.id>`, `subject: "<from> -> <to>"` |
| `MISSING_INFERENCE_RATIONALE` | `INFERRED` edge has an empty or whitespace `inference_rationale`. | `view_id: <view.id>`, `subject: "<from> -> <to>"` |
| `INCONSISTENT_CROSS_VIEW_EDGE` | The same `(from, to, flow_kind)` edge appears in multiple detail views with a different `verb` or `provenance` ([`validateCrossViewConsistency`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/validate.go#L747-L759)). | `view_id: <view.id>`, `subject: "<from> -> <to>"` |
| `INVALID_BUS_GROUP` | Edges sharing one `via_bus_id` within a view contain both `> 1` distinct `from` sources AND `> 1` distinct `to` targets. | `view_id: <view.id>`, `subject: <via_bus_id>` |

---

## `--facts` Provenance & Coverage Validation (`ValidateWithFacts`)

Passing `--facts <facts.json>` runs all **54** offline structural rules **plus** **5** facts-coupled guarantees (**59** error codes total) via [`manifest.ValidateWithFacts`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/validate.go#L118-L139) ([`internal/manifest/validate.go`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/validate.go)) and [`EvidenceIndex.WalkIssue`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/evidence.go#L76-L113) ([`internal/manifest/evidence.go`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/evidence.go)):

```bash
gcp-arch validate architecture.yaml --facts facts.json
```

| Error Code | Trigger Condition | `view_id` / `subject` |
| :--- | :--- | :--- |
| `NIL_FACTS` | `facts` argument passed to `ValidateWithFacts` is `nil`. | `view_id: ""`, `subject: ""` |
| `UNACCOUNTED_FACT_RESOURCE` | A resource URI in `facts.Resources` does not appear in any of `components[].backed_by`, `containers[].backed_by`, or `excluded_resources`. | `view_id: ""`, `subject: <resource.uri>` |
| `UNKNOWN_EVIDENCE_EDGE` | A detail-view `VERIFIED` edge cites an `evidence_edge_ids` entry that does not exist in `facts.Edges`. | `view_id: <view.id>`, `subject: "<from> -> <to>"` |
| `INVALID_EDGE_PROVENANCE` | A detail-view `VERIFIED` edge fails [`EvidenceIndex.WalkIssue`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/evidence.go#L76-L113): cites a `Heuristic: true` `FactEdge`, touches a container-backed or third-component URI, fails the dual-BFS directed walk, or uses permission-only evidence (`IAM_RESOURCE_BINDING` / `WORKLOAD_IDENTITY`) on a non-`ADMIN` edge. | `view_id: <view.id>`, `subject: "<from> -> <to>"` |
| `PLACEMENT_CONTAINER_MISMATCH` | A detail-view placement assigns a component to a resource-backed container (`len(container.backed_by) > 0`, such as a GKE cluster or VPC), and the component's resources declare `parent_uri` metadata in `facts.json`, yet neither a component URI nor its `parent_uri` ancestor chain reaches any URI in `container.backed_by` ([`validatePlacementContainers`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/validate.go#L143-L208) -> `ParentIndex` / `ComponentInsideContainer`). Containers without `backed_by` and components with no `parent_uri` metadata pass. | `view_id: <view.id>`, `subject: <component_id>` |

### How `EvidenceIndex.WalkIssue` & the Dual-BFS Directed-Walk Proof Work

[`EvidenceIndex.WalkIssue`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/evidence.go#L12-L113) verifies every detail-view `VERIFIED` edge against `facts.Edges` and the manifest's component/container ownership before accepting it:

1. **No Heuristic Evidence (`fe.Heuristic`):** Rejects any cited `FactEdge` with `Heuristic: true` (for example, `MONITORING_TRAFFIC`, shared-SA BigQuery jobs, or single-target project IAM bindings) with `INVALID_EDGE_PROVENANCE`; heuristic edges may only inform `INFERRED` edges.
2. **No Container or Third-Component Shortcuts (`EvidenceIndex.blocked`):** Rejects any cited edge whose `FromURI` or `ToURI` belongs to a container's `backed_by` set (`containers[].backed_by`) or to a third component's `backed_by` set (`owner != from && owner != to`). Walks may only traverse URIs owned by `from`, `to`, or unowned/excluded helper resources, preventing edges from routing through a shared VPC/cluster container or bypassing an intermediate component.
3. **Dual-BFS Directed Walk (`isValidDirectedWalk`):** Proves via [`isValidDirectedWalk`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/validate.go#L831-L851) that the cited `evidence_edge_ids` form an authentic, unbroken causal chain from `from.backed_by` to `to.backed_by` without extraneous padding:

```mermaid
flowchart LR
    subgraph Valid ["Valid Multi-Hop Walk (PASS)"]
        A1["from.backed_by\n(GlobalForwardingRule)"] -->|"e1: K8S_INGRESS_BACKEND"| B1["Bridge / Folded\n(k8s.io/Service)"]
        B1 -->|"e2: K8S_SERVICE_SELECTOR"| C1["to.backed_by\n(apps.k8s.io/Deployment)"]
    end

    subgraph Invalid ["Extraneous Cited Edge (FAIL: INVALID_EDGE_PROVENANCE)"]
        A2["from.backed_by\n(Cloud Run Service)"] -->|"e3: IAM_RESOURCE_BINDING"| C2["to.backed_by\n(GCS Bucket)"]
        X2["Unrelated URI\n(not reachable from from.backed_by)"] -->|"e4: cited extra edge"| Y2["Dead-End URI\n(cannot reach to.backed_by)"]
    end
```

   * **Subgraph Construction:** Build forward adjacency (`fe.FromURI -> fe.ToURI`) and reverse adjacency (`fe.ToURI -> fe.FromURI`) using **only** the `FactEdge`s listed in `edge.evidence_edge_ids`.
   * **Forward BFS (`fwdReach`):** Seed BFS with every URI in `from.backed_by` and follow forward edges.
   * **Reverse BFS (`bwdReach`):** Seed BFS with every URI in `to.backed_by` and follow reverse edges.
   * **Universal Walk Check:** Every cited edge `fe` MUST satisfy **both** `fwdReach[fe.FromURI]` and `bwdReach[fe.ToURI]`. Because every cited edge is checked individually, a single disconnected or unrelated `FactEdge.ID` inside `evidence_edge_ids` fails validation with `INVALID_EDGE_PROVENANCE` even if another subset of cited edges connects `from` to `to`.

4. **Permission-Only Evidence Requires `flow_kind: ADMIN`:** If every cited `FactEdge` has kind `IAM_RESOURCE_BINDING` or `WORKLOAD_IDENTITY` ([`permissionEdgeKinds`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/evidence.go#L12-L16), [`permissionOnly`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/evidence.go#L106-L113)), `edge.flow_kind` MUST equal `ADMIN` (IAM/identity grants prove access rather than live runtime traffic or CI/CD delivery).

Edges on `view_0_system_index` are exempt from `evidence_edge_ids` and `WalkIssue` checks because they are deterministic view-level rollups rather than direct resource walks.

---

## `--json` Output & Git PR CI Workflow

Passing `--json` to `gcp-arch validate` emits a structured [`ValidationReport`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/types.go#L210-L222) ([`internal/manifest/types.go`](https://github.com/arrase/gcp-arch/blob/main/internal/manifest/types.go#L210-L222)):

```go
type ValidationReport struct {
    Valid         bool              `json:"valid"`
    ExcludedCount int               `json:"excluded_count,omitempty"`
    Errors        []ValidationIssue `json:"errors,omitempty"`
}

type ValidationIssue struct {
    Code    string `json:"code"`
    ViewID  string `json:"view_id,omitempty"`
    Subject string `json:"subject,omitempty"`
    Message string `json:"message"`
}
```

=== "Passing Manifest (Exit Code 0)"

    ```json
    {
      "valid": true,
      "excluded_count": 1
    }
    ```

    *(`excluded_count` is omitted when `excluded_resources` is empty.)*

=== "Failing Manifest (Exit Code 2)"

    ```json
    {
      "valid": false,
      "errors": [
        {
          "code": "EDGE_VERB_TOO_LONG",
          "view_id": "gke_developer_platform",
          "subject": "atlantis -> acme_tfstate",
          "message": "edge verb \"reads and writes terraform state\" has 32 chars (max 16)"
        },
        {
          "code": "REJECTED_EDGE_PRESENT",
          "view_id": "gke_developer_platform",
          "subject": "atlantis_https_lb -> acme_tfstate",
          "message": "edge (atlantis_https_lb -> acme_tfstate) is forbidden by rejected_edges"
        }
      ]
    }
    ```

| Exit Code | Meaning ([`cmd/gcp-arch/validate.go`](https://github.com/arrase/gcp-arch/blob/main/cmd/gcp-arch/validate.go#L71-L83)) |
| :---: | :--- |
| `0` | Manifest is valid. Prints `Valid: <file> (<V> views, <C> components)` (or `Valid: <file> (<V> views, <C> components, <E> excluded)` when `len(excluded_resources) > 0`), or `{"valid": true}` with `"excluded_count": <E>` when `> 0` under `--json`. |
| `1` | Runtime / I/O failure (e.g., unreadable or missing `architecture.yaml` or `--facts` file). |
| `2` | YAML decode failure (`YAML_DECODE_ERROR`), invalid `--facts` JSON/schema version, or one or more `ValidationError` issues. |

### GitHub Actions PR Validation Gate

Because `gcp-arch validate` needs zero GCP credentials and zero Chrome installation, it makes an ideal fast pull-request gate whenever engineers curate `architecture.yaml`:

```yaml
name: Validate Architecture Manifest

on:
  pull_request:
    paths:
      - "docs/architecture.yaml"

permissions:
  contents: read

jobs:
  validate-manifest:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Download gcp-arch static binary
        run: |
          curl -fL "https://github.com/arrase/gcp-arch/releases/latest/download/gcp-arch-linux-amd64" -o gcp-arch
          chmod +x gcp-arch

      - name: Validate architecture.yaml offline
        run: ./gcp-arch validate docs/architecture.yaml --json
```

---

## Day-2 Manual Curation Guide

Teams are encouraged to review and polish `architecture.yaml` in Git pull requests. The table below summarizes which manual edits are safe, which validation rules still apply, and how [Day-2 `--reconcile`](synthesis.md#day-2-reconciliation-reconcile) treats them:

| Editable Field | Safe Manual Curation Examples | Validation Rules to Respect | Behavior Under `--reconcile` |
| :--- | :--- | :--- | :--- |
| **`components[].title` & `subtitle`** | Rename auto-generated labels to domain terminology (e.g., `Checkout Service` instead of raw service slug). | `title` must remain non-empty. | **Frozen & preserved.** |
| **`components[].notes` & `views[].notes`** | Add ADR references, ownership notes, or security review context. | Any valid YAML string. | **Frozen & preserved.** |
| **`components[].inline_meta`** | Reorder, trim, or enrich card metadata rows (e.g., SLO tier, CIDR, VPC egress mode). | Max **`4`** rows; non-empty `label` and `value`. | **Frozen & preserved** (exact row order maintained). |
| **`components[].icon_key`** | Swap a component's icon to a more specific product icon. | Must exist in the 85-icon embedded registry (`INVALID_ICON_KEY`). | **Frozen & preserved.** |
| **`views[].placements` order & `column`** | Reorder cards top-to-bottom within a column (governs Stage 3 `allocateContainerBands` packing order and barycenter tie-breaking) or move a card to another valid column. | `column` within `0..4` (`0..2` for `hub_spoke`); `1..12` placements per detail view; every component placed in `>= 1` detail view. | **Preserved** on existing placements; new components are appended only to views with remaining capacity (`< 12`). |
| **`rejected_edges`** | Tombstone a noisy or architecturally misleading edge (remove it from `views[].edges` and add `{from, to, reason}` to `rejected_edges`). | Non-empty `from`, `to` (both must be declared top-level `components[].id`, otherwise `UNKNOWN_PAIR_COMPONENT`), and `reason`; matching edge must not remain in any view (`REJECTED_EDGE_PRESENT`). | **Frozen & strictly respected** across Paths A, B, and C (pruned by Path B only if either endpoint component is removed). |
| **`acknowledged_edges`** | Record non-heuristic inter-component pairs intentionally left undrawn so Day-2 reconciliation does not treat them as drift. | Both `from` and `to` must be declared top-level `components[].id` (`UNKNOWN_PAIR_COMPONENT`). | **Recomputed** from `undrawnProjectedPairs` on Paths A, B, and C (replacing `acknowledged_edges` with the current set of undrawn, unrejected non-heuristic projected pairs in `facts.Edges`; Path B also prunes removed components beforehand). |
| **Manual `INFERRED` edges** | Document logical or out-of-band flows not visible in CAI/Monitoring facts. | `provenance: INFERRED`, non-empty `inference_rationale`, `verb` `1..16` runes, endpoints placed in the view; identical `(from, to, flow_kind)` edges across detail views must share `verb` and `provenance` (`INCONSISTENT_CROSS_VIEW_EDGE`). | **Preserved** across all Day-2 paths as long as both endpoints remain placed. |
| **`excluded_resources`** | Move boilerplate resources out of components into `excluded_resources` with an audit reason. | Non-empty `uri`, `asset_type`, `project_id`, and `reason`; unique URI (`DUPLICATE_ACCOUNTED_URI`); must **not** exclude any of the 15 stateful datastore asset types (`EXCLUDED_DATASTORE`); remaining components must keep `>= 1` `backed_by` resource. | **Preserved** until the resource vanishes from GCP (at which point Path B prunes it). |

!!! warning "Edits That Trigger Reconciliation Deltas or Overwrites"
    * **Do not hand-edit `view_0_system_index` structure expecting layout overrides to survive drift:** On the zero-delta fast path, `view_0_system_index` is left untouched on disk. When topology or edge deltas trigger re-synthesis (`internal/synth/reconcile.go`), Go regenerates `view_0_system_index` deterministically while preserving curated `title`, `subtitle`, and `focus_summary` edits on matching views and curated `title`, `subtitle`, and `notes` on matching components.
    * **Removing URIs from `backed_by`:** Deleting an active GCP resource URI from `components[].backed_by` without placing it in another component, a container, `transversal_items`, or `excluded_resources` will fail `validate --facts` (`UNACCOUNTED_FACT_RESOURCE`) and cause the next `--reconcile` to detect a delta.

---

## Next Steps

* **Stage 2 Pipeline Internals:** Understand the deterministic containment skeleton, single bounded Vertex AI call, conservation gate, and Day-2 `--reconcile` in [Stage 2 — Gemini 4 Synthesis & Day-2 Reconciliation](synthesis.md).
* **Stage 3 Offline Rendering:** See how `architecture.yaml` drives nested Flexbox containers, Hanan-grid A* orthogonal routing, streaming PNG verification, and 1:1 vector PDF pagination in [Stage 3 — Offline Rendering & Routing](rendering.md).
* **Visual Reference Bundles:** Inspect complete real-world `architecture.yaml` manifests and their Retina rendered diagrams in [Examples & Visual Gallery](examples.md).
* **CLI & Configuration:** Review all flags, exit codes, and `config.yaml` defaults in [CLI & Configuration](cli_configuration.md).
