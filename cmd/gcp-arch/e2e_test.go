package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/icons"
	"github.com/arrase/gcp-arch/internal/manifest"
	"github.com/arrase/gcp-arch/internal/render"
	"github.com/arrase/gcp-arch/internal/synth"
)

func requireChromeForE2E(t *testing.T) string {
	t.Helper()
	chromePath, err := render.FindChrome("")
	if err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatalf("Chrome/Chromium required in CI=true environment: %v", err)
		}
		t.Skipf("skipping real Chrome E2E check: %v", err)
	}
	return chromePath
}

func countPDFPages(pdf []byte) int {
	re := regexp.MustCompile(`/Type\s*/Page\b`)
	return len(re.FindAll(pdf, -1))
}

func pdfHasNonEmptyTextStream(pdf []byte) bool {
	// Headless Chrome compresses page content streams with FlateDecode (zlib).
	idx := 0
	for {
		streamBody, nextIdx, ok := nextPDFStream(pdf, idx)
		if !ok {
			return false
		}
		if pdfStreamHasText(streamBody) {
			return true
		}
		idx = nextIdx
	}
}

func nextPDFStream(pdf []byte, from int) (body []byte, nextIdx int, ok bool) {
	streamMarker := []byte("stream")
	endStreamMarker := []byte("endstream")
	pos := bytes.Index(pdf[from:], streamMarker)
	if pos < 0 {
		return nil, 0, false
	}
	start := from + pos + len(streamMarker)
	if start < len(pdf) && pdf[start] == '\r' {
		start++
	}
	if start < len(pdf) && pdf[start] == '\n' {
		start++
	}
	end := bytes.Index(pdf[start:], endStreamMarker)
	if end < 0 {
		return nil, 0, false
	}
	return pdf[start : start+end], start + end + len(endStreamMarker), true
}

func pdfStreamHasText(streamBody []byte) bool {
	body := streamBody
	requireET := true
	if zr, err := zlib.NewReader(bytes.NewReader(streamBody)); err == nil {
		body, _ = io.ReadAll(zr)
		_ = zr.Close()
		requireET = false
	}
	if !bytes.Contains(body, []byte("BT")) {
		return false
	}
	if requireET && !bytes.Contains(body, []byte("ET")) {
		return false
	}
	return bytes.Contains(body, []byte("Tj")) || bytes.Contains(body, []byte("TJ"))
}

// buildAcmeMockModelClient translates acme_cicd_expected.architecture.yaml into valid Pass 1 and
// Pass 2 short-ID JSON responses so the full synthesize pipeline (interning, pre-Pass-2 gate,
// concurrent errgroup Pass 2, deterministic View 0 builder, ValidateWithFacts) executes end-to-end.
func buildAcmeMockModelClient(t *testing.T, facts *extract.FactsFile, expected *manifest.Manifest) synth.ModelClient {
	t.Helper()
	table := synth.NewInternTable(facts)
	pass1 := buildAcmePass1Plan(t, table, expected)
	pass2ByView := buildAcmePass2Views(t, table, expected, &pass1)

	return &stubModelClient{
		handler: func(_ int, _, userPrompt string, _ *genai.Schema) ([]byte, error) {
			var probe struct {
				View struct {
					ID string `json:"id"`
				} `json:"view"`
			}
			if err := json.Unmarshal([]byte(userPrompt), &probe); err == nil && probe.View.ID != "" {
				pv, ok := pass2ByView[probe.View.ID]
				if !ok {
					return nil, fmt.Errorf("unexpected Pass 2 view id %q", probe.View.ID)
				}
				return json.Marshal(pv)
			}
			return json.Marshal(pass1)
		},
	}
}

func buildAcmePass1Plan(t *testing.T, table *synth.InternTable, expected *manifest.Manifest) synth.Pass1Plan {
	t.Helper()
	var pass1 synth.Pass1Plan
	for _, c := range expected.Components {
		meta := make([]synth.Pass1InlineMeta, 0, len(c.InlineMeta))
		for _, im := range c.InlineMeta {
			meta = append(meta, synth.Pass1InlineMeta{Label: im.Label, Value: im.Value})
		}
		pass1.Components = append(pass1.Components, synth.Pass1Component{
			ID:            c.ID,
			Title:         c.Title,
			Subtitle:      c.Subtitle,
			IconKey:       c.IconKey,
			ComponentKind: c.ComponentKind,
			ProjectID:     c.ProjectID,
			BackedByIDs:   internBackedByURIs(t, table, c.BackedBy),
			InlineMeta:    meta,
		})
	}
	for _, ct := range expected.Containers {
		pass1.ContainerDefs = append(pass1.ContainerDefs, synth.Pass1ContainerDef{
			ID:                ct.ID,
			Title:             ct.Title,
			Subtitle:          ct.Subtitle,
			Kind:              ct.Kind,
			ParentContainerID: ct.ParentContainerID,
			BackedByIDs:       internBackedByURIs(t, table, ct.BackedBy),
		})
	}
	for _, ex := range expected.ExcludedResources {
		rid, ok := table.ResourceID(ex.URI)
		if !ok {
			t.Fatalf("expected excluded URI %q not found in facts intern table", ex.URI)
		}
		pass1.ExcludedResources = append(pass1.ExcludedResources, synth.Pass1ExcludedResource{
			ResourceID: rid,
			Reason:     ex.Reason,
		})
	}
	return pass1
}

func internBackedByURIs(t *testing.T, table *synth.InternTable, refs []manifest.BackedResource) []string {
	t.Helper()
	rids := make([]string, 0, len(refs))
	for _, br := range refs {
		rid, ok := table.ResourceID(br.URI)
		if !ok {
			t.Fatalf("expected URI %q not found in facts intern table", br.URI)
		}
		rids = append(rids, rid)
	}
	return rids
}

func buildAcmePass2Views(t *testing.T, table *synth.InternTable, expected *manifest.Manifest, pass1 *synth.Pass1Plan) map[string]synth.Pass2View {
	t.Helper()
	pass2ByView := make(map[string]synth.Pass2View, len(expected.Views))
	for _, v := range expected.Views {
		if v.ID == manifest.SystemIndexViewID {
			continue
		}
		compIDs, placements := buildAcmePass2Placements(v.Placements)
		edges := buildAcmePass2Edges(t, table, v.ID, v.Edges)

		pass1.DetailViews = append(pass1.DetailViews, synth.Pass1DetailView{
			ID:           v.ID,
			Title:        v.Title,
			Subtitle:     v.Subtitle,
			Archetype:    v.Archetype,
			AccentMode:   v.AccentMode,
			ComponentIDs: compIDs,
		})
		pass2ByView[v.ID] = synth.Pass2View{
			ContainerIDs: v.ContainerIDs,
			Placements:   placements,
			Edges:        edges,
		}
	}
	return pass2ByView
}

func buildAcmePass2Placements(src []manifest.Placement) ([]string, []synth.Pass2Placement) {
	compIDs := make([]string, 0, len(src))
	placements := make([]synth.Pass2Placement, 0, len(src))
	for _, p := range src {
		compIDs = append(compIDs, p.ComponentID)
		refs := make([]synth.Pass2OffPageRef, 0, len(p.OffPageRefs))
		for _, r := range p.OffPageRefs {
			refs = append(refs, synth.Pass2OffPageRef{
				TargetViewID: r.TargetViewID,
				Label:        r.Label,
				Direction:    r.Direction,
			})
		}
		placements = append(placements, synth.Pass2Placement{
			ComponentID: p.ComponentID,
			ContainerID: p.ContainerID,
			Column:      p.Column,
			OffPageRefs: refs,
		})
	}
	return compIDs, placements
}

func buildAcmePass2Edges(t *testing.T, table *synth.InternTable, viewID string, src []manifest.Edge) []synth.Pass2Edge {
	t.Helper()
	edges := make([]synth.Pass2Edge, 0, len(src))
	for _, e := range src {
		shortIDs := make([]string, 0, len(e.EvidenceEdgeIDs))
		for _, hashID := range e.EvidenceEdgeIDs {
			eid, ok := table.EdgeID(hashID)
			if !ok {
				t.Fatalf("expected evidence edge %q (view %s: %s->%s) missing in facts intern table", hashID, viewID, e.From, e.To)
			}
			shortIDs = append(shortIDs, eid)
		}
		edges = append(edges, synth.Pass2Edge{
			From:               e.From,
			To:                 e.To,
			Verb:               e.Verb,
			FlowKind:           e.FlowKind,
			Provenance:         e.Provenance,
			EvidenceEdgeIDs:    shortIDs,
			InferenceRationale: e.InferenceRationale,
			ViaBusID:           e.ViaBusID,
		})
	}
	return edges
}

func TestAcmeCICDOfflineEndToEndAcceptance(t *testing.T) {
	fixtureFactsPath, fixtureExpectedPath := acmeFixturePaths(t)
	fixtureFacts, err := extract.LoadFacts(fixtureFactsPath)
	if err != nil {
		t.Fatalf("LoadFacts: %v", err)
	}
	expectedManifest, err := manifest.Load(fixtureExpectedPath)
	if err != nil {
		t.Fatalf("manifest.Load: %v", err)
	}
	reg, err := icons.Load()
	if err != nil {
		t.Fatalf("icons.Load: %v", err)
	}

	tmp := t.TempDir()
	factsPath := filepath.Join(tmp, "facts.json")
	archPath := filepath.Join(tmp, "architecture.yaml")
	outDir := filepath.Join(tmp, "out")

	t.Run("1_extract_cli", func(t *testing.T) {
		runE2EStage1Extract(t, fixtureFacts, factsPath)
	})
	t.Run("2_synthesize_and_validate_provenance", func(t *testing.T) {
		runE2EStage2SynthesizeAndValidate(t, fixtureFacts, expectedManifest, reg, factsPath, archPath)
	})
	t.Run("3_render_headless_chrome_and_collision_gates", func(t *testing.T) {
		runE2EStage3RenderAndCollisionGates(t, tmp, archPath, outDir)
	})
	t.Run("4_day2_reconcile_fast_path_and_delta_notes_preservation", func(t *testing.T) {
		runE2EStage4Day2Reconcile(t, fixtureFacts, tmp, factsPath, archPath)
	})
}

func runE2EStage1Extract(t *testing.T, fixtureFacts *extract.FactsFile, factsPath string) {
	t.Helper()
	deps := appDeps{
		newExtractClients: func(_ context.Context, opts extract.ClientOptions) (*extract.Clients, error) {
			return &extract.Clients{QuotaProject: opts.QuotaProject, ProgressWriter: opts.ProgressWriter}, nil
		},
		extractFacts: func(_ context.Context, _ *extract.Clients, _ extract.ExtractOptions) (*extract.FactsFile, error) {
			return fixtureFacts, nil
		},
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"extract", "--projects", "acme-cicd", "-o", factsPath}, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("extract exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	fi, err := os.Stat(factsPath)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("facts.json stat err=%v mode=%o, want 0600", err, fi.Mode().Perm())
	}
	for _, target := range []string{"acme-dev", "acme-prod"} {
		if !strings.Contains(stderr.String(), target) {
			t.Errorf("extract stderr missing out-of-scope target %q: %s", target, stderr.String())
		}
	}

	forbiddenTypes := map[string]bool{
		"k8s.io/ServiceAccount":               true,
		"k8s.io/Secret":                       true,
		"k8s.io/Endpoints":                    true,
		"k8s.io/ConfigMap":                    true,
		"k8s.io/Pod":                          true,
		"apps.k8s.io/ReplicaSet":              true,
		"sqladmin.googleapis.com/BackupRun":   true,
		"sqladmin.googleapis.com/Backup":      true,
		"cloudbuild.googleapis.com/Build":     true,
		"firestore.googleapis.com/Database":   true,
		"redis.googleapis.com/Instance":       true,
		"memcache.googleapis.com/Instance":    true,
		"osconfig.googleapis.com/OSPolicy":    true,
		"serviceusage.googleapis.com/Service": true,
	}
	for _, r := range fixtureFacts.Resources {
		if forbiddenTypes[r.AssetType] {
			t.Errorf("facts.json leaked forbidden plumbing/false-service resource %s (%s)", r.URI, r.AssetType)
		}
		lowURI := strings.ToLower(r.URI)
		for _, noise := range []string{"firestore", "redis", "memcache", "osconfig"} {
			if strings.Contains(lowURI, noise) {
				t.Errorf("facts.json leaked terraform-plan false service %q in URI %s", noise, r.URI)
			}
		}
	}
}

func runE2EStage2SynthesizeAndValidate(t *testing.T, fixtureFacts *extract.FactsFile, expectedManifest *manifest.Manifest, reg *icons.Registry, factsPath, archPath string) {
	t.Helper()
	deps := appDeps{
		newModelClient: func(_ context.Context, _ synth.VertexClientOptions) (synth.ModelClient, error) {
			return buildAcmeMockModelClient(t, fixtureFacts, expectedManifest), nil
		},
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"synthesize", factsPath, "--vertex-project", "consultant-ai-sandbox", "-o", archPath}, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("synthesize exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"validate", archPath}, &stdout, &stderr, appDeps{}); code != exitOK {
		t.Fatalf("validate offline exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"validate", archPath, "--facts", factsPath, "--json"}, &stdout, &stderr, appDeps{}); code != exitOK {
		t.Fatalf("validate --facts exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	var rep manifest.ValidationReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil || !rep.Valid {
		t.Fatalf("validate --facts JSON report invalid: err=%v rep=%+v", err, rep)
	}

	synthesized, err := manifest.Load(archPath)
	if err != nil {
		t.Fatalf("manifest.Load(%s): %v", archPath, err)
	}
	if len(synthesized.Views) != 4 || synthesized.Views[0].ID != manifest.SystemIndexViewID {
		t.Fatalf("expected 4 views with view_0_system_index at [0], got %d views", len(synthesized.Views))
	}

	assertE2ECoverageAndProvenance(t, synthesized, fixtureFacts, reg)
}

func assertE2ECoverageAndProvenance(t *testing.T, synthesized *manifest.Manifest, fixtureFacts *extract.FactsFile, reg *icons.Registry) {
	t.Helper()
	covRows := manifest.ComputeCoverage(synthesized)
	if len(covRows) != len(fixtureFacts.Resources) {
		t.Fatalf("ComputeCoverage rows = %d, want 100%% of facts resources (%d)", len(covRows), len(fixtureFacts.Resources))
	}
	covURIs := make(map[string]bool, len(covRows))
	for _, row := range covRows {
		covURIs[row.URI] = true
	}
	for _, r := range fixtureFacts.Resources {
		if !covURIs[r.URI] {
			t.Errorf("ComputeCoverage missing URI %s", r.URI)
		}
	}

	if err := manifest.ValidateWithFacts(synthesized, fixtureFacts, reg); err != nil {
		t.Fatalf("ValidateWithFacts failed: %v", err)
	}
	assertE2ERequiredVerifiedEdges(t, synthesized)
	assertE2ENoForbiddenComponents(t, synthesized)
}

func assertE2ERequiredVerifiedEdges(t *testing.T, synthesized *manifest.Manifest) {
	t.Helper()
	requiredVerified := map[string]bool{
		"cicd_delivery_pipeline|github_acme->cloudbuild_triggers":            false,
		"cicd_delivery_pipeline|cloudbuild_triggers->acme_artifact_registry": false,
		"gke_developer_platform|atlantis_https_lb->atlantis":                 false,
		"gke_developer_platform|atlantis->acme_tfstate":                      false,
		"gke_developer_platform|atlantis->acme_private_modules":              false,
		"gke_developer_platform|sonarqube->sonarqube_db":                     false,
	}
	for _, v := range synthesized.Views {
		checkViewRequiredEdges(t, v, requiredVerified)
	}
	for k, found := range requiredVerified {
		if !found {
			t.Errorf("missing required VERIFIED edge %s", k)
		}
	}
}

func checkViewRequiredEdges(t *testing.T, v manifest.View, required map[string]bool) {
	t.Helper()
	for _, e := range v.Edges {
		key := v.ID + "|" + e.From + "->" + e.To
		if _, want := required[key]; !want {
			continue
		}
		if e.Provenance != manifest.ProvenanceVerified || len(e.EvidenceEdgeIDs) == 0 {
			t.Errorf("required edge %s must be VERIFIED with evidence_edge_ids, got provenance=%s ids=%v", key, e.Provenance, e.EvidenceEdgeIDs)
		}
		required[key] = true
	}
}

func assertE2ENoForbiddenComponents(t *testing.T, synthesized *manifest.Manifest) {
	t.Helper()
	forbiddenTerms := []string{"unclassified", "backuprun", "firestore", "redis", "memcache", "osconfig"}
	for _, c := range synthesized.Components {
		low := strings.ToLower(c.ID + " " + c.Title)
		for _, forbidden := range forbiddenTerms {
			if strings.Contains(low, forbidden) {
				t.Errorf("synthesized component %q (%q) contains forbidden term %q", c.ID, c.Title, forbidden)
			}
		}
	}
}

func runE2EStage3RenderAndCollisionGates(t *testing.T, tmp, archPath, outDir string) {
	t.Helper()
	chromePath := requireChromeForE2E(t)

	deps := appDeps{
		newBrowserRunner: func(explicit string) (render.BrowserRunner, error) {
			p := explicit
			if p == "" {
				p = chromePath
			}
			return &render.ChromeDPRunner{ChromePath: p}, nil
		},
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"render", archPath, "--format", "pdf,png,html", "-o", outDir}, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("render exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}

	m, err := manifest.Load(archPath)
	if err != nil {
		t.Fatal(err)
	}
	assertE2ERenderedHTMLAndPDF(t, outDir, len(m.Views))
	assertE2ERenderedPNGs(t, outDir)

	realRunner := &render.ChromeDPRunner{ChromePath: chromePath}
	assertE2ECardCollisionExit2(t, realRunner, tmp, archPath)
	assertE2ECollinearCollisionExit2(t, realRunner, tmp, archPath)
}

func assertE2ERenderedHTMLAndPDF(t *testing.T, outDir string, wantPages int) {
	t.Helper()
	htmlBytes, err := os.ReadFile(filepath.Join(outDir, "architecture.html"))
	if err != nil {
		t.Fatalf("read out/architecture.html: %v", err)
	}
	htmlStr := string(htmlBytes)
	if !strings.Contains(htmlStr, `data-gcp-arch-routed="1"`) {
		t.Fatalf("out/architecture.html missing data-gcp-arch-routed=\"1\"")
	}
	if count := strings.Count(htmlStr, `class="connector-layer"`); count != 4 {
		t.Fatalf("expected 4 routed connector-layer SVGs across 4 views, got %d", count)
	}
	if strings.Contains(htmlStr, "Inventory Coverage Table") || strings.Contains(htmlStr, `class="coverage-sheet"`) {
		t.Fatalf("unexpected Inventory Coverage Table section in out/architecture.html")
	}

	pdfBytes, err := os.ReadFile(filepath.Join(outDir, "architecture.pdf"))
	if err != nil {
		t.Fatalf("read out/architecture.pdf: %v", err)
	}
	if !bytes.HasPrefix(pdfBytes, []byte("%PDF-")) {
		t.Fatalf("out/architecture.pdf missing %%PDF- header")
	}
	if gotPages := countPDFPages(pdfBytes); gotPages != wantPages {
		t.Fatalf("PDF /Type /Page count = %d, want %d", gotPages, wantPages)
	}
	if !pdfHasNonEmptyTextStream(pdfBytes) {
		t.Fatalf("out/architecture.pdf has no non-empty vector text streams")
	}
}

func assertE2ERenderedPNGs(t *testing.T, outDir string) {
	t.Helper()
	pngFiles, err := filepath.Glob(filepath.Join(outDir, "views", "*.png"))
	if err != nil {
		t.Fatalf("glob view PNGs: %v", err)
	}
	if len(pngFiles) != 4 {
		t.Fatalf("expected 4 view PNGs in out/views/, got %d (%v)", len(pngFiles), pngFiles)
	}
	for _, p := range pngFiles {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("decode PNG %s: %v", p, err)
		}
		if cfg.Width != 3120 {
			t.Fatalf("PNG %s width = %d, want 3120 (3x of 1040px)", filepath.Base(p), cfg.Width)
		}
	}
}

func assertE2ECardCollisionExit2(t *testing.T, realRunner *render.ChromeDPRunner, tmp, archPath string) {
	t.Helper()
	cardCollisionDeps := appDeps{
		newBrowserRunner: func(_ string) (render.BrowserRunner, error) {
			return &stubBrowserRunner{
				handler: func(ctx context.Context, req render.BrowserRenderRequest) (*render.BrowserOutput, error) {
					blocker := `<article class="arch-card" data-component-id="injected_blocker" style="position:absolute;left:120px;top:0px;width:640px;height:360px;z-index:9;">Blocker</article>`
					req.HTML = bytes.Replace(req.HTML, []byte(`<div class="corridor-col"`), []byte(blocker+`<div class="corridor-col"`), 1)
					return realRunner.Render(ctx, req)
				},
			}, nil
		},
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"render", archPath, "--format", "html", "-o", filepath.Join(tmp, "col-card")}, &stdout, &stderr, cardCollisionDeps); code != exitUsage {
		t.Fatalf("injected card collision exit=%d, want 2 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "injected_blocker") {
		t.Errorf("card collision stderr missing colliding subject ID: %s", stderr.String())
	}
}

func assertE2ECollinearCollisionExit2(t *testing.T, realRunner *render.ChromeDPRunner, tmp, archPath string) {
	t.Helper()
	collinearDeps := appDeps{
		newBrowserRunner: func(_ string) (render.BrowserRunner, error) {
			return &stubBrowserRunner{
				handler: func(ctx context.Context, req render.BrowserRenderRequest) (*render.BrowserOutput, error) {
					var fakeEdges []string
					for k := 0; k < 26; k++ {
						from, to := "atlantis_https_lb", "sonarqube"
						if k%2 == 1 {
							from, to = "sonarqube_https_lb", "atlantis"
						}
						fakeEdges = append(fakeEdges, fmt.Sprintf(`{"from":%q,"to":%q,"verb":"e%d","flow_kind":"RUNTIME","provenance":"INFERRED","waypoint_row":0,"lateral_corridor":0}`, from, to, k))
					}
					payload := fmt.Sprintf(`[{"id":"view_0_system_index","edges":[]},{"id":"cicd_delivery_pipeline","edges":[]},{"id":"gke_developer_platform","edges":[%s]},{"id":"shared_network_psa","edges":[]}]`, strings.Join(fakeEdges, ","))
					reData := regexp.MustCompile(`<script id="gcp-arch-routing-data" type="application/json">.*?</script>`)
					req.HTML = reData.ReplaceAll(req.HTML, []byte(`<script id="gcp-arch-routing-data" type="application/json">`+payload+`</script>`))
					return realRunner.Render(ctx, req)
				},
			}, nil
		},
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"render", archPath, "--format", "html", "-o", filepath.Join(tmp, "col-line")}, &stdout, &stderr, collinearDeps); code != exitUsage {
		t.Fatalf("injected collinear collision exit=%d, want 2 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "collinear_collision") {
		t.Errorf("collinear collision stderr missing collinear_collision: %s", stderr.String())
	}
}

func runE2EStage4Day2Reconcile(t *testing.T, fixtureFacts *extract.FactsFile, tmp, factsPath, archPath string) {
	t.Helper()
	assertE2EDay2FastPath(t, factsPath, archPath)
	assertE2EDay2DeltaNotesPreservation(t, fixtureFacts, tmp, archPath)
}

func assertE2EDay2FastPath(t *testing.T, factsPath, archPath string) {
	t.Helper()
	beforeBytes, err := os.ReadFile(archPath)
	if err != nil {
		t.Fatal(err)
	}
	var llmCalls atomic.Int32
	fastDeps := appDeps{
		newModelClient: func(_ context.Context, _ synth.VertexClientOptions) (synth.ModelClient, error) {
			llmCalls.Add(1)
			return nil, fmt.Errorf("LLM must not be invoked on unchanged fast path")
		},
	}
	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := run([]string{"synthesize", factsPath, "--reconcile", archPath}, &stdout, &stderr, fastDeps)
	elapsed := time.Since(start)
	if code != exitOK {
		t.Fatalf("unchanged --reconcile exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if llmCalls.Load() != 0 {
		t.Fatalf("expected 0 LLM calls on fast path, got %d", llmCalls.Load())
	}
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("0-LLM fast path took %v, want < 100ms", elapsed)
	}
	afterFastBytes, err := os.ReadFile(archPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeBytes, afterFastBytes) {
		t.Fatalf("unchanged --reconcile produced non-zero diff")
	}
}

func assertE2EDay2DeltaNotesPreservation(t *testing.T, fixtureFacts *extract.FactsFile, tmp, archPath string) {
	t.Helper()
	const curatedTitle = "Atlantis PR Automation (Curated)"
	const curatedNote = "Client-approved PR automation boundary; preserve across Day-2 updates"

	curateAtlantisComponent(t, archPath, curatedTitle, curatedNote)

	day2FactsPath, webhookRID, webhookEID := writeE2EDay2DeltaFacts(t, fixtureFacts, tmp)
	var deltaCalls atomic.Int32
	deltaDeps := buildE2EDeltaModelDeps(&deltaCalls, webhookRID, webhookEID)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"synthesize", day2FactsPath, "--reconcile", archPath, "--vertex-project", "consultant-ai-sandbox"}, &stdout, &stderr, deltaDeps); code != exitOK {
		t.Fatalf("delta --reconcile exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if deltaCalls.Load() != 1 {
		t.Fatalf("expected 1 LLM call for Day-2 delta patch, got %d", deltaCalls.Load())
	}

	assertE2EReconciledComponents(t, archPath, curatedTitle, curatedNote)

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"validate", archPath, "--facts", day2FactsPath}, &stdout, &stderr, appDeps{}); code != exitOK {
		t.Fatalf("validate after Day-2 reconcile exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
}

func curateAtlantisComponent(t *testing.T, archPath, title, note string) {
	t.Helper()
	curated, err := manifest.Load(archPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := range curated.Components {
		if curated.Components[i].ID == "atlantis" {
			curated.Components[i].Title = title
			curated.Components[i].Notes = note
		}
	}
	if err := manifest.Write(archPath, curated); err != nil {
		t.Fatal(err)
	}
}

func assertE2EReconciledComponents(t *testing.T, archPath, curatedTitle, curatedNote string) {
	t.Helper()
	reconciled, err := manifest.Load(archPath)
	if err != nil {
		t.Fatalf("reload reconciled architecture.yaml: %v", err)
	}
	byID := make(map[string]manifest.Component, len(reconciled.Components))
	for _, c := range reconciled.Components {
		byID[c.ID] = c
	}
	atlantis, sawCuratedAtlantis := byID["atlantis"]
	if sawCuratedAtlantis && (atlantis.Title != curatedTitle || atlantis.Notes != curatedNote) {
		t.Fatalf("Day-2 reconcile clobbered curated fields on atlantis: title=%q notes=%q", atlantis.Title, atlantis.Notes)
	}
	_, sawWebhook := byID["release_webhook"]
	if !sawCuratedAtlantis || !sawWebhook {
		t.Fatalf("expected curated atlantis (%v) and new release_webhook (%v)", sawCuratedAtlantis, sawWebhook)
	}
}

func writeE2EDay2DeltaFacts(t *testing.T, fixtureFacts *extract.FactsFile, tmp string) (path, webhookRID, webhookEID string) {
	t.Helper()
	day2Facts := *fixtureFacts
	day2Facts.Resources = append(append([]extract.FactResource(nil), fixtureFacts.Resources...), extract.FactResource{
		URI:         "//run.googleapis.com/projects/acme-cicd/locations/europe-southwest1/services/release-webhook",
		AssetType:   "run.googleapis.com/Service",
		ProjectID:   "acme-cicd",
		Location:    "europe-southwest1",
		DisplayName: "release-webhook",
	})
	newEdge := extract.FactEdge{
		FromURI: "//cloudbuild.googleapis.com/projects/acme-cicd/locations/europe-southwest1/triggers/deploy-api-prod",
		ToURI:   "//run.googleapis.com/projects/acme-cicd/locations/europe-southwest1/services/release-webhook",
		Kind:    extract.EdgeCloudBuildStepRef,
		Detail:  "gcloud run deploy release-webhook",
	}
	newEdge.ID = extract.EdgeID(newEdge.Kind, newEdge.FromURI, newEdge.ToURI, newEdge.Detail)
	day2Facts.Edges = append(append([]extract.FactEdge(nil), fixtureFacts.Edges...), newEdge)

	path = filepath.Join(tmp, "facts_day2.json")
	if err := extract.WriteFacts(path, &day2Facts); err != nil {
		t.Fatal(err)
	}
	day2Table := synth.NewInternTable(&day2Facts)
	var ok bool
	webhookRID, ok = day2Table.ResourceID("//run.googleapis.com/projects/acme-cicd/locations/europe-southwest1/services/release-webhook")
	if !ok {
		t.Fatal("release-webhook missing from Day-2 intern table")
	}
	webhookEID, ok = day2Table.EdgeID(newEdge.ID)
	if !ok {
		t.Fatal("newEdge missing from Day-2 intern table")
	}
	return path, webhookRID, webhookEID
}

func buildE2EDeltaModelDeps(deltaCalls *atomic.Int32, webhookRID, webhookEID string) appDeps {
	return appDeps{
		newModelClient: func(_ context.Context, _ synth.VertexClientOptions) (synth.ModelClient, error) {
			return &stubModelClient{
				handler: func(int, string, string, *genai.Schema) ([]byte, error) {
					deltaCalls.Add(1)
					return json.Marshal(synth.ReconcilePatch{
						NewComponents: []synth.Pass1Component{{
							ID:            "release_webhook",
							Title:         "Release Webhook",
							Subtitle:      "Cloud Run / europe-southwest1",
							IconKey:       "cloud_run",
							ComponentKind: manifest.KindWorkload,
							ProjectID:     "acme-cicd",
							BackedByIDs:   []string{webhookRID},
						}},
						NewPlacements: []synth.ReconcileNewPlacement{{
							ViewID: "cicd_delivery_pipeline",
							Placement: synth.Pass2Placement{
								ComponentID: "release_webhook",
								ContainerID: "proj_acme_cicd",
								Column:      3,
							},
						}},
						NewEdges: []synth.ReconcileNewEdge{{
							ViewID: "cicd_delivery_pipeline",
							Edge: synth.Pass2Edge{
								From:            "cloudbuild_triggers",
								To:              "release_webhook",
								Verb:            "deploys webhook",
								FlowKind:        manifest.FlowCICD,
								Provenance:      manifest.ProvenanceVerified,
								EvidenceEdgeIDs: []string{webhookEID},
							},
						}},
					})
				},
			}, nil
		},
	}
}
