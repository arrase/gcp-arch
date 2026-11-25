# Stage 2 — Gemini 4 Synthesis & Day-2 Reconciliation

Stage 2 (`gcp-arch synthesize <facts.json>`) converts a sanitized, read-only `v3` `facts.json` snapshot produced by [Stage 1 — Read-Only Extraction](extraction.md) into a Git-tracked, deterministic `v1` `architecture.yaml` manifest. Rather than prompting an LLM to draw unconstrained diagrams directly from raw cloud dumps, Stage 2 enforces a **closed-world two-pass synthesis pipeline** backed by short-ID interning, structured Vertex AI response schemas, deterministic Go projection for `View 0`, and full post-synthesis provenance verification.

On Day 2, `gcp-arch synthesize --reconcile` diffs fresh cloud facts against your curated `architecture.yaml` in place—preserving human-edited titles, `notes`, `rejected_edges`, and view archetypes—and completes in **`< 100 ms` with `0` Vertex AI calls** when no resources or inter-component edges changed.

---

## Quick Usage

=== "Day-1 Synthesis"

    Generate a brand-new multi-view `architecture.yaml` from an extracted `v3` `facts.json`:
    ```bash
    gcp-arch synthesize facts.json \
      --vertex-project consultant-ai-sandbox \
      -o architecture.yaml
    ```

=== "Day-2 Reconciliation"

    Reconcile fresh `facts.json` into an existing Git-tracked manifest in place (omitting `-o/--out` updates `--reconcile` atomically in place):
    ```bash
    gcp-arch synthesize facts.json \
      --reconcile architecture.yaml \
      --vertex-project consultant-ai-sandbox
    ```

=== "Model, Thinking & Concurrency Tuning"

    Override the default Gemini 4 model, Vertex location, reasoning effort, or Pass-2 worker pool:
    ```bash
    gcp-arch synthesize facts.json \
      --vertex-project consultant-ai-sandbox \
      --vertex-location global \
      --model gemini-4-carbon \
      --thinking-level high \
      --concurrency 4 \
      -o architecture.yaml
    ```

### CLI Flags & Config Precedence

Settings resolve in standard precedence order: **CLI flags > `~/.config/gcp-arch/config.yaml` > built-in defaults** (see [CLI & Configuration](cli_configuration.md)).

| Flag | Config Key (`config.yaml`) | Default | Description |
| :--- | :--- | :--- | :--- |
| `--vertex-project <ID>` | `vertex_project` | `""` | GCP billing project ID for Vertex AI Gemini calls. Required for Day-1 synthesis; optional on 0-LLM Day-2 `--reconcile` paths. |
| `--vertex-location <LOC>` | `vertex_location` | `global` | Vertex AI endpoint location (`synth.DefaultVertexLocation`). |
| `--model <ID>` | `model` | `gemini-4-carbon` | Vertex AI model identifier (`synth.DefaultVertexModel`). |
| `--thinking-level <LVL>` | `thinking_level` | `""` *(model default)* | Gemini thinking/effort level: `minimal`, `low`, `medium`, `high`, or `xhigh`. Omit to let Vertex AI apply the model default. |
| `--reconcile <FILE>` | — | `""` | Existing `architecture.yaml` file for Day-2 delta reconciliation. |
| `-o`, `--out <FILE>` | — | `architecture.yaml` *(or `--reconcile` path)* | Output manifest path. When `--reconcile` is set and `-o/--out` is omitted, updates write atomically in place. |
| `--concurrency <N>` | `concurrency` | `4` | Max concurrent Pass-2 per-view synthesis calls (`>= 1`, `synth.DefaultConcurrency`). |
| `-q`, `--quiet` | — | `false` | Suppress informational stdout summary lines. |

---

## Pre-Synthesis Guardrails & Token Efficiency

Before a single Vertex AI RPC is dispatched, Stage 2 enforces a strict resource scale guardrail and compacts the entire fact graph into deterministic short handles.

### 1. Hard Active-Resource Ceiling (`MaxActiveResources = 500`)

Both `synth.Synthesize` (Day 1) and `synth.Reconcile` (Day 2) enforce a hard ceiling of **`500` active resources** (`MaxActiveResources = 500` in `internal/synth/pipeline.go`).

* If `len(facts.Resources) > 500`, Stage 2 returns `*synth.ScaleLimitError`, which maps directly to **CLI exit code `2`**.
* **Zero wasted Vertex ADC calls:** `cmd/gcp-arch/synthesize.go` wraps the Vertex AI client in a `lazyModelClient` (`sync.Once`) that defers `NewVertexClient` initialization until `GenerateJSON` is actually called. Scale-limit checks run *before* any Vertex AI credentials are loaded or billed.

!!! warning "Exceeding the 500-Resource Ceiling"
    `gcp-arch` is engineered for focused, readable application-domain architecture dossiers rather than organization-wide inventory dumps. If extraction produces more than `500` active resources after Stage 1 plumbing folding, narrow your Stage 1 scope with `--projects` or `--folder` to a single application domain or platform boundary and re-run `gcp-arch extract`.

### 2. Deterministic Short-ID Interning (`internal/synth/intern.go`)

Raw GCP Cloud Asset Inventory URIs (such as `//container.googleapis.com/projects/acme-cicd/locations/europe-southwest1-b/clusters/cicd/k8s/namespaces/atlantis/apps/deployments/atlantis`) and 16-hex-character SHA-256 edge IDs inflate prompt/completion tokens and invite character-level transcription errors.

`NewInternTable(facts)` builds an immutable, bidirectional mapping without mutating the source `FactsFile`:

1. **Resources (`r_0001`..`r_NNNN`):** Clones `facts.Resources`, sorts deterministically by `(URI, AssetType, ProjectID)`, and assigns sequential zero-padded handles `r_0001`, `r_0002`, ..., `r_NNNN`.
2. **Edges (`e_0001`..`e_MMMM`):** Clones `facts.Edges`, backfills any missing `ID` via `extract.EdgeID(Kind, FromURI, ToURI, Detail)` (first 16 hex characters of `SHA-256(kind + ":" + fromURI + ":" + toURI + ":" + detail)`), sorts deterministically by `(FromURI, ToURI, Kind, Detail, ID)`, and assigns `e_0001`, `e_0002`, ..., `e_MMMM`.
3. **Pass-1 Adjacency Projection (`BuildAdjacency`):** Emits a deduplicated, sorted adjacency map `r_XXXX -> [{to, kind}]` covering every `FactEdge` whose endpoints exist in the intern table. Pass 1 uses this compact structural graph instead of raw edge details to partition components and views cleanly.
4. **Canonical Un-Interning:** When materializing the manifest (`materializeComponent`, `materializeContainer`, `materializePlanGlobals`, `materializeEdge`), Go translates every `r_XXXX` back to its canonical `{uri, asset_type, project_id}` tuple and every `e_XXXX` back to its canonical 16-hex-character `FactEdge.ID` hash. Any handle not present in `InternTable` is immediately flagged as a validation error and never silently dropped or hallucinated into YAML.

---

## Vertex AI Gemini 4 Client & Structured Output

Stage 2 communicates with Vertex AI through the official Google GenAI Go SDK (`google.golang.org/genai`) in `internal/synth/client.go`.

### Client Configuration & Credential Isolation

| Property | Value / Behavior | Source |
| :--- | :--- | :--- |
| **SDK Backend** | `genai.BackendVertexAI` | `internal/synth/client.go` |
| **OAuth Scope** | `https://www.googleapis.com/auth/cloud-platform` | Strictly separated from Stage 1's `cloud-platform.read-only` target discovery credentials (see [Security, IAM & Preflight](security_iam.md)). |
| **Default Model** | `gemini-4-carbon` (`DefaultVertexModel`) | `internal/extract/client.go` / `internal/synth/client.go` |
| **Default Location** | `global` (`DefaultVertexLocation`) | `internal/extract/client.go` / `internal/synth/client.go` |
| **Sampling Temperature** | `0.1` (`float32(0.1)`) | Low-variance analytical decoding hardcoded in `VertexClient.GenerateJSON`. |
| **Response MIME Type** | `application/json` | Paired with an explicit non-recursive `genai.Schema` (`ResponseSchema`). |
| **Thinking Config** | Optional `genai.ThinkingConfig{ThinkingLevel}` | Only attached when `--thinking-level` (or `config.yaml` `thinking_level`) is non-empty. |

### Thinking Level Options

`ParseThinkingLevel` (`internal/synth/client.go`) normalizes case-insensitively and accepts five explicit levels plus the empty default:

| `--thinking-level` Value | SDK Constant | Behavior |
| :--- | :--- | :--- |
| *(omitted / empty)* | `""` | Omits `ThinkingConfig` so Vertex AI applies the model's built-in default reasoning budget. |
| `minimal` | `genai.ThinkingLevelMinimal` | Minimal reasoning tokens for fastest latency. |
| `low` | `genai.ThinkingLevelLow` | Light reasoning for small, straightforward topologies. |
| `medium` | `genai.ThinkingLevelMedium` | Balanced reasoning depth and latency. |
| `high` | `genai.ThinkingLevelHigh` | Deep architectural reasoning for complex multi-project or Shared VPC graphs. |
| `xhigh` | `genai.ThinkingLevelXhigh` | Maximum reasoning depth. |

Passing any unrecognized value fails immediately with CLI exit code `2` before loading `facts.json` or initializing Vertex AI.

### Non-Recursive Structured `ResponseSchema` Design

`internal/synth/schemas.go` defines three flat, non-recursive `genai.Schema` contracts so Vertex AI constrained decoding guarantees syntactically valid JSON matching Go's unmarshal structs:

| Schema Function | Stage / Mode | Top-Level JSON Fields |
| :--- | :--- | :--- |
| **`Pass1PlanSchema()`** | Day 1 — Pass 1 | `components`, `container_defs`, `excluded_resources`, `detail_views` (all required) |
| **`Pass2ViewSchema()`** | Day 1 — Pass 2 *(per view)* | `container_ids`, `placements`, `edges` (all required) |
| **`ReconcilePatchSchema()`** | Day 2 — `--reconcile` Path C | `append_backed_by`, `append_container_backed_by`, `new_components`, `new_containers`, `new_excluded_resources`, `new_placements`, `new_edges`, `new_views` (all optional) |

Strict schema-level enums (`Format: "enum"`) are enforced directly inside constrained decoding for all small, fixed vocabularies:

* `component_kind` (8 values): `EXTERNAL_CALLER`, `INGRESS`, `WORKLOAD`, `PIPELINE`, `MESSAGING`, `DATASTORE`, `COHORT`, `EXTERNAL_TARGET`
* `kind` (6 container kinds): `PROJECT`, `VPC`, `GKE_CLUSTER`, `PSA_ZONE`, `EXTERNAL_ZONE`, `SHARED_HUB`
* `archetype` (3 values): `landscape_matrix`, `stage_pipeline`, `hub_spoke`
* `accent_mode` (3 values): `RUNTIME`, `CICD`, `MIXED`
* `flow_kind` (3 values): `RUNTIME`, `CICD`, `ADMIN`
* `provenance` (2 values): `VERIFIED`, `INFERRED`
* `direction` (2 values): `IN`, `OUT`

!!! info "Why `icon_key` Is Typed as `STRING` Instead of an 85-Value Schema Enum"
    The embedded icon registry (`internal/icons/assets/icons.yaml`) contains **85** registered SVG icon slugs. Embedding an 85-branch enum inside a repeated object field in a Vertex AI `ResponseSchema` causes constrained-decoding grammar state explosion and slower schema compilation. Instead, `componentSchema()` keeps `icon_key` as `genai.TypeString`, `Pass1SystemPrompt` and `ReconcileSystemPrompt` inject the sorted list of all 85 valid slugs from `reg.ValidKeys()`, and Go validates every returned `icon_key` via `reg.Has(c.IconKey)`—feeding exact offending slugs back into the retry loop if the model ever drifts.

---

## Day-1 Two-Pass Synthesis Pipeline

Splitting Day-1 synthesis into **Pass 1 (Global Partitioning)** and **Pass 2 (Bounded Parallel Per-View Synthesis)** keeps every LLM call focused on a compact, bounded context window while preventing cross-view inconsistencies.

```mermaid
flowchart TD
    Facts["facts.json (v3)"] --> Cap{"len(resources) <= 500?"}
    Cap -- "No (> 500)" --> ErrCap["ScaleLimitError (exit 2)\n0 Vertex AI calls"]
    Cap -- "Yes" --> Intern["NewInternTable\nr_0001..r_NNNN & e_0001..e_MMMM\n+ BuildAdjacency"]

    Intern --> P1["Pass 1: Global Partitioning\nGemini 4 (Pass1PlanSchema)"]
    P1 --> Gate1{"Pre-Pass-2 Gate\nvalidatePass1Plan"}
    Gate1 -- "Issues & retries <= 2" --> P1Retry["Inject validation_feedback\ninto next Pass 1 prompt"]
    P1Retry --> P1
    Gate1 -- "Still invalid (> 2 retries)" --> ErrP1["PASS1_VALIDATION_FAILED\n(exit 2)"]

    Gate1 -- "Valid Pass1Plan" --> Mat["materializePlanGlobals\nUn-intern r_XXXX -> {uri, asset_type, project_id}"]
    Mat --> P2Pool["Pass 2: Bounded errgroup\n(--concurrency, default 4)"]

    subgraph P2Worker ["Per-View Worker (1..M detail views)"]
        P2["Populate Locked View\nGemini 4 (Pass2ViewSchema)"] --> Gate2{"Per-View Gate\nvalidatePopulatedView\n+ dual-BFS walk check"}
        Gate2 -- "Issues & retries <= 2" --> P2Retry["Inject validation_feedback\ninto next Pass 2 prompt"]
        P2Retry --> P2
        Gate2 -- "Still invalid (> 2 retries)" --> ErrP2["PASS2_VALIDATION_FAILED\n(exit 2)"]
    end

    P2Pool --> P2Worker
    P2Worker --> SortViews["Sort Detail Views by View.ID"]
    SortViews --> View0["BuildSystemIndexView (Go)\nPrepend view_0_system_index if >= 2 detail views"]
    View0 --> FinalGate["manifest.ValidateWithFacts\n100% coverage + dual-BFS provenance"]
    FinalGate --> Write["manifest.Write (0644 atomic)\narchitecture.yaml (v1)"]
```

### Pass 1 — Global Partitioning & Pre-Pass-2 Gate

Pass 1 receives the global resource list (`scopes`, `hierarchy`, `resources`, deduplicated `adjacency`, and any retry `validation_feedback`) and produces a `Pass1Plan` declaring all logical `components`, `container_defs`, `excluded_resources`, and `detail_views` (each with a locked list of `component_ids`).

**Closed-world rules enforced in `Pass1SystemPrompt` (`internal/synth/prompts.go`):**

1. **100% Resource Accounting:** Every `r_XXXX` in `input.resources` MUST appear in at least one of `components[].backed_by_ids`, `container_defs[].backed_by_ids`, or `excluded_resources[].resource_id`.
2. **1:1 Folding:** Dedicated 1:1 `ServiceAccount`, `Secret`, `CryptoKey`, `Ingress`, `k8s.io/Service`, and `ForwardingRule` resources are folded into their owning workload or ingress component's `backed_by_ids`, surfacing concise attributes in `inline_meta` (maximum **4** rows per card).
3. **Stateful Datastore Homogeneity:** Distinct stateful datastore asset types from the 15-type datastore set (see [Stateful Datastore Homogeneity](manifest_schema.md#stateful-datastore-homogeneity)) may never be mixed inside a single component's `backed_by_ids`.
4. **Focused Detail Views:** Partition into `1..M` cohesive detail views targeting `6–12` components each (hard cap `1..12` blocks per view). Pass 1 must **never** emit `view_0_system_index` (Go projects `View 0` deterministically). Every component must appear in at least one detail view.
5. **Flat Containers (`depth <= 2`):** Define visual grouping frames (`PROJECT`, `VPC`, `GKE_CLUSTER`, `PSA_ZONE`, `EXTERNAL_ZONE`, `SHARED_HUB`) with parent nesting depth `<= 2`. VPC and GKE Cluster `r_XXXX` handles are accounted for via `container_defs[].backed_by_ids` or dedicated hub components.
6. **Archetype Selection:** Assign `stage_pipeline` for CI/CD and ETL pipelines, `hub_spoke` for VPC/PSA/PSC networking topologies, or `landscape_matrix` for application and ingress landscapes.
7. **Valid Icon Slugs:** Choose `icon_key` strictly from the 85 sorted keys injected from `icons.Registry.ValidKeys()`.

**Pre-Pass-2 Validation Gate (`validatePass1Plan` in `internal/synth/pipeline.go`):**

Before launching any Pass-2 view workers, Go verifies:

* Non-empty, unique component IDs; non-empty component titles; valid `icon_key` in `icons.Registry`; non-empty `backed_by_ids`; `<= 4` `inline_meta` rows; all `r_XXXX` handles exist in `InternTable`; stateful datastore homogeneity across the 15 datastore asset types.
* Non-empty, unique container IDs; all `backed_by_ids` exist in `InternTable`; valid `parent_container_id`; zero parent cycles; global nesting depth `<= 2`.
* All `excluded_resources` reference valid `r_XXXX` handles and carry a non-empty audit `reason`.
* **100% resource coverage:** Any unaccounted `r_XXXX` handles are listed verbatim in the retry feedback.
* At least one detail view; unique, non-empty view IDs never equal to `view_0_system_index`; `1..12` valid component IDs per view; zero unplaced components across the suite.

If any issue is found (or JSON fails to unmarshal), Stage 2 retries Pass 1 up to **`maxValidationRetries = 2` times** (**3 total attempts**), passing the exact offending IDs and messages inside `validation_feedback`. Exhausting retries returns a `*manifest.ValidationError` with code `PASS1_VALIDATION_FAILED` (CLI exit code `2`).

---

### Pass 2 — Bounded Parallel Per-View Synthesis

Once Pass 1 clears its gate, `materializePlanGlobals` un-interns components, containers, and excluded resources into canonical manifest structs, and `pass2Runner.populateViews` dispatches one Pass-2 worker per detail view inside a bounded `errgroup.WithContext` (`g.SetLimit(concurrency)`, default `4`).

Each view receives a tightly scoped, deterministic JSON payload built by `BuildPass2UserPrompt`:

| Payload Field | Contents |
| :--- | :--- |
| `view` | Locked `Pass1DetailView` header (`id`, `title`, `subtitle`, `archetype`, `accent_mode`, `component_ids`). |
| `max_column` | `4` for `landscape_matrix` / `stage_pipeline`; `2` for `hub_spoke`. |
| `components` | Sorted `Pass1Component` definitions assigned to this view. |
| `container_defs` | Global container definitions available to activate in this view. |
| `resources` | `internedResourceDesc` records for resources backing this view's components plus any bridge resources referenced by `incident_edges`. |
| `incident_edges` | Interned `e_XXXX` edges where both endpoints belong to this view's components, **OR** one endpoint belongs to this view and the other belongs to the **bridge set** (`excluded_resources` + `container_defs[].backed_by_ids`). Including bridge edges lets Pass 2 construct multi-hop `VERIFIED` directed walks that traverse excluded or container-backed resources (such as a GKE cluster or Shared VPC). |
| `other_view_ids` | Sorted IDs of all other detail views (for valid `off_page_refs` targets). |
| `cross_view_neighbors` | Sorted list of `{local_component_id, remote_component_id, remote_view_ids, direction}` covering: (1) directed `FactEdge`s crossing out of (`OUT`) or into (`IN`) this view's resources to components placed in other detail views, and (2) **shared components** placed in both this view and another detail view (`addSharedViewNeighbors`, emitted with `direction: "OUT"`). |
| `validation_feedback` | Exact error messages from the previous attempt on retries. |

**Per-View Validation Gate (`validatePopulatedView`) & Final Assembly:**

Each worker un-interns `e_XXXX` handles via `materializeEdge` and validates:

* Active `container_ids` exist and have nesting depth `<= 2` within the view.
* `1..12` placements; every locked `component_id` placed exactly once; `container_id` (if set) is listed in `container_ids`; `0 <= column <= max_column`; `off_page_refs` target another detail view (never self).
* Every edge's `from` and `to` are placed in the view; `verb` is `1..16` UTF-8 runes; shared `via_bus_id` groups never mix multiple distinct sources AND multiple distinct targets; `VERIFIED` edges supply `evidence_edge_ids` forming a connected directed walk from `from.backed_by` to `to.backed_by`; `INFERRED` edges supply a non-empty `inference_rationale`.

Each view retries independently up to **`2` retries** (**3 total attempts**), returning `PASS2_VALIDATION_FAILED` on exhaustion. After all workers finish:

1. Detail views are sorted deterministically by `View.ID`.
2. `Metadata.GeneratedAt` is set from `opts.GeneratedAt` if specified, else `facts.ReadTime`, else current UTC RFC 3339 timestamp.
3. `BuildSystemIndexView` prepends `view_0_system_index` at `views[0]` whenever `>= 2` detail views exist.
4. The complete manifest must pass `manifest.ValidateWithFacts(m, facts, reg)` before `manifest.Write` atomically writes `architecture.yaml` (`0644`).

---

## Closed-World Guarantees & Directed-Walk Provenance

Stage 2 enforces three non-negotiable closed-world invariants across both Day-1 synthesis and Day-2 reconciliation:

1. **Zero Invented Resources:** Every resource handle (`r_XXXX`) and edge handle (`e_XXXX`) returned by Gemini 4 is checked against `InternTable`. Unknown handles immediately fail validation and trigger retry feedback.
2. **100% Resource Accounting:** Every active resource in `facts.Resources` MUST be accounted for in `components[].backed_by`, `containers[].backed_by`, or `excluded_resources` (with an explicit audit `reason`). Nothing can silently vanish from the architecture.
3. **Dual-BFS Directed-Walk Provenance:** Every edge in a detail view carries an explicit `provenance`:
   * **`VERIFIED`:** Must cite one or more `FactEdge.ID`s in `evidence_edge_ids`. Go verifies the proof using **dual BFS** (`hasDirectedWalk` in `internal/synth/pipeline.go` and `isValidDirectedWalk` in `internal/manifest/validate.go`):
     - Build forward adjacency (`FromURI -> ToURI`) and reverse adjacency (`ToURI -> FromURI`) strictly over the subgraph induced by the cited `evidence_edge_ids`.
     - Run forward BFS seeded with all URIs in `from.backed_by` (`fwdReach`).
     - Run reverse BFS seeded with all URIs in `to.backed_by` (`bwdReach`).
     - **Pass condition:** Every single cited `FactEdge` `fe` MUST satisfy `fwdReach[fe.FromURI] && bwdReach[fe.ToURI]`—guaranteeing both that a directed walk connects `from` to `to` and that no disconnected or extraneous evidence edges were padded into the list.
   * **`INFERRED`:** Permitted when a real architectural relationship cannot be proven by extracted facts (for example, application-level logic without IAM/Monitoring telemetry). Must supply a non-empty `inference_rationale`, and renders as a dashed connector with a numbered footnote in [Stage 3 — Offline Rendering & Routing](rendering.md).

---

## Deterministic `View 0` (`view_0_system_index`) Projection

`View 0` is **never generated by the LLM**. `BuildSystemIndexView` (`internal/synth/view0.go`) projects it deterministically in Go after Pass 2 (on Day 1) and after pruning or patching (on Day 2).

### Trigger & Fixed Header

* **Trigger:** Created and pinned at `views[0]` if and only if **`>= 2` detail views** exist. For single-view manifests (`1` detail view), `BuildSystemIndexView` returns `nil` and `view_0_system_index` is omitted (matching `ValidateStructure`'s `UNEXPECTED_SYSTEM_INDEX_VIEW` rule).
* **Fixed Header:**
  * `id`: `view_0_system_index`
  * `title`: `System Index Map`
  * `subtitle`: `"<D> logical views across <C> architecture components"`
  * `archetype`: `landscape_matrix`
  * `accent_mode`: `MIXED`
  * `container_ids`: omitted (View 0 has no container boxes)

### 16-Slot Budget & Archetype Column Assignment

View 0 supports up to **`16` placements** (`maxSystemIndexPlacements = 16`), filled in strict priority order:

1. **Detail Views** (sorted by `View.ID`, capped at `16`).
2. **`EXTERNAL_CALLER` components** placed in any detail view (deduplicated, sorted by `Component.ID`, filling remaining slots).
3. **`EXTERNAL_TARGET` components** placed in any detail view (deduplicated, sorted by `Component.ID`, filling remaining slots).

Each placed item is assigned to a deterministic column based on its role or detail-view archetype:

| Placed Entity | Column Index | Notes |
| :--- | :---: | :--- |
| `EXTERNAL_CALLER` component | `0` | Left-most entry actors (e.g., GitHub, Public Internet). |
| Detail view (`stage_pipeline`) | `1` | CI/CD, ETL, or streaming pipelines. |
| Detail view (`landscape_matrix`) | `2` | Application, GKE, or ingress landscapes (default fallback). |
| Detail view (`hub_spoke`) | `3` | Shared VPC, PSA, or PSC networking topologies. |
| `EXTERNAL_TARGET` component | `4` *(or `3`)* | Column `4` if **any** detail view uses `hub_spoke`; otherwise column `3`. |

### 3-Phase Summary Edge Projection

View 0 summary edges are deduplicated per directed `(from, to)` pair (first phase wins), ignore self-loops and unplaced endpoints, honor `rejected_edges`, receive `provenance: VERIFIED` without `evidence_edge_ids`, truncate verbs to `<= 16` UTF-8 runes (`truncateVerb`, falling back to `"cross-view flow"` if blank), and sort deterministically by `(From, To, FlowKind, Verb)`:

| Phase | Source Signal | Direction | `flow_kind` Resolution | Edge Verb |
| :--- | :--- | :--- | :--- | :--- |
| **1. External Flows** (`addExternalEdges`) | Detail-view edges where `e.From == callerID` or `e.To == targetID` | `caller -> detailView` or `detailView -> target` | Highest priority across matching edges in the view: **`RUNTIME` > `CICD` > `ADMIN`** | Caller: `calls ingress` (`RUNTIME`), `triggers CI/CD` (`CICD`), `administers` (`ADMIN`). Target: `sends traffic` (`RUNTIME`), `deploys to` (`CICD`), `configures` (`ADMIN`). |
| **2. Off-Page `OUT` Refs** (`addOffPageEdges`) | Placement `off_page_refs` with `direction == "OUT"` targeting another placed detail view | `sourceView -> ref.TargetViewID` | `CICD` if `sourceView.AccentMode == CICD`, else `RUNTIME` (`flowFromAccent`) | `truncateVerb(ref.Label)` |
| **3. Shared Components** (`addSharedComponentEdges`) | Pair of detail views sharing at least one placed non-external component (neither `EXTERNAL_CALLER` nor `EXTERNAL_TARGET`) with no edge in either direction from Phases 1–2 | Lower-column view `->` higher-column view (ties broken by sorted `View.ID`) | `CICD` only if **both** views have `AccentMode == CICD`, else `RUNTIME` | `shared resources` |

---

## Day-2 Reconciliation (`--reconcile`)

Infrastructure evolves continuously, while architecture manifests accumulate valuable human curation in Git (custom titles, `notes`, `inline_meta`, placement ordering, and `rejected_edges` tombstones). Running `gcp-arch synthesize facts.json --reconcile architecture.yaml` keeps the manifest synchronized without regenerating from scratch or wiping manual edits.

### Reconciliation Decision Flow

```mermaid
flowchart TD
    Start["Inputs: fresh facts.json (v3)\n+ curated architecture.yaml (v1)"] --> Cap{"len(resources) <= 500?"}
    Cap -- "No" --> ErrScale["ScaleLimitError (exit 2)"]
    Cap -- "Yes" --> Diff["Pre-Diffing:\n1. diffManifestURIs -> addedURIs, vanishedURIs\n2. refreshVerifiedEdges (auto shortest-walk BFS)\n3. findUnrepresentedInterComponentEdges"]

    Diff --> CheckA{"added == 0 AND vanished == 0\nAND invalidated == 0\nAND newEdges == 0?"}
    CheckA -- "Yes" --> PathA["Path A: Unchanged Fast-Path\n0 LLM calls (< 100 ms)\nValidateWithFacts"]
    PathA --> EqualCheck{"manifestsEqualInPlace?"}
    EqualCheck -- "Yes (in-place & byte-identical)" --> NoTouch["Leave file untouched on disk\nPrint: Unchanged ... (0 deltas)"]
    EqualCheck -- "No (refreshed hashes or -o diff)" --> WriteA["Atomic Write (0644)"]

    CheckA -- "No" --> Prune["pruneManifest:\n• Drop vanished URIs from backed_by / excluded\n• Drop empty components & empty views\n• Prune stale off_page_refs & broken VERIFIED edges"]
    Prune --> Delta["Compute Post-Prune Deltas:\nunclaimedRIDs + deltaEdgeIDs\n(with 1-hop bridge expansion)"]
    Delta --> CheckB{"unclaimedRIDs == 0\nAND deltaEdgeIDs == 0?"}

    CheckB -- "Yes" --> PathB["Path B: Deletion-Only Prune Path\n0 LLM calls\nrebuildSystemIndex -> ValidateWithFacts"]
    PathB --> WriteB["Atomic Write (0644)"]

    CheckB -- "No" --> ClientCheck{"--vertex-project\nconfigured?"}
    ClientCheck -- "No" --> ErrClient["Usage error (exit 2)\npass --vertex-project"]
    ClientCheck -- "Yes" --> PathC["Path C: Drift Delta-Patch Path\nGemini 4 (ReconcilePatchSchema)\nFreeze titles, notes, archetypes, rejected_edges"]
    PathC --> Apply["applyReconcilePatch\n+ rebuildSystemIndex\n+ ValidateWithFacts"]
    Apply -- "Invalid & retries <= 2" --> PathC
    Apply -- "Valid" --> WriteC["Atomic Write (0644)"]
```

### Pre-Diffing & Automatic `FactEdge.ID` Hash Refresh

Before deciding whether an LLM call is needed, `Reconcile` (`internal/synth/pipeline.go`) executes three deterministic checks:

1. **URI Diff (`diffManifestURIs`):** Compares `facts.Resources` against all URIs accounted for in `components[].backed_by`, `containers[].backed_by`, and `excluded_resources` to produce `addedURIs` and `vanishedURIs`.
2. **Automatic Evidence Refresh (`refreshVerifiedEdges`):** Scans every detail-view `VERIFIED` edge. If its existing `evidence_edge_ids` still form a directed walk in `facts.Edges`, the edge stays untouched. If a `FactEdge.ID` hash shifted (for example, because a Cloud Monitoring `detail` count or metadata string changed), Go runs deterministic shortest-walk BFS (`findShortestWalkIDs`) over `facts.Edges` between `from.backed_by` and `to.backed_by` and silently replaces `evidence_edge_ids` with the fresh walk. Only edges with **no** remaining directed walk increment `invalidatedEdgeCount`.
3. **Unrepresented Inter-Component Edges (`findUnrepresentedInterComponentEdges`):** Identifies any `FactEdge` connecting two distinct existing components `(fc, tc)` that is neither represented in any detail view nor tombstoned in `rejected_edges`.

### The Three Execution Paths

#### Path A — Unchanged Fast-Path (`0` LLM Calls, `< 100 ms`)

* **Condition:** `len(addedURIs) == 0 && len(vanishedURIs) == 0 && invalidatedEdgeCount == 0 && len(newEdgeIDs) == 0`.
* **Behavior:** Validates the manifest against `facts.json` via `ValidateWithFacts` without invoking Vertex AI (`--vertex-project` is optional).
* **Disk Write Optimization (`manifestsEqualInPlace`):** When writing in place (`filepath.Clean(targetPath) == filepath.Clean(reconcilePath)`), the CLI compares canonical `manifest.Marshal` bytes before and after reconciliation. If byte-identical, the file on disk is **left completely untouched** (preserving mtime and producing zero Git diff) and stdout prints `Unchanged <file> (0 deltas)`. If `refreshVerifiedEdges` updated shifted `FactEdge.ID` hashes—or if `-o` points to a different file—the reconciled manifest is written atomically, still with `0` LLM calls.

#### Path B — Deletion-Only Prune Path (`0` LLM Calls)

* **Condition:** Resources vanished and/or existing `VERIFIED` edges became invalid, leaving **`0` unclaimed resources** and **`0` unrepresented inter-component edges** after pruning.
* **Pruning Cascade (`pruneManifest`):**
  1. Strips vanished URIs from `components[].backed_by`, `containers[].backed_by`, and `excluded_resources`.
  2. Removes any component whose `backed_by` becomes empty (`removedComps`).
  3. Strips placements referencing `removedComps` from detail views; removes any detail view whose `placements` become empty (`removedViews`).
  4. Prunes stale `off_page_refs` pointing to `removedViews` from all surviving placements.
  5. Removes edges with unplaced endpoints and any `VERIFIED` edge whose directed walk no longer exists in `facts.json`. (`INFERRED` edges whose endpoints remain placed are preserved!)
* **Completion:** Deterministically rebuilds `View 0` (`rebuildSystemIndex`), runs `ValidateWithFacts`, and atomically writes the pruned manifest with `0` Vertex AI calls (`--vertex-project` is optional).

#### Path C — Drift Delta-Patch Path (Vertex AI Gemini 4)

* **Condition:** One or more `unclaimed_resources` or `delta_edges` remain after pruning.
* **Frozen Curation Contract:** `BuildReconcileUserPrompt` sends read-only summaries (`frozen_components`, `frozen_containers`, `existing_views` with `remaining_capacity = max(0, 12 - len(placements))`, `rejected_edges`) alongside `unclaimed_resources` and `delta_edges` (which includes 1-hop bridge expansion through `excluded_resources` and `containers[].backed_by` via `addUnclaimedIncidentEdges`). Existing view IDs, archetypes, curated component titles/subtitles/`icon_key`/`inline_meta`, `notes`, and `rejected_edges` are **strictly frozen** and cannot be overwritten by the patch.
* **Supported `ReconcilePatch` Operations (`applyReconcilePatch`):**

| Patch Field | Effect on Existing Manifest |
| :--- | :--- |
| `append_backed_by` | Appends unique unclaimed resources to an existing component's `backed_by` without altering its title, subtitle, `inline_meta`, or `notes`. |
| `append_container_backed_by` | Appends unique unclaimed resources to an existing container's `backed_by`. |
| `new_components` | Registers brand-new components (rejects ID collisions with existing components). |
| `new_containers` | Registers brand-new containers (rejects ID collisions with existing containers). |
| `new_excluded_resources` | Appends unclaimed resources to `excluded_resources` with an audit `reason`. |
| `new_placements` | Inserts placements into existing detail views that have `remaining_capacity > 0` (automatically adding `placement.container_id` to `view.container_ids` if needed). |
| `new_edges` | Appends new edges to existing detail views (strictly rejecting any `(from, to)` pair present in `rejected_edges`). |
| `new_views` | Creates overflow detail views (`1..12` placements, never `view_0_system_index`, no ID collisions) when existing views lack capacity. |

* **Validation & Retries:** On each attempt (up to `2` retries / `3` total attempts), Go clones `cur`, applies the patch, deterministically rebuilds `View 0`, checks merge errors (`RECONCILE_INVALID_JSON` / `RECONCILE_MERGE_FAILED`), and validates the entire manifest via `manifest.ValidateWithFacts` (retaining exact `*manifest.ValidationError` issues or `RECONCILE_VALIDATION_FAILED`).

### `--vertex-project` & `-o/--out` Behavior Matrix

| Scenario | `--vertex-project` Required? | `-o / --out` Omitted Behavior | Stdout Summary *(unless `-q`)* | Failure Exit Code |
| :--- | :---: | :--- | :--- | :---: |
| **Day-1 synthesis** | **Yes** (checked before loading facts) | Writes `architecture.yaml` | `Wrote <out> (<V> views, <C> components)` | `2` if missing flag, `> 500` resources, or validation retry exhaustion; `1` on Vertex RPC/ADC error |
| **Day-2 Path A** *(0 deltas, identical)* | **Optional** | Leaves `--reconcile` file untouched on disk | `Unchanged <file> (0 deltas)` | `2` if existing manifest fails `ValidateWithFacts` |
| **Day-2 Path A** *(hash refresh only)* | **Optional** | Updates `--reconcile` atomically in place | `Reconciled <file> (<V> views, <C> components)` | `1` on write I/O failure |
| **Day-2 Path B** *(deletions only)* | **Optional** | Updates `--reconcile` atomically in place | `Reconciled <file> (<V> views, <C> components)` | `2` if pruned manifest fails `ValidateWithFacts` |
| **Day-2 Path C** *(new resources/edges)* | **Yes** (`client == nil` returns exit `2` with `pass --vertex-project`) | Updates `--reconcile` atomically in place | `Reconciled <file> (<V> views, <C> components)` | `2` if missing `--vertex-project`, `> 500` resources, or validation retry exhaustion; `1` on Vertex RPC/ADC error |

!!! tip "Safe CI Drift Detection Without Vertex AI Credentials"
    Because Paths A and B require **zero** Vertex AI credentials, you can run `gcp-arch synthesize fresh.facts.json --reconcile architecture.yaml` without `--vertex-project` in read-only drift-check jobs: exit code `0` confirms no new resources or edges appeared (or only clean deletions occurred), whereas exit code `2` with `vertex AI model client is required (pass --vertex-project)` pinpoints the exact count of unclaimed resources and new inter-component edges that need synthesis.

---

## Next Steps

* **Schema & Validation Rules:** Explore every field, enum, offline error code, and Day-2 manual curation rule in [Manifest Schema & Validation](manifest_schema.md).
* **Stage 3 Rendering:** Learn how `architecture.yaml` compiles offline into vector PDFs, `3x` PNGs, and HTML dossiers in [Stage 3 — Offline Rendering & Routing](rendering.md).
* **Stage 1 Inputs:** Review how `facts.json` (`v3`) resources, edges, and plumbing folding are constructed in [Stage 1 — Read-Only Extraction](extraction.md).
* **IAM & Preflight:** Check required Vertex AI permissions and zero-token model probes in [Security, IAM & Preflight](security_iam.md).
