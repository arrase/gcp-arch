package extract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEdgeIDDeterminism(t *testing.T) {
	got := EdgeID(EdgeCloudBuildStepRef, "//cloudbuild.googleapis.com/projects/p/locations/global/triggers/t1", "//storage.googleapis.com/tfstate", "gs://tfstate")
	sum := sha256.Sum256([]byte("CLOUDBUILD_STEP_REF://cloudbuild.googleapis.com/projects/p/locations/global/triggers/t1://storage.googleapis.com/tfstate:gs://tfstate"))
	want := hex.EncodeToString(sum[:])[:16]
	if got != want {
		t.Fatalf("EdgeID = %q, want %q", got, want)
	}
	if len(got) != 16 {
		t.Fatalf("len(EdgeID) = %d, want 16", len(got))
	}
	if got == EdgeID(EdgeConfigRef, "//cloudbuild.googleapis.com/projects/p/locations/global/triggers/t1", "//storage.googleapis.com/tfstate", "gs://tfstate") {
		t.Fatal("EdgeID collided across different kinds")
	}
}

func TestWriteAndLoadFactsDeterministicAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	path := filepath.Join(dir, "facts.json")

	orig := &FactsFile{
		ReadTime: "2026-11-23T02:00:00Z",
		Scopes:   []string{"projects/z-proj", "projects/a-proj"},
		Hierarchy: Hierarchy{
			Organizations: []OrgNode{
				{OrgID: "200", DisplayName: "Org B"},
				{OrgID: "100", DisplayName: "Org A"},
			},
			Folders: []FolderNode{
				{FolderID: "f2", Parent: "organizations/100"},
				{FolderID: "f1", Parent: "organizations/100"},
			},
			Projects: []ProjectNode{
				{ProjectID: "z-proj", ProjectNumber: "222"},
				{ProjectID: "a-proj", ProjectNumber: "111", IsPSATenant: false},
			},
		},
		Resources: []FactResource{
			{
				URI:                 "//storage.googleapis.com/z-bkt",
				AssetType:           "storage.googleapis.com/Bucket",
				ProjectID:           "z-proj",
				DisplayName:         "z-bkt",
				ProjectCapabilities: []string{"roles/viewer", "roles/editor"},
			},
			{
				URI:         "//github.com/acme-corp/platform-services",
				AssetType:   "external.github.com/Repository",
				ProjectID:   "external",
				DisplayName: "platform-services",
				IsExternal:  true,
			},
		},
		Edges: []FactEdge{
			{
				FromURI: "//storage.googleapis.com/z-bkt",
				ToURI:   "//github.com/acme-corp/platform-services",
				Kind:    EdgeConfigRef,
				Detail:  "ref-b",
			},
			{
				FromURI: "//github.com/acme-corp/platform-services",
				ToURI:   "//storage.googleapis.com/z-bkt",
				Kind:    EdgeCloudBuildStepRef,
				Detail:  "gs://z-bkt",
			},
		},
		SuppressedPlumbingCounts: map[string]int{
			"k8s.io/Pod":                12,
			"cloudbuild.googleapis.com": 819,
		},
		OutOfScopeTargets: []OutOfScopeHint{
			{
				TargetProjectID:  "target-b",
				SyntheticURI:     "//cloudresourcemanager.googleapis.com/projects/target-b",
				ReferencedByURIs: []string{"//b", "//a"},
				SourceKind:       "cloudbuild_deploy_flag",
			},
			{
				TargetProjectID:  "target-a",
				SyntheticURI:     "//cloudresourcemanager.googleapis.com/projects/target-a",
				ReferencedByURIs: []string{"//c"},
				SourceKind:       "config_ref_cross_project",
			},
		},
		CoverageLedger: []CoverageEntry{
			{Scope: "projects/z-proj", Source: "monitoring", State: CoverageOK},
			{Scope: "projects/a-proj", Source: "cai", State: CoverageIAMDenied, Detail: "403"},
		},
	}

	if err := WriteFacts(path, orig); err != nil {
		t.Fatalf("WriteFacts error: %v", err)
	}

	// Verify caller struct was NOT mutated.
	if orig.Scopes[0] != "projects/z-proj" || orig.Resources[0].URI != "//storage.googleapis.com/z-bkt" {
		t.Fatal("WriteFacts mutated caller slices")
	}

	// Verify 0600 file permissions and 0700 parent dir permissions.
	assertFactsPermissions(t, dir, path)

	loaded, err := LoadFacts(path)
	if err != nil {
		t.Fatalf("LoadFacts error: %v", err)
	}
	assertLoadedFactsCanonical(t, loaded)
	assertFactsByteDeterministic(t, dir, path, loaded)
}

func assertFactsPermissions(t *testing.T, dir, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat facts.json: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("facts.json perm = %o, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat parent dir: %v", err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("parent dir perm = %o, want 0700", di.Mode().Perm())
	}
}

func assertLoadedFactsCanonical(t *testing.T, loaded *FactsFile) {
	t.Helper()
	if loaded.SchemaVersion != FactsSchemaVersion {
		t.Fatalf("SchemaVersion = %q, want %q", loaded.SchemaVersion, FactsSchemaVersion)
	}
	if !reflect.DeepEqual(loaded.Scopes, []string{"projects/a-proj", "projects/z-proj"}) {
		t.Fatalf("Scopes = %v", loaded.Scopes)
	}
	if loaded.Hierarchy.Organizations[0].OrgID != "100" || loaded.Hierarchy.Folders[0].FolderID != "f1" || loaded.Hierarchy.Projects[0].ProjectID != "a-proj" {
		t.Fatalf("Hierarchy not sorted: %+v", loaded.Hierarchy)
	}
	if loaded.Resources[0].URI != "//github.com/acme-corp/platform-services" {
		t.Fatalf("Resources not sorted: %+v", loaded.Resources)
	}
	if !reflect.DeepEqual(loaded.Resources[1].ProjectCapabilities, []string{"roles/editor", "roles/viewer"}) {
		t.Fatalf("ProjectCapabilities not sorted: %v", loaded.Resources[1].ProjectCapabilities)
	}
	if loaded.Edges[0].FromURI != "//github.com/acme-corp/platform-services" {
		t.Fatalf("Edges not sorted: %+v", loaded.Edges)
	}
	wantEdgeID := EdgeID(EdgeCloudBuildStepRef, "//github.com/acme-corp/platform-services", "//storage.googleapis.com/z-bkt", "gs://z-bkt")
	if loaded.Edges[0].ID != wantEdgeID {
		t.Fatalf("auto-populated edge ID = %q, want %q", loaded.Edges[0].ID, wantEdgeID)
	}
	if loaded.OutOfScopeTargets[0].TargetProjectID != "target-a" || !reflect.DeepEqual(loaded.OutOfScopeTargets[1].ReferencedByURIs, []string{"//a", "//b"}) {
		t.Fatalf("OutOfScopeTargets not sorted: %+v", loaded.OutOfScopeTargets)
	}
	if loaded.CoverageLedger[0].Scope != "projects/a-proj" {
		t.Fatalf("CoverageLedger not sorted: %+v", loaded.CoverageLedger)
	}
}

func assertFactsByteDeterministic(t *testing.T, dir, path string, loaded *FactsFile) {
	t.Helper()
	// Re-writing loaded facts must yield byte-identical JSON.
	raw1, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	path2 := filepath.Join(dir, "facts2.json")
	if err := WriteFacts(path2, loaded); err != nil {
		t.Fatal(err)
	}
	raw2, err := os.ReadFile(path2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw1, raw2) {
		t.Fatalf("WriteFacts not byte-deterministic:\n%s\nvs\n%s", raw1, raw2)
	}
}

func TestWriteAndLoadFactsErrorsAndEmptyCollections(t *testing.T) {
	dir := t.TempDir()

	// Empty FactsFile serializes empty JSON arrays/objects (not null).
	emptyPath := filepath.Join(dir, "empty.json")
	if err := WriteFacts(emptyPath, &FactsFile{}); err != nil {
		t.Fatalf("WriteFacts empty error: %v", err)
	}
	raw, err := os.ReadFile(emptyPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "null") {
		t.Fatalf("expected non-null empty JSON collections, got:\n%s", raw)
	}

	// Nil pointer error.
	if err := WriteFacts(emptyPath, nil); err == nil {
		t.Fatal("expected error for nil FactsFile")
	}
	// Wrong schema version on write.
	if err := WriteFacts(emptyPath, &FactsFile{SchemaVersion: "v2"}); err == nil {
		t.Fatal("expected error for unsupported schema_version on WriteFacts")
	}

	// LoadFacts errors: missing file, invalid JSON, wrong schema_version.
	if _, err := LoadFacts(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("expected error loading missing file")
	}
	badJSON := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badJSON, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFacts(badJSON); err == nil {
		t.Fatal("expected error decoding invalid JSON")
	}
	wrongVer := filepath.Join(dir, "v2.json")
	if err := os.WriteFile(wrongVer, []byte(`{"schema_version":"v2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFacts(wrongVer); err == nil {
		t.Fatal("expected error for v2 schema_version in LoadFacts")
	}
}

func TestBuildProjectIndex(t *testing.T) {
	projects := []ProjectNode{
		{ProjectID: "app-prod", ProjectNumber: "111222333"},
		{ProjectID: "no-num"},
	}
	aliases := map[string]string{
		"111222333": "override-prod",
		"999888777": "shared-vpc-host",
	}
	got := BuildProjectIndex(projects, aliases)
	want := map[string]string{
		"111222333": "override-prod",
		"999888777": "shared-vpc-host",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildProjectIndex = %v, want %v", got, want)
	}
}

func TestCanonicalizeAIP122(t *testing.T) {
	projIdx := map[string]string{
		"111222333": "acme-cicd",
		"999888777": "shared-vpc-host",
	}

	tests := []struct {
		name string
		raw  string
		want string
	}{
		// Compute /locations/ -> /global/, /regions/, /zones/
		{
			name: "compute aip122 locations global network",
			raw:  "//compute.googleapis.com/projects/111222333/locations/global/networks/cicd",
			want: "//compute.googleapis.com/projects/acme-cicd/global/networks/cicd",
		},
		{
			name: "compute relative locations global network",
			raw:  "projects/111222333/locations/global/networks/cicd",
			want: "//compute.googleapis.com/projects/acme-cicd/global/networks/cicd",
		},
		{
			name: "compute aip122 locations region subnetwork",
			raw:  "//compute.googleapis.com/projects/111222333/locations/europe-west1/subnetworks/cicd-sub",
			want: "//compute.googleapis.com/projects/acme-cicd/regions/europe-west1/subnetworks/cicd-sub",
		},
		{
			name: "compute relative locations region subnetwork",
			raw:  "projects/999888777/locations/europe-west1/subnetworks/shared-sub",
			want: "//compute.googleapis.com/projects/shared-vpc-host/regions/europe-west1/subnetworks/shared-sub",
		},
		{
			name: "compute aip122 locations zone instance",
			raw:  "//compute.googleapis.com/projects/111222333/locations/europe-west1-b/instances/vm-1",
			want: "//compute.googleapis.com/projects/acme-cicd/zones/europe-west1-b/instances/vm-1",
		},
		{
			name: "compute relative locations zone neg",
			raw:  "projects/111222333/locations/europe-west1-b/networkEndpointGroups/k8s1-neg",
			want: "//compute.googleapis.com/projects/acme-cicd/zones/europe-west1-b/networkEndpointGroups/k8s1-neg",
		},
		{
			name: "compute http selflink locations global network",
			raw:  "https://www.googleapis.com/compute/v1/projects/111222333/locations/global/networks/cicd",
			want: "//compute.googleapis.com/projects/acme-cicd/global/networks/cicd",
		},
		{
			name: "compute host without double slash locations region router",
			raw:  "compute.googleapis.com/v1/projects/111222333/locations/europe-west1/routers/nat-rtr",
			want: "//compute.googleapis.com/projects/acme-cicd/regions/europe-west1/routers/nat-rtr",
		},
		{
			name: "compute explicit service locations global sslPolicies",
			raw:  "//compute.googleapis.com/projects/111222333/locations/global/sslPolicies/tls-12",
			want: "//compute.googleapis.com/projects/acme-cicd/global/sslPolicies/tls-12",
		},
		{
			name: "compute explicit service locations region resourcePolicies",
			raw:  "//compute.googleapis.com/projects/111222333/locations/europe-west1/resourcePolicies/snap-sched",
			want: "//compute.googleapis.com/projects/acme-cicd/regions/europe-west1/resourcePolicies/snap-sched",
		},
		// Non-Compute /locations/ paths MUST NOT be rewritten
		{
			name: "cloudbuild preserves locations global",
			raw:  "//cloudbuild.googleapis.com/projects/111222333/locations/global/triggers/deploy-api",
			want: "//cloudbuild.googleapis.com/projects/acme-cicd/locations/global/triggers/deploy-api",
		},
		{
			name: "cloud run preserves locations region",
			raw:  "//run.googleapis.com/projects/111222333/locations/europe-west1/services/webhook",
			want: "//run.googleapis.com/projects/acme-cicd/locations/europe-west1/services/webhook",
		},
		{
			name: "artifact registry preserves locations region",
			raw:  "//artifactregistry.googleapis.com/projects/111222333/locations/europe-west1/repositories/acme-apps",
			want: "//artifactregistry.googleapis.com/projects/acme-cicd/locations/europe-west1/repositories/acme-apps",
		},
		{
			name: "redis preserves locations region instances",
			raw:  "//redis.googleapis.com/projects/111222333/locations/europe-west1/instances/cache-1",
			want: "//redis.googleapis.com/projects/acme-cicd/locations/europe-west1/instances/cache-1",
		},
		{
			name: "ambiguous relative locations instances stays unguessed",
			raw:  "projects/111222333/locations/us-central1/instances/unknown-svc",
			want: "projects/acme-cicd/locations/us-central1/instances/unknown-svc",
		},
		// GKE container.googleapis.com /zones/<z>/clusters/<c> -> /locations/<z>/clusters/<c>
		{
			name: "gke aip122 zones cluster normalizes to locations",
			raw:  "//container.googleapis.com/projects/111222333/zones/europe-west1-b/clusters/cicd",
			want: "//container.googleapis.com/projects/acme-cicd/locations/europe-west1-b/clusters/cicd",
		},
		{
			name: "gke aip122 zones cluster k8s workload normalizes to locations",
			raw:  "//container.googleapis.com/projects/111222333/zones/europe-west1-b/clusters/cicd/k8s/namespaces/atlantis/apps/deployments/atlantis",
			want: "//container.googleapis.com/projects/acme-cicd/locations/europe-west1-b/clusters/cicd/k8s/namespaces/atlantis/apps/deployments/atlantis",
		},
		{
			name: "gke http url zones cluster normalizes to locations",
			raw:  "https://container.googleapis.com/v1/projects/111222333/zones/europe-west1-b/clusters/cicd",
			want: "//container.googleapis.com/projects/acme-cicd/locations/europe-west1-b/clusters/cicd",
		},
		{
			name: "gke relative zones cluster normalizes to locations",
			raw:  "projects/111222333/zones/europe-west1-b/clusters/cicd",
			want: "//container.googleapis.com/projects/acme-cicd/locations/europe-west1-b/clusters/cicd",
		},
		{
			name: "gke locations regional cluster preserved",
			raw:  "//container.googleapis.com/projects/111222333/locations/europe-west1/clusters/cicd",
			want: "//container.googleapis.com/projects/acme-cicd/locations/europe-west1/clusters/cicd",
		},
		// General AIP-122 / URL / Storage / Relative coverage
		{name: "empty", raw: "   ", want: ""},
		{name: "only slashes", raw: "///", want: ""},
		{name: "gs empty bucket", raw: "gs://", want: ""},
		{name: "gs uri", raw: "gs://acme-tfstate/env/prod.tfstate", want: "//storage.googleapis.com/acme-tfstate"},
		{name: "storage www url", raw: "https://www.googleapis.com/storage/v1/b/acme-tfstate/o/state", want: "//storage.googleapis.com/acme-tfstate"},
		{name: "storage direct v1 url", raw: "https://storage.googleapis.com/storage/v1/b/acme-tfstate", want: "//storage.googleapis.com/acme-tfstate"},
		{name: "storage direct path url", raw: "https://storage.googleapis.com/acme-tfstate/dir/a.json", want: "//storage.googleapis.com/acme-tfstate"},
		{name: "storage projects bucket http", raw: "https://storage.googleapis.com/projects/_/buckets/acme-tfstate", want: "//storage.googleapis.com/acme-tfstate"},
		{name: "storage aip122 projects bucket", raw: "//storage.googleapis.com/projects/_/buckets/acme-tfstate", want: "//storage.googleapis.com/acme-tfstate"},
		{name: "storage host no slash projects bucket", raw: "storage.googleapis.com/projects/111222333/buckets/acme-tfstate", want: "//storage.googleapis.com/acme-tfstate"},
		{name: "storage relative projects bucket", raw: "projects/111222333/buckets/acme-tfstate", want: "//storage.googleapis.com/acme-tfstate"},
		{name: "sqladmin www url", raw: "https://www.googleapis.com/sql/v1beta4/projects/111222333/instances/sonarqube?alt=json#frag", want: "//sqladmin.googleapis.com/projects/acme-cicd/instances/sonarqube"},
		{name: "regional aiplatform url", raw: "https://us-central1-aiplatform.googleapis.com/v1beta1/projects/111222333/locations/us-central1/endpoints/42", want: "//aiplatform.googleapis.com/projects/acme-cicd/locations/us-central1/endpoints/42"},
		{name: "host only aip122", raw: "//storage.googleapis.com", want: "//storage.googleapis.com"},
		{name: "non googleapis url strips userinfo and query", raw: "https://user:pass@github.com/acme-corp/platform-services?token=secret#top", want: "https://github.com/acme-corp/platform-services"},
		{name: "relative project", raw: "projects/111222333", want: "//cloudresourcemanager.googleapis.com/projects/acme-cicd"},
		{name: "relative appengine", raw: "apps/111222333/services/default", want: "//appengine.googleapis.com/apps/acme-cicd/services/default"},
		{name: "relative compute global", raw: "projects/111222333/global/networks/cicd", want: "//compute.googleapis.com/projects/acme-cicd/global/networks/cicd"},
		{name: "relative compute region", raw: "projects/111222333/regions/europe-west1/subnetworks/cicd", want: "//compute.googleapis.com/projects/acme-cicd/regions/europe-west1/subnetworks/cicd"},
		{name: "relative compute zone", raw: "projects/111222333/zones/europe-west1-b/instances/vm-1", want: "//compute.googleapis.com/projects/acme-cicd/zones/europe-west1-b/instances/vm-1"},
		{name: "relative pubsub", raw: "projects/111222333/topics/build-events", want: "//pubsub.googleapis.com/projects/acme-cicd/topics/build-events"},
		{name: "relative bigquery", raw: "projects/111222333/datasets/metrics", want: "//bigquery.googleapis.com/projects/acme-cicd/datasets/metrics"},
		{name: "relative secretmanager", raw: "projects/111222333/secrets/db-pass", want: "//secretmanager.googleapis.com/projects/acme-cicd/secrets/db-pass"},
		{name: "relative iam sa", raw: "projects/111222333/serviceAccounts/atlantis@acme-cicd.iam.gserviceaccount.com", want: "//iam.googleapis.com/projects/acme-cicd/serviceAccounts/atlantis@acme-cicd.iam.gserviceaccount.com"},
		{name: "relative dns", raw: "projects/111222333/managedZones/corp", want: "//dns.googleapis.com/projects/acme-cicd/managedZones/corp"},
		{name: "relative firestore", raw: "projects/111222333/databases/(default)", want: "//firestore.googleapis.com/projects/acme-cicd/databases/(default)"},
		{name: "relative spanner", raw: "projects/111222333/instances/sp1/databases/db1", want: "//spanner.googleapis.com/projects/acme-cicd/instances/sp1/databases/db1"},
		{name: "relative bigtable", raw: "projects/111222333/instances/bt1/tables/t1", want: "//bigtableadmin.googleapis.com/projects/acme-cicd/instances/bt1/tables/t1"},
		{name: "relative alloydb", raw: "projects/111222333/locations/europe-west1/clusters/c1/instances/primary", want: "//alloydb.googleapis.com/projects/acme-cicd/locations/europe-west1/clusters/c1/instances/primary"},
		{name: "relative kms", raw: "projects/111222333/locations/europe-west1/keyRings/kr/cryptoKeys/k1", want: "//cloudkms.googleapis.com/projects/acme-cicd/locations/europe-west1/keyRings/kr/cryptoKeys/k1"},
		{name: "relative vpcaccess", raw: "projects/111222333/locations/europe-west1/connectors/c1", want: "//vpcaccess.googleapis.com/projects/acme-cicd/locations/europe-west1/connectors/c1"},
		{name: "relative eventarc", raw: "projects/111222333/locations/europe-west1/triggers/tr1", want: "//eventarc.googleapis.com/projects/acme-cicd/locations/europe-west1/triggers/tr1"},
		{name: "relative cloudtasks", raw: "projects/111222333/locations/europe-west1/queues/q1", want: "//cloudtasks.googleapis.com/projects/acme-cicd/locations/europe-west1/queues/q1"},
		{name: "relative workflows", raw: "projects/111222333/locations/europe-west1/workflows/w1", want: "//workflows.googleapis.com/projects/acme-cicd/locations/europe-west1/workflows/w1"},
		{name: "relative cloudfunctions", raw: "projects/111222333/locations/europe-west1/functions/fn1", want: "//cloudfunctions.googleapis.com/projects/acme-cicd/locations/europe-west1/functions/fn1"},
		{name: "relative composer", raw: "projects/111222333/locations/europe-west1/environments/env1", want: "//composer.googleapis.com/projects/acme-cicd/locations/europe-west1/environments/env1"},
		{name: "relative datastream", raw: "projects/111222333/locations/europe-west1/streams/st1", want: "//datastream.googleapis.com/projects/acme-cicd/locations/europe-west1/streams/st1"},
		{name: "relative ncc", raw: "projects/111222333/locations/global/hubs/h1", want: "//networkconnectivity.googleapis.com/projects/acme-cicd/locations/global/hubs/h1"},
		{name: "relative netapp", raw: "projects/111222333/locations/europe-west1/volumes/v1", want: "//netapp.googleapis.com/projects/acme-cicd/locations/europe-west1/volumes/v1"},
		{name: "relative discovery", raw: "projects/111222333/locations/global/dataStores/ds1", want: "//discoveryengine.googleapis.com/projects/acme-cicd/locations/global/dataStores/ds1"},
		{name: "relative dialogflow", raw: "projects/111222333/locations/europe-west1/agents/a1", want: "//dialogflow.googleapis.com/projects/acme-cicd/locations/europe-west1/agents/a1"},
		{name: "relative documentai", raw: "projects/111222333/locations/eu/processors/p1", want: "//documentai.googleapis.com/projects/acme-cicd/locations/eu/processors/p1"},
		{name: "relative certmanager", raw: "projects/111222333/locations/global/certificates/cert1", want: "//certificatemanager.googleapis.com/projects/acme-cicd/locations/global/certificates/cert1"},
		{name: "relative apigateway", raw: "projects/111222333/locations/europe-west1/gateways/gw1", want: "//apigateway.googleapis.com/projects/acme-cicd/locations/europe-west1/gateways/gw1"},
		{name: "relative analyticshub", raw: "projects/111222333/locations/eu/dataExchanges/dx1", want: "//analyticshub.googleapis.com/projects/acme-cicd/locations/eu/dataExchanges/dx1"},
		{name: "relative bqdt", raw: "projects/111222333/locations/eu/transferConfigs/tc1", want: "//bigquerydatatransfer.googleapis.com/projects/acme-cicd/locations/eu/transferConfigs/tc1"},
		{name: "relative dataplex", raw: "projects/111222333/locations/europe-west1/lakes/l1", want: "//dataplex.googleapis.com/projects/acme-cicd/locations/europe-west1/lakes/l1"},
		{name: "relative bqconn", raw: "projects/111222333/locations/europe-west1/connections/c1", want: "//bigqueryconnection.googleapis.com/projects/acme-cicd/locations/europe-west1/connections/c1"},
		{name: "relative cloud run", raw: "projects/111222333/locations/europe-west1/services/webhook", want: "//run.googleapis.com/projects/acme-cicd/locations/europe-west1/services/webhook"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanonicalizeAIP122(tc.raw, projIdx); got != tc.want {
				t.Fatalf("CanonicalizeAIP122(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestCanonicalizeAIP122WithService(t *testing.T) {
	projIdx := map[string]string{"111": "acme-cicd"}

	cases := []struct {
		raw, svc, want string
	}{
		{
			raw:  "projects/111/locations/us-central1/instances/cache-1",
			svc:  "redis.googleapis.com",
			want: "//redis.googleapis.com/projects/acme-cicd/locations/us-central1/instances/cache-1",
		},
		{
			raw:  "projects/111/locations/europe-west1-b/instances/vm-1",
			svc:  "compute.googleapis.com",
			want: "//compute.googleapis.com/projects/acme-cicd/zones/europe-west1-b/instances/vm-1",
		},
		{
			raw:  "projects/111/zones/europe-west1-b/clusters/cicd",
			svc:  "container.googleapis.com",
			want: "//container.googleapis.com/projects/acme-cicd/locations/europe-west1-b/clusters/cicd",
		},
		{
			raw:  "projects/111/locations/global/triggers/t1",
			svc:  "cloudbuild.googleapis.com",
			want: "//cloudbuild.googleapis.com/projects/acme-cicd/locations/global/triggers/t1",
		},
	}

	for _, tc := range cases {
		if got := CanonicalizeAIP122WithService(tc.raw, tc.svc, projIdx); got != tc.want {
			t.Errorf("CanonicalizeAIP122WithService(%q, %q) = %q, want %q", tc.raw, tc.svc, got, tc.want)
		}
	}
}
