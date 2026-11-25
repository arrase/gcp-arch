package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/manifest"
	"github.com/arrase/gcp-arch/internal/render"
	"github.com/arrase/gcp-arch/internal/synth"
)

type stubModelClient struct {
	mu      sync.Mutex
	calls   int
	handler func(call int, systemPrompt, userPrompt string, schema *genai.Schema) ([]byte, error)
}

func (s *stubModelClient) GenerateJSON(_ context.Context, systemPrompt, userPrompt string, schema *genai.Schema) ([]byte, error) {
	s.mu.Lock()
	call := s.calls
	s.calls++
	s.mu.Unlock()
	return s.handler(call, systemPrompt, userPrompt, schema)
}

type stubBrowserRunner struct {
	handler func(ctx context.Context, req render.BrowserRenderRequest) (*render.BrowserOutput, error)
}

func (s *stubBrowserRunner) Render(ctx context.Context, req render.BrowserRenderRequest) (*render.BrowserOutput, error) {
	return s.handler(ctx, req)
}

func acmeFixturePaths(t *testing.T) (factsPath, manifestPath string) {
	t.Helper()
	factsPath = filepath.Join("..", "..", "testdata", "acme_cicd_v3.facts.json")
	manifestPath = filepath.Join("..", "..", "testdata", "acme_cicd_expected.architecture.yaml")
	if _, err := os.Stat(factsPath); err != nil {
		t.Fatalf("missing fixture %s: %v", factsPath, err)
	}
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("missing fixture %s: %v", manifestPath, err)
	}
	return factsPath, manifestPath
}

func TestClassifyExitCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, exitOK},
		{"usageErr", usageErr(errors.New("bad flag")), exitUsage},
		{"runtimeErr", runtimeErr(errors.New("io error")), exitRuntime},
		{"ValidationError", &manifest.ValidationError{Issues: []manifest.ValidationIssue{{Code: "X", Message: "bad"}}}, exitUsage},
		{"wrapped ValidationError", fmt.Errorf("outer: %w", &manifest.ValidationError{Issues: []manifest.ValidationIssue{{Code: "X", Message: "bad"}}}), exitUsage},
		{"ScaleLimitError", &synth.ScaleLimitError{ResourceCount: 501, MaxAllowed: 500}, exitUsage},
		{"wrapped ScaleLimitError", runtimeErr(&synth.ScaleLimitError{ResourceCount: 501, MaxAllowed: 500}), exitUsage},
		{"LayoutCollisionError", &render.LayoutCollisionError{Collisions: []render.Collision{{Kind: "card", ViewID: "v1"}}}, exitUsage},
		{"wrapped LayoutCollisionError", fmt.Errorf("render: %w", &render.LayoutCollisionError{}), exitUsage},
		{"unwrapped cobra error", errors.New("unknown flag: --nope"), exitUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyExitCode(tc.err); got != tc.want {
				t.Fatalf("classifyExitCode(%s) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

func TestDefaultDepsSmoke(t *testing.T) {
	deps := defaultDeps()
	if deps.loadConfig == nil ||
		deps.newExtractClients == nil ||
		deps.runPreflight == nil ||
		deps.probeVertexModel == nil ||
		deps.extractFacts == nil ||
		deps.newModelClient == nil ||
		deps.newBrowserRunner == nil {
		t.Fatal("defaultDeps returned nil hooks")
	}
}

func TestRootUnknownFlagAndRetiredSubcommands(t *testing.T) {
	for _, args := range [][]string{
		{"--bogus"},
		{"boguscmd"},
		{"generate", "--projects", "app-prod"},
		{"snapshot", "--projects", "app-prod"},
		{"views", "acme.snap"},
		{"explore", "acme.snap"},
		{"export", "acme.snap"},
		{"inspect", "acme.snap"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr, appDeps{}); code != exitUsage {
			t.Errorf("args %v: got exit %d, want 2 (stderr=%s)", args, code, stderr.String())
		}
	}
}

func TestParseProjectAliases(t *testing.T) {
	valid, err := parseProjectAliases([]string{
		"999000111222=shared-vpc-host",
		"123456=app-prod",
		"999000111222=shared-vpc-host",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if valid["999000111222"] != "shared-vpc-host" || valid["123456"] != "app-prod" {
		t.Fatalf("unexpected aliases map: %+v", valid)
	}

	invalidCases := [][]string{
		{"missing-equals"},
		{"abc=shared-vpc-host"},
		{"123="},
		{"=shared-vpc-host"},
		{"123=bad/project"},
		{"123=-leading-dash"},
		{"123=proj-a", "123=proj-b"},
	}
	for _, raw := range invalidCases {
		if _, err := parseProjectAliases(raw); err == nil {
			t.Errorf("expected error for invalid aliases %v", raw)
		} else if classifyExitCode(err) != exitUsage {
			t.Errorf("expected exit code 2 for %v, got %d", raw, classifyExitCode(err))
		}
	}
}

func TestPreflightSubcommand(t *testing.T) {
	t.Run("flag validation exits 2", testPreflightFlagValidation)
	t.Run("projects and project alias with vertex probe, json and table output", testPreflightJSONAndTableOutput)
	t.Run("fatal quota error prints report and exits 1", testPreflightFatalQuotaError)
	t.Run("ADC failure exits 1", testPreflightADCFailure)
}

func testPreflightFlagValidation(t *testing.T) {
	for _, args := range [][]string{
		{"preflight"},
		{"preflight", "--org", "projects/bad"},
		{"preflight", "--projects", "app-prod,,data-prod"},
		{"preflight", "--projects", "app-prod", "--project-alias", "bad"},
		{"preflight", "--projects", "app-prod", "extra-arg"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr, appDeps{}); code != exitUsage {
			t.Errorf("preflight %v: got exit %d, want 2 (stderr=%s)", args, code, stderr.String())
		}
	}
}

func testPreflightJSONAndTableOutput(t *testing.T) {
	var (
		gotQuota      string
		gotAliases    map[string]string
		gotScopes     []string
		gotVertexOpts extract.VertexPreflightOptions
		probeCalled   bool
	)
	deps := appDeps{
		newExtractClients: func(_ context.Context, opts extract.ClientOptions) (*extract.Clients, error) {
			gotQuota = opts.QuotaProject
			gotAliases = opts.ProjectAliases
			return &extract.Clients{QuotaProject: opts.QuotaProject}, nil
		},
		probeVertexModel: func(_ context.Context, project, location, model string) error {
			probeCalled = true
			if project != "ai-sandbox" || location != "us-central1" || model != "gemini-4-carbon" {
				return fmt.Errorf("unexpected vertex probe params: %s %s %s", project, location, model)
			}
			return nil
		},
		runPreflight: func(ctx context.Context, _ *extract.Clients, scopes []string, vertexOpts extract.VertexPreflightOptions) (*extract.PreflightReport, error) {
			gotScopes = scopes
			gotVertexOpts = vertexOpts
			if vertexOpts.ModelProbe != nil {
				if err := vertexOpts.ModelProbe(ctx, vertexOpts.Project, vertexOpts.Location, vertexOpts.Model); err != nil {
					return nil, err
				}
			}
			return &extract.PreflightReport{
				QuotaProject: "billing-qp",
				Scopes:       scopes,
				Probes: []extract.PreflightProbe{
					{Scope: "projects/app-prod", Source: "cloudasset.googleapis.com", RequiredRole: "roles/cloudasset.viewer", Status: extract.ProbeGranted},
					{Scope: "projects/data-prod", Source: "monitoring.googleapis.com", RequiredRole: "roles/monitoring.viewer", Status: extract.ProbeVPCSC, Detail: "blocked by perimeter", VPCSCUUID: "uuid-1234", Remediation: "Allow caller egress in VPC-SC"},
					{Scope: "projects/ai-sandbox", Source: "vertex.model", RequiredRole: "roles/aiplatform.user", Status: extract.ProbeGranted},
				},
			}, nil
		},
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"preflight",
		"--org", "111", "--projects", "app-prod", "--project", "data-prod",
		"--project-alias", "999000=shared-host", "--quota-project", "billing-qp",
		"--vertex-project", "ai-sandbox", "--vertex-location", "us-central1", "--model", "gemini-4-carbon",
		"-q", "--json",
	}, &stdout, &stderr, deps)
	if code != exitOK {
		t.Fatalf("preflight --json exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if gotQuota != "billing-qp" || gotAliases["999000"] != "shared-host" || len(gotScopes) != 3 {
		t.Fatalf("preflight wiring mismatch: quota=%q aliases=%+v scopes=%v", gotQuota, gotAliases, gotScopes)
	}
	if !probeCalled || gotVertexOpts.Project != "ai-sandbox" || gotVertexOpts.Location != "us-central1" || gotVertexOpts.Model != "gemini-4-carbon" {
		t.Fatalf("vertex preflight wiring failed: called=%v opts=%+v", probeCalled, gotVertexOpts)
	}

	var rep PreflightReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil || rep.GrantedCount != 2 || rep.MissingCount != 1 {
		t.Fatalf("unexpected PreflightReport: err=%v rep=%+v", err, rep)
	}

	probeCalled = false
	stdout.Reset()
	stderr.Reset()
	if code = run([]string{"preflight", "--project", "app-prod,data-prod"}, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("preflight table exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if probeCalled || gotVertexOpts.Project != "" || gotVertexOpts.ModelProbe != nil {
		t.Fatalf("expected no vertex probe when --vertex-project omitted, got called=%v opts=%+v", probeCalled, gotVertexOpts)
	}
	outStr := stdout.String()
	for _, want := range []string{"Quota Project: billing-qp", "GRANTED", "VPC_SC_BLOCKED", "[vpc_sc_uuid=uuid-1234]", "Remediation:"} {
		if !strings.Contains(outStr, want) {
			t.Errorf("preflight table missing %q:\n%s", want, outStr)
		}
	}
}

func testPreflightFatalQuotaError(t *testing.T) {
	deps := appDeps{
		newExtractClients: func(_ context.Context, _ extract.ClientOptions) (*extract.Clients, error) {
			return &extract.Clients{QuotaProject: "bad-qp"}, nil
		},
		runPreflight: func(_ context.Context, _ *extract.Clients, scopes []string, _ extract.VertexPreflightOptions) (*extract.PreflightReport, error) {
			qErr := &extract.QuotaProjectError{
				Classified: extract.ClassifiedError{Kind: extract.ErrorKindQuotaFatal, Reason: "SERVICE_DISABLED", Message: "disabled"},
			}
			return &extract.PreflightReport{
				QuotaProject: "bad-qp",
				Scopes:       scopes,
				Probes:       []extract.PreflightProbe{{Scope: scopes[0], Source: "cai", Status: extract.ProbeQuotaError}},
				FatalError:   qErr.Error(),
			}, qErr
		},
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"preflight", "--projects", "app-prod", "--json"}, &stdout, &stderr, deps); code != exitRuntime {
		t.Fatalf("got exit %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "QUOTA_PROJECT_ERROR") {
		t.Fatalf("expected JSON report on stdout even on fatal error, got: %s", stdout.String())
	}
}

func testPreflightADCFailure(t *testing.T) {
	deps := appDeps{
		newExtractClients: func(_ context.Context, _ extract.ClientOptions) (*extract.Clients, error) {
			return nil, errors.New("no ADC")
		},
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"preflight", "--projects", "app-prod"}, &stdout, &stderr, deps); code != exitRuntime {
		t.Fatalf("got exit %d, want 1", code)
	}
}

func TestExtractSubcommand(t *testing.T) {
	tmp := t.TempDir()
	t.Run("flag validation exits 2", testExtractFlagValidation)
	t.Run("writes 0600 facts.json, accepts --project alias, prints out-of-scope hints & coverage warnings to stderr", func(t *testing.T) {
		testExtractSuccessAndQuiet(t, tmp)
	})
	t.Run("runtime errors exit 1", func(t *testing.T) {
		testExtractRuntimeErrors(t, tmp)
	})
}

func testExtractFlagValidation(t *testing.T) {
	for _, args := range [][]string{
		{"extract"},
		{"extract", "--projects", "app-prod,,data-prod"},
		{"extract", "--projects", "app-prod", "--cloudbuild-runs", "0"},
		{"extract", "--projects", "app-prod", "--cloudbuild-runs", "-2"},
		{"extract", "--projects", "app-prod", "-o", "   "},
		{"extract", "--projects", "app-prod", "--project-alias", "bad"},
		{"extract", "--projects", "app-prod", "extra-arg"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr, appDeps{}); code != exitUsage {
			t.Errorf("extract %v: got exit %d, want 2 (stderr=%s)", args, code, stderr.String())
		}
	}
}

func testExtractSuccessAndQuiet(t *testing.T, tmp string) {
	outFacts := filepath.Join(tmp, "nested", "facts.json")
	var gotOpts extract.ExtractOptions
	var gotAliases map[string]string
	deps := appDeps{
		newExtractClients: func(_ context.Context, opts extract.ClientOptions) (*extract.Clients, error) {
			gotAliases = opts.ProjectAliases
			c := &extract.Clients{QuotaProject: opts.QuotaProject, ProgressWriter: opts.ProgressWriter}
			c.ReportProgress(true, "extract-progress-line")
			return c, nil
		},
		extractFacts: func(_ context.Context, _ *extract.Clients, opts extract.ExtractOptions) (*extract.FactsFile, error) {
			gotOpts = opts
			return &extract.FactsFile{
				SchemaVersion: extract.FactsSchemaVersion,
				ReadTime:      "2026-11-23T02:00:00Z",
				Scopes:        opts.Scopes,
				Resources: []extract.FactResource{
					{URI: "//storage.googleapis.com/b1", AssetType: "storage.googleapis.com/Bucket", ProjectID: "acme-cicd", DisplayName: "b1"},
				},
				OutOfScopeTargets: []extract.OutOfScopeHint{
					{TargetProjectID: "acme-prod", SyntheticURI: "//cloudresourcemanager.googleapis.com/projects/acme-prod", ReferencedByURIs: []string{"//a", "//b"}, SourceKind: "cloudbuild_deploy_flag"},
					{TargetProjectID: "acme-dev", SyntheticURI: "//cloudresourcemanager.googleapis.com/projects/acme-dev", ReferencedByURIs: []string{"//a"}, SourceKind: "cloudbuild_deploy_flag"},
				},
				CoverageLedger: []extract.CoverageEntry{
					{Scope: "projects/acme-cicd", Source: "cai_resource", State: extract.CoverageOK},
					{Scope: "projects/acme-cicd", Source: "dataflow_jobs", State: extract.CoverageIAMDenied, Detail: "missing viewer"},
				},
			}, nil
		},
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"extract", "--project", "acme-cicd",
		"--project-alias", "100200300401=acme-cicd",
		"--cloudbuild-runs", "5", "-o", outFacts,
	}, &stdout, &stderr, deps)
	if code != exitOK {
		t.Fatalf("extract exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if gotOpts.CloudBuildRuns != 5 || len(gotOpts.Scopes) != 1 || gotOpts.Scopes[0] != "projects/acme-cicd" || gotAliases["100200300401"] != "acme-cicd" {
		t.Fatalf("unexpected ExtractOptions=%+v aliases=%+v", gotOpts, gotAliases)
	}
	if fi, err := os.Stat(outFacts); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("facts.json stat err=%v mode=%o, want 0600", err, fi.Mode().Perm())
	}
	if dirInfo, err := os.Stat(filepath.Dir(outFacts)); err != nil || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("parent dir stat err=%v mode=%o, want 0700", err, dirInfo.Mode().Perm())
	}
	errOut := stderr.String()
	for _, want := range []string{"extract-progress-line", "acme-dev", "acme-prod", "IAM_DENIED"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
	if !strings.Contains(stdout.String(), "Wrote "+outFacts) {
		t.Errorf("unexpected stdout: %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	outQuiet := filepath.Join(tmp, "quiet.facts.json")
	if code = run([]string{"extract", "--projects", "acme-cicd", "-q", "-o", outQuiet}, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("quiet extract exit=%d", code)
	}
	if strings.Contains(stderr.String(), "extract-progress-line") || stdout.Len() != 0 || !strings.Contains(stderr.String(), "acme-dev") {
		t.Errorf("quiet extract output mismatch: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func testExtractRuntimeErrors(t *testing.T, tmp string) {
	var stdout, stderr bytes.Buffer
	depsClientErr := appDeps{
		newExtractClients: func(_ context.Context, _ extract.ClientOptions) (*extract.Clients, error) {
			return nil, errors.New("adc broken")
		},
	}
	if code := run([]string{"extract", "--projects", "p1", "-o", filepath.Join(tmp, "x.json")}, &stdout, &stderr, depsClientErr); code != exitRuntime {
		t.Fatalf("newExtractClients error: got exit %d, want 1", code)
	}

	depsExtractErr := appDeps{
		newExtractClients: func(_ context.Context, _ extract.ClientOptions) (*extract.Clients, error) {
			return &extract.Clients{}, nil
		},
		extractFacts: func(_ context.Context, _ *extract.Clients, _ extract.ExtractOptions) (*extract.FactsFile, error) {
			return nil, errors.New("cai rpc failed")
		},
	}
	if code := run([]string{"extract", "--projects", "p1", "-o", filepath.Join(tmp, "x.json")}, &stdout, &stderr, depsExtractErr); code != exitRuntime {
		t.Fatalf("extractFacts error: got exit %d, want 1", code)
	}
}

func writeMinimalFactsFile(t *testing.T, path string, extraBucket bool) *extract.FactsFile {
	t.Helper()
	e1 := extract.FactEdge{
		FromURI: "//run.googleapis.com/projects/p1/locations/us-central1/services/api",
		ToURI:   "//storage.googleapis.com/p1-data",
		Kind:    extract.EdgeConfigRef,
		Detail:  "gs://p1-data",
	}
	e1.ID = extract.EdgeID(e1.Kind, e1.FromURI, e1.ToURI, e1.Detail)

	facts := &extract.FactsFile{
		SchemaVersion: extract.FactsSchemaVersion,
		ReadTime:      "2026-11-23T02:00:00Z",
		Scopes:        []string{"projects/p1"},
		Hierarchy:     extract.Hierarchy{Projects: []extract.ProjectNode{{ProjectID: "p1"}}},
		Resources: []extract.FactResource{
			{URI: e1.FromURI, AssetType: "run.googleapis.com/Service", ProjectID: "p1", DisplayName: "api"},
			{URI: e1.ToURI, AssetType: "storage.googleapis.com/Bucket", ProjectID: "p1", DisplayName: "p1-data"},
		},
		Edges: []extract.FactEdge{e1},
	}
	if extraBucket {
		facts.Resources = append(facts.Resources, extract.FactResource{
			URI: "//storage.googleapis.com/p1-archive", AssetType: "storage.googleapis.com/Bucket", ProjectID: "p1", DisplayName: "p1-archive",
		})
	}
	if err := extract.WriteFacts(path, facts); err != nil {
		t.Fatalf("WriteFacts: %v", err)
	}
	return facts
}

func minimalModelStub(facts *extract.FactsFile) *stubModelClient {
	table := synth.NewInternTable(facts)
	apiRID, _ := table.ResourceID("//run.googleapis.com/projects/p1/locations/us-central1/services/api")
	bktRID, _ := table.ResourceID("//storage.googleapis.com/p1-data")
	e1ID, _ := table.EdgeID(facts.Edges[0].ID)

	return &stubModelClient{
		handler: func(call int, _ string, _ string, _ *genai.Schema) ([]byte, error) {
			if call == 0 {
				return json.Marshal(synth.Pass1Plan{
					Components: []synth.Pass1Component{
						{ID: "api", Title: "API Service", IconKey: "cloud_run", ComponentKind: manifest.KindWorkload, ProjectID: "p1", BackedByIDs: []string{apiRID}},
						{ID: "data_bucket", Title: "Data Bucket", IconKey: "cloud_storage", ComponentKind: manifest.KindDatastore, ProjectID: "p1", BackedByIDs: []string{bktRID}},
					},
					DetailViews: []synth.Pass1DetailView{
						{ID: "app_view", Title: "App Architecture", Archetype: manifest.ArchetypeLandscapeMatrix, AccentMode: manifest.AccentRuntime, ComponentIDs: []string{"api", "data_bucket"}},
					},
				})
			}
			return json.Marshal(synth.Pass2View{
				Placements: []synth.Pass2Placement{
					{ComponentID: "api", Column: 0},
					{ComponentID: "data_bucket", Column: 1},
				},
				Edges: []synth.Pass2Edge{
					{From: "api", To: "data_bucket", Verb: "writes data", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{e1ID}},
				},
			})
		},
	}
}

func TestSynthesizeSubcommand(t *testing.T) {
	tmp := t.TempDir()
	factsPath := filepath.Join(tmp, "facts.json")
	facts := writeMinimalFactsFile(t, factsPath, false)

	t.Run("flag and schema usage errors exit 2; missing file exits 1", func(t *testing.T) {
		testSynthesizeUsageErrors(t, tmp, factsPath)
	})
	t.Run("scale limit >500 exits 2 without calling Vertex AI", func(t *testing.T) {
		testSynthesizeScaleLimit(t, tmp)
	})
	t.Run("day1 success, vertex RPC failure exits 1, validation exhaustion exits 2", func(t *testing.T) {
		testSynthesizeDay1Outcomes(t, tmp, factsPath, facts)
	})
	t.Run("day2 reconcile fast path without vertex-project leaves file untouched, delta requires vertex-project and writes in place or to -o", func(t *testing.T) {
		testSynthesizeDay2Reconcile(t, tmp, factsPath, facts)
	})
}

func testSynthesizeUsageErrors(t *testing.T, tmp, factsPath string) {
	badJSON := filepath.Join(tmp, "bad.json")
	if err := os.WriteFile(badJSON, []byte(`{"schema_version":"v2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"missing arg", []string{"synthesize"}, exitUsage},
		{"extra arg", []string{"synthesize", factsPath, "extra", "--vertex-project", "vp"}, exitUsage},
		{"day1 missing vertex-project", []string{"synthesize", factsPath}, exitUsage},
		{"invalid thinking-level", []string{"synthesize", factsPath, "--vertex-project", "vp", "--thinking-level", "ultra"}, exitUsage},
		{"invalid concurrency", []string{"synthesize", factsPath, "--vertex-project", "vp", "--concurrency", "0"}, exitUsage},
		{"empty out", []string{"synthesize", factsPath, "--vertex-project", "vp", "-o", "  "}, exitUsage},
		{"bad schema version", []string{"synthesize", badJSON, "--vertex-project", "vp"}, exitUsage},
		{"missing facts file", []string{"synthesize", filepath.Join(tmp, "missing.json"), "--vertex-project", "vp"}, exitRuntime},
	}
	for _, tc := range cases {
		var stdout, stderr bytes.Buffer
		if code := run(tc.args, &stdout, &stderr, appDeps{}); code != tc.want {
			t.Errorf("%s: got exit %d, want %d (stderr=%s)", tc.name, code, tc.want, stderr.String())
		}
	}
}

func testSynthesizeScaleLimit(t *testing.T, tmp string) {
	hugePath := filepath.Join(tmp, "huge.facts.json")
	huge := &extract.FactsFile{
		SchemaVersion: extract.FactsSchemaVersion,
		Resources:     make([]extract.FactResource, synth.MaxActiveResources+1),
	}
	for i := range huge.Resources {
		huge.Resources[i] = extract.FactResource{
			URI: fmt.Sprintf("//storage.googleapis.com/b-%04d", i), AssetType: "storage.googleapis.com/Bucket", ProjectID: "p1",
		}
	}
	if err := extract.WriteFacts(hugePath, huge); err != nil {
		t.Fatal(err)
	}
	called := false
	deps := appDeps{
		newModelClient: func(_ context.Context, _ synth.VertexClientOptions) (synth.ModelClient, error) {
			called = true
			return nil, errors.New("must not be called")
		},
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"synthesize", hugePath, "--vertex-project", "vp", "-o", filepath.Join(tmp, "out.yaml")}, &stdout, &stderr, deps); code != exitUsage {
		t.Fatalf("scale limit got exit %d, want 2 (stderr=%s)", code, stderr.String())
	}
	if called {
		t.Fatal("newModelClient must not be called when exceeding 500 active resources")
	}
}

func testSynthesizeDay1Outcomes(t *testing.T, tmp, factsPath string, facts *extract.FactsFile) {
	outYAML := filepath.Join(tmp, "arch.yaml")
	var gotVertexOpts synth.VertexClientOptions
	deps := appDeps{
		newModelClient: func(_ context.Context, opts synth.VertexClientOptions) (synth.ModelClient, error) {
			gotVertexOpts = opts
			return minimalModelStub(facts), nil
		},
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"synthesize", factsPath,
		"--vertex-project", "ai-sandbox", "--vertex-location", "global",
		"--model", "gemini-4-carbon", "--thinking-level", "high",
		"--concurrency", "2", "-o", outYAML,
	}, &stdout, &stderr, deps)
	if code != exitOK {
		t.Fatalf("day1 synthesize exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if gotVertexOpts.Project != "ai-sandbox" || gotVertexOpts.Location != "global" || gotVertexOpts.Model != "gemini-4-carbon" || gotVertexOpts.ThinkingLevel != "high" {
		t.Fatalf("unexpected VertexClientOptions: %+v", gotVertexOpts)
	}
	if !strings.Contains(stdout.String(), "Wrote "+outYAML) {
		t.Errorf("unexpected stdout: %q", stdout.String())
	}

	rpcFailDeps := appDeps{
		newModelClient: func(_ context.Context, _ synth.VertexClientOptions) (synth.ModelClient, error) {
			return &stubModelClient{handler: func(int, string, string, *genai.Schema) ([]byte, error) {
				return nil, errors.New("vertex rpc unavailable")
			}}, nil
		},
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"synthesize", factsPath, "--vertex-project", "ai-sandbox", "-o", outYAML}, &stdout, &stderr, rpcFailDeps); code != exitRuntime {
		t.Fatalf("vertex RPC error got exit %d, want 1", code)
	}

	valFailDeps := appDeps{
		newModelClient: func(_ context.Context, _ synth.VertexClientOptions) (synth.ModelClient, error) {
			return &stubModelClient{handler: func(int, string, string, *genai.Schema) ([]byte, error) {
				return []byte(`{"components":[],"container_defs":[],"excluded_resources":[],"detail_views":[]}`), nil
			}}, nil
		},
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"synthesize", factsPath, "--vertex-project", "ai-sandbox", "-o", outYAML}, &stdout, &stderr, valFailDeps); code != exitUsage {
		t.Fatalf("validation exhaustion got exit %d, want 2 (stderr=%s)", code, stderr.String())
	}
}

func testSynthesizeDay2Reconcile(t *testing.T, tmp, factsPath string, facts *extract.FactsFile) {
	archPath := filepath.Join(tmp, "curated.yaml")
	depsDay1 := appDeps{
		newModelClient: func(_ context.Context, _ synth.VertexClientOptions) (synth.ModelClient, error) {
			return minimalModelStub(facts), nil
		},
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"synthesize", factsPath, "--vertex-project", "ai-sandbox", "-q", "-o", archPath}, &stdout, &stderr, depsDay1); code != exitOK {
		t.Fatalf("setup day1 exit=%d stderr=%s", code, stderr.String())
	}

	beforeBytes := assertReconcileFastPathUntouched(t, factsPath, archPath)
	assertReconcileErrorCases(t, tmp, factsPath)
	assertReconcileDeltaWrites(t, tmp, archPath, beforeBytes)
}

func assertReconcileFastPathUntouched(t *testing.T, factsPath, archPath string) []byte {
	t.Helper()
	oldTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(archPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	beforeBytes, err := os.ReadFile(archPath)
	if err != nil {
		t.Fatal(err)
	}

	neverInitDeps := appDeps{
		newModelClient: func(_ context.Context, _ synth.VertexClientOptions) (synth.ModelClient, error) {
			t.Fatal("newModelClient must not be called on 0-LLM fast path")
			return nil, nil
		},
	}
	for _, args := range [][]string{
		{"synthesize", factsPath, "--reconcile", archPath},
		{"synthesize", factsPath, "--reconcile", archPath, "--vertex-project", "ai-sandbox"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr, neverInitDeps); code != exitOK {
			t.Fatalf("fast path %v exit=%d, want 0 (stderr=%s)", args, code, stderr.String())
		}
		fi, err := os.Stat(archPath)
		if err != nil || !fi.ModTime().Equal(oldTime) {
			t.Fatalf("fast path %v rewrote unchanged file: err=%v mtime=%v", args, err, fi.ModTime())
		}
		afterBytes, _ := os.ReadFile(archPath)
		if !bytes.Equal(beforeBytes, afterBytes) {
			t.Fatalf("fast path %v modified file contents", args)
		}
	}
	return beforeBytes
}

func assertReconcileErrorCases(t *testing.T, tmp, factsPath string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"synthesize", factsPath, "--reconcile", filepath.Join(tmp, "nope.yaml")}, &stdout, &stderr, appDeps{}); code != exitRuntime {
		t.Fatalf("missing reconcile file exit=%d, want 1", code)
	}
	badYAML := filepath.Join(tmp, "bad.yaml")
	if err := os.WriteFile(badYAML, []byte("schema_version: v1\nunknown_field: 123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"synthesize", factsPath, "--reconcile", badYAML}, &stdout, &stderr, appDeps{}); code != exitUsage {
		t.Fatalf("unknown YAML field exit=%d, want 2", code)
	}
}

func assertReconcileDeltaWrites(t *testing.T, tmp, archPath string, beforeBytes []byte) {
	t.Helper()
	deltaFactsPath := filepath.Join(tmp, "facts_delta.json")
	deltaFacts := writeMinimalFactsFile(t, deltaFactsPath, true)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"synthesize", deltaFactsPath, "--reconcile", archPath}, &stdout, &stderr, appDeps{}); code != exitUsage {
		t.Fatalf("delta reconcile without --vertex-project exit=%d, want 2 (stderr=%s)", code, stderr.String())
	}

	deltaTable := synth.NewInternTable(deltaFacts)
	archRID, _ := deltaTable.ResourceID("//storage.googleapis.com/p1-archive")
	deltaDeps := appDeps{
		newModelClient: func(_ context.Context, _ synth.VertexClientOptions) (synth.ModelClient, error) {
			return &stubModelClient{
				handler: func(int, string, string, *genai.Schema) ([]byte, error) {
					return json.Marshal(synth.ReconcilePatch{
						NewComponents: []synth.Pass1Component{
							{ID: "archive_bucket", Title: "Archive Bucket", IconKey: "cloud_storage", ComponentKind: manifest.KindDatastore, ProjectID: "p1", BackedByIDs: []string{archRID}},
						},
						NewPlacements: []synth.ReconcileNewPlacement{
							{ViewID: "app_view", Placement: synth.Pass2Placement{ComponentID: "archive_bucket", Column: 1}},
						},
					})
				},
			}, nil
		},
	}

	explicitOut := filepath.Join(tmp, "reconciled_copy.yaml")
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"synthesize", deltaFactsPath, "--reconcile", archPath, "--vertex-project", "ai-sandbox", "-o", explicitOut}, &stdout, &stderr, deltaDeps); code != exitOK {
		t.Fatalf("delta reconcile -o exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if _, err := os.Stat(explicitOut); err != nil {
		t.Fatalf("explicit -o file missing: %v", err)
	}
	if origNow, _ := os.ReadFile(archPath); !bytes.Equal(beforeBytes, origNow) {
		t.Fatal("explicit -o unexpectedly mutated --reconcile input file")
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"synthesize", deltaFactsPath, "--reconcile", archPath, "--vertex-project", "ai-sandbox"}, &stdout, &stderr, deltaDeps); code != exitOK {
		t.Fatalf("delta reconcile in-place exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	updated, err := manifest.Load(archPath)
	if err != nil || len(updated.Components) != 3 {
		t.Fatalf("reload reconciled archPath: err=%v len=%d", err, len(updated.Components))
	}
}

func TestValidateSubcommand(t *testing.T) {
	tmp := t.TempDir()
	factsPath, manifestPath := acmeFixturePaths(t)

	t.Run("offline and with --facts happy paths (text & json)", func(t *testing.T) {
		testValidateHappyPaths(t, factsPath, manifestPath)
	})
	t.Run("unknown YAML field, structural violation, provenance violation, and missing files", func(t *testing.T) {
		testValidateViolationsAndMissingFiles(t, tmp, factsPath, manifestPath)
	})
}

func testValidateHappyPaths(t *testing.T, factsPath, manifestPath string) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"validate", manifestPath}, &stdout, &stderr, appDeps{}); code != exitOK {
		t.Fatalf("validate offline exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Valid:") {
		t.Errorf("unexpected stdout: %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"validate", manifestPath, "--facts", factsPath, "--json"}, &stdout, &stderr, appDeps{}); code != exitOK {
		t.Fatalf("validate --facts --json exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	var rep manifest.ValidationReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil || !rep.Valid || len(rep.Errors) != 0 {
		t.Fatalf("unexpected ValidationReport: err=%v rep=%+v", err, rep)
	}
}

func testValidateViolationsAndMissingFiles(t *testing.T, tmp, factsPath, manifestPath string) {
	assertValidateUnknownFieldAndStructural(t, tmp, manifestPath)
	assertValidateProvenanceViolation(t, tmp, factsPath, manifestPath)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"validate"}, &stdout, &stderr, appDeps{}); code != exitUsage {
		t.Fatalf("validate no args exit=%d, want 2", code)
	}
	if code := run([]string{"validate", filepath.Join(tmp, "missing.yaml")}, &stdout, &stderr, appDeps{}); code != exitRuntime {
		t.Fatalf("validate missing file exit=%d, want 1", code)
	}
	if code := run([]string{"validate", manifestPath, "--facts", filepath.Join(tmp, "missing.json")}, &stdout, &stderr, appDeps{}); code != exitRuntime {
		t.Fatalf("validate missing facts exit=%d, want 1", code)
	}
}

func assertValidateUnknownFieldAndStructural(t *testing.T, tmp, manifestPath string) {
	t.Helper()
	unknownFieldYAML := filepath.Join(tmp, "unknown.yaml")
	if err := os.WriteFile(unknownFieldYAML, []byte("schema_version: v1\nbogus_top_key: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"validate", unknownFieldYAML, "--json"}, &stdout, &stderr, appDeps{}); code != exitUsage {
		t.Fatalf("unknown YAML field exit=%d, want 2", code)
	}
	var decodeRep manifest.ValidationReport
	if err := json.Unmarshal(stdout.Bytes(), &decodeRep); err != nil || decodeRep.Valid || len(decodeRep.Errors) == 0 {
		t.Fatalf("expected JSON error report for unknown YAML field: err=%v out=%s", err, stdout.String())
	}

	m, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	m.Components[0].IconKey = "nonexistent_icon_xyz"
	badStructPath := filepath.Join(tmp, "bad_struct.yaml")
	if err := manifest.Write(badStructPath, m); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"validate", badStructPath, "--json"}, &stdout, &stderr, appDeps{}); code != exitUsage {
		t.Fatalf("structural violation exit=%d, want 2", code)
	}
	var structRep manifest.ValidationReport
	if err := json.Unmarshal(stdout.Bytes(), &structRep); err != nil || structRep.Valid || structRep.Errors[0].Code != manifest.ErrInvalidIconKey {
		t.Fatalf("unexpected structural report: err=%v rep=%+v", err, structRep)
	}
}

func assertValidateProvenanceViolation(t *testing.T, tmp, factsPath, manifestPath string) {
	t.Helper()
	m2, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := range m2.Views {
		if m2.Views[i].ID == "gke_developer_platform" {
			m2.Views[i].Edges[0].EvidenceEdgeIDs = []string{"190f6dfb3f1fa702"}
		}
	}
	for i := range m2.Components {
		if m2.Components[i].ID == "atlantis" && len(m2.Components[i].BackedBy) > 1 {
			m2.Components[i].BackedBy = m2.Components[i].BackedBy[:1]
		}
	}
	badProvPath := filepath.Join(tmp, "bad_prov.yaml")
	if err := manifest.Write(badProvPath, m2); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"validate", badProvPath, "--facts", factsPath, "--json"}, &stdout, &stderr, appDeps{}); code != exitUsage {
		t.Fatalf("provenance violation exit=%d, want 2", code)
	}
	var provRep manifest.ValidationReport
	if err := json.Unmarshal(stdout.Bytes(), &provRep); err != nil || provRep.Valid {
		t.Fatalf("unexpected provenance report: err=%v rep=%+v", err, provRep)
	}
	var sawUnaccounted, sawBadProv bool
	for _, iss := range provRep.Errors {
		if iss.Code == manifest.ErrUnaccountedFactResource {
			sawUnaccounted = true
		}
		if iss.Code == manifest.ErrInvalidEdgeProvenance {
			sawBadProv = true
		}
	}
	if !sawUnaccounted || !sawBadProv {
		t.Fatalf("expected UNACCOUNTED_FACT_RESOURCE and INVALID_EDGE_PROVENANCE, got %+v", provRep.Errors)
	}
}

func TestRenderSubcommand(t *testing.T) {
	tmp := t.TempDir()
	_, manifestPath := acmeFixturePaths(t)

	t.Run("flag and schema validation exit 2; missing manifest exits 1", func(t *testing.T) {
		testRenderValidationErrors(t, tmp, manifestPath)
	})
	t.Run("chrome-free --format html fallback writes HTML and warns stderr; pdf/png without Chrome exits 1", func(t *testing.T) {
		testRenderChromeFreeFallback(t, tmp, manifestPath)
	})
	t.Run("layout collision exits 2 with colliding IDs on stderr; runner runtime error exits 1; happy path exits 0", func(t *testing.T) {
		testRenderRunnerOutcomes(t, tmp, manifestPath)
	})
}

func testRenderValidationErrors(t *testing.T, tmp, manifestPath string) {
	badYAML := filepath.Join(tmp, "bad.yaml")
	if err := os.WriteFile(badYAML, []byte("schema_version: v1\nunknown_field: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _ := manifest.Load(manifestPath)
	m.Components[0].IconKey = "bad_icon_xyz"
	badStruct := filepath.Join(tmp, "bad_struct.yaml")
	_ = manifest.Write(badStruct, m)

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"missing arg", []string{"render"}, exitUsage},
		{"extra arg", []string{"render", manifestPath, "extra"}, exitUsage},
		{"unsupported format svg", []string{"render", manifestPath, "--format", "svg"}, exitUsage},
		{"empty format", []string{"render", manifestPath, "--format", ""}, exitUsage},
		{"empty out dir", []string{"render", manifestPath, "-o", "  "}, exitUsage},
		{"unknown YAML field", []string{"render", badYAML}, exitUsage},
		{"structural error", []string{"render", badStruct}, exitUsage},
		{"missing manifest", []string{"render", filepath.Join(tmp, "nope.yaml")}, exitRuntime},
	}
	for _, tc := range cases {
		var stdout, stderr bytes.Buffer
		if code := run(tc.args, &stdout, &stderr, appDeps{}); code != tc.want {
			t.Errorf("%s: got exit %d, want %d (stderr=%s)", tc.name, code, tc.want, stderr.String())
		}
	}
}

func testRenderChromeFreeFallback(t *testing.T, tmp, manifestPath string) {
	noChromeDeps := appDeps{
		newBrowserRunner: func(_ string) (render.BrowserRunner, error) {
			return nil, errors.New("chrome binary not installed")
		},
	}
	outHTMLDir := filepath.Join(tmp, "html-only")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"render", manifestPath, "--format", "html", "-o", outHTMLDir}, &stdout, &stderr, noChromeDeps); code != exitOK {
		t.Fatalf("chrome-free html render exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "without CLI collision verification") {
		t.Errorf("expected stderr collision-skipped notice, got %q", stderr.String())
	}
	htmlBytes, err := os.ReadFile(filepath.Join(outHTMLDir, "architecture.html"))
	if err != nil || !bytes.Contains(htmlBytes, []byte("__GCP_ARCH_RENDER_PROMISE__")) {
		t.Fatalf("fallback HTML invalid: err=%v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"render", manifestPath, "--format", "pdf,png,html", "-o", filepath.Join(tmp, "fail-pdf")}, &stdout, &stderr, noChromeDeps); code != exitRuntime {
		t.Fatalf("missing chrome for pdf/png exit=%d, want 1 (stderr=%s)", code, stderr.String())
	}
}

func testRenderRunnerOutcomes(t *testing.T, tmp, manifestPath string) {
	collisionDeps := appDeps{
		newBrowserRunner: func(_ string) (render.BrowserRunner, error) {
			return &stubBrowserRunner{
				handler: func(_ context.Context, _ render.BrowserRenderRequest) (*render.BrowserOutput, error) {
					return &render.BrowserOutput{
						Result: render.BrowserRenderResult{
							OK: false,
							Collisions: []render.Collision{{
								ViewID: "gke_developer_platform", Kind: "card_collision",
								Subjects: []string{"atlantis", "sonarqube"}, Detail: "segment intersects card",
							}},
						},
					}, nil
				},
			}, nil
		},
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"render", manifestPath, "-o", filepath.Join(tmp, "col")}, &stdout, &stderr, collisionDeps); code != exitUsage {
		t.Fatalf("collision exit=%d, want 2 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "atlantis") || !strings.Contains(stderr.String(), "card_collision") {
		t.Errorf("stderr missing colliding IDs: %s", stderr.String())
	}

	runErrDeps := appDeps{
		newBrowserRunner: func(_ string) (render.BrowserRunner, error) {
			return &stubBrowserRunner{
				handler: func(_ context.Context, _ render.BrowserRenderRequest) (*render.BrowserOutput, error) {
					return nil, errors.New("cdp crashed")
				},
			}, nil
		},
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"render", manifestPath, "-o", filepath.Join(tmp, "crash")}, &stdout, &stderr, runErrDeps); code != exitRuntime {
		t.Fatalf("browser crash exit=%d, want 1", code)
	}

	okDeps := appDeps{
		newBrowserRunner: func(_ string) (render.BrowserRunner, error) {
			return &stubBrowserRunner{
				handler: func(_ context.Context, req render.BrowserRenderRequest) (*render.BrowserOutput, error) {
					return &render.BrowserOutput{
						Result: render.BrowserRenderResult{OK: true, HTML: string(req.HTML)},
						PDF:    []byte("%PDF-1.4\n"),
						PNGs:   map[string][]byte{"00-view_0_system_index.png": {0x89, 'P', 'N', 'G'}},
					}, nil
				},
			}, nil
		},
	}
	outOK := filepath.Join(tmp, "ok")
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"render", manifestPath, "--format", "pdf,png,html", "-o", outOK}, &stdout, &stderr, okDeps); code != exitOK {
		t.Fatalf("happy render exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	for _, p := range []string{
		filepath.Join(outOK, "architecture.pdf"),
		filepath.Join(outOK, "architecture.html"),
		filepath.Join(outOK, "views", "00-view_0_system_index.png"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing rendered artifact %s: %v", p, err)
		}
	}
	if !strings.Contains(stdout.String(), "architecture.pdf") {
		t.Errorf("unexpected render stdout: %q", stdout.String())
	}
}

func TestConfigFileLoadingAndPrecedence(t *testing.T) {
	t.Run("defaultConfigPath respects XDG_CONFIG_HOME and falls back to ~/.config", testDefaultConfigPath)
	t.Run("missing and comments-only config return zero config; malformed configs return exit 2 or 1", testConfigFileValidationCases)
	t.Run("end-to-end precedence CLI > env > config.yaml > defaults across subcommands", testConfigPrecedenceEndToEnd)
}

func testDefaultConfigPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	got, err := defaultConfigPath()
	if err != nil {
		t.Fatalf("defaultConfigPath(XDG): %v", err)
	}
	if want := filepath.Join(tmp, "gcp-arch", "config.yaml"); got != want {
		t.Fatalf("defaultConfigPath(XDG) = %q, want %q", got, want)
	}

	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", tmp)
	got, err = defaultConfigPath()
	if err != nil {
		t.Fatalf("defaultConfigPath(HOME): %v", err)
	}
	if want := filepath.Join(tmp, ".config", "gcp-arch", "config.yaml"); got != want {
		t.Fatalf("defaultConfigPath(HOME) = %q, want %q", got, want)
	}
}

func testConfigFileValidationCases(t *testing.T) {
	tmp := t.TempDir()

	if cfg, err := loadConfigFile(filepath.Join(tmp, "does-not-exist.yaml")); err != nil || cfg.VertexProject != "" || len(cfg.Projects) != 0 {
		t.Fatalf("missing config = (%+v, %v), want zero config", cfg, err)
	}

	emptyPath := filepath.Join(tmp, "empty.yaml")
	if err := os.WriteFile(emptyPath, []byte("# comments only\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := loadConfigFile(emptyPath); err != nil || cfg.Model != "" {
		t.Fatalf("comments-only config = (%+v, %v), want zero config", cfg, err)
	}

	nullFieldsPath := filepath.Join(tmp, "nulls.yaml")
	if err := os.WriteFile(nullFieldsPath, []byte("org:\nfolder: null\nprojects:\nformat:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := loadConfigFile(nullFieldsPath); err != nil || cfg.Org != nil || cfg.Format != nil {
		t.Fatalf("null-fields config = (%+v, %v), want nil slices", cfg, err)
	}

	if _, err := loadConfigFile(tmp); classifyExitCode(err) != exitRuntime {
		t.Fatalf("directory config exit = %d, want 1 (err=%v)", classifyExitCode(err), err)
	}

	invalidYAMLs := map[string]string{
		"unknown_key":     "bogus_key: 123\n",
		"trailing_doc":    "model: gemini-4-carbon\n---\nmodel: other\n",
		"bad_thinking":    "thinking_level: turbo\n",
		"bad_concurrency": "concurrency: 0\n",
		"bad_cb_runs":     "cloudbuild_runs: 0\n",
		"bad_format":      "format: [pdf, svg]\n",
		"empty_format":    "format: []\n",
		"bad_alias_key":   "project_aliases:\n  abc: proj-a\n",
		"bad_alias_val":   "project_aliases:\n  123: bad/proj\n",
		"bad_scope":       "org: projects/wrong\n",
	}
	for name, content := range invalidYAMLs {
		p := filepath.Join(tmp, name+".yaml")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfigFile(p); classifyExitCode(err) != exitUsage {
			t.Errorf("%s: got exit %d (err=%v), want 2", name, classifyExitCode(err), err)
		}
	}
}

func testConfigPrecedenceEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "gcp-arch")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgYAML := `
org: "111"
projects:
  - cfg-proj-a
  - cfg-proj-b
project_aliases:
  999000: cfg-shared-host
  888000: cfg-net-host
quota_project: cfg-quota
vertex_project: cfg-vertex
vertex_location: europe-west4
model: gemini-4-neon
thinking_level: medium
concurrency: 2
cloudbuild_runs: 7
chrome_path: /cfg/chrome
format:
  - html
`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", tmp)

	assertPreflightConfigPrecedence(t)
	assertExtractAndSynthesizeConfigPrecedence(t, tmp)
	assertRenderConfigPrecedence(t, tmp)
}

func assertPreflightConfigPrecedence(t *testing.T) {
	t.Helper()
	var (
		gotScopes     []string
		gotQuota      string
		gotAliases    map[string]string
		gotVertexOpts extract.VertexPreflightOptions
	)
	preflightDeps := appDeps{
		loadConfig: loadDefaultConfig,
		newExtractClients: func(_ context.Context, opts extract.ClientOptions) (*extract.Clients, error) {
			gotQuota = opts.QuotaProject
			gotAliases = opts.ProjectAliases
			return &extract.Clients{QuotaProject: opts.QuotaProject}, nil
		},
		runPreflight: func(_ context.Context, _ *extract.Clients, scopes []string, vopts extract.VertexPreflightOptions) (*extract.PreflightReport, error) {
			gotScopes = scopes
			gotVertexOpts = vopts
			return &extract.PreflightReport{Scopes: scopes}, nil
		},
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"preflight"}, &stdout, &stderr, preflightDeps); code != exitOK {
		t.Fatalf("preflight from config exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if len(gotScopes) != 3 || gotScopes[0] != "organizations/111" || gotScopes[1] != "projects/cfg-proj-a" || gotScopes[2] != "projects/cfg-proj-b" {
		t.Fatalf("preflight config scopes = %v", gotScopes)
	}
	if gotQuota != "cfg-quota" || gotAliases["999000"] != "cfg-shared-host" || gotAliases["888000"] != "cfg-net-host" {
		t.Fatalf("preflight config quota=%q aliases=%+v", gotQuota, gotAliases)
	}
	if gotVertexOpts.Project != "cfg-vertex" || gotVertexOpts.Location != "europe-west4" || gotVertexOpts.Model != "gemini-4-neon" {
		t.Fatalf("preflight config vertex = %+v", gotVertexOpts)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{
		"preflight", "--folder", "555",
		"--project-alias", "999000=cli-override",
		"--quota-project", "cli-quota", "--vertex-project", "",
	}, &stdout, &stderr, preflightDeps); code != exitOK {
		t.Fatalf("preflight CLI override exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if len(gotScopes) != 1 || gotScopes[0] != "folders/555" {
		t.Fatalf("CLI scope must replace config scopes, got %v", gotScopes)
	}
	if gotQuota != "cli-quota" || gotAliases["999000"] != "cli-override" || gotAliases["888000"] != "cfg-net-host" || gotVertexOpts.Project != "" {
		t.Fatalf("CLI override failed: quota=%q aliases=%+v vertex=%+v", gotQuota, gotAliases, gotVertexOpts)
	}
}

func assertExtractAndSynthesizeConfigPrecedence(t *testing.T, tmp string) {
	t.Helper()
	var gotQuota string
	var gotExtractOpts extract.ExtractOptions
	outFacts := filepath.Join(tmp, "out.facts.json")
	extractDeps := appDeps{
		loadConfig: loadDefaultConfig,
		newExtractClients: func(_ context.Context, opts extract.ClientOptions) (*extract.Clients, error) {
			gotQuota = opts.QuotaProject
			return &extract.Clients{QuotaProject: opts.QuotaProject}, nil
		},
		extractFacts: func(_ context.Context, _ *extract.Clients, opts extract.ExtractOptions) (*extract.FactsFile, error) {
			gotExtractOpts = opts
			return &extract.FactsFile{SchemaVersion: extract.FactsSchemaVersion, Scopes: opts.Scopes}, nil
		},
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"extract", "-o", outFacts}, &stdout, &stderr, extractDeps); code != exitOK {
		t.Fatalf("extract from config exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if gotExtractOpts.CloudBuildRuns != 7 || gotQuota != "cfg-quota" || len(gotExtractOpts.Scopes) != 3 {
		t.Fatalf("extract config opts=%+v quota=%q", gotExtractOpts, gotQuota)
	}

	factsPath := filepath.Join(tmp, "min.facts.json")
	facts := writeMinimalFactsFile(t, factsPath, false)
	var gotSynthVertex synth.VertexClientOptions
	synthDeps := appDeps{
		loadConfig: loadDefaultConfig,
		newModelClient: func(_ context.Context, opts synth.VertexClientOptions) (synth.ModelClient, error) {
			gotSynthVertex = opts
			return minimalModelStub(facts), nil
		},
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"synthesize", factsPath, "-o", filepath.Join(tmp, "a1.yaml")}, &stdout, &stderr, synthDeps); code != exitOK {
		t.Fatalf("synthesize from config exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if gotSynthVertex.Project != "cfg-vertex" || gotSynthVertex.Location != "europe-west4" || gotSynthVertex.Model != "gemini-4-neon" || gotSynthVertex.ThinkingLevel != "medium" {
		t.Fatalf("synthesize config vertex opts = %+v", gotSynthVertex)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"synthesize", factsPath, "--thinking-level", "", "--model", "gemini-4-carbon", "-o", filepath.Join(tmp, "a2.yaml")}, &stdout, &stderr, synthDeps); code != exitOK {
		t.Fatalf("synthesize CLI override exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if gotSynthVertex.ThinkingLevel != "" || gotSynthVertex.Model != "gemini-4-carbon" || gotSynthVertex.Project != "cfg-vertex" {
		t.Fatalf("synthesize CLI override vertex opts = %+v", gotSynthVertex)
	}
}

func assertRenderConfigPrecedence(t *testing.T, tmp string) {
	t.Helper()
	_, manifestPath := acmeFixturePaths(t)
	var gotChromePath string
	var gotWantPDF, gotWantPNG bool
	renderDeps := appDeps{
		loadConfig: loadDefaultConfig,
		newBrowserRunner: func(cp string) (render.BrowserRunner, error) {
			gotChromePath = cp
			return &stubBrowserRunner{
				handler: func(_ context.Context, req render.BrowserRenderRequest) (*render.BrowserOutput, error) {
					gotWantPDF = req.WantPDF
					gotWantPNG = req.WantPNG
					return &render.BrowserOutput{
						Result: render.BrowserRenderResult{OK: true, HTML: string(req.HTML)},
						PDF:    []byte("%PDF-1.4\n"),
					}, nil
				},
			}, nil
		},
	}

	var stdout, stderr bytes.Buffer
	t.Setenv("CHROME_PATH", "")
	if code := run([]string{"render", manifestPath, "-o", filepath.Join(tmp, "r1")}, &stdout, &stderr, renderDeps); code != exitOK {
		t.Fatalf("render from config exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if gotChromePath != "/cfg/chrome" || gotWantPDF || gotWantPNG {
		t.Fatalf("render config failed: chrome=%q pdf=%v png=%v", gotChromePath, gotWantPDF, gotWantPNG)
	}

	t.Setenv("CHROME_PATH", "/env/chrome")
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"render", manifestPath, "--format", "pdf", "-o", filepath.Join(tmp, "r2")}, &stdout, &stderr, renderDeps); code != exitOK {
		t.Fatalf("render env override exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if gotChromePath != "" || !gotWantPDF || gotWantPNG {
		t.Fatalf("render env/format override failed: chrome=%q pdf=%v png=%v", gotChromePath, gotWantPDF, gotWantPNG)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"render", manifestPath, "--chrome-path", "/cli/chrome", "-o", filepath.Join(tmp, "r3")}, &stdout, &stderr, renderDeps); code != exitOK {
		t.Fatalf("render CLI chrome override exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if gotChromePath != "/cli/chrome" {
		t.Fatalf("render CLI chrome override got %q, want /cli/chrome", gotChromePath)
	}
}

func TestVersionFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--version"}, &stdout, &stderr, appDeps{}); code != exitOK {
		t.Fatalf("--version exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "gcp-arch version "+version) {
		t.Fatalf("unexpected --version output: %q", stdout.String())
	}
}
