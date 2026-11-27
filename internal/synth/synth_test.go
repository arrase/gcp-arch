package synth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/icons"
	"github.com/arrase/gcp-arch/internal/manifest"
)

type stubModelClient struct {
	resp       annotationResponse
	lastPrompt string
	lastSchema *Schema
	calls      int
}

func (s *stubModelClient) GenerateJSON(_ context.Context, _, userPrompt string, schema *Schema) ([]byte, error) {
	s.calls++
	s.lastPrompt = userPrompt
	s.lastSchema = schema
	return json.Marshal(s.resp)
}

func TestBuildSkeletonAndConservationAllCatalogFixtures(t *testing.T) {
	reg, err := icons.Load()
	if err != nil {
		t.Fatalf("icons.Load(): %v", err)
	}
	for _, path := range loadAllCatalogAndAcmePaths(t) {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			verifySkeletonAndConservationForFixture(t, path, name, reg)
		})
	}
}

func TestBuildSkeletonAndConservationAllExampleFixtures(t *testing.T) {
	reg, err := icons.Load()
	if err != nil {
		t.Fatal(err)
	}
	exampleFiles, err := filepath.Glob(filepath.Join("..", "..", "testdata", "examples", "*.facts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(exampleFiles) != 5 {
		t.Fatalf("expected 5 example fixtures under testdata/examples, got %d", len(exampleFiles))
	}
	for _, path := range exampleFiles {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			verifySkeletonAndConservationForFixture(t, path, name, reg)
			facts, err := extract.LoadFacts(path)
			if err != nil {
				t.Fatal(err)
			}
			skel := buildSkeleton(facts, reg)
			assertZeroOrphanNodes(t, name, skel)
		})
	}
}

func loadAllCatalogAndAcmePaths(t *testing.T) []string {
	t.Helper()
	catalogFiles, err := filepath.Glob(filepath.Join("..", "..", "testdata", "catalog", "*.facts.json"))
	if err != nil {
		t.Fatalf("glob catalog: %v", err)
	}
	allFiles := append([]string{filepath.Join("..", "..", "testdata", "acme_cicd_v3.facts.json")}, catalogFiles...)
	if len(allFiles) != 11 {
		t.Fatalf("expected 11 fixtures, got %d", len(allFiles))
	}
	return allFiles
}

func verifySkeletonAndConservationForFixture(t *testing.T, path, name string, reg *icons.Registry) {
	t.Helper()
	facts, err := extract.LoadFacts(path)
	if err != nil {
		t.Fatalf("LoadFacts(%s): %v", path, err)
	}
	skel := buildSkeleton(facts, reg)
	if len(skel.Nodes) == 0 {
		t.Fatalf("%s: expected non-empty Nodes", name)
	}
	for _, n := range skel.Nodes {
		if !reg.Has(n.IconKey) {
			t.Errorf("%s: node %q (%s) has invalid icon key %q", name, n.ID, n.AssetType, n.IconKey)
		}
		if len(n.Members) == 0 {
			t.Errorf("%s: node %q has empty Members", name, n.ID)
		}
	}

	m := assembleManifest(skel, annotationResponse{}, reg, "2026-11-26T00:00:00Z")
	rep, err := manifest.ValidateWithFactsAndConservation(m, facts, reg)
	if err != nil {
		t.Fatalf("%s: ValidateWithFactsAndConservation failed: %v", name, err)
	}
	if rep.Status != "PASS" || rep.View0Coverage != 1.0 {
		t.Fatalf("%s: conservation failed or incomplete View0: %+v", name, rep)
	}

	wantViews := 1 + targetDetailViewCount(len(m.Components))
	if len(m.Views) != wantViews {
		t.Fatalf("%s: got %d views for %d components, want %d", name, len(m.Views), len(m.Components), wantViews)
	}
	verifyManifestRoundTrip(t, m, facts, reg, name)
}

func verifyManifestRoundTrip(t *testing.T, m *manifest.Manifest, facts *extract.FactsFile, reg *icons.Registry, name string) {
	t.Helper()
	yamlBytes, err := manifest.Marshal(m)
	if err != nil {
		t.Fatalf("%s: Marshal failed: %v", name, err)
	}
	tmpYAML := filepath.Join(t.TempDir(), "arch.yaml")
	if err := manifest.Write(tmpYAML, m); err != nil {
		t.Fatalf("%s: Write failed: %v", name, err)
	}
	loaded, err := manifest.Load(tmpYAML)
	if err != nil {
		t.Fatalf("%s: Load KnownFields failed: %v\nyaml:\n%s", name, err, string(yamlBytes))
	}
	if err := manifest.ValidateWithFacts(loaded, facts, reg); err != nil {
		t.Fatalf("%s: ValidateWithFacts after Load failed: %v", name, err)
	}
}

func assertZeroOrphanNodes(t *testing.T, name string, skel *skeleton) {
	t.Helper()
	deg := make(map[string]int, len(skel.Nodes))
	for _, e := range skel.Edges {
		deg[e.FromNodeID]++
		deg[e.ToNodeID]++
	}
	for _, n := range skel.Nodes {
		if deg[n.ID] == 0 {
			t.Errorf("%s: orphan 0-degree node %q (%s)", name, n.Title, n.AssetType)
		}
	}
}

func TestSkeletonZeroOrphansAcrossAllCatalogFixtures(t *testing.T) {
	reg, err := icons.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range loadAllCatalogAndAcmePaths(t) {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			facts, err := extract.LoadFacts(path)
			if err != nil {
				t.Fatal(err)
			}
			assertZeroOrphanNodes(t, name, buildSkeleton(facts, reg))
		})
	}
}

func TestRegionScopedContainerIDsPreventCollisions(t *testing.T) {
	reg, err := icons.Load()
	if err != nil {
		t.Fatal(err)
	}
	vpcURI := "//compute.googleapis.com/projects/p1/global/networks/vpc1"
	subEU := "//compute.googleapis.com/projects/p1/regions/europe-west1/subnetworks/default"
	subUS := "//compute.googleapis.com/projects/p1/regions/us-central1/subnetworks/default"
	gkeEU := "//container.googleapis.com/projects/p1/locations/europe-west1/clusters/main"
	gkeUS := "//container.googleapis.com/projects/p1/locations/us-central1/clusters/main"

	facts := &extract.FactsFile{
		SchemaVersion: extract.FactsSchemaVersion,
		Scopes:        []string{"projects/p1"},
		Hierarchy:     extract.Hierarchy{Projects: []extract.ProjectNode{{ProjectID: "p1"}}},
		Resources: []extract.FactResource{
			{URI: vpcURI, AssetType: "compute.googleapis.com/Network", ProjectID: "p1", DisplayName: "vpc1"},
			{URI: subEU, AssetType: "compute.googleapis.com/Subnetwork", ProjectID: "p1", Location: "europe-west1", DisplayName: "default", ParentURI: vpcURI},
			{URI: subUS, AssetType: "compute.googleapis.com/Subnetwork", ProjectID: "p1", Location: "us-central1", DisplayName: "default", ParentURI: vpcURI},
			{URI: gkeEU, AssetType: "container.googleapis.com/Cluster", ProjectID: "p1", Location: "europe-west1", DisplayName: "main", ParentURI: subEU},
			{URI: gkeUS, AssetType: "container.googleapis.com/Cluster", ProjectID: "p1", Location: "us-central1", DisplayName: "main", ParentURI: subUS},
			{URI: "//sqladmin.googleapis.com/projects/p1/instances/db1", AssetType: "sqladmin.googleapis.com/Instance", ProjectID: "p1", Location: "europe-west1", DisplayName: "db1", ParentURI: vpcURI, Attributes: map[string]any{"psa_tenant_project": "tenant-a"}},
			{URI: "//sqladmin.googleapis.com/projects/p1/instances/db2", AssetType: "sqladmin.googleapis.com/Instance", ProjectID: "p1", Location: "europe-west1", DisplayName: "db2", ParentURI: vpcURI, Attributes: map[string]any{"psa_tenant_project": "tenant-b"}},
			{URI: "//compute.googleapis.com/projects/p1/global/forwardingRules/lb1", AssetType: "compute.googleapis.com/GlobalForwardingRule", ProjectID: "p1", DisplayName: "lb1", Attributes: map[string]any{"load_balancing_scheme": "EXTERNAL", "ip_address": "203.0.113.10"}},
			{URI: "//compute.googleapis.com/projects/p1/global/forwardingRules/lb2", AssetType: "compute.googleapis.com/GlobalForwardingRule", ProjectID: "p1", DisplayName: "lb2", Attributes: map[string]any{"load_balancing_scheme": "EXTERNAL_MANAGED", "ip_address": "203.0.113.11", "port_range": "443"}},
		},
	}

	skel := buildSkeleton(facts, reg)

	countKind := func(kind manifest.ContainerKind) int {
		n := 0
		for _, ct := range skel.Containers {
			if ct.Kind == kind {
				n++
			}
		}
		return n
	}
	if got := countKind(manifest.ContainerSubnet); got != 2 {
		t.Fatalf("expected 2 distinct region-scoped subnets, got %d", got)
	}
	if got := countKind(manifest.ContainerGKECluster); got != 2 {
		t.Fatalf("expected 2 distinct location-scoped GKE clusters, got %d", got)
	}
	if got := countKind(manifest.ContainerPSAZone); got != 2 {
		t.Fatalf("expected 2 distinct PSA tenant zones, got %d", got)
	}
	if got := countKind(manifest.ContainerExposureZone); got != 2 {
		t.Fatalf("expected 2 distinct LB scheme exposure zones, got %d", got)
	}

	// Verify ForwardingRule VIP inline metadata has no dangling colon when port_range is empty.
	for _, n := range skel.Nodes {
		if n.Title == "lb1" {
			if len(n.InlineMeta) == 0 || n.InlineMeta[0].Value != "203.0.113.10" {
				t.Fatalf("lb1 VIP = %+v, want 203.0.113.10 (no trailing colon)", n.InlineMeta)
			}
		}
		if n.Title == "lb2" {
			if len(n.InlineMeta) == 0 || n.InlineMeta[0].Value != "203.0.113.11:443" {
				t.Fatalf("lb2 VIP = %+v, want 203.0.113.11:443", n.InlineMeta)
			}
		}
	}
}

func TestUniqueNodeIDsAndContentBasedTransversalIDs(t *testing.T) {
	reg, err := icons.Load()
	if err != nil {
		t.Fatal(err)
	}
	// Three colliding display names on non-cohort resources: two "foo" and one literal "foo-2".
	facts := &extract.FactsFile{
		SchemaVersion: extract.FactsSchemaVersion,
		Scopes:        []string{"projects/p1"},
		Resources: []extract.FactResource{
			{URI: "//sqladmin.googleapis.com/projects/p1/instances/a", AssetType: "sqladmin.googleapis.com/Instance", ProjectID: "p1", DisplayName: "foo"},
			{URI: "//sqladmin.googleapis.com/projects/p1/instances/b", AssetType: "sqladmin.googleapis.com/Instance", ProjectID: "p1", DisplayName: "foo"},
			{URI: "//sqladmin.googleapis.com/projects/p1/instances/c", AssetType: "sqladmin.googleapis.com/Instance", ProjectID: "p1", DisplayName: "foo-2"},
			{URI: "//secretmanager.googleapis.com/projects/p1/secrets/db-pass", AssetType: "secretmanager.googleapis.com/Secret", ProjectID: "p1", DisplayName: "db-pass"},
		},
	}

	skel1 := buildSkeleton(facts, reg)
	seen := make(map[string]bool)
	for _, n := range skel1.Nodes {
		if seen[n.ID] {
			t.Fatalf("duplicate node ID generated: %q", n.ID)
		}
		seen[n.ID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("expected 3 unique node IDs, got %d: %v", len(seen), seen)
	}
	origTID := skel1.TransversalItems[0].ID
	if strings.HasPrefix(origTID, "tr_001_") {
		t.Fatalf("transversal ID must be content-based, not positional: %q", origTID)
	}

	// Inserting an alphabetically earlier transversal item must NOT shift db-pass's ID.
	facts2 := *facts
	facts2.Resources = append(slices.Clone(facts.Resources), extract.FactResource{
		URI: "//iam.googleapis.com/projects/p1/serviceAccounts/aaa@p1.iam.gserviceaccount.com", AssetType: "iam.googleapis.com/ServiceAccount", ProjectID: "p1", DisplayName: "aaa",
	})
	skel2 := buildSkeleton(&facts2, reg)
	found := false
	for _, ti := range skel2.TransversalItems {
		if ti.DisplayName == "db-pass" {
			found = true
			if ti.ID != origTID {
				t.Fatalf("transversal ID drifted from %q to %q after unrelated insertion", origTID, ti.ID)
			}
		}
	}
	if !found {
		t.Fatal("db-pass transversal item missing")
	}
}

func TestGenericBuildCohortTitle(t *testing.T) {
	reg, err := icons.Load()
	if err != nil {
		t.Fatal(err)
	}
	targetURI := "//cloudresourcemanager.googleapis.com/projects/billing-staging"
	t1 := "//cloudbuild.googleapis.com/projects/ci-proj/locations/global/triggers/deploy-a"
	t2 := "//cloudbuild.googleapis.com/projects/ci-proj/locations/global/triggers/deploy-b"
	t3 := "//cloudbuild.googleapis.com/projects/ci-proj/locations/global/triggers/deploy-c"

	facts := &extract.FactsFile{
		SchemaVersion: extract.FactsSchemaVersion,
		Scopes:        []string{"projects/ci-proj"},
		Resources: []extract.FactResource{
			{URI: targetURI, AssetType: "cloudresourcemanager.googleapis.com/Project", ProjectID: "billing-staging", DisplayName: "billing-staging", IsExternal: true},
			{URI: t1, AssetType: "cloudbuild.googleapis.com/BuildTrigger", ProjectID: "ci-proj", DisplayName: "deploy-a"},
			{URI: t2, AssetType: "cloudbuild.googleapis.com/BuildTrigger", ProjectID: "ci-proj", DisplayName: "deploy-b"},
			{URI: t3, AssetType: "cloudbuild.googleapis.com/BuildTrigger", ProjectID: "ci-proj", DisplayName: "deploy-c"},
		},
		Edges: []extract.FactEdge{
			{FromURI: t1, ToURI: targetURI, Kind: extract.EdgeCloudBuildStepRef, Detail: "--project=billing-staging"},
			{FromURI: t2, ToURI: targetURI, Kind: extract.EdgeCloudBuildStepRef, Detail: "--project=billing-staging"},
			{FromURI: t3, ToURI: targetURI, Kind: extract.EdgeCloudBuildStepRef, Detail: "--project=billing-staging"},
		},
	}

	skel := buildSkeleton(facts, reg)
	var cohortTitle string
	for _, n := range skel.Nodes {
		if n.ComponentKind == manifest.KindCohort {
			cohortTitle = n.Title
		}
	}
	if cohortTitle != "billing-staging App Deploy Triggers (3x)" {
		t.Fatalf("cohort title = %q, want %q", cohortTitle, "billing-staging App Deploy Triggers (3x)")
	}
}

func TestComputeTopoRanksBreaksSingleAndNestedCycles(t *testing.T) {
	t.Run("simple push-callback cycle", func(t *testing.T) {
		nodes := []skeletonNode{
			{ID: "svc_a", ComponentKind: manifest.KindWorkload, TopoRank: 2},
			{ID: "topic", ComponentKind: manifest.KindMessaging, TopoRank: 3},
			{ID: "sub", ComponentKind: manifest.KindMessaging, TopoRank: 3},
			{ID: "bq_raw", ComponentKind: manifest.KindDatastore, TopoRank: 5},
			{ID: "bq_mart", ComponentKind: manifest.KindDatastore, TopoRank: 5},
		}
		edges := []skeletonEdge{
			{ID: "e1", FromNodeID: "svc_a", ToNodeID: "topic"},
			{ID: "e2", FromNodeID: "topic", ToNodeID: "sub"},
			{ID: "e3", FromNodeID: "sub", ToNodeID: "svc_a"},
			{ID: "e4", FromNodeID: "svc_a", ToNodeID: "bq_raw"},
			{ID: "e5", FromNodeID: "bq_raw", ToNodeID: "bq_mart"},
		}
		computeTopoRanks(nodes, edges)
		rankOf := make(map[string]int, len(nodes))
		for _, n := range nodes {
			rankOf[n.ID] = n.TopoRank
		}
		if rankOf["svc_a"] >= rankOf["topic"] || rankOf["topic"] >= rankOf["sub"] {
			t.Fatalf("expected svc_a < topic < sub, got %+v", rankOf)
		}
		if rankOf["bq_raw"] >= rankOf["bq_mart"] {
			t.Fatalf("expected bq_raw (%d) < bq_mart (%d) despite upstream cycle", rankOf["bq_raw"], rankOf["bq_mart"])
		}
	})

	t.Run("nested figure-8 double cycle produces strict DAG", func(t *testing.T) {
		nodes := []skeletonNode{
			{ID: "a", ComponentKind: manifest.KindWorkload, TopoRank: 1},
			{ID: "b", ComponentKind: manifest.KindWorkload, TopoRank: 2},
			{ID: "c", ComponentKind: manifest.KindWorkload, TopoRank: 3},
			{ID: "d", ComponentKind: manifest.KindDatastore, TopoRank: 5},
		}
		edges := []skeletonEdge{
			{ID: "e1", FromNodeID: "a", ToNodeID: "b"},
			{ID: "e2", FromNodeID: "b", ToNodeID: "a"},
			{ID: "e3", FromNodeID: "b", ToNodeID: "c"},
			{ID: "e4", FromNodeID: "c", ToNodeID: "b"},
			{ID: "e5", FromNodeID: "c", ToNodeID: "d"},
		}
		computeTopoRanks(nodes, edges)
		rankOf := make(map[string]int, len(nodes))
		for _, n := range nodes {
			rankOf[n.ID] = n.TopoRank
		}
		if rankOf["a"] >= rankOf["b"] || rankOf["b"] >= rankOf["c"] || rankOf["c"] >= rankOf["d"] {
			t.Fatalf("expected strict monotonic progression a < b < c < d, got %+v", rankOf)
		}
	})
}

func TestSynthesizePromptTrimmingAndAnnotationBoundaries(t *testing.T) {
	reg, err := icons.Load()
	if err != nil {
		t.Fatal(err)
	}
	facts, err := extract.LoadFacts(filepath.Join("..", "..", "testdata", "catalog", "cat5_high_fan_in_out_24svc.facts.json"))
	if err != nil {
		t.Fatal(err)
	}
	skel := buildSkeleton(facts, reg)

	resp := annotationResponse{
		FocusSummary: "End-to-end fulfillment & payments runtime",
		DetailViews: []annotationDetailView{{
			ViewSlot:     "view_1",
			Title:        "Fulfillment Runtime",
			Subtitle:     "Order routing",
			FocusSummary: "Fulfillment path",
			NodeIDs:      []string{skel.Nodes[0].ID, skel.Nodes[1].ID, "hallucinated_node"},
		}},
	}
	for _, n := range skel.Nodes {
		resp.NodeEnrichments = append(resp.NodeEnrichments, annotationNodeEnrich{
			NodeID:   n.ID,
			Subtitle: "Bad ellipsis...",
		})
	}
	for _, e := range skel.Edges {
		resp.EdgeEnrichments = append(resp.EdgeEnrichments, annotationEdgeEnrich{
			EdgeID:      e.ID,
			Verb:        "Extremely Long Edge Verb Over Limit",
			FlowKind:    string(manifest.FlowRuntime),
			SemanticTag: "traffic",
		})
	}

	client := &stubModelClient{resp: resp}
	result, err := Synthesize(context.Background(), facts, client, reg)
	if err != nil {
		t.Fatalf("Synthesize failed: %v", err)
	}
	if result.Conservation.Status != "PASS" {
		t.Fatalf("expected conservation PASS, got %+v", result.Conservation)
	}
	if strings.Contains(client.lastPrompt, "valid_icon_keys") {
		t.Fatal("prompt payload must not include valid_icon_keys")
	}
	nodeProps := client.lastSchema.Properties["node_enrichments"].Items.Properties
	if _, hasSummary := nodeProps["summary"]; hasSummary {
		t.Fatal("schema must not request unused summary")
	}
	if _, hasIconKey := nodeProps["icon_key"]; hasIconKey {
		t.Fatal("schema must not request deterministic icon_key")
	}

	m := result.Manifest
	for _, c := range m.Components {
		if !reg.Has(c.IconKey) {
			t.Errorf("component %q has invalid icon %q", c.ID, c.IconKey)
		}
		if strings.Contains(c.Subtitle, "...") {
			t.Errorf("component %q leaked ellipsis subtitle %q", c.ID, c.Subtitle)
		}
	}
	placedDetail := make(map[string]bool)
	for _, v := range m.Views {
		for _, p := range v.Placements {
			if p.ComponentID == "hallucinated_node" {
				t.Fatalf("view %q retained hallucinated node", v.ID)
			}
			if v.ID != manifest.SystemIndexViewID {
				placedDetail[p.ComponentID] = true
			}
		}
		for _, e := range v.Edges {
			if utf8.RuneCountInString(e.Verb) > 14 {
				t.Errorf("edge verb %q exceeded 14 runes", e.Verb)
			}
		}
	}
	for _, c := range m.Components {
		if !placedDetail[c.ID] {
			t.Errorf("component %q omitted from detail views", c.ID)
		}
	}
}

func TestAnnotationSanitization(t *testing.T) {
	if got := cleanAnnotation("Private AlloyDB Cluster (PSA)',"); got != "Private AlloyDB Cluster (PSA)" {
		t.Fatalf("cleanAnnotation stray quote/comma = %q", got)
	}
	if got := cleanAnnotation(`Sends data";`); got != "Sends data" {
		t.Fatalf("cleanAnnotation stray dquote/semi = %q", got)
	}
	if got := cleanAnnotation("Truncated\u2026"); got != "" {
		t.Fatalf("cleanAnnotation unicode ellipsis = %q, want empty", got)
	}
	if got := cleanAnnotation("Bad ellipsis..."); got != "" {
		t.Fatalf("cleanAnnotation ASCII ellipsis = %q, want empty", got)
	}
	if got := cleanAnnotation(`Balanced "orders"`); got != `Balanced "orders"` {
		t.Fatalf("cleanAnnotation stripped balanced quotes: %q", got)
	}
	if got := cleanAnnotation("Private Cloud Run Cohort (5 Services) (5x) (5x) (5x) (5x)"); got != "Private Cloud Run Cohort (5 Services) (5x)" {
		t.Fatalf("cleanAnnotation repeated count spam = %q", got)
	}
	if got := cleanAnnotation("Verify that that order routes cleanly"); got != "Verify that that order routes cleanly" {
		t.Fatalf("cleanAnnotation must not delete consecutive English words, got %q", got)
	}

	if got := clampVerb(`Sends data";`, 14); got != "Sends data" {
		t.Fatalf("clampVerb stray dquote/semi = %q", got)
	}
	if got := clampVerb("Truncated\u2026", 14); got != "Truncated" {
		t.Fatalf("clampVerb unicode ellipsis = %q", got)
	}
	if got := clampVerb("\u2026", 14); got != "Calls" {
		t.Fatalf("clampVerb only ellipsis = %q, want Calls", got)
	}
}

func TestExposureClassificationExact(t *testing.T) {
	reg, err := icons.Load()
	if err != nil {
		t.Fatal(err)
	}
	facts := &extract.FactsFile{
		SchemaVersion: extract.FactsSchemaVersion,
		Scopes:        []string{"projects/p1"},
		Hierarchy:     extract.Hierarchy{Projects: []extract.ProjectNode{{ProjectID: "p1", DisplayName: "p1"}}},
		Resources: []extract.FactResource{
			{URI: "//compute.googleapis.com/projects/p1/global/networks/vpc1", AssetType: "compute.googleapis.com/Network", ProjectID: "p1", DisplayName: "vpc1"},
			{URI: "//compute.googleapis.com/projects/p1/regions/europe-west1/subnetworks/sub-priv", AssetType: "compute.googleapis.com/Subnetwork", ProjectID: "p1", Location: "europe-west1", DisplayName: "sub-priv", ParentURI: "//compute.googleapis.com/projects/p1/global/networks/vpc1"},
			{URI: "//cloudfunctions.googleapis.com/projects/p1/locations/europe-west1/functions/fn-internal", AssetType: "cloudfunctions.googleapis.com/CloudFunction", ProjectID: "p1", Location: "europe-west1", DisplayName: "fn-internal", ParentURI: "//compute.googleapis.com/projects/p1/regions/europe-west1/subnetworks/sub-priv", Attributes: map[string]any{"ingress": "ALLOW_INTERNAL_ONLY"}},
			{URI: "//run.googleapis.com/projects/p1/locations/europe-west1/services/svc-public", AssetType: "run.googleapis.com/Service", ProjectID: "p1", Location: "europe-west1", DisplayName: "svc-public", ParentURI: "//compute.googleapis.com/projects/p1/regions/europe-west1/subnetworks/sub-priv", Attributes: map[string]any{"ingress": "INGRESS_TRAFFIC_ALL"}},
		},
	}

	skel := buildSkeleton(facts, reg)
	byTitle := make(map[string]skeletonNode)
	for _, n := range skel.Nodes {
		byTitle[n.Title] = n
	}
	if got := byTitle["fn-internal"].Exposure; got != "PRIVATE" {
		t.Fatalf("ALLOW_INTERNAL_ONLY misclassified as %q, want PRIVATE", got)
	}
	if got := byTitle["svc-public"].Exposure; got != "PUBLIC" {
		t.Fatalf("INGRESS_TRAFFIC_ALL misclassified as %q, want PUBLIC", got)
	}
}

func TestSharedVPCSingleHostAndMembersRetention(t *testing.T) {
	reg, err := icons.Load()
	if err != nil {
		t.Fatal(err)
	}
	cat3Facts, err := extract.LoadFacts(filepath.Join("..", "..", "testdata", "catalog", "cat3_shared_vpc_hybrid_armor.facts.json"))
	if err != nil {
		t.Fatal(err)
	}
	cat3Skel := buildSkeleton(cat3Facts, reg)
	vpcCount := 0
	for _, ct := range cat3Skel.Containers {
		if ct.Kind == manifest.ContainerVPC {
			vpcCount++
		}
	}
	if vpcCount != 1 {
		t.Fatalf("expected exactly 1 Shared VPC container, got %d", vpcCount)
	}
	svcProjTagged := 0
	for _, n := range cat3Skel.Nodes {
		if n.ServiceProjectID != "" {
			svcProjTagged++
		}
	}
	if svcProjTagged == 0 {
		t.Fatal("expected service-project workloads to carry ServiceProjectID")
	}

	acmeFacts, err := extract.LoadFacts(filepath.Join("..", "..", "testdata", "acme_cicd_v3.facts.json"))
	if err != nil {
		t.Fatal(err)
	}
	acmeSkel := buildSkeleton(acmeFacts, reg)
	allMembers := make(map[string]bool)
	for _, n := range acmeSkel.Nodes {
		for _, m := range n.Members {
			allMembers[m] = true
		}
	}
	for _, r := range acmeFacts.Resources {
		if r.AssetType == "cloudbuild.googleapis.com/BuildTrigger" ||
			r.AssetType == "k8s.io/Service" ||
			r.AssetType == "networking.k8s.io/Ingress" {
			if !allMembers[r.DisplayName] {
				t.Errorf("folded/cohort member %q (%s) missing from Component.Members", r.DisplayName, r.AssetType)
			}
		}
	}
}

func TestDetailViewConnectivityAndOffPageRefs(t *testing.T) {
	reg, err := icons.Load()
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{
		filepath.Join("..", "..", "testdata", "acme_cicd_v3.facts.json"),
		filepath.Join("..", "..", "testdata", "catalog", "cat3_ncc_hub_spoke_psc.facts.json"),
		filepath.Join("..", "..", "testdata", "catalog", "cat3_shared_vpc_hybrid_armor.facts.json"),
		filepath.Join("..", "..", "testdata", "catalog", "cat4_cdc_dataflow_bigquery_vertex.facts.json"),
		filepath.Join("..", "..", "testdata", "catalog", "cat5_transversal_iam_mesh_explosion.facts.json"),
	}
	for _, p := range paths {
		name := filepath.Base(p)
		t.Run(name, func(t *testing.T) {
			facts, err := extract.LoadFacts(p)
			if err != nil {
				t.Fatal(err)
			}
			skel := buildSkeleton(facts, reg)
			m := assembleManifest(skel, annotationResponse{}, reg, "2026-11-26T00:00:00Z")
			if err := manifest.ValidateWithFacts(m, facts, reg); err != nil {
				t.Fatalf("%s ValidateWithFacts failed: %v", name, err)
			}
			for _, v := range m.Views {
				if v.ID == manifest.SystemIndexViewID {
					continue
				}
				deg := make(map[string]int, len(v.Placements))
				for _, e := range v.Edges {
					deg[e.From]++
					deg[e.To]++
				}
				for _, pl := range v.Placements {
					if deg[pl.ComponentID] == 0 && len(pl.OffPageRefs) == 0 {
						t.Errorf("%s/%s: component %q has 0 incident edges AND 0 OffPageRefs", name, v.ID, pl.ComponentID)
					}
				}
			}
		})
	}
}

func TestScaleLimitError(t *testing.T) {
	reg, err := icons.Load()
	if err != nil {
		t.Fatal(err)
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

	client := &stubModelClient{}
	_, err = Synthesize(context.Background(), huge, client, reg)
	var scaleErr *ScaleLimitError
	if !errors.As(err, &scaleErr) {
		t.Fatalf("expected *ScaleLimitError from Synthesize, got %v", err)
	}
	if client.calls != 0 {
		t.Fatalf("client must not be called on ScaleLimitError, got %d calls", client.calls)
	}
	if _, err = Reconcile(context.Background(), huge, &manifest.Manifest{}, client, reg); !errors.As(err, &scaleErr) {
		t.Fatalf("expected *ScaleLimitError from Reconcile, got %v", err)
	}
}

func TestClassifyAssetSpecExtendedServices(t *testing.T) {
	reg, err := icons.Load()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		assetType string
		wantKind  manifest.ComponentKind
		wantRole  string
		wantLayer int
		wantIcon  string
	}{
		{"apigateway.googleapis.com/Gateway", manifest.KindIngress, "ingress", 1, "api_gateway"},
		{"apigee.googleapis.com/Organization", manifest.KindIngress, "ingress", 1, "apigee_api_platform"},
		{"clouddeploy.googleapis.com/DeliveryPipeline", manifest.KindPipeline, "cicd", 2, "cloud_deploy"},
		{"batch.googleapis.com/Job", manifest.KindWorkload, "compute", 2, "batch"},
		{"appengine.googleapis.com/Service", manifest.KindWorkload, "compute", 2, "app_engine"},
		{"managedkafka.googleapis.com/Cluster", manifest.KindMessaging, "async", 3, "managed_kafka"},
		{"dataproc.googleapis.com/Cluster", manifest.KindPipeline, "pipeline", 3, "dataproc"},
		{"dataform.googleapis.com/Repository", manifest.KindPipeline, "data", 3, "dataform"},
		{"datafusion.googleapis.com/Instance", manifest.KindPipeline, "pipeline", 3, "cloud_data_fusion"},
		{"looker.googleapis.com/Instance", manifest.KindWorkload, "data", 6, "looker"},
		{"dialogflow.googleapis.com/Agent", manifest.KindWorkload, "ai", 2, "dialogflow"},
		{"documentai.googleapis.com/Processor", manifest.KindPipeline, "ai", 3, "document_ai"},
	}
	for _, tc := range cases {
		got := classifyAssetSpec(extract.FactResource{AssetType: tc.assetType, DisplayName: "svc"}, reg)
		if got.componentKind != tc.wantKind || got.roleTag != tc.wantRole || got.baseLayer != tc.wantLayer || got.iconKey != tc.wantIcon {
			t.Errorf("%s: got %+v, want kind=%s role=%s layer=%d icon=%s",
				tc.assetType, got, tc.wantKind, tc.wantRole, tc.wantLayer, tc.wantIcon)
		}
		if !reg.Has(got.iconKey) {
			t.Errorf("%s: icon key %q not in registry", tc.assetType, got.iconKey)
		}
	}
}

func TestCat2DLQReplayOrderAndEventarcIngestBucket(t *testing.T) {
	reg, err := icons.Load()
	if err != nil {
		t.Fatal(err)
	}

	cat2DLQ, err := extract.LoadFacts(filepath.Join("..", "..", "testdata", "catalog", "cat2_pubsub_dlq_cloudrun.facts.json"))
	if err != nil {
		t.Fatal(err)
	}
	dlqSkel := buildSkeleton(cat2DLQ, reg)
	rankByTitle := make(map[string]int, len(dlqSkel.Nodes))
	for _, n := range dlqSkel.Nodes {
		rankByTitle[n.Title] = n.TopoRank
	}
	if rankByTitle["orders-events-dlq"] <= rankByTitle["orders-events-topic"] ||
		rankByTitle["orders-dlq-replay-fn"] <= rankByTitle["orders-events-dlq"] ||
		rankByTitle["orders-dlq-archive"] <= rankByTitle["orders-dlq-replay-fn"] {
		t.Fatalf("unexpected DLQ replay ranks: %+v", rankByTitle)
	}

	cat2Eventarc, err := extract.LoadFacts(filepath.Join("..", "..", "testdata", "catalog", "cat2_eventarc_workflows_retry.facts.json"))
	if err != nil {
		t.Fatal(err)
	}
	evSkel := buildSkeleton(cat2Eventarc, reg)
	evRanks := make(map[string]int, len(evSkel.Nodes))
	for _, n := range evSkel.Nodes {
		evRanks[n.Title] = n.TopoRank
	}
	chain := []string{"media-raw-uploads", "media-upload-trigger", "media-transcode-workflow", "media-retry-tasks", "media-transcode-worker"}
	for i := 0; i+1 < len(chain); i++ {
		if evRanks[chain[i]] >= evRanks[chain[i+1]] {
			t.Fatalf("expected %s (%d) < %s (%d)", chain[i], evRanks[chain[i]], chain[i+1], evRanks[chain[i+1]])
		}
	}
}

func TestContainerBuilderRetainsSlicesAfterManyAppends(t *testing.T) {
	cb := newContainerBuilder(&extract.FactsFile{}, map[string]extract.FactResource{})
	vpc := cb.addContainer(manifest.Container{
		ID:    "vpc_test",
		Kind:  manifest.ContainerVPC,
		Title: "VPC: test",
		BackedBy: []manifest.BackedResource{{
			URI:       "//compute.googleapis.com/projects/p1/global/networks/vpc1",
			AssetType: "compute.googleapis.com/Network",
			ProjectID: "p1",
		}},
	})
	for i := range 25 {
		id := "extra_cont_" + string(rune('a'+i))
		cb.addContainer(manifest.Container{
			ID:                id,
			Kind:              manifest.ContainerExposureZone,
			Title:             id,
			ParentContainerID: "vpc_test",
		})
	}
	appendUniqueStr(&vpc.FoldedEdgeIDs, "edge_vpc_peering_1")
	appendUniqueStr(&vpc.Badges, "Peering: peer-vpc")

	finalized := cb.finalizeContainers(nil)
	var found *manifest.Container
	for i := range finalized {
		if finalized[i].ID == "vpc_test" {
			found = &finalized[i]
		}
	}
	if found == nil || !slices.Contains(found.FoldedEdgeIDs, "edge_vpc_peering_1") || !slices.Contains(found.Badges, "Peering: peer-vpc") {
		t.Fatalf("VPC lost FoldedEdgeIDs or Badges after >20 appends: %+v", found)
	}
}
