package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/asset/apiv1/assetpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"golang.org/x/oauth2/google"
	"golang.org/x/time/rate"
	"google.golang.org/api/bigquery/v2"
	"google.golang.org/api/dataflow/v1b3"
	"google.golang.org/api/googleapi"
	"google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// --- Fake gRPC Services over bufconn ---

type fakeAssetServer struct {
	assetpb.UnimplementedAssetServiceServer
	handler func(context.Context, *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error)
}

func (f *fakeAssetServer) ListAssets(ctx context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
	return f.handler(ctx, req)
}

type fakeMonitoringServer struct {
	monitoringpb.UnimplementedMetricServiceServer
	handler func(context.Context, *monitoringpb.ListTimeSeriesRequest) (*monitoringpb.ListTimeSeriesResponse, error)
}

func (f *fakeMonitoringServer) ListTimeSeries(ctx context.Context, req *monitoringpb.ListTimeSeriesRequest) (*monitoringpb.ListTimeSeriesResponse, error) {
	return f.handler(ctx, req)
}

func startBufconnServer(t *testing.T, register func(*grpc.Server)) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(2 << 20)
	srv := grpc.NewServer()
	register(srv)
	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
	})

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func grpcErrorWithInfo(code codes.Code, msg, reason, domain string, meta map[string]string) error {
	st := status.New(code, msg)
	detailed, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason:   reason,
		Domain:   domain,
		Metadata: meta,
	})
	if err != nil {
		panic(err)
	}
	return detailed.Err()
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	st, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("NewStruct: %v", err)
	}
	return st
}

func newTestClients(
	t *testing.T,
	assetHandler func(context.Context, *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error),
	monHandler func(context.Context, *monitoringpb.ListTimeSeriesRequest) (*monitoringpb.ListTimeSeriesResponse, error),
	httpHandler http.Handler,
	aliases map[string]string,
) (*Clients, *httptest.Server) {
	t.Helper()
	if assetHandler == nil {
		assetHandler = func(context.Context, *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
			return &assetpb.ListAssetsResponse{ReadTime: timestamppb.Now()}, nil
		}
	}
	if monHandler == nil {
		monHandler = func(context.Context, *monitoringpb.ListTimeSeriesRequest) (*monitoringpb.ListTimeSeriesResponse, error) {
			return &monitoringpb.ListTimeSeriesResponse{}, nil
		}
	}
	conn := startBufconnServer(t, func(s *grpc.Server) {
		assetpb.RegisterAssetServiceServer(s, &fakeAssetServer{handler: assetHandler})
		monitoringpb.RegisterMetricServiceServer(s, &fakeMonitoringServer{handler: monHandler})
	})
	if httpHandler == nil {
		httpHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		})
	}
	httpSrv := httptest.NewServer(httpHandler)
	t.Cleanup(httpSrv.Close)

	var progress bytes.Buffer
	clients, err := NewClients(context.Background(), ClientOptions{
		QuotaProject:        "caller-quota",
		ProjectAliases:      aliases,
		AssetConn:           conn,
		MonitoringConn:      conn,
		HTTPClient:          httpSrv.Client(),
		BigQueryEndpoint:    httpSrv.URL,
		DataflowEndpoint:    httpSrv.URL,
		CAIRateLimit:        rate.Limit(10000),
		MonitoringRateLimit: rate.Limit(10000),
		BQLimiter:           rate.NewLimiter(rate.Limit(10000), 100),
		DataflowLimiter:     rate.NewLimiter(rate.Limit(10000), 100),
		ProgressWriter:      &progress,
		RetryBackoff:        time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClients: %v", err)
	}
	return clients, httpSrv
}

func hasEdge(edges []FactEdge, from, to string, kind FactEdgeKind) bool {
	for _, e := range edges {
		if e.FromURI == from && e.ToURI == to && e.Kind == kind {
			return true
		}
	}
	return false
}

func findResource(resources []FactResource, uri string) *FactResource {
	for i := range resources {
		if resources[i].URI == uri {
			return &resources[i]
		}
	}
	return nil
}

// --- Client, Error Classification, Rate Limit & Preflight Tests ---

func TestClassifyGCPErrorRateLimitAndQuotaProject(t *testing.T) {
	assertQuotaAndDegradedErrors(t)
	assertRateLimitAndRetry(t)
	assertQuotaProjectAndProgress(t)
}

func assertQuotaAndDegradedErrors(t *testing.T) {
	t.Helper()
	if ce := ClassifyGCPError(nil, "", "", ""); ce.Kind != ErrorKindNone || ce.CoverageState() != "" {
		t.Fatalf("expected ErrorKindNone for nil, got %+v", ce)
	}

	for _, reason := range []string{"SERVICE_DISABLED", "BILLING_DISABLED", "USER_PROJECT_DENIED"} {
		err := grpcErrorWithInfo(codes.PermissionDenied, "quota fail", reason, "googleapis.com", map[string]string{"consumer": "projects/caller-qp"})
		ce := ClassifyGCPError(err, "caller-qp", "target-p", "111")
		if !ce.IsFatalQuota() || ce.IsDegraded() {
			t.Errorf("reason %s: got %+v, want fatal quota", reason, ce)
		}
		qErr := &QuotaProjectError{Classified: ce}
		if !strings.Contains(qErr.Error(), reason) || !errors.Is(qErr, err) {
			t.Errorf("QuotaProjectError mismatch: %v", qErr)
		}
		if rem := quotaRemediation("caller-qp", reason); rem == "" {
			t.Errorf("empty quotaRemediation for %s", reason)
		}
	}
	if rem := quotaRemediation("", "OTHER"); !strings.Contains(rem, "<QUOTA_PROJECT>") {
		t.Errorf("unexpected default quotaRemediation: %q", rem)
	}

	childDisabled := grpcErrorWithInfo(codes.PermissionDenied, "API disabled in target", "SERVICE_DISABLED", "googleapis.com", map[string]string{"consumer": "projects/child-a"})
	if ce := ClassifyGCPError(childDisabled, "caller-qp", "child-a", "222"); !ce.IsDegraded() || ce.CoverageState() != CoverageServiceDisabled {
		t.Fatalf("expected CoverageServiceDisabled for target child project, got %+v", ce)
	}

	vpcErr := grpcErrorWithInfo(codes.PermissionDenied, "prohibited", "SECURITY_POLICY_VIOLATED", "googleapis.com", map[string]string{"vpcServiceControlsUniqueIdentifier": "uuid-1"})
	if ce := ClassifyGCPError(vpcErr, "caller-qp", "child-a", ""); !ce.IsDegraded() || ce.CoverageState() != CoverageVPCSCBlocked || ce.VPCSCUUID != "uuid-1" {
		t.Fatalf("unexpected gRPC VPC-SC classification: %+v", ce)
	}
	restVPCSC := &googleapi.Error{
		Code:    http.StatusForbidden,
		Message: "blocked",
		Details: []any{
			map[string]any{
				"@type":    "type.googleapis.com/google.rpc.ErrorInfo",
				"reason":   "VPC_SERVICE_CONTROLS",
				"domain":   "googleapis.com",
				"metadata": map[string]any{"vpcServiceControlsUniqueIdentifier": "rest-uuid-9"},
			},
		},
	}
	if ce := ClassifyGCPError(restVPCSC, "caller-qp", "child-a", ""); ce.CoverageState() != CoverageVPCSCBlocked || ce.VPCSCUUID != "rest-uuid-9" {
		t.Fatalf("unexpected REST VPC-SC classification: %+v", ce)
	}
	restBodyFallback := &googleapi.Error{
		Code: http.StatusForbidden,
		Body: `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"IAM_PERMISSION_DENIED","domain":"iam.googleapis.com","metadata":{"vpcServiceControlsUniqueIdentifier":""}}]}}`,
	}
	if ce := ClassifyGCPError(restBodyFallback, "caller-qp", "child-a", ""); ce.CoverageState() != CoverageIAMDenied {
		t.Fatalf("unexpected REST body fallback classification: %+v", ce)
	}
	if ce := ClassifyGCPError(&googleapi.Error{Code: http.StatusUnauthorized}, "caller-qp", "", ""); ce.Kind != ErrorKindUnauthenticated {
		t.Fatalf("expected Unauthenticated on 401, got %+v", ce)
	}
	if ce := ClassifyGCPError(status.Error(codes.Unauthenticated, "expired"), "caller-qp", "", ""); ce.Kind != ErrorKindUnauthenticated {
		t.Fatalf("expected Unauthenticated on gRPC Unauthenticated, got %+v", ce)
	}
	if ce := ClassifyGCPError(status.Error(codes.PermissionDenied, "403"), "caller-qp", "", ""); ce.CoverageState() != CoverageIAMDenied {
		t.Fatalf("expected IAM_DENIED on raw PermissionDenied, got %+v", ce)
	}
	if ce := ClassifyGCPError(errors.New("network timeout"), "caller-qp", "", ""); ce.Kind != ErrorKindOther {
		t.Fatalf("expected ErrorKindOther, got %+v", ce)
	}
}

func assertRateLimitAndRetry(t *testing.T) {
	t.Helper()
	if isRateLimitError(nil) || isRateLimitError(errors.New("plain")) {
		t.Fatal("unexpected rate limit detection")
	}
	if !isRateLimitError(status.Error(codes.ResourceExhausted, "quota")) ||
		!isRateLimitError(grpcErrorWithInfo(codes.Unavailable, "rl", "RATE_LIMIT_EXCEEDED", "googleapis.com", nil)) ||
		!isRateLimitError(&googleapi.Error{Code: http.StatusTooManyRequests}) ||
		!isRateLimitError(&googleapi.Error{Code: http.StatusForbidden, Errors: []googleapi.ErrorItem{{Reason: "rateLimitExceeded"}}}) {
		t.Fatal("failed to detect rate limit errors")
	}
	attempts := 0
	err := retryOnRateLimit(context.Background(), time.Millisecond, func() error {
		attempts++
		if attempts < 3 {
			return &googleapi.Error{Code: http.StatusTooManyRequests}
		}
		return nil
	})
	if err != nil || attempts != 3 {
		t.Fatalf("retryOnRateLimit: err=%v attempts=%d", err, attempts)
	}
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := retryOnRateLimit(canceledCtx, time.Second, func() error {
		return status.Error(codes.ResourceExhausted, "rl")
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func assertQuotaProjectAndProgress(t *testing.T) {
	t.Helper()
	creds := &google.Credentials{JSON: []byte(`{"type":"authorized_user","quota_project_id":"adc-qp"}`)}
	if got := ResolveQuotaProject(creds, ""); got != "adc-qp" {
		t.Fatalf("ResolveQuotaProject(adc, \"\") = %q, want adc-qp", got)
	}
	if got := ResolveQuotaProject(creds, "override-qp"); got != "override-qp" {
		t.Fatalf("ResolveQuotaProject(adc, override) = %q, want override-qp", got)
	}
	if got := ResolveQuotaProject(&google.Credentials{}, ""); got != "" {
		t.Fatalf("ResolveQuotaProject(empty) = %q, want empty", got)
	}

	var buf bytes.Buffer
	c := &Clients{ProgressWriter: &buf}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c.ReportProgress(false, "step %d", n)
		}(i)
	}
	wg.Wait()
	c.ReportProgress(true, "done")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 || lines[1] != "done" {
		t.Fatalf("unexpected throttled progress output: %q", buf.String())
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if rem := iamRemediation("folders/123", "roles/viewer"); !strings.Contains(rem, "folders add-iam-policy-binding") {
		t.Errorf("unexpected folder remediation: %q", rem)
	}
	if rem := iamRemediation("organizations/999", "roles/viewer"); !strings.Contains(rem, "organizations add-iam-policy-binding") {
		t.Errorf("unexpected org remediation: %q", rem)
	}
	if rem := iamRemediation("custom/scope", "roles/viewer"); !strings.Contains(rem, "Grant roles/viewer") {
		t.Errorf("unexpected fallback remediation: %q", rem)
	}
}

func TestRunPreflightTargetAndVertexProbes(t *testing.T) {
	clients, httpSrv, counters := setupPreflightProbeClients(t)
	assertFolderAndVertexPreflight(t, clients, httpSrv, counters)
	assertCustomModelDeniedPreflight(t, clients)
	assertFatalQuotaPreflight(t)
}

type preflightProbeCounters struct {
	sawFolderProbe  atomic.Bool
	bqCalls         atomic.Int32
	dfCalls         atomic.Int32
	crmCalls        atomic.Int32
	vertexRESTCalls atomic.Int32
}

func setupPreflightProbeClients(t *testing.T) (*Clients, *httptest.Server, *preflightProbeCounters) {
	t.Helper()
	counters := &preflightProbeCounters{}
	assetHandler := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		if req.PageSize != 1 {
			t.Errorf("expected PageSize=1, got %d", req.PageSize)
		}
		if slices.Contains(req.AssetTypes, "cloudresourcemanager.googleapis.com/Folder") {
			counters.sawFolderProbe.Store(true)
			return &assetpb.ListAssetsResponse{
				Assets: []*assetpb.Asset{{
					Name:      "//cloudresourcemanager.googleapis.com/folders/444",
					AssetType: "cloudresourcemanager.googleapis.com/Folder",
					Resource:  &assetpb.Resource{Data: mustStruct(t, map[string]any{"displayName": ""})},
				}},
			}, nil
		}
		return &assetpb.ListAssetsResponse{
			Assets: []*assetpb.Asset{{
				Name:      "//storage.googleapis.com/b1",
				Ancestors: []string{"projects/111222333", "folders/444", "organizations/999"},
			}},
		}, nil
	}
	monHandler := func(_ context.Context, req *monitoringpb.ListTimeSeriesRequest) (*monitoringpb.ListTimeSeriesResponse, error) {
		if req.View != monitoringpb.ListTimeSeriesRequest_HEADERS {
			t.Errorf("expected HEADERS view, got %v", req.View)
		}
		return &monitoringpb.ListTimeSeriesResponse{}, nil
	}

	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/projects/app-prod/jobs", func(w http.ResponseWriter, r *http.Request) {
		counters.bqCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs":[]}`))
	})
	httpMux.HandleFunc("/v1b3/projects/app-prod/locations/-/jobs", func(w http.ResponseWriter, r *http.Request) {
		counters.dfCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs":[]}`))
	})
	httpMux.HandleFunc("/v1/projects/ai-sandbox:testIamPermissions", func(w http.ResponseWriter, r *http.Request) {
		counters.crmCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"permissions":["aiplatform.endpoints.predict"]}`))
	})
	httpMux.HandleFunc("/v1/projects/ai-sandbox/locations/global/publishers/google/models/gemini-4-carbon", func(w http.ResponseWriter, r *http.Request) {
		counters.vertexRESTCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"publishers/google/models/gemini-4-carbon"}`))
	})
	httpMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected HTTP path probed: %s", r.URL.Path)
		http.NotFound(w, r)
	})

	clients, httpSrv := newTestClients(t, assetHandler, monHandler, httpMux, map[string]string{"111222333": "app-prod"})
	return clients, httpSrv, counters
}

func assertFolderAndVertexPreflight(t *testing.T, clients *Clients, httpSrv *httptest.Server, counters *preflightProbeCounters) {
	t.Helper()
	rep, err := RunPreflight(context.Background(), clients, []string{"folders/444"}, VertexPreflightOptions{
		Project:        "ai-sandbox",
		HTTPClient:     httpSrv.Client(),
		CRMEndpoint:    httpSrv.URL,
		VertexEndpoint: httpSrv.URL,
	})
	if err != nil {
		t.Fatalf("RunPreflight error: %v", err)
	}
	if !counters.sawFolderProbe.Load() || counters.bqCalls.Load() != 1 || counters.dfCalls.Load() != 1 ||
		counters.crmCalls.Load() != 1 || counters.vertexRESTCalls.Load() != 1 {
		t.Fatalf("probe call counts mismatch: folder=%v bq=%d df=%d crm=%d vertex=%d",
			counters.sawFolderProbe.Load(), counters.bqCalls.Load(), counters.dfCalls.Load(),
			counters.crmCalls.Load(), counters.vertexRESTCalls.Load())
	}

	probeMap := make(map[string]PreflightProbe)
	for _, p := range rep.Probes {
		probeMap[p.Scope+"|"+p.Source] = p
		if strings.Contains(p.Source, "accesscontextmanager") || strings.Contains(p.Source, "rag") {
			t.Fatalf("legacy probe should be omitted: %+v", p)
		}
	}
	if p := probeMap["folders/444|cai.folders"]; p.Status != ProbeDenied {
		t.Errorf("empty folder displayName should report ProbeDenied, got %+v", p)
	}
	if p := probeMap["projects/ai-sandbox|vertex.iam"]; p.Status != ProbeGranted {
		t.Errorf("vertex.iam status = %+v, want GRANTED", p)
	}
	if p := probeMap["projects/ai-sandbox|vertex.model"]; p.Status != ProbeGranted {
		t.Errorf("vertex.model status = %+v, want GRANTED", p)
	}
}

func assertCustomModelDeniedPreflight(t *testing.T, clients *Clients) {
	t.Helper()
	deniedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"permissions":[]}`))
	}))
	t.Cleanup(deniedSrv.Close)

	var customCalled bool
	rep2, err := RunPreflight(context.Background(), clients, []string{"projects/app-prod"}, VertexPreflightOptions{
		Project:     "ai-sandbox",
		Location:    "europe-west1",
		Model:       "gemini-4-custom",
		HTTPClient:  deniedSrv.Client(),
		CRMEndpoint: deniedSrv.URL,
		ModelProbe: func(_ context.Context, proj, loc, model string) error {
			customCalled = true
			if proj != "ai-sandbox" || loc != "europe-west1" || model != "gemini-4-custom" {
				t.Errorf("unexpected ModelProbe args: %s %s %s", proj, loc, model)
			}
			return &googleapi.Error{Code: http.StatusForbidden, Message: "model denied"}
		},
	})
	if err != nil {
		t.Fatalf("RunPreflight with degraded vertex IAM/model should not return fatal error: %v", err)
	}
	if !customCalled {
		t.Fatal("expected custom ModelProbe to be invoked")
	}
	var sawIAMDenied, sawModelDenied bool
	for _, p := range rep2.Probes {
		if p.Source == "vertex.iam" && p.Status == ProbeDenied {
			sawIAMDenied = true
		}
		if p.Source == "vertex.model" && p.Status == ProbeDenied {
			sawModelDenied = true
		}
	}
	if !sawIAMDenied || !sawModelDenied {
		t.Fatalf("expected vertex.iam & vertex.model DENIED, got %+v", rep2.Probes)
	}
}

func assertFatalQuotaPreflight(t *testing.T) {
	t.Helper()
	quotaClients, _ := newTestClients(t, func(context.Context, *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		return nil, grpcErrorWithInfo(codes.PermissionDenied, "quota disabled", "SERVICE_DISABLED", "googleapis.com", nil)
	}, nil, nil, nil)
	_, err := RunPreflight(context.Background(), quotaClients, []string{"projects/p1"}, VertexPreflightOptions{})
	var qErr *QuotaProjectError
	if !errors.As(err, &qErr) {
		t.Fatalf("expected *QuotaProjectError, got %v", err)
	}
}

// --- Sanitization & Allowlist Tests ---

func TestSanitizeURLAndExtractSanitizedRefs(t *testing.T) {
	projIdx := map[string]string{"111222333": "acme-cicd"}

	if got := SanitizeURL("jdbc:postgresql://admin:secret@10.100.0.5:5432/sonar?ssl=true#x"); got != "postgresql://10.100.0.5:5432/sonar" {
		t.Fatalf("SanitizeURL(jdbc) = %q", got)
	}
	if got := SanitizeURL("not a url?tok=1#f"); got != "not a url" {
		t.Fatalf("SanitizeURL(plain) = %q", got)
	}

	cases := []struct {
		name    string
		raw     string
		wantEnv []string
		wantSec []string
	}{
		{
			name:    "artifact registry docker pkg dev",
			raw:     "docker push europe-southwest1-docker.pkg.dev/111222333/acme-apps/api:sha-abc",
			wantEnv: []string{"//artifactregistry.googleapis.com/projects/acme-cicd/locations/europe-southwest1/repositories/acme-apps"},
		},
		{
			name:    "gcr container registry",
			raw:     "eu.gcr.io/acme-prod/worker:v2",
			wantEnv: []string{"gcr.io/acme-prod"},
		},
		{
			name:    "gcs bucket strips object path and query",
			raw:     "gs://acme-tfstate/prod/default.tfstate?generation=1",
			wantEnv: []string{"gs://acme-tfstate"},
		},
		{
			name:    "bare cloud sql connection name",
			raw:     "INSTANCE=acme-cicd:europe-southwest1:sonarqube",
			wantEnv: []string{"//sqladmin.googleapis.com/projects/acme-cicd/instances/sonarqube"},
		},
		{
			name:    "cloud sql unix socket",
			raw:     "/cloudsql/111222333:europe-southwest1:sonarqube",
			wantEnv: []string{"//sqladmin.googleapis.com/projects/acme-cicd/instances/sonarqube"},
		},
		{
			name:    "gcloud deploy flags",
			raw:     "gcloud beta run deploy release-webhook --project acme-prod --cluster=prod-gke",
			wantEnv: []string{"--project=acme-prod", "--cluster=prod-gke", "run-deploy:release-webhook"},
		},
		{
			name:    "jdbc connection string strips creds keeps private ip",
			raw:     "jdbc:postgresql://sonar:hunter2@10.100.0.5:5432/sonar?sslmode=require",
			wantEnv: []string{"10.100.0.5"},
		},
		{
			name:    "private ips RFC1918 and CGNAT only",
			raw:     "172.16.4.12 192.168.1.9 100.64.0.8 8.8.8.8",
			wantEnv: []string{"172.16.4.12", "192.168.1.9", "100.64.0.8"},
		},
		{
			name:    "cloud run and cloud functions urls",
			raw:     "https://u:p@api-abc123-ew.a.run.app/v1?k=1 https://europe-west1-acme-cicd.cloudfunctions.net/notify?tok=x",
			wantEnv: []string{"https://api-abc123-ew.a.run.app", "https://europe-west1-acme-cicd.cloudfunctions.net/notify"},
		},
		{
			name:    "secret manager routed to secretRefs only",
			raw:     "projects/111222333/secrets/sonarqube-db-password/versions/latest",
			wantSec: []string{"//secretmanager.googleapis.com/projects/acme-cicd/secrets/sonarqube-db-password/versions/latest"},
		},
		{
			name:    "aip122 and googleapis url",
			raw:     "//pubsub.googleapis.com/projects/111222333/topics/events https://www.googleapis.com/compute/v1/projects/111222333/global/networks/cicd?alt=json",
			wantEnv: []string{"//pubsub.googleapis.com/projects/acme-cicd/topics/events", "//compute.googleapis.com/projects/acme-cicd/global/networks/cicd"},
		},
		{
			name:    "bigquery project dataset",
			raw:     "analytics-prod.gold_layer.orders",
			wantEnv: []string{"analytics-prod.gold_layer"},
		},
		{
			name: "raw secrets dropped",
			raw:  "sk-live-secret-token-99999 password=hunter2 api.example.com",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envRefs, secRefs := ExtractSanitizedRefs(tc.raw, projIdx)
			for _, w := range tc.wantEnv {
				if !slices.Contains(envRefs, w) {
					t.Errorf("missing envRef %q in %v", w, envRefs)
				}
			}
			if len(envRefs) != len(tc.wantEnv) {
				t.Errorf("envRefs = %v, want %v", envRefs, tc.wantEnv)
			}
			for _, w := range tc.wantSec {
				if !slices.Contains(secRefs, w) {
					t.Errorf("missing secRef %q in %v", w, secRefs)
				}
			}
			if len(secRefs) != len(tc.wantSec) {
				t.Errorf("secRefs = %v, want %v", secRefs, tc.wantSec)
			}
		})
	}
}

func TestProjectAttributesAllowlistAndZeroSensitiveLeakage(t *testing.T) {
	projIdx := map[string]string{"111": "acme-cicd"}

	// Inject EVERY sensitive key plus last-applied-configuration and webhookConfig into payloads.
	poisonPayload := map[string]any{
		"startup-script":            "#!/bin/bash\nexport SECRET=1",
		"startup-script-url":        "https://evil/script.sh",
		"shutdown-script":           "rm -rf /",
		"user-data":                 "cloud-init-secret",
		"env":                       []any{map[string]any{"name": "DB_PASS", "value": "hunter2"}},
		"envVariables":              map[string]any{"TOKEN": "secret-tok"},
		"secretEnv":                 []any{"MY_SECRET"},
		"environmentVariables":      map[string]any{"API_KEY": "sk-123"},
		"buildEnvironmentVariables": map[string]any{"NPM_TOKEN": "npm-123"},
		"args":                      []any{"--password=hunter2"},
		"command":                   []any{"/bin/sh", "-c", "echo secret"},
		"script":                    "export SECRET=abc",
		"data":                      map[string]any{"db.properties": "password=hunter2"},
		"binaryData":                map[string]any{"cert": "base64secret"},
		"stringData":                map[string]any{"pass": "hunter2"},
		"substitutions":             map[string]any{"_TOKEN": "tok"},
		"query":                     "SELECT ssn FROM users",
		"rawQuery":                  "SELECT * FROM secrets",
		"schema":                    map[string]any{"fields": []any{map[string]any{"name": "ssn"}}},
		"webhookConfig":             map[string]any{"caBundle": "CERTDATA"},
		"metadata": map[string]any{
			"namespace": "sonarqube",
			"annotations": map[string]any{
				"kubectl.kubernetes.io/last-applied-configuration": `{"secret":"hunter2"}`,
			},
			"items": []any{
				map[string]any{"key": "startup-script", "value": "export SECRET=1"},
			},
		},
	}

	sampleTypes := []struct {
		assetType string
		extra     map[string]any
		wantKey   string
	}{
		{"compute.googleapis.com/Network", map[string]any{"routingConfig": map[string]any{"routingMode": "REGIONAL"}, "autoCreateSubnetworks": false}, "routing_mode"},
		{"compute.googleapis.com/Subnetwork", map[string]any{"ipCidrRange": "10.10.0.0/20", "privateIpGoogleAccess": true, "network": "projects/111/global/networks/cicd"}, "cidr"},
		{"compute.googleapis.com/Instance", map[string]any{"machineType": "zones/eu/machineTypes/e2-standard-4", "zone": "zones/europe-southwest1-b", "networkInterfaces": []any{map[string]any{"networkIP": "10.10.0.9", "network": "projects/111/global/networks/cicd"}}}, "machine_type"},
		{"compute.googleapis.com/InstanceGroupManager", map[string]any{"targetSize": 3, "baseInstanceName": "web"}, "target_size"},
		{"compute.googleapis.com/ForwardingRule", map[string]any{"IPAddress": "34.1.2.3", "portRange": "443", "IPProtocol": "TCP", "loadBalancingScheme": "EXTERNAL", "ports": []any{"443"}}, "load_balancing_scheme"},
		{"compute.googleapis.com/Router", map[string]any{"network": "projects/111/global/networks/cicd", "nats": []any{map[string]any{"name": "nat1"}}, "bgp": map[string]any{"asn": 64512}}, "nat_count"},
		{"compute.googleapis.com/VpnGateway", map[string]any{"network": "projects/111/global/networks/cicd"}, "vpc"},
		{"compute.googleapis.com/InterconnectAttachment", map[string]any{"type": "DEDICATED", "bandwidth": "BPS_10G", "vlanTag8021q": 100}, "vlan_tag"},
		{"compute.googleapis.com/ServiceAttachment", map[string]any{"connectionPreference": "ACCEPT_AUTOMATIC", "enableProxyProtocol": true}, "connection_preference"},
		{"container.googleapis.com/Cluster", map[string]any{"autopilot": map[string]any{"enabled": true}, "privateClusterConfig": map[string]any{"enablePrivateNodes": true}, "releaseChannel": map[string]any{"channel": "REGULAR"}, "networkConfig": map[string]any{"network": "cicd"}}, "mode"},
		{"apps.k8s.io/Deployment", map[string]any{"spec": map[string]any{"replicas": 2, "template": map[string]any{"spec": map[string]any{"serviceAccountName": "sonar-ksa"}}}}, "replicas"},
		{"batch.k8s.io/CronJob", map[string]any{"spec": map[string]any{"schedule": "0 * * * *", "jobTemplate": map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"serviceAccountName": "cron-ksa"}}}}}}, "schedule"},
		{"batch.k8s.io/Job", map[string]any{"spec": map[string]any{"completions": 1}}, "completions"},
		{"k8s.io/Service", map[string]any{"spec": map[string]any{"type": "ClusterIP", "clusterIP": "10.0.0.1", "ports": []any{map[string]any{"port": 80}}}}, "service_type"},
		{"networking.k8s.io/Ingress", map[string]any{"spec": map[string]any{"ingressClassName": "gce"}}, "ingress_class"},
		{"sqladmin.googleapis.com/Instance", map[string]any{"databaseVersion": "POSTGRES_16", "settings": map[string]any{"tier": "db-custom-2-7680", "ipConfiguration": map[string]any{"privateNetwork": "projects/111/global/networks/cicd", "ipv4Enabled": false}}, "ipAddresses": []any{map[string]any{"type": "PRIVATE", "ipAddress": "10.100.0.5"}}}, "database_version"},
		{"alloydb.googleapis.com/Cluster", map[string]any{"databaseVersion": "POSTGRES_15", "network": "projects/111/global/networks/cicd"}, "database_version"},
		{"spanner.googleapis.com/Instance", map[string]any{"nodeCount": 2, "config": "projects/111/instanceConfigs/regional-eu"}, "node_count"},
		{"firestore.googleapis.com/Database", map[string]any{"type": "FIRESTORE_NATIVE", "concurrencyMode": "OPTIMISTIC"}, "database_type"},
		{"bigtableadmin.googleapis.com/Instance", map[string]any{"type": "PRODUCTION"}, "instance_type"},
		{"redis.googleapis.com/Instance", map[string]any{"tier": "STANDARD_HA", "memorySizeGb": 4, "host": "10.100.1.5", "port": 6379}, "tier"},
		{"redis.googleapis.com/Cluster", map[string]any{"shardCount": 3, "nodeType": "REDIS_STANDARD_SMALL", "pscConfigs": []any{map[string]any{"network": "projects/111/global/networks/cicd"}}}, "shard_count"},
		{"memcache.googleapis.com/Instance", map[string]any{"nodeCount": 2, "nodeConfig": map[string]any{"memorySizeMb": 1024}}, "node_count"},
		{"storage.googleapis.com/Bucket", map[string]any{"storageClass": "STANDARD", "iamConfiguration": map[string]any{"uniformBucketLevelAccess": map[string]any{"enabled": true}}}, "storage_class"},
		{"file.googleapis.com/Instance", map[string]any{"tier": "BASIC_HDD", "networks": []any{map[string]any{"network": "cicd", "ipAddresses": []any{"10.100.2.5"}}}}, "tier"},
		{"netapp.googleapis.com/StoragePool", map[string]any{"serviceLevel": "PREMIUM", "capacityGib": 2048}, "service_level"},
		{"bigquery.googleapis.com/Dataset", map[string]any{"location": "EU"}, "location"},
		{"bigqueryconnection.googleapis.com/Connection", map[string]any{"cloudSql": map[string]any{"instanceId": "p:r:i"}}, "connection_type"},
		{"bigquerydatatransfer.googleapis.com/TransferConfig", map[string]any{"dataSourceId": "scheduled_query", "schedule": "every 24 hours"}, "data_source_id"},
		{"artifactregistry.googleapis.com/Repository", map[string]any{"format": "DOCKER", "mode": "STANDARD_REPOSITORY"}, "format"},
		{"cloudbuild.googleapis.com/BuildTrigger", map[string]any{"github": map[string]any{"owner": "acme-corp", "name": "platform-services", "push": map[string]any{"branch": "^main$"}}, "filename": "cloudbuild.yaml"}, "repo_url"},
		{"clouddeploy.googleapis.com/DeliveryPipeline", map[string]any{"suspended": true}, "suspended"},
		{"run.googleapis.com/Service", map[string]any{"ingress": "INGRESS_TRAFFIC_ALL", "uri": "https://u:p@svc.run.app?q=1"}, "ingress"},
		{"run.googleapis.com/Job", map[string]any{"template": map[string]any{"template": map[string]any{"serviceAccount": "job@acme-cicd.iam.gserviceaccount.com"}}}, "service_account"},
		{"cloudfunctions.googleapis.com/CloudFunction", map[string]any{"buildConfig": map[string]any{"runtime": "go122", "entryPoint": "Handle"}}, "runtime"},
		{"appengine.googleapis.com/Service", map[string]any{"serviceAccount": "ae@acme-cicd.iam.gserviceaccount.com"}, "service_account"},
		{"batch.googleapis.com/Job", map[string]any{"allocationPolicy": map[string]any{"serviceAccount": map[string]any{"email": "batch@acme-cicd.iam.gserviceaccount.com"}}}, "service_account"},
		{"pubsub.googleapis.com/Topic", map[string]any{"kmsKeyName": "projects/111/locations/eu/keyRings/kr/cryptoKeys/k1"}, "kms_key_uri"},
		{"pubsub.googleapis.com/Subscription", map[string]any{"pushConfig": map[string]any{"pushEndpoint": "https://u:p@svc.run.app/push?tok=secret"}, "ackDeadlineSeconds": 30}, "push_endpoint"},
		{"managedkafka.googleapis.com/Cluster", map[string]any{"capacityConfig": map[string]any{"vcpuCount": 3}}, "vcpu_count"},
		{"eventarc.googleapis.com/Trigger", map[string]any{"serviceAccount": "ea@acme-cicd.iam.gserviceaccount.com", "eventFilters": []any{map[string]any{"attribute": "type", "value": "google.cloud.audit.log.v1.written"}}}, "event_type"},
		{"cloudtasks.googleapis.com/Queue", map[string]any{"state": "RUNNING"}, "state"},
		{"cloudscheduler.googleapis.com/Job", map[string]any{"schedule": "*/5 * * * *", "state": "ENABLED"}, "schedule"},
		{"workflows.googleapis.com/Workflow", map[string]any{"state": "ACTIVE"}, "state"},
		{"dataflow.googleapis.com/Job", map[string]any{"type": "JOB_TYPE_STREAMING", "currentState": "JOB_STATE_RUNNING"}, "job_type"},
		{"datastream.googleapis.com/Stream", map[string]any{"state": "RUNNING"}, "state"},
		{"dataproc.googleapis.com/Cluster", map[string]any{"config": map[string]any{"gceClusterConfig": map[string]any{"serviceAccount": "dp@acme-cicd.iam.gserviceaccount.com"}}}, "service_account"},
		{"dataproc.googleapis.com/Batch", map[string]any{"environmentConfig": map[string]any{"executionConfig": map[string]any{"serviceAccount": "dp@acme-cicd.iam.gserviceaccount.com"}}}, "service_account"},
		{"metastore.googleapis.com/Service", map[string]any{"tier": "DEVELOPER"}, "tier"},
		{"composer.googleapis.com/Environment", map[string]any{"config": map[string]any{"softwareConfig": map[string]any{"imageVersion": "composer-2"}}}, "image_version"},
		{"dataform.googleapis.com/Repository", map[string]any{"gitRemoteSettings": map[string]any{"url": "https://tok:x@github.com/org/repo?a=1"}}, "repo_url"},
		{"dataplex.googleapis.com/Lake", map[string]any{"state": "ACTIVE"}, "state"},
		{"datafusion.googleapis.com/Instance", map[string]any{"type": "BASIC", "version": "6.10"}, "version"},
		{"looker.googleapis.com/Instance", map[string]any{"platformEdition": "LOOKER_CORE_STANDARD"}, "platform_edition"},
		{"dns.googleapis.com/ManagedZone", map[string]any{"dnsName": "corp.internal.", "visibility": "private"}, "visibility"},
		{"vpcaccess.googleapis.com/Connector", map[string]any{"ipCidrRange": "10.8.0.0/28", "machineType": "e2-micro"}, "cidr"},
		{"apigateway.googleapis.com/Gateway", map[string]any{"defaultHostname": "gw.apigateway.dev", "state": "ACTIVE"}, "default_hostname"},
		{"apigee.googleapis.com/Organization", map[string]any{"runtimeType": "CLOUD"}, "runtime_type"},
		{"aiplatform.googleapis.com/Endpoint", map[string]any{"deployedModels": []any{map[string]any{"id": "1"}}}, "deployed_model_count"},
		{"aiplatform.googleapis.com/IndexEndpoint", map[string]any{"deployedIndexes": []any{map[string]any{"id": "1"}}}, "deployed_index_count"},
		{"aiplatform.googleapis.com/ReasoningEngine", map[string]any{"spec": map[string]any{"serviceAccount": "ag@acme-cicd.iam.gserviceaccount.com"}}, "service_account"},
		{"aiplatform.googleapis.com/FeatureOnlineStore", map[string]any{"state": "STABLE"}, "state"},
		{"discoveryengine.googleapis.com/Engine", map[string]any{"solutionType": "SOLUTION_TYPE_SEARCH"}, "solution_type"},
		{"discoveryengine.googleapis.com/DataStore", map[string]any{"industryVertical": "GENERIC"}, "industry_vertical"},
		{"dialogflow.googleapis.com/Agent", map[string]any{"defaultLanguageCode": "en"}, "default_language_code"},
		{"documentai.googleapis.com/Processor", map[string]any{"type": "OCR_PROCESSOR", "state": "ENABLED"}, "processor_type"},
		{"iam.googleapis.com/ServiceAccount", map[string]any{"email": "atlantis@acme-cicd.iam.gserviceaccount.com"}, "email"},
		{"secretmanager.googleapis.com/Secret", map[string]any{"replication": map[string]any{"automatic": map[string]any{}}}, "replication"},
		{"cloudkms.googleapis.com/CryptoKey", map[string]any{"purpose": "ENCRYPT_DECRYPT"}, "purpose"},
		{"logging.googleapis.com/LogBucket", map[string]any{"retentionDays": 30}, "retention_days"},
		{"monitoring.googleapis.com/AlertPolicy", map[string]any{"enabled": true}, "enabled"},
	}

	for _, tc := range sampleTypes {
		merged := make(map[string]any, len(poisonPayload)+len(tc.extra))
		for k, v := range poisonPayload {
			merged[k] = v
		}
		for k, v := range tc.extra {
			merged[k] = v
		}
		attrs := ProjectAttributes(tc.assetType, mustStruct(t, merged), "acme-cicd", projIdx)
		if _, ok := attrs[tc.wantKey]; !ok {
			t.Errorf("%s: missing expected allowlisted attr %q in %v", tc.assetType, tc.wantKey, attrs)
		}
		rawJSON, _ := json.Marshal(attrs)
		s := strings.ToLower(string(rawJSON))
		for _, forbidden := range []string{"hunter2", "secret-tok", "sk-123", "npm-123", "certdata", "ssn", "last-applied-configuration", "webhookconfig", "startup-script"} {
			if strings.Contains(s, forbidden) {
				t.Errorf("%s: sensitive value %q leaked into attributes: %s", tc.assetType, forbidden, rawJSON)
			}
		}
		for k := range attrs {
			if IsSensitiveKey(k) {
				t.Errorf("%s: sensitive key %q appeared in allowlisted attrs", tc.assetType, k)
			}
		}
	}

	// Unknown asset type returns nil (never forwards raw payload).
	if got := ProjectAttributes("custom.example.com/Widget", mustStruct(t, poisonPayload), "acme-cicd", projIdx); got != nil {
		t.Fatalf("unknown asset type must return nil attributes, got %v", got)
	}
	if !IsPlumbingAssetType("admissionregistration.k8s.io/ValidatingWebhookConfiguration") ||
		!IsPlumbingAssetType("gateway.networking.k8s.io/HTTPRoute") ||
		!IsPlumbingAssetType("sqladmin.googleapis.com/BackupRun") ||
		!IsPlumbingAssetType("cloudbuild.googleapis.com/Build") ||
		!IsPSATenantProject("p1234567890abcdef-tp") ||
		IsPSATenantProject("acme-cicd") {
		t.Fatal("plumbing or PSA tenant classifier mismatch")
	}
}

// --- Full End-to-End GKE, Cloud Build Min-Heap, Structural, Private-IP, IAM & Enricher Tests ---

func TestExtractGKECloudBuildPrivateIPAndPSA(t *testing.T) {
	pinnedTime := time.Date(2026, 11, 23, 2, 0, 0, 0, time.UTC)
	var recordedReqs []*assetpb.ListAssetsRequest
	var reqMu sync.Mutex

	assetHandler := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		reqMu.Lock()
		recordedReqs = append(recordedReqs, req)
		reqMu.Unlock()

		if req.ContentType == assetpb.ContentType_ACCESS_POLICY {
			t.Error("ACCESS_POLICY must never be requested in Stage 1")
		}

		if req.ContentType == assetpb.ContentType_RESOURCE {
			if req.PageToken == "" {
				// Page 1: K8s workloads, ConfigMaps, KSAs, Services, Ingresses, Builds arrive BEFORE Project asset!
				return &assetpb.ListAssetsResponse{
					ReadTime:      timestamppb.New(pinnedTime),
					NextPageToken: "p2",
					Assets: []*assetpb.Asset{
						// System namespace objects -> suppressed.
						{
							Name:      "//container.googleapis.com/projects/100200300401/zones/europe-southwest1-b/clusters/cicd/k8s/namespaces/kube-system/apps/deployments/kube-dns",
							AssetType: "apps.k8s.io/Deployment",
							Ancestors: []string{"projects/100200300401"},
							Resource:  &assetpb.Resource{Data: mustStruct(t, map[string]any{"metadata": map[string]any{"name": "kube-dns", "namespace": "kube-system"}})},
						},
						{
							Name:      "//container.googleapis.com/projects/100200300401/zones/europe-southwest1-b/clusters/cicd/k8s/namespaces/gmp-system/apps/deployments/collector",
							AssetType: "apps.k8s.io/Deployment",
							Ancestors: []string{"projects/100200300401"},
							Resource:  &assetpb.Resource{Data: mustStruct(t, map[string]any{"metadata": map[string]any{"name": "collector", "namespace": "gmp-system"}})},
						},
						// K8s Namespace & NodePool -> folded counts & suppressed.
						{
							Name:      "//container.googleapis.com/projects/100200300401/zones/europe-southwest1-b/clusters/cicd/k8s/namespaces/sonarqube",
							AssetType: "k8s.io/Namespace",
							Ancestors: []string{"projects/100200300401"},
						},
						{
							Name:      "//container.googleapis.com/projects/100200300401/zones/europe-southwest1-b/clusters/cicd/nodePools/default-pool",
							AssetType: "container.googleapis.com/NodePool",
							Ancestors: []string{"projects/100200300401"},
							Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
								"instanceGroupUrls": []any{"https://www.googleapis.com/compute/v1/projects/100200300401/zones/europe-southwest1-b/instanceGroups/gke-cicd-default-pool-grp"},
							})},
						},
						// K8s ServiceAccounts with Workload Identity annotation -> indexed & suppressed.
						{
							Name:      "//container.googleapis.com/projects/100200300401/zones/europe-southwest1-b/clusters/cicd/k8s/namespaces/sonarqube/serviceaccounts/sonarqube-ksa",
							AssetType: "k8s.io/ServiceAccount",
							Ancestors: []string{"projects/100200300401"},
							Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
								"metadata": map[string]any{
									"name":        "sonarqube-ksa",
									"namespace":   "sonarqube",
									"annotations": map[string]any{"iam.gke.io/gcp-service-account": "sonarqube@acme-cicd.iam.gserviceaccount.com"},
								},
							})},
						},
						{
							Name:      "//container.googleapis.com/projects/100200300401/zones/europe-southwest1-b/clusters/cicd/k8s/namespaces/ops/serviceaccounts/nightly-ksa",
							AssetType: "k8s.io/ServiceAccount",
							Ancestors: []string{"projects/100200300401"},
							Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
								"metadata": map[string]any{
									"name":        "nightly-ksa",
									"namespace":   "ops",
									"annotations": map[string]any{"iam.gke.io/gcp-service-account": "sonarqube@acme-cicd.iam.gserviceaccount.com"},
								},
							})},
						},
						// K8s ConfigMap with JDBC private IP + secret -> scanned before suppressing!
						{
							Name:      "//container.googleapis.com/projects/100200300401/zones/europe-southwest1-b/clusters/cicd/k8s/namespaces/sonarqube/configmaps/sonar-cfg",
							AssetType: "k8s.io/ConfigMap",
							Ancestors: []string{"projects/100200300401"},
							Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
								"metadata": map[string]any{"name": "sonar-cfg", "namespace": "sonarqube"},
								"data": map[string]any{
									"SONAR_JDBC_URL": "jdbc:postgresql://sonar:supersecret@10.100.0.5:5432/sonar",
									"ARCHIVE_BUCKET": "gs://acme-tfstate/sonar",
								},
							})},
						},
						// SonarQube Deployment referencing ConfigMap & initContainer args.
						{
							Name:      "//container.googleapis.com/projects/100200300401/zones/europe-southwest1-b/clusters/cicd/k8s/namespaces/sonarqube/apps/deployments/sonarqube",
							AssetType: "apps.k8s.io/Deployment",
							Ancestors: []string{"projects/100200300401"},
							Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
								"metadata": map[string]any{
									"name":        "sonarqube",
									"namespace":   "sonarqube",
									"annotations": map[string]any{"kubectl.kubernetes.io/last-applied-configuration": `{"secret":"drop-me"}`},
								},
								"spec": map[string]any{
									"replicas": 1,
									"template": map[string]any{
										"metadata": map[string]any{"labels": map[string]any{"app": "sonarqube"}},
										"spec": map[string]any{
											"serviceAccountName": "sonarqube-ksa",
											"initContainers": []any{
												map[string]any{"command": []any{"/bin/sh", "-c", "gsutil cp gs://acme-private-modules/init.sh /tmp/init.sh"}},
											},
											"containers": []any{
												map[string]any{
													"envFrom": []any{map[string]any{"configMapRef": map[string]any{"name": "sonar-cfg"}}},
												},
											},
										},
									},
								},
							})},
						},
						// CronJob with jobTemplate pod labels + Workload Identity.
						{
							Name:      "//container.googleapis.com/projects/100200300401/zones/europe-southwest1-b/clusters/cicd/k8s/namespaces/ops/batch/cronjobs/nightly-scan",
							AssetType: "batch.k8s.io/CronJob",
							Ancestors: []string{"projects/100200300401"},
							Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
								"metadata": map[string]any{"name": "nightly-scan", "namespace": "ops"},
								"spec": map[string]any{
									"schedule": "0 2 * * *",
									"jobTemplate": map[string]any{
										"spec": map[string]any{
											"template": map[string]any{
												"metadata": map[string]any{"labels": map[string]any{"job": "nightly"}},
												"spec":     map[string]any{"serviceAccountName": "nightly-ksa"},
											},
										},
									},
								},
							})},
						},
						// Service with NEG status & selector for sonarqube.
						{
							Name:      "//container.googleapis.com/projects/100200300401/zones/europe-southwest1-b/clusters/cicd/k8s/namespaces/sonarqube/services/sonarqube",
							AssetType: "k8s.io/Service",
							Ancestors: []string{"projects/100200300401"},
							Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
								"metadata": map[string]any{
									"name":      "sonarqube",
									"namespace": "sonarqube",
									"annotations": map[string]any{
										"cloud.google.com/neg-status": `{"network_endpoint_groups":{"9000":"k8s1-neg-sonarqube-9000"},"zones":["europe-southwest1-b"]}`,
									},
								},
								"spec": map[string]any{
									"type":     "ClusterIP",
									"selector": map[string]any{"app": "sonarqube"},
									"ports":    []any{map[string]any{"port": 9000}},
								},
							})},
						},
						// Headless Service with EMPTY selector -> must NOT match any workload!
						{
							Name:      "//container.googleapis.com/projects/100200300401/zones/europe-southwest1-b/clusters/cicd/k8s/namespaces/sonarqube/services/external-headless",
							AssetType: "k8s.io/Service",
							Ancestors: []string{"projects/100200300401"},
							Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
								"metadata": map[string]any{"name": "external-headless", "namespace": "sonarqube"},
								"spec":     map[string]any{"type": "ClusterIP", "selector": map[string]any{}},
							})},
						},
						// L4 LoadBalancer Service in ops namespace selecting CronJob pod labels.
						{
							Name:      "//container.googleapis.com/projects/100200300401/zones/europe-southwest1-b/clusters/cicd/k8s/namespaces/ops/services/ops-lb",
							AssetType: "k8s.io/Service",
							Ancestors: []string{"projects/100200300401"},
							Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
								"metadata": map[string]any{"name": "ops-lb", "namespace": "ops"},
								"spec":     map[string]any{"type": "LoadBalancer", "selector": map[string]any{"job": "nightly"}},
								"status":   map[string]any{"loadBalancer": map[string]any{"ingress": []any{map[string]any{"ip": "34.175.10.20"}}}},
							})},
						},
						// Ingress for sonarqube.
						{
							Name:      "//container.googleapis.com/projects/100200300401/zones/europe-southwest1-b/clusters/cicd/k8s/namespaces/sonarqube/networking.k8s.io/ingresses/sonarqube",
							AssetType: "networking.k8s.io/Ingress",
							Ancestors: []string{"projects/100200300401"},
							Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
								"metadata": map[string]any{
									"name":        "sonarqube",
									"namespace":   "sonarqube",
									"annotations": map[string]any{"ingress.kubernetes.io/https-forwarding-rule": "k8s2-fs-sonarqube"},
								},
								"spec": map[string]any{
									"rules": []any{map[string]any{
										"host": "sonarqube.cicd.example.com",
										"http": map[string]any{
											"paths": []any{map[string]any{
												"path":    "/*",
												"backend": map[string]any{"service": map[string]any{"name": "sonarqube"}},
											}},
										},
									}},
								},
							})},
						},
						// 5 Cloud Build Builds (4 SUCCESS, 1 FAILURE) -> top-2 min-heap keeps ONLY b3 & b4!
						{
							Name:      "//cloudbuild.googleapis.com/projects/100200300401/locations/europe-southwest1/builds/b1-old",
							AssetType: "cloudbuild.googleapis.com/Build",
							Ancestors: []string{"projects/100200300401"},
							Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
								"status":         "SUCCESS",
								"buildTriggerId": "trig-api",
								"createTime":     "2026-11-01T00:00:00Z",
								"steps":          []any{map[string]any{"script": "gsutil cp a gs://old-evicted-bucket/x"}},
							})},
						},
						{
							Name:      "//cloudbuild.googleapis.com/projects/100200300401/locations/europe-southwest1/builds/b2-old",
							AssetType: "cloudbuild.googleapis.com/Build",
							Ancestors: []string{"projects/100200300401"},
							Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
								"status":         "SUCCESS",
								"buildTriggerId": "trig-api",
								"createTime":     "2026-11-02T00:00:00Z",
								"steps":          []any{map[string]any{"script": "gsutil cp a gs://old-evicted-bucket/y"}},
							})},
						},
						{
							Name:      "//cloudbuild.googleapis.com/projects/100200300401/locations/europe-southwest1/builds/b3-new",
							AssetType: "cloudbuild.googleapis.com/Build",
							Ancestors: []string{"projects/100200300401"},
							Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
								"status":         "SUCCESS",
								"buildTriggerId": "trig-api",
								"createTime":     "2026-11-20T00:00:00Z",
								"images":         []any{"europe-southwest1-docker.pkg.dev/100200300401/acme-apps/api:v3"},
								"steps": []any{
									map[string]any{"name": "gcr.io/cloud-builders/docker", "args": []any{"build", "."}},
									map[string]any{"name": "gcr.io/google.com/cloudsdktool/cloud-sdk", "entrypoint": "gcloud", "args": []any{"run", "deploy", "api-svc", "--project=acme-prod"}},
								},
							})},
						},
						{
							Name:      "//cloudbuild.googleapis.com/projects/100200300401/locations/europe-southwest1/builds/b4-newest",
							AssetType: "cloudbuild.googleapis.com/Build",
							Ancestors: []string{"projects/100200300401"},
							Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
								"status":         "SUCCESS",
								"buildTriggerId": "trig-api",
								"createTime":     "2026-11-22T00:00:00Z",
								"artifacts":      map[string]any{"objects": map[string]any{"location": "gs://acme-tfstate/releases"}},
								"steps":          []any{map[string]any{"script": "docker push gcr.io/distroless/static && docker push gcr.io/legacy-target/app:v1"}},
							})},
						},
						{
							Name:      "//cloudbuild.googleapis.com/projects/100200300401/locations/europe-southwest1/builds/b5-failed",
							AssetType: "cloudbuild.googleapis.com/Build",
							Ancestors: []string{"projects/100200300401"},
							Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
								"status":         "FAILURE",
								"buildTriggerId": "trig-api",
								"createTime":     "2026-11-23T00:00:00Z",
								"steps":          []any{map[string]any{"script": "gcloud run deploy broken --project=failed-proj"}},
							})},
						},
						// Triggerless build -> attached to project URI.
						{
							Name:      "//cloudbuild.googleapis.com/projects/100200300401/locations/europe-southwest1/builds/b-manual",
							AssetType: "cloudbuild.googleapis.com/Build",
							Ancestors: []string{"projects/100200300401"},
							Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
								"status":     "SUCCESS",
								"createTime": "2026-11-21T00:00:00Z",
								"steps":      []any{map[string]any{"script": "gsutil ls gs://acme-tfstate"}},
							})},
						},
					},
				}, nil
			}
			// Page 2: Project, VPC, Subnet, GKE Cluster, LB chain, Cloud SQL, Buckets, AR, BuildTrigger, GKE worker VM/MIG, Default sandbox assets.
			return &assetpb.ListAssetsResponse{
				ReadTime: timestamppb.New(pinnedTime),
				Assets: []*assetpb.Asset{
					{
						Name:      "//cloudresourcemanager.googleapis.com/projects/100200300401",
						AssetType: "cloudresourcemanager.googleapis.com/Project",
						Ancestors: []string{"projects/100200300401", "organizations/999"},
						Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
							"projectId":     "acme-cicd",
							"projectNumber": "100200300401",
							"name":          "Acme Platform CICD",
						})},
					},
					{
						Name:      "//compute.googleapis.com/projects/100200300401/locations/global/networks/cicd",
						AssetType: "compute.googleapis.com/Network",
						Ancestors: []string{"projects/100200300401"},
						Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
							"name":                  "cicd",
							"autoCreateSubnetworks": false,
							"peerings": []any{
								map[string]any{
									"name":    "servicenetworking-googleapis-com",
									"network": "https://www.googleapis.com/compute/v1/projects/p1234567890abcdef-tp/global/networks/servicenetworking",
									"state":   "ACTIVE",
								},
							},
						})},
					},
					{
						Name:      "//compute.googleapis.com/projects/100200300401/regions/europe-southwest1/subnetworks/cicd-k8s",
						AssetType: "compute.googleapis.com/Subnetwork",
						Ancestors: []string{"projects/100200300401"},
						Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
							"name":        "cicd-k8s",
							"ipCidrRange": "10.10.0.0/20",
							"network":     "projects/100200300401/locations/global/networks/cicd",
						})},
					},
					// Unused default VPC + subnet + _Default LogBucket + unreferenced default SA -> all pruned!
					{
						Name:      "//compute.googleapis.com/projects/100200300401/global/networks/default",
						AssetType: "compute.googleapis.com/Network",
						Ancestors: []string{"projects/100200300401"},
						Resource:  &assetpb.Resource{Data: mustStruct(t, map[string]any{"name": "default", "autoCreateSubnetworks": true})},
					},
					{
						Name:      "//compute.googleapis.com/projects/100200300401/regions/europe-southwest1/subnetworks/default",
						AssetType: "compute.googleapis.com/Subnetwork",
						Ancestors: []string{"projects/100200300401"},
						Resource:  &assetpb.Resource{Data: mustStruct(t, map[string]any{"name": "default", "network": "projects/100200300401/global/networks/default"})},
					},
					{
						Name:      "//logging.googleapis.com/projects/100200300401/locations/global/buckets/_Default",
						AssetType: "logging.googleapis.com/LogBucket",
						Ancestors: []string{"projects/100200300401"},
					},
					{
						Name:      "//iam.googleapis.com/projects/100200300401/serviceAccounts/100200300401-compute@developer.gserviceaccount.com",
						AssetType: "iam.googleapis.com/ServiceAccount",
						Ancestors: []string{"projects/100200300401"},
						Resource:  &assetpb.Resource{Data: mustStruct(t, map[string]any{"email": "100200300401-compute@developer.gserviceaccount.com"})},
					},
					// GKE Cluster + GKE MIG & Worker VM (to be suppressed).
					{
						Name:      "//container.googleapis.com/projects/100200300401/zones/europe-southwest1-b/clusters/cicd",
						AssetType: "container.googleapis.com/Cluster",
						Ancestors: []string{"projects/100200300401"},
						Resource: &assetpb.Resource{Location: "europe-southwest1-b", Data: mustStruct(t, map[string]any{
							"name":          "cicd",
							"networkConfig": map[string]any{"network": "projects/100200300401/global/networks/cicd", "subnetwork": "projects/100200300401/regions/europe-southwest1/subnetworks/cicd-k8s"},
						})},
					},
					{
						Name:      "//compute.googleapis.com/projects/100200300401/zones/europe-southwest1-b/instanceGroupManagers/gke-cicd-default-pool-grp",
						AssetType: "compute.googleapis.com/InstanceGroupManager",
						Ancestors: []string{"projects/100200300401"},
					},
					{
						Name:      "//compute.googleapis.com/projects/100200300401/zones/europe-southwest1-b/instances/gke-cicd-default-pool-abc1",
						AssetType: "compute.googleapis.com/Instance",
						Ancestors: []string{"projects/100200300401"},
						Resource:  &assetpb.Resource{Data: mustStruct(t, map[string]any{"name": "gke-cicd-default-pool-abc1"})},
					},
					// LB chain for SonarQube NEG + L4 ForwardingRule for ops-lb.
					{
						Name:      "//compute.googleapis.com/projects/100200300401/global/forwardingRules/k8s2-fs-sonarqube",
						AssetType: "compute.googleapis.com/GlobalForwardingRule",
						Ancestors: []string{"projects/100200300401"},
						Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
							"loadBalancingScheme": "EXTERNAL",
							"IPAddress":           "34.111.222.10",
							"target":              "projects/100200300401/global/targetHttpsProxies/k8s2-ts-sonarqube",
						})},
					},
					{
						Name:      "//compute.googleapis.com/projects/100200300401/global/targetHttpsProxies/k8s2-ts-sonarqube",
						AssetType: "compute.googleapis.com/TargetHttpsProxy",
						Ancestors: []string{"projects/100200300401"},
						Resource:  &assetpb.Resource{Data: mustStruct(t, map[string]any{"urlMap": "projects/100200300401/global/urlMaps/k8s2-um-sonarqube"})},
					},
					{
						Name:      "//compute.googleapis.com/projects/100200300401/global/urlMaps/k8s2-um-sonarqube",
						AssetType: "compute.googleapis.com/UrlMap",
						Ancestors: []string{"projects/100200300401"},
						Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
							"defaultService": "projects/100200300401/global/backendServices/k8s1-bs-sonarqube",
						})},
					},
					{
						Name:      "//compute.googleapis.com/projects/100200300401/global/backendServices/k8s1-bs-sonarqube",
						AssetType: "compute.googleapis.com/BackendService",
						Ancestors: []string{"projects/100200300401"},
						Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
							"backends": []any{map[string]any{"group": "projects/100200300401/zones/europe-southwest1-b/networkEndpointGroups/k8s1-neg-sonarqube-9000"}},
						})},
					},
					{
						Name:      "//compute.googleapis.com/projects/100200300401/zones/europe-southwest1-b/networkEndpointGroups/k8s1-neg-sonarqube-9000",
						AssetType: "compute.googleapis.com/NetworkEndpointGroup",
						Ancestors: []string{"projects/100200300401"},
					},
					{
						Name:      "//compute.googleapis.com/projects/100200300401/regions/europe-southwest1/forwardingRules/l4-ops-fr",
						AssetType: "compute.googleapis.com/ForwardingRule",
						Ancestors: []string{"projects/100200300401"},
						Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
							"loadBalancingScheme": "EXTERNAL",
							"IPAddress":           "34.175.10.20",
						})},
					},
					// Cloud SQL sonarqube on private IP 10.100.0.5 + BackupRun (suppressed).
					{
						Name:      "//sqladmin.googleapis.com/projects/100200300401/instances/sonarqube",
						AssetType: "sqladmin.googleapis.com/Instance",
						Ancestors: []string{"projects/100200300401"},
						Resource: &assetpb.Resource{Location: "europe-southwest1", Data: mustStruct(t, map[string]any{
							"databaseVersion": "POSTGRES_16",
							"ipAddresses":     []any{map[string]any{"type": "PRIVATE", "ipAddress": "10.100.0.5"}},
							"settings": map[string]any{
								"tier":            "db-custom-2-7680",
								"ipConfiguration": map[string]any{"privateNetwork": "projects/100200300401/global/networks/cicd"},
							},
						})},
					},
					{
						Name:      "//sqladmin.googleapis.com/projects/100200300401/instances/sonarqube/backupRuns/1",
						AssetType: "sqladmin.googleapis.com/BackupRun",
						Ancestors: []string{"projects/100200300401"},
					},
					// Buckets, Artifact Registry + DockerImages, ServiceAccount, BuildTrigger.
					{Name: "//storage.googleapis.com/acme-tfstate", AssetType: "storage.googleapis.com/Bucket", Ancestors: []string{"projects/100200300401"}},
					{Name: "//storage.googleapis.com/acme-private-modules", AssetType: "storage.googleapis.com/Bucket", Ancestors: []string{"projects/100200300401"}},
					{Name: "//storage.googleapis.com/old-evicted-bucket", AssetType: "storage.googleapis.com/Bucket", Ancestors: []string{"projects/100200300401"}},
					{
						Name:      "//artifactregistry.googleapis.com/projects/100200300401/locations/europe-southwest1/repositories/acme-apps",
						AssetType: "artifactregistry.googleapis.com/Repository",
						Ancestors: []string{"projects/100200300401"},
						Resource:  &assetpb.Resource{Location: "europe-southwest1", Data: mustStruct(t, map[string]any{"format": "DOCKER"})},
					},
					{
						Name:      "//artifactregistry.googleapis.com/projects/100200300401/locations/europe-southwest1/repositories/acme-apps/dockerImages/api@sha256:1",
						AssetType: "artifactregistry.googleapis.com/DockerImage",
						Ancestors: []string{"projects/100200300401"},
					},
					{
						Name:      "//iam.googleapis.com/projects/100200300401/serviceAccounts/sonarqube@acme-cicd.iam.gserviceaccount.com",
						AssetType: "iam.googleapis.com/ServiceAccount",
						Ancestors: []string{"projects/100200300401"},
						Resource:  &assetpb.Resource{Data: mustStruct(t, map[string]any{"email": "sonarqube@acme-cicd.iam.gserviceaccount.com", "uniqueId": "100001"})},
					},
					{
						Name:      "//cloudbuild.googleapis.com/projects/100200300401/locations/europe-southwest1/triggers/build-api",
						AssetType: "cloudbuild.googleapis.com/BuildTrigger",
						Ancestors: []string{"projects/100200300401"},
						Resource: &assetpb.Resource{Location: "europe-southwest1", Data: mustStruct(t, map[string]any{
							"id":     "trig-api",
							"name":   "build-api",
							"github": map[string]any{"owner": "acme-corp", "name": "platform-services", "push": map[string]any{"branch": "^main$"}},
						})},
					},
				},
			}, nil
		}

		// IAM_POLICY: verify Organization & Folder are in AssetTypes; return broad + specific + resource bindings.
		if req.ContentType == assetpb.ContentType_IAM_POLICY {
			if !slices.Contains(req.AssetTypes, "cloudresourcemanager.googleapis.com/Organization") ||
				!slices.Contains(req.AssetTypes, "cloudresourcemanager.googleapis.com/Folder") {
				t.Errorf("IAM_POLICY AssetTypes missing Organization/Folder: %v", req.AssetTypes)
			}
			return &assetpb.ListAssetsResponse{
				ReadTime: timestamppb.New(pinnedTime),
				Assets: []*assetpb.Asset{
					{
						Name:      "//cloudresourcemanager.googleapis.com/projects/100200300401",
						AssetType: "cloudresourcemanager.googleapis.com/Project",
						Ancestors: []string{"projects/100200300401"},
						IamPolicy: &iampb.Policy{Bindings: []*iampb.Binding{
							{Role: "roles/viewer", Members: []string{"serviceAccount:sonarqube@acme-cicd.iam.gserviceaccount.com"}},
							{Role: "roles/cloudsql.client", Members: []string{"serviceAccount:sonarqube@acme-cicd.iam.gserviceaccount.com"}},
						}},
					},
					{
						Name:      "//storage.googleapis.com/acme-tfstate",
						AssetType: "storage.googleapis.com/Bucket",
						Ancestors: []string{"projects/100200300401"},
						IamPolicy: &iampb.Policy{Bindings: []*iampb.Binding{
							{Role: "roles/storage.objectAdmin", Members: []string{"serviceAccount:sonarqube@acme-cicd.iam.gserviceaccount.com"}},
						}},
					},
				},
			}, nil
		}
		return &assetpb.ListAssetsResponse{}, nil
	}

	clients, _ := newTestClients(t, assetHandler, nil, nil, nil)
	facts, err := Extract(context.Background(), clients, ExtractOptions{
		Scopes:         []string{"projects/acme-cicd"},
		CloudBuildRuns: 2,
	})
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}

	// Verify ReadTime pinning (page 1 unset, page 2 and IAM_POLICY pinned to pinnedTime).
	if len(recordedReqs) != 3 {
		t.Fatalf("expected 3 CAI requests (2 RESOURCE + 1 IAM_POLICY), got %d", len(recordedReqs))
	}
	if recordedReqs[0].ReadTime != nil || !recordedReqs[1].ReadTime.AsTime().Equal(pinnedTime) || !recordedReqs[2].ReadTime.AsTime().Equal(pinnedTime) {
		t.Fatalf("ReadTime pinning failed")
	}

	assertGKECloudBuildEdgesAndPSA(t, facts)
	assertGKECloudBuildAttributesAndSanitization(t, facts)
}

func assertGKECloudBuildEdgesAndPSA(t *testing.T, facts *FactsFile) {
	t.Helper()
	var psaProj *ProjectNode
	for i := range facts.Hierarchy.Projects {
		if facts.Hierarchy.Projects[i].ProjectID == "p1234567890abcdef-tp" {
			psaProj = &facts.Hierarchy.Projects[i]
		}
	}
	if psaProj == nil || !psaProj.IsPSATenant {
		t.Fatalf("expected p1234567890abcdef-tp marked IsPSATenant=true, got %+v", facts.Hierarchy.Projects)
	}

	vpcURI := "//compute.googleapis.com/projects/acme-cicd/global/networks/cicd"
	clusterURI := "//container.googleapis.com/projects/acme-cicd/locations/europe-southwest1-b/clusters/cicd"
	sonarDepURI := clusterURI + "/k8s/namespaces/sonarqube/apps/deployments/sonarqube"
	sonarSvcURI := clusterURI + "/k8s/namespaces/sonarqube/services/sonarqube"
	emptySvcURI := clusterURI + "/k8s/namespaces/sonarqube/services/external-headless"
	sonarIngURI := clusterURI + "/k8s/namespaces/sonarqube/networking.k8s.io/ingresses/sonarqube"
	cronURI := clusterURI + "/k8s/namespaces/ops/batch/cronjobs/nightly-scan"
	opsSvcURI := clusterURI + "/k8s/namespaces/ops/services/ops-lb"
	frSonarURI := "//compute.googleapis.com/projects/acme-cicd/global/forwardingRules/k8s2-fs-sonarqube"
	frOpsURI := "//compute.googleapis.com/projects/acme-cicd/regions/europe-southwest1/forwardingRules/l4-ops-fr"
	sqlURI := "//sqladmin.googleapis.com/projects/acme-cicd/instances/sonarqube"
	saURI := "//iam.googleapis.com/projects/acme-cicd/serviceAccounts/sonarqube@acme-cicd.iam.gserviceaccount.com"
	trigURI := "//cloudbuild.googleapis.com/projects/acme-cicd/locations/europe-southwest1/triggers/build-api"
	arURI := "//artifactregistry.googleapis.com/projects/acme-cicd/locations/europe-southwest1/repositories/acme-apps"
	githubURI := "//github.com/acme-corp/platform-services"
	prodStubURI := "//cloudresourcemanager.googleapis.com/projects/acme-prod"
	legacyStubURI := "//cloudresourcemanager.googleapis.com/projects/legacy-target"

	expectedEdges := []struct {
		from, to string
		kind     FactEdgeKind
	}{
		{frSonarURI, sonarSvcURI, EdgeK8sNEGBackend},
		{sonarIngURI, sonarSvcURI, EdgeK8sIngressBackend},
		{sonarSvcURI, sonarDepURI, EdgeK8sServiceSelector},
		{sonarDepURI, saURI, EdgeWorkloadIdentity},
		{sonarDepURI, sqlURI, EdgeConfigRef}, // private IP 10.100.0.5 from ConfigMap!
		{sonarDepURI, "//storage.googleapis.com/acme-tfstate", EdgeConfigRef},
		{sonarDepURI, "//storage.googleapis.com/acme-private-modules", EdgeConfigRef}, // from initContainers!
		{frOpsURI, opsSvcURI, EdgeK8sIngressBackend},                                  // L4 LB IP!
		{opsSvcURI, cronURI, EdgeK8sServiceSelector},                                  // CronJob!
		{cronURI, saURI, EdgeWorkloadIdentity},                                        // CronJob WIF!
		{vpcURI, sqlURI, EdgePSAPeering},
		{saURI, sqlURI, EdgeIAMResourceBinding}, // unambiguous single Cloud SQL in project!
		{saURI, "//storage.googleapis.com/acme-tfstate", EdgeIAMResourceBinding},
		{githubURI, trigURI, EdgeCloudBuildRepo},
		{trigURI, arURI, EdgeCloudBuildStepRef},
		{trigURI, "//storage.googleapis.com/acme-tfstate", EdgeCloudBuildStepRef},
		{trigURI, prodStubURI, EdgeCloudBuildStepRef},
		{trigURI, legacyStubURI, EdgeCloudBuildStepRef},
		{"//cloudresourcemanager.googleapis.com/projects/acme-cicd", "//storage.googleapis.com/acme-tfstate", EdgeCloudBuildStepRef}, // triggerless build!
	}
	for _, want := range expectedEdges {
		if !hasEdge(facts.Edges, want.from, want.to, want.kind) {
			t.Errorf("missing expected edge %s -[%s]-> %s", want.from, want.kind, want.to)
		}
	}

	if hasEdge(facts.Edges, emptySvcURI, sonarDepURI, EdgeK8sServiceSelector) {
		t.Fatal("empty selector Service falsely emitted K8S_SERVICE_SELECTOR edge")
	}
	if hasEdge(facts.Edges, trigURI, "//storage.googleapis.com/old-evicted-bucket", EdgeCloudBuildStepRef) {
		t.Fatal("evicted old Cloud Build run leaked edge past min-heap")
	}
	if findResource(facts.Resources, "//cloudresourcemanager.googleapis.com/projects/failed-proj") != nil {
		t.Fatal("FAILURE Cloud Build run emitted out-of-scope stub")
	}
	for _, excluded := range []string{"cloud-builders", "distroless", "p1234567890abcdef-tp"} {
		if findResource(facts.Resources, "//cloudresourcemanager.googleapis.com/projects/"+excluded) != nil {
			t.Errorf("excluded project %q emitted external stub", excluded)
		}
	}
}

func assertGKECloudBuildAttributesAndSanitization(t *testing.T, facts *FactsFile) {
	t.Helper()
	frSonarURI := "//compute.googleapis.com/projects/acme-cicd/global/forwardingRules/k8s2-fs-sonarqube"
	saURI := "//iam.googleapis.com/projects/acme-cicd/serviceAccounts/sonarqube@acme-cicd.iam.gserviceaccount.com"
	arURI := "//artifactregistry.googleapis.com/projects/acme-cicd/locations/europe-southwest1/repositories/acme-apps"

	fr := findResource(facts.Resources, frSonarURI)
	if fr == nil || fr.Attributes["tls"] != true {
		t.Fatalf("expected TLS on ForwardingRule, got %+v", fr)
	}
	hosts, _ := fr.Attributes["ingress_hosts"].([]string)
	if !slices.Contains(hosts, "sonarqube.cicd.example.com") {
		t.Fatalf("expected sonarqube.cicd.example.com attached to ForwardingRule, got %v", fr.Attributes)
	}

	saRes := findResource(facts.Resources, saURI)
	if saRes == nil || !slices.Contains(saRes.ProjectCapabilities, "roles/viewer") {
		t.Fatalf("expected roles/viewer on SA ProjectCapabilities, got %+v", saRes)
	}

	arRes := findResource(facts.Resources, arURI)
	if arRes == nil || arRes.Attributes["image_count"] != 1 {
		t.Fatalf("expected image_count=1 on Artifact Registry, got %+v", arRes)
	}
	if facts.SuppressedPlumbingCounts["cloudbuild.googleapis.com/Build"] != 6 {
		t.Fatalf("expected 6 suppressed Builds, got %d", facts.SuppressedPlumbingCounts["cloudbuild.googleapis.com/Build"])
	}
	if facts.SuppressedPlumbingCounts["compute.googleapis.com/Instance"] != 1 ||
		facts.SuppressedPlumbingCounts["compute.googleapis.com/InstanceGroupManager"] != 1 {
		t.Fatalf("expected GKE worker VM & MIG suppressed, got %v", facts.SuppressedPlumbingCounts)
	}
	if findResource(facts.Resources, "//compute.googleapis.com/projects/acme-cicd/global/networks/default") != nil ||
		findResource(facts.Resources, "//logging.googleapis.com/projects/acme-cicd/locations/global/buckets/_Default") != nil ||
		findResource(facts.Resources, "//iam.googleapis.com/projects/acme-cicd/serviceAccounts/100200300401-compute@developer.gserviceaccount.com") != nil {
		t.Fatal("default sandbox assets were not pruned")
	}

	outPath := filepath.Join(t.TempDir(), "facts.json")
	if err := WriteFacts(outPath, facts); err != nil {
		t.Fatalf("WriteFacts: %v", err)
	}
	fi, _ := os.Stat(outPath)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 mode, got %o", fi.Mode().Perm())
	}
	rawBytes, _ := os.ReadFile(outPath)
	rawStr := string(rawBytes)
	for _, secret := range []string{"supersecret", "drop-me", "last-applied-configuration"} {
		if strings.Contains(rawStr, secret) {
			t.Fatalf("secret %q leaked into facts.json", secret)
		}
	}
}

func TestExtractStructuralEdgesAndIAMCondition(t *testing.T) {
	assetHandler := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		if req.ContentType == assetpb.ContentType_RESOURCE {
			return &assetpb.ListAssetsResponse{
				ReadTime: timestamppb.Now(),
				Assets: []*assetpb.Asset{
					{Name: "//cloudresourcemanager.googleapis.com/projects/p1", AssetType: "cloudresourcemanager.googleapis.com/Project", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "p1", "projectNumber": "1001"})}},
					{Name: "//compute.googleapis.com/projects/p1/global/networks/vpc-a", AssetType: "compute.googleapis.com/Network", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"peerings": []any{map[string]any{"name": "peer-ext", "network": "projects/ext-hub/global/networks/hub", "state": "ACTIVE"}},
					})}},
					{Name: "//compute.googleapis.com/projects/p1/regions/us-central1/subnetworks/sub-a", AssetType: "compute.googleapis.com/Subnetwork", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"network": "projects/p1/global/networks/vpc-a"})}},
					{Name: "//vpcaccess.googleapis.com/projects/p1/locations/us-central1/connectors/conn-a", AssetType: "vpcaccess.googleapis.com/Connector", Resource: &assetpb.Resource{Location: "us-central1", Data: mustStruct(t, map[string]any{"subnet": map[string]any{"name": "sub-a"}})}},
					// Cloud Run Service + active Revision + Serverless NEG + ForwardingRule.
					{Name: "//run.googleapis.com/projects/p1/locations/us-central1/revisions/api-0001", AssetType: "run.googleapis.com/Revision", Resource: &assetpb.Resource{Location: "us-central1", Data: mustStruct(t, map[string]any{
						"serviceAccount": "api@p1.iam.gserviceaccount.com",
						"vpcAccess":      map[string]any{"connector": "projects/p1/locations/us-central1/connectors/conn-a"},
						"containers": []any{map[string]any{"env": []any{
							map[string]any{"name": "DB", "value": "p1:us-central1:db-1"},
							map[string]any{"name": "SEC", "valueSource": map[string]any{"secretKeyRef": map[string]any{"secret": "jwt-key"}}},
						}}},
					})}},
					{Name: "//run.googleapis.com/projects/p1/locations/us-central1/services/api", AssetType: "run.googleapis.com/Service", Resource: &assetpb.Resource{Location: "us-central1", Data: mustStruct(t, map[string]any{
						"uri":     "https://api-abcdef-uc.a.run.app",
						"traffic": []any{map[string]any{"revision": "api-0001", "percent": 100}},
					})}},
					{Name: "//compute.googleapis.com/projects/p1/global/forwardingRules/fr-api", AssetType: "compute.googleapis.com/GlobalForwardingRule", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"target": "projects/p1/global/targetHttpProxies/hp-api"})}},
					{Name: "//compute.googleapis.com/projects/p1/global/targetHttpProxies/hp-api", AssetType: "compute.googleapis.com/TargetHttpProxy", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"urlMap": "projects/p1/global/urlMaps/um-api"})}},
					{Name: "//compute.googleapis.com/projects/p1/global/urlMaps/um-api", AssetType: "compute.googleapis.com/UrlMap", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"defaultService": "projects/p1/global/backendServices/bs-api",
						"pathMatchers": []any{map[string]any{
							"pathRules": []any{map[string]any{"paths": []any{"/static/*"}, "service": "projects/p1/global/backendBuckets/bb-static"}},
						}},
					})}},
					{Name: "//compute.googleapis.com/projects/p1/global/backendServices/bs-api", AssetType: "compute.googleapis.com/BackendService", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"backends": []any{map[string]any{"group": "projects/p1/regions/us-central1/networkEndpointGroups/neg-api"}},
					})}},
					{Name: "//compute.googleapis.com/projects/p1/regions/us-central1/networkEndpointGroups/neg-api", AssetType: "compute.googleapis.com/NetworkEndpointGroup", Resource: &assetpb.Resource{Location: "us-central1", Data: mustStruct(t, map[string]any{"cloudRun": map[string]any{"service": "api"}})}},
					{Name: "//compute.googleapis.com/projects/p1/global/backendBuckets/bb-static", AssetType: "compute.googleapis.com/BackendBucket", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"bucketName": "static-bkt"})}},
					// Pub/Sub Topic + Subscription (push to Cloud Run + BQ sink + GCS sink), Scheduler, Eventarc, Router+VPN, Datastream, BQ Dataset/Table/Connection.
					{Name: "//storage.googleapis.com/static-bkt", AssetType: "storage.googleapis.com/Bucket", Ancestors: []string{"projects/p1"}},
					{Name: "//secretmanager.googleapis.com/projects/p1/secrets/jwt-key", AssetType: "secretmanager.googleapis.com/Secret", Ancestors: []string{"projects/p1"}},
					{Name: "//sqladmin.googleapis.com/projects/p1/instances/db-1", AssetType: "sqladmin.googleapis.com/Instance", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"ipAddresses": []any{map[string]any{"type": "PRIVATE", "ipAddress": "10.20.0.5"}}, "settings": map[string]any{"ipConfiguration": map[string]any{"privateNetwork": "projects/p1/global/networks/vpc-a"}}})}},
					{Name: "//sqladmin.googleapis.com/projects/p1/instances/db-2", AssetType: "sqladmin.googleapis.com/Instance", Ancestors: []string{"projects/p1"}},
					{Name: "//bigquery.googleapis.com/projects/p1/datasets/ds1", AssetType: "bigquery.googleapis.com/Dataset", Ancestors: []string{"projects/p1"}},
					{Name: "//bigquery.googleapis.com/projects/p1/datasets/ds1/tables/ext_t", AssetType: "bigquery.googleapis.com/Table", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"externalDataConfiguration": map[string]any{"sourceUris": []any{"gs://static-bkt/*.parquet"}}})}},
					{Name: "//pubsub.googleapis.com/projects/p1/topics/orders", AssetType: "pubsub.googleapis.com/Topic", Ancestors: []string{"projects/p1"}},
					{Name: "//pubsub.googleapis.com/projects/p1/subscriptions/orders-sub", AssetType: "pubsub.googleapis.com/Subscription", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"topic":              "projects/p1/topics/orders",
						"pushConfig":         map[string]any{"pushEndpoint": "https://api-abcdef-uc.a.run.app/hook"},
						"bigqueryConfig":     map[string]any{"table": "p1:ds1.orders"},
						"cloudStorageConfig": map[string]any{"bucket": "static-bkt"},
					})}},
					{Name: "//cloudscheduler.googleapis.com/projects/p1/locations/us-central1/jobs/cron-1", AssetType: "cloudscheduler.googleapis.com/Job", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"pubsubTarget": map[string]any{"topicName": "projects/p1/topics/orders"},
						"httpTarget":   map[string]any{"uri": "https://api-abcdef-uc.a.run.app/tick"},
					})}},
					{Name: "//eventarc.googleapis.com/projects/p1/locations/us-central1/triggers/ea-1", AssetType: "eventarc.googleapis.com/Trigger", Resource: &assetpb.Resource{Location: "us-central1", Data: mustStruct(t, map[string]any{
						"transport":   map[string]any{"pubsub": map[string]any{"topic": "projects/p1/topics/orders"}},
						"destination": map[string]any{"cloudRun": map[string]any{"service": "api", "region": "us-central1"}},
					})}},
					{Name: "//compute.googleapis.com/projects/p1/regions/us-central1/routers/rtr-1", AssetType: "compute.googleapis.com/Router", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"network": "projects/p1/global/networks/vpc-a"})}},
					{Name: "//compute.googleapis.com/projects/p1/regions/us-central1/vpnGateways/vgw-1", AssetType: "compute.googleapis.com/VpnGateway", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"network": "projects/p1/global/networks/vpc-a"})}},
					{Name: "//compute.googleapis.com/projects/p1/regions/us-central1/vpnTunnels/tun-1", AssetType: "compute.googleapis.com/VpnTunnel", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"router": "projects/p1/regions/us-central1/routers/rtr-1", "vpnGateway": "projects/p1/regions/us-central1/vpnGateways/vgw-1"})}},
					{Name: "//datastream.googleapis.com/projects/p1/locations/us-central1/streams/cdc-1", AssetType: "datastream.googleapis.com/Stream", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"sourceConfig":      map[string]any{"sourceConnectionProfile": "projects/p1/locations/us-central1/connectionProfiles/cp-src"},
						"destinationConfig": map[string]any{"destinationConnectionProfile": "projects/p1/locations/us-central1/connectionProfiles/cp-dst"},
					})}},
					{Name: "//datastream.googleapis.com/projects/p1/locations/us-central1/connectionProfiles/cp-src", AssetType: "datastream.googleapis.com/ConnectionProfile", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"postgresqlProfile":   map[string]any{"hostname": "10.20.0.5"},
						"privateConnectivity": map[string]any{"privateConnection": "projects/p1/locations/us-central1/privateConnections/pc-1"},
					})}},
					{Name: "//datastream.googleapis.com/projects/p1/locations/us-central1/privateConnections/pc-1", AssetType: "datastream.googleapis.com/PrivateConnection", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"vpcPeeringConfig": map[string]any{"vpc": "projects/p1/global/networks/vpc-a"}})}},
					{Name: "//datastream.googleapis.com/projects/p1/locations/us-central1/connectionProfiles/cp-dst", AssetType: "datastream.googleapis.com/ConnectionProfile", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"gcsProfile": map[string]any{"bucket": "static-bkt"}})}},
					{Name: "//iam.googleapis.com/projects/p1/serviceAccounts/api@p1.iam.gserviceaccount.com", AssetType: "iam.googleapis.com/ServiceAccount", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"email": "api@p1.iam.gserviceaccount.com", "uniqueId": "20001"})}},
				},
			}, nil
		}
		// IAM_POLICY: 2 Cloud SQL instances exist in p1 -> condition-pinned binding MUST resolve to db-1 only; cross-project SA emits iam_cross_project hint!
		return &assetpb.ListAssetsResponse{
			ReadTime: timestamppb.Now(),
			Assets: []*assetpb.Asset{
				{
					Name:      "//cloudresourcemanager.googleapis.com/projects/p1",
					AssetType: "cloudresourcemanager.googleapis.com/Project",
					IamPolicy: &iampb.Policy{Bindings: []*iampb.Binding{
						{
							Role:      "roles/cloudsql.client",
							Members:   []string{"serviceAccount:api@p1.iam.gserviceaccount.com"},
							Condition: &expr.Expr{Title: "pin db-1", Expression: `resource.name == "//sqladmin.googleapis.com/projects/p1/instances/db-1"`},
						},
					}},
				},
				{
					Name:      "//storage.googleapis.com/static-bkt",
					AssetType: "storage.googleapis.com/Bucket",
					IamPolicy: &iampb.Policy{Bindings: []*iampb.Binding{
						{Role: "roles/storage.objectViewer", Members: []string{"serviceAccount:reader@ext-caller.iam.gserviceaccount.com"}},
					}},
				},
			},
		}, nil
	}

	clients, _ := newTestClients(t, assetHandler, nil, nil, nil)
	facts, err := Extract(context.Background(), clients, ExtractOptions{Scopes: []string{"projects/p1"}})
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}

	apiURI := "//run.googleapis.com/projects/p1/locations/us-central1/services/api"
	frURI := "//compute.googleapis.com/projects/p1/global/forwardingRules/fr-api"
	bktURI := "//storage.googleapis.com/static-bkt"
	subURI := "//pubsub.googleapis.com/projects/p1/subscriptions/orders-sub"
	topicURI := "//pubsub.googleapis.com/projects/p1/topics/orders"
	dsURI := "//bigquery.googleapis.com/projects/p1/datasets/ds1"
	db1URI := "//sqladmin.googleapis.com/projects/p1/instances/db-1"
	db2URI := "//sqladmin.googleapis.com/projects/p1/instances/db-2"
	saURI := "//iam.googleapis.com/projects/p1/serviceAccounts/api@p1.iam.gserviceaccount.com"
	streamURI := "//datastream.googleapis.com/projects/p1/locations/us-central1/streams/cdc-1"

	for _, want := range []struct {
		from, to string
		kind     FactEdgeKind
	}{
		{frURI, apiURI, EdgeConfigRef},
		{frURI, bktURI, EdgeConfigRef},
		{apiURI, db1URI, EdgeConfigRef},
		{apiURI, "//secretmanager.googleapis.com/projects/p1/secrets/jwt-key", EdgeConfigRef},
		{apiURI, "//vpcaccess.googleapis.com/projects/p1/locations/us-central1/connectors/conn-a", EdgeConfigRef},
		{topicURI, subURI, EdgeConfigRef},
		{subURI, apiURI, EdgeConfigRef},
		{subURI, dsURI, EdgeConfigRef},
		{subURI, bktURI, EdgeConfigRef},
		{"//cloudscheduler.googleapis.com/projects/p1/locations/us-central1/jobs/cron-1", topicURI, EdgeConfigRef},
		{"//cloudscheduler.googleapis.com/projects/p1/locations/us-central1/jobs/cron-1", apiURI, EdgeConfigRef},
		{topicURI, "//eventarc.googleapis.com/projects/p1/locations/us-central1/triggers/ea-1", EdgeConfigRef},
		{"//eventarc.googleapis.com/projects/p1/locations/us-central1/triggers/ea-1", apiURI, EdgeConfigRef},
		{"//compute.googleapis.com/projects/p1/regions/us-central1/routers/rtr-1", "//compute.googleapis.com/projects/p1/regions/us-central1/vpnGateways/vgw-1", EdgeConfigRef},
		{db1URI, streamURI, EdgeConfigRef},
		{streamURI, bktURI, EdgeConfigRef},
		{bktURI, dsURI, EdgeConfigRef}, // BigLake folded Table
		{"//compute.googleapis.com/projects/p1/global/networks/vpc-a", "//cloudresourcemanager.googleapis.com/projects/ext-hub", EdgeVPCPeering},
		{saURI, db1URI, EdgeIAMResourceBinding},
		{"//cloudresourcemanager.googleapis.com/projects/ext-caller", bktURI, EdgeIAMResourceBinding},
	} {
		if !hasEdge(facts.Edges, want.from, want.to, want.kind) {
			t.Errorf("missing edge %s -[%s]-> %s", want.from, want.kind, want.to)
		}
	}
	if hasEdge(facts.Edges, saURI, db2URI, EdgeIAMResourceBinding) {
		t.Fatal("IAM condition-pinned binding leaked onto db-2")
	}
	var sawIAMHint bool
	for _, h := range facts.OutOfScopeTargets {
		if h.TargetProjectID == "ext-caller" && h.SourceKind == "iam_cross_project" {
			sawIAMHint = true
		}
	}
	if !sawIAMHint {
		t.Fatalf("missing iam_cross_project hint in %+v", facts.OutOfScopeTargets)
	}
}

func TestExtractEnrichersMonitoringMethodFilterBQAndDataflow(t *testing.T) {
	assertReadOnlyDiscoveryMethodFilter(t)

	assetHandler := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		if req.ContentType == assetpb.ContentType_RESOURCE {
			return &assetpb.ListAssetsResponse{
				ReadTime: timestamppb.Now(),
				Assets: []*assetpb.Asset{
					{Name: "//cloudresourcemanager.googleapis.com/projects/data-p", AssetType: "cloudresourcemanager.googleapis.com/Project", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "data-p", "projectNumber": "5001"})}},
					{Name: "//serviceusage.googleapis.com/projects/data-p/services/bigquery.googleapis.com", AssetType: "serviceusage.googleapis.com/Service", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"state": "ENABLED"})}},
					{Name: "//serviceusage.googleapis.com/projects/data-p/services/dataflow.googleapis.com", AssetType: "serviceusage.googleapis.com/Service", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"state": "ENABLED"})}},
					{Name: "//serviceusage.googleapis.com/projects/data-p/services/monitoring.googleapis.com", AssetType: "serviceusage.googleapis.com/Service", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"state": "ENABLED"})}},
					{Name: "//serviceusage.googleapis.com/projects/data-p/services/aiplatform.googleapis.com", AssetType: "serviceusage.googleapis.com/Service", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"state": "ENABLED"})}},
					{Name: "//bigquery.googleapis.com/projects/data-p/datasets/raw_ds", AssetType: "bigquery.googleapis.com/Dataset", Ancestors: []string{"projects/data-p"}},
					{Name: "//bigquery.googleapis.com/projects/data-p/datasets/gold_ds", AssetType: "bigquery.googleapis.com/Dataset", Ancestors: []string{"projects/data-p"}},
					{Name: "//storage.googleapis.com/landing-bkt", AssetType: "storage.googleapis.com/Bucket", Ancestors: []string{"projects/data-p"}},
					{Name: "//pubsub.googleapis.com/projects/data-p/subscriptions/events-sub", AssetType: "pubsub.googleapis.com/Subscription", Ancestors: []string{"projects/data-p"}},
					{Name: "//iam.googleapis.com/projects/data-p/serviceAccounts/worker@data-p.iam.gserviceaccount.com", AssetType: "iam.googleapis.com/ServiceAccount", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"email": "worker@data-p.iam.gserviceaccount.com", "uniqueId": "9001"})}},
					{Name: "//run.googleapis.com/projects/data-p/locations/us-central1/services/worker-svc", AssetType: "run.googleapis.com/Service", Resource: &assetpb.Resource{Location: "us-central1", Data: mustStruct(t, map[string]any{"template": map[string]any{"serviceAccount": "worker@data-p.iam.gserviceaccount.com"}})}},
				},
			}, nil
		}
		return &assetpb.ListAssetsResponse{ReadTime: timestamppb.Now()}, nil
	}

	var sawResourceMethodGroup atomic.Bool
	monHandler := newEnricherMonitoringHandler(t, &sawResourceMethodGroup)
	mux := newEnricherBQDataflowMux()

	clients, _ := newTestClients(t, assetHandler, monHandler, mux, nil)
	facts, err := Extract(context.Background(), clients, ExtractOptions{Scopes: []string{"projects/data-p"}})
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	if !sawResourceMethodGroup.Load() {
		t.Fatal("Monitoring consumed_api query did not group by resource.labels.method")
	}

	assertEnricherFacts(t, facts)
}

func assertReadOnlyDiscoveryMethodFilter(t *testing.T) {
	t.Helper()
	for _, ro := range []string{
		"storage.buckets.get",
		"redis.projects.locations.instances.list",
		"compute.instances.aggregatedList",
		"google.firestore.admin.v1.FirestoreAdmin.ListDatabases",
		"google.firestore.v1.Firestore.BatchGetDocuments",
		"google.iam.v1.IAMPolicy.TestIamPermissions",
		"google.cloud.redis.v1.CloudRedis.GetInstance",
		"WatchChanges",
		"PollStatus",
	} {
		if !IsReadOnlyDiscoveryMethod(ro) {
			t.Errorf("IsReadOnlyDiscoveryMethod(%q) = false, want true", ro)
		}
	}
	for _, active := range []string{
		"google.cloud.bigquery.storage.v1.BigQueryRead.CreateReadSession",
		"storage.objects.insert",
		"google.pubsub.v1.Publisher.Publish",
		"google.cloud.aiplatform.v1.PredictionService.GenerateContent",
	} {
		if IsReadOnlyDiscoveryMethod(active) {
			t.Errorf("IsReadOnlyDiscoveryMethod(%q) = true, want false", active)
		}
	}
}

func newEnricherMonitoringHandler(t *testing.T, sawResourceMethodGroup *atomic.Bool) func(context.Context, *monitoringpb.ListTimeSeriesRequest) (*monitoringpb.ListTimeSeriesResponse, error) {
	t.Helper()
	return func(_ context.Context, req *monitoringpb.ListTimeSeriesRequest) (*monitoringpb.ListTimeSeriesResponse, error) {
		if strings.Contains(req.Filter, "consumed_api") {
			if slices.Contains(req.GetAggregation().GetGroupByFields(), "resource.labels.method") {
				sawResourceMethodGroup.Store(true)
			}
			if slices.Contains(req.GetAggregation().GetGroupByFields(), "metric.labels.method") {
				t.Errorf("must group by resource.labels.method, NOT metric.labels.method")
			}
			return &monitoringpb.ListTimeSeriesResponse{
				TimeSeries: []*monitoringpb.TimeSeries{
					{
						Resource: &monitoredres.MonitoredResource{Labels: map[string]string{
							"project_id": "data-p", "service": "storage-api.googleapis.com",
							"credential_id": "serviceaccount:9001", "method": "storage.objects.insert",
						}},
						Points: []*monitoringpb.Point{{Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 420}}}},
					},
					{
						Resource: &monitoredres.MonitoredResource{Labels: map[string]string{
							"project_id": "data-p", "service": "firestore.googleapis.com",
							"credential_id": "serviceaccount:9001", "method": "google.firestore.admin.v1.FirestoreAdmin.ListDatabases",
						}},
						Points: []*monitoringpb.Point{{Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 22}}}},
					},
					{
						Resource: &monitoredres.MonitoredResource{Labels: map[string]string{
							"project_id": "data-p", "service": "redis.googleapis.com",
							"credential_id": "serviceaccount:9001", "method": "redis.projects.locations.instances.get",
						}},
						Points: []*monitoringpb.Point{{Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 22}}}},
					},
					{
						Resource: &monitoredres.MonitoredResource{Labels: map[string]string{
							"project_id": "data-p", "service": "bigquerystorage.googleapis.com",
							"credential_id": "serviceaccount:9001", "method": "CreateReadSession",
						}},
						Points: []*monitoringpb.Point{{Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 100}}}},
					},
					{
						Resource: &monitoredres.MonitoredResource{Labels: map[string]string{
							"project_id": "data-p", "service": "aiplatform.googleapis.com",
							"credential_id": "serviceaccount:9001", "method": "google.cloud.aiplatform.v1.PredictionService.GenerateContent",
						}},
						Points: []*monitoringpb.Point{{Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 50}}}},
					},
				},
			}, nil
		}
		if strings.Contains(req.Filter, "model_invocation_count") {
			return &monitoringpb.ListTimeSeriesResponse{
				TimeSeries: []*monitoringpb.TimeSeries{{
					Resource: &monitoredres.MonitoredResource{Labels: map[string]string{
						"project_id": "data-p", "location": "us-central1", "publisher": "google", "model_user_id": "gemini-4-carbon",
					}},
					Metric: &metric.Metric{Labels: map[string]string{"request_type": "dedicated"}},
					Points: []*monitoringpb.Point{{Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: 50}}}},
				}},
			}, nil
		}
		if strings.Contains(req.Filter, "consumed_token_throughput") {
			return &monitoringpb.ListTimeSeriesResponse{
				TimeSeries: []*monitoringpb.TimeSeries{{
					Resource: &monitoredres.MonitoredResource{Labels: map[string]string{
						"project_id": "data-p", "location": "us-central1", "publisher": "google", "model_user_id": "gemini-4-carbon",
					}},
					Metric: &metric.Metric{Labels: map[string]string{"request_type": "dedicated"}},
					Points: []*monitoringpb.Point{{Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 1000}}}},
				}},
			}, nil
		}
		return &monitoringpb.ListTimeSeriesResponse{}, nil
	}
}

func newEnricherBQDataflowMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/projects/data-p/jobs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := &bigquery.JobList{
			Jobs: []*bigquery.JobListJobs{{
				Id:               "job-elt-1",
				PrincipalSubject: "serviceAccount:worker@data-p.iam.gserviceaccount.com",
				Statistics: &bigquery.JobStatistics{
					Query: &bigquery.JobStatistics2{
						ReferencedTables: []*bigquery.TableReference{
							{ProjectId: "data-p", DatasetId: "raw_ds", TableId: "events"},
							{ProjectId: "data-p", DatasetId: "_temp", TableId: "anon12345678"},
						},
					},
				},
				Configuration: &bigquery.JobConfiguration{
					Query: &bigquery.JobConfigurationQuery{
						DestinationTable: &bigquery.TableReference{ProjectId: "data-p", DatasetId: "gold_ds", TableId: "agg"},
					},
				},
			}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/v1b3/projects/data-p/locations/-/jobs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&dataflow.ListJobsResponse{
			Jobs: []*dataflow.Job{{Id: "df-1", Name: "stream-orders", Location: "us-central1"}},
		})
	})
	mux.HandleFunc("/v1b3/projects/data-p/locations/us-central1/jobs/df-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&dataflow.Job{
			Id:           "df-1",
			Name:         "stream-orders",
			Type:         "JOB_TYPE_STREAMING",
			CurrentState: "JOB_STATE_RUNNING",
			Environment: &dataflow.Environment{
				ServiceAccountEmail: "df-worker@data-p.iam.gserviceaccount.com",
				TempStoragePrefix:   "gs://landing-bkt/tmp",
			},
			PipelineDescription: &dataflow.PipelineDescription{
				DisplayData: []*dataflow.DisplayData{
					{StrValue: "projects/data-p/subscriptions/events-sub"},
					{StrValue: "data-p.gold_ds.stream_out"},
				},
			},
		})
	})
	return mux
}

func assertEnricherFacts(t *testing.T, facts *FactsFile) {
	t.Helper()
	workerURI := "//run.googleapis.com/projects/data-p/locations/us-central1/services/worker-svc"
	rawDS := "//bigquery.googleapis.com/projects/data-p/datasets/raw_ds"
	goldDS := "//bigquery.googleapis.com/projects/data-p/datasets/gold_ds"
	bktURI := "//storage.googleapis.com/landing-bkt"
	subURI := "//pubsub.googleapis.com/projects/data-p/subscriptions/events-sub"
	dfURI := "//dataflow.googleapis.com/projects/data-p/locations/us-central1/jobs/df-1"
	pmURI := "//aiplatform.googleapis.com/projects/data-p/locations/us-central1/publishers/google/models/gemini-4-carbon"

	if !hasEdge(facts.Edges, rawDS, goldDS, EdgeBQLineage) {
		t.Errorf("missing BQ_LINEAGE raw_ds -> gold_ds")
	}
	if !hasEdge(facts.Edges, workerURI, goldDS, EdgeBQLineage) {
		t.Errorf("missing BQ_LINEAGE worker-svc -> gold_ds")
	}
	if findResource(facts.Resources, dfURI) == nil {
		t.Fatalf("expected active Dataflow job %s in resources", dfURI)
	}
	if !hasEdge(facts.Edges, subURI, dfURI, EdgeConfigRef) ||
		!hasEdge(facts.Edges, dfURI, bktURI, EdgeConfigRef) ||
		!hasEdge(facts.Edges, dfURI, goldDS, EdgeConfigRef) {
		t.Errorf("missing Dataflow CONFIG_REF edges in %+v", facts.Edges)
	}
	if findResource(facts.Resources, pmURI) == nil {
		t.Fatalf("expected PublisherModel %s in resources", pmURI)
	}
	if !hasEdge(facts.Edges, workerURI, pmURI, EdgeMonitoringTraffic) {
		t.Errorf("expected MONITORING_TRAFFIC edge %s -> %s", workerURI, pmURI)
	}
	for _, r := range facts.Resources {
		if strings.Contains(r.AssetType, "firestore") || strings.Contains(r.AssetType, "redis") {
			t.Errorf("unexpected zero-instance noise resource: %+v", r)
		}
	}
}

func TestExtractMonitoringUnambiguousCompanionAPI(t *testing.T) {
	assetHandler := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		if req.ContentType == assetpb.ContentType_RESOURCE {
			return &assetpb.ListAssetsResponse{
				ReadTime: timestamppb.Now(),
				Assets: []*assetpb.Asset{
					{Name: "//cloudresourcemanager.googleapis.com/projects/p1", AssetType: "cloudresourcemanager.googleapis.com/Project", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "p1"})}},
					{Name: "//storage.googleapis.com/only-bkt", AssetType: "storage.googleapis.com/Bucket", Ancestors: []string{"projects/p1"}},
					{Name: "//bigquery.googleapis.com/projects/p1/datasets/only_ds", AssetType: "bigquery.googleapis.com/Dataset", Ancestors: []string{"projects/p1"}},
					{Name: "//iam.googleapis.com/projects/p1/serviceAccounts/app@p1.iam.gserviceaccount.com", AssetType: "iam.googleapis.com/ServiceAccount", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"email": "app@p1.iam.gserviceaccount.com", "uniqueId": "7777"})}},
					{Name: "//run.googleapis.com/projects/p1/locations/us-central1/services/app", AssetType: "run.googleapis.com/Service", Resource: &assetpb.Resource{Location: "us-central1", Data: mustStruct(t, map[string]any{"template": map[string]any{"serviceAccount": "app@p1.iam.gserviceaccount.com"}})}},
				},
			}, nil
		}
		return &assetpb.ListAssetsResponse{ReadTime: timestamppb.Now()}, nil
	}
	monHandler := func(_ context.Context, req *monitoringpb.ListTimeSeriesRequest) (*monitoringpb.ListTimeSeriesResponse, error) {
		if strings.Contains(req.Filter, "consumed_api") {
			return &monitoringpb.ListTimeSeriesResponse{
				TimeSeries: []*monitoringpb.TimeSeries{
					{
						Resource: &monitoredres.MonitoredResource{Labels: map[string]string{
							"project_id": "p1", "service": "storage-api.googleapis.com", "credential_id": "serviceaccount:7777", "method": "storage.objects.create",
						}},
						Points: []*monitoringpb.Point{{Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 150}}}},
					},
					{
						Resource: &monitoredres.MonitoredResource{Labels: map[string]string{
							"project_id": "p1", "service": "bigquerystorage.googleapis.com", "credential_id": "serviceaccount:7777", "method": "CreateReadSession",
						}},
						Points: []*monitoringpb.Point{{Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 80}}}},
					},
					{
						Resource: &monitoredres.MonitoredResource{Labels: map[string]string{
							"project_id": "p1", "service": "aiplatform.googleapis.com", "credential_id": "serviceaccount:7777", "method": "GenerateContent",
						}},
						Points: []*monitoringpb.Point{{Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 25}}}},
					},
				},
			}, nil
		}
		if strings.Contains(req.Filter, "model_invocation_count") {
			return &monitoringpb.ListTimeSeriesResponse{
				TimeSeries: []*monitoringpb.TimeSeries{{
					Resource: &monitoredres.MonitoredResource{Labels: map[string]string{
						"project_id": "p1", "location": "global", "publisher": "google", "model_user_id": "gemini-4-carbon",
					}},
					Points: []*monitoringpb.Point{{Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 25}}}},
				}},
			}, nil
		}
		return &monitoringpb.ListTimeSeriesResponse{}, nil
	}

	clients, _ := newTestClients(t, assetHandler, monHandler, nil, nil)
	facts, err := Extract(context.Background(), clients, ExtractOptions{Scopes: []string{"projects/p1"}})
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	appURI := "//run.googleapis.com/projects/p1/locations/us-central1/services/app"
	if !hasEdge(facts.Edges, appURI, "//storage.googleapis.com/only-bkt", EdgeMonitoringTraffic) {
		t.Errorf("missing MONITORING_TRAFFIC app -> only-bkt via storage-api companion mapping")
	}
	if !hasEdge(facts.Edges, appURI, "//bigquery.googleapis.com/projects/p1/datasets/only_ds", EdgeMonitoringTraffic) {
		t.Errorf("missing MONITORING_TRAFFIC app -> only_ds via bigquerystorage companion mapping")
	}
	if !hasEdge(facts.Edges, appURI, "//aiplatform.googleapis.com/projects/p1/locations/global/publishers/google/models/gemini-4-carbon", EdgeMonitoringTraffic) {
		t.Errorf("missing MONITORING_TRAFFIC app -> PublisherModel")
	}
}

func TestExtractDegradationAndErrors(t *testing.T) {
	if _, err := Extract(context.Background(), nil, ExtractOptions{Scopes: []string{"projects/p1"}}); err == nil {
		t.Fatal("expected error for nil Clients")
	}
	clients, _ := newTestClients(t, nil, nil, nil, nil)
	if _, err := Extract(context.Background(), clients, ExtractOptions{}); err == nil {
		t.Fatal("expected error for empty Scopes")
	}

	// Target scope VPC-SC block degrades gracefully while sibling succeeds.
	assetHandler := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		if req.Parent == "projects/blocked" {
			return nil, grpcErrorWithInfo(codes.PermissionDenied, "vpc-sc", "SECURITY_POLICY_VIOLATED", "googleapis.com", map[string]string{"vpcServiceControlsUniqueIdentifier": "u-1"})
		}
		return &assetpb.ListAssetsResponse{
			ReadTime: timestamppb.Now(),
			Assets: []*assetpb.Asset{
				{Name: "//storage.googleapis.com/ok-bkt", AssetType: "storage.googleapis.com/Bucket", Ancestors: []string{"projects/allowed"}},
			},
		}, nil
	}
	monHandler := func(_ context.Context, _ *monitoringpb.ListTimeSeriesRequest) (*monitoringpb.ListTimeSeriesResponse, error) {
		return nil, grpcErrorWithInfo(codes.PermissionDenied, "no mon perm", "IAM_PERMISSION_DENIED", "monitoring.googleapis.com", nil)
	}
	degradeClients, _ := newTestClients(t, assetHandler, monHandler, nil, nil)
	facts, err := Extract(context.Background(), degradeClients, ExtractOptions{Scopes: []string{"projects/blocked", "projects/allowed"}})
	if err != nil {
		t.Fatalf("expected graceful degradation, got %v", err)
	}
	var sawVPCSC, sawMonDenied bool
	for _, ce := range facts.CoverageLedger {
		if ce.Scope == "projects/blocked" && ce.State == CoverageVPCSCBlocked {
			sawVPCSC = true
		}
		if ce.Scope == "projects/allowed" && ce.Source == "monitoring" && ce.State == CoverageIAMDenied {
			sawMonDenied = true
		}
	}
	if !sawVPCSC || !sawMonDenied {
		t.Fatalf("missing expected degraded ledger entries: %+v", facts.CoverageLedger)
	}
}

// --- acme-cicd v3 Fixture Validation Test ---

func TestAcmeCICDV3Fixture(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "testdata", "acme_cicd_v3.facts.json")
	facts, err := LoadFacts(fixturePath)
	if err != nil {
		t.Fatalf("LoadFacts(%s): %v", fixturePath, err)
	}
	if facts.SchemaVersion != FactsSchemaVersion {
		t.Fatalf("SchemaVersion = %q, want %q", facts.SchemaVersion, FactsSchemaVersion)
	}

	assertAcmeCICDBuildTriggers(t, facts)
	assertAcmeCICDGKEAndSQL(t, facts)
	assertAcmeCICDFixtureIntegrity(t, fixturePath, facts)
}

func assertAcmeCICDBuildTriggers(t *testing.T, facts *FactsFile) {
	t.Helper()
	githubURI := "//github.com/acme-corp/platform-services"
	gh := findResource(facts.Resources, githubURI)
	if gh == nil || !gh.IsExternal || gh.AssetType != "external.github.com/Repository" {
		t.Fatalf("missing or invalid synthetic GitHub resource: %+v", gh)
	}

	var triggers []FactResource
	for _, r := range facts.Resources {
		if r.AssetType == "cloudbuild.googleapis.com/BuildTrigger" {
			triggers = append(triggers, r)
		}
	}
	if len(triggers) != 20 {
		t.Fatalf("BuildTrigger count = %d, want 20", len(triggers))
	}
	for _, tr := range triggers {
		if !hasEdge(facts.Edges, githubURI, tr.URI, EdgeCloudBuildRepo) {
			t.Errorf("trigger %s missing CLOUDBUILD_TRIGGER_REPO from platform-services", tr.URI)
		}
	}

	arURI := "//artifactregistry.googleapis.com/projects/acme-cicd/locations/europe-southwest1/repositories/acme-apps"
	ar := findResource(facts.Resources, arURI)
	if ar == nil || ar.Attributes["image_count"] != float64(643) {
		t.Fatalf("Artifact Registry acme-apps missing or image_count != 643: %+v", ar)
	}

	var sawARStep, sawTFStateStep, sawModulesStep, sawDevStubStep, sawProdStubStep bool
	for _, e := range facts.Edges {
		if e.Kind != EdgeCloudBuildStepRef {
			continue
		}
		switch e.ToURI {
		case arURI:
			sawARStep = true
		case "//storage.googleapis.com/acme-tfstate":
			sawTFStateStep = true
		case "//storage.googleapis.com/acme-private-modules":
			sawModulesStep = true
		case "//cloudresourcemanager.googleapis.com/projects/acme-dev":
			sawDevStubStep = true
		case "//cloudresourcemanager.googleapis.com/projects/acme-prod":
			sawProdStubStep = true
		}
	}
	if !sawARStep || !sawTFStateStep || !sawModulesStep || !sawDevStubStep || !sawProdStubStep {
		t.Fatalf("missing expected CLOUDBUILD_STEP_REF targets: ar=%v tfstate=%v modules=%v dev=%v prod=%v",
			sawARStep, sawTFStateStep, sawModulesStep, sawDevStubStep, sawProdStubStep)
	}
	if len(facts.OutOfScopeTargets) < 2 {
		t.Fatalf("expected >= 2 out_of_scope_targets, got %+v", facts.OutOfScopeTargets)
	}
}

func assertAcmeCICDGKEAndSQL(t *testing.T, facts *FactsFile) {
	t.Helper()
	vpcURI := "//compute.googleapis.com/projects/acme-cicd/global/networks/cicd"
	clusterURI := "//container.googleapis.com/projects/acme-cicd/locations/europe-southwest1-b/clusters/cicd"
	sqlURI := "//sqladmin.googleapis.com/projects/acme-cicd/instances/sonarqube"
	if findResource(facts.Resources, vpcURI) == nil || findResource(facts.Resources, clusterURI) == nil || findResource(facts.Resources, sqlURI) == nil {
		t.Fatal("missing core acme-cicd resources (VPC cicd, GKE cicd, or Cloud SQL sonarqube)")
	}
	if !hasEdge(facts.Edges, vpcURI, sqlURI, EdgePSAPeering) {
		t.Fatal("missing PSA_PEERING edge VPC cicd -> Cloud SQL sonarqube")
	}

	var psaTenantOK bool
	for _, p := range facts.Hierarchy.Projects {
		if p.ProjectID == "p1234567890abcdef-tp" && p.IsPSATenant {
			psaTenantOK = true
		}
	}
	if !psaTenantOK {
		t.Fatalf("p1234567890abcdef-tp not marked is_psa_tenant=true in hierarchy: %+v", facts.Hierarchy.Projects)
	}

	atlDep := clusterURI + "/k8s/namespaces/atlantis/apps/deployments/atlantis"
	atlSvc := clusterURI + "/k8s/namespaces/atlantis/services/atlantis"
	atlIng := clusterURI + "/k8s/namespaces/atlantis/networking.k8s.io/ingresses/atlantis"
	atlFR := "//compute.googleapis.com/projects/acme-cicd/global/forwardingRules/k8s2-fs-demo1234-atlantis-ingress-0001"
	atlSA := "//iam.googleapis.com/projects/acme-cicd/serviceAccounts/atlantis@acme-cicd.iam.gserviceaccount.com"

	sonarDep := clusterURI + "/k8s/namespaces/sonarqube/apps/deployments/sonarqube"
	sonarSvc := clusterURI + "/k8s/namespaces/sonarqube/services/sonarqube"
	sonarIng := clusterURI + "/k8s/namespaces/sonarqube/networking.k8s.io/ingresses/sonarqube"
	sonarFR := "//compute.googleapis.com/projects/acme-cicd/global/forwardingRules/k8s2-fs-demo1234-sonarqube-ingress-0002"
	sonarSA := "//iam.googleapis.com/projects/acme-cicd/serviceAccounts/sonarqube@acme-cicd.iam.gserviceaccount.com"

	for _, want := range []struct {
		from, to string
		kind     FactEdgeKind
	}{
		{atlFR, atlSvc, EdgeK8sNEGBackend},
		{atlIng, atlSvc, EdgeK8sIngressBackend},
		{atlSvc, atlDep, EdgeK8sServiceSelector},
		{atlDep, atlSA, EdgeWorkloadIdentity},
		{atlSA, "//storage.googleapis.com/acme-tfstate", EdgeIAMResourceBinding},
		{atlSA, "//storage.googleapis.com/acme-private-modules", EdgeIAMResourceBinding},
		{atlDep, "//storage.googleapis.com/acme-tfstate", EdgeConfigRef},
		{atlDep, "//storage.googleapis.com/acme-private-modules", EdgeConfigRef},
		{sonarFR, sonarSvc, EdgeK8sNEGBackend},
		{sonarIng, sonarSvc, EdgeK8sIngressBackend},
		{sonarSvc, sonarDep, EdgeK8sServiceSelector},
		{sonarDep, sonarSA, EdgeWorkloadIdentity},
		{sonarDep, sqlURI, EdgeConfigRef},
		{sonarSA, sqlURI, EdgeIAMResourceBinding},
	} {
		if !hasEdge(facts.Edges, want.from, want.to, want.kind) {
			t.Errorf("fixture missing edge %s -[%s]-> %s", want.from, want.kind, want.to)
		}
	}
}

func assertAcmeCICDFixtureIntegrity(t *testing.T, fixturePath string, facts *FactsFile) {
	t.Helper()
	resURIs := make(map[string]bool, len(facts.Resources))
	for _, r := range facts.Resources {
		resURIs[r.URI] = true
		for k := range r.Attributes {
			if IsSensitiveKey(k) {
				t.Errorf("fixture resource %s contains sensitive key %q", r.URI, k)
			}
		}
	}
	for _, e := range facts.Edges {
		if wantID := EdgeID(e.Kind, e.FromURI, e.ToURI, e.Detail); e.ID != wantID {
			t.Errorf("edge %s -> %s has ID %q, want %q", e.FromURI, e.ToURI, e.ID, wantID)
		}
		if !resURIs[e.FromURI] || !resURIs[e.ToURI] {
			t.Errorf("dangling edge in fixture: %s -> %s", e.FromURI, e.ToURI)
		}
	}

	if facts.SuppressedPlumbingCounts["cloudbuild.googleapis.com/Build"] < 800 ||
		facts.SuppressedPlumbingCounts["sqladmin.googleapis.com/BackupRun"] == 0 ||
		facts.SuppressedPlumbingCounts["k8s.io/ServiceAccount"] == 0 {
		t.Fatalf("unexpected suppressed_plumbing_counts: %v", facts.SuppressedPlumbingCounts)
	}
	if len(facts.CoverageLedger) == 0 {
		t.Fatal("fixture missing coverage_ledger")
	}

	origRaw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	tmpPath := filepath.Join(t.TempDir(), "roundtrip.facts.json")
	if err := WriteFacts(tmpPath, facts); err != nil {
		t.Fatal(err)
	}
	rtRaw, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(origRaw, rtRaw) {
		t.Fatalf("testdata/acme_cicd_v3.facts.json is not in canonical WriteFacts sorted format")
	}
	rawLower := strings.ToLower(string(origRaw))
	for _, forbidden := range []string{`"startup-script"`, `"last-applied-configuration"`, `"webhookconfig"`, `"stringdata"`, `"binarydata"`, `"rawquery"`, `"substitutions"`} {
		if strings.Contains(rawLower, forbidden) {
			t.Fatalf("forbidden key %s found in fixture", forbidden)
		}
	}
}

func TestExtractHelperEdgeCases(t *testing.T) {
	// Ensure canonicalizeSlice, parseGitRepoURL, and companion host mapping cover edge cases.
	if h, o, r := parseGitRepoURL("https://user:pass@gitlab.com/myorg/myrepo.git?ref=main"); h != "gitlab.com" || o != "myorg" || r != "myrepo" {
		t.Fatalf("parseGitRepoURL(gitlab) = %q %q %q", h, o, r)
	}
	if h, _, _ := parseGitRepoURL("https://bitbucket.org/a/b"); h != "" {
		t.Fatalf("unexpected bitbucket match: %q", h)
	}
	if got := CanonicalServiceHost("BIGQUERYSTORAGE.GOOGLEAPIS.COM"); got != "bigquery.googleapis.com" {
		t.Fatalf("CanonicalServiceHost = %q", got)
	}
	if got := CanonicalServiceHost("storage-component.googleapis.com"); got != "storage.googleapis.com" {
		t.Fatalf("CanonicalServiceHost = %q", got)
	}
	if got := CanonicalServiceHost("pubsub.googleapis.com"); got != "pubsub.googleapis.com" {
		t.Fatalf("CanonicalServiceHost = %q", got)
	}
	if got := parseNEGStatusAnnotation("not-json"); got != nil {
		t.Fatalf("expected nil for invalid neg-status JSON, got %v", got)
	}
	if got := fmt.Sprintf("%v", canonicalizeSlice(nil, nil)); got != "[]" {
		t.Fatalf("unexpected canonicalizeSlice(nil): %s", got)
	}
}

func TestExtractMultiServiceStructuralHierarchyAndTriggers(t *testing.T) {
	assetHandler := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		if req.ContentType == assetpb.ContentType_RESOURCE {
			return &assetpb.ListAssetsResponse{
				ReadTime: timestamppb.Now(),
				Assets: []*assetpb.Asset{
					// Org, Folder, Project
					{Name: "//cloudresourcemanager.googleapis.com/organizations/999", AssetType: "cloudresourcemanager.googleapis.com/Organization", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"displayName": "Acme Corp"})}},
					{Name: "//cloudresourcemanager.googleapis.com/folders/444", AssetType: "cloudresourcemanager.googleapis.com/Folder", Ancestors: []string{"folders/444", "organizations/999"}, Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"displayName": "Platform"})}},
					{Name: "//cloudresourcemanager.googleapis.com/projects/2002", AssetType: "cloudresourcemanager.googleapis.com/Project", Ancestors: []string{"projects/2002", "folders/444", "organizations/999"}, Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "multi-p", "projectNumber": "2002", "name": "Multi"})}},
					// VPC, Subnet, Router, Interconnect, ServiceAttachment, NCC Hub+Spoke, Standalone VM, MIG, InstanceTemplate
					{Name: "//compute.googleapis.com/projects/2002/global/networks/vpc1", AssetType: "compute.googleapis.com/Network", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"name": "vpc1"})}},
					{Name: "//compute.googleapis.com/projects/2002/regions/us-central1/subnetworks/sub1", AssetType: "compute.googleapis.com/Subnetwork", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"network": "projects/2002/global/networks/vpc1"})}},
					{Name: "//compute.googleapis.com/projects/2002/regions/us-central1/routers/rtr1", AssetType: "compute.googleapis.com/Router", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"network":    "projects/2002/global/networks/vpc1",
						"interfaces": []any{map[string]any{"linkedInterconnectAttachment": "projects/2002/regions/us-central1/interconnectAttachments/ic1"}},
					})}},
					{Name: "//compute.googleapis.com/projects/2002/regions/us-central1/interconnectAttachments/ic1", AssetType: "compute.googleapis.com/InterconnectAttachment", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"router": "projects/2002/regions/us-central1/routers/rtr1"})}},
					{Name: "//compute.googleapis.com/projects/2002/regions/us-central1/serviceAttachments/sa1", AssetType: "compute.googleapis.com/ServiceAttachment", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"targetService": "projects/2002/regions/us-central1/forwardingRules/fr-tcp"})}},
					{Name: "//networkconnectivity.googleapis.com/projects/2002/locations/global/hubs/hub1", AssetType: "networkconnectivity.googleapis.com/Hub"},
					{Name: "//networkconnectivity.googleapis.com/projects/2002/locations/global/spokes/spoke1", AssetType: "networkconnectivity.googleapis.com/Spoke", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"hub":                           "projects/2002/locations/global/hubs/hub1",
						"linkedVpcNetwork":              map[string]any{"uri": "projects/2002/global/networks/vpc1"},
						"linkedInterconnectAttachments": map[string]any{"uris": []any{"projects/2002/regions/us-central1/interconnectAttachments/ic1"}},
					})}},
					{Name: "//compute.googleapis.com/projects/2002/global/instanceTemplates/tpl1", AssetType: "compute.googleapis.com/InstanceTemplate", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"properties": map[string]any{
							"networkInterfaces": []any{map[string]any{"network": "projects/2002/global/networks/vpc1", "subnetwork": "projects/2002/regions/us-central1/subnetworks/sub1"}},
							"serviceAccounts":   []any{map[string]any{"email": "vm@multi-p.iam.gserviceaccount.com"}},
						},
					})}},
					{Name: "//compute.googleapis.com/projects/2002/zones/us-central1-a/instanceGroupManagers/mig1", AssetType: "compute.googleapis.com/InstanceGroupManager", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"targetSize": 2})}},
					{Name: "//compute.googleapis.com/projects/2002/zones/us-central1-a/instances/vm1", AssetType: "compute.googleapis.com/Instance", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"networkInterfaces": []any{map[string]any{"networkIP": "10.30.0.9", "network": "projects/2002/global/networks/vpc1", "subnetwork": "projects/2002/regions/us-central1/subnetworks/sub1"}},
						"serviceAccounts":   []any{map[string]any{"email": "default"}},
						"metadata":          map[string]any{"items": []any{map[string]any{"key": "created-by", "value": "projects/2002/zones/us-central1-a/instanceGroupManagers/mig1"}}},
					})}},
					// LB: TargetTcpProxy, TargetSslProxy, UrlMap routeRules/weightedBackendServices, CloudFn/AppEngine/PSC/MIG NEGs
					{Name: "//compute.googleapis.com/projects/2002/regions/us-central1/forwardingRules/fr-tcp", AssetType: "compute.googleapis.com/ForwardingRule", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"loadBalancingScheme": "INTERNAL",
						"IPAddress":           "10.30.0.20",
						"network":             "projects/2002/global/networks/vpc1",
						"target":              "projects/2002/global/targetSslProxies/sp1",
					})}},
					{Name: "//compute.googleapis.com/projects/2002/global/targetSslProxies/sp1", AssetType: "compute.googleapis.com/TargetSslProxy", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"service": "projects/2002/global/backendServices/bs-multi"})}},
					{Name: "//compute.googleapis.com/projects/2002/global/forwardingRules/fr-route", AssetType: "compute.googleapis.com/GlobalForwardingRule", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"target": "projects/2002/global/targetGrpcProxies/gp1"})}},
					{Name: "//compute.googleapis.com/projects/2002/global/targetGrpcProxies/gp1", AssetType: "compute.googleapis.com/TargetGrpcProxy", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"urlMap": "projects/2002/global/urlMaps/um-route"})}},
					{Name: "//compute.googleapis.com/projects/2002/global/urlMaps/um-route", AssetType: "compute.googleapis.com/UrlMap", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"hostRules": []any{map[string]any{"hosts": []any{"api.acme.dev"}}},
						"pathMatchers": []any{map[string]any{
							"defaultService": "projects/2002/global/backendServices/bs-multi",
							"routeRules": []any{map[string]any{
								"matchRules":  []any{map[string]any{"prefixMatch": "/v2"}},
								"service":     "projects/2002/global/backendServices/bs-multi",
								"routeAction": map[string]any{"weightedBackendServices": []any{map[string]any{"backendService": "projects/2002/global/backendServices/bs-multi"}}},
							}},
						}},
					})}},
					{Name: "//compute.googleapis.com/projects/2002/global/backendServices/bs-multi", AssetType: "compute.googleapis.com/BackendService", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"backends": []any{
							map[string]any{"group": "projects/2002/regions/us-central1/networkEndpointGroups/neg-fn"},
							map[string]any{"group": "projects/2002/regions/us-central1/networkEndpointGroups/neg-ae"},
							map[string]any{"group": "projects/2002/regions/us-central1/networkEndpointGroups/neg-psc"},
							map[string]any{"group": "projects/2002/zones/us-central1-a/instanceGroups/mig1"},
						},
					})}},
					{Name: "//compute.googleapis.com/projects/2002/regions/us-central1/networkEndpointGroups/neg-fn", AssetType: "compute.googleapis.com/NetworkEndpointGroup", Resource: &assetpb.Resource{Location: "us-central1", Data: mustStruct(t, map[string]any{"cloudFunction": map[string]any{"function": "fn1"}})}},
					{Name: "//compute.googleapis.com/projects/2002/regions/us-central1/networkEndpointGroups/neg-ae", AssetType: "compute.googleapis.com/NetworkEndpointGroup", Resource: &assetpb.Resource{Location: "us-central1", Data: mustStruct(t, map[string]any{"appEngine": map[string]any{"service": "web"}})}},
					{Name: "//compute.googleapis.com/projects/2002/regions/us-central1/networkEndpointGroups/neg-psc", AssetType: "compute.googleapis.com/NetworkEndpointGroup", Resource: &assetpb.Resource{Location: "us-central1", Data: mustStruct(t, map[string]any{"pscTargetService": "projects/2002/regions/us-central1/serviceAttachments/sa1"})}},
					// CloudFunction v2, AppEngine Service+Version, Cloud Run v1 Knative Revision+Service, Cloud Run Job
					{Name: "//cloudfunctions.googleapis.com/projects/2002/locations/us-central1/functions/fn1", AssetType: "cloudfunctions.googleapis.com/CloudFunction", Resource: &assetpb.Resource{Location: "us-central1", Data: mustStruct(t, map[string]any{
						"httpsTrigger": map[string]any{"url": "https://us-central1-multi-p.cloudfunctions.net/fn1"},
						"eventTrigger": map[string]any{"pubsubTopic": "projects/2002/topics/t1"},
						"serviceConfig": map[string]any{
							"environmentVariables":       map[string]any{"BKT": "gs://bkt1"},
							"secretEnvironmentVariables": []any{map[string]any{"secret": "s1"}},
							"secretVolumes":              []any{map[string]any{"secret": "s1"}},
							"vpcConnector":               "projects/2002/locations/us-central1/connectors/c1",
						},
					})}},
					{Name: "//appengine.googleapis.com/apps/2002/services/web/versions/v1", AssetType: "appengine.googleapis.com/Version", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"serviceAccount":     "ae@multi-p.iam.gserviceaccount.com",
						"vpcAccessConnector": map[string]any{"name": "projects/2002/locations/us-central1/connectors/c1"},
						"envVariables":       map[string]any{"FN": "https://us-central1-multi-p.cloudfunctions.net/fn1"},
					})}},
					{Name: "//appengine.googleapis.com/apps/2002/services/web", AssetType: "appengine.googleapis.com/Service", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"split": map[string]any{"allocations": map[string]any{"v1": 1.0}},
					})}},
					{Name: "//run.googleapis.com/projects/2002/locations/us-central1/revisions/kn-001", AssetType: "run.googleapis.com/Revision", Resource: &assetpb.Resource{Location: "us-central1", Data: mustStruct(t, map[string]any{
						"metadata": map[string]any{
							"labels": map[string]any{"serving.knative.dev/service": "kn-svc"},
							"annotations": map[string]any{
								"run.googleapis.com/vpc-access-connector": "projects/2002/locations/us-central1/connectors/c1",
								"run.googleapis.com/cloudsql-instances":   "2002:us-central1:sql1",
							},
						},
						"spec": map[string]any{"serviceAccountName": "kn@multi-p.iam.gserviceaccount.com"},
					})}},
					{Name: "//run.googleapis.com/projects/2002/locations/us-central1/services/kn-svc", AssetType: "run.googleapis.com/Service", Resource: &assetpb.Resource{Location: "us-central1", Data: mustStruct(t, map[string]any{
						"status": map[string]any{
							"latestReadyRevisionName": "kn-001",
							"traffic":                 []any{map[string]any{"latestRevision": true, "percent": 100}},
						},
					})}},
					{Name: "//run.googleapis.com/projects/2002/locations/us-central1/jobs/run-job", AssetType: "run.googleapis.com/Job", Resource: &assetpb.Resource{Location: "us-central1", Data: mustStruct(t, map[string]any{
						"template": map[string]any{"template": map[string]any{
							"vpcAccess":  map[string]any{"connector": "projects/2002/locations/us-central1/connectors/c1"},
							"containers": []any{map[string]any{"args": []any{"--cluster=gke1", "gcloud run deploy kn-svc"}}},
						}},
					})}},
					{Name: "//vpcaccess.googleapis.com/projects/2002/locations/us-central1/connectors/c1", AssetType: "vpcaccess.googleapis.com/Connector", Resource: &assetpb.Resource{Location: "us-central1", Data: mustStruct(t, map[string]any{"network": "vpc1"})}},
					// Datastores: Cloud SQL, AlloyDB Cluster+Instance, Redis Instance+Cluster, Memcache, Filestore, Spanner+DB, Bigtable+Cluster+Table, AR Package/Version, KMS CryptoKey
					{Name: "//sqladmin.googleapis.com/projects/2002/instances/sql1", AssetType: "sqladmin.googleapis.com/Instance", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"settings": map[string]any{"ipConfiguration": map[string]any{"privateNetwork": "projects/2002/global/networks/vpc1"}}})}},
					{Name: "//alloydb.googleapis.com/projects/2002/locations/us-central1/clusters/adb1", AssetType: "alloydb.googleapis.com/Cluster", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"network": "projects/2002/global/networks/vpc1"})}},
					{Name: "//alloydb.googleapis.com/projects/2002/locations/us-central1/clusters/adb1/instances/primary", AssetType: "alloydb.googleapis.com/Instance", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"ipAddress": "10.30.1.5"})}},
					{Name: "//redis.googleapis.com/projects/2002/locations/us-central1/instances/r1", AssetType: "redis.googleapis.com/Instance", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"authorizedNetwork": "projects/2002/global/networks/vpc1", "host": "10.30.1.6", "readEndpoint": "10.30.1.7"})}},
					{Name: "//redis.googleapis.com/projects/2002/locations/us-central1/clusters/rc1", AssetType: "redis.googleapis.com/Cluster", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"pscConfigs": []any{map[string]any{"network": "projects/2002/global/networks/vpc1"}}, "discoveryEndpoints": []any{map[string]any{"address": "10.30.1.8"}}})}},
					{Name: "//memcache.googleapis.com/projects/2002/locations/us-central1/instances/mc1", AssetType: "memcache.googleapis.com/Instance", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"authorizedNetwork": "projects/2002/global/networks/vpc1", "discoveryEndpoint": "10.30.1.9", "memcacheNodes": []any{map[string]any{"host": "10.30.1.10"}}})}},
					{Name: "//file.googleapis.com/projects/2002/locations/us-central1-a/instances/fs1", AssetType: "file.googleapis.com/Instance", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"networks": []any{map[string]any{"network": "projects/2002/global/networks/vpc1", "ipAddresses": []any{"10.30.1.11"}}}})}},
					{Name: "//spanner.googleapis.com/projects/2002/instances/sp1", AssetType: "spanner.googleapis.com/Instance"},
					{Name: "//spanner.googleapis.com/projects/2002/instances/sp1/databases/db1", AssetType: "spanner.googleapis.com/Database"},
					{Name: "//bigtableadmin.googleapis.com/projects/2002/instances/bt1", AssetType: "bigtableadmin.googleapis.com/Instance"},
					{Name: "//bigtableadmin.googleapis.com/projects/2002/instances/bt1/clusters/c1", AssetType: "bigtableadmin.googleapis.com/Cluster"},
					{Name: "//bigtableadmin.googleapis.com/projects/2002/instances/bt1/tables/t1", AssetType: "bigtableadmin.googleapis.com/Table"},
					{Name: "//artifactregistry.googleapis.com/projects/2002/locations/us-central1/repositories/repo1", AssetType: "artifactregistry.googleapis.com/Repository"},
					{Name: "//artifactregistry.googleapis.com/projects/2002/locations/us-central1/repositories/repo1/packages/pkg1", AssetType: "artifactregistry.googleapis.com/Package"},
					{Name: "//artifactregistry.googleapis.com/projects/2002/locations/us-central1/repositories/repo1/packages/pkg1/versions/v1", AssetType: "artifactregistry.googleapis.com/Version"},
					{Name: "//cloudkms.googleapis.com/projects/2002/locations/us-central1/keyRings/kr1", AssetType: "cloudkms.googleapis.com/KeyRing"},
					{Name: "//cloudkms.googleapis.com/projects/2002/locations/us-central1/keyRings/kr1/cryptoKeys/k1", AssetType: "cloudkms.googleapis.com/CryptoKey"},
					// Analytics, AI, Integration
					{Name: "//storage.googleapis.com/bkt1", AssetType: "storage.googleapis.com/Bucket", Ancestors: []string{"projects/2002"}},
					{Name: "//storage.googleapis.com/bkt2", AssetType: "storage.googleapis.com/Bucket", Ancestors: []string{"projects/2002"}},
					{Name: "//pubsub.googleapis.com/projects/2002/topics/t1", AssetType: "pubsub.googleapis.com/Topic"},
					{Name: "//secretmanager.googleapis.com/projects/2002/secrets/s1", AssetType: "secretmanager.googleapis.com/Secret"},
					{Name: "//container.googleapis.com/projects/2002/locations/us-central1/clusters/gke1", AssetType: "container.googleapis.com/Cluster", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"networkConfig": map[string]any{"network": "projects/2002/global/networks/vpc1"}})}},
					{Name: "//bigquery.googleapis.com/projects/2002/datasets/ds1", AssetType: "bigquery.googleapis.com/Dataset"},
					{Name: "//bigquery.googleapis.com/projects/2002/datasets/ds1/routines/r1", AssetType: "bigquery.googleapis.com/Routine", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"remoteFunctionOptions": map[string]any{"connection": "2002.us.bqconn1", "endpoint": "https://us-central1-multi-p.cloudfunctions.net/fn1"}})}},
					{Name: "//bigquery.googleapis.com/projects/2002/datasets/ds1/models/m1", AssetType: "bigquery.googleapis.com/Model", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"remoteModelInfo": map[string]any{"connection": "projects/2002/locations/us/connections/bqconn1"}})}},
					{Name: "//bigqueryconnection.googleapis.com/projects/2002/locations/us/connections/bqconn1", AssetType: "bigqueryconnection.googleapis.com/Connection", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"cloudSql":     map[string]any{"instanceId": "2002:us-central1:sql1"},
						"cloudSpanner": map[string]any{"database": "projects/2002/instances/sp1/databases/db1"},
					})}},
					{Name: "//bigquerydatatransfer.googleapis.com/projects/2002/locations/us/transferConfigs/tc1", AssetType: "bigquerydatatransfer.googleapis.com/TransferConfig", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"destinationDatasetId": "ds1", "notificationPubsubTopic": "projects/2002/topics/t1"})}},
					{Name: "//storagetransfer.googleapis.com/projects/2002/transferJobs/tj1", AssetType: "storagetransfer.googleapis.com/TransferJob", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"transferSpec":       map[string]any{"gcsDataSource": map[string]any{"bucketName": "bkt1"}, "gcsDataSink": map[string]any{"bucketName": "bkt2"}},
						"notificationConfig": map[string]any{"pubsubTopic": "projects/2002/topics/t1"},
					})}},
					{Name: "//dataproc.googleapis.com/projects/2002/regions/us-central1/clusters/dp1", AssetType: "dataproc.googleapis.com/Cluster", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"config": map[string]any{
							"configBucket":    "bkt1",
							"tempBucket":      "bkt2",
							"metastoreConfig": map[string]any{"dataprocMetastoreService": "projects/2002/locations/us-central1/services/ms1"},
						},
					})}},
					{Name: "//dataproc.googleapis.com/projects/2002/locations/us-central1/batches/dpb1", AssetType: "dataproc.googleapis.com/Batch", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"environmentConfig": map[string]any{"peripheralsConfig": map[string]any{"metastoreService": "projects/2002/locations/us-central1/services/ms1"}},
					})}},
					{Name: "//metastore.googleapis.com/projects/2002/locations/us-central1/services/ms1", AssetType: "metastore.googleapis.com/Service", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"artifactGcsUri": "gs://bkt1/ms"})}},
					{Name: "//composer.googleapis.com/projects/2002/locations/us-central1/environments/comp1", AssetType: "composer.googleapis.com/Environment", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"config": map[string]any{
							"gkeCluster":     "projects/2002/locations/us-central1/clusters/gke1",
							"dagGcsPrefix":   "gs://bkt1/dags",
							"softwareConfig": map[string]any{"envVariables": map[string]any{"OUT": "gs://bkt2"}},
						},
					})}},
					{Name: "//discoveryengine.googleapis.com/projects/2002/locations/global/engines/eng1", AssetType: "discoveryengine.googleapis.com/Engine", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"dataStoreIds": []any{"store1"}})}},
					{Name: "//discoveryengine.googleapis.com/projects/2002/locations/global/dataStores/store1", AssetType: "discoveryengine.googleapis.com/DataStore", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"gcsSource":      map[string]any{"inputUris": []any{"gs://bkt1/docs/*"}},
						"bigquerySource": map[string]any{"projectId": "2002", "datasetId": "ds1"},
					})}},
					{Name: "//dialogflow.googleapis.com/projects/2002/locations/us-central1/agents/ag1", AssetType: "dialogflow.googleapis.com/Agent", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"webhooks": []any{map[string]any{"genericWebService": map[string]any{"uri": "https://us-central1-multi-p.cloudfunctions.net/fn1"}}},
					})}},
					{Name: "//aiplatform.googleapis.com/projects/2002/locations/us-central1/reasoningEngines/re1", AssetType: "aiplatform.googleapis.com/ReasoningEngine", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"spec": map[string]any{"deploymentSpec": map[string]any{
							"env":       []any{map[string]any{"value": "gs://bkt1"}},
							"secretEnv": []any{map[string]any{"secretRef": map[string]any{"secret": "s1"}}},
						}},
					})}},
					{Name: "//aiplatform.googleapis.com/projects/2002/locations/us-central1/featureOnlineStores/fos1", AssetType: "aiplatform.googleapis.com/FeatureOnlineStore"},
					{Name: "//aiplatform.googleapis.com/projects/2002/locations/us-central1/featureOnlineStores/fos1/featureViews/fv1", AssetType: "aiplatform.googleapis.com/FeatureView", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"bigQuerySource": map[string]any{"uri": "bq://multi-p.ds1.tbl"}})}},
					{Name: "//analyticshub.googleapis.com/projects/2002/locations/us/dataExchanges/dx1", AssetType: "analyticshub.googleapis.com/DataExchange"},
					{Name: "//analyticshub.googleapis.com/projects/2002/locations/us/dataExchanges/dx1/listings/l1", AssetType: "analyticshub.googleapis.com/Listing", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"bigqueryDataset": map[string]any{"dataset": "projects/2002/datasets/ds1"}})}},
					{Name: "//dataplex.googleapis.com/projects/2002/locations/us-central1/lakes/lake1", AssetType: "dataplex.googleapis.com/Lake"},
					{Name: "//dataplex.googleapis.com/projects/2002/locations/us-central1/lakes/lake1/zones/z1/assets/a1", AssetType: "dataplex.googleapis.com/Asset", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"resourceSpec": map[string]any{"name": "projects/2002/buckets/bkt1"}})}},
					// Cloud Build trigger variants: GitLab sourceToBuild, triggerTemplate, repositoryEventConfig + inline build steps!
					{Name: "//cloudbuild.googleapis.com/projects/2002/locations/global/triggers/tr-gitlab", AssetType: "cloudbuild.googleapis.com/BuildTrigger", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"id":            "tr-gitlab",
						"sourceToBuild": map[string]any{"uri": "https://gitlab.com/acme/platform.git", "ref": "refs/heads/main"},
						"build": map[string]any{
							"steps": []any{map[string]any{"name": "gcr.io/cloud-builders/gcloud", "args": []any{"container", "clusters", "get-credentials", "--cluster=gke1"}}},
						},
					})}},
					{Name: "//cloudbuild.googleapis.com/projects/2002/locations/global/triggers/tr-csr", AssetType: "cloudbuild.googleapis.com/BuildTrigger", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"id":              "tr-csr",
						"triggerTemplate": map[string]any{"repoName": "github_acme_legacy", "tagName": "^v.*$"},
						"build": map[string]any{
							"steps": []any{map[string]any{"script": "gcloud run deploy kn-svc"}},
						},
					})}},
					{Name: "//cloudbuild.googleapis.com/projects/2002/locations/global/triggers/tr-gen2", AssetType: "cloudbuild.googleapis.com/BuildTrigger", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
						"id":                    "tr-gen2",
						"repositoryEventConfig": map[string]any{"repository": "projects/2002/locations/us-central1/connections/gh/repositories/app-repo", "pullRequest": map[string]any{"branch": "^main$"}},
					})}},
				},
			}, nil
		}
		// IAM_POLICY with Subnetwork attachment role vs non-attachment role.
		return &assetpb.ListAssetsResponse{
			ReadTime: timestamppb.Now(),
			Assets: []*assetpb.Asset{
				{
					Name:      "//compute.googleapis.com/projects/2002/regions/us-central1/subnetworks/sub1",
					AssetType: "compute.googleapis.com/Subnetwork",
					IamPolicy: &iampb.Policy{Bindings: []*iampb.Binding{
						{Role: "roles/compute.networkUser", Members: []string{"serviceAccount:ae@multi-p.iam.gserviceaccount.com"}},
						{Role: "roles/viewer", Members: []string{"serviceAccount:ae@multi-p.iam.gserviceaccount.com"}},
					}},
				},
			},
		}, nil
	}

	clients, _ := newTestClients(t, assetHandler, nil, nil, nil)
	facts, err := Extract(context.Background(), clients, ExtractOptions{Scopes: []string{"organizations/999"}})
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}

	if len(facts.Hierarchy.Organizations) != 1 || facts.Hierarchy.Organizations[0].DisplayName != "Acme Corp" {
		t.Fatalf("missing org hierarchy: %+v", facts.Hierarchy)
	}
	if len(facts.Hierarchy.Folders) != 1 || facts.Hierarchy.Folders[0].DisplayName != "Platform" {
		t.Fatalf("missing folder hierarchy: %+v", facts.Hierarchy)
	}

	for _, want := range []struct {
		from, to string
		kind     FactEdgeKind
	}{
		{"//compute.googleapis.com/projects/multi-p/global/forwardingRules/fr-route", "//cloudfunctions.googleapis.com/projects/multi-p/locations/us-central1/functions/fn1", EdgeConfigRef},
		{"//compute.googleapis.com/projects/multi-p/global/forwardingRules/fr-route", "//appengine.googleapis.com/apps/multi-p/services/web", EdgeConfigRef},
		{"//compute.googleapis.com/projects/multi-p/global/forwardingRules/fr-route", "//compute.googleapis.com/projects/multi-p/zones/us-central1-a/instanceGroupManagers/mig1", EdgeConfigRef},
		{"//appengine.googleapis.com/apps/multi-p/services/web", "//cloudfunctions.googleapis.com/projects/multi-p/locations/us-central1/functions/fn1", EdgeConfigRef},
		{"//run.googleapis.com/projects/multi-p/locations/us-central1/services/kn-svc", "//sqladmin.googleapis.com/projects/multi-p/instances/sql1", EdgeConfigRef},
		{"//run.googleapis.com/projects/multi-p/locations/us-central1/jobs/run-job", "//container.googleapis.com/projects/multi-p/locations/us-central1/clusters/gke1", EdgeConfigRef},
		{"//run.googleapis.com/projects/multi-p/locations/us-central1/jobs/run-job", "//run.googleapis.com/projects/multi-p/locations/us-central1/services/kn-svc", EdgeConfigRef},
		{"//bigquery.googleapis.com/projects/multi-p/datasets/ds1", "//bigqueryconnection.googleapis.com/projects/multi-p/locations/us/connections/bqconn1", EdgeConfigRef},
		{"//bigqueryconnection.googleapis.com/projects/multi-p/locations/us/connections/bqconn1", "//spanner.googleapis.com/projects/multi-p/instances/sp1", EdgeConfigRef},
		{"//storage.googleapis.com/bkt1", "//storagetransfer.googleapis.com/projects/multi-p/transferJobs/tj1", EdgeConfigRef},
		{"//dataproc.googleapis.com/projects/multi-p/regions/us-central1/clusters/dp1", "//metastore.googleapis.com/projects/multi-p/locations/us-central1/services/ms1", EdgeConfigRef},
		{"//composer.googleapis.com/projects/multi-p/locations/us-central1/environments/comp1", "//container.googleapis.com/projects/multi-p/locations/us-central1/clusters/gke1", EdgeConfigRef},
		{"//discoveryengine.googleapis.com/projects/multi-p/locations/global/engines/eng1", "//discoveryengine.googleapis.com/projects/multi-p/locations/global/dataStores/store1", EdgeConfigRef},
		{"//dialogflow.googleapis.com/projects/multi-p/locations/us-central1/agents/ag1", "//cloudfunctions.googleapis.com/projects/multi-p/locations/us-central1/functions/fn1", EdgeConfigRef},
		{"//aiplatform.googleapis.com/projects/multi-p/locations/us-central1/reasoningEngines/re1", "//secretmanager.googleapis.com/projects/multi-p/secrets/s1", EdgeConfigRef},
		{"//bigquery.googleapis.com/projects/multi-p/datasets/ds1", "//aiplatform.googleapis.com/projects/multi-p/locations/us-central1/featureOnlineStores/fos1", EdgeConfigRef},
		{"//bigquery.googleapis.com/projects/multi-p/datasets/ds1", "//analyticshub.googleapis.com/projects/multi-p/locations/us/dataExchanges/dx1", EdgeConfigRef},
		{"//dataplex.googleapis.com/projects/multi-p/locations/us-central1/lakes/lake1", "//storage.googleapis.com/bkt1", EdgeConfigRef},
		{"//networkconnectivity.googleapis.com/projects/multi-p/locations/global/hubs/hub1", "//compute.googleapis.com/projects/multi-p/global/networks/vpc1", EdgeConfigRef},
		{"//gitlab.com/acme/platform", "//cloudbuild.googleapis.com/projects/multi-p/locations/global/triggers/tr-gitlab", EdgeCloudBuildRepo},
		{"//cloudbuild.googleapis.com/projects/multi-p/locations/global/triggers/tr-gitlab", "//container.googleapis.com/projects/multi-p/locations/us-central1/clusters/gke1", EdgeCloudBuildStepRef},
		{"//github.com/acme/legacy", "//cloudbuild.googleapis.com/projects/multi-p/locations/global/triggers/tr-csr", EdgeCloudBuildRepo},
		{"//cloudbuild.googleapis.com/projects/multi-p/locations/global/triggers/tr-csr", "//run.googleapis.com/projects/multi-p/locations/us-central1/services/kn-svc", EdgeCloudBuildStepRef},
		{"//iam.googleapis.com/projects/multi-p/serviceAccounts/ae@multi-p.iam.gserviceaccount.com", "//compute.googleapis.com/projects/multi-p/regions/us-central1/subnetworks/sub1", EdgeIAMResourceBinding},
	} {
		if !hasEdge(facts.Edges, want.from, want.to, want.kind) {
			t.Errorf("missing edge %s -[%s]-> %s", want.from, want.kind, want.to)
		}
	}
}

func TestEnricherErrorHandlingAndPagination(t *testing.T) {
	// Test BQ Load/Copy, pagination, Dataflow/BQ SERVICE_DISABLED degradation, Vertex HTTP error parsing, and Monitoring NotFound.
	assetHandler := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		if req.ContentType == assetpb.ContentType_RESOURCE {
			return &assetpb.ListAssetsResponse{
				ReadTime: timestamppb.Now(),
				Assets: []*assetpb.Asset{
					{Name: "//cloudresourcemanager.googleapis.com/projects/p-err", AssetType: "cloudresourcemanager.googleapis.com/Project", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "p-err", "projectNumber": "8001"})}},
					{Name: "//bigquery.googleapis.com/projects/p-err/datasets/src_ds", AssetType: "bigquery.googleapis.com/Dataset"},
					{Name: "//bigquery.googleapis.com/projects/p-err/datasets/dst_ds", AssetType: "bigquery.googleapis.com/Dataset"},
					{Name: "//iam.googleapis.com/projects/p-err/serviceAccounts/svc@p-err.iam.gserviceaccount.com", AssetType: "iam.googleapis.com/ServiceAccount", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"email": "svc@p-err.iam.gserviceaccount.com"})}},
				},
			}, nil
		}
		return &assetpb.ListAssetsResponse{ReadTime: timestamppb.Now()}, nil
	}
	monHandler := func(_ context.Context, req *monitoringpb.ListTimeSeriesRequest) (*monitoringpb.ListTimeSeriesResponse, error) {
		if strings.Contains(req.Filter, "PublisherModel") {
			return nil, status.Error(codes.NotFound, "metric descriptor not found")
		}
		return &monitoringpb.ListTimeSeriesResponse{
			TimeSeries: []*monitoringpb.TimeSeries{{
				Resource: &monitoredres.MonitoredResource{Labels: map[string]string{
					"project_id": "p-err", "service": "bigquery.googleapis.com", "credential_id": "user:human", "method": "jobs.insert",
				}},
				Points: []*monitoringpb.Point{{Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 5}}}},
			}},
		}, nil
	}

	var bqPage atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/projects/p-err/jobs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if bqPage.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(&bigquery.JobList{
				NextPageToken: "page2",
				Jobs: []*bigquery.JobListJobs{{
					JobReference: &bigquery.JobReference{JobId: "copy-1"},
					UserEmail:    "svc@p-err.iam.gserviceaccount.com",
					Configuration: &bigquery.JobConfiguration{
						Copy: &bigquery.JobConfigurationTableCopy{
							SourceTable:      &bigquery.TableReference{ProjectId: "p-err", DatasetId: "src_ds", TableId: "t1"},
							SourceTables:     []*bigquery.TableReference{{ProjectId: "p-err", DatasetId: "src_ds", TableId: "t2"}},
							DestinationTable: &bigquery.TableReference{ProjectId: "p-err", DatasetId: "dst_ds", TableId: "t3"},
						},
					},
				}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(&bigquery.JobList{
			Jobs: []*bigquery.JobListJobs{{
				Id:        "load-1",
				UserEmail: "svc@p-err.iam.gserviceaccount.com",
				Configuration: &bigquery.JobConfiguration{
					Load: &bigquery.JobConfigurationLoad{
						DestinationTable: &bigquery.TableReference{ProjectId: "p-err", DatasetId: "dst_ds", TableId: "t4"},
					},
				},
			}},
		})
	})
	mux.HandleFunc("/v1b3/projects/p-err/locations/-/jobs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"Dataflow API disabled","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SERVICE_DISABLED","domain":"googleapis.com","metadata":{"consumer":"projects/p-err"}}]}}`))
	})
	mux.HandleFunc("/v1/projects/ai-err:testIamPermissions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"VPC-SC blocked","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SECURITY_POLICY_VIOLATED","metadata":{"vpcServiceControlsUniqueIdentifier":"v-uuid-1"}}]}}`))
	})
	mux.HandleFunc("/v1/projects/ai-err/locations/global/publishers/google/models/gemini-4-carbon", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"denied"}}`))
	})

	clients, httpSrv := newTestClients(t, assetHandler, monHandler, mux, nil)
	facts, err := Extract(context.Background(), clients, ExtractOptions{Scopes: []string{"projects/p-err"}})
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	if !hasEdge(facts.Edges, "//bigquery.googleapis.com/projects/p-err/datasets/src_ds", "//bigquery.googleapis.com/projects/p-err/datasets/dst_ds", EdgeBQLineage) {
		t.Error("missing BQ_LINEAGE from paginated Copy job")
	}
	if !hasEdge(facts.Edges, "//iam.googleapis.com/projects/p-err/serviceAccounts/svc@p-err.iam.gserviceaccount.com", "//bigquery.googleapis.com/projects/p-err/datasets/dst_ds", EdgeBQLineage) {
		t.Error("missing BQ_LINEAGE SA -> dst_ds from Load/Copy job")
	}
	var sawDFDisabled bool
	for _, ce := range facts.CoverageLedger {
		if ce.Source == "dataflow_jobs" && ce.State == CoverageServiceDisabled {
			sawDFDisabled = true
		}
	}
	if !sawDFDisabled {
		t.Fatalf("expected dataflow_jobs SERVICE_DISABLED in coverage ledger, got %+v", facts.CoverageLedger)
	}

	// Exercise parseHTTPError via RunPreflight against failing Vertex CRM & model endpoints.
	rep, err := RunPreflight(context.Background(), clients, []string{"projects/p-err"}, VertexPreflightOptions{
		Project:        "ai-err",
		HTTPClient:     httpSrv.Client(),
		CRMEndpoint:    httpSrv.URL,
		VertexEndpoint: httpSrv.URL,
	})
	if err != nil {
		t.Fatalf("degraded RunPreflight should succeed without fatal error, got: %v", err)
	}
	var sawVertexVPCSC, sawVertexModelDenied bool
	for _, p := range rep.Probes {
		if p.Source == "vertex.iam" && p.Status == ProbeVPCSC && p.VPCSCUUID == "v-uuid-1" {
			sawVertexVPCSC = true
		}
		if p.Source == "vertex.model" && p.Status == ProbeDenied {
			sawVertexModelDenied = true
		}
	}
	if !sawVertexVPCSC || !sawVertexModelDenied {
		t.Fatalf("expected vertex.iam VPC_SC_BLOCKED and vertex.model DENIED, got %+v", rep.Probes)
	}
}

func TestExtractIAMServiceAgentsAndNonEmittingBindings(t *testing.T) {
	if !isDefaultServiceAccount("100001@cloudbuild.gserviceaccount.com") {
		t.Fatal("expected @cloudbuild.gserviceaccount.com to be treated as default service account")
	}

	assetHandler := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		if req.ContentType == assetpb.ContentType_RESOURCE {
			return &assetpb.ListAssetsResponse{
				ReadTime: timestamppb.Now(),
				Assets: []*assetpb.Asset{
					{
						Name:      "//cloudresourcemanager.googleapis.com/projects/100001",
						AssetType: "cloudresourcemanager.googleapis.com/Project",
						Resource:  &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "p1", "projectNumber": "100001"})},
					},
					{Name: "//storage.googleapis.com/p1-bkt", AssetType: "storage.googleapis.com/Bucket", Ancestors: []string{"projects/100001"}},
					{Name: "//secretmanager.googleapis.com/projects/100001/secrets/s1", AssetType: "secretmanager.googleapis.com/Secret", Ancestors: []string{"projects/100001"}},
					{Name: "//secretmanager.googleapis.com/projects/100001/secrets/s2", AssetType: "secretmanager.googleapis.com/Secret", Ancestors: []string{"projects/100001"}},
				},
			}, nil
		}
		return &assetpb.ListAssetsResponse{
			Assets: []*assetpb.Asset{
				{
					Name:      "//storage.googleapis.com/p1-bkt",
					AssetType: "storage.googleapis.com/Bucket",
					Ancestors: []string{"projects/100001"},
					IamPolicy: &iampb.Policy{Bindings: []*iampb.Binding{
						{
							Role: "roles/storage.objectAdmin",
							Members: []string{
								"serviceAccount:service-100001@gcp-sa-cloudbuild.iam.gserviceaccount.com",
								"serviceAccount:service-100001@container-engine-robot.iam.gserviceaccount.com",
								"serviceAccount:100001@cloudbuild.gserviceaccount.com",
								"serviceAccount:100001@cloudservices.gserviceaccount.com",
								"serviceAccount:service-999999@gcp-sa-cloudbuild.iam.gserviceaccount.com",
								"serviceAccount:valid@ext-real.iam.gserviceaccount.com",
							},
						},
					}},
				},
				{
					Name:      "//cloudresourcemanager.googleapis.com/organizations/1234",
					AssetType: "cloudresourcemanager.googleapis.com/Organization",
					IamPolicy: &iampb.Policy{Bindings: []*iampb.Binding{
						{Role: "roles/secretmanager.secretAccessor", Members: []string{"serviceAccount:ext-sa@ext-org-noop.iam.gserviceaccount.com"}},
					}},
				},
				{
					Name:      "//cloudresourcemanager.googleapis.com/folders/5678",
					AssetType: "cloudresourcemanager.googleapis.com/Folder",
					IamPolicy: &iampb.Policy{Bindings: []*iampb.Binding{
						{Role: "roles/storage.objectViewer", Members: []string{"serviceAccount:ext-sa@ext-folder-noop.iam.gserviceaccount.com"}},
					}},
				},
				{
					Name:      "//cloudresourcemanager.googleapis.com/projects/100001",
					AssetType: "cloudresourcemanager.googleapis.com/Project",
					IamPolicy: &iampb.Policy{Bindings: []*iampb.Binding{
						// Ambiguous across s1 & s2 -> non-emitting, must NOT register ext-proj-noop!
						{Role: "roles/secretmanager.secretAccessor", Members: []string{"serviceAccount:ext-sa@ext-proj-noop.iam.gserviceaccount.com"}},
					}},
				},
				{
					Name:      "//storage.googleapis.com/nonexistent-bkt",
					AssetType: "storage.googleapis.com/Bucket",
					Ancestors: []string{"projects/100001"},
					IamPolicy: &iampb.Policy{Bindings: []*iampb.Binding{
						{Role: "roles/storage.objectViewer", Members: []string{"serviceAccount:ext-sa@ext-missing-noop.iam.gserviceaccount.com"}},
					}},
				},
			},
		}, nil
	}

	clients, _ := newTestClients(t, assetHandler, nil, nil, nil)
	facts, err := Extract(context.Background(), clients, ExtractOptions{Scopes: []string{"projects/p1"}})
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}

	// Only ext-real should appear as an out-of-scope stub/hint, referencing p1-bkt.
	if len(facts.OutOfScopeTargets) != 1 {
		t.Fatalf("expected exactly 1 out-of-scope hint (ext-real), got %+v", facts.OutOfScopeTargets)
	}
	hint := facts.OutOfScopeTargets[0]
	if hint.TargetProjectID != "ext-real" || hint.SourceKind != "iam_cross_project" {
		t.Fatalf("unexpected hint: %+v", hint)
	}
	if !slices.Equal(hint.ReferencedByURIs, []string{"//storage.googleapis.com/p1-bkt"}) {
		t.Fatalf("expected ReferencedByURIs=[//storage.googleapis.com/p1-bkt], got %v", hint.ReferencedByURIs)
	}
	for _, r := range facts.Resources {
		if r.IsExternal && r.ProjectID != "ext-real" {
			t.Errorf("unexpected external stub resource: %+v", r)
		}
		if r.AssetType == "iam.googleapis.com/ServiceAccount" {
			t.Errorf("unexpected phantom ServiceAccount retained: %+v", r)
		}
	}
	if !hasEdge(facts.Edges, "//cloudresourcemanager.googleapis.com/projects/ext-real", "//storage.googleapis.com/p1-bkt", EdgeIAMResourceBinding) {
		t.Errorf("expected IAM_RESOURCE_BINDING edge ext-real -> p1-bkt")
	}
}

func TestExtractK8sDefaultServiceAccountNoGCECollision(t *testing.T) {
	clusterURI := "//container.googleapis.com/projects/k1/locations/us-central1/clusters/c1"
	depURI := clusterURI + "/k8s/namespaces/app/apps/deployments/web"
	gceSA := "200002-compute@developer.gserviceaccount.com"

	assetHandler := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		if req.ContentType == assetpb.ContentType_RESOURCE {
			return &assetpb.ListAssetsResponse{
				ReadTime: timestamppb.Now(),
				Assets: []*assetpb.Asset{
					{
						Name:      "//cloudresourcemanager.googleapis.com/projects/200002",
						AssetType: "cloudresourcemanager.googleapis.com/Project",
						Resource:  &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "k1", "projectNumber": "200002"})},
					},
					{
						Name:      clusterURI,
						AssetType: "container.googleapis.com/Cluster",
						Ancestors: []string{"projects/200002"},
						Resource:  &assetpb.Resource{Location: "us-central1", Data: mustStruct(t, map[string]any{"name": "c1"})},
					},
					{
						Name:      depURI,
						AssetType: "apps.k8s.io/Deployment",
						Ancestors: []string{"projects/200002"},
						Resource: &assetpb.Resource{
							Parent: clusterURI,
							Data: mustStruct(t, map[string]any{
								"metadata": map[string]any{"name": "web", "namespace": "app"},
								"spec": map[string]any{
									"replicas": 2,
									"template": map[string]any{
										"spec": map[string]any{
											"serviceAccountName": "default",
										},
									},
								},
							}),
						},
					},
					{
						Name:      "//iam.googleapis.com/projects/200002/serviceAccounts/" + gceSA,
						AssetType: "iam.googleapis.com/ServiceAccount",
						Ancestors: []string{"projects/200002"},
						Resource:  &assetpb.Resource{Data: mustStruct(t, map[string]any{"email": gceSA, "uniqueId": "22222"})},
					},
					{
						Name:      "//storage.googleapis.com/k1-only-bkt",
						AssetType: "storage.googleapis.com/Bucket",
						Ancestors: []string{"projects/200002"},
					},
				},
			}, nil
		}
		return &assetpb.ListAssetsResponse{}, nil
	}

	monHandler := func(_ context.Context, req *monitoringpb.ListTimeSeriesRequest) (*monitoringpb.ListTimeSeriesResponse, error) {
		if strings.Contains(req.Filter, "consumed_api") {
			return &monitoringpb.ListTimeSeriesResponse{
				TimeSeries: []*monitoringpb.TimeSeries{{
					Resource: &monitoredres.MonitoredResource{Labels: map[string]string{
						"project_id":    "k1",
						"service":       "storage.googleapis.com",
						"credential_id": "serviceaccount:22222",
						"method":        "storage.objects.get",
					}},
					Points: []*monitoringpb.Point{{Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 25}}}},
				}},
			}, nil
		}
		return &monitoringpb.ListTimeSeriesResponse{}, nil
	}

	clients, _ := newTestClients(t, assetHandler, monHandler, nil, nil)
	facts, err := Extract(context.Background(), clients, ExtractOptions{Scopes: []string{"projects/k1"}})
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}

	dep := findResource(facts.Resources, depURI)
	if dep == nil {
		t.Fatalf("expected Deployment %s", depURI)
	}
	if _, hasSA := dep.Attributes["service_account"]; hasSA {
		t.Errorf("K8s Deployment without WIF must not set attrs[service_account], got %v", dep.Attributes["service_account"])
	}
	if dep.Attributes["k8s_service_account"] != "default" {
		t.Errorf("expected k8s_service_account=default, got %v", dep.Attributes["k8s_service_account"])
	}
	if findResource(facts.Resources, "//iam.googleapis.com/projects/k1/serviceAccounts/"+gceSA) != nil {
		t.Errorf("unreferenced GCE default SA %s should have been pruned, not retained by K8s serviceAccountName=default", gceSA)
	}
	if hasEdge(facts.Edges, depURI, "//storage.googleapis.com/k1-only-bkt", EdgeMonitoringTraffic) {
		t.Errorf("K8s Deployment must not receive GCE default SA monitoring traffic")
	}
}

func TestExtractGKEClusterBareNetworkAndSubnetwork(t *testing.T) {
	// Standalone ProjectAttributes without region in data must NEVER fabricate /global/networks/<subnetwork>.
	rawAttrs := ProjectAttributes("container.googleapis.com/Cluster", mustStruct(t, map[string]any{
		"network":    "cicd",
		"subnetwork": "cicd-k8s",
	}), "cicd-p", nil)
	if rawAttrs["vpc"] != "//compute.googleapis.com/projects/cicd-p/global/networks/cicd" {
		t.Fatalf("unexpected vpc: %v", rawAttrs["vpc"])
	}
	if rawAttrs["subnet"] != "cicd-k8s" {
		t.Fatalf("expected bare subnet without region to remain raw 'cicd-k8s', got %v", rawAttrs["subnet"])
	}

	clusterURI := "//container.googleapis.com/projects/cicd-p/locations/europe-southwest1-b/clusters/cicd"
	vpcURI := "//compute.googleapis.com/projects/cicd-p/global/networks/cicd"
	subURI := "//compute.googleapis.com/projects/cicd-p/regions/europe-southwest1/subnetworks/cicd-k8s"
	vmURI := "//compute.googleapis.com/projects/cicd-p/zones/global/instances/vm-bare"

	assetHandler := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		if req.ContentType == assetpb.ContentType_RESOURCE {
			return &assetpb.ListAssetsResponse{
				ReadTime: timestamppb.Now(),
				Assets: []*assetpb.Asset{
					{Name: "//cloudresourcemanager.googleapis.com/projects/cicd-p", AssetType: "cloudresourcemanager.googleapis.com/Project", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "cicd-p"})}},
					{Name: vpcURI, AssetType: "compute.googleapis.com/Network", Ancestors: []string{"projects/cicd-p"}, Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"name": "cicd"})}},
					{Name: subURI, AssetType: "compute.googleapis.com/Subnetwork", Ancestors: []string{"projects/cicd-p"}, Resource: &assetpb.Resource{Location: "europe-southwest1", Data: mustStruct(t, map[string]any{"name": "cicd-k8s", "network": vpcURI})}},
					{
						Name:      clusterURI,
						AssetType: "container.googleapis.com/Cluster",
						Ancestors: []string{"projects/cicd-p"},
						Resource: &assetpb.Resource{
							Location: "europe-southwest1-b",
							Data: mustStruct(t, map[string]any{
								"name":       "cicd",
								"network":    "cicd",
								"subnetwork": "cicd-k8s",
							}),
						},
					},
					{
						// Instance with global/unknown region and bare subnetwork: ParentURI must fall back to canonical vpc, not bare subnet.
						Name:      vmURI,
						AssetType: "compute.googleapis.com/Instance",
						Ancestors: []string{"projects/cicd-p"},
						Resource: &assetpb.Resource{
							Location: "global",
							Data: mustStruct(t, map[string]any{
								"name": "vm-bare",
								"networkInterfaces": []any{map[string]any{
									"network":    "cicd",
									"subnetwork": "unresolved-sub",
								}},
							}),
						},
					},
				},
			}, nil
		}
		return &assetpb.ListAssetsResponse{}, nil
	}

	clients, _ := newTestClients(t, assetHandler, nil, nil, nil)
	facts, err := Extract(context.Background(), clients, ExtractOptions{Scopes: []string{"projects/cicd-p"}})
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}

	cl := findResource(facts.Resources, clusterURI)
	if cl == nil {
		t.Fatalf("expected cluster %s", clusterURI)
	}
	if cl.Attributes["vpc"] != vpcURI {
		t.Errorf("cluster vpc = %v, want %s", cl.Attributes["vpc"], vpcURI)
	}
	if cl.Attributes["subnet"] != subURI {
		t.Errorf("cluster subnet = %v, want %s", cl.Attributes["subnet"], subURI)
	}
	if cl.ParentURI != subURI {
		t.Errorf("cluster parent_uri = %q, want %q", cl.ParentURI, subURI)
	}

	vm := findResource(facts.Resources, vmURI)
	if vm == nil {
		t.Fatalf("expected vm %s", vmURI)
	}
	if vm.ParentURI != vpcURI {
		t.Errorf("vm parent_uri = %q, want canonical vpc %q (not bare subnet)", vm.ParentURI, vpcURI)
	}
}

func TestExtractInboundCrossProjectEdges(t *testing.T) {
	subURI := "//pubsub.googleapis.com/projects/sub-p/subscriptions/my-sub"
	orphanSubURI := "//pubsub.googleapis.com/projects/sub-p/subscriptions/orphan-sub"
	goldDS := "//bigquery.googleapis.com/projects/sub-p/datasets/gold_ds"

	assetHandler := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		if req.ContentType == assetpb.ContentType_RESOURCE {
			return &assetpb.ListAssetsResponse{
				ReadTime: timestamppb.Now(),
				Assets: []*assetpb.Asset{
					{Name: "//cloudresourcemanager.googleapis.com/projects/sub-p", AssetType: "cloudresourcemanager.googleapis.com/Project", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "sub-p"})}},
					{
						Name:      subURI,
						AssetType: "pubsub.googleapis.com/Subscription",
						Ancestors: []string{"projects/sub-p"},
						Resource:  &assetpb.Resource{Data: mustStruct(t, map[string]any{"name": "projects/sub-p/subscriptions/my-sub", "topic": "projects/ext-pub/topics/orders"})},
					},
					{
						Name:      orphanSubURI,
						AssetType: "pubsub.googleapis.com/Subscription",
						Ancestors: []string{"projects/sub-p"},
						Resource:  &assetpb.Resource{Data: mustStruct(t, map[string]any{"name": "projects/sub-p/subscriptions/orphan-sub", "topic": "projects/sub-p/topics/missing-topic"})},
					},
					{
						Name:      goldDS,
						AssetType: "bigquery.googleapis.com/Dataset",
						Ancestors: []string{"projects/sub-p"},
					},
				},
			}, nil
		}
		return &assetpb.ListAssetsResponse{}, nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/projects/sub-p/jobs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&bigquery.JobList{
			Jobs: []*bigquery.JobListJobs{{
				Id: "bq-cross-1",
				Statistics: &bigquery.JobStatistics{
					Query: &bigquery.JobStatistics2{
						ReferencedTables: []*bigquery.TableReference{
							{ProjectId: "ext-bq", DatasetId: "raw_ext", TableId: "t1"},
						},
					},
				},
				Configuration: &bigquery.JobConfiguration{
					Query: &bigquery.JobConfigurationQuery{
						DestinationTable: &bigquery.TableReference{ProjectId: "sub-p", DatasetId: "gold_ds", TableId: "out"},
					},
				},
			}},
		})
	})

	clients, _ := newTestClients(t, assetHandler, nil, mux, nil)
	facts, err := Extract(context.Background(), clients, ExtractOptions{Scopes: []string{"projects/sub-p"}})
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}

	extPubStub := "//cloudresourcemanager.googleapis.com/projects/ext-pub"
	extBQStub := "//cloudresourcemanager.googleapis.com/projects/ext-bq"

	if findResource(facts.Resources, extPubStub) == nil {
		t.Fatalf("expected external stub %s", extPubStub)
	}
	if !hasEdge(facts.Edges, extPubStub, subURI, EdgeConfigRef) {
		t.Errorf("expected inbound cross-project CONFIG_REF %s -> %s", extPubStub, subURI)
	}
	if findResource(facts.Resources, extBQStub) == nil {
		t.Fatalf("expected external stub %s", extBQStub)
	}
	if !hasEdge(facts.Edges, extBQStub, goldDS, EdgeBQLineage) {
		t.Errorf("expected inbound cross-project BQ_LINEAGE %s -> %s", extBQStub, goldDS)
	}

	// Verify both OutOfScopeTargets record the in-scope resource URI as ReferencedByURIs.
	hintsByProj := make(map[string]OutOfScopeHint)
	for _, h := range facts.OutOfScopeTargets {
		hintsByProj[h.TargetProjectID] = h
	}
	if !slices.Contains(hintsByProj["ext-pub"].ReferencedByURIs, subURI) {
		t.Errorf("ext-pub hint missing %s: %+v", subURI, hintsByProj["ext-pub"])
	}
	if !slices.Contains(hintsByProj["ext-bq"].ReferencedByURIs, goldDS) {
		t.Errorf("ext-bq hint missing %s: %+v", goldDS, hintsByProj["ext-bq"])
	}

	// Missing in-scope topic must not emit any edge to orphanSubURI.
	for _, e := range facts.Edges {
		if e.ToURI == orphanSubURI || e.FromURI == orphanSubURI {
			t.Errorf("unexpected edge involving orphan-sub with missing in-scope topic: %+v", e)
		}
	}
}

func TestExtractSanitizedRefsRegexEdgeCases(t *testing.T) {
	input := "-instances=acme-cicd:europe-west1:sonarqube=tcp:5432,other-p:us-central1:db2=tcp:5433 --project=proj-a --project=proj-b --cluster=c1 --cluster=c2"
	envRefs, _ := ExtractSanitizedRefs(input, nil)

	want := []string{
		"--cluster=c1",
		"--cluster=c2",
		"--project=proj-a",
		"--project=proj-b",
		"//sqladmin.googleapis.com/projects/other-p/instances/db2",
		"//sqladmin.googleapis.com/projects/acme-cicd/instances/sonarqube",
	}
	for _, w := range want {
		if !slices.Contains(envRefs, w) {
			t.Errorf("expected ref %q in %v", w, envRefs)
		}
	}
}

func TestExtractCloudBuildEmptyRefSuccessEvictsOlderBuild(t *testing.T) {
	trigURI := "//cloudbuild.googleapis.com/projects/cb-p/locations/global/triggers/trig-evict"
	bktURI := "//storage.googleapis.com/stale-bkt"

	assetHandler := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		if req.ContentType == assetpb.ContentType_RESOURCE {
			return &assetpb.ListAssetsResponse{
				ReadTime: timestamppb.Now(),
				Assets: []*assetpb.Asset{
					{Name: "//cloudresourcemanager.googleapis.com/projects/cb-p", AssetType: "cloudresourcemanager.googleapis.com/Project", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "cb-p"})}},
					{Name: bktURI, AssetType: "storage.googleapis.com/Bucket", Ancestors: []string{"projects/cb-p"}},
					{
						Name:      trigURI,
						AssetType: "cloudbuild.googleapis.com/BuildTrigger",
						Ancestors: []string{"projects/cb-p"},
						Resource:  &assetpb.Resource{Data: mustStruct(t, map[string]any{"id": "trig-evict", "name": "deploy"})},
					},
					{
						Name:      "//cloudbuild.googleapis.com/projects/cb-p/builds/b-old",
						AssetType: "cloudbuild.googleapis.com/Build",
						Ancestors: []string{"projects/cb-p"},
						Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
							"status":         "SUCCESS",
							"buildTriggerId": "trig-evict",
							"createTime":     "2026-01-01T00:00:00Z",
							"steps":          []any{map[string]any{"args": []any{"gs://stale-bkt/old.tar.gz"}}},
						})},
					},
					{
						Name:      "//cloudbuild.googleapis.com/projects/cb-p/builds/b-new",
						AssetType: "cloudbuild.googleapis.com/Build",
						Ancestors: []string{"projects/cb-p"},
						Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
							"status":         "SUCCESS",
							"buildTriggerId": "trig-evict",
							"createTime":     "2026-02-01T00:00:00Z",
							"steps":          []any{map[string]any{"name": "ubuntu", "args": []any{"echo", "ok"}}},
						})},
					},
				},
			}, nil
		}
		return &assetpb.ListAssetsResponse{}, nil
	}

	clients, _ := newTestClients(t, assetHandler, nil, nil, nil)
	facts, err := Extract(context.Background(), clients, ExtractOptions{Scopes: []string{"projects/cb-p"}, CloudBuildRuns: 1})
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	if hasEdge(facts.Edges, trigURI, bktURI, EdgeCloudBuildStepRef) {
		t.Errorf("newer empty-ref SUCCESS build should have evicted older build's stale bucket ref")
	}
}

func TestExtractDefaultVPCRetainedViaSubnetAndOverlappingScopeDedup(t *testing.T) {
	defVPC := "//compute.googleapis.com/projects/ov-p/global/networks/default"
	defSub := "//compute.googleapis.com/projects/ov-p/regions/us-central1/subnetworks/default"
	vmURI := "//compute.googleapis.com/projects/ov-p/zones/us-central1-a/instances/vm-1"

	assetHandler := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		if req.ContentType == assetpb.ContentType_RESOURCE {
			return &assetpb.ListAssetsResponse{
				ReadTime: timestamppb.Now(),
				Assets: []*assetpb.Asset{
					{Name: "//cloudresourcemanager.googleapis.com/projects/ov-p", AssetType: "cloudresourcemanager.googleapis.com/Project", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "ov-p"})}},
					{Name: defVPC, AssetType: "compute.googleapis.com/Network", Ancestors: []string{"projects/ov-p"}, Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"name": "default"})}},
					{Name: defSub, AssetType: "compute.googleapis.com/Subnetwork", Ancestors: []string{"projects/ov-p"}, Resource: &assetpb.Resource{Location: "us-central1", Data: mustStruct(t, map[string]any{"name": "default", "network": defVPC})}},
					{
						Name:      vmURI,
						AssetType: "compute.googleapis.com/Instance",
						Ancestors: []string{"projects/ov-p"},
						Resource: &assetpb.Resource{
							Location: "us-central1-a",
							Data: mustStruct(t, map[string]any{
								"name":              "vm-1",
								"networkInterfaces": []any{map[string]any{"subnetwork": defSub}},
							}),
						},
					},
				},
			}, nil
		}
		return &assetpb.ListAssetsResponse{}, nil
	}

	clients, _ := newTestClients(t, assetHandler, nil, nil, nil)
	facts, err := Extract(context.Background(), clients, ExtractOptions{Scopes: []string{"folders/f1", "projects/ov-p"}})
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}

	if findResource(facts.Resources, defSub) == nil {
		t.Fatalf("expected default subnet %s to be retained", defSub)
	}
	if findResource(facts.Resources, defVPC) == nil {
		t.Fatalf("expected default VPC %s to be retained via referenced default subnet", defVPC)
	}
	seen := make(map[string]bool)
	for _, r := range facts.Resources {
		if seen[r.URI] {
			t.Errorf("duplicate FactResource URI %s across overlapping scopes", r.URI)
		}
		seen[r.URI] = true
	}
}

func TestExtractCrossProjectConnectorMultiProjectGKEAndRecanonicalizedSpoke(t *testing.T) {
	hostVPC := "//compute.googleapis.com/projects/host-p/global/networks/shared-vpc"
	hostSub := "//compute.googleapis.com/projects/host-p/regions/us-central1/subnetworks/conn-sub"
	connURI := "//vpcaccess.googleapis.com/projects/svc-p/locations/us-central1/connectors/svpc-conn"
	hubURI := "//networkconnectivity.googleapis.com/projects/host-p/locations/global/hubs/core-hub"
	vpnGwURI := "//compute.googleapis.com/projects/host-p/regions/us-central1/vpnGateways/gw-1"
	icURI := "//compute.googleapis.com/projects/host-p/regions/us-central1/interconnectAttachments/ic-1"
	adbCluster := "//alloydb.googleapis.com/projects/host-p/locations/us-central1/clusters/adb-1"

	clA := "//container.googleapis.com/projects/gke-a/locations/us-central1/clusters/c-a"
	clB := "//container.googleapis.com/projects/gke-b/locations/us-central1/clusters/c-b"
	frA := "//compute.googleapis.com/projects/gke-a/global/forwardingRules/shared-fr"
	frB := "//compute.googleapis.com/projects/gke-b/global/forwardingRules/shared-fr"
	svcHostNEG := clA + "/k8s/namespaces/app/services/cross-svc"

	assetHandler := func(_ context.Context, req *assetpb.ListAssetsRequest) (*assetpb.ListAssetsResponse, error) {
		if req.ContentType == assetpb.ContentType_RESOURCE {
			return &assetpb.ListAssetsResponse{
				ReadTime: timestamppb.Now(),
				Assets: []*assetpb.Asset{
					// Return assets BEFORE Project records so numeric project numbers (555001) test reconcilePostCAI!
					{
						Name:      connURI,
						AssetType: "vpcaccess.googleapis.com/Connector",
						Ancestors: []string{"projects/svc-p"},
						Resource: &assetpb.Resource{
							Location: "us-central1",
							Data: mustStruct(t, map[string]any{
								"subnet": map[string]any{"name": "conn-sub", "projectId": "555001"},
							}),
						},
					},
					{
						Name:      "//networkconnectivity.googleapis.com/projects/555001/locations/us-central1/spokes/s-num",
						AssetType: "networkconnectivity.googleapis.com/Spoke",
						Ancestors: []string{"projects/555001"},
						Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
							"hub":                           "projects/555001/locations/global/hubs/core-hub",
							"linkedVpnTunnels":              map[string]any{"uris": []any{"https://www.googleapis.com/compute/v1/projects/555001/regions/us-central1/vpnTunnels/tun-1"}},
							"linkedInterconnectAttachments": map[string]any{"uris": []any{"https://www.googleapis.com/compute/v1/projects/555001/regions/us-central1/interconnectAttachments/ic-1"}},
						})},
					},
					{
						Name:      "//compute.googleapis.com/projects/555001/regions/us-central1/vpnTunnels/tun-1",
						AssetType: "compute.googleapis.com/VpnTunnel",
						Ancestors: []string{"projects/555001"},
						Resource:  &assetpb.Resource{Data: mustStruct(t, map[string]any{"vpnGateway": "projects/555001/regions/us-central1/vpnGateways/gw-1"})},
					},
					{Name: hubURI, AssetType: "networkconnectivity.googleapis.com/Hub", Ancestors: []string{"projects/555001"}},
					{Name: vpnGwURI, AssetType: "compute.googleapis.com/VpnGateway", Ancestors: []string{"projects/555001"}},
					{Name: icURI, AssetType: "compute.googleapis.com/InterconnectAttachment", Ancestors: []string{"projects/555001"}},
					// AlloyDB Cluster + 2 Instances (deterministic lowest-URI private IP selection).
					{Name: adbCluster, AssetType: "alloydb.googleapis.com/Cluster", Ancestors: []string{"projects/555001"}, Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"networkConfig": map[string]any{"network": hostVPC}})}},
					{Name: adbCluster + "/instances/inst-b", AssetType: "alloydb.googleapis.com/Instance", Ancestors: []string{"projects/555001"}, Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"ipAddress": "10.9.0.2"})}},
					{Name: adbCluster + "/instances/inst-a", AssetType: "alloydb.googleapis.com/Instance", Ancestors: []string{"projects/555001"}, Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"ipAddress": "10.9.0.1"})}},
					// Projects & Shared VPC
					{Name: "//cloudresourcemanager.googleapis.com/projects/555001", AssetType: "cloudresourcemanager.googleapis.com/Project", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "host-p", "projectNumber": "555001"})}},
					{Name: "//cloudresourcemanager.googleapis.com/projects/svc-p", AssetType: "cloudresourcemanager.googleapis.com/Project", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "svc-p"})}},
					{Name: "//cloudresourcemanager.googleapis.com/projects/gke-a", AssetType: "cloudresourcemanager.googleapis.com/Project", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "gke-a"})}},
					{Name: "//cloudresourcemanager.googleapis.com/projects/gke-b", AssetType: "cloudresourcemanager.googleapis.com/Project", Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"projectId": "gke-b"})}},
					{Name: hostVPC, AssetType: "compute.googleapis.com/Network", Ancestors: []string{"projects/555001"}, Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{"name": "shared-vpc"})}},
					{Name: hostSub, AssetType: "compute.googleapis.com/Subnetwork", Ancestors: []string{"projects/555001"}, Resource: &assetpb.Resource{Location: "us-central1", Data: mustStruct(t, map[string]any{"name": "conn-sub", "network": hostVPC})}},
					// Multi-project GKE FR name isolation (gke-a & gke-b both use FR short name "shared-fr") + cross-project NEG lookup (frB -> NEG in gke-a).
					{Name: clA, AssetType: "container.googleapis.com/Cluster", Ancestors: []string{"projects/gke-a"}},
					{Name: clB, AssetType: "container.googleapis.com/Cluster", Ancestors: []string{"projects/gke-b"}},
					{
						Name:      svcHostNEG,
						AssetType: "k8s.io/Service",
						Ancestors: []string{"projects/gke-a"},
						Resource: &assetpb.Resource{
							Parent: clA,
							Data: mustStruct(t, map[string]any{
								"metadata": map[string]any{
									"name":        "cross-svc",
									"namespace":   "app",
									"annotations": map[string]any{"cloud.google.com/neg-status": `{"network_endpoint_groups":{"80":"k8s1-cross-neg"}}`},
								},
							}),
						},
					},
					{
						Name:      clA + "/k8s/namespaces/app/networking.k8s.io/ingresses/ing-a",
						AssetType: "networking.k8s.io/Ingress",
						Ancestors: []string{"projects/gke-a"},
						Resource: &assetpb.Resource{
							Parent: clA,
							Data: mustStruct(t, map[string]any{
								"metadata": map[string]any{"name": "ing-a", "namespace": "app", "annotations": map[string]any{"ingress.kubernetes.io/forwarding-rule": "shared-fr"}},
								"spec":     map[string]any{"rules": []any{map[string]any{"host": "a.example.com"}}},
							}),
						},
					},
					{
						Name:      clB + "/k8s/namespaces/app/networking.k8s.io/ingresses/ing-b",
						AssetType: "networking.k8s.io/Ingress",
						Ancestors: []string{"projects/gke-b"},
						Resource: &assetpb.Resource{
							Parent: clB,
							Data: mustStruct(t, map[string]any{
								"metadata": map[string]any{"name": "ing-b", "namespace": "app", "annotations": map[string]any{"ingress.kubernetes.io/forwarding-rule": "shared-fr"}},
								"spec":     map[string]any{"rules": []any{map[string]any{"host": "b.example.com"}}},
							}),
						},
					},
					{Name: frA, AssetType: "compute.googleapis.com/GlobalForwardingRule", Ancestors: []string{"projects/gke-a"}},
					{
						Name:      frB,
						AssetType: "compute.googleapis.com/GlobalForwardingRule",
						Ancestors: []string{"projects/gke-b"},
						Resource:  &assetpb.Resource{Data: mustStruct(t, map[string]any{"target": "//compute.googleapis.com/projects/gke-b/global/targetHttpProxies/px-b"})},
					},
					{
						Name:      "//compute.googleapis.com/projects/gke-b/global/targetHttpProxies/px-b",
						AssetType: "compute.googleapis.com/TargetHttpProxy",
						Ancestors: []string{"projects/gke-b"},
						Resource:  &assetpb.Resource{Data: mustStruct(t, map[string]any{"urlMap": "//compute.googleapis.com/projects/gke-b/global/urlMaps/um-b"})},
					},
					{
						Name:      "//compute.googleapis.com/projects/gke-b/global/urlMaps/um-b",
						AssetType: "compute.googleapis.com/UrlMap",
						Ancestors: []string{"projects/gke-b"},
						Resource:  &assetpb.Resource{Data: mustStruct(t, map[string]any{"defaultService": "//compute.googleapis.com/projects/gke-b/global/backendServices/bs-b"})},
					},
					{
						Name:      "//compute.googleapis.com/projects/gke-b/global/backendServices/bs-b",
						AssetType: "compute.googleapis.com/BackendService",
						Ancestors: []string{"projects/gke-b"},
						Resource: &assetpb.Resource{Data: mustStruct(t, map[string]any{
							"backends": []any{map[string]any{"group": "//compute.googleapis.com/projects/gke-a/zones/us-central1-a/networkEndpointGroups/k8s1-cross-neg"}},
						})},
					},
				},
			}, nil
		}
		return &assetpb.ListAssetsResponse{}, nil
	}

	clients, _ := newTestClients(t, assetHandler, nil, nil, nil)
	facts, err := Extract(context.Background(), clients, ExtractOptions{Scopes: []string{"projects/host-p"}})
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}

	// 1. Cross-project Connector preserves subnet_project -> canonical host-p subnet -> Shared VPC detection.
	conn := findResource(facts.Resources, connURI)
	if conn == nil {
		t.Fatalf("expected connector %s", connURI)
	}
	if conn.Attributes["subnet"] != hostSub {
		t.Errorf("connector subnet = %v, want %s", conn.Attributes["subnet"], hostSub)
	}
	if conn.Attributes["subnet_project"] != "host-p" {
		t.Errorf("connector subnet_project = %v, want host-p", conn.Attributes["subnet_project"])
	}
	var svcProjNode ProjectNode
	for _, p := range facts.Hierarchy.Projects {
		if p.ProjectID == "svc-p" {
			svcProjNode = p
		}
	}
	if svcProjNode.SharedVPCHostProject != "host-p" {
		t.Errorf("expected svc-p SharedVPCHostProject=host-p via cross-project connector subnet, got %q", svcProjNode.SharedVPCHostProject)
	}

	// 2. Re-canonicalized NCC Spoke linkedVpnTunnels and linkedInterconnectAttachments emit Hub edges.
	if !hasEdge(facts.Edges, hubURI, vpnGwURI, EdgeConfigRef) {
		t.Errorf("expected NCC Hub -> VpnGateway edge via re-canonicalized linkedVpnTunnels")
	}
	if !hasEdge(facts.Edges, hubURI, icURI, EdgeConfigRef) {
		t.Errorf("expected NCC Hub -> InterconnectAttachment edge via re-canonicalized linkedInterconnectAttachments")
	}

	// 3. Deterministic AlloyDB private IP (inst-a < inst-b -> 10.9.0.1).
	adb := findResource(facts.Resources, adbCluster)
	if adb == nil || adb.Attributes["private_ip"] != "10.9.0.1" {
		t.Errorf("expected deterministic AlloyDB private_ip=10.9.0.1, got %+v", adb)
	}

	// 4. Multi-project GKE ingressesByFRName isolation + cross-project NEG lookup.
	resFRA := findResource(facts.Resources, frA)
	resFRB := findResource(facts.Resources, frB)
	if resFRA == nil || resFRB == nil {
		t.Fatalf("expected both ForwardingRules")
	}
	hostsA, _ := resFRA.Attributes["ingress_hosts"].([]string)
	hostsB, _ := resFRB.Attributes["ingress_hosts"].([]string)
	if !slices.Equal(hostsA, []string{"a.example.com"}) {
		t.Errorf("frA ingress_hosts = %v, want [a.example.com]", hostsA)
	}
	if !slices.Equal(hostsB, []string{"b.example.com"}) {
		t.Errorf("frB ingress_hosts = %v, want [b.example.com]", hostsB)
	}
	if !hasEdge(facts.Edges, frB, svcHostNEG, EdgeK8sNEGBackend) {
		t.Errorf("expected cross-project K8S_NEG_BACKEND edge %s -> %s", frB, svcHostNEG)
	}
}
