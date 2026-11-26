package extract

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/asset/apiv1/assetpb"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	bigquery "google.golang.org/api/bigquery/v2"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPSATenantRegexAndServiceNetworkingFallback(t *testing.T) {
	for _, nonTenant := range []string{"prod-tp", "partner-tp", "pipeline-tp", "p12345-tp", "p1234567890ghijkl-tp"} {
		if IsPSATenantProject(nonTenant) {
			t.Errorf("IsPSATenantProject(%q) = true, want false", nonTenant)
		}
	}
	for _, tenant := range []string{"p1234567890abcdef-tp", "P001122334455-TP"} {
		if !IsPSATenantProject(tenant) {
			t.Errorf("IsPSATenantProject(%q) = false, want true", tenant)
		}
	}

	assetHandler := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		if req.ContentType != assetpb.ContentType_RESOURCE {
			return &assetpb.ListAssetsResponse{ReadTime: timestamppb.Now()}, nil
		}
		return &assetpb.ListAssetsResponse{
			ReadTime: timestamppb.Now(),
			Assets: []*assetpb.Asset{
				{Name: "//cloudresourcemanager.googleapis.com/projects/101", AssetType: "cloudresourcemanager.googleapis.com/Project", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "app-p", "projectNumber": "101"})}},
				{
					Name:      "//compute.googleapis.com/projects/app-p/global/networks/main",
					AssetType: "compute.googleapis.com/Network",
					Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"peerings": []any{
							// Authoritative servicenetworking peering onto a non-regex tenant project.
							map[string]any{
								"name":    "servicenetworking-googleapis-com",
								"network": "https://www.googleapis.com/compute/v1/projects/custom-sql-tenant/global/networks/servicenetworking",
								"state":   "ACTIVE",
							},
							// Regular customer project whose ID happens to end in -tp ("prod-tp").
							map[string]any{
								"name":    "peer-to-prod",
								"network": "https://www.googleapis.com/compute/v1/projects/prod-tp/global/networks/hub",
								"state":   "ACTIVE",
							},
						},
					})},
				},
			},
		}, nil
	}

	clients, _ := newTestClients(t, assetHandler, nil, nil, nil)
	facts, err := Extract(context.Background(), clients, ExtractOptions{Scopes: []string{"projects/app-p"}})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if findResource(facts.Resources, "//cloudresourcemanager.googleapis.com/projects/custom-sql-tenant") != nil {
		t.Fatalf("servicenetworking tenant custom-sql-tenant must not be emitted as an external project stub")
	}
	if findResource(facts.Resources, "//cloudresourcemanager.googleapis.com/projects/prod-tp") == nil {
		t.Fatalf("customer project prod-tp must be emitted as an out-of-scope peer project stub")
	}
}

func TestSharedServiceAccountAmbiguityAndEnricherDefaultSARetention(t *testing.T) {
	const (
		sharedEmail = "shared@p1.iam.gserviceaccount.com"
		sharedURI   = "//iam.googleapis.com/projects/p1/serviceAccounts/" + sharedEmail
		defEmail    = "202-compute@developer.gserviceaccount.com"
		defURI      = "//iam.googleapis.com/projects/p1/serviceAccounts/" + defEmail
		bktURI      = "//storage.googleapis.com/p1-data"
		dsURI       = "//bigquery.googleapis.com/projects/p1/datasets/analytics"
	)

	assetHandler := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		if req.ContentType != assetpb.ContentType_RESOURCE {
			return &assetpb.ListAssetsResponse{ReadTime: timestamppb.Now()}, nil
		}
		return &assetpb.ListAssetsResponse{
			ReadTime: timestamppb.Now(),
			Assets: []*assetpb.Asset{
				{Name: "//cloudresourcemanager.googleapis.com/projects/202", AssetType: "cloudresourcemanager.googleapis.com/Project", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "p1", "projectNumber": "202"})}},
				{Name: sharedURI, AssetType: "iam.googleapis.com/ServiceAccount", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"email": sharedEmail, "uniqueId": "777"})}},
				{Name: defURI, AssetType: "iam.googleapis.com/ServiceAccount", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"email": defEmail, "uniqueId": "888"})}},
				{Name: "//run.googleapis.com/projects/p1/locations/us-central1/services/svc-a", AssetType: "run.googleapis.com/Service", Resource: &assetpb.Resource{Location: "us-central1", Data: mustStruct(t, map[string]any{"template": map[string]any{"serviceAccount": sharedEmail}})}},
				{Name: "//run.googleapis.com/projects/p1/locations/us-central1/services/svc-b", AssetType: "run.googleapis.com/Service", Resource: &assetpb.Resource{Location: "us-central1", Data: mustStruct(t, map[string]any{"template": map[string]any{"serviceAccount": sharedEmail}})}},
				{Name: bktURI, AssetType: "storage.googleapis.com/Bucket", Ancestors: []string{"projects/202"}, Resource: &assetpb.Resource{Location: "us-central1"}},
				{Name: dsURI, AssetType: "bigquery.googleapis.com/Dataset", Resource: &assetpb.Resource{Location: "US"}},
			},
		}, nil
	}
	monHandler := func(_ context.Context, req *monitoringpb.ListTimeSeriesRequest) (*monitoringpb.ListTimeSeriesResponse, error) {
		if !strings.Contains(req.Filter, "consumed_api") {
			return &monitoringpb.ListTimeSeriesResponse{}, nil
		}
		return &monitoringpb.ListTimeSeriesResponse{
			TimeSeries: []*monitoringpb.TimeSeries{
				{
					Resource: &monitoredres.MonitoredResource{Type: "consumed_api", Labels: map[string]string{"project_id": "p1", "service": "storage.googleapis.com", "credential_id": "serviceaccount:777", "method": "storage.objects.get"}},
					Points:   []*monitoringpb.Point{{Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 10}}}},
				},
				{
					Resource: &monitoredres.MonitoredResource{Type: "consumed_api", Labels: map[string]string{"project_id": "p1", "service": "storage.googleapis.com", "credential_id": "serviceaccount:888", "method": "storage.objects.get"}},
					Points:   []*monitoringpb.Point{{Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 5}}}},
				},
			},
		}, nil
	}
	bqHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&bigquery.JobList{
			Jobs: []*bigquery.JobListJobs{{
				UserEmail: sharedEmail,
				Configuration: &bigquery.JobConfiguration{Query: &bigquery.JobConfigurationQuery{
					DestinationTable: &bigquery.TableReference{ProjectId: "p1", DatasetId: "analytics", TableId: "t"},
				}},
				Statistics: &bigquery.JobStatistics{Query: &bigquery.JobStatistics2{
					ReferencedTables: []*bigquery.TableReference{{ProjectId: "p1", DatasetId: "analytics", TableId: "src"}},
				}},
			}},
		})
	})

	clients, _ := newTestClients(t, assetHandler, monHandler, bqHandler, nil)
	facts, err := Extract(context.Background(), clients, ExtractOptions{Scopes: []string{"projects/p1"}})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// Both Monitoring and BigQuery attribute shared-SA activity to the SA URI as Heuristic.
	if e := findEdge(facts.Edges, sharedURI, bktURI, EdgeMonitoringTraffic); e == nil || !e.Heuristic {
		t.Fatalf("expected heuristic MONITORING_TRAFFIC from shared SA, got %+v", e)
	}
	if e := findEdge(facts.Edges, sharedURI, dsURI, EdgeBQLineage); e == nil || !e.Heuristic {
		t.Fatalf("expected heuristic BQ_LINEAGE from shared SA, got %+v", e)
	}
	// Default SA referenced only by Monitoring enricher must be retained in resources[] (no dangling edge).
	if findResource(facts.Resources, defURI) == nil || findEdgeInResources(facts.Resources, facts.Edges, defURI, bktURI) == nil {
		t.Fatalf("expected enricher-referenced default SA %s retained with closed edge", defURI)
	}
}

func findEdgeInResources(resources []FactResource, edges []FactEdge, from, to string) *FactEdge {
	if findResource(resources, from) == nil || findResource(resources, to) == nil {
		return nil
	}
	for i := range edges {
		if edges[i].FromURI == from && edges[i].ToURI == to {
			return &edges[i]
		}
	}
	return nil
}

func TestCloudBuildClusterRefScopingAndEdgeClosure(t *testing.T) {
	const (
		trigP1 = "//cloudbuild.googleapis.com/projects/p1/locations/global/triggers/t-local"
		trigP3 = "//cloudbuild.googleapis.com/projects/p3/locations/global/triggers/t-cross"
		gkeP1  = "//container.googleapis.com/projects/p1/locations/us-central1/clusters/shared"
		gkeP2  = "//container.googleapis.com/projects/p2/locations/us-central1/clusters/shared"
	)

	assetHandler := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		if req.ContentType != assetpb.ContentType_RESOURCE {
			return &assetpb.ListAssetsResponse{ReadTime: timestamppb.Now()}, nil
		}
		return &assetpb.ListAssetsResponse{
			ReadTime: timestamppb.Now(),
			Assets: []*assetpb.Asset{
				{Name: "//cloudresourcemanager.googleapis.com/projects/101", AssetType: "cloudresourcemanager.googleapis.com/Project", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "p1", "projectNumber": "101"})}},
				{Name: "//cloudresourcemanager.googleapis.com/projects/202", AssetType: "cloudresourcemanager.googleapis.com/Project", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "p2", "projectNumber": "202"})}},
				{Name: "//cloudresourcemanager.googleapis.com/projects/303", AssetType: "cloudresourcemanager.googleapis.com/Project", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "p3", "projectNumber": "303"})}},
				{Name: gkeP1, AssetType: "container.googleapis.com/Cluster", Resource: &assetpb.Resource{Location: "us-central1"}},
				{Name: gkeP2, AssetType: "container.googleapis.com/Cluster", Resource: &assetpb.Resource{Location: "us-central1"}},
				// Trigger in p1 without --project resolves uniquely to p1's own cluster.
				{Name: trigP1, AssetType: "cloudbuild.googleapis.com/BuildTrigger", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
					"build": map[string]any{"steps": []any{map[string]any{"name": "gcr.io/cloud-builders/gcloud", "args": []any{"container", "clusters", "get-credentials", "--cluster=shared"}}}},
				})}},
				// Trigger in p3: step 1 has bare --cluster=shared (ambiguous across p1/p2 -> dropped);
				// step 2 scopes --project=202 (project number mapped via projIdx -> p2) -> links to gkeP2.
				{Name: trigP3, AssetType: "cloudbuild.googleapis.com/BuildTrigger", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
					"build": map[string]any{"steps": []any{
						map[string]any{"name": "gcr.io/cloud-builders/gcloud", "args": []any{"--cluster=shared"}},
						map[string]any{"name": "gcr.io/cloud-builders/gcloud", "args": []any{"--cluster=shared", "--project=202"}},
					}},
				})}},
			},
		}, nil
	}

	clients, _ := newTestClients(t, assetHandler, nil, nil, nil)
	facts, err := Extract(context.Background(), clients, ExtractOptions{Scopes: []string{"projects/p1", "projects/p2", "projects/p3"}})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !hasEdgeDetail(facts.Edges, trigP1, gkeP1, EdgeCloudBuildStepRef, "--cluster=shared") {
		t.Fatalf("expected trigP1 -> gkeP1 local cluster ref, got %+v", facts.Edges)
	}
	if hasEdge(facts.Edges, trigP3, gkeP1, EdgeCloudBuildStepRef) {
		t.Fatalf("ambiguous --cluster=shared leaked onto gkeP1")
	}
	if !hasEdgeDetail(facts.Edges, trigP3, gkeP2, EdgeCloudBuildStepRef, "--cluster=shared") {
		t.Fatalf("expected --project=2 scoped --cluster=shared to link trigP3 -> gkeP2, got %+v", facts.Edges)
	}

	// Edge closure: in-scope Project ToURI materializes Project; missing endpoint drops edge + records CoverageDanglingEdges.
	st := newExtractState([]string{"projects/p1"}, nil, 3)
	st.inScopeProjects["p1"] = true
	st.addEdge(trigP1, "//cloudresourcemanager.googleapis.com/projects/p1", EdgeConfigRef, "proj_ref")
	st.addEdge(trigP1, "//missing.googleapis.com/projects/p1/things/ghost", EdgeConfigRef, "dangling")

	resources := []FactResource{{URI: trigP1, AssetType: "cloudbuild.googleapis.com/BuildTrigger", ProjectID: "p1"}}
	valid := map[string]bool{trigP1: true}
	closed := st.filterFactEdges(&resources, valid)
	if len(closed) != 1 || findResource(resources, "//cloudresourcemanager.googleapis.com/projects/p1") == nil {
		t.Fatalf("expected in-scope Project materialized and 1 closed edge, got resources=%+v edges=%+v", resources, closed)
	}
	if len(st.ledger) != 1 || st.ledger[0].State != CoverageDanglingEdges || st.ledger[0].Source != "edge_closure" {
		t.Fatalf("expected CoverageDanglingEdges ledger entry, got %+v", st.ledger)
	}
}
