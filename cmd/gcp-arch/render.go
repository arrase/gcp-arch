package main

import (
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

type renderCmdOpts struct {
	formatSpec string
	outDir     string
	chromePath string
	quiet      bool
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
			return executeRender(cmd, args[0], stdout, stderr, deps, opts)
		},
	}

	cmd.Flags().SortFlags = false
	cmd.Flags().StringVar(&opts.formatSpec, "format", "pdf,png,html", "Comma-separated output formats (pdf, png, html)")
	cmd.Flags().StringVarP(&opts.outDir, "out", "o", "out", "Output directory `DIR`")
	cmd.Flags().StringVar(&opts.chromePath, "chrome-path", "", "Explicit path to Chrome or Chromium `BINARY`")
	cmd.Flags().BoolVarP(&opts.quiet, "quiet", "q", false, "Suppress informational stdout summary")
	return cmd
}

func executeRender(cmd *cobra.Command, manifestPath string, stdout, stderr io.Writer, deps appDeps, opts renderCmdOpts) error {
	formats, wantPDF, wantPNG, wantHTML, err := parseRenderFormats(opts.formatSpec)
	if err != nil {
		return err
	}
	if strings.TrimSpace(opts.outDir) == "" {
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

	runner, fallbackDone, err := resolveRenderRunner(m, reg, stdout, stderr, deps, opts, wantPDF, wantPNG, wantHTML)
	if err != nil || fallbackDone {
		return err
	}

	summary, err := render.Render(cmd.Context(), m, reg, render.RenderOptions{
		Formats:       formats,
		OutputDir:     opts.outDir,
		ChromePath:    opts.chromePath,
		Runner:        runner,
		WarningWriter: stderr,
	})
	if err != nil {
		var ve *manifest.ValidationError
		var lce *render.LayoutCollisionError
		if errors.As(err, &ve) || errors.As(err, &lce) {
			return err
		}
		return runtimeErr(err)
	}

	if !opts.quiet {
		writeRenderSummary(stdout, summary)
	}
	return nil
}

func resolveRenderRunner(
	m *manifest.Manifest,
	reg *icons.Registry,
	stdout, stderr io.Writer,
	deps appDeps,
	opts renderCmdOpts,
	wantPDF, wantPNG, wantHTML bool,
) (render.BrowserRunner, bool, error) {
	if deps.newBrowserRunner == nil {
		return nil, false, nil
	}
	r, findErr := deps.newBrowserRunner(opts.chromePath)
	if findErr == nil {
		return r, false, nil
	}
	if !wantHTML || wantPDF || wantPNG {
		return nil, false, runtimeErr(fmt.Errorf("Chrome/Chromium binary required for pdf/png output: %w", findErr))
	}
	rawHTML, htmlErr := render.BuildHTML(m, reg)
	if htmlErr != nil {
		return nil, false, runtimeErr(htmlErr)
	}
	htmlPath := filepath.Join(opts.outDir, "architecture.html")
	if writeErr := ioutil.WriteFileAtomic(htmlPath, rawHTML, 0o644, 0o755); writeErr != nil {
		return nil, false, runtimeErr(writeErr)
	}
	_, _ = fmt.Fprintf(stderr, "warning: Chrome/Chromium not found (%v); wrote self-contained %s without CLI collision verification\n", findErr, htmlPath)
	if !opts.quiet {
		_, _ = fmt.Fprintf(stdout, "Wrote %s\n", htmlPath)
	}
	return nil, true, nil
}

func parseRenderFormats(raw string) (formats []render.Format, wantPDF, wantPNG, wantHTML bool, err error) {
	if strings.TrimSpace(raw) == "" {
		return nil, false, false, false, usageErr(fmt.Errorf("--format cannot be empty (allowed: pdf, png, html)"))
	}
	seen := make(map[render.Format]bool, 3)
	for _, part := range strings.Split(raw, ",") {
		f := render.Format(strings.ToLower(strings.TrimSpace(part)))
		switch f {
		case render.FormatPDF:
			wantPDF = true
		case render.FormatPNG:
			wantPNG = true
		case render.FormatHTML:
			wantHTML = true
		default:
			return nil, false, false, false, usageErr(fmt.Errorf("unsupported --format value %q (allowed: pdf, png, html)", part))
		}
		if !seen[f] {
			seen[f] = true
			formats = append(formats, f)
		}
	}
	return formats, wantPDF, wantPNG, wantHTML, nil
}

func writeRenderSummary(w io.Writer, summary *render.RenderSummary) {
	if summary.PDFPath != "" {
		_, _ = fmt.Fprintf(w, "Wrote %s\n", summary.PDFPath)
	}
	if len(summary.PNGPaths) > 0 {
		_, _ = fmt.Fprintf(w, "Wrote %d view PNG(s) in %s\n", len(summary.PNGPaths), filepath.Dir(summary.PNGPaths[0]))
	}
	if summary.HTMLPath != "" {
		_, _ = fmt.Fprintf(w, "Wrote %s\n", summary.HTMLPath)
	}
}
