package synth

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/arrase/gcp-arch/internal/manifest"
)

const maxSystemIndexPlacements = 16

// BuildSystemIndexView deterministically projects View 0 (view_0_system_index) from populated
// detail views and cross-view relationships when >= 2 detail views exist. Returns nil when fewer
// than 2 detail views exist.
func BuildSystemIndexView(m *manifest.Manifest) *manifest.View {
	if m == nil {
		return nil
	}

	detailViews := collectSortedDetailViews(m.Views)
	if len(detailViews) < 2 {
		return nil
	}

	compByID := make(map[string]manifest.Component, len(m.Components))
	for _, c := range m.Components {
		compByID[c.ID] = c
	}
	rejected := make(map[[2]string]bool, len(m.RejectedEdges))
	for _, re := range m.RejectedEdges {
		rejected[[2]string{re.From, re.To}] = true
	}

	extCallers, extTargets, viewPlaced := collectView0Externals(detailViews, compByID)
	placements, placedInIndex, colOf := buildView0Placements(detailViews, extCallers, extTargets)
	idx := &view0Index{
		detailViews:   detailViews,
		extCallers:    extCallers,
		extTargets:    extTargets,
		viewPlaced:    viewPlaced,
		compByID:      compByID,
		placedInIndex: placedInIndex,
		colOf:         colOf,
		rejected:      rejected,
	}
	edges := idx.buildEdges()

	return &manifest.View{
		ID:         manifest.SystemIndexViewID,
		Title:      "System Index Map",
		Subtitle:   fmt.Sprintf("%d logical views across %d architecture components", len(detailViews), len(m.Components)),
		Archetype:  manifest.ArchetypeLandscapeMatrix,
		AccentMode: manifest.AccentMixed,
		Placements: placements,
		Edges:      edges,
	}
}

func collectSortedDetailViews(views []manifest.View) []manifest.View {
	var detailViews []manifest.View
	for _, v := range views {
		if v.ID != manifest.SystemIndexViewID {
			detailViews = append(detailViews, v)
		}
	}
	slices.SortFunc(detailViews, func(a, b manifest.View) int {
		return cmp.Compare(a.ID, b.ID)
	})
	if len(detailViews) > maxSystemIndexPlacements {
		detailViews = detailViews[:maxSystemIndexPlacements]
	}
	return detailViews
}

func collectView0Externals(detailViews []manifest.View, compByID map[string]manifest.Component) ([]string, []string, map[string]map[string]bool) {
	var extCallers, extTargets []string
	seenCaller := make(map[string]bool)
	seenTarget := make(map[string]bool)
	viewPlaced := make(map[string]map[string]bool, len(detailViews))

	for _, dv := range detailViews {
		placed := make(map[string]bool, len(dv.Placements))
		for _, p := range dv.Placements {
			placed[p.ComponentID] = true
			c, ok := compByID[p.ComponentID]
			if !ok {
				continue
			}
			if c.ComponentKind == manifest.KindExternalCaller && !seenCaller[c.ID] {
				seenCaller[c.ID] = true
				extCallers = append(extCallers, c.ID)
			} else if c.ComponentKind == manifest.KindExternalTarget && !seenTarget[c.ID] {
				seenTarget[c.ID] = true
				extTargets = append(extTargets, c.ID)
			}
		}
		viewPlaced[dv.ID] = placed
	}
	slices.Sort(extCallers)
	slices.Sort(extTargets)

	remaining := maxSystemIndexPlacements - len(detailViews)
	if len(extCallers) > remaining {
		extCallers = extCallers[:remaining]
	}
	remaining -= len(extCallers)
	if len(extTargets) > remaining {
		extTargets = extTargets[:remaining]
	}
	return extCallers, extTargets, viewPlaced
}

func buildView0Placements(detailViews []manifest.View, extCallers, extTargets []string) ([]manifest.Placement, map[string]bool, map[string]int) {
	placements := make([]manifest.Placement, 0, len(extCallers)+len(detailViews)+len(extTargets))
	placedInIndex := make(map[string]bool, maxSystemIndexPlacements)
	colOf := make(map[string]int, maxSystemIndexPlacements)

	for _, id := range extCallers {
		placements = append(placements, manifest.Placement{ComponentID: id, Column: 0})
		placedInIndex[id] = true
		colOf[id] = 0
	}

	hasHubSpoke := false
	for _, dv := range detailViews {
		if dv.Archetype == manifest.ArchetypeHubSpoke {
			hasHubSpoke = true
		}
		col := detailViewIndexColumn(dv)
		placements = append(placements, manifest.Placement{ComponentID: dv.ID, Column: col})
		placedInIndex[dv.ID] = true
		colOf[dv.ID] = col
	}

	targetCol := 3
	if hasHubSpoke {
		targetCol = 4
	}
	for _, id := range extTargets {
		placements = append(placements, manifest.Placement{ComponentID: id, Column: targetCol})
		placedInIndex[id] = true
		colOf[id] = targetCol
	}
	return placements, placedInIndex, colOf
}

type view0Index struct {
	detailViews   []manifest.View
	extCallers    []string
	extTargets    []string
	viewPlaced    map[string]map[string]bool
	compByID      map[string]manifest.Component
	placedInIndex map[string]bool
	colOf         map[string]int
	rejected      map[[2]string]bool
	edgeMap       map[viewEdgeKey]manifest.Edge
}

func (idx *view0Index) addEdge(from, to, verb string, fk manifest.FlowKind) {
	if from == to || !idx.placedInIndex[from] || !idx.placedInIndex[to] || idx.rejected[[2]string{from, to}] {
		return
	}
	k := viewEdgeKey{from: from, to: to}
	if _, exists := idx.edgeMap[k]; exists {
		return
	}
	idx.edgeMap[k] = manifest.Edge{
		From:       from,
		To:         to,
		Verb:       truncateVerb(verb),
		FlowKind:   fk,
		Provenance: manifest.ProvenanceVerified,
	}
}

func (idx *view0Index) buildEdges() []manifest.Edge {
	idx.edgeMap = make(map[viewEdgeKey]manifest.Edge)

	idx.addExternalEdges()
	idx.addOffPageEdges()
	idx.addSharedComponentEdges()

	edges := make([]manifest.Edge, 0, len(idx.edgeMap))
	for _, e := range idx.edgeMap {
		edges = append(edges, e)
	}
	slices.SortFunc(edges, func(a, b manifest.Edge) int {
		return cmp.Or(
			cmp.Compare(a.From, b.From),
			cmp.Compare(a.To, b.To),
			cmp.Compare(a.FlowKind, b.FlowKind),
			cmp.Compare(a.Verb, b.Verb),
		)
	})
	return edges
}

func (idx *view0Index) addExternalEdges() {
	for _, dv := range idx.detailViews {
		for _, cid := range idx.extCallers {
			if fk, ok := callerFlowInView(dv, cid); ok {
				idx.addEdge(cid, dv.ID, callerVerb(fk), fk)
			}
		}
		for _, tid := range idx.extTargets {
			if fk, ok := targetFlowInView(dv, tid); ok {
				idx.addEdge(dv.ID, tid, targetVerb(fk), fk)
			}
		}
	}
}

func (idx *view0Index) addOffPageEdges() {
	for _, dv := range idx.detailViews {
		for _, p := range dv.Placements {
			for _, ref := range p.OffPageRefs {
				if ref.Direction == "OUT" && idx.placedInIndex[ref.TargetViewID] && ref.TargetViewID != dv.ID {
					idx.addEdge(dv.ID, ref.TargetViewID, ref.Label, flowFromAccent(dv.AccentMode))
				}
			}
		}
	}
}

func (idx *view0Index) addSharedComponentEdges() {
	for i := 0; i < len(idx.detailViews); i++ {
		for j := i + 1; j < len(idx.detailViews); j++ {
			idx.addSharedEdgeForPair(idx.detailViews[i], idx.detailViews[j])
		}
	}
}

func (idx *view0Index) addSharedEdgeForPair(a, b manifest.View) {
	if edgeExistsEitherWay(idx.edgeMap, a.ID, b.ID) {
		return
	}
	if !sharesNonExternalComponent(idx.viewPlaced[a.ID], idx.viewPlaced[b.ID], idx.compByID) {
		return
	}
	from, to := a, b
	if idx.colOf[b.ID] < idx.colOf[a.ID] {
		from, to = b, a
	}
	fk := manifest.FlowRuntime
	if from.AccentMode == manifest.AccentCICD && to.AccentMode == manifest.AccentCICD {
		fk = manifest.FlowCICD
	}
	idx.addEdge(from.ID, to.ID, "shared resources", fk)
}

func detailViewIndexColumn(dv manifest.View) int {
	switch dv.Archetype {
	case manifest.ArchetypeStagePipeline:
		return 1
	case manifest.ArchetypeHubSpoke:
		return 3
	default:
		return 2
	}
}

func flowPriority(fk manifest.FlowKind) int {
	switch fk {
	case manifest.FlowRuntime:
		return 0
	case manifest.FlowCICD:
		return 1
	default:
		return 2
	}
}

func callerFlowInView(dv manifest.View, callerID string) (manifest.FlowKind, bool) {
	var best manifest.FlowKind
	for _, e := range dv.Edges {
		if e.From == callerID {
			if best == "" || flowPriority(e.FlowKind) < flowPriority(best) {
				best = e.FlowKind
			}
		}
	}
	return best, best != ""
}

func targetFlowInView(dv manifest.View, targetID string) (manifest.FlowKind, bool) {
	var best manifest.FlowKind
	for _, e := range dv.Edges {
		if e.To == targetID {
			if best == "" || flowPriority(e.FlowKind) < flowPriority(best) {
				best = e.FlowKind
			}
		}
	}
	return best, best != ""
}

func callerVerb(fk manifest.FlowKind) string {
	switch fk {
	case manifest.FlowCICD:
		return "triggers CI/CD"
	case manifest.FlowAdmin:
		return "administers"
	default:
		return "calls ingress"
	}
}

func targetVerb(fk manifest.FlowKind) string {
	switch fk {
	case manifest.FlowCICD:
		return "deploys to"
	case manifest.FlowAdmin:
		return "configures"
	default:
		return "sends traffic"
	}
}

func flowFromAccent(a manifest.AccentMode) manifest.FlowKind {
	if a == manifest.AccentCICD {
		return manifest.FlowCICD
	}
	return manifest.FlowRuntime
}

type viewEdgeKey struct{ from, to string }

func edgeExistsEitherWay(edges map[viewEdgeKey]manifest.Edge, a, b string) bool {
	_, okAB := edges[viewEdgeKey{from: a, to: b}]
	_, okBA := edges[viewEdgeKey{from: b, to: a}]
	return okAB || okBA
}

func sharesNonExternalComponent(a, b map[string]bool, compByID map[string]manifest.Component) bool {
	for cid := range a {
		if !b[cid] {
			continue
		}
		c, ok := compByID[cid]
		if !ok {
			continue
		}
		if c.ComponentKind == manifest.KindExternalCaller || c.ComponentKind == manifest.KindExternalTarget {
			continue
		}
		return true
	}
	return false
}

func truncateVerb(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "cross-view flow"
	}
	if utf8.RuneCountInString(s) <= 16 {
		return s
	}
	runes := []rune(s)
	return strings.TrimSpace(string(runes[:16]))
}
