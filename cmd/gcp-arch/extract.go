package main

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/arrase/gcp-arch/internal/extract"
)

func newExtractCmd(stdout, stderr io.Writer, deps appDeps, cfg *appConfig) *cobra.Command {
	var (
		scopes         scopeFlags
		projectAliases []string
		quotaProject   string
		cloudBuildRuns int
		quiet          bool
		outPath        string
	)

	cmd := &cobra.Command{
		Use:   "extract",
		Short: "Extract read-only multi-project GCP facts into a sanitized v3 facts.json file",
		Long: `Collect rate-limited read-only GCP inventory, IAM policies, GKE workload/ingress
chains, in-stream Cloud Build step references, and Cloud Monitoring/BigQuery/Dataflow
lineage into an atomic 0600-permissioned v3 facts.json file.

At least one target scope (--org, --folder, or --projects) is required.`,
		Example: `  gcp-arch extract --projects acme-cicd -o facts.json
  gcp-arch extract --projects app-prod,data-prod --project-alias 999000111222=shared-vpc-host -o prod.facts.json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !scopes.anyChanged(cmd) {
				scopes.applyConfig(cfg)
			}
			if !cmd.Flags().Changed("quota-project") && cfg.QuotaProject != "" {
				quotaProject = cfg.QuotaProject
			}
			if !cmd.Flags().Changed("cloudbuild-runs") && cfg.CloudBuildRuns != nil {
				cloudBuildRuns = *cfg.CloudBuildRuns
			}

			parsed, err := scopes.parse()
			if err != nil {
				return err
			}
			if parsed.empty() {
				return usageErr(fmt.Errorf("at least one of --org, --folder, or --projects is required"))
			}
			if cloudBuildRuns < 1 {
				return usageErr(fmt.Errorf("--cloudbuild-runs must be >= 1"))
			}
			if strings.TrimSpace(outPath) == "" {
				return usageErr(fmt.Errorf("-o/--out path cannot be empty"))
			}

			aliases, err := mergeProjectAliases(cfg.ProjectAliases, projectAliases)
			if err != nil {
				return err
			}

			progressW := stderr
			if quiet {
				progressW = io.Discard
			}

			ctx := cmd.Context()
			clients, err := deps.newExtractClients(ctx, extract.ClientOptions{
				QuotaProject:   quotaProject,
				ProjectAliases: aliases,
				ProgressWriter: progressW,
			})
			if err != nil {
				return runtimeErr(err)
			}
			if clients != nil {
				defer func() { _ = clients.Close() }()
			}

			facts, err := deps.extractFacts(ctx, clients, extract.ExtractOptions{
				Scopes:         parsed.canonical,
				CloudBuildRuns: cloudBuildRuns,
			})
			if err != nil {
				return runtimeErr(err)
			}

			if err := extract.WriteFacts(outPath, facts); err != nil {
				return runtimeErr(err)
			}

			writeOutOfScopeHints(stderr, facts.OutOfScopeTargets)
			writeDegradedCoverage(stderr, facts.CoverageLedger)

			if !quiet {
				_, _ = fmt.Fprintf(stdout, "Wrote %s (%d resources, %d edges)\n", outPath, len(facts.Resources), len(facts.Edges))
			}
			return nil
		},
	}

	cmd.Flags().SortFlags = false
	scopes.register(cmd)
	cmd.Flags().StringArrayVar(&projectAliases, "project-alias", nil, "Map project number to ID (`NUM=ID`, repeatable)")
	cmd.Flags().StringVar(&quotaProject, "quota-project", "", "Explicit GCP quota project `ID` (defaults to ADC quota_project_id)")
	cmd.Flags().IntVar(&cloudBuildRuns, "cloudbuild-runs", extract.DefaultCloudBuildRuns, "Latest succeeded Cloud Build runs folded per trigger")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress progress output")
	cmd.Flags().StringVarP(&outPath, "out", "o", "facts.json", "Output v3 facts JSON `FILE`")
	return cmd
}

func writeOutOfScopeHints(w io.Writer, hints []extract.OutOfScopeHint) {
	if len(hints) == 0 {
		return
	}
	sorted := slices.Clone(hints)
	slices.SortFunc(sorted, func(a, b extract.OutOfScopeHint) int {
		return cmp.Or(
			cmp.Compare(a.TargetProjectID, b.TargetProjectID),
			cmp.Compare(a.SourceKind, b.SourceKind),
		)
	})
	_, _ = fmt.Fprintf(w, "Out-of-scope deployment target projects discovered (%d):\n", len(sorted))
	for _, h := range sorted {
		_, _ = fmt.Fprintf(w, "  - %s (%s, referenced by %d resource(s))\n", h.TargetProjectID, h.SourceKind, len(h.ReferencedByURIs))
	}
}

func writeDegradedCoverage(w io.Writer, ledger []extract.CoverageEntry) {
	var degraded []extract.CoverageEntry
	for _, e := range ledger {
		if e.State != extract.CoverageOK {
			degraded = append(degraded, e)
		}
	}
	if len(degraded) == 0 {
		return
	}
	slices.SortFunc(degraded, func(a, b extract.CoverageEntry) int {
		return cmp.Or(cmp.Compare(a.Scope, b.Scope), cmp.Compare(a.Source, b.Source))
	})
	_, _ = fmt.Fprintf(w, "Coverage warnings (%d degraded):\n", len(degraded))
	for _, e := range degraded {
		line := fmt.Sprintf("  [%s] %s (%s)", e.State, e.Scope, e.Source)
		if e.Detail != "" {
			line += ": " + e.Detail
		}
		_, _ = fmt.Fprintln(w, line)
	}
}
