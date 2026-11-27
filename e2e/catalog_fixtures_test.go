package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/manifest"
)

type catalogManifest struct {
	SchemaVersion string                `json:"schema_version"`
	GeneratedBy   string                `json:"generated_by"`
	Fixtures      []catalogManifestCase `json:"fixtures"`
}

type catalogManifestCase struct {
	Name                      string   `json:"name"`
	File                      string   `json:"file"`
	Category                  string   `json:"category"`
	Description               string   `json:"description"`
	TotalResources            int      `json:"total_resources"`
	ExpectedPrimaryURIs       []string `json:"expected_primary_uris"`
	ExpectedTransversalURIs   []string `json:"expected_transversal_uris"`
	TotalEdges                int      `json:"total_edges"`
	ExpectedFunctionalEdgeIDs []string `json:"expected_functional_edge_ids"`
}

func TestCatalogFixturesStage1FidelityAndManifest(t *testing.T) {
	catalogDir := filepath.Join(repoRoot(t), "testdata", "catalog")
	manifestBytes, err := os.ReadFile(filepath.Join(catalogDir, "manifest.json"))
	if err != nil {
		t.Fatalf("read catalog manifest.json: %v", err)
	}
	var manifest catalogManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("decode catalog manifest.json: %v", err)
	}
	if len(manifest.Fixtures) != 11 {
		t.Fatalf("expected 11 fixtures in manifest.json (10 catalog + 1 baseline), got %d", len(manifest.Fixtures))
	}

	diskFiles, err := filepath.Glob(filepath.Join(catalogDir, "*.facts.json"))
	if err != nil {
		t.Fatalf("glob catalog facts: %v", err)
	}
	if len(diskFiles) != 10 {
		t.Fatalf("expected 10 *.facts.json files in %s, got %d", catalogDir, len(diskFiles))
	}

	manifestByFile := make(map[string]catalogManifestCase, len(manifest.Fixtures))
	for _, mc := range manifest.Fixtures {
		manifestByFile[filepath.Clean(filepath.Join(catalogDir, mc.File))] = mc
	}

	allFiles := append([]string{filepath.Join(repoRoot(t), "testdata", "acme_cicd_v3.facts.json")}, diskFiles...)
	for _, path := range allFiles {
		mc, ok := manifestByFile[filepath.Clean(path)]
		if !ok {
			t.Fatalf("fixture %s missing from testdata/catalog/manifest.json", path)
		}
		t.Run(mc.Name, func(t *testing.T) {
			verifyFactsFixture(t, path, mc)
		})
	}
}

func verifyFactsFixture(t *testing.T, path string, mc catalogManifestCase) {
	t.Helper()
	facts, err := extract.LoadFacts(path)
	if err != nil {
		t.Fatalf("extract.LoadFacts(%s) failed: %v", path, err)
	}
	if facts.SchemaVersion != extract.FactsSchemaVersion {
		t.Fatalf("schema_version=%q, want %q", facts.SchemaVersion, extract.FactsSchemaVersion)
	}
	if len(facts.Resources) == 0 || len(facts.Resources) > 500 {
		t.Fatalf("resource count %d outside [1, 500]", len(facts.Resources))
	}
	if facts.SuppressedPlumbingCounts == nil {
		t.Fatal("suppressed_plumbing_counts must not be nil")
	}

	verifyWriteFactsRoundTrip(t, path, mc, facts)
	resByURI, gotPrimary, gotTransversal := verifyResourcesAndParentChains(t, mc, facts)
	verifyEdgesAndManifest(t, mc, facts, resByURI, gotPrimary, gotTransversal)
	verifyCategoryRequirements(t, mc.Category, facts)
}

func verifyWriteFactsRoundTrip(t *testing.T, path string, mc catalogManifestCase, facts *extract.FactsFile) {
	t.Helper()
	if mc.Category == "baseline" {
		return
	}
	rawOrig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tmpCopy := filepath.Join(t.TempDir(), "roundtrip.facts.json")
	if err := extract.WriteFacts(tmpCopy, facts); err != nil {
		t.Fatalf("WriteFacts round-trip failed: %v", err)
	}
	rawRT, err := os.ReadFile(tmpCopy)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rawOrig, rawRT) {
		t.Fatalf("fixture %s is not WriteFacts byte-deterministic", mc.File)
	}
}

func verifyResourcesAndParentChains(
	t *testing.T,
	mc catalogManifestCase,
	facts *extract.FactsFile,
) (map[string]extract.FactResource, []string, []string) {
	t.Helper()
	resByURI := make(map[string]extract.FactResource, len(facts.Resources))
	var gotPrimary, gotTransversal []string
	for _, r := range facts.Resources {
		if r.URI == "" || !strings.HasPrefix(r.URI, "//") {
			t.Fatalf("invalid non-canonical URI %q", r.URI)
		}
		if _, dup := resByURI[r.URI]; dup {
			t.Fatalf("duplicate URI %q", r.URI)
		}
		resByURI[r.URI] = r

		// Baseline acme_cicd_v3 carries 2 out-of-scope Project stubs with is_external: true.
		if extract.IsPlumbingAssetType(r.AssetType) && !(mc.Category == "baseline" && r.IsExternal && r.AssetType == "cloudresourcemanager.googleapis.com/Project") {
			t.Fatalf("plumbing asset type %q leaked into active resources (%s)", r.AssetType, r.URI)
		}
		if manifest.TransversalAccountingAssetTypes[r.AssetType] {
			gotTransversal = append(gotTransversal, r.URI)
		} else {
			gotPrimary = append(gotPrimary, r.URI)
		}
	}

	for _, r := range facts.Resources {
		verifyParentChain(t, r, resByURI)
	}
	return resByURI, gotPrimary, gotTransversal
}

func verifyParentChain(t *testing.T, r extract.FactResource, resByURI map[string]extract.FactResource) {
	t.Helper()
	if r.ParentURI == "" {
		return
	}
	visited := map[string]bool{r.URI: true}
	cur := r.ParentURI
	for cur != "" {
		if visited[cur] {
			t.Fatalf("cycle in parent_uri chain starting at %s", r.URI)
		}
		visited[cur] = true
		pRes, exists := resByURI[cur]
		if !exists {
			t.Fatalf("resource %s references missing parent_uri %q", r.URI, cur)
		}
		cur = pRes.ParentURI
	}
}

func verifyEdgesAndManifest(
	t *testing.T,
	mc catalogManifestCase,
	facts *extract.FactsFile,
	resByURI map[string]extract.FactResource,
	gotPrimary, gotTransversal []string,
) {
	t.Helper()
	edgeSeen := make(map[string]bool, len(facts.Edges))
	var gotFuncEdges []string
	for _, e := range facts.Edges {
		if e.FromURI == e.ToURI {
			t.Fatalf("self-loop edge on %s", e.FromURI)
		}
		if _, ok := resByURI[e.FromURI]; !ok {
			t.Fatalf("edge %s has dangling from_uri %q", e.ID, e.FromURI)
		}
		if _, ok := resByURI[e.ToURI]; !ok {
			t.Fatalf("edge %s has dangling to_uri %q", e.ID, e.ToURI)
		}
		wantID := extract.EdgeID(e.Kind, e.FromURI, e.ToURI, e.Detail)
		if e.ID != wantID {
			t.Fatalf("edge ID %q != EdgeID(%q, %q, %q, %q) = %q", e.ID, e.Kind, e.FromURI, e.ToURI, e.Detail, wantID)
		}
		if edgeSeen[e.ID] {
			t.Fatalf("duplicate edge ID %q", e.ID)
		}
		edgeSeen[e.ID] = true

		if manifest.IsManifestFunctionalEdge(e, resByURI) {
			gotFuncEdges = append(gotFuncEdges, e.ID)
		}
	}

	slices.Sort(gotPrimary)
	slices.Sort(gotTransversal)
	slices.Sort(gotFuncEdges)

	if len(facts.Resources) != mc.TotalResources {
		t.Fatalf("total_resources=%d, manifest wants %d", len(facts.Resources), mc.TotalResources)
	}
	if len(facts.Edges) != mc.TotalEdges {
		t.Fatalf("total_edges=%d, manifest wants %d", len(facts.Edges), mc.TotalEdges)
	}
	if !slices.Equal(gotPrimary, mc.ExpectedPrimaryURIs) {
		t.Fatalf("primary URIs mismatch vs manifest.json (got %d, want %d)", len(gotPrimary), len(mc.ExpectedPrimaryURIs))
	}
	if !slices.Equal(gotTransversal, mc.ExpectedTransversalURIs) {
		t.Fatalf("transversal URIs mismatch vs manifest.json (got %d, want %d)", len(gotTransversal), len(mc.ExpectedTransversalURIs))
	}
	if !slices.Equal(gotFuncEdges, mc.ExpectedFunctionalEdgeIDs) {
		t.Fatalf("functional edge IDs mismatch vs manifest.json (got %d, want %d)", len(gotFuncEdges), len(mc.ExpectedFunctionalEdgeIDs))
	}
}

func verifyCategoryRequirements(t *testing.T, category string, f *extract.FactsFile) {
	t.Helper()
	switch category {
	case "cat1":
		verifyCat1Requirements(t, f)
	case "cat2":
		verifyCat2Requirements(t, f)
	case "cat3":
		verifyCat3Requirements(t, f)
	case "cat4":
		verifyCat4Requirements(t, f)
	case "cat5":
		verifyCat5Requirements(t, f)
	}
}

func verifyCat1Requirements(t *testing.T, f *extract.FactsFile) {
	t.Helper()
	hasLB := hasAnyAsset(f, "compute.googleapis.com/GlobalForwardingRule", "compute.googleapis.com/ForwardingRule")
	hasDB := hasAnyAsset(f, "sqladmin.googleapis.com/Instance", "alloydb.googleapis.com/Cluster")
	if !hasLB || !hasDB {
		t.Fatalf("cat1 fixture missing LB (%v) or SQL/AlloyDB (%v)", hasLB, hasDB)
	}
}

func verifyCat2Requirements(t *testing.T, f *extract.FactsFile) {
	t.Helper()
	for _, e := range f.Edges {
		if e.Kind == extract.EdgeConfigRef && e.Detail == "dead_letter" {
			return
		}
	}
	t.Fatal("cat2 fixture missing dead_letter edge")
}

func verifyCat3Requirements(t *testing.T, f *extract.FactsFile) {
	t.Helper()
	if len(f.Hierarchy.Projects) < 3 {
		t.Fatalf("cat3 fixture needs >= 3 projects, got %d", len(f.Hierarchy.Projects))
	}
	hasHybridOrHub := hasAnyAsset(f, "compute.googleapis.com/VpnGateway", "compute.googleapis.com/InterconnectAttachment", "networkconnectivity.googleapis.com/Hub", "compute.googleapis.com/ServiceAttachment")
	if !hasHybridOrHub {
		t.Fatal("cat3 fixture missing hybrid/NCC/PSC network resource")
	}
	if f.SuppressedPlumbingCounts["compute.googleapis.com/SecurityPolicy"] == 0 || f.SuppressedPlumbingCounts["compute.googleapis.com/Firewall"] == 0 {
		t.Fatal("cat3 fixture missing Cloud Armor/Firewall plumbing counts")
	}
}

func verifyCat4Requirements(t *testing.T, f *extract.FactsFile) {
	t.Helper()
	hasDataAI := hasAnyAsset(f, "dataflow.googleapis.com/Job", "datastream.googleapis.com/Stream", "aiplatform.googleapis.com/Endpoint", "aiplatform.googleapis.com/ReasoningEngine")
	hasBQ := hasAnyAsset(f, "bigquery.googleapis.com/Dataset")
	if !hasDataAI || !hasBQ {
		t.Fatalf("cat4 fixture missing Data/AI (%v) or BigQuery (%v)", hasDataAI, hasBQ)
	}
}

func verifyCat5Requirements(t *testing.T, f *extract.FactsFile) {
	t.Helper()
	serviceCount, longNameCount := 0, 0
	for _, r := range f.Resources {
		switch r.AssetType {
		case "run.googleapis.com/Service", "cloudfunctions.googleapis.com/CloudFunction",
			"apps.k8s.io/Deployment", "batch.k8s.io/CronJob",
			"workflows.googleapis.com/Workflow", "dataflow.googleapis.com/Job":
			serviceCount++
			if len(r.DisplayName) >= 48 && len(r.DisplayName) <= 63 {
				longNameCount++
			}
		}
	}
	if serviceCount <= 20 {
		t.Fatalf("cat5 fixture must have > 20 primary services, got %d", serviceCount)
	}
	if longNameCount < 20 {
		t.Fatalf("cat5 fixture must have >= 20 ultra-long (48-63 char) service names, got %d", longNameCount)
	}
	if len(f.Edges) < 150 {
		t.Fatalf("cat5 fixture must have dense edges (>= 150), got %d", len(f.Edges))
	}
}

func hasAnyAsset(f *extract.FactsFile, types ...string) bool {
	want := make(map[string]bool, len(types))
	for _, t := range types {
		want[t] = true
	}
	for _, r := range f.Resources {
		if want[r.AssetType] {
			return true
		}
	}
	return false
}
