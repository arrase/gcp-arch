package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/arrase/gcp-arch/internal/extract"
)

// PreflightReport is the JSON diagnostic schema emitted by `gcp-arch preflight --json`.
type PreflightReport struct {
	QuotaProject string                   `json:"quota_project,omitempty"`
	Scopes       []string                 `json:"scopes"`
	GrantedCount int                      `json:"granted_count"`
	MissingCount int                      `json:"missing_count"`
	Probes       []extract.PreflightProbe `json:"probes"`
	FatalError   string                   `json:"fatal_error,omitempty"`
}

type preflightCmdOpts struct {
	scopes         scopeFlags
	projectAliases []string
	quotaProject   string
	vertexProject  string
	vertexLocation string
	model          string
	quiet          bool
	asJSON         bool
}

func newPreflightCmd(stdout, stderr io.Writer, deps appDeps, cfg *appConfig) *cobra.Command {
	var opts preflightCmdOpts

	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "Probe read-only target GCP permissions and optional Vertex AI readiness",
		Long: `Probe target-scope read-only permissions (Cloud Asset Inventory, Cloud Monitoring,
BigQuery jobs.list, Dataflow jobs.list) using cloud-platform.read-only credentials.
When --vertex-project is supplied, also run a zero-token Vertex AI IAM and model
readiness check using cloud-platform credentials.

At least one target scope (--org, --folder, or --projects) is required.`,
		Example: `  gcp-arch preflight --projects acme-cicd --vertex-project consultant-ai-sandbox
  gcp-arch preflight --folder 555000111 --quota-project my-quota-proj --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.applyConfigDefaults(cmd, cfg)
			return executePreflight(cmd, stdout, stderr, deps, cfg, opts)
		},
	}

	cmd.Flags().SortFlags = false
	opts.scopes.register(cmd)
	cmd.Flags().StringArrayVar(&opts.projectAliases, "project-alias", nil, "Map project number to ID (`NUM=ID`, repeatable)")
	cmd.Flags().StringVar(&opts.quotaProject, "quota-project", "", "Explicit GCP quota project `ID` (defaults to ADC quota_project_id)")
	cmd.Flags().StringVar(&opts.vertexProject, "vertex-project", "", "GCP billing project `ID` for zero-token Vertex AI readiness probe")
	cmd.Flags().StringVar(&opts.vertexLocation, "vertex-location", extract.DefaultVertexLocation, "Vertex AI location for model readiness probe")
	cmd.Flags().StringVar(&opts.model, "model", extract.DefaultVertexModel, "Vertex AI model ID for readiness probe")
	cmd.Flags().BoolVarP(&opts.quiet, "quiet", "q", false, "Suppress progress output on stderr")
	cmd.Flags().BoolVar(&opts.asJSON, "json", false, "Emit PreflightReport as JSON")
	return cmd
}

func (o *preflightCmdOpts) applyConfigDefaults(cmd *cobra.Command, cfg *appConfig) {
	if !o.scopes.anyChanged(cmd) {
		o.scopes.applyConfig(cfg)
	}
	if !cmd.Flags().Changed("quota-project") && cfg.QuotaProject != "" {
		o.quotaProject = cfg.QuotaProject
	}
	if !cmd.Flags().Changed("vertex-project") && cfg.VertexProject != "" {
		o.vertexProject = cfg.VertexProject
	}
	if !cmd.Flags().Changed("vertex-location") && cfg.VertexLocation != "" {
		o.vertexLocation = cfg.VertexLocation
	}
	if !cmd.Flags().Changed("model") && cfg.Model != "" {
		o.model = cfg.Model
	}
}

func executePreflight(cmd *cobra.Command, stdout, stderr io.Writer, deps appDeps, cfg *appConfig, opts preflightCmdOpts) error {
	parsed, err := opts.scopes.parse()
	if err != nil {
		return err
	}
	if parsed.empty() {
		return usageErr(fmt.Errorf("at least one of --org, --folder, or --projects is required"))
	}

	aliases, err := mergeProjectAliases(cfg.ProjectAliases, opts.projectAliases)
	if err != nil {
		return err
	}

	progressW := stderr
	if opts.quiet {
		progressW = io.Discard
	}

	ctx := cmd.Context()
	clients, err := deps.newExtractClients(ctx, extract.ClientOptions{
		QuotaProject:   opts.quotaProject,
		ProjectAliases: aliases,
		ProgressWriter: progressW,
	})
	if err != nil {
		return runtimeErr(err)
	}
	if clients != nil {
		defer func() { _ = clients.Close() }()
	}

	var vertexOpts extract.VertexPreflightOptions
	if vp := strings.TrimSpace(opts.vertexProject); vp != "" {
		vertexOpts = extract.VertexPreflightOptions{
			Project:    vp,
			Location:   opts.vertexLocation,
			Model:      opts.model,
			ModelProbe: deps.probeVertexModel,
		}
	}

	rawReport, probeErr := deps.runPreflight(ctx, clients, parsed.canonical, vertexOpts)
	if rawReport != nil {
		report := buildPreflightReport(rawReport)
		if opts.asJSON {
			if err := writeJSON(stdout, report); err != nil {
				return runtimeErr(err)
			}
		} else {
			writePreflightTable(stdout, report)
		}
	}

	if probeErr != nil {
		return runtimeErr(probeErr)
	}
	return nil
}

func buildPreflightReport(raw *extract.PreflightReport) PreflightReport {
	report := PreflightReport{
		QuotaProject: raw.QuotaProject,
		Scopes:       append([]string(nil), raw.Scopes...),
		Probes:       append([]extract.PreflightProbe(nil), raw.Probes...),
		FatalError:   raw.FatalError,
	}
	if report.Scopes == nil {
		report.Scopes = []string{}
	}
	if report.Probes == nil {
		report.Probes = []extract.PreflightProbe{}
	}
	for _, p := range report.Probes {
		if p.Status == extract.ProbeGranted {
			report.GrantedCount++
		} else {
			report.MissingCount++
		}
	}
	return report
}

func writePreflightTable(w io.Writer, report PreflightReport) {
	if report.QuotaProject != "" {
		_, _ = fmt.Fprintf(w, "Quota Project: %s\n", report.QuotaProject)
	}
	_, _ = fmt.Fprintf(w, "Scopes: %s\n", strings.Join(report.Scopes, ", "))
	_, _ = fmt.Fprintf(w, "Summary: %d granted, %d missing/degraded\n\n", report.GrantedCount, report.MissingCount)

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SCOPE\tAPI\tSTATUS\tREQUIRED ROLE\tDETAIL")
	var remediations []string
	seenRem := make(map[string]struct{})
	for _, p := range report.Probes {
		detail := p.Detail
		if p.VPCSCUUID != "" {
			if detail != "" {
				detail += " "
			}
			detail += "[vpc_sc_uuid=" + p.VPCSCUUID + "]"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.Scope, p.Source, p.Status, p.RequiredRole, detail)
		if p.Status != extract.ProbeGranted && p.Remediation != "" {
			if _, dup := seenRem[p.Remediation]; !dup {
				seenRem[p.Remediation] = struct{}{}
				remediations = append(remediations, p.Remediation)
			}
		}
	}
	_ = tw.Flush()

	if len(remediations) > 0 {
		_, _ = fmt.Fprintln(w, "\nRemediation:")
		for _, r := range remediations {
			_, _ = fmt.Fprintf(w, "  %s\n", r)
		}
	}
}
