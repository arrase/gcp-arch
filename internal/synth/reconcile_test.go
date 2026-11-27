package synth

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/icons"
	"github.com/arrase/gcp-arch/internal/manifest"
)

func TestReconcileZeroDeltaFastPathLayeredFlow(t *testing.T) {
	reg, err := icons.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		filepath.Join("..", "..", "testdata", "catalog", "cat1_cloudrun_cloudsql.facts.json"),
		filepath.Join("..", "..", "testdata", "catalog", "cat3_ncc_hub_spoke_psc.facts.json"),
	} {
		t.Run(filepath.Base(rel), func(t *testing.T) {
			facts, err := extract.LoadFacts(rel)
			if err != nil {
				t.Fatal(err)
			}
			skel := buildSkeleton(facts, reg)
			orig := assembleManifest(skel, annotationResponse{FocusSummary: "Curated overview"}, reg, "2026-11-26T00:00:00Z")
			beforeYAML, err := manifest.Marshal(orig)
			if err != nil {
				t.Fatal(err)
			}

			start := time.Now()
			reconciled, err := Reconcile(context.Background(), facts, orig, nil, reg)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("zero-delta Reconcile with nil client failed: %v", err)
			}
			if elapsed >= 50*time.Millisecond {
				t.Fatalf("zero-delta Reconcile took %v, expected fast path", elapsed)
			}
			afterYAML, err := manifest.Marshal(reconciled)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(beforeYAML, afterYAML) {
				t.Fatalf("zero-delta Reconcile produced YAML diff")
			}
			if !reflect.DeepEqual(orig.TransversalItems, reconciled.TransversalItems) {
				t.Fatal("TransversalItems mutated")
			}
		})
	}
}

func TestReconcileZeroDeltaFastPathLegacyV1Fixture(t *testing.T) {
	reg, err := icons.Load()
	if err != nil {
		t.Fatal(err)
	}
	facts, err := extract.LoadFacts(filepath.Join("..", "..", "testdata", "acme_cicd_v3.facts.json"))
	if err != nil {
		t.Fatal(err)
	}
	expected, err := manifest.Load(filepath.Join("..", "..", "testdata", "acme_cicd_expected.architecture.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	reconciled, err := Reconcile(context.Background(), facts, expected, nil, reg)
	if err != nil {
		t.Fatalf("legacy v1 Reconcile fast path failed: %v", err)
	}
	beforeYAML, _ := manifest.Marshal(expected)
	afterYAML, _ := manifest.Marshal(reconciled)
	if !bytes.Equal(beforeYAML, afterYAML) {
		t.Fatalf("legacy v1 Reconcile fast path produced YAML diff")
	}
}

func TestReconcileZeroDeltaRefreshesEdgeProvenance(t *testing.T) {
	reg, err := icons.Load()
	if err != nil {
		t.Fatal(err)
	}
	uAPI := "//run.googleapis.com/projects/p1/locations/europe-west1/services/api"
	uDB := "//sqladmin.googleapis.com/projects/p1/instances/db"
	eID := extract.EdgeID(extract.EdgeConfigRef, uAPI, uDB, "tcp:5432")
	facts := &extract.FactsFile{
		SchemaVersion: extract.FactsSchemaVersion,
		Scopes:        []string{"projects/p1"},
		Resources: []extract.FactResource{
			{URI: uAPI, AssetType: "run.googleapis.com/Service", ProjectID: "p1", DisplayName: "api"},
			{URI: uDB, AssetType: "sqladmin.googleapis.com/Instance", ProjectID: "p1", DisplayName: "db"},
		},
		Edges: []extract.FactEdge{
			{ID: eID, FromURI: uAPI, ToURI: uDB, Kind: extract.EdgeConfigRef, Detail: "tcp:5432"},
		},
	}

	orig := assembleManifest(buildSkeleton(facts, reg), annotationResponse{}, reg, "2026-11-26T00:00:00Z")
	if orig.Views[0].Edges[0].Provenance != manifest.ProvenanceVerified {
		t.Fatalf("expected VERIFIED edge initially, got %s", orig.Views[0].Edges[0].Provenance)
	}

	// Turn the FactEdge heuristic: zero resource deltas, provenance downgrades cleanly to INFERRED with 0 LLM calls.
	heuristicFacts := *facts
	heuristicFacts.Edges = slices.Clone(facts.Edges)
	heuristicFacts.Edges[0].Heuristic = true

	downgraded, err := Reconcile(context.Background(), &heuristicFacts, orig, nil, reg)
	if err != nil {
		t.Fatalf("downgrade Reconcile failed: %v", err)
	}
	if downgraded.Views[0].Edges[0].Provenance != manifest.ProvenanceInferred ||
		!strings.HasPrefix(downgraded.Views[0].Edges[0].InferenceRationale, lostEvidenceRationalePrefix) {
		t.Fatalf("expected edge downgraded to INFERRED, got %+v", downgraded.Views[0].Edges[0])
	}
	// Verify original manifest was not mutated.
	if orig.Views[0].Edges[0].Provenance != manifest.ProvenanceVerified {
		t.Fatal("Reconcile mutated caller's existing manifest")
	}

	// Restore non-heuristic FactEdge: Reconcile upgrades edge back to VERIFIED with 0 LLM calls.
	upgraded, err := Reconcile(context.Background(), facts, downgraded, nil, reg)
	if err != nil {
		t.Fatalf("upgrade Reconcile failed: %v", err)
	}
	if upgraded.Views[0].Edges[0].Provenance != manifest.ProvenanceVerified || !slices.Equal(upgraded.Views[0].Edges[0].EvidenceEdgeIDs, []string{eID}) {
		t.Fatalf("expected edge upgraded back to VERIFIED with %s, got %+v", eID, upgraded.Views[0].Edges[0])
	}
}

func TestReconcileDeltaPathPreservesCuratedEdits(t *testing.T) {
	reg, err := icons.Load()
	if err != nil {
		t.Fatal(err)
	}
	facts, err := extract.LoadFacts(filepath.Join("..", "..", "testdata", "catalog", "cat1_cloudrun_cloudsql.facts.json"))
	if err != nil {
		t.Fatal(err)
	}
	existing := assembleManifest(buildSkeleton(facts, reg), annotationResponse{FocusSummary: "Original summary"}, reg, "2026-11-26T00:00:00Z")

	// Apply curated human edits to a component and View 0.
	curatedCompID := existing.Components[0].ID
	existing.Components[0].Title = "Curated Human Title"
	existing.Components[0].Subtitle = "Curated Human Subtitle"
	existing.Components[0].Notes = "Owned by Payments SRE"
	existing.Views[0].Title = "Curated Payments Overview"
	existing.Views[0].Notes = "Reviewed 2026-11-27"
	// Leave existing.Views[0].Subtitle as the auto-generated counts string to verify counts refresh!

	// Add a new resource + functional edge to trigger Day-2 delta path.
	newURI := "//storage.googleapis.com/p-new-audit-bucket"
	drifted := *facts
	drifted.Resources = append(slices.Clone(facts.Resources), extract.FactResource{
		URI:         newURI,
		AssetType:   "storage.googleapis.com/Bucket",
		ProjectID:   facts.Resources[0].ProjectID,
		DisplayName: "p-new-audit-bucket",
	})
	newEdge := extract.FactEdge{
		FromURI: existing.Components[0].BackedBy[0].URI,
		ToURI:   newURI,
		Kind:    extract.EdgeConfigRef,
		Detail:  "audit_export",
	}
	newEdge.ID = extract.EdgeID(newEdge.Kind, newEdge.FromURI, newEdge.ToURI, newEdge.Detail)
	drifted.Edges = append(slices.Clone(facts.Edges), newEdge)

	// Delta path with nil client must fail clearly.
	if _, err := Reconcile(context.Background(), &drifted, existing, nil, reg); err == nil {
		t.Fatal("expected error on delta Reconcile with nil client")
	}

	// Delta path with client must synthesize new topology AND preserve curated human edits.
	client := &stubModelClient{resp: annotationResponse{FocusSummary: "Updated summary"}}
	reconciled, err := Reconcile(context.Background(), &drifted, existing, client, reg)
	if err != nil {
		t.Fatalf("delta Reconcile failed: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("expected 1 LLM call on delta Reconcile, got %d", client.calls)
	}

	rep, err := manifest.ValidateWithFactsAndConservation(reconciled, &drifted, reg)
	if err != nil || rep.Status != "PASS" {
		t.Fatalf("reconciled manifest failed validation/conservation: err=%v rep=%+v", err, rep)
	}

	// Verify curated component edits preserved.
	var foundComp *manifest.Component
	for i := range reconciled.Components {
		if reconciled.Components[i].ID == curatedCompID {
			foundComp = &reconciled.Components[i]
		}
	}
	if foundComp == nil {
		t.Fatalf("curated component %q missing after reconcile", curatedCompID)
	}
	if foundComp.Title != "Curated Human Title" || foundComp.Subtitle != "Curated Human Subtitle" || foundComp.Notes != "Owned by Payments SRE" {
		t.Fatalf("curated component edits lost: %+v", foundComp)
	}

	// Verify curated view Title and Notes preserved, while uncustomized auto-subtitle refreshed to new counts.
	if reconciled.Views[0].Title != "Curated Payments Overview" || reconciled.Views[0].Notes != "Reviewed 2026-11-27" {
		t.Fatalf("curated view Title/Notes lost: title=%q notes=%q", reconciled.Views[0].Title, reconciled.Views[0].Notes)
	}
	if reconciled.Views[0].Subtitle == existing.Views[0].Subtitle {
		t.Fatalf("expected uncustomized auto overview subtitle to update with new counts, stayed %q", reconciled.Views[0].Subtitle)
	}

	// Verify custom View 0 subtitle IS preserved when customized.
	existing.Views[0].Subtitle = "Custom executive subtitle"
	reconciled2, err := Reconcile(context.Background(), &drifted, existing, client, reg)
	if err != nil {
		t.Fatalf("delta Reconcile (custom view subtitle) failed: %v", err)
	}
	if reconciled2.Views[0].Subtitle != "Custom executive subtitle" {
		t.Fatalf("custom view subtitle lost: got %q", reconciled2.Views[0].Subtitle)
	}
}
