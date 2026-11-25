package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/manifest"
	"github.com/arrase/gcp-arch/internal/render"
	"github.com/arrase/gcp-arch/internal/synth"
)

const (
	exitOK      = 0
	exitRuntime = 1
	exitUsage   = 2
)

var version = "dev"

type cliError struct {
	code int
	err  error
}

func (e *cliError) Error() string { return e.err.Error() }
func (e *cliError) Unwrap() error { return e.err }

func usageErr(err error) error {
	return &cliError{code: exitUsage, err: err}
}

func runtimeErr(err error) error {
	return &cliError{code: exitRuntime, err: err}
}

// appDeps injects live GCP, Vertex AI, config loading, and headless browser hooks so tests run deterministically offline.
type appDeps struct {
	loadConfig        func() (appConfig, error)
	newExtractClients func(ctx context.Context, opts extract.ClientOptions) (*extract.Clients, error)
	runPreflight      func(ctx context.Context, clients *extract.Clients, scopes []string, vertexOpts extract.VertexPreflightOptions) (*extract.PreflightReport, error)
	probeVertexModel  func(ctx context.Context, project, location, model string) error
	extractFacts      func(ctx context.Context, clients *extract.Clients, opts extract.ExtractOptions) (*extract.FactsFile, error)
	newModelClient    func(ctx context.Context, opts synth.VertexClientOptions) (synth.ModelClient, error)
	newBrowserRunner  func(chromePath string) (render.BrowserRunner, error)
}

func defaultDeps() appDeps {
	return appDeps{
		loadConfig:        loadDefaultConfig,
		newExtractClients: extract.NewClients,
		runPreflight:      extract.RunPreflight,
		probeVertexModel:  synth.ProbeVertexModel,
		extractFacts:      extract.Extract,
		newModelClient: func(ctx context.Context, opts synth.VertexClientOptions) (synth.ModelClient, error) {
			return synth.NewVertexClient(ctx, opts)
		},
		newBrowserRunner: func(chromePath string) (render.BrowserRunner, error) {
			found, err := render.FindChrome(chromePath)
			if err != nil {
				return nil, err
			}
			return &render.ChromeDPRunner{ChromePath: found}, nil
		},
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, defaultDeps()))
}

func run(args []string, stdout, stderr io.Writer, deps appDeps) int {
	return runWithContext(context.Background(), args, stdout, stderr, deps)
}

func runWithContext(ctx context.Context, args []string, stdout, stderr io.Writer, deps appDeps) int {
	root := newRootCmd(stdout, stderr, deps)
	root.SetArgs(args)

	if err := root.ExecuteContext(ctx); err != nil {
		code := classifyExitCode(err)
		_, _ = fmt.Fprintf(stderr, "Error: %v\n", err)
		return code
	}
	return exitOK
}

func newRootCmd(stdout, stderr io.Writer, deps appDeps) *cobra.Command {
	var cfg appConfig

	root := &cobra.Command{
		Use:     "gcp-arch",
		Version: version,
		Short:   "Extract GCP architecture facts, synthesize multi-view manifests, and render PDF/PNG/HTML diagrams",
		Long: `Three-stage semantic multi-view GCP architecture pipeline:
  1. extract     Capture read-only multi-project facts into facts.json (v3, 0600)
  2. synthesize  Decompose facts via Vertex AI Gemini 4 into Git-tracked architecture.yaml (v1)
  3. validate    Verify architecture.yaml structure offline or with --facts provenance
  4. render      Export vector PDF, 3x PNG, and standalone HTML via headless Chrome

Optional defaults are read from ~/.config/gcp-arch/config.yaml
(or $XDG_CONFIG_HOME/gcp-arch/config.yaml) when present.
Precedence: CLI flags > environment variables > config.yaml > built-in defaults.

Exit Codes:
  0  Success
  1  Runtime or external I/O failure (GCP auth/quota, Vertex AI RPC, missing Chrome for pdf/png)
  2  CLI usage error, > 500 active-resource cap overflow, schema/validation error, or layout collision`,
		SilenceUsage:      true,
		SilenceErrors:     true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			if deps.loadConfig == nil {
				return nil
			}
			loaded, err := deps.loadConfig()
			if err != nil {
				return err
			}
			cfg = loaded
			return nil
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)

	root.AddCommand(
		newPreflightCmd(stdout, stderr, deps, &cfg),
		newExtractCmd(stdout, stderr, deps, &cfg),
		newSynthesizeCmd(stdout, deps, &cfg),
		newValidateCmd(stdout),
		newRenderCmd(stdout, stderr, deps, &cfg),
	)
	return root
}

func classifyExitCode(err error) int {
	if err == nil {
		return exitOK
	}
	var ve *manifest.ValidationError
	if errors.As(err, &ve) {
		return exitUsage
	}
	var sle *synth.ScaleLimitError
	if errors.As(err, &sle) {
		return exitUsage
	}
	var lce *render.LayoutCollisionError
	if errors.As(err, &lce) {
		return exitUsage
	}
	var ce *cliError
	if errors.As(err, &ce) {
		return ce.code
	}
	// Errors not wrapped by RunE come from Cobra flag/argument validation.
	return exitUsage
}

type scopeFlags struct {
	orgs     []string
	folders  []string
	projects []string
}

func (f *scopeFlags) register(cmd *cobra.Command) {
	cmd.Flags().SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		if name == "project" {
			return "projects"
		}
		return pflag.NormalizedName(name)
	})
	cmd.Flags().StringArrayVar(&f.orgs, "org", nil, "GCP Organization `ID`(s) (repeatable or comma-separated)")
	cmd.Flags().StringArrayVar(&f.folders, "folder", nil, "GCP Folder `ID`(s) (repeatable or comma-separated)")
	cmd.Flags().StringArrayVar(&f.projects, "projects", nil, "GCP Project `ID`(s) (repeatable or comma-separated; alias --project)")
}

func (f *scopeFlags) anyChanged(cmd *cobra.Command) bool {
	return cmd.Flags().Changed("org") || cmd.Flags().Changed("folder") || cmd.Flags().Changed("projects")
}

func (f *scopeFlags) applyConfig(cfg *appConfig) {
	f.orgs = append([]string(nil), cfg.Org...)
	f.folders = append([]string(nil), cfg.Folder...)
	f.projects = append([]string(nil), cfg.Projects...)
}

type parsedScopes struct {
	orgs      []string
	folders   []string
	projects  []string
	canonical []string
}

func (p parsedScopes) empty() bool {
	return len(p.canonical) == 0
}

func (f *scopeFlags) parse() (parsedScopes, error) {
	var out parsedScopes
	seenCanon := make(map[string]struct{})

	if err := out.appendKind(f.orgs, "org", "organizations/", &out.orgs, seenCanon); err != nil {
		return out, err
	}
	if err := out.appendKind(f.folders, "folder", "folders/", &out.folders, seenCanon); err != nil {
		return out, err
	}
	if err := out.appendKind(f.projects, "projects", "projects/", &out.projects, seenCanon); err != nil {
		return out, err
	}
	return out, nil
}

func (p *parsedScopes) appendKind(rawVals []string, flagName, prefix string, target *[]string, seenCanon map[string]struct{}) error {
	seenID := make(map[string]struct{})
	for _, raw := range rawVals {
		for _, part := range strings.Split(raw, ",") {
			id, err := parseScopeToken(part, flagName, prefix)
			if err != nil {
				return err
			}
			appendUnique(target, seenID, id)
			appendUnique(&p.canonical, seenCanon, prefix+id)
		}
	}
	return nil
}

func parseScopeToken(part, flagName, prefix string) (string, error) {
	v := strings.TrimSpace(part)
	if v == "" {
		return "", usageErr(fmt.Errorf("empty value provided to --%s", flagName))
	}
	id := strings.TrimPrefix(v, prefix)
	if id == "" || strings.ContainsAny(id, "/ \t\r\n") {
		return "", usageErr(fmt.Errorf("invalid --%s value %q", flagName, v))
	}
	return id, nil
}

func appendUnique(dst *[]string, seen map[string]struct{}, val string) {
	if _, dup := seen[val]; dup {
		return
	}
	seen[val] = struct{}{}
	*dst = append(*dst, val)
}

func parseProjectAliases(raw []string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	aliases := make(map[string]string, len(raw))
	for _, entry := range raw {
		num, id, ok := strings.Cut(entry, "=")
		num = strings.TrimSpace(num)
		id = strings.TrimSpace(id)
		if !ok || !isAllDigits(num) || !isValidProjectID(id) {
			return nil, usageErr(fmt.Errorf("invalid --project-alias %q (expected <NUMBER=ID>)", entry))
		}
		if prev, exists := aliases[num]; exists && prev != id {
			return nil, usageErr(fmt.Errorf("conflicting --project-alias for project number %s (%q vs %q)", num, prev, id))
		}
		aliases[num] = id
	}
	return aliases, nil
}

func mergeProjectAliases(base map[string]string, rawCLI []string) (map[string]string, error) {
	cliAliases, err := parseProjectAliases(rawCLI)
	if err != nil {
		return nil, err
	}
	if len(base) == 0 {
		return cliAliases, nil
	}
	merged := make(map[string]string, len(base)+len(cliAliases))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range cliAliases {
		merged[k] = v
	}
	return merged, nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isValidProjectID(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		isAlphaNum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if i == 0 && !isAlphaNum {
			return false
		}
		if !isAlphaNum && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func loadFacts(path string) (*extract.FactsFile, error) {
	f, err := extract.LoadFacts(path)
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return nil, runtimeErr(err)
		}
		return nil, usageErr(err)
	}
	return f, nil
}

func loadManifest(path string) (*manifest.Manifest, error) {
	m, err := manifest.Load(path)
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return nil, runtimeErr(err)
		}
		return nil, usageErr(err)
	}
	return m, nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
