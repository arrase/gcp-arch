package synth

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/icons"
	"github.com/arrase/gcp-arch/internal/manifest"
)

func TestSynthesizeToReconcileIdempotenceWithCrossViewUndrawnEdges(t *testing.T) {
	reg, err := icons.Load()
	if err != nil {
		t.Fatal(err)
	}
	facts := minimalTwoViewFacts()
	crossEdge := extract.FactEdge{
		FromURI: "//cloudbuild.googleapis.com/projects/p1/locations/global/triggers/t1",
		ToURI:   "//run.googleapis.com/projects/p1/locations/us-central1/services/app",
		Kind:    extract.EdgeCloudBuildStepRef,
		Detail:  "gcloud run deploy app",
	}
	crossEdge.ID = extract.EdgeID(crossEdge.Kind, crossEdge.FromURI, crossEdge.ToURI, crossEdge.Detail)
	facts.Edges = append(facts.Edges, crossEdge)
	table := NewInternTable(facts)

	client := &mockModelClient{handler: func(_ int, _, user string, _ *genai.Schema) ([]byte, error) {
		if strings.Contains(user, `"adjacency"`) {
			return json.Marshal(Pass1Plan{
				Components: []Pass1Component{
					{ID: "repo", Title: "GitHub Repo", IconKey: "github", ComponentKind: manifest.KindExternalCaller, ProjectID: "external", BackedByIDs: []string{mustTestRID(t, table, "//github.com/org/repo")}},
					{ID: "cb", Title: "Cloud Build", IconKey: "cloud_build", ComponentKind: manifest.KindPipeline, ProjectID: "p1", BackedByIDs: []string{mustTestRID(t, table, "//cloudbuild.googleapis.com/projects/p1/locations/global/triggers/t1")}},
					{ID: "bucket", Title: "Artifacts Bucket", IconKey: "cloud_storage", ComponentKind: manifest.KindDatastore, ProjectID: "p1", BackedByIDs: []string{mustTestRID(t, table, "//storage.googleapis.com/b1")}},
					{ID: "app", Title: "Cloud Run App", IconKey: "cloud_run", ComponentKind: manifest.KindWorkload, ProjectID: "p1", BackedByIDs: []string{mustTestRID(t, table, "//run.googleapis.com/projects/p1/locations/us-central1/services/app")}},
					{ID: "db", Title: "Cloud SQL DB", IconKey: "cloud_sql", ComponentKind: manifest.KindDatastore, ProjectID: "p1", BackedByIDs: []string{mustTestRID(t, table, "//sqladmin.googleapis.com/projects/p1/instances/db1")}},
				},
				DetailViews: []Pass1DetailView{
					{ID: "cicd_flow", Title: "CI/CD Flow", Archetype: manifest.ArchetypeStagePipeline, AccentMode: manifest.AccentCICD, ComponentIDs: []string{"repo", "cb", "bucket"}},
					{ID: "app_runtime", Title: "App Runtime", Archetype: manifest.ArchetypeLandscapeMatrix, AccentMode: manifest.AccentRuntime, ComponentIDs: []string{"app", "db"}},
				},
			})
		}
		if strings.Contains(user, `"id":"cicd_flow"`) {
			return json.Marshal(Pass2View{
				Placements: []Pass2Placement{
					{ComponentID: "repo", Column: 0},
					{ComponentID: "cb", Column: 1},
					{ComponentID: "bucket", Column: 2},
				},
				Edges: []Pass2Edge{
					{From: "repo", To: "cb", Verb: "triggers", FlowKind: manifest.FlowCICD, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{mustTestEID(t, table, facts, 0)}},
					{From: "cb", To: "bucket", Verb: "writes", FlowKind: manifest.FlowCICD, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{mustTestEID(t, table, facts, 1)}},
				},
			})
		}
		return json.Marshal(Pass2View{
			Placements: []Pass2Placement{
				{ComponentID: "app", Column: 1},
				{ComponentID: "db", Column: 3},
			},
			Edges: []Pass2Edge{
				{From: "app", To: "db", Verb: "SQL", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{mustTestEID(t, table, facts, 2)}},
			},
		})
	}}

	m, err := Synthesize(context.Background(), facts, client, reg, Options{})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(m.AcknowledgedEdges) != 1 || m.AcknowledgedEdges[0] != (manifest.ComponentPair{From: "cb", To: "app"}) {
		t.Fatalf("expected acknowledged cross-view pair cb->app, got %+v", m.AcknowledgedEdges)
	}

	// Symmetric OUT / IN off_page_refs must be derived deterministically in Go.
	cicd := findTestView(t, m.Views, "cicd_flow")
	app := findTestView(t, m.Views, "app_runtime")
	if got := cicd.Placements[1].OffPageRefs; len(got) != 1 || got[0].TargetViewID != "app_runtime" || got[0].Direction != "OUT" {
		t.Fatalf("expected symmetric OUT off_page_ref on cb, got %+v", got)
	}
	if got := app.Placements[0].OffPageRefs; len(got) != 1 || got[0].TargetViewID != "cicd_flow" || got[0].Direction != "IN" {
		t.Fatalf("expected symmetric IN off_page_ref on app, got %+v", got)
	}

	// Day-2 Reconcile with nil client must take the 0-LLM fast path with zero YAML diff.
	before, _ := manifest.Marshal(m)
	reconciled, err := Reconcile(context.Background(), facts, m, nil, reg, Options{})
	if err != nil {
		t.Fatalf("Reconcile fast path with acknowledged cross-view edge failed: %v", err)
	}
	after, _ := manifest.Marshal(reconciled)
	if string(before) != string(after) {
		t.Fatalf("Reconcile mutated manifest:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestReconcileDowngradesLostWalkAndIgnoresHeuristicChurn(t *testing.T) {
	facts, expected, reg := loadAcmeFixtures(t)
	curated := cloneManifest(expected)
	// Add a human-curated INFERRED edge that must never be clobbered.
	for i := range curated.Views {
		if curated.Views[i].ID == "gke_developer_platform" {
			curated.Views[i].Edges = append(curated.Views[i].Edges, manifest.Edge{
				From: "atlantis", To: "sonarqube", Verb: "PR status", FlowKind: manifest.FlowCICD,
				Provenance: manifest.ProvenanceInferred, InferenceRationale: "Human-documented webhook callback",
			})
		}
	}

	// 1. Heuristic MONITORING_TRAFFIC edge churn must take 0 LLM calls and leave YAML unchanged.
	monFacts := *facts
	monEdge := extract.FactEdge{
		FromURI:   "//container.googleapis.com/projects/acme-cicd/locations/europe-southwest1-b/clusters/cicd/k8s/namespaces/atlantis/apps/deployments/atlantis",
		ToURI:     "//artifactregistry.googleapis.com/projects/acme-cicd/locations/europe-southwest1/repositories/acme-apps",
		Kind:      extract.EdgeMonitoringTraffic,
		Detail:    "artifactregistry.googleapis.com (12 reqs/7d)",
		Heuristic: true,
	}
	monEdge.ID = extract.EdgeID(monEdge.Kind, monEdge.FromURI, monEdge.ToURI, monEdge.Detail)
	monFacts.Edges = append(append([]extract.FactEdge(nil), facts.Edges...), monEdge)

	before, _ := manifest.Marshal(curated)
	monRes, err := Reconcile(context.Background(), &monFacts, curated, nil, reg, Options{})
	if err != nil {
		t.Fatalf("heuristic monitoring churn must take 0 LLM calls: %v", err)
	}
	afterMon, _ := manifest.Marshal(monRes)
	if string(before) != string(afterMon) {
		t.Fatalf("heuristic monitoring edge caused YAML churn:\nbefore:\n%s\nafter:\n%s", before, afterMon)
	}

	// 2. Transiently vanished walk downgrades VERIFIED -> INFERRED (never deletes) in 0 LLM calls.
	lostFacts := *facts
	lostFacts.Edges = nil
	for _, fe := range facts.Edges {
		if fe.ID != "21fd172f5bac6a44" {
			lostFacts.Edges = append(lostFacts.Edges, fe)
		}
	}
	downgraded, err := Reconcile(context.Background(), &lostFacts, curated, nil, reg, Options{})
	if err != nil {
		t.Fatalf("lost-walk reconcile with nil client failed: %v", err)
	}
	gkeView := findTestView(t, downgraded.Views, "gke_developer_platform")
	var sawDowngraded, sawHumanInferred bool
	for _, e := range gkeView.Edges {
		if e.From == "sonarqube" && e.To == "sonarqube_db" {
			sawDowngraded = e.Provenance == manifest.ProvenanceInferred && strings.HasPrefix(e.InferenceRationale, lostEvidenceRationalePrefix)
		}
		if e.From == "atlantis" && e.To == "sonarqube" {
			sawHumanInferred = e.Provenance == manifest.ProvenanceInferred && e.InferenceRationale == "Human-documented webhook callback"
		}
	}
	if !sawDowngraded || !sawHumanInferred {
		t.Fatalf("expected downgraded sonarqube->sonarqube_db (%v) and preserved human INFERRED (%v)", sawDowngraded, sawHumanInferred)
	}

	// 3. When the walk reappears, the auto-downgraded edge recovers to VERIFIED while the human INFERRED edge stays INFERRED.
	recovered, err := Reconcile(context.Background(), facts, downgraded, nil, reg, Options{})
	if err != nil {
		t.Fatalf("recovery reconcile failed: %v", err)
	}
	if gotYAML, _ := manifest.Marshal(recovered); string(gotYAML) != string(before) {
		t.Fatalf("recovered manifest did not match original curated manifest:\ngot:\n%s\nwant:\n%s", gotYAML, before)
	}
}

func TestReconcilePrunesDeletedBackedContainerAndCleansPairs(t *testing.T) {
	facts, expected, reg := loadAcmeFixtures(t)
	cur := cloneManifest(expected)
	cur.RejectedEdges = []manifest.RejectedEdge{{From: "cloudbuild_staging_bucket", To: "sonarqube_db", Reason: "stale"}}
	cur.AcknowledgedEdges = []manifest.ComponentPair{{From: "cloudbuild_staging_bucket", To: "atlantis"}}

	const gkeURI = "//container.googleapis.com/projects/acme-cicd/locations/europe-southwest1-b/clusters/cicd"
	const stagingURI = "//storage.googleapis.com/acme-cicd_cloudbuild"

	prunedFacts := *facts
	prunedFacts.Resources = nil
	for _, r := range facts.Resources {
		if r.URI == gkeURI || r.URI == stagingURI {
			continue
		}
		if r.ParentURI == gkeURI {
			r.ParentURI = ""
		}
		prunedFacts.Resources = append(prunedFacts.Resources, r)
	}
	prunedFacts.Edges = nil
	for _, fe := range facts.Edges {
		if fe.FromURI != gkeURI && fe.ToURI != gkeURI && fe.FromURI != stagingURI && fe.ToURI != stagingURI {
			prunedFacts.Edges = append(prunedFacts.Edges, fe)
		}
	}

	res, err := Reconcile(context.Background(), &prunedFacts, cur, nil, reg, Options{})
	if err != nil {
		t.Fatalf("container + pair pruning reconcile failed: %v", err)
	}
	for _, ct := range res.Containers {
		if ct.ID == "gke_cicd" {
			t.Fatalf("expected resource-backed container gke_cicd to be pruned when cluster vanished")
		}
	}
	if len(res.RejectedEdges) != 0 || len(res.AcknowledgedEdges) != 0 {
		t.Fatalf("expected dangling rejected/acknowledged pairs cleaned, got rejected=%v ack=%v", res.RejectedEdges, res.AcknowledgedEdges)
	}
}

func TestView0FairExternalCapAndSharedSinkLink(t *testing.T) {
	m := &manifest.Manifest{
		Components: []manifest.Component{
			{ID: "caller_1", ComponentKind: manifest.KindExternalCaller},
			{ID: "caller_2", ComponentKind: manifest.KindExternalCaller},
			{ID: "caller_3", ComponentKind: manifest.KindExternalCaller},
			{ID: "sink", ComponentKind: manifest.KindDatastore},
			{ID: "target_1", ComponentKind: manifest.KindExternalTarget},
			{ID: "target_2", ComponentKind: manifest.KindExternalTarget},
			{ID: "target_3", ComponentKind: manifest.KindExternalTarget},
		},
	}
	for i := range 14 {
		vid := fmt.Sprintf("v_%02d", i)
		arch := manifest.ArchetypeLandscapeMatrix
		accent := manifest.AccentRuntime
		if i == 0 {
			arch = manifest.ArchetypeStagePipeline
			accent = manifest.AccentCICD
		}
		var refs []manifest.OffPageRef
		switch i {
		case 0:
			refs = []manifest.OffPageRef{{TargetViewID: "v_01", Label: "locks state", Direction: "IN"}}
		case 1:
			refs = []manifest.OffPageRef{{TargetViewID: "v_00", Label: "updates state", Direction: "IN"}}
		}
		m.Views = append(m.Views, manifest.View{
			ID: vid, Title: vid, Archetype: arch, AccentMode: accent,
			Placements: []manifest.Placement{
				{ComponentID: "caller_1", Column: 0},
				{ComponentID: "caller_2", Column: 0},
				{ComponentID: "caller_3", Column: 0},
				{ComponentID: "sink", Column: 2, OffPageRefs: refs},
				{ComponentID: "target_1", Column: 3},
				{ComponentID: "target_2", Column: 3},
				{ComponentID: "target_3", Column: 3},
			},
		})
	}
	v0 := BuildSystemIndexView(m)
	if v0 == nil || len(v0.Placements) != maxSystemIndexPlacements {
		t.Fatalf("expected %d View 0 placements, got %+v", maxSystemIndexPlacements, v0)
	}
	var sawCaller, sawTarget, sawSharedSinkEdge bool
	for _, p := range v0.Placements {
		sawCaller = sawCaller || p.ComponentID == "caller_1"
		sawTarget = sawTarget || p.ComponentID == "target_1"
	}
	for _, e := range v0.Edges {
		if e.From == "v_00" && e.To == "v_01" && e.FlowKind == manifest.FlowCICD {
			sawSharedSinkEdge = true
		}
	}
	if !sawCaller || !sawTarget || !sawSharedSinkEdge {
		t.Fatalf("expected fair caller (%v) + target (%v) cap and v_00->v_01 shared-sink edge (%v), edges=%+v", sawCaller, sawTarget, sawSharedSinkEdge, v0.Edges)
	}
}
