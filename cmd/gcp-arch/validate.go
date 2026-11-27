package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/arrase/gcp-arch/internal/icons"
	"github.com/arrase/gcp-arch/internal/manifest"
)

func newValidateCmd(stdout io.Writer) *cobra.Command {
	var (
		factsPath string
		asJSON    bool
	)

	cmd := &cobra.Command{
		Use:   "validate <architecture.yaml>",
		Short: "Validate architecture.yaml structure offline or with --facts provenance and conservation checks",
		Long: `Validate an architecture.yaml manifest completely offline with zero network calls.
Without --facts, validates strict YAML schema (KnownFields), archetype bounds,
acyclic container nesting, icon keys, datastore homogeneity, and rejected_edges
compliance.

With --facts <facts.json>, additionally enforces 100% FactsFile resource accounting,
directed-walk FactEdge provenance on VERIFIED edges, and deterministic conservation reporting.`,
		Example: `  gcp-arch validate architecture.yaml
  gcp-arch validate architecture.yaml --facts facts.json --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return executeValidate(stdout, args[0], factsPath, asJSON)
		},
	}

	cmd.Flags().SortFlags = false
	cmd.Flags().StringVar(&factsPath, "facts", "", "Optional v3 facts.json `FILE` for provenance and 100% resource coverage checks")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit ValidationReport as JSON")
	return cmd
}

func executeValidate(stdout io.Writer, manifestPath, factsPath string, asJSON bool) error {
	m, err := manifest.Load(manifestPath)
	if err != nil {
		return handleValidateLoadError(stdout, manifestPath, asJSON, err)
	}

	reg, err := icons.Load()
	if err != nil {
		return runtimeErr(err)
	}

	valErr, conservation, err := runManifestValidation(m, reg, factsPath)
	if err != nil {
		return err
	}
	if conservation != nil && conservation.Status != "PASS" && m.IsLayeredFlow() {
		cIssues := conservation.Issues()
		if valErr == nil {
			valErr = &manifest.ValidationError{Issues: cIssues}
		} else {
			var ve *manifest.ValidationError
			if errors.As(valErr, &ve) {
				ve.Issues = append(ve.Issues, cIssues...)
			}
		}
	}
	if valErr != nil {
		var ve *manifest.ValidationError
		if asJSON && errors.As(valErr, &ve) {
			rep := ve.Report()
			rep.Conservation = conservation
			if err := writeJSON(stdout, rep); err != nil {
				return runtimeErr(err)
			}
		}
		return valErr
	}

	if asJSON {
		if err := writeJSON(stdout, manifest.ValidationReport{
			Valid:         true,
			ExcludedCount: len(m.ExcludedResources),
			Conservation:  conservation,
		}); err != nil {
			return runtimeErr(err)
		}
		return nil
	}

	parts := []string{
		fmt.Sprintf("%d views", len(m.Views)),
		fmt.Sprintf("%d components", len(m.Components)),
	}
	if len(m.ExcludedResources) > 0 {
		parts = append(parts, fmt.Sprintf("%d excluded", len(m.ExcludedResources)))
	}
	if conservation != nil {
		parts = append(parts, fmt.Sprintf("conservation=%s primary=%d/%d, view0=%.2f",
			conservation.Status, conservation.RepresentedPrimaryCount, conservation.FactsPrimaryCount, conservation.View0Coverage))
	}
	_, _ = fmt.Fprintf(stdout, "Valid: %s (%s)\n", manifestPath, strings.Join(parts, ", "))
	return nil
}

func handleValidateLoadError(stdout io.Writer, manifestPath string, asJSON bool, err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return runtimeErr(err)
	}
	if asJSON {
		_ = writeJSON(stdout, manifest.ValidationReport{
			Valid: false,
			Errors: []manifest.ValidationIssue{{
				Code:    "YAML_DECODE_ERROR",
				Subject: manifestPath,
				Message: err.Error(),
			}},
		})
	}
	return usageErr(err)
}

func runManifestValidation(m *manifest.Manifest, reg *icons.Registry, factsPath string) (valErr error, conservation *manifest.ConservationReport, loadErr error) {
	if strings.TrimSpace(factsPath) == "" {
		return manifest.ValidateStructure(m, reg), nil, nil
	}
	facts, err := loadFacts(factsPath)
	if err != nil {
		return nil, nil, err
	}
	rep, err := manifest.ValidateWithFactsAndConservation(m, facts, reg)
	return err, &rep, nil
}
