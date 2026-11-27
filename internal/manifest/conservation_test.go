package manifest

import (
	"fmt"
	"testing"

	"github.com/arrase/gcp-arch/internal/extract"
)

func TestLayeredFlowSchemaNestingPlacementsAndTransversal(t *testing.T) {
	reg := loadTestIcons(t)

	facts := &extract.FactsFile{
		SchemaVersion: extract.FactsSchemaVersion,
		Scopes:        []string{"projects/p1"},
		Resources: []extract.FactResource{
			{URI: "//compute.googleapis.com/projects/p1/global/networks/vpc1", AssetType: "compute.googleapis.com/Network", ProjectID: "p1", DisplayName: "vpc1"},
			{URI: "//compute.googleapis.com/projects/p1/regions/europe-west1/subnetworks/sub1", AssetType: "compute.googleapis.com/Subnetwork", ProjectID: "p1", DisplayName: "sub1"},
			{URI: "//iam.googleapis.com/projects/p1/serviceAccounts/sa@p1.iam.gserviceaccount.com", AssetType: "iam.googleapis.com/ServiceAccount", ProjectID: "p1", DisplayName: "sa"},
			{URI: "//compute.googleapis.com/projects/p1/regions/europe-west1/routers/nat1", AssetType: "compute.googleapis.com/Router", ProjectID: "p1", DisplayName: "nat1"},
		},
	}

	m := &Manifest{
		SchemaVersion: SchemaVersion,
		Metadata:      Metadata{GeneratedAt: "2026-11-26T00:00:00Z", Scopes: []string{"projects/p1"}},
		Containers: []Container{
			{ID: "org_1", Title: "Org", Kind: ContainerOrg},
			{ID: "fld_1", Title: "Folder", Kind: ContainerFolder, ParentContainerID: "org_1"},
			{ID: "proj_1", Title: "Project", Kind: ContainerProject, ParentContainerID: "fld_1"},
			{ID: "vpc_1", Title: "VPC", Kind: ContainerVPC, ParentContainerID: "proj_1", BackedBy: []BackedResource{{URI: "//compute.googleapis.com/projects/p1/global/networks/vpc1", AssetType: "compute.googleapis.com/Network", ProjectID: "p1"}}},
			{ID: "reg_1", Title: "Region", Kind: ContainerRegion, ParentContainerID: "vpc_1"},
			{ID: "sub_1", Title: "Subnet", Kind: ContainerSubnet, ParentContainerID: "reg_1", BackedBy: []BackedResource{{URI: "//compute.googleapis.com/projects/p1/regions/europe-west1/subnetworks/sub1", AssetType: "compute.googleapis.com/Subnetwork", ProjectID: "p1"}}},
			{ID: "zone_1", Title: "Private Zone", Kind: ContainerExposureZone, Exposure: "PRIVATE", ParentContainerID: "sub_1"},
		},
		TransversalItems: []TransversalItem{
			{
				ID:          "tr_sa",
				Category:    "IAM_SA",
				AssetType:   "iam.googleapis.com/ServiceAccount",
				DisplayName: "sa",
				ProjectID:   "p1",
				BackedBy:    []BackedResource{{URI: "//iam.googleapis.com/projects/p1/serviceAccounts/sa@p1.iam.gserviceaccount.com", AssetType: "iam.googleapis.com/ServiceAccount", ProjectID: "p1"}},
			},
			{
				ID:            "tr_nat",
				Category:      "NAT_ROUTER",
				AssetType:     "compute.googleapis.com/Router",
				DisplayName:   "nat1",
				ProjectID:     "p1",
				BackedBy:      []BackedResource{{URI: "//compute.googleapis.com/projects/p1/regions/europe-west1/routers/nat1", AssetType: "compute.googleapis.com/Router", ProjectID: "p1"}},
				BackedEdgeIDs: []string{"edge_nat"},
			},
		},
	}

	view := View{
		ID:                      SystemIndexViewID,
		Title:                   "Overview",
		Archetype:               ArchetypeLayeredFlow,
		AccentMode:              AccentMixed,
		IncludeTransversalPanel: true,
		ContainerIDs:            []string{"org_1", "fld_1", "proj_1", "vpc_1", "reg_1", "sub_1", "zone_1"},
	}

	// Add 20 components placed at columns 0..7 (> 16 placements and > 4 columns).
	for i := range 20 {
		id := fmt.Sprintf("svc_%02d", i)
		uri := "//run.googleapis.com/projects/p1/locations/europe-west1/services/" + id
		facts.Resources = append(facts.Resources, extract.FactResource{
			URI:         uri,
			AssetType:   "run.googleapis.com/Service",
			ProjectID:   "p1",
			DisplayName: id,
		})
		m.Components = append(m.Components, Component{
			ID:            id,
			Title:         id,
			IconKey:       "cloud_run",
			ComponentKind: KindWorkload,
			ProjectID:     "p1",
			TopoRank:      i % 8,
			SubRank:       i,
			Members:       []string{id},
			BackedBy:      []BackedResource{{URI: uri, AssetType: "run.googleapis.com/Service", ProjectID: "p1"}},
		})
		view.Placements = append(view.Placements, Placement{
			ComponentID: id,
			ContainerID: "zone_1",
			Column:      i % 8,
		})
	}

	eFunc := extract.EdgeID(extract.EdgeConfigRef, facts.Resources[4].URI, facts.Resources[5].URI, "https")
	eNat := extract.EdgeID(extract.EdgeConfigRef, facts.Resources[4].URI, facts.Resources[3].URI, "egress_nat")
	eSA := extract.EdgeID(extract.EdgeConfigRef, facts.Resources[4].URI, facts.Resources[2].URI, "service_account")
	facts.Edges = []extract.FactEdge{
		{ID: eFunc, FromURI: facts.Resources[4].URI, ToURI: facts.Resources[5].URI, Kind: extract.EdgeConfigRef, Detail: "https"},
		{ID: eNat, FromURI: facts.Resources[4].URI, ToURI: facts.Resources[3].URI, Kind: extract.EdgeConfigRef, Detail: "egress_nat"},
		{ID: eSA, FromURI: facts.Resources[4].URI, ToURI: facts.Resources[2].URI, Kind: extract.EdgeConfigRef, Detail: "service_account"},
	}
	m.TransversalItems[0].BackedEdgeIDs = []string{eSA}
	m.TransversalItems[1].BackedEdgeIDs = []string{eNat}
	view.Edges = []Edge{
		{
			From:            "svc_00",
			To:              "svc_01",
			Verb:            "Calls",
			FlowKind:        FlowRuntime,
			SemanticTag:     "traffic",
			Provenance:      ProvenanceVerified,
			EvidenceEdgeIDs: []string{eFunc},
		},
	}
	m.Views = []View{view}

	if err := ValidateStructure(m, reg); err != nil {
		t.Fatalf("ValidateStructure failed for 7-level 20-node manifest: %v", err)
	}
	rep, err := ValidateWithFactsAndConservation(m, facts, reg)
	if err != nil {
		t.Fatalf("ValidateWithFactsAndConservation failed for 7-level 20-node manifest: %v", err)
	}
	if rep.Status != "PASS" {
		t.Fatalf("ValidateWithFactsAndConservation conservation = FAIL: %+v", rep)
	}
	if !m.IsLayeredFlow() {
		t.Fatal("expected IsLayeredFlow() == true")
	}
	if issues := rep.Issues(); len(issues) != 0 {
		t.Fatalf("expected 0 issues for PASS report, got %+v", issues)
	}
	if rep.FactsPrimaryCount != 23 || rep.RepresentedPrimaryCount != 23 {
		t.Fatalf("unexpected primary counts: %+v", rep)
	}
	if rep.FactsFunctionalEdgeCount != 2 || rep.View0Coverage != 1.0 {
		t.Fatalf("unexpected edge/coverage stats: %+v", rep)
	}
}

func newViolationBaseFacts() *extract.FactsFile {
	u1 := "//run.googleapis.com/projects/p1/locations/europe-west1/services/api"
	u2 := "//sqladmin.googleapis.com/projects/p1/instances/db"
	uSA := "//iam.googleapis.com/projects/p1/serviceAccounts/sa@p1.iam.gserviceaccount.com"
	e1 := extract.EdgeID(extract.EdgeConfigRef, u1, u2, "tcp:5432")
	e2 := extract.EdgeID(extract.EdgeConfigRef, u1, uSA, "service_account")
	return &extract.FactsFile{
		SchemaVersion: extract.FactsSchemaVersion,
		Scopes:        []string{"projects/p1"},
		Resources: []extract.FactResource{
			{URI: u1, AssetType: "run.googleapis.com/Service", ProjectID: "p1", DisplayName: "api"},
			{URI: u2, AssetType: "sqladmin.googleapis.com/Instance", ProjectID: "p1", DisplayName: "db"},
			{URI: uSA, AssetType: "iam.googleapis.com/ServiceAccount", ProjectID: "p1", DisplayName: "sa"},
		},
		Edges: []extract.FactEdge{
			{ID: e1, FromURI: u1, ToURI: u2, Kind: extract.EdgeConfigRef, Detail: "tcp:5432"},
			{ID: e2, FromURI: u1, ToURI: uSA, Kind: extract.EdgeConfigRef, Detail: "service_account"},
		},
	}
}

func newViolationBaseManifest(f *extract.FactsFile) *Manifest {
	return &Manifest{
		SchemaVersion: SchemaVersion,
		Metadata:      Metadata{GeneratedAt: "2026-11-26T00:00:00Z", Scopes: []string{"projects/p1"}},
		Components: []Component{
			{ID: "api", Title: "api", IconKey: "cloud_run", ComponentKind: KindWorkload, ProjectID: "p1", BackedBy: []BackedResource{{URI: f.Resources[0].URI, AssetType: f.Resources[0].AssetType, ProjectID: "p1"}}},
			{ID: "db", Title: "db", IconKey: "cloud_sql", ComponentKind: KindDatastore, ProjectID: "p1", BackedBy: []BackedResource{{URI: f.Resources[1].URI, AssetType: f.Resources[1].AssetType, ProjectID: "p1"}}},
		},
		TransversalItems: []TransversalItem{
			{
				ID:            "tr_sa",
				Category:      "IAM_SA",
				AssetType:     f.Resources[2].AssetType,
				DisplayName:   "sa",
				ProjectID:     "p1",
				BackedBy:      []BackedResource{{URI: f.Resources[2].URI, AssetType: f.Resources[2].AssetType, ProjectID: "p1"}},
				BackedEdgeIDs: []string{f.Edges[1].ID},
			},
		},
		Views: []View{{
			ID:         SystemIndexViewID,
			Title:      "Overview",
			Archetype:  ArchetypeLayeredFlow,
			AccentMode: AccentRuntime,
			Placements: []Placement{{ComponentID: "api", Column: 0}, {ComponentID: "db", Column: 1}},
			Edges: []Edge{{
				From: "api", To: "db", Verb: "Queries", FlowKind: FlowRuntime, Provenance: ProvenanceVerified, EvidenceEdgeIDs: []string{f.Edges[0].ID},
			}},
		}},
	}
}

func TestVerifyConservationDetectsViolations(t *testing.T) {
	t.Run("missing primary resource", testConservationMissingPrimary)
	t.Run("hallucinated primary resource", testConservationHallucinatedPrimary)
	t.Run("missing functional edge", testConservationMissingEdge)
	t.Run("unrepresented transversal resource and edge", testConservationMissingTransversal)
	t.Run("view0 incomplete coverage", testConservationView0Incomplete)
	t.Run("spoof_structural_edge", testConservationSpoofStructuralEdge)
	t.Run("spoof_comp_folded_edge", testConservationSpoofCompFoldedEdge)
	t.Run("primary_in_transversal", testConservationPrimaryInTransversal)
	t.Run("primary_in_container", testConservationPrimaryInContainer)
	t.Run("missing_view0_edge_endpoints", testConservationMissingView0Endpoints)
	t.Run("structural_link_bypass_rejected", testConservationStructuralLinkBypass)
}

func testConservationMissingPrimary(t *testing.T) {
	f := newViolationBaseFacts()
	m := newViolationBaseManifest(f)
	m.Components = m.Components[:1]
	m.Views[0].Placements = m.Views[0].Placements[:1]
	rep := VerifyConservation(m, f)
	if rep.Status != "FAIL" || len(rep.MissingPrimaryURIs) != 1 || rep.MissingPrimaryURIs[0] != f.Resources[1].URI {
		t.Fatalf("expected missing db URI, got %+v", rep)
	}
}

func testConservationHallucinatedPrimary(t *testing.T) {
	f := newViolationBaseFacts()
	m := newViolationBaseManifest(f)
	m.Components = append(m.Components, Component{
		ID: "ghost", Title: "ghost", IconKey: "cloud_run", ComponentKind: KindWorkload, ProjectID: "p1",
		BackedBy: []BackedResource{{URI: "//run.googleapis.com/ghost", AssetType: "run.googleapis.com/Service", ProjectID: "p1"}},
	})
	m.Views[0].Placements = append(m.Views[0].Placements, Placement{ComponentID: "ghost", Column: 1})
	rep := VerifyConservation(m, f)
	if rep.Status != "FAIL" || len(rep.HallucinatedPrimaryURIs) != 1 {
		t.Fatalf("expected hallucinated URI, got %+v", rep)
	}
}

func testConservationMissingEdge(t *testing.T) {
	f := newViolationBaseFacts()
	m := newViolationBaseManifest(f)
	m.Views[0].Edges = nil
	rep := VerifyConservation(m, f)
	if rep.Status != "FAIL" || len(rep.MissingEdgeIDs) != 1 || rep.MissingEdgeIDs[0] != f.Edges[0].ID {
		t.Fatalf("expected missing functional edge, got %+v", rep)
	}
}

func testConservationMissingTransversal(t *testing.T) {
	f := newViolationBaseFacts()
	m := newViolationBaseManifest(f)
	m.TransversalItems = nil
	rep := VerifyConservation(m, f)
	if rep.Status != "FAIL" || len(rep.UnrepresentedTransversalURIs) != 1 || len(rep.MissingTransversalEdgeIDs) != 1 {
		t.Fatalf("expected transversal conservation failure, got %+v", rep)
	}
}

func testConservationView0Incomplete(t *testing.T) {
	f := newViolationBaseFacts()
	m := newViolationBaseManifest(f)
	m.Views[0].Placements = m.Views[0].Placements[:1]
	rep := VerifyConservation(m, f)
	if rep.Status != "FAIL" || rep.View0Coverage != 0.5 || len(rep.View0MissingNodeIDs) != 1 || rep.View0MissingNodeIDs[0] != "db" {
		t.Fatalf("expected view0 coverage failure, got %+v", rep)
	}
}

func testConservationSpoofStructuralEdge(t *testing.T) {
	f := newViolationBaseFacts()
	m := newViolationBaseManifest(f)
	m.Views[0].Edges = nil
	m.StructuralEdges = append(m.StructuralEdges, StructuralEdge{
		EdgeID:   f.Edges[0].ID,
		Kind:     string(f.Edges[0].Kind),
		FromURI:  "//fake/from",
		ToURI:    "//fake/to",
		Reason:   "bogus_catch_all",
		TargetID: "db",
	})
	rep := VerifyConservation(m, f)
	if rep.Status != "FAIL" || len(rep.MissingEdgeIDs) != 1 || rep.MissingEdgeIDs[0] != f.Edges[0].ID {
		t.Fatalf("expected spoof_structural_edge to FAIL with missing %s, got %+v", f.Edges[0].ID, rep)
	}

	// Even with matching URIs and valid structural reason, inter-component functional edge cannot be hidden in StructuralEdges.
	m.StructuralEdges[0].FromURI = f.Edges[0].FromURI
	m.StructuralEdges[0].ToURI = f.Edges[0].ToURI
	m.StructuralEdges[0].Reason = "container_attachment"
	rep2 := VerifyConservation(m, f)
	if rep2.Status != "FAIL" || len(rep2.MissingEdgeIDs) != 1 || rep2.MissingEdgeIDs[0] != f.Edges[0].ID {
		t.Fatalf("expected inter-component StructuralEdge to FAIL with missing %s, got %+v", f.Edges[0].ID, rep2)
	}
}

func testConservationSpoofCompFoldedEdge(t *testing.T) {
	f := newViolationBaseFacts()
	m := newViolationBaseManifest(f)
	m.Views[0].Edges = nil
	m.Components[0].FoldedEdgeIDs = []string{f.Edges[0].ID}
	rep := VerifyConservation(m, f)
	if rep.Status != "FAIL" || len(rep.MissingEdgeIDs) != 1 || rep.MissingEdgeIDs[0] != f.Edges[0].ID {
		t.Fatalf("expected spoof_comp_folded_edge to FAIL with missing %s, got %+v", f.Edges[0].ID, rep)
	}
}

func testConservationPrimaryInTransversal(t *testing.T) {
	f := newViolationBaseFacts()
	m := newViolationBaseManifest(f)
	dbURI := f.Resources[1].URI
	m.Components = m.Components[:1]
	m.Views[0].Placements = m.Views[0].Placements[:1]
	m.Views[0].Edges = nil
	m.TransversalItems = append(m.TransversalItems, TransversalItem{
		ID:          "tr_spoof_db",
		Category:    "DATA",
		AssetType:   f.Resources[1].AssetType,
		DisplayName: "db",
		ProjectID:   "p1",
		BackedBy:    []BackedResource{{URI: dbURI, AssetType: f.Resources[1].AssetType, ProjectID: "p1"}},
	})
	rep := VerifyConservation(m, f)
	if rep.Status != "FAIL" || len(rep.MissingPrimaryURIs) != 1 || rep.MissingPrimaryURIs[0] != dbURI {
		t.Fatalf("expected primary_in_transversal to FAIL with missing %s, got %+v", dbURI, rep)
	}
}

func testConservationPrimaryInContainer(t *testing.T) {
	f := newViolationBaseFacts()
	m := newViolationBaseManifest(f)
	apiURI := f.Resources[0].URI
	m.Components = m.Components[1:]
	m.Views[0].Placements = m.Views[0].Placements[1:]
	m.Views[0].Edges = nil
	m.Containers = append(m.Containers, Container{
		ID:       "cont_spoof_api",
		Title:    "Spoofed API Container",
		Kind:     ContainerProject,
		BackedBy: []BackedResource{{URI: apiURI, AssetType: "compute.googleapis.com/Network", ProjectID: "p1"}},
	})
	rep := VerifyConservation(m, f)
	if rep.Status != "FAIL" || len(rep.MissingPrimaryURIs) != 1 || rep.MissingPrimaryURIs[0] != apiURI {
		t.Fatalf("expected primary_in_container to FAIL with missing %s, got %+v", apiURI, rep)
	}
}

func testConservationMissingView0Endpoints(t *testing.T) {
	f := newViolationBaseFacts()
	m := newViolationBaseManifest(f)
	u3 := "//storage.googleapis.com/p1-bkt"
	f.Resources = append(f.Resources, extract.FactResource{URI: u3, AssetType: "storage.googleapis.com/Bucket", ProjectID: "p1", DisplayName: "bkt"})
	m.Components = append(m.Components, Component{
		ID: "bkt", Title: "bkt", IconKey: "cloud_storage", ComponentKind: KindDatastore, ProjectID: "p1",
		BackedBy: []BackedResource{{URI: u3, AssetType: "storage.googleapis.com/Bucket", ProjectID: "p1"}},
	})
	m.Views[0].Placements = append(m.Views[0].Placements, Placement{ComponentID: "bkt", Column: 2})
	m.Views[0].Edges[0].To = "bkt"
	rep := VerifyConservation(m, f)
	if rep.Status != "FAIL" || len(rep.MissingEdgeIDs) != 1 || rep.MissingEdgeIDs[0] != f.Edges[0].ID {
		t.Fatalf("expected missing_view0_edge_endpoints to FAIL with missing %s, got %+v", f.Edges[0].ID, rep)
	}
}

func testConservationStructuralLinkBypass(t *testing.T) {
	uRouter := "//compute.googleapis.com/projects/p1/regions/europe-west1/routers/nat1"
	f := newViolationBaseFacts()
	f.Resources = append(f.Resources, extract.FactResource{
		URI: uRouter, AssetType: "compute.googleapis.com/Router", ProjectID: "p1", DisplayName: "nat1",
	})
	eRouter := extract.EdgeID(extract.EdgeConfigRef, f.Resources[0].URI, uRouter, "egress_nat")
	f.Edges = append(f.Edges, extract.FactEdge{
		ID: eRouter, FromURI: f.Resources[0].URI, ToURI: uRouter, Kind: extract.EdgeConfigRef, Detail: "egress_nat",
	})

	m := newViolationBaseManifest(f)
	m.TransversalItems = append(m.TransversalItems, TransversalItem{
		ID:          "tr_nat",
		Category:    "NAT_ROUTER",
		AssetType:   "compute.googleapis.com/Router",
		DisplayName: "nat1",
		ProjectID:   "p1",
		BackedBy:    []BackedResource{{URI: uRouter, AssetType: "compute.googleapis.com/Router", ProjectID: "p1"}},
	})
	m.StructuralEdges = append(m.StructuralEdges, StructuralEdge{
		EdgeID:   eRouter,
		Kind:     string(extract.EdgeConfigRef),
		FromURI:  f.Resources[0].URI,
		ToURI:    uRouter,
		Reason:   "structural_link",
		TargetID: "bogus",
	})
	rep := VerifyConservation(m, f)
	if rep.Status != "FAIL" || len(rep.MissingEdgeIDs) != 1 || rep.MissingEdgeIDs[0] != eRouter {
		t.Fatalf("expected structural_link bypass with bogus TargetID/transversal endpoint to FAIL with missing %s, got %+v", eRouter, rep)
	}

	// Even with a valid TransversalItem TargetID, Router edges cannot bypass BackedEdgeIDs via structural_link.
	m.StructuralEdges[0].TargetID = "tr_nat"
	rep2 := VerifyConservation(m, f)
	if rep2.Status != "FAIL" || len(rep2.MissingEdgeIDs) != 1 || rep2.MissingEdgeIDs[0] != eRouter {
		t.Fatalf("expected structural_link bypass on Router edge to FAIL with missing %s, got %+v", eRouter, rep2)
	}
}

func TestConservationReportIssuesAndIsLayeredFlow(t *testing.T) {
	if (*Manifest)(nil).IsLayeredFlow() {
		t.Fatal("nil manifest must not be layered flow")
	}
	legacy, _ := baseFixture()
	if legacy.IsLayeredFlow() {
		t.Fatal("legacy landscape_matrix fixture must not be layered flow")
	}
	legacy.SuppressedPlumbingCounts = map[string]int{"compute.googleapis.com/Route": 2}
	if !legacy.IsLayeredFlow() {
		t.Fatal("manifest with SuppressedPlumbingCounts must report IsLayeredFlow")
	}

	rep := ConservationReport{
		Status:                       "FAIL",
		MissingPrimaryURIs:           []string{"//missing/primary"},
		HallucinatedPrimaryURIs:      []string{"//ghost/primary"},
		MissingEdgeIDs:               []string{"e1"},
		UnrepresentedTransversalURIs: []string{"//missing/sa"},
		MissingTransversalEdgeIDs:    []string{"e2"},
		View0MissingNodeIDs:          []string{"comp_a"},
	}
	issues := rep.Issues()
	if len(issues) != 6 {
		t.Fatalf("expected 6 issues, got %d: %+v", len(issues), issues)
	}
	wantCodes := []string{
		"CONSERVATION_MISSING_PRIMARY",
		"CONSERVATION_HALLUCINATED_PRIMARY",
		"CONSERVATION_MISSING_EDGE",
		"CONSERVATION_UNREPRESENTED_TRANSVERSAL",
		"CONSERVATION_MISSING_TRANSVERSAL_EDGE",
		"CONSERVATION_VIEW0_COVERAGE",
	}
	for i, want := range wantCodes {
		if issues[i].Code != want {
			t.Errorf("issues[%d].Code = %q, want %q", i, issues[i].Code, want)
		}
	}

	fallback := failConservationReport().Issues()
	if len(fallback) != 1 || fallback[0].Code != "CONSERVATION_FAILED" {
		t.Fatalf("expected fallback CONSERVATION_FAILED issue, got %+v", fallback)
	}
}
