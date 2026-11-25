package synth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/icons"
	"github.com/arrase/gcp-arch/internal/manifest"
)

type mockModelClient struct {
	mu      sync.Mutex
	calls   int
	prompts []string
	handler func(call int, systemPrompt, userPrompt string, schema *genai.Schema) ([]byte, error)
}

func (m *mockModelClient) GenerateJSON(ctx context.Context, systemPrompt, userPrompt string, schema *genai.Schema) ([]byte, error) {
	m.mu.Lock()
	call := m.calls
	m.calls++
	m.prompts = append(m.prompts, userPrompt)
	m.mu.Unlock()
	return m.handler(call, systemPrompt, userPrompt, schema)
}

func loadAcmeFixtures(t *testing.T) (*extract.FactsFile, *manifest.Manifest, *icons.Registry) {
	t.Helper()
	factsPath := filepath.Join("..", "..", "testdata", "acme_cicd_v3.facts.json")
	facts, err := extract.LoadFacts(factsPath)
	if err != nil {
		t.Fatalf("LoadFacts: %v", err)
	}
	manifestPath := filepath.Join("..", "..", "testdata", "acme_cicd_expected.architecture.yaml")
	m, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatalf("manifest.Load: %v", err)
	}
	reg, err := icons.Load()
	if err != nil {
		t.Fatalf("icons.Load: %v", err)
	}
	return facts, m, reg
}

func TestInternTableAndAdjacency(t *testing.T) {
	facts, _, _ := loadAcmeFixtures(t)
	table := NewInternTable(facts)

	assertResourceInternRoundTrip(t, table, len(facts.Resources))
	assertEdgeInternRoundTrip(t, table, len(facts.Edges))

	// Deduplication test: inject a duplicate (from, to, kind) edge with different detail.
	dupFacts := *facts
	dupFacts.Edges = append(append([]extract.FactEdge(nil), facts.Edges...), extract.FactEdge{
		FromURI: facts.Edges[0].FromURI,
		ToURI:   facts.Edges[0].ToURI,
		Kind:    facts.Edges[0].Kind,
		Detail:  "duplicate-detail-variant",
	}, extract.FactEdge{
		FromURI: "//unknown/source",
		ToURI:   "//unknown/target",
		Kind:    extract.EdgeConfigRef,
	})
	dupTable := NewInternTable(&dupFacts)
	adj1 := BuildAdjacency(facts, table)
	adj2 := BuildAdjacency(&dupFacts, dupTable)

	b1, _ := json.Marshal(adj1)
	b2, _ := json.Marshal(adj2)
	if string(b1) != string(b2) {
		t.Fatalf("expected deduplicated adjacency to match:\nwant: %s\ngot:  %s", b1, b2)
	}
}

func assertResourceInternRoundTrip(t *testing.T, table *InternTable, wantCount int) {
	t.Helper()
	resIDs := table.ResourceIDs()
	if len(resIDs) != wantCount {
		t.Fatalf("got %d interned resources, want %d", len(resIDs), wantCount)
	}
	if resIDs[0] != "r_0001" || resIDs[len(resIDs)-1] != fmt.Sprintf("r_%04d", wantCount) {
		t.Fatalf("unexpected resource ID bounds: %s .. %s", resIDs[0], resIDs[len(resIDs)-1])
	}
	for _, rid := range resIDs {
		r, ok := table.ResourceByID(rid)
		if !ok {
			t.Fatalf("ResourceByID(%q) missing", rid)
		}
		gotID, ok := table.ResourceID(r.URI)
		if !ok || gotID != rid {
			t.Fatalf("ResourceID(%q) = %q (%v), want %q", r.URI, gotID, ok, rid)
		}
	}
}

func assertEdgeInternRoundTrip(t *testing.T, table *InternTable, wantCount int) {
	t.Helper()
	edgeIDs := table.EdgeIDs()
	if len(edgeIDs) != wantCount {
		t.Fatalf("got %d interned edges, want %d", len(edgeIDs), wantCount)
	}
	for _, eid := range edgeIDs {
		fe, ok := table.EdgeByID(eid)
		if !ok {
			t.Fatalf("EdgeByID(%q) missing", eid)
		}
		gotEID, ok := table.EdgeID(fe.ID)
		if !ok || gotEID != eid {
			t.Fatalf("EdgeID(%q) = %q (%v), want %q", fe.ID, gotEID, ok, eid)
		}
	}
}

func TestSchemasNonRecursiveAndConstraints(t *testing.T) {
	p1 := Pass1PlanSchema()
	p2 := Pass2ViewSchema()
	rc := ReconcilePatchSchema()

	// Verify icon_key is plain TypeString (no enum) while required enums exist.
	compProps := p1.Properties["components"].Items.Properties
	if compProps["icon_key"].Type != genai.TypeString || len(compProps["icon_key"].Enum) != 0 {
		t.Fatalf("icon_key must be plain TypeString without enum, got type=%s enum=%v", compProps["icon_key"].Type, compProps["icon_key"].Enum)
	}
	if len(compProps["component_kind"].Enum) != 8 {
		t.Fatalf("expected 8 component_kind enum values, got %d", len(compProps["component_kind"].Enum))
	}
	contProps := p1.Properties["container_defs"].Items.Properties
	if len(contProps["kind"].Enum) != 6 {
		t.Fatalf("expected 6 container_kind enum values, got %d", len(contProps["kind"].Enum))
	}
	viewProps := p1.Properties["detail_views"].Items.Properties
	if len(viewProps["archetype"].Enum) != 3 {
		t.Fatalf("expected 3 archetype enum values, got %d", len(viewProps["archetype"].Enum))
	}
	edgeProps := p2.Properties["edges"].Items.Properties
	if len(edgeProps["flow_kind"].Enum) != 3 || len(edgeProps["provenance"].Enum) != 2 {
		t.Fatalf("unexpected flow_kind/provenance enums")
	}

	// Ensure all three schemas serialize cleanly without cycles.
	for name, s := range map[string]*genai.Schema{"Pass1": p1, "Pass2": p2, "Reconcile": rc} {
		if _, err := json.Marshal(s); err != nil {
			t.Fatalf("marshal %s schema failed: %v", name, err)
		}
	}
}

func TestAcmeExpectedManifestAndZeroLLMFastPath(t *testing.T) {
	facts, expected, reg := loadAcmeFixtures(t)

	if err := manifest.ValidateWithFacts(expected, facts, reg); err != nil {
		t.Fatalf("acme_cicd_expected.architecture.yaml failed ValidateWithFacts: %v", err)
	}

	// Coverage must account for 100% of facts resources.
	cov := manifest.ComputeCoverage(expected)
	if len(cov) != len(facts.Resources) {
		t.Fatalf("ComputeCoverage rows = %d, want %d", len(cov), len(facts.Resources))
	}

	// Deterministic BuildSystemIndexView must match expected views[0].
	v0 := BuildSystemIndexView(expected)
	if v0 == nil {
		t.Fatalf("BuildSystemIndexView returned nil for 3 detail views")
	}
	gotV0, _ := json.Marshal(v0)
	wantV0, _ := json.Marshal(expected.Views[0])
	if string(gotV0) != string(wantV0) {
		t.Fatalf("BuildSystemIndexView mismatch:\ngot:  %s\nwant: %s", gotV0, wantV0)
	}

	// Zero-LLM fast path: nil client, < 100 ms, zero diff.
	start := time.Now()
	reconciled, err := Reconcile(context.Background(), facts, expected, nil, reg, Options{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Reconcile fast path failed: %v", err)
	}
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("Reconcile fast path took %v (want < 100ms)", elapsed)
	}
	beforeYAML, _ := manifest.Marshal(expected)
	afterYAML, _ := manifest.Marshal(reconciled)
	if string(beforeYAML) != string(afterYAML) {
		t.Fatalf("Reconcile fast path produced YAML diff:\nbefore:\n%s\nafter:\n%s", beforeYAML, afterYAML)
	}
}

func TestScaleLimitError(t *testing.T) {
	reg, err := icons.Load()
	if err != nil {
		t.Fatalf("icons.Load: %v", err)
	}
	huge := &extract.FactsFile{
		SchemaVersion: extract.FactsSchemaVersion,
		Resources:     make([]extract.FactResource, MaxActiveResources+1),
	}
	for i := range huge.Resources {
		huge.Resources[i] = extract.FactResource{
			URI:       fmt.Sprintf("//storage.googleapis.com/b-%04d", i),
			AssetType: "storage.googleapis.com/Bucket",
			ProjectID: "p1",
		}
	}

	client := &mockModelClient{handler: func(int, string, string, *genai.Schema) ([]byte, error) {
		t.Fatalf("model client must not be called when exceeding %d resources", MaxActiveResources)
		return nil, nil
	}}

	_, err = Synthesize(context.Background(), huge, client, reg, Options{})
	var scaleErr *ScaleLimitError
	if !errors.As(err, &scaleErr) {
		t.Fatalf("expected *ScaleLimitError from Synthesize, got %v", err)
	}
	if scaleErr.ResourceCount != MaxActiveResources+1 || !strings.Contains(scaleErr.Error(), "narrow --projects") {
		t.Fatalf("unexpected ScaleLimitError: %v", scaleErr)
	}

	_, err = Reconcile(context.Background(), huge, &manifest.Manifest{}, client, reg, Options{})
	if !errors.As(err, &scaleErr) {
		t.Fatalf("expected *ScaleLimitError from Reconcile, got %v", err)
	}
}

func minimalTwoViewFacts() *extract.FactsFile {
	e1 := extract.FactEdge{
		FromURI: "//github.com/org/repo",
		ToURI:   "//cloudbuild.googleapis.com/projects/p1/locations/global/triggers/t1",
		Kind:    extract.EdgeCloudBuildRepo,
		Detail:  "push:main",
	}
	e1.ID = extract.EdgeID(e1.Kind, e1.FromURI, e1.ToURI, e1.Detail)

	e2 := extract.FactEdge{
		FromURI: "//cloudbuild.googleapis.com/projects/p1/locations/global/triggers/t1",
		ToURI:   "//storage.googleapis.com/b1",
		Kind:    extract.EdgeCloudBuildStepRef,
		Detail:  "gs://b1",
	}
	e2.ID = extract.EdgeID(e2.Kind, e2.FromURI, e2.ToURI, e2.Detail)

	e3 := extract.FactEdge{
		FromURI: "//run.googleapis.com/projects/p1/locations/us-central1/services/app",
		ToURI:   "//sqladmin.googleapis.com/projects/p1/instances/db1",
		Kind:    extract.EdgeConfigRef,
		Detail:  "10.0.0.5",
	}
	e3.ID = extract.EdgeID(e3.Kind, e3.FromURI, e3.ToURI, e3.Detail)

	return &extract.FactsFile{
		SchemaVersion: extract.FactsSchemaVersion,
		ReadTime:      "2026-11-23T02:00:00Z",
		Scopes:        []string{"projects/p1"},
		Hierarchy: extract.Hierarchy{
			Projects: []extract.ProjectNode{{ProjectID: "p1", DisplayName: "P1"}},
		},
		Resources: []extract.FactResource{
			{URI: "//cloudbuild.googleapis.com/projects/p1/locations/global/triggers/t1", AssetType: "cloudbuild.googleapis.com/BuildTrigger", ProjectID: "p1", DisplayName: "t1"},
			{URI: "//github.com/org/repo", AssetType: "external.github.com/Repository", ProjectID: "external", DisplayName: "repo", IsExternal: true},
			{URI: "//run.googleapis.com/projects/p1/locations/us-central1/services/app", AssetType: "run.googleapis.com/Service", ProjectID: "p1", DisplayName: "app"},
			{URI: "//sqladmin.googleapis.com/projects/p1/instances/db1", AssetType: "sqladmin.googleapis.com/Instance", ProjectID: "p1", DisplayName: "db1"},
			{URI: "//storage.googleapis.com/b1", AssetType: "storage.googleapis.com/Bucket", ProjectID: "p1", DisplayName: "b1"},
		},
		Edges: []extract.FactEdge{e1, e2, e3},
	}
}

func TestSynthesizeMultiViewAndSingleView(t *testing.T) {
	reg, err := icons.Load()
	if err != nil {
		t.Fatalf("icons.Load: %v", err)
	}
	facts := minimalTwoViewFacts()
	table := NewInternTable(facts)

	t.Run("MultiViewWithRetryRecoveryAndView0", func(t *testing.T) {
		testSynthesizeMultiViewRetryRecovery(t, facts, table, reg)
	})
	t.Run("SingleViewOmitsView0", func(t *testing.T) {
		testSynthesizeSingleViewOmitsView0(t, facts, table, reg)
	})
	t.Run("Pass1AndPass2RetryExhaustion", func(t *testing.T) {
		testSynthesizeRetryExhaustion(t, facts, table, reg)
	})
}

func mustTestRID(t *testing.T, table *InternTable, uri string) string {
	t.Helper()
	id, ok := table.ResourceID(uri)
	if !ok {
		t.Fatalf("missing resource %s", uri)
	}
	return id
}

func mustTestEID(t *testing.T, table *InternTable, facts *extract.FactsFile, idx int) string {
	t.Helper()
	id, ok := table.EdgeID(facts.Edges[idx].ID)
	if !ok {
		t.Fatalf("missing edge %d", idx)
	}
	return id
}

func testSynthesizeMultiViewRetryRecovery(t *testing.T, facts *extract.FactsFile, table *InternTable, reg *icons.Registry) {
	t.Helper()
	var pass1Calls, pass2AppCalls int
	var mu sync.Mutex

	client := &mockModelClient{handler: func(_ int, _, user string, _ *genai.Schema) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()

		if strings.Contains(user, `"adjacency"`) {
			pass1Calls++
			return mockMultiViewPass1(t, pass1Calls, user, table)
		}
		if strings.Contains(user, `"id":"app_runtime"`) {
			pass2AppCalls++
			return mockAppRuntimePass2(t, pass2AppCalls, user, table, facts)
		}
		return json.Marshal(Pass2View{
			ContainerIDs: []string{"proj_p1"},
			Placements: []Pass2Placement{
				{ComponentID: "repo", Column: 0},
				{ComponentID: "cb", ContainerID: "proj_p1", Column: 1},
				{ComponentID: "bucket", ContainerID: "proj_p1", Column: 2, OffPageRefs: []Pass2OffPageRef{{TargetViewID: "app_runtime", Label: "Runtime", Direction: "OUT"}}},
			},
			Edges: []Pass2Edge{
				{From: "repo", To: "cb", Verb: "triggers", FlowKind: manifest.FlowCICD, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{mustTestEID(t, table, facts, 0)}},
				{From: "cb", To: "bucket", Verb: "writes build", FlowKind: manifest.FlowCICD, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{mustTestEID(t, table, facts, 1)}},
			},
		})
	}}

	m, err := Synthesize(context.Background(), facts, client, reg, Options{Concurrency: 2})
	if err != nil {
		t.Fatalf("Synthesize failed: %v", err)
	}
	if len(m.Views) != 3 || m.Views[0].ID != manifest.SystemIndexViewID {
		t.Fatalf("expected 3 views with view_0_system_index at [0], got %d views (%v)", len(m.Views), m.Views[0].ID)
	}
	if pass1Calls != 2 || pass2AppCalls != 2 {
		t.Fatalf("expected retry recovery counts pass1=2, pass2App=2; got %d, %d", pass1Calls, pass2AppCalls)
	}
}

func mockMultiViewPass1(t *testing.T, attempt int, user string, table *InternTable) ([]byte, error) {
	t.Helper()
	if attempt == 1 {
		bad := Pass1Plan{
			Components: []Pass1Component{
				{ID: "repo", Title: "Repo", IconKey: "not_a_real_icon", ComponentKind: manifest.KindExternalCaller, ProjectID: "external", BackedByIDs: []string{mustTestRID(t, table, "//github.com/org/repo")}},
				{ID: "mixed_ds", Title: "Mixed", IconKey: "cloud_sql", ComponentKind: manifest.KindDatastore, ProjectID: "p1", BackedByIDs: []string{mustTestRID(t, table, "//sqladmin.googleapis.com/projects/p1/instances/db1"), mustTestRID(t, table, "//storage.googleapis.com/b1")}},
			},
			DetailViews: []Pass1DetailView{
				{ID: "cicd", Title: "CI/CD", Archetype: manifest.ArchetypeStagePipeline, AccentMode: manifest.AccentCICD, ComponentIDs: []string{"repo"}},
			},
		}
		return json.Marshal(bad)
	}
	if !strings.Contains(user, "validation_feedback") || !strings.Contains(user, "not_a_real_icon") {
		t.Errorf("expected Pass 1 retry prompt to include validation_feedback, got: %s", user)
	}
	good := Pass1Plan{
		Components: []Pass1Component{
			{ID: "repo", Title: "GitHub Repo", IconKey: "github", ComponentKind: manifest.KindExternalCaller, ProjectID: "external", BackedByIDs: []string{mustTestRID(t, table, "//github.com/org/repo")}},
			{ID: "cb", Title: "Cloud Build", IconKey: "cloud_build", ComponentKind: manifest.KindPipeline, ProjectID: "p1", BackedByIDs: []string{mustTestRID(t, table, "//cloudbuild.googleapis.com/projects/p1/locations/global/triggers/t1")}},
			{ID: "bucket", Title: "Artifacts Bucket", IconKey: "cloud_storage", ComponentKind: manifest.KindDatastore, ProjectID: "p1", BackedByIDs: []string{mustTestRID(t, table, "//storage.googleapis.com/b1")}},
			{ID: "app", Title: "Cloud Run App", IconKey: "cloud_run", ComponentKind: manifest.KindWorkload, ProjectID: "p1", BackedByIDs: []string{mustTestRID(t, table, "//run.googleapis.com/projects/p1/locations/us-central1/services/app")}},
			{ID: "db", Title: "Cloud SQL DB", IconKey: "cloud_sql", ComponentKind: manifest.KindDatastore, ProjectID: "p1", BackedByIDs: []string{mustTestRID(t, table, "//sqladmin.googleapis.com/projects/p1/instances/db1")}},
		},
		ContainerDefs: []Pass1ContainerDef{{ID: "proj_p1", Title: "Project p1", Kind: manifest.ContainerProject}},
		DetailViews: []Pass1DetailView{
			{ID: "app_runtime", Title: "App Runtime", Archetype: manifest.ArchetypeLandscapeMatrix, AccentMode: manifest.AccentRuntime, ComponentIDs: []string{"app", "db"}},
			{ID: "cicd_flow", Title: "CI/CD Flow", Archetype: manifest.ArchetypeStagePipeline, AccentMode: manifest.AccentCICD, ComponentIDs: []string{"repo", "cb", "bucket"}},
		},
	}
	return json.Marshal(good)
}

func mockAppRuntimePass2(t *testing.T, attempt int, user string, table *InternTable, facts *extract.FactsFile) ([]byte, error) {
	t.Helper()
	placements := []Pass2Placement{
		{ComponentID: "app", ContainerID: "proj_p1", Column: 1},
		{ComponentID: "db", ContainerID: "proj_p1", Column: 3},
	}
	if attempt == 1 {
		return json.Marshal(Pass2View{
			ContainerIDs: []string{"proj_p1"},
			Placements:   placements,
			Edges: []Pass2Edge{
				{From: "app", To: "db", Verb: "this verb is definitely longer than 16 chars", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{mustTestEID(t, table, facts, 0)}},
			},
		})
	}
	if !strings.Contains(user, "validation_feedback") {
		t.Errorf("expected Pass 2 retry to carry validation_feedback")
	}
	return json.Marshal(Pass2View{
		ContainerIDs: []string{"proj_p1"},
		Placements:   placements,
		Edges: []Pass2Edge{
			{From: "app", To: "db", Verb: "SQL queries", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{mustTestEID(t, table, facts, 2)}},
		},
	})
}

func testSynthesizeSingleViewOmitsView0(t *testing.T, facts *extract.FactsFile, table *InternTable, reg *icons.Registry) {
	t.Helper()
	client := &mockModelClient{handler: func(_ int, _, user string, _ *genai.Schema) ([]byte, error) {
		if strings.Contains(user, `"adjacency"`) {
			plan := Pass1Plan{
				Components: []Pass1Component{
					{ID: "repo", Title: "GitHub", IconKey: "github", ComponentKind: manifest.KindExternalCaller, ProjectID: "external", BackedByIDs: []string{mustTestRID(t, table, "//github.com/org/repo")}},
					{ID: "cb", Title: "Cloud Build", IconKey: "cloud_build", ComponentKind: manifest.KindPipeline, ProjectID: "p1", BackedByIDs: []string{mustTestRID(t, table, "//cloudbuild.googleapis.com/projects/p1/locations/global/triggers/t1")}},
					{ID: "bucket", Title: "Bucket", IconKey: "cloud_storage", ComponentKind: manifest.KindDatastore, ProjectID: "p1", BackedByIDs: []string{mustTestRID(t, table, "//storage.googleapis.com/b1")}},
					{ID: "app", Title: "App", IconKey: "cloud_run", ComponentKind: manifest.KindWorkload, ProjectID: "p1", BackedByIDs: []string{mustTestRID(t, table, "//run.googleapis.com/projects/p1/locations/us-central1/services/app")}},
					{ID: "db", Title: "DB", IconKey: "cloud_sql", ComponentKind: manifest.KindDatastore, ProjectID: "p1", BackedByIDs: []string{mustTestRID(t, table, "//sqladmin.googleapis.com/projects/p1/instances/db1")}},
				},
				DetailViews: []Pass1DetailView{
					{ID: "all_in_one", Title: "All In One", Archetype: manifest.ArchetypeLandscapeMatrix, AccentMode: manifest.AccentMixed, ComponentIDs: []string{"repo", "cb", "bucket", "app", "db"}},
				},
			}
			return json.Marshal(plan)
		}
		return json.Marshal(Pass2View{
			Placements: []Pass2Placement{
				{ComponentID: "repo", Column: 0},
				{ComponentID: "cb", Column: 1},
				{ComponentID: "bucket", Column: 3},
				{ComponentID: "app", Column: 2},
				{ComponentID: "db", Column: 4},
			},
			Edges: []Pass2Edge{
				{From: "repo", To: "cb", Verb: "triggers", FlowKind: manifest.FlowCICD, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{mustTestEID(t, table, facts, 0)}},
				{From: "cb", To: "bucket", Verb: "stores", FlowKind: manifest.FlowCICD, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{mustTestEID(t, table, facts, 1)}},
				{From: "app", To: "db", Verb: "queries", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{mustTestEID(t, table, facts, 2)}},
			},
		})
	}}

	m, err := Synthesize(context.Background(), facts, client, reg, Options{})
	if err != nil {
		t.Fatalf("Synthesize single view failed: %v", err)
	}
	if len(m.Views) != 1 || m.Views[0].ID != "all_in_one" {
		t.Fatalf("expected View 0 omitted for single detail view, got views: %+v", m.Views)
	}
	if BuildSystemIndexView(m) != nil {
		t.Fatalf("BuildSystemIndexView must return nil when < 2 detail views")
	}
}

func testSynthesizeRetryExhaustion(t *testing.T, facts *extract.FactsFile, table *InternTable, reg *icons.Registry) {
	t.Helper()
	badP1 := &mockModelClient{handler: func(int, string, string, *genai.Schema) ([]byte, error) {
		return []byte(`{"components":[],"container_defs":[],"excluded_resources":[],"detail_views":[]}`), nil
	}}
	_, err := Synthesize(context.Background(), facts, badP1, reg, Options{})
	var valErr *manifest.ValidationError
	if !errors.As(err, &valErr) || !strings.Contains(err.Error(), "pass 1 failed") {
		t.Fatalf("expected wrapped *manifest.ValidationError on Pass 1 exhaustion, got %v", err)
	}
	if badP1.calls != 3 {
		t.Fatalf("expected 3 attempts (initial + 2 retries), got %d", badP1.calls)
	}

	rpcFail := &mockModelClient{handler: func(int, string, string, *genai.Schema) ([]byte, error) {
		return nil, fmt.Errorf("vertex rpc unavailable")
	}}
	_, rpcErr := Synthesize(context.Background(), facts, rpcFail, reg, Options{})
	if rpcErr == nil || errors.As(rpcErr, &valErr) {
		t.Fatalf("expected non-ValidationError runtime error on RPC failure, got %v", rpcErr)
	}

	badP2 := &mockModelClient{handler: func(_ int, _, user string, _ *genai.Schema) ([]byte, error) {
		if strings.Contains(user, `"adjacency"`) {
			plan := Pass1Plan{
				Components: []Pass1Component{
					{ID: "repo", Title: "GitHub", IconKey: "github", ComponentKind: manifest.KindExternalCaller, ProjectID: "external", BackedByIDs: []string{mustTestRID(t, table, "//github.com/org/repo")}},
				},
				ExcludedResources: []Pass1ExcludedResource{
					{ResourceID: mustTestRID(t, table, "//cloudbuild.googleapis.com/projects/p1/locations/global/triggers/t1"), Reason: "excluded"},
					{ResourceID: mustTestRID(t, table, "//run.googleapis.com/projects/p1/locations/us-central1/services/app"), Reason: "excluded"},
					{ResourceID: mustTestRID(t, table, "//sqladmin.googleapis.com/projects/p1/instances/db1"), Reason: "excluded"},
					{ResourceID: mustTestRID(t, table, "//storage.googleapis.com/b1"), Reason: "excluded"},
				},
				DetailViews: []Pass1DetailView{
					{ID: "v1", Title: "V1", Archetype: manifest.ArchetypeHubSpoke, AccentMode: manifest.AccentRuntime, ComponentIDs: []string{"repo"}},
				},
			}
			return json.Marshal(plan)
		}
		return json.Marshal(Pass2View{
			Placements: []Pass2Placement{{ComponentID: "repo", Column: 4}},
		})
	}}
	_, err = Synthesize(context.Background(), facts, badP2, reg, Options{})
	if !errors.As(err, &valErr) || !strings.Contains(err.Error(), "pass 2 view") {
		t.Fatalf("expected wrapped *manifest.ValidationError on Pass 2 exhaustion, got %v", err)
	}
}

func TestReconcileEdgeRefreshPruneAndDeltaPatch(t *testing.T) {
	facts, expected, reg := loadAcmeFixtures(t)
	curated := buildCuratedAcmeManifest(expected)

	t.Run("ZeroLLMFastPathRefreshesChangedEdgeDetailHash", func(t *testing.T) {
		testReconcileZeroLLMHashRefresh(t, facts, curated, reg)
	})
	t.Run("PurePruningWithoutLLM", func(t *testing.T) {
		testReconcilePurePruning(t, facts, curated, reg)
	})
	t.Run("Day2ResourceAndEdgeDeltaPreservesCurationAndRejectedEdges", func(t *testing.T) {
		testReconcileDay2Delta(t, facts, curated, reg)
	})
}

func buildCuratedAcmeManifest(expected *manifest.Manifest) *manifest.Manifest {
	curated := cloneManifest(expected)
	for i := range curated.Components {
		if curated.Components[i].ID == "atlantis" {
			curated.Components[i].Title = "Atlantis Custom Curated Title"
			curated.Components[i].Notes = "Do not clobber this handoff note"
		}
	}
	for i := range curated.Views {
		if curated.Views[i].ID == "cicd_delivery_pipeline" {
			curated.Views[i].Notes = "Preserve view-level curation note"
		}
	}
	curated.RejectedEdges = []manifest.RejectedEdge{
		{From: "atlantis", To: "sonarqube_db", Reason: "Noisy monitoring correlation"},
	}
	return curated
}

func testReconcileZeroLLMHashRefresh(t *testing.T, facts *extract.FactsFile, curated *manifest.Manifest, reg *icons.Registry) {
	t.Helper()
	factsRefreshed := *facts
	factsRefreshed.Edges = append([]extract.FactEdge(nil), facts.Edges...)
	for i := range factsRefreshed.Edges {
		if factsRefreshed.Edges[i].ID == "21fd172f5bac6a44" {
			factsRefreshed.Edges[i].Detail = "10.100.0.5:5432"
			factsRefreshed.Edges[i].ID = extract.EdgeID(
				factsRefreshed.Edges[i].Kind,
				factsRefreshed.Edges[i].FromURI,
				factsRefreshed.Edges[i].ToURI,
				factsRefreshed.Edges[i].Detail,
			)
		}
	}
	res, err := Reconcile(context.Background(), &factsRefreshed, curated, nil, reg, Options{})
	if err != nil {
		t.Fatalf("expected 0-LLM hash refresh to succeed with nil client, got: %v", err)
	}
	if err := manifest.ValidateWithFacts(res, &factsRefreshed, reg); err != nil {
		t.Fatalf("refreshed manifest invalid: %v", err)
	}
}

func testReconcilePurePruning(t *testing.T, facts *extract.FactsFile, curated *manifest.Manifest, reg *icons.Registry) {
	t.Helper()
	prunedFacts := *facts
	prunedFacts.Resources = nil
	for _, r := range facts.Resources {
		if r.URI != "//storage.googleapis.com/acme-cicd_cloudbuild" {
			prunedFacts.Resources = append(prunedFacts.Resources, r)
		}
	}
	prunedFacts.Edges = nil
	for _, e := range facts.Edges {
		if e.ToURI != "//storage.googleapis.com/acme-cicd_cloudbuild" {
			prunedFacts.Edges = append(prunedFacts.Edges, e)
		}
	}

	res, err := Reconcile(context.Background(), &prunedFacts, curated, nil, reg, Options{GeneratedAt: "2026-11-24T00:00:00Z"})
	if err != nil {
		t.Fatalf("pure pruning failed: %v", err)
	}
	for _, c := range res.Components {
		if c.ID == "cloudbuild_staging_bucket" {
			t.Fatalf("expected vanished component cloudbuild_staging_bucket to be pruned")
		}
	}
}

func testReconcileDay2Delta(t *testing.T, facts *extract.FactsFile, curated *manifest.Manifest, reg *icons.Registry) {
	t.Helper()
	deltaFacts, webhookRID, topicRID, rejectedEID, cbToTopicEID, topicToWebhookEID := buildDay2DeltaFacts(facts)

	if _, err := Reconcile(context.Background(), &deltaFacts, curated, nil, reg, Options{}); err == nil {
		t.Fatalf("expected error when delta exists and client == nil")
	}

	client := &mockModelClient{handler: func(call int, _, user string, _ *genai.Schema) ([]byte, error) {
		if !strings.Contains(user, cbToTopicEID) || !strings.Contains(user, topicToWebhookEID) {
			t.Errorf("reconcile prompt must include edges incident to unclaimed resources (%s, %s); got: %s", cbToTopicEID, topicToWebhookEID, user)
		}
		if strings.Contains(user, rejectedEID) {
			t.Errorf("reconcile prompt must exclude rejected edge handle %s; got: %s", rejectedEID, user)
		}
		if call == 0 {
			return json.Marshal(ReconcilePatch{
				NewEdges: []ReconcileNewEdge{
					{ViewID: "gke_developer_platform", Edge: Pass2Edge{
						From: "atlantis", To: "sonarqube_db", Verb: "bad", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceInferred, InferenceRationale: "bad",
					}},
				},
			})
		}
		return json.Marshal(ReconcilePatch{
			NewComponents: []Pass1Component{
				{ID: "build_events", Title: "Build Events Topic", Subtitle: "Pub/Sub topic", IconKey: "pubsub", ComponentKind: manifest.KindMessaging, ProjectID: "acme-cicd", BackedByIDs: []string{topicRID}},
				{ID: "release_webhook", Title: "Release Webhook", Subtitle: "Cloud Run service", IconKey: "cloud_run", ComponentKind: manifest.KindWorkload, ProjectID: "acme-cicd", BackedByIDs: []string{webhookRID}},
			},
			NewPlacements: []ReconcileNewPlacement{
				{ViewID: "cicd_delivery_pipeline", Placement: Pass2Placement{ComponentID: "build_events", ContainerID: "proj_acme_cicd", Column: 2}},
				{ViewID: "cicd_delivery_pipeline", Placement: Pass2Placement{ComponentID: "release_webhook", ContainerID: "proj_acme_cicd", Column: 3}},
			},
			NewEdges: []ReconcileNewEdge{
				{ViewID: "cicd_delivery_pipeline", Edge: Pass2Edge{
					From: "cloudbuild_triggers", To: "build_events", Verb: "publishes", FlowKind: manifest.FlowCICD, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{cbToTopicEID},
				}},
				{ViewID: "cicd_delivery_pipeline", Edge: Pass2Edge{
					From: "build_events", To: "release_webhook", Verb: "notifies", FlowKind: manifest.FlowCICD, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{topicToWebhookEID},
				}},
			},
		})
	}}

	updated, err := Reconcile(context.Background(), &deltaFacts, curated, client, reg, Options{GeneratedAt: "2026-11-24T01:00:00Z"})
	if err != nil {
		t.Fatalf("Reconcile delta failed: %v", err)
	}
	if client.calls != 2 {
		t.Fatalf("expected 2 reconcile attempts (1 retry), got %d", client.calls)
	}
	assertDay2CurationPreserved(t, updated)
}

func buildDay2DeltaFacts(facts *extract.FactsFile) (extract.FactsFile, string, string, string, string, string) {
	deltaFacts := *facts
	deltaFacts.Resources = append(append([]extract.FactResource(nil), facts.Resources...),
		extract.FactResource{
			URI: "//run.googleapis.com/projects/acme-cicd/locations/europe-southwest1/services/release-webhook", AssetType: "run.googleapis.com/Service", ProjectID: "acme-cicd", Location: "europe-southwest1", DisplayName: "release-webhook",
		},
		extract.FactResource{
			URI: "//pubsub.googleapis.com/projects/acme-cicd/topics/build-events", AssetType: "pubsub.googleapis.com/Topic", ProjectID: "acme-cicd", DisplayName: "build-events",
		},
	)

	rejectedFactEdge := extract.FactEdge{
		FromURI: "//container.googleapis.com/projects/acme-cicd/locations/europe-southwest1-b/clusters/cicd/k8s/namespaces/atlantis/apps/deployments/atlantis",
		ToURI:   "//sqladmin.googleapis.com/projects/acme-cicd/instances/sonarqube",
		Kind:    extract.EdgeMonitoringTraffic,
		Detail:  "noisy",
	}
	rejectedFactEdge.ID = extract.EdgeID(rejectedFactEdge.Kind, rejectedFactEdge.FromURI, rejectedFactEdge.ToURI, rejectedFactEdge.Detail)

	cbToTopic := extract.FactEdge{
		FromURI: "//cloudbuild.googleapis.com/projects/acme-cicd/locations/europe-southwest1/triggers/build-api-prod",
		ToURI:   "//pubsub.googleapis.com/projects/acme-cicd/topics/build-events",
		Kind:    extract.EdgeConfigRef,
		Detail:  "pubsub_topic",
	}
	cbToTopic.ID = extract.EdgeID(cbToTopic.Kind, cbToTopic.FromURI, cbToTopic.ToURI, cbToTopic.Detail)

	topicToWebhook := extract.FactEdge{
		FromURI: "//pubsub.googleapis.com/projects/acme-cicd/topics/build-events",
		ToURI:   "//run.googleapis.com/projects/acme-cicd/locations/europe-southwest1/services/release-webhook",
		Kind:    extract.EdgeConfigRef,
		Detail:  "push_subscription",
	}
	topicToWebhook.ID = extract.EdgeID(topicToWebhook.Kind, topicToWebhook.FromURI, topicToWebhook.ToURI, topicToWebhook.Detail)

	deltaFacts.Edges = append(append([]extract.FactEdge(nil), facts.Edges...), rejectedFactEdge, cbToTopic, topicToWebhook)
	table := NewInternTable(&deltaFacts)
	webhookRID, _ := table.ResourceID("//run.googleapis.com/projects/acme-cicd/locations/europe-southwest1/services/release-webhook")
	topicRID, _ := table.ResourceID("//pubsub.googleapis.com/projects/acme-cicd/topics/build-events")
	rejectedEID, _ := table.EdgeID(rejectedFactEdge.ID)
	cbToTopicEID, _ := table.EdgeID(cbToTopic.ID)
	topicToWebhookEID, _ := table.EdgeID(topicToWebhook.ID)
	return deltaFacts, webhookRID, topicRID, rejectedEID, cbToTopicEID, topicToWebhookEID
}

func findTestComponent(t *testing.T, components []manifest.Component, id string) manifest.Component {
	t.Helper()
	for _, c := range components {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("%s missing after reconcile", id)
	return manifest.Component{}
}

func findTestView(t *testing.T, views []manifest.View, id string) manifest.View {
	t.Helper()
	for _, v := range views {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("%s missing after reconcile", id)
	return manifest.View{}
}

func assertDay2CurationPreserved(t *testing.T, updated *manifest.Manifest) {
	t.Helper()
	atlantis := findTestComponent(t, updated.Components, "atlantis")
	if atlantis.Title != "Atlantis Custom Curated Title" || atlantis.Notes != "Do not clobber this handoff note" {
		t.Fatalf("curation clobbered on atlantis: %+v", atlantis)
	}
	cicdView := findTestView(t, updated.Views, "cicd_delivery_pipeline")
	if cicdView.Notes != "Preserve view-level curation note" {
		t.Fatalf("View.Notes clobbered on cicd_delivery_pipeline: %q", cicdView.Notes)
	}
	if len(updated.RejectedEdges) != 1 || updated.RejectedEdges[0].From != "atlantis" {
		t.Fatalf("rejected_edges not preserved: %+v", updated.RejectedEdges)
	}
	if updated.Views[0].ID != manifest.SystemIndexViewID {
		t.Fatalf("expected View 0 recomputed at views[0], got %q", updated.Views[0].ID)
	}
}

func TestVertexClientAndModelProbeOverHTTP(t *testing.T) {
	var seenPaths []string
	var seenBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPaths = append(seenPaths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, ":generateContent") {
			body, _ := io.ReadAll(r.Body)
			seenBody = string(body)
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"{\"ok\":true}"}]}}]}`))
			return
		}
		// Models.Get response.
		_, _ = w.Write([]byte(`{"name":"publishers/google/models/gemini-4-carbon","displayName":"Gemini 4 Carbon"}`))
	}))
	defer srv.Close()

	// Empty project must fail fast.
	if _, err := NewVertexClient(context.Background(), VertexClientOptions{}); err == nil {
		t.Fatalf("expected error for empty Vertex project")
	}

	vc, err := NewVertexClient(context.Background(), VertexClientOptions{
		Project:    "ai-billing-proj",
		HTTPClient: srv.Client(),
		BaseURL:    srv.URL + "/",
	})
	if err != nil {
		t.Fatalf("NewVertexClient: %v", err)
	}
	if vc.model != DefaultVertexModel {
		t.Fatalf("default model = %q, want %q", vc.model, DefaultVertexModel)
	}

	raw, err := vc.GenerateJSON(context.Background(), "system prompt", "user prompt", &genai.Schema{Type: genai.TypeObject})
	if err != nil {
		t.Fatalf("GenerateJSON: %v", err)
	}
	if string(raw) != `{"ok":true}` {
		t.Fatalf("unexpected GenerateJSON output: %s", raw)
	}
	if !strings.Contains(seenBody, `"responseMimeType":"application/json"`) || !strings.Contains(seenBody, `"temperature":0.1`) {
		t.Fatalf("expected application/json and temperature 0.1 in request body: %s", seenBody)
	}
	if strings.Contains(seenBody, `"thinkingConfig"`) {
		t.Fatalf("expected thinkingConfig omitted when ThinkingLevel is unset, got: %s", seenBody)
	}

	// ThinkingLevel parsing and ThinkingConfig payload forwarding.
	levelCases := []struct {
		raw  string
		want genai.ThinkingLevel
	}{
		{raw: "", want: ""},
		{raw: "minimal", want: genai.ThinkingLevelMinimal},
		{raw: "LOW", want: genai.ThinkingLevelLow},
		{raw: " Medium ", want: genai.ThinkingLevelMedium},
		{raw: "high", want: genai.ThinkingLevelHigh},
		{raw: "xhigh", want: genai.ThinkingLevelXhigh},
	}
	for _, tc := range levelCases {
		got, err := ParseThinkingLevel(tc.raw)
		if err != nil || got != tc.want {
			t.Fatalf("ParseThinkingLevel(%q) = (%q, %v), want (%q, nil)", tc.raw, got, err, tc.want)
		}
	}
	if _, err := ParseThinkingLevel("turbo"); err == nil {
		t.Fatalf("expected error for invalid thinking level")
	}
	if _, err := NewVertexClient(context.Background(), VertexClientOptions{
		Project:       "ai-billing-proj",
		ThinkingLevel: "turbo",
		HTTPClient:    srv.Client(),
		BaseURL:       srv.URL + "/",
	}); err == nil {
		t.Fatalf("expected NewVertexClient error for invalid thinking level")
	}

	vcHigh, err := NewVertexClient(context.Background(), VertexClientOptions{
		Project:       "ai-billing-proj",
		ThinkingLevel: "high",
		HTTPClient:    srv.Client(),
		BaseURL:       srv.URL + "/",
	})
	if err != nil {
		t.Fatalf("NewVertexClient(high): %v", err)
	}
	seenBody = ""
	if _, err := vcHigh.GenerateJSON(context.Background(), "system prompt", "user prompt", &genai.Schema{Type: genai.TypeObject}); err != nil {
		t.Fatalf("GenerateJSON(high): %v", err)
	}
	if !strings.Contains(seenBody, `"thinkingConfig":{"thinkingLevel":"HIGH"}`) {
		t.Fatalf("expected thinkingConfig HIGH in request body: %s", seenBody)
	}

	if err := probeVertexModelWithOptions(context.Background(), VertexClientOptions{
		Project:    "ai-billing-proj",
		Location:   "global",
		Model:      "gemini-4-carbon",
		HTTPClient: srv.Client(),
		BaseURL:    srv.URL + "/",
	}); err != nil {
		t.Fatalf("probeVertexModelWithOptions failed: %v", err)
	}
	if len(seenPaths) != 3 {
		t.Fatalf("expected 3 Vertex HTTP requests, got %v", seenPaths)
	}

	// ProbeVertexModel with empty project fails fast.
	if err := ProbeVertexModel(context.Background(), "", "global", "gemini-4-carbon"); err == nil {
		t.Fatalf("expected error from ProbeVertexModel with empty project")
	}

	// Empty JSON text & HTTP error responses.
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "empty") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"   "}]}}]}`))
			return
		}
		http.Error(w, `{"error":{"code":500,"message":"boom"}}`, http.StatusInternalServerError)
	}))
	defer errSrv.Close()

	emptyClient, err := NewVertexClient(context.Background(), VertexClientOptions{
		Project: "p", Model: "empty", HTTPClient: errSrv.Client(), BaseURL: errSrv.URL + "/",
	})
	if err != nil {
		t.Fatalf("NewVertexClient empty: %v", err)
	}
	if _, err := emptyClient.GenerateJSON(context.Background(), "s", "u", &genai.Schema{Type: genai.TypeObject}); err == nil {
		t.Fatalf("expected empty JSON response error")
	}

	failClient, err := NewVertexClient(context.Background(), VertexClientOptions{
		Project: "p", Model: "fail", HTTPClient: errSrv.Client(), BaseURL: errSrv.URL + "/",
	})
	if err != nil {
		t.Fatalf("NewVertexClient fail: %v", err)
	}
	if _, err := failClient.GenerateJSON(context.Background(), "s", "u", &genai.Schema{Type: genai.TypeObject}); err == nil {
		t.Fatalf("expected HTTP 500 error from GenerateJSON")
	}
	if err := probeVertexModelWithOptions(context.Background(), VertexClientOptions{
		Project: "p", Model: "fail", HTTPClient: errSrv.Client(), BaseURL: errSrv.URL + "/",
	}); err == nil {
		t.Fatalf("expected HTTP 500 error from probeVertexModelWithOptions")
	}
}

func TestReconcileAllPatchOperationsAndEdgeOnlyDelta(t *testing.T) {
	facts, expected, reg := loadAcmeFixtures(t)

	// 1. Edge-only delta (zero new resources): add a new CONFIG_REF from atlantis -> acme_artifact_registry.
	edgeOnlyFacts := *facts
	newEdge := extract.FactEdge{
		FromURI: "//container.googleapis.com/projects/acme-cicd/locations/europe-southwest1-b/clusters/cicd/k8s/namespaces/atlantis/apps/deployments/atlantis",
		ToURI:   "//artifactregistry.googleapis.com/projects/acme-cicd/locations/europe-southwest1/repositories/acme-apps",
		Kind:    extract.EdgeConfigRef,
		Detail:  "container_image",
	}
	newEdge.ID = extract.EdgeID(newEdge.Kind, newEdge.FromURI, newEdge.ToURI, newEdge.Detail)
	edgeOnlyFacts.Edges = append(append([]extract.FactEdge(nil), facts.Edges...), newEdge)
	table1 := NewInternTable(&edgeOnlyFacts)
	newEID, _ := table1.EdgeID(newEdge.ID)

	edgeClient := &mockModelClient{handler: func(call int, sys, user string, schema *genai.Schema) ([]byte, error) {
		if !strings.Contains(user, newEID) {
			t.Errorf("expected delta edge %q in reconcile prompt", newEID)
		}
		patch := ReconcilePatch{
			NewPlacements: []ReconcileNewPlacement{
				{ViewID: "gke_developer_platform", Placement: Pass2Placement{ComponentID: "acme_artifact_registry", ContainerID: "proj_acme_cicd", Column: 2}},
			},
			NewEdges: []ReconcileNewEdge{
				{ViewID: "gke_developer_platform", Edge: Pass2Edge{
					From: "atlantis", To: "acme_artifact_registry", Verb: "pulls image", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{newEID},
				}},
			},
		}
		return json.Marshal(patch)
	}}
	res1, err := Reconcile(context.Background(), &edgeOnlyFacts, expected, edgeClient, reg, Options{})
	if err != nil {
		t.Fatalf("edge-only reconcile failed: %v", err)
	}
	if edgeClient.calls != 1 {
		t.Fatalf("expected 1 reconcile LLM call, got %d", edgeClient.calls)
	}
	if err := manifest.ValidateWithFacts(res1, &edgeOnlyFacts, reg); err != nil {
		t.Fatalf("edge-only result invalid: %v", err)
	}

	// 2. Full patch operations: append_backed_by, append_container_backed_by, new_containers,
	// new_excluded_resources, and overflow new_views.
	richFacts := *facts
	richFacts.Resources = append(append([]extract.FactResource(nil), facts.Resources...),
		extract.FactResource{URI: "//secretmanager.googleapis.com/projects/acme-cicd/secrets/extra-secret", AssetType: "secretmanager.googleapis.com/Secret", ProjectID: "acme-cicd", DisplayName: "extra-secret"},
		extract.FactResource{URI: "//compute.googleapis.com/projects/acme-cicd/regions/europe-southwest1/subnetworks/extra-subnet", AssetType: "compute.googleapis.com/Subnetwork", ProjectID: "acme-cicd", DisplayName: "extra-subnet"},
		extract.FactResource{URI: "//run.googleapis.com/projects/acme-cicd/locations/europe-southwest1/services/overflow-svc", AssetType: "run.googleapis.com/Service", ProjectID: "acme-cicd", DisplayName: "overflow-svc"},
		extract.FactResource{URI: "//logging.googleapis.com/projects/acme-cicd/sinks/audit-sink", AssetType: "logging.googleapis.com/LogSink", ProjectID: "acme-cicd", DisplayName: "audit-sink"},
	)
	table2 := NewInternTable(&richFacts)
	secRID, _ := table2.ResourceID("//secretmanager.googleapis.com/projects/acme-cicd/secrets/extra-secret")
	subRID, _ := table2.ResourceID("//compute.googleapis.com/projects/acme-cicd/regions/europe-southwest1/subnetworks/extra-subnet")
	svcRID, _ := table2.ResourceID("//run.googleapis.com/projects/acme-cicd/locations/europe-southwest1/services/overflow-svc")
	sinkRID, _ := table2.ResourceID("//logging.googleapis.com/projects/acme-cicd/sinks/audit-sink")

	richClient := &mockModelClient{handler: func(call int, sys, user string, schema *genai.Schema) ([]byte, error) {
		patch := ReconcilePatch{
			AppendBackedBy:          []ReconcileAppendBackedBy{{ComponentID: "atlantis", ResourceIDs: []string{secRID}}},
			AppendContainerBackedBy: []ReconcileAppendContainerBackedBy{{ContainerID: "gke_cicd", ResourceIDs: []string{subRID}}},
			NewContainers:           []Pass1ContainerDef{{ID: "overflow_zone", Title: "Overflow Zone", Kind: manifest.ContainerProject}},
			NewExcludedResources:    []Pass1ExcludedResource{{ResourceID: sinkRID, Reason: "Audit sink omitted from architecture diagram"}},
			NewComponents: []Pass1Component{
				{ID: "overflow_svc", Title: "Overflow Service", IconKey: "cloud_run", ComponentKind: manifest.KindWorkload, ProjectID: "acme-cicd", BackedByIDs: []string{svcRID}},
			},
			NewViews: []ReconcileNewView{
				{
					ID:           "overflow_workloads",
					Title:        "Overflow Workloads",
					Archetype:    manifest.ArchetypeLandscapeMatrix,
					AccentMode:   manifest.AccentRuntime,
					ContainerIDs: []string{"overflow_zone"},
					Placements:   []Pass2Placement{{ComponentID: "overflow_svc", ContainerID: "overflow_zone", Column: 2}},
				},
			},
		}
		return json.Marshal(patch)
	}}
	res2, err := Reconcile(context.Background(), &richFacts, expected, richClient, reg, Options{})
	if err != nil {
		t.Fatalf("rich patch reconcile failed: %v", err)
	}
	if len(res2.Views) != 5 || res2.Views[0].ID != manifest.SystemIndexViewID {
		t.Fatalf("expected 5 views (View 0 + 4 detail views), got %d", len(res2.Views))
	}

	// 3. Invalid patch merge issues & retry exhaustion.
	badClient := &mockModelClient{handler: func(int, string, string, *genai.Schema) ([]byte, error) {
		return json.Marshal(ReconcilePatch{
			AppendBackedBy:          []ReconcileAppendBackedBy{{ComponentID: "nope", ResourceIDs: []string{"r_9999"}}},
			AppendContainerBackedBy: []ReconcileAppendContainerBackedBy{{ContainerID: "nope", ResourceIDs: []string{"r_9999"}}},
			NewComponents:           []Pass1Component{{ID: "atlantis", BackedByIDs: []string{"r_9999"}}},
			NewContainers:           []Pass1ContainerDef{{ID: "gke_cicd", BackedByIDs: []string{"r_9999"}}},
			NewExcludedResources:    []Pass1ExcludedResource{{ResourceID: "r_9999", Reason: "bad"}},
			NewPlacements:           []ReconcileNewPlacement{{ViewID: manifest.SystemIndexViewID}},
			NewEdges:                []ReconcileNewEdge{{ViewID: "missing_view"}},
			NewViews:                []ReconcileNewView{{ID: manifest.SystemIndexViewID}, {ID: "cicd_delivery_pipeline"}},
		})
	}}
	_, err = Reconcile(context.Background(), &richFacts, expected, badClient, reg, Options{})
	var valErr *manifest.ValidationError
	if !errors.As(err, &valErr) || !strings.Contains(err.Error(), "reconcile failed") {
		t.Fatalf("expected wrapped *manifest.ValidationError on reconcile exhaustion, got %v", err)
	}
}

func TestValidationHelpersAndView0EdgeCases(t *testing.T) {
	reg, err := icons.Load()
	if err != nil {
		t.Fatalf("icons.Load: %v", err)
	}
	facts := minimalTwoViewFacts()
	facts.Resources[2].ParentURI = "//storage.googleapis.com/b1"
	table := NewInternTable(facts)

	// Exercise validatePass1Plan edge cases: container cycle, depth > 2, duplicate IDs, unknown handles, unplaced component.
	badPlan := &Pass1Plan{
		Components: []Pass1Component{
			{ID: "", Title: "", IconKey: "github", BackedByIDs: nil},
			{ID: "c1", Title: "", IconKey: "bad", BackedByIDs: []string{"r_9999"}, InlineMeta: make([]Pass1InlineMeta, 5)},
			{ID: "c1", Title: "Dup", IconKey: "github", BackedByIDs: []string{"r_0001"}},
		},
		ContainerDefs: []Pass1ContainerDef{
			{ID: "", Title: "Empty", Kind: manifest.ContainerProject},
			{ID: "a", Title: "A", Kind: manifest.ContainerProject, ParentContainerID: "b", BackedByIDs: []string{"r_9999"}},
			{ID: "b", Title: "B", Kind: manifest.ContainerVPC, ParentContainerID: "a"},
			{ID: "b", Title: "DupB", Kind: manifest.ContainerVPC},
			{ID: "d1", Title: "D1", Kind: manifest.ContainerProject},
			{ID: "d2", Title: "D2", Kind: manifest.ContainerVPC, ParentContainerID: "d1"},
			{ID: "d3", Title: "D3", Kind: manifest.ContainerGKECluster, ParentContainerID: "d2"},
			{ID: "orphan_parent", Title: "O", Kind: manifest.ContainerVPC, ParentContainerID: "missing"},
		},
		ExcludedResources: []Pass1ExcludedResource{
			{ResourceID: "r_9999", Reason: "bad"},
			{ResourceID: "r_0002", Reason: ""},
		},
		DetailViews: []Pass1DetailView{
			{ID: manifest.SystemIndexViewID, ComponentIDs: nil},
			{ID: "v1", ComponentIDs: []string{"unknown_comp"}},
			{ID: "v1", ComponentIDs: []string{"c1"}},
		},
	}
	issues := validatePass1Plan(badPlan, table, reg)
	if len(issues) < 10 {
		t.Fatalf("expected many Pass 1 validation issues, got %d: %v", len(issues), issues)
	}

	// Exercise validatePopulatedView edge cases: depth > 2, unknown container/component, duplicate placement,
	// bad off-page ref, missing locked component, mixed via_bus_id, INFERRED without rationale, unknown edge handle.
	compByID := map[string]manifest.Component{
		"c1": {ID: "c1", BackedBy: []manifest.BackedResource{{URI: facts.Resources[0].URI}}},
		"c2": {ID: "c2", BackedBy: []manifest.BackedResource{{URI: facts.Resources[1].URI}}},
		"c3": {ID: "c3", BackedBy: []manifest.BackedResource{{URI: facts.Resources[2].URI}}},
		"c4": {ID: "c4", BackedBy: []manifest.BackedResource{{URI: facts.Resources[3].URI}}},
	}
	contByID := map[string]manifest.Container{
		"d1": {ID: "d1"},
		"d2": {ID: "d2", ParentContainerID: "d1"},
		"d3": {ID: "d3", ParentContainerID: "d2"},
	}
	_, convIssues := materializeEdge(Pass2Edge{From: "c1", To: "c2", EvidenceEdgeIDs: []string{"e_9999"}}, table)
	if len(convIssues) == 0 {
		t.Fatalf("expected unknown evidence edge handle issue")
	}
	pvIssues := validatePopulatedView(manifest.View{
		ID:           "v1",
		Archetype:    manifest.ArchetypeHubSpoke,
		ContainerIDs: []string{"d1", "d2", "d3", "unknown_cont"},
		Placements: []manifest.Placement{
			{ComponentID: "c1", ContainerID: "not_in_view", Column: 5, OffPageRefs: []manifest.OffPageRef{{TargetViewID: "v1"}}},
			{ComponentID: "c1", Column: 0},
			{ComponentID: "c2", Column: 1},
			{ComponentID: "c3", Column: 1},
			{ComponentID: "c4", Column: 2},
			{ComponentID: "missing_comp", Column: 0},
		},
		Edges: []manifest.Edge{
			{From: "c1", To: "c2", Verb: "v", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceVerified, ViaBusID: "mix"},
			{From: "c3", To: "c4", Verb: "v", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceInferred, ViaBusID: "mix"},
			{From: "c1", To: "unplaced", Verb: "", FlowKind: manifest.FlowRuntime, Provenance: manifest.Provenance("BAD")},
		},
	}, []string{"c1", "locked_missing"}, compByID, contByID, nil, map[string]bool{"v1": true})
	if len(pvIssues) < 8 {
		t.Fatalf("expected many Pass 2 validation issues, got %d: %v", len(pvIssues), pvIssues)
	}

	// Exercise BuildPass2UserPrompt with cross-view IN/OUT neighbors, shared components, and container bridge IDs.
	plan := &Pass1Plan{
		Components: []Pass1Component{
			{ID: "repo", BackedByIDs: []string{"r_0002"}},
			{ID: "bucket", BackedByIDs: []string{"r_0005"}},
		},
		ContainerDefs: []Pass1ContainerDef{
			{ID: "cb_cont", Title: "CB", Kind: manifest.ContainerSharedHub, BackedByIDs: []string{"r_0001"}},
		},
		DetailViews: []Pass1DetailView{
			{ID: "v1", Archetype: manifest.ArchetypeStagePipeline, ComponentIDs: []string{"repo"}},
			{ID: "v2", Archetype: manifest.ArchetypeLandscapeMatrix, ComponentIDs: []string{"repo", "bucket"}},
		},
	}
	p2Prompt, err := BuildPass2UserPrompt(plan.DetailViews[1], plan, table, nil)
	if err != nil || !strings.Contains(p2Prompt, "cross_view_neighbors") || !strings.Contains(p2Prompt, `"r_0001"`) {
		t.Fatalf("BuildPass2UserPrompt cross-view neighbors & container bridge failed: %v, prompt=%s", err, p2Prompt)
	}

	// Exercise BuildSystemIndexView: shared components, RUNTIME/ADMIN verbs, rejected edges, truncation.
	m := &manifest.Manifest{
		Components: []manifest.Component{
			{ID: "ext_in", ComponentKind: manifest.KindExternalCaller},
			{ID: "ext_admin", ComponentKind: manifest.KindExternalCaller},
			{ID: "shared_app", ComponentKind: manifest.KindWorkload},
			{ID: "ext_out_rt", ComponentKind: manifest.KindExternalTarget},
			{ID: "ext_out_adm", ComponentKind: manifest.KindExternalTarget},
		},
		RejectedEdges: []manifest.RejectedEdge{{From: "ext_admin", To: "v_a", Reason: "blocked"}},
		Views: []manifest.View{
			{
				ID: "v_a", Title: "View A", Archetype: manifest.ArchetypeLandscapeMatrix, AccentMode: manifest.AccentRuntime,
				Placements: []manifest.Placement{
					{ComponentID: "ext_in", Column: 0},
					{ComponentID: "ext_admin", Column: 0},
					{ComponentID: "shared_app", Column: 2, OffPageRefs: []manifest.OffPageRef{{TargetViewID: "v_b", Label: "very long off page ref label exceeding sixteen chars", Direction: "OUT"}}},
					{ComponentID: "ext_out_rt", Column: 3},
				},
				Edges: []manifest.Edge{
					{From: "ext_in", To: "shared_app", Verb: "calls", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceVerified},
					{From: "ext_admin", To: "shared_app", Verb: "admin", FlowKind: manifest.FlowAdmin, Provenance: manifest.ProvenanceVerified},
					{From: "shared_app", To: "ext_out_rt", Verb: "sends", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceVerified},
				},
			},
			{
				ID: "v_b", Title: "View B", Archetype: manifest.ArchetypeStagePipeline, AccentMode: manifest.AccentCICD,
				Placements: []manifest.Placement{
					{ComponentID: "shared_app", Column: 1},
					{ComponentID: "ext_out_adm", Column: 3},
				},
				Edges: []manifest.Edge{
					{From: "shared_app", To: "ext_out_adm", Verb: "config", FlowKind: manifest.FlowAdmin, Provenance: manifest.ProvenanceVerified},
				},
			},
			{
				ID: "v_c", Title: "View C", Archetype: manifest.ArchetypeHubSpoke, AccentMode: manifest.AccentRuntime,
				Placements: []manifest.Placement{{ComponentID: "shared_app", Column: 1}},
			},
		},
	}
	v0 := BuildSystemIndexView(m)
	if v0 == nil {
		t.Fatalf("expected non-nil View 0")
	}
	for _, e := range v0.Edges {
		if e.From == "ext_admin" && e.To == "v_a" {
			t.Fatalf("rejected edge leaked into View 0")
		}
		if len([]rune(e.Verb)) > 16 {
			t.Fatalf("View 0 verb exceeded 16 chars: %q", e.Verb)
		}
	}
	if BuildSystemIndexView(nil) != nil {
		t.Fatalf("expected nil for nil manifest")
	}
	if callerVerb(manifest.FlowAdmin) == "" || targetVerb(manifest.FlowAdmin) == "" || truncateVerb("") == "" {
		t.Fatalf("unexpected empty helper verbs")
	}
}
