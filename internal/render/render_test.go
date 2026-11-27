package render

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
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

func encodeTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := buildTestDiagramImage(w, h)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

func buildTestDiagramImage(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	bg := color.NRGBA{R: 248, G: 250, B: 252, A: 255}
	fillTestRect(img, 0, 0, w, h, bg)

	palette := []color.NRGBA{
		{R: 15, G: 23, B: 42, A: 255},
		{R: 37, G: 99, B: 235, A: 255},
		{R: 13, G: 148, B: 136, A: 255},
		{R: 99, G: 102, B: 241, A: 255},
		{R: 217, G: 119, B: 6, A: 255},
		{R: 220, G: 38, B: 38, A: 255},
		{R: 5, G: 150, B: 105, A: 255},
		{R: 124, G: 58, B: 237, A: 255},
		{R: 2, G: 132, B: 199, A: 255},
		{R: 30, G: 41, B: 59, A: 255},
	}
	marginX := max(16, w/20)
	marginY := max(16, h/20)
	boxW := max(12, (w-2*marginX)/(len(palette)*2))
	boxH := max(24, h/5)
	for i, c := range palette {
		x0 := marginX + i*(boxW*2)
		fillTestRect(img, x0, marginY, min(w-marginX, x0+boxW), min(h-marginY, marginY+boxH), c)
	}
	return img
}

func fillTestRect(img *image.NRGBA, x0, y0, x1, y1 int, c color.NRGBA) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
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
	if strings.Contains(html, `<img src=x onerror=alert(1)>`) {
		t.Fatalf("unescaped XSS payload detected in HTML output")
	}
	if strings.Contains(html, `<b>sql</b>`) {
		t.Fatalf("unescaped HTML tag in routing JSON")
	}
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

func TestAdaptiveSkiaScaleGuard(t *testing.T) {
	if s := computeAdaptiveScale(1800, 1100); s != maxRasterScale {
		t.Fatalf("small view scale = %.2f, want %.2f", s, maxRasterScale)
	}
	if s := computeAdaptiveScale(3600, 1800); s != maxRasterScale {
		t.Fatalf("3600x1800 DIP scale = %.2f, want %.2f (7200<=8192 and 25.92M<=28M)", s, maxRasterScale)
	}
	sBig := computeAdaptiveScale(6000, 1000)
	if sBig >= maxRasterScale || sBig*6000 > maxRasterDim || sBig < 1.35 {
		t.Fatalf("6000x1000 DIP adaptive scale = %.2f (raster=%.0f), expected ~1.36 <= %.0f", sBig, sBig*6000, maxRasterDim)
	}
	sLarge := computeAdaptiveScale(4521, 3496)
	pixelsLarge := (4521.0 * sLarge) * (3496.0 * sLarge)
	if sLarge >= maxRasterScale || sLarge < 1.0 || pixelsLarge > maxRasterPixels {
		t.Fatalf("4521x3496 DIP adaptive scale = %.2f (pixels=%.0f), expected in [1.0, %.2f) and <= %.0f", sLarge, pixelsLarge, maxRasterScale, maxRasterPixels)
	}
	if sHuge := computeAdaptiveScale(18000, 4000); sHuge != 1.0 {
		t.Fatalf("extreme DIP scale = %.2f, want clamped 1.0", sHuge)
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

	t.Run("WritesAllArtifactsAtomicallyAndCleansStalePNGs", func(t *testing.T) {
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
	staleDir := filepath.Join(outDir, "views")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	staleFile := filepath.Join(staleDir, "99-stale-view.png")
	if err := os.WriteFile(staleFile, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	cons := manifest.ConservationReport{
		Status:                   "PASS",
		FactsPrimaryCount:        10,
		RepresentedPrimaryCount:  10,
		FactsFunctionalEdgeCount: 8,
		View0Coverage:            1.0,
	}
	runner := &mockBrowserRunner{
		out: &BrowserOutput{
			Result: BrowserRenderResult{OK: true, HTML: `<!DOCTYPE html><html data-gcp-arch-routed="1"></html>`},
			PNGs: map[string][]byte{
				"00-view_0_system_index.png":    encodeTestPNG(t, 900, 600),
				"01-cicd_delivery_pipeline.png": encodeTestPNG(t, 900, 600),
			},
			PDF: []byte("%PDF-1.4 mock"),
		},
	}
	summary, err := Render(context.Background(), m, reg, RenderOptions{
		OutputDir:           outDir,
		Runner:              runner,
		Conservation:        &cons,
		WriteGeometryReport: true,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !summary.UsedChrome || len(summary.PNGPaths) != 2 || summary.PDFPath == "" || summary.HTMLPath == "" || summary.GeometryPath == "" {
		t.Fatalf("unexpected RenderSummary: %+v", summary)
	}
	if _, err := os.Stat(staleFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected stale PNG %s to be cleaned, stat err=%v", staleFile, err)
	}
	for _, p := range append([]string{summary.HTMLPath, summary.PDFPath, summary.GeometryPath}, summary.PNGPaths...) {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected artifact %s to exist: %v", p, err)
		}
	}

	rawGeo, err := os.ReadFile(summary.GeometryPath)
	if err != nil {
		t.Fatal(err)
	}
	var geo GeometryReport
	if err := json.Unmarshal(rawGeo, &geo); err != nil {
		t.Fatalf("decode geometry_report.json: %v", err)
	}
	if !geo.AllPass() || geo.Conservation == nil || geo.Conservation.FactsPrimaryCount != 10 {
		t.Fatalf("unexpected geometry report: %+v", geo)
	}

	// Default WriteGeometryReport=false computes GeometryReport in memory without writing geometry_report.json.
	noReportDir := t.TempDir()
	noRepSummary, err := Render(context.Background(), m, reg, RenderOptions{
		Formats:   []Format{FormatPNG},
		OutputDir: noReportDir,
		Runner:    runner,
	})
	if err != nil {
		t.Fatalf("Render without WriteGeometryReport: %v", err)
	}
	if noRepSummary.GeometryPath != "" || noRepSummary.GeometryReport == nil || !noRepSummary.GeometryReport.AllPass() {
		t.Fatalf("unexpected summary without WriteGeometryReport: %+v", noRepSummary)
	}
	if _, err := os.Stat(filepath.Join(noReportDir, "geometry_report.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no geometry_report.json when WriteGeometryReport is false, stat err=%v", err)
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
					Kind:     "card_collision",
					Subjects: []string{"atlantis", "sonarqube"},
					Detail:   "card boxes overlap",
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
	if !strings.Contains(colErr.Error(), "card_collision") || !strings.Contains(colErr.Error(), "move one of the listed components to another column") {
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
	missingChrome := filepath.Join(outDir, "nonexistent-chrome")

	explicitHTMLDir := filepath.Join(outDir, "explicit-html")
	var warn bytes.Buffer
	if _, err := Render(context.Background(), m, reg, RenderOptions{
		Formats:       []Format{FormatHTML},
		OutputDir:     explicitHTMLDir,
		ChromePath:    missingChrome,
		WarningWriter: &warn,
	}); err == nil || !strings.Contains(err.Error(), "explicit chrome path") {
		t.Fatalf("expected explicit chrome path error under FormatHTML alone, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(explicitHTMLDir, "architecture.html")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no fallback architecture.html on explicit missing ChromePath, stat err=%v", err)
	}

	if _, err := Render(context.Background(), m, reg, RenderOptions{
		Formats:    []Format{FormatPDF},
		OutputDir:  outDir,
		ChromePath: missingChrome,
	}); err == nil || !strings.Contains(err.Error(), "explicit chrome path") {
		t.Fatalf("expected explicit chrome path error requesting pdf, got: %v", err)
	}

	t.Setenv("PATH", "")
	t.Setenv("CHROME_PATH", "")
	origOSPaths := chromeOSPaths
	chromeOSPaths = nil
	t.Cleanup(func() { chromeOSPaths = origOSPaths })

	fallbackDir := filepath.Join(outDir, "fallback-html")
	warn.Reset()
	summary, err := Render(context.Background(), m, reg, RenderOptions{
		Formats:       []Format{FormatHTML},
		OutputDir:     fallbackDir,
		WarningWriter: &warn,
	})
	if err != nil {
		t.Fatalf("expected Chrome-free HTML fallback to succeed, got: %v", err)
	}
	if summary.UsedChrome || summary.HTMLPath == "" || !strings.Contains(warn.String(), "warning:") {
		t.Fatalf("unexpected Chrome-free HTML fallback result: summary=%+v warn=%q", summary, warn.String())
	}
	htmlBytes, err := os.ReadFile(summary.HTMLPath)
	if err != nil || !bytes.Contains(htmlBytes, []byte("__GCP_ARCH_RENDER_PROMISE__")) || bytes.Contains(htmlBytes, []byte(`data-gcp-arch-routed="1"`)) {
		t.Fatalf("expected unrouted fallback HTML at %s, err=%v", summary.HTMLPath, err)
	}

	if _, err := Render(context.Background(), m, reg, RenderOptions{
		Formats:   []Format{FormatPDF},
		OutputDir: outDir,
	}); err == nil || !strings.Contains(err.Error(), "required for pdf/png output") {
		t.Fatalf("expected missing Chrome error requesting pdf, got: %v", err)
	}

	if _, err := Render(context.Background(), m, reg, RenderOptions{
		Formats:   []Format{"svg"},
		OutputDir: outDir,
	}); err == nil {
		t.Fatalf("expected error on unsupported format svg")
	}
}

func testFindChromeDiscovery(t *testing.T) {
	dir := t.TempDir()
	cliBin := filepath.Join(dir, "cli-chrome")
	envBin := filepath.Join(dir, "env-chrome")
	cfgBin := filepath.Join(dir, "cfg-chrome")
	pathDir := filepath.Join(dir, "bin")
	pathBin := filepath.Join(pathDir, "google-chrome")
	if err := os.MkdirAll(pathDir, 0o755); err != nil {
		t.Fatalf("mkdir pathDir: %v", err)
	}
	for _, p := range []string{cliBin, envBin, cfgBin, pathBin} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write fake chrome %s: %v", p, err)
		}
	}
	t.Setenv("PATH", pathDir)
	missing := filepath.Join(dir, "missing")

	t.Setenv("CHROME_PATH", envBin)
	got, err := FindChrome(cliBin, cfgBin)
	if err != nil || got != cliBin {
		t.Fatalf("FindChrome explicit = (%q, %v), want %q", got, err, cliBin)
	}

	if _, err := FindChrome(missing, cfgBin); err == nil {
		t.Fatalf("expected error for missing explicit chrome path")
	}

	got, err = FindChrome("", cfgBin)
	if err != nil || got != envBin {
		t.Fatalf("FindChrome CHROME_PATH = (%q, %v), want %q", got, err, envBin)
	}

	t.Setenv("CHROME_PATH", missing)
	got, err = FindChrome("", cfgBin)
	if err != nil || got != cfgBin {
		t.Fatalf("FindChrome missing CHROME_PATH + valid config = (%q, %v), want %q", got, err, cfgBin)
	}

	got, err = FindChrome("", missing)
	if err != nil || got != pathBin {
		t.Fatalf("FindChrome PATH fallback = (%q, %v), want %q", got, err, pathBin)
	}

	t.Setenv("PATH", "")
	t.Setenv("CHROME_PATH", "")
	origOSPaths := chromeOSPaths
	chromeOSPaths = nil
	t.Cleanup(func() { chromeOSPaths = origOSPaths })
	if _, err := FindChrome("", missing); err == nil {
		t.Fatalf("expected error when no Chrome binary exists anywhere")
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

	// 2. PNG + geometry_report.json check.
	if len(summary.PNGPaths) != len(m.Views) {
		t.Fatalf("expected %d view PNGs, got %d (%v)", len(m.Views), len(summary.PNGPaths), summary.PNGPaths)
	}
	if summary.GeometryReport == nil || !summary.GeometryReport.AllPass() {
		t.Fatalf("geometry_report did not pass: %+v", summary.GeometryReport)
	}
	for i, p := range summary.PNGPaths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read png %s: %v", p, err)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("decode png %s: %v", p, err)
		}
		vr := summary.GeometryReport.Views[i]
		if cfg.Width != vr.WidthPx || cfg.Height != vr.HeightPx {
			t.Fatalf("PNG %s %dx%d != report %dx%d", filepath.Base(p), cfg.Width, cfg.Height, vr.WidthPx, vr.HeightPx)
		}
		if cfg.Width < PNGMinWidth || cfg.Height < PNGMinHeight || vr.AspectRatio < PNGMinAspectRatio || vr.AspectRatio > PNGMaxAspectRatio {
			t.Fatalf("PNG %s dimensions/AR out of geometry bounds: %dx%d ar=%.3f", filepath.Base(p), cfg.Width, cfg.Height, vr.AspectRatio)
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
	if gotPages := countPDFPages(pdfBytes); gotPages != len(m.Views) {
		t.Fatalf("PDF /Type /Page count = %d, want %d", gotPages, len(m.Views))
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
		name         string
		n            int
		expectBridge bool
	}{
		{name: "K2,2", n: 2, expectBridge: false},
		{name: "K3,3", n: 3, expectBridge: true},
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
			if !strings.Contains(htmlStr, `data-gcp-arch-routed="1"`) {
				t.Fatalf("%s expected data-gcp-arch-routed=\"1\"", tc.name)
			}
			if tc.expectBridge {
				if !strings.Contains(htmlStr, `data-bridge="1"`) || !strings.Contains(htmlStr, `data-bridge-halo="1"`) {
					t.Fatalf("%s expected 4.5px halo bridge paths on perpendicular crossings", tc.name)
				}
				lastLine := strings.LastIndex(htmlStr, "<line ")
				firstHalo := strings.Index(htmlStr, `data-bridge-halo="1"`)
				if lastLine > firstHalo {
					t.Fatalf("%s expected all non-bridged <line> segments painted before data-bridge-halo (lastLine=%d, firstHalo=%d)", tc.name, lastLine, firstHalo)
				}
			}
		})
	}
}

func TestHeadlessChromeHighFanInHybridManifestAndGeometryReport(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)

	m := &manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Metadata:      manifest.Metadata{GeneratedAt: "2026-11-27T00:00:00Z", Scopes: []string{"projects/host-net", "projects/svc-app"}},
		Containers: []manifest.Container{
			{ID: "org", Title: "enterprise.example", Kind: manifest.ContainerOrg, OrderHint: 10},
			{ID: "proj_host", Title: "host-net", Kind: manifest.ContainerProject, ParentContainerID: "org", OrderHint: 30},
			{ID: "vpc", Title: "vpc-shared-hub", Kind: manifest.ContainerVPC, ParentContainerID: "proj_host", OrderHint: 40, Badges: []string{"Shared VPC (1 svc project)"}},
			{ID: "pub_zone", Title: "Public Ingress", Kind: manifest.ContainerExposureZone, Exposure: "PUBLIC", ParentContainerID: "vpc", OrderHint: 45},
			{ID: "priv_sub", Title: "europe-west1 / sub-services", Kind: manifest.ContainerSubnet, Exposure: "PRIVATE", ParentContainerID: "vpc", OrderHint: 65},
			{ID: "psa_zone", Title: "Private Service Access", Kind: manifest.ContainerPSAZone, Exposure: "PSA", ParentContainerID: "vpc", OrderHint: 75},
		},
		TransversalItems: []manifest.TransversalItem{
			{ID: "tr_sa", Category: "IAM_SA", AssetType: "iam.googleapis.com/ServiceAccount", DisplayName: "svc-orders-runtime-sa@svc-app.iam.gserviceaccount.com", ProjectID: "svc-app"},
			{ID: "tr_kms", Category: "KMS", AssetType: "cloudkms.googleapis.com/CryptoKey", DisplayName: "ck-orders-pii-cmek-key-europe-west1", ProjectID: "svc-app"},
			{ID: "tr_sec", Category: "SECRET", AssetType: "secretmanager.googleapis.com/Secret", DisplayName: "db-orders-primary-connection-secret", ProjectID: "svc-app"},
		},
		SuppressedPlumbingCounts: map[string]int{"compute.googleapis.com/Route": 8},
	}

	v := manifest.View{
		ID:                      manifest.SystemIndexViewID,
		Title:                   "System Architecture Overview",
		FocusSummary:            "High fan-in 20-service hybrid topology with long resource names and transversal panel",
		IncludeTransversalPanel: true,
		Archetype:               manifest.ArchetypeLayeredFlow,
		AccentMode:              manifest.AccentMixed,
		ContainerIDs:            []string{"org", "proj_host", "vpc", "pub_zone", "priv_sub", "psa_zone"},
	}

	m.Components = append(m.Components,
		manifest.Component{
			ID: "glb", Title: "edge-global-https-load-balancer-storefront", IconKey: "cloud_load_balancing",
			ComponentKind: manifest.KindIngress, ProjectID: "host-net", TopoRank: 1,
			Badges:   []manifest.ComponentBadge{{Kind: "exposure", Label: "PUBLIC"}, {Kind: "armor", Label: "waf: edge-armor"}},
			BackedBy: []manifest.BackedResource{{URI: "//compute/gfr/glb", AssetType: "compute.googleapis.com/GlobalForwardingRule", ProjectID: "host-net"}},
		},
		manifest.Component{
			ID: "db", Title: "pg-orders-primary-cloudsql-instance-europe-west1", IconKey: "cloud_sql",
			ComponentKind: manifest.KindDatastore, ProjectID: "svc-app", TopoRank: 4,
			Badges:   []manifest.ComponentBadge{{Kind: "project", Label: "svc-app"}, {Kind: "kms", Label: "cmek"}},
			BackedBy: []manifest.BackedResource{{URI: "//sql/db", AssetType: "sqladmin.googleapis.com/Instance", ProjectID: "svc-app"}},
		},
		manifest.Component{
			ID: "dlq", Title: "orders-events-dead-letter-topic", IconKey: "pubsub",
			ComponentKind: manifest.KindMessaging, ProjectID: "svc-app", TopoRank: 3,
			BackedBy: []manifest.BackedResource{{URI: "//pubsub/dlq", AssetType: "pubsub.googleapis.com/Topic", ProjectID: "svc-app"}},
		},
	)
	v.Placements = append(v.Placements,
		manifest.Placement{ComponentID: "glb", ContainerID: "pub_zone", Column: 1},
		manifest.Placement{ComponentID: "db", ContainerID: "psa_zone", Column: 4},
		manifest.Placement{ComponentID: "dlq", ContainerID: "priv_sub", Column: 3},
	)

	for i := 1; i <= 18; i++ {
		id := fmt.Sprintf("svc_%02d", i)
		longName := fmt.Sprintf("checkout-fulfillment-worker-microservice-%02d-europe-west1", i)
		m.Components = append(m.Components, manifest.Component{
			ID: id, Title: longName, IconKey: "cloud_run", ComponentKind: manifest.KindWorkload,
			ProjectID: "svc-app", ServiceProjectID: "svc-app", TopoRank: 2, SubRank: i,
			Badges:   []manifest.ComponentBadge{{Kind: "sa", Label: "sa: runtime"}},
			BackedBy: []manifest.BackedResource{{URI: "//run/" + id, AssetType: "run.googleapis.com/Service", ProjectID: "svc-app"}},
		})
		v.Placements = append(v.Placements, manifest.Placement{ComponentID: id, ContainerID: "priv_sub", Column: 2})
		v.Edges = append(v.Edges,
			manifest.Edge{From: "glb", To: id, Verb: "routes traffic", FlowKind: manifest.FlowRuntime, SemanticTag: "ingress", Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{fmt.Sprintf("e_in_%d", i)}},
			manifest.Edge{From: id, To: "db", Verb: "queries orders", FlowKind: manifest.FlowRuntime, SemanticTag: "data", Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{fmt.Sprintf("e_db_%d", i)}},
		)
	}
	v.Edges = append(v.Edges, manifest.Edge{
		From: "svc_01", To: "dlq", Verb: "dead letters", FlowKind: manifest.FlowRuntime, SemanticTag: "dlq", IsRetryOrDLQ: true,
		Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e_dlq"},
	})
	m.Views = []manifest.View{v}

	outDir := t.TempDir()
	cons := manifest.ConservationReport{
		Status:                   "PASS",
		FactsPrimaryCount:        21,
		RepresentedPrimaryCount:  21,
		FactsFunctionalEdgeCount: 37,
		View0Coverage:            1.0,
	}
	summary, err := Render(context.Background(), m, reg, RenderOptions{
		Formats:      []Format{FormatHTML, FormatPNG},
		OutputDir:    outDir,
		ChromePath:   chromePath,
		Conservation: &cons,
	})
	if err != nil {
		t.Fatalf("high fan-in hybrid Render failed: %v", err)
	}
	if summary.GeometryReport == nil || !summary.GeometryReport.AllPass() {
		t.Fatalf("geometry_report failed: %+v", summary.GeometryReport)
	}
	vr := summary.GeometryReport.Views[0]
	if len(vr.NodeBoxes) != 21 || len(vr.ContainerBoxes) != 6 || len(vr.BadgeBoxes) != 6 {
		t.Fatalf("unexpected box counts: nodes=%d containers=%d badges=%d", len(vr.NodeBoxes), len(vr.ContainerBoxes), len(vr.BadgeBoxes))
	}
	if len(vr.LabelBoxes) == 0 {
		t.Fatalf("expected non-empty verb label boxes on rendered view")
	}
	if vr.AspectRatio < 0.35 || vr.AspectRatio > 2.60 {
		t.Fatalf("aspect ratio %.3f outside [0.35, 2.60]", vr.AspectRatio)
	}
	// Verify BadgeBoxes (.arch-box-header) never overlap NodeBoxes (.arch-card).
	for _, b := range vr.BadgeBoxes {
		for _, n := range vr.NodeBoxes {
			dx := math.Min(b.X+b.Width, n.X+n.Width) - math.Max(b.X, n.X)
			dy := math.Min(b.Y+b.Height, n.Y+n.Height) - math.Max(b.Y, n.Y)
			if dx > 0.5 && dy > 0.5 {
				t.Fatalf("badge box %q overlapped node box %q", b.ID, n.ID)
			}
		}
	}
}

func TestHeadlessChromeForcedCollisionsReturnLayoutCollisionError(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)
	runner := &ChromeDPRunner{ChromePath: chromePath}

	// 1. Forced card collision: inject an obstacle .arch-card overlapping the first card.
	m := buildBipartiteManifest(1, 1)
	rawHTML, err := BuildHTML(m, reg)
	if err != nil {
		t.Fatalf("BuildHTML: %v", err)
	}
	injectedCard := `<article class="arch-card" data-component-id="blocker" style="position:absolute;left:20px;top:20px;width:240px;height:140px;z-index:9;">Blocker</article>`
	forcedCardHTML := bytes.Replace(rawHTML, []byte(`<svg class="connector-layer"></svg>`), []byte(`<svg class="connector-layer"></svg>`+injectedCard), 1)

	_, err = runner.Render(context.Background(), BrowserRenderRequest{HTML: forcedCardHTML})
	var colErr *LayoutCollisionError
	if !errors.As(err, &colErr) {
		t.Fatalf("expected *LayoutCollisionError on forced card overlap, got %v", err)
	}
	if !strings.Contains(colErr.Error(), "card_collision") {
		t.Fatalf("expected card_collision kind, got: %v", colErr)
	}

	// 2. Forced text ellipsis collision: verify text-overflow: ellipsis fails layout verification.
	ellipsisHTML := bytes.Replace(rawHTML, []byte("</head>"), []byte(`<style>.arch-card-title{text-overflow:ellipsis;overflow:hidden;white-space:nowrap;}</style></head>`), 1)
	_, err = runner.Render(context.Background(), BrowserRenderRequest{HTML: ellipsisHTML})
	if !errors.As(err, &colErr) {
		t.Fatalf("expected *LayoutCollisionError on forced text-overflow:ellipsis, got %v", err)
	}
	if !strings.Contains(colErr.Error(), "text_ellipsis") {
		t.Fatalf("expected text_ellipsis error, got: %v", colErr)
	}
}

func mustPrepare(t *testing.T, m *manifest.Manifest, reg *icons.Registry) *PreparedDocument {
	t.Helper()
	doc, err := PrepareDocument(m, reg)
	if err != nil {
		t.Fatalf("PrepareDocument: %v", err)
	}
	return doc
}

func buildHugeMultiViewManifest() *manifest.Manifest {
	m := &manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Metadata:      manifest.Metadata{GeneratedAt: "2026-11-27T06:00:00Z", Scopes: []string{"projects/prod-core"}},
		Containers: []manifest.Container{
			{ID: "proj", Title: "prod-core", Kind: manifest.ContainerProject, OrderHint: 10},
			{ID: "vpc", Title: "vpc-prod-core", Kind: manifest.ContainerVPC, ParentContainerID: "proj", OrderHint: 20},
			{ID: "sub_1", Title: "europe-west1 / sub-tier-1", Kind: manifest.ContainerSubnet, Exposure: "PRIVATE", ParentContainerID: "vpc", OrderHint: 30},
			{ID: "sub_2", Title: "europe-west1 / sub-tier-2", Kind: manifest.ContainerSubnet, Exposure: "PRIVATE", ParentContainerID: "vpc", OrderHint: 40},
			{ID: "sub_3", Title: "europe-west1 / sub-tier-3", Kind: manifest.ContainerSubnet, Exposure: "PRIVATE", ParentContainerID: "vpc", OrderHint: 50},
			{ID: "sub_4", Title: "europe-west1 / sub-tier-4", Kind: manifest.ContainerSubnet, Exposure: "PRIVATE", ParentContainerID: "vpc", OrderHint: 60},
		},
		Components: []manifest.Component{
			{
				ID: "ing", Title: "enterprise_global_ingress_balancer_edge", IconKey: "cloud_load_balancing", ComponentKind: manifest.KindIngress, ProjectID: "prod-core",
				BackedBy: []manifest.BackedResource{{URI: "//compute/ing", AssetType: "compute.googleapis.com/GlobalForwardingRule", ProjectID: "prod-core"}},
			},
			{
				ID: "gw", Title: "enterprise_edge_gateway_proxy_service", IconKey: "api_gateway", ComponentKind: manifest.KindWorkload, ProjectID: "prod-core",
				BackedBy: []manifest.BackedResource{{URI: "//apigw/gw", AssetType: "apigateway.googleapis.com/Gateway", ProjectID: "prod-core"}},
			},
			{
				ID: "auth", Title: "enterprise_identity_token_broker_service", IconKey: "cloud_run", ComponentKind: manifest.KindWorkload, ProjectID: "prod-core",
				BackedBy: []manifest.BackedResource{{URI: "//run/auth", AssetType: "run.googleapis.com/Service", ProjectID: "prod-core"}},
			},
			{
				ID: "bus", Title: "enterprise_domain_events_pubsub_topic", IconKey: "pubsub", ComponentKind: manifest.KindMessaging, ProjectID: "prod-core",
				BackedBy: []manifest.BackedResource{{URI: "//pubsub/bus", AssetType: "pubsub.googleapis.com/Topic", ProjectID: "prod-core"}},
			},
			{
				ID: "cache", Title: "enterprise_distributed_memorystore_cluster", IconKey: "memorystore", ComponentKind: manifest.KindDatastore, ProjectID: "prod-core",
				BackedBy: []manifest.BackedResource{{URI: "//redis/cache", AssetType: "redis.googleapis.com/Instance", ProjectID: "prod-core"}},
			},
			{
				ID: "bq", Title: "enterprise_analytics_bigquery_warehouse", IconKey: "bigquery", ComponentKind: manifest.KindDatastore, ProjectID: "prod-core",
				BackedBy: []manifest.BackedResource{{URI: "//bq/bq", AssetType: "bigquery.googleapis.com/Dataset", ProjectID: "prod-core"}},
			},
			{
				ID: "db", Title: "enterprise_orders_primary_cloudsql_store", IconKey: "cloud_sql", ComponentKind: manifest.KindDatastore, ProjectID: "prod-core",
				BackedBy: []manifest.BackedResource{{URI: "//sql/db", AssetType: "sqladmin.googleapis.com/Instance", ProjectID: "prod-core"}},
			},
		},
	}

	placements := []manifest.Placement{
		{ComponentID: "ing", ContainerID: "sub_1", Column: 0},
		{ComponentID: "gw", ContainerID: "sub_1", Column: 1},
		{ComponentID: "auth", ContainerID: "sub_1", Column: 2},
		{ComponentID: "bus", ContainerID: "sub_1", Column: 4},
		{ComponentID: "cache", ContainerID: "sub_1", Column: 5},
		{ComponentID: "bq", ContainerID: "sub_1", Column: 6},
		{ComponentID: "db", ContainerID: "sub_1", Column: 7},
	}
	subForIdx := func(i int) string {
		switch {
		case i <= 3:
			return "sub_1"
		case i <= 6:
			return "sub_2"
		case i <= 9:
			return "sub_3"
		default:
			return "sub_4"
		}
	}
	for i := 1; i <= 12; i++ {
		id := fmt.Sprintf("w_%02d", i)
		m.Components = append(m.Components, manifest.Component{
			ID:            id,
			Title:         fmt.Sprintf("orders-worker-service-%02d", i),
			Subtitle:      "Cloud Run Gen2 Service",
			IconKey:       "cloud_run",
			ComponentKind: manifest.KindWorkload,
			ProjectID:     "prod-core",
			InlineMeta: []manifest.InlineMeta{
				{Label: "Region", Value: "europe-west1"},
				{Label: "Runtime", Value: "cloud-run-v2"},
				{Label: "Tier", Value: "primary-worker"},
			},
			BackedBy: []manifest.BackedResource{{URI: "//run/" + id, AssetType: "run.googleapis.com/Service", ProjectID: "prod-core"}},
		})
		placements = append(placements, manifest.Placement{ComponentID: id, ContainerID: subForIdx(i), Column: 3})
	}

	edges := []manifest.Edge{
		{From: "ing", To: "gw", Verb: "Routes edge", FlowKind: manifest.FlowRuntime, SemanticTag: "ingress", Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e0"}},
		{From: "gw", To: "auth", Verb: "Verifies token", FlowKind: manifest.FlowRuntime, SemanticTag: "ingress", Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e1"}},
		{From: "auth", To: "w_01", Verb: "Routes primary", FlowKind: manifest.FlowRuntime, SemanticTag: "ingress", Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e2"}},
		{From: "w_01", To: "bus", Verb: "Publishes event", FlowKind: manifest.FlowRuntime, SemanticTag: "async", Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e3"}},
		{From: "w_04", To: "w_07", Verb: "Streams", FlowKind: manifest.FlowRuntime, SemanticTag: "async", Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e4"}},
		{From: "w_07", To: "w_12", Verb: "Delegates", FlowKind: manifest.FlowRuntime, SemanticTag: "traffic", Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e5"}},
		{From: "bus", To: "cache", Verb: "Caches state", FlowKind: manifest.FlowRuntime, SemanticTag: "data", Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e6"}},
		{From: "cache", To: "bq", Verb: "Exports metrics", FlowKind: manifest.FlowRuntime, SemanticTag: "data", Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e7"}},
		{From: "bq", To: "db", Verb: "Writes audit", FlowKind: manifest.FlowRuntime, SemanticTag: "data", Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e8"}},
	}

	viewIDs := []string{manifest.SystemIndexViewID, "view_1", "view_2", "view_3"}
	for idx, vid := range viewIDs {
		m.Views = append(m.Views, manifest.View{
			ID:           vid,
			Title:        fmt.Sprintf("Multi-View Canvas %d", idx),
			Archetype:    manifest.ArchetypeLayeredFlow,
			AccentMode:   manifest.AccentRuntime,
			ContainerIDs: []string{"proj", "vpc", "sub_1", "sub_2", "sub_3", "sub_4"},
			Placements:   placements,
			Edges:        edges,
		})
	}
	return m
}

func assertBoxInsideImage(t *testing.T, viewID, kind string, boxes []Box, w, h int) {
	t.Helper()
	fw, fh := float64(w), float64(h)
	for _, bx := range boxes {
		if bx.Width <= 0 || bx.Height <= 0 || bx.X < 0 || bx.Y < 0 || bx.X+bx.Width > fw || bx.Y+bx.Height > fh {
			t.Fatalf("view %s %s box %q (%.2f,%.2f %.2fx%.2f) out of PNG bounds %dx%d",
				viewID, kind, bx.ID, bx.X, bx.Y, bx.Width, bx.Height, w, h)
		}
	}
}

func TestHeadlessChromeMultiViewHugeCanvasIsolationAndBoxAlignment(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)
	m := buildHugeMultiViewManifest()
	outDir := t.TempDir()

	summary, err := Render(context.Background(), m, reg, RenderOptions{
		Formats:    []Format{FormatPNG, FormatPDF, FormatHTML},
		OutputDir:  outDir,
		ChromePath: chromePath,
	})
	if err != nil {
		t.Fatalf("Render huge multi-view manifest failed: %v", err)
	}
	if summary.GeometryReport == nil || !summary.GeometryReport.AllPass() {
		t.Fatalf("expected GeometryReport AllPass, got %+v", summary.GeometryReport)
	}
	if len(summary.PNGPaths) != 4 || len(summary.GeometryReport.Views) != 4 {
		t.Fatalf("expected 4 PNGs and 4 ViewReports, got %d / %d", len(summary.PNGPaths), len(summary.GeometryReport.Views))
	}

	totalHeightPx := 0
	for _, vr := range summary.GeometryReport.Views {
		totalHeightPx += vr.HeightPx
	}
	if totalHeightPx <= 16000 {
		t.Fatalf("expected cumulative multi-view height > 16000 px, got %d", totalHeightPx)
	}
	for i, pngPath := range summary.PNGPaths {
		vr := summary.GeometryReport.Views[i]
		t.Run(vr.ID, func(t *testing.T) {
			verifyHugeViewPNGAndBoxes(t, vr, pngPath)
		})
	}

	pdfBytes, err := os.ReadFile(summary.PDFPath)
	if err != nil {
		t.Fatalf("read pdf: %v", err)
	}
	if !bytes.HasPrefix(pdfBytes, []byte("%PDF-")) || countPDFPages(pdfBytes) != 4 {
		t.Fatalf("expected valid 4-page PDF, got pages=%d", countPDFPages(pdfBytes))
	}
}

func verifyHugeViewPNGAndBoxes(t *testing.T, vr ViewReport, pngPath string) {
	t.Helper()
	w, h, issues := InspectPNGFile(pngPath, &vr)
	if w != vr.WidthPx || h != vr.HeightPx {
		t.Fatalf("view %s PNG %dx%d != report %dx%d", vr.ID, w, h, vr.WidthPx, vr.HeightPx)
	}
	if len(issues) > 0 {
		t.Fatalf("view %s streaming PNG & box checks failed: %v", vr.ID, issues)
	}
	if float64(vr.WidthPx)*float64(vr.HeightPx) > maxRasterPixels {
		t.Fatalf("view %s total raster pixels %d exceeds %.0f ceiling", vr.ID, vr.WidthPx*vr.HeightPx, maxRasterPixels)
	}
	if len(vr.NodeBoxes) != 19 || len(vr.ContainerBoxes) != 6 || len(vr.BadgeBoxes) != 6 || len(vr.LabelBoxes) == 0 {
		t.Fatalf("view %s unexpected box counts: nodes=%d containers=%d badges=%d labels=%d",
			vr.ID, len(vr.NodeBoxes), len(vr.ContainerBoxes), len(vr.BadgeBoxes), len(vr.LabelBoxes))
	}
	assertBoxInsideImage(t, vr.ID, "node", vr.NodeBoxes, vr.WidthPx, vr.HeightPx)
	assertBoxInsideImage(t, vr.ID, "container", vr.ContainerBoxes, vr.WidthPx, vr.HeightPx)
	assertBoxInsideImage(t, vr.ID, "badge", vr.BadgeBoxes, vr.WidthPx, vr.HeightPx)
	assertBoxInsideImage(t, vr.ID, "label", vr.LabelBoxes, vr.WidthPx, vr.HeightPx)
}

func TestHeadlessChromeLargeScaleAreaCapAndPDFPagination(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)
	m := buildHugeMultiViewManifest()
	outDir := t.TempDir()

	// Inject a large (~4521x3496 DIP) overview alongside 3 detail views without modifying layout logic.
	summary, err := Render(context.Background(), m, reg, RenderOptions{
		Formats:   []Format{FormatPNG, FormatPDF, FormatHTML},
		OutputDir: outDir,
		Runner:    &largeViewSizeWrapper{inner: &ChromeDPRunner{ChromePath: chromePath}},
	})
	if err != nil {
		t.Fatalf("Render large-scale multi-view failed: %v", err)
	}
	if summary.GeometryReport == nil || !summary.GeometryReport.AllPass() {
		t.Fatalf("expected GeometryReport AllPass, got %+v", summary.GeometryReport)
	}
	vr0 := summary.GeometryReport.Views[0]
	pixels0 := float64(vr0.WidthPx) * float64(vr0.HeightPx)
	if pixels0 > maxRasterPixels || vr0.WidthPx >= int(math.Round(4521*maxRasterScale)) {
		t.Fatalf("expected large-scale view_0 raster pixels <= %.0f and scaled below %.1fx (%dx%d = %.0f px)", maxRasterPixels, maxRasterScale, vr0.WidthPx, vr0.HeightPx, pixels0)
	}
	for i, pngPath := range summary.PNGPaths {
		vr := summary.GeometryReport.Views[i]
		t.Run(vr.ID, func(t *testing.T) {
			if float64(vr.WidthPx)*float64(vr.HeightPx) > maxRasterPixels {
				t.Fatalf("view %s raster pixels %d > %.0f", vr.ID, vr.WidthPx*vr.HeightPx, maxRasterPixels)
			}
			if _, _, issues := InspectPNGFile(pngPath, &vr); len(issues) > 0 {
				t.Fatalf("view %s streaming PNG & box checks failed: %v", vr.ID, issues)
			}
		})
	}
	pdfBytes, err := os.ReadFile(summary.PDFPath)
	if err != nil {
		t.Fatalf("read pdf: %v", err)
	}
	if pages := countPDFPages(pdfBytes); pages != 4 {
		t.Fatalf("expected 4 PDF pages for 4 large-scale views, got %d", pages)
	}
}

type largeViewSizeWrapper struct {
	inner BrowserRunner
}

func (c *largeViewSizeWrapper) Render(ctx context.Context, req BrowserRenderRequest) (*BrowserOutput, error) {
	css := `<style>.view-sheet[data-view-id="view_0_system_index"]{min-width:4521px !important;min-height:3496px !important;}</style></head>`
	req.HTML = bytes.Replace(req.HTML, []byte("</head>"), []byte(css), 1)
	return c.inner.Render(ctx, req)
}

type opacityZeroRunner struct {
	inner BrowserRunner
}

func (o *opacityZeroRunner) Render(ctx context.Context, req BrowserRenderRequest) (*BrowserOutput, error) {
	css := `<style>.view-sheet > * { opacity: 0 !important; }</style></head>`
	req.HTML = bytes.Replace(req.HTML, []byte("</head>"), []byte(css), 1)
	return o.inner.Render(ctx, req)
}

func encodeSolidColorPNG(t *testing.T, w, h int, fill color.NRGBA, border color.NRGBA, borderPx int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	fillTestRect(img, 0, 0, w, h, fill)
	if borderPx > 0 {
		fillTestRect(img, 0, 0, w, borderPx, border)
		fillTestRect(img, 0, h-borderPx, w, h, border)
		fillTestRect(img, 0, 0, borderPx, h, border)
		fillTestRect(img, w-borderPx, 0, w, h, border)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func encodeMonochromeBarsPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	bg := color.NRGBA{R: 248, G: 250, B: 252, A: 255}
	fillTestRect(img, 0, 0, w, h, bg)
	for i := range 10 {
		v := uint8(20 + i*20)
		x0 := 40 + i*36
		fillTestRect(img, x0, 40, x0+24, 240, color.NRGBA{R: v, G: v, B: v, A: 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode monochrome png: %v", err)
	}
	return buf.Bytes()
}

func TestPixelVerificationRejectsBlankAndSolidPNGs(t *testing.T) {
	reg := loadTestIcons(t)
	t.Run("RealChromeBlankOpacityZeroReturnsGeometryReportError", func(t *testing.T) {
		testRealChromeBlankOpacityZero(t, reg)
	})
	t.Run("OfflineNegativeMatrix", func(t *testing.T) {
		testOfflineNegativePNGMatrix(t, reg)
	})
}

func testRealChromeBlankOpacityZero(t *testing.T, reg *icons.Registry) {
	t.Helper()
	chromePath := requireChromeForTest(t)
	m := buildBipartiteManifest(1, 1)
	outDir := t.TempDir()
	wrapper := &opacityZeroRunner{inner: &ChromeDPRunner{ChromePath: chromePath}}

	summary, err := Render(context.Background(), m, reg, RenderOptions{
		Formats:             []Format{FormatPNG},
		OutputDir:           outDir,
		Runner:              wrapper,
		WriteGeometryReport: true,
	})
	var gre *GeometryReportError
	if !errors.As(err, &gre) {
		t.Fatalf("expected *GeometryReportError for blank opacity-zero Chrome PNG, got %v", err)
	}
	if summary == nil || summary.GeometryPath == "" || summary.GeometryReport == nil {
		t.Fatalf("expected geometry_report.json to be written alongside GeometryReportError, got %+v", summary)
	}
	if _, statErr := os.Stat(summary.GeometryPath); statErr != nil {
		t.Fatalf("expected geometry_report.json on disk: %v", statErr)
	}
	if summary.GeometryReport.AllPass() || summary.GeometryReport.Views[0].Status != "FAIL" || len(summary.GeometryReport.Views[0].Overlaps) == 0 {
		t.Fatalf("expected FAIL ViewReport with non-empty Overlaps, got %+v", summary.GeometryReport.Views[0])
	}
}

func encodePartialTileBlankedPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := buildTestDiagramImage(w, h)
	// Fill a NodeBox region (120..360, 260..420) with Skia solid tile whiteout color (#eff5fe).
	fillTestRect(img, 100, 250, 380, 440, color.NRGBA{R: 239, G: 245, B: 254, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode partial-tile png: %v", err)
	}
	if _, _, issues := InspectPNGBytes(buf.Bytes(), nil); len(issues) > 0 {
		t.Fatalf("expected partial-tile PNG to pass whole-image InspectPNGBytes, got %v", issues)
	}
	return buf.Bytes()
}

func testOfflineNegativePNGMatrix(t *testing.T, reg *icons.Registry) {
	t.Helper()
	m := loadAcmeManifest(t)
	white := color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	dark := color.NRGBA{R: 15, G: 23, B: 42, A: 255}
	bg := color.NRGBA{R: 248, G: 250, B: 252, A: 255}

	cases := []struct {
		name        string
		pngBytes    []byte
		viewReports []ViewReport
		wantPass    bool
		wantSnippet string
	}{
		{name: "blank_white", pngBytes: encodeSolidColorPNG(t, 900, 600, white, white, 0), wantPass: false, wantSnippet: "png_blank"},
		{name: "overfilled_solid", pngBytes: encodeSolidColorPNG(t, 900, 600, dark, bg, 3), wantPass: false, wantSnippet: "png_solid"},
		{name: "monochrome_gray", pngBytes: encodeMonochromeBarsPNG(t, 900, 600), wantPass: false, wantSnippet: "png_monochrome"},
		{
			name:     "partial_tile_node_box_blank",
			pngBytes: encodePartialTileBlankedPNG(t, 900, 600),
			viewReports: []ViewReport{{
				ID:          manifest.SystemIndexViewID,
				Title:       "System Index",
				PNGPath:     "views/00-view_0_system_index.png",
				WidthPx:     900,
				HeightPx:    600,
				AspectRatio: 1.5,
				NodeBoxes:   []Box{{ID: "n_wiped_card", X: 120, Y: 260, Width: 240, Height: 160}},
				Status:      "PASS",
			}},
			wantPass:    false,
			wantSnippet: "png_box_blank",
		},
		{name: "valid_multicolor", pngBytes: encodeTestPNG(t, 900, 600), wantPass: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertOfflinePNGCase(t, m, reg, tc.name, tc.pngBytes, tc.viewReports, tc.wantPass, tc.wantSnippet)
		})
	}
}

func assertOfflinePNGCase(t *testing.T, m *manifest.Manifest, reg *icons.Registry, name string, pngBytes []byte, viewReports []ViewReport, wantPass bool, wantSnippet string) {
	t.Helper()
	outDir := t.TempDir()
	runner := &mockBrowserRunner{
		out: &BrowserOutput{
			Result: BrowserRenderResult{
				OK:          true,
				HTML:        `<!DOCTYPE html><html data-gcp-arch-routed="1"></html>`,
				ViewReports: viewReports,
			},
			PNGs: map[string][]byte{"00-view_0_system_index.png": pngBytes},
		},
	}
	summary, err := Render(context.Background(), m, reg, RenderOptions{
		Formats:   []Format{FormatPNG},
		OutputDir: outDir,
		Runner:    runner,
	})
	if wantPass {
		if err != nil || summary == nil || summary.GeometryReport == nil || !summary.GeometryReport.AllPass() {
			t.Fatalf("expected PASS for %s, got err=%v report=%+v", name, err, summary)
		}
		return
	}
	var gre *GeometryReportError
	if !errors.As(err, &gre) {
		t.Fatalf("expected *GeometryReportError for %s, got %v", name, err)
	}
	if summary == nil || summary.GeometryReport == nil || summary.GeometryReport.Views[0].Status != "FAIL" {
		t.Fatalf("expected FAIL status for %s, got %+v", name, summary)
	}
	joined := strings.Join(summary.GeometryReport.Views[0].Overlaps, "; ")
	if !strings.Contains(joined, wantSnippet) {
		t.Fatalf("expected %q in Overlaps for %s, got %v", wantSnippet, name, summary.GeometryReport.Views[0].Overlaps)
	}
}
