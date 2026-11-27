package synth

import (
	"context"
	"fmt"
	"time"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/icons"
	"github.com/arrase/gcp-arch/internal/manifest"
)

// MaxActiveResources is the active-resource ceiling for a single synthesis run.
const MaxActiveResources = 500

// ScaleLimitError is returned when facts.json exceeds MaxActiveResources (mapped to CLI exit code 2).
type ScaleLimitError struct {
	ResourceCount int
	MaxAllowed    int
}

func (e *ScaleLimitError) Error() string {
	return fmt.Sprintf("facts file contains %d active resources, exceeding the %d-resource synthesis ceiling; narrow --projects or --folder to a single application domain", e.ResourceCount, e.MaxAllowed)
}

// Result holds the synthesized architecture manifest, conservation report, and LLM usage telemetry.
type Result struct {
	Manifest     *manifest.Manifest
	Conservation manifest.ConservationReport
	Usage        UsageStats
}

// Synthesize builds a deterministic GCP architecture skeleton from facts, annotates it via a
// single bounded Vertex AI call, validates structure and provenance, and verifies deterministic
// resource and functional-edge conservation.
func Synthesize(
	ctx context.Context,
	facts *extract.FactsFile,
	client ModelClient,
	reg *icons.Registry,
) (*Result, error) {
	if err := validateSynthesizeInputs(facts, client, reg); err != nil {
		return nil, err
	}

	skel := buildSkeleton(facts, reg)
	llmResp, stats, err := annotateSkeleton(ctx, client, skel)
	if err != nil {
		return nil, err
	}

	m := assembleManifest(skel, llmResp, reg, time.Now().UTC().Format(time.RFC3339))
	rep, err := manifest.ValidateWithFactsAndConservation(m, facts, reg)
	if err != nil {
		return nil, err
	}
	if rep.Status != "PASS" {
		return nil, &manifest.ValidationError{Issues: rep.Issues()}
	}

	return &Result{
		Manifest:     m,
		Conservation: rep,
		Usage:        stats,
	}, nil
}

func validateSynthesizeInputs(facts *extract.FactsFile, client ModelClient, reg *icons.Registry) error {
	if facts == nil {
		return fmt.Errorf("facts file must not be nil")
	}
	if client == nil {
		return fmt.Errorf("model client must not be nil")
	}
	if reg == nil {
		return fmt.Errorf("icons registry must not be nil")
	}
	if len(facts.Resources) > MaxActiveResources {
		return &ScaleLimitError{ResourceCount: len(facts.Resources), MaxAllowed: MaxActiveResources}
	}
	return nil
}
