package manifest

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/icons"
)

func loadTestIcons(t *testing.T) *icons.Registry {
	t.Helper()
	reg, err := icons.Load()
	if err != nil {
		t.Fatalf("icons.Load(): %v", err)
	}
	return reg
}

func assertHasValidationCode(t *testing.T, err error, wantCode string) *ValidationError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected ValidationError with code %q, got nil", wantCode)
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %T (%v)", err, err)
	}
	for _, iss := range ve.Issues {
		if iss.Code == wantCode {
			return ve
		}
	}
	t.Fatalf("ValidationError missing code %q; got issues:\n%s", wantCode, ve.Error())
	return nil
}

// baseFixture returns a valid single-view manifest and matching FactsFile modeling:
// HTTPS LB -> Atlantis (GKE workload + folded SA) -> Terraform State Buckets (2 GCS buckets)
// and SonarQube (GKE workload) -> SonarQube DB (Cloud SQL + folded 1:1 SA + CryptoKey + Secret)
// via a multi-hop directed walk passing through an excluded ConfigMap/NEG intermediate URI.
func baseFixture() (*Manifest, *extract.FactsFile) {
	const (
		uriLB        = "//compute.googleapis.com/projects/acme-cicd/global/forwardingRules/https-lb"
		uriNEG       = "//compute.googleapis.com/projects/acme-cicd/zones/europe-west1-b/networkEndpointGroups/atlantis-neg"
		uriSvc       = "//container.googleapis.com/projects/acme-cicd/locations/europe-west1/clusters/cicd/k8s/namespaces/atlantis/services/atlantis"
		uriAtlantis  = "//container.googleapis.com/projects/acme-cicd/locations/europe-west1/clusters/cicd/k8s/namespaces/atlantis/apps/deployments/atlantis"
		uriAtlSA     = "//iam.googleapis.com/projects/acme-cicd/serviceAccounts/atlantis@acme-cicd.iam.gserviceaccount.com"
		uriSonar     = "//container.googleapis.com/projects/acme-cicd/locations/europe-west1/clusters/cicd/k8s/namespaces/sonarqube/apps/deployments/sonarqube"
		uriSQL       = "//sqladmin.googleapis.com/projects/acme-cicd/instances/sonarqube"
		uriSQLSA     = "//iam.googleapis.com/projects/acme-cicd/serviceAccounts/sql-backup@acme-cicd.iam.gserviceaccount.com"
		uriSQLKMS    = "//cloudkms.googleapis.com/projects/acme-cicd/locations/europe-west1/keyRings/db/cryptoKeys/sql-key"
		uriSQLSecret = "//secretmanager.googleapis.com/projects/acme-cicd/secrets/sonarqube-db-pass"
		uriBktState  = "//storage.googleapis.com/acme-tfstate"
		uriBktMods   = "//storage.googleapis.com/acme-private-modules"
		uriVPC       = "//compute.googleapis.com/projects/acme-cicd/global/networks/cicd"
		uriGKE       = "//container.googleapis.com/projects/acme-cicd/locations/europe-west1/clusters/cicd"
		uriNatIP     = "//compute.googleapis.com/projects/acme-cicd/regions/europe-west1/addresses/nat-ip"
	)

	eLBToNEG := extract.FactEdge{
		ID:      extract.EdgeID(extract.EdgeK8sNEGBackend, uriLB, uriNEG, "https:443"),
		FromURI: uriLB,
		ToURI:   uriNEG,
		Kind:    extract.EdgeK8sNEGBackend,
		Detail:  "https:443",
	}
	eNEGToSvc := extract.FactEdge{
		ID:      extract.EdgeID(extract.EdgeK8sNEGBackend, uriNEG, uriSvc, "port:4141"),
		FromURI: uriNEG,
		ToURI:   uriSvc,
		Kind:    extract.EdgeK8sNEGBackend,
		Detail:  "port:4141",
	}
	eSvcToAtlantis := extract.FactEdge{
		ID:      extract.EdgeID(extract.EdgeK8sServiceSelector, uriSvc, uriAtlantis, "app=atlantis"),
		FromURI: uriSvc,
		ToURI:   uriAtlantis,
		Kind:    extract.EdgeK8sServiceSelector,
		Detail:  "app=atlantis",
	}
	eAtlToState := extract.FactEdge{
		ID:      extract.EdgeID(extract.EdgeConfigRef, uriAtlantis, uriBktState, "gs://acme-tfstate"),
		FromURI: uriAtlantis,
		ToURI:   uriBktState,
		Kind:    extract.EdgeConfigRef,
		Detail:  "gs://acme-tfstate",
	}
	eAtlSAToMods := extract.FactEdge{
		ID:      extract.EdgeID(extract.EdgeIAMResourceBinding, uriAtlSA, uriBktMods, "roles/storage.objectViewer"),
		FromURI: uriAtlSA,
		ToURI:   uriBktMods,
		Kind:    extract.EdgeIAMResourceBinding,
		Detail:  "roles/storage.objectViewer",
	}
	eSonarToSQL := extract.FactEdge{
		ID:      extract.EdgeID(extract.EdgeConfigRef, uriSonar, uriSQL, "jdbc:10.42.0.5:5432"),
		FromURI: uriSonar,
		ToURI:   uriSQL,
		Kind:    extract.EdgeConfigRef,
		Detail:  "jdbc:10.42.0.5:5432",
	}

	facts := &extract.FactsFile{
		SchemaVersion: extract.FactsSchemaVersion,
		ReadTime:      "2026-11-23T02:00:00Z",
		Scopes:        []string{"projects/acme-cicd"},
		Hierarchy: extract.Hierarchy{
			Projects: []extract.ProjectNode{{ProjectID: "acme-cicd"}},
		},
		Resources: []extract.FactResource{
			{URI: uriLB, AssetType: "compute.googleapis.com/GlobalForwardingRule", ProjectID: "acme-cicd", DisplayName: "https-lb"},
			{URI: uriNEG, AssetType: "compute.googleapis.com/NetworkEndpointGroup", ProjectID: "acme-cicd", DisplayName: "atlantis-neg"},
			{URI: uriSvc, AssetType: "k8s.io/Service", ProjectID: "acme-cicd", DisplayName: "atlantis"},
			{URI: uriAtlantis, AssetType: "apps.k8s.io/Deployment", ProjectID: "acme-cicd", DisplayName: "atlantis"},
			{URI: uriAtlSA, AssetType: "iam.googleapis.com/ServiceAccount", ProjectID: "acme-cicd", DisplayName: "atlantis-sa"},
			{URI: uriSonar, AssetType: "apps.k8s.io/Deployment", ProjectID: "acme-cicd", DisplayName: "sonarqube"},
			{URI: uriSQL, AssetType: "sqladmin.googleapis.com/Instance", ProjectID: "acme-cicd", DisplayName: "sonarqube"},
			{URI: uriSQLSA, AssetType: "iam.googleapis.com/ServiceAccount", ProjectID: "acme-cicd", DisplayName: "sql-backup"},
			{URI: uriSQLKMS, AssetType: "cloudkms.googleapis.com/CryptoKey", ProjectID: "acme-cicd", DisplayName: "sql-key"},
			{URI: uriSQLSecret, AssetType: "secretmanager.googleapis.com/Secret", ProjectID: "acme-cicd", DisplayName: "sonarqube-db-pass"},
			{URI: uriBktState, AssetType: "storage.googleapis.com/Bucket", ProjectID: "acme-cicd", DisplayName: "acme-tfstate"},
			{URI: uriBktMods, AssetType: "storage.googleapis.com/Bucket", ProjectID: "acme-cicd", DisplayName: "acme-private-modules"},
			{URI: uriVPC, AssetType: "compute.googleapis.com/Network", ProjectID: "acme-cicd", DisplayName: "cicd"},
			{URI: uriGKE, AssetType: "container.googleapis.com/Cluster", ProjectID: "acme-cicd", DisplayName: "cicd"},
			{URI: uriNatIP, AssetType: "compute.googleapis.com/Address", ProjectID: "acme-cicd", DisplayName: "nat-ip"},
		},
		Edges: []extract.FactEdge{
			eLBToNEG, eNEGToSvc, eSvcToAtlantis, eAtlToState, eAtlSAToMods, eSonarToSQL,
		},
		SuppressedPlumbingCounts: map[string]int{},
		CoverageLedger:           []extract.CoverageEntry{{Scope: "projects/acme-cicd", Source: "cai", State: extract.CoverageOK}},
	}

	m := &Manifest{
		SchemaVersion: SchemaVersion,
		Metadata: Metadata{
			GeneratedAt: "2026-11-23T02:00:00Z",
			Scopes:      []string{"projects/acme-cicd"},
		},
		Components: []Component{
			{
				ID:            "https_lb",
				Title:         "HTTPS Ingress LB",
				IconKey:       "cloud_load_balancing",
				ComponentKind: KindIngress,
				ProjectID:     "acme-cicd",
				BackedBy: []BackedResource{
					{URI: uriLB, AssetType: "compute.googleapis.com/GlobalForwardingRule", ProjectID: "acme-cicd"},
				},
			},
			{
				ID:            "atlantis",
				Title:         "Atlantis PR Automation",
				Subtitle:      "gke:cicd / ns:atlantis",
				IconKey:       "k8s_workload",
				ComponentKind: KindWorkload,
				ProjectID:     "acme-cicd",
				Notes:         "Curated title",
				BackedBy: []BackedResource{
					{URI: uriAtlantis, AssetType: "apps.k8s.io/Deployment", ProjectID: "acme-cicd"},
					{URI: uriAtlSA, AssetType: "iam.googleapis.com/ServiceAccount", ProjectID: "acme-cicd"},
					{URI: uriSvc, AssetType: "k8s.io/Service", ProjectID: "acme-cicd"},
				},
				InlineMeta: []InlineMeta{
					{Label: "Namespace", Value: "atlantis"},
					{Label: "Service Account", Value: "atlantis@acme-cicd.iam"},
				},
			},
			{
				ID:            "sonarqube",
				Title:         "SonarQube Code Quality",
				IconKey:       "k8s_workload",
				ComponentKind: KindWorkload,
				ProjectID:     "acme-cicd",
				BackedBy: []BackedResource{
					{URI: uriSonar, AssetType: "apps.k8s.io/Deployment", ProjectID: "acme-cicd"},
				},
			},
			{
				ID:            "sonarqube_db",
				Title:         "SonarQube Cloud SQL",
				IconKey:       "cloud_sql",
				ComponentKind: KindDatastore,
				ProjectID:     "acme-cicd",
				// Valid: single stateful datastore asset_type + folded 1:1 ServiceAccount, CryptoKey, Secret.
				BackedBy: []BackedResource{
					{URI: uriSQL, AssetType: "sqladmin.googleapis.com/Instance", ProjectID: "acme-cicd"},
					{URI: uriSQLSA, AssetType: "iam.googleapis.com/ServiceAccount", ProjectID: "acme-cicd"},
					{URI: uriSQLKMS, AssetType: "cloudkms.googleapis.com/CryptoKey", ProjectID: "acme-cicd"},
					{URI: uriSQLSecret, AssetType: "secretmanager.googleapis.com/Secret", ProjectID: "acme-cicd"},
				},
			},
			{
				ID:            "tf_buckets",
				Title:         "Terraform GCS Buckets",
				IconKey:       "cloud_storage",
				ComponentKind: KindDatastore,
				ProjectID:     "acme-cicd",
				// Valid homogeneous datastore cohort: both URIs are storage.googleapis.com/Bucket.
				BackedBy: []BackedResource{
					{URI: uriBktState, AssetType: "storage.googleapis.com/Bucket", ProjectID: "acme-cicd"},
					{URI: uriBktMods, AssetType: "storage.googleapis.com/Bucket", ProjectID: "acme-cicd"},
				},
			},
		},
		Containers: []Container{
			{
				ID:    "vpc_cicd",
				Title: "VPC: cicd",
				Kind:  ContainerVPC,
				BackedBy: []BackedResource{
					{URI: uriVPC, AssetType: "compute.googleapis.com/Network", ProjectID: "acme-cicd"},
				},
			},
			{
				ID:                "gke_cicd",
				Title:             "GKE Cluster: cicd",
				Kind:              ContainerGKECluster,
				ParentContainerID: "vpc_cicd",
				BackedBy: []BackedResource{
					{URI: uriGKE, AssetType: "container.googleapis.com/Cluster", ProjectID: "acme-cicd"},
				},
			},
		},
		ExcludedResources: []ExcludedResource{
			{URI: uriNEG, AssetType: "compute.googleapis.com/NetworkEndpointGroup", ProjectID: "acme-cicd", Reason: "Folded LB backend NEG"},
			{URI: uriNatIP, AssetType: "compute.googleapis.com/Address", ProjectID: "acme-cicd", Reason: "Static egress NAT IP"},
		},
		RejectedEdges: []RejectedEdge{
			{From: "atlantis", To: "sonarqube_db", Reason: "No direct DB access"},
		},
		Views: []View{
			{
				ID:           "gke_platform",
				Title:        "GKE Developer Platform",
				Archetype:    ArchetypeLandscapeMatrix,
				AccentMode:   AccentRuntime,
				ContainerIDs: []string{"vpc_cicd", "gke_cicd"},
				Placements: []Placement{
					{ComponentID: "https_lb", Column: 1},
					{ComponentID: "atlantis", ContainerID: "gke_cicd", Column: 2},
					{ComponentID: "sonarqube", ContainerID: "gke_cicd", Column: 2},
					{ComponentID: "tf_buckets", Column: 4},
					{ComponentID: "sonarqube_db", Column: 4},
				},
				Edges: []Edge{
					{
						// 3-hop directed walk across excluded NEG + folded Service into Atlantis.
						From:            "https_lb",
						To:              "atlantis",
						Verb:            "HTTPS ingress",
						FlowKind:        FlowRuntime,
						Provenance:      ProvenanceVerified,
						EvidenceEdgeIDs: []string{eLBToNEG.ID, eNEGToSvc.ID, eSvcToAtlantis.ID},
						ViaBusID:        "ingress_bus",
					},
					{
						// INFERRED edge with rationale sharing single-source bus from https_lb.
						From:               "https_lb",
						To:                 "sonarqube",
						Verb:               "HTTPS ingress",
						FlowKind:           FlowRuntime,
						Provenance:         ProvenanceInferred,
						InferenceRationale: "Ingress host rule observed for sonarqube.internal",
						ViaBusID:           "ingress_bus",
					},
					{
						// Parallel valid walks from atlantis (deployment + folded SA) to tf_buckets (2 buckets).
						From:            "atlantis",
						To:              "tf_buckets",
						Verb:            "terraform state",
						FlowKind:        FlowCICD,
						Provenance:      ProvenanceVerified,
						EvidenceEdgeIDs: []string{eAtlToState.ID, eAtlSAToMods.ID},
					},
					{
						From:            "sonarqube",
						To:              "sonarqube_db",
						Verb:            "PostgreSQL 5432",
						FlowKind:        FlowRuntime,
						Provenance:      ProvenanceVerified,
						EvidenceEdgeIDs: []string{eSonarToSQL.ID},
					},
				},
			},
		},
	}

	return m, facts
}

func TestValidateValidSingleViewAndFoldedDatastore(t *testing.T) {
	reg := loadTestIcons(t)
	m, facts := baseFixture()

	if err := ValidateStructure(m, reg); err != nil {
		t.Fatalf("ValidateStructure unexpected error: %v", err)
	}
	if err := ValidateWithFacts(m, facts, reg); err != nil {
		t.Fatalf("ValidateWithFacts unexpected error: %v", err)
	}

	// Single-target fan-in via_bus_id must also pass.
	m.Views[0].Edges[2].ViaBusID = ""
	m.Views[0].Edges = append(m.Views[0].Edges, Edge{
		From:               "sonarqube",
		To:                 "tf_buckets",
		Verb:               "read artifacts",
		FlowKind:           FlowRuntime,
		Provenance:         ProvenanceInferred,
		InferenceRationale: "Shared bucket read",
		ViaBusID:           "bucket_fanin",
	})
	m.Views[0].Edges[2].ViaBusID = "bucket_fanin"
	if err := ValidateWithFacts(m, facts, reg); err != nil {
		t.Fatalf("single-target fan-in via_bus_id failed: %v", err)
	}
}

func TestValidateValidMultiViewWithSystemIndex(t *testing.T) {
	reg := loadTestIcons(t)
	m, facts := baseFixture()

	// Add a third top-level container proj_acme_cicd -> vpc_cicd -> gke_cicd in global Containers;
	// detail views activate at most 2 levels ([proj_acme_cicd, vpc_cicd] and [vpc_cicd, gke_cicd]).
	m.Containers = append([]Container{{
		ID:    "proj_acme_cicd",
		Title: "Project: acme-cicd",
		Kind:  ContainerProject,
	}}, m.Containers...)
	m.Containers[1].ParentContainerID = "proj_acme_cicd" // vpc_cicd -> proj_acme_cicd

	detail1 := m.Views[0]
	detail1.Placements[0].OffPageRefs = []OffPageRef{
		{TargetViewID: "network_view", Label: "Network Topology", Direction: "OUT"},
	}

	detail2 := View{
		ID:           "network_view",
		Title:        "Shared Network & PSA",
		Archetype:    ArchetypeHubSpoke,
		AccentMode:   AccentMixed,
		ContainerIDs: []string{"proj_acme_cicd", "vpc_cicd"},
		Placements: []Placement{
			{ComponentID: "sonarqube", ContainerID: "vpc_cicd", Column: 0},
			{ComponentID: "sonarqube_db", Column: 2},
		},
		Edges: []Edge{
			{
				From:            "sonarqube",
				To:              "sonarqube_db",
				Verb:            "PostgreSQL 5432",
				FlowKind:        FlowRuntime,
				Provenance:      ProvenanceVerified,
				EvidenceEdgeIDs: []string{facts.Edges[5].ID},
			},
		},
	}

	view0 := View{
		ID:         SystemIndexViewID,
		Title:      "System Index",
		Archetype:  ArchetypeLandscapeMatrix,
		AccentMode: AccentMixed,
		Placements: []Placement{
			{ComponentID: "https_lb", Column: 0},
			{ComponentID: "gke_platform", Column: 2},
			{ComponentID: "network_view", Column: 3},
		},
		Edges: []Edge{
			{
				From:       "https_lb",
				To:         "gke_platform",
				Verb:       "HTTPS traffic",
				FlowKind:   FlowRuntime,
				Provenance: ProvenanceVerified,
			},
			{
				From:               "gke_platform",
				To:                 "network_view",
				Verb:               "PSA peering",
				FlowKind:           FlowRuntime,
				Provenance:         ProvenanceInferred,
				InferenceRationale: "Cross-view index link",
			},
		},
	}

	m.Views = []View{view0, detail1, detail2}

	if err := ValidateStructure(m, reg); err != nil {
		t.Fatalf("multi-view ValidateStructure failed: %v", err)
	}
	if err := ValidateWithFacts(m, facts, reg); err != nil {
		t.Fatalf("multi-view ValidateWithFacts failed: %v", err)
	}
}

func TestValidateStructuralErrorCodes(t *testing.T) {
	reg := loadTestIcons(t)

	// Nil manifest
	ve := assertHasValidationCode(t, ValidateStructure(nil, reg), ErrInvalidSchemaVersion)
	if !strings.Contains(ve.Error(), ErrInvalidSchemaVersion) {
		t.Fatalf("Error() missing code: %s", ve.Error())
	}
	if rep := ve.Report(); rep.Valid || len(rep.Errors) == 0 {
		t.Fatalf("unexpected Report(): %+v", rep)
	}

	cases := []struct {
		name     string
		mutate   func(m *Manifest)
		wantCode string
	}{
		{
			name:     "wrong schema version",
			mutate:   func(m *Manifest) { m.SchemaVersion = "v2" },
			wantCode: ErrInvalidSchemaVersion,
		},
		{
			name:     "empty component id",
			mutate:   func(m *Manifest) { m.Components[0].ID = "" },
			wantCode: ErrEmptyComponentID,
		},
		{
			name:     "whitespace component id",
			mutate:   func(m *Manifest) { m.Components[0].ID = "   " },
			wantCode: ErrEmptyComponentID,
		},
		{
			name:     "duplicate component id",
			mutate:   func(m *Manifest) { m.Components = append(m.Components, m.Components[0]) },
			wantCode: ErrDuplicateComponentID,
		},
		{
			name:     "empty component title",
			mutate:   func(m *Manifest) { m.Components[0].Title = "   " },
			wantCode: ErrEmptyComponentTitle,
		},
		{
			name:     "invalid component kind",
			mutate:   func(m *Manifest) { m.Components[0].ComponentKind = "UNKNOWN" },
			wantCode: ErrInvalidComponentKind,
		},
		{
			name:     "empty project id",
			mutate:   func(m *Manifest) { m.Components[0].ProjectID = "" },
			wantCode: ErrEmptyProjectID,
		},
		{
			name:     "invalid icon key",
			mutate:   func(m *Manifest) { m.Components[0].IconKey = "not_a_real_icon" },
			wantCode: ErrInvalidIconKey,
		},
		{
			name:     "empty backed_by",
			mutate:   func(m *Manifest) { m.Components[0].BackedBy = nil },
			wantCode: ErrEmptyBackedBy,
		},
		{
			name:     "invalid backed resource fields",
			mutate:   func(m *Manifest) { m.Components[0].BackedBy[0].AssetType = "" },
			wantCode: ErrInvalidBackedResource,
		},
		{
			name: "inline_meta exceeds 4 rows",
			mutate: func(m *Manifest) {
				m.Components[0].InlineMeta = []InlineMeta{
					{Label: "A", Value: "1"},
					{Label: "B", Value: "2"},
					{Label: "C", Value: "3"},
					{Label: "D", Value: "4"},
					{Label: "E", Value: "5"},
				}
			},
			wantCode: ErrInlineMetaLimit,
		},
		{
			name: "inline_meta missing value",
			mutate: func(m *Manifest) {
				m.Components[0].InlineMeta = []InlineMeta{{Label: "A", Value: ""}}
			},
			wantCode: ErrInvalidInlineMeta,
		},
		{
			name: "heterogeneous datastore catch-all rejected",
			mutate: func(m *Manifest) {
				m.Components[3].BackedBy = append(m.Components[3].BackedBy, BackedResource{
					URI:       "//storage.googleapis.com/mixed-bucket",
					AssetType: "storage.googleapis.com/Bucket",
					ProjectID: "acme-cicd",
				})
			},
			wantCode: ErrHeterogeneousDatastore,
		},
		{
			name:     "empty container id",
			mutate:   func(m *Manifest) { m.Containers[0].ID = "" },
			wantCode: ErrEmptyContainerID,
		},
		{
			name:     "whitespace container id",
			mutate:   func(m *Manifest) { m.Containers[0].ID = " \t " },
			wantCode: ErrEmptyContainerID,
		},
		{
			name:     "duplicate container id",
			mutate:   func(m *Manifest) { m.Containers = append(m.Containers, m.Containers[0]) },
			wantCode: ErrDuplicateContainerID,
		},
		{
			name:     "empty container title",
			mutate:   func(m *Manifest) { m.Containers[0].Title = "" },
			wantCode: ErrEmptyContainerTitle,
		},
		{
			name:     "invalid container kind",
			mutate:   func(m *Manifest) { m.Containers[0].Kind = "BOGUS_ZONE" },
			wantCode: ErrInvalidContainerKind,
		},
		{
			name:     "invalid container backed_by",
			mutate:   func(m *Manifest) { m.Containers[0].BackedBy[0].URI = "" },
			wantCode: ErrInvalidBackedResource,
		},
		{
			name:     "unknown parent container",
			mutate:   func(m *Manifest) { m.Containers[0].ParentContainerID = "missing_parent" },
			wantCode: ErrUnknownParentContainer,
		},
		{
			name:     "container parent cycle",
			mutate:   func(m *Manifest) { m.Containers[0].ParentContainerID = "gke_cicd" },
			wantCode: ErrContainerCycle,
		},
		{
			name:     "invalid excluded resource",
			mutate:   func(m *Manifest) { m.ExcludedResources[0].URI = "" },
			wantCode: ErrInvalidExcludedResource,
		},
		{
			name:     "missing exclusion reason",
			mutate:   func(m *Manifest) { m.ExcludedResources[0].Reason = "  " },
			wantCode: ErrMissingExclusionReason,
		},
		{
			name:     "invalid rejected edge",
			mutate:   func(m *Manifest) { m.RejectedEdges[0].Reason = "" },
			wantCode: ErrInvalidRejectedEdge,
		},
		{
			name:     "no views",
			mutate:   func(m *Manifest) { m.Views = nil },
			wantCode: ErrNoViews,
		},
		{
			name:     "empty view id",
			mutate:   func(m *Manifest) { m.Views[0].ID = "" },
			wantCode: ErrEmptyViewID,
		},
		{
			name:     "duplicate view id",
			mutate:   func(m *Manifest) { m.Views = append(m.Views, m.Views[0]) },
			wantCode: ErrDuplicateViewID,
		},
		{
			name:     "empty view title",
			mutate:   func(m *Manifest) { m.Views[0].Title = "" },
			wantCode: ErrEmptyViewTitle,
		},
		{
			name: "missing view_0_system_index when >= 2 detail views",
			mutate: func(m *Manifest) {
				v2 := m.Views[0]
				v2.ID = "second_view"
				m.Views = append(m.Views, v2)
			},
			wantCode: ErrMissingSystemIndexView,
		},
		{
			name: "unexpected view_0_system_index when only 1 detail view",
			mutate: func(m *Manifest) {
				v0 := View{
					ID:         SystemIndexViewID,
					Title:      "Index",
					Archetype:  ArchetypeLandscapeMatrix,
					AccentMode: AccentMixed,
					Placements: []Placement{{ComponentID: "gke_platform", Column: 0}},
				}
				m.Views = append([]View{v0}, m.Views...)
			},
			wantCode: ErrUnexpectedSystemIndexView,
		},
		{
			name: "misplaced view_0_system_index not at views[0]",
			mutate: func(m *Manifest) {
				v2 := m.Views[0]
				v2.ID = "second_view"
				v0 := View{
					ID:         SystemIndexViewID,
					Title:      "Index",
					Archetype:  ArchetypeLandscapeMatrix,
					AccentMode: AccentMixed,
					Placements: []Placement{{ComponentID: "gke_platform", Column: 0}},
				}
				m.Views = append(m.Views, v2, v0)
			},
			wantCode: ErrMisplacedSystemIndexView,
		},
		{
			name:     "invalid archetype",
			mutate:   func(m *Manifest) { m.Views[0].Archetype = "freeform" },
			wantCode: ErrInvalidArchetype,
		},
		{
			name:     "invalid accent mode",
			mutate:   func(m *Manifest) { m.Views[0].AccentMode = "NEON" },
			wantCode: ErrInvalidAccentMode,
		},
		{
			name:     "unknown container_ids ref",
			mutate:   func(m *Manifest) { m.Views[0].ContainerIDs = append(m.Views[0].ContainerIDs, "ghost_vpc") },
			wantCode: ErrUnknownContainerRef,
		},
		{
			name:     "0 placements on detail view",
			mutate:   func(m *Manifest) { m.Views[0].Placements = nil },
			wantCode: ErrViewPlacementCount,
		},
		{
			name:     "duplicate placement in view",
			mutate:   func(m *Manifest) { m.Views[0].Placements = append(m.Views[0].Placements, m.Views[0].Placements[0]) },
			wantCode: ErrDuplicatePlacement,
		},
		{
			name: "unknown component ref in detail view",
			mutate: func(m *Manifest) {
				m.Views[0].Placements = append(m.Views[0].Placements, Placement{ComponentID: "ghost", Column: 0})
			},
			wantCode: ErrUnknownComponentRef,
		},
		{
			name: "unknown component ref in system index",
			mutate: func(m *Manifest) {
				v2 := m.Views[0]
				v2.ID = "second_view"
				v0 := View{
					ID: SystemIndexViewID, Title: "Index", Archetype: ArchetypeLandscapeMatrix, AccentMode: AccentMixed,
					Placements: []Placement{{ComponentID: "unknown_summary", Column: 0}},
				}
				m.Views = []View{v0, m.Views[0], v2}
			},
			wantCode: ErrUnknownComponentRef,
		},
		{
			name:     "placement container_id not in view.container_ids",
			mutate:   func(m *Manifest) { m.Views[0].Placements[0].ContainerID = "not_in_view" },
			wantCode: ErrPlacementContainerNotInView,
		},
		{
			name:     "column out of bounds for landscape_matrix",
			mutate:   func(m *Manifest) { m.Views[0].Placements[0].Column = 5 },
			wantCode: ErrColumnOutOfBounds,
		},
		{
			name: "column out of bounds for hub_spoke",
			mutate: func(m *Manifest) {
				m.Views[0].Archetype = ArchetypeHubSpoke
				m.Views[0].Placements[3].Column = 3
			},
			wantCode: ErrColumnOutOfBounds,
		},
		{
			name: "invalid off_page_refs target and direction",
			mutate: func(m *Manifest) {
				m.Views[0].Placements[0].OffPageRefs = []OffPageRef{
					{TargetViewID: "gke_platform", Direction: "SIDEWAYS", Label: ""},
				}
			},
			wantCode: ErrInvalidOffPageRef,
		},
		{
			name:     "edge endpoint not placed in view",
			mutate:   func(m *Manifest) { m.Views[0].Edges[0].To = "not_placed" },
			wantCode: ErrEdgeEndpointNotPlaced,
		},
		{
			name: "rejected edge present in view",
			mutate: func(m *Manifest) {
				m.Views[0].Edges = append(m.Views[0].Edges, Edge{
					From: "atlantis", To: "sonarqube_db", Verb: "forbidden", FlowKind: FlowRuntime,
					Provenance: ProvenanceInferred, InferenceRationale: "test",
				})
			},
			wantCode: ErrRejectedEdgePresent,
		},
		{
			name:     "empty edge verb",
			mutate:   func(m *Manifest) { m.Views[0].Edges[0].Verb = " " },
			wantCode: ErrEmptyEdgeVerb,
		},
		{
			name:     "edge verb longer than 16 chars",
			mutate:   func(m *Manifest) { m.Views[0].Edges[0].Verb = "12345678901234567" },
			wantCode: ErrEdgeVerbTooLong,
		},
		{
			name:     "invalid flow_kind",
			mutate:   func(m *Manifest) { m.Views[0].Edges[0].FlowKind = "BATCH" },
			wantCode: ErrInvalidFlowKind,
		},
		{
			name:     "invalid provenance",
			mutate:   func(m *Manifest) { m.Views[0].Edges[0].Provenance = "GUESSED" },
			wantCode: ErrInvalidProvenance,
		},
		{
			name:     "VERIFIED detail edge missing evidence_edge_ids",
			mutate:   func(m *Manifest) { m.Views[0].Edges[0].EvidenceEdgeIDs = nil },
			wantCode: ErrMissingEvidenceEdgeIDs,
		},
		{
			name:     "VERIFIED detail edge blank entry in evidence_edge_ids",
			mutate:   func(m *Manifest) { m.Views[0].Edges[0].EvidenceEdgeIDs = []string{"   "} },
			wantCode: ErrMissingEvidenceEdgeIDs,
		},
		{
			name:     "INFERRED edge missing inference_rationale",
			mutate:   func(m *Manifest) { m.Views[0].Edges[1].InferenceRationale = "" },
			wantCode: ErrMissingInferenceRationale,
		},
		{
			name: "mixed via_bus_id (multiple sources AND multiple targets) rejected",
			mutate: func(m *Manifest) {
				m.Views[0].Edges[2].ViaBusID = "mixed_bus" // atlantis -> tf_buckets
				m.Views[0].Edges[3].ViaBusID = "mixed_bus" // sonarqube -> sonarqube_db
			},
			wantCode: ErrInvalidBusGroup,
		},
		{
			name: "unplaced component rejected",
			mutate: func(m *Manifest) {
				m.Components = append(m.Components, Component{
					ID: "orphan_comp", Title: "Orphan", IconKey: "cloud_run", ComponentKind: KindWorkload, ProjectID: "acme-cicd",
					BackedBy: []BackedResource{{URI: "//run.googleapis.com/orphan", AssetType: "run.googleapis.com/Service", ProjectID: "acme-cicd"}},
				})
			},
			wantCode: ErrUnplacedComponent,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := baseFixture()
			tc.mutate(m)
			_ = assertHasValidationCode(t, ValidateStructure(m, reg), tc.wantCode)
		})
	}
}

func TestValidateWithFactsProvenanceAndAccounting(t *testing.T) {
	reg := loadTestIcons(t)

	// Nil facts error.
	mNil, _ := baseFixture()
	_ = assertHasValidationCode(t, ValidateWithFacts(mNil, nil, reg), ErrNilFacts)
	// Nil manifest with non-nil facts.
	_, fOnly := baseFixture()
	_ = assertHasValidationCode(t, ValidateWithFacts(nil, fOnly, reg), ErrInvalidSchemaVersion)

	// 1. Unordered multi-hop evidence_edge_ids still form a valid directed walk!
	mUnordered, fUnordered := baseFixture()
	origIDs := mUnordered.Views[0].Edges[0].EvidenceEdgeIDs
	mUnordered.Views[0].Edges[0].EvidenceEdgeIDs = []string{origIDs[2], origIDs[0], origIDs[1]}
	if err := ValidateWithFacts(mUnordered, fUnordered, reg); err != nil {
		t.Fatalf("unordered valid multi-hop walk failed: %v", err)
	}

	// 2. Unaccounted FactResource rejected.
	mMissing, fMissing := baseFixture()
	fMissing.Resources = append(fMissing.Resources, extract.FactResource{
		URI: "//sqladmin.googleapis.com/projects/acme-cicd/instances/orphaned-db", AssetType: "sqladmin.googleapis.com/Instance", ProjectID: "acme-cicd",
	})
	ve := assertHasValidationCode(t, ValidateWithFacts(mMissing, fMissing, reg), ErrUnaccountedFactResource)
	if !strings.Contains(ve.Error(), "orphaned-db") {
		t.Fatalf("expected orphaned-db URI in error: %s", ve.Error())
	}

	// 3. Unknown evidence_edge_id rejected.
	mUnknown, fUnknown := baseFixture()
	mUnknown.Views[0].Edges[3].EvidenceEdgeIDs = []string{"0000000000000000"}
	_ = assertHasValidationCode(t, ValidateWithFacts(mUnknown, fUnknown, reg), ErrUnknownEvidenceEdge)

	// 4. Wrong direction edge rejected (sonarqube_db -> sonarqube cited using sonarqube -> sonarqube_db).
	mRev, fRev := baseFixture()
	mRev.Views[0].Edges[3].From = "sonarqube_db"
	mRev.Views[0].Edges[3].To = "sonarqube"
	_ = assertHasValidationCode(t, ValidateWithFacts(mRev, fRev, reg), ErrInvalidEdgeProvenance)

	// 5. Broken multi-hop chain (missing middle hop NEG -> Service) rejected.
	mBroken, fBroken := baseFixture()
	mBroken.Views[0].Edges[0].EvidenceEdgeIDs = []string{fBroken.Edges[0].ID, fBroken.Edges[2].ID}
	_ = assertHasValidationCode(t, ValidateWithFacts(mBroken, fBroken, reg), ErrInvalidEdgeProvenance)

	// 6. Valid walk plus unrelated extraneous edge rejected.
	mNoise, fNoise := baseFixture()
	mNoise.Views[0].Edges[3].EvidenceEdgeIDs = []string{fNoise.Edges[5].ID, fNoise.Edges[3].ID}
	_ = assertHasValidationCode(t, ValidateWithFacts(mNoise, fNoise, reg), ErrInvalidEdgeProvenance)

	// 7. Valid start edge that dead-ends without reaching target rejected.
	mDeadEnd, fDeadEnd := baseFixture()
	mDeadEnd.Views[0].Edges[0].EvidenceEdgeIDs = []string{fDeadEnd.Edges[0].ID}
	_ = assertHasValidationCode(t, ValidateWithFacts(mDeadEnd, fDeadEnd, reg), ErrInvalidEdgeProvenance)

	// 8. Blank entry in evidence_edge_ids rejected cleanly under ValidateWithFacts.
	mBlankEv, fBlankEv := baseFixture()
	mBlankEv.Views[0].Edges[3].EvidenceEdgeIDs = []string{fBlankEv.Edges[5].ID, "  "}
	_ = assertHasValidationCode(t, ValidateWithFacts(mBlankEv, fBlankEv, reg), ErrMissingEvidenceEdgeIDs)
}

func placementComponentIDs(placements []Placement) []string {
	ids := make([]string, len(placements))
	for i, p := range placements {
		ids[i] = p.ComponentID
	}
	return ids
}

func findEdgeEvidence(edges []Edge, from, to string) []string {
	for _, e := range edges {
		if e.From == from && e.To == to {
			return e.EvidenceEdgeIDs
		}
	}
	return nil
}

func assertLoadedRoundTripOrder(t *testing.T, loaded *Manifest, origPlacementOrder, origEvidenceOrder []string) {
	t.Helper()
	if !reflect.DeepEqual(loaded.Metadata.Scopes, []string{"projects/a", "projects/z"}) {
		t.Fatalf("Scopes not sorted: %v", loaded.Metadata.Scopes)
	}
	if loaded.Views[0].ID != SystemIndexViewID || loaded.Views[1].ID != "a_network" || loaded.Views[2].ID != "gke_platform" {
		t.Fatalf("Views order wrong: %q, %q, %q", loaded.Views[0].ID, loaded.Views[1].ID, loaded.Views[2].ID)
	}

	// Verify Placements order preserved on gke_platform.
	gotPlacementOrder := placementComponentIDs(loaded.Views[2].Placements)
	if !reflect.DeepEqual(gotPlacementOrder, origPlacementOrder) {
		t.Fatalf("Placements order altered: got %v, want %v", gotPlacementOrder, origPlacementOrder)
	}
	if !reflect.DeepEqual(loaded.Views[2].Placements[0].OffPageRefs[0].TargetViewID, "a_view") {
		t.Fatalf("OffPageRefs not sorted: %+v", loaded.Views[2].Placements[0].OffPageRefs)
	}

	// Verify EvidenceEdgeIDs order preserved on https_lb -> atlantis edge.
	gotEv := findEdgeEvidence(loaded.Views[2].Edges, "https_lb", "atlantis")
	if !reflect.DeepEqual(gotEv, origEvidenceOrder) {
		t.Fatalf("EvidenceEdgeIDs order altered: got %v, want %v", gotEv, origEvidenceOrder)
	}
}

func TestMarshalWriteLoadRoundTripAndOrderPreservation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "architecture.yaml")

	m, _ := baseFixture()
	// Shuffle top-level slices & add multi-view with view_0_system_index placed LAST to verify
	// Marshal pins view_0_system_index at views[0], sorts detail views/components/containers,
	// AND preserves Placements order, InlineMeta order, and EvidenceEdgeIDs order!
	m.Metadata.Scopes = []string{"projects/z", "projects/a"}
	m.Components[0], m.Components[4] = m.Components[4], m.Components[0]
	m.Containers[0], m.Containers[1] = m.Containers[1], m.Containers[0]
	m.ExcludedResources[0], m.ExcludedResources[1] = m.ExcludedResources[1], m.ExcludedResources[0]
	m.Views[0].ContainerIDs = []string{"vpc_cicd", "gke_cicd"}
	m.Views[0].Placements[0].OffPageRefs = []OffPageRef{
		{TargetViewID: "z_view", Label: "Z", Direction: "OUT"},
		{TargetViewID: "a_view", Label: "A", Direction: "IN"},
	}

	origPlacementOrder := placementComponentIDs(m.Views[0].Placements)
	origEvidenceOrder := append([]string(nil), m.Views[0].Edges[0].EvidenceEdgeIDs...)

	v2 := View{ID: "a_network", Title: "Network", Archetype: ArchetypeHubSpoke, AccentMode: AccentMixed, Placements: []Placement{{ComponentID: "atlantis", Column: 1}}}
	v0 := View{ID: SystemIndexViewID, Title: "Index", Archetype: ArchetypeLandscapeMatrix, AccentMode: AccentMixed, Placements: []Placement{{ComponentID: "gke_platform", Column: 0}}}
	m.Views = []View{m.Views[0], v2, v0}

	if err := Write(path, m); err != nil {
		t.Fatalf("Write error: %v", err)
	}

	// Verify caller not mutated.
	if m.Views[0].ID != "gke_platform" || m.Metadata.Scopes[0] != "projects/z" {
		t.Fatal("Marshal/Write mutated caller Manifest")
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("manifest perm = %o, want 0644", fi.Mode().Perm())
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	assertLoadedRoundTripOrder(t, loaded, origPlacementOrder, origEvidenceOrder)

	// Re-marshaling must be byte-identical.
	b1, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatalf("Marshal not byte-deterministic:\n%s\nvs\n%s", b1, b2)
	}
}

func TestMarshalNilAndEdgeSortTieBreakers(t *testing.T) {
	if _, err := Marshal(nil); err == nil {
		t.Fatal("expected error marshaling nil manifest")
	}

	// Verify edge sort tie-breakers on InferenceRationale and EvidenceEdgeIDs.
	tieA := &Manifest{
		SchemaVersion: SchemaVersion,
		Views: []View{{
			ID: "v",
			Edges: []Edge{
				{From: "a", To: "b", Verb: "v", FlowKind: FlowRuntime, Provenance: ProvenanceInferred, InferenceRationale: "reason-z"},
				{From: "a", To: "b", Verb: "v", FlowKind: FlowRuntime, Provenance: ProvenanceInferred, InferenceRationale: "reason-a"},
				{From: "a", To: "b", Verb: "v", FlowKind: FlowRuntime, Provenance: ProvenanceVerified, EvidenceEdgeIDs: []string{"e2"}},
				{From: "a", To: "b", Verb: "v", FlowKind: FlowRuntime, Provenance: ProvenanceVerified, EvidenceEdgeIDs: []string{"e1"}},
			},
		}},
	}
	tieB := &Manifest{
		SchemaVersion: SchemaVersion,
		Views: []View{{
			ID: "v",
			Edges: []Edge{
				{From: "a", To: "b", Verb: "v", FlowKind: FlowRuntime, Provenance: ProvenanceVerified, EvidenceEdgeIDs: []string{"e1"}},
				{From: "a", To: "b", Verb: "v", FlowKind: FlowRuntime, Provenance: ProvenanceInferred, InferenceRationale: "reason-a"},
				{From: "a", To: "b", Verb: "v", FlowKind: FlowRuntime, Provenance: ProvenanceVerified, EvidenceEdgeIDs: []string{"e2"}},
				{From: "a", To: "b", Verb: "v", FlowKind: FlowRuntime, Provenance: ProvenanceInferred, InferenceRationale: "reason-z"},
			},
		}},
	}
	tb1, err := Marshal(tieA)
	if err != nil {
		t.Fatal(err)
	}
	tb2, err := Marshal(tieB)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tb1, tb2) {
		t.Fatalf("Marshal edge tie-breakers not deterministic:\n%s\nvs\n%s", tb1, tb2)
	}
}

func TestLoadStrictKnownFieldsAndErrors(t *testing.T) {
	dir := t.TempDir()

	// Unknown field must be rejected by KnownFields(true).
	unknownPath := filepath.Join(dir, "unknown.yaml")
	if err := os.WriteFile(unknownPath, []byte("schema_version: v1\nunknown_key: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(unknownPath); err == nil {
		t.Fatal("expected KnownFields(true) error on unknown YAML key")
	}

	// Empty file rejected.
	emptyPath := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(emptyPath, []byte("# comment only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(emptyPath); err == nil {
		t.Fatal("expected error loading empty YAML")
	}

	// Trailing second YAML document rejected.
	multiDocPath := filepath.Join(dir, "multidoc.yaml")
	if err := os.WriteFile(multiDocPath, []byte("schema_version: v1\n---\nschema_version: v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(multiDocPath); err == nil {
		t.Fatal("expected error on trailing YAML document")
	}

	// Missing file rejected.
	if _, err := Load(filepath.Join(dir, "does-not-exist.yaml")); err == nil {
		t.Fatal("expected error loading missing file")
	}
}
