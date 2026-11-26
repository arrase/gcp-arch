package manifest

import (
	"slices"
	"strings"
	"testing"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/icons"
)

func findComponent(m *Manifest, id string) *Component {
	for i := range m.Components {
		if m.Components[i].ID == id {
			return &m.Components[i]
		}
	}
	return nil
}

func addFactEdge(facts *extract.FactsFile, kind extract.FactEdgeKind, from, to, detail string, heuristic bool) extract.FactEdge {
	fe := extract.FactEdge{
		ID:        extract.EdgeID(kind, from, to, detail),
		FromURI:   from,
		ToURI:     to,
		Kind:      kind,
		Detail:    detail,
		Heuristic: heuristic,
	}
	facts.Edges = append(facts.Edges, fe)
	return fe
}

func TestValidateAccountingRules(t *testing.T) {
	reg := loadTestIcons(t)

	t.Run("excluded datastore", func(t *testing.T) {
		m, _ := baseFixture()
		m.ExcludedResources = append(m.ExcludedResources, ExcludedResource{
			URI: "//storage.googleapis.com/orphan", AssetType: "storage.googleapis.com/Bucket", ProjectID: "acme-cicd", Reason: "unused",
		})
		_ = assertHasValidationCode(t, ValidateStructure(m, reg), ErrExcludedDatastore)
	})

	t.Run("duplicate accounted uri", func(t *testing.T) {
		m, _ := baseFixture()
		vpc := m.Containers[0].BackedBy[0]
		m.ExcludedResources = append(m.ExcludedResources, ExcludedResource{URI: vpc.URI, AssetType: vpc.AssetType, ProjectID: vpc.ProjectID, Reason: "dup"})
		ve := assertHasValidationCode(t, ValidateStructure(m, reg), ErrDuplicateAccountedURI)
		if !strings.Contains(ve.Error(), "container vpc_cicd") {
			t.Fatalf("expected owners in message, got %v", ve)
		}
	})

	t.Run("unknown pair components", func(t *testing.T) {
		m, _ := baseFixture()
		m.RejectedEdges = append(m.RejectedEdges, RejectedEdge{From: "ghost", To: "atlantis", Reason: "stale"})
		m.AcknowledgedEdges = []ComponentPair{{From: "atlantis", To: "phantom"}}
		ve := assertHasValidationCode(t, ValidateStructure(m, reg), ErrUnknownPairComponent)
		if !strings.Contains(ve.Error(), "ghost") || !strings.Contains(ve.Error(), "phantom") {
			t.Fatalf("expected both dangling pairs reported, got %v", ve)
		}
	})
}

func TestValidateViewIndexRules(t *testing.T) {
	reg := loadTestIcons(t)

	t.Run("view id collides with component id", func(t *testing.T) {
		m, _ := baseFixture()
		m.Views[0].ID = "atlantis"
		_ = assertHasValidationCode(t, ValidateStructure(m, reg), ErrViewComponentIDCollision)
	})

	t.Run("too many detail views", func(t *testing.T) {
		m, _ := baseFixture()
		base := m.Views[0]
		m.Views = nil
		for i := range maxDetailViews + 1 {
			v := base
			v.ID = "view_" + string(rune('a'+i))
			m.Views = append(m.Views, v)
		}
		_ = assertHasValidationCode(t, ValidateStructure(m, reg), ErrTooManyDetailViews)
	})

	t.Run("inconsistent cross-view edge", func(t *testing.T) {
		m, _ := baseFixture()
		second := m.Views[0]
		second.ID = "second"
		second.Edges = []Edge{{From: "sonarqube", To: "sonarqube_db", Verb: "SQL", FlowKind: FlowRuntime, Provenance: ProvenanceInferred, InferenceRationale: "guess"}}
		m.Views = append(m.Views, second)
		_ = assertHasValidationCode(t, ValidateStructure(m, reg), ErrInconsistentCrossViewEdge)
	})
}

func TestValidateEvidenceRules(t *testing.T) {
	reg := loadTestIcons(t)

	t.Run("heuristic evidence cannot verify", func(t *testing.T) {
		m, facts := baseFixture()
		for i := range facts.Edges {
			if facts.Edges[i].ToURI == findComponent(m, "sonarqube_db").BackedBy[0].URI {
				facts.Edges[i].Heuristic = true
			}
		}
		ve := assertHasValidationCode(t, ValidateWithFacts(m, facts, reg), ErrInvalidEdgeProvenance)
		if !strings.Contains(ve.Error(), "heuristic") {
			t.Fatalf("expected heuristic message, got %v", ve)
		}
	})

	t.Run("walk may not shortcut another component", func(t *testing.T) {
		m, facts := baseFixture()
		sonar := findComponent(m, "sonarqube").BackedBy[0].URI
		atl := findComponent(m, "atlantis").BackedBy[0].URI
		hop := addFactEdge(facts, extract.EdgeConfigRef, sonar, atl, "http://atlantis", false)
		state := findEdgeEvidence(m.Views[0].Edges, "atlantis", "tf_buckets")[0]
		m.Views[0].Edges = append(m.Views[0].Edges, Edge{
			From: "sonarqube", To: "tf_buckets", Verb: "reads state", FlowKind: FlowRuntime,
			Provenance: ProvenanceVerified, EvidenceEdgeIDs: []string{hop.ID, state},
		})
		ve := assertHasValidationCode(t, ValidateWithFacts(m, facts, reg), ErrInvalidEdgeProvenance)
		if !strings.Contains(ve.Error(), "another component") {
			t.Fatalf("expected shortcut message, got %v", ve)
		}
	})

	t.Run("walk may not route through a container resource", func(t *testing.T) {
		m, facts := baseFixture()
		atl := findComponent(m, "atlantis").BackedBy[0].URI
		vpc := m.Containers[0].BackedBy[0].URI
		sql := findComponent(m, "sonarqube_db").BackedBy[0].URI
		e1 := addFactEdge(facts, extract.EdgeConfigRef, atl, vpc, "network", false)
		e2 := addFactEdge(facts, extract.EdgePSAPeering, vpc, sql, "servicenetworking", false)
		m.RejectedEdges = nil
		m.Views[0].Edges = append(m.Views[0].Edges, Edge{
			From: "atlantis", To: "sonarqube_db", Verb: "SQL", FlowKind: FlowRuntime,
			Provenance: ProvenanceVerified, EvidenceEdgeIDs: []string{e1.ID, e2.ID},
		})
		ve := assertHasValidationCode(t, ValidateWithFacts(m, facts, reg), ErrInvalidEdgeProvenance)
		if !strings.Contains(ve.Error(), "container") {
			t.Fatalf("expected container block message, got %v", ve)
		}
	})

	t.Run("permission-only evidence must be ADMIN", func(t *testing.T) {
		testPermissionOnlyEvidenceMustBeAdmin(t, reg)
	})
}

func testPermissionOnlyEvidenceMustBeAdmin(t *testing.T, reg *icons.Registry) {
	t.Helper()
	m, facts := baseFixture()
	var iamID string
	for _, fe := range facts.Edges {
		if fe.Kind == extract.EdgeIAMResourceBinding {
			iamID = fe.ID
		}
	}
	for i, e := range m.Views[0].Edges {
		if e.From == "atlantis" && e.To == "tf_buckets" {
			m.Views[0].Edges[i].EvidenceEdgeIDs = []string{iamID}
		}
	}
	ve := assertHasValidationCode(t, ValidateWithFacts(m, facts, reg), ErrInvalidEdgeProvenance)
	if !strings.Contains(ve.Error(), "ADMIN") {
		t.Fatalf("expected ADMIN message, got %v", ve)
	}
	for i, e := range m.Views[0].Edges {
		if e.From == "atlantis" && e.To == "tf_buckets" {
			m.Views[0].Edges[i].FlowKind = FlowAdmin
		}
	}
	if err := ValidateWithFacts(m, facts, reg); err != nil {
		t.Fatalf("ADMIN permission edge should validate: %v", err)
	}
}

func TestValidatePlacementContainers(t *testing.T) {
	reg := loadTestIcons(t)
	m, facts := baseFixture()
	gke := m.Containers[1].BackedBy[0].URI
	vpc := m.Containers[0].BackedBy[0].URI
	atl := findComponent(m, "atlantis").BackedBy[0].URI
	sonar := findComponent(m, "sonarqube").BackedBy[0].URI
	for i := range facts.Resources {
		switch facts.Resources[i].URI {
		case atl:
			facts.Resources[i].ParentURI = gke
		case gke:
			facts.Resources[i].ParentURI = vpc
		case sonar:
			facts.Resources[i].ParentURI = "//container.googleapis.com/projects/acme-cicd/locations/us-east1/clusters/other"
		}
	}
	ve := assertHasValidationCode(t, ValidateWithFacts(m, facts, reg), ErrPlacementContainerMismatch)
	if len(ve.Issues) != 1 || ve.Issues[0].Subject != "sonarqube" {
		t.Fatalf("expected only sonarqube mismatch, got %v", ve)
	}

	// Ancestor chain: a workload whose cluster sits in the VPC may be placed directly in the VPC.
	m.Views[0].Placements[1].ContainerID = "vpc_cicd"
	for i := range facts.Resources {
		if facts.Resources[i].URI == sonar {
			facts.Resources[i].ParentURI = gke
		}
	}
	if err := ValidateWithFacts(m, facts, reg); err != nil {
		t.Fatalf("ancestor placement should validate: %v", err)
	}
}

func TestEvidenceIndexShortestWalkAndPairs(t *testing.T) {
	m, facts := baseFixture()
	idx := NewEvidenceIndex(m.Components, m.Containers, facts.Edges)

	// The folded k8s Service already belongs to atlantis, so the walk ends there after 2 hops.
	want := findEdgeEvidence(m.Views[0].Edges, "https_lb", "atlantis")[:2]
	if got := idx.ShortestWalk("https_lb", "atlantis"); !slices.Equal(got, want) {
		t.Fatalf("ShortestWalk = %v, want %v", got, want)
	}
	if got := idx.ShortestWalk("sonarqube_db", "sonarqube"); got != nil {
		t.Fatalf("expected no reverse walk, got %v", got)
	}

	pairs := idx.ProjectedPairs()
	for _, p := range [][2]string{{"atlantis", "tf_buckets"}, {"sonarqube", "sonarqube_db"}} {
		if !pairs[p] {
			t.Fatalf("missing projected pair %v in %v", p, pairs)
		}
	}
	if pairs[[2]string{"https_lb", "atlantis"}] {
		t.Fatalf("pair through excluded NEG must not be a direct projection: %v", pairs)
	}

	for i := range facts.Edges {
		facts.Edges[i].Heuristic = true
	}
	idx = NewEvidenceIndex(m.Components, m.Containers, facts.Edges)
	if got := idx.ShortestWalk("https_lb", "atlantis"); got != nil {
		t.Fatalf("heuristic edges must not form walks, got %v", got)
	}
	if code, _ := idx.WalkIssue(Edge{From: "a", To: "b", Provenance: ProvenanceVerified}); code != ErrMissingEvidenceEdgeIDs {
		t.Fatalf("expected missing evidence code, got %q", code)
	}
}

func TestMarshalNormalizesAcknowledgedEdges(t *testing.T) {
	m, _ := baseFixture()
	m.AcknowledgedEdges = []ComponentPair{{From: "sonarqube", To: "tf_buckets"}, {From: "atlantis", To: "sonarqube"}, {From: "sonarqube", To: "tf_buckets"}}
	data, err := Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	want := "acknowledged_edges:\n  - from: atlantis\n    to: sonarqube\n  - from: sonarqube\n    to: tf_buckets\nviews:"
	if got := string(data); !strings.Contains(got, want) {
		t.Fatalf("acknowledged_edges not sorted/deduplicated:\n%s", got)
	}
}
