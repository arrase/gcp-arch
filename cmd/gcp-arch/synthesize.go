package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"google.golang.org/genai"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/icons"
	"github.com/arrase/gcp-arch/internal/manifest"
	"github.com/arrase/gcp-arch/internal/synth"
)

// lazyModelClient defers Vertex AI client initialization until GenerateJSON is actually invoked,
// ensuring Day-2 --reconcile zero-LLM fast paths and >500 scale-limit checks avoid Vertex ADC calls.
type lazyModelClient struct {
	initFn func(ctx context.Context) (synth.ModelClient, error)

	once   sync.Once
	client synth.ModelClient
	err    error
}

func (l *lazyModelClient) GenerateJSON(ctx context.Context, systemPrompt, userPrompt string, schema *genai.Schema) ([]byte, error) {
	l.once.Do(func() {
		l.client, l.err = l.initFn(ctx)
	})
	if l.err != nil {
		return nil, l.err
	}
	return l.client.GenerateJSON(ctx, systemPrompt, userPrompt, schema)
}

type synthesizeCmdOpts struct {
	vertexProject  string
	vertexLocation string
	model          string
	thinkingLevel  string
	reconcilePath  string
	outPath        string
	concurrency    int
	quiet          bool
}

func newSynthesizeCmd(stdout io.Writer, deps appDeps, cfg *appConfig) *cobra.Command {
	var opts synthesizeCmdOpts

	cmd := &cobra.Command{
		Use:   "synthesize <facts.json>",
		Short: "Synthesize or Day-2 reconcile a multi-view architecture.yaml manifest via Vertex AI",
		Long: `Run two-pass closed-world Vertex AI Gemini 4 synthesis over a v3 facts.json file
to produce a Git-tracked architecture.yaml manifest (Day 1), or incrementally reconcile
resource and edge deltas into an existing architecture.yaml (Day 2 via --reconcile).

When --reconcile is used and -o/--out is omitted, updates are written atomically in place;
if zero resource or edge deltas exist, reconcile finishes in < 100 ms with 0 LLM calls
(requiring no --vertex-project) and leaves the existing file untouched.`,
		Example: `  gcp-arch synthesize facts.json --vertex-project consultant-ai-sandbox -o architecture.yaml
  gcp-arch synthesize facts.json --reconcile architecture.yaml --vertex-project consultant-ai-sandbox`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.applyConfigDefaults(cmd, cfg)
			return executeSynthesize(cmd, args[0], stdout, deps, opts)
		},
	}

	cmd.Flags().SortFlags = false
	cmd.Flags().StringVar(&opts.vertexProject, "vertex-project", "", "GCP billing project `ID` for Vertex AI Gemini calls")
	cmd.Flags().StringVar(&opts.vertexLocation, "vertex-location", synth.DefaultVertexLocation, "Vertex AI location")
	cmd.Flags().StringVar(&opts.model, "model", synth.DefaultVertexModel, "Vertex AI model ID")
	cmd.Flags().StringVar(&opts.thinkingLevel, "thinking-level", "", "Vertex AI thinking level (minimal, low, medium, high, xhigh; omit for model default)")
	cmd.Flags().StringVar(&opts.reconcilePath, "reconcile", "", "Existing architecture.yaml `FILE` for Day-2 delta reconciliation")
	cmd.Flags().StringVarP(&opts.outPath, "out", "o", "architecture.yaml", "Output architecture.yaml `FILE`")
	cmd.Flags().IntVar(&opts.concurrency, "concurrency", synth.DefaultConcurrency, "Max concurrent Pass-2 view synthesis calls")
	cmd.Flags().BoolVarP(&opts.quiet, "quiet", "q", false, "Suppress informational stdout summary")
	return cmd
}

func (o *synthesizeCmdOpts) applyConfigDefaults(cmd *cobra.Command, cfg *appConfig) {
	if !cmd.Flags().Changed("vertex-project") && cfg.VertexProject != "" {
		o.vertexProject = cfg.VertexProject
	}
	if !cmd.Flags().Changed("vertex-location") && cfg.VertexLocation != "" {
		o.vertexLocation = cfg.VertexLocation
	}
	if !cmd.Flags().Changed("model") && cfg.Model != "" {
		o.model = cfg.Model
	}
	if !cmd.Flags().Changed("thinking-level") && cfg.ThinkingLevel != "" {
		o.thinkingLevel = cfg.ThinkingLevel
	}
	if !cmd.Flags().Changed("concurrency") && cfg.Concurrency != nil {
		o.concurrency = *cfg.Concurrency
	}
}

func executeSynthesize(cmd *cobra.Command, factsPath string, stdout io.Writer, deps appDeps, opts synthesizeCmdOpts) error {
	if _, err := synth.ParseThinkingLevel(opts.thinkingLevel); err != nil {
		return usageErr(err)
	}
	if opts.concurrency < 1 {
		return usageErr(fmt.Errorf("--concurrency must be >= 1"))
	}

	isReconcile := strings.TrimSpace(opts.reconcilePath) != ""
	vp := strings.TrimSpace(opts.vertexProject)
	if !isReconcile && vp == "" {
		return usageErr(fmt.Errorf("--vertex-project is required when synthesizing a new architecture manifest"))
	}

	targetPath := opts.outPath
	if isReconcile && !cmd.Flags().Changed("out") {
		targetPath = opts.reconcilePath
	}
	if strings.TrimSpace(targetPath) == "" {
		return usageErr(fmt.Errorf("-o/--out path cannot be empty"))
	}

	facts, err := loadFacts(factsPath)
	if err != nil {
		return err
	}
	reg, err := icons.Load()
	if err != nil {
		return runtimeErr(err)
	}

	client := buildLazyModelClient(vp, opts, deps)
	synthOpts := synth.Options{Concurrency: opts.concurrency}
	if !isReconcile {
		return executeDay1Synthesize(cmd.Context(), stdout, facts, client, reg, synthOpts, targetPath, opts.quiet)
	}
	return executeDay2Reconcile(cmd.Context(), stdout, facts, client, reg, synthOpts, opts.reconcilePath, targetPath, opts.quiet)
}

func buildLazyModelClient(vertexProject string, opts synthesizeCmdOpts, deps appDeps) synth.ModelClient {
	if vertexProject == "" {
		return nil
	}
	return &lazyModelClient{
		initFn: func(ctx context.Context) (synth.ModelClient, error) {
			return deps.newModelClient(ctx, synth.VertexClientOptions{
				Project:       vertexProject,
				Location:      opts.vertexLocation,
				Model:         opts.model,
				ThinkingLevel: opts.thinkingLevel,
			})
		},
	}
}

func executeDay1Synthesize(
	ctx context.Context,
	stdout io.Writer,
	facts *extract.FactsFile,
	client synth.ModelClient,
	reg *icons.Registry,
	synthOpts synth.Options,
	targetPath string,
	quiet bool,
) error {
	result, err := synth.Synthesize(ctx, facts, client, reg, synthOpts)
	if err != nil {
		return classifySynthError(err)
	}
	if err := manifest.Write(targetPath, result); err != nil {
		return runtimeErr(err)
	}
	if !quiet {
		_, _ = fmt.Fprintf(stdout, "Wrote %s (%d views, %d components)\n", targetPath, len(result.Views), len(result.Components))
	}
	return nil
}

func executeDay2Reconcile(
	ctx context.Context,
	stdout io.Writer,
	facts *extract.FactsFile,
	client synth.ModelClient,
	reg *icons.Registry,
	synthOpts synth.Options,
	reconcilePath, targetPath string,
	quiet bool,
) error {
	existing, err := loadManifest(reconcilePath)
	if err != nil {
		return err
	}

	reconciled, err := synth.Reconcile(ctx, facts, existing, client, reg, synthOpts)
	if err != nil {
		var sle *synth.ScaleLimitError
		var ve *manifest.ValidationError
		if errors.As(err, &sle) || errors.As(err, &ve) {
			return err
		}
		if client == nil {
			return usageErr(fmt.Errorf("%w (pass --vertex-project)", err))
		}
		return runtimeErr(err)
	}

	unchanged, err := manifestsEqualInPlace(existing, reconciled, reconcilePath, targetPath)
	if err != nil {
		return runtimeErr(err)
	}
	if unchanged {
		if !quiet {
			_, _ = fmt.Fprintf(stdout, "Unchanged %s (0 deltas)\n", reconcilePath)
		}
		return nil
	}

	if err := manifest.Write(targetPath, reconciled); err != nil {
		return runtimeErr(err)
	}
	if !quiet {
		_, _ = fmt.Fprintf(stdout, "Reconciled %s (%d views, %d components)\n", targetPath, len(reconciled.Views), len(reconciled.Components))
	}
	return nil
}

func manifestsEqualInPlace(existing, reconciled *manifest.Manifest, reconcilePath, targetPath string) (bool, error) {
	if filepath.Clean(targetPath) != filepath.Clean(reconcilePath) {
		return false, nil
	}
	beforeBytes, err := manifest.Marshal(existing)
	if err != nil {
		return false, err
	}
	afterBytes, err := manifest.Marshal(reconciled)
	if err != nil {
		return false, err
	}
	return bytes.Equal(beforeBytes, afterBytes), nil
}

func classifySynthError(err error) error {
	var sle *synth.ScaleLimitError
	if errors.As(err, &sle) {
		return err
	}
	var ve *manifest.ValidationError
	if errors.As(err, &ve) {
		return err
	}
	return runtimeErr(err)
}
