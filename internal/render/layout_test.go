package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/arrase/gcp-arch/internal/manifest"
)

func layoutComp(id string) manifest.Component {
	return manifest.Component{
		ID: id, Title: id, IconKey: "cloud_run", ComponentKind: manifest.KindWorkload, ProjectID: "p",
		BackedBy: []manifest.BackedResource{{URI: "//u/" + id, AssetType: "t", ProjectID: "p"}},
	}
}

func layoutManifest(containers []manifest.Container, views ...manifest.View) *manifest.Manifest {
	m := &manifest.Manifest{SchemaVersion: manifest.SchemaVersion, Containers: containers, Views: views}
	seen := map[string]bool{}
	for _, v := range views {
		for _, p := range v.Placements {
			if !seen[p.ComponentID] {
				seen[p.ComponentID] = true
				m.Components = append(m.Components, layoutComp(p.ComponentID))
			}
		}
	}
	return m
}

func runtimeEdge(from, to string) manifest.Edge {
	return manifest.Edge{From: from, To: to, Verb: "call", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceInferred, InferenceRationale: "r"}
}

var nested7LevelContainers = []manifest.Container{
	{ID: "org", Title: "acme.example", Kind: manifest.ContainerOrg, OrderHint: 10},
	{ID: "fld", Title: "prod-folder", Kind: manifest.ContainerFolder, ParentContainerID: "org", OrderHint: 20},
	{ID: "proj", Title: "host-proj", Kind: manifest.ContainerProject, ParentContainerID: "fld", OrderHint: 30},
	{ID: "vpc", Title: "vpc-enterprise-hub", Kind: manifest.ContainerVPC, ParentContainerID: "proj", OrderHint: 40, Badges: []string{"Shared VPC (2 svc projects)"}},
	{ID: "reg", Title: "europe-west1", Kind: manifest.ContainerRegion, ParentContainerID: "vpc", OrderHint: 50},
	{ID: "sub_priv", Title: "sub-private", Kind: manifest.ContainerSubnet, Exposure: "PRIVATE", ParentContainerID: "reg", OrderHint: 65},
	{ID: "gke", Title: "gke-prod", Kind: manifest.ContainerGKECluster, Exposure: "PRIVATE", ParentContainerID: "sub_priv", OrderHint: 70},
	{ID: "psa", Title: "Private Service Access", Kind: manifest.ContainerPSAZone, Exposure: "PSA", ParentContainerID: "vpc", OrderHint: 75},
}

func TestPrepareSevenLevelHierarchyAndSkippedAncestors(t *testing.T) {
	reg := loadTestIcons(t)
	vFull := manifest.View{
		ID: manifest.SystemIndexViewID, Title: "Overview", Archetype: manifest.ArchetypeLayeredFlow, AccentMode: manifest.AccentRuntime,
		ContainerIDs: []string{"org", "fld", "proj", "vpc", "reg", "sub_priv", "gke", "psa"},
		Placements: []manifest.Placement{
			{ComponentID: "app", ContainerID: "gke", Column: 2},
			{ComponentID: "db", ContainerID: "psa", Column: 4},
		},
		Edges: []manifest.Edge{runtimeEdge("app", "db")},
	}
	vSkip := manifest.View{
		ID: "skip_level", Title: "Skipped", Archetype: manifest.ArchetypeLayeredFlow, AccentMode: manifest.AccentRuntime,
		ContainerIDs: []string{"proj", "gke"},
		Placements: []manifest.Placement{
			{ComponentID: "app", ContainerID: "gke", Column: 0},
			{ComponentID: "db", ContainerID: "proj", Column: 1},
		},
		Edges: []manifest.Edge{runtimeEdge("app", "db")},
	}
	m := layoutManifest(nested7LevelContainers, vFull, vSkip)
	m.ExcludedResources = []manifest.ExcludedResource{{
		URI: "//logging.googleapis.com/projects/p/sinks/_Default", AssetType: "logging.googleapis.com/LogSink",
		ProjectID: "p", Reason: "Default sink omitted",
	}}

	doc := mustPrepare(t, m, reg)

	// Verify 7-level depth reaches Depth == 7 for gke inside org > fld > proj > vpc > reg > sub_priv > gke.
	var findDepth func(nodes []PreparedContainerNode, target string) int
	findDepth = func(nodes []PreparedContainerNode, target string) int {
		for _, n := range nodes {
			if n.ID == target {
				return n.Depth
			}
			if d := findDepth(n.Children, target); d > 0 {
				return d
			}
			if d := findDepth(n.PrimaryChildren, target); d > 0 {
				return d
			}
			if d := findDepth(n.SecondaryChildren, target); d > 0 {
				return d
			}
		}
		return 0
	}
	if got := findDepth(doc.Views[0].RootContainers, "gke"); got != 7 {
		t.Fatalf("expected 7-level container depth for gke, got %d", got)
	}
	// Verify skipped intermediate containers still nest gke at Depth == 2 directly under proj.
	if got := findDepth(doc.Views[1].RootContainers, "gke"); got != 2 {
		t.Fatalf("expected skipped-level container depth 2 for gke under proj, got %d", got)
	}

	htmlBytes, err := BuildHTML(m, reg)
	if err != nil {
		t.Fatalf("BuildHTML: %v", err)
	}
	s := string(htmlBytes)
	if !strings.Contains(s, "Excluded (1)") || !strings.Contains(s, "Default sink omitted") {
		t.Fatalf("expected excluded_resources footnote on views[0]")
	}
	if !strings.Contains(s, `data-exposure="private"`) || !strings.Contains(s, `data-exposure="psa"`) {
		t.Fatalf("expected lowercase-normalized data-exposure attributes in HTML")
	}
}

func TestPrepareFullMembersAndSemanticStageBanners(t *testing.T) {
	reg := loadTestIcons(t)
	var members []string
	for i := 1; i <= 20; i++ {
		members = append(members, fmt.Sprintf("build-trigger-%02d", i))
	}
	m := &manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Components: []manifest.Component{
			{
				ID: "cohort_triggers", Title: "Cloud Build Triggers", IconKey: "cloud_build",
				ComponentKind: manifest.KindCohort, ProjectID: "p", Members: members,
				ServiceProjectID: "svc-storefront",
				BackedBy:         []manifest.BackedResource{{URI: "//cb/t1", AssetType: "cloudbuild.googleapis.com/BuildTrigger", ProjectID: "p"}},
			},
			{
				ID: "df_etl", Title: "CDC Dataflow Job", IconKey: "dataflow",
				ComponentKind: manifest.KindPipeline, ProjectID: "p",
				BackedBy: []manifest.BackedResource{{URI: "//df/j1", AssetType: "dataflow.googleapis.com/Job", ProjectID: "p"}},
			},
			{
				ID: "vertex_llm", Title: "Vertex Endpoint", IconKey: "vertexai",
				ComponentKind: manifest.KindPipeline, ProjectID: "p",
				BackedBy: []manifest.BackedResource{{URI: "//aiplatform/ep1", AssetType: "aiplatform.googleapis.com/Endpoint", ProjectID: "p"}},
			},
		},
		Views: []manifest.View{
			{
				ID: manifest.SystemIndexViewID, Title: "Overview", Archetype: manifest.ArchetypeLayeredFlow, AccentMode: manifest.AccentMixed,
				Placements: []manifest.Placement{
					{ComponentID: "cohort_triggers", Column: 1},
					{ComponentID: "df_etl", Column: 3},
					{ComponentID: "vertex_llm", Column: 4},
				},
				Edges: []manifest.Edge{
					{From: "cohort_triggers", To: "df_etl", Verb: "deploys pipeline", FlowKind: manifest.FlowCICD, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e1"}},
					{From: "df_etl", To: "vertex_llm", Verb: "streams features", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e2"}},
				},
			},
			{
				ID: "data_ai_pipeline", Title: "Data & AI Pipeline", Archetype: manifest.ArchetypeStagePipeline, AccentMode: manifest.AccentRuntime,
				Placements: []manifest.Placement{
					{ComponentID: "df_etl", Column: 0},
					{ComponentID: "vertex_llm", Column: 1},
				},
				Edges: []manifest.Edge{
					{From: "df_etl", To: "vertex_llm", Verb: "streams features", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e2"}},
				},
			},
		},
	}

	doc := mustPrepare(t, m, reg)
	if len(doc.Views[0].StageBanners) != 0 {
		t.Fatalf("expected zero stage banners on ArchetypeLayeredFlow, got %+v", doc.Views[0].StageBanners)
	}
	if got := doc.Views[1].StageBanners; len(got) != 2 || !strings.Contains(got[0].Label, "Pipelines & ETL") || !strings.Contains(got[1].Label, "AI & ML") {
		t.Fatalf("unexpected semantic stage banners: %+v", got)
	}

	htmlBytes, err := BuildHTML(m, reg)
	if err != nil {
		t.Fatalf("BuildHTML: %v", err)
	}
	htmlStr := string(htmlBytes)
	for _, mem := range members {
		if !strings.Contains(htmlStr, mem) {
			t.Fatalf("cohort member %q was truncated/omitted from HTML", mem)
		}
	}
	if strings.Contains(htmlStr, "+17 more") || strings.Contains(htmlStr, "more)") {
		t.Fatalf("unexpected cohort truncation marker in HTML")
	}
	if !strings.Contains(htmlStr, "svc-storefront") {
		t.Fatalf("expected service_project_id chip in HTML")
	}
}

func TestPrepareTransversalPanelFilteringAndSuppressedSummary(t *testing.T) {
	reg := loadTestIcons(t)
	m := &manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Components: []manifest.Component{
			layoutComp("svc_a"),
			layoutComp("svc_b"),
		},
		TransversalItems: []manifest.TransversalItem{
			{ID: "tr_sa_a", Category: "IAM_SA", AssetType: "iam.googleapis.com/ServiceAccount", DisplayName: "sa-a", ProjectID: "p", AttachedNodeIDs: []string{"svc_a"}},
			{ID: "tr_kms_b", Category: "KMS", AssetType: "cloudkms.googleapis.com/CryptoKey", DisplayName: "kms-b", ProjectID: "p", AttachedNodeIDs: []string{"svc_b"}},
		},
		SuppressedPlumbingCounts: map[string]int{
			"compute.googleapis.com/Route": 6,
		},
		Views: []manifest.View{
			{
				ID: manifest.SystemIndexViewID, Title: "Overview", Archetype: manifest.ArchetypeLayeredFlow, AccentMode: manifest.AccentRuntime,
				IncludeTransversalPanel: true,
				Placements:              []manifest.Placement{{ComponentID: "svc_a", Column: 1}, {ComponentID: "svc_b", Column: 2}},
				Edges:                   []manifest.Edge{runtimeEdge("svc_a", "svc_b")},
			},
			{
				ID: "detail_a", Title: "Detail A", Archetype: manifest.ArchetypeLayeredFlow, AccentMode: manifest.AccentRuntime,
				IncludeTransversalPanel: true,
				Placements:              []manifest.Placement{{ComponentID: "svc_a", Column: 1}},
			},
		},
	}

	doc := mustPrepare(t, m, reg)
	if doc.Views[0].TransversalCount != 2 || doc.Views[0].SuppressedTotal != 6 {
		t.Fatalf("view 0 transversal/suppressed mismatch: count=%d suppressed=%d", doc.Views[0].TransversalCount, doc.Views[0].SuppressedTotal)
	}
	if doc.Views[1].TransversalCount != 1 || doc.Views[1].TransversalGroups[0].Items[0].DisplayName != "sa-a" {
		t.Fatalf("detail view should filter transversal items to placed nodes: %+v", doc.Views[1].TransversalGroups)
	}
}

func evalRoutedPage[T any](t *testing.T, chromePath string, html []byte, expr string) T {
	t.Helper()
	htmlPath := filepath.Join(t.TempDir(), "page.html")
	if err := os.WriteFile(htmlPath, html, 0o600); err != nil {
		t.Fatalf("write page: %v", err)
	}
	opts := append(slices.Clone(chromedp.DefaultExecAllocatorOptions[:]),
		chromedp.ExecPath(chromePath),
		chromedp.UserDataDir(t.TempDir()),
		chromedp.Flag("headless", "new"),
		chromedp.DisableGPU,
	)
	if needsNoSandbox() {
		opts = append(opts, chromedp.NoSandbox)
	}
	runCtx, cancelTimeout := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelTimeout()
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(runCtx, opts...)
	defer cancelAlloc()
	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()
	if err := chromedp.Do(ctx, chromedp.Navigate((&url.URL{Scheme: "file", Path: htmlPath}).String())); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	out, err := chromedp.Run(ctx, chromedp.Evaluate[T](expr, chromedp.EvalAwaitPromise))
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return out
}

func TestHeadlessChromeNestedFlexboxContainerStyling(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)
	v := manifest.View{
		ID: "vpc_nested", Title: "Nested Flexbox Containers", Archetype: manifest.ArchetypeLayeredFlow, AccentMode: manifest.AccentRuntime,
		ContainerIDs: []string{"org", "fld", "proj", "vpc", "reg", "sub_priv", "gke", "psa"},
		Placements: []manifest.Placement{
			{ComponentID: "app", ContainerID: "gke", Column: 2},
			{ComponentID: "db", ContainerID: "psa", Column: 4},
		},
		Edges: []manifest.Edge{runtimeEdge("app", "db")},
	}
	raw, err := BuildHTML(layoutManifest(nested7LevelContainers, v), reg)
	if err != nil {
		t.Fatalf("BuildHTML: %v", err)
	}

	type styles struct {
		OK        bool   `json:"ok"`
		VPCBorder string `json:"vpcBorder"`
		PSABorder string `json:"psaBorder"`
		GKEBg     string `json:"gkeBg"`
		Depth     string `json:"depth"`
	}
	got := evalRoutedPage[styles](t, chromePath, raw, `window.__GCP_ARCH_RENDER_PROMISE__.then(r => {
		const box = id => document.querySelector('.arch-box[data-container-id="' + id + '"]');
		const s = id => getComputedStyle(box(id));
		return {
			ok: r.ok,
			vpcBorder: s('vpc').borderTopStyle,
			psaBorder: s('psa').borderTopStyle,
			gkeBg: s('gke').backgroundColor,
			depth: box('gke').dataset.depth
		};
	})`)
	if !got.OK || got.VPCBorder != "solid" || got.PSABorder != "dashed" || got.Depth != "7" || got.GKEBg != "rgb(239, 246, 255)" {
		t.Fatalf("nested flexbox styling mismatch: %+v", got)
	}
}

func TestHeadlessChromeCollisionKeepsConnectorsInStandaloneHTML(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)
	raw, err := BuildHTML(buildBipartiteManifest(2, 2), reg)
	if err != nil {
		t.Fatalf("BuildHTML: %v", err)
	}
	blocker := `<article class="arch-card" data-component-id="blocker" style="position:absolute;left:20px;top:20px;width:240px;height:140px;z-index:9;">Blocker</article>`
	collided := bytes.Replace(raw, []byte(`<svg class="connector-layer"></svg>`), []byte(`<svg class="connector-layer"></svg>`+blocker), 1)

	type result struct {
		OK         bool   `json:"ok"`
		Collisions int    `json:"collisions"`
		Layers     int    `json:"layers"`
		Strokes    int    `json:"strokes"`
		Routed     string `json:"routed"`
	}
	got := evalRoutedPage[result](t, chromePath, collided, `window.__GCP_ARCH_RENDER_PROMISE__.then(r => ({
		ok: r.ok, collisions: r.collisions.length,
		layers: document.querySelectorAll('svg.connector-layer').length,
		strokes: document.querySelectorAll('svg.connector-layer line, svg.connector-layer path').length,
		routed: document.documentElement.dataset.gcpArchRouted || ''}))`)
	if got.OK || got.Collisions == 0 || got.Layers != 1 || got.Strokes == 0 || got.Routed != "" {
		t.Fatalf("collided standalone page must keep routed connectors and still report ok:false: %+v", got)
	}
}

func TestHeadlessChromePrintScaleFitsWideContent(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)
	raw, err := BuildHTML(buildBipartiteManifest(2, 2), reg)
	if err != nil {
		t.Fatalf("BuildHTML: %v", err)
	}
	wide := bytes.Replace(raw, []byte("</head>"), []byte("<style>.diagram-stage{min-width:1500px}</style></head>"), 1)

	out, err := (&ChromeDPRunner{ChromePath: chromePath}).Render(context.Background(), BrowserRenderRequest{HTML: wide, WantHTML: true, WantPNG: true, WantPDF: true})
	if err != nil {
		t.Fatalf("render wide layout: %v", err)
	}
	rect := out.Result.ViewRects[0]
	if rect.Width < 1500 {
		t.Fatalf("view rect must cover the full content width, got %.1f", rect.Width)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(out.PNGs["00-k_2_2.png"]))
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	if cfg.Width != int(math.Round(rect.Width*maxRasterScale)) {
		t.Fatalf("PNG width = %d, want %.1fx view rect width %.1f", cfg.Width, maxRasterScale, rect.Width)
	}
	match := regexp.MustCompile(`--print-scale:\s*([0-9.]+)`).FindStringSubmatch(out.Result.HTML)
	if match == nil {
		t.Fatalf("expected --print-scale on a sheet wider than 1040px")
	}
	scale, _ := strconv.ParseFloat(match[1], 64)
	if wantMax := 1040 / rect.Width; scale > wantMax+0.0001 || scale < wantMax-0.02 {
		t.Fatalf("--print-scale = %v, want ~1040/%.1f = %.4f", scale, rect.Width, wantMax)
	}
	if pages := countPDFPages(out.PDF); pages != 1 {
		t.Fatalf("scaled wide view must print on exactly 1 page, got %d", pages)
	}
}

func TestHeadlessChromeTextSelectorCoverage(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)

	m := &manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Metadata:      manifest.Metadata{GeneratedAt: "2026-11-26T00:00:00Z", Scopes: []string{"projects/p"}},
		Containers: []manifest.Container{
			{ID: "proj_p", Title: "Project p", Kind: manifest.ContainerProject},
		},
		Components: []manifest.Component{
			{
				ID: "src", Title: "Source Service", IconKey: "cloud_run", ComponentKind: manifest.KindWorkload, ProjectID: "p",
				InlineMeta: []manifest.InlineMeta{{Label: "Key", Value: "very-long-inline-meta-value-for-overflow-check"}},
				BackedBy:   []manifest.BackedResource{{URI: "//u/src", AssetType: "run.googleapis.com/Service", ProjectID: "p"}},
			},
			{
				ID: "dst", Title: "Destination DB", IconKey: "cloud_sql", ComponentKind: manifest.KindDatastore, ProjectID: "p",
				BackedBy: []manifest.BackedResource{{URI: "//u/dst", AssetType: "sqladmin.googleapis.com/Instance", ProjectID: "p"}},
			},
		},
		TransversalItems: []manifest.TransversalItem{
			{ID: "tr_sa", Category: "IAM_SA", AssetType: "iam.googleapis.com/ServiceAccount", DisplayName: "sa@p.iam", ProjectID: "p", AttachedNodeIDs: []string{"src"}},
		},
		SuppressedPlumbingCounts: map[string]int{"compute.googleapis.com/Route": 3},
		Views: []manifest.View{
			{
				ID: manifest.SystemIndexViewID, Title: "Overview", Archetype: manifest.ArchetypeStagePipeline, AccentMode: manifest.AccentRuntime,
				IncludeTransversalPanel: true,
				ContainerIDs:            []string{"proj_p"},
				Placements: []manifest.Placement{
					{ComponentID: "src", ContainerID: "proj_p", Column: 0, OffPageRefs: []manifest.OffPageRef{{TargetViewID: "detail_v", Label: "To detail_v", Direction: "OUT"}}},
					{ComponentID: "dst", ContainerID: "proj_p", Column: 1},
				},
				Edges: []manifest.Edge{runtimeEdge("src", "dst")},
			},
			{
				ID: "detail_v", Title: "Detail", Archetype: manifest.ArchetypeLayeredFlow, AccentMode: manifest.AccentRuntime,
				ContainerIDs: []string{"proj_p"},
				Placements: []manifest.Placement{
					{ComponentID: "src", ContainerID: "proj_p", Column: 0},
					{ComponentID: "dst", ContainerID: "proj_p", Column: 1},
				},
				Edges: []manifest.Edge{runtimeEdge("src", "dst")},
			},
		},
	}

	raw, err := BuildHTML(m, reg)
	if err != nil {
		t.Fatalf("BuildHTML: %v", err)
	}
	runner := &ChromeDPRunner{ChromePath: chromePath}

	for _, sel := range []string{
		".stage-banner",
		".box-kind-pill",
		".arch-card-meta-value",
		".off-page-pill",
		".gov-footer",
		".view-footnote-item",
	} {
		t.Run(sel, func(t *testing.T) {
			css := fmt.Sprintf(`<style>%s{display:block;max-width:24px;overflow:hidden;white-space:nowrap;text-overflow:ellipsis;}</style></head>`, sel)
			injected := bytes.Replace(raw, []byte("</head>"), []byte(css), 1)
			_, err := runner.Render(context.Background(), BrowserRenderRequest{HTML: injected})
			var colErr *LayoutCollisionError
			if !errors.As(err, &colErr) {
				t.Fatalf("expected *LayoutCollisionError for %s truncation, got %v", sel, err)
			}
			if !strings.Contains(colErr.Error(), "text_ellipsis") && !strings.Contains(colErr.Error(), "text_overflow") {
				t.Fatalf("expected text_ellipsis/text_overflow for %s, got: %v", sel, colErr)
			}
		})
	}
}

func TestHeadlessChromeSourceFanOutZeroUnplacedPills(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)

	// 1 source node fanning out 10 identical-verb edges to 10 target nodes in a single lane.
	comps := []manifest.Component{layoutComp("hub")}
	placements := []manifest.Placement{{ComponentID: "hub", Column: 0}}
	var edges []manifest.Edge
	for i := range 10 {
		tid := fmt.Sprintf("target_%02d", i)
		comps = append(comps, layoutComp(tid))
		placements = append(placements, manifest.Placement{ComponentID: tid, Column: 1})
		edges = append(edges, manifest.Edge{
			From: "hub", To: tid, Verb: "Dispatches", FlowKind: manifest.FlowRuntime,
			SemanticTag: "async", Provenance: manifest.ProvenanceInferred, InferenceRationale: "fanout",
		})
	}
	m := &manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Components:    comps,
		Views: []manifest.View{{
			ID: manifest.SystemIndexViewID, Title: "Source Fan-Out", Archetype: manifest.ArchetypeLayeredFlow,
			AccentMode: manifest.AccentRuntime, Placements: placements, Edges: edges,
		}},
	}

	outDir := t.TempDir()
	summary, err := Render(context.Background(), m, reg, RenderOptions{
		Formats:    []Format{FormatPNG, FormatHTML},
		OutputDir:  outDir,
		ChromePath: chromePath,
	})
	if err != nil {
		t.Fatalf("Render source fan-out failed: %v", err)
	}
	if summary.GeometryReport == nil || !summary.GeometryReport.AllPass() {
		t.Fatalf("expected GeometryReport AllPass, got %+v", summary.GeometryReport)
	}
	vr := summary.GeometryReport.Views[0]
	if len(vr.UnplacedLabels) != 0 {
		t.Fatalf("expected 0 unplaced_labels, got %v", vr.UnplacedLabels)
	}
	if len(vr.LabelBoxes) == 0 {
		t.Fatalf("expected placed/bundled LabelBoxes, got 0")
	}
}

func TestHeadlessChromeARFailureReturnsGeometryReportError(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)
	m := buildBipartiteManifest(1, 1)

	runner := &ChromeDPRunner{ChromePath: chromePath}
	outDir := t.TempDir()

	// Inject extreme aspect ratio (> 2.60) via a custom wrapper around ChromeDPRunner.
	wrapper := &arInjectingRunner{inner: runner, widthPx: 3200, heightPx: 600}
	summary, err := Render(context.Background(), m, reg, RenderOptions{
		Formats:             []Format{FormatPNG},
		OutputDir:           outDir,
		Runner:              wrapper,
		WriteGeometryReport: true,
	})
	var gre *GeometryReportError
	if !errors.As(err, &gre) {
		t.Fatalf("expected *GeometryReportError on extreme aspect ratio, got %v", err)
	}
	if summary == nil || summary.GeometryPath == "" || summary.GeometryReport == nil {
		t.Fatalf("expected geometry_report.json to be written alongside GeometryReportError, got %+v", summary)
	}
	if summary.GeometryReport.AllPass() || summary.GeometryReport.Views[0].Status != "FAIL" {
		t.Fatalf("expected FAIL status in geometry_report.json, got %+v", summary.GeometryReport.Views[0])
	}
}

type arInjectingRunner struct {
	inner             BrowserRunner
	widthPx, heightPx int
}

func (a *arInjectingRunner) Render(ctx context.Context, req BrowserRenderRequest) (*BrowserOutput, error) {
	css := fmt.Sprintf(`<style>.view-sheet{min-width:%dpx !important;width:%dpx !important;min-height:%dpx !important;height:%dpx !important;}</style></head>`,
		a.widthPx, a.widthPx, a.heightPx, a.heightPx)
	req.HTML = bytes.Replace(req.HTML, []byte("</head>"), []byte(css), 1)
	return a.inner.Render(ctx, req)
}

func TestHeadlessChromeContainerPiercingAvoidance(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)

	containers := []manifest.Container{
		{ID: "box_left", Title: "Left Project", Kind: manifest.ContainerProject, OrderHint: 10},
		{ID: "box_mid", Title: "Unrelated Cluster", Kind: manifest.ContainerGKECluster, OrderHint: 20},
		{ID: "box_right", Title: "Right Project", Kind: manifest.ContainerProject, OrderHint: 30},
	}
	v := manifest.View{
		ID:           manifest.SystemIndexViewID,
		Title:        "Container Avoidance",
		Archetype:    manifest.ArchetypeLayeredFlow,
		AccentMode:   manifest.AccentRuntime,
		ContainerIDs: []string{"box_left", "box_mid", "box_right"},
		Placements: []manifest.Placement{
			{ComponentID: "src", ContainerID: "box_left", Column: 0},
			{ComponentID: "mid_top", ContainerID: "box_mid", Column: 1},
			{ComponentID: "mid_bot", ContainerID: "box_mid", Column: 1},
			{ComponentID: "dst", ContainerID: "box_right", Column: 2},
		},
		Edges: []manifest.Edge{
			{From: "src", To: "dst", Verb: "Caches state", FlowKind: manifest.FlowRuntime, SemanticTag: "data", Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e1"}},
			{From: "mid_top", To: "mid_bot", Verb: "Internal", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e2"}},
		},
	}
	raw, err := BuildHTML(layoutManifest(containers, v), reg)
	if err != nil {
		t.Fatalf("BuildHTML: %v", err)
	}

	type checkRes struct {
		OK            bool `json:"ok"`
		MidBoxPierced bool `json:"midBoxPierced"`
		PillInMidBox  bool `json:"pillInMidBox"`
	}
	got := evalRoutedPage[checkRes](t, chromePath, raw, `window.__GCP_ARCH_RENDER_PROMISE__.then(r => {
		const stage = document.querySelector('.diagram-stage');
		const sr = stage.getBoundingClientRect();
		const mid = document.querySelector('.arch-box[data-container-id="box_mid"]').getBoundingClientRect();
		const mb = { left: mid.left - sr.left, top: mid.top - sr.top, right: mid.right - sr.left, bottom: mid.bottom - sr.top };
		let pierced = false;
		for (const ln of document.querySelectorAll('svg.connector-layer line')) {
			if (ln.getAttribute('stroke') === '#0d9488') {
				const x1 = parseFloat(ln.getAttribute('x1')), y1 = parseFloat(ln.getAttribute('y1'));
				const x2 = parseFloat(ln.getAttribute('x2')), y2 = parseFloat(ln.getAttribute('y2'));
				const minX = Math.min(x1, x2), maxX = Math.max(x1, x2);
				const minY = Math.min(y1, y2), maxY = Math.max(y1, y2);
				if (!(maxX <= mb.left + 1 || minX >= mb.right - 1 || maxY <= mb.top + 1 || minY >= mb.bottom - 1)) {
					pierced = true;
				}
			}
		}
		let pillIn = false;
		for (const g of document.querySelectorAll('svg.connector-layer g.verb-pill')) {
			if (g.textContent.includes('Caches state')) {
				const rc = g.querySelector('rect');
				const pl = parseFloat(rc.getAttribute('x')), pt = parseFloat(rc.getAttribute('y'));
				const pr = pl + parseFloat(rc.getAttribute('width')), pb = pt + parseFloat(rc.getAttribute('height'));
				if (!(pr <= mb.left || pl >= mb.right || pb <= mb.top || pt >= mb.bottom)) pillIn = true;
			}
		}
		return { ok: r.ok, midBoxPierced: pierced, pillInMidBox: pillIn };
	})`)
	if !got.OK || got.MidBoxPierced || got.PillInMidBox {
		t.Fatalf("expected src->dst wire and pill to route around unrelated mid_box, got %+v", got)
	}
}

func TestHeadlessChromeVerticalPortAndPillWireClearance(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)

	containers := []manifest.Container{
		{ID: "box_top", Title: "Payment Core", Kind: manifest.ContainerExposureZone, Exposure: "PRIVATE", OrderHint: 10},
		{ID: "box_bot", Title: "Web Portal", Kind: manifest.ContainerExposureZone, Exposure: "PRIVATE", OrderHint: 20},
	}
	v := manifest.View{
		ID:           manifest.SystemIndexViewID,
		Title:        "PSC Vertical Port & Pill Clearance",
		Archetype:    manifest.ArchetypeLayeredFlow,
		AccentMode:   manifest.AccentRuntime,
		ContainerIDs: []string{"box_top", "box_bot"},
		Placements: []manifest.Placement{
			{ComponentID: "ilb", ContainerID: "box_top", Column: 0},
			{ComponentID: "psc_att", ContainerID: "box_top", Column: 1},
			{ComponentID: "mig", ContainerID: "box_top", Column: 2},
			{ComponentID: "web_ep", ContainerID: "box_bot", Column: 1},
		},
		Edges: []manifest.Edge{
			{From: "ilb", To: "psc_att", Verb: "Routes TCP", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e1"}},
			{From: "psc_att", To: "mig", Verb: "Balances", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e2"}},
			{From: "web_ep", To: "psc_att", Verb: "PSC tunnel", FlowKind: manifest.FlowRuntime, SemanticTag: "peering", Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e3"}},
		},
	}
	// Stack box_top above box_bot inside a parent project so web_ep sits below psc_att.
	proj := manifest.Container{ID: "proj", Title: "Org", Kind: manifest.ContainerProject, OrderHint: 5}
	containers[0].ParentContainerID = "proj"
	containers[1].ParentContainerID = "proj"
	allConts := append([]manifest.Container{proj}, containers...)
	v.ContainerIDs = append([]string{"proj"}, v.ContainerIDs...)

	raw, err := BuildHTML(layoutManifest(allConts, v), reg)
	if err != nil {
		t.Fatalf("BuildHTML: %v", err)
	}
	// Force vertical stacking of box_top and box_bot to reproduce #7 geometry.
	stackedHTML := bytes.Replace(raw, []byte(`class="arch-box-body"`), []byte(`class="arch-box-body stack-vertical"`), 1)

	type clearanceRes struct {
		OK            bool    `json:"ok"`
		Collisions    int     `json:"collisions"`
		MinPillGap    float64 `json:"minPillGap"`
		PillBoxBorder bool    `json:"pillBoxBorder"`
	}
	got := evalRoutedPage[clearanceRes](t, chromePath, stackedHTML, `window.__GCP_ARCH_RENDER_PROMISE__.then(r => {
		const pills = Array.from(document.querySelectorAll('svg.connector-layer g.verb-pill rect')).map(rc => ({
			l: parseFloat(rc.getAttribute('x')), t: parseFloat(rc.getAttribute('y')),
			r: parseFloat(rc.getAttribute('x')) + parseFloat(rc.getAttribute('width')),
			b: parseFloat(rc.getAttribute('y')) + parseFloat(rc.getAttribute('height')),
			txt: rc.parentElement.textContent
		}));
		const lines = Array.from(document.querySelectorAll('svg.connector-layer line')).map(ln => ({
			x1: parseFloat(ln.getAttribute('x1')), y1: parseFloat(ln.getAttribute('y1')),
			x2: parseFloat(ln.getAttribute('x2')), y2: parseFloat(ln.getAttribute('y2')),
			stroke: ln.getAttribute('stroke')
		}));
		let minGap = 999;
		for (const p of pills) {
			for (const s of lines) {
				const dx = Math.max(p.l - Math.max(s.x1, s.x2), Math.min(s.x1, s.x2) - p.r, 0);
				const dy = Math.max(p.t - Math.max(s.y1, s.y2), Math.min(s.y1, s.y2) - p.b, 0);
				const d = Math.hypot(dx, dy);
				if (d > 0.01 && d < minGap) minGap = d;
			}
		}
		return { ok: r.ok, collisions: r.collisions.length, minPillGap: minGap, pillBoxBorder: r.collisions.some(c => c.kind === 'pill_border_collision') };
	})`)
	if !got.OK || got.Collisions != 0 || got.MinPillGap < 2.9 || got.PillBoxBorder {
		t.Fatalf("expected clean pill-to-wire and pill-to-border clearance, got %+v", got)
	}
}

func TestHeadlessChromeCleanUnderscoreWrappingAndOrphanAudit(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)

	m := &manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Components: []manifest.Component{
			{
				ID:            "warehouse",
				Title:         "enterprise_commerce_raw_events_warehouse",
				Subtitle:      "Raw Commerce Events BigQuery Dataset",
				IconKey:       "bigquery",
				ComponentKind: manifest.KindDatastore,
				ProjectID:     "p",
				BackedBy:      []manifest.BackedResource{{URI: "//bq/ds", AssetType: "bigquery.googleapis.com/Dataset", ProjectID: "p"}},
			},
			layoutComp("worker"),
		},
		Views: []manifest.View{{
			ID:         manifest.SystemIndexViewID,
			Title:      "Underscore Wrapping",
			Archetype:  manifest.ArchetypeLayeredFlow,
			AccentMode: manifest.AccentRuntime,
			Placements: []manifest.Placement{
				{ComponentID: "worker", Column: 0},
				{ComponentID: "warehouse", Column: 1},
			},
			Edges: []manifest.Edge{runtimeEdge("worker", "warehouse")},
		}},
	}

	raw, err := BuildHTML(m, reg)
	if err != nil {
		t.Fatalf("BuildHTML: %v", err)
	}
	htmlStr := string(raw)
	if !strings.Contains(htmlStr, `data-wide="2"`) {
		t.Fatalf("expected data-wide=\"2\" for 40-char unbroken identifier")
	}
	if !strings.Contains(htmlStr, `enterprise_<wbr>commerce_<wbr>raw_<wbr>events_<wbr>warehouse`) {
		t.Fatalf("expected <wbr> breaks after underscores in .arch-card-title")
	}

	outDir := t.TempDir()
	summary, err := Render(context.Background(), m, reg, RenderOptions{
		Formats:    []Format{FormatPNG, FormatHTML},
		OutputDir:  outDir,
		ChromePath: chromePath,
	})
	if err != nil || !summary.GeometryReport.AllPass() {
		t.Fatalf("expected clean render on long underscore identifier, err=%v rep=%+v", err, summary)
	}

	// Verify that removing <wbr> and squeezing card width to 320px triggers text_orphan_wrap in Chrome.
	broken := bytes.ReplaceAll(raw, []byte("enterprise_<wbr>commerce_<wbr>raw_<wbr>events_<wbr>warehouse"), []byte("enterprise_commerce_raw_events_warehouse"))
	broken = bytes.Replace(broken, []byte("</head>"), []byte(`<style>.arch-card[data-wide="2"]{width:320px!important}.arch-card-title{overflow-wrap:anywhere!important;word-break:break-word!important}</style></head>`), 1)
	runner := &ChromeDPRunner{ChromePath: chromePath}
	_, orphanErr := runner.Render(context.Background(), BrowserRenderRequest{HTML: broken})
	var colErr *LayoutCollisionError
	if !errors.As(orphanErr, &colErr) || !strings.Contains(colErr.Error(), "text_orphan_wrap") {
		t.Fatalf("expected text_orphan_wrap collision when mid-word orphan split occurs, got %v", orphanErr)
	}
}

func TestHeadlessChromeEagerBusTrunkBundling(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)

	comps := []manifest.Component{
		{ID: "bus", Title: "domain-events-topic", IconKey: "pubsub", ComponentKind: manifest.KindMessaging, ProjectID: "p", BackedBy: []manifest.BackedResource{{URI: "//ps/t", AssetType: "pubsub.googleapis.com/Topic", ProjectID: "p"}}},
	}
	placements := []manifest.Placement{{ComponentID: "bus", Column: 1}}
	var edges []manifest.Edge
	for i := range 12 {
		id := fmt.Sprintf("pub_%02d", i)
		comps = append(comps, layoutComp(id))
		placements = append(placements, manifest.Placement{ComponentID: id, Column: 0})
		edges = append(edges, manifest.Edge{
			From: id, To: "bus", Verb: "Publishes", FlowKind: manifest.FlowRuntime,
			SemanticTag: "async", Provenance: manifest.ProvenanceInferred, InferenceRationale: "bus fan-in",
		})
	}
	for i := range 8 {
		id := fmt.Sprintf("sub_%02d", i)
		comps = append(comps, layoutComp(id))
		placements = append(placements, manifest.Placement{ComponentID: id, Column: 2})
		edges = append(edges, manifest.Edge{
			From: "bus", To: id, Verb: "Pushes event", FlowKind: manifest.FlowRuntime,
			SemanticTag: "async", Provenance: manifest.ProvenanceInferred, InferenceRationale: "bus fan-out",
		})
	}
	m := &manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Components:    comps,
		Views: []manifest.View{{
			ID: manifest.SystemIndexViewID, Title: "Eager Bus Bundling", Archetype: manifest.ArchetypeLayeredFlow,
			AccentMode: manifest.AccentRuntime, Placements: placements, Edges: edges,
		}},
	}

	outDir := t.TempDir()
	summary, err := Render(context.Background(), m, reg, RenderOptions{
		Formats:    []Format{FormatPNG, FormatHTML},
		OutputDir:  outDir,
		ChromePath: chromePath,
	})
	if err != nil {
		t.Fatalf("Render bus bundling failed: %v", err)
	}
	if summary.GeometryReport == nil || !summary.GeometryReport.AllPass() {
		t.Fatalf("expected GeometryReport AllPass, got %+v", summary.GeometryReport)
	}
	vr := summary.GeometryReport.Views[0]
	if len(vr.UnplacedLabels) != 0 {
		t.Fatalf("expected 0 unplaced_labels, got %v", vr.UnplacedLabels)
	}
	if len(vr.LabelBoxes) != 2 {
		t.Fatalf("expected exactly 2 bundled pills (12x fan-in + 8x fan-out), got %d: %+v", len(vr.LabelBoxes), vr.LabelBoxes)
	}
	htmlBytes, err := os.ReadFile(summary.HTMLPath)
	if err != nil {
		t.Fatalf("read html: %v", err)
	}
	htmlStr := string(htmlBytes)
	if !strings.Contains(htmlStr, "Publishes (12×)") || !strings.Contains(htmlStr, "Pushes event (8×)") {
		t.Fatalf("expected bundled pill labels in routed HTML")
	}
	if !strings.Contains(htmlStr, "<circle ") || !strings.Contains(htmlStr, `stroke-width="2.6"`) {
		t.Fatalf("expected bus junction circles and 2.6px trunk strokes in routed SVG")
	}
}

func TestHeadlessChromeVerticalEdgeAcrossFiveStackedBorders(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)

	containers := []manifest.Container{
		{ID: "proj", Title: "ERP Core Prod", Kind: manifest.ContainerProject, OrderHint: 10},
		{ID: "vpc", Title: "VPC: erp-vpc", Kind: manifest.ContainerVPC, ParentContainerID: "proj", OrderHint: 20},
		{ID: "reg", Title: "Region: europe-west1", Kind: manifest.ContainerRegion, ParentContainerID: "vpc", OrderHint: 30},
		{ID: "sub", Title: "Subnet: erp-app-subnet", Kind: manifest.ContainerSubnet, Exposure: "PRIVATE", ParentContainerID: "reg", OrderHint: 40},
		{ID: "zone_pub", Title: "Public Exposure", Kind: manifest.ContainerExposureZone, Exposure: "PUBLIC", ParentContainerID: "sub", OrderHint: 50},
		{ID: "zone_priv", Title: "Private Workloads", Kind: manifest.ContainerExposureZone, Exposure: "PRIVATE", ParentContainerID: "sub", OrderHint: 55},
		{ID: "zone_managed", Title: "Managed Services & Messaging", Kind: manifest.ContainerExposureZone, Exposure: "PRIVATE", ParentContainerID: "proj", OrderHint: 80},
	}
	v := manifest.View{
		ID:           manifest.SystemIndexViewID,
		Title:        "Five Stacked Container Borders",
		Archetype:    manifest.ArchetypeLayeredFlow,
		AccentMode:   manifest.AccentRuntime,
		ContainerIDs: []string{"proj", "vpc", "reg", "sub", "zone_pub", "zone_priv", "zone_managed"},
		Placements: []manifest.Placement{
			{ComponentID: "dns_zone", ContainerID: "zone_managed", Column: 1},
			{ComponentID: "ext_lb", ContainerID: "zone_pub", Column: 2},
			{ComponentID: "app_mig", ContainerID: "zone_priv", Column: 3},
			{ComponentID: "backups", ContainerID: "zone_managed", Column: 5},
		},
		Edges: []manifest.Edge{
			{From: "dns_zone", To: "ext_lb", Verb: "Resolves DNS", FlowKind: manifest.FlowRuntime, SemanticTag: "ingress", Provenance: manifest.ProvenanceInferred, InferenceRationale: "dns"},
			{From: "ext_lb", To: "app_mig", Verb: "Routes traffic", FlowKind: manifest.FlowRuntime, SemanticTag: "ingress", Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e1"}},
			{From: "app_mig", To: "backups", Verb: "Writes backups", FlowKind: manifest.FlowRuntime, SemanticTag: "data", Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e2"}},
		},
	}

	raw, err := BuildHTML(layoutManifest(containers, v), reg)
	if err != nil {
		t.Fatalf("BuildHTML: %v", err)
	}

	type auditRes struct {
		OK            bool     `json:"ok"`
		Collisions    []string `json:"collisions"`
		UnplacedCount int      `json:"unplacedCount"`
		PillWire      bool     `json:"pillWire"`
		PillBorder    bool     `json:"pillBorder"`
		PillsPlaced   int      `json:"pillsPlaced"`
	}
	got := evalRoutedPage[auditRes](t, chromePath, raw, `window.__GCP_ARCH_RENDER_PROMISE__.then(r => {
		const cols = r.collisions || [];
		return {
			ok: r.ok,
			collisions: cols.map(c => c.kind + ':' + c.detail),
			unplacedCount: cols.filter(c => c.kind === 'unplaced_label').length,
			pillWire: cols.some(c => c.kind === 'pill_wire_collision'),
			pillBorder: cols.some(c => c.kind === 'pill_border_collision'),
			pillsPlaced: document.querySelectorAll('svg.connector-layer g.verb-pill').length
		};
	})`)
	if !got.OK || got.UnplacedCount != 0 || got.PillWire || got.PillBorder || got.PillsPlaced != 3 {
		t.Fatalf("expected 3 placed pills with zero unplaced_label, pill_wire_collision, or pill_border_collision, got %+v", got)
	}

	outDir := t.TempDir()
	summary, err := Render(context.Background(), layoutManifest(containers, v), reg, RenderOptions{
		Formats:    []Format{FormatPNG, FormatHTML},
		OutputDir:  outDir,
		ChromePath: chromePath,
	})
	if err != nil {
		t.Fatalf("Render 5-border vertical stack failed: %v", err)
	}
	if summary.GeometryReport == nil || !summary.GeometryReport.AllPass() {
		t.Fatalf("expected GeometryReport AllPass, got %+v", summary.GeometryReport)
	}
}

func TestPrepareComponentCardDeduplicatesSubtitleTokens(t *testing.T) {
	c := manifest.Component{
		ID:       "cohort_1",
		Title:    "platform-commerce (5x Service)",
		Subtitle: "Private Cloud Run Cohort (5 Services) (5x) (5x) (5x) (5x) (5x)",
		IconKey:  "cloud_run",
	}
	card := resolveComponentCard(c, manifest.Placement{ComponentID: "cohort_1", Column: 1}, 0)
	want := "Private Cloud Run Cohort (5 Services) (5x)"
	if card.Subtitle != want {
		t.Fatalf("resolveComponentCard Subtitle = %q, want %q", card.Subtitle, want)
	}
}

func TestHeadlessChromeSharedSidePortsSeparated(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)

	containers := []manifest.Container{
		{ID: "priv_zone", Title: "Private Workloads", Kind: manifest.ContainerExposureZone, Exposure: "PRIVATE", OrderHint: 10},
		{ID: "managed", Title: "Managed Services & Messaging", Kind: manifest.ContainerExposureZone, Exposure: "PRIVATE", OrderHint: 20},
	}
	v := manifest.View{
		ID:           manifest.SystemIndexViewID,
		Title:        "Shared Side In/Out Ports",
		Archetype:    manifest.ArchetypeLayeredFlow,
		AccentMode:   manifest.AccentRuntime,
		ContainerIDs: []string{"priv_zone", "managed"},
		Placements: []manifest.Placement{
			{ComponentID: "worker", ContainerID: "priv_zone", Column: 1},
			{ComponentID: "raw_uploads", ContainerID: "managed", Column: 0},
			{ComponentID: "retry_tasks", ContainerID: "managed", Column: 2},
		},
		Edges: []manifest.Edge{
			{From: "worker", To: "raw_uploads", Verb: "Reads raw", FlowKind: manifest.FlowRuntime, SemanticTag: "data", Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e1"}},
			{From: "retry_tasks", To: "worker", Verb: "HTTP dispatch", FlowKind: manifest.FlowRuntime, SemanticTag: "retry", IsRetryOrDLQ: true, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e2"}},
		},
	}
	raw, err := BuildHTML(layoutManifest(containers, v), reg)
	if err != nil {
		t.Fatalf("BuildHTML: %v", err)
	}
	stacked := bytes.Replace(raw, []byte(`class="diagram-stage"`), []byte(`class="diagram-stage wrap-stage" style="max-width:700px"`), 1)

	type portRes struct {
		OK       bool    `json:"ok"`
		ReadsX   float64 `json:"readsX"`
		RetryX   float64 `json:"retryX"`
		WorkerBo float64 `json:"workerBottom"`
	}
	got := evalRoutedPage[portRes](t, chromePath, stacked, `window.__GCP_ARCH_RENDER_PROMISE__.then(r => {
		const stage = document.querySelector('.diagram-stage').getBoundingClientRect();
		const wr = document.querySelector('.arch-card[data-component-id="worker"]').getBoundingClientRect();
		const workerBottom = wr.bottom - stage.top;
		let readsX = -1, retryX = -1;
		for (const ln of document.querySelectorAll('svg.connector-layer line')) {
			const x1 = parseFloat(ln.getAttribute('x1')), y1 = parseFloat(ln.getAttribute('y1'));
			const x2 = parseFloat(ln.getAttribute('x2')), y2 = parseFloat(ln.getAttribute('y2'));
			const stroke = ln.getAttribute('stroke');
			if (stroke === '#0d9488' && Math.abs(y1 - workerBottom) < 2) readsX = x1;
			if (stroke === '#d97706' && Math.abs(y2 - workerBottom) < 2) retryX = x2;
		}
		return { ok: r.ok, readsX, retryX, workerBottom };
	})`)
	if !got.OK || got.ReadsX <= 0 || got.RetryX <= 0 {
		t.Fatalf("expected clean route and both bottom ports found, got %+v", got)
	}
	if math.Abs(got.RetryX-got.ReadsX) < 20 || got.ReadsX >= got.RetryX {
		t.Fatalf("expected separated bottom ports with Reads raw left of HTTP dispatch, got %+v", got)
	}
}

func TestHeadlessChromeWideHeaderWhitespaceDirectVerticalEntry(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)

	containers := []manifest.Container{
		{ID: "top_box", Title: "VPC Compute", Kind: manifest.ContainerVPC, OrderHint: 10},
		{ID: "bot_box", Title: "Managed Services", Kind: manifest.ContainerExposureZone, Exposure: "PRIVATE", OrderHint: 20},
	}
	v := manifest.View{
		ID:           manifest.SystemIndexViewID,
		Title:        "Wide Header Direct Vertical Entry",
		Archetype:    manifest.ArchetypeLayeredFlow,
		AccentMode:   manifest.AccentRuntime,
		ContainerIDs: []string{"top_box", "bot_box"},
		Placements: []manifest.Placement{
			{ComponentID: "left_a", ContainerID: "top_box", Column: 0},
			{ComponentID: "right_a", ContainerID: "top_box", Column: 2},
			{ComponentID: "left_b", ContainerID: "bot_box", Column: 0},
			{ComponentID: "mid_b", ContainerID: "bot_box", Column: 1},
			{ComponentID: "right_b", ContainerID: "bot_box", Column: 2},
		},
		Edges: []manifest.Edge{
			{From: "left_a", To: "right_a", Verb: "Calls", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e1"}},
			{From: "left_b", To: "mid_b", Verb: "Reads", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e2"}},
			{From: "mid_b", To: "right_b", Verb: "Writes", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e3"}},
			{From: "right_a", To: "right_b", Verb: "Streams CDC", FlowKind: manifest.FlowRuntime, SemanticTag: "data", Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e4"}},
		},
	}
	raw, err := BuildHTML(layoutManifest(containers, v), reg)
	if err != nil {
		t.Fatalf("BuildHTML: %v", err)
	}
	stacked := bytes.Replace(raw, []byte(`class="diagram-stage"`), []byte(`class="diagram-stage wrap-stage" style="max-width:1100px"`), 1)

	type entryRes struct {
		OK          bool    `json:"ok"`
		DetouredOut bool    `json:"detouredOut"`
		HeaderWidth float64 `json:"headerWidth"`
		BoxWidth    float64 `json:"boxWidth"`
		RightCardX  float64 `json:"rightCardX"`
	}
	got := evalRoutedPage[entryRes](t, chromePath, stacked, `window.__GCP_ARCH_RENDER_PROMISE__.then(r => {
		const stage = document.querySelector('.diagram-stage').getBoundingClientRect();
		const bot = document.querySelector('.arch-box[data-container-id="bot_box"]').getBoundingClientRect();
		const hdr = document.querySelector('.arch-box[data-container-id="bot_box"] > .arch-box-header');
		const rc = document.querySelector('.arch-card[data-component-id="right_b"]').getBoundingClientRect();
		const bl = bot.left - stage.left, br = bot.right - stage.left;
		let kidsR = 0;
		for (const ch of hdr.children) kidsR = Math.max(kidsR, ch.getBoundingClientRect().right - stage.left);
		let detouredOut = false;
		for (const ln of document.querySelectorAll('svg.connector-layer line')) {
			if (ln.getAttribute('stroke') !== '#0d9488') continue;
			const x1 = parseFloat(ln.getAttribute('x1')), x2 = parseFloat(ln.getAttribute('x2'));
			if (Math.min(x1, x2) < bl - 2 || Math.max(x1, x2) > br + 2) detouredOut = true;
		}
		return {
			ok: r.ok,
			detouredOut,
			headerWidth: kidsR - bl,
			boxWidth: br - bl,
			rightCardX: rc.left - stage.left
		};
	})`)
	if !got.OK || got.DetouredOut || got.BoxWidth <= got.HeaderWidth+100 || got.RightCardX <= got.HeaderWidth {
		t.Fatalf("expected direct vertical entry through empty header whitespace without far-edge detour, got %+v", got)
	}
}

func TestHeadlessChromePillBorderClearanceAndAuditThreshold(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)

	containers := []manifest.Container{
		{ID: "priv_zone", Title: "Private Workloads", Kind: manifest.ContainerExposureZone, Exposure: "PRIVATE", OrderHint: 10},
	}
	v := manifest.View{
		ID:           manifest.SystemIndexViewID,
		Title:        "Pill Border Clearance",
		Archetype:    manifest.ArchetypeLayeredFlow,
		AccentMode:   manifest.AccentRuntime,
		ContainerIDs: []string{"priv_zone"},
		Placements: []manifest.Placement{
			{ComponentID: "svc_a", ContainerID: "priv_zone", Column: 0},
			{ComponentID: "svc_b", ContainerID: "priv_zone", Column: 1},
		},
		Edges: []manifest.Edge{
			{From: "svc_a", To: "svc_b", Verb: "Calls", FlowKind: manifest.FlowRuntime, Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e1"}},
		},
	}
	raw, err := BuildHTML(layoutManifest(containers, v), reg)
	if err != nil {
		t.Fatalf("BuildHTML: %v", err)
	}

	// Verify normal render achieves >= 6.0 CSS px border clearance.
	type gapRes struct {
		OK           bool    `json:"ok"`
		MinBorderGap float64 `json:"minBorderGap"`
	}
	clean := evalRoutedPage[gapRes](t, chromePath, raw, `window.__GCP_ARCH_RENDER_PROMISE__.then(r => {
		const stage = document.querySelector('.diagram-stage').getBoundingClientRect();
		const box = document.querySelector('.arch-box[data-container-id="priv_zone"]').getBoundingClientRect();
		const b = { l: box.left - stage.left, t: box.top - stage.top, r: box.right - stage.left, b: box.bottom - stage.top };
		let minGap = 999;
		for (const rc of document.querySelectorAll('svg.connector-layer g.verb-pill rect')) {
			const pl = parseFloat(rc.getAttribute('x')), pt = parseFloat(rc.getAttribute('y'));
			const pr = pl + parseFloat(rc.getAttribute('width')), pb = pt + parseFloat(rc.getAttribute('height'));
			minGap = Math.min(minGap, Math.abs(pt - b.t), Math.abs(b.b - pb), Math.abs(pl - b.l), Math.abs(b.r - pr));
		}
		return { ok: r.ok, minBorderGap: minGap };
	})`)
	if !clean.OK || clean.MinBorderGap < 5.9 {
		t.Fatalf("expected clean pill-to-border gap >= 6.0 CSS px, got %+v", clean)
	}

	// Verify the audit rule triggers pill_border_collision when a pill sits < 6.0 CSS px from a container border.
	vTight := manifest.View{
		ID:           manifest.SystemIndexViewID,
		Title:        "Pill Border Audit",
		Archetype:    manifest.ArchetypeLayeredFlow,
		AccentMode:   manifest.AccentRuntime,
		ContainerIDs: []string{"priv_zone"},
		Placements: []manifest.Placement{
			{ComponentID: "svc_a", ContainerID: "priv_zone", Column: 0},
			{ComponentID: "svc_b", ContainerID: "priv_zone", Column: 1},
		},
		Edges: []manifest.Edge{
			{From: "svc_b", To: "svc_a", Verb: "Retry loop", FlowKind: manifest.FlowRuntime, IsRetryOrDLQ: true, SemanticTag: "retry", Provenance: manifest.ProvenanceVerified, EvidenceEdgeIDs: []string{"e1"}},
		},
	}
	rawTight, err := BuildHTML(layoutManifest(containers, vTight), reg)
	if err != nil {
		t.Fatalf("BuildHTML tight: %v", err)
	}
	rawTight = bytes.ReplaceAll(rawTight, []byte("borderPad: 5.5"), []byte("borderPad: 1"))
	rawTight = bytes.ReplaceAll(rawTight, []byte("borderPad: 4.5"), []byte("borderPad: 1"))
	rawTight = bytes.Replace(rawTight, []byte("</head>"), []byte(`<style>.arch-box{padding-bottom:16px!important}</style></head>`), 1)
	rawTight = bytes.Replace(rawTight, []byte("const MAX_REFLOW_PASSES = 3;"), []byte("const MAX_REFLOW_PASSES = 0;"), 1)

	runner := &ChromeDPRunner{ChromePath: chromePath}
	_, colErr := runner.Render(context.Background(), BrowserRenderRequest{HTML: rawTight})
	var lce *LayoutCollisionError
	if !errors.As(colErr, &lce) || !strings.Contains(lce.Error(), "pill_border_collision") {
		t.Fatalf("expected pill_border_collision when pill is < 6.0 CSS px from border, got %v", colErr)
	}
}
