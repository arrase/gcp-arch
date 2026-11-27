package extract

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"cloud.google.com/go/asset/apiv1/assetpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/api/bigquery/v2"
	"google.golang.org/api/dataflow/v1b3"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
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
	bqHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasPrefix(r.URL.Path, "/projects/") {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_ = json.NewEncoder(w).Encode(&bigquery.JobList{
			Jobs: []*bigquery.JobListJobs{{
				UserEmail: sharedEmail,
				Configuration: &bigquery.JobConfiguration{
					Query: &bigquery.JobConfigurationQuery{
						DestinationTable: &bigquery.TableReference{ProjectId: "p1", DatasetId: "analytics", TableId: "t"},
					},
				},
				Statistics: &bigquery.JobStatistics{
					Query: &bigquery.JobStatistics2{
						ReferencedTables: []*bigquery.TableReference{{ProjectId: "p1", DatasetId: "analytics", TableId: "src"}},
					},
				},
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

func TestClassifyDisabledServiceSingleProjectAndCrossProject(t *testing.T) {
	// Single-project scan (callerQuotaProject == targetProjectID): optional enricher APIs degrade,
	// while cloudasset.googleapis.com, BILLING_DISABLED, and cross-project quota failures remain fatal.
	for _, svc := range []string{"bigquery.googleapis.com", "dataflow.googleapis.com", "monitoring.googleapis.com"} {
		err := grpcErrorWithInfo(codes.PermissionDenied, "disabled", "SERVICE_DISABLED", "googleapis.com", map[string]string{
			"consumer": "projects/solo-p",
			"service":  svc,
		})
		ce := ClassifyGCPError(err, "solo-p", "solo-p", "101")
		if ce.Kind != ErrorKindTargetServiceDisabled || !ce.IsDegraded() {
			t.Fatalf("single-project %s SERVICE_DISABLED: got %+v, want TARGET_SERVICE_DISABLED", svc, ce)
		}
	}

	fatalCases := []struct {
		name      string
		reason    string
		consumer  string
		service   string
		callerQP  string
		targetID  string
		targetNum string
	}{
		{"single-project cloudasset", "SERVICE_DISABLED", "projects/solo-p", "cloudasset.googleapis.com", "solo-p", "solo-p", "101"},
		{"single-project billing", "BILLING_DISABLED", "projects/solo-p", "bigquery.googleapis.com", "solo-p", "solo-p", "101"},
		{"cross-project quota df", "SERVICE_DISABLED", "projects/caller-qp", "dataflow.googleapis.com", "caller-qp", "target-p", "202"},
	}
	for _, tc := range fatalCases {
		err := grpcErrorWithInfo(codes.PermissionDenied, tc.name, tc.reason, "googleapis.com", map[string]string{
			"consumer": tc.consumer,
			"service":  tc.service,
		})
		if ce := ClassifyGCPError(err, tc.callerQP, tc.targetID, tc.targetNum); !ce.IsFatalQuota() {
			t.Fatalf("%s must stay fatal, got %+v", tc.name, ce)
		}
	}
}

func hasCoverageState(ledger []CoverageEntry, scope, source string, state CoverageState) bool {
	for _, e := range ledger {
		if (scope == "" || e.Scope == scope) && (source == "" || e.Source == source) && e.State == state {
			return true
		}
	}
	return false
}

func TestExtractCAIScopeAndSingleProjectEnricherServiceDisabled(t *testing.T) {
	assetHandler := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		if req.Parent == "projects/child-disabled" {
			return nil, grpcErrorWithInfo(codes.PermissionDenied, "cai disabled on child", "SERVICE_DISABLED", "googleapis.com", map[string]string{
				"consumer": "projects/child-disabled",
				"service":  "cloudasset.googleapis.com",
			})
		}
		if req.ContentType != assetpb.ContentType_RESOURCE {
			return &assetpb.ListAssetsResponse{ReadTime: timestamppb.Now()}, nil
		}
		return &assetpb.ListAssetsResponse{
			ReadTime: timestamppb.Now(),
			Assets: []*assetpb.Asset{
				{Name: "//cloudresourcemanager.googleapis.com/projects/solo-p", AssetType: assetTypeProject, Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "solo-p"})}},
			},
		}, nil
	}
	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	httpMux.HandleFunc("/projects/solo-p/jobs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"BQ disabled","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SERVICE_DISABLED","domain":"googleapis.com","metadata":{"consumer":"projects/solo-p","service":"bigquery.googleapis.com"}}]}}`))
	})
	httpMux.HandleFunc("/v1b3/projects/solo-p/locations/-/jobs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"DF disabled","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SERVICE_DISABLED","domain":"googleapis.com","metadata":{"consumer":"projects/solo-p","service":"dataflow.googleapis.com"}}]}}`))
	})

	clients, _ := newTestClients(t, assetHandler, nil, httpMux, nil)
	facts, err := Extract(context.Background(), clients, ExtractOptions{Scopes: []string{"projects/child-disabled"}})
	if err != nil {
		t.Fatalf("expected child-disabled CAI SERVICE_DISABLED to degrade, got %v", err)
	}
	if !hasCoverageState(facts.CoverageLedger, "projects/child-disabled", "", CoverageServiceDisabled) {
		t.Fatalf("expected CoverageServiceDisabled for projects/child-disabled, got %+v", facts.CoverageLedger)
	}

	// Single-project scan with QuotaProject == targetProjectID degrades optional BQ/Dataflow APIs cleanly.
	clients.QuotaProject = "solo-p"
	soloFacts, err := Extract(context.Background(), clients, ExtractOptions{Scopes: []string{"projects/solo-p"}})
	if err != nil {
		t.Fatalf("expected single-project optional enricher SERVICE_DISABLED to degrade, got %v", err)
	}
	if !hasCoverageState(soloFacts.CoverageLedger, "", sourceBQJobs, CoverageServiceDisabled) ||
		!hasCoverageState(soloFacts.CoverageLedger, "", sourceDataflowJobs, CoverageServiceDisabled) {
		t.Fatalf("expected BQ and Dataflow degraded coverage entries, got %+v", soloFacts.CoverageLedger)
	}
}

func TestPreflightOrgScopeSamplesProjectAssetType(t *testing.T) {
	var sawProjectTypeProbe, bqProbed atomic.Bool

	assetHandler := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		if slices.Contains(req.AssetTypes, assetTypeProject) {
			sawProjectTypeProbe.Store(true)
			return &assetpb.ListAssetsResponse{
				Assets: []*assetpb.Asset{{
					Name:      "//cloudresourcemanager.googleapis.com/projects/7001",
					AssetType: assetTypeProject,
					Ancestors: []string{"projects/7001", "organizations/999"},
					Resource:  &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "org-child"})},
				}},
			}, nil
		}
		if slices.Contains(req.AssetTypes, "cloudresourcemanager.googleapis.com/Folder") {
			return &assetpb.ListAssetsResponse{}, nil
		}
		// Unfiltered call returns the Organization itself (no projects/ ancestor).
		return &assetpb.ListAssetsResponse{
			Assets: []*assetpb.Asset{{
				Name:      "//cloudresourcemanager.googleapis.com/organizations/999",
				AssetType: "cloudresourcemanager.googleapis.com/Organization",
				Ancestors: []string{"organizations/999"},
			}},
		}, nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/projects/org-child/jobs", func(w http.ResponseWriter, _ *http.Request) {
		bqProbed.Store(true)
		_, _ = w.Write([]byte(`{"jobs":[]}`))
	})
	mux.HandleFunc("/v1b3/projects/org-child/locations/-/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jobs":[]}`))
	})

	clients, _ := newTestClients(t, assetHandler, nil, mux, nil)
	if _, err := RunPreflight(context.Background(), clients, []string{"organizations/999"}, VertexPreflightOptions{}); err != nil {
		t.Fatalf("RunPreflight: %v", err)
	}
	if !sawProjectTypeProbe.Load() || !bqProbed.Load() {
		t.Fatalf("expected Project AssetTypes probe (%v) and org-child BQ enricher probe (%v)", sawProjectTypeProbe.Load(), bqProbed.Load())
	}
}

func TestPreflightOrgScopeFallbackSampling(t *testing.T) {
	// When Project AssetTypes returns empty, generic PageSize=1 still samples project from ancestors.
	var sawFallback, fallbackBQ atomic.Bool
	fallbackAssets := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		if len(req.AssetTypes) == 0 {
			sawFallback.Store(true)
			return &assetpb.ListAssetsResponse{
				Assets: []*assetpb.Asset{{
					Name:      "//storage.googleapis.com/b1",
					Ancestors: []string{"projects/fb-proj", "organizations/999"},
				}},
			}, nil
		}
		return &assetpb.ListAssetsResponse{}, nil
	}
	fbMux := http.NewServeMux()
	fbMux.HandleFunc("/projects/fb-proj/jobs", func(w http.ResponseWriter, _ *http.Request) {
		fallbackBQ.Store(true)
		_, _ = w.Write([]byte(`{"jobs":[]}`))
	})
	fbMux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	fbClients, _ := newTestClients(t, fallbackAssets, nil, fbMux, nil)
	if _, err := RunPreflight(context.Background(), fbClients, []string{"organizations/999"}, VertexPreflightOptions{}); err != nil {
		t.Fatalf("RunPreflight fallback: %v", err)
	}
	if !sawFallback.Load() || !fallbackBQ.Load() {
		t.Fatalf("expected fallback CAI probe (%v) and fb-proj BQ probe (%v)", sawFallback.Load(), fallbackBQ.Load())
	}
}

func TestBigQueryStreamingDedupAndDataflowFields(t *testing.T) {
	const (
		rawDS  = "//bigquery.googleapis.com/projects/p-df/datasets/raw"
		goldDS = "//bigquery.googleapis.com/projects/p-df/datasets/gold"
		tempDS = "//bigquery.googleapis.com/projects/p-df/datasets/staging"
		bktURI = "//storage.googleapis.com/p-df-out"
		vpcURI = "//compute.googleapis.com/projects/p-df/global/networks/data-vpc"
		subURI = "//compute.googleapis.com/projects/p-df/regions/us-central1/subnetworks/data-sub"
		saURI  = "//iam.googleapis.com/projects/p-df/serviceAccounts/etl@p-df.iam.gserviceaccount.com"
	)

	assetHandler := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		if req.ContentType != assetpb.ContentType_RESOURCE {
			return &assetpb.ListAssetsResponse{ReadTime: timestamppb.Now()}, nil
		}
		return &assetpb.ListAssetsResponse{
			ReadTime: timestamppb.Now(),
			Assets: []*assetpb.Asset{
				{Name: "//cloudresourcemanager.googleapis.com/projects/p-df", AssetType: assetTypeProject, Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "p-df"})}},
				{Name: vpcURI, AssetType: "compute.googleapis.com/Network"},
				{Name: subURI, AssetType: assetTypeSubnetwork, Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"network": vpcURI})}},
				{Name: rawDS, AssetType: "bigquery.googleapis.com/Dataset"},
				{Name: goldDS, AssetType: "bigquery.googleapis.com/Dataset"},
				{Name: tempDS, AssetType: "bigquery.googleapis.com/Dataset"},
				{Name: bktURI, AssetType: "storage.googleapis.com/Bucket", Ancestors: []string{"projects/p-df"}},
				{Name: saURI, AssetType: assetTypeServiceAccount, Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"email": "etl@p-df.iam.gserviceaccount.com"})}},
			},
		}, nil
	}

	var sawDFFields atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/projects/p-df/jobs", func(w http.ResponseWriter, _ *http.Request) {
		// Emit duplicate ETL jobs plus a read-only query to verify streaming dedup.
		job := &bigquery.JobListJobs{
			Id:        "dup-job",
			UserEmail: "etl@p-df.iam.gserviceaccount.com",
			Statistics: &bigquery.JobStatistics{
				Query: &bigquery.JobStatistics2{
					ReferencedTables: []*bigquery.TableReference{{ProjectId: "p-df", DatasetId: "raw", TableId: "t1"}},
				},
			},
			Configuration: &bigquery.JobConfiguration{
				Query: &bigquery.JobConfigurationQuery{
					DestinationTable: &bigquery.TableReference{ProjectId: "p-df", DatasetId: "gold", TableId: "t2"},
				},
			},
		}
		readOnly := &bigquery.JobListJobs{
			Id:        "ro-job",
			UserEmail: "etl@p-df.iam.gserviceaccount.com",
			Statistics: &bigquery.JobStatistics{
				Query: &bigquery.JobStatistics2{
					ReferencedTables: []*bigquery.TableReference{{ProjectId: "p-df", DatasetId: "staging", TableId: "t3"}},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(&bigquery.JobList{Jobs: []*bigquery.JobListJobs{job, job, readOnly}})
	})
	mux.HandleFunc("/v1b3/projects/p-df/locations/-/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(&dataflow.ListJobsResponse{
			Jobs: []*dataflow.Job{{Id: "job-1", Name: "etl-pipe", Location: "us-central1"}},
		})
	})
	mux.HandleFunc("/v1b3/projects/p-df/locations/us-central1/jobs/job-1", func(w http.ResponseWriter, r *http.Request) {
		fields := r.URL.Query().Get("fields")
		if strings.Contains(fields, "workerPools(network,subnetwork)") && strings.Contains(fields, "dataset") {
			sawDFFields.Store(true)
		}
		_ = json.NewEncoder(w).Encode(&dataflow.Job{
			Id:           "job-1",
			Name:         "etl-pipe",
			Type:         "JOB_TYPE_BATCH",
			CurrentState: "JOB_STATE_RUNNING",
			Environment: &dataflow.Environment{
				ServiceAccountEmail: "etl@p-df.iam.gserviceaccount.com",
				Dataset:             "p-df.staging",
				WorkerPools: []*dataflow.WorkerPool{{
					Network:    "data-vpc",
					Subnetwork: "projects/p-df/regions/us-central1/subnetworks/data-sub",
				}},
			},
			PipelineDescription: &dataflow.PipelineDescription{
				OriginalPipelineTransform: []*dataflow.TransformSummary{{
					DisplayData: []*dataflow.DisplayData{{Url: "gs://p-df-out/export"}},
				}},
			},
		})
	})

	clients, _ := newTestClients(t, assetHandler, nil, mux, nil)
	facts, err := Extract(context.Background(), clients, ExtractOptions{Scopes: []string{"projects/p-df"}})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !sawDFFields.Load() {
		t.Fatal("Dataflow jobs.get did not pass expected fields parameter")
	}
	dfURI := "//dataflow.googleapis.com/projects/p-df/locations/us-central1/jobs/job-1"
	dfRes := findResource(facts.Resources, dfURI)
	if dfRes == nil || dfRes.Attributes["vpc"] != vpcURI || dfRes.Attributes["subnet"] != subURI || dfRes.ParentURI != subURI {
		t.Fatalf("expected Dataflow job with vpc=%s subnet=%s, got %+v", vpcURI, subURI, dfRes)
	}
	for _, want := range []struct {
		from, to string
		kind     FactEdgeKind
	}{
		{rawDS, goldDS, EdgeBQLineage},
		{dfURI, rawDS, EdgeBQLineage},
		{dfURI, goldDS, EdgeBQLineage},
		{dfURI, tempDS, EdgeBQLineage},
		{dfURI, bktURI, EdgeConfigRef},
	} {
		if !hasEdge(facts.Edges, want.from, want.to, want.kind) {
			t.Errorf("missing expected edge %s -[%s]-> %s", want.from, want.kind, want.to)
		}
	}
}

func TestExtractSanitizedRefsFastPathAndIAMIngestionFilter(t *testing.T) {
	// Fast path: plain scalar values allocate nothing and return nil, nil.
	for _, plain := range []string{"", "true", "8080", "production", "INFO"} {
		e, s := ExtractSanitizedRefs(plain, nil)
		if len(e) != 0 || len(s) != 0 {
			t.Fatalf("ExtractSanitizedRefs(%q) = %v, %v, want empty", plain, e, s)
		}
	}
	// Command without /:.- still matches gcloud run deploy.
	e, _ := ExtractSanitizedRefs("gcloud run deploy api", nil)
	if !slices.Equal(e, []string{"run-deploy:api"}) {
		t.Fatalf("ExtractSanitizedRefs(gcloud run deploy api) = %v, want [run-deploy:api]", e)
	}

	// IAM ingestion filtering keeps Org/Folder broad roles for SA capabilities while dropping
	// non-SA members, workloadIdentityUser, and non-broad Org/Folder bindings.
	st := newExtractState([]string{"organizations/999"}, nil, 3)
	st.ingestIAMPolicyAsset(&assetpb.Asset{
		Name:      "//cloudresourcemanager.googleapis.com/organizations/999",
		AssetType: "cloudresourcemanager.googleapis.com/Organization",
		IamPolicy: &iampb.Policy{Bindings: []*iampb.Binding{
			{Role: "roles/viewer", Members: []string{"user:alice@example.com", "serviceAccount:sa@p1.iam.gserviceaccount.com", "serviceAccount:p1.svc.id.goog[ns/ksa]"}},
			{Role: "roles/storage.objectAdmin", Members: []string{"serviceAccount:sa@p1.iam.gserviceaccount.com"}},
			{Role: "roles/iam.workloadIdentityUser", Members: []string{"serviceAccount:sa@p1.iam.gserviceaccount.com"}},
		}},
	})
	if len(st.iamBindings) != 1 || st.iamBindings[0].role != "roles/viewer" || !slices.Equal(st.iamBindings[0].saEmails, []string{"sa@p1.iam.gserviceaccount.com"}) {
		t.Fatalf("unexpected filtered IAM bindings: %+v", st.iamBindings)
	}
}
