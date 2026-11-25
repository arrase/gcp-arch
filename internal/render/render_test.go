package render

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/arrase/gcp-arch/internal/icons"
	"github.com/arrase/gcp-arch/internal/manifest"
)

func loadTestIcons(t *testing.T) *icons.Registry {
	t.Helper()
	reg, err := icons.Load()
	if err != nil {
		t.Fatalf("icons.Load: %v", err)
	}
	return reg
}

func loadAcmeManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "acme_cicd_expected.architecture.yaml")
	m, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("manifest.Load: %v", err)
	}
	return m
}

func TestBundledWOFF2FontHeaders(t *testing.T) {
	const woff2Magic = uint32(0x774F4632) // "wOF2"
	if len(bundledFonts) != 6 {
		t.Fatalf("expected 6 bundled fonts, got %d", len(bundledFonts))
	}
	for _, f := range bundledFonts {
		data, err := embeddedAssets.ReadFile(f.Path)
		if err != nil {
			t.Fatalf("read %s: %v", f.Path, err)
		}
		if len(data) < 4 {
			t.Fatalf("font %s truncated (%d bytes)", f.Path, len(data))
		}
		got := binary.BigEndian.Uint32(data[:4])
		if got != woff2Magic {
			t.Fatalf("font %s magic = 0x%08X, want wOF2 (0x%08X)", f.Path, got, woff2Magic)
		}
	}
}

func TestBuildHTMLCSPXSSAndCoverage(t *testing.T) {
	reg := loadTestIcons(t)
	payload := `</script><img src=x onerror=alert(1)>`

	m := &manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Metadata:      manifest.Metadata{GeneratedAt: "2026-11-23T02:00:00Z", Scopes: []string{"projects/xss-test"}},
		Components: []manifest.Component{
			{
				ID:            "src",
				Title:         "Source " + payload,
				Subtitle:      "sub " + payload,
				IconKey:       "cloud_run",
				ComponentKind: manifest.KindWorkload,
				ProjectID:     "xss-test",
				BackedBy:      []manifest.BackedResource{{URI: "//run.googleapis.com/projects/xss-test/locations/us-central1/services/src", AssetType: "run.googleapis.com/Service", ProjectID: "xss-test"}},
				InlineMeta:    []manifest.InlineMeta{{Label: "Env " + payload, Value: "val " + payload}},
			},
			{
				ID:            "dst",
				Title:         "Target DB",
				IconKey:       "cloud_sql",
				ComponentKind: manifest.KindDatastore,
				ProjectID:     "xss-test",
				BackedBy:      []manifest.BackedResource{{URI: "//sqladmin.googleapis.com/projects/xss-test/instances/dst", AssetType: "sqladmin.googleapis.com/Instance", ProjectID: "xss-test"}},
			},
		},
		ExcludedResources: []manifest.ExcludedResource{
			{URI: "//storage.googleapis.com/excluded", AssetType: "storage.googleapis.com/Bucket", ProjectID: "xss-test", Reason: "reason " + payload},
		},
		Views: []manifest.View{
			{
				ID:         "v1",
				Title:      "View " + payload,
				Archetype:  manifest.ArchetypeLandscapeMatrix,
				AccentMode: manifest.AccentRuntime,
				Placements: []manifest.Placement{
					{ComponentID: "src", Column: 0},
					{ComponentID: "dst", Column: 2},
				},
				Edges: []manifest.Edge{
					{From: "src", To: "dst", Verb: "<b>sql</b>", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceInferred, InferenceRationale: "rationale " + payload},
				},
			},
		},
	}

	raw, err := BuildHTML(m, reg)
	if err != nil {
		t.Fatalf("BuildHTML: %v", err)
	}
	html := string(raw)

	if !strings.Contains(html, ContentSecurityPolicy) {
		t.Fatalf("missing strict Content-Security-Policy header")
	}
	if !strings.Contains(html, `data:font/woff2;base64,`) {
		t.Fatalf("missing embedded base64 woff2 @font-face rules")
	}
	if !strings.Contains(html, `<symbol id="gcp-icon-cloud_run"`) || !strings.Contains(html, `<symbol id="gcp-icon-cloud_sql"`) {
		t.Fatalf("missing embedded SVG icon symbols")
	}
	if strings.Contains(html, "Inventory Coverage Table") || strings.Contains(html, `class="coverage-sheet"`) {
		t.Fatalf("unexpected Inventory Coverage Table section in rendered HTML")
	}
	// Ensure zero raw XSS payloads survived unescaped anywhere in HTML or JSON <script>.
	if strings.Contains(html, `<img src=x onerror=alert(1)>`) {
		t.Fatalf("unescaped XSS payload detected in HTML output")
	}
	if strings.Contains(html, `<b>sql</b>`) {
		t.Fatalf("unescaped HTML tag in routing JSON")
	}
	// Verify only expected </script> closing tags exist (2 scripts: routing JSON + router.js).
	if count := strings.Count(html, "</script>"); count != 2 {
		t.Fatalf("expected exactly 2 </script> tags, got %d (possible script breakout)", count)
	}

	if _, err := BuildHTML(nil, reg); err == nil {
		t.Fatalf("expected error on nil manifest")
	}
	if _, err := BuildHTML(m, nil); err == nil {
		t.Fatalf("expected error on nil registry")
	}
}

func TestPrepareDocumentBarycenterWaypointsBusesAndSpans(t *testing.T) {
	reg := loadTestIcons(t)
	m := &manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Components: []manifest.Component{
			{ID: "a1", Title: "A1", IconKey: "cloud_run", ComponentKind: manifest.KindWorkload, ProjectID: "p", BackedBy: []manifest.BackedResource{{URI: "//u/a1", AssetType: "t", ProjectID: "p"}}},
			{ID: "a2", Title: "A2", IconKey: "cloud_run", ComponentKind: manifest.KindWorkload, ProjectID: "p", BackedBy: []manifest.BackedResource{{URI: "//u/a2", AssetType: "t", ProjectID: "p"}}},
			{ID: "b1", Title: "B1", IconKey: "cloud_run", ComponentKind: manifest.KindWorkload, ProjectID: "p", BackedBy: []manifest.BackedResource{{URI: "//u/b1", AssetType: "t", ProjectID: "p"}}},
			{ID: "b2", Title: "B2", IconKey: "cloud_run", ComponentKind: manifest.KindWorkload, ProjectID: "p", BackedBy: []manifest.BackedResource{{URI: "//u/b2", AssetType: "t", ProjectID: "p"}}},
			{ID: "c1", Title: "C1", IconKey: "cloud_sql", ComponentKind: manifest.KindDatastore, ProjectID: "p", BackedBy: []manifest.BackedResource{{URI: "//u/c1", AssetType: "t", ProjectID: "p"}}},
			{ID: "c2", Title: "C2", IconKey: "cloud_sql", ComponentKind: manifest.KindDatastore, ProjectID: "p", BackedBy: []manifest.BackedResource{{URI: "//u/c2", AssetType: "t", ProjectID: "p"}}},
		},
		Containers: []manifest.Container{
			{ID: "proj", Title: "Project", Kind: manifest.ContainerProject},
			{ID: "vpc", Title: "VPC", Kind: manifest.ContainerVPC, ParentContainerID: "proj"},
		},
		Views: []manifest.View{
			{
				ID:           "v1",
				Title:        "Crossing & Bus Test",
				Archetype:    manifest.ArchetypeStagePipeline,
				AccentMode:   manifest.AccentRuntime,
				ContainerIDs: []string{"proj", "vpc"},
				// Place b1/b2 inverted relative to a1/a2 so barycenter reordering flips them cleanly.
				Placements: []manifest.Placement{
					{ComponentID: "a1", ContainerID: "vpc", Column: 0},
					{ComponentID: "a2", ContainerID: "vpc", Column: 0},
					{ComponentID: "b2", ContainerID: "proj", Column: 1},
					{ComponentID: "b1", ContainerID: "proj", Column: 1},
					{ComponentID: "c1", ContainerID: "proj", Column: 2},
					{ComponentID: "c2", ContainerID: "proj", Column: 2},
				},
				Edges: []manifest.Edge{
					{From: "a1", To: "b1", Verb: "top", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e1"}},
					{From: "a2", To: "b2", Verb: "bottom", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e2"}},
					// Auto fan-out bus from b1 -> c1, c2
					{From: "b1", To: "c1", Verb: "write 1", FlowKind: manifest.FlowCICD, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e3"}},
					{From: "b1", To: "c2", Verb: "write 2", FlowKind: manifest.FlowCICD, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e4"}},
					// Multi-column edge a1 -> c2 (must emit .edge-waypoint)
					{From: "a1", To: "c2", Verb: "skip col", FlowKind: manifest.FlowAdmin, Provenance: manifest.ProvenanceInferred, InferenceRationale: "admin path"},
					// Same-column lateral edge c1 -> c2 (rightmost column -> left lateral detour)
					{From: "c1", To: "c2", Verb: "replicate", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceInferred, InferenceRationale: "replica"},
					// Same-column lateral edge a1 -> a2 (leftmost column -> right lateral detour)
					{From: "a1", To: "a2", Verb: "sync", FlowKind: manifest.FlowAdmin, Provenance: manifest.ProvenanceInferred, InferenceRationale: "local sync"},
				},
			},
		},
	}

	doc := PrepareDocument(m, reg)
	v := doc.Views[0]

	// Check barycenter ordered b1 above b2 in column 1.
	rowByID := map[string]int{}
	for _, c := range v.Cards {
		rowByID[c.ComponentID] = c.GridRow
	}
	if rowByID["b1"] >= rowByID["b2"] {
		t.Fatalf("expected barycenter sorting to place b1 (row %d) above b2 (row %d)", rowByID["b1"], rowByID["b2"])
	}

	// Check container depth & HasInnerTop detection (proj depth 1 wraps vpc depth 2 at row 0).
	if len(v.Containers) != 2 || !v.Containers[0].HasInnerTop || v.Containers[1].Depth != 2 {
		t.Fatalf("unexpected container spans: %+v", v.Containers)
	}

	// Check waypoints generated for multi-column edge a1 -> c2.
	if len(v.Waypoints) == 0 {
		t.Fatalf("expected .edge-waypoint generated for multi-column edge a1 -> c2")
	}

	// Check auto bus grouping for b1 -> c1, c2 and lateral detours for a1->a2 & c1->c2.
	assertBarycenterRoutingEdges(t, v.Edges)
	if len(v.Footnotes) != 3 {
		t.Fatalf("expected 3 INFERRED footnotes, got %d", len(v.Footnotes))
	}

	// Single-column view with same-column edge must enable right-edge corridor.
	singleCol := &manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Components:    m.Components[:2],
		Views: []manifest.View{{
			ID: "single", Title: "Single", Archetype: manifest.ArchetypeLandscapeMatrix, AccentMode: manifest.AccentRuntime,
			Placements: []manifest.Placement{{ComponentID: "a1", Column: 0}, {ComponentID: "a2", Column: 0}},
			Edges:      []manifest.Edge{{From: "a1", To: "a2", Verb: "call", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceInferred, InferenceRationale: "r"}},
		}},
	}
	scDoc := PrepareDocument(singleCol, reg)
	if !scDoc.Views[0].HasRightEdgeCol {
		t.Fatalf("expected HasRightEdgeCol on single-column lateral edge")
	}
}

func isOutBusEdge(e RoutingEdge, from string) bool {
	return e.From == from && (e.To == "c1" || e.To == "c2") && e.BusGroup != "" && e.BusKind == "out"
}

func isLateralEdge(e RoutingEdge, from, to, side string) bool {
	return e.From == from && e.To == to && e.LateralSide == side
}

func assertBarycenterRoutingEdges(t *testing.T, edges []RoutingEdge) {
	t.Helper()
	var foundBus, foundLeftLat, foundRightLat bool
	for _, e := range edges {
		if isOutBusEdge(e, "b1") {
			foundBus = true
		}
		if isLateralEdge(e, "c1", "c2", "left") {
			foundLeftLat = true
		}
		if isLateralEdge(e, "a1", "a2", "right") {
			foundRightLat = true
		}
	}
	if !foundBus || !foundLeftLat || !foundRightLat {
		t.Fatalf("missing expected routing attributes: bus=%v leftLat=%v rightLat=%v", foundBus, foundLeftLat, foundRightLat)
	}
}

type mockBrowserRunner struct {
	out *BrowserOutput
	err error
}

func (m *mockBrowserRunner) Render(ctx context.Context, req BrowserRenderRequest) (*BrowserOutput, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.out, nil
}

func TestRenderOrchestrationWithMockRunnerAndFallbacks(t *testing.T) {
	reg := loadTestIcons(t)
	m := loadAcmeManifest(t)

	t.Run("WritesAllArtifactsAtomically", func(t *testing.T) {
		testRenderWritesAllArtifactsAtomically(t, m, reg)
	})
	t.Run("LayoutCollisionErrorReturnsWithoutWritingFiles", func(t *testing.T) {
		testRenderLayoutCollisionError(t, m, reg)
	})
	t.Run("ChromeFreeHTMLFallbackAndPDFMissingChromeError", func(t *testing.T) {
		testRenderChromeFreeFallbackAndErrors(t, m, reg)
	})
	t.Run("FindChromeDiscovery", testFindChromeDiscovery)
}

func testRenderWritesAllArtifactsAtomically(t *testing.T, m *manifest.Manifest, reg *icons.Registry) {
	t.Helper()
	outDir := t.TempDir()
	runner := &mockBrowserRunner{
		out: &BrowserOutput{
			Result: BrowserRenderResult{OK: true, HTML: `<!DOCTYPE html><html data-gcp-arch-routed="1"></html>`},
			PNGs: map[string][]byte{
				"00-view_0_system_index.png":    []byte("fake-png-0"),
				"01-cicd_delivery_pipeline.png": []byte("fake-png-1"),
			},
			PDF: []byte("%PDF-1.4 mock"),
		},
	}
	summary, err := Render(context.Background(), m, reg, RenderOptions{
		OutputDir: outDir,
		Runner:    runner,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !summary.UsedChrome || len(summary.PNGPaths) != 2 || summary.PDFPath == "" || summary.HTMLPath == "" {
		t.Fatalf("unexpected RenderSummary: %+v", summary)
	}
	for _, p := range append([]string{summary.HTMLPath, summary.PDFPath}, summary.PNGPaths...) {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected artifact %s to exist: %v", p, err)
		}
	}
}

func testRenderLayoutCollisionError(t *testing.T, m *manifest.Manifest, reg *icons.Registry) {
	t.Helper()
	outDir := t.TempDir()
	runner := &mockBrowserRunner{
		out: &BrowserOutput{
			Result: BrowserRenderResult{
				OK: false,
				Collisions: []Collision{{
					ViewID:   "gke_developer_platform",
					Kind:     "card_segment_collision",
					Subjects: []string{"atlantis->acme_tfstate", "sonarqube"},
					Detail:   "segment intersects card",
				}},
			},
		},
	}
	_, err := Render(context.Background(), m, reg, RenderOptions{
		OutputDir: outDir,
		Runner:    runner,
	})
	var colErr *LayoutCollisionError
	if !errors.As(err, &colErr) {
		t.Fatalf("expected *LayoutCollisionError, got %v", err)
	}
	if !strings.Contains(colErr.Error(), "card_segment_collision") {
		t.Fatalf("unexpected LayoutCollisionError message: %v", colErr)
	}
	entries, _ := os.ReadDir(outDir)
	if len(entries) != 0 {
		t.Fatalf("expected zero output files on collision, found %d", len(entries))
	}
}

func testRenderChromeFreeFallbackAndErrors(t *testing.T, m *manifest.Manifest, reg *icons.Registry) {
	t.Helper()
	outDir := t.TempDir()
	var warn bytes.Buffer
	summary, err := Render(context.Background(), m, reg, RenderOptions{
		Formats:       []Format{FormatHTML},
		OutputDir:     outDir,
		ChromePath:    filepath.Join(outDir, "nonexistent-chrome"),
		WarningWriter: &warn,
	})
	if err != nil {
		t.Fatalf("expected Chrome-free HTML fallback to succeed, got: %v", err)
	}
	if summary.UsedChrome || summary.HTMLPath == "" || !strings.Contains(warn.String(), "warning:") {
		t.Fatalf("unexpected Chrome-free HTML fallback result: summary=%+v warn=%q", summary, warn.String())
	}

	// Requesting PDF with missing explicit Chrome must fail cleanly.
	if _, err := Render(context.Background(), m, reg, RenderOptions{
		Formats:    []Format{FormatPDF},
		OutputDir:  outDir,
		ChromePath: filepath.Join(outDir, "nonexistent-chrome"),
	}); err == nil {
		t.Fatalf("expected error requesting pdf without Chrome")
	}

	// Unsupported format error.
	if _, err := Render(context.Background(), m, reg, RenderOptions{
		Formats:   []Format{"svg"},
		OutputDir: outDir,
	}); err == nil {
		t.Fatalf("expected error on unsupported format svg")
	}
}

func testFindChromeDiscovery(t *testing.T) {
	tmpBin := filepath.Join(t.TempDir(), "fake-chrome")
	if err := os.WriteFile(tmpBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake chrome: %v", err)
	}
	got, err := FindChrome(tmpBin)
	if err != nil || got != tmpBin {
		t.Fatalf("FindChrome explicit = (%q, %v), want %q", got, err, tmpBin)
	}
	t.Setenv("CHROME_PATH", tmpBin)
	got, err = FindChrome("")
	if err != nil || got != tmpBin {
		t.Fatalf("FindChrome CHROME_PATH = (%q, %v), want %q", got, err, tmpBin)
	}
	if _, err := FindChrome(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatalf("expected error for missing explicit chrome path")
	}
}

func requireChromeForTest(t *testing.T) string {
	t.Helper()
	chromePath, err := FindChrome("")
	if err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatalf("Chrome/Chromium required in CI=true environment: %v", err)
		}
		t.Skipf("skipping real Chrome test: %v", err)
	}
	return chromePath
}

func countPDFPages(pdf []byte) int {
	re := regexp.MustCompile(`/Type\s*/Page\b`)
	return len(re.FindAll(pdf, -1))
}

func nextPDFStream(pdf []byte, idx int) ([]byte, int, bool) {
	pos := bytes.Index(pdf[idx:], []byte("stream"))
	if pos < 0 {
		return nil, 0, false
	}
	start := idx + pos + len("stream")
	if start < len(pdf) && pdf[start] == '\r' {
		start++
	}
	if start < len(pdf) && pdf[start] == '\n' {
		start++
	}
	end := bytes.Index(pdf[start:], []byte("endstream"))
	if end < 0 {
		return nil, 0, false
	}
	return pdf[start : start+end], start + end + len("endstream"), true
}

func hasPDFTextOps(data []byte, requireET bool) bool {
	if !bytes.Contains(data, []byte("BT")) {
		return false
	}
	if requireET && !bytes.Contains(data, []byte("ET")) {
		return false
	}
	return bytes.Contains(data, []byte("Tj")) || bytes.Contains(data, []byte("TJ"))
}

func streamHasTextOperators(streamBody []byte) bool {
	if zr, err := zlib.NewReader(bytes.NewReader(streamBody)); err == nil {
		decompressed, _ := io.ReadAll(zr)
		_ = zr.Close()
		return hasPDFTextOps(decompressed, false)
	}
	return hasPDFTextOps(streamBody, true)
}

func pdfHasNonEmptyTextStream(pdf []byte) bool {
	// Headless Chrome compresses page content streams with FlateDecode (zlib).
	for idx := 0; ; {
		streamBody, nextIdx, ok := nextPDFStream(pdf, idx)
		if !ok {
			return false
		}
		if streamHasTextOperators(streamBody) {
			return true
		}
		idx = nextIdx
	}
}

func TestHeadlessChromeAcmeCICDGoldenSuite(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)
	m := loadAcmeManifest(t)
	outDir := t.TempDir()

	summary, err := Render(context.Background(), m, reg, RenderOptions{
		Formats:    []Format{FormatPDF, FormatPNG, FormatHTML},
		OutputDir:  outDir,
		ChromePath: chromePath,
	})
	if err != nil {
		t.Fatalf("real Chrome Render on acme-cicd failed: %v", err)
	}

	// 1. HTML output check: data-gcp-arch-routed="1" & injected connector SVG layers.
	htmlBytes, err := os.ReadFile(summary.HTMLPath)
	if err != nil {
		t.Fatalf("read architecture.html: %v", err)
	}
	htmlStr := string(htmlBytes)
	if !strings.Contains(htmlStr, `data-gcp-arch-routed="1"`) {
		t.Fatalf("architecture.html missing data-gcp-arch-routed=\"1\"")
	}
	if count := strings.Count(htmlStr, `class="connector-layer"`); count != len(m.Views) {
		t.Fatalf("expected %d injected connector-layer SVGs, got %d", len(m.Views), count)
	}

	// Re-running already-routed HTML in Chrome must hit the data-gcp-arch-routed="1" guard cleanly.
	runner := &ChromeDPRunner{ChromePath: chromePath}
	reOut, err := runner.Render(context.Background(), BrowserRenderRequest{HTML: htmlBytes})
	if err != nil || !reOut.Result.OK || len(reOut.Result.ViewRects) != len(m.Views) {
		t.Fatalf("pre-routed HTML guard failed: out=%+v err=%v", reOut, err)
	}

	// 2. PNG check: 4 views, 00..03 indexed, 3x width (1040 * 3 = 3120px).
	if len(summary.PNGPaths) != 4 {
		t.Fatalf("expected 4 view PNGs, got %d (%v)", len(summary.PNGPaths), summary.PNGPaths)
	}
	for _, p := range summary.PNGPaths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read png %s: %v", p, err)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("decode png %s: %v", p, err)
		}
		if cfg.Width != 3120 {
			t.Fatalf("PNG %s width = %d, want 3120 (3x of 1040px)", filepath.Base(p), cfg.Width)
		}
		if cfg.Height <= 0 || cfg.Height > 718*3 {
			t.Fatalf("PNG %s height = %d px (%.1f CSS px), must fit <= 718px (190mm single A4 page)", filepath.Base(p), cfg.Height, float64(cfg.Height)/3.0)
		}
	}

	// 3. PDF check: valid %PDF- header, exact page count == len(views), non-empty text streams.
	pdfBytes, err := os.ReadFile(summary.PDFPath)
	if err != nil {
		t.Fatalf("read architecture.pdf: %v", err)
	}
	if !bytes.HasPrefix(pdfBytes, []byte("%PDF-")) {
		t.Fatalf("architecture.pdf missing %%PDF- header")
	}
	wantPages := len(m.Views)
	gotPages := countPDFPages(pdfBytes)
	if gotPages != wantPages {
		t.Fatalf("PDF /Type /Page count = %d, want %d", gotPages, wantPages)
	}
	if !pdfHasNonEmptyTextStream(pdfBytes) {
		t.Fatalf("expected non-empty vector text streams in architecture.pdf")
	}
}

func buildBipartiteManifest(leftN, rightN int) *manifest.Manifest {
	m := &manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Metadata:      manifest.Metadata{GeneratedAt: "2026-11-23T02:00:00Z", Scopes: []string{"projects/k-test"}},
	}
	v := manifest.View{
		ID:         fmt.Sprintf("k_%d_%d", leftN, rightN),
		Title:      fmt.Sprintf("Bipartite K_%d,%d", leftN, rightN),
		Archetype:  manifest.ArchetypeLandscapeMatrix,
		AccentMode: manifest.AccentRuntime,
	}
	for i := 0; i < leftN; i++ {
		id := fmt.Sprintf("l%d", i)
		m.Components = append(m.Components, manifest.Component{
			ID: id, Title: fmt.Sprintf("App %d", i+1), IconKey: "cloud_run", ComponentKind: manifest.KindWorkload, ProjectID: "k-test",
			BackedBy: []manifest.BackedResource{{URI: "//run/" + id, AssetType: "run.googleapis.com/Service", ProjectID: "k-test"}},
		})
		v.Placements = append(v.Placements, manifest.Placement{ComponentID: id, Column: 0})
	}
	for j := 0; j < rightN; j++ {
		id := fmt.Sprintf("r%d", j)
		m.Components = append(m.Components, manifest.Component{
			ID: id, Title: fmt.Sprintf("Store %d", j+1), IconKey: "cloud_sql", ComponentKind: manifest.KindDatastore, ProjectID: "k-test",
			BackedBy: []manifest.BackedResource{{URI: "//sql/" + id, AssetType: "sqladmin.googleapis.com/Instance", ProjectID: "k-test"}},
		})
		v.Placements = append(v.Placements, manifest.Placement{ComponentID: id, Column: 1})
	}
	for i := 0; i < leftN; i++ {
		for j := 0; j < rightN; j++ {
			v.Edges = append(v.Edges, manifest.Edge{
				From:               fmt.Sprintf("l%d", i),
				To:                 fmt.Sprintf("r%d", j),
				Verb:               fmt.Sprintf("q%d-%d", i+1, j+1),
				FlowKind:           manifest.FlowRuntime,
				Provenance:         manifest.ProvenanceInferred,
				InferenceRationale: "bipartite flow",
			})
		}
	}
	m.Views = []manifest.View{v}
	return m
}

func TestHeadlessChromeBipartiteCrossingsAndHaloBridges(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)

	for _, tc := range []struct {
		name string
		n    int
	}{
		{name: "K2,2", n: 2},
		{name: "K3,3", n: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := buildBipartiteManifest(tc.n, tc.n)
			outDir := t.TempDir()
			summary, err := Render(context.Background(), m, reg, RenderOptions{
				Formats:    []Format{FormatHTML},
				OutputDir:  outDir,
				ChromePath: chromePath,
			})
			if err != nil {
				t.Fatalf("%s failed with collision/error: %v", tc.name, err)
			}
			htmlBytes, err := os.ReadFile(summary.HTMLPath)
			if err != nil {
				t.Fatalf("read html: %v", err)
			}
			htmlStr := string(htmlBytes)
			if !strings.Contains(htmlStr, `data-bridge="1"`) || !strings.Contains(htmlStr, `data-bridge-halo="1"`) {
				t.Fatalf("%s expected 4px halo bridge paths on irreducible perpendicular crossings", tc.name)
			}
			// Verify SVG paint order: all <line> elements precede the first data-bridge-halo path.
			lastLine := strings.LastIndex(htmlStr, "<line ")
			firstHalo := strings.Index(htmlStr, `data-bridge-halo="1"`)
			if lastLine > firstHalo {
				t.Fatalf("%s expected all non-bridged <line> segments painted before data-bridge-halo (lastLine=%d, firstHalo=%d)", tc.name, lastLine, firstHalo)
			}
		})
	}
}

func TestHeadlessChromeHubSpokeBidirectionalViaBusAndMinRow1NestedContainers(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)

	t.Run("HubSpokeBidirectionalViaBusID", func(t *testing.T) {
		m := &manifest.Manifest{
			SchemaVersion: manifest.SchemaVersion,
			Metadata:      manifest.Metadata{GeneratedAt: "2026-11-23T02:00:00Z", Scopes: []string{"projects/hub"}},
			Components: []manifest.Component{
				{ID: "hub", Title: "Shared VPC Hub", IconKey: "virtual_private_cloud", ComponentKind: manifest.KindCohort, ProjectID: "hub", BackedBy: []manifest.BackedResource{{URI: "//u/hub", AssetType: "t", ProjectID: "hub"}}},
				{ID: "left_a", Title: "Left Spoke A", IconKey: "cloud_run", ComponentKind: manifest.KindWorkload, ProjectID: "hub", BackedBy: []manifest.BackedResource{{URI: "//u/la", AssetType: "t", ProjectID: "hub"}}},
				{ID: "left_b", Title: "Left Spoke B", IconKey: "cloud_run", ComponentKind: manifest.KindWorkload, ProjectID: "hub", BackedBy: []manifest.BackedResource{{URI: "//u/lb", AssetType: "t", ProjectID: "hub"}}},
				{ID: "right_a", Title: "Right PSA A", IconKey: "cloud_sql", ComponentKind: manifest.KindDatastore, ProjectID: "hub", BackedBy: []manifest.BackedResource{{URI: "//u/ra", AssetType: "t", ProjectID: "hub"}}},
				{ID: "right_b", Title: "Right PSA B", IconKey: "cloud_sql", ComponentKind: manifest.KindDatastore, ProjectID: "hub", BackedBy: []manifest.BackedResource{{URI: "//u/rb", AssetType: "t", ProjectID: "hub"}}},
			},
			Views: []manifest.View{{
				ID:         "hub_bi",
				Title:      "Hub Bidirectional Bus",
				Archetype:  manifest.ArchetypeHubSpoke,
				AccentMode: manifest.AccentRuntime,
				Placements: []manifest.Placement{
					{ComponentID: "left_a", Column: 0},
					{ComponentID: "left_b", Column: 0},
					{ComponentID: "hub", Column: 1},
					{ComponentID: "right_a", Column: 2},
					{ComponentID: "right_b", Column: 2},
				},
				Edges: []manifest.Edge{
					{From: "hub", To: "left_a", Verb: "peer left A", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceInferred, InferenceRationale: "hub", ViaBusID: "hub_fanout"},
					{From: "hub", To: "left_b", Verb: "peer left B", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceInferred, InferenceRationale: "hub", ViaBusID: "hub_fanout"},
					{From: "hub", To: "right_a", Verb: "peer right A", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceInferred, InferenceRationale: "hub", ViaBusID: "hub_fanout"},
					{From: "hub", To: "right_b", Verb: "peer right B", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceInferred, InferenceRationale: "hub", ViaBusID: "hub_fanout"},
				},
			}},
		}

		doc := PrepareDocument(m, reg)
		seenGroups := map[string]int{}
		for _, e := range doc.Views[0].Edges {
			seenGroups[e.BusGroup]++
		}
		if seenGroups["explicit_hub_fanout_-1"] != 2 || seenGroups["explicit_hub_fanout_1"] != 2 {
			t.Fatalf("expected directional explicit bus split (-1 and 1), got %+v", seenGroups)
		}

		outDir := t.TempDir()
		if _, err := Render(context.Background(), m, reg, RenderOptions{
			Formats:    []Format{FormatHTML},
			OutputDir:  outDir,
			ChromePath: chromePath,
		}); err != nil {
			t.Fatalf("hub_spoke bidirectional via_bus_id failed in real Chrome: %v", err)
		}
	})

	t.Run("NestedDepth2ContainerStartingOnRow1", func(t *testing.T) {
		m := &manifest.Manifest{
			SchemaVersion: manifest.SchemaVersion,
			Metadata:      manifest.Metadata{GeneratedAt: "2026-11-23T02:00:00Z", Scopes: []string{"projects/row1"}},
			Components: []manifest.Component{
				{ID: "top_card", Title: "Uncontained Top Gateway", IconKey: "cloud_load_balancing", ComponentKind: manifest.KindIngress, ProjectID: "row1", BackedBy: []manifest.BackedResource{{URI: "//u/top", AssetType: "t", ProjectID: "row1"}}},
				{ID: "top_right", Title: "Uncontained Monitoring", IconKey: "cloud_monitoring", ComponentKind: manifest.KindWorkload, ProjectID: "row1", BackedBy: []manifest.BackedResource{{URI: "//u/topr", AssetType: "t", ProjectID: "row1"}}},
				{ID: "inner_app", Title: "Nested GKE Workload", IconKey: "k8s_workload", ComponentKind: manifest.KindWorkload, ProjectID: "row1", BackedBy: []manifest.BackedResource{{URI: "//u/app", AssetType: "t", ProjectID: "row1"}}},
				{ID: "outer_db", Title: "Shared Project Database", IconKey: "cloud_sql", ComponentKind: manifest.KindDatastore, ProjectID: "row1", BackedBy: []manifest.BackedResource{{URI: "//u/db", AssetType: "t", ProjectID: "row1"}}},
			},
			Containers: []manifest.Container{
				{ID: "proj_outer", Title: "Project With A Very Long Container Title That Must Stay Strictly Inside MinCol", Kind: manifest.ContainerProject},
				{ID: "gke_inner", Title: "GKE Cluster: nested-row1", Kind: manifest.ContainerGKECluster, ParentContainerID: "proj_outer"},
			},
			Views: []manifest.View{{
				ID:           "nested_row1",
				Title:        "Nested Containers Starting on Row 1",
				Archetype:    manifest.ArchetypeLandscapeMatrix,
				AccentMode:   manifest.AccentRuntime,
				ContainerIDs: []string{"proj_outer", "gke_inner"},
				Placements: []manifest.Placement{
					{ComponentID: "top_card", Column: 0},
					{ComponentID: "top_right", Column: 1},
					{ComponentID: "inner_app", ContainerID: "gke_inner", Column: 0},
					{ComponentID: "outer_db", ContainerID: "proj_outer", Column: 1},
				},
				Edges: []manifest.Edge{
					{From: "top_card", To: "top_right", Verb: "telemetry", FlowKind: manifest.FlowAdmin, Provenance: manifest.ProvenanceInferred, InferenceRationale: "telemetry"},
					{From: "inner_app", To: "outer_db", Verb: "SQL query", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceInferred, InferenceRationale: "sql"},
				},
			}},
		}

		doc := PrepareDocument(m, reg)
		if !strings.Contains(doc.Views[0].RowTracksCSS, "var(--corridor-row-1, 52px)") {
			t.Fatalf("expected row 1 corridor to reserve 52px for depth-1+depth-2 badges on minRow==1, got %q", doc.Views[0].RowTracksCSS)
		}
		if len(doc.Views[0].Containers) != 2 || doc.Views[0].Containers[0].BadgeGridCol != 1 || doc.Views[0].Containers[0].BadgeGridRow != 4 {
			t.Fatalf("expected badges anchored at grid cell (col=1, row=4), got %+v", doc.Views[0].Containers)
		}

		outDir := t.TempDir()
		if _, err := Render(context.Background(), m, reg, RenderOptions{
			Formats:    []Format{FormatHTML},
			OutputDir:  outDir,
			ChromePath: chromePath,
		}); err != nil {
			t.Fatalf("nested minRow==1 containers failed in real Chrome: %v", err)
		}
	})
}

func TestHeadlessChromeCorridorWidening8ParallelLanes(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)

	// 4 left cards x 2 distinct flow kinds -> 4 right cards inverted, yielding 8 distinct vertical lanes
	// in Corridor 0 (no auto-bus merging because each (from, flow_kind) has degree 1!).
	m := &manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Metadata:      manifest.Metadata{GeneratedAt: "2026-11-23T02:00:00Z", Scopes: []string{"projects/lanes"}},
	}
	v := manifest.View{
		ID:         "lanes_8",
		Title:      "8 Parallel Lanes Widening",
		Archetype:  manifest.ArchetypeLandscapeMatrix,
		AccentMode: manifest.AccentMixed,
	}
	for i := 0; i < 4; i++ {
		lid := fmt.Sprintf("src_%d", i)
		rid := fmt.Sprintf("dst_%d", i)
		m.Components = append(m.Components,
			manifest.Component{ID: lid, Title: fmt.Sprintf("Source %d", i+1), IconKey: "cloud_run", ComponentKind: manifest.KindWorkload, ProjectID: "lanes", BackedBy: []manifest.BackedResource{{URI: "//u/" + lid, AssetType: "t", ProjectID: "lanes"}}},
			manifest.Component{ID: rid, Title: fmt.Sprintf("Target %d", i+1), IconKey: "cloud_storage", ComponentKind: manifest.KindDatastore, ProjectID: "lanes", BackedBy: []manifest.BackedResource{{URI: "//u/" + rid, AssetType: "t", ProjectID: "lanes"}}},
		)
		v.Placements = append(v.Placements, manifest.Placement{ComponentID: lid, Column: 0})
		v.Placements = append(v.Placements, manifest.Placement{ComponentID: rid, Column: 1})
	}
	// 8 distinct non-bus edges (4 RUNTIME + 4 CICD) crossing rows so all 8 occupy vertical lanes in Corridor 0.
	for i := 0; i < 4; i++ {
		v.Edges = append(v.Edges,
			manifest.Edge{
				From: fmt.Sprintf("src_%d", i), To: fmt.Sprintf("dst_%d", 3-i),
				Verb: fmt.Sprintf("rt-%d", i+1), FlowKind: manifest.FlowRuntime,
				Provenance: manifest.ProvenanceInferred, InferenceRationale: "lane",
			},
			manifest.Edge{
				From: fmt.Sprintf("src_%d", i), To: fmt.Sprintf("dst_%d", (i+1)%4),
				Verb: fmt.Sprintf("ci-%d", i+1), FlowKind: manifest.FlowCICD,
				Provenance: manifest.ProvenanceInferred, InferenceRationale: "lane",
			},
		)
	}
	m.Views = []manifest.View{v}

	outDir := t.TempDir()
	summary, err := Render(context.Background(), m, reg, RenderOptions{
		Formats:    []Format{FormatHTML},
		OutputDir:  outDir,
		ChromePath: chromePath,
	})
	if err != nil {
		t.Fatalf("8-parallel-lane render failed: %v", err)
	}
	htmlBytes, err := os.ReadFile(summary.HTMLPath)
	if err != nil {
		t.Fatalf("read html: %v", err)
	}
	if !strings.Contains(string(htmlBytes), "--corridor-col-0:") {
		t.Fatalf("expected corridor widening to set --corridor-col-0 CSS custom property")
	}
}

func TestHeadlessChromeForcedCollisionsReturnLayoutCollisionError(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)
	runner := &ChromeDPRunner{ChromePath: chromePath}

	// 1. Forced card collision: inject an obstacle .arch-card positioned directly in the middle of Corridor 0.
	m := buildBipartiteManifest(1, 1)
	rawHTML, err := BuildHTML(m, reg)
	if err != nil {
		t.Fatalf("BuildHTML: %v", err)
	}
	injectedCard := `<article class="arch-card" data-component-id="blocker" style="position:absolute;left:420px;top:20px;width:200px;height:140px;z-index:9;">Blocker</article>`
	forcedCardHTML := bytes.Replace(rawHTML, []byte(`<div class="corridor-col"`), []byte(injectedCard+`<div class="corridor-col"`), 1)

	_, err = runner.Render(context.Background(), BrowserRenderRequest{HTML: forcedCardHTML})
	var colErr *LayoutCollisionError
	if !errors.As(err, &colErr) {
		t.Fatalf("expected *LayoutCollisionError on forced card overlap, got %v", err)
	}
	if !strings.Contains(colErr.Error(), "card") {
		t.Fatalf("expected card collision kind, got: %v", colErr)
	}

	// 2. Forced parallel collinear collision: 26 distinct vertical lanes packed into a 160px-capped corridor
	// forces lane separation < 6px even after max reflow widening.
	dense := &manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Metadata:      manifest.Metadata{GeneratedAt: "2026-11-23T02:00:00Z", Scopes: []string{"projects/dense"}},
	}
	dv := manifest.View{ID: "dense", Title: "Dense Collinear", Archetype: manifest.ArchetypeLandscapeMatrix, AccentMode: manifest.AccentMixed}
	for i := 0; i < 6; i++ {
		lid := fmt.Sprintf("a%d", i)
		rid := fmt.Sprintf("b%d", i)
		dense.Components = append(dense.Components,
			manifest.Component{ID: lid, Title: lid, IconKey: "cloud_run", ComponentKind: manifest.KindWorkload, ProjectID: "dense", BackedBy: []manifest.BackedResource{{URI: "//u/" + lid, AssetType: "t", ProjectID: "dense"}}},
			manifest.Component{ID: rid, Title: rid, IconKey: "cloud_sql", ComponentKind: manifest.KindDatastore, ProjectID: "dense", BackedBy: []manifest.BackedResource{{URI: "//u/" + rid, AssetType: "t", ProjectID: "dense"}}},
		)
		dv.Placements = append(dv.Placements, manifest.Placement{ComponentID: lid, Column: 0}, manifest.Placement{ComponentID: rid, Column: 1})
	}
	dense.Views = []manifest.View{dv}
	// Inject 26 standalone routing edges directly into the JSON payload (bypassing auto-bus grouping) to saturate 160px.
	rawDenseHTML, err := BuildHTML(dense, reg)
	if err != nil {
		t.Fatalf("BuildHTML dense: %v", err)
	}
	var fakeEdges []string
	for k := 0; k < 26; k++ {
		fromIdx := k % 6
		toIdx := (5 - (k % 6) + (k / 6)) % 6
		if fromIdx == toIdx {
			toIdx = (toIdx + 1) % 6
		}
		fakeEdges = append(fakeEdges, fmt.Sprintf(`{"from":"a%d","to":"b%d","verb":"e%d","flow_kind":"RUNTIME","provenance":"INFERRED","waypoint_row":0,"lateral_corridor":0}`, fromIdx, toIdx, k))
	}
	densePayload := fmt.Sprintf(`[{"id":"dense","edges":[%s]}]`, strings.Join(fakeEdges, ","))
	reData := regexp.MustCompile(`<script id="gcp-arch-routing-data" type="application/json">.*?</script>`)
	forcedCollinearHTML := reData.ReplaceAll(rawDenseHTML, []byte(`<script id="gcp-arch-routing-data" type="application/json">`+densePayload+`</script>`))

	_, err = runner.Render(context.Background(), BrowserRenderRequest{HTML: forcedCollinearHTML})
	if !errors.As(err, &colErr) {
		t.Fatalf("expected *LayoutCollisionError on forced 26-lane collinear saturation, got %v", err)
	}
	if !strings.Contains(colErr.Error(), "collinear_collision") {
		t.Fatalf("expected collinear_collision error, got: %v", colErr)
	}
}
