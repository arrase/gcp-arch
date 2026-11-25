# Semantic Multi-View GCP Architecture Pipeline (`gcp-arch`)

*   **Author:** Juan Ezquerro LLanes
*   **Status:** Implemented
*   **Date:** 2026-11-23

--------------------------------------------------------------------------------

## Overview

`gcp-arch` is a Go (`go 1.26.11` in [go.mod](../../go.mod)) command-line tool for cloud engineers and consultants who generate Google Cloud Platform (GCP) architecture diagrams for client handoff documents (PDF reports and Google Docs) and engineer onboarding. Previously, the retired v2 `gcp-arch` relied on 15 static Go graph-compaction passes, static `Tier 1–3` edge rules, and a coordinate layout built on `elk-go` (an Eclipse Layout Kernel port for Go) whose headless CLI exporter (`gcp-arch export`) emitted only Scalable Vector Graphics (`.svg`) and Draw.io (`.drawio`) files (requiring interactive browser clicks in `gcp-arch explore` for `.png` and offering no vector `.pdf` output). When evaluated against a reference CI/CD platform (`acme-cicd`), that static pipeline failed to communicate how the system worked: it collapsed active delivery pipelines into footer counters, hid Kubernetes applications inside opaque workload totals, left backing databases disconnected, exposed raw load-balancer hashes, and flooded diagrams with unclassified Kubernetes, backup, and discovery-API entries.

Because `gcp-arch` had zero external production users requiring backward compatibility, this architecture retired ~80% of the legacy v2 codebase (deleting the retired `internal/compiler`, `internal/layout`, `internal/drawio`, `internal/svg`, `internal/explore`, `internal/theme`, `internal/ir`, `internal/catalog`, and `internal/ingest` packages), retained the zero-dependency [internal/ioutil](../../internal/ioutil) atomic-write leaf package ([internal/ioutil/atomic.go](../../internal/ioutil/atomic.go), [internal/ioutil/atomic_test.go](../../internal/ioutil/atomic_test.go)), and implemented a three-stage pipeline across five domain packages ([internal/extract](../../internal/extract), [internal/icons](../../internal/icons), [internal/manifest](../../internal/manifest), [internal/synth](../../internal/synth), and [internal/render](../../internal/render)) orchestrated by [cmd/gcp-arch](../../cmd/gcp-arch):

1.  **Stage 1 (`extract` -> `facts.json`):** Implemented in [internal/extract](../../internal/extract), salvaging low-level GCP API transport, rate limiters, error classification, credential scrubbing, structural reference paths (from retired v2 `base_catalog.yaml`), and private-IP joiners (from retired v2 `internal/compiler`), while expanding read-only extraction across Cloud Asset Inventory (CAI, GCP's resource metadata inventory), Identity and Access Management (IAM) policies, Google Kubernetes Engine (GKE) Ingress/Service/Workload chains, Cloud Build triggers and recent succeeded build step references folded directly from the CAI stream, Cloud Monitoring runtime traffic (including Vertex AI `PublisherModel` invocation and dedicated-throughput metric synthesis in [internal/extract/enrichers.go](../../internal/extract/enrichers.go)), active Dataflow job lineage, and BigQuery SQL lineage into a canonical `v3` `facts.json` file.
2.  **Stage 2 (`synthesize` -> `architecture.yaml`):** Implemented in [internal/synth](../../internal/synth) and validated by [internal/manifest](../../internal/manifest). Runs a two-pass Gemini 4 synthesis on Vertex AI (billed to a dedicated `--vertex-project`, with optional `--thinking-level`) over short-ID-interned facts (`r_0001`, `e_0001`). Pass 1 partitions the system over deduplicated short-ID adjacency into focused `6–12` block logical views and assigns each view one of three layout archetypes (`landscape_matrix`, `stage_pipeline`, or `hub_spoke`). Pass 2 populates human-readable component titles, technical sublabels, inline card metadata rows, and provenance-checked edges (`VERIFIED` vs. `INFERRED`), while Go deterministically projects `View 0` (System Index Map, [internal/synth/view0.go](../../internal/synth/view0.go)) whenever `>= 2` detail views exist and validates the resulting Git-tracked `architecture.yaml` manifest.
3.  **Stage 3 (`render` -> `.pdf`, `3x .png`, `.html`):** Implemented in [internal/render](../../internal/render) using [internal/icons](../../internal/icons). Renders `architecture.yaml` offline (with zero dependency on `facts.json`) using a fixed-width HTML and CSS Grid template system with bundled `.woff2` fonts (`Inter` and `JetBrains Mono`) and embedded inline SVG `<symbol>` service icons. A local headless Chrome or Chromium process driven via `chromedp` lays out each archetype grid with flat background container frames and reserved CSS routing corridors (vertical `.corridor-col` and horizontal `.corridor-row` tracks indexed by `RoutingEdge.WaypointRow` for multi-column routing), runs an in-page script ([internal/render/assets/router.js](../../internal/render/assets/router.js)) over measured `getBoundingClientRect()` coordinates to route orthogonal SVG connectors with zero card, badge, label, or collinear overlaps, and exports vector `.pdf` pages (one page per view), high-resolution `3x .png` images for Google Docs, and a self-contained `.html` architecture dossier.

## The Problem

### User Need and Business Context

Engineers and consultants auditing or handing over a live GCP environment need page-fitted diagrams that explain two things within seconds to another engineer: what applications run in the environment, and how traffic, data, and deployment pipelines flow between them. Those diagrams are delivered inside written PDF handoff packages and pasted into collaborative Google Docs specifications, while the underlying source definition stays in Git alongside infrastructure repositories so teams can review and maintain architecture changes over time.

### Why the Retired Static Pipeline Failed

Auditing the retired `v2` static compiler and `elk-go` renderer against the historical `acme-cicd` `v2` evaluation exposed four systemic failures:

1.  **Primary workflows buried in static counters:** Static compaction rules classified resources by rigid service-type buckets (`ZoneKind` and `LayerRole` in the retired v2 `internal/ir`) without understanding workload roles. In `acme-cicd`, the project's central purpose—building container images from GitHub (`github-acme`) across 20 Cloud Build triggers into 643 Artifact Registry images (`acme-apps`) and running Terraform pull-request automation—was routed by `TRANSVERSAL_COUNTER` and `TRANSVERSAL_CONTROL_API` rules (in retired v2 `internal/catalog/assets/base_catalog.yaml` and `internal/compiler/compact_finalize.go`) into a disconnected gray footer badge (`Cloud Build: Count 852`), while `acme-tfstate` and `acme-private-modules` sat in an unlinked Cloud Storage stack.
2.  **Destructive GKE and Cloud Build ingestion:** The retired v2 sanitizer (`internal/ingest/sanitize.go`) folded `k8s.io/Namespace` and `apps.k8s.io/{Deployment,StatefulSet,DaemonSet}` objects into integer counts and discarded `k8s.io/Service` and `networking.k8s.io/Ingress` objects outright. As a result, the `cicd` GKE cluster rendered as a single opaque box (`Workloads: 45`) hiding `Atlantis` and `SonarQube`; external HTTPS load balancers displayed raw generated forwarding-rule hashes (`k8s2-fs-demo1234-...`) instead of linking through Network Endpoint Groups (NEGs, GCP load-balancer backends mapping Kubernetes pods) to their services; and the `sonarqube` Cloud SQL database floated with zero edges. Similarly, `sanitizeResourceData` scanned `containers[].args` and `containers[].env` but never traversed Cloud Build `steps[]` / `build.steps[]` (nor matched `*.pkg.dev`, `--project`, `--cluster`, or `gcloud run deploy`), retired `base_catalog.yaml` listed `cloudbuild.googleapis.com/Trigger` instead of CAI's actual `cloudbuild.googleapis.com/BuildTrigger` type, and all 819 historical `Build` runs remained unfolded.
3.  **Plumbing leaks, URI drift, tenant project confusion, and read-call noise:** Unhandled Kubernetes, backup, and zero-instance API entries surfaced as `Standalone Unclassified Service ×27` in the retired `acme-cicd` `v2` output (`17` uncataloged CAI resource-type cards such as `k8s.io/ServiceAccount`, `k8s.io/Secret`, `k8s.io/Endpoints`, and `sqladmin.googleapis.com/BackupRun` plus `10` zero-asset `consumed_api` cards, `4` of which—`firestore`, `redis`, `memcache`, and `osconfig`—stemmed from `~22` read-only `terraform plan` state refreshes), while `"data"` and `"stringdata"` keys were not stripped from `ConfigMap` payloads. Inconsistent network URI formats (`/locations/global/networks/cicd` vs. `/global/networks/cicd`) created duplicate `VPC: cicd` containers under `--expand-standalone`. Google-managed Private Services Access (PSA) tenant projects (`p1234567890abcdef-tp`, internal projects created by Google to host managed Cloud SQL instances) rendered as external customer peer projects.
4.  **Mechanical graph layout and CLI output gaps:** Even when restricted to 6–12 nodes, `elk-go` computed mechanical compound container boxes with uneven internal whitespace, could not align multi-column editorial grids or inline metadata tables, and relied on approximate character-width tables (retired v2 `internal/layout/textwrap.go`) that clipped or drifted when PDF viewers substituted system fonts. Furthermore, headless CLI `gcp-arch export` emitted only `.svg` and `.drawio` files—neither of which could be inserted directly into Google Docs (which requires raster `.png`) or delivered as paginated vector `.pdf` reports.

## Goals

*   **Clean Go Codebase Reset:** Retire legacy v2 packages (`internal/compiler`, `internal/layout`, `internal/drawio`, `internal/svg`, `internal/explore`, `internal/theme`, `internal/ir`, `internal/catalog`, `internal/ingest`) and remove direct dependencies `github.com/d2lang/elk-go`, `github.com/beevik/etree`, and direct `cloud.google.com/go/accesscontextmanager` from [go.mod](../../go.mod) (`cloud.google.com/go/accesscontextmanager` remains only as an `// indirect` transitive dependency of `cloud.google.com/go/asset`), retaining zero-dependency [internal/ioutil](../../internal/ioutil) alongside five pipeline packages ([internal/extract](../../internal/extract), [internal/icons](../../internal/icons), [internal/manifest](../../internal/manifest), [internal/synth](../../internal/synth), [internal/render](../../internal/render)) behind five CLI commands (`preflight`, `extract`, `synthesize`, `validate`, `render`), `--version`, and persistent user defaults in [cmd/gcp-arch/config.go](../../cmd/gcp-arch/config.go).
*   **Structural Multi-Project `v3` Fact Extraction (`extract`):** Produce a credential-scrubbed `facts.json` (`0600`) within `<= 80` CAI requests per minute that retains GKE workloads (`Deployment`, `StatefulSet`, `DaemonSet`, `CronJob`, `Job` with namespace metadata), `Service` (`cloud.google.com/neg-status` and L4 LoadBalancer IPs), `Ingress`, and Workload Identity links; retains `BuildTrigger` plus sanitized step references folded in-stream from the latest 1–3 succeeded CAI `Build` records per trigger; captures `Organization`/`Folder`/`Project`/resource IAM policies, runtime Cloud Monitoring edges (filtering read-only `resource.labels.method` calls and synthesizing Vertex AI `PublisherModel` resources/edges when `aiplatform.googleapis.com` is enabled), active Dataflow job lineage, BigQuery SQL lineage, and salvaged structural/private-IP `CONFIG_REF` links; canonicalizes [AIP-122](https://google.aip.dev/122) (Google API Improvement Proposal 122: Resource Names) URIs; classifies `*-tp` Google tenant projects as PSA backing zones; enforces per-asset-type attribute allowlists; and emits synthetic external/cross-project `FactResource` stubs plus `stderr` scope hints.
*   **Closed-World Two-Pass Vertex AI Synthesis & Day-2 Reconciliation (`synthesize`, `validate`):** Decompose `facts.json` (`<= 500` active resources) via Vertex AI Gemini 4 (`google.golang.org/genai`, decoupled `--vertex-project`, `--vertex-location`, `--model`, and optional `--thinking-level`) using short-ID interning (`r_XXXX`, `e_XXXX`) into a Git-tracked `architecture.yaml` containing focused logical views (plus a Go-projected `View 0` system index when `>= 2` detail views exist), enforcing 100% non-plumbing resource accounting across components, containers, and exclusions (`manifest.ValidateWithFacts`), directed-walk `FactEdge` provenance (`VERIFIED` vs. footnoted `INFERRED`), stateful datastore homogeneity, zero-LLM fast paths when URIs and projected edges are unchanged, and delta `--reconcile` updates on Day 2.
*   **Editorial HTML/CSS Grid & Chrome CDP Rendering (`render`):** Render `architecture.yaml` standalone and offline (zero network calls, zero `facts.json` dependency, bundled `Inter` and `JetBrains Mono` `.woff2` fonts, embedded inline `<symbol>` service icons from [internal/icons/assets/icons.yaml](../../internal/icons/assets/icons.yaml)) across three fixed-width layout archetypes (`landscape_matrix`, `stage_pipeline`, `hub_spoke`, flat background container frames up to depth `2`, `data-cols="5"` compact typography/icon sizing, inline card metadata rows, dynamic card `min-height` scaling for `>= 3` ports, CSS `.corridor-col` and `.corridor-row` waypoint tracks, directionally partitioned shared bus spines, `4px` corridor bridges, and print-time `--print-scale` fit) with automated zero-collision verification, exporting vector `.pdf` (one page per view), `3x .png`, and standalone `.html` in `<= 2 s` via `chromedp`.
*   **`acme-cicd` Acceptance Benchmark:** On [testdata/acme_cicd_v3.facts.json](../../testdata/acme_cicd_v3.facts.json) / [testdata/acme_cicd_expected.architecture.yaml](../../testdata/acme_cicd_expected.architecture.yaml), automatically unpack and render both the CI/CD delivery pipeline (`GitHub github-acme -> 20 Cloud Build triggers -> Artifact Registry acme-apps` plus cross-project deployment target stubs) and the GKE application ingress flows (`HTTPS Load Balancers -> GKE cicd Ingress/NEG -> Atlantis -> gs://acme-tfstate & gs://acme-private-modules`, and `SonarQube -> Cloud SQL sonarqube`) with human-readable titles, zero unclassified K8s/backup warning cards, zero card/header/label/collinear collisions, and 100% audited resource coverage.

## Non-Goals

*   **Local IaC Parsing:** Parsing local Terraform (`.tf`) HCL files, remote Terraform module sources, or `*.tfstate` state files.
*   **Data or Secret Payload Inspection:** Reading Secret Manager secret versions, Kubernetes `Secret` payload values, Cloud Storage object contents, or BigQuery table rows.
*   **Legacy Diagram Editors and Formats:** Generating `.drawio`, Excalidraw, or standalone `.svg` files, or shipping an interactive web explorer (`gcp-arch explore`) or mouse-dragging canvas editor.
*   **Backward Compatibility with `v2` Artifacts:** Supporting legacy `.snap` files, `.gcp-arch/decisions.yaml`, `L0`/`L1`/`L2` `elk-go` views, or deprecated `snapshot`/`views`/`explore`/`export`/`inspect` CLI subcommands.

## Open Questions

*   None.

## Critical User Journeys (CUJs)

### CUJ 1: Day-1 Multi-View Handoff Generation on a GKE + CI/CD Project (`acme-cicd`)

*   **Given:** A consultant holds read-only GCP credentials (`cloud-platform.read-only`) on target project `acme-cicd`, Vertex AI user access (`roles/aiplatform.user` with `cloud-platform` scope) on billing project `consultant-ai-sandbox`, and Google Chrome installed locally (even if a desktop Chrome session is already open).
*   **When:** The consultant runs:
    1.  `gcp-arch preflight --projects acme-cicd --vertex-project consultant-ai-sandbox`
    2.  `gcp-arch extract --projects acme-cicd -o facts.json`
    3.  `gcp-arch synthesize facts.json --vertex-project consultant-ai-sandbox -o architecture.yaml`
    4.  `gcp-arch render architecture.yaml --format pdf,png,html -o out/`
*   **Then:**
    *   `extract` writes sanitized `facts.json` (`0600`), retaining the `cicd` GKE cluster's `atlantis` and `sonarqube` workloads, `Service` NEG annotations, `Ingress` host/path rules, 20 `BuildTrigger` definitions with step references folded in-stream from their latest succeeded CAI `Build` records (`gs://acme-tfstate`, `gs://acme-private-modules`, Artifact Registry `acme-apps`), private-IP `SonarQube -> Cloud SQL sonarqube` linkage, PSA `*-tp` peering, synthetic external `FactResource` stubs for `github-acme` and external deploy projects, and an explicit `stderr` scope hint listing those external target projects.
    *   `synthesize` interns resource URIs and `FactEdge` records to short IDs, runs Pass 1 (with deduplicated short-ID adjacency) and Pass 2 against Gemini 4 (`gemini-4-carbon`) in `consultant-ai-sandbox`, deterministically projects `View 0` (System Index) in Go across the resulting detail views (`CI/CD Delivery Pipeline`, `GKE Developer Platform & Ingress`, `Shared Network & PSA Topology`), validates 100% resource accounting and directed-walk `FactEdge` provenance via `manifest.ValidateWithFacts`, and writes `architecture.yaml`.
    *   `render` launches one isolated headless Chrome session (`--user-data-dir=<tempdir>`) offline, verifies zero card, badge, verb-pill, and collinear overlaps across all views, and writes `out/architecture.pdf` (vector multi-page PDF with exactly `len(views)` pages, one per view and no separate Inventory Coverage Table sheet), `out/views/00-view_0_system_index.png` .. `0N-<view_id>.png` (`3x` raster views ready for Google Docs), and `out/architecture.html`.

### CUJ 2: Day-2 Git Curation Preservation and Incremental Drift Reconciliation

*   **Given:** An engineer previously committed `architecture.yaml` to Git after renaming a block title to `"Atlantis PR Automation"`, adding a `notes` annotation, adding a `rejected_edges` tombstone for a noisy monitoring correlation, and adjusting a view subtitle. Two weeks later, the team deploys a new Cloud Run service (`release-webhook`), adds a Pub/Sub topic (`build-events`), and wires an existing Cloud Build trigger to a new bucket in `acme-cicd`.
*   **When:** The engineer runs `gcp-arch extract --projects acme-cicd -o facts.json`, followed by `gcp-arch synthesize facts.json --reconcile architecture.yaml --vertex-project consultant-ai-sandbox`, `gcp-arch validate architecture.yaml --facts facts.json`, and `gcp-arch render architecture.yaml -o out/`.
*   **Then:**
    *   Whenever `facts.json` has zero added/vanished resource URIs and zero added/vanished inter-component `FactEdge` projections relative to `architecture.yaml`, `synthesize --reconcile architecture.yaml` exits in `< 100 ms` with `0` Vertex AI calls (requiring no `--vertex-project`) and zero diff to `architecture.yaml`. Similarly, deletion-only drift (vanished URIs or edges leaving zero unclaimed resources and zero new inter-component edges) prunes vanished entries—and deletes any detail view whose placements all vanish along with `off_page_refs` targeting it—in `0` Vertex AI calls without `--vertex-project`.
    *   When `release-webhook`, `build-events`, or new inter-component `FactEdge`s appear, `synthesize --reconcile` prunes vanished URIs/edges (and any emptied detail views plus targeting `off_page_refs`), freezes all existing view IDs, archetypes, human-edited block titles (`"Atlantis PR Automation"`), `notes`, and `rejected_edges` tombstones, requests a placement/edge patch only for the unclaimed resources and newly wired edges (with up to `2` validation-feedback retries), validates the merged manifest (`1..12` blocks per detail view), and atomically writes canonical `architecture.yaml` in place when canonical YAML bytes changed.

### CUJ 3: Deterministic Validation Failure in Local/Artifact Provenance CI and Clean-Checkout Git PR CI

*   **Given:** During manual editing of `architecture.yaml` in a pull request, an author (a) introduces an invalid icon key or breaks single-source `via_bus_id` rules, or (b) in a pipeline with extracted `facts.json`, marks an unbacked cross-service edge as `provenance: VERIFIED` and deletes a Cloud SQL instance from its view without adding it to `excluded_resources`.
*   **When:** Clean-checkout Git PR CI runs `gcp-arch validate architecture.yaml` (requiring no gitignored `facts.json` and zero network calls), or an extraction/artifact CI job runs `gcp-arch validate architecture.yaml --facts facts.json` (zero network calls).
*   **Then:**
    *   Without `--facts`, `validate` ([cmd/gcp-arch/validate.go](../../cmd/gcp-arch/validate.go), [internal/manifest/validate.go](../../internal/manifest/validate.go)) checks schema integrity, `KnownFields(true)`, archetype column bounds, container depth `<= 2`, valid icon keys, single-source `via_bus_id`, and `rejected_edges` compliance, exiting `2` on any structural violation.
    *   With `--facts facts.json`, `validate` additionally enforces directed-walk `FactEdge` provenance and 100% `FactsFile` resource accounting, exiting `2` with actionable errors identifying (a) the exact view and edge whose `evidence_edge_ids` fail to form a directed walk from `from.backed_by` to `to.backed_by`, and (b) the orphaned Cloud SQL AIP-122 URI missing from `components[].backed_by`, `containers[].backed_by`, and `excluded_resources`.

## Technical Plan (Architecture)

### System Architecture and Package Topology

`gcp-arch` is structured as a strictly linear three-stage pipeline over two persistent on-disk contracts: `facts.json` (`schema_version: "v3"`, machine-generated plain UTF-8 JSON, `0600` permissioned and gitignored) and `architecture.yaml` (`schema_version: "v1"`, self-contained for offline rendering and structural validation, human-editable, and tracked in Git).

```mermaid
flowchart LR
    GCP["Live GCP Scopes (CAI, IAM, Monitoring, Dataflow, BigQuery)"] --> Extract["Stage 1: internal/extract (gcp-arch extract)"]
    Extract --> Facts["facts.json (v3, 0600, gitignored)"]
    Facts --> Synth["Stage 2: internal/synth (gcp-arch synthesize)"]
    Vertex["Vertex AI Gemini 4 (--vertex-project)"] <--> Synth
    Synth --> Manifest["architecture.yaml (v1, Git-tracked)"]
    Facts -. "--facts (optional provenance check)" .-> Validate["internal/manifest (gcp-arch validate)"]
    Manifest --> Validate
    Validate --> Render["Stage 3: internal/render (gcp-arch render)"]
    Icons["internal/icons (Embedded SVGs + .woff2 Fonts)"] --> Render
    Render --> HTML["architecture.html (Standalone Offline Dossier)"]
    Render <--> CDP["Headless Chrome / Chromium (chromedp)"]
    CDP --> PDF["architecture.pdf (Vector Pages, 1 per View)"]
    CDP --> PNG["views/*.png (3x Raster Views for Google Docs)"]
```

#### Package Disposition and Dependency Changes

| Path / Module | Action | Role in Implemented Architecture |
| --- | --- | --- |
| Retired v2 `internal/compiler`, `internal/layout`, `internal/drawio`, `internal/svg`, `internal/explore`, `internal/theme`, `internal/ir`, `internal/catalog`, `internal/ingest` | **Deleted (Phase 4)** | Retired the 15 static compaction passes, `ZoneKind`/`LayerRole` IR types, `elk-go` coordinate slicers, Draw.io/SVG emitters, and loopback web explorer after salvaging reusable ingestion, structural reference rules, plumbing rules, private-IP matching, and `<symbol>` def-hoisting into the v3 packages. |
| [internal/ioutil](../../internal/ioutil) | **Retained** | Zero-dependency crash-safe sibling-temp-file writer (`WriteFileAtomic(path, data, filePerm, dirPerm)` in [internal/ioutil/atomic.go](../../internal/ioutil/atomic.go), unit-tested in [internal/ioutil/atomic_test.go](../../internal/ioutil/atomic_test.go)) shared across `extract`, `manifest`, and `render` without coupling `manifest` or `render` to GCP SDK imports. |
| `github.com/d2lang/elk-go`, `github.com/beevik/etree`, direct `cloud.google.com/go/accesscontextmanager` | **Removed direct deps from [go.mod](../../go.mod)** | No longer directly used once `elk-go`, Draw.io XML generation, and legacy VPC-SC `ACCESS_POLICY` perimeter borders were retired (`cloud.google.com/go/accesscontextmanager` remains only as an `// indirect` transitive dependency pulled by `cloud.google.com/go/asset`). |
| `google.golang.org/genai`, `github.com/chromedp/chromedp` | **Added to [go.mod](../../go.mod)** | Official Google Gen AI Go SDK (`v1.79.1`, `BackendVertexAI`) for Pass 1, Pass 2, `--reconcile`, and `ProbeVertexModel` calls, and pure-Go `chromedp` (`v0.24.0`, `CGO_ENABLED=0`) for isolated headless Chrome target/session management, `3x` screenshots, and `printToPDF`. |
| [internal/icons](../../internal/icons) | **Created** | Embeds `85` validated inline SVG icons in [internal/icons/assets/icons.yaml](../../internal/icons/assets/icons.yaml) (`80` salvaged from retired v2 `internal/catalog/assets/icons.yaml` + `5` new `24x24` pipeline icons: `github`, `external_client`, `private_services_access`, `k8s_workload`, `k8s_ingress`), strict XML/CSS [ValidateSVG](../../internal/icons/validate.go), and `<defs>`-hoisting `compileIconSymbol` in [internal/icons/registry.go](../../internal/icons/registry.go) (salvaged from retired v2 `internal/svg/render.go`). |
| [internal/extract](../../internal/extract) | **Created** | Multi-project read-only GCP collector ([cai.go](../../internal/extract/cai.go), [client.go](../../internal/extract/client.go), [constants.go](../../internal/extract/constants.go)), Compute-aware AIP-122 URI canonicalizer ([canonical.go](../../internal/extract/canonical.go)), plumbing/default suppression & allowlisted scrubber ([sanitize.go](../../internal/extract/sanitize.go)), GKE chains ([gke.go](../../internal/extract/gke.go)), structural/private-IP/external-stub resolvers ([edges.go](../../internal/extract/edges.go)), and Monitoring (`consumed_api` + Vertex AI `PublisherModel`)/Dataflow/BigQuery enrichers ([enrichers.go](../../internal/extract/enrichers.go)). |
| [internal/manifest](../../internal/manifest) | **Created** | Canonical `architecture.yaml` Go structs and enums, strict `gopkg.in/yaml.v3` (`KnownFields(true)`) parser/serializer ([types.go](../../internal/manifest/types.go)), and structural + `--facts` provenance/100%-accounting validator (`ValidateStructure`, `ValidateWithFacts` in [validate.go](../../internal/manifest/validate.go)). |
| [internal/synth](../../internal/synth) | **Created** | Short-ID (`r_XXXX`, `e_XXXX`) bidirectional interning table and deduplicated Pass-1 adjacency projector ([intern.go](../../internal/synth/intern.go)), `<= 500` active-resource guard, Vertex AI Gemini 4 client with `ThinkingLevel` and `ProbeVertexModel` ([client.go](../../internal/synth/client.go)), non-recursive `genai.Schema` definitions ([schemas.go](../../internal/synth/schemas.go)), prompts ([prompts.go](../../internal/synth/prompts.go)), deterministic Go `View 0` builder ([view0.go](../../internal/synth/view0.go)), and Pass 1/Pass 2/Day-2 `--reconcile` orchestrator ([pipeline.go](../../internal/synth/pipeline.go)). |
| [internal/render](../../internal/render) | **Created** | Fixed-width offline HTML/CSS Grid archetype generator (`landscape_matrix`, `stage_pipeline`, `hub_spoke`), bundled `.woff2` font & `<symbol>` sprite injector ([html.go](../../internal/render/html.go)), barycenter/container/waypoint/bus pre-processor ([prepare.go](../../internal/render/prepare.go)), stylesheet ([diagram.css](../../internal/render/assets/diagram.css)), embedded JS corridor router ([router.js](../../internal/render/assets/router.js)), and single-session `chromedp` exporter ([cdp.go](../../internal/render/cdp.go)). |

#### CLI Command Surface, User Config File, and Exit Codes ([cmd/gcp-arch](../../cmd/gcp-arch))

The root command in [cmd/gcp-arch/main.go](../../cmd/gcp-arch/main.go) exposes `--version` (`var version = "dev"`, stamped at release build time via `-ldflags "-s -w -X main.version=${VERSION}"` in [.github/workflows/release.yml](../../.github/workflows/release.yml)) and loads optional persistent user defaults in `PersistentPreRunE` via [cmd/gcp-arch/config.go](../../cmd/gcp-arch/config.go):

*   **User Config Path & Strict Validation:** Root `PersistentPreRunE` loads `~/.config/gcp-arch/config.yaml` (or `$XDG_CONFIG_HOME/gcp-arch/config.yaml` when `XDG_CONFIG_HOME` is set) across **all** subcommands—including `validate`. A missing file is silently ignored. When present, `loadConfigFile` decodes with `yaml.NewDecoder` + `KnownFields(true)`, rejects trailing YAML documents, accepts either a YAML list or a single YAML scalar via `stringSlice.UnmarshalYAML` for `org`, `folder`, `projects`, and `format`, and validates every field (`org`, `folder`, `projects`, `project_aliases`, `quota_project`, `vertex_project`, `vertex_location`, `model`, `thinking_level`, `concurrency >= 1`, `cloudbuild_runs >= 1`, `chrome_path`, `format`), exiting with code `2` on any schema or value error.
*   **Precedence & Merge Semantics:** `CLI flags > environment variables > config.yaml > built-in defaults`, where environment-variable precedence applies specifically to `CHROME_PATH` (Chrome binary discovery) and `XDG_CONFIG_HOME` (config directory resolution). On `preflight` and `extract`, passing any CLI scope flag (`--org`, `--folder`, or `--projects`/`--project`) replaces the entire scope set from `config.yaml`, whereas `--project-alias` flags overlay on top of `config.yaml` `project_aliases` (CLI overrides per project number via `mergeProjectAliases`).
*   **Exit Codes:** All commands return exit code `0` on success, `1` on runtime or external I/O failure (GCP auth/quota failure, Vertex AI RPC error, missing Chrome binary when `pdf`/`png` is requested or an explicit `--chrome-path` does not exist), and `2` on CLI flag/config error, active-resource cap overflow (`> 500`), `validate` rule violation, or `render` card/badge/pill/collinear collision failure:

1.  `gcp-arch preflight (--org <id> | --folder <id> | --projects <p1,p2>) [--project-alias <NUM=ID>] [--quota-project <id>] [--vertex-project <id>] [--vertex-location global] [--model gemini-4-carbon] [-q|--quiet] [--json]` ([cmd/gcp-arch/preflight.go](../../cmd/gcp-arch/preflight.go)): Probes target read-only permissions (`cloudasset.assets.listResource`, `cai.folders` / `roles/resourcemanager.folderViewer` on `--org` and `--folder` scopes via `probeCAIFolderAccess`, `monitoring.timeSeries.list`, `bigquery.jobs.list`, `dataflow.jobs.list`) using `cloud-platform.read-only` credentials, and when `--vertex-project` is supplied, runs a zero-token Vertex AI readiness check (`probeVertexReadiness`: `projects.testIamPermissions` for `aiplatform.endpoints.predict` plus `VertexPreflightOptions.ModelProbe` wired to `synth.ProbeVertexModel` / `Models.Get`, falling back to `probeVertexModelREST` when `ModelProbe` is `nil`, in `--vertex-location` using `cloud-platform` OAuth scope). (`--project` is accepted as an alias for `--projects` on `preflight` and `extract`.)
2.  `gcp-arch extract (--org <id> | --folder <id> | --projects <p1,p2>) [--project-alias <NUM=ID>] [--quota-project <id>] [--cloudbuild-runs 3] [-q|--quiet] [-o facts.json]` ([cmd/gcp-arch/extract.go](../../cmd/gcp-arch/extract.go)): Streams rate-limited multi-project facts into `facts.json` (`0600`, `0700` parent dir) and prints discovered out-of-scope deployment target projects and degraded coverage warnings to `stderr`. Passing `-q`/`--quiet` suppresses progress output and the `stdout` summary line while still emitting actionable out-of-scope target hints (`writeOutOfScopeHints`) and degraded coverage warnings (`writeDegradedCoverage`) to `stderr`.
3.  `gcp-arch synthesize <facts.json> [--vertex-project <id>] [--vertex-location global] [--model gemini-4-carbon] [--thinking-level minimal|low|medium|high|xhigh] [--reconcile <architecture.yaml>] [-o architecture.yaml] [--concurrency 4] [-q|--quiet]` ([cmd/gcp-arch/synthesize.go](../../cmd/gcp-arch/synthesize.go)): Generates a new `architecture.yaml` (Day 1, requiring `--vertex-project`) or reconciles resource/edge deltas into `<architecture.yaml>` (Day 2; writes atomically to `<architecture.yaml>` when `-o` is omitted, skips `--vertex-project` validation on both unchanged and deletion-only prune-only `0`-LLM paths via `lazyModelClient`, and skips writing when canonical marshaled YAML is unchanged in place). `--thinking-level` is omitted by default so Vertex AI applies the model default (also configurable via `thinking_level` in `config.yaml`). Fails with exit code `2` if `len(activeResources) > 500` or post-synthesis validation fails.
4.  `gcp-arch validate <architecture.yaml> [--facts <facts.json>] [--json]` ([cmd/gcp-arch/validate.go](../../cmd/gcp-arch/validate.go)): Runs offline structural validation (`ValidateStructure`) when `--facts` is omitted, and full directed-walk `FactEdge` provenance + 100% `FactsFile` resource accounting (`ValidateWithFacts`) when `--facts` is supplied, with zero network calls.
5.  `gcp-arch render <architecture.yaml> [--format pdf,png,html] [-o out/] [--chrome-path <bin>] [-q|--quiet]` ([cmd/gcp-arch/render.go](../../cmd/gcp-arch/render.go)): Runs `manifest.ValidateStructure` and renders `out/architecture.pdf` (exactly `len(views)` vector pages, one per view, with no separate Inventory Coverage Table sheet), `out/views/<00..NN>-<view_id>.png` (`3x` device scale), and `out/architecture.html` with zero `--facts` flag. Resolves Chrome/Chromium in order: `--chrome-path` (fails fast with exit code `1` for **all** formats including `--format html` alone if missing) -> `CHROME_PATH` -> `config.yaml` `chrome_path` -> `PATH` -> standard OS install paths (falling through when `CHROME_PATH` or `config.yaml` `chrome_path` is unset or non-existent). The Chrome-free unrouted HTML fallback applies **only** when `--format html` is requested alone without an explicit `--chrome-path` and auto-discovery finds no binary—writing self-contained `out/architecture.html` with the embedded JS router and printing a `stderr` notice that CLI collision verification was skipped.

### Stage 1: Structural Multi-Project Fact Extraction ([internal/extract](../../internal/extract))

#### Canonical `v3` `facts.json` Data Model

`facts.json` ([internal/extract/schema.go](../../internal/extract/schema.go)) is plain indented UTF-8 JSON storing deterministic sorted slices with zero secret, script, or `ConfigMap` data payloads:

*   `schema_version`: `"v3"`
*   `read_time`: RFC 3339 UTC timestamp pinned from CAI page 1.
*   `scopes`: Canonical sorted scope strings (`organizations/...`, `folders/...`, `projects/...`).
*   `hierarchy`: `organizations[]`, `folders[]`, and `projects[]` (`project_id`, `project_number`, `display_name`, `parent`, `is_psa_tenant`, `shared_vpc_host_project`).
*   `resources[]` (`FactResource`): `uri` (canonical AIP-122 URI, or synthetic external URI such as `//github.com/<owner>/<repo>`), `asset_type`, `project_id`, `location`, `display_name`, `parent_uri` (owning VPC, subnet, or GKE cluster), `is_external` (`true` for external GitHub/GitLab repos and out-of-scope target project stubs), `attributes` (constructed exclusively from a strict per-`asset_type` field allowlist: CIDR, machine tier, database engine, GKE `namespace`, `replicas`, `ingress_hosts`, `repo_url`, `trigger_filter`, `kms_key_uri`, `service_account`, Vertex AI `PublisherModel` `publisher`/`model_id`/`invocations`/`dedicated_throughput`, and folded child counts such as `image_count: 643`, `table_count: 42`, or `node_pool_count: 2`), and `project_capabilities[]` (sorted role strings for broad ancestor IAM roles `roles/{owner,editor,viewer,browser}` held by a Service Account).
*   `edges[]` (`FactEdge`): Deterministic ID (`sha256(kind + ":" + from_uri + ":" + to_uri + ":" + detail)[:16]`), `from_uri`, `to_uri`, `kind` (`CONFIG_REF`, `K8S_INGRESS_BACKEND`, `K8S_NEG_BACKEND`, `K8S_SERVICE_SELECTOR`, `WORKLOAD_IDENTITY`, `CLOUDBUILD_TRIGGER_REPO`, `CLOUDBUILD_STEP_REF`, `IAM_RESOURCE_BINDING`, `PSA_PEERING`, `VPC_PEERING`, `BQ_LINEAGE`, `MONITORING_TRAFFIC`), and `detail` (port/protocol, IAM role, or sanitized command target).
*   `suppressed_plumbing_counts`: Sorted map of dropped plumbing/default `asset_type -> count` for audit transparency.
*   `out_of_scope_targets[]` (`OutOfScopeHint`): `target_project_id`, `synthetic_uri` (`//cloudresourcemanager.googleapis.com/projects/<target_project_id>`), `referenced_by_uris[]`, `source_kind` (`"cloudbuild_deploy_flag"`, `"config_ref_cross_project"`, or `"iam_cross_project"`).
*   `coverage_ledger[]` (`CoverageEntry`): Per-scope collection state (`OK`, `IAM_DENIED`, `VPC_SC_BLOCKED`, `SERVICE_DISABLED`, `TRUNCATING_CAP`). Salvaged `ClassifyGCPError` ([internal/extract/client.go](../../internal/extract/client.go)) inspects REST `*googleapi.Error` and gRPC `*errdetails.ErrorInfo` to distinguish fatal caller quota/auth failures (`ErrorKindQuotaFatal`, `ErrorKindUnauthenticated`) from target-scope degradations mapped via `ClassifiedError.CoverageState()` to `IAM_DENIED`, `VPC_SC_BLOCKED`, and `SERVICE_DISABLED` (while `TRUNCATING_CAP` is recorded when BigQuery `jobs.list` reaches its `20`-page cap).

#### Targeted Extraction Rules and Salvaged Resolvers

1.  **Compute-Scoped AIP-122 & GKE Cluster URI Canonicalization ([internal/extract/canonical.go](../../internal/extract/canonical.go)):** `CanonicalizeAIP122` normalizes regional API prefixes, maps project numbers to canonical `project_id`s (using discovered `Project` assets plus `--project-alias <NUM=ID>`), and normalizes `container.googleapis.com` `/zones/<z>/clusters/<c>` to `/locations/<z>/clusters/<c>`. For relative `projects/<p>/...` paths without a service host, `inferServiceFromRelative` (`inferScopedProjectService`, `inferZonalService`, `inferLocationsSubpathService`) uses `isComputeScopeCollection(collection)` to infer `compute.googleapis.com`, explicitly excluding `"instances"` inside `inferLocationsSubpathService` (`segs[4] != "instances"`) so ambiguous relative `/locations/<loc>/instances/<name>` paths do not infer `compute.googleapis.com` and collide with Redis, Memcache, or Filestore. Then `normalizeServicePath` rewrites `/locations/global/<collection>/` -> `/global/<collection>/`, `/locations/<region>/<collection>/` -> `/regions/<region>/<collection>/`, and `/locations/<zone>/<collection>/` -> `/zones/<zone>/<collection>/` (matching `gcpZoneRe`) **strictly when the resolved service is `compute.googleapis.com`** (`case svcCompute:`)—never altering non-Compute services even when their collection name appears in `isComputeScopeCollection` (e.g., `//redis.googleapis.com/.../locations/europe-west1/instances/cache-1`), while explicit `//compute.googleapis.com/.../locations/<zone>/instances/<name>` URIs still normalize to `/zones/<zone>/instances/<name>`, preserving `/locations/` on Cloud Build, Cloud Run, and Artifact Registry URIs while guaranteeing `VPC: cicd` resolves to one canonical URI.
2.  **Salvaged Plumbing/Default Suppression, Sub-Resource Folding & Allowlisted Scrubbing ([internal/extract/sanitize.go](../../internal/extract/sanitize.go)):**
    *   *Plumbing & Default Suppression:* Incorporates `IGNORED_PLUMBING` and `ABSORBED_CHILD` rules salvaged from retired v2 `base_catalog.yaml` plus `isDefaultSandboxAsset` and `isDefaultServiceAccount` (suppressing unused `default` VPCs, auto-mode regional `default` subnets, `_Default`/`_Required` `LogBucket`s, and unreferenced default Service Accounts). Suppresses Kubernetes, backup, GKE-node, Gateway, control-plane, and absorbed sub-resource plumbing (`plumbingAssetTypes` and `plumbingAssetPrefixes` in [internal/extract/sanitize.go](../../internal/extract/sanitize.go)) into `suppressed_plumbing_counts` after extracting structural/GKE edges and folding child counts onto parent repositories/datasets/clusters—including `cloudbuild.googleapis.com/Build`, `k8s.io/{Namespace,ServiceAccount,Secret,ConfigMap,Endpoints,Pod,Event,Node,ResourceQuota,PersistentVolume,PersistentVolumeClaim}`, `discovery.k8s.io/EndpointSlice`, `apps.k8s.io/ReplicaSet`, `storage.k8s.io/StorageClass`, prefix groups `admissionregistration.k8s.io/`, `rbac.authorization.k8s.io/`, `gateway.networking.k8s.io/`, `autoscaling.k8s.io/`, and `servicedirectory.googleapis.com/`, `sqladmin.googleapis.com/{BackupRun,Backup}`, `container.googleapis.com/NodePool`, `gkehub.googleapis.com/Membership`, `gke-<cluster>-*` `InstanceGroupManager`s/`InstanceTemplate`s/worker `Instance` VMs, Load Balancer/Compute intermediates (`Route`, `Firewall`, `Policy`, `Project`, `InstanceTemplate`, `Autoscaler`, `HealthCheck`, `Disk`, `Address`, `GlobalAddress`, `Target{Http,Https,Grpc,Tcp,Ssl}Proxy`, `SslCertificate`, `UrlMap`, `BackendService`, `BackendBucket`, `SecurityPolicy`, `NetworkEndpointGroup`, `ExternalVpnGateway`, `VpnTunnel`), absorbed DB/storage/analytics/API/AI child types (`alloydb.googleapis.com/Instance`, `spanner.googleapis.com/Database`, `bigtableadmin.googleapis.com/{Cluster,AppProfile,Table}`, `bigquery.googleapis.com/{Table,Routine,Model,RowAccessPolicy}`, `artifactregistry.googleapis.com/{DockerImage,Package,Version}`, `run.googleapis.com/Revision`, `aiplatform.googleapis.com/{Model,Index,FeatureGroup,FeatureView}`, etc.), and control-plane types (`accesscontextmanager.googleapis.com/{AccessPolicy,ServicePerimeter}`, `servicenetworking.googleapis.com/Connection`, `containeranalysis.googleapis.com/Occurrence`, and `suppressedPlumbingServices`—where `IsPlumbingAssetType` excludes `compute.googleapis.com`, `container.googleapis.com`, and `iam.googleapis.com`, whereas Cloud Monitoring `aggregateConsumedAPICalls` suppresses all hosts in `suppressedPlumbingServices`). Hierarchy and enabled-API assets (`cloudresourcemanager.googleapis.com/{Organization,Folder,Project}` and `serviceusage.googleapis.com/Service`) are also listed in `plumbingAssetTypes`, but during CAI `RESOURCE` ingestion `ingestHierarchyAsset` captures them into `Hierarchy` / `enabledServices` and returns early before `ingestFoldedOrPlumbingAsset`, so they are omitted from `resources[]` without being tallied in `suppressed_plumbing_counts`.
    *   *Allowlisted Attributes & Secret Scrubbing:* `FactResource.Attributes` is populated strictly from an explicit per-`asset_type` field allowlist—never by forwarding denylist-filtered maps. Before discarding raw payloads, `ExtractSanitizedRefs` scans `k8s.io/ConfigMap` `data`, workload `containers[]` and `initContainers[]` (`command`, `args`, `env`), and Cloud Build `steps[]` (`name`, `entrypoint`, `args`, `script`), while all `22` `sensitiveDroppedKeys` (`startup-script`, `startup-script-url`, `shutdown-script`, `user-data`, `env`, `envvariables`, `secretenv`, `environmentvariables`, `buildenvironmentvariables`, `args`, `command`, `script`, `data`, `binarydata`, `stringdata`, `substitutions`, `query`, `rawquery`, `schema`, `webhookconfig`, `kubectl.kubernetes.io/last-applied-configuration`, `last-applied-configuration`) are unconditionally discarded.
3.  **Salvaged Structural `CONFIG_REF`, Private-IP (`SonarQube -> Cloud SQL`), BigQuery, and Dataflow Joins ([internal/extract/cai.go](../../internal/extract/cai.go), [internal/extract/edges.go](../../internal/extract/edges.go), [internal/extract/enrichers.go](../../internal/extract/enrichers.go)):**
    *   Implements structural metadata extraction via `extractRetainedMetadata` and `retainedMetadataExtractors` ([internal/extract/cai.go](../../internal/extract/cai.go))—including generic `kms_key_uri` -> `cmek` `CONFIG_REF` edge emission across all assets carrying a `kms_key_uri` attribute—populating `outboundRefs` and metadata resolved by `resolveLoadBalancerNonGKEBackends`, `resolveStructuralRefs`, and `resolveAbsorbedBridges` ([internal/extract/edges.go](../../internal/extract/edges.go)). Covers: Compute & networking (`compute.googleapis.com/{Network,Subnetwork,Instance,ForwardingRule,GlobalForwardingRule,Router,InterconnectAttachment,ServiceAttachment}`, VPC peering `VPC_PEERING`, Serverless/PSC NEGs, and `BackendBucket`s), GKE & managed data stores (`container.googleapis.com/Cluster`, `sqladmin.googleapis.com/Instance`, `alloydb.googleapis.com/Cluster`, `redis.googleapis.com/{Instance,Cluster}`, `memcache.googleapis.com/Instance`, `file.googleapis.com/Instance`), build/serverless runtimes (`cloudbuild.googleapis.com/BuildTrigger`, `run.googleapis.com/{Service,Job}` with `extractCloudRunTraffic`/`reconcileCloudRunService` + `*.run.app` host matching, `cloudfunctions.googleapis.com/CloudFunction`, `appengine.googleapis.com/Service` with `reconcileAppEngineService`), messaging/orchestration (`pubsub.googleapis.com/Subscription` push & BQ/GCS/DLQ sinks, `eventarc.googleapis.com/Trigger`, `cloudscheduler.googleapis.com/Job`, `cloudtasks.googleapis.com/Queue` via `extractCloudTasksQueueMetadata`: `httpTarget.uriOverride` -> `CONFIG_REF` `cloudtasks.http`, `appEngineRoutingOverride.service` / `appEngineHttpQueue.appEngineRoutingOverride.service` -> `CONFIG_REF` `cloudtasks.appengine`, plus `projectCloudTasksQueueAttrs` projecting `httpTarget.{oidcToken,oauthToken}.serviceAccountEmail` onto `service_account` for IAM/monitoring matching), data/analytics/AI (`storagetransfer.googleapis.com/TransferJob`, `bigquerydatatransfer.googleapis.com/TransferConfig`, `bigqueryconnection.googleapis.com/Connection`, `datastream.googleapis.com/Stream`, `dataproc.googleapis.com/{Cluster,Batch}`, `metastore.googleapis.com/Service`, `composer.googleapis.com/Environment`, `discoveryengine.googleapis.com/{Engine,DataStore}`, `dialogflow.googleapis.com/Agent`, `aiplatform.googleapis.com/ReasoningEngine`), identity/KMS (`iam.googleapis.com/ServiceAccount`, `cloudkms.googleapis.com/CryptoKey`), and absorbed-resource bridges in `resolveAbsorbedBridges` (`compute.googleapis.com/VpnTunnel`, `networkconnectivity.googleapis.com/Spoke`, `aiplatform.googleapis.com/FeatureView`, `analyticshub.googleapis.com/Listing`, `dataplex.googleapis.com/Asset`, `datastream.googleapis.com/{ConnectionProfile,PrivateConnection}` CDC/GCS stream bridges in `resolveDatastreamStreamBridges`, and folded BigQuery external `Table`/`Routine`/`Model` URIs in `resolveBigQueryFoldedExternalURIs` for `bq_remote_fn`, `biglake`, and `bq_external`).
    *   Implements VPC/PSA-scoped private-IPv4 indexing (`indexVPCAndPrivateIPs` and `matchPrivateIP`, salvaged from retired v2 `internal/compiler`) plus bare `<proj>:<region>:<instance>` Cloud SQL connection-name matching in `ExtractSanitizedRefs`, deterministically joining `SonarQube`'s JDBC/ConfigMap/env references to `sqladmin.googleapis.com/Instance` (`CONFIG_REF`).
    *   Implements concurrent `runEnrichers` (`fetchBigQueryJobs`/`applyBigQueryLinks`/`applySingleBigQueryLink` for `BQ_LINEAGE` `sql_elt` dataset-to-dataset and `bq_job` principal edges—where `resolveBQJobPrincipalSource` attributes a job principal to its dedicated workload when exactly `1` workload uses that Service Account and falls back to the unsuppressed `iam.googleapis.com/ServiceAccount` URI (`sa.uri`) when `0` or `> 1` workloads match—and `fetchDataflowJobs`/`applyDataflowLinks` for `CONFIG_REF` edges on active Dataflow jobs via `Jobs.List(proj, "-").Filter("ACTIVE")`, covering both streaming and batch jobs, gated on `dataflow.googleapis.com` enabled, `<= 10 QPS`), retiring `EnrichRagCorpora` and direct `accesscontextmanager` calls.
4.  **GKE Ingress / L4 LB -> NEG -> Service -> Workload -> Workload Identity Join ([internal/extract/gke.go](../../internal/extract/gke.go)):**
    *   Retains `container.googleapis.com/Cluster`, `apps.k8s.io/{Deployment,StatefulSet,DaemonSet}`, `batch.k8s.io/{CronJob,Job}`, `k8s.io/Service`, and `networking.k8s.io/Ingress` outside `kube-system`, `gke-managed-*`, and `gmp-system` namespaces (copying `metadata.namespace` onto `attributes["namespace"]`).
    *   Resolves `ForwardingRule -> TargetHttp(s)Proxy -> UrlMap -> BackendService -> NetworkEndpointGroup -> k8s.io/Service` via `Service` annotation `cloud.google.com/neg-status` (`K8S_NEG_BACKEND`), joins L4 `Service.status.loadBalancer.ingress[].ip` to `ForwardingRule.IPAddress` (`K8S_INGRESS_BACKEND`), and attaches `Ingress` host/path rules to the entry `ForwardingRule` so synthesis sees human hostnames (`atlantis.*`, `sonarqube.*`) instead of `k8s2-fs-...` hashes.
    *   Requires `len(Service.spec.selector) > 0` before matching `Service.spec.selector` against workload pod-template labels (`spec.template.metadata.labels`, or `spec.jobTemplate.spec.template.metadata.labels` for `CronJob`) within `(cluster_uri, namespace)` to emit `K8S_SERVICE_SELECTOR` edges.
    *   Indexes `k8s.io/ServiceAccount` annotation `iam.gke.io/gcp-service-account` in memory before suppressing `k8s.io/ServiceAccount`, joining workload `serviceAccountName` (including `CronJob` `spec.jobTemplate.spec.template.spec.serviceAccountName`) to `//iam.googleapis.com/projects/<proj>/serviceAccounts/<email>` (`WORKLOAD_IDENTITY`).
5.  **In-Stream CAI Cloud Build Trigger & Top-`N` Succeeded Build Folding ([internal/extract/cai.go](../../internal/extract/cai.go), [internal/extract/sanitize.go](../../internal/extract/sanitize.go)):**
    *   Ingests `cloudbuild.googleapis.com/BuildTrigger` (emitting a synthetic external `FactResource` `//github.com/<owner>/<repo>` with `is_external: true` and a `CLOUDBUILD_TRIGGER_REPO` `FactEdge`, plus inline `build.steps[]`, `build.images[]`, and `serviceAccount`).
    *   During CAI `RESOURCE` streaming, maintains an in-memory bounded min-heap by `createTime` retaining at most `--cloudbuild-runs` (default `3`) `cloudbuild.googleapis.com/Build` assets with `status == "SUCCESS"` per `buildTriggerId` (plus up to `5` triggerless succeeded builds per project). `flushCloudBuildHeap` passes their `steps[]` (`name`, `entrypoint`, `args`, `script`), `images[]`, and `artifacts.objects.location` through `ExtractSanitizedRefs` (`<region>-(docker|python|maven|npm|apt|yum|go|generic|kfp).pkg.dev/<proj>/<repo>` via `artifactRegistryRe`, `gcr.io/<proj>/...`, `gs://<bucket>`, `--project=<id>` / `--project <id>`, `--cluster=<name>`, `gcloud run deploy <svc>`) to emit `CLOUDBUILD_STEP_REF` edges on the parent `BuildTrigger` (or on `//cloudresourcemanager.googleapis.com/projects/<proj>` for triggerless builds, where `maybeAddInScopeProjectResource` synthesizes an in-scope `cloudresourcemanager.googleapis.com/Project` `FactResource`—overriding `plumbingAssetTypes` suppression—whenever triggerless builds emit edges) before counting all `Build` assets in `suppressed_plumbing_counts`—requiring zero `cloudbuild/v1` RPCs or extra IAM roles.
6.  **PSA Tenant Classification, Monitoring (`consumed_api` + Vertex AI `PublisherModel`), IAM Edges & External Stubs ([internal/extract/cai.go](../../internal/extract/cai.go), [internal/extract/enrichers.go](../../internal/extract/enrichers.go), [internal/extract/edges.go](../../internal/extract/edges.go)):**
    *   `indexSingleVPCPeering` classifies peer projects matching `^p[a-z0-9]+-tp$` (`IsPSATenantProject`) or any active VPC peering whose name contains `"servicenetworking"` (case-insensitive) as PSA tenants (`is_psa_tenant: true` via `recordPSATenant`). `emitPSAPeeringAndSharedVPC` records Shared VPC hosts (`shared_vpc_host_project`, plus out-of-scope host project stubs and `shared_vpc` `CONFIG_REF` edges when the host VPC is outside `scopes`), emits `vpc -> instance` `PSA_PEERING` edges (`emitPSAPeeringEdge`, annotating `psa_tenant_project` when the VPC has a recorded `servicenetworking` peering) for PSA-managed services (`isPSAManagedService`: `sqladmin.googleapis.com/Instance`, `alloydb.googleapis.com/Cluster`, `redis.googleapis.com/Instance`, `memcache.googleapis.com/Instance`, `file.googleapis.com/Instance`) whose `vpc` attribute resolves to an in-scope VPC, and emits `vpc -> cluster` `CONFIG_REF` (`detail: "psc_network"`) for `redis.googleapis.com/Cluster` (which connects via PSC `pscConfigs[].network`).
    *   In `runEnrichers` (`fetchMonitoring` / `applyMonitoringResult` in [internal/extract/enrichers.go](../../internal/extract/enrichers.go)):
        *   Groups `serviceruntime.googleapis.com/api/request_count` (`resource.type = "consumed_api"`) by **`resource.labels.method`**, applies `IsReadOnlyDiscoveryMethod` unconditionally across all services to filter out case-insensitive discovery/read-only methods (`get*`, `list*`, `aggregatedlist*`, `batchget*`, `testiampermissions`, `watch*`, `poll*`, and `listen`), suppresses `consumed_api` metrics in `aggregateConsumedAPICalls` for `suppressedPlumbingServices` and `transversalControlAPIs` (`modelarmor`, `cloudtrace`, `logging`, `monitoring`, `secretmanager`, `cloudkms`, `dlp`, `artifactregistry`, `cloudbuild`, `clouddeploy`), and joins `ServiceAccount.uniqueId` via `resolveMonitoringSource` (resolving to the dedicated workload when exactly `1` workload uses the Service Account, dropping the edge when `> 1` workloads share it, and falling back to the unsuppressed `iam.googleapis.com/ServiceAccount` URI (`sa.uri`) when `0` workloads match) + salvaged companion API host mappings (`bigquerystorage -> bigquery`, `storage-api -> storage`) to unambiguous single-instance project targets (`MONITORING_TRAFFIC` in `emitConsumedAPIEdges`).
        *   When `aiplatform.googleapis.com` is enabled (or not known disabled) in a project, `fetchProjectPublisherMetrics` queries `aiplatform.googleapis.com/publisher/online_serving/model_invocation_count` (`queryPublisherInvocations`) and, if invocations are present, `aiplatform.googleapis.com/publisher/online_serving/consumed_token_throughput` (`queryPublisherThroughput`, recording `dedicated_throughput` when `metric.labels.request_type == "dedicated"`). `materializePublisherModels` synthesizes `aiplatform.googleapis.com/PublisherModel` `FactResource`s (`//aiplatform.googleapis.com/projects/<proj>/locations/<loc>/publishers/<pub>/models/<model>`), and `emitPublisherModelEdges` uses `resolveMonitoringSource` (dedicated workload when `1` workload matches, dropped when `> 1` workloads share the Service Account, and fallback to the unsuppressed `iam.googleapis.com/ServiceAccount` URI (`sa.uri`) when `0` workloads match) to link `generateContent`/`embedContent` callers to the active `PublisherModel` (`MONITORING_TRAFFIC`) whenever a single publisher model is active in the project and no non-SA GenAI callers are present.
    *   Includes `Organization` and `Folder` in `iamPolicyAssetTypes`. Emits `IAM_RESOURCE_BINDING` `FactEdge`s for resource-scoped IAM bindings and for project-level service-specific roles (such as `roles/cloudsql.client`, `roles/cloudsql.instanceUser`, `roles/redis.editor`) bound to a dedicated Service Account when either an IAM Condition pins the target URI or the project contains a single instance of that target service; stores broad ancestor roles (`roles/{owner,editor,viewer,browser}`) on `FactResource.ProjectCapabilities`.
    *   Any referenced `project_id` outside `scopes` (excluding `*-tp` PSA tenants and public builder mirrors such as `cloud-builders`) emits both an `out_of_scope_targets[]` hint (`stderr`) and a synthetic external `FactResource` (`is_external: true`, `//cloudresourcemanager.googleapis.com/projects/<id>`) so cross-project deployment edges validate cleanly.

### Stage 2: Closed-World Vertex AI Synthesis and Validation ([internal/manifest](../../internal/manifest), [internal/synth](../../internal/synth))

#### Short-ID Interning, Active-Resource Cap, and Two-Pass Gemini 4 Execution

Before calling Vertex AI, [internal/synth/pipeline.go](../../internal/synth/pipeline.go) verifies `len(facts.Resources) <= 500` (failing fast with `*synth.ScaleLimitError`, mapped to exit code `2` and instructing the caller to narrow `--projects`/`--folder` if exceeded after plumbing/default suppression) and builds an in-memory deterministic `InternTable` ([internal/synth/intern.go](../../internal/synth/intern.go)):

*   Sorted `facts.Resources[i]` <-> `r_0001 .. r_NNNN`
*   Sorted `facts.Edges[j]` <-> `e_0001 .. e_MMMM`

All calls use `google.golang.org/genai` ([internal/synth/client.go](../../internal/synth/client.go)) configured with `BackendVertexAI`, `Project: --vertex-project`, `Location: --vertex-location` (default `"global"`), `Model: --model` (default `"gemini-4-carbon"`), optional `ThinkingLevel` (`--thinking-level minimal|low|medium|high|xhigh`, validated via `ParseThinkingLevel` and sent through `genai.ThinkingConfig` only when non-empty), `Temperature: 0.1`, `ResponseMIMEType: "application/json"`, and non-recursive `genai.Schema` definitions ([internal/synth/schemas.go](../../internal/synth/schemas.go)):

1.  **Pass 1 (`runPass1` inside `Synthesize`):** Input contains `hierarchy`, interned `r_XXXX` resource descriptors, valid `icon_key` slugs in the system prompt, and a **deduplicated short-ID adjacency list** (`r_XXXX -> [{to: r_YYYY, kind}]`) so Pass 1 sees exact topological connectivity before locking components and views. Output conforms to `Pass1PlanSchema`:
    *   `components[]`: Canonical `id` slug (`atlantis`, `sonarqube_db`, `cicd_triggers`), `title`, `subtitle`, `icon_key` (`genai.TypeString`, validated in Go via `icons.Has`), `component_kind` (`EXTERNAL_CALLER | INGRESS | WORKLOAD | PIPELINE | MESSAGING | DATASTORE | COHORT | EXTERNAL_TARGET`), `project_id`, `backed_by_ids` (`["r_0012", ...]` holding primary URIs plus folded 1:1 `ServiceAccount`, `Secret`, `CryptoKey`, `Ingress`, `Service`, and `ForwardingRule` URIs), and `inline_meta` (`[{label, value}]`, up to 4 rows).
    *   `container_defs[]`: Global container definitions (`id`, `title`, `subtitle`, `kind`: `PROJECT | VPC | GKE_CLUSTER | PSA_ZONE | EXTERNAL_ZONE | SHARED_HUB`, `parent_container_id`, `backed_by_ids`: `["r_0003"]` accounting for `VPC` and `GKE_CLUSTER` `FactResource`s).
    *   `excluded_resources[]`: `[{resource_id: "r_XXXX", reason: "..."}]` for intentional non-diagrammed active inventory.
    *   `detail_views[]`: `1..M` focused logical views (`6–12` target candidate component IDs per view), each assigned `id`, `title`, `subtitle`, `archetype` (`landscape_matrix | stage_pipeline | hub_spoke`), and `accent_mode` (`RUNTIME | CICD | MIXED`).
    *   *Pre-Pass-2 Gate:* Go verifies that `components[].backed_by_ids ∪ container_defs[].backed_by_ids ∪ excluded_resources` covers 100% of `r_0001..r_NNNN`, that stateful datastore URIs inside each `Component` share one `asset_type`, that `icons.Has(icon_key)` holds, and that every component belongs to `>= 1` detail view (retrying Pass 1 up to `2` times with exact offending IDs on failure).
2.  **Pass 2 (`(*pass2Runner).populateViews` inside `Synthesize`) & Deterministic `View 0` Projection:** Runs concurrently (`errgroup.SetLimit(concurrency)`) across `detail_views[]` with component IDs/titles locked. Each call receives only the `r_XXXX` details and `e_XXXX` `FactEdge` records incident to that view's candidate components (plus 1-hop cross-view neighbors for `off_page_refs`). Output conforms to `Pass2ViewSchema`:
    *   `container_ids[]`: Referenced `container_defs` active in this view (nesting depth `<= 2`).
    *   `placements[]`: Ordered slice of `{component_id, container_id, column, off_page_refs[]}` (`column` in `0..4` for `landscape_matrix`/`stage_pipeline` or `0..2` for `hub_spoke`; YAML slice order serves as the deterministic tie-breaker for Stage-3 barycenter row sorting).
    *   `edges[]`: `{from, to, verb (<= 16 chars), flow_kind (RUNTIME | CICD | ADMIN), provenance (VERIFIED | INFERRED), evidence_edge_ids (["e_0041", ...]), inference_rationale, via_bus_id}`.
    *   **Deterministic `View 0` (`view_0_system_index`, [internal/synth/view0.go](../../internal/synth/view0.go)):** If `len(detail_views) == 1`, `View 0` is omitted so single-view scopes avoid a duplicate summary page. Whenever `len(detail_views) >= 2`, Go constructs `views[0]` (`id: "view_0_system_index"`, `archetype: "landscape_matrix"`, `<= 16` view/external summary blocks) deterministically from the populated detail views and cross-view edges with zero extra LLM calls.

#### Canonical `architecture.yaml` Contract Example

Every resource reference in `architecture.yaml` stores `{uri, asset_type, project_id}` (`BackedResource` in [internal/manifest/types.go](../../internal/manifest/types.go)) so `gcp-arch render` and `gcp-arch validate` operate standalone without `facts.json`:

```yaml
schema_version: v1
metadata:
  generated_at: "2026-11-23T02:00:00Z"
  scopes: ["projects/acme-cicd"]
components:
  - id: github_acme
    title: GitHub (github-acme)
    subtitle: Source Repositories
    icon_key: github
    component_kind: EXTERNAL_CALLER
    project_id: external
    backed_by:
      - uri: //github.com/acme-corp/platform-services
        asset_type: external.github.com/Repository
        project_id: external
  - id: atlantis
    title: Atlantis PR Automation
    subtitle: gke:cicd / ns:atlantis
    icon_key: k8s_workload
    component_kind: WORKLOAD
    project_id: acme-cicd
    notes: Curated title for client handoff
    backed_by:
      - uri: //container.googleapis.com/projects/acme-cicd/locations/europe-west1/clusters/cicd/k8s/namespaces/atlantis/apps/deployments/atlantis
        asset_type: apps.k8s.io/Deployment
        project_id: acme-cicd
      - uri: //iam.googleapis.com/projects/acme-cicd/serviceAccounts/atlantis@acme-cicd.iam.gserviceaccount.com
        asset_type: iam.googleapis.com/ServiceAccount
        project_id: acme-cicd
    inline_meta:
      - label: Service Account
        value: atlantis@acme-cicd.iam
containers:
  - id: gke_cicd
    title: "GKE Cluster: cicd"
    subtitle: europe-west1
    kind: GKE_CLUSTER
    parent_container_id: vpc_cicd
    backed_by:
      - uri: //container.googleapis.com/projects/acme-cicd/locations/europe-west1/clusters/cicd
        asset_type: container.googleapis.com/Cluster
        project_id: acme-cicd
excluded_resources: []
rejected_edges:
  - from: atlantis
    to: sonarqube_db
    reason: Unrelated monitoring correlation suppressed by reviewer
views:
  - id: gke_platform
    title: GKE Developer Platform & Ingress
    subtitle: HTTPS Ingress to Atlantis and SonarQube
    archetype: landscape_matrix
    accent_mode: RUNTIME
    container_ids: [vpc_cicd, gke_cicd]
    placements:
      - component_id: atlantis
        container_id: gke_cicd
        column: 2
    edges:
      - from: atlantis
        to: tfstate_bucket
        verb: terraform state
        flow_kind: CICD
        provenance: VERIFIED
        evidence_edge_ids: ["a1b2c3d4e5f60718"]
```

#### Deterministic Validation Gates ([internal/manifest/validate.go](../../internal/manifest/validate.go))

`ValidateStructure(m, icons)` (always run by `validate` and `render`) and `ValidateWithFacts(m, facts, icons)` (run when `--facts` is supplied and inside `synthesize`) enforce four invariants:

1.  **Directed-Walk `FactEdge` Provenance Gate (`--facts`):** On detail views, an edge with `provenance: VERIFIED` is valid if and only if its non-empty `evidence_edge_ids` resolve in `facts.Edges` to a **connected directed walk** starting at a URI in `from.backed_by` and ending at a URI in `to.backed_by` (allowing intermediate hops across folded or `excluded_resources` URIs). Broad ancestor IAM roles are absent from `facts.Edges` by construction. Edges without a directed walk must set `provenance: INFERRED` with a non-empty `inference_rationale` (rendered dashed with a `[n]` footnote).
2.  **100% Resource Accounting (`--facts`):** With `--facts`, `manifest.ValidateWithFacts` enforces that every `FactResource.URI` in `facts.json` appears in `components[].backed_by`, `containers[].backed_by`, or `excluded_resources[]` (with a non-empty `reason`). Accounting is verified purely at validation/synthesis time; Stage 3 (`render`) renders only the architectural views (`len(views)` PDF pages) and does not emit a separate Inventory Coverage Table in HTML or PDF.
3.  **Stateful-Only Homogeneous Datastore & Cohort Gate (Structural):** Within any `Component.backed_by` slice, all URIs belonging to stateful datastore asset types (Cloud SQL, AlloyDB, Spanner, Bigtable, Firestore, Memorystore, Cloud Storage, BigQuery Dataset, Artifact Registry, Filestore, Pub/Sub Topic) must share a single uniform `asset_type`—preventing heterogeneous datastore catch-alls while allowing folded 1:1 `ServiceAccount`, `Secret`, `CryptoKey`, `Ingress`, `Service`, and `ForwardingRule` URIs on the same component.
4.  **Archetype, Density, Single-Source Bus & Tombstone Gate (Structural):** When `>= 2` detail views exist, `views[0].id` must be `view_0_system_index` (`1..16` `Placements`); every detail view must contain `1..12` `Placements` (counting `len(view.Placements)`, excluding `container_ids`), enforce container nesting depth `<= 2`, obey column bounds (`0..4` for `landscape_matrix`/`stage_pipeline`, `0..2` for `hub_spoke`) and `len(verb) <= 16`, require all edges sharing a `via_bus_id` within a view to share the same `from` (or same `to`) component, validate `icons.Has(icon_key)`, and forbid any global `(from, to)` component-ID pair listed in `rejected_edges`.

#### Day-2 Delta Reconciliation (`synthesize --reconcile`)

When `--reconcile <architecture.yaml>` is passed ([cmd/gcp-arch/synthesize.go](../../cmd/gcp-arch/synthesize.go), [internal/synth/pipeline.go](../../internal/synth/pipeline.go)):

1.  Go decodes `<architecture.yaml>` via strict `yaml.v3` (`KnownFields(true)`), diffs `facts.Resources` against `components[].backed_by ∪ containers[].backed_by ∪ excluded_resources`, and projects `facts.Edges` onto existing `(from, to)` component pairs (ignoring pairs in `rejected_edges`).
2.  **Zero-LLM Fast Path:** If zero URIs were added/removed and zero inter-component `FactEdge` projections were added/invalidated (updating only refreshed deterministic `FactEdge` IDs if an attribute detail changed on the same edge), `synth.Reconcile` runs `ValidateWithFacts` and returns in `< 100 ms` with `0` Vertex AI calls (requiring no `--vertex-project`); `cmd/gcp-arch/synthesize.go` compares canonical marshaled bytes (`manifestsEqualInPlace`) and skips disk writes when unchanged.
3.  **Pruning, Prune-Only Zero-LLM Path, and Resource & Edge Delta Patch:** `pruneManifest` removes vanished URIs from `backed_by` and `excluded_resources` slices (dropping any component whose `backed_by` becomes empty), prunes `VERIFIED` edges whose underlying `FactEdge` walk vanished, and via `pruneDetailViews` / `pruneRemovedOffPageRefs` deletes any detail view whose placements all vanish along with any `off_page_refs` targeting removed views. If deletion-only drift leaves zero unclaimed resources and zero new inter-component edges (`len(unclaimedRIDs) == 0 && len(deltaEdgeIDs) == 0`), `synth.Reconcile` deterministically rebuilds `View 0`, validates via `ValidateWithFacts`, and finishes in `0` Vertex AI calls without requiring `--vertex-project`. Otherwise, when unclaimed URIs or newly wired inter-component `FactEdge`s exist, `reconcileRunner.run` invokes Gemini 4 with `ReconcilePatchSchema` ([internal/synth/schemas.go](../../internal/synth/schemas.go)) across up to `2` validation-feedback retries (`maxValidationRetries = 2`, up to `3` total model calls) with frozen component/container headers, remaining view capacities (`12 - len(placements)`), `rejected_edges`, and the delta `r_XXXX` / `e_XXXX` items. It merges returned `append_backed_by`, `append_container_backed_by`, `new_components`, `new_containers`, `new_excluded_resources`, `new_placements`, `new_edges`, and overflow `new_views` (`1..12` blocks), recomputes `View 0` in Go if `>= 2` detail views exist, validates via `ValidateWithFacts`, and returns `(*manifest.Manifest, error)` without writing to disk. [cmd/gcp-arch/synthesize.go](../../cmd/gcp-arch/synthesize.go) then checks canonical YAML equality and writes updated output (`notes` preserved) via `manifest.Write` -> [ioutil.WriteFileAtomic](../../internal/ioutil/atomic.go).

### Stage 3: Editorial CSS Grid Archetypes, Corridor Router, and Chrome CDP Export ([internal/icons](../../internal/icons), [internal/render](../../internal/render))

#### Offline Typography, `<symbol>` Icons, Escaping, and Visual Hierarchy

*   **Zero-Network Asset Bundle (`embed.FS`):** [internal/render/html.go](../../internal/render/html.go) embeds `Inter` (`400`, `500`, `600`, `700`) and `JetBrains Mono` (`400`, `500`) `.woff2` files as base64 `@font-face` rules alongside deduplicated `<svg style="display:none"><defs>...</defs><symbol id="gcp-icon-...">...</symbol></svg>` sprites compiled via `compileIconSymbol` in [internal/icons/registry.go](../../internal/icons/registry.go). All manifest strings are escaped via `html/template` and HTML-safe `encoding/json` serialization (`<`, `>`, `&` escaped), and `Content-Security-Policy` (`default-src 'none'; style-src 'unsafe-inline'; font-src data:; img-src data:; script-src 'unsafe-inline'`) blocks any external network fetch.
*   **Card & Edge Visual Encoding ([internal/render/assets/diagram.css](../../internal/render/assets/diagram.css)):**
    *   *Cards (`.arch-card`):* White `#FFFFFF` background, `1px solid #CBD5E1` border (`border-radius: 6px`), `min-height: 54px` (dynamically expanded via inline `min-height: max(54, 24 + (maxPorts - 1) * 24)px` in `buildPreparedCards` whenever a card has `>= 3` ports on either side), `24x24` inline `<use href="#gcp-icon-...">` SVG icon, `13px/600` `Inter` title (`#0F172A`), `10.5px` `JetBrains Mono` technical sublabel (`#475569`), optional compact inline metadata rows (`border-top: 1px solid #F1F5F9`, up to 4 `key: value` rows), and optional cross-view pill badges (`-> View 2: CI/CD`). Five-column sheets (`.view-sheet[data-cols="5"]`) apply compact overrides (`6px 7px` card padding, `20x20` icon, `11.5px` title, `9.5px` subtitle, `8.5px` metadata rows) to preserve readability inside the `1040px` canvas width.
    *   *Flat Background Container Frames (`.arch-container`, max depth `2`):* Placed directly on the flat `.view-sheet` CSS Grid spanning `(minCol..maxCol, minRow..maxRow)` behind member `.arch-card` items (`pointer-events: none`) with a compact top-left corner badge (`.arch-container-badge`) rendering the container `Title` only (no subtitle) anchored strictly inside `minCol` so interleaved vertical `.corridor-col` tracks remain unobstructed. Depth-1 frames (`PROJECT`, `EXTERNAL_ZONE`, `PSA_ZONE`, `SHARED_HUB`) use `#F8FAFC` fill, `1px solid #94A3B8` (`dashed` for `PSA_ZONE` and `EXTERNAL_ZONE`), and outward negative margins (`margin: -26px -9px -8px -9px`, or `margin-top: -46px` when hosting a depth-2 inner container on the top row); depth-2 frames (`VPC`, `GKE_CLUSTER`) use `#F1F5F9` fill, `1px solid #CBD5E1`, and outward negative margins (`margin: -22px -4px -4px -4px`).
    *   *Connectors:* `RUNTIME` flows render in `#1D4ED8` (slate blue), `CICD` flows in `#B45309` (amber ochre), and `ADMIN`/IAM flows in `#475569` (neutral slate). `VERIFIED` edges use solid `1.5px` strokes; `INFERRED` edges use `5 3` dashed strokes with an inline `[n]` tag matching a numbered rationale footer printed beneath that view's canvas.

#### Fixed-Width Flat CSS Grid Archetypes, Waypoints, and Bus Spines

Every `.view-sheet` is locked to a fixed CSS width of `1040px` (matching the printable `A4 landscape` width inside `10mm` page margins, paired with `@media print { .view-sheet { zoom: var(--print-scale, 1); } }` scaling whenever rendered sheet height exceeds `708px` so each view fits cleanly on a single PDF page) so `.html`, `3x .png`, and `.pdf` share identical pixel coordinates. Content columns interleave with dedicated vertical routing corridor tracks (`var(--corridor-col-i, var(--corridor-col-default))`, defaulting to `56px`, or `64px` on `stage_pipeline`) and content rows interleave with horizontal `.corridor-row` tracks (`var(--corridor-row-j, <default>)`) whose defaults are computed by `defaultRowCorridorTrack` ([internal/render/prepare.go](../../internal/render/prepare.go)): `18px` (plain row gap), `32px` (container badge row), `52px` (nested depth-2 inner-top row), `var(--corridor-row-default)` (`48px`, waypoint row only), and `calc(var(--corridor-row-default) + 46px)` (`94px`, waypoint + nested depth-2 inner-top row):

```text
   Col 0 (Callers)   Corridor 0    Col 1 (Ingress)   Corridor 1    Col 2 (Compute/GKE)   Corridor 2   Col 3 (Datastores)
 +-----------------+            +-----------------+            +-----------------------+            +-------------------+
 | [GitHub Repos]  |            | [HTTPS LB]      |            | [GKE: cicd badge]     |            | [Artifact Reg]    |
 +-----------------+   56px..   +-----------------+   56px..   |  [Atlantis]           |   56px..   | [GCS: tfstate]    |
                       160px                          160px    |  [SonarQube]          |   160px    | [SQL: sonarqube]  |
   (.corridor-row gutter: 18px/32px/52px/48px/94px..160px)     +-----------------------+            +-------------------+
```

1.  `landscape_matrix`: Up to 5 semantic columns (`Col 0..4`: `External Callers -> Ingress/Edge -> Compute/Orchestration -> Messaging/Cache -> Backing Datastores`), used for application/ingress architectures and `View 0`.
2.  `stage_pipeline`: `3–5` sequential stage columns (`Col 0..4`) with top stage banners and `64px` initial inter-stage corridors, used for CI/CD build/deploy pipelines.
3.  `hub_spoke`: A 3-column symmetric grid (`Col 0..2`: `Left Spokes | Corridor 0 | Central Hub | Corridor 1 | Right Spokes / PSA Zones`), used for Shared VPC and PSA peering topologies.

Before emitting HTML, Go performs three deterministic preparation steps in [internal/render/prepare.go](../../internal/render/prepare.go):

*   **Barycenter Row Ordering:** Within each `(column, container_id)`, cards are sorted by the median row index of connected neighbors, breaking ties deterministically using the `placements[]` YAML slice order.
*   **Lateral Same-Column & Sugiyama Multi-Column Waypoints (`buildRoutingEdges`):** Any edge with `|col(to) - col(from)| > 1` is assigned a clear horizontal corridor index (`RoutingEdge.WaypointRow`, serialized as `edge.waypoint_row` in the embedded JSON routing payload) via `pickClearWaypointRow` (avoiding intermediate container badges) so `router.js` routes across `geom.hCorridors[edge.waypoint_row]` via `.corridor-row` tracks, while any edge with `col(from) == col(to)` routes laterally into the adjacent `.corridor-col` track to bypass intermediate cards in the same column.
*   **Directionally Partitioned Fan-Out / Fan-In Bus Spines (`assignBusGroups`):** Both explicit `via_bus_id` groups and automatic `>= 2`-edge groups sharing `(from, flow_kind)` or `(to, flow_kind)` are partitioned by horizontal direction (`cmp.Compare(to.col, from.col)`, skipping same-column edges where direction is `0`), merging each directional group onto a shared vertical corridor trunk spine with perpendicular taps and per-branch `<= 16`-char verb labels.

#### In-Page Orthogonal Router and Zero-Collision Verifier ([internal/render/assets/router.js](../../internal/render/assets/router.js))

The deterministic router script runs identically in headless Chrome and (unless `data-gcp-arch-routed="1"` is already set) in standalone browser-opened `--format html` files:

1.  Await `document.fonts.ready` followed by two nested `requestAnimationFrame` callbacks on the fixed `1040px` `.view-sheet` canvas.
2.  Measure `getBoundingClientRect()` relative to each `.view-sheet` in `measureSheetGeometry` for all `.arch-card`, `.arch-container-badge`, `.corridor-col`, and `.corridor-row` elements (`geom.vCorridors` and `geom.hCorridors`, indexed directly by `edge.waypoint_row` for multi-column routes).
3.  Distribute source/target ports along card borders (`>= 10px` from corners, sorted by target coordinate) and run 3 passes of opposing-port deconfliction (`deconflictCorridorPorts`, enforcing `8px` minimum separation across every shared vertical corridor in `assignPortCoords`), then assign each orthogonal corridor segment to a discrete `8px`-spaced lane inside its `.corridor-col` or `.corridor-row` track (`12px` outer padding from card walls), placing `<= 16`-char verb pills in clear spans.
4.  **Collision & Crossing Check:** Verify zero overlaps between every routed segment/verb-pill and (a) non-incident `.arch-card` rects (`4px` clearance), (b) `.arch-container-badge` rects (`4px` clearance), (c) other verb-pill rects (`4px` clearance), and (d) parallel collinear segments (`>= 6px` separation). Junctions between segments belonging to the *same* bus spine are joined with a `2.5px` dot, while irreducible perpendicular crossings between unrelated corridor segments (such as non-planar $K_{2,2}$ or $K_{3,3}$ bipartite links) render a `4px` background halo bridge on the horizontal segment without aborting export.
5.  **Bounded Corridor Widening (`applyCorridorWidening`, Max 2 Reflow Passes):** Widens vertical corridors (`--corridor-col-i`) to `min(160px, max(24 + 8 * lanes, pillReserve))` and horizontal corridors (`--corridor-row-j`) to `min(160px, 24 + 8 * lanes + badgeReserve + 12)`. If collisions persist without incremental track growth, any active corridor below cap widens directly to `160px` (`MAX_CORRIDOR_SIZE`). Whenever any track widens, `sheet.getBoundingClientRect()` forces synchronous CSS Grid reflow and re-runs steps 2–4 (at most 2 passes).
6.  **Strict Zero-Collision Gate & Print Scaling:** If any card collision, container-badge collision, verb-pill overlap, or parallel collinear overlap remains after pass 2, set `window.__GCP_ARCH_RENDER_RESULT__ = {ok: false, collisions: [...]}` (causing CLI `gcp-arch render` to print colliding IDs to `stderr` and exit `2`). When `ok == true`, inject `<svg class="connector-layer">`, measure `sheet.getBoundingClientRect().height` and set `--print-scale` to `(708 / h).toFixed(4)` if `h > 708`, mark `document.documentElement.dataset.gcpArchRouted = "1"`, and expose `{ok: true, collisions: [], viewRects: [...], html: '<!DOCTYPE html>\n' + document.documentElement.outerHTML}`.

#### Single-Session `chromedp` Exporter ([internal/render/cdp.go](../../internal/render/cdp.go))

When `pdf` or `png` output is requested (or when `html` is rendered on a machine with Chrome/Chromium available):

1.  Go locates Chrome/Chromium ([cmd/gcp-arch/render.go](../../cmd/gcp-arch/render.go) + `FindChrome(explicitPath string, fallbackPaths ...string)` in [internal/render/cdp.go](../../internal/render/cdp.go)) in order: explicit `--chrome-path` (failing fast with exit code `1` for **all** formats including `--format html` alone if missing) -> `CHROME_PATH` -> `config.yaml` `chrome_path` (passed via `RenderOptions.ConfigChromePath` / `appDeps.newBrowserRunner(chromePath, configChromePath)`) -> `PATH` (`google-chrome`, `google-chrome-stable`, `chromium`, `chromium-browser`) -> standard OS install paths (macOS `/Applications/*`, Linux `/usr/bin/*`), falling through whenever `CHROME_PATH` or `fallbackPaths` is unset or points to a non-existent file. The Chrome-free unrouted HTML fallback applies only when `--format html` is requested alone without an explicit `--chrome-path` and auto-discovery finds no binary. It creates an isolated ephemeral `--user-data-dir` temp directory (cleaned up via `defer os.RemoveAll`, so an existing desktop Chrome session never hijacks invocation), and starts a single `chromedp.NewExecAllocator` + `chromedp.NewContext` session (`--headless=new`, `--disable-gpu`, `--no-first-run`, `--no-default-browser-check`, `--disable-background-networking`, `--disable-sync`, `--disable-extensions`, `--metrics-recording-only`, `--mute-audio`, with `needsNoSandbox()` adding `--no-sandbox` inside a Linux container when `/proc/sys/kernel/unprivileged_userns_clone` is `"0"` or when running as root (`os.Geteuid() == 0`)).
2.  Go sets fixed viewport metrics (`emulation.SetDeviceMetricsOverride(1200, 900, 3.0, false)`) **before** navigating to a `0600` temp `.html` file, then evaluates `window.__GCP_ARCH_RENDER_PROMISE__` (`EvalAwaitPromise`). If `ok == false`, Chrome is closed and `render` exits `2`.
3.  **HTML Export (`out/architecture.html`):** Writes the serialized post-routed `result.HTML` (`data-gcp-arch-routed="1"`) via [ioutil.WriteFileAtomic](../../internal/ioutil/atomic.go) (`0644`).
4.  **PNG Export (`out/views/<00..NN>-<view_id>.png`):** Loops over `result.ViewRects` calling `page.CaptureScreenshot` with `Format: page.CaptureScreenshotFormatPng`, `CaptureBeyondViewport: true`, and `Clip: &page.Viewport{X, Y, Width, Height, Scale: 1}` (capturing crisp `3x` PNGs via the active `deviceScaleFactor: 3.0` even for views below the fold).
5.  **PDF Export (`out/architecture.pdf`):** Calls `page.PrintToPDF` (`PrintBackground: true`, `PreferCSSPageSize: true`), honoring `@page { size: A4 landscape; margin: 10mm; }`, `.view-sheet { break-after: page; }`, and `@media print { .view-sheet { zoom: var(--print-scale, 1); } }` over the fixed `1040px` layout to emit exactly `len(views)` vector pages (one page per view, no separate Inventory Coverage Table sheet).

## Risks, Limits & Mitigations

*   **LLM Hallucination or Resource Omission in Stage 2:** An unconstrained language model can invent unconfigured arrows or drop unconnected datastores. *Mitigation:* Intern all resources and edges to short handles (`r_XXXX`, `e_XXXX`) and enforce `manifest.ValidateWithFacts` ([internal/manifest/validate.go](../../internal/manifest/validate.go)): an edge renders as `VERIFIED` (solid) only if `evidence_edge_ids` form a connected directed walk in `facts.Edges` from `from.backed_by` to `to.backed_by`, while unbacked links must be `INFERRED` (dashed + rationale footnote). Stateful datastore catch-alls are rejected, and 100% of active `FactResource`s must appear in `components[].backed_by`, `containers[].backed_by`, or `excluded_resources`.
*   **Multi-Hundred-Project `--org` / `--folder` Token Overflow:** Emitting thousands of excluded boilerplate resources in one Pass-1 JSON response would exceed Vertex AI's `65,536` output token limit, and large `ResponseSchema` enums can hit constrained-decoding state limits. *Mitigation:* Deterministically fold unused `default` VPCs/subnets, `_Default`/`_Required` log buckets, and unreferenced default Service Accounts into `suppressed_plumbing_counts` in Go during Stage 1, type `icon_key` as plain `STRING` validated via `icons.Has`, and enforce a hard `<= 500` active `FactResource` ceiling on `gcp-arch synthesize` (exiting `2` with guidance to scope `--projects`/`--folder` to an application domain).
*   **Day-2 Drift vs. Human Curation in Git:** Regenerating `architecture.yaml` from scratch clobbers curated titles, `notes`, and archetypes, while ignoring edge-only changes leaves diagrams stale. *Mitigation:* `synthesize --reconcile` diffs both resource URIs and projected inter-component `FactEdge` pairs in Go first (`0` Vertex AI calls when unchanged) and applies delta-only patches onto canonical `yaml.v3` structs while freezing existing titles, `notes`, archetypes, and `rejected_edges`.
*   **Corridor Congestion & Non-Planar Bipartite Crossings in Stage 3:** Fixed corridors can overflow on parallel fan-out, and $K_{2,2}$/$K_{3,3}$ bipartite links cannot be routed planarly inside a single corridor. *Mitigation:* Render `.arch-container` boxes as flat background frames with corner badges outside `.corridor-col` tracks, merge `>= 2` shared flows onto bus spines, widen congested corridors across up to 2 synchronous reflow passes (`56px..160px`), render irreducible perpendicular corridor crossings with `4px` halo bridges, and abort (`exit 2`) strictly on card, badge, verb-pill, or collinear overlaps.
*   **Credential Exposure & Local Browser Boundary:** Kubernetes annotations/scripts can hold raw credentials, and Vertex AI rejects `cloud-platform.read-only` tokens. *Mitigation:* Construct `FactResource.Attributes` solely from per-`asset_type` field allowlists (dropping all `22` `sensitiveDroppedKeys` including `last-applied-configuration`, `command`, `script`, and `ConfigMap` `data`), isolate `cloud-platform.read-only` credentials on target GCP scopes from `cloud-platform` Vertex AI credentials on `--vertex-project`, escape all HTML/JSON payloads in `render`, and drive Chrome offline (`Content-Security-Policy: default-src 'none'`).

## Alternatives Considered

### Alternative A: Retain Retired Static Go Rule Compiler (`internal/compiler`) and `elk-go` 40-Box Band Layout

*   **Pros:** Zero LLM API calls, zero local Chrome dependency for vector output, and reused existing compaction passes and `elk-go` layout code.
*   **Cons:** Cannot infer workload roles or application boundaries from service types alone; collapsed `acme-cicd` Cloud Build pipelines into footer counters, folded GKE clusters into opaque counts, and produced uneven container whitespace.
*   **Reason for Rejection:** Failed semantically and visually on `acme-cicd` and could not produce Google-Docs-compatible `3x .png` or paginated vector `.pdf` handoff bundles from the CLI.

### Alternative B: Parse Local Terraform (`.tf`) Files Alongside Live GCP APIs

*   **Pros:** Surfaces infrastructure-as-code module groupings and resource references authored directly in HCL files.
*   **Cons:** Requires HCL expression evaluation, authenticated fetching of private remote Terraform modules (`gs://acme-private-modules`), multi-repo workspace correlation, and drift reconciliation between Git branches and live deployed resources.
*   **Reason for Rejection:** Fixing CAI GKE (`Ingress`/`Service`/`neg-status`), private-IP Cloud SQL matching, and in-stream CAI Cloud Build step extraction recovers live topology without coupling `gcp-arch` to local repository checkouts or HCL evaluation.

### Alternative C: Use Draw.io (`.drawio` / `.drawio.svg`) as the Editable Repository Artifact

*   **Pros:** Lets users drag boxes manually in a desktop or web GUI after generation.
*   **Cons:** Lacks multi-column CSS Grid typography and inline card metadata tables, and introduces XML coordinate-merge conflicts across Day-2 updates.
*   **Reason for Rejection:** Target users prioritize clean page-fitted output and code-reviewable Git YAML diffs over manual GUI box-dragging.

### Alternative D: Render One Monolithic Global Diagram per Multi-Project System

*   **Pros:** Places every resource across all scanned projects on a single canvas with zero `off_page_refs` badges.
*   **Cons:** Shrinks typography below readable point sizes on A4 PDF pages and Google Docs widths once a system exceeds ~12–15 blocks.
*   **Reason for Rejection:** Unreadable in written PDF and Google Docs deliverables beyond a single small project; decomposing into `1..12` block logical views (plus `View 0` when multi-view) preserves legibility.

### Alternative E: Direct Single-Pass LLM Raw HTML + Inline `<svg>` Coordinate Generation

*   **Pros:** Requires zero deterministic layout preparation or in-page JavaScript routing; a single prompt asks the LLM to emit raw HTML and `<svg>` paths directly from inventory.
*   **Cons:** Raw CAI dumps overflow model context, unconstrained prompts omit low-degree datastores or invent unconfigured edges, Day-2 reruns overwrite human edits, and unmeasured LLM `<svg><path>` pixel coordinates slice through card text past ~8–9 blocks.
*   **Reason for Rejection:** Violates 100% resource accounting, deterministic `FactEdge` provenance, Day-2 curation stability, and zero-card-collision layout guarantees.

### Alternative F: Pure-Go `elk-go` SVG Renderer Over Synthesized `architecture.yaml`

*   **Pros:** Keeps CLI export free of any local Chrome/Chromium binary while still using Stage-2 Gemini 4 view decomposition.
*   **Cons:** `elk-go` (`INCLUDE_CHILDREN`) produces uneven compound-container whitespace, lacks CSS Grid alignment and inline flexbox metadata rows, relies on approximate character-width tables, and emits `.svg` files rejected by Google Docs.
*   **Reason for Rejection:** Sacrifices exact `.woff2` font measurement, native vector `.pdf` pagination, and headless `3x .png` export for Google Docs.

### Alternative G: Full Clean-Slate Rewrite in Python (`google-genai` + `Playwright`)

*   **Pros:** Concise scripting syntax for JSON/YAML transformation and Vertex AI SDK calls.
*   **Cons:** Calls the exact same Vertex AI Gemini 4 model and renders the exact same HTML/CSS/JS templates while discarding verified Go CAI `ReadTime` pagination, `<= 80` RPM rate limiting, `ClassifyGCPError` handling, zero-slot BigQuery job lineage, private-IP indexing, and credential scrubbing, and requires distributing a Python runtime and `.venv` instead of a single `CGO_ENABLED=0` binary.
*   **Reason for Rejection:** Delivers zero semantic or visual gain while discarding race-tested Go ingestion code and single-binary distribution.

### Alternative H: Direct LLM YAML Text Generation or Raw `yaml.Node` AST Surgery

*   **Pros:** Direct LLM YAML output avoids `genai.Schema` definitions; `*yaml.Node` AST splicing preserves `#` YAML comments across `--reconcile`.
*   **Cons:** Unquoted colons and indentation errors in LLM YAML waste retries, while custom `*yaml.Node` sequence splicing adds brittle AST mutation code for a codebase with zero legacy users.
*   **Reason for Rejection:** Vertex AI `ResponseSchema` JSON decoding paired with canonical `yaml.v3` struct serialization (and explicit `notes` fields on manifest structs) eliminates parse failures and AST surgery simultaneously.

### Alternative I: Fixed-Width `56px` Corridors or Hard Zero-Crossing Aborts on Perpendicular Segments

*   **Pros:** Fixed `56px` corridors avoid synchronous reflow passes; aborting on every perpendicular crossing guarantees strictly planar drawings.
*   **Cons:** Fixed `56px` corridors fail when `>= 6` parallel lanes share a track, while hard zero-crossing aborts deterministically fail (`exit 2`) on valid minimal $K_{2,2}$ and $K_{3,3}$ bipartite flows (`2–3` workloads sharing `2–3` datastores).
*   **Reason for Rejection:** Combining 2-pass corridor widening (`56px..160px`), `>= 2`-edge bus spines, and `4px` halo bridges on irreducible perpendicular crossings (while enforcing `exit 2` on card, badge, verb-pill, and collinear overlaps) guarantees clean pages without rejecting valid architectures.

## Non-Functional Requirements

*   **Security & Privacy:**
    *   *Separated OAuth Scopes:* Target-scope `preflight` and `extract` clients ([internal/extract/client.go](../../internal/extract/client.go)) authenticate exclusively with `https://www.googleapis.com/auth/cloud-platform.read-only` and never mutate GCP resources; `--vertex-project` clients ([internal/synth/client.go](../../internal/synth/client.go)) authenticate separately with `https://www.googleapis.com/auth/cloud-platform` (`roles/aiplatform.user`).
    *   *Allowlisted Metadata & Zero Payload Retention:* `extract` never calls Secret Manager payload APIs, Cloud Storage object APIs, or BigQuery table-read APIs, populates `FactResource.Attributes` strictly from per-`asset_type` allowlists ([internal/extract/sanitize.go](../../internal/extract/sanitize.go)), and drops all `22` `sensitiveDroppedKeys` (`startup-script`, `startup-script-url`, `shutdown-script`, `user-data`, `env`, `envvariables`, `secretenv`, `environmentvariables`, `buildenvironmentvariables`, `args`, `command`, `script`, `data`, `binarydata`, `stringdata`, `substitutions`, `query`, `rawquery`, `schema`, `webhookconfig`, `kubectl.kubernetes.io/last-applied-configuration`, `last-applied-configuration`) before writing `facts.json` (`0600`).
    *   *Customer-Controlled Vertex AI Boundary & Escaped Offline Rendering:* Stage 2 sends only sanitized structural metadata to Vertex AI inside `--vertex-project`. Stage 3 ([internal/render/html.go](../../internal/render/html.go), [internal/render/cdp.go](../../internal/render/cdp.go)) escapes all strings via `html/template` and HTML-safe `encoding/json` and executes offline (`--disable-background-networking` and `Content-Security-Policy: default-src 'none'`).
    *   *SVG Injection Hardening:* All embedded icons in [internal/icons](../../internal/icons) pass [ValidateSVG](../../internal/icons/validate.go) (rejecting `<script>`, `<foreignObject>`, `<iframe>`, `<embed>`, `<object>`, `on*` handlers, `@import`, `expression()`, backslash escapes, and external URLs).
*   **Performance, Quotas & Scale:**
    *   *Stage 1 (`extract`):* Enforces `<= 80` CAI `ListAssets` requests per minute, `<= 10 QPS` Cloud Monitoring, `<= 10 QPS` BigQuery `jobs.list` (metadata projection only, USD 0 SQL slot cost), and `<= 10 QPS` Dataflow `jobs.list` (active jobs only when enabled), folding Cloud Build runs in-stream from CAI with `0` extra RPCs and keeping post-GC Go heap below `512 MB`.
    *   *Stage 2 (`synthesize`):* Supports up to `500` active `FactResource`s per run after plumbing/default suppression; short-ID interning cuts URI token volume by ~80%; Pass 2 executes concurrently (`--concurrency 4` default); Day-2 `--reconcile` executes `0` Vertex AI calls in `< 100 ms` when no resource or edge deltas exist.
    *   *Stage 3 (`render`):* Reuses one isolated headless Chrome process (`chromedp`) across all views, completing layout, corridor reflow, `3x .png` screenshots, vector `.pdf` printing, and `.html` export in `<= 2 s` for a 4–8 view suite.
*   **Rollout & Compatibility:**
    *   Legacy `v2` packages, `.drawio`/`.svg` exporters, `explore`, `.snap` files, and the historical `acme-cicd` `v2` baseline archive have been removed without dual-pipeline flags.

--------------------------------------------------------------------------------

## Detailed Implementation Breakdown

Implementation was delivered across **four incremental phases**, keeping `go build ./... && go test -race ./...` green at every boundary before atomic CLI cutover and legacy deletion in Phase 4.

### Phase 1: Core Contracts, Embedded Icons, Canonicalizer, and Manifest Validator

#### 1.1 [internal/icons/assets/icons.yaml](../../internal/icons/assets/icons.yaml), [internal/icons/validate.go](../../internal/icons/validate.go), [internal/icons/registry.go](../../internal/icons/registry.go), and [internal/icons/icons_test.go](../../internal/icons/icons_test.go)

*   **Package:** [internal/icons](../../internal/icons)
*   **Implemented Components:**
    *   Salvaged `80` inline SVG definitions (`map[string]struct{ SVG string }`) from the retired v2 `internal/catalog/assets/icons.yaml` into [internal/icons/assets/icons.yaml](../../internal/icons/assets/icons.yaml) and appended `5` deterministic inline `24x24` SVG definitions for `github`, `external_client`, `private_services_access`, `k8s_workload`, and `k8s_ingress` (`85` total icon keys).
    *   In [internal/icons/validate.go](../../internal/icons/validate.go), migrated `ValidateSVG(svg string) error` from the retired v2 `internal/catalog/svg.go`, returning standard `fmt.Errorf` errors rejecting `<script>`, `<foreignObject>`, `<iframe>`, `<embed>`, `<object>`, `on*` attributes, `@import`, `expression()`, backslash escapes, and external URLs.
    *   In [internal/icons/registry.go](../../internal/icons/registry.go), salvaged the XML token rewriter and `<defs>`-hoisting logic from `compileIconSymbol` in the retired v2 `internal/svg/render.go`, exposing `Load() (*Registry, error)`, `Has(key string) bool`, `ValidKeys() []string` (sorted clone), and `SymbolSprite(keys []string) string` emitting `<svg style="display:none"><defs>...</defs><symbol id="gcp-icon-<key>" viewBox="...">...</symbol></svg>` with internal `id`/`href`/`url(#...)` tokens prefixed by `icon-<key>-`.
    *   In [internal/icons/icons_test.go](../../internal/icons/icons_test.go), verifies `ValidateSVG` rejection cases, `ValidKeys()` count/sorting (`const wantCount = 85`), and `<filter>`/`<mask>` `<defs>` hoisting across all `85` icons.

#### 1.2 [internal/extract/schema.go](../../internal/extract/schema.go), [internal/extract/canonical.go](../../internal/extract/canonical.go), and [internal/extract/schema_canonical_test.go](../../internal/extract/schema_canonical_test.go)

*   **Package:** Base schema and canonicalizer in [internal/extract](../../internal/extract) (reusing [internal/ioutil/atomic.go](../../internal/ioutil/atomic.go) `WriteFileAtomic`)
*   **Implemented Components:**
    *   In [internal/extract/schema.go](../../internal/extract/schema.go), implements `LoadFacts(path string) (*FactsFile, error)` and `WriteFacts(path string, f *FactsFile) error` (plain indented UTF-8 JSON written via `ioutil.WriteFileAtomic(path, data, 0o600, 0o700)` after deterministic slice sorting) over exact Go types:

        ```go
        package extract

        const FactsSchemaVersion = "v3"

        type FactEdgeKind string
        const (
            EdgeConfigRef          FactEdgeKind = "CONFIG_REF"
            EdgeK8sIngressBackend  FactEdgeKind = "K8S_INGRESS_BACKEND"
            EdgeK8sNEGBackend      FactEdgeKind = "K8S_NEG_BACKEND"
            EdgeK8sServiceSelector FactEdgeKind = "K8S_SERVICE_SELECTOR"
            EdgeWorkloadIdentity   FactEdgeKind = "WORKLOAD_IDENTITY"
            EdgeCloudBuildRepo     FactEdgeKind = "CLOUDBUILD_TRIGGER_REPO"
            EdgeCloudBuildStepRef  FactEdgeKind = "CLOUDBUILD_STEP_REF"
            EdgeIAMResourceBinding FactEdgeKind = "IAM_RESOURCE_BINDING"
            EdgePSAPeering         FactEdgeKind = "PSA_PEERING"
            EdgeVPCPeering         FactEdgeKind = "VPC_PEERING"
            EdgeBQLineage          FactEdgeKind = "BQ_LINEAGE"
            EdgeMonitoringTraffic  FactEdgeKind = "MONITORING_TRAFFIC"
        )

        type CoverageState string
        const (
            CoverageOK              CoverageState = "OK"
            CoverageIAMDenied       CoverageState = "IAM_DENIED"
            CoverageVPCSCBlocked    CoverageState = "VPC_SC_BLOCKED"
            CoverageServiceDisabled CoverageState = "SERVICE_DISABLED"
            CoverageTruncatingCap   CoverageState = "TRUNCATING_CAP"
        )

        type FactsFile struct {
            SchemaVersion            string           `json:"schema_version"`
            ReadTime                 string           `json:"read_time"`
            Scopes                   []string         `json:"scopes"`
            Hierarchy                Hierarchy        `json:"hierarchy"`
            Resources                []FactResource   `json:"resources"`
            Edges                    []FactEdge       `json:"edges"`
            SuppressedPlumbingCounts map[string]int   `json:"suppressed_plumbing_counts"`
            OutOfScopeTargets        []OutOfScopeHint `json:"out_of_scope_targets,omitempty"`
            CoverageLedger           []CoverageEntry  `json:"coverage_ledger"`
        }

        type Hierarchy struct {
            Organizations []OrgNode     `json:"organizations,omitempty"`
            Folders       []FolderNode  `json:"folders,omitempty"`
            Projects      []ProjectNode `json:"projects"`
        }

        type OrgNode struct {
            OrgID       string `json:"org_id"`
            DisplayName string `json:"display_name,omitempty"`
        }

        type FolderNode struct {
            FolderID    string `json:"folder_id"`
            DisplayName string `json:"display_name,omitempty"`
            Parent      string `json:"parent,omitempty"`
        }

        type ProjectNode struct {
            ProjectID            string `json:"project_id"`
            ProjectNumber        string `json:"project_number,omitempty"`
            DisplayName          string `json:"display_name,omitempty"`
            Parent               string `json:"parent,omitempty"`
            IsPSATenant          bool   `json:"is_psa_tenant,omitempty"`
            SharedVPCHostProject string `json:"shared_vpc_host_project,omitempty"`
        }

        type FactResource struct {
            URI                 string         `json:"uri"`
            AssetType           string         `json:"asset_type"`
            ProjectID           string         `json:"project_id"`
            Location            string         `json:"location,omitempty"`
            DisplayName         string         `json:"display_name"`
            ParentURI           string         `json:"parent_uri,omitempty"`
            IsExternal          bool           `json:"is_external,omitempty"`
            Attributes          map[string]any `json:"attributes,omitempty"`
            ProjectCapabilities []string       `json:"project_capabilities,omitempty"`
        }

        type FactEdge struct {
            ID      string       `json:"id"`
            FromURI string       `json:"from_uri"`
            ToURI   string       `json:"to_uri"`
            Kind    FactEdgeKind `json:"kind"`
            Detail  string       `json:"detail,omitempty"`
        }

        type OutOfScopeHint struct {
            TargetProjectID  string   `json:"target_project_id"`
            SyntheticURI     string   `json:"synthetic_uri"`
            ReferencedByURIs []string `json:"referenced_by_uris"`
            SourceKind       string   `json:"source_kind"`
        }

        type CoverageEntry struct {
            Scope  string        `json:"scope"`
            Source string        `json:"source"`
            State  CoverageState `json:"state"`
            Detail string        `json:"detail,omitempty"`
        }
        ```

    *   In [internal/extract/canonical.go](../../internal/extract/canonical.go), migrated `CanonicalizeAIP122` and `CanonicalizeAIP122WithService` from the retired v2 `internal/ir/canonical.go`, updating `inferServiceFromRelative` (`inferScopedProjectService`, `inferZonalService`, `inferLocationsSubpathService`) and `normalizeServicePath` so: (a) for relative `projects/<p>/...` paths without a service host, `inferServiceFromRelative` uses `isComputeScopeCollection(collection)` to infer `compute.googleapis.com`, while `inferLocationsSubpathService` explicitly excludes `"instances"` (`segs[4] != "instances"`) so ambiguous relative `/locations/<loc>/instances/<name>` paths do not infer `compute.googleapis.com` and collide with Redis, Memcache, or Filestore; (b) `normalizeServicePath` rewrites `/locations/{global,<region>,<zone>}/<collection>/` to `/{global,regions/<region>,zones/<zone>}/<collection>/` (`gcpZoneRe` for zones) **strictly when the resolved service is `compute.googleapis.com`** (`case svcCompute:`)—never altering non-Compute services even when their collection name is in `isComputeScopeCollection` (e.g., `//redis.googleapis.com/.../locations/europe-west1/instances/cache-1`, plus Cloud Build, Cloud Run, and Artifact Registry `/locations/` paths), while explicit `//compute.googleapis.com/.../locations/<zone>/instances/<name>` URIs still normalize to `/zones/<zone>/instances/<name>`; and (c) `container.googleapis.com` `/zones/<z>/clusters/<c>` normalizes to `/locations/<z>/clusters/<c>`.
    *   In [internal/extract/schema_canonical_test.go](../../internal/extract/schema_canonical_test.go), tests `EdgeID` determinism (`TestEdgeIDDeterminism`), deterministic `WriteFacts`/`LoadFacts` sorting, `0600`/`0700` permissions, and error/empty-collection handling (`TestWriteAndLoadFactsDeterministicAndPermissions`, `TestWriteAndLoadFactsErrorsAndEmptyCollections`), `BuildProjectIndex`, and Compute vs. non-Compute / relative-path / `"instances"`-exclusion / GKE URI canonicalization (`TestCanonicalizeAIP122`, `TestCanonicalizeAIP122WithService`).

#### 1.3 [internal/manifest/types.go](../../internal/manifest/types.go), [internal/manifest/validate.go](../../internal/manifest/validate.go), and [internal/manifest/manifest_test.go](../../internal/manifest/manifest_test.go)

*   **Package:** `architecture.yaml` contract, YAML codec, and validator in [internal/manifest](../../internal/manifest)
*   **Implemented Components:**
    *   In [internal/manifest/types.go](../../internal/manifest/types.go), implements `Load(path string) (*Manifest, error)` (`yaml.NewDecoder` with `KnownFields(true)` and trailing-document rejection), `Marshal(m *Manifest) ([]byte, error)` (deterministic sorting preserving `Placements`, `InlineMeta`, `EvidenceEdgeIDs`, and `views[0]` `view_0_system_index` ordering), `Write(path string, m *Manifest) error` (`ioutil.WriteFileAtomic(path, data, 0o644, 0o755)`), and exact Go structs/enums:

        ```go
        package manifest

        const (
            SchemaVersion     = "v1"
            SystemIndexViewID = "view_0_system_index"
        )

        type ComponentKind string
        const (
            KindExternalCaller ComponentKind = "EXTERNAL_CALLER"
            KindIngress        ComponentKind = "INGRESS"
            KindWorkload       ComponentKind = "WORKLOAD"
            KindPipeline       ComponentKind = "PIPELINE"
            KindMessaging      ComponentKind = "MESSAGING"
            KindDatastore      ComponentKind = "DATASTORE"
            KindCohort         ComponentKind = "COHORT"
            KindExternalTarget ComponentKind = "EXTERNAL_TARGET"
        )

        type ContainerKind string
        const (
            ContainerProject      ContainerKind = "PROJECT"
            ContainerVPC          ContainerKind = "VPC"
            ContainerGKECluster   ContainerKind = "GKE_CLUSTER"
            ContainerPSAZone      ContainerKind = "PSA_ZONE"
            ContainerExternalZone ContainerKind = "EXTERNAL_ZONE"
            ContainerSharedHub    ContainerKind = "SHARED_HUB"
        )

        type Archetype string
        const (
            ArchetypeLandscapeMatrix Archetype = "landscape_matrix"
            ArchetypeStagePipeline   Archetype = "stage_pipeline"
            ArchetypeHubSpoke        Archetype = "hub_spoke"
        )

        type AccentMode string
        const (
            AccentRuntime AccentMode = "RUNTIME"
            AccentCICD    AccentMode = "CICD"
            AccentMixed   AccentMode = "MIXED"
        )

        type FlowKind string
        const (
            FlowRuntime FlowKind = "RUNTIME"
            FlowCICD    FlowKind = "CICD"
            FlowAdmin   FlowKind = "ADMIN"
        )

        type Provenance string
        const (
            ProvenanceVerified Provenance = "VERIFIED"
            ProvenanceInferred Provenance = "INFERRED"
        )

        type Manifest struct {
            SchemaVersion     string             `yaml:"schema_version"`
            Metadata          Metadata           `yaml:"metadata"`
            Components        []Component        `yaml:"components"`
            Containers        []Container        `yaml:"containers,omitempty"`
            ExcludedResources []ExcludedResource `yaml:"excluded_resources,omitempty"`
            RejectedEdges     []RejectedEdge     `yaml:"rejected_edges,omitempty"`
            Views             []View             `yaml:"views"`
        }

        type Metadata struct {
            GeneratedAt string   `yaml:"generated_at"`
            Scopes      []string `yaml:"scopes"`
        }

        type BackedResource struct {
            URI       string `yaml:"uri"`
            AssetType string `yaml:"asset_type"`
            ProjectID string `yaml:"project_id"`
        }

        type InlineMeta struct {
            Label string `yaml:"label"`
            Value string `yaml:"value"`
        }

        type Component struct {
            ID            string           `yaml:"id"`
            Title         string           `yaml:"title"`
            Subtitle      string           `yaml:"subtitle,omitempty"`
            IconKey       string           `yaml:"icon_key"`
            ComponentKind ComponentKind    `yaml:"component_kind"`
            ProjectID     string           `yaml:"project_id"`
            Notes         string           `yaml:"notes,omitempty"`
            BackedBy      []BackedResource `yaml:"backed_by"`
            InlineMeta    []InlineMeta     `yaml:"inline_meta,omitempty"`
        }

        type Container struct {
            ID                string           `yaml:"id"`
            Title             string           `yaml:"title"`
            Subtitle          string           `yaml:"subtitle,omitempty"`
            Kind              ContainerKind    `yaml:"kind"`
            ParentContainerID string           `yaml:"parent_container_id,omitempty"`
            BackedBy          []BackedResource `yaml:"backed_by,omitempty"`
        }

        type ExcludedResource struct {
            URI       string `yaml:"uri"`
            AssetType string `yaml:"asset_type"`
            ProjectID string `yaml:"project_id"`
            Reason    string `yaml:"reason"`
        }

        type RejectedEdge struct {
            From   string `yaml:"from"`
            To     string `yaml:"to"`
            Reason string `yaml:"reason"`
        }

        type View struct {
            ID           string      `yaml:"id"`
            Title        string      `yaml:"title"`
            Subtitle     string      `yaml:"subtitle,omitempty"`
            Archetype    Archetype   `yaml:"archetype"`
            AccentMode   AccentMode  `yaml:"accent_mode"`
            Notes        string      `yaml:"notes,omitempty"`
            ContainerIDs []string    `yaml:"container_ids,omitempty"`
            Placements   []Placement `yaml:"placements"`
            Edges        []Edge      `yaml:"edges,omitempty"`
        }

        type Placement struct {
            ComponentID string       `yaml:"component_id"`
            ContainerID string       `yaml:"container_id,omitempty"`
            Column      int          `yaml:"column"`
            OffPageRefs []OffPageRef `yaml:"off_page_refs,omitempty"`
        }

        type OffPageRef struct {
            TargetViewID string `yaml:"target_view_id"`
            Label        string `yaml:"label"`
            Direction    string `yaml:"direction"` // "IN" | "OUT"
        }

        type Edge struct {
            From               string     `yaml:"from"`
            To                 string     `yaml:"to"`
            Verb               string     `yaml:"verb"`
            FlowKind           FlowKind   `yaml:"flow_kind"`
            Provenance         Provenance `yaml:"provenance"`
            EvidenceEdgeIDs    []string   `yaml:"evidence_edge_ids,omitempty"`
            InferenceRationale string     `yaml:"inference_rationale,omitempty"`
            ViaBusID           string     `yaml:"via_bus_id,omitempty"`
        }

        type ValidationReport struct {
            Valid  bool              `json:"valid"`
            Errors []ValidationIssue `json:"errors,omitempty"`
        }

        type ValidationIssue struct {
            Code    string `json:"code"`
            ViewID  string `json:"view_id,omitempty"`
            Subject string `json:"subject,omitempty"`
            Message string `json:"message"`
        }
        ```

    *   In [internal/manifest/validate.go](../../internal/manifest/validate.go), implements `ValidateStructure(m *Manifest, icons *icons.Registry) error` and `ValidateWithFacts(m *Manifest, facts *extract.FactsFile, icons *icons.Registry) error` (returning `*ValidationError` wrapping `[]ValidationIssue`, mapped to exit code `2`), enforcing directed-walk `FactEdge` provenance, 100% `FactsFile` resource accounting across `components[].backed_by`, `containers[].backed_by`, and `excluded_resources`, stateful-datastore `asset_type` homogeneity, `View 0` rules (`views[0].id == "view_0_system_index"` iff `len(detailViews) >= 2`, `1..16` placements), `1..12` `Placements` per detail view, column bounds (`0..4` / `0..4` / `0..2`), `len(verb) <= 16`, single-source/single-target `via_bus_id`, container depth `<= 2`, and `rejected_edges`.
    *   In [internal/manifest/manifest_test.go](../../internal/manifest/manifest_test.go), tests every validation error code, multi-hop directed-walk provenance, 100% resource accounting, folded 1:1 `ServiceAccount`/`CryptoKey` on `DATASTORE` components, and `KnownFields(true)` rejection.
*   **Phase 1 Checkpoint:** `go build ./... && go test -race -count=1 ./internal/icons ./internal/manifest ./internal/extract`

---

### Phase 2: Complete Multi-Project Fact Extractor and `acme-cicd` `v3` Fixture

#### 2.1 [internal/extract/constants.go](../../internal/extract/constants.go), [internal/extract/client.go](../../internal/extract/client.go), [internal/extract/sanitize.go](../../internal/extract/sanitize.go), [internal/extract/cai.go](../../internal/extract/cai.go), [internal/extract/gke.go](../../internal/extract/gke.go), [internal/extract/edges.go](../../internal/extract/edges.go), [internal/extract/enrichers.go](../../internal/extract/enrichers.go), and [internal/extract/extract_test.go](../../internal/extract/extract_test.go)

*   **Package:** [internal/extract](../../internal/extract) (salvaged from retired v2 `internal/ingest`, `internal/catalog`, and `internal/compiler`)
*   **Implemented Components:**
    *   [internal/extract/constants.go](../../internal/extract/constants.go): Centralizes shared GCP service hosts, asset types, AIP-122/CLI prefixes, preflight probe constants (`sourceCAIFolders = "cai.folders"`, `roleFolderViewer = "roles/resourcemanager.folderViewer"`, `roleAIPlatformUser = "roles/aiplatform.user"`, `sourceVertexIAM = "vertex.iam"`), and HTTP scheme helpers.
    *   [internal/extract/client.go](../../internal/extract/client.go): Rate-limited clients (`80 RPM` CAI, `10 QPS` Monitoring/BigQuery/Dataflow, scoped strictly to `https://www.googleapis.com/auth/cloud-platform.read-only`, omitting `ACM`/`RAGLimiter`), `--project-alias` map support, `ClassifyGCPError` (distinguishing fatal `ErrorKindQuotaFatal`/`ErrorKindUnauthenticated` from degraded `CoverageServiceDisabled`, `CoverageIAMDenied`, `CoverageVPCSCBlocked`), and `RunPreflight(ctx, clients, scopes, vertexOpts)` (probing target `cloudasset.assets.listResource`, `cai.folders` / `roles/resourcemanager.folderViewer` via `probeCAIFolderAccess` on `--org`/`--folder` scopes, `monitoring.timeSeries.list`, `bigquery.jobs.list`, and `dataflow.jobs.list`, plus optional zero-token `--vertex-project` `probeVertexReadiness` running `projects.testIamPermissions` for `aiplatform.endpoints.predict` and `VertexPreflightOptions.ModelProbe`—falling back to `probeVertexModelREST` when `ModelProbe` is `nil`—using `cloud-platform` scope).
    *   [internal/extract/sanitize.go](../../internal/extract/sanitize.go): `SanitizeURL` and `ExtractSanitizedRefs` (`artifactRegistryRe` matching `-(docker|python|maven|npm|apt|yum|go|generic|kfp).pkg.dev/<proj>/<repo>`, `gcr.io/<proj>/...`, bare `<proj>:<region>:<instance>` Cloud SQL refs, `--project=<id>` / `--project <id>`, `--cluster=<name>`, and `gcloud run deploy <svc>`); per-`asset_type` attribute allowlists (including `projectCloudTasksQueueAttrs` projecting `httpTarget.{oidcToken,oauthToken}.serviceAccountEmail` onto `service_account`); scanning of `ConfigMap.data`, workload `containers` + `initContainers` (`command`, `args`, `env`), and Cloud Build `steps[]` (`name`, `entrypoint`, `args`, `script`) before dropping payloads and all `22` `sensitiveDroppedKeys` (including `webhookconfig`, `kubectl.kubernetes.io/last-applied-configuration`, and `last-applied-configuration`); `plumbingAssetTypes`, `plumbingAssetPrefixes` (`admissionregistration.k8s.io/`, `rbac.authorization.k8s.io/`, `gateway.networking.k8s.io/`, `autoscaling.k8s.io/`, `servicedirectory.googleapis.com/`), `IsPlumbingAssetType` (suppressing `plumbingAssetTypes`, `plumbingAssetPrefixes`, and `suppressedPlumbingServices` excluding `compute.googleapis.com`, `container.googleapis.com`, and `iam.googleapis.com`, whereas Cloud Monitoring `aggregateConsumedAPICalls` in [internal/extract/enrichers.go](../../internal/extract/enrichers.go) suppresses all hosts in `suppressedPlumbingServices`), control-plane plumbing (`accesscontextmanager.googleapis.com/{AccessPolicy,ServicePerimeter}`, `servicenetworking.googleapis.com/Connection`, `containeranalysis.googleapis.com/Occurrence`, plus `cloudresourcemanager.googleapis.com/{Organization,Folder,Project}` and `serviceusage.googleapis.com/Service`, which `ingestHierarchyAsset` in [internal/extract/cai.go](../../internal/extract/cai.go) captures into `Hierarchy` / `enabledServices` and returns early before `ingestFoldedOrPlumbingAsset` so they are omitted from `resources[]` without incrementing `suppressed_plumbing_counts`), default sandbox/SA suppression, GKE node-pool/VM suppression, LB sub-resource folding onto `ForwardingRule`/`GlobalForwardingRule`, and `IsPSATenantProject` (`^p[a-z0-9]+-tp$`).
    *   [internal/extract/cai.go](../../internal/extract/cai.go): `ReadTime`-pinned CAI `RESOURCE` and `IAM_POLICY` pagination (including `cloudresourcemanager.googleapis.com/Organization` and `Folder` in `iamPolicyAssetTypes` and dropping `ACCESS_POLICY`), `extractRetainedMetadata` (emitting generic `kms_key_uri` -> `cmek` `CONFIG_REF` refs before dispatching to `retainedMetadataExtractors` across `compute.googleapis.com/{Network,Subnetwork,Instance,ForwardingRule,GlobalForwardingRule,Router,InterconnectAttachment,ServiceAttachment}`, `container.googleapis.com/Cluster`, `sqladmin.googleapis.com/Instance`, `alloydb.googleapis.com/Cluster`, `redis.googleapis.com/{Instance,Cluster}`, `memcache.googleapis.com/Instance`, `file.googleapis.com/Instance`, `cloudbuild.googleapis.com/BuildTrigger`, `run.googleapis.com/{Service,Job}`, `cloudfunctions.googleapis.com/CloudFunction`, `appengine.googleapis.com/Service`, `pubsub.googleapis.com/Subscription`, `eventarc.googleapis.com/Trigger`, `cloudtasks.googleapis.com/Queue` via `extractCloudTasksQueueMetadata`, `cloudscheduler.googleapis.com/Job`, `storagetransfer.googleapis.com/TransferJob`, `bigquerydatatransfer.googleapis.com/TransferConfig`, `bigqueryconnection.googleapis.com/Connection`, `datastream.googleapis.com/Stream`, `dataproc.googleapis.com/{Cluster,Batch}`, `metastore.googleapis.com/Service`, `composer.googleapis.com/Environment`, `discoveryengine.googleapis.com/{Engine,DataStore}`, `dialogflow.googleapis.com/Agent`, `aiplatform.googleapis.com/ReasoningEngine`, `iam.googleapis.com/ServiceAccount`, and `cloudkms.googleapis.com/CryptoKey`), `ingestAbsorbedRecord` (indexing `VpnTunnel`, `networkconnectivity.googleapis.com/Spoke`, `datastream.googleapis.com/{ConnectionProfile,PrivateConnection}`, `alloydb.googleapis.com/Instance`, `aiplatform.googleapis.com/FeatureView`, `analyticshub.googleapis.com/Listing`, and `dataplex.googleapis.com/Asset`), `foldBigQueryChild` (capturing external `Table`/`Routine`/`Model` URIs), `extractCloudRunTraffic` / `reconcileCloudRunService`, `reconcileAppEngineService`, in-stream min-heap folding (`flushCloudBuildHeap`) of top-`N` (`--cloudbuild-runs 3`) `SUCCESS` `cloudbuild.googleapis.com/Build` assets per `buildTriggerId` (plus `<= 5` triggerless builds per project, with `maybeAddInScopeProjectResource` synthesizing an in-scope `Project` `FactResource` whenever triggerless builds emit `CLOUDBUILD_STEP_REF` edges) to emit `CLOUDBUILD_STEP_REF` and `CLOUDBUILD_TRIGGER_REPO` (with synthetic `//github.com/<owner>/<repo>` `FactResource`), and `IAM_RESOURCE_BINDING` vs `ProjectCapabilities` (including unambiguous single-instance/conditioned project-level `roles/cloudsql.client` bindings).
    *   [internal/extract/gke.go](../../internal/extract/gke.go): Joins `ForwardingRule -> ... -> NetworkEndpointGroup -> k8s.io/Service` via `cloud.google.com/neg-status` (`K8S_NEG_BACKEND`), L4 `Service.status.loadBalancer.ingress[].ip -> ForwardingRule` (`K8S_INGRESS_BACKEND`), `Ingress` host/path attributes, guarded `len(Service.spec.selector) > 0` workload matching (`spec.template` and `CronJob` `spec.jobTemplate.spec.template`, `K8S_SERVICE_SELECTOR`), and `k8s.io/ServiceAccount` `iam.gke.io/gcp-service-account` (`WORKLOAD_IDENTITY`).
    *   [internal/extract/edges.go](../../internal/extract/edges.go): `resolveLoadBalancerNonGKEBackends`, `resolveStructuralRefs` resolving `outboundRefs` populated by `extractRetainedMetadata` / `retainedMetadataExtractors` in [internal/extract/cai.go](../../internal/extract/cai.go), `resolveAbsorbedBridges` (running `resolveAbsorbedResourceBridges` for `compute.googleapis.com/VpnTunnel`, `networkconnectivity.googleapis.com/Spoke`, `aiplatform.googleapis.com/FeatureView`, `analyticshub.googleapis.com/Listing`, and `dataplex.googleapis.com/Asset`; `resolveDatastreamStreamBridges` for `datastream.googleapis.com/Stream` CDC/GCS bridges across absorbed `ConnectionProfile`/`PrivateConnection` records; and `resolveBigQueryFoldedExternalURIs` for `bq_remote_fn`, `biglake`, and `bq_external` edges), `indexCloudRunHosts`, `indexSingleVPCPeering` / `emitPSAPeeringAndSharedVPC` (`VPC_PEERING`, `PSA_PEERING` for `isPSAManagedService`, `redis.googleapis.com/Cluster` PSC `psc_network`, and Shared VPC host recording), `indexVPCAndPrivateIPs` + `matchPrivateIP`, and synthetic external `FactResource` (`IsExternal: true`) + `OutOfScopeHint` generation for out-of-scope target projects.
    *   [internal/extract/enrichers.go](../../internal/extract/enrichers.go): `runEnrichers` orchestrating `fetchBigQueryJobs`/`applyBigQueryLinks`/`applySingleBigQueryLink` (`BQ_LINEAGE` `sql_elt` and `bq_job` edges, with `resolveBQJobPrincipalSource` resolving a principal to its dedicated workload when `1` workload uses that Service Account and falling back to the unsuppressed `iam.googleapis.com/ServiceAccount` URI (`sa.uri`) when `0` or `> 1` workloads match), `fetchDataflowJobs`/`applyDataflowLinks` (active Dataflow jobs via `Jobs.List(proj, "-").Filter("ACTIVE")`, `CONFIG_REF`), and `fetchMonitoring`/`applyMonitoringResult` (grouping `consumed_api` by **`resource.labels.method`**, applying `IsReadOnlyDiscoveryMethod` unconditionally across all services to filter `get*`/`list*`/`aggregatedlist*`/`batchget*`/`testiampermissions`/`watch*`/`poll*`/`listen`, suppressing `transversalControlAPIs` and `suppressedPlumbingServices` in `aggregateConsumedAPICalls`, resolving callers via `resolveMonitoringSource`—dedicated workload when `1` workload matches, dropped when `> 1` workloads share the Service Account, and fallback to the unsuppressed `iam.googleapis.com/ServiceAccount` URI (`sa.uri`) when `0` workloads match—to join companion API hosts to unambiguous single-instance project targets as `MONITORING_TRAFFIC` in `emitConsumedAPIEdges`, plus `queryPublisherInvocations`, `queryPublisherThroughput`, `materializePublisherModels`, and `emitPublisherModelEdges` querying `aiplatform.googleapis.com/publisher/online_serving/{model_invocation_count,consumed_token_throughput}` to synthesize `aiplatform.googleapis.com/PublisherModel` `FactResource`s and `MONITORING_TRAFFIC` edges via `resolveMonitoringSource` when `aiplatform.googleapis.com` is enabled).
    *   [internal/extract/extract_test.go](../../internal/extract/extract_test.go): Race-enabled tests covering `cai.folders` preflight probe, GKE `CronJob`/empty-selector/NEG/Workload-Identity joins, in-stream CAI `Build` top-`N` min-heap folding (`CloudBuildRuns: 2` and `1`), allowlist secret scrubbing (`last-applied-configuration`, `ConfigMap.data`, `script`), `SonarQube -> Cloud SQL` private-IP matching, `resource.labels.method` lowercase/PascalCase filtering, Vertex AI `PublisherModel` metric synthesis, `0600` `WriteFacts` mode, and external stub emission.

#### 2.2 [testdata/acme_cicd_v3.facts.json](../../testdata/acme_cicd_v3.facts.json)

*   **Artifact:** Deterministic `v3` `facts.json` fixture in `testdata/`
*   **Contents:** Models `acme-cicd` end-to-end: synthetic external `//github.com/acme-corp/platform-services`, 20 `BuildTrigger` assets with folded succeeded build `CLOUDBUILD_STEP_REF` edges (`acme-apps` Artifact Registry with `image_count: 643`, `gs://acme-tfstate`, `gs://acme-private-modules`, and out-of-scope deploy project stubs `acme-dev` / `acme-prod`), `VPC: cicd`, GKE cluster `cicd` with `Ingress`/`ForwardingRule`/`Service` chains for `atlantis` and `sonarqube`, private-IP `PSA_PEERING` + `CONFIG_REF` to `sonarqube` Cloud SQL in `p1234567890abcdef-tp`, and suppressed plumbing counts.
*   **Phase 2 Checkpoint:** `go build ./... && go test -race -count=1 ./internal/extract ./internal/icons ./internal/manifest`

---

### Phase 3: Two-Pass Vertex AI Synthesizer and Offline CSS Grid / `chromedp` Renderer

#### 3.1 [go.mod](../../go.mod) and [go.sum](../../go.sum)

*   **Dependencies:** Added `google.golang.org/genai v1.79.1` and `github.com/chromedp/chromedp v0.24.0` (with direct `elk-go`, `beevik/etree`, and direct `accesscontextmanager` pruned in Phase 4).

#### 3.2 [internal/synth/intern.go](../../internal/synth/intern.go), [internal/synth/schemas.go](../../internal/synth/schemas.go), [internal/synth/prompts.go](../../internal/synth/prompts.go), [internal/synth/client.go](../../internal/synth/client.go), [internal/synth/view0.go](../../internal/synth/view0.go), [internal/synth/pipeline.go](../../internal/synth/pipeline.go), and [internal/synth/synth_test.go](../../internal/synth/synth_test.go)

*   **Package:** [internal/synth](../../internal/synth)
*   **Implemented Components:**
    *   [internal/synth/intern.go](../../internal/synth/intern.go): Bidirectional `InternTable` (`r_0001..r_NNNN` <-> `FactResource`, `e_0001..e_MMMM` <-> `FactEdge`) plus `BuildAdjacency(facts, table)` emitting deduplicated `r_XXXX -> [{to: r_YYYY, kind}]` pairs for Pass 1.
    *   [internal/synth/schemas.go](../../internal/synth/schemas.go): JSON Go structs and non-recursive `*genai.Schema` definitions (`Pass1PlanSchema`, `Pass2ViewSchema`, `ReconcilePatchSchema` including `append_backed_by`, `append_container_backed_by`, `new_components`, `new_containers`, `new_excluded_resources`, `new_placements`, `new_edges`, and `new_views`) with `icon_key` typed as `genai.TypeString` and enum constraints on `archetype`, `accent_mode`, component/container `kind`, `flow_kind`, `provenance`, and `direction`.
    *   [internal/synth/prompts.go](../../internal/synth/prompts.go): Closed-world system instructions and JSON prompt builders for Pass 1 (adjacency-informed view decomposition, 1:1 folding into `backed_by_ids` + `inline_meta`), Pass 2 (per-view placements, `<= 16`-char `verb`, directed-walk `e_XXXX` citation, `via_bus_id`), and `--reconcile` (resource + inter-component edge delta patching).
    *   [internal/synth/client.go](../../internal/synth/client.go): `type ModelClient interface { GenerateJSON(ctx context.Context, systemPrompt, userPrompt string, schema *genai.Schema) ([]byte, error) }`, `VertexClient` over `google.golang.org/genai` (`BackendVertexAI`, `cloud-platform` OAuth scope, default model `"gemini-4-carbon"`), `VertexClientOptions.ThinkingLevel`, `ParseThinkingLevel` (`minimal`, `low`, `medium`, `high`, `xhigh`, or empty for model default) wiring `genai.ThinkingConfig`, and zero-token `ProbeVertexModel` (`Models.Get`).
    *   [internal/synth/view0.go](../../internal/synth/view0.go): `BuildSystemIndexView(m *manifest.Manifest) *manifest.View` producing `view_0_system_index` deterministically when `len(detailViews) >= 2` (and returning `nil` when `len(detailViews) == 1`).
    *   [internal/synth/pipeline.go](../../internal/synth/pipeline.go): Enforces `len(facts.Resources) <= 500`; implements `Synthesize` (`runPass1` + pre-Pass-2 gate + concurrent `errgroup` `(*pass2Runner).populateViews` + `<= 2` validation-feedback retries + `BuildSystemIndexView` + `ValidateWithFacts`) and `Reconcile` (`0`-LLM fast path on zero resource/edge deltas, vanished URI/edge pruning and empty-view deletion via `pruneDetailViews`/`pruneRemovedOffPageRefs`, deletion-only prune-only `0`-LLM path when zero unclaimed resources and zero new inter-component edges remain, `ReconcilePatch` merge with up to `2` validation-feedback retries (`maxValidationRetries = 2`, up to `3` total model calls), returning `(*manifest.Manifest, error)` without performing disk I/O—canonical YAML comparison and `manifest.Write` are handled by [cmd/gcp-arch/synthesize.go](../../cmd/gcp-arch/synthesize.go)).
    *   [internal/synth/synth_test.go](../../internal/synth/synth_test.go): Deterministic mock-`ModelClient` tests covering short-ID interning, Pass-1 adjacency projection, `ThinkingLevel` parsing/forwarding, `> 500` active-resource exit-code-`2` guard, retry-feedback recovery, `2`-retry exhaustion failure, single-view `View 0` omission vs multi-view `View 0` projection, `0`-LLM fast path, and Day-2 edge-only + resource delta reconciliation.

#### 3.3 [internal/render](../../internal/render) Static Font Assets, Stylesheet, Router, HTML Builder & `chromedp` Exporter

*   **Implemented Repository Files in [internal/render](../../internal/render):**
    *   [internal/render/assets/fonts/Inter-Regular.woff2](../../internal/render/assets/fonts/Inter-Regular.woff2) (extracted from official `Inter-4.1.zip` `extras/woff2/Inter-Regular.woff2`)
    *   [internal/render/assets/fonts/Inter-Medium.woff2](../../internal/render/assets/fonts/Inter-Medium.woff2) (`Inter-4.1.zip` `extras/woff2/Inter-Medium.woff2`)
    *   [internal/render/assets/fonts/Inter-SemiBold.woff2](../../internal/render/assets/fonts/Inter-SemiBold.woff2) (`Inter-4.1.zip` `extras/woff2/Inter-SemiBold.woff2`)
    *   [internal/render/assets/fonts/Inter-Bold.woff2](../../internal/render/assets/fonts/Inter-Bold.woff2) (`Inter-4.1.zip` `extras/woff2/Inter-Bold.woff2`)
    *   [internal/render/assets/fonts/Inter-OFL.txt](../../internal/render/assets/fonts/Inter-OFL.txt) (`Inter-4.1.zip` `LICENSE.txt`)
    *   [internal/render/assets/fonts/JetBrainsMono-Regular.woff2](../../internal/render/assets/fonts/JetBrainsMono-Regular.woff2) (extracted from official `JetBrainsMono-2.304.zip` `fonts/webfonts/JetBrainsMono-Regular.woff2`)
    *   [internal/render/assets/fonts/JetBrainsMono-Medium.woff2](../../internal/render/assets/fonts/JetBrainsMono-Medium.woff2) (`JetBrainsMono-2.304.zip` `fonts/webfonts/JetBrainsMono-Medium.woff2`)
    *   [internal/render/assets/fonts/JetBrainsMono-OFL.txt](../../internal/render/assets/fonts/JetBrainsMono-OFL.txt) (`JetBrainsMono-2.304.zip` `OFL.txt`)
    *   [internal/render/assets/diagram.css](../../internal/render/assets/diagram.css) (fixed `1040px` `.view-sheet`, `@page { size: A4 landscape; margin: 10mm; }`, `@media print { .view-sheet { zoom: var(--print-scale, 1); } }`, `data-cols="5"` compact typography/icon overrides (`20x20` icon, `11.5px` title, `9.5px` subtitle, `8.5px` metadata), CSS Grid templates for `landscape_matrix`, `stage_pipeline`, and `hub_spoke` with `var(--corridor-col-*)` / `var(--corridor-row-*)` tracks, flat spanning `.arch-container` frames with outward negative margins (`-26px -9px -8px -9px` depth-1, `-22px -4px -4px -4px` depth-2) + `.arch-container-badge`, `.arch-card` metadata tables, and `[n]` footnotes)
    *   [internal/render/assets/router.js](../../internal/render/assets/router.js) (`data-gcp-arch-routed` guard, `document.fonts.ready` + two nested `requestAnimationFrame` callbacks, `measureSheetGeometry` over `.arch-card`/`.arch-container-badge`/`.corridor-col`/`.corridor-row`, `assignPortCoords` 3-pass opposing-port deconfliction at `8px` min separation, `8px` lane routing, `>= 2`-edge bus spines, `4px` perpendicular halo bridges, 2-pass `applyCorridorWidening`—vertical `min(160px, max(24 + 8 * lanes, pillReserve))`, horizontal `min(160px, 24 + 8 * lanes + badgeReserve + 12)`, plus `160px` fallback widening on persistent collisions—forcing reflow via `sheet.getBoundingClientRect()`, strict card/badge/pill/collinear collision check, `--print-scale` calculation when sheet height exceeds `708px`, and `window.__GCP_ARCH_RENDER_PROMISE__` resolving `{ok, collisions, viewRects, html}`)
    *   [internal/render/prepare.go](../../internal/render/prepare.go) (barycenter row sorting with YAML-order tie-breaking, flat container `(minCol..maxCol, minRow..maxRow)` span computation with title-only container badges, `defaultRowCorridorTrack` (`18px`/`32px`/`52px`/`48px`/`94px`), dynamic card `min-height: max(54, 24 + (maxPorts - 1) * 24)px` when `>= 3` ports on one side in `buildPreparedCards`, multi-column `RoutingEdge.WaypointRow` (`pickClearWaypointRow` indexing `.corridor-row` tracks) + same-column lateral detour generation in `buildRoutingEdges`, and directionally partitioned `assignBusGroups` skipping same-column edges)
    *   [internal/render/html.go](../../internal/render/html.go) (`html/template` + HTML-safe `encoding/json` rendering, `Content-Security-Policy: default-src 'none'; ...`, base64 `.woff2` `@font-face` rules, `icons.SymbolSprite`, title-only `.arch-container-badge`, and view-only HTML output with no Inventory Coverage Table)
    *   [internal/render/cdp.go](../../internal/render/cdp.go) (`FindChrome(explicitPath string, fallbackPaths ...string)` failing fast on missing `explicitPath` for all formats including `--format html` alone and falling through `CHROME_PATH` -> `fallbackPaths` (`RenderOptions.ConfigChromePath` / `config.yaml` `chrome_path` wired by [cmd/gcp-arch/render.go](../../cmd/gcp-arch/render.go)) -> `PATH` (`google-chrome`, `google-chrome-stable`, `chromium`, `chromium-browser`) -> OS install paths, `BrowserRunner` interface + `ChromeDPRunner` over `chromedp` with isolated `--user-data-dir`, allocator flags (`--headless=new`, `--disable-gpu`, `--no-first-run`, `--no-default-browser-check`, `--disable-background-networking`, `--disable-sync`, `--disable-extensions`, `--metrics-recording-only`, `--mute-audio`), `needsNoSandbox()` Linux container `--no-sandbox` fallback (when `/proc/sys/kernel/unprivileged_userns_clone` is `"0"` or `os.Geteuid() == 0`), upfront `SetDeviceMetricsOverride(1200, 900, 3.0, false)`, `CaptureBeyondViewport: true` `00`-indexed PNGs, `PrintToPDF` emitting `len(views)` pages, and Chrome-free `--format html` fallback strictly when auto-discovery fails without an explicit `--chrome-path`)
    *   [internal/render/render_test.go](../../internal/render/render_test.go) (`wOF2` (`0x774F4632`) font header verification, HTML/CSP/XSS-escaping tests on `</script>` strings, assertion that no Inventory Coverage Table is rendered, `prepare.go` barycenter/waypoint/bus unit tests, mock `BrowserRunner` unit tests covering `cdp.go` orchestration and `LayoutCollisionError` exit-`2` handling for deterministic `>= 85%` coverage, plus real headless Chrome tests—skipped only if Chrome is absent unless `CI=true`—verifying zero overlaps on $K_{2,2}$/$K_{3,3}$ views with `4px` bridges, corridor widening on `>= 8` parallel lanes, exit-`2` on forced card/collinear collision, vector `.pdf` output, and `3x` `.png` pixel dimensions)

#### 3.4 [testdata/acme_cicd_expected.architecture.yaml](../../testdata/acme_cicd_expected.architecture.yaml)

*   **Artifact:** Golden `v1` `architecture.yaml` fixture in `testdata/` passing `ValidateWithFacts` against [testdata/acme_cicd_v3.facts.json](../../testdata/acme_cicd_v3.facts.json) across `view_0_system_index`, `cicd_delivery_pipeline`, `gke_developer_platform`, and `shared_network_psa`.
*   **Phase 3 Checkpoint:** `go build ./... && go test -race -count=1 ./internal/...`

---

### Phase 4: Atomic CLI Cutover, Legacy Deletion, Documentation, Examples & CI Automation

#### 4.1 [cmd/gcp-arch](../../cmd/gcp-arch) Cutover and Legacy Subcommand Deletion

*   **Deleted (5 retired v2 files):** `cmd/gcp-arch/explore.go`, `cmd/gcp-arch/export.go`, `cmd/gcp-arch/inspect.go`, `cmd/gcp-arch/snapshot.go`, `cmd/gcp-arch/views.go`
*   **Implemented Files:**
    *   [cmd/gcp-arch/main.go](../../cmd/gcp-arch/main.go): Exposes `--version` (`var version = "dev"`), loads and strictly validates `~/.config/gcp-arch/config.yaml` via `PersistentPreRunE` on all subcommands (including `validate`), defines typed `appDeps` hooks (`loadConfig`, `newExtractClients`, `runPreflight`, `probeVertexModel`, `extractFacts`, `newModelClient`, `newBrowserRunner(chromePath, configChromePath string)`), registers `preflight`, `extract`, `synthesize`, `validate`, and `render`, preserves `--org`, `--folder`, `--projects` (`--project` alias), `--project-alias`, and `-q/--quiet`, and maps `*manifest.ValidationError`, `*synth.ScaleLimitError`, and `*render.LayoutCollisionError` to exit code `2`.
    *   [cmd/gcp-arch/config.go](../../cmd/gcp-arch/config.go): Strict `yaml.v3` (`KnownFields(true)`) loader and validator for `~/.config/gcp-arch/config.yaml` (or `$XDG_CONFIG_HOME/gcp-arch/config.yaml`), supporting single-scalar coercion via `stringSlice.UnmarshalYAML` for `org`, `folder`, `projects`, and `format`, enforcing `CLI flags > environment variables (CHROME_PATH, XDG_CONFIG_HOME) > config.yaml > built-in defaults` with scope replacement and `project_aliases` overlay semantics.
    *   [cmd/gcp-arch/preflight.go](../../cmd/gcp-arch/preflight.go): Invokes `internal/extract.RunPreflight` (including `cai.folders` on `--org`/`--folder` scopes and `--vertex-project`, `--vertex-location`, `--model` readiness probes wiring `appDeps.probeVertexModel` to `VertexPreflightOptions.ModelProbe`, with `probeVertexModelREST` fallback when `ModelProbe` is `nil`).
    *   [cmd/gcp-arch/extract.go](../../cmd/gcp-arch/extract.go): Implements Stage 1 extraction, keeping actionable out-of-scope target hints and degraded coverage warnings on `stderr` even under `-q`/`--quiet`.
    *   [cmd/gcp-arch/synthesize.go](../../cmd/gcp-arch/synthesize.go): Implements Stage 2 Day-1 synthesis and Day-2 `--reconcile`, including `--thinking-level`, lazy Vertex client initialization, canonical byte comparison (`manifestsEqualInPlace`), and atomic `manifest.Write`.
    *   [cmd/gcp-arch/validate.go](../../cmd/gcp-arch/validate.go): Implements offline structural (`ValidateStructure`) and `--facts` provenance/accounting (`ValidateWithFacts`) validation with `--json` output.
    *   [cmd/gcp-arch/render.go](../../cmd/gcp-arch/render.go): Implements Stage 3 offline rendering, passing `--chrome-path` and `config.yaml` `chrome_path` separately to `appDeps.newBrowserRunner(chromePath, configChromePath)` and `render.RenderOptions.ConfigChromePath` (failing fast with exit code `1` on missing `--chrome-path` for all formats including `--format html` alone, falling through missing `CHROME_PATH` / `config.yaml` `chrome_path` to `PATH` and OS paths, and invoking the Chrome-free `--format html` fallback with `stderr` notice only when auto-discovery fails without an explicit `--chrome-path`).
    *   [cmd/gcp-arch/main_test.go](../../cmd/gcp-arch/main_test.go) and [cmd/gcp-arch/e2e_test.go](../../cmd/gcp-arch/e2e_test.go): Tests flag/config validation, `--project` alias, `--thinking-level`, `--version`, `--json` output on `preflight`/`validate`, full offline `extract -> synthesize -> validate -> render` pipeline, Day-2 `--reconcile`, and exit-code `0`/`1`/`2` matrix.

#### 4.2 Retired v2 Package, Fixture, and Example SVG Deletion

*   **Deleted retired v2 `internal/compiler/` (18 files):** `compact.go`, `compact_finalize.go`, `compiler_test.go`, `decisions.go`, `edges.go`, `edges_empirical.go`, `edges_index.go`, `edges_test.go`, `edges_tier1.go`, `edges_tier1_services.go`, `explain.go`, `helpers.go`, `passes_ai.go`, `passes_compute.go`, `passes_data.go`, `passes_messaging.go`, `passes_network.go`, `placement.go`
*   **Deleted retired v2 `internal/layout/` (13 files):** `cycles.go`, `elk.go`, `elk_l0.go`, `elk_l2.go`, `elk_post.go`, `layout_test.go`, `predicates.go`, `textwrap.go`, `views.go`, `views_l0.go`, `views_l1.go`, `views_l2.go`, `viewspec.go`
*   **Deleted retired v2 `internal/drawio/` (4 files), `internal/svg/` (2 files), `internal/explore/` (5 files), `internal/theme/` (2 files):** `internal/drawio/{drawio_test.go,render.go,styles.go,xmlutil.go}`, `internal/svg/{render.go,render_test.go}`, `internal/explore/{server.go,server_test.go,assets/{app.css,app.js,index.html}}`, `internal/theme/{palette.go,palette_test.go}`
*   **Deleted retired v2 `internal/ir/` (4 files), `internal/catalog/` (5 files), `internal/ingest/` (6 files):** `internal/ir/{canonical.go,model.go,model_test.go,predicates.go}`, `internal/catalog/{catalog.go,catalog_test.go,svg.go,assets/{base_catalog.yaml,icons.yaml}}`, `internal/ingest/{cai.go,client.go,enrichers.go,ingest_test.go,sanitize.go,snapshot.go}`
*   **Deleted retired v2 Fixtures (2 files):** `testdata/enterprise_reference.snap.json`, `testdata/org_hub_v2.snap.json`
*   **Deleted retired v2 Example SVGs (9 files):** `enterprise-l0-governance.svg`, `enterprise-l1-shared-vpc-hub.svg`, `enterprise-l2-app-prod-audit.svg`, `enterprise-l2-app-prod.svg`, `enterprise-l2-coupled-app-data.svg`, `enterprise-l2-data-prod.svg`, `org-hub-l0-governance.svg`, `org-hub-l1-net-hub-prod.svg`, `org-hub-l2-coupled-retail.svg`

#### 4.3 Dependencies, `.gitignore`, Reference Examples, Repository CI/Quality Automation, and `README.md`

*   **[go.mod](../../go.mod) and [go.sum](../../go.sum):** Pruned direct dependencies `github.com/d2lang/elk-go`, `github.com/beevik/etree`, and direct `cloud.google.com/go/accesscontextmanager` via `go mod tidy`; `cloud.google.com/go/accesscontextmanager` remains solely as an `// indirect` transitive dependency pulled by `cloud.google.com/go/asset`.
*   **[.gitignore](../../.gitignore):** Removed legacy `/.gcp-arch/`, `*.snap`, `*.snap.gz`, `*.snap.json`, `!testdata/**/*.snap.json`, and `*.drawio`; retained `.gcp-arch-*.tmp` and `*.bak`; added `facts.json`, `*.facts.json`, `!testdata/**/*.facts.json`, and `out/`.
*   **Reference Example Bundles ([examples/README.md](../../examples/README.md)):**
    *   `examples/platform-cicd/`: [architecture.yaml](../../examples/platform-cicd/architecture.yaml), [architecture.html](../../examples/platform-cicd/architecture.html), [architecture.pdf](../../examples/platform-cicd/architecture.pdf), and `3x` view PNGs ([00-view_0_system_index.png](../../examples/platform-cicd/views/00-view_0_system_index.png), [01-cicd_delivery_pipeline.png](../../examples/platform-cicd/views/01-cicd_delivery_pipeline.png), [02-gke_developer_platform.png](../../examples/platform-cicd/views/02-gke_developer_platform.png), [03-shared_network_psa.png](../../examples/platform-cicd/views/03-shared_network_psa.png)).
    *   `examples/retail-cloud-native/`: [architecture.yaml](../../examples/retail-cloud-native/architecture.yaml), [architecture.html](../../examples/retail-cloud-native/architecture.html), [architecture.pdf](../../examples/retail-cloud-native/architecture.pdf), and `3x` view PNGs ([00-view_0_system_index.png](../../examples/retail-cloud-native/views/00-view_0_system_index.png), [01-data_ai_pipeline.png](../../examples/retail-cloud-native/views/01-data_ai_pipeline.png), [02-shared_vpc_connectivity.png](../../examples/retail-cloud-native/views/02-shared_vpc_connectivity.png), [03-storefront_runtime.png](../../examples/retail-cloud-native/views/03-storefront_runtime.png)).
*   **Repository CI, Quality, Security & Release Automation:**
    *   [.github/workflows/ci.yml](../../.github/workflows/ci.yml): `gofmt`, `go vet`, `golangci-lint`, race-enabled `go test -v -race -coverprofile=coverage.out -covermode=atomic ./...`, `CGO_ENABLED=0 go build`, and SonarQube Cloud analysis.
    *   [.github/workflows/codeql.yml](../../.github/workflows/codeql.yml): CodeQL security-and-quality scanning for Go and GitHub Actions.
    *   [.github/workflows/security.yml](../../.github/workflows/security.yml): `govulncheck`, PR dependency review, and `zizmor` workflow security checks.
    *   [.github/workflows/release.yml](../../.github/workflows/release.yml): Cross-platform `CGO_ENABLED=0` release builds injecting `-ldflags "-s -w -X main.version=${VERSION}"`.
    *   [.github/dependabot.yml](../../.github/dependabot.yml): Weekly `gomod` and `github-actions` update PRs with 7-day cooldown.
    *   [.golangci.yml](../../.golangci.yml): `golangci-lint` v2 linter (`bodyclose`, `errcheck`, `errorlint`, `gocognit`, `gocritic`, `govet`, `ineffassign`, `misspell`, `revive`, `staticcheck`, `unused`) and `gofmt` configuration.
    *   [sonar-project.properties](../../sonar-project.properties): SonarQube Cloud project, coverage, and exclusion configuration.
*   **[README.md](../../README.md):** Documents the 5-command pipeline, `--version`, `~/.config/gcp-arch/config.yaml` defaults, `--thinking-level`, separated target (`cloud-platform.read-only`) vs. `--vertex-project` (`roles/aiplatform.user`) credentials, `cai.folders` preflight and Vertex AI `PublisherModel` metric synthesis, `<= 500` active-resource ceiling, `architecture.yaml` schema, Day-2 `--reconcile`, offline vs. `--facts` validation, and Chrome/Chromium setup.

### Verification Plan

*   **Compilation & Dependency Pruning:** `go mod tidy && git diff --exit-code go.mod go.sum && ! grep -E 'elk-go|beevik/etree' go.mod go.sum && awk '/accesscontextmanager/ && !/\/\/ indirect/ { exit 1 }' go.mod && CGO_ENABLED=0 go build -o ./bin/gcp-arch ./cmd/gcp-arch`
*   **Unit & Concurrency Race Suite:** `go test -race -count=1 ./...`
*   **Static Analysis:** `go vet ./...` (plus `golangci-lint` in [.github/workflows/ci.yml](../../.github/workflows/ci.yml))
*   **Per-Package Test Coverage Gate:** `>= 85%` statement coverage in **each** package under `./internal/...` ([internal/ioutil](../../internal/ioutil), [internal/icons](../../internal/icons), [internal/extract](../../internal/extract), [internal/manifest](../../internal/manifest), [internal/synth](../../internal/synth), [internal/render](../../internal/render)), verified via `go test -cover ./internal/...`.
*   **Automated Offline End-to-End Acceptance ([cmd/gcp-arch/e2e_test.go](../../cmd/gcp-arch/e2e_test.go) over [testdata/acme_cicd_v3.facts.json](../../testdata/acme_cicd_v3.facts.json)):**
    *   Assert 100% `FactsFile` resource accounting via `manifest.ValidateWithFacts` across `components[].backed_by`, `containers[].backed_by`, and `excluded_resources`, and assert `render` runs offline without `facts.json` and emits no Inventory Coverage Table;
    *   Assert `VERIFIED` directed-walk provenance on `github-acme -> Cloud Build -> Artifact Registry acme-apps`, `HTTPS LB -> GKE cicd -> Atlantis -> acme-tfstate & acme-private-modules`, and `SonarQube -> Cloud SQL sonarqube`;
    *   Assert zero unclassified K8s/backup blocks and zero `terraform plan` false service cards;
    *   Assert `< 100 ms` `0`-LLM fast path on unchanged `--reconcile` and `notes` preservation on delta `--reconcile`;
    *   Assert `0` card/badge/pill/collinear overlaps in headless Chrome across all 4 `acme-cicd` views, exit code `2` on injected card/collinear collision, valid `%PDF-` header with `/Type /Page` count matching `len(views)` (`4` pages, no Inventory Coverage Table sheet) and non-empty text streams in `out/architecture.pdf`, and `3x` (`3120px` width) PNG dimensions in `out/views/*.png`.
*   **Optional Manual Live-GCP Verification:** Manual verification with live GCP/Vertex credentials (`gcp-arch preflight --projects <id> --vertex-project <id> && gcp-arch extract --projects <id> -o facts.json && gcp-arch synthesize facts.json --vertex-project <id> -o architecture.yaml && gcp-arch validate architecture.yaml --facts facts.json && gcp-arch render architecture.yaml -o out/`), confirming every goal criterion end-to-end on live cloud infrastructure.

--------------------------------------------------------------------------------

<!-- Provenance: Indicates initial structural drafting via EGM. -->

*Drafted with EGM*
