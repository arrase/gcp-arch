---
hide:
  - toc
---

<div class="hero-section">
  <div class="hero-eyebrow">
    <i class="fa-solid fa-diagram-project" aria-hidden="true"></i>
    <span>Semantic Multi-View GCP Architecture Pipeline</span>
  </div>
  <h1>GCP Arch</h1>
  <p class="hero-subtitle">
    A standalone static Go binary (<code>CGO_ENABLED=0</code>) that extracts read-only multi-project Google Cloud metadata, synthesizes evidence-backed multi-view architectures with Gemini 4 on Vertex AI, and renders offline 1:1 per-view vector PDF reports, adaptive up to 2.0x Retina PNG images, and self-contained HTML dossiers.
  </p>
  <div class="hero-badges">
    <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.26%2B-00ADD8.svg?logo=go&amp;logoColor=white" alt="Go 1.26+"></a>
    <a href="https://github.com/arrase/gcp-arch/releases"><img src="https://img.shields.io/badge/Build-CGO__ENABLED%3D0-4f46e5.svg" alt="Static Binary"></a>
    <a href="https://cloud.google.com/vertex-ai"><img src="https://img.shields.io/badge/Synthesis-Vertex%20AI%20Gemini%204-4285F4.svg?logo=googlecloud&amp;logoColor=white" alt="Vertex AI Gemini 4"></a>
    <a href="rendering/"><img src="https://img.shields.io/badge/Render-100%25%20Offline%20Chrome-10b981.svg" alt="Offline Rendering"></a>
  </div>
  <div class="hero-badges">
    <a href="https://sonarcloud.io/summary/overall?id=arrase_gcp-arch"><img src="https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&amp;metric=alert_status" alt="Quality Gate Status"></a>
    <a href="https://sonarcloud.io/summary/overall?id=arrase_gcp-arch&amp;metric=coverage" alt="Coverage"><img src="https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&amp;metric=coverage" alt="Coverage"></a>
    <a href="https://sonarcloud.io/summary/overall?id=arrase_gcp-arch"><img src="https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&amp;metric=bugs" alt="Bugs"></a>
    <a href="https://sonarcloud.io/summary/overall?id=arrase_gcp-arch"><img src="https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&amp;metric=vulnerabilities" alt="Vulnerabilities"></a>
    <a href="https://sonarcloud.io/summary/overall?id=arrase_gcp-arch"><img src="https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&amp;metric=code_smells" alt="Code Smells"></a>
    <a href="https://sonarcloud.io/summary/overall?id=arrase_gcp-arch"><img src="https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&amp;metric=sqale_index" alt="Technical Debt"></a>
    <a href="https://sonarcloud.io/summary/overall?id=arrase_gcp-arch"><img src="https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&amp;metric=duplicated_lines_density" alt="Duplicated Lines (%)"></a>
    <a href="https://sonarcloud.io/summary/overall?id=arrase_gcp-arch"><img src="https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&amp;metric=security_rating" alt="Security Rating"></a>
    <a href="https://sonarcloud.io/summary/overall?id=arrase_gcp-arch"><img src="https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&amp;metric=reliability_rating" alt="Reliability Rating"></a>
    <a href="https://sonarcloud.io/summary/overall?id=arrase_gcp-arch"><img src="https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&amp;metric=sqale_rating" alt="Maintainability Rating"></a>
    <a href="https://sonarcloud.io/summary/overall?id=arrase_gcp-arch"><img src="https://sonarcloud.io/api/project_badges/measure?project=arrase_gcp-arch&amp;metric=ncloc" alt="Lines of Code"></a>
  </div>
</div>

<div class="section-header">
  <h2>Key Features</h2>
  <span>Evidence-backed discovery, closed-world synthesis &amp; print-ready output</span>
</div>

<div class="projects-grid">
  <div class="feature-card">
    <div>
      <i class="fa-solid fa-magnifying-glass-chart feature-icon" aria-hidden="true"></i>
      <h3>Read-Only Multi-Project Discovery</h3>
      <p>Streams <code>ReadTime</code>-pinned Cloud Asset Inventory, scoped IAM policies, GKE Ingress/NEG/Workload Identity, Cloud Build triggers &amp; runs, Cloud Monitoring traffic (including Vertex AI <code>PublisherModel</code> metrics), BigQuery lineage, and active Dataflow jobs.</p>
    </div>
  </div>
  <div class="feature-card">
    <div>
      <i class="fa-solid fa-wand-magic-sparkles feature-icon" aria-hidden="true"></i>
      <h3>Deterministic Skeleton + Gemini 4 Annotation</h3>
      <p>Builds a deterministic GCP containment skeleton in Go (<code>&le; 500</code> active resources) and enriches it via a single bounded Vertex AI REST call—enforcing 100% resource/edge conservation and directed-walk provenance on every <code>VERIFIED</code> edge.</p>
    </div>
  </div>
  <div class="feature-card">
    <div>
      <i class="fa-solid fa-arrows-rotate feature-icon" aria-hidden="true"></i>
      <h3>Deterministic View 0 &amp; Day-2 Drift</h3>
      <p>Constructs <code>view_0_system_index</code> deterministically in Go and reconciles Day-2 infrastructure drift in place—preserving curated titles, subtitles, and <code>notes</code> with a <code>0</code>-LLM fast path in <code>&lt; 5 ms</code> when unchanged.</p>
    </div>
  </div>
  <div class="feature-card">
    <div>
      <i class="fa-solid fa-file-pdf feature-icon" aria-hidden="true"></i>
      <h3>Offline PDF, Retina PNG &amp; HTML Dossiers</h3>
      <p>Compiles Git-tracked <code>architecture.yaml</code> completely offline with zero network calls and zero <code>facts.json</code> dependency, using embedded <code>Inter</code> and <code>JetBrains Mono</code> fonts, 85 inline SVG icons, and 1:1 per-view vector PDF pagination.</p>
    </div>
  </div>
  <div class="feature-card">
    <div>
      <i class="fa-solid fa-diagram-project feature-icon" aria-hidden="true"></i>
      <h3>Zero-Collision A* Router &amp; Streaming PNG Audit</h3>
      <p>Nested Flexbox containers (<code>layered_flow</code>) paired with a DOM-measured Hanan-grid A* orthogonal router, jump-arc bridges, strict collision checks, and streaming scanline PNG pixel verification (<code>&lt; 1 MB</code> Go RAM).</p>
    </div>
  </div>
  <div class="feature-card">
    <div>
      <i class="fa-solid fa-shield-halved feature-icon" aria-hidden="true"></i>
      <h3>Credential Isolation &amp; Sanitization</h3>
      <p>Strictly separates <code>cloud-platform.read-only</code> target discovery from <code>--vertex-project</code> billing, scrubs secrets via per-asset allowlists, and writes <code>0600</code> gitignored <code>facts.json</code> atomically.</p>
    </div>
  </div>
</div>

---

## Quick Installation

Build a zero-dependency static binary from source (requires **Go 1.26+**) or download a pre-built cross-platform binary from [GitHub Releases](https://github.com/arrase/gcp-arch/releases):

=== "Build from Source (Recommended)"

    ```bash
    git clone https://github.com/arrase/gcp-arch.git
    cd gcp-arch

    # Build a fully static binary (CGO_ENABLED=0)
    CGO_ENABLED=0 go build -o ./bin/gcp-arch ./cmd/gcp-arch

    # Verify static compilation metadata
    go version -m ./bin/gcp-arch | grep CGO_ENABLED=0
    ```

=== "Pre-Built Release Binaries"

    Static release binaries are published whenever a GitHub Release is published ([`.github/workflows/release.yml`](https://github.com/arrase/gcp-arch/blob/main/.github/workflows/release.yml)) for five targets (`linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`):

    ```bash
    # Example: Linux amd64
    curl -fL "https://github.com/arrase/gcp-arch/releases/latest/download/gcp-arch-linux-amd64" -o gcp-arch
    chmod +x gcp-arch
    ./gcp-arch --version
    ```

---

## Prerequisites

1. **Google Cloud Application Default Credentials (ADC)** *(for `preflight`, `extract`, and `synthesize`)*:
   ```bash
   gcloud auth application-default login
   ```
2. **Read-Only Target IAM & Separate Vertex AI Project**:
   - Target scopes need `roles/cloudasset.viewer` (plus recommended read-only roles for Monitoring, BigQuery, Dataflow, and Folder names).
   - Synthesis bills to a separate `--vertex-project` with `roles/aiplatform.user`. See [Security, IAM & Preflight](security_iam.md) for the exact permission matrix.
3. **Local Chrome or Chromium** *(for `render` PDF/PNG export & CLI collision/pixel verification)*:
   - Auto-discovered via `--chrome-path`, `CHROME_PATH`, `config.yaml`, `PATH`, or standard Linux/macOS install locations ([`internal/render/chrome.go`](https://github.com/arrase/gcp-arch/blob/main/internal/render/chrome.go)).
   - Runs in an isolated ephemeral `--user-data-dir` with `--headless=new` and `--disable-background-networking`.
   - `--format html` alone works even without Chrome/Chromium when `--chrome-path` is unset and auto-discovery finds no binary (writing a self-contained HTML dossier with a warning that CLI collision verification was skipped); passing an explicit nonexistent `--chrome-path` still fails fast with exit code `1` ([`cmd/gcp-arch/render.go`](https://github.com/arrase/gcp-arch/blob/main/cmd/gcp-arch/render.go#L153-L155)).

---

## 60-Second End-to-End Quick Start

`gcp-arch` runs as a strictly linear three-stage pipeline across five CLI subcommands:

```bash
# 1. Verify read-only target IAM + zero-token Vertex AI model availability
gcp-arch preflight --projects acme-cicd --vertex-project my-vertex-project

# 2. Stage 1: Extract sanitized v3 structural & runtime facts (0600, gitignored)
gcp-arch extract --projects acme-cicd -o facts.json

# 3. Stage 2: Synthesize Git-tracked multi-view architecture.yaml via Gemini 4
gcp-arch synthesize facts.json --vertex-project my-vertex-project -o architecture.yaml

# 4. Validate offline structure + 100% resource coverage & directed-walk provenance
gcp-arch validate architecture.yaml --facts facts.json

# 5. Stage 3: Render offline 1:1 vector PDF, adaptive up to 2.0x PNGs, and standalone HTML
gcp-arch render architecture.yaml --format pdf,png,html -o out/
```

!!! tip "Day-2 Drift Reconciliation"
    After teams curate component/view titles, subtitles, or `notes` inside Git, keep `architecture.yaml` synchronized without losing human edits:
    ```bash
    gcp-arch extract --projects acme-cicd -o facts.json
    gcp-arch synthesize facts.json --reconcile architecture.yaml --vertex-project my-vertex-project
    gcp-arch render architecture.yaml -o out/
    ```
    When all active resource URIs and conserved functional/transversal edges match (ignoring transient `heuristic: true` monitoring fluctuations), `--reconcile` finishes on the zero-delta fast path in **`< 5 ms` with `0` Vertex AI calls** without needing `--vertex-project`, running `refreshEdgeProvenance` in place and leaving `architecture.yaml` untouched when canonical bytes are unchanged ([`internal/synth/reconcile.go`](https://github.com/arrase/gcp-arch/blob/main/internal/synth/reconcile.go#L75-L106), [`cmd/gcp-arch/synthesize.go`](https://github.com/arrase/gcp-arch/blob/main/cmd/gcp-arch/synthesize.go#L205-L234)).

---

## Why `gcp-arch`?

| Capability | Manual Diagramming / Raw Resource Dumps | `gcp-arch` |
| :--- | :--- | :--- |
| **Signal vs. Noise** | Raw CAI/Terraform exports dump hundreds of routes, health checks, revisions, and default plumbing onto an unreadable canvas. | **Deterministic plumbing folding & transversal separation** collapses LB chains, Cloud Build runs, K8s plumbing, and separates IAM/KMS/Secrets/Observability into governance panels. |
| **Hallucination Risk** | Free-form LLM diagram prompts invent services, drop resources, and fabricate connections without proof. | **Deterministic skeleton + conservation gate** locks containment, cohorts, and URIs in Go, enforcing 100% resource/edge conservation and directed-walk provenance on every `VERIFIED` edge. |
| **Page Readability** | Massive wall-of-boxes graphs fail to print on A4 landscape or paste cleanly into Google Docs. | **Multi-view semantic partitioning** renders `View 0` plus stage-partitioned detail views with 1:1 per-view vector PDF pagination and adaptive up to `2.0x` Retina PNGs (`8,192px` / `28M` px cap). |
| **Connector Routing** | Force-directed/layered auto-layouts overlap labels, clip through cards, and stack parallel lines. | **Hanan-grid A* orthogonal router + streaming PNG audit** deconflicts ports, draws halo jump-arcs, verifies pixel & geometry invariants (`< 1 MB` RAM), and fails fast (exit `2`) on any violation. |
| **Day-2 Maintenance** | Re-generating diagrams wipes out human titles, annotations, and architectural grouping decisions. | **Incremental `--reconcile`** takes a `0`-LLM fast path when unchanged and preserves curated component/view titles, subtitles, and `notes` across delta runs. |
| **CI & Offline Portability** | Diagram rendering requires live cloud access, external CDNs, or SaaS editors. | **100% offline `validate` & `render`** operate solely on Git-tracked `architecture.yaml` with embedded fonts, icons, and strict CSP. |

---

## Visual Output Gallery

All **6** reference bundles in [`examples/`](https://github.com/arrase/gcp-arch/tree/main/examples) span **49 Google Cloud APIs** and **75 unique asset types**, validated against their paired `v3` facts fixtures (`conservation=PASS`, `view0=1.00`) and rendered completely offline:

| Platform CI/CD (`View 0`) | Retail Cloud-Native (`View 0`) |
| :---: | :---: |
| ![Platform CI/CD System Architecture Overview](assets/img/platform-cicd/00-view_0_system_index.png) | ![Retail Cloud-Native System Architecture Overview](assets/img/retail-cloud-native/00-view_0_system_index.png) |
| *`acme-cicd` → `acme-dev` / `acme-prod` · 17 components (5 Cloud Build cohorts, GKE, PSA Cloud SQL) across 3 views* | *Shared VPC Host + 3 service projects · Cloud Run, AlloyDB/Redis PSA, Dataflow, BigQuery, Vertex AI & Looker across 3 views* |

| FinTech Global Payments (`View 0`) | Enterprise Lakehouse & MLOps (`View 0`) |
| :---: | :---: |
| ![FinTech Global Payments System Architecture Overview](assets/img/fintech-global-payments/00-view_0_system_index.png) | ![Enterprise Lakehouse & MLOps System Architecture Overview](assets/img/enterprise-lakehouse-mlops/00-view_0_system_index.png) |
| *2 folders, 5 projects · NCC Hub (Interconnect/HA VPN), PSC Fabrics, Private GKE, Spanner, AlloyDB, Bigtable & Filestore across 4 views* | *2 folders, 3 peered VPCs · Managed Kafka, Data Fusion, Datastream, Composer, Dataflow, Dataproc, Dataform, Dataplex, BigQuery, Vertex AI & Looker across 4 views* |

| Agentic AI & Document Platform (`View 0`) | Enterprise Zero-Trust Mesh (`View 0`) |
| :---: | :---: |
| ![Agentic AI & Document Platform System Architecture Overview](assets/img/agentic-ai-document-platform/00-view_0_system_index.png) | ![Enterprise Zero-Trust Mesh System Architecture Overview](assets/img/enterprise-zero-trust-mesh/00-view_0_system_index.png) |
| *Shared VPC Host + 2 service projects · Vertex Reasoning Engine, Vector Search, Discovery Engine, Dialogflow CX, Document AI OCR & Eventarc across 3 views* | *2 folders, 4 projects, 2 regions · Zero-Trust Shared VPC, Cloud Deploy, GKE, App Engine, Cloud Batch, Cloud Run, AlloyDB/SQL PSA & Gemini across 3 views* |

Explore all **20** high-DPI Retina view diagrams in the [Examples & Visual Gallery](examples.md).

---

## How It Works

```mermaid
flowchart LR
    GCP["Target GCP Scopes\n(Org / Folder / Projects)"] -->|"read-only OAuth\nCAI, IAM, GKE, Monitoring, BQ, Dataflow"| Extract["Stage 1: extract"]
    Extract -->|"0600 gitignored"| Facts["facts.json (v3)"]
    Facts -->|"<= 500 active resources\ndeterministic skeleton"| Synth["Stage 2: synthesize"]
    Vertex["Vertex AI Gemini 4\n(--vertex-project, REST)"] <-->|"Single bounded call\nor --reconcile delta"| Synth
    Synth -->|"Conservation gate\nValidateWithFacts"| Manifest["architecture.yaml (v1)\nGit-tracked"]
    Manifest -->|"offline, zero network"| Validate["validate"]
    Manifest -->|"offline, embedded fonts & icons"| Render["Stage 3: render\n(Headless Chrome + router.js)"]
    Render --> Deliverables["architecture.pdf (1:1)\nviews/*.png (<= 2.0x)\narchitecture.html"]
```

1. **Preflight (`gcp-arch preflight` — [`cmd/gcp-arch/preflight.go`](https://github.com/arrase/gcp-arch/blob/main/cmd/gcp-arch/preflight.go)):** Probes read-only target permissions (`cloudasset`, folder display names, `monitoring`, `bigquery`, `dataflow`) and runs zero-token Vertex AI IAM & model availability checks with copy-pasteable `gcloud` remediations.
2. **Stage 1 — Extract (`gcp-arch extract` — [`cmd/gcp-arch/extract.go`](https://github.com/arrase/gcp-arch/blob/main/cmd/gcp-arch/extract.go), [`internal/extract/`](https://github.com/arrase/gcp-arch/tree/main/internal/extract)):** Streams `ReadTime`-pinned CAI resources and scoped IAM policies, folds noisy sub-resources, resolves GKE/Cloud Build/network relationships, runs concurrent 7-day enrichers, and scrubs secrets into a deterministic `v3` `facts.json`.
3. **Stage 2 — Synthesize (`gcp-arch synthesize` — [`cmd/gcp-arch/synthesize.go`](https://github.com/arrase/gcp-arch/blob/main/cmd/gcp-arch/synthesize.go), [`internal/synth/`](https://github.com/arrase/gcp-arch/tree/main/internal/synth)):** Builds a deterministic containment skeleton in Go, enriches it with a single bounded Vertex AI Gemini 4 REST call (`cloud.google.com/go/auth`), assembles `View 0` plus stage-partitioned detail views, and validates 100% resource/edge conservation and directed-walk provenance.
4. **Validate (`gcp-arch validate` — [`cmd/gcp-arch/validate.go`](https://github.com/arrase/gcp-arch/blob/main/cmd/gcp-arch/validate.go), [`internal/manifest/`](https://github.com/arrase/gcp-arch/tree/main/internal/manifest)):** Enforces strict `KnownFields(true)` YAML decoding and structural rules offline in Git PR CI, or full resource accounting, conservation, and directed-walk provenance with `--facts`.
5. **Stage 3 — Render (`gcp-arch render` — [`cmd/gcp-arch/render.go`](https://github.com/arrase/gcp-arch/blob/main/cmd/gcp-arch/render.go), [`internal/render/`](https://github.com/arrase/gcp-arch/tree/main/internal/render), [`internal/icons/`](https://github.com/arrase/gcp-arch/tree/main/internal/icons)):** Compiles a strict-CSP self-contained HTML document with embedded `Inter`/`JetBrains Mono` fonts and 85 SVG icons, runs the Hanan-grid A* orthogonal router with zero-collision and streaming PNG pixel verification (`< 1 MB` Go RAM) inside isolated headless Chrome, and exports 1:1 per-view vector `.pdf`, adaptive up to `2.0x` Retina `.png`s (`8,192px` max dim / `28M` px cap), pre-routed `.html`, and opt-in `geometry_report.json` (`--geometry-report`).

---

## Practical Recipes

### 1. Multi-Project Scan with Shared VPC Host Alias

Resolve numeric project numbers seen in Shared VPC or cross-project URIs into readable IDs:

```bash
gcp-arch extract \
  --projects app-prod,data-prod \
  --project-alias 999000111222=shared-vpc-host \
  -o prod.facts.json
```

### 2. Clean-Checkout Git PR Validation Gate

Validate human edits to `architecture.yaml` in pull requests with zero GCP credentials and zero network access:

```bash
gcp-arch validate architecture.yaml --json
```

### 3. Tune Vertex AI Reasoning Effort

Control Gemini 4 reasoning effort on Vertex AI:

```bash
gcp-arch synthesize facts.json \
  --vertex-project my-vertex-project \
  --model gemini-4-carbon \
  --thinking-level high \
  -o architecture.yaml
```

### 4. Standalone Offline HTML Dossier Export

Generate a single-file offline `.html` deliverable (works even on machines without Chrome/Chromium when `--chrome-path` is unset):

```bash
gcp-arch render architecture.yaml --format html -o out/
```

---

## CLI Commands & Exit Codes

Root CLI entrypoint, config loader, and atomic I/O live in [`cmd/gcp-arch/main.go`](https://github.com/arrase/gcp-arch/blob/main/cmd/gcp-arch/main.go), [`cmd/gcp-arch/config.go`](https://github.com/arrase/gcp-arch/blob/main/cmd/gcp-arch/config.go), and [`internal/atomicfile/`](https://github.com/arrase/gcp-arch/tree/main/internal/atomicfile).

| Command | Network / Auth Required | Primary Output |
| :--- | :--- | :--- |
| `gcp-arch preflight` | Target `read-only` + optional `--vertex-project` | Human table or `--json` `PreflightReport` |
| `gcp-arch extract` | Target `cloud-platform.read-only` | `facts.json` (`v3`, `0600` atomic write) |
| `gcp-arch synthesize <facts.json>` | `--vertex-project` (`cloud-platform`; optional on 0-LLM Day-2 fast path) | `architecture.yaml` (`v1`, `0644` atomic write) |
| `gcp-arch validate <architecture.yaml>` | **Offline** (zero network) | Validation status or `--json` `ValidationReport` |
| `gcp-arch render <architecture.yaml>` | **Offline** (local Chrome/Chromium for `pdf`/`png`) | `architecture.pdf` (1:1), `views/*.png` (`<= 2.0x`), `architecture.html`, opt-in `geometry_report.json` |

| Exit Code | Meaning |
| :--- | :--- |
| `0` | Success. |
| `1` | Runtime or external I/O failure: ADC/auth or quota project error, Vertex AI RPC error, missing input file, or missing Chrome/Chromium binary (for `pdf`/`png` or explicit missing `--chrome-path`). |
| `2` | Usage, config, schema, scale, validation, or layout/geometry failure: invalid flags/config, `> 500` active resources (`ScaleLimitError`), invalid YAML/unknown keys, `validate` violation (`ValidationError`), unresolved overlap during `render` (`LayoutCollisionError`), or PNG pixel/geometry/conservation failure (`GeometryReportError`). |

<div class="section-header">
  <h2>Documentation</h2>
  <span>Complete guides, pipeline internals &amp; schema references</span>
</div>

<div class="projects-grid">
  <a href="cli_configuration/" class="feature-card">
    <div>
      <div class="card-top">
        <i class="fa-solid fa-terminal feature-icon" aria-hidden="true"></i>
        <span class="card-badge">Read →</span>
      </div>
      <h3>CLI &amp; Configuration</h3>
      <p>Complete reference for all five subcommands, flags, <code>~/.config/gcp-arch/config.yaml</code> precedence, Chrome discovery, and exit codes.</p>
    </div>
  </a>
  <a href="security_iam/" class="feature-card">
    <div>
      <div class="card-top">
        <i class="fa-solid fa-shield-halved feature-icon" aria-hidden="true"></i>
        <span class="card-badge">Read →</span>
      </div>
      <h3>Security, IAM &amp; Preflight</h3>
      <p>OAuth scope separation, minimum IAM role matrix, quota project handling, VPC-SC detection, preflight probes, and credential scrubbing.</p>
    </div>
  </a>
  <a href="extraction/" class="feature-card">
    <div>
      <div class="card-top">
        <i class="fa-solid fa-magnifying-glass-chart feature-icon" aria-hidden="true"></i>
        <span class="card-badge">Read →</span>
      </div>
      <h3>Stage 1 — Read-Only Extraction</h3>
      <p><code>ReadTime</code>-pinned CAI streaming, GKE &amp; Cloud Build resolution, 7-day Monitoring/BigQuery/Dataflow enrichers, and <code>facts.json</code> v3.</p>
    </div>
  </a>
  <a href="synthesis/" class="feature-card">
    <div>
      <div class="card-top">
        <i class="fa-solid fa-wand-magic-sparkles feature-icon" aria-hidden="true"></i>
        <span class="card-badge">Read →</span>
      </div>
      <h3>Stage 2 — Gemini 4 Synthesis</h3>
      <p>500-resource guardrail, deterministic containment skeleton, single bounded Vertex AI REST call, conservation gate, and Day-2 <code>--reconcile</code>.</p>
    </div>
  </a>
  <a href="rendering/" class="feature-card">
    <div>
      <div class="card-top">
        <i class="fa-solid fa-file-pdf feature-icon" aria-hidden="true"></i>
        <span class="card-badge">Read →</span>
      </div>
      <h3>Stage 3 — Offline Rendering</h3>
      <p>Nested Flexbox containers, Hanan-grid A* router, halo jump-arcs, <code>chromedp</code> 1:1 PDF &amp; adaptive <code>2.0x</code> PNGs, streaming pixel checks, and 85 embedded icons.</p>
    </div>
  </a>
  <a href="manifest_schema/" class="feature-card">
    <div>
      <div class="card-top">
        <i class="fa-solid fa-file-code feature-icon" aria-hidden="true"></i>
        <span class="card-badge">Read →</span>
      </div>
      <h3>Manifest Schema &amp; Validation</h3>
      <p>Full <code>architecture.yaml</code> (<code>v1</code>) specification, strict YAML codec, all offline &amp; <code>--facts</code> validation codes, and Day-2 curation guide.</p>
    </div>
  </a>
  <a href="examples/" class="feature-card">
    <div>
      <div class="card-top">
        <i class="fa-solid fa-images feature-icon" aria-hidden="true"></i>
        <span class="card-badge">Read →</span>
      </div>
      <h3>Examples &amp; Visual Gallery</h3>
      <p>Walkthrough of all 6 complex GCP reference bundles across 49 APIs with all 20 rendered Retina view PNGs and offline commands.</p>
    </div>
  </a>
  <a href="architecture/" class="feature-card">
    <div>
      <div class="card-top">
        <i class="fa-solid fa-sitemap feature-icon" aria-hidden="true"></i>
        <span class="card-badge">Read →</span>
      </div>
      <h3>System Architecture</h3>
      <p>End-to-end data flow, package boundaries, atomic crash-safe I/O, rate-limiting architecture, and design invariants.</p>
    </div>
  </a>
  <a href="developer_guide/" class="feature-card">
    <div>
      <div class="card-top">
        <i class="fa-solid fa-code feature-icon" aria-hidden="true"></i>
        <span class="card-badge">Read →</span>
      </div>
      <h3>Developer Guide</h3>
      <p>Go 1.26 static builds, race &amp; headless Chrome testing, <code>golangci-lint</code> v2, CodeQL/<code>govulncheck</code>/<code>zizmor</code> CI, and release packaging.</p>
    </div>
  </a>
</div>
