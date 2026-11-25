package render

import (
	"cmp"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/arrase/gcp-arch/internal/icons"
	"github.com/arrase/gcp-arch/internal/manifest"
)

// PreparedDocument holds the fully resolved layout structures for HTML emission and JS routing.
type PreparedDocument struct {
	Metadata     manifest.Metadata
	Views        []PreparedView
	UsedIconKeys []string
}

// PreparedView contains the pre-computed grid tracks, containers, cards, edges, and footnotes for one view.
type PreparedView struct {
	Index           int
	ID              string
	Title           string
	Subtitle        string
	Archetype       manifest.Archetype
	AccentMode      manifest.AccentMode
	NumCols         int
	NumRows         int
	HasRightEdgeCol bool
	StageBanners    []StageBanner
	ColTracksCSS    string
	RowTracksCSS    string
	CorridorCols    []int
	CorridorRows    []int
	Containers      []PreparedContainer
	Cards           []PreparedCard
	Waypoints       []PreparedWaypoint
	Edges           []RoutingEdge
	Footnotes       []PreparedFootnote
}

// StageBanner labels a column in a stage_pipeline view.
type StageBanner struct {
	GridCol int
	Label   string
}

// PreparedContainer holds grid coordinates and nesting metadata for a rendered container box.
type PreparedContainer struct {
	ID           string
	Title        string
	Kind         manifest.ContainerKind
	Depth        int
	HasInnerTop  bool
	GridColSpec  string
	GridRowSpec  string
	BadgeGridCol int
	BadgeGridRow int
}

// PreparedCard holds resolved display fields and CSS Grid coordinates for a rendered component card.
type PreparedCard struct {
	ComponentID string
	Title       string
	Subtitle    string
	IconKey     string
	GridCol     int
	GridRow     int
	CSSGridCol  int
	CSSGridRow  int
	MinHeightPx int
	InlineMeta  []manifest.InlineMeta
	OffPageRefs []manifest.OffPageRef
}

// PreparedWaypoint marks an intermediate horizontal corridor cell traversed by a multi-column edge.
type PreparedWaypoint struct {
	EdgeIdx    int
	CSSGridCol int
	CSSGridRow int
}

// RoutingEdge is serialized to JSON for router.js to measure and route orthogonal SVG connectors.
type RoutingEdge struct {
	From            string              `json:"from"`
	To              string              `json:"to"`
	Verb            string              `json:"verb"`
	FlowKind        manifest.FlowKind   `json:"flow_kind"`
	Provenance      manifest.Provenance `json:"provenance"`
	BusGroup        string              `json:"bus_group,omitempty"`
	BusKind         string              `json:"bus_kind,omitempty"` // "out" | "in"
	WaypointRow     int                 `json:"waypoint_row"`
	LateralCorridor int                 `json:"lateral_corridor"`
	LateralSide     string              `json:"lateral_side,omitempty"` // "left" | "right"
	FootnoteNum     int                 `json:"footnote_num,omitempty"`
}

// PreparedFootnote records a numbered inference rationale rendered below a view sheet.
type PreparedFootnote struct {
	Number    int
	EdgeLabel string
	Rationale string
}

// PrepareDocument runs deterministic barycenter row sorting, flat container span calculation,
// multi-column waypoint / same-column lateral detour assignment, and >=2-edge bus spine grouping.
func PrepareDocument(m *manifest.Manifest, reg *icons.Registry) *PreparedDocument {
	compByID := make(map[string]manifest.Component, len(m.Components))
	for _, c := range m.Components {
		compByID[c.ID] = c
	}
	contByID := make(map[string]manifest.Container, len(m.Containers))
	for _, ct := range m.Containers {
		contByID[ct.ID] = ct
	}
	detailByID := make(map[string]manifest.View, len(m.Views))
	for _, v := range m.Views {
		if v.ID != manifest.SystemIndexViewID {
			detailByID[v.ID] = v
		}
	}

	usedIcons := make(map[string]bool)
	views := make([]PreparedView, 0, len(m.Views))
	for i, v := range m.Views {
		pv := prepareView(i, v, compByID, contByID, detailByID, reg, usedIcons)
		views = append(views, pv)
	}

	iconKeys := make([]string, 0, len(usedIcons))
	for k := range usedIcons {
		iconKeys = append(iconKeys, k)
	}
	sort.Strings(iconKeys)

	return &PreparedDocument{
		Metadata:     m.Metadata,
		Views:        views,
		UsedIconKeys: iconKeys,
	}
}

type cardSlot struct {
	placement manifest.Placement
	yamlIdx   int
	gridCol   int
	outerCont string
	innerCont string
	row       int
}

type baryGroupKey struct {
	hasBary bool
	minBary float64
	minYAML int
}

type containerSpan struct {
	minCol, maxCol int
	minRow, maxRow int
	hasMember      bool
}

func prepareView(
	index int,
	v manifest.View,
	compByID map[string]manifest.Component,
	contByID map[string]manifest.Container,
	detailByID map[string]manifest.View,
	reg *icons.Registry,
	usedIcons map[string]bool,
) PreparedView {
	colMap, origCols, numCols := computeDenseColumns(v)

	viewContSet := make(map[string]bool, len(v.ContainerIDs))
	for _, cid := range v.ContainerIDs {
		viewContSet[cid] = true
	}

	cols, slotsByID := buildCardSlots(v.Placements, colMap, numCols, viewContSet, contByID)
	runBarycenterSweeps(cols, slotsByID, v.Edges, numCols)
	numRows := maxColumnRows(cols)

	preparedContainers, hasInnerTopAtRow, badgeAtRowCol := buildPreparedContainers(v.ContainerIDs, cols, viewContSet, contByID)
	busGroup, busKind := assignBusGroups(v.Edges, slotsByID)
	routingEdges, waypoints, footnotes, portCounts, hasRightEdgeCol := buildRoutingEdges(v.Edges, slotsByID, busGroup, busKind, numCols, numRows, badgeAtRowCol)
	cards := buildPreparedCards(v.ID, cols, compByID, detailByID, reg, usedIcons, portCounts)
	colTracks, rowTracks, corridorCols, corridorRows := buildGridTracks(numCols, numRows, hasRightEdgeCol, routingEdges, hasInnerTopAtRow, badgeAtRowCol)
	banners := buildStageBanners(v.Archetype, origCols, cards, compByID)

	return PreparedView{
		Index:           index,
		ID:              v.ID,
		Title:           v.Title,
		Subtitle:        v.Subtitle,
		Archetype:       v.Archetype,
		AccentMode:      v.AccentMode,
		NumCols:         numCols,
		NumRows:         numRows,
		HasRightEdgeCol: hasRightEdgeCol,
		StageBanners:    banners,
		ColTracksCSS:    strings.Join(colTracks, " "),
		RowTracksCSS:    strings.Join(rowTracks, " "),
		CorridorCols:    corridorCols,
		CorridorRows:    corridorRows,
		Containers:      preparedContainers,
		Cards:           cards,
		Waypoints:       waypoints,
		Edges:           routingEdges,
		Footnotes:       footnotes,
	}
}

func computeDenseColumns(v manifest.View) (map[int]int, []int, int) {
	colMap := make(map[int]int)
	var origCols []int
	if v.Archetype == manifest.ArchetypeHubSpoke {
		origCols = []int{0, 1, 2}
		colMap[0], colMap[1], colMap[2] = 0, 1, 2
		return colMap, origCols, 3
	}
	seenCol := make(map[int]bool)
	for _, p := range v.Placements {
		if !seenCol[p.Column] {
			seenCol[p.Column] = true
			origCols = append(origCols, p.Column)
		}
	}
	sort.Ints(origCols)
	for dense, orig := range origCols {
		colMap[orig] = dense
	}
	numCols := len(origCols)
	if numCols == 0 {
		numCols = 1
	}
	return colMap, origCols, numCols
}

func buildCardSlots(
	placements []manifest.Placement,
	colMap map[int]int,
	numCols int,
	viewContSet map[string]bool,
	contByID map[string]manifest.Container,
) ([][]*cardSlot, map[string]*cardSlot) {
	slotsByID := make(map[string]*cardSlot, len(placements))
	cols := make([][]*cardSlot, numCols)
	for i, p := range placements {
		gc := colMap[p.Column]
		s := &cardSlot{
			placement: p,
			yamlIdx:   i,
			gridCol:   gc,
			outerCont: resolveOuterContainer(p.ContainerID, viewContSet, contByID),
			innerCont: p.ContainerID,
			row:       len(cols[gc]),
		}
		slotsByID[p.ComponentID] = s
		cols[gc] = append(cols[gc], s)
	}
	return cols, slotsByID
}

func resolveOuterContainer(cid string, viewContSet map[string]bool, contByID map[string]manifest.Container) string {
	if cid == "" || !viewContSet[cid] {
		return ""
	}
	ct := contByID[cid]
	if ct.ParentContainerID != "" && viewContSet[ct.ParentContainerID] {
		return ct.ParentContainerID
	}
	return cid
}

func runBarycenterSweeps(cols [][]*cardSlot, slotsByID map[string]*cardSlot, edges []manifest.Edge, numCols int) {
	neighbors := make(map[string][]string, len(slotsByID))
	for _, e := range edges {
		fs, okF := slotsByID[e.From]
		ts, okT := slotsByID[e.To]
		if !okF || !okT || fs.gridCol == ts.gridCol {
			continue
		}
		neighbors[e.From] = append(neighbors[e.From], e.To)
		neighbors[e.To] = append(neighbors[e.To], e.From)
	}
	for c := 1; c < numCols; c++ {
		sortColumnByBarycenter(cols[c], slotsByID, neighbors)
	}
	for c := numCols - 2; c >= 0; c-- {
		sortColumnByBarycenter(cols[c], slotsByID, neighbors)
	}
	sortColumnByBarycenter(cols[numCols-1], slotsByID, neighbors)
}

func sortColumnByBarycenter(colSlots []*cardSlot, slotsByID map[string]*cardSlot, neighbors map[string][]string) {
	if len(colSlots) <= 1 {
		return
	}
	bary, hasBary := computeSlotBarycenters(colSlots, slotsByID, neighbors)
	outerKeys, innerKeys := computeContainerGroupKeys(colSlots, bary, hasBary)

	slices.SortFunc(colSlots, func(a, b *cardSlot) int {
		if a.outerCont != b.outerCont {
			return compareContainerSlot(a, b, a.outerCont, b.outerCont, outerKeys, bary, hasBary)
		}
		if a.innerCont != b.innerCont {
			return compareContainerSlot(a, b, a.innerCont, b.innerCont, innerKeys, bary, hasBary)
		}
		return compareBaryGroup(slotGroupKey(a, bary, hasBary), slotGroupKey(b, bary, hasBary))
	})
	for r, s := range colSlots {
		s.row = r
	}
}

func computeSlotBarycenters(colSlots []*cardSlot, slotsByID map[string]*cardSlot, neighbors map[string][]string) (map[string]float64, map[string]bool) {
	bary := make(map[string]float64, len(colSlots))
	hasBary := make(map[string]bool, len(colSlots))
	for _, s := range colSlots {
		nbs := neighbors[s.placement.ComponentID]
		if len(nbs) == 0 {
			continue
		}
		rows := make([]int, 0, len(nbs))
		for _, nb := range nbs {
			if ns, ok := slotsByID[nb]; ok {
				rows = append(rows, ns.row)
			}
		}
		if len(rows) == 0 {
			continue
		}
		sort.Ints(rows)
		mid := len(rows) / 2
		if len(rows)%2 == 1 {
			bary[s.placement.ComponentID] = float64(rows[mid])
		} else {
			bary[s.placement.ComponentID] = float64(rows[mid-1]+rows[mid]) / 2.0
		}
		hasBary[s.placement.ComponentID] = true
	}
	return bary, hasBary
}

func computeContainerGroupKeys(colSlots []*cardSlot, bary map[string]float64, hasBary map[string]bool) (map[string]baryGroupKey, map[string]baryGroupKey) {
	outerKeys := make(map[string]baryGroupKey)
	innerKeys := make(map[string]baryGroupKey)
	for _, s := range colSlots {
		if s.outerCont != "" {
			updateGroupKey(outerKeys, s.outerCont, s, bary, hasBary)
		}
		if s.innerCont != "" {
			updateGroupKey(innerKeys, s.innerCont, s, bary, hasBary)
		}
	}
	return outerKeys, innerKeys
}

func updateGroupKey(m map[string]baryGroupKey, k string, s *cardSlot, bary map[string]float64, hasBary map[string]bool) {
	hb := hasBary[s.placement.ComponentID]
	bv := bary[s.placement.ComponentID]
	cur, exists := m[k]
	if !exists {
		m[k] = baryGroupKey{hasBary: hb, minBary: bv, minYAML: s.yamlIdx}
		return
	}
	if s.yamlIdx < cur.minYAML {
		cur.minYAML = s.yamlIdx
	}
	if hb && (!cur.hasBary || bv < cur.minBary) {
		cur.hasBary = true
		cur.minBary = bv
	}
	m[k] = cur
}

func slotGroupKey(s *cardSlot, bary map[string]float64, hasBary map[string]bool) baryGroupKey {
	return baryGroupKey{
		hasBary: hasBary[s.placement.ComponentID],
		minBary: bary[s.placement.ComponentID],
		minYAML: s.yamlIdx,
	}
}

func compareContainerSlot(a, b *cardSlot, contA, contB string, keys map[string]baryGroupKey, bary map[string]float64, hasBary map[string]bool) int {
	ka := slotGroupKey(a, bary, hasBary)
	if contA != "" {
		ka = keys[contA]
	}
	kb := slotGroupKey(b, bary, hasBary)
	if contB != "" {
		kb = keys[contB]
	}
	if c := compareBaryGroup(ka, kb); c != 0 {
		return c
	}
	return cmp.Compare(contA, contB)
}

func compareBaryGroup(a, b baryGroupKey) int {
	if a.hasBary && b.hasBary && a.minBary != b.minBary {
		return cmp.Compare(a.minBary, b.minBary)
	}
	if a.hasBary != b.hasBary {
		if a.hasBary {
			return -1
		}
		return 1
	}
	return cmp.Compare(a.minYAML, b.minYAML)
}

func maxColumnRows(cols [][]*cardSlot) int {
	numRows := 1
	for _, colSlots := range cols {
		if len(colSlots) > numRows {
			numRows = len(colSlots)
		}
	}
	return numRows
}

func buildPreparedContainers(
	containerIDs []string,
	cols [][]*cardSlot,
	viewContSet map[string]bool,
	contByID map[string]manifest.Container,
) ([]PreparedContainer, map[int]bool, map[[2]int]bool) {
	spans := computeContainerSpans(cols, viewContSet, contByID)
	sortedContIDs := slices.Clone(containerIDs)
	sort.Strings(sortedContIDs)

	hasInnerTop, hasInnerTopAtRow := findContainersWithInnerTop(sortedContIDs, spans, viewContSet, contByID)

	badgeAtRowCol := make(map[[2]int]bool)
	var prepared []PreparedContainer
	for _, cid := range sortedContIDs {
		sp := spans[cid]
		if sp == nil || !sp.hasMember {
			continue
		}
		ct := contByID[cid]
		depth := 1
		if isChildContainerInView(ct, viewContSet) {
			depth = 2
		}
		badgeAtRowCol[[2]int{sp.minRow, sp.minCol}] = true
		prepared = append(prepared, PreparedContainer{
			ID:           ct.ID,
			Title:        ct.Title,
			Kind:         ct.Kind,
			Depth:        depth,
			HasInnerTop:  hasInnerTop[ct.ID],
			GridColSpec:  fmt.Sprintf("%d / %d", 2*sp.minCol+1, 2*sp.maxCol+2),
			GridRowSpec:  fmt.Sprintf("%d / %d", 2*sp.minRow+2, 2*sp.maxRow+3),
			BadgeGridCol: 2*sp.minCol + 1,
			BadgeGridRow: 2*sp.minRow + 2,
		})
	}
	slices.SortFunc(prepared, func(a, b PreparedContainer) int {
		return cmp.Or(cmp.Compare(a.Depth, b.Depth), cmp.Compare(a.ID, b.ID))
	})
	return prepared, hasInnerTopAtRow, badgeAtRowCol
}

func isChildContainerInView(ct manifest.Container, viewContSet map[string]bool) bool {
	return ct.ParentContainerID != "" && viewContSet[ct.ParentContainerID]
}

func findContainersWithInnerTop(
	sortedContIDs []string,
	spans map[string]*containerSpan,
	viewContSet map[string]bool,
	contByID map[string]manifest.Container,
) (map[string]bool, map[int]bool) {
	hasInnerTop := make(map[string]bool)
	hasInnerTopAtRow := make(map[int]bool)
	for _, cid := range sortedContIDs {
		sp := spans[cid]
		if sp == nil || !sp.hasMember {
			continue
		}
		ct := contByID[cid]
		if !isChildContainerInView(ct, viewContSet) {
			continue
		}
		if psp := spans[ct.ParentContainerID]; psp != nil && psp.minRow == sp.minRow {
			hasInnerTop[ct.ParentContainerID] = true
			hasInnerTopAtRow[psp.minRow] = true
		}
	}
	return hasInnerTop, hasInnerTopAtRow
}

func computeContainerSpans(cols [][]*cardSlot, viewContSet map[string]bool, contByID map[string]manifest.Container) map[string]*containerSpan {
	spans := make(map[string]*containerSpan, len(viewContSet))
	record := func(cid string, col, row int) {
		sp, ok := spans[cid]
		if !ok {
			spans[cid] = &containerSpan{minCol: col, maxCol: col, minRow: row, maxRow: row, hasMember: true}
			return
		}
		sp.minCol = min(sp.minCol, col)
		sp.maxCol = max(sp.maxCol, col)
		sp.minRow = min(sp.minRow, row)
		sp.maxRow = max(sp.maxRow, row)
	}
	for _, colSlots := range cols {
		for _, s := range colSlots {
			cid := s.innerCont
			if cid == "" || !viewContSet[cid] {
				continue
			}
			record(cid, s.gridCol, s.row)
			parent := contByID[cid].ParentContainerID
			if parent != "" && viewContSet[parent] {
				record(parent, s.gridCol, s.row)
			}
		}
	}
	return spans
}

func assignBusGroups(edges []manifest.Edge, slotsByID map[string]*cardSlot) ([]string, []string) {
	busGroup := make([]string, len(edges))
	busKind := make([]string, len(edges))

	assignExplicitBusGroups(edges, slotsByID, busGroup, busKind)
	assignAutoBusGroups(edges, slotsByID, busGroup, busKind, "out")
	assignAutoBusGroups(edges, slotsByID, busGroup, busKind, "in")
	return busGroup, busKind
}

func edgeHorizontalDir(e manifest.Edge, slotsByID map[string]*cardSlot) int {
	fs, okF := slotsByID[e.From]
	ts, okT := slotsByID[e.To]
	if !okF || !okT {
		return 0
	}
	return cmp.Compare(ts.gridCol, fs.gridCol)
}

func assignExplicitBusGroups(edges []manifest.Edge, slotsByID map[string]*cardSlot, busGroup, busKind []string) {
	type explicitKey struct {
		bid string
		dir int
	}
	groups := make(map[explicitKey][]int)
	for i, e := range edges {
		if e.ViaBusID == "" {
			continue
		}
		d := edgeHorizontalDir(e, slotsByID)
		if d == 0 {
			continue
		}
		k := explicitKey{bid: e.ViaBusID, dir: d}
		groups[k] = append(groups[k], i)
	}
	for k, idxs := range groups {
		if len(idxs) < 2 {
			continue
		}
		froms := make(map[string]bool)
		for _, idx := range idxs {
			froms[edges[idx].From] = true
		}
		bk := "in"
		if len(froms) == 1 {
			bk = "out"
		}
		gid := fmt.Sprintf("explicit_%s_%d", k.bid, k.dir)
		for _, idx := range idxs {
			busGroup[idx] = gid
			busKind[idx] = bk
		}
	}
}

func assignAutoBusGroups(edges []manifest.Edge, slotsByID map[string]*cardSlot, busGroup, busKind []string, kind string) {
	type dirKey struct {
		node string
		fk   manifest.FlowKind
		dir  int
	}
	buckets := make(map[dirKey][]int)
	for i, e := range edges {
		if busGroup[i] != "" {
			continue
		}
		d := edgeHorizontalDir(e, slotsByID)
		if d == 0 {
			continue
		}
		node := e.From
		if kind == "in" {
			node = e.To
		}
		k := dirKey{node: node, fk: e.FlowKind, dir: d}
		buckets[k] = append(buckets[k], i)
	}
	for k, idxs := range buckets {
		if len(idxs) < 2 {
			continue
		}
		gid := fmt.Sprintf("auto_%s_%s_%s_%d", kind, k.node, k.fk, k.dir)
		for _, idx := range idxs {
			busGroup[idx] = gid
			busKind[idx] = kind
		}
	}
}

func buildRoutingEdges(
	edges []manifest.Edge,
	slotsByID map[string]*cardSlot,
	busGroup, busKind []string,
	numCols, numRows int,
	badgeAtRowCol map[[2]int]bool,
) ([]RoutingEdge, []PreparedWaypoint, []PreparedFootnote, map[string]int, bool) {
	hasRightEdgeCol := false
	routingEdges := make([]RoutingEdge, 0, len(edges))
	var waypoints []PreparedWaypoint
	var footnotes []PreparedFootnote
	portCounts := make(map[string]int)
	countedBusPort := make(map[string]bool)

	for i, e := range edges {
		fs, okF := slotsByID[e.From]
		ts, okT := slotsByID[e.To]
		if !okF || !okT {
			continue
		}

		re := RoutingEdge{
			From:       e.From,
			To:         e.To,
			Verb:       e.Verb,
			FlowKind:   e.FlowKind,
			Provenance: e.Provenance,
			BusGroup:   busGroup[i],
			BusKind:    busKind[i],
		}
		if e.Provenance == manifest.ProvenanceInferred {
			fnNum := len(footnotes) + 1
			re.FootnoteNum = fnNum
			footnotes = append(footnotes, PreparedFootnote{
				Number:    fnNum,
				EdgeLabel: e.From + " -> " + e.To,
				Rationale: e.InferenceRationale,
			})
		}

		if recordEdgePorts(&re, fs, ts, numCols, portCounts, countedBusPort) {
			hasRightEdgeCol = true
		}

		if absInt(ts.gridCol-fs.gridCol) > 1 {
			loCol, hiCol := min(fs.gridCol, ts.gridCol), max(fs.gridCol, ts.gridCol)
			hRow := pickClearWaypointRow(loCol, hiCol, fs.row, ts.row, numRows, badgeAtRowCol)
			re.WaypointRow = hRow
			for mid := loCol + 1; mid < hiCol; mid++ {
				waypoints = append(waypoints, PreparedWaypoint{
					EdgeIdx:    len(routingEdges),
					CSSGridCol: 2*mid + 1,
					CSSGridRow: 2*hRow + 1,
				})
			}
		}

		routingEdges = append(routingEdges, re)
	}
	return routingEdges, waypoints, footnotes, portCounts, hasRightEdgeCol
}

func recordEdgePorts(
	re *RoutingEdge,
	fs, ts *cardSlot,
	numCols int,
	portCounts map[string]int,
	countedBusPort map[string]bool,
) bool {
	if ts.gridCol > fs.gridCol {
		incrementPort(re.From+"|R", re.BusGroup, re.BusKind == "out", portCounts, countedBusPort)
		incrementPort(re.To+"|L", re.BusGroup, re.BusKind == "in", portCounts, countedBusPort)
		return false
	}
	if ts.gridCol < fs.gridCol {
		portCounts[re.From+"|L"]++
		portCounts[re.To+"|R"]++
		return false
	}
	if fs.gridCol < numCols-1 {
		re.LateralCorridor = fs.gridCol
		re.LateralSide = "right"
		portCounts[re.From+"|R"]++
		portCounts[re.To+"|R"]++
		return false
	}
	if numCols > 1 {
		re.LateralCorridor = fs.gridCol - 1
		re.LateralSide = "left"
		portCounts[re.From+"|L"]++
		portCounts[re.To+"|L"]++
		return false
	}
	re.LateralCorridor = 0
	re.LateralSide = "right"
	portCounts[re.From+"|R"]++
	portCounts[re.To+"|R"]++
	return true
}

func incrementPort(portKey, busGroup string, isSharedBus bool, portCounts map[string]int, countedBusPort map[string]bool) {
	if busGroup != "" && isSharedBus {
		if countedBusPort[busGroup] {
			return
		}
		countedBusPort[busGroup] = true
	}
	portCounts[portKey]++
}

func buildPreparedCards(
	viewID string,
	cols [][]*cardSlot,
	compByID map[string]manifest.Component,
	detailByID map[string]manifest.View,
	reg *icons.Registry,
	usedIcons map[string]bool,
	portCounts map[string]int,
) []PreparedCard {
	var cards []PreparedCard
	for _, colSlots := range cols {
		for _, s := range colSlots {
			p := s.placement
			card := resolveCard(viewID, p, compByID, detailByID, reg)
			usedIcons[card.IconKey] = true
			card.GridCol = s.gridCol
			card.GridRow = s.row
			card.CSSGridCol = 2*s.gridCol + 1
			card.CSSGridRow = 2*s.row + 2
			card.OffPageRefs = slices.Clone(p.OffPageRefs)

			maxPorts := max(portCounts[p.ComponentID+"|L"], portCounts[p.ComponentID+"|R"])
			if maxPorts >= 3 {
				card.MinHeightPx = max(54, 24+(maxPorts-1)*24)
			}
			cards = append(cards, card)
		}
	}
	return cards
}

func buildGridTracks(
	numCols, numRows int,
	hasRightEdgeCol bool,
	routingEdges []RoutingEdge,
	hasInnerTopAtRow map[int]bool,
	badgeAtRowCol map[[2]int]bool,
) ([]string, []string, []int, []int) {
	numVCorridors := numCols - 1
	if hasRightEdgeCol {
		numVCorridors = numCols
	}
	corridorCols := make([]int, numVCorridors)
	for i := range numVCorridors {
		corridorCols[i] = i
	}
	corridorRows := make([]int, numRows+1)
	for j := range numRows + 1 {
		corridorRows[j] = j
	}

	colTracks := make([]string, 0, numCols+numVCorridors)
	for c := range numCols {
		colTracks = append(colTracks, "minmax(0, 1fr)")
		if c < numVCorridors {
			colTracks = append(colTracks, fmt.Sprintf("var(--corridor-col-%d, var(--corridor-col-default))", c))
		}
	}

	waypointRows := make(map[int]bool)
	for _, re := range routingEdges {
		if re.WaypointRow > 0 {
			waypointRows[re.WaypointRow] = true
		}
	}
	hasBadgeAtRow := make(map[int]bool)
	for k := range badgeAtRowCol {
		hasBadgeAtRow[k[0]] = true
	}

	rowTracks := make([]string, 0, 2*numRows+1)
	for r := range numRows + 1 {
		defRow := defaultRowCorridorTrack(waypointRows[r], hasInnerTopAtRow[r], hasBadgeAtRow[r])
		rowTracks = append(rowTracks, fmt.Sprintf("var(--corridor-row-%d, %s)", r, defRow))
		if r < numRows {
			rowTracks = append(rowTracks, "auto")
		}
	}
	return colTracks, rowTracks, corridorCols, corridorRows
}

func defaultRowCorridorTrack(hasWaypoint, hasInnerTop, hasBadge bool) string {
	switch {
	case hasWaypoint && hasInnerTop:
		return "calc(var(--corridor-row-default) + 46px)"
	case hasInnerTop:
		return "52px"
	case hasWaypoint:
		return "var(--corridor-row-default)"
	case hasBadge:
		return "32px"
	default:
		return "18px"
	}
}

func buildStageBanners(archetype manifest.Archetype, origCols []int, cards []PreparedCard, compByID map[string]manifest.Component) []StageBanner {
	if archetype != manifest.ArchetypeStagePipeline {
		return nil
	}
	banners := make([]StageBanner, 0, len(origCols))
	for dense := range origCols {
		lbl := stageBannerLabel(dense, cards, compByID)
		banners = append(banners, StageBanner{GridCol: 2*dense + 1, Label: lbl})
	}
	return banners
}

func pickClearWaypointRow(loCol, hiCol, fromRow, toRow, numRows int, badgeAtRowCol map[[2]int]bool) int {
	hasBadge := func(hRow int) bool {
		if hRow >= numRows {
			return false
		}
		for c := loCol + 1; c < hiCol; c++ {
			if badgeAtRowCol[[2]int{hRow, c}] {
				return true
			}
		}
		return false
	}

	target := (fromRow + toRow + 1) / 2
	bestRow := numRows // bottom corridor never has top-anchored container badges
	bestDist := 1 << 30
	// Prefer bottom corridor when target is near bottom, or closest clear corridor in [1..numRows].
	for hRow := 1; hRow <= numRows; hRow++ {
		if hasBadge(hRow) {
			continue
		}
		dist := absInt(hRow - target)
		if dist < bestDist {
			bestDist = dist
			bestRow = hRow
		}
	}
	return bestRow
}

func resolveCard(
	viewID string,
	p manifest.Placement,
	compByID map[string]manifest.Component,
	detailByID map[string]manifest.View,
	reg *icons.Registry,
) PreparedCard {
	if c, ok := compByID[p.ComponentID]; ok {
		return PreparedCard{
			ComponentID: c.ID,
			Title:       c.Title,
			Subtitle:    c.Subtitle,
			IconKey:     c.IconKey,
			InlineMeta:  slices.Clone(c.InlineMeta),
		}
	}
	if viewID == manifest.SystemIndexViewID {
		if dv, ok := detailByID[p.ComponentID]; ok {
			iconKey := systemIndexViewIcon(dv, reg)
			sub := dv.Subtitle
			if sub == "" {
				sub = fmt.Sprintf("%d components · %s", len(dv.Placements), dv.Archetype)
			}
			return PreparedCard{
				ComponentID: dv.ID,
				Title:       dv.Title,
				Subtitle:    sub,
				IconKey:     iconKey,
				InlineMeta: []manifest.InlineMeta{
					{Label: "Archetype", Value: string(dv.Archetype)},
					{Label: "Blocks", Value: fmt.Sprintf("%d", len(dv.Placements))},
				},
			}
		}
	}
	return PreparedCard{
		ComponentID: p.ComponentID,
		Title:       p.ComponentID,
		IconKey:     "family_compute",
	}
}

func systemIndexViewIcon(dv manifest.View, reg *icons.Registry) string {
	var candidates []string
	switch dv.Archetype {
	case manifest.ArchetypeStagePipeline:
		candidates = []string{"cloud_build", "family_integration", "family_compute"}
	case manifest.ArchetypeHubSpoke:
		candidates = []string{"virtual_private_cloud", "family_network", "family_compute"}
	default:
		if dv.AccentMode == manifest.AccentCICD {
			candidates = []string{"cloud_build", "family_compute"}
		} else {
			candidates = []string{"google_kubernetes_engine", "family_compute"}
		}
	}
	for _, k := range candidates {
		if reg == nil || reg.Has(k) {
			return k
		}
	}
	return candidates[len(candidates)-1]
}

func stageBannerLabel(denseCol int, cards []PreparedCard, compByID map[string]manifest.Component) string {
	counts := make(map[manifest.ComponentKind]int)
	for _, card := range cards {
		if card.GridCol != denseCol {
			continue
		}
		if comp, ok := compByID[card.ComponentID]; ok {
			counts[comp.ComponentKind]++
		}
	}
	var bestKind manifest.ComponentKind
	bestCount := 0
	for _, k := range []manifest.ComponentKind{
		manifest.KindExternalCaller,
		manifest.KindIngress,
		manifest.KindPipeline,
		manifest.KindWorkload,
		manifest.KindMessaging,
		manifest.KindDatastore,
		manifest.KindCohort,
		manifest.KindExternalTarget,
	} {
		if counts[k] > bestCount {
			bestCount = counts[k]
			bestKind = k
		}
	}
	var role string
	switch bestKind {
	case manifest.KindExternalCaller:
		role = "Source"
	case manifest.KindIngress:
		role = "Ingress"
	case manifest.KindPipeline:
		role = "Build & CI"
	case manifest.KindWorkload:
		role = "Workloads"
	case manifest.KindMessaging:
		role = "Messaging"
	case manifest.KindDatastore:
		role = "Artifacts & State"
	case manifest.KindCohort:
		role = "Services"
	case manifest.KindExternalTarget:
		role = "Deploy Targets"
	default:
		return fmt.Sprintf("Stage %d", denseCol+1)
	}
	return fmt.Sprintf("Stage %d · %s", denseCol+1, role)
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
