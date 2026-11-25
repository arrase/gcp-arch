package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/arrase/gcp-arch/internal/icons"
	"github.com/arrase/gcp-arch/internal/ioutil"
	"github.com/arrase/gcp-arch/internal/manifest"
	"github.com/arrase/gcp-arch/internal/render"
)

const wroteLineFmt = "Wrote %s\n"

type renderCmdOpts struct {
	formatSpec string
	outDir     string
	chromePath string
	quiet      bool
}

type renderContext struct {
	stdout io.Writer
	stderr io.Writer
	deps   appDeps
	opts   renderCmdOpts
}

type renderFormats struct {
	list     []render.Format
	wantPDF  bool
	wantPNG  bool
	wantHTML bool
}

func (f renderFormats) htmlOnly() bool {
	return f.wantHTML && !f.wantPDF && !f.wantPNG
}

func newRenderCmd(stdout, stderr io.Writer, deps appDeps, cfg *appConfig) *cobra.Command {
	var opts renderCmdOpts

	cmd := &cobra.Command{
		Use:   "render <architecture.yaml>",
		Short: "Render architecture.yaml offline into vector PDF, 3x PNG, and standalone HTML",
		Long: `Validate architecture.yaml structurally and render vector architecture.pdf,
3x high-resolution per-view PNGs (out/views/00..NN-<view_id>.png), and standalone
offline architecture.html using local headless Chrome/Chromium with automated
zero-collision verification.

Requires no facts.json and makes zero external network calls. When --format html
is requested alone and Chrome/Chromium is absent, writes self-contained HTML with
the embedded JS router and prints a notice on stderr that CLI collision checks
were skipped.`,
		Example: `  gcp-arch render architecture.yaml --format pdf,png,html -o out/
  gcp-arch render architecture.yaml --format html -o examples/platform-cicd/`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("format") && cfg.Format != nil {
				opts.formatSpec = strings.Join(cfg.Format, ",")
			}
			if !cmd.Flags().Changed("chrome-path") && strings.TrimSpace(os.Getenv("CHROME_PATH")) == "" && cfg.ChromePath != "" {
				opts.chromePath = cfg.ChromePath
			}
			rc := renderContext{stdout: stdout, stderr: stderr, deps: deps, opts: opts}
			return rc.execute(cmd.Context(), args[0])
		},
	}

	cmd.Flags().SortFlags = false
	cmd.Flags().StringVar(&opts.formatSpec, "format", "pdf,png,html", "Comma-separated output formats (pdf, png, html)")
	cmd.Flags().StringVarP(&opts.outDir, "out", "o", "out", "Output directory `DIR`")
	cmd.Flags().StringVar(&opts.chromePath, "chrome-path", "", "Explicit path to Chrome or Chromium `BINARY`")
	cmd.Flags().BoolVarP(&opts.quiet, "quiet", "q", false, "Suppress informational stdout summary")
	return cmd
}

func (rc renderContext) execute(ctx context.Context, manifestPath string) error {
	formats, err := parseRenderFormats(rc.opts.formatSpec)
	if err != nil {
		return err
	}
	if strings.TrimSpace(rc.opts.outDir) == "" {
		return usageErr(fmt.Errorf("-o/--out directory cannot be empty"))
	}

	m, err := loadManifest(manifestPath)
	if err != nil {
		return err
	}
	reg, err := icons.Load()
	if err != nil {
		return runtimeErr(err)
	}
	if err := manifest.ValidateStructure(m, reg); err != nil {
		return err
	}

	runner, fallbackDone, err := rc.resolveRunner(m, reg, formats.htmlOnly())
	if err != nil || fallbackDone {
		return err
	}

	summary, err := render.Render(ctx, m, reg, render.RenderOptions{
		Formats:       formats.list,
		OutputDir:     rc.opts.outDir,
		ChromePath:    rc.opts.chromePath,
		Runner:        runner,
		WarningWriter: rc.stderr,
	})
	if err != nil {
		return classifyRenderError(err)
	}

	if !rc.opts.quiet {
		writeRenderSummary(rc.stdout, summary)
	}
	return nil
}

func (rc renderContext) resolveRunner(m *manifest.Manifest, reg *icons.Registry, htmlOnly bool) (render.BrowserRunner, bool, error) {
	if rc.deps.newBrowserRunner == nil {
		return nil, false, nil
	}
	r, findErr := rc.deps.newBrowserRunner(rc.opts.chromePath)
	if findErr == nil {
		return r, false, nil
	}
	if !htmlOnly {
		return nil, false, runtimeErr(fmt.Errorf("Chrome/Chromium binary required for pdf/png output: %w", findErr))
	}
	rawHTML, htmlErr := render.BuildHTML(m, reg)
	if htmlErr != nil {
		return nil, false, runtimeErr(htmlErr)
	}
	htmlPath := filepath.Join(rc.opts.outDir, "architecture.html")
	if writeErr := ioutil.WriteFileAtomic(htmlPath, rawHTML, 0o644, 0o755); writeErr != nil {
		return nil, false, runtimeErr(writeErr)
	}
	_, _ = fmt.Fprintf(rc.stderr, "warning: Chrome/Chromium not found (%v); wrote self-contained %s without CLI collision verification\n", findErr, htmlPath)
	if !rc.opts.quiet {
		_, _ = fmt.Fprintf(rc.stdout, wroteLineFmt, htmlPath)
	}
	return nil, true, nil
}

func classifyRenderError(err error) error {
	var ve *manifest.ValidationError
	var lce *render.LayoutCollisionError
	if errors.As(err, &ve) || errors.As(err, &lce) {
		return err
	}
	return runtimeErr(err)
}

func parseRenderFormats(raw string) (renderFormats, error) {
	if strings.TrimSpace(raw) == "" {
		return renderFormats{}, usageErr(fmt.Errorf("--format cannot be empty (allowed: pdf, png, html)"))
	}
	var out renderFormats
	seen := make(map[render.Format]bool, 3)
	for _, part := range strings.Split(raw, ",") {
		f := render.Format(strings.ToLower(strings.TrimSpace(part)))
		switch f {
		case render.FormatPDF:
			out.wantPDF = true
		case render.FormatPNG:
			out.wantPNG = true
		case render.FormatHTML:
			out.wantHTML = true
		default:
			return renderFormats{}, usageErr(fmt.Errorf("unsupported --format value %q (allowed: pdf, png, html)", part))
		}
		if !seen[f] {
			seen[f] = true
			out.list = append(out.list, f)
		}
	}
	return out, nil
}

func writeRenderSummary(w io.Writer, summary *render.RenderSummary) {
	if summary.PDFPath != "" {
		_, _ = fmt.Fprintf(w, wroteLineFmt, summary.PDFPath)
	}
	if len(summary.PNGPaths) > 0 {
		_, _ = fmt.Fprintf(w, "Wrote %d view PNG(s) in %s\n", len(summary.PNGPaths), filepath.Dir(summary.PNGPaths[0]))
	}
	if summary.HTMLPath != "" {
		_, _ = fmt.Fprintf(w, wroteLineFmt, summary.HTMLPath)
	}
}
