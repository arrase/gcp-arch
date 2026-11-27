package render

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

const (
	defaultChromeSessionTimeout = 2 * time.Minute
	maxRasterScale              = 2.0
	maxRasterDim                = 8192.0
	maxRasterPixels             = 28_000_000.0
)

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
	OK          bool         `json:"ok"`
	Collisions  []Collision  `json:"collisions"`
	ViewRects   []ViewRect   `json:"viewRects"`
	ViewReports []ViewReport `json:"viewReports"`
	HTML        string       `json:"html"`
}

// BrowserRenderRequest configures one isolated headless Chrome rendering pass.
type BrowserRenderRequest struct {
	HTML       []byte
	WantHTML   bool
	WantPNG    bool
	WantPDF    bool
	ChromePath string
	// OnPNG, when non-nil, streams each captured view PNG directly to the caller so PNG
	// bytes can be written and verified immediately without retaining all PNGs in memory.
	OnPNG func(filename string, pngData []byte, vr *ViewReport) error
}

// BrowserOutput bundles the post-routed HTML, optional high-DPI view PNGs, and optional vector PDF.
type BrowserOutput struct {
	Result BrowserRenderResult
	PNGs   map[string][]byte // keyed by "<00..NN>-<view_id>.png" (populated when Request.OnPNG is nil)
	PDF    []byte
}

// BrowserRunner abstracts headless browser execution for deterministic testing and chromedp export.
type BrowserRunner interface {
	Render(ctx context.Context, req BrowserRenderRequest) (*BrowserOutput, error)
}

var chromeOSPaths = defaultChromeOSPaths()

func defaultChromeOSPaths() []string {
	if runtime.GOOS == "darwin" {
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	}
	return []string{
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
	}
}

// FindChrome locates a local Chrome or Chromium binary via explicitPath (failing fast if missing),
// CHROME_PATH, optional fallbackPaths (such as config.yaml chrome_path), PATH, or standard OS
// installation paths.
func FindChrome(explicitPath string, fallbackPaths ...string) (string, error) {
	if p := strings.TrimSpace(explicitPath); p != "" {
		if isExistingFile(p) {
			return p, nil
		}
		return "", fmt.Errorf("explicit chrome path %q not found", p)
	}
	if env := strings.TrimSpace(os.Getenv("CHROME_PATH")); env != "" && isExistingFile(env) {
		return env, nil
	}
	for _, fb := range fallbackPaths {
		if p := strings.TrimSpace(fb); p != "" && isExistingFile(p) {
			return p, nil
		}
	}

	candidates := []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"}
	for _, name := range candidates {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}

	for _, p := range chromeOSPaths {
		if isExistingFile(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("no Chrome or Chromium executable found; install Chrome or pass --chrome-path / CHROME_PATH")
}

func isExistingFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// ChromeDPRunner executes a single isolated headless Chrome session via chromedp.
type ChromeDPRunner struct {
	ChromePath string
}

// Render runs the in-page orthogonal router over a 0600 temp HTML file inside an isolated
// ephemeral --user-data-dir Chrome session with a 1x initial viewport, then captures
// optional routed HTML, vector PDF pages, and per-view high-DPI PNGs.
func (r *ChromeDPRunner) Render(ctx context.Context, req BrowserRenderRequest) (*BrowserOutput, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultChromeSessionTimeout)
		defer cancel()
	}

	chromePath, err := resolveChromeExecutable(req.ChromePath, r.ChromePath)
	if err != nil {
		return nil, err
	}

	workDir, htmlPath, userDataDir, err := prepareChromeWorkDir(req.HTML)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, buildChromeAllocOpts(chromePath, userDataDir)...)
	defer cancelAlloc()

	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	// Keep a compact 1.0x viewport throughout navigation, routing, PrintToPDF, and beyond-viewport
	// Clip{Scale} PNG capture to minimize Chromium software tile RAM with zero Skia whiteout.
	if err := setChromeViewportMetrics(browserCtx, 1280, 800, 1.0); err != nil {
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

	if req.WantHTML {
		htmlStr, err := chromedp.Run(browserCtx, chromedp.Evaluate[string](
			`'<!DOCTYPE html>\n' + document.documentElement.outerHTML`,
		))
		if err != nil {
			return nil, fmt.Errorf("serialize routed html: %w", err)
		}
		res.HTML = htmlStr
	}

	out := &BrowserOutput{Result: res}
	if req.WantPDF {
		out.PDF, err = captureChromePDF(browserCtx)
		if err != nil {
			return nil, err
		}
	}
	if req.WantPNG {
		if err := captureChromePNGs(browserCtx, &out.Result, out, req.OnPNG); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func resolveChromeExecutable(reqChromePath, defaultChromePath string) (string, error) {
	if reqChromePath != "" {
		return reqChromePath, nil
	}
	if defaultChromePath != "" {
		return defaultChromePath, nil
	}
	return FindChrome("")
}

func prepareChromeWorkDir(htmlBytes []byte) (workDir, htmlPath, userDataDir string, err error) {
	workDir, err = os.MkdirTemp("", "gcp-arch-chrome-*")
	if err != nil {
		return "", "", "", fmt.Errorf("create temp chrome dir: %w", err)
	}
	userDataDir = filepath.Join(workDir, "profile")
	if err := os.MkdirAll(userDataDir, 0o700); err != nil {
		_ = os.RemoveAll(workDir)
		return "", "", "", fmt.Errorf("create temp user-data-dir: %w", err)
	}
	htmlPath = filepath.Join(workDir, "input.html")
	if err := os.WriteFile(htmlPath, htmlBytes, 0o600); err != nil {
		_ = os.RemoveAll(workDir)
		return "", "", "", fmt.Errorf("write temp html: %w", err)
	}
	return workDir, htmlPath, userDataDir, nil
}

func buildChromeAllocOpts(chromePath, userDataDir string) []chromedp.ExecAllocatorOption {
	allocOpts := append(slices.Clone(chromedp.DefaultExecAllocatorOptions[:]),
		chromedp.ExecPath(chromePath),
		chromedp.UserDataDir(userDataDir),
		chromedp.Flag("headless", "new"),
		chromedp.DisableGPU,
		chromedp.Flag("mute-audio", true),
		chromedp.Flag("disk-cache-size", "1"),
		chromedp.Flag("media-cache-size", "1"),
		chromedp.Flag("renderer-process-limit", "1"),
	)
	if needsNoSandbox() {
		allocOpts = append(allocOpts, chromedp.NoSandbox)
	}
	return allocOpts
}

type isolatedViewMeasure struct {
	ViewRect       ViewRect `json:"view_rect"`
	NodeBoxes      []Box    `json:"node_boxes"`
	ContainerBoxes []Box    `json:"container_boxes"`
	BadgeBoxes     []Box    `json:"badge_boxes"`
	LabelBoxes     []Box    `json:"label_boxes"`
}

func setChromeViewportMetrics(browserCtx context.Context, width, height int64, scale float64) error {
	_, err := chromedp.Call(browserCtx, emulation.SetDeviceMetricsOverride, emulation.SetDeviceMetricsOverrideParams{
		Width:             width,
		Height:            height,
		DeviceScaleFactor: scale,
		Mobile:            false,
	})
	return err
}

func captureChromePNGs(
	browserCtx context.Context,
	res *BrowserRenderResult,
	out *BrowserOutput,
	onPNG func(filename string, pngData []byte, vr *ViewReport) error,
) error {
	if onPNG == nil {
		out.PNGs = make(map[string][]byte, len(res.ViewRects))
	}
	for i := range res.ViewRects {
		if err := captureSingleViewPNG(browserCtx, i, res, out, onPNG); err != nil {
			return err
		}
	}
	return nil
}

func captureSingleViewPNG(
	browserCtx context.Context,
	idx int,
	res *BrowserRenderResult,
	out *BrowserOutput,
	onPNG func(filename string, pngData []byte, vr *ViewReport) error,
) error {
	isoRect, err := chromedp.Run(browserCtx, chromedp.Evaluate[ViewRect](
		fmt.Sprintf("window.__GCP_ARCH_ISOLATE_VIEW__(%d)", idx),
		chromedp.EvalAwaitPromise,
	))
	if err != nil {
		return fmt.Errorf("isolate view %d: %w", idx, err)
	}

	scale := computeAdaptiveScale(isoRect.Width, isoRect.Height)
	measured, err := chromedp.Run(browserCtx, chromedp.Evaluate[isolatedViewMeasure](
		fmt.Sprintf("window.__GCP_ARCH_MEASURE_ISOLATED_VIEW__(%d)", idx),
		chromedp.EvalAwaitPromise,
	))
	if err != nil {
		return fmt.Errorf("measure isolated view %d: %w", idx, err)
	}

	vr := measured.ViewRect
	res.ViewRects[idx] = vr

	captureBeyond := true
	shot, err := chromedp.Call(browserCtx, page.CaptureScreenshot, page.CaptureScreenshotParams{
		Format:                page.CaptureScreenshotFormatPng,
		CaptureBeyondViewport: &captureBeyond,
		Clip: &page.Viewport{
			X:      vr.X,
			Y:      vr.Y,
			Width:  vr.Width,
			Height: vr.Height,
			Scale:  scale,
		},
	})
	if err != nil {
		return fmt.Errorf("capture screenshot for view %q: %w", vr.ViewID, err)
	}

	// Hide view immediately so Chromium drops its display layer before capturing the next view.
	_, _ = chromedp.Run(browserCtx, chromedp.Evaluate[chromedp.Void](
		fmt.Sprintf("window.__GCP_ARCH_HIDE_VIEW__ && window.__GCP_ARCH_HIDE_VIEW__(%d)", idx),
	))

	filename := fmt.Sprintf("%02d-%s.png", vr.Index, vr.ViewID)
	cfg, err := png.DecodeConfig(bytes.NewReader(shot.Data))
	if err != nil {
		return fmt.Errorf("decode captured png %q: %w", filename, err)
	}

	var rep *ViewReport
	if idx < len(out.Result.ViewReports) {
		rep = &out.Result.ViewReports[idx]
		rep.NodeBoxes = measured.NodeBoxes
		rep.ContainerBoxes = measured.ContainerBoxes
		rep.BadgeBoxes = measured.BadgeBoxes
		rep.LabelBoxes = measured.LabelBoxes
		updateViewReportGeometryFromPNG(rep, vr, cfg.Width, cfg.Height, filepath.ToSlash(filepath.Join("views", filename)))
	}

	if onPNG != nil {
		return onPNG(filename, shot.Data, rep)
	}
	out.PNGs[filename] = shot.Data
	return nil
}

func captureChromePDF(browserCtx context.Context) ([]byte, error) {
	printBG := true
	preferCSS := true
	pdfRes, err := chromedp.Call(browserCtx, page.PrintToPDF, page.PrintToPDFParams{
		PrintBackground:   &printBG,
		PreferCSSPageSize: &preferCSS,
	})
	if err != nil {
		return nil, fmt.Errorf("print to pdf: %w", err)
	}
	return pdfRes.Data, nil
}

// computeAdaptiveScale chooses up to 2.0x Retina DPI while clamping both the maximum raster
// dimension (8,192px ceiling) and total pixel area (28M px) to keep Chromium software
// rasterization RAM low and prevent Skia partial-tile whiteout.
func computeAdaptiveScale(widthDIP, heightDIP float64) float64 {
	scale := maxRasterScale
	if maxDim := math.Max(widthDIP, heightDIP); maxDim > 0 && maxDim*scale > maxRasterDim {
		scale = math.Floor((maxRasterDim/maxDim)*100) / 100
	}
	if area := widthDIP * heightDIP; area > 0 && area*scale*scale > maxRasterPixels {
		areaScale := math.Floor(math.Sqrt(maxRasterPixels/area)*100) / 100
		if areaScale < scale {
			scale = areaScale
		}
	}
	if scale < 1.0 {
		scale = 1.0
	}
	return scale
}

var (
	noSandboxOnce   sync.Once
	noSandboxCached bool
)

// needsNoSandbox enables --no-sandbox strictly inside a Linux container without unprivileged user namespaces.
func needsNoSandbox() bool {
	noSandboxOnce.Do(func() {
		noSandboxCached = detectNeedsNoSandbox()
	})
	return noSandboxCached
}

func detectNeedsNoSandbox() bool {
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
