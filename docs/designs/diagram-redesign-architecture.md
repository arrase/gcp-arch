# GCP Architecture Diagram Redesign Architecture

* **Status:** Implemented
* **Date:** 2026-11-27

## 1. Objective & Architectural Constraints

The redesign of `gcp-arch` unifies Stage 2 (`internal/manifest/`, `internal/synth/`) and Stage 3 (`internal/render/`, `internal/icons/`, `cmd/gcp-arch/`) so that any GCP environment—from simple 3-tier workloads to multi-project Shared VPC hub-and-spoke topologies and 24-service high-fan-in/fan-out meshes—renders as crisp, high-resolution diagrams understandable by a human engineer in under 5 seconds:

* **Collector Isolation:** Stage 1 (`internal/extract/`, `cmd/gcp-arch/extract.go`, `cmd/gcp-arch/preflight.go`) stays decoupled; synthesis consumes `extract.FactsFile` (`v3`) cleanly at the `LoadFacts` boundary.
* **Full-Depth Hierarchy & Transversal Separation:** Unlimited containment depth (`Org > Folder > Project > VPC > Region > Subnet > ExposureZone/GKE/PSA/Hybrid`), consistent left-to-right traffic/data flow, and semantic separation of IAM, Service Accounts, KMS, Secrets, Observability, Cloud NAT/Routers, and Firewall/Cloud Armor into card chips and dedicated governance panels.
* **Deterministic Conservation & Stability:** Zero dropped or hallucinated primary resources and functional dependencies across repeated Vertex AI (`my-vertex-project`, `global`, `gemini-4-carbon`) runs.
* **Live Verification:** End-to-end verification executes against live Vertex AI and real headless Chrome PNG rasterization without mocks or stubs.

---

## 2. Layout Strategy Evaluation & Discard Rationale

Three representation, layout, and rendering architectures were benchmarked on `testdata/acme_cicd_v3.facts.json` (44 resources, 93 edges) and repeated runs of `tough_adversarial_v3.facts.json` (91 resources, 231 edges, 25 services, 7-level hierarchy, 79-char resource names, 101 transversal edges) using live Vertex AI (`gemini-4-carbon`) and real rasterizers (Chrome/Chromium, `elkjs`, and Graphviz `dot`):

| Dimension | Nested Flexbox + A* Router (Selected) | `elkjs` Layered Layout (Discarded) | Graphviz `dot` (Discarded) |
| :--- | :--- | :--- | :--- |
| **Visual Review Score (/50)** | **44.0 / 50** | 34.5 / 50 | 15.5 / 50 |
| **Pipeline** | Go Skeleton + `gemini-4-carbon` + Nested Flexbox + DOM Hanan-Grid A* Router + Chrome PNG/PDF | Go + Node `elkjs` + SVG/HTML + Chrome PNG | Go + Graphviz `dot` + SVG/HTML + Chrome PNG |
| **`tough` View 0 Canvas & Density** | **Compact `4513x2659` DIP (AR `1.76`, `15.43%` card fill, `0%` whiteout)** | `11030x3731` DIP (`3.43x` area, AR `2.61`, `4.44%` card fill, `~25%` top void) | `4808x8375` DIP (AR `0.57`, **`61.4%–70.8%` Chrome Skia bottom whiteout**) |
| **Geometry & Collision Audit** | `0` card overlaps, `0` ellipsis/clipping, `0` non-endpoint pierces | `0` overlaps, `94/94` pills; severe DPI clamping & wide whitespace | **14/14 views `FAIL`**: `ortho` `8` pill-card + `93` pill-wire hits; `polyline` `25` card pierces |
| **Conservation & Hierarchy** | `PASS` (91/91 URIs, 105/105 functional edges; single Shared VPC Host + chips) | Dropped `4` `PSA_PEERING` edges; duplicated Shared VPC `3x` and flipped project columns across runs | Dropped `10` `CONFIG_REF (vpc_connector)` edges; omitted `19/91` names from HTML |
| **Runtime Dependencies** | **Pure Go + local Chrome (`chromedp`)** | Go + Node + `npm elkjs` + Chrome | Go + Graphviz `dot` + Chrome |

### Why `elkjs` Was Discarded

1. **Compound Whitespace & Panoramic Aspect Collapse:** On `tough_adversarial` View 0, ELK placed projects sequentially at `11,030 x 3,731` DIP (`3.43x` larger area, `4.44%` card fill) and stranded `shared-infrastructure` top-right, leaving a ~25% blank top void.
2. **DPI Degradation:** Extreme canvas dimensions forced severe `DeviceScaleFactor` clamping below Chrome Skia's bitmap ceiling, blurring card and governance text.
3. **Containment & Stability Defects:** ELK's `INCLUDE_CHILDREN` duplicated `vpc-enterprise-hub` into 3 separate boxes, fragmented `Private Exposure`, and swapped project columns across identical runs.
4. **Operational Dependency Overhead:** Required a Node/ESM sidecar (`elkjs`) and pre-layout DOM measurement pass.

### Why Graphviz `dot` Was Discarded

1. **14/14 Automated Geometry Failures:** Every rendered view across `acme_cicd` and `tough` failed geometry checks (`splines=ortho` lacks native edge labels and collided `xlabels` with 8 cards and 93 wires; `splines=polyline` pierced 25 non-endpoint cards).
2. **Vertical Collapse & Chrome Skia Whiteout:** `rankdir=LR` stacked nested `cluster_*` subgraphs vertically past Chrome's Skia surface ceiling, triggering 61.4%–70.8% bottom whiteout in PNG exports.

*(The compact prompt structure, deterministic title/view ID locking, wire-vs-card collision checks, and adaptive Skia DPI ceiling guard evaluated during prototyping were unified into the production Go + Chrome renderer.)*

---

## 3. Production Architecture (Unified Deterministic Skeleton + Vertex AI + Chrome Renderer)

```
FactsFile (v3)
   │
   ▼
Stage 2A: Go Deterministic Containment Skeleton
(internal/synth/{synth,assets,skeleton,containers,edges,ranking}.go)
   ├─ 1:1 plumbing folds (k8s Service->Deployment, Ingress->LB, Subscription->Topic, VpnGateway->Tunnel)
   ├─ Homogeneous cohorts (BuildTriggers & identical-wiring siblings with 100% member retention)
   ├─ Unlimited containment tree (Org > Folder > Project > VPC > Region > Subnet > Zone/GKE/PSA/Hybrid)
   ├─ Transversal separation (IAM_SA, KMS, SECRET, OBSERVABILITY, NAT_ROUTER, FIREWALL_ARMOR, VPC_ACCESS)
   ├─ Orphan bridging (INFERRED edges for Private DNS, NCC Hub spokes, Hybrid VPN/Interconnect, CI staging)
   └─ Tarjan SCC cycle breaking + ingress bucket rank + longest-path TopoRank + 3-pass barycenter SubRank
   │
   ▼
Stage 2B: Bounded Vertex AI Annotator
(internal/synth/{annotate,client,reconcile}.go)
   ├─ Direct Vertex AI REST over cloud.google.com/go/auth (gemini-4-carbon, location=global,
   │  Temperature=0.1, ResponseSchema, 240s per-attempt timeout + up to 2 transient retries)
   ├─ Emits ONLY: focus_summary, stage-partitioned detail_views, node subtitles, edge verb/flow_kind/semantic_tag
   ├─ Go locks node titles, view IDs, hierarchy, and populates symmetric cross-view OffPageRefs
   └─ Sanitization rejects/strips "...", U+2026 ("…"), and stray trailing quotes/punctuation
   │
   ▼
Stage 2C: Deterministic Conservation Gate (internal/manifest/conservation.go)
   ├─ VerifyConservation: 0 missing/hallucinated primary URIs, 0 missing functional/transversal edges
   ├─ Strict StructuralEdge validation (rejects bogus structural_link targets and transversal bypasses)
   └─ 100% View 0 component coverage & ValidateWithFacts directed-walk verification
   │
   ▼
Stage 3: Nested Flexbox Layout, A* Orthogonal Router & Chrome CDP Export
(internal/render/{prepare,html,render,chrome,geometry,pngcheck}.go)
   ├─ Nested Flexbox HTML/CSS containers (zero negative margins, zero ellipsis, min-width: 0 wrapping)
   ├─ Multi-column .dock-bottom / .wide-gov transversal governance panel balancing tall IAM meshes
   ├─ DOM-measured Hanan-grid A* router (router.js): card/header avoidance, cross-bucket lane separation,
   │  horizontal jumper bridges, destination fan-in & source fan-out pill bundling, zero silent pill drops
   └─ Headless Chrome (chromedp):
      ├─ 1:1 per-view vector PDF (PrintToPDF, PreferCSSPageSize, @media print --print-scale zoom)
      ├─ Per-view isolated Retina PNG (adaptive up to 2.0x DPI, 8,192px max dim / 28M px cap)
      ├─ Streaming scanline PNG pixel & geometry verification (< 1 MB Go RAM) on every PNG render
      └─ Standalone offline HTML & opt-in geometry_report.json (--geometry-report)
```

### Key Design Guarantees

1. **Collector Isolation:** `internal/extract/**`, `cmd/gcp-arch/extract.go`, and `cmd/gcp-arch/preflight.go` stay cleanly separated; `synthesize` consumes `v3` `FactsFile` JSON directly.
2. **Deterministic Conservation & Stability:** Locking containment, cohorts, canonical `Title`/`View.ID`, `TopoRank`, and `SubRank` in deterministic Go keeps repeated Vertex AI runs structurally invariant, while `VerifyConservation` and `ValidateWithFacts` mathematically verify URI and functional-edge conservation. Day-2 `--reconcile` (`internal/synth/reconcile.go`) runs a `< 5 ms` zero-LLM fast path when unchanged and preserves curated component/view edits across delta runs.
3. **Automated Geometry & Streaming Pixel Verification:** Every PNG render (`ChromeDPRunner.Render` / `render.Render` / `gcp-arch render`) runs streaming scanline verification (`internal/render/pngcheck.go`, `< 1 MB` Go RAM) and fails non-zero (`*render.GeometryReportError`, exit code `2`) on any card/header/badge/pill/wire overlap, coincident containers, node-container boundary straddling, text clipping/overflow/ellipsis, unplaced verb pills (`unplaced_labels`), dimension below `600x400`, aspect ratio outside `[0.35, 2.60]`, blank/solid/low-contrast/monochrome/edge-clipped PNG pixels, or blanked `NodeBox`/`BadgeBox` regions (`png_box_blank`). Passing `--geometry-report` additionally persists `geometry_report.json` to disk, and `--facts` appends deterministic conservation verification.
4. **Visual Comprehension Standard:** Rendered high-resolution PNGs across all reference bundles and catalog fixtures maintain `<5s` visual comprehension of traffic ingress, container hierarchy, dependency flow, and clutter-free transversal separation.

---

## 4. Reproducible CLI & E2E Usage

Run the live end-to-end catalog suite (synthesizing and rasterizing all `testdata/catalog/*.facts.json` fixtures against live Vertex AI and headless Chrome, including multi-run stability checks in `e2e/catalog_e2e_test.go` and `e2e/geometry_test.go`):

```bash
GCP_ARCH_E2E=1 GCP_ARCH_VERTEX_PROJECT=my-vertex-project go test -v ./e2e
```

Run offline PNG streaming regression checks across all 6 committed examples:

```bash
go test -v -run TestExamplesPNGStreamingRegression ./e2e
```

Individual stage commands:

```bash
CGO_ENABLED=0 go build -o ./bin/gcp-arch ./cmd/gcp-arch
./bin/gcp-arch synthesize testdata/acme_cicd_v3.facts.json --vertex-project my-vertex-project -o out/acme/architecture.yaml
./bin/gcp-arch validate out/acme/architecture.yaml --facts testdata/acme_cicd_v3.facts.json
./bin/gcp-arch render out/acme/architecture.yaml --facts testdata/acme_cicd_v3.facts.json --geometry-report --format pdf,png,html -o out/acme/
```
