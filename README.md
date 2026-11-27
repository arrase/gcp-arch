# `gcp-arch` — Semantic Multi-View GCP Architecture Pipeline

[![Quality Gate Status](https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&metric=alert_status)](https://sonarcloud.io/summary/overall?id=arrase_gcp-arch)
[![Coverage](https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&metric=coverage)](https://sonarcloud.io/summary/overall?id=arrase_gcp-arch)
[![Bugs](https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&metric=bugs)](https://sonarcloud.io/summary/overall?id=arrase_gcp-arch)
[![Vulnerabilities](https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&metric=vulnerabilities)](https://sonarcloud.io/summary/overall?id=arrase_gcp-arch)
[![Code Smells](https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&metric=code_smells)](https://sonarcloud.io/summary/overall?id=arrase_gcp-arch)
[![Technical Debt](https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&metric=sqale_index)](https://sonarcloud.io/summary/overall?id=arrase_gcp-arch)
[![Duplicated Lines (%)](https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&metric=duplicated_lines_density)](https://sonarcloud.io/summary/overall?id=arrase_gcp-arch)
[![Security Rating](https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&metric=security_rating)](https://sonarcloud.io/summary/overall?id=arrase_gcp-arch)
[![Reliability Rating](https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&metric=reliability_rating)](https://sonarcloud.io/summary/overall?id=arrase_gcp-arch)
[![Maintainability Rating](https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&metric=sqale_rating)](https://sonarcloud.io/summary/overall?id=arrase_gcp-arch)
[![Lines of Code](https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&metric=ncloc)](https://sonarcloud.io/summary/overall?id=arrase_gcp-arch)

`gcp-arch` is a standalone static Go binary (`CGO_ENABLED=0`) that generates page-fitted Google Cloud Platform (GCP) architecture deliverables — vector `.pdf` reports (1:1 per-view pagination), high-DPI Retina `.png` images (adaptive up to `2.0x`) for Google Docs, and self-contained offline `.html` dossiers — from a Git-tracked `architecture.yaml` manifest.

It runs as a strictly linear three-stage pipeline across five CLI subcommands (`preflight`, `extract`, `synthesize`, `validate`, `render`); see the architecture specifications in [`docs/designs/diagram-redesign-architecture.md`](docs/designs/diagram-redesign-architecture.md) and [`docs/designs/semantic-multi-view-pipeline.md`](docs/designs/semantic-multi-view-pipeline.md):

1. **Stage 1 (`extract` → `facts.json`, [`internal/extract/`](internal/extract/)):** Collects rate-limited, read-only multi-project metadata from Cloud Asset Inventory (CAI), IAM policies, GKE (`Ingress`, `Service` NEG/L4 annotations, workloads, Workload Identity), in-stream Cloud Build triggers and top-`N` succeeded builds, Cloud Monitoring runtime traffic (including Vertex AI `PublisherModel` metrics `model_invocation_count` and `consumed_token_throughput` when `aiplatform.googleapis.com` is enabled, synthesizing `aiplatform.googleapis.com/PublisherModel` resources and `MONITORING_TRAFFIC` edges), active Dataflow jobs, and BigQuery SQL lineage into a credential-scrubbed `v3` `facts.json` (`0600`, gitignored).
2. **Stage 2 (`synthesize` → `architecture.yaml`, [`internal/synth/`](internal/synth/), [`internal/manifest/`](internal/manifest/)):** Builds a deterministic GCP containment skeleton in Go (`<= 500` active resources, with 1:1 plumbing folds, homogeneous cohorts, transversal separation, topological ranking, and barycenter ordering), enriches it via a single bounded Vertex AI Gemini 4 REST call (`cloud.google.com/go/auth`, billed to a separate `--vertex-project`) into `View 0` (`view_0_system_index`) plus stage-partitioned detail views (`layered_flow`), and verifies 100% resource/edge conservation and directed-walk `FactEdge` provenance.
3. **Stage 3 (`render` → `.pdf`, Retina `.png`, `.html`, [`internal/render/`](internal/render/), [`internal/icons/`](internal/icons/)):** Renders `architecture.yaml` completely offline (zero network calls, zero `facts.json` dependency) using embedded `Inter` and `JetBrains Mono` `.woff2` fonts, inline SVG `<symbol>` service icons, nested Flexbox containers (`layered_flow`) and CSS Grid archetypes, an in-page DOM-measured Hanan-grid A* orthogonal router with zero-collision checks, streaming scanline PNG verification (`< 1 MB` Go RAM), and a single isolated local headless Chrome/Chromium session via `chromedp`.

---

## Build & Setup

Requires **Go 1.26+**:

```bash
CGO_ENABLED=0 go build -o ./bin/gcp-arch ./cmd/gcp-arch
```

Verify static build metadata:

```bash
go version -m ./bin/gcp-arch | grep CGO_ENABLED=0
```

Authenticate locally with Application Default Credentials (ADC):

```bash
gcloud auth application-default login
```

### Chrome / Chromium Setup (for `render`)

`gcp-arch render` locates a local Chrome or Chromium binary automatically in this order:

1. `--chrome-path <BINARY>` (fails fast with exit code `1` if the specified path does not exist, for all formats including `--format html` alone)
2. `CHROME_PATH` environment variable (falls back to `config.yaml` if unset or pointing to a non-existent path)
3. `chrome_path` in `~/.config/gcp-arch/config.yaml` (falls back to `PATH` if unset or pointing to a non-existent path)
4. `PATH` (`google-chrome`, `google-chrome-stable`, `chromium`, `chromium-browser`)
5. Standard OS install locations (`/usr/bin/...` on Linux, `/Applications/Google Chrome.app/...` on macOS)

Chrome runs in an isolated ephemeral `--user-data-dir` (safe even when a desktop Chrome session is already open) with `--headless=new` and `--disable-background-networking`.

- **`pdf` and `png` exports** require local Chrome/Chromium (exit code `1` if absent).
- **`--format html` alone (without an explicit `--chrome-path`)** works even when Chrome/Chromium auto-discovery finds no binary: `gcp-arch` writes a self-contained unrouted `out/architecture.html` with the embedded JS router and prints a warning on `stderr` that CLI collision verification was skipped. Passing an explicit missing `--chrome-path` still fails fast with exit code `1`.

### User Configuration File (`~/.config/gcp-arch/config.yaml`)

To avoid repeating common flags across invocations, `gcp-arch` automatically reads `~/.config/gcp-arch/config.yaml` (or `$XDG_CONFIG_HOME/gcp-arch/config.yaml` when `XDG_CONFIG_HOME` is set) if the file exists (see [`cmd/gcp-arch/config.go`](cmd/gcp-arch/config.go)).

**Precedence:** `CLI flags` > `environment variables` (`CHROME_PATH` for Chrome binary discovery; `XDG_CONFIG_HOME` for config file location) > `config.yaml` > `built-in defaults`.

When present, `~/.config/gcp-arch/config.yaml` is strictly validated on **all** subcommands (including `validate`); unknown keys or invalid values fail fast with exit code `2`. List fields (`org`, `folder`, `projects`, and `format`) accept either a YAML list or a single scalar value. Passing any scope flag (`--org`, `--folder`, or `--projects`/`--project`) on the CLI completely replaces the default scopes from `config.yaml`; `--project-alias` flags overlay `project_aliases` from `config.yaml`.

```yaml
# ~/.config/gcp-arch/config.yaml
# org, folder, projects, and format accept either a YAML list or a single scalar.
org: []
folder: []
projects:
  - acme-cicd
project_aliases:
  999000111222: shared-vpc-host
quota_project: my-quota-proj
vertex_project: my-vertex-project
vertex_location: global
model: gemini-4-carbon
thinking_level: high       # minimal | low | medium | high | xhigh (omit for model default)
cloudbuild_runs: 3
chrome_path: /usr/bin/google-chrome
format:
  - pdf
  - png
  - html
```

---

## Separated Credentials & Minimum IAM Roles

`gcp-arch` isolates read-only target GCP inspection from Vertex AI synthesis billing:

- **Target scopes (`preflight`, `extract`):** Authenticate exclusively with `https://www.googleapis.com/auth/cloud-platform.read-only`. Secret Manager payloads, Kubernetes `Secret` / `ConfigMap` data, Cloud Storage object bytes, and BigQuery table rows are never retained in `facts.json`.
- **Vertex AI billing project (`--vertex-project` on `preflight`, `synthesize`):** Authenticates separately with `https://www.googleapis.com/auth/cloud-platform` (`roles/aiplatform.user`) against your billing/sandbox project. Only sanitized structural metadata (`facts.json` allowlisted attributes and edges) is sent to Vertex AI.

| Scope | Role | Purpose | Required |
| :--- | :--- | :--- | :--- |
| Target org / folder / project(s) | `roles/cloudasset.viewer` | CAI `RESOURCE` and scoped `IAM_POLICY` streaming (`<= 80` RPM), including in-stream Cloud Build `BuildTrigger` & `Build` folding | Required |
| Organization / Folder | `roles/resourcemanager.folderViewer` | Resolve human-readable `Folder` display names on `--org` / `--folder` scans | Recommended for org/folder |
| Target project(s) | `roles/monitoring.viewer` | 7-day `consumed_api` runtime traffic grouped by `resource.labels.method` (`<= 10` QPS); when `aiplatform.googleapis.com` is enabled, also queries Vertex AI `PublisherModel` metrics (`model_invocation_count`, `consumed_token_throughput`) to synthesize `aiplatform.googleapis.com/PublisherModel` resources and `MONITORING_TRAFFIC` edges ([`internal/extract/enrichers.go`](internal/extract/enrichers.go)) | Recommended |
| Target project(s) | `roles/bigquery.resourceViewer` | 7-day `bigquery.jobs.list` SQL lineage metadata (USD 0 SQL slot cost; raw SQL never read) | Recommended |
| Target project(s) | `roles/dataflow.viewer` | Active Dataflow job pipeline links (streaming and batch jobs, `<= 10` QPS, gated on enabled API) | Optional |
| Caller `--quota-project` | `roles/serviceusage.serviceUsageConsumer` | Quota attribution for target-scope read-only API calls (defaults to ADC `quota_project_id`) | Required on quota project |
| `--vertex-project` | `roles/aiplatform.user` | Vertex AI Gemini 4 calls (`aiplatform.endpoints.predict` + model metadata check) | Required for `synthesize` |

---

## CLI Command Reference

Root entrypoint ([`cmd/gcp-arch/main.go`](cmd/gcp-arch/main.go)):
- `gcp-arch --version`: Print the binary version (`dev` in local builds; release versions are injected at build time via `-ldflags "-X main.version=..."` in [`.github/workflows/release.yml`](.github/workflows/release.yml)).

### 1. `gcp-arch preflight`

Probes read-only target permissions (`cloudasset.assets.listResource`, `cai.folders` folder-name resolution via `roles/resourcemanager.folderViewer` on `--org` and `--folder` scopes, `monitoring.timeSeries.list`, `bigquery.jobs.list`, `dataflow.jobs.list`) and, when `--vertex-project` is provided, runs a zero-token Vertex AI IAM (`aiplatform.endpoints.predict`) and model availability probe ([`cmd/gcp-arch/preflight.go`](cmd/gcp-arch/preflight.go), [`internal/extract/client.go`](internal/extract/client.go)).

```bash
gcp-arch preflight --projects acme-cicd --vertex-project my-vertex-project
gcp-arch preflight --folder 555000111 --quota-project my-quota-proj --json
```

Flags:
- `--org <ID>` / `--folder <ID>` / `--projects <p1,p2>` (alias `--project`): Target scope(s); repeatable or comma-separated (at least one required).
- `--project-alias <NUM=ID>`: Repeatable mapping from project number to project ID.
- `--quota-project <ID>`: Explicit GCP quota project (defaults to ADC `quota_project_id`).
- `--vertex-project <ID>`: Optional Vertex AI billing project for zero-token readiness checks.
- `--vertex-location <LOC>`: Vertex AI location (default `global`).
- `--model <MODEL>`: Vertex AI model ID (default `gemini-4-carbon`).
- `-q, --quiet`: Suppress progress output on `stderr`.
- `--json`: Emit `PreflightReport` as JSON.

### 2. `gcp-arch extract`

Extracts multi-project structural facts into a deterministic, `0600`-permissioned `v3` `facts.json` file (inside a `0700` parent directory if newly created) and prints discovered out-of-scope deployment target projects to `stderr` ([`cmd/gcp-arch/extract.go`](cmd/gcp-arch/extract.go)).

```bash
gcp-arch extract --projects acme-cicd -o facts.json
gcp-arch extract --projects app-prod,data-prod --project-alias 999000111222=shared-vpc-host -o prod.facts.json
```

Flags:
- `--org <ID>` / `--folder <ID>` / `--projects <p1,p2>` (alias `--project`): Target scope(s) (at least one required).
- `--project-alias <NUM=ID>`: Repeatable project number-to-ID override.
- `--quota-project <ID>`: Explicit GCP quota project.
- `--cloudbuild-runs <N>`: Latest succeeded Cloud Build runs folded in-stream per trigger (default `3`).
- `-q, --quiet`: Suppress progress output and the stdout summary (actionable out-of-scope target hints and degraded coverage warnings are still printed to `stderr`).
- `-o, --out <FILE>`: Output path (default `facts.json`).

### 3. `gcp-arch synthesize <facts.json>`

Builds a deterministic GCP containment skeleton and enriches it via a single bounded Vertex AI Gemini 4 annotation call to produce a Git-tracked `architecture.yaml` (Day 1), or incrementally reconciles resource and edge deltas into an existing curated `architecture.yaml` (Day 2 via `--reconcile`; [`cmd/gcp-arch/synthesize.go`](cmd/gcp-arch/synthesize.go)).

**Active-Resource Ceiling (`<= 500`):** After plumbing and default-resource suppression, `synthesize` enforces a hard ceiling of `500` active `FactResource` entries per run. Exceeding `500` fails immediately with exit code `2` without calling Vertex AI; narrow `--projects` or `--folder` to a single application domain.

```bash
# Day 1: synthesize a new multi-view architecture.yaml
gcp-arch synthesize facts.json --vertex-project my-vertex-project -o architecture.yaml

# Day 2: reconcile drift in place (0 LLM calls in < 5 ms when unchanged; --vertex-project optional on fast path)
gcp-arch synthesize facts.json --reconcile architecture.yaml --vertex-project my-vertex-project
```

Flags:
- `--vertex-project <ID>`: Billing project for Vertex AI calls (required on Day 1; optional on Day 2 on the `0`-LLM zero-delta fast path).
- `--vertex-location <LOC>`: Vertex AI location (default `global`).
- `--model <MODEL>`: Vertex AI model ID (default `gemini-4-carbon`).
- `--thinking-level <LEVEL>`: Vertex AI thinking/effort level (`minimal`, `low`, `medium`, `high`, `xhigh`; omitted by default so Vertex AI uses the model default).
- `--reconcile <FILE>`: Existing `architecture.yaml` for Day-2 reconciliation. When `-o` is omitted, updates `<FILE>` atomically in place; when unchanged, leaves `<FILE>` untouched with zero diff.
- `-o, --out <FILE>`: Output manifest path (default `architecture.yaml`, or `--reconcile` path when omitted on Day 2).
- `-q, --quiet`: Suppress informational stdout summary.

### 4. `gcp-arch validate <architecture.yaml>`

Validates `architecture.yaml` strictly offline with zero network calls ([`cmd/gcp-arch/validate.go`](cmd/gcp-arch/validate.go)):

- **Without `--facts` (Git PR CI):** Enforces strict `yaml.v3` `KnownFields(true)` decoding, `schema_version: v1`, valid icon keys, 1:1 unique URI accounting across components/containers/`excluded_resources`, stateful datastore homogeneity (and forbids excluding stateful datastores), every component placed in `>= 1` detail view, `View 0` rules (`view_0_system_index` iff `2..16` detail views, disjoint view and component IDs), `1..12` placements per detail view (`1..16` on `View 0`), archetype column bounds (`0..4` for `landscape_matrix`/`stage_pipeline`, `0..2` for `hub_spoke`), structural container nesting depth `<= 2`, `len(verb) <= 16`, cross-view `(from, to, flow_kind)` verb/provenance consistency, single-source/single-target `via_bus_id`, and `rejected_edges` / `acknowledged_edges` integrity.
- **With `--facts <facts.json>` (Extraction/Artifact CI):** Additionally verifies 100% `FactsFile` resource accounting, checks that placements inside resource-backed containers (`VPC`, `GKE_CLUSTER`) match `FactResource.ParentURI` chains, and enforces that every detail-view `VERIFIED` edge's `evidence_edge_ids` form a connected non-heuristic directed walk in `facts.Edges` from `from.backed_by` to `to.backed_by` without shortcutting other components or containers (requiring `flow_kind: ADMIN` when backed solely by IAM/Workload Identity permissions).

```bash
# Clean-checkout Git PR CI (no facts.json needed)
gcp-arch validate architecture.yaml

# Full provenance & 100% resource coverage check
gcp-arch validate architecture.yaml --facts facts.json --json
```

Flags:
- `--facts <FILE>`: Optional `v3` `facts.json` for provenance and 100% resource coverage verification.
- `--json`: Emit `ValidationReport` JSON (`{"valid": true|false, "excluded_count": <N>, "errors": [...]}`; `"excluded_count"` is omitted when `0` and emitted whenever `excluded_resources` is non-empty, while text output appends `, <E> excluded` when exclusions exist).

### 5. `gcp-arch render <architecture.yaml>`

Validates `architecture.yaml` structurally and exports 1:1 per-view vector `out/architecture.pdf` (one A4 landscape page per view), adaptive up to `2.0x` Retina per-view `out/views/<00..NN>-<view_id>.png` (`8,192px` max dimension / `28M` px cap), and self-contained offline `out/architecture.html` (no `facts.json` required; [`cmd/gcp-arch/render.go`](cmd/gcp-arch/render.go)). Streaming scanline PNG pixel and geometry verification (`< 1 MB` Go RAM) runs automatically on every PNG render and fails with `*render.GeometryReportError` (exit code `2`) on any dimension, aspect-ratio, pixel-integrity, or layout violation.

```bash
gcp-arch render architecture.yaml --format pdf,png,html -o out/
gcp-arch render architecture.yaml --facts facts.json --geometry-report -o out/
gcp-arch render architecture.yaml --format html -o examples/platform-cicd/
```

Flags:
- `--format <LIST>`: Comma-separated subset of `pdf`, `png`, `html` (default `pdf,png,html`).
- `-o, --out <DIR>`: Output directory (default `out`).
- `--facts <FILE>`: Optional `v3` `facts.json` to include deterministic resource/edge conservation status in geometry verification.
- `--geometry-report`: Opt-in flag to write machine-readable `geometry_report.json` alongside rendered PNGs (streaming PNG pixel & geometry checks run automatically even when omitted).
- `--chrome-path <BINARY>`: Explicit path to Chrome or Chromium executable.
- `-q, --quiet`: Suppress informational stdout summary.

---

## Exit Codes

| Exit Code | Meaning |
| :--- | :--- |
| `0` | Success. |
| `1` | Runtime or external I/O failure: ADC/GCP authentication or quota error, Vertex AI RPC error, missing input file, or missing Chrome/Chromium binary (when `pdf`/`png` is requested, or when an explicit `--chrome-path` does not exist). |
| `2` | Usage, schema, scale, validation, or layout/geometry failure: invalid CLI flags/args, `> 500` active resources (`ScaleLimitError`), invalid YAML or unknown fields (`KnownFields`), `validate` rule/provenance/coverage violation (`ValidationError`), unresolved card/badge/pill/wire/text collisions (`LayoutCollisionError`), or PNG pixel/aspect-ratio/conservation failures (`GeometryReportError`). |

---

## `architecture.yaml` Schema & Day-2 Curation

`architecture.yaml` (`schema_version: v1`) is the Git-tracked source of truth. Every `backed_by` and `excluded_resources` entry carries `{uri, asset_type, project_id}` so `validate` and `render` work standalone without `facts.json`. See [`examples/platform-cicd/architecture.yaml`](examples/platform-cicd/architecture.yaml) and [`examples/README.md`](examples/README.md) for complete multi-view reference bundles.

```yaml
schema_version: v1
metadata:
  generated_at: "2026-11-23T02:00:00Z"
  scopes:
    - projects/acme-cicd
components:
  - id: atlantis
    title: Atlantis PR Automation
    subtitle: gke:cicd / ns:atlantis
    icon_key: k8s_workload
    component_kind: WORKLOAD
    project_id: acme-cicd
    notes: Curated title for client handoff   # Preserved across Day-2 --reconcile
    backed_by:
      - uri: //container.googleapis.com/projects/acme-cicd/locations/europe-southwest1-b/clusters/cicd/k8s/namespaces/atlantis/apps/deployments/atlantis
        asset_type: apps.k8s.io/Deployment
        project_id: acme-cicd
    inline_meta:
      - label: Service Account
        value: atlantis@acme-cicd.iam
containers:
  - id: gke_cicd
    title: "GKE Cluster: cicd"
    subtitle: europe-southwest1-b
    kind: GKE_CLUSTER
    parent_container_id: proj_acme_cicd
    backed_by:
      - uri: //container.googleapis.com/projects/acme-cicd/locations/europe-southwest1-b/clusters/cicd
        asset_type: container.googleapis.com/Cluster
        project_id: acme-cicd
excluded_resources: []
rejected_edges:
  - from: atlantis
    to: sonarqube_db
    reason: Suppress unrelated monitoring correlation
acknowledged_edges:                           # Undrawn projected pairs settled across Day-2 --reconcile
  - from: cloudbuild_triggers
    to: atlantis
views:
  - id: gke_developer_platform
    title: GKE Developer Platform & Ingress
    archetype: landscape_matrix               # landscape_matrix | stage_pipeline | hub_spoke
    accent_mode: RUNTIME                      # RUNTIME | CICD | MIXED
    container_ids: [proj_acme_cicd, gke_cicd]
    placements:
      - component_id: atlantis
        container_id: gke_cicd
        column: 1
    edges:
      - from: atlantis
        to: acme_tfstate
        verb: locks tfstate                   # <= 16 chars
        flow_kind: CICD                       # RUNTIME | CICD | ADMIN
        provenance: VERIFIED                  # VERIFIED | INFERRED
        evidence_edge_ids: ["c21876e6fb659711"]
```

### Day-2 Reconciliation (`--reconcile`)

When your environment changes after teams have edited titles, subtitles, or `notes` in Git:

1. Re-run `gcp-arch extract --projects <p> -o facts.json`.
2. Run `gcp-arch synthesize facts.json --reconcile architecture.yaml --vertex-project <ai-proj>` ([`internal/synth/reconcile.go`](internal/synth/reconcile.go)):
   - **Zero-delta fast path (all active URIs and functional/transversal edges match):** completes in `< 5 ms` with `0` Vertex AI calls (even without `--vertex-project`), refreshes `VERIFIED` edge provenance in place (downgrading vanished transient lineage to `INFERRED` with rationale footnote rather than deleting it), validates with `ValidateWithFactsAndConservation`, and leaves `architecture.yaml` untouched on disk when canonical YAML is unchanged.
   - **Topology or edge delta detected:** runs the unified synthesis pipeline (deterministic containment skeleton + single bounded Vertex AI Gemini 4 call with automatic transient HTTP `429`/`503`/`504` retries in [`internal/synth/client.go`](internal/synth/client.go)), preserves curated human edits (`title`, `subtitle`, `notes` on matching components and `title`, `subtitle`, `focus_summary` on matching views), verifies 100% resource and functional-edge conservation, and atomically writes `architecture.yaml`.
3. Re-render via `gcp-arch render architecture.yaml -o out/`.

---

## Architecture & Source Map

Design specifications: [`docs/designs/diagram-redesign-architecture.md`](docs/designs/diagram-redesign-architecture.md) and [`docs/designs/semantic-multi-view-pipeline.md`](docs/designs/semantic-multi-view-pipeline.md)

| Area | Source | Responsibility |
| :--- | :--- | :--- |
| CLI & Config | [`cmd/gcp-arch/main.go`](cmd/gcp-arch/main.go), [`cmd/gcp-arch/config.go`](cmd/gcp-arch/config.go) | Cobra root (`--version`), exit-code classification, and `~/.config/gcp-arch/config.yaml` defaults |
| Subcommands | [`preflight.go`](cmd/gcp-arch/preflight.go), [`extract.go`](cmd/gcp-arch/extract.go), [`synthesize.go`](cmd/gcp-arch/synthesize.go), [`validate.go`](cmd/gcp-arch/validate.go), [`render.go`](cmd/gcp-arch/render.go) | Flag wiring, config fallback, and output formatting for the five CLI commands |
| Stage 1 (`extract`) | [`internal/extract/`](internal/extract/) | Read-only CAI, IAM, GKE, Cloud Build, Cloud Monitoring (`consumed_api` & Vertex `PublisherModel`), BigQuery, and Dataflow extraction + preflight probes |
| Stage 2 (`synthesize`) | [`internal/synth/`](internal/synth/) | Deterministic containment skeleton (`skeleton.go`, `containers.go`, `edges.go`, `ranking.go`, `assets.go`), direct Vertex AI REST client & bounded annotator (`synth.go`, `client.go`, `annotate.go`), and Day-2 `--reconcile` (`reconcile.go`) |
| Manifest & Validation | [`internal/manifest/`](internal/manifest/) | `architecture.yaml` (`v1`) schema, strict `KnownFields` YAML codec, deterministic conservation (`conservation.go`), and structural/provenance validation |
| Stage 3 (`render`) | [`internal/render/`](internal/render/) | Offline HTML/CSS/SVG/JS generation (`prepare.go`, `html.go`), Hanan-grid A* orthogonal router, headless Chrome (`chromedp`) PDF/PNG export (`render.go`, `chrome.go`), and streaming PNG pixel & geometry verification (`geometry.go`, `pngcheck.go`) |
| Embedded Icons | [`internal/icons/`](internal/icons/) | Embedded GCP and third-party SVG icon registry (`icons.yaml`) |
| Atomic File I/O | [`internal/atomicfile/`](internal/atomicfile/) | Crash-safe atomic writes with explicit file and directory permissions |

---

## Examples

Complete rendered multi-view reference bundles (6 complex architectures spanning **49 GCP APIs** and **75 unique asset types**, all verified with `conservation=PASS`, `view0=1.00`, and automated geometry & streaming PNG pixel verification) are published under [`examples/`](examples/README.md) (see [`examples/README.md`](examples/README.md) for previews, paired `v3` facts fixtures, and regeneration commands):

- **[`examples/platform-cicd/`](examples/platform-cicd/):** Multi-Project CI/CD, GKE DevOps & PSA Network (`17` components, `3` views).
- **[`examples/retail-cloud-native/`](examples/retail-cloud-native/):** Cloud-Native E-Commerce Runtime, Data/AI Streaming & Shared VPC (`18` components, `3` views).
- **[`examples/fintech-global-payments/`](examples/fintech-global-payments/):** Global FinTech Payments, Hybrid NCC Transit (Interconnect/HA VPN), PSC Fabrics & Multi-Engine Ledger (`31` components, `4` views).
- **[`examples/enterprise-lakehouse-mlops/`](examples/enterprise-lakehouse-mlops/):** Enterprise CDC Lakehouse (Kafka, Data Fusion, Datastream, Composer, Dataflow, Dataproc, Dataform, Dataplex, BigQuery) & Vertex AI MLOps (`30` components, `4` views).
- **[`examples/agentic-ai-document-platform/`](examples/agentic-ai-document-platform/):** Agentic AI (Vertex AI Reasoning Engine, Vector Search, Discovery Engine, Dialogflow CX, Gemini) & Document AI OCR Pipeline (`25` components, `3` views).
- **[`examples/enterprise-zero-trust-mesh/`](examples/enterprise-zero-trust-mesh/):** Multi-Region Zero-Trust Mesh (Shared VPC, Private GKE, Cloud Deploy, App Engine, Cloud Batch, Cloud Run, AlloyDB/Cloud SQL PSA & Risk AI) (`26` components, `3` views).
