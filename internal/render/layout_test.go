package render

import (
	"bytes"
	"context"
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

func containersByID(v PreparedView) map[string]PreparedContainer {
	out := make(map[string]PreparedContainer, len(v.Containers))
	for _, c := range v.Containers {
		out[c.ID] = c
	}
	return out
}

func cardCells(v PreparedView) map[string][2]int {
	out := make(map[string][2]int, len(v.Cards))
	for _, c := range v.Cards {
		out[c.ComponentID] = [2]int{c.GridCol, c.GridRow}
	}
	return out
}

var nestedVPCContainers = []manifest.Container{
	{ID: "proj", Title: "Project", Kind: manifest.ContainerProject},
	{ID: "vpc", Title: "VPC", Kind: manifest.ContainerVPC, ParentContainerID: "proj"},
	{ID: "gke", Title: "GKE", Kind: manifest.ContainerGKECluster, ParentContainerID: "vpc"},
	{ID: "psa", Title: "PSA", Kind: manifest.ContainerPSAZone, ParentContainerID: "vpc"},
}

func nestedVPCView(id string, containerIDs []string, gkeCont, psaCont string) manifest.View {
	return manifest.View{
		ID: id, Title: id, Archetype: manifest.ArchetypeLandscapeMatrix, AccentMode: manifest.AccentRuntime,
		ContainerIDs: containerIDs,
		Placements: []manifest.Placement{
			{ComponentID: "app", ContainerID: gkeCont, Column: 0},
			{ComponentID: "db", ContainerID: psaCont, Column: 1},
		},
		Edges: []manifest.Edge{runtimeEdge("app", "db")},
	}
}

func TestPrepareStructuralContainerDepth(t *testing.T) {
	reg := loadTestIcons(t)
	m := layoutManifest(nestedVPCContainers,
		nestedVPCView("vpc_outer", []string{"vpc", "gke", "psa"}, "gke", "psa"),
		nestedVPCView("gke_alone", []string{"gke"}, "gke", ""),
		nestedVPCView("skip_level", []string{"proj", "gke"}, "gke", "proj"),
	)
	doc := mustPrepare(t, m, reg)

	vpcOuter := containersByID(doc.Views[0])
	if c := vpcOuter["vpc"]; c.Depth != 1 || !c.HasInnerTop {
		t.Fatalf("VPC listed without its project must be a depth-1 frame hosting an inner top: %+v", c)
	}
	if vpcOuter["gke"].Depth != 2 || vpcOuter["psa"].Depth != 2 {
		t.Fatalf("GKE/PSA nested in VPC must be depth 2: %+v", vpcOuter)
	}
	if !strings.HasPrefix(doc.Views[0].RowTracksCSS, "var(--corridor-row-0, 52px)") {
		t.Fatalf("expected 52px inner-top corridor above a depth-1 VPC frame, got %q", doc.Views[0].RowTracksCSS)
	}
	if c := containersByID(doc.Views[1])["gke"]; c.Depth != 1 || c.HasInnerTop {
		t.Fatalf("GKE cluster alone must be a depth-1 frame: %+v", c)
	}
	skip := containersByID(doc.Views[2])
	if skip["gke"].Depth != 2 || skip["proj"].Depth != 1 || !skip["proj"].HasInnerTop {
		t.Fatalf("GKE under an unlisted VPC must nest at depth 2 in its listed grandparent: %+v", skip)
	}

	m.ExcludedResources = []manifest.ExcludedResource{{
		URI: "//logging.googleapis.com/projects/p/sinks/_Default", AssetType: "logging.googleapis.com/LogSink",
		ProjectID: "p", Reason: "Default sink omitted",
	}}
	htmlBytes, err := BuildHTML(m, reg)
	if err != nil {
		t.Fatalf("BuildHTML with excluded_resources: %v", err)
	}
	if s := string(htmlBytes); !strings.Contains(s, "Excluded (1)") || !strings.Contains(s, "Default sink omitted") {
		t.Fatalf("expected excluded_resources footnote on views[0], got:\n%s", s)
	}
}

func TestContainerBandsKeepForeignCardsOutsideFrames(t *testing.T) {
	reg := loadTestIcons(t)
	containers := []manifest.Container{
		{ID: "wide", Title: "Wide", Kind: manifest.ContainerProject},
		{ID: "left", Title: "Left", Kind: manifest.ContainerProject},
		{ID: "right", Title: "Right", Kind: manifest.ContainerProject},
		{ID: "outer", Title: "Outer", Kind: manifest.ContainerProject},
		{ID: "inner", Title: "Inner", Kind: manifest.ContainerVPC, ParentContainerID: "outer"},
	}
	v := manifest.View{
		ID: "bands", Title: "Bands", Archetype: manifest.ArchetypeLandscapeMatrix, AccentMode: manifest.AccentRuntime,
		ContainerIDs: []string{"wide", "left", "right", "outer", "inner"},
		Placements: []manifest.Placement{
			{ComponentID: "free_mid", Column: 1},
			{ComponentID: "wide_l", ContainerID: "wide", Column: 0},
			{ComponentID: "wide_r", ContainerID: "wide", Column: 2},
			{ComponentID: "left_a", ContainerID: "left", Column: 0},
			{ComponentID: "left_b", ContainerID: "left", Column: 1},
			{ComponentID: "right_b", ContainerID: "right", Column: 1},
			{ComponentID: "right_c", ContainerID: "right", Column: 2},
			{ComponentID: "inner_a", ContainerID: "inner", Column: 0},
			{ComponentID: "inner_c", ContainerID: "inner", Column: 2},
			{ComponentID: "outer_b", ContainerID: "outer", Column: 1},
			{ComponentID: "free_low", Column: 1},
		},
		Edges: []manifest.Edge{runtimeEdge("wide_l", "free_mid"), runtimeEdge("left_a", "right_c"), runtimeEdge("inner_a", "outer_b")},
	}
	m := layoutManifest(containers, v)

	// The same placements stacked per column (the pre-band layout) put free_mid inside "wide".
	slots := []*cardSlot{
		{placement: v.Placements[0], gridCol: 1, row: 0},
		{placement: v.Placements[1], gridCol: 0, row: 0, cont: "wide"},
		{placement: v.Placements[2], gridCol: 2, row: 0, cont: "wide"},
	}
	viewParent := buildViewParents(v.ContainerIDs, map[string]manifest.Container{"inner": containers[4]})
	if got := checkContainerFrames("bands", computeContainerSpans(slots, viewParent), slots, viewParent, []int{0, 1, 2}); len(got) != 1 {
		t.Fatalf("expected the per-column layout to enclose free_mid inside wide, got %+v", got)
	}

	doc, err := PrepareDocument(m, reg)
	if err != nil {
		t.Fatalf("band allocation must never produce frame collisions for a valid manifest: %v", err)
	}
	cells := cardCells(doc.Views[0])
	want := map[string][2]int{
		"free_mid": {1, 0}, "wide_l": {0, 1}, "wide_r": {2, 1},
		"left_a": {0, 2}, "left_b": {1, 2}, "right_b": {1, 3}, "right_c": {2, 3},
		"inner_a": {0, 4}, "inner_c": {2, 4}, "outer_b": {1, 5}, "free_low": {1, 6},
	}
	for id, cell := range want {
		if cells[id] != cell {
			t.Fatalf("card %s at (col,row)=%v, want %v (all cells: %v)", id, cells[id], cell, cells)
		}
	}
	if c := containersByID(doc.Views[0])["outer"]; c.GridRowSpec != "10 / 13" || c.GridColSpec != "1 / 6" {
		t.Fatalf("outer frame must wrap the inner band plus its own row: %+v", c)
	}
}

func TestCheckContainerFramesReportsForeignCardsAndOverlaps(t *testing.T) {
	slot := func(id string, col, row int, cont string) *cardSlot {
		return &cardSlot{placement: manifest.Placement{ComponentID: id, Column: col, ContainerID: cont}, gridCol: col, row: row, cont: cont}
	}
	slots := []*cardSlot{
		slot("x1", 0, 0, "x"), slot("x2", 2, 0, "x"),
		slot("u", 1, 0, ""),
		slot("y1", 1, 1, "y"), slot("y2", 3, 0, "y"),
		slot("z1", 0, 1, "z"),
	}
	contByID := map[string]manifest.Container{"z": {ID: "z", ParentContainerID: "x"}}
	viewParent := buildViewParents([]string{"x", "y", "z"}, contByID)
	got := checkContainerFrames("v", computeContainerSpans(slots, viewParent), slots, viewParent, []int{0, 1, 2, 3})

	var summary []string
	for _, c := range got {
		summary = append(summary, c.Kind+":"+strings.Join(c.Subjects, ","))
	}
	want := []string{
		"container_frame_collision:x,u",
		"container_frame_collision:x,y1",
		"container_overlap_collision:x,y",
		"container_frame_collision:y,x2",
		"container_frame_collision:y,u",
	}
	if !slices.Equal(summary, want) {
		t.Fatalf("frame collisions = %v, want %v", summary, want)
	}
	msg := (&LayoutCollisionError{Collisions: got}).Error()
	if !strings.Contains(msg, `card "u" (column 1) lies inside the frame of container "x"`) || !strings.Contains(msg, "move one of the listed components") {
		t.Fatalf("collision error must name the container, the foreign card, and the fix: %s", msg)
	}
}

func TestBarycenterSweepsAreDeterministic(t *testing.T) {
	reg := loadTestIcons(t)
	v := manifest.View{
		ID: "sweeps", Title: "Sweeps", Archetype: manifest.ArchetypeLandscapeMatrix, AccentMode: manifest.AccentRuntime,
		ContainerIDs: []string{"proj"},
	}
	for i := range 4 {
		v.Placements = append(v.Placements,
			manifest.Placement{ComponentID: fmt.Sprintf("a%d", i), Column: 0},
			manifest.Placement{ComponentID: fmt.Sprintf("b%d", i), ContainerID: "proj", Column: 1},
			manifest.Placement{ComponentID: fmt.Sprintf("c%d", i), ContainerID: "proj", Column: 2},
		)
		v.Edges = append(v.Edges, runtimeEdge(fmt.Sprintf("a%d", i), fmt.Sprintf("b%d", 3-i)))
	}
	m := layoutManifest([]manifest.Container{{ID: "proj", Title: "Project", Kind: manifest.ContainerProject}}, v)

	first := cardCells(mustPrepare(t, m, reg).Views[0])
	for i := 0; i < 25; i++ {
		got := cardCells(mustPrepare(t, m, reg).Views[0])
		for id, cell := range first {
			if got[id] != cell {
				t.Fatalf("run %d: card %s at %v, first run %v", i, id, got[id], cell)
			}
		}
	}
	// Bands start in YAML order (row i for a_i, b_i, c_i); the first left-to-right pass anchors
	// column 0 and reorders column 1 so every a_i -> b_(3-i) edge becomes straight, while the
	// unconnected c_i keep YAML order inside the proj band.
	for i := range 4 {
		if first[fmt.Sprintf("a%d", i)][1] != i || first[fmt.Sprintf("b%d", i)][1] != 3-i || first[fmt.Sprintf("c%d", i)][1] != i {
			t.Fatalf("unexpected sweep result: %v", first)
		}
	}
}

func TestAssignBusGroupsPriority(t *testing.T) {
	slotsByID := map[string]*cardSlot{}
	for _, id := range []string{"s", "u", "s2", "v", "w", "q", "x"} {
		slotsByID[id] = &cardSlot{gridCol: 0}
	}
	for _, id := range []string{"t1", "t2", "t3", "t4", "t5", "t6", "y", "z"} {
		slotsByID[id] = &cardSlot{gridCol: 1}
	}
	edge := func(from, to, bus string) manifest.Edge {
		return manifest.Edge{From: from, To: to, FlowKind: manifest.FlowRuntime, ViaBusID: bus}
	}
	edges := []manifest.Edge{
		edge("s", "t1", ""),   // 0: fan-out(s) and fan-in(t1) -> fan-out wins
		edge("s", "t2", ""),   // 1
		edge("u", "t1", ""),   // 2: fan-in(t1) left with 1 edge -> dissolved
		edge("v", "t3", ""),   // 3: fan-in(t3) keeps 2 edges
		edge("w", "t3", ""),   // 4
		edge("s2", "t3", ""),  // 5: fan-out(s2) wins over fan-in(t3)
		edge("s2", "t4", ""),  // 6
		edge("x", "y", "b"),   // 7: explicit beats fan-out(x)
		edge("x", "z", "b"),   // 8
		edge("x", "t5", ""),   // 9: fan-out(x) left with 1 edge -> dissolved
		edge("q", "t5", "lo"), // 10: explicit singleton dissolved, joins fan-out(q)
		edge("q", "t6", ""),   // 11
	}
	group, kind := assignBusGroups(edges, slotsByID)
	want := []string{
		"auto_out_s_RUNTIME_1", "auto_out_s_RUNTIME_1", "",
		"auto_in_t3_RUNTIME_1", "auto_in_t3_RUNTIME_1",
		"auto_out_s2_RUNTIME_1", "auto_out_s2_RUNTIME_1",
		"explicit_b_1", "explicit_b_1", "",
		"auto_out_q_RUNTIME_1", "auto_out_q_RUNTIME_1",
	}
	if !slices.Equal(group, want) {
		t.Fatalf("bus groups = %q, want %q", group, want)
	}
	if kind[0] != "out" || kind[3] != "in" || kind[7] != "out" || kind[2] != "" {
		t.Fatalf("unexpected bus kinds %q", kind)
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
	)
	if needsNoSandbox() {
		opts = append(opts, chromedp.NoSandbox)
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
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

func TestHeadlessChromeStructuralDepthStyling(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)
	raw, err := BuildHTML(layoutManifest(nestedVPCContainers, nestedVPCView("vpc_outer", []string{"vpc", "gke", "psa"}, "gke", "psa")), reg)
	if err != nil {
		t.Fatalf("BuildHTML: %v", err)
	}
	type styles struct {
		OK           bool   `json:"ok"`
		VPCMarginTop string `json:"vpcMarginTop"`
		VPCFill      string `json:"vpcFill"`
		GKEMarginTop string `json:"gkeMarginTop"`
		PSABorder    string `json:"psaBorder"`
		PSAFill      string `json:"psaFill"`
	}
	got := evalRoutedPage[styles](t, chromePath, raw, `window.__GCP_ARCH_RENDER_PROMISE__.then(r => {
		const s = id => getComputedStyle(document.querySelector('.arch-container[data-container-id="' + id + '"]'));
		return {ok: r.ok, vpcMarginTop: s('vpc').marginTop, vpcFill: s('vpc').backgroundColor,
			gkeMarginTop: s('gke').marginTop, psaBorder: s('psa').borderTopStyle, psaFill: s('psa').backgroundColor};
	})`)
	want := styles{OK: true, VPCMarginTop: "-46px", VPCFill: "rgb(248, 250, 252)", GKEMarginTop: "-22px", PSABorder: "dashed", PSAFill: "rgb(241, 245, 249)"}
	if got != want {
		t.Fatalf("structural depth styling = %+v, want %+v", got, want)
	}
}

func TestHeadlessChromeCollisionKeepsConnectorsInStandaloneHTML(t *testing.T) {
	chromePath := requireChromeForTest(t)
	reg := loadTestIcons(t)
	raw, err := BuildHTML(buildBipartiteManifest(2, 2), reg)
	if err != nil {
		t.Fatalf("BuildHTML: %v", err)
	}
	blocker := `<article class="arch-card" data-component-id="blocker" style="position:absolute;left:420px;top:20px;width:200px;height:140px;z-index:9;">Blocker</article>`
	collided := bytes.Replace(raw, []byte(`<div class="corridor-col"`), []byte(blocker+`<div class="corridor-col"`), 1)

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
	wide := bytes.Replace(raw, []byte("</head>"), []byte("<style>.view-grid{min-width:1500px}</style></head>"), 1)

	out, err := (&ChromeDPRunner{ChromePath: chromePath}).Render(context.Background(), BrowserRenderRequest{HTML: wide, WantPNG: true, WantPDF: true})
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
	if cfg.Width != int(math.Round(rect.Width*3)) {
		t.Fatalf("PNG width = %d, want 3x view rect width %.1f", cfg.Width, rect.Width)
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
