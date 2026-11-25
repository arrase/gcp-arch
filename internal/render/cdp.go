package render

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/arrase/gcp-arch/internal/icons"
	"github.com/arrase/gcp-arch/internal/ioutil"
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

// Collision records a strict layout collision detected by router.js after corridor reflow passes.
type Collision struct {
	ViewID   string   `json:"view_id"`
	Kind     string   `json:"kind"`
	Subjects []string `json:"subjects"`
	Detail   string   `json:"detail"`
}

// LayoutCollisionError is returned when router.js detects unresolved card, badge, pill, or
// parallel collinear overlaps (mapped to CLI exit code 2).
type LayoutCollisionError struct {
	Collisions []Collision
}

func (e *LayoutCollisionError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "layout collision verification failed (%d collision(s)):", len(e.Collisions))
	for _, c := range e.Collisions {
		fmt.Fprintf(&b, "\n  - [%s] view=%s subjects=%v: %s", c.Kind, c.ViewID, c.Subjects, c.Detail)
	}
	return b.String()
}

// ViewRect captures a rendered .view-sheet bounding rectangle in page DIP coordinates.
type ViewRect struct {
	ViewID string  `json:"view_id"`
	Index  int     `json:"index"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// BrowserRenderResult mirrors window.__GCP_ARCH_RENDER_RESULT__ returned by router.js.
type BrowserRenderResult struct {
	OK         bool        `json:"ok"`
	Collisions []Collision `json:"collisions"`
	ViewRects  []ViewRect  `json:"viewRects"`
	HTML       string      `json:"html"`
}

// BrowserRenderRequest configures one isolated headless Chrome rendering pass.
type BrowserRenderRequest struct {
	HTML       []byte
	WantPNG    bool
	WantPDF    bool
	ChromePath string
}

// BrowserOutput bundles the post-routed HTML, optional 3x view PNGs, and optional vector PDF.
type BrowserOutput struct {
	Result BrowserRenderResult
	PNGs   map[string][]byte // keyed by "<00..NN>-<view_id>.png"
	PDF    []byte
}

// BrowserRunner abstracts headless browser execution for deterministic testing and chromedp export.
type BrowserRunner interface {
	Render(ctx context.Context, req BrowserRenderRequest) (*BrowserOutput, error)
}

// RenderOptions configures offline rendering and artifact emission.
type RenderOptions struct {
	Formats       []Format
	OutputDir     string
	ChromePath    string
	Runner        BrowserRunner
	WarningWriter io.Writer
}

// RenderSummary reports the written files after Render succeeds.
type RenderSummary struct {
	HTMLPath   string
	PDFPath    string
	PNGPaths   []string
	UsedChrome bool
}

// Render validates m structurally, generates the offline HTML bundle, runs headless Chrome via
// BrowserRunner (or falls back gracefully when only --format html is requested without Chrome),
// and atomically writes all requested artifacts into opts.OutputDir.
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

	summary := &RenderSummary{}

	runner, chromePath, err := resolveRenderRunner(opts)
	if err != nil {
		if wantHTML && !wantPDF && !wantPNG {
			return writeHTMLOnlyFallback(outDir, rawHTML, opts.WarningWriter, err)
		}
		return nil, fmt.Errorf("Chrome/Chromium binary required for pdf/png output: %w", err)
	}

	out, err := runner.Render(ctx, BrowserRenderRequest{
		HTML:       rawHTML,
		WantPNG:    wantPNG,
		WantPDF:    wantPDF,
		ChromePath: chromePath,
	})
	if err != nil {
		return nil, err
	}
	if !out.Result.OK {
		return nil, &LayoutCollisionError{Collisions: out.Result.Collisions}
	}
	summary.UsedChrome = true

	if err := writeRenderArtifacts(outDir, rawHTML, out, wantHTML, wantPNG, wantPDF, summary); err != nil {
		return nil, err
	}
	return summary, nil
}

func resolveRenderRunner(opts RenderOptions) (BrowserRunner, string, error) {
	if opts.Runner != nil {
		return opts.Runner, opts.ChromePath, nil
	}
	found, err := FindChrome(opts.ChromePath)
	if err != nil {
		return nil, "", err
	}
	return &ChromeDPRunner{ChromePath: found}, found, nil
}

func writeHTMLOnlyFallback(outDir string, rawHTML []byte, warnW io.Writer, findErr error) (*RenderSummary, error) {
	htmlPath := filepath.Join(outDir, "architecture.html")
	if err := ioutil.WriteFileAtomic(htmlPath, rawHTML, 0o644, 0o755); err != nil {
		return nil, err
	}
	if warnW == nil {
		warnW = os.Stderr
	}
	_, _ = fmt.Fprintf(warnW, "warning: Chrome/Chromium not found (%v); wrote self-contained %s without CLI collision verification\n", findErr, htmlPath)
	return &RenderSummary{HTMLPath: htmlPath, UsedChrome: false}, nil
}

func writeRenderArtifacts(outDir string, rawHTML []byte, out *BrowserOutput, wantHTML, wantPNG, wantPDF bool, summary *RenderSummary) error {
	if wantHTML {
		htmlBytes := []byte(out.Result.HTML)
		if len(htmlBytes) == 0 {
			htmlBytes = rawHTML
		}
		htmlPath := filepath.Join(outDir, "architecture.html")
		if err := ioutil.WriteFileAtomic(htmlPath, htmlBytes, 0o644, 0o755); err != nil {
			return err
		}
		summary.HTMLPath = htmlPath
	}

	if wantPNG {
		names := make([]string, 0, len(out.PNGs))
		for name := range out.PNGs {
			names = append(names, name)
		}
		sort.Strings(names)
		viewsDir := filepath.Join(outDir, "views")
		for _, name := range names {
			pngPath := filepath.Join(viewsDir, name)
			if err := ioutil.WriteFileAtomic(pngPath, out.PNGs[name], 0o644, 0o755); err != nil {
				return err
			}
			summary.PNGPaths = append(summary.PNGPaths, pngPath)
		}
	}

	if wantPDF {
		pdfPath := filepath.Join(outDir, "architecture.pdf")
		if err := ioutil.WriteFileAtomic(pdfPath, out.PDF, 0o644, 0o755); err != nil {
			return err
		}
		summary.PDFPath = pdfPath
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

// FindChrome locates a local Chrome or Chromium binary via explicitPath, CHROME_PATH, PATH, or
// standard OS installation paths.
func FindChrome(explicitPath string) (string, error) {
	if p := strings.TrimSpace(explicitPath); p != "" {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
		return "", fmt.Errorf("explicit chrome path %q not found", p)
	}
	if env := strings.TrimSpace(os.Getenv("CHROME_PATH")); env != "" {
		if info, err := os.Stat(env); err == nil && !info.IsDir() {
			return env, nil
		}
	}

	candidates := []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"}
	for _, name := range candidates {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}

	var osPaths []string
	switch runtime.GOOS {
	case "darwin":
		osPaths = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	default:
		osPaths = []string{
			"/usr/bin/google-chrome",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
		}
	}
	for _, p := range osPaths {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("no Chrome or Chromium executable found; install Chrome or pass --chrome-path / CHROME_PATH")
}

// ChromeDPRunner executes a single isolated headless Chrome session via chromedp.
type ChromeDPRunner struct {
	ChromePath string
}

// Render runs the in-page orthogonal router over a 0600 temp HTML file inside an isolated
// ephemeral --user-data-dir Chrome session with upfront 3x viewport metrics, then captures
// per-view 3x PNGs and vector PDF pages.
func (r *ChromeDPRunner) Render(ctx context.Context, req BrowserRenderRequest) (*BrowserOutput, error) {
	chromePath := req.ChromePath
	if chromePath == "" {
		chromePath = r.ChromePath
	}
	if chromePath == "" {
		var err error
		chromePath, err = FindChrome("")
		if err != nil {
			return nil, err
		}
	}

	workDir, err := os.MkdirTemp("", "gcp-arch-chrome-*")
	if err != nil {
		return nil, fmt.Errorf("create temp chrome dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	userDataDir := filepath.Join(workDir, "profile")
	if err := os.MkdirAll(userDataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create temp user-data-dir: %w", err)
	}

	htmlPath := filepath.Join(workDir, "input.html")
	if err := os.WriteFile(htmlPath, req.HTML, 0o600); err != nil {
		return nil, fmt.Errorf("write temp html: %w", err)
	}

	allocOpts := append(slices.Clone(chromedp.DefaultExecAllocatorOptions[:]),
		chromedp.ExecPath(chromePath),
		chromedp.UserDataDir(userDataDir),
		chromedp.Flag("headless", "new"),
		chromedp.DisableGPU,
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("metrics-recording-only", true),
		chromedp.Flag("mute-audio", true),
	)
	if needsNoSandbox() {
		allocOpts = append(allocOpts, chromedp.NoSandbox)
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, allocOpts...)
	defer cancelAlloc()

	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	// Set fixed 3x viewport metrics BEFORE navigating so layout and CaptureScreenshot share 3x DPI.
	if _, err := chromedp.Call(browserCtx, emulation.SetDeviceMetricsOverride, emulation.SetDeviceMetricsOverrideParams{
		Width:             1200,
		Height:            900,
		DeviceScaleFactor: 3.0,
		Mobile:            false,
	}); err != nil {
		return nil, fmt.Errorf("set device metrics override: %w", err)
	}

	fileURL := (&url.URL{Scheme: "file", Path: htmlPath}).String()
	if err := chromedp.Do(browserCtx, chromedp.Navigate(fileURL)); err != nil {
		return nil, fmt.Errorf("navigate temp html: %w", err)
	}

	res, err := chromedp.Run(browserCtx, chromedp.Evaluate[BrowserRenderResult](
		"window.__GCP_ARCH_RENDER_PROMISE__",
		chromedp.EvalAwaitPromise,
	))
	if err != nil {
		return nil, fmt.Errorf("await in-page router: %w", err)
	}
	if !res.OK {
		return nil, &LayoutCollisionError{Collisions: res.Collisions}
	}

	out := &BrowserOutput{Result: res}

	if req.WantPNG {
		out.PNGs = make(map[string][]byte, len(res.ViewRects))
		captureBeyond := true
		for _, vr := range res.ViewRects {
			shot, err := chromedp.Call(browserCtx, page.CaptureScreenshot, page.CaptureScreenshotParams{
				Format:                page.CaptureScreenshotFormatPng,
				CaptureBeyondViewport: &captureBeyond,
				Clip: &page.Viewport{
					X:      vr.X,
					Y:      vr.Y,
					Width:  vr.Width,
					Height: vr.Height,
					Scale:  1,
				},
			})
			if err != nil {
				return nil, fmt.Errorf("capture screenshot for view %q: %w", vr.ViewID, err)
			}
			filename := fmt.Sprintf("%02d-%s.png", vr.Index, vr.ViewID)
			out.PNGs[filename] = shot.Data
		}
	}

	if req.WantPDF {
		printBG := true
		preferCSS := true
		pdfRes, err := chromedp.Call(browserCtx, page.PrintToPDF, page.PrintToPDFParams{
			PrintBackground:   &printBG,
			PreferCSSPageSize: &preferCSS,
		})
		if err != nil {
			return nil, fmt.Errorf("print to pdf: %w", err)
		}
		out.PDF = pdfRes.Data
	}

	return out, nil
}

// needsNoSandbox enables --no-sandbox strictly inside a Linux container without unprivileged user namespaces.
func needsNoSandbox() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	inContainer := false
	if _, err := os.Stat("/.dockerenv"); err == nil {
		inContainer = true
	} else if _, err := os.Stat("/run/.containerenv"); err == nil {
		inContainer = true
	} else if data, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		s := string(data)
		if strings.Contains(s, "docker") || strings.Contains(s, "kubepods") || strings.Contains(s, "containerd") {
			inContainer = true
		}
	}
	if !inContainer {
		return false
	}
	if data, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone"); err == nil {
		if strings.TrimSpace(string(data)) == "0" {
			return true
		}
	}
	return os.Geteuid() == 0
}
