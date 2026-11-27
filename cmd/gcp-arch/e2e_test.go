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

func buildAnnotationStubClient(calls *atomic.Int32) synth.ModelClient {
	return &stubModelClient{
		handler: func(int, string, string, *synth.Schema) ([]byte, error) {
			if calls != nil {
				calls.Add(1)
			}
			return []byte(`{"focus_summary":"ACME CI/CD platform automating container builds and GKE deployments.","detail_views":[],"node_enrichments":[],"edge_enrichments":[]}`), nil
		},
	}
}

func TestAcmeCICDOfflineEndToEndAcceptance(t *testing.T) {
	fixtureFactsPath, _ := acmeFixturePaths(t)
	fixtureFacts, err := extract.LoadFacts(fixtureFactsPath)
	if err != nil {
		t.Fatalf("LoadFacts: %v", err)
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
		runE2EStage2SynthesizeAndValidate(t, fixtureFacts, reg, factsPath, archPath)
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
				t.Errorf("facts.json leaked false service %q in URI %s", noise, r.URI)
			}
		}
	}
}

func runE2EStage2SynthesizeAndValidate(t *testing.T, fixtureFacts *extract.FactsFile, reg *icons.Registry, factsPath, archPath string) {
	t.Helper()
	deps := appDeps{
		newModelClient: func(_ context.Context, _ synth.VertexClientOptions) (synth.ModelClient, error) {
			return buildAnnotationStubClient(nil), nil
		},
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"synthesize", factsPath, "--vertex-project", "my-vertex-project", "-o", archPath}, &stdout, &stderr, deps); code != exitOK {
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
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil || !rep.Valid || rep.Conservation == nil || rep.Conservation.Status != "PASS" {
		t.Fatalf("validate --facts JSON report invalid: err=%v rep=%+v", err, rep)
	}

	synthesized, err := manifest.Load(archPath)
	if err != nil {
		t.Fatalf("manifest.Load(%s): %v", archPath, err)
	}
	if len(synthesized.Views) < 2 || synthesized.Views[0].ID != manifest.SystemIndexViewID {
		t.Fatalf("expected >= 2 views with view_0_system_index at [0], got %d views", len(synthesized.Views))
	}

	if err := manifest.ValidateWithFacts(synthesized, fixtureFacts, reg); err != nil {
		t.Fatalf("ValidateWithFacts failed: %v", err)
	}
	assertE2ENoForbiddenComponents(t, synthesized)
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
		newBrowserRunner: func(explicit, _ string) (render.BrowserRunner, error) {
			p := explicit
			if p == "" {
				p = chromePath
			}
			return &render.ChromeDPRunner{ChromePath: p}, nil
		},
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"render", archPath, "--format", "pdf,png,html", "--geometry-report", "-o", outDir}, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("render exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}

	m, err := manifest.Load(archPath)
	if err != nil {
		t.Fatal(err)
	}
	assertE2ERenderedHTMLAndPDF(t, outDir, len(m.Views))
	assertE2ERenderedPNGs(t, outDir, len(m.Views))

	realRunner := &render.ChromeDPRunner{ChromePath: chromePath}
	assertE2ECardCollisionExit2(t, realRunner, tmp, archPath)
	assertE2EEllipsisCollisionExit2(t, realRunner, tmp, archPath)
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
	if count := strings.Count(htmlStr, `class="connector-layer"`); count != wantPages {
		t.Fatalf("expected %d routed connector-layer SVGs across %d views, got %d", wantPages, wantPages, count)
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

func assertE2ERenderedPNGs(t *testing.T, outDir string, wantViews int) {
	t.Helper()
	pngFiles, err := filepath.Glob(filepath.Join(outDir, "views", "*.png"))
	if err != nil {
		t.Fatalf("glob view PNGs: %v", err)
	}
	if len(pngFiles) != wantViews {
		t.Fatalf("expected %d view PNGs in out/views/, got %d (%v)", wantViews, len(pngFiles), pngFiles)
	}
	rawGeo, err := os.ReadFile(filepath.Join(outDir, "geometry_report.json"))
	if err != nil {
		t.Fatalf("read geometry_report.json: %v", err)
	}
	var geo render.GeometryReport
	if err := json.Unmarshal(rawGeo, &geo); err != nil {
		t.Fatalf("decode geometry_report.json: %v", err)
	}
	if !geo.AllPass() || len(geo.Views) != len(pngFiles) {
		t.Fatalf("geometry_report.json failed or count mismatch: %+v", geo)
	}
	for i, p := range pngFiles {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("decode PNG %s: %v", p, err)
		}
		vr := geo.Views[i]
		if cfg.Width != vr.WidthPx || cfg.Height != vr.HeightPx {
			t.Fatalf("PNG %s %dx%d != report %dx%d", filepath.Base(p), cfg.Width, cfg.Height, vr.WidthPx, vr.HeightPx)
		}
		if cfg.Width < render.PNGMinWidth || cfg.Height < render.PNGMinHeight || vr.AspectRatio < render.PNGMinAspectRatio || vr.AspectRatio > render.PNGMaxAspectRatio {
			t.Fatalf("PNG %s dimensions/AR out of bounds: %dx%d ar=%.3f", filepath.Base(p), cfg.Width, cfg.Height, vr.AspectRatio)
		}
	}
}

func assertE2ECardCollisionExit2(t *testing.T, realRunner *render.ChromeDPRunner, tmp, archPath string) {
	t.Helper()
	cardCollisionDeps := appDeps{
		newBrowserRunner: func(_, _ string) (render.BrowserRunner, error) {
			return &stubBrowserRunner{
				handler: func(ctx context.Context, req render.BrowserRenderRequest) (*render.BrowserOutput, error) {
					blocker := `<article class="arch-card" data-component-id="injected_blocker" style="position:absolute;left:20px;top:20px;width:240px;height:140px;z-index:9;">Blocker</article>`
					req.HTML = bytes.Replace(req.HTML, []byte(`<svg class="connector-layer"></svg>`), []byte(`<svg class="connector-layer"></svg>`+blocker), 1)
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

func assertE2EEllipsisCollisionExit2(t *testing.T, realRunner *render.ChromeDPRunner, tmp, archPath string) {
	t.Helper()
	ellipsisDeps := appDeps{
		newBrowserRunner: func(_, _ string) (render.BrowserRunner, error) {
			return &stubBrowserRunner{
				handler: func(ctx context.Context, req render.BrowserRenderRequest) (*render.BrowserOutput, error) {
					req.HTML = bytes.Replace(req.HTML, []byte("</head>"), []byte(`<style>.arch-card-title{text-overflow:ellipsis;overflow:hidden;white-space:nowrap;}</style></head>`), 1)
					return realRunner.Render(ctx, req)
				},
			}, nil
		},
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"render", archPath, "--format", "html", "-o", filepath.Join(tmp, "col-ellipsis")}, &stdout, &stderr, ellipsisDeps); code != exitUsage {
		t.Fatalf("injected text ellipsis collision exit=%d, want 2 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "text_ellipsis") {
		t.Errorf("ellipsis collision stderr missing text_ellipsis: %s", stderr.String())
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
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("0-LLM fast path took %v, want < 500ms", elapsed)
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

	curatedID := curateFirstAtlantisComponent(t, archPath, curatedTitle, curatedNote)
	day2FactsPath := writeE2EDay2DeltaFacts(t, fixtureFacts, tmp)

	var deltaCalls atomic.Int32
	deltaDeps := appDeps{
		newModelClient: func(_ context.Context, _ synth.VertexClientOptions) (synth.ModelClient, error) {
			return buildAnnotationStubClient(&deltaCalls), nil
		},
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"synthesize", day2FactsPath, "--reconcile", archPath, "--vertex-project", "my-vertex-project"}, &stdout, &stderr, deltaDeps); code != exitOK {
		t.Fatalf("delta --reconcile exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if deltaCalls.Load() != 1 {
		t.Fatalf("expected 1 LLM call for Day-2 delta reconcile, got %d", deltaCalls.Load())
	}

	assertE2EReconciledComponents(t, archPath, curatedID, curatedTitle, curatedNote)

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"validate", archPath, "--facts", day2FactsPath}, &stdout, &stderr, appDeps{}); code != exitOK {
		t.Fatalf("validate after Day-2 reconcile exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
}

func curateFirstAtlantisComponent(t *testing.T, archPath, title, note string) string {
	t.Helper()
	curated, err := manifest.Load(archPath)
	if err != nil {
		t.Fatal(err)
	}
	targetID := ""
	for i := range curated.Components {
		if strings.Contains(curated.Components[i].ID, "atlantis") {
			curated.Components[i].Title = title
			curated.Components[i].Notes = note
			targetID = curated.Components[i].ID
			break
		}
	}
	if targetID == "" {
		t.Fatal("no atlantis component found in synthesized manifest")
	}
	if err := manifest.Write(archPath, curated); err != nil {
		t.Fatal(err)
	}
	return targetID
}

func assertE2EReconciledComponents(t *testing.T, archPath, curatedID, curatedTitle, curatedNote string) {
	t.Helper()
	reconciled, err := manifest.Load(archPath)
	if err != nil {
		t.Fatalf("reload reconciled architecture.yaml: %v", err)
	}
	var sawCurated, sawWebhook bool
	for _, c := range reconciled.Components {
		if c.ID == curatedID {
			sawCurated = true
			if c.Title != curatedTitle || c.Notes != curatedNote {
				t.Fatalf("Day-2 reconcile clobbered curated fields on %s: title=%q notes=%q", c.ID, c.Title, c.Notes)
			}
		}
		if strings.Contains(c.ID, "release_webhook") {
			sawWebhook = true
		}
	}
	if !sawCurated || !sawWebhook {
		t.Fatalf("expected curated %s (%v) and new release_webhook (%v)", curatedID, sawCurated, sawWebhook)
	}
}

func writeE2EDay2DeltaFacts(t *testing.T, fixtureFacts *extract.FactsFile, tmp string) string {
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

	path := filepath.Join(tmp, "facts_day2.json")
	if err := extract.WriteFacts(path, &day2Facts); err != nil {
		t.Fatal(err)
	}
	return path
}
