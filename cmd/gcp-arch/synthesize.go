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
	"time"

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

func (l *lazyModelClient) GenerateJSONWithUsage(ctx context.Context, systemPrompt, userPrompt string, schema *genai.Schema) ([]byte, synth.UsageStats, error) {
	l.once.Do(func() {
		l.client, l.err = l.initFn(ctx)
	})
	if l.err != nil {
		return nil, synth.UsageStats{}, l.err
	}
	if uc, ok := l.client.(synth.UsageModelClient); ok {
		return uc.GenerateJSONWithUsage(ctx, systemPrompt, userPrompt, schema)
	}
	start := time.Now()
	raw, err := l.client.GenerateJSON(ctx, systemPrompt, userPrompt, schema)
	return raw, synth.UsageStats{DurationMs: time.Since(start).Milliseconds(), ResponseBytes: len(raw)}, err
}

type synthesizeCmdOpts struct {
	vertexProject  string
	vertexLocation string
	model          string
	thinkingLevel  string
	reconcilePath  string
	outPath        string
	quiet          bool
}

func newSynthesizeCmd(stdout io.Writer, deps appDeps, cfg *appConfig) *cobra.Command {
	var opts synthesizeCmdOpts

	cmd := &cobra.Command{
		Use:   "synthesize <facts.json>",
		Short: "Synthesize or Day-2 reconcile a multi-view architecture.yaml manifest via Vertex AI",
		Long: `Build a deterministic GCP containment skeleton from a v3 facts.json file and
enrich it via bounded Vertex AI Gemini 4 annotation into a Git-tracked
architecture.yaml manifest (Day 1), or incrementally reconcile resource and edge
deltas into an existing architecture.yaml (Day 2 via --reconcile).

When --reconcile is used and -o/--out is omitted, updates are written atomically in place;
if zero resource or edge deltas exist, reconcile finishes with 0 LLM calls (requiring no
--vertex-project) and leaves the existing file untouched.`,
		Example: `  gcp-arch synthesize facts.json --vertex-project my-vertex-project -o architecture.yaml
  gcp-arch synthesize facts.json --reconcile architecture.yaml --vertex-project my-vertex-project`,
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
}

func executeSynthesize(cmd *cobra.Command, factsPath string, stdout io.Writer, deps appDeps, opts synthesizeCmdOpts) error {
	if _, err := synth.ParseThinkingLevel(opts.thinkingLevel); err != nil {
		return usageErr(err)
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

	sr := synthesizeRun{
		stdout:        stdout,
		facts:         facts,
		client:        buildLazyModelClient(vp, opts, deps),
		reg:           reg,
		reconcilePath: opts.reconcilePath,
		targetPath:    targetPath,
		quiet:         opts.quiet,
	}
	if !isReconcile {
		return sr.executeDay1(cmd.Context())
	}
	return sr.executeDay2(cmd.Context())
}

type synthesizeRun struct {
	stdout        io.Writer
	facts         *extract.FactsFile
	client        synth.ModelClient
	reg           *icons.Registry
	reconcilePath string
	targetPath    string
	quiet         bool
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

func (r synthesizeRun) executeDay1(ctx context.Context) error {
	res, err := synth.Synthesize(ctx, r.facts, r.client, r.reg)
	if err != nil {
		return classifySynthError(err, false)
	}
	if err := manifest.Write(r.targetPath, res.Manifest); err != nil {
		return runtimeErr(err)
	}
	if !r.quiet {
		_, _ = fmt.Fprintf(
			r.stdout,
			"Wrote %s (%d views, %d components, conservation=%s primary=%d/%d, tokens=%d/%d/%d, duration=%dms)\n",
			r.targetPath, len(res.Manifest.Views), len(res.Manifest.Components), res.Conservation.Status,
			res.Conservation.RepresentedPrimaryCount, res.Conservation.FactsPrimaryCount,
			res.Usage.PromptTokens, res.Usage.CandidateTokens, res.Usage.TotalTokens, res.Usage.DurationMs,
		)
	}
	return nil
}

func (r synthesizeRun) executeDay2(ctx context.Context) error {
	existing, err := loadManifest(r.reconcilePath)
	if err != nil {
		return err
	}

	reconciled, err := synth.Reconcile(ctx, r.facts, existing, r.client, r.reg)
	if err != nil {
		return classifySynthError(err, r.client == nil)
	}

	unchanged, err := manifestsEqualInPlace(existing, reconciled, r.reconcilePath, r.targetPath)
	if err != nil {
		return runtimeErr(err)
	}
	if unchanged {
		if !r.quiet {
			_, _ = fmt.Fprintf(r.stdout, "Unchanged %s (0 deltas)\n", r.reconcilePath)
		}
		return nil
	}

	if err := manifest.Write(r.targetPath, reconciled); err != nil {
		return runtimeErr(err)
	}
	if !r.quiet {
		_, _ = fmt.Fprintf(r.stdout, "Reconciled %s (%d views, %d components)\n", r.targetPath, len(reconciled.Views), len(reconciled.Components))
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

func classifySynthError(err error, missingClient bool) error {
	var sle *synth.ScaleLimitError
	var ve *manifest.ValidationError
	if errors.As(err, &sle) || errors.As(err, &ve) {
		return err
	}
	if missingClient {
		return usageErr(fmt.Errorf("%w (pass --vertex-project)", err))
	}
	return runtimeErr(err)
}
