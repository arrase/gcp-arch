# Hierarchical Multi-Level Visual Explorer and Report Diagram Exporter (`gcp-arch`)

*   **Author:** Juan Ezquerro LLanes
*   **Status:** Superseded
*   **Date:** 2026-11-20

> [!WARNING]
> **Superseded Design (v2):** This v2 snapshot, loopback visual explorer, and `elk-go` SVG exporter architecture has been superseded and retired from the codebase (retaining only [internal/ioutil/atomic.go](../../internal/ioutil/atomic.go)). See [semantic-multi-view-pipeline.md](semantic-multi-view-pipeline.md) for the active, implemented v3 semantic multi-view pipeline. The historical content below is preserved for reference only.

--------------------------------------------------------------------------------

## Overview

`gcp-arch` is a standalone static Go command-line tool (`CGO_ENABLED=0`) that inspects live Google Cloud Platform (GCP) environments through read-only credentials and compiles architecture diagrams for engineers and cloud consultants preparing client audit and handoff reports. In its current implementation, `gcp-arch` writes every discovered resource across all scanned projects into a single two-tab Draw.io (`.drawio`) file and relies on a 3-way XML merger to preserve manual coordinate edits across reruns. That model fails in three ways: sibling cards sort by hash into a tall left-hand column that inverts forward event pipelines, enterprise organizations with 300 to 1,500+ projects collapse into a single unreadable sheet containing thousands of boxes, and live `--org` collection aborts whenever any child project has a disabled API.

Because `gcp-arch` is in pre-release development with zero production users, backward compatibility with existing `.drawio` files, CLI flags, and `v1` snapshots is not required. This design replaces the flat two-tab `.drawio` generator and its 3-way XML merger with a snapshot-first workflow backed by one pure-Go layout and Scalable Vector Graphics (`.svg`) rendering core. Engineers capture an organization once into a local `v2` snapshot file (`.snap`), browse and curate the architecture locally in a web browser via `gcp-arch explore`, and export bounded views across three zoom levels—Organization and Folder governance (`L0`), Network Hubs (`L1`), and scoped `1..K` Project or Workload diagrams (`L2`)—as uniform `.svg` or stateless `.drawio` deliverables from the CLI and `.svg`, high-resolution `2x`/`3x` `.png`, or `.drawio` deliverables from `gcp-arch explore`.

## The Problem

### Objectives

Cloud engineers and external consultants auditing a client's Google Cloud environment must turn hundreds of projects and thousands of underlying resources into page-sized architecture diagrams suitable for written reports and slide decks. `gcp-arch` needs to scan a large organization once without aborting on partial permissions or disabled child-project APIs, and let the engineer navigate from an organization directory map down into shared networks and individual application workloads in a local browser. Within that browser workflow, the engineer must be able to confirm uncertain Secret Manager or multi-target connections with one click and export diagrams bounded to at most 40 main-canvas boxes per view with consistent left-to-right data flow.

### State of the Art and Current Limits

Today, `gcp-arch` collects resources from Google Cloud Asset Inventory (CAI) and four read-only metadata enrichers: BigQuery SQL job history, Dataflow pipeline descriptions, Cloud Monitoring API traffic metrics, and Vertex AI Retrieval-Augmented Generation (RAG) corpora. It collapses low-level child objects into composite service cards via 15 compaction passes and infers three tiers of directed edges: `Tier 1` verified structural pointers and SQL lineage, `Tier 2` `1:1` active API metric correlations, and `Tier 3` ambiguous multi-target or Secret Manager candidates held back from automatic drawing. Finally, it writes one two-tab `.drawio` workbook (`L1-Platform-Network` and `L2-Architecture`).

While this pipeline works on isolated 1–2 project demos, four limits prevent its use on enterprise engagements:

1.  **Unbounded canvas density past ~40 nodes:** Rendering 300 to 1,500+ projects onto one `L1` tab and one `L2` tab produces thousands of cards on a single sheet, shrinking text below legibility on PDF/A4 pages and stalling Draw.io's editor. Splitting an organization into one `.drawio` tab per project likewise produces hundreds of tabs with no search or cross-project navigation.
2.  **Live `--org` collection failures and unbounded heap growth:** During organization-wide scans, `gcp-arch` treats any `SERVICE_DISABLED` API error on a child project as a fatal caller quota-project error (`exit 1`) and runs BigQuery and Dataflow job queries sequentially across projects where those APIs are disabled. It also exhausts its 50-call Vertex AI regional cap on auto-mode VPC subnets in the first two projects, retains ignored plumbing assets (`Firewall`, `Route`, `Policy`) in memory, and discards `Organization` and `Folder` display names.
3.  **Hash-driven ordering and broken network containment on `L2`:** Current `L2` layout sorts sibling cards by truncated `SHA-256` cell identifiers, stacking unlinked resources (`degree == 0`) into a vertical wall in the leftmost column alongside internet ingress, reversing forward event pipelines (`Cloud Storage -> Eventarc -> Cloud Run`), and obscuring Shared VPC workloads where a central Host Project lends subnets to application Service Projects.
4.  **Incompatible styling and brittle 3-way `.drawio` merges:** Confirming `Tier 3` Secret Manager links today requires opening Draw.io and drawing arrows by hand, which makes curated diagrams look visually different from unedited exports and breaks whenever dynamic card folding changes parent container coordinates across snapshots. Meanwhile, presentation exports include internal `Unresolved Links: N` audit badges and an unbounded `CoverageLedger` box (the per-scope API collection and permission-degradation status table, reaching `~6,000` lines at 1,500-project scale).

### New Approach

This design restructures `gcp-arch` around four coordinated changes:

1.  **Hardened `v2` snapshot collection decoupled from rendering:** `gcp-arch snapshot` captures a sanitized `SnapshotSchemaVersion = "v2"` `.snap` file containing `Organization` and `Folder` display names, Shared VPC Host and subnet IAM bindings, stream-dropped plumbing records (`< 512 MB` post-GC Go live heap), caller-scoped `SERVICE_DISABLED` handling, and concurrent rate-limited enrichers gated on enabled project services. All downstream commands (`views`, `explore`, `export`, `inspect`) operate offline against the `.snap` file without GCP API calls (`< 500 ms` warm view projection, `< 1 s` cold CLI export).
2.  **Symmetric `<= 40`-box hierarchical views (`L0`, `L1`, `L2`):** Instead of rendering every discovered resource onto one sheet, a stateless view specification (`ViewSpec`) slices the compiled graph into bounded views (`MaxViewBoxes = 40`):
    *   `L0` (*Organization & Folder Governance*): Tree-and-grid map of Organizations, Folders, VPC Service Controls (`VPC-SC`, organization data-exfiltration perimeters) coverage badges, project counts, cohort summary cards for flat folders exceeding 25 active projects, and deterministic tail rollups when folder or cohort counts exceed 40.
    *   `L1` (*Network Hub Topology*): One view per Shared VPC Host, Network Connectivity Center (`NCC`, Google Cloud's transit hub connecting VPC spokes, VPNs, and Interconnects), or multi-subnet hub VPC showing regional subnets, unused-subnet summaries, peering/hybrid links, and folder-grouped service project fleet cards within the 40-box budget.
    *   `L2` (*Project / Workload Architecture*): Scoped to `1..K` projects (with workload cohort folding, hub-detachment component slicing, or capped `k-hop` neighborhood slicing for dense monoprojects). `L2` frames VPC and borrowed Shared VPC subnet workloads in an upper band above project-scoped managed global services (`BigQuery`, `Cloud Storage`, `Pub/Sub`), flows strictly left-to-right from upstream callers through compute and messaging to data/AI stores and downstream targets, and collapses unlinked inventory into a toggleable compact footer band.
3.  **Single pure-Go SVG layout and rendering core:** One `CGO_ENABLED=0` Go pipeline computes multi-line text wrapping from embedded font advance metrics prior to layout, runs `elk-go` (`v0.2.0`) paired with deterministic Go corridor/gutter geometry, and emits canonical `.svg` markup with deduplicated inline `<symbol>` service icons. That identical layout drives CLI `.svg` and stateless `.drawio` exports as well as the interactive viewport of `gcp-arch explore` (served from Go `embed.FS` on loopback with zero Node.js/npm build toolchain, using browser HTML5 `<canvas>` for `2x`/`3x` `.png` report downloads).
4.  **Upstream `Tier 3` curation replacing 3-way `.drawio` XML merging:** The 3-way `.drawio` merger (`internal/drawio/merge.go` and `merge_reconcile.go`, `1,283` lines) is removed. Instead, `gcp-arch explore` surfaces `Tier 3` candidate links (`Node.UnresolvedCandidates`) as a 1-click confirmation checklist persisted to `.gcp-arch/decisions.yaml`. Confirming a candidate promotes it to a rendered edge *before* layout runs—pulling stacked target buckets or datasets out of folded footer stacks into the main flow—so every diagram across `.svg`, `.png`, `explore`, and `.drawio` shares identical geometry and clean presentation defaults.

### Impact

Consultants and platform engineers can audit a 1,500-project GCP organization from a single static binary and navigate its folder hierarchy and Shared VPC hubs locally in `< 500 ms` per warm view without uploading client metadata to third-party SaaS tools. Teams curate Secret Manager and shared-Service-Account edges once in `.gcp-arch/decisions.yaml` without reconciling 3-way Draw.io XML merges, and drop uniform, page-fitted `.svg` and high-DPI `.png` figures directly into Word, PowerPoint, Google Docs, Confluence, or Markdown reports while retaining stateless one-way `.drawio` export for optional freehand post-editing.

### Risks and Mitigations

*   **Cross-Band Orthogonal Edge Collisions After Vertical Band Separation on `L2`:** Separating upper `VPC / Delegated Subnet` cards from lower `Managed / Global` cards can cause multi-column or cross-band edges to cut across synthesized container title bars if edges are interpolated blindly across shifted coordinates. *Mitigation:* Use `elk-go` strictly to establish DAG-monotonic column assignments and intra-column vertical barycenters on a single-component `topGraph`, synthesize exact `ContainerBox` frames around Y-striped bands in Go while reserving explicit inter-column wall gaps and horizontal band gutters outside container headers, and route orthogonal corridor/gutter channels in Go verified by automated zero-intersection geometry tests.
*   **Loopback Browser Exposure (`DNS Rebinding` and Local Multi-UID Access) in `gcp-arch explore`:** Starting a local HTTP server over sensitive client cloud topology risks cross-site requests, DNS-rebinding attacks from untrusted browser tabs, or token scraping by other local users. *Mitigation:* Bind strictly to a loopback address (`127.0.0.1` by default), validate the HTTP `Host` header against the bound `<loopback-host>:<port>`, set `Cross-Origin-Resource-Policy: same-origin` and strict `Content-Security-Policy` without `CORS` headers, and bootstrap a per-process 32-byte random token via URL fragment (`#token=<hex>`, never transmitted in HTTP requests or unauthenticated HTML) required in `X-GCP-Arch-Token` on all `/api/*` endpoints.
*   **Silent Hiding of Audited Resources Inside `<= 40`-Box Folded Cards:** Collapsing empty projects, unused subnets, large flat folders, or unlinked `degree == 0` storage buckets to meet the 40-box visual budget could hide a specific resource an auditor needs to verify. *Mitigation:* Enforce strict empty-project and unused-subnet predicates (accounting for LB proxy-only subnets, VPC connectors, Private Service Connect `natSubnets`, internal reservations, subnet `networkUser` IAM bindings, and non-default KMS/security assets), omit destructive node-hiding overrides from `.gcp-arch/decisions.yaml`, retain every folded member's canonical URI for `explore` search, provide a UI/CLI toggle to expand standalone stacks, and add stateless `gcp-arch inspect <snap> --explain <uri>` provenance lookup.
*   **Browser `<canvas>` SVG Rasterization Quirks Without `CGO`:** Keeping `CGO_ENABLED=0` means high-DPI `.png` export relies on browser HTML5 `<canvas>`, where missing root `<svg>` pixel dimensions fail in Firefox, nested base64 `<image>` tags decode asynchronously as blank squares, and oversized `3x` canvases hit browser pixel ceilings. *Mitigation:* Emit explicit numeric `width` and `height` attributes on the root `<svg>`, inline deduplicated catalog icons as native `<symbol>` definitions with scoped internal `<filter>`/`<mask>` IDs so `img.onload` rasterizes synchronously in one document, clamp `<canvas>` scale factors to browser pixel limits, and keep CLI `gcp-arch export` pure-Go (`--format svg|drawio`).
*   **Partial-Permission Blind Spots on Folder/VPC-SC Rollups:** In restricted corporate Landing Zones, missing `roles/resourcemanager.folderViewer` or `roles/accesscontextmanager.policyReader` prevents resolving folder names or full perimeter membership. *Mitigation:* Render unresolved folder ancestors explicitly as `Folder <ID> (unverified name)`, draw enforced VPC-SC red dashed borders on `Folder Summary Cards` only when 100% of active descendant projects belong to verified enforced perimeters (showing `VPC-SC: X/Y projects` when `0 < X < Y`), and surface only view-relevant non-`OK` degradations when `--audit-overlays` is enabled.

### Cost and Timeline

*   **Infrastructure and Licensing Cost:** USD 0 in cloud licensing, SaaS fees, or generative AI token usage. Collection uses free-tier read-only GCP API quotas on the engineer's workstation within `< 512 MB` post-GC Go live heap (`HeapAlloc`), and `explore` runs locally with zero external network calls.
*   **Implementation Cost and Timeline:** One software engineer across five incremental phases (~4–5 weeks), keeping Phases 1–4 additive so `go build ./...` and `go test -race ./...` pass at every phase boundary before the atomic Phase 5 CLI cutover:
    *   *Phase 1 (Week 1):* Snapshot `v2` schema, catalog plumbing/VPN/SSL-proxy rules, org-scale ingestion hardening (caller/target-scoped `ClassifyGCPError`, `EnabledServices`-gated concurrent rate-limited enrichers with BigQuery field masks, safe stream-dropped plumbing, `Organization`/`Folder`/`XPNProjectStatus`/`networkUser` capture, mutex-throttled `stderr` progress), and `v2` test fixtures.
    *   *Phase 2 (Week 2):* Compiler `ZoneAssignment` truth table + `VPCConnectorURI` retention, `.gcp-arch/decisions.yaml` `confirmed_edges` overlay, stateless `ExplainURI`, strict empty/unused predicates, and additive `ViewSpec` projectors + `<= 40`-box budget slicers for `L0`, `L1`, and `L2`.
    *   *Phase 3 (Week 3):* Embedded proportional/monospace font-metric `<tspan>` wrapper, additive `L0` two-phase `mrtree`+grid layout, `L1` hub layout, and `L2` 1-pass `elk-go` + Go Y-band striping, `ContainerBox` synthesis, and corridor/band-gutter orthogonal router with geometry invariant tests.
    *   *Phase 4 (Week 4):* Shared visual theme/atomic-write utilities and canonical pure-Go SVG emitter (deduplicated inline `<symbol>` icons, presentation vs. `--audit-overlays` modes).
    *   *Phase 5 (Week 5):* Loopback-hardened `embed.FS` browser explorer (`gcp-arch explore` with `<canvas>` `2x`/`3x` `.png` export), atomic retirement of `internal/drawio/merge*.go` and `cmd/gcp-arch/generate.go`, stateless single-view `.drawio` exporter, new CLI surface (`snapshot`, `views`, `explore`, `export`, offline `inspect --explain`), documentation updates, and end-to-end exams.

### Metrics and Verification Exams

*   **Mid-Term Exam (1,500-Project Ingestion & View Budget Compliance):**
    *   Ingest and compile a synthetic 1,500-project organization fixture (including disabled child-project APIs, auto-mode VPCs, 150-project flat folders across `> 40` prefixes, a 200-spoke Shared VPC Host, and an 85-node shared-backbone monoproject) within `< 512 MB` post-GC Go `HeapAlloc` and zero fatal exits on child-project `SERVICE_DISABLED` errors.
    *   Verify that every generated default `L0`, `L1`, and `L2` view obeys `MaxViewBoxes = 40` (`len(Containers) + len(Cards) <= 40` on `L0`/`L1`; `len(Cards) <= 40` on `L2`) while accounting for 100% of architectural resources via direct cards, cohorts, or stateless `inspect --explain` lookup.
*   **Final Exam (Layout Geometry Invariants, Curation & Multi-Format Parity):**
    *   Pass automated geometry assertions across the `v2` enterprise reference fixture and stress views: `len(ViewGraph.Edges) == len(LayoutedPage.Edges)`, strictly increasing X coordinates along forward DAG chains (`Storage -> Eventarc -> Cloud Run -> SQL`), disjoint `BandVPC` and `BandManaged` `ContainerBox` envelopes, and zero non-adjacent edge intersections with cards or container title bars.
    *   Confirm a `Tier 3` Secret Manager candidate via `POST /api/decisions` into `.gcp-arch/decisions.yaml` and verify in `< 500 ms` that the target bucket unfolds out of its `Standalone` footer stack into the main `L2` Data column with identical `LayoutedPage` geometry across `.svg`, `explore`, and `.drawio`.
    *   Produce valid XML/SVG/Draw.io outputs with numeric root `<svg width height>` attributes, inline `<symbol>` icons, zero `<foreignObject>` tags, zero leaked `Unresolved Links` badges in presentation mode, and `403` rejection of cross-origin, forged-`Host`, or missing-`X-GCP-Arch-Token` requests on `explore` `/api/*`.

## Goals

*   **Org-Scale `v2` Snapshot Collection:** Collect `300–1,500+` GCP projects into a sanitized `0600` `SnapshotSchemaVersion = "v2"` `.snap` file within `<= 80` CAI RPM and `< 512 MB` post-GC Go `HeapAlloc`, preserving `Organization` and `Folder` display names, project aliases, Shared VPC Host/IAM metadata, and non-fatal handling of child-project disabled APIs.
*   **Bounded `<= 40`-Box Readability Across `L0`, `L1`, and `L2`:** Guarantee default views emit at most 40 rendered containers/cards on `L0` and `L1` and at most 40 main-canvas cards on `L2` (`MaxViewBoxes = 40`, excluding compact footer groups and legend) via `L0` Folder Summary/Cohort Cards and tail rollups, `L1` per-hub views with Service Project Fleet Cards, and `L2` `1..K` workload scopes with workload cohorts and hub-detachment / `k-hop` slicing.
*   **Google Cloud Architecture Center Visual Hierarchy on `L2`:** Enforce strictly monotonic left-to-right architectural flow (`Upstream External -> Ingress -> Compute/Orchestration -> Messaging -> Data/Storage -> Consumption/AI -> Downstream External`) and strict vertical separation between upper `VPC / Delegated Shared VPC Subnet` frames and lower project-scoped `Managed / Global` rows using semantic tie-breaking instead of `SHA-256` hashes.
*   **Single `CGO_ENABLED=0` Go SVG Core Across CLI and Browser Explorer:** Render identical `LayoutedPage` geometry and pre-wrapped `<tspan>` text across CLI `.svg` exports, stateless `.drawio` exports, and `gcp-arch explore` (`< 500 ms` warm view projection and `< 1 s` cold CLI export from `.snap`), with synchronous `2x`/`3x` `.png` export via browser `<canvas>`.
*   **Upstream `Tier 3` Curation and Clean Presentation Defaults:** Replace stateful 3-way `.drawio` XML merging with `.gcp-arch/decisions.yaml` (`confirmed_edges`) and 1-click `Tier 3` confirmation in `gcp-arch explore`, omitting `Unresolved Links` badges and `CoverageOK` (non-degraded API status) rows from presentation diagrams unless `--audit-overlays` is requested.

## Non-Goals

*   **Generative AI, Paid GCP Tiers, or Payload Inspection:** `gcp-arch` will not call LLMs, require Security Command Center Premium or VPC Flow Logs, or read Secret Manager secret versions, Cloud Storage objects, or BigQuery table data.
*   **Browser Freehand Canvas Editing or Frontend Build Toolchains:** `gcp-arch explore` is a read/curate topological navigator and exporter, not a freehand box-dragging editor, and will not introduce Node.js, npm, TypeScript, or bundler dependencies.
*   **Stateful 3-Way `.drawio` Coordinate Merging, Node Redaction, or Legacy Compatibility:** Manual coordinate edits made inside Draw.io will not merge back into subsequent snapshots, `.gcp-arch/decisions.yaml` will not support hiding audited workloads from views, and legacy `v1` snapshots or `gcp-arch generate` flags are not preserved.
*   **External CLI Browser Dependencies or Multi-User Web Hosting:** CLI `gcp-arch export` will not shell out to external Chromium/Chrome processes (`2x`/`3x` `.png` rasterization lives in `gcp-arch explore`), and `explore` runs strictly as a single-user loopback workstation server.

## Open Questions

*   None. All scope, CLI taxonomy, snapshot `v2` cutover, and `L2` 1-pass `elk-go` + Go corridor/gutter routing decisions are resolved.

## Critical User Journeys (CUJs)

### CUJ 1: Enterprise Organization Discovery and Hierarchical Drill-Down (`L0 -> L1 -> L2`)

*   **Given:** A consultant holds read-only Application Default Credentials (`ADC`: `roles/cloudasset.viewer`, `roles/resourcemanager.folderViewer`, `roles/monitoring.viewer`, `roles/bigquery.resourceViewer`) across a 600-project GCP organization (`org-acme`) where 400 projects have Dataflow disabled and 120 sandbox projects contain only default `_Default` log buckets, default service accounts, and untouched `default` VPCs.
*   **When:** The consultant runs `gcp-arch preflight --org 123456789012`, captures an offline inventory via `gcp-arch snapshot --org 123456789012 -o acme.snap`, and launches `gcp-arch explore acme.snap`.
*   **Then:**
    *   `snapshot` streams rate-limited progress to `stderr`, skips disabled per-project APIs without aborting, drops ignored plumbing records on stream within `< 512 MB` post-GC `HeapAlloc`, and writes `acme.snap` (`0600`).
    *   `explore` binds to `127.0.0.1`, prints its `#token=<hex>` loopback URL, and renders the `L0` Organization & Folder tree (`<= 40` boxes) in `< 500 ms`, showing resolved folder display names (`prod`, `non-prod`, `platform`), collapsing the 120 empty sandbox projects into a compact badge, and badging `prod` with its Shared VPC attachment count and VPC-SC perimeter status.
    *   Clicking the `platform/net-hub-prod` badge transitions to `L1` for that Shared VPC Host (`<= 40` boxes), showing regional subnets, summarizing unused subnets, and grouping 85 attached service projects into `Folder Attachment Fleet Cards`; clicking `prod/retail` filters `L0` to that folder, clicking `retail-app-prod` opens its `L2` diagram, and clicking its outbound `retail-data-prod` external project stub expands `projects=retail-app-prod,retail-data-prod` into one coupled left-to-right `L2` diagram.

### CUJ 2: One-Click `Tier 3` Secret Manager Curation and Uniform Report Export

*   **Given:** Inside `gcp-arch explore acme.snap` on the `retail-app-prod` `L2` view, an `orders-api` Cloud Run service reads database and storage names from Secret Manager (`Tier 3`), so `orders-archive-bkt` currently sits folded inside a footer card (`Standalone Cloud Storage ×14`) with no automatic arrow.
*   **When:** The consultant clicks `orders-api`, inspects its `Tier 3` candidate checklist in the side panel, confirms the link to `orders-archive-bkt` (updating `.gcp-arch/decisions.yaml`), and clicks `Export PNG (2x)` and `Export SVG`.
*   **Then:**
    *   Confirming the candidate updates `.gcp-arch/decisions.yaml` and re-renders the view in `< 500 ms`: `orders-archive-bkt` unfolds out of `Standalone Cloud Storage` (`×14 -> ×13`) into the `Data/Storage` column of `retail-app-prod` with an orthogonal connector from `orders-api`.
    *   Both the downloaded `.svg` and `2x` `.png` files match the browser viewport geometry (rendering crisp inline `<symbol>` service icons with zero blank squares or `<foreignObject>` tags) and omit internal `Unresolved Links` and `CoverageOK` audit overlays so they drop directly into the client report.

### CUJ 3: Headless CLI Batch Export and Resource Audit Explanation

*   **Given:** `.gcp-arch/decisions.yaml` and `acme.snap` are checked into an engagement workspace, and a reviewer asks both for batch SVG/Draw.io diagrams of key views and why a specific subnet (`projects/net-hub-prod/regions/us-central1/subnetworks/Unused-Lab-04`) does not appear expanded on `L2`.
*   **When:** The consultant runs `gcp-arch views acme.snap --decisions .gcp-arch/decisions.yaml`, exports views via `gcp-arch export acme.snap --level l2 --project retail-app-prod,retail-data-prod --decisions .gcp-arch/decisions.yaml --format svg -o reports/retail-l2.svg` (and `--format drawio -o reports/retail-l2.drawio`), and runs `gcp-arch inspect acme.snap --explain //compute.googleapis.com/projects/net-hub-prod/regions/us-central1/subnetworks/Unused-Lab-04`.
*   **Then:**
    *   `views` lists discovered `L0`, `L1` hub, and `L2` project view specs with exact post-decision box counts; `export` deterministically writes byte-identical `.svg` and single-view `.drawio` files sharing identical `LayoutedPage` geometry.
    *   `inspect --explain` reports that `Unused-Lab-04` is a `PRIVATE` subnet ingested in `net-hub-prod` that matched zero attached workloads, GKE nodes, VPC connectors, Private Service Connect `natSubnets`, internal reservations, or subnet-level `roles/compute.networkUser` bindings, so it folded into `Unused Subnets` on `L1` (`xpn:net-hub-prod`) and was omitted from `L2` (`retail-app-prod`).

## Technical Plan (Architecture)

### 1. System Pipeline and Package Boundaries

`gcp-arch` is organized as a five-stage deterministic pipeline compiled into one static Go binary (`CGO_ENABLED=0`, Go 1.26):

```text
+---------------------------------------------------------------------------------------+
| 1. Org-Scale Ingest (`internal/ingest`)                                               |
|  - ADC Auth + Caller/Target Quota Guard + Token Buckets (CAI, BQ, DF, Mon, RAG)       |
|  - CAI ListAssets(RESOURCE, ACCESS_POLICY, scoped IAM_POLICY incl. Subnetwork)        |
|  - Stream extraction of Orgs, Folders, Projects (Shared VPC), Revisions & FoldedCount |
|  - Safe stream-drop IGNORED_PLUMBING + URL/env/SQL sanitization (< 512 MB HeapAlloc)  |
|  - EnabledServices-gated concurrent enrichers (BQ w/ Fields mask, DF, Mon, RAG)       |
|  - Atomic 0600 gzip JSON Snapshot v2 writer/loader (`gcp-arch snapshot`)              |
+-------------------------------------------+-------------------------------------------+
                                            | SnapshotFile v2 (RawInventory + Ledger)
                                            v
+---------------------------------------------------------------------------------------+
| 2. Compaction, Curation & Placement (`internal/catalog`, `internal/compiler`)         |
|  - Base YAML catalog + `.gcp-arch/catalog/*.yaml` overlays (inline SVG icons only)    |
|  - 15 semantic compaction passes + Three-tier edge resolution (`Tier 1..3`)           |
|  - `.gcp-arch/decisions.yaml` overlay: promote confirmed Tier 3 edges + sync badges   |
|  - Exhaustive `ZoneAssignment` truth table + stateless `ExplainURI` provenance query  |
+-------------------------------------------+-------------------------------------------+
                                            | ir.Graph
                                            v
+---------------------------------------------------------------------------------------+
| 3. View Discovery & `<= 40`-Box Slicing (`internal/layout`)                           |
|  - Shared stateless `ViewSpec` across CLI flags and `explore` URL query params        |
|  - Read-only empty-project & unused-subnet predicates                                 |
|  - L0 (Org/Folder cohorts), L1 (per-Hub + Fleet Cards), L2 (1..K projects / hub-cut)  |
|  - Unified compact footer band (`TransversalGroups` + toggleable `Standalone` stacks) |
+-------------------------------------------+-------------------------------------------+
                                            | ViewGraph (<= 40 main-canvas boxes)
                                            v
+---------------------------------------------------------------------------------------+
| 4. Multi-Level Pre-Wrapped Geometry (`internal/layout`)                               |
|  - Proportional/monospace font-advance `<tspan>` wrapper (`ComputeWrappedCardBox`)    |
|  - L0: Per-container 2D child grids + top-down `elk-go` `mrtree` (`MODEL_ORDER`)      |
|  - L1: Hub-and-spoke `elk-go` `layered` (`INCLUDE_CHILDREN`) layout                   |
|  - L2: 1-pass `elk-go` `layered` on single-component `topGraph` -> Go global Y-bands  |
|        + `ContainerBox` synthesis -> Go corridor & band-gutter orthogonal edge router |
+-------------------------------------------+-------------------------------------------+
                                            | LayoutedPage
                                            v
+---------------------------------------------------------------------------------------+
| 5. Canonical Rendering & Local Explorer (`internal/theme`, `svg`, `drawio`, `explore`)|
|  - Shared palette (`internal/theme`) + Pure-Go SVG emitter (`<symbol>`, `<tspan>`)    |
|  - Stateless one-way `.drawio` serializer (`merge.go` & `merge_reconcile.go` retired) |
|  - `gcp-arch explore` on loopback (`embed.FS` vanilla JS + `<canvas>` 2x/3x PNG)      |
|  - CLI: `preflight`, `snapshot`, `views`, `explore`, `export`, `inspect --explain`    |
+---------------------------------------------------------------------------------------+
```

### 2. Snapshot `v2` Schema and Org-Scale Ingestion Hardening (`internal/ir`, `internal/ingest`)

*   **Strict `v2` Snapshot Envelope:** `SnapshotSchemaVersion` advances from `"v1"` to `"v2"`. `LoadSnapshot` rejects any other `schema_version` with a typed schema error mapped to CLI exit code `2` (usage/schema error) and retains zero `v1` fallback code.
*   **Hierarchy, Shared VPC, Project Aliases, and Plumbing Counters in `RawInventory`:**
    *   `RawInventory` stores `Organizations`, `Folders` (`FolderID`, `DisplayName`, `ParentURI`), `Aliases` (`--project-alias <PROJECT_NUMBER=PROJECT_ID>` mappings bridging unreadable external project numbers to human-readable IDs), and `SuppressedPlumbingCounts` (dropped plumbing counts keyed by `AssetType`).
    *   `ProjectMeta` records `XPNProjectStatus` (`"HOST"` when `compute.googleapis.com/Project` marks a Shared VPC Host), preserved consistently across stream ingestion, project-number reconciliation, and compiler metadata merging.
    *   Stream sanitization extracts hierarchy and project records upfront and drops explicitly cataloged `IGNORED_PLUMBING` asset types (while preserving core Compute, GKE, IAM, VPN, and SSL-proxy resources) so post-GC Go `HeapAlloc` remains `< 512 MB` across 1,500 projects.
    *   CAI IAM collection adds `compute.googleapis.com/Subnetwork` bindings filtered to subnet-attachment roles (`roles/compute.networkUser` and custom `compute.subnetworks.use` roles), capturing Shared VPC subnet delegation even when Service Project workloads are scaled to zero.
*   **Caller- and Target-Scoped Quota Error Classification:** Error classification compares Google API error metadata against both the caller's `QuotaProject` and the target child project's ID and numeric `ProjectNumber`. Organization/folder CAI quota failures and caller `QuotaProject` `SERVICE_DISABLED` / `BILLING_DISABLED` errors abort with `exit 1`, whereas errors naming a scanned child workload project record non-fatal `CoverageServiceDisabled` entries so `--org` scans continue across partially enabled estates.
*   **`EnabledServices`-Gated Concurrent Rate-Limited Enrichers and Telemetry:**
    *   BigQuery, Dataflow, Monitoring, and Vertex AI RAG enrichers run concurrent per-project worker pools protected by dedicated token-bucket rate limiters (`429` exponential backoff) and skip projects whose `EnabledServices` list omits the corresponding API.
    *   BigQuery job listing applies a server-side response field mask so `Projection("full")` transfers only lineage metadata without multi-megabyte SQL text or query stage plans.
    *   Monitoring enrichment skips default-only sandbox projects and gates Vertex AI `PublisherModel` metric calls on `aiplatform.googleapis.com` (querying throughput only when invocations are non-zero); RAG corpus discovery ignores regions derived solely from `autoCreateSubnetworks == true` VPC subnets.
    *   CAI pagination and enrichers emit mutex-synchronized `stderr` progress lines throttled to at most 1 Hz (suppressed under `-q/--quiet`).

### 3. Upstream Curation (`decisions.yaml`), Strict Predicates, and `ViewSpec` Slicing (`internal/compiler`, `internal/layout`)

*   **Declarative Curation Overlay (`.gcp-arch/decisions.yaml`):**
    *   Schema (`schema_version: "v1"`, strict `KnownFields(true)` YAML decoding) contains solely `confirmed_edges`: a deterministic list of `{source_uri, target_uri, label}` rules promoting matching `Node.UnresolvedCandidates` (matching either absorbed child `TargetURI` or parent `TargetNodeID`) into `EdgeTierConfirmed` (`TIER1_CONFIRMED`) edges prior to view projection, removing promoted entries from `Node.UnresolvedCandidates`, and updating `"Unresolved Links"` counts on `Node.Badges`. Audited nodes are never hidden or deleted by `.gcp-arch/decisions.yaml`.
*   **Read-Only Empty-Project and Unused-Subnet Predicates:**
    *   *Empty Project:* `IsEmptyProject` returns `true` when a project is not a Shared VPC Host (`XPNProjectStatus != "HOST"`), owns zero `ARCHITECTURAL_NODE` workloads/datastores, owns zero non-default VPC networks or custom subnets, borrows zero Shared VPC resources, and holds zero non-default transversal security/KMS/IAM assets (ignoring `default` VPCs, `_Default`/`_Required` Cloud Logging buckets, and auto-created Google default/service-agent accounts).
    *   *Unused Subnet:* `IsUnusedSubnet` returns `true` only for `PRIVATE`-purpose subnets (exempting load-balancer proxy-only, Private Service Connect, and peer-migration `purpose` subnets) that match zero `ir.Node` `SubnetURI`s or `CollapsedAssets`, zero raw `vpcaccess.googleapis.com/Connector` `subnet.name` references, zero `ServiceAttachment` `natSubnets`, zero internal `Address` reservations, and zero subnet-scoped `roles/compute.networkUser` bindings. `L0` and `L1` Shared VPC attachment rollups additionally resolve `<PROJECT_NUMBER>` principals from Google-managed robot service agents across both Host `Project` and `Subnetwork` `networkUser` bindings.
*   **Unified `ViewSpec` and `MaxViewBoxes = 40` Slicing:**
    *   `ViewSpec` (`Level` `l0|l1|l2`, `RootURI`, `HubID`, `Projects []string`, `FocusURI`, `Hops int`, `Component int`, `ExpandStandalone bool`, `AuditOverlays bool`) is 100% format-agnostic across `.svg`, `.png`, `explore`, and `.drawio`, and encodes identically to URL query strings and `gcp-arch export` CLI flags.
    *   `MaxViewBoxes = 40` enforces `len(Containers) + len(Cards) <= 40` on `L0` and `L1` and `len(Cards) <= 40` on `L2` (counting all main-canvas connected workloads, unfolded standalone datastores/ingress/`N <= 2` cards, workload cohorts, and external/boundary stubs; `L2` `ContainerBox` frames enclose those `<= 40` cards, and `FooterGroups` + `LegendBox` sit outside the 40-box main-canvas cap).
    *   **`L0` (Organization & Folder Governance):** Expands `Organization -> Folder` layers breadth-first from `RootURI` while `len(Containers) + len(Cards) <= 40` (falling back to a flat project/cohort grid on project-only snapshots lacking org/folder records). Empty projects collapse into `Empty/Sandbox Projects ×N` per parent. Unexpanded folders render as `Folder Summary Cards` showing active project count, Shared VPC attachment count, and VPC-SC status (red dashed border iff 100% of active descendant projects belong to verified enforced perimeters; `VPC-SC: X/Y projects` badge when `0 < X < Y`; `VPC-SC: unverified` under `--audit-overlays` when access policies were unreadable). Flat folders with `> 25` active child projects group children by `(VPCSCPerimeter, HyphenPrefix)` into `Project Cohort Cards`, and excess folders/cohorts beyond 40 roll into deterministic `Other Folders ×K` / `Other Project Cohorts ×K` cards.
    *   **`L1` (Per-Hub Network Topology):** `DiscoverViews` emits one `L1` view per Shared VPC Host, per `NCC Hub`, and per standalone multi-subnet VPC with peering/hybrid connectivity. Active subnets group by region, unused subnets collapse into `Unused Subnets ×N` per VPC/region, attached `Service Projects` fold by parent folder into `Folder Attachment Fleet Cards` (`DrillTarget` -> `level=l0&root=folders/<id>`) when direct project cards would exceed `40`, and any remaining subnet/fleet overflow rolls into deterministic `Active Subnets (<region>) ×K` / `Other Service Project Folders ×K` cards.
    *   **`L2` (`1..K` Project / Workload Architecture):** `DiscoverViews` emits one default `L2` view per non-empty project (sub-indexed by `Component = 0..C-1` when `> 40` cards), while `explore` external-project stubs support 1-click multi-project expansion (`projects=p1,p2` when combined cards `<= 40`, else navigating to `projects=p2`). Non-database, non-BigQuery-Dataset, non-ingress cards with `inDegree + outDegree == 0` in the active view fold into `Standalone <DisplayType> ×N` whenever `N > 2` per `(ProjectID, DisplayType)` unless `ExpandStandalone` is set; those stacks share a compact footer band (`FooterGroups`) alongside `TransversalGroups` (compact domain summary cards for cross-cutting KMS, Secret Manager, IAM, and logging inventory). When main-canvas `L2` cards exceed `40`, `ProjectL2` first folds `> 3` homogeneous leaf workloads into `Workload Cohort Cards`, then either runs priority-ranked `k-hop` BFS capped at `40` cards (when `FocusURI != ""`) or partitions the graph via greedy hub-detachment bin-packing (`Component = 0..C-1` pages of `<= 40` cards with detached shared hubs replicated as clickable cross-component boundary stubs).
*   **Stateless Audit Traceability (`ExplainURI`):** Instead of mutating an in-memory map during layout, `compiler.ExplainURI(g, inv, cat, uri)` resolves provenance on demand—matching exact retained `Node`, `CollapsedAsset`, and `AssetRecord` URIs first, then matching AIP-122 URI patterns against `FoldedChildren` and catalog/suppressed plumbing asset types, and evaluating read-only `IsEmptyProject`/`IsUnusedSubnet` membership.

### 4. Hybrid Placement Truth Table and Multi-Level Layout Engine (`internal/compiler`, `internal/layout`)

*   **Compiler-Populated `ZoneAssignment` Truth Table:** `internal/compiler` retains `Node.VPCConnectorURI` during compaction and populates `Node.Zone` (`Kind`, `NetworkURI`, `SubnetURI`, `SharedVPCHost`, `SubzoneLabel`) once as a total function over `ir.Node`, while `views_l2.go` splits out-of-view `IsExternalRef` projects directionally per `ViewSpec`:

    | Resource Pattern | Assigned `L2` Band | Visual Frame Placement |
    | :--- | :--- | :--- |
    | Workload/LB/endpoint with local `SubnetURI` (`GCE`, `GKE`, `Dataflow`, `Workbench`, regional internal LB) | `BandVPC` (`ZoneVPCSubnet`) | Inside `Project` -> local `VPC` -> `Subnet (<region> · <cidr>)` frame |
    | Workload with borrowed Shared VPC `SubnetURI` (`SharedVPCHost != ""`) | `BandVPC` (`ZoneVPCDelegatedSubnet`) | Inside workload `Project` -> `Delegated Subnet (<subnet> · host: <host-id>)` frame |
    | Private Services Access datastore (`Cloud SQL`, `AlloyDB`, `Memorystore`, `Filestore` with `NetworkURI != ""` and `SubnetURI == ""`) | `BandVPC` (`ZoneVPCPSA`) | Inside `Project` -> `VPC` (or `Shared VPC: <host-id>`) -> `Private Services Access` strip |
    | Serverless (`Cloud Run`, `Cloud Functions`, `App Engine`) with `VPCConnectorURI != ""` and `SubnetURI == ""` | `BandVPC` (`ZoneVPCConnector`) | Inside `Project` -> `VPC` (or `Shared VPC: <host-id>`) -> `Serverless VPC Access` strip |
    | Other network-scoped resources with `NetworkURI != ""` and `SubnetURI == ""` (`Apigee`, `Composer 2/3`, global internal LB) | `BandVPC` (`ZoneVPCNetwork`) | Directly inside `Project` -> `VPC` (or `Shared VPC: <host-id>`) frame |
    | Serverless without VPC egress, external `Cloud Load Balancer` (`EXTERNAL`/`EXTERNAL_MANAGED`), and managed APIs (`BigQuery`, `Cloud Storage`, `Pub/Sub`, `Eventarc`, `Spanner`, `Firestore`, `Bigtable`, `Vertex AI`) | `BandManaged` (`ZoneManaged`) | Directly inside `Project` frame below `BandVPC` (external LBs anchored in leftmost internal ingress partition) |
    | External caller nodes (`LayerRoleExternalSource`) and out-of-scope GCP Project stubs (`IsExternalRef`) | `BandExternalUpstream` / `BandExternalDownstream` (`ZoneExternal`) | Outside `Project` frames: far-left column for inbound callers, far-right column for outbound targets (split into two stubs when bidirectional) |
    | Pure `L1` network infrastructure (`Network`, `Subnetwork`, `Router`, `VPN`, `Interconnect`, `NCC Hub`, `Connector`, `DNS`) & `Transversal` counters | Omitted from `L2` main flow (`ZoneL1Only` / `ZoneTransversal`) | Rendered on `L1` hub topology views or `L2` `FooterGroups` respectively |

*   **Pre-ELK Font Advance Wrapping (`internal/layout/textwrap.go`):** Uses one normalized sans-serif rune-width table scaled by font size/weight plus one monospace advance constant to wrap long GCP identifiers on `-`, `_`, `/`, `.`, and whitespace into bounded `TitleLines` and `SubtitleLines`, computing exact `CardBox` heights before `elk-go` runs.
*   **`L0` Two-Phase Tree+Grid Layout:** Phase 1 sizes a deterministic `ceil(sqrt(N))`-column 2D grid inside every expanded `Organization` or `Folder` container that holds direct child projects, cohorts, or empty-project badges. Phase 2 runs `elk-go` `mrtree` (`weighting = MODEL_ORDER`) on the `Organization -> Folder` tree using those pre-computed container dimensions, then translates internal child grids into canvas coordinates.
*   **`L2` Single-Pass `elk-go` + Go Banded Corridor/Gutter Pipeline:**
    1.  **Strict DAG-Monotonic Partitioning:** After cycle back-edge detection, Go reserves `Partition 0` exclusively for `BandExternalUpstream`, seeds zero-indegree internal event-source storage roots (`Cloud Storage -> Eventarc/PubSub/Functions`) immediately left of their earliest target role (`>= 1`) *before* forward topological dynamic programming, advances internal forward DAG edges strictly (`P(u) + 1` floored by architectural role from `Ingress` through `StatefulStorage` and `ConsumptionInference`), and places `BandExternalDownstream` in `maxInternalP + 1`.
    2.  **Single `elk-go` Column & Barycenter Pass:** Runs `elk.Layout` once on a flat single-component `topGraph` (`partitioning` active, `separateConnectedComponents` disabled, semantic `NODES_AND_EDGES` model order) to assign DAG-monotonic columns and untangle intra-column relative vertical order.
    3.  **Go Global Y-Band Striping & `ContainerBox` Synthesis:** Buckets each column strictly by `(ProjectOrder, BandOrder, SubnetIndex)` while preserving Pass-1 relative Y order within each bucket, places cards onto global horizontal Y-stripes (`BandVPC` frames above `BandManaged` rows separated by a dedicated horizontal band gutter), widens inter-column X-corridors wherever vertical container walls sit, and synthesizes `Project`, `VPC`, `Subnet`, `Delegated Subnet`, and `Private Services Access` `ContainerBox` envelopes with parent-relative coordinates.
    4.  **Go Corridor & Band-Gutter Orthogonal Edge Router:** Routes edges directly through the reserved inter-column vertical corridors and horizontal band gutters—using 2-bend Z-paths for unobstructed adjacent-column forward edges, 4-bend gutter-track U/S-paths strictly outside container headers for multi-column or cross-band edges, and south-gutter loops for cycle back-edges—populating exact `StartPoint`, `Waypoints`, `EndPoint`, and `LabelPos` coordinates on `RoutedEdge`.

### 5. Canonical Go SVG Emitter, Stateless `.drawio`, and `gcp-arch explore` (`internal/theme`, `internal/svg`, `internal/drawio`, `internal/explore`, `cmd/gcp-arch`)

*   **Shared Theme & Pure-Go SVG Renderer (`internal/theme`, `internal/svg`):**
    *   `internal/theme` centralizes project pill colors, 13-family card accent colors, container border/fill hex styles, and edge category colors shared between `internal/svg` and `internal/drawio`.
    *   `internal/svg` serializes `LayoutedPage` to standalone UTF-8 SVG 1.1 with explicit numeric `width`, `height`, and `viewBox` attributes on `<svg>` and deduplicated inline `<symbol id="icon-<key>" viewBox="...">` definitions inside `<defs>` (scoping internal `<filter>`/`<mask>` IDs with `icon-<key>-` prefixes and referencing icons via `<use href="#icon-<key>"/>`). `catalog.LoadCatalog` rejects deprecated Draw.io-only `stencil:` fields while `catalog.ValidateSVG` allows safe CSS style semicolons.
    *   Renders containers, rounded service cards, `<text><tspan>` wrapped lines, header pills (`Shared VPC`, `VPC-SC`), orthogonal `<path>` connectors with rounded `6px` corners across `[StartPoint, ...Waypoints, EndPoint]` (solid for `Tier 1` / `Confirmed`, dashed for `Tier 2`), unified footer `CounterGroup` (multi-row badge/count summary card) and `Standalone` stack cards, and compact `LegendBox`. Interactive elements carry `data-cell-id`, `data-node-uri`, and `data-drill-target` attributes.
    *   **Presentation vs. Audit Mode:** Default presentation mode (`AuditOverlays == false`) omits `Unresolved Links: N` badges and `CoverageOK` rows, rendering only the visual symbol legend on canvas while keeping full diagnostics in the `explore` inspector. Enabling `--audit-overlays` (`AuditOverlays == true`) renders `Unresolved Links: N` pills on cards with unconfirmed `Tier 3` candidates and appends a coverage footer restricted to non-`OK` `CoverageLedger` entries affecting scopes in the active view.
*   **Stateless Single-View `.drawio` Exporter (`internal/drawio`):** Deletes `internal/drawio/merge.go` and `internal/drawio/merge_reconcile.go` (`1,283` lines) plus ~250 lines of orphaned merge/staging/placeholder helpers in `render.go`, `styles.go`, and `xmlutil.go`, emitting a single-diagram uncompressed `<mxfile>` directly from `LayoutedPage` using `catalog.SVGDataURI` icons and shared `internal/ioutil.WriteFileAtomic`.
*   **Local Browser Explorer (`internal/explore`):**
    *   Loads `.snap` and `.gcp-arch/decisions.yaml`, retains an immutable pre-decisions baseline `*ir.Graph` alongside `*ir.RawInventory`, and starts an `http.Server` bound strictly to a loopback `netip.AddrPort` (`127.0.0.1:0` by default; non-loopback addresses return `exit 2`).
    *   Serves buildless `embed.FS` vanilla `index.html`, `app.css`, and `app.js` providing hierarchy breadcrumbs, view switcher, in-memory global search across direct and folded `MemberURIs`, pan/zoom SVG viewport, right-hand node inspector (`CollapsedAssets`, edge `Evidence`, active-scope `CoverageLedger` warnings, and 1-click `Tier 3` candidate confirm/unconfirm checkboxes), `Show Standalone` and `Audit Overlays` switches, and 1-click `Export SVG`, `Export PNG (2x)`, `Export PNG (3x)` (via offscreen HTML5 `<canvas>` clamped to browser pixel limits), and `Export .drawio`.
    *   **Loopback Security Middleware:** Prints/opens `http://<loopback-host>:<port>/#token=<32-byte-crypto-rand-hex>` so `app.js` captures the fragment into `sessionStorage` and scrubs `location.hash` via `history.replaceState` without inline scripts; validates `Host` header == bound `<loopback-host>:<port>`; sets `Cross-Origin-Resource-Policy: same-origin`, `X-Content-Type-Options: nosniff`, and `Content-Security-Policy: default-src 'self'; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'` with zero `CORS` headers; and requires `X-GCP-Arch-Token` on all 6 `/api/*` endpoints (`GET /api/views`, `GET /api/view`, `GET /api/search`, `GET /api/explain`, `GET /api/export`, `POST /api/decisions`).
*   **CLI Subcommand Surface (`cmd/gcp-arch`):**
    *   `gcp-arch preflight`: Retains existing CLI flags and JSON/table probe output, adding caller/target-scoped `ClassifyGCPError` and a CAI `Folder` display-name probe on `--org`/`--folder`.
    *   `gcp-arch snapshot`: Live GCP collection to `v2` `.snap` (`--org`, `--folder`, `--project`, `--quota-project`, `--project-alias <NUMBER=ID>`, `--catalog-dir`, `-q/--quiet`, `-o/--out`).
    *   `gcp-arch views <snap>`: Offline discovery of `L0`, `L1`, and `L2` `ViewSpec`s (`--decisions`, `--catalog-dir`, `--project-alias`, `--json`).
    *   `gcp-arch explore <snap>`: Loopback explorer (`--listen 127.0.0.1:0`, `--no-open`, `--decisions`, `--catalog-dir`, `--project-alias`).
    *   `gcp-arch export <snap>`: Pure-Go single-view export (`--level l0|l1|l2`, `--root`, `--hub`, `--project`, `--focus`, `--hops`, `--component`, `--expand-standalone`, `--audit-overlays`, `--decisions`, `--catalog-dir`, `--project-alias`, `--format svg|drawio`, `-o/--out`).
    *   `gcp-arch inspect <snap>`: Strictly offline IR summary plus stateless `--explain <uri>` provenance report (`--decisions`, `--catalog-dir`, `--project-alias`, `--json`). `cmd/gcp-arch/generate.go` is deleted.

## Alternatives Considered

### Alternative A: Single Static 1-or-2-Tab `.drawio` File for an Entire Organization

*   **Pros:** Emits a single file without a view slicer or local HTTP server.
*   **Cons:** Human legibility and Draw.io responsiveness degrade sharply past ~40 boxes per sheet; rendering 300–1,500 projects onto one canvas produces thousands of cards and edges that shrink below readable print size on A4 or slide pages.
*   **Reason for Rejection:** Bounded `<= 40`-box hierarchical views (`L0`, `L1`, `L2`) are required so every exported diagram is readable on a report page or slide without manual cropping.

### Alternative B: Single `.drawio` Workbook with One Tab per Project Across an Organization

*   **Pros:** Bounds per-tab node count for small individual projects without introducing a browser explorer.
*   **Cons:** Generates 300–1,500 tabs in one multi-megabyte `.drawio` XML file, stalls Draw.io tab switching, splits tightly coupled 2–4 project workloads (`app-prod` + `data-prod`) across separate tabs, and offers no global search or `L0 -> L1 -> L2` navigation to discover where active workloads reside.
*   **Reason for Rejection:** Multi-hundred-tab workbooks cannot be navigated or searched at enterprise scale and prevent composable `1..K` project workload diagrams.

### Alternative C: Representing Shared VPC Host Projects Only as External Stubs Outside Workload Projects

*   **Pros:** Avoids rendering delegated subnet frames inside workload project boundaries on `L2`.
*   **Cons:** Enterprise GCP landing zones commonly deploy application workloads in Shared VPC Service Projects borrowing subnets from a central Host Project; pushing the network outside the project frame strips subnet and CIDR containment from GKE clusters, VMs, and Private Services Access databases, mixing network-bound compute visually with serverless global APIs like BigQuery and Cloud Storage.
*   **Reason for Rejection:** Rendering `Delegated Subnet (<subnet> · host: <host-id>)` frames inside the upper `BandVPC` zone of the workload `Project` preserves network containment on `L2` while dedicating `L1` to full Shared VPC Host hub topology.

### Alternative D: Modeling `L2` Zones (`VPC` / `Managed`) as `INCLUDE_CHILDREN` ELK Compound Containers with Static `elk.partitioning`

*   **Pros:** Uses `elk-go` hierarchical compound containment directly to size `VPC` and `Project` boxes in a single layout pass.
*   **Cons:** In `github.com/d2lang/elk-go` (`v0.2.0`), `elk.partitioning.activate` is ignored on nested child subgraphs, static role partition numbers invert forward event pipelines (`Cloud Storage -> Eventarc -> Cloud Run`), and flows crossing out of and back into an inner `VPC` compound node (`GKE -> Pub/Sub -> Cloud SQL`) create container-level cycles.
*   **Reason for Rejection:** Placing all `L2` cards on flat `topGraph` with Go-computed strict DAG-monotonic partitions and synthesizing `ContainerBox` envelopes around Y-separated bands eliminates compound cycles and inverted event chains.

### Alternative E: Two-Pass `elk-go` `INTERACTIVE` Margin Routing or Per-Band Independent `elk-go` Subgraphs on `L2`

*   **Pros:** Attempts to delegate post-shift orthogonal edge routing back to a second `elk-go` `INTERACTIVE` pass or isolate bands into separate `elk.Graph` runs.
*   **Cons:** In `elk-go` `v0.2.0`, `cmEnforcedGroupOrders` fails JSON import, `org.eclipse.elk.margins` is unregistered and reset to zero before `INTERACTIVE` placement (routing long edges through synthesized container headers), and running separate `elk-go` subgraphs per band hides cross-band edges from crossing minimization and misaligns multi-column partitions.
*   **Reason for Rejection:** Running a single `elk-go` pass on unified `topGraph` (`separateConnectedComponents = false`) for column and barycenter discovery followed by Go Y-band striping, `ContainerBox` synthesis, and a ~100-line Go corridor/band-gutter orthogonal router guarantees header-free orthogonal routing within the `< 500 ms` per-view budget.

### Alternative F: Pure-Go (`CGO_ENABLED=0`) Standalone SVG-to-PNG Rasterizer (`oksvg` / `rasterx`), Custom Go Pixel Painter, or CLI Headless Chromium Wrapper

*   **Pros:** Produces `.png` files from the CLI without opening `gcp-arch explore`.
*   **Cons:** Pure-Go SVG rasterizers lack `<filter>`, `<mask>`, and `<feBlend>` support needed by embedded GCP icons (`pubsub`), a custom Go 2D painter requires bundled font overhead and ~1,500+ lines of parallel rendering logic, and shelling out to headless Chromium on the CLI breaks under Linux Snap `/tmp` confinement and introduces external runtime dependencies.
*   **Reason for Rejection:** HTML5 `<canvas>` in `gcp-arch explore` (backed by inline `<symbol>` SVG defs and numeric root `<svg>` dimensions) renders `2x`/`3x` PNGs synchronously from the canonical Go SVG while keeping the CLI binary 100% self-contained (`--format svg|drawio`).

### Alternative G: Retaining the 3-Way `.drawio` XML Merger (`internal/drawio/merge.go`) Alongside Dynamic View Folding

*   **Pros:** Preserves manual box dragging and hand-drawn arrows inside previously exported `.drawio` files across snapshot refreshes.
*   **Cons:** Threshold-driven stack folding (`<= 2` vs. `> 2` unlinked resources), `<= 40`-box cohort slicing, and `BandVPC` / `BandManaged` transitions invalidate parent-relative Draw.io coordinates and trigger false `REMOVED IN GCP` reconciliation states across runs; furthermore, curating `Tier 3` links inside Draw.io forces mixed visual styling across a single client report.
*   **Reason for Rejection:** Retiring `internal/drawio/merge.go` and `merge_reconcile.go` (`1,283` lines) and moving `Tier 3` confirmation upstream into `gcp-arch explore` + `.gcp-arch/decisions.yaml` keeps all report exports visually uniform while retaining stateless one-way `.drawio` export.

### Alternative H: Client-Side JavaScript Graph Layout and Rendering Framework (`ReactFlow`, `Cytoscape`, `Vite` / `npm`)

*   **Pros:** Provides prebuilt pan/zoom UI widgets and client-side node rendering.
*   **Cons:** Introduces a Node.js/npm/TypeScript build pipeline into a pure-Go repository and computes text wrapping and box dimensions via browser DOM layout, causing geometry drift against CLI `.svg` and `.drawio` exports.
*   **Reason for Rejection:** Serving the Go-rendered `<svg>` directly inside buildless `embed.FS` vanilla HTML/CSS/JS guarantees zero frontend build dependencies and identical geometry between `explore` and CLI exports.

## Non-Functional Requirements

### Security, Credential Hygiene, and Loopback Isolation

*   Uses read-only `ADC` (`cloud-platform.read-only`) exclusively; never invokes write APIs or data/secret payload endpoints (`secretmanager.versions.access`, GCS object reads, BigQuery row reads).
*   Strips URL `userinfo`/query strings, raw environment variable values, VM startup scripts, and raw SQL text during CAI/enricher ingestion; writes `.snap` and `.gcp-arch/decisions.yaml` atomically with `0600` file permissions (`0700` parent directories).
*   XML-escapes all text interpolated into `.svg` `<tspan>` nodes and `.drawio` attributes, enforces `catalog.ValidateSVG` on custom catalog icons (rejecting `<script>`, `<foreignObject>`, `on*` handlers, `data:`/`javascript:` schemes, and external URLs), and binds `gcp-arch explore` strictly to loopback (`netip.Addr.IsLoopback()`) with `Host` header validation, `Cross-Origin-Resource-Policy: same-origin`, `Content-Security-Policy`, and `#token=<hex>` -> `X-GCP-Arch-Token` authentication on `/api/*`.

### Performance, Memory, and API Quota Ceilings

*   Caps CAI `ListAssets` at `<= 80` requests/minute per `quota_project` (`page_size = 1000`), caps Cloud Monitoring (`<= 10` QPS, `86400s` `ALIGN_SUM`), BigQuery (`<= 10` QPS, `<= 20` pages/project with `Fields` mask), Dataflow (`<= 10` QPS), and Vertex AI `ragCorpora` (`<= 10` QPS, `<= 50` calls excluding auto-mode VPC subnet regions) via shared token-bucket limiters across `8` concurrent project workers gated on `EnabledServices`.
*   Maintains post-GC Go live heap `HeapAlloc < 512 MB` across `1,500`-project / `150,000`-sub-resource snapshots via stream-time child projection and stream-dropped `IGNORED_PLUMBING` records.
*   Completes single-view projection, `elk-go` + Go corridor/gutter layout, and SVG/Draw.io serialization in `< 500 ms` from an in-memory `ir.Graph` (and `< 1 s` cold CLI export from a `.snap` file) for `<= 40`-box views.

### Determinism and Schema Validation

*   Produces byte-identical `.svg` and `.drawio` outputs across repeated runs for identical `.snap`, `.gcp-arch/decisions.yaml`, catalog overlays, `--project-alias` overrides, and `ViewSpec` inputs (`elk.randomSeed = 1`, deterministic semantic model-order sorting, rounded 2-decimal coordinates).
*   Enforces strict schema validation (`exit 2`) on `SnapshotSchemaVersion == "v2"`, `.gcp-arch/catalog/*.yaml` (`schema_version: "v1"`, inline `svg:` icons only), and `.gcp-arch/decisions.yaml` (`schema_version: "v1"`).

## Future Improvements

*   Add offline snapshot topology diffing (`gcp-arch diff <old.snap> <new.snap>`) to compare two `v2` snapshots across audit dates and highlight added, removed, or modified nodes and edges in `gcp-arch explore` and Markdown change summaries.
*   Add an opt-in VPC Flow Logs and Cloud Trace BigQuery enricher to resolve remaining `1:N` private-IP database candidates automatically in organizations that export network telemetry.

--------------------------------------------------------------------------------

## Detailed Implementation Plan

### Incremental Delivery Phases

All changes use Go 1.26 standard-library packages and existing dependencies already pinned in `go.mod` (`github.com/d2lang/elk-go v0.2.0`, `github.com/beevik/etree v1.8.1`, `github.com/spf13/cobra v1.10.2`, `github.com/tidwall/gjson v1.20.4`, `gopkg.in/yaml.v3 v3.0.1`, `golang.org/x/sync`, `golang.org/x/time`, and Google Cloud read-only client libraries), adding zero new third-party modules. Phases 1–4 are strictly additive so `CGO_ENABLED=0 go build ./...` and `CGO_ENABLED=1 go test -race ./...` remain green at every phase boundary, followed by the atomic legacy retirement and CLI cutover in Phase 5:

*   **Phase 1 — Snapshot `v2` Schema, Catalog Plumbing Rules, Org-Scale Ingest Hardening & `v2` Fixtures:** Update `internal/ir`, `internal/ioutil` (atomic file writer), `internal/catalog` (`base_catalog.yaml` rules + semicolon-tolerant `ValidateSVG` + stencil rejection), `internal/ingest`, `cmd/gcp-arch/main.go` (`ClassifyGCPError` / schema exit-code wiring), non-destructive `v2` metadata in `testdata/enterprise_reference.snap.json` (keeping `shared-vpc-host` external until Phase 5), and new `testdata/org_hub_v2.snap.json`.
*   **Phase 2 — `decisions.yaml` Curation, Exhaustive `ZoneAssignment`, Stateless `ExplainURI` & Additive `<= 40`-Box `ViewSpec` Projectors:** Update `internal/compiler` (`decisions.go`, `placement.go`, `explain.go`, `compact.go` `VPCConnectorURI` + `XPNProjectStatus` merge) and add `internal/layout` `predicates.go`, `viewspec.go`, `ProjectView(g, inv, spec)`, `views_l0.go`, and `ViewSpec`-scoped projectors in `views_l1.go`/`views_l2.go` alongside legacy `ProjectViews`/`TransversalGroups`.
*   **Phase 3 — Pre-ELK Font Wrapping & Additive Multi-Level Geometry Engine:** Add `internal/layout/textwrap.go`, `elk_l0.go` (`mrtree` + internal 2D grids), `elk_l2.go` (1-pass `elk-go` + Go Y-band striping + `ContainerBox` synthesis + corridor/band-gutter orthogonal router), `RoutedEdge.{StartPoint,EndPoint,LabelPos}`, and `LayoutView(vg)` alongside legacy `LayoutGraph`, verified by `TestMultiLevelGeometryInvariants`.
*   **Phase 4 — Shared Theme Palette & Canonical Pure-Go SVG Renderer:** Create `internal/theme` and `internal/svg` (inline `<symbol>` icon defs with scoped IDs, numeric `<svg width height>`, `<tspan>` text, orthogonal rounded `<path>`, presentation vs. `--audit-overlays`) without deleting `MergeAndWriteDrawio` yet.
*   **Phase 5 — Loopback `explore` Server, Stateless `.drawio` Cutover, CLI Replacement & Docs:** Create `internal/explore`, delete `internal/drawio/merge.go`, `merge_reconcile.go`, orphaned merge helpers, legacy `ProjectViews`/`LayoutGraph`/`TransversalGroups`, and `cmd/gcp-arch/generate.go` atomically, update `internal/drawio` for single-view export, add `snapshot.go`, `views.go`, `explore.go`, `export.go`, and offline-only `inspect.go`, expand `testdata/enterprise_reference.snap.json` + `e2e_test.go`, and update `README.md`.

### Target Files to Create/Modify

All paths below are relative to the repository root (`/home/arrase/Develop/gcp-arch`):

#### 1. `internal/ir/model.go` and `internal/ir/model_test.go` (Modify — Phase 1 & Phase 2)

*   **Type / Scope:** Intermediate Representation, Snapshot `v2` Metadata, Zone Placement & Provenance Types.
*   **Proposed Changes:**
    *   Define `OrganizationMeta` (`OrgID string` `json:"org_id"`, `DisplayName string` `json:"display_name,omitempty"`) and `FolderMeta` (`FolderID string` `json:"folder_id"`, `DisplayName string` `json:"display_name,omitempty"`, `ParentURI string` `json:"parent_uri,omitempty"`), and add `XPNProjectStatus string` `json:"xpn_project_status,omitempty"` on `ProjectMeta`.
    *   Extend `RawInventory` with `Organizations map[string]OrganizationMeta` `json:"organizations,omitempty"`, `Folders map[string]FolderMeta` `json:"folders,omitempty"`, `SuppressedPlumbingCounts map[string]int` `json:"suppressed_plumbing_counts,omitempty"` (keyed by bare `AssetType`), and `Aliases map[string]string` `json:"aliases,omitempty"` (`projectNumber -> projectID`).
    *   Add `CoverageServiceDisabled CoverageState = "SERVICE_DISABLED"` and `EdgeTierConfirmed EdgeTier = "TIER1_CONFIRMED"`.
    *   Define `ZoneKind` (`ZoneVPCSubnet`, `ZoneVPCDelegatedSubnet`, `ZoneVPCPSA`, `ZoneVPCConnector`, `ZoneVPCNetwork`, `ZoneManaged`, `ZoneExternal`, `ZoneL1Only`, `ZoneTransversal`) and `ZoneAssignment` (`Kind ZoneKind` `json:"kind"`, `NetworkURI string` `json:"network_uri,omitempty"`, `SubnetURI string` `json:"subnet_uri,omitempty"`, `SharedVPCHost string` `json:"shared_vpc_host,omitempty"`, `SubzoneLabel string` `json:"subzone_label,omitempty"`); attach `VPCConnectorURI string` `json:"vpc_connector_uri,omitempty"` and `Zone ZoneAssignment` `json:"zone"` to `Node`.
    *   Attach `Organizations map[string]OrganizationMeta` `json:"organizations,omitempty"` and `Folders map[string]FolderMeta` `json:"folders,omitempty"` to `Graph`, and define `ProvenanceRecord` (`URI string` `json:"uri"`, `AssetType string` `json:"asset_type"`, `ProjectID string` `json:"project_id,omitempty"`, `Disposition string` `json:"disposition"`, `OwnerNodeID string` `json:"owner_node_id,omitempty"`, `SummaryLabel string` `json:"summary_label,omitempty"`, `Reason string` `json:"reason"`, `Views []string` `json:"views,omitempty"`) returned by `compiler.ExplainURI` (not stored as a mutable map on `Graph`).
    *   Extend `model_test.go` for `v2` JSON round-tripping and deterministic `CoverageLedger` sorting with `CoverageServiceDisabled`.

#### 2. `internal/ioutil/atomic.go` and `internal/ioutil/atomic_test.go` (Create — Phase 1), `internal/ingest/snapshot.go` (Modify — Phase 1)

*   **Type / Scope:** Shared Atomic File Writer, Snapshot `v2` Envelope & Schema Error Type.
*   **Proposed Changes:**
    *   Create `ioutil.WriteFileAtomic(path string, data []byte, filePerm, dirPerm os.FileMode) error` implementing `MkdirAll(dirPerm)` -> `CreateTemp` -> `Write` -> `Sync` -> `Chmod(filePerm)` -> `Close` -> `Rename` (with deferred temp cleanup on error), reused across `SaveSnapshot` (`0600`/`0700`), `SaveDecisions` (`0600`/`0700`), `WriteDrawioAtomic` (`0644`/`0755`), and CLI `.svg` export (`0644`/`0755`).
    *   In `internal/ingest/snapshot.go`, set `const SnapshotSchemaVersion = "v2"`, refactor `SaveSnapshot` to use `ioutil.WriteFileAtomic`, and define `SnapshotSchemaError` (`Path string`, `Got string`, `Want string`) returned by `LoadSnapshot` whenever `snap.SchemaVersion != SnapshotSchemaVersion` (mapped to exit code `2` in `cmd/gcp-arch/main.go`).

#### 3. `internal/ingest/client.go` (Modify — Phase 1)

*   **Type / Scope:** Caller/Target-Scoped GCP Error Classification, Rate Limiters, Progress Reporter & Preflight Probes.
*   **Proposed Changes:**
    *   Add `ErrorKindTargetServiceDisabled ErrorKind = "TARGET_SERVICE_DISABLED"`; extend `Clients` / `ClientOptions` with `BQLimiter *rate.Limiter`, `DataflowLimiter *rate.Limiter`, `RAGLimiter *rate.Limiter` (each defaulting to `rate.NewLimiter(rate.Limit(10), 10)`), `ProgressWriter io.Writer`, and mutex-protected `ReportProgress(force bool, format string, args ...any)` throttled to `1s` intervals.
    *   Update `ClassifyGCPError(err error, callerQuotaProject, targetProjectID, targetProjectNumber string) ClassifiedError`: inspect `ErrorInfo.Metadata["consumer"]` (e.g., `"projects/12345"` or `"projects/child-id"`). For `SERVICE_DISABLED` or `BILLING_DISABLED`, return `ErrorKindTargetServiceDisabled` (`CoverageState() == ir.CoverageServiceDisabled`, `IsDegraded() == true`) when `(targetProjectID != "" || targetProjectNumber != "")`, `consumer` matches `targetProjectID` or `targetProjectNumber`, and `consumer` does not match `callerQuotaProject`; in all other cases (including org/folder CAI calls where `targetProjectID == ""` or consumer matching `callerQuotaProject`), return `ErrorKindQuotaFatal`. `USER_PROJECT_DENIED` remains unconditionally `ErrorKindQuotaFatal`. Add a shared `429` / `RATE_LIMIT_EXCEEDED` exponential-backoff retry wrapper (`<= 3` retries) for enricher RPCs.
    *   Update `RunPreflight`: pass `(err, clients.QuotaProject, sampleProj, "")` into `ClassifyGCPError` and, when `--org` or `--folder` scopes are present, append a 7th `PreflightProbe` (`Source: "cai.folders"`, `RequiredRole: "roles/resourcemanager.folderViewer"`) executing `clients.Asset.ListAssets` (`AssetTypes: []string{"cloudresourcemanager.googleapis.com/Folder"}`, `ContentType: assetpb.ContentType_RESOURCE`, `PageSize: 1`), recording `ProbeOK` when accessible with non-empty `displayName` (or zero folders exist) and `ProbeDegraded` on `PERMISSION_DENIED` / empty `displayName`.

#### 4. `internal/ingest/sanitize.go` (Modify — Phase 1)

*   **Type / Scope:** Stream Hierarchy Extraction, `XPNProjectStatus` Retention & Safe `IGNORED_PLUMBING` Dropping.
*   **Proposed Changes:** Update `ProjectAndSanitizeAsset(inv *ir.RawInventory, cat *catalog.Catalog, asset *assetpb.Asset, projIdx map[string]string)`:
    *   Extract `cloudresourcemanager.googleapis.com/Organization` (`displayName` -> `inv.Organizations[orgID]`), `cloudresourcemanager.googleapis.com/Folder` (`displayName`, `parent` -> `inv.Folders[folderID]`), `cloudresourcemanager.googleapis.com/Project` (`ingestProjectAsset`, updated to preserve existing `XPNProjectStatus` alongside `VPCSCPerimeter`), and `compute.googleapis.com/Project` (`xpnProjectStatus` merged onto `inv.Projects[projKey]`), returning immediately after handling each of these four metadata types.
    *   Verify via test that existing denylist `sanitizeStructMap` continues retaining `autoCreateSubnetworks` on `compute.googleapis.com/Network` and `purpose`, `role`, and `privateIpGoogleAccess` on `compute.googleapis.com/Subnetwork` `ProjectedFields`.
    *   When `cat != nil`, check `spec, ok := cat.Asset(asset.AssetType)`: drop the asset and increment `inv.SuppressedPlumbingCounts[asset.AssetType]` without appending to `inv.Assets` when `(ok && spec.Disposition == catalog.DispositionIgnoredPlumbing) || (!ok && cat.IsSuppressedPlumbingService(host) && host != "compute.googleapis.com" && host != "container.googleapis.com" && host != "iam.googleapis.com")`.

#### 5. `internal/ingest/cai.go` (Modify — Phase 1)

*   **Type / Scope:** Paginated CAI Collector, Subnet IAM Capture, `inv.Projects` Reconciliation & Progress Telemetry.
*   **Proposed Changes:**
    *   Pass `cat` into `ProjectAndSanitizeAsset`, and pass `(err, clients.QuotaProject, "", "")` at all three `ClassifyGCPError` call sites in `CollectCAI`, `collectAccessPolicies`, and `collectIAMPolicies`.
    *   Add `"compute.googleapis.com/Subnetwork"` to `iamPolicyAssetTypes`; in `collectIAMPolicies`, when `a.GetAssetType() == "compute.googleapis.com/Subnetwork"`, retain only bindings whose `Role` equals `"roles/compute.networkUser"` or contains `"networkUser"` / `"compute.subnetworks.use"`.
    *   In `reconcileInventory`, re-key numeric project keys in `inv.Projects` into canonical `ProjectID` keys while merging `VPCSCPerimeter` and `XPNProjectStatus`, and canonicalize `ParentURI` project/folder references.
    *   Call `clients.ReportProgress` during `RESOURCE`, `ACCESS_POLICY`, and `IAM_POLICY` pagination.

#### 6. `internal/ingest/enrichers.go` (Modify — Phase 1)

*   **Type / Scope:** Concurrent Rate-Limited `EnabledServices`-Gated Enrichers & BigQuery `Fields` Mask.
*   **Proposed Changes:**
    *   Update `handleEnrichError` to accept `(err error, clients *Clients, projID, projNum, scope, source, role string, ledger *[]ir.CoverageEntry)` and invoke `ClassifyGCPError(err, clients.QuotaProject, projID, projNum)`.
    *   Parallelize the **per-project loops inside** `EnrichBigQueryJobs`, `EnrichDataflowJobs`, and `EnrichMonitoring` using `errgroup.Group` (`SetLimit(8)`), waiting on `clients.BQLimiter`, `clients.DataflowLimiter`, and `clients.MonitoringLimiter` before each RPC, guarding shared slice/ledger appends with a local `sync.Mutex`, calling `clients.ReportProgress`, and sorting collected slices deterministically before assignment.
    *   Add `isServiceMaybeEnabled(inv *ir.RawInventory, proj, host string) bool` (`false` when `len(inv.EnabledServices[proj]) > 0` and `host` is absent), gating `EnrichBigQueryJobs` (`bigquery.googleapis.com`), `EnrichDataflowJobs` (`dataflow.googleapis.com`), `EnrichMonitoring` (`monitoring.googleapis.com`, plus skipping projects holding only `default` networks, default service accounts, and `_Default`/`_Required` log buckets; gating `queryPublisherInvocations` on `aiplatform.googleapis.com`; and running `queryPublisherThroughput` only when invocations > 0), and `EnrichRagCorpora` (`aiplatform.googleapis.com`, `clients.RAGLimiter`, ignoring `Subnetwork` locations whose `network` has `autoCreateSubnetworks == true`).
    *   In `EnrichBigQueryJobs`, chain `.Fields("jobs(id,jobReference,user_email,principal_subject,configuration(query/destinationTable,load/destinationTable,copy(sourceTable,sourceTables,destinationTable)),statistics/query/referencedTables),nextPageToken")` on `clients.BigQuery.Jobs.List(proj)`.

#### 7. `internal/ingest/ingest_test.go`, `testdata/enterprise_reference.snap.json` (Modify — Phase 1 & Phase 5), and `testdata/org_hub_v2.snap.json` (Create — Phase 1)

*   **Type / Scope:** `v2` Snapshot Fixtures, Updated Callers & `TestOrgScale1500ProjectsHeapCeiling`.
*   **Proposed Changes:**
    *   Phase 1: Update all 15 `ProjectAndSanitizeAsset` and 6 `CollectCAI` call sites in `ingest_test.go`, update `TestRunPreflight*` probe counts (`7` on folder/org scopes, `6` on project scopes), bump `"schema_version": "v2"` plus `organizations` and `folders` metadata on `testdata/enterprise_reference.snap.json` (preserving out-of-scope `shared-vpc-host` until Phase 5 so `cmd/gcp-arch` E2E tests stay green), and create `testdata/org_hub_v2.snap.json` containing an in-scope Shared VPC Host (`xpn_project_status: "HOST"`), active borrowed subnets, a `REGIONAL_MANAGED_PROXY` subnet, an unused `PRIVATE` lab subnet, and Host/Subnetwork `roles/compute.networkUser` bindings (including robot service agents).
    *   Implement `TestOrgScale1500ProjectsHeapCeiling` and companion unit tests in `ingest_test.go` verifying: (a) child-project `SERVICE_DISABLED` (both project ID and numeric `projects/<NUMBER>` consumer) records `CoverageServiceDisabled` while caller `QuotaProject` or org-CAI `SERVICE_DISABLED` returns `*QuotaProjectError`, (b) `EnabledServices`-gated enrichers issue `0` HTTP calls for disabled projects and include `fields=` on BigQuery `jobs.list`, (c) auto-mode VPC subnets do not trigger 40-region `ragCorpora` calls, (d) GCE/GKE/IAM/`TargetSslProxy`/`TargetVpnGateway` assets survive stream sanitization while 1,500 synthetic projects with 150,000 `Firewall`/`Route` records stay within post-GC `runtime.MemStats.HeapAlloc < 512<<20`, and (e) `LoadSnapshot` returns `*SnapshotSchemaError` on `"v1"`.

#### 8. `internal/catalog/assets/base_catalog.yaml`, `internal/catalog/assets/icons.yaml`, `internal/catalog/catalog.go`, `internal/catalog/svg.go`, and `internal/catalog/catalog_test.go` (Modify — Phase 1)

*   **Type / Scope:** Catalog Plumbing/VPN/Proxy Rules, Semicolon-Safe Inline SVGs & Stencil Retirement.
*   **Proposed Changes:**
    *   `base_catalog.yaml`: add `compute.googleapis.com/Project` (`disposition: "IGNORED_PLUMBING"`), `compute.googleapis.com/TargetSslProxy` (`disposition: "ABSORBED_CHILD"`), and `compute.googleapis.com/TargetVpnGateway` (`disposition: "ARCHITECTURAL_NODE"`, `family: "Networking"`, `layer_role: "INGRESS"`, `icon_key: "cloud_vpn"`).
    *   `icons.yaml`: add self-contained inline `<svg viewBox="0 0 24 24">` definitions for `gcp_organization`, `gcp_folder`, and `project_cohort`.
    *   `catalog.go`: reject non-empty `IconSpec.Stencil` in `mergeYAML` with a `*SchemaError` instructing overlay authors to provide inline `svg:`, and delete unreachable `validateStencilPath`.
    *   `svg.go`: remove the `strings.Contains(s, ";")` rejection in `ValidateSVG` (retaining `<script>`/`<foreignObject>`/tag, `on*` event-handler, `javascript:`/`vbscript:`/`data:` scheme, and external URL checks) while keeping `SVGDataURI(svg string) string` (`"data:image/svg+xml," + base64`) for `.drawio`. Update `catalog_test.go`.

#### 9. `internal/compiler/decisions.go` (Create — Phase 2)

*   **Type / Scope:** Declarative `.gcp-arch/decisions.yaml` Loader, Atomic Writer & `confirmed_edges` Overlay.
*   **Proposed Changes:**
    *   Define `const DecisionsSchemaVersion = "v1"`, `DecisionsSchemaError`, `ConfirmedEdgeRule` (`SourceURI string` `yaml:"source_uri" json:"source_uri"`, `TargetURI string` `yaml:"target_uri" json:"target_uri"`, `Label string` `yaml:"label,omitempty" json:"label,omitempty"`), and `DecisionsFile` (`SchemaVersion string` `yaml:"schema_version" json:"schema_version"`, `ConfirmedEdges []ConfirmedEdgeRule` `yaml:"confirmed_edges,omitempty" json:"confirmed_edges,omitempty"`).
    *   Implement `LoadDecisions(path string, explicit bool) (*DecisionsFile, error)` (`yaml.NewDecoder` with `KnownFields(true)`; returns empty `v1` struct when `!explicit` and `os.IsNotExist(err)`) and `SaveDecisions(path string, df *DecisionsFile) error` (sorts `ConfirmedEdges` by `(SourceURI, TargetURI, Label)` and calls `ioutil.WriteFileAtomic` with `0600`/`0700`).
    *   Implement `ApplyDecisions(g *ir.Graph, df *DecisionsFile) error`: matches `rule.SourceURI == node.ID` and matches `rule.TargetURI` against `cand.TargetURI` or `cand.TargetNodeID` (resolving target node `dstNode = g.Nodes[cand.TargetNodeID]` or `g.Nodes[rule.TargetURI]`), promotes or upgrades the `(srcNode.ID, dstNode.ID)` edge to `Tier: ir.EdgeTierConfirmed` (`Evidence: ["decision:confirmed"]`, `Category`: `EdgeCategoryMessaging` for `FamilyMessaging`, `EdgeCategoryIAM` for `FamilySecurityIAM`, else `EdgeCategoryDataFlow`), filters promoted candidates out of `srcNode.UnresolvedCandidates`, and updates or removes the `"Unresolved Links"` badge on `srcNode.Badges`.

#### 10. `internal/compiler/placement.go` (Create — Phase 2), `internal/compiler/explain.go` (Create — Phase 2), `internal/compiler/compact.go`, `internal/compiler/compact_finalize.go`, `internal/compiler/edges.go`, `internal/compiler/compiler_test.go`, and `internal/compiler/edges_test.go` (Modify — Phase 2)

*   **Type / Scope:** `VPCConnectorURI` & `XPNProjectStatus` Retention, Exhaustive `ZoneAssignment` & Stateless `ExplainURI`.
*   **Proposed Changes:**
    *   In `compact.go`: update `mergeProjectMeta` to copy `XPNProjectStatus`; copy `inv.Organizations` and `inv.Folders` onto `g` (applying `inv.Aliases` alongside CLI aliases); in `applyVPCConnectorNetwork`, store the matched connector URI on `node.VPCConnectorURI` and, when an out-of-scope Shared VPC connector resolves to a `/subnetworks/...` URI rather than a `/networks/...` URI, assign `node.SubnetURI` (and `SharedVPCHost`) instead of `node.NetworkURI`; in `compact_finalize.go`, merge keys of `inv.SuppressedPlumbingCounts` into deduplicated sorted `g.SuppressedPlumbing []string`.
    *   Create `AssignZones(g *ir.Graph)` in `placement.go` (invoked at the end of `ResolveEdges` and after `ApplyDecisions`), evaluating every `ir.Node` in priority order:
        1.  `Transversal` nodes (`Disposition == TRANSVERSAL_COUNTER`): `ZoneTransversal`.
        2.  `L1`-only infrastructure (`Network`, `Subnetwork`, `Router`, `VpnTunnel`, `TargetVpnGateway`, `VpnGateway`, `InterconnectAttachment`, `Hub`, `Spoke`, `Connector`, `ManagedZone`, `Policy`): `ZoneL1Only`.
        3.  `IsExternalRef` or `LayerRole == LayerRoleExternalSource` (no synthetic `Internet` node is invented unless already present in `g.Nodes`): `ZoneExternal`.
        4.  `SubnetURI != ""` and `SharedVPCHost != ""`: `ZoneVPCDelegatedSubnet` (`SubzoneLabel: "Delegated Subnet: " + shortName(SubnetURI) + " (host: " + SharedVPCHost + ")"`).
        5.  `SubnetURI != ""` and `SharedVPCHost == ""`: `ZoneVPCSubnet` (`SubzoneLabel: "Subnet: " + shortName(SubnetURI)`).
        6.  `NetworkURI != ""` and `SubnetURI == ""` and service host in `{"sqladmin.googleapis.com", "alloydb.googleapis.com", "redis.googleapis.com", "memcache.googleapis.com", "file.googleapis.com"}`: `ZoneVPCPSA` (`SubzoneLabel: "Private Services Access"`).
        7.  `VPCConnectorURI != ""` and `SubnetURI == ""`: `ZoneVPCConnector` (`SubzoneLabel: "Serverless VPC Access"`).
        8.  `NetworkURI != ""` and `SubnetURI == ""` and not `EXTERNAL`/`EXTERNAL_MANAGED` `ForwardingRule`: `ZoneVPCNetwork` (`SubzoneLabel: "VPC: " + shortName(NetworkURI)`).
        9.  All remaining nodes (serverless without VPC egress, regional/global `EXTERNAL`/`EXTERNAL_MANAGED` Load Balancers, and managed global/regional APIs): `ZoneManaged`.
    *   Create `ExplainURI(g *ir.Graph, inv *ir.RawInventory, cat *catalog.Catalog, uri string) (ir.ProvenanceRecord, bool)` in `explain.go`: checks exact `g.Nodes[uri]`, scans `Node.CollapsedAssets` and `inv.Assets` (identifying empty-project, unused-subnet, transversal, or standalone dispositions), and falls back to AIP-122 URI pattern extraction (`//<service>/projects/<proj>/...`) against `inv.FoldedChildren` parent prefixes and `cat.Asset(inferredType)` / `inv.SuppressedPlumbingCounts`.
    *   Add unit tests in `compiler_test.go` and `edges_test.go` covering all 9 `ZoneAssignment` branches, `XPNProjectStatus` & `VPCConnectorURI` preservation, `ApplyDecisions` badge/candidate updates, and exact + stream-dropped `ExplainURI` queries.

#### 11. `internal/layout/predicates.go` (Create — Phase 2)

*   **Type / Scope:** Pure Read-Only Empty-Project, Unused-Subnet & Shared VPC Attachment Evaluators.
*   **Proposed Changes:**
    *   Implement `IsEmptyProject(g *ir.Graph, inv *ir.RawInventory, projectID string) (empty bool, reason string)`: returns `true` iff `g.Projects[projectID].XPNProjectStatus != "HOST"`, zero `g.Nodes` in `projectID` have `Zone.Kind != ZoneL1Only && Zone.Kind != ZoneTransversal`, zero `inv.Assets` in `projectID` are non-`default` `Network`s or custom `Subnetwork`s, zero nodes borrow a Shared VPC (`SharedVPCHost != ""`), and zero `inv.Assets` in `projectID` are non-default transversal assets (ignoring `default` VPCs, `_Default`/`_Required` `LogBucket`s, and default service accounts matching `*-compute@developer.gserviceaccount.com`, `*@appspot.gserviceaccount.com`, `*@cloudservices.gserviceaccount.com`, or `service-*@*.iam.gserviceaccount.com`).
    *   Implement `IsUnusedSubnet(g *ir.Graph, inv *ir.RawInventory, subnetURI string) (unused bool, reason string)`: returns `false` immediately if `Subnetwork.ProjectedFields["purpose"]` is non-empty and not `"PRIVATE"`/`"PRIVATE_RFC_1918"` (exempting `REGIONAL_MANAGED_PROXY`, `GLOBAL_MANAGED_PROXY`, `INTERNAL_HTTPS_LOAD_BALANCER`, `PRIVATE_SERVICE_CONNECT`, `PEER_MIGRATION`); otherwise returns `true` iff zero `g.Nodes` match `SubnetURI` or `CollapsedAssets`, zero `vpcaccess.googleapis.com/Connector` assets reference the subnet in `ProjectedFields["subnet"]`, zero `ServiceAttachment` assets list `subnetURI` in `natSubnets`, zero internal `compute.googleapis.com/Address` assets reference `subnetURI`, and zero `inv.IAMBindings` with `ResourceURI == subnetURI` grant `networkUser` / `subnetworks.use`.
    *   Implement `AttachedServiceProjects(g *ir.Graph, inv *ir.RawInventory, hostProjectID string) []string` combining `Node.SharedVPCHost == hostProjectID` with project IDs/numbers parsed from user and Google robot service-agent principals (`service-<NUM>@...`, `<NUM>@cloudservices.gserviceaccount.com`, `<NUM>-compute@developer.gserviceaccount.com`) on Host `Project` and `Subnetwork` `networkUser` bindings. Neither predicate mutates `g` or `inv`.

#### 12. `internal/layout/viewspec.go` (Create — Phase 2), `internal/layout/views_l0.go` (Create — Phase 2), `internal/layout/views.go`, `internal/layout/views_l1.go`, and `internal/layout/views_l2.go` (Modify — Phase 2 & Phase 5)

*   **Type / Scope:** Stateless `ViewSpec`, View Discovery & Symmetric `<= 40`-Box Projectors.
*   **Proposed Changes:**
    *   `viewspec.go`: define `const MaxViewBoxes = 40`, `ViewLevel` (`LevelL0 = "l0"`, `LevelL1 = "l1"`, `LevelL2 = "l2"`), `ViewSpec` (`Level ViewLevel`, `RootURI string`, `HubID string`, `Projects []string`, `FocusURI string`, `Hops int`, `Component int` with `-1` meaning unset/default `0`, `ExpandStandalone bool`, `AuditOverlays bool`), `ParseViewSpecQuery(q url.Values) (ViewSpec, error)` / `(ViewSpec).QueryValues() url.Values` using exact canonical query keys `level`, `root`, `hub`, `projects` (comma-separated), `focus`, `hops`, `component`, `standalone` (`0`|`1`), and `audit` (`0`|`1`), `ViewSummary` (`ID string` `json:"id"`, `Level ViewLevel` `json:"level"`, `Title string` `json:"title"`, `BoxCount int` `json:"box_count"`, `Query string` `json:"query"`, `ExportFlags string` `json:"export_flags"`), `DiscoverViews(g *ir.Graph, inv *ir.RawInventory) []ViewSummary`, and `ProjectView(g *ir.Graph, inv *ir.RawInventory, spec ViewSpec) (*ViewGraph, error)`.
    *   `views.go`: add `Level ViewLevel`, `Spec ViewSpec`, `AuditOverlays bool`, and `FooterGroups []CounterGroup` on `ViewGraph` and `LayoutedPage`; add `MemberURIs []string` and `DrillTarget string` on `ViewContainer`/`ContainerBox` and `ViewCard`/`CardBox`; add `TitleLines []string`, `SubtitleLines []string`, and `Height float64` on `ViewCard` and `CardBox`; add `StartPoint Point`, `EndPoint Point`, and `LabelPos *Point` on `RoutedEdge`. (Retain legacy `ProjectViews`/`TransversalGroups` until Phase 5 cutover.)
    *   `views_l0.go`: project `Organization -> Folder` tree from `RootURI` (or flat project/cohort view when `len(g.Organizations) == 0 && len(g.Folders) == 0`) keeping `len(Containers) + len(Cards) <= 40`; emit `Folder Summary Cards` (`HasVPCSC = true` iff 100% of active descendant projects have non-empty `VPCSCPerimeter`, `VPC-SC: X/Y projects` badge iff `0 < X < Y`, and `VPC-SC: unverified` under `AuditOverlays` when `cai.access_policies` degraded); collapse empty projects into `Empty/Sandbox Projects ×N`; group `> 25` active child projects into `(VPCSCPerimeter, HyphenPrefix)` `Project Cohort Cards` (`DrillTarget`: `level=l2&projects=<csv>` when combined `L2` cards `<= 40`, else first project `level=l2&projects=<first>`); and roll excess folders/cohorts past 40 into `Other Folders ×K` / `Other Project Cohorts ×K`.
    *   `views_l1.go`: identify hubs by canonical `HubID` (`xpn:<hostProjectID>`, `ncc:<hubShortName>`, `vpc:<projectID>/<networkName>` for standalone `>= 2`-subnet VPCs with peering/VPN/Interconnect; auto-selecting the sole hub when `spec.HubID == ""` and `len(hubs) == 1`); collapse `IsUnusedSubnet` into `Unused Subnets ×N`; fold attached service projects into `Folder Attachment Fleet Cards` (`DrillTarget = "level=l0&root=folders/<id>"`) and tail-bucket excess subnets/fleets so `len(Containers) + len(Cards) <= 40`; treat `EdgeTierConfirmed` identically to `EdgeTierVerified` in edge-tier precedence.
    *   `views_l2.go`: scope projection to `spec.Projects` (`1..K` projects); split bidirectional `IsExternalRef` stubs into `Upstream (<proj>)` and `Downstream (<proj>)` with `DrillTarget` (`level=l2&projects=<current>,<proj>` when combined `L2` cards `<= 40`, else `level=l2&projects=<proj>`); fold `inDegree + outDegree == 0` cards whose `Family != FamilyDatabase`, `PrimaryAssetType != "bigquery.googleapis.com/Dataset"`, and `LayerRole != LayerRoleIngress` into `Standalone <DisplayType> ×N` inside `FooterGroups` whenever `N > 2` per `(ProjectID, DisplayType)` and `!spec.ExpandStandalone`; when `len(Cards) > 40`, first fold `> 3` homogeneous leaf workloads sharing `(ProjectID, Zone, DisplayType, HyphenPrefix, NeighborSignature)` into `Workload Cohort Cards`, then slice via capped `k-hop` BFS (`FocusURI != ""`, default `Hops = 2`, cap `40`) or greedy hub-detachment bin-packing into `Component = 0..C-1` pages (`<= 40` cards including replicated detached hub boundary stubs).

#### 13. `internal/layout/textwrap.go` (Create — Phase 3)

*   **Type / Scope:** Proportional/Monospace Font Advance Width Table & Pre-ELK Line Wrapper.
*   **Proposed Changes:** Embed one normalized sans-serif rune-width lookup table scaled by `(fontSize, boldFactor)` for `FontTitle12Bold` (`12px`, line height `16px`, max `2` lines), `FontSub10Regular` (`10px`, line height `14px`, max `2` lines), and `FontPill9Bold` (`9.5px`), plus a single constant advance width (`5.7px`) for `FontBadge9Mono`. Using `CardWidth = 220`, `IconGutter = 44`, and `RightPadding = 14` (`maxTextWidth = 162px`), expose `MeasureTextWidth`, `WrapTextLines` (breaking on whitespace and `-`, `_`, `/`, `.` before hard-wrapping overlong tokens and appending `…` when exceeding `maxLines`), and `ComputeWrappedCardBox(card *ViewCard)` setting `TitleLines`, `SubtitleLines`, and exact `Height` (`>= 64px`) prior to ELK.

#### 14. `internal/layout/elk_l0.go` (Create — Phase 3), `internal/layout/elk_l2.go` (Create — Phase 3), `internal/layout/elk.go`, `internal/layout/elk_post.go`, and `internal/layout/layout_test.go` (Modify — Phase 3 & Phase 5)

*   **Type / Scope:** `L0` Tree+Grid Engine, `L2` 1-Pass `elk-go` + Corridor/Gutter Engine & `TestMultiLevelGeometryInvariants`.
*   **Proposed Changes:**
    *   `elk_l0.go`: size internal `ceil(sqrt(N))`-column 2D grids inside every expanded `Organization`/`Folder` holding direct child cards, run `elk-go` `mrtree` (`"elk.algorithm": "mrtree"`, `"elk.direction": "DOWN"`, `"elk.mrtree.searchOrder": "DFS"`, `"elk.mrtree.weighting": "MODEL_ORDER"`, `"elk.spacing.nodeNode": "36"`), emit expanded org/folders as `ContainerBox`es, unexpanded folders/cohorts/projects as `CardBox`es, and hierarchy links as orthogonal tree `RoutedEdge`s.
    *   `elk_l2.go`: implement `LayoutL2View(vg *ViewGraph) (*LayoutedPage, error)`:
        1.  Run `DetectCycles`, then `ComputeL2Partitions` (`Partition 0` = `BandExternalUpstream`; `roleFloor`: `Ingress=1, Compute=2, Messaging=3, StatefulStorage=4, ConsumptionInference=5`; seed zero-indegree internal `StatefulStorage` event-source roots at `max(1, min_{(u,w) in DAG} roleFloor(w) - 1)` *before* topological DP; forward DP `P(v) = max(roleFloor(v), max_{(u,v) in DAG} (P(u) + 1))`; `BandExternalDownstream` = `maxInternalP + 1`).
        2.  Sort cards by `(ProjectOrder, BandOrder, SubnetKey, EffectivePartition, RoleOrdinal, FamilyOrdinal, DisplayType, DisplayName, NodeID)` and run a single `elk.Layout` pass on flat `topGraph` (`"elk.algorithm": "layered"`, `"elk.direction": "RIGHT"`, `"elk.partitioning.activate": "true"`, `"elk.separateConnectedComponents": "false"`, `"elk.layered.considerModelOrder.strategy": "NODES_AND_EDGES"`, `"elk.randomSeed": "1"`).
        3.  Bucket each column by `(ProjectOrder, BandOrder, SubnetIndex)` preserving ELK's relative Y order inside each bucket, assign global horizontal Y-stripes (`BandGutter = 56px`), shift column X coordinates by `+48px` per vertical container wall crossed between adjacent columns, and synthesize `ContainerBox` frames (`ContainerKindProject`, `ContainerKindVPC`, `ContainerKindSubnet`, `ContainerKindDelegatedSubnet`, `ContainerKindPSA`, `ContainerPadding {Top: 44, Left: 24, Right: 24, Bottom: 28}`) with parent-relative `X, Y, ParentID` alongside `AbsX, AbsY`.
        4.  Run `routeL2Edges` *after* container synthesis: distribute East/West/South card ports (`8px` spacing), route unobstructed adjacent-column same-band forward edges as 2-bend inter-column Z-paths, route multi-column or cross-band forward edges as 4-bend U/S-paths along dedicated horizontal `BandGutter` tracks outside `44px` container headers, and route cycle back-edges via south-gutter loops, populating `StartPoint`, `Waypoints`, `EndPoint`, `LabelPos`, and `IsAdjacentRankForward`.
    *   `elk.go` & `elk_post.go`: add `LayoutView(vg *ViewGraph) (*LayoutedPage, error)` dispatching `L0`, `L1` (`INCLUDE_CHILDREN` `layered`), and `L2`, populate `StartPoint`/`EndPoint`/`LabelPos` in `extractELKEdges`, place `FooterGroups` across up to 4 columns below `maxFlowY + 64`, and omit `CoverageOK` / `Unresolved Links` when `!vg.AuditOverlays`.
    *   `layout_test.go`: implement `TestMultiLevelGeometryInvariants` verifying `<= 40`-box compliance across 150-project flat folders (`> 50` prefixes), 200-spoke Shared VPC Hubs, and 85-node shared-LB/SQL monoprojects, `len(vg.Edges) == len(page.Edges)`, strict X monotonicity (`X(Storage) < X(Eventarc) < X(Cloud Run) < X(SQL)`), disjoint `ContainerBox` bands, zero non-adjacent edge intersections with cards or `44px` container title bars, and 10-run byte-identical determinism.

#### 15. `internal/theme/palette.go` (Create — Phase 4), `internal/svg/render.go`, and `internal/svg/render_test.go` (Create — Phase 4)

*   **Type / Scope:** Shared Visual Theme Palette & Canonical Pure-Go `LayoutedPage -> SVG` Serializer.
*   **Proposed Changes:**
    *   `internal/theme/palette.go`: extract `ProjectPillColor`, container fill/stroke styles (`Project`, `VPC`, `Subnet`, `DelegatedSubnet`, `PSA`, `VPCSC` `#D93025`, `Folder`), `EdgeCategoryColor`, badge styles, and the 13-`ServiceFamily` card-accent hex map so `internal/svg` and `internal/drawio` share one source of truth.
    *   `internal/svg/render.go`: implement `RenderSVG(page *layout.LayoutedPage, cat *catalog.Catalog) ([]byte, error)` emitting standalone UTF-8 SVG 1.1 with explicit numeric `width`, `height`, and `viewBox` attributes on `<svg>`, white background `<rect>`, `<defs>` arrow markers and deduplicated `<symbol id="icon-<key>" viewBox="...">` icons (rewriting internal `id="..."` and `url(#...)` tokens with `icon-<key>-` prefixes so `pubsub` filters/masks never collide), `<use href="#icon-<key>" width="28" height="28"/>`, container frames, accented service cards, `xml.EscapeText` `<text><tspan>` lines, orthogonal `6px`-rounded `<path>` polylines across `[StartPoint, ...Waypoints, EndPoint]`, `FooterGroups`, `LegendBox`, and `data-cell-id`/`data-node-uri`/`data-drill-target` attributes.
    *   `render_test.go`: assert well-formed XML, numeric `width`/`height` attributes, inline `<symbol>` defs with prefixed filter/mask IDs, zero `<foreignObject>` or `<script>` elements, XML-escaped special characters, and complete absence of `Unresolved Links` badges and `CoverageOK` rows when `AuditOverlays == false`.

#### 16. `internal/drawio/merge.go` and `internal/drawio/merge_reconcile.go` (Delete — Phase 5)

*   **Type / Scope:** Retirement of Stateful 3-Way `.drawio` XML Merger.
*   **Proposed Changes:** Delete `merge.go` (`707` lines) and `merge_reconcile.go` (`576` lines; `1,283` lines total), removing `MergeAndWriteDrawio` and `MergeOptions`.

#### 17. `internal/drawio/render.go`, `internal/drawio/styles.go`, `internal/drawio/xmlutil.go`, and `internal/drawio/drawio_test.go` (Modify — Phase 5)

*   **Type / Scope:** Stateless Single-View `.drawio` Exporter & Orphaned Merge Dead-Code Removal.
*   **Proposed Changes:**
    *   `render.go`: update `RenderDrawioXML(page *layout.LayoutedPage, cat *catalog.Catalog) ([]byte, error)` to emit a single uncompressed `<diagram id="<view-id>" name="<view-title>">` from `page` (`FooterGroups`, pre-wrapped `TitleLines`/`SubtitleLines` joined with `<br>`, parent-relative coordinates, `page.AuditOverlays` filtering) and add `WriteDrawioAtomic(path string, data []byte) error` delegating to `ioutil.WriteFileAtomic` (`0644`/`0755`); delete `TabManifest`, `ManifestEntry`, `marshalManifest`, `buildStagingContainerElement`, `staging*` constants, and `gcpArchManifest`/`gcpBadges`/`placeholders="1"` attributes.
    *   `styles.go`: delegate colors to `internal/theme`, inline badge HTML directly in `FormatCardLabel`, update `FormatLegendLabel` for `EdgeTierConfirmed`, and delete `IconSpec.Stencil` handling, `StagingContainerStyle`, `%gcpBadges%` placeholder indirection, and `removedInGCP`.
    *   `xmlutil.go`: delete merge-only helpers (`DecodeDiagramContent`, `EncodeDiagramContent`, `ParseStyleMap`, `CanonicalStyleHash`, `NormalizeLabel`, `CanonicalWaypointsHash`, `IsManagedCell`, `ExtractGeometry`, `ExtractWaypoints`, `StripWaypoints`).
    *   `drawio_test.go`: update `TestStyleHelpersAllVariants` (removing `Stencil` branch) and replace 3-way merge tests with stateless single-view XML validity, geometry parity against `LayoutedPage`, and presentation vs. `AuditOverlays` assertions.

#### 18. `internal/explore/server.go`, `internal/explore/assets/index.html`, `internal/explore/assets/app.css`, `internal/explore/assets/app.js`, and `internal/explore/server_test.go` (Create — Phase 5)

*   **Type / Scope:** Loopback-Hardened Local Browser Explorer (`embed.FS` Vanilla UI + Typed REST API).
*   **Proposed Changes:**
    *   `server.go`: hold immutable `baseGraph *ir.Graph`, `inv *ir.RawInventory`, and `cat *catalog.Catalog` alongside `sync.RWMutex`-protected `decisions *compiler.DecisionsFile` and active cloned `activeGraph *ir.Graph`; validate `netip.Addr.IsLoopback()` on `--listen`; generate a 32-byte `crypto/rand` hex token and expose `URL() string` (`"http://" + boundAddrPort.String() + "/#token=" + token`); enforce `Host == boundAddrPort.String()`, `Cross-Origin-Resource-Policy: same-origin`, `X-Content-Type-Options: nosniff`, `Content-Security-Policy: default-src 'self'; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'`, zero `CORS` headers, and constant-time `X-GCP-Arch-Token` verification on all 6 `/api/*` routes:
        *   `GET /api/views` -> JSON `{views: []layout.ViewSummary}`.
        *   `GET /api/view?<ViewSpecQuery>` -> JSON `{spec, title, box_count, svg: string, nodes: map[cellID]InspectorNode, edges: map[cellID]InspectorEdge, warnings: []ir.CoverageEntry, confirmed_edges: []compiler.ConfirmedEdgeRule}` (`InspectorNode` carries `NodeURI`, `DisplayName`, `DisplayType`, `ProjectID`, `Zone`, `CollapsedAssets`, `MemberURIs`, `UnresolvedCandidates`, `Badges`).
        *   `GET /api/search?q=<substr>` -> JSON `{results: []SearchHit}` scanning `activeGraph.Nodes`, `CollapsedAssets`, `Projects`, `Folders`, and `Organizations` with target `ViewSpec` query strings.
        *   `GET /api/explain?uri=<uri>` -> JSON `ir.ProvenanceRecord` (`404` when unknown).
        *   `GET /api/export?format=svg|drawio&<ViewSpecQuery>` -> raw `image/svg+xml` or `application/xml` attachment bytes.
        *   `POST /api/decisions` -> accepts JSON `{action: "confirm"|"unconfirm", edge: ConfirmedEdgeRule}`, updates/saves `.gcp-arch/decisions.yaml` via `SaveDecisions`, deep-clones `baseGraph` under write lock, runs `ApplyDecisions` + `AssignZones`, and returns `{confirmed_edges: df.ConfirmedEdges}` (`200 OK`), after which `app.js` re-fetches `GET /api/view?<currentSpec>`.
    *   `assets/{index.html,app.css,app.js}` (`//go:embed assets/*`): read `#token=` into `sessionStorage` and clear `location.hash` via `history.replaceState`; render breadcrumbs, view selector, search box, pan/zoom SVG viewport, inspector drawer with 1-click `Tier 3` confirm/unconfirm checkboxes, `Show Standalone` / `Audit Overlays` toggles, authenticated `fetch` -> `Blob` downloads for `.svg`/`.drawio`, and offscreen HTML5 `<canvas>` `2x`/`3x` `.png` export (clamping `effectiveScale = min(scale, 32767/max(w,h), sqrt(268e6/(w*h)))` before `drawImage` + `toBlob`).
    *   `server_test.go`: verify non-loopback bind rejection (`127.0.0.1` and `[::1]` accepted, `0.0.0.0` rejected), forged `Host` `403`, missing/invalid `X-GCP-Arch-Token` `403`, zero `Access-Control-Allow-Origin` header, `< 500 ms` `/api/view` response, folded-URI `/api/search` discovery, and `POST /api/decisions` confirm + unconfirm round-trip restoring `baseGraph` state cleanly.

#### 19. `cmd/gcp-arch/generate.go` (Delete — Phase 5), `cmd/gcp-arch/snapshot.go`, `cmd/gcp-arch/views.go`, `cmd/gcp-arch/explore.go`, `cmd/gcp-arch/export.go` (Create — Phase 5), `cmd/gcp-arch/main.go`, `cmd/gcp-arch/preflight.go`, `cmd/gcp-arch/inspect.go`, `cmd/gcp-arch/main_test.go`, and `cmd/gcp-arch/e2e_test.go` (Modify — Phase 1 & Phase 5)

*   **Type / Scope:** Snapshot-First CLI Subcommands, Shared Offline Compiler Flags & End-to-End Test Suite.
*   **Proposed Changes:**
    *   Delete `cmd/gcp-arch/generate.go` and the dual live/snapshot `loadInventoryAndCompile` path in `main.go`.
    *   `main.go`: register `preflight`, `snapshot`, `views`, `explore`, `export`, and `inspect`; map `*ingest.SnapshotSchemaError` and `*compiler.DecisionsSchemaError` to `exitUsage` (`2`); inject `openBrowser func(url string) error` into `appDeps` (defaulting to OS launcher unless `--no-open`, overridden with a no-op recorder in unit/E2E tests); define shared `offlineCompileFlags` (`--decisions`, default `.gcp-arch/decisions.yaml`; `--catalog-dir`; `--project-alias`) and `compileSnapshot(snapPath, flags)` merging `inv.Aliases` with CLI `--project-alias` overrides, running `CompileNodes` -> `ResolveEdges` -> `ApplyDecisions` -> `AssignZones`.
    *   `snapshot.go`: implement `gcp-arch snapshot` requiring at least one of `--org`, `--folder`, or `--project`, supporting `--quota-project`, `--project-alias <NUMBER=ID>` (persisted to `inv.Aliases`), `--catalog-dir`, `-q/--quiet`, and `-o/--out` (default `<first-scope>.snap`, or `<first-scope>-plus-<N>.snap` when multiple scopes are passed), streaming 1 Hz progress to `stderr` and printing the saved path + asset/node summary to `stdout`.
    *   `views.go`: implement `gcp-arch views <snap>` with `offlineCompileFlags` and `--json`, printing aligned table or JSON `[]layout.ViewSummary`.
    *   `explore.go`: implement `gcp-arch explore <snap>` with `offlineCompileFlags`, `--listen` (default `127.0.0.1:0`), and `--no-open`, printing `srv.URL()` to `stdout`.
    *   `export.go`: implement `gcp-arch export <snap>` with `offlineCompileFlags`, required `--level` (`l0|l1|l2`) and `-o/--out` (`exit 2` if omitted), optional selectors (`--root`, `--hub`, `--project`, `--focus`, `--hops` default `2`, `--component` default `-1`), view toggles (`--expand-standalone`, `--audit-overlays`), and `--format` (`svg|drawio`, default `svg`; unsupported formats return `exit 2`).
    *   `inspect.go`: accept a single required positional `<snap>` argument (`exit 2` on missing/extra args; legacy `--from-snapshot` and live scope flags removed) with `offlineCompileFlags`, `--json`, and `--explain <uri>` (printing `ir.ProvenanceRecord` as table or JSON, returning `exit 1` when `ExplainURI` returns `false`).
    *   `main_test.go`, `e2e_test.go`, and `testdata/enterprise_reference.snap.json` (Phase 5): expand `enterprise_reference.snap.json` with in-scope `shared-vpc-host` (`HOST`), unused lab subnet, and `networkUser` bindings, and test CLI flag validation (`exit 2`), `v1` snapshot rejection (`exit 2`), full `snapshot -> views -> inspect --explain -> explore (confirm/unconfirm Tier 3 + security rejections) -> export (svg, drawio)` lifecycle, and 1,500-project synthetic budget/geometry assertions.

#### 20. `README.md` (Modify — Phase 5)

*   **Type / Scope:** User & Operations Documentation.
*   **Proposed Changes:** Replace two-tab `generate` and Day-2 `.drawio` merge sections with the snapshot-first workflow (`preflight`, `snapshot`, `views`, `explore`, `export`, `inspect --explain`), document `roles/resourcemanager.folderViewer` for `--org`/`--folder` display names and `--project-alias <NUMBER=ID>`, document `L0`/`L1`/`L2` `<= 40`-box navigation, `.gcp-arch/decisions.yaml` `confirmed_edges` curation, browser `<canvas>` `2x`/`3x` `.png` export, and inline `svg:` custom catalog icon examples.

### Verification Plan

*   **Static Binary Build:** `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/gcp-arch ./cmd/gcp-arch && go version -m bin/gcp-arch | grep CGO_ENABLED=0`
*   **Unit & Race Detector Suite:** `CGO_ENABLED=1 go test -race -count=1 ./internal/... ./cmd/...`
*   **1,500-Project / 150,000-Asset Heap Ceiling Exam:** `CGO_ENABLED=0 go test -count=1 -run TestOrgScale1500ProjectsHeapCeiling ./internal/ingest` (calling `runtime.GC()` and asserting post-GC retained `runtime.MemStats.HeapAlloc < 512<<20`, preserved GCE/GKE/IAM/`TargetSslProxy`/`TargetVpnGateway` assets and `Organization`/`Folder` names, and non-fatal handling of child-project `SERVICE_DISABLED` errors).
*   **Layout Geometry & `<= 40`-Box Budget Exam:** `CGO_ENABLED=1 go test -race -count=1 -run TestMultiLevelGeometryInvariants ./internal/layout` (asserting `MaxViewBoxes = 40` across 150-project flat folders, 200-spoke Shared VPC Hubs, and 85-node shared-backbone monoprojects; `len(ViewGraph.Edges) == len(LayoutedPage.Edges)`; strictly monotonic X order on `Storage -> Eventarc -> Cloud Run -> SQL`; disjoint `BandVPC`/`BandManaged` `ContainerBox` envelopes; and zero non-adjacent edge/card/`44px`-header intersections).
*   **Static Analysis & Linting:** `go vet ./...` (zero warnings or errors).
*   **Statement Coverage Gate:** `go test -coverprofile=coverage.out ./internal/... ./cmd/... && go tool cover -func=coverage.out` achieving `>= 85%` statement coverage across `./internal/...` and `./cmd/...`.
*   **End-to-End Multi-Surface Exam:** `CGO_ENABLED=1 go test -race -v -run TestEnterpriseEndToEnd ./cmd/gcp-arch` verifying `snapshot -> views -> explore (POST /api/decisions Tier 3 confirm/unconfirm + DNS-rebinding/token rejection) -> export (.svg & .drawio) -> inspect --explain` with byte-identical deterministic outputs, inline `<symbol>` icons, numeric `<svg width height>` attributes, and zero `<foreignObject>` tags.

--------------------------------------------------------------------------------

<!-- Provenance: Indicates initial structural drafting via EGM. -->

*Drafted with EGM*
