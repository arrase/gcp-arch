package render

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/arrase/gcp-arch/internal/atomicfile"
	"github.com/arrase/gcp-arch/internal/icons"
	"github.com/arrase/gcp-arch/internal/manifest"
)

// Format identifies an output artifact produced by Render.
type Format string

// Supported render output formats.
const (
	FormatPDF  Format = "pdf"
	FormatPNG  Format = "png"
	FormatHTML Format = "html"
)

// RenderOptions configures offline rendering and artifact emission.
type RenderOptions struct {
	Formats             []Format
	OutputDir           string
	ChromePath          string
	ConfigChromePath    string
	Runner              BrowserRunner
	WarningWriter       io.Writer
	Conservation        *manifest.ConservationReport
	WriteGeometryReport bool
}

// RenderSummary reports the written files and geometry report after Render runs.
type RenderSummary struct {
	HTMLPath       string
	PDFPath        string
	PNGPaths       []string
	GeometryPath   string
	GeometryReport *GeometryReport
	UsedChrome     bool
}

// Render validates m structurally, generates the offline HTML bundle, runs headless Chrome via
// BrowserRunner (or falls back gracefully when only FormatHTML is requested without Chrome),
// verifies all rendered PNGs with streaming pixel checks, and atomically writes all requested
// artifacts into opts.OutputDir.
func Render(ctx context.Context, m *manifest.Manifest, reg *icons.Registry, opts RenderOptions) (*RenderSummary, error) {
	if err := manifest.ValidateStructure(m, reg); err != nil {
		return nil, err
	}
	outDir := strings.TrimSpace(opts.OutputDir)
	if outDir == "" {
		outDir = "out"
	}

	wantPDF, wantPNG, wantHTML, err := parseFormats(opts.Formats)
	if err != nil {
		return nil, err
	}

	rawHTML, err := BuildHTML(m, reg)
	if err != nil {
		return nil, err
	}

	runner, chromePath, err := resolveRenderRunner(opts)
	if err != nil {
		if strings.TrimSpace(opts.ChromePath) != "" {
			return nil, err
		}
		if wantHTML && !wantPDF && !wantPNG {
			return writeHTMLOnlyFallback(outDir, rawHTML, opts.WarningWriter, err)
		}
		return nil, fmt.Errorf("Chrome/Chromium binary required for pdf/png output: %w", err)
	}

	var streamed pngStreamCollector
	var onPNG func(filename string, pngData []byte, vr *ViewReport) error
	if wantPNG {
		streamed.viewsDir = filepath.Join(outDir, "views")
		onPNG = streamed.handlePNG
	}

	out, err := runner.Render(ctx, BrowserRenderRequest{
		HTML:       rawHTML,
		WantHTML:   wantHTML,
		WantPNG:    wantPNG,
		WantPDF:    wantPDF,
		ChromePath: chromePath,
		OnPNG:      onPNG,
	})
	if err != nil {
		return nil, err
	}
	if !out.Result.OK {
		return nil, &LayoutCollisionError{Collisions: out.Result.Collisions}
	}

	summary := &RenderSummary{UsedChrome: true}
	if err := writeRenderArtifacts(&renderArtifactContext{
		outDir:   outDir,
		rawHTML:  rawHTML,
		out:      out,
		streamed: &streamed,
		opts:     opts,
		wantHTML: wantHTML,
		wantPNG:  wantPNG,
		wantPDF:  wantPDF,
	}, summary); err != nil {
		return nil, err
	}
	if summary.GeometryReport != nil && !summary.GeometryReport.AllPass() {
		return summary, &GeometryReportError{
			Path:   summary.GeometryPath,
			Report: summary.GeometryReport,
			Detail: summarizeGeometryFailures(summary.GeometryReport),
		}
	}
	return summary, nil
}

type streamedPNGItem struct {
	path     string
	filename string
	vr       *ViewReport
}

type pngStreamCollector struct {
	viewsDir string
	cleaned  bool
	paths    []string
	items    []streamedPNGItem
}

func (c *pngStreamCollector) handlePNG(filename string, pngData []byte, vr *ViewReport) error {
	if !c.cleaned {
		cleanStalePNGs(c.viewsDir)
		c.cleaned = true
	}
	pngPath := filepath.Join(c.viewsDir, filename)
	if err := atomicfile.Write(pngPath, pngData, 0o644, 0o755); err != nil {
		return err
	}
	c.paths = append(c.paths, pngPath)
	c.items = append(c.items, streamedPNGItem{path: pngPath, filename: filename, vr: vr})
	return nil
}

func (c *pngStreamCollector) verifyAll() []ViewReport {
	var fallbackViews []ViewReport
	for _, item := range c.items {
		if item.vr != nil {
			*item.vr = normalizeViewReport(*item.vr)
			if item.vr.PNGPath == "" {
				item.vr.PNGPath = filepath.ToSlash(filepath.Join("views", item.filename))
			}
			ValidateViewPNGFile(item.vr, item.path)
			applyViewDimensionChecks(item.vr)
			continue
		}
		stem := strings.TrimSuffix(item.filename, ".png")
		fb := normalizeViewReport(ViewReport{
			ID:      stem,
			Title:   stem,
			PNGPath: filepath.ToSlash(filepath.Join("views", item.filename)),
			Status:  "PASS",
		})
		ValidateViewPNGFile(&fb, item.path)
		applyViewDimensionChecks(&fb)
		fallbackViews = append(fallbackViews, fb)
	}
	return fallbackViews
}

func cleanStalePNGs(viewsDir string) {
	if existing, err := filepath.Glob(filepath.Join(viewsDir, "*.png")); err == nil {
		for _, old := range existing {
			_ = os.Remove(old)
		}
	}
}

func resolveRenderRunner(opts RenderOptions) (BrowserRunner, string, error) {
	if opts.Runner != nil {
		return opts.Runner, opts.ChromePath, nil
	}
	found, err := FindChrome(opts.ChromePath, opts.ConfigChromePath)
	if err != nil {
		return nil, "", err
	}
	return &ChromeDPRunner{ChromePath: found}, found, nil
}

func writeHTMLOnlyFallback(outDir string, rawHTML []byte, warnW io.Writer, findErr error) (*RenderSummary, error) {
	htmlPath := filepath.Join(outDir, "architecture.html")
	if err := atomicfile.Write(htmlPath, rawHTML, 0o644, 0o755); err != nil {
		return nil, err
	}
	if warnW == nil {
		warnW = os.Stderr
	}
	_, _ = fmt.Fprintf(warnW, "warning: Chrome/Chromium not found (%v); wrote self-contained %s without CLI collision verification\n", findErr, htmlPath)
	return &RenderSummary{HTMLPath: htmlPath, UsedChrome: false}, nil
}

type renderArtifactContext struct {
	outDir   string
	rawHTML  []byte
	out      *BrowserOutput
	streamed *pngStreamCollector
	opts     RenderOptions
	wantHTML bool
	wantPNG  bool
	wantPDF  bool
}

func writeRenderArtifacts(a *renderArtifactContext, summary *RenderSummary) error {
	if a.wantHTML {
		htmlBytes := []byte(a.out.Result.HTML)
		if len(htmlBytes) == 0 {
			htmlBytes = a.rawHTML
		}
		htmlPath := filepath.Join(a.outDir, "architecture.html")
		if err := atomicfile.Write(htmlPath, htmlBytes, 0o644, 0o755); err != nil {
			return err
		}
		summary.HTMLPath = htmlPath
		a.out.Result.HTML = ""
	}

	if a.wantPNG {
		if err := finalizePNGAndGeometry(a.outDir, a.out, a.streamed, a.opts.Conservation, a.opts.WriteGeometryReport, summary); err != nil {
			return err
		}
	}

	if a.wantPDF {
		pdfPath := filepath.Join(a.outDir, "architecture.pdf")
		if err := atomicfile.Write(pdfPath, a.out.PDF, 0o644, 0o755); err != nil {
			return err
		}
		summary.PDFPath = pdfPath
		a.out.PDF = nil
	}
	return nil
}

func finalizePNGAndGeometry(
	outDir string,
	out *BrowserOutput,
	streamed *pngStreamCollector,
	conservation *manifest.ConservationReport,
	writeReport bool,
	summary *RenderSummary,
) error {
	var rep *GeometryReport
	if len(streamed.paths) > 0 {
		fallbackViews := streamed.verifyAll()
		slices.Sort(streamed.paths)
		summary.PNGPaths = streamed.paths
		rep = buildStreamedGeometryReport(out, fallbackViews, conservation)
	} else {
		var err error
		rep, err = writeBufferedPNGsAndBuildReport(outDir, out, conservation, summary)
		if err != nil {
			return err
		}
	}
	if rep == nil {
		return nil
	}
	summary.GeometryReport = rep
	if !writeReport {
		return nil
	}

	repBytes, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal geometry report: %w", err)
	}
	repBytes = append(repBytes, '\n')
	repPath := filepath.Join(outDir, "geometry_report.json")
	if err := atomicfile.Write(repPath, repBytes, 0o644, 0o755); err != nil {
		return err
	}
	summary.GeometryPath = repPath
	return nil
}

func buildStreamedGeometryReport(out *BrowserOutput, fallbackViews []ViewReport, conservation *manifest.ConservationReport) *GeometryReport {
	var views []ViewReport
	if len(out.Result.ViewReports) > 0 {
		views = make([]ViewReport, len(out.Result.ViewReports))
		for i, vr := range out.Result.ViewReports {
			views[i] = normalizeViewReport(vr)
		}
	} else {
		views = fallbackViews
	}
	if len(views) == 0 {
		return nil
	}
	return &GeometryReport{
		Views:        views,
		Conservation: normalizeConservationCopy(conservation),
	}
}

func writeBufferedPNGsAndBuildReport(
	outDir string,
	out *BrowserOutput,
	conservation *manifest.ConservationReport,
	summary *RenderSummary,
) (*GeometryReport, error) {
	viewsDir := filepath.Join(outDir, "views")
	cleanStalePNGs(viewsDir)

	names := make([]string, 0, len(out.PNGs))
	for name := range out.PNGs {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		pngPath := filepath.Join(viewsDir, name)
		if err := atomicfile.Write(pngPath, out.PNGs[name], 0o644, 0o755); err != nil {
			return nil, err
		}
		summary.PNGPaths = append(summary.PNGPaths, pngPath)
	}

	var views []ViewReport
	if len(out.Result.ViewReports) > 0 {
		views = buildReportedViewReports(out, names)
	} else if len(names) > 0 {
		views = buildFallbackViewReports(out, names)
	}
	if len(views) == 0 {
		return nil, nil
	}
	return &GeometryReport{
		Views:        views,
		Conservation: normalizeConservationCopy(conservation),
	}, nil
}

func buildReportedViewReports(out *BrowserOutput, sortedPNGNames []string) []ViewReport {
	views := make([]ViewReport, len(out.Result.ViewReports))
	for i := range views {
		nvr := normalizeViewReport(out.Result.ViewReports[i])
		if nvr.PNGPath == "" && i < len(sortedPNGNames) {
			nvr.PNGPath = filepath.ToSlash(filepath.Join("views", sortedPNGNames[i]))
		}
		ValidateViewPNGReport(&nvr, findViewPNGBytes(nvr, i, sortedPNGNames, out.PNGs))
		applyViewDimensionChecks(&nvr)
		views[i] = nvr
	}
	return views
}

func buildFallbackViewReports(out *BrowserOutput, sortedPNGNames []string) []ViewReport {
	views := make([]ViewReport, len(sortedPNGNames))
	for i, name := range sortedPNGNames {
		stem := strings.TrimSuffix(name, ".png")
		vr := normalizeViewReport(ViewReport{
			ID:      stem,
			Title:   stem,
			PNGPath: filepath.ToSlash(filepath.Join("views", name)),
			Status:  "PASS",
		})
		ValidateViewPNGReport(&vr, out.PNGs[name])
		applyViewDimensionChecks(&vr)
		views[i] = vr
	}
	return views
}

func findViewPNGBytes(vr ViewReport, idx int, sortedPNGNames []string, pngs map[string][]byte) []byte {
	if vr.PNGPath != "" {
		if b, ok := pngs[filepath.Base(vr.PNGPath)]; ok {
			return b
		}
	}
	if idx < len(sortedPNGNames) {
		return pngs[sortedPNGNames[idx]]
	}
	return nil
}

func parseFormats(formats []Format) (pdf, png, html bool, err error) {
	if len(formats) == 0 {
		return true, true, true, nil
	}
	for _, f := range formats {
		switch Format(strings.ToLower(strings.TrimSpace(string(f)))) {
		case FormatPDF:
			pdf = true
		case FormatPNG:
			png = true
		case FormatHTML:
			html = true
		default:
			return false, false, false, fmt.Errorf("unsupported render format %q (allowed: pdf, png, html)", f)
		}
	}
	return pdf, png, html, nil
}
