# Developer Guide

Welcome to the `gcp-arch` developer guide. This page covers local setup, static builds, dependency architecture, race and headless Chrome test suites, `golangci-lint` v2 configuration, CI/CD & security pipelines, and step-by-step extension workflows.

---

## Development Prerequisites

| Tool | Requirement | Purpose |
| :--- | :--- | :--- |
| **Go** | **`1.26.11`+** ([`go.mod`](https://github.com/arrase/gcp-arch/blob/main/go.mod)) | Compiling the static `CGO_ENABLED=0` binary and running unit, race, and coverage suites |
| **Chrome or Chromium** | Local binary on `PATH` or `CHROME_PATH` | Executing real headless rendering tests in [`internal/render/render_test.go`](https://github.com/arrase/gcp-arch/blob/main/internal/render/render_test.go), [`internal/render/layout_test.go`](https://github.com/arrase/gcp-arch/blob/main/internal/render/layout_test.go), and [`cmd/gcp-arch/e2e_test.go`](https://github.com/arrase/gcp-arch/blob/main/cmd/gcp-arch/e2e_test.go) |
| **`golangci-lint`** | **v2** ([`.golangci.yml`](https://github.com/arrase/gcp-arch/blob/main/.golangci.yml) `version: "2"`) | Local static analysis and `gofmt` verification matching GitHub Actions CI |
| **MkDocs Material** | `mkdocs-material==9.7.7` (Python 3.14 in CI) | Live local preview (`mkdocs serve`) and strict documentation builds (`mkdocs build --strict`) |

---

## Cloning & Static Builds

`gcp-arch` is built as a standalone static binary with CGO disabled (`CGO_ENABLED=0`).

```bash
# 1. Clone the repository
git clone https://github.com/arrase/gcp-arch.git
cd gcp-arch

# 2. Build a static local binary (version defaults to "dev")
CGO_ENABLED=0 go build -o ./bin/gcp-arch ./cmd/gcp-arch

# 3. Verify static compilation metadata
go version -m ./bin/gcp-arch | grep CGO_ENABLED=0

# 4. Check CLI version and help
./bin/gcp-arch --version
```

### Stripped Release Builds & Version Injection

[`cmd/gcp-arch/main.go`](https://github.com/arrase/gcp-arch/blob/main/cmd/gcp-arch/main.go#L27) declares `var version = "dev"` and wires it directly to Cobra's `--version` flag. Production release binaries strip symbol tables and DWARF debug info (`-s -w`) while injecting the release tag via `-ldflags`:

```bash
VERSION="v1.0.0"
CGO_ENABLED=0 go build \
  -ldflags="-s -w -X main.version=${VERSION}" \
  -o ./bin/gcp-arch \
  ./cmd/gcp-arch

./bin/gcp-arch --version
# gcp-arch version v1.0.0
```

---

## Core Dependencies ([`go.mod`](https://github.com/arrase/gcp-arch/blob/main/go.mod))

Every direct dependency in [`go.mod`](https://github.com/arrase/gcp-arch/blob/main/go.mod#L5-L22) maps cleanly to one stage of the pipeline:

| Module | Version | Role in `gcp-arch` |
| :--- | :--- | :--- |
| `github.com/spf13/cobra` | `v1.10.2` | CLI root command, subcommands (`preflight`, `extract`, `synthesize`, `validate`, `render`), and `--version` |
| `github.com/spf13/pflag` | `v1.1.0` | POSIX flag parsing and `--project` → `--projects` flag normalization |
| `cloud.google.com/go/asset` | `v1.30.1` | Cloud Asset Inventory (`assetpb`) `ListAssets` gRPC streaming (`RESOURCE` & `IAM_POLICY`) |
| `cloud.google.com/go/monitoring` | `v1.32.1` | Cloud Monitoring (`monitoringpb`) 7-day `consumed_api` and Vertex `PublisherModel` queries |
| `cloud.google.com/go/auth` | `v0.24.4` | ADC credential loading and `grpctransport` / `httptransport` (isolated `cloud-platform.read-only` for extraction & `cloud-platform` for direct Vertex AI REST calls) |
| `cloud.google.com/go/iam` | `v1.15.1` | IAM policy protobuf structures attached to CAI `IAM_POLICY` responses |
| `golang.org/x/sync` | `v0.25.0` | Bounded concurrency via `errgroup` (Stage 1 enricher workers) |
| `golang.org/x/time` | `v0.16.0` | Token-bucket `rate.Limiter` enforcing `80` RPM CAI and `10` QPS Monitoring/enricher caps |
| `google.golang.org/grpc` | `v1.85.0` | gRPC dial options, status code inspection, and `32 MiB` max recv message sizing |
| `google.golang.org/protobuf` | `v1.36.12` | `structpb.Struct` / `timestamppb.Timestamp` decoding for CAI assets and `ReadTime` pinning |
| `google.golang.org/genproto` / `googleapis/api` / `googleapis/rpc` | pinned commits | Structured `errdetails.ErrorInfo` unpacking (`QUOTA_FATAL`, `VPC_SC_BLOCKED`, `SERVICE_DISABLED`) |
| `gopkg.in/yaml.v3` | `v3.0.1` | Strict `KnownFields(true)` decoding and canonical 2-space encoding for `architecture.yaml`, `config.yaml`, and `icons.yaml` |
| `github.com/chromedp/chromedp` | `v0.24.0` | Isolated headless Chrome/Chromium orchestration over CDP |
| `github.com/chromedp/cdproto` | `v0.160.4` | Typed CDP calls (`emulation.SetDeviceMetricsOverride`, `page.CaptureScreenshot`, `page.PrintToPDF`) |

---

## Running Tests & Coverage

### Full Race & Coverage Run

Run the exact test command executed in CI:

```bash
go test -v -race -coverprofile=coverage.out -covermode=atomic ./...
```

Inspect per-package and per-function coverage:

```bash
go tool cover -func=coverage.out | tail -n 25
```

`gcp-arch` targets **`>= 85%`** test coverage across `internal/...` packages (`internal/render/assets/**` is excluded from Go coverage instrumentation in [`sonar-project.properties`](https://github.com/arrase/gcp-arch/blob/main/sonar-project.properties)).

### Golden Test Fixtures ([`testdata/`](https://github.com/arrase/gcp-arch/tree/main/testdata))

| Fixture File | Format | Contents & Usage |
| :--- | :---: | :--- |
| [`testdata/acme_cicd_v3.facts.json`](https://github.com/arrase/gcp-arch/blob/main/testdata/acme_cicd_v3.facts.json) | `v3` JSON | Synthetic sanitized `acme-cicd` snapshot (**44** `FactResource` entries, **93** `FactEdge` entries) driving offline synthesis, validation, E2E pipeline tests, and [`examples/platform-cicd/`](https://github.com/arrase/gcp-arch/tree/main/examples/platform-cicd). |
| [`testdata/acme_cicd_expected.architecture.yaml`](https://github.com/arrase/gcp-arch/blob/main/testdata/acme_cicd_expected.architecture.yaml) | `v1` YAML | Frozen 4-view, 14-component reference manifest validating Day-2 `--reconcile` backward compatibility and real headless Chrome export. |
| [`testdata/examples/*.facts.json`](https://github.com/arrase/gcp-arch/tree/main/testdata/examples) | `v3` JSON | **5** synthetic multi-project `v3` fixtures backing [`examples/retail-cloud-native/`](https://github.com/arrase/gcp-arch/tree/main/examples/retail-cloud-native), [`examples/fintech-global-payments/`](https://github.com/arrase/gcp-arch/tree/main/examples/fintech-global-payments), [`examples/enterprise-lakehouse-mlops/`](https://github.com/arrase/gcp-arch/tree/main/examples/enterprise-lakehouse-mlops), [`examples/agentic-ai-document-platform/`](https://github.com/arrase/gcp-arch/tree/main/examples/agentic-ai-document-platform), and [`examples/enterprise-zero-trust-mesh/`](https://github.com/arrase/gcp-arch/tree/main/examples/enterprise-zero-trust-mesh). |
| [`testdata/catalog/*.facts.json`](https://github.com/arrase/gcp-arch/tree/main/testdata/catalog) | `v3` JSON | **10** synthetic catalog fixtures covering simple 3-tier workloads through 24-service meshes and IAM explosions, driving deterministic skeleton tests (`internal/synth/synth_test.go`) and live Vertex AI + Chrome E2E tests (`e2e/catalog_e2e_test.go`). |

### Real Headless Chrome Render Tests

[`internal/render/render_test.go`](https://github.com/arrase/gcp-arch/blob/main/internal/render/render_test.go), [`internal/render/layout_test.go`](https://github.com/arrase/gcp-arch/blob/main/internal/render/layout_test.go), and [`cmd/gcp-arch/e2e_test.go`](https://github.com/arrase/gcp-arch/blob/main/cmd/gcp-arch/e2e_test.go) include end-to-end tests backed by a real local Chrome/Chromium instance (`requireChromeForTest` / `requireChromeForE2E`):

- **Local Behavior:** Automatically calls `t.Skipf(...)` if no Chrome/Chromium binary is installed locally.
- **CI Behavior (`CI=true`):** Hard-fails via `t.Fatalf(...)` if Chrome/Chromium is missing, preventing silent skips in CI runners.

The real Chrome suites in [`internal/render/render_test.go`](https://github.com/arrase/gcp-arch/blob/main/internal/render/render_test.go) and [`internal/render/layout_test.go`](https://github.com/arrase/gcp-arch/blob/main/internal/render/layout_test.go) verify:

| Test Function | Verification Scope |
| :--- | :--- |
| [`TestHeadlessChromeAcmeCICDGoldenSuite`](https://github.com/arrase/gcp-arch/blob/main/internal/render/render_test.go#L544) | Renders all 4 golden `acme-cicd` views: checks `data-gcp-arch-routed="1"` idempotency guard, `4` injected `connector-layer` SVGs, passing geometry & streaming PNG checks, valid `%PDF-` header, exact `/Type /Page` count (`4`), and non-empty zlib-decompressed vector text streams (`BT`/`Tj`). |
| [`TestHeadlessChromeBipartiteCrossingsAndHaloBridges`](https://github.com/arrase/gcp-arch/blob/main/internal/render/render_test.go#L663) | Renders irreducible bipartite graphs **K<sub>2,2</sub>** (`K2,2`) and **K<sub>3,3</sub>** (`K3,3`), asserting zero collisions, `data-bridge="1"` + `data-bridge-halo="1"` jump-arc paths, and strict SVG paint order. |
| [`TestHeadlessChromeHighFanInHybridManifestAndGeometryReport`](https://github.com/arrase/gcp-arch/blob/main/internal/render/render_test.go#L708) | Renders a high-fan-in multi-container `layered_flow` manifest with `--geometry-report` enabled, verifying zero collisions, non-empty `NodeBoxes`/`ContainerBoxes`/`BadgeBoxes`, and passing streaming pixel checks. |
| [`TestHeadlessChromeForcedCollisionsReturnLayoutCollisionError`](https://github.com/arrase/gcp-arch/blob/main/internal/render/render_test.go#L829) | Invokes `ChromeDPRunner.Render` directly to verify both forced card overlaps and `text-overflow: ellipsis` truncation return `*LayoutCollisionError`. *(Zero-files-written enforcement on collision is verified separately by [`testRenderLayoutCollisionError`](https://github.com/arrase/gcp-arch/blob/main/internal/render/render_test.go#L323) using `mockBrowserRunner`.)* |
| [`TestHeadlessChromeLargeScaleAreaCapAndPDFPagination`](https://github.com/arrase/gcp-arch/blob/main/internal/render/render_test.go#L1060) | Verifies adaptive Retina DPI clamping under the `8,192px` max-dim / `28M` px area cap and exact 1:1 per-view vector PDF pagination. |
| [`TestPixelVerificationRejectsBlankAndSolidPNGs`](https://github.com/arrase/gcp-arch/blob/main/internal/render/render_test.go#L1157) | Verifies blank, solid, monochrome, edge-clipped, and blank-box PNGs fail with `*GeometryReportError`. |
| [`TestHeadlessChromeNestedFlexboxContainerStyling`](https://github.com/arrase/gcp-arch/blob/main/internal/render/layout_test.go#L277) | Renders a 7-level nested Flexbox hierarchy (`Org -> Folder -> Project -> VPC -> Region -> Subnet -> GKE` + `PSA`) and asserts computed CSS border styles, fills, and `data-depth="7"`. |
| [`TestHeadlessChromeCollisionKeepsConnectorsInStandaloneHTML`](https://github.com/arrase/gcp-arch/blob/main/internal/render/layout_test.go#L317) | Injects a mid-corridor `.arch-card` blocker and verifies standalone HTML preserves the rendered `svg.connector-layer` strokes while still reporting `ok: false` and omitting `data-gcp-arch-routed`. |
| [`TestHeadlessChromePrintScaleFitsWideContent`](https://github.com/arrase/gcp-arch/blob/main/internal/render/layout_test.go#L344) | Renders a wide (`>= 1500px`) view stage and asserts full-width `2.0x` PNG capture, `--print-scale` width scaling (`~1040 / rect.Width`), and single-page PDF output. |
| [`TestHeadlessChromeContainerPiercingAvoidance`](https://github.com/arrase/gcp-arch/blob/main/internal/render/layout_test.go#L545) | Verifies the Hanan-grid A* router avoids piercing intermediate containers and cards across complex multi-tier layouts. |

---

## Linting & Formatting (`.golangci.yml`)

Code quality is enforced with **`golangci-lint` v2** (`version: "2"` in `.golangci.yml`), `gofmt`, and `go vet`:

```bash
# Check formatting
gofmt -l .

# Run standard static vet checks
go vet ./...

# Run golangci-lint v2 using repository .golangci.yml
golangci-lint run ./...
```

### Enabled Formatter & 11 Linters

`.golangci.yml` sets `default: none` and explicitly enables the **`gofmt`** formatter plus **11** targeted linters:

| Linter | Enforcement Focus |
| :--- | :--- |
| `bodyclose` | Guarantees all HTTP response bodies (`resp.Body.Close()`) are closed without leaks |
| `errcheck` | Flags unchecked returned errors across production and test code |
| `errorlint` | Enforces Go 1.13+ error wrapping (`%w`) and `errors.Is` / `errors.As` checks |
| `gocognit` | Caps cognitive complexity per function to keep pipeline stages clean and modular |
| `gocritic` | Catches subtle Go performance, style, and diagnostic pitfalls |
| `govet` | Official Go static analyzer (format strings, shadow/copylock/structtag issues) |
| `ineffassign` | Detects assignments to variables whose values are never read |
| `misspell` | Catches spelling errors in comments, identifiers, and error strings |
| `revive` | Enforces exported symbol doc comments (`exported` rule configured with `disable-stuttering-check`) |
| `staticcheck` | Deep static analysis for deprecated APIs, simplifications, and correctness bugs |
| `unused` | Flags dead functions, types, fields, variables, and constants |

---

## CI/CD, Security, Quality & Release Automation

Every GitHub Actions workflow pins actions to immutable full commit SHAs and declares least-privilege top-level `permissions: {}`.

### GitHub Actions Workflows ([`.github/workflows/`](https://github.com/arrase/gcp-arch/tree/main/.github/workflows))

| Workflow | Triggers | Jobs & Responsibilities |
| :--- | :--- | :--- |
| **[`ci.yml`](https://github.com/arrase/gcp-arch/blob/main/.github/workflows/ci.yml)** | `push` (`main`), `pull_request` | **1. `lint`:** `gofmt -l .`, `go vet ./...`, `golangci-lint-action` v9.<br>**2. `tests`:** `go test -v -race -coverprofile=coverage.out -covermode=atomic ./...`, `CGO_ENABLED=0 go build -v ./...`, uploads `coverage.out`.<br>**3. `sonarcloud`:** Downloads `coverage.out` and executes `SonarSource/sonarqube-scan-action` against SonarQube Cloud (`main` + non-fork PRs). |
| **[`codeql.yml`](https://github.com/arrase/gcp-arch/blob/main/.github/workflows/codeql.yml)** | `push` (`main`), `pull_request`, weekly Monday `04:17 UTC` | Runs CodeQL `security-and-quality` queries across both **`go`** (`build-mode: autobuild`) and **`actions`** (`build-mode: none`), uploading SARIF results to GitHub Security. |
| **[`security.yml`](https://github.com/arrase/gcp-arch/blob/main/.github/workflows/security.yml)** | `push` (`main`), `pull_request`, daily `05:41 UTC` | **1. `govulncheck`:** Installs `golang.org/x/vuln/cmd/govulncheck@v1.1.4` and scans `./...` for reachable Go CVEs.<br>**2. `dependency-review`:** Runs `actions/dependency-review-action` on pull requests.<br>**3. `zizmor`:** Static security auditing of all GitHub Actions workflow YAML files. |
| **[`release.yml`](https://github.com/arrase/gcp-arch/blob/main/.github/workflows/release.yml)** | `release` (`published`) | Cross-compiles **5** static `CGO_ENABLED=0` stripped binaries (`-ldflags "-s -w -X main.version=${VERSION}"`): `gcp-arch-linux-amd64`, `gcp-arch-linux-arm64`, `gcp-arch-darwin-amd64`, `gcp-arch-darwin-arm64`, `gcp-arch-windows-amd64.exe`, then uploads them to GitHub Releases via `softprops/action-gh-release`. |
| **[`deploy.yml`](https://github.com/arrase/gcp-arch/blob/main/.github/workflows/deploy.yml)** | `push` (`main` touching `gh-pages/**`, `mkdocs.yml`, or `deploy.yml`), `workflow_dispatch` | Installs `mkdocs-material==9.7.7` (binary wheels only) on Python `3.14`, builds `mkdocs build --strict`, adds `.nojekyll`, and deploys `./site` to GitHub Pages via OIDC. |

### Dependabot & SonarQube Cloud Configuration

- **Dependabot ([`.github/dependabot.yml`](https://github.com/arrase/gcp-arch/blob/main/.github/dependabot.yml)):** Schedules weekly Monday `06:00` checks with a **7-day cooldown** for both `gomod` (up to `10` open PRs, prefix `chore(deps)`) and grouped `github-actions` (up to `5` open PRs, prefix `chore(ci)`).
- **SonarQube Cloud ([`sonar-project.properties`](https://github.com/arrase/gcp-arch/blob/main/sonar-project.properties)):** Configures organization `arrase`, project `arrase_gcp-arch`, `sonar.go.coverage.reportPaths=coverage.out`, `sonar.qualitygate.wait=true`, source exclusions (`**/*_test.go`, `.github/**`, `examples/**`, `testdata/**`, `gh-pages/**`), and coverage exclusions (`internal/render/assets/**`).

---

## Common Extension Workflows

### 1. Adding a New Embedded SVG Icon

1. **Add the inline `<svg>` entry** to [`internal/icons/assets/icons.yaml`](https://github.com/arrase/gcp-arch/blob/main/internal/icons/assets/icons.yaml) under the appropriate category section (`schema_version: v1`):
   ```yaml
   icons:
     my_new_service:
       svg: '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><path fill="#4285F4" d="..."/></svg>'
   ```
   Ensure the SVG complies strictly with `ValidateSVG` ([`internal/icons/validate.go`](https://github.com/arrase/gcp-arch/blob/main/internal/icons/validate.go)): single `<svg>` root, no `<!DOCTYPE>` or processing instructions, no `<script>`/`<foreignObject>`/`on*` handlers, no external URLs, and only local `#id` references (internal IDs and `<defs>` will be auto-prefixed with `icon-<key>-` and hoisted at load time).
2. **Update the expected icon count** (`const wantCount = 85`) in `TestLoadRegistryKeysAndFullSprite` ([`internal/icons/icons_test.go`](https://github.com/arrase/gcp-arch/blob/main/internal/icons/icons_test.go)).
3. **Verify compilation, ID uniqueness, and race safety:**
   ```bash
   go test -v -race ./internal/icons/...
   ```

### 2. Adding a Stage 1 Attribute Projector or Enricher

- **Allowlisted Asset Attribute Projector ([`internal/extract/sanitize.go`](https://github.com/arrase/gcp-arch/blob/main/internal/extract/sanitize.go)):**
  1. Write a focused `project<Service>Attrs(p *attrProjector, data *structpb.Struct)` function extracting *only* non-sensitive architectural metadata using `p.setStr`, `p.setBool`, `p.setInt`, and `p.setURI` (never forwarding raw `env`, `args`, `command`, `script`, or `ConfigMap` payload keys).
  2. Register the `asset_type` key in the `assetAttrProjectors` map in [`internal/extract/sanitize.go#L469-L554`](https://github.com/arrase/gcp-arch/blob/main/internal/extract/sanitize.go#L469-L554).
  3. Add unit tests in [`internal/extract/extract_test.go`](https://github.com/arrase/gcp-arch/blob/main/internal/extract/extract_test.go) covering both extracted attributes and secret scrubbing guarantees.
- **Runtime API Enricher ([`internal/extract/enrichers.go`](https://github.com/arrase/gcp-arch/blob/main/internal/extract/enrichers.go)):**
  1. Gate the new enricher on `st.enabledProjects(candidates, "<service>.googleapis.com")`, share the `fetchProjectItemsConcurrently` worker helper (`maxEnrichWorkers = 8`), and follow the per-enricher rate-limiter pattern on `Clients` (`clients.BQLimiter`, `clients.DataflowLimiter`, `clients.MonitoringLimiter` — each initialized at `DefaultEnricherQPS = 10` / `DefaultMonitoringQPS = 10` in [`internal/extract/client.go`](https://github.com/arrase/gcp-arch/blob/main/internal/extract/client.go)).
  2. Record degraded outcomes (`IAM_DENIED`, `VPC_SC_BLOCKED`, `SERVICE_DISABLED`, `TRUNCATING_CAP`) in `CoverageLedger` rather than aborting the entire extraction on non-fatal target permissions.
  3. Wire an optional readiness probe in `RunPreflight` ([`internal/extract/client.go#L971`](https://github.com/arrase/gcp-arch/blob/main/internal/extract/client.go#L971)) and document the minimum read-only IAM role in [Security, IAM & Preflight](security_iam.md).

### 3. Regenerating Reference Examples & Running Live Catalog E2E

After modifying [`diagram.css`](https://github.com/arrase/gcp-arch/blob/main/internal/render/assets/diagram.css), [`router.js`](https://github.com/arrase/gcp-arch/blob/main/internal/render/assets/router.js), [`prepare.go`](https://github.com/arrase/gcp-arch/blob/main/internal/render/prepare.go), [`icons.yaml`](https://github.com/arrase/gcp-arch/blob/main/internal/icons/assets/icons.yaml), or any example manifest/fixture, regenerate and verify all 6 bundles offline:

```bash
CGO_ENABLED=0 go build -o ./bin/gcp-arch ./cmd/gcp-arch

for pair in \
  "platform-cicd:testdata/acme_cicd_v3.facts.json" \
  "retail-cloud-native:testdata/examples/retail_cloud_native_v3.facts.json" \
  "fintech-global-payments:testdata/examples/fintech_global_payments_v3.facts.json" \
  "enterprise-lakehouse-mlops:testdata/examples/enterprise_lakehouse_mlops_v3.facts.json" \
  "agentic-ai-document-platform:testdata/examples/agentic_ai_document_platform_v3.facts.json" \
  "enterprise-zero-trust-mesh:testdata/examples/enterprise_zero_trust_mesh_v3.facts.json"
do
  slug="${pair%%:*}"
  facts="${pair#*:}"
  ./bin/gcp-arch validate "examples/$slug/architecture.yaml" --facts "$facts"
  ./bin/gcp-arch render "examples/$slug/architecture.yaml" --facts "$facts" --format pdf,png,html -o "examples/$slug/"
done

# Verify streaming PNG pixel & geometry checks across all 6 bundles
go test -v -run TestExamplesPNGStreamingRegression ./e2e

# Optional: run full live Vertex AI + Chrome E2E across testdata/catalog/
GCP_ARCH_E2E=1 GCP_ARCH_VERTEX_PROJECT=my-vertex-project go test -v ./e2e
```

!!! tip "Keeping Gallery Assets Synchronized"
    When refreshing the `examples/*/views/*.png` outputs, copy the updated PNGs into `gh-pages/assets/img/<slug>/` so the [Examples & Visual Gallery](examples.md) reflects the latest renderer output.

### 4. Previewing & Building Documentation Locally

Install MkDocs Material matching `.github/workflows/deploy.yml` in a local virtual environment:

```bash
python3 -m venv .venv
.venv/bin/pip install --only-binary :all: mkdocs-material==9.7.7

# Start live-reloading preview at http://127.0.0.1:8000/
.venv/bin/mkdocs serve

# Run strict verification (fails on broken links or missing assets)
.venv/bin/mkdocs build --strict
```

---

## Pre-PR Verification Checklist

Before opening a pull request against `main`, run the full local validation suite:

```bash
gofmt -l .
go vet ./...
golangci-lint run ./...
go test -v -race -coverprofile=coverage.out -covermode=atomic ./...
CGO_ENABLED=0 go build -o ./bin/gcp-arch ./cmd/gcp-arch
```

If you edited documentation under `gh-pages/` or `mkdocs.yml`, also run `.venv/bin/mkdocs build --strict` and verify zero warnings.
