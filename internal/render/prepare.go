package render

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/arrase/gcp-arch/internal/icons"
	"github.com/arrase/gcp-arch/internal/manifest"
)

// PreparedDocument holds the fully resolved layout structures for HTML emission and JS routing.
type PreparedDocument struct {
	Views        []PreparedView
	UsedIconKeys []string
}

// PreparedView contains the nested container tree, topological card lanes, transversal panel,
// routing edges, and footnotes for one rendered diagram sheet.
type PreparedView struct {
	Index                   int
	ID                      string
	Title                   string
	Subtitle                string
	FocusSummary            string
	IncludeTransversalPanel bool
	IsDense                 bool
	StageBanners            []StageBanner
	RootContainers          []PreparedContainerNode
	LeftUncontainedLanes    []PreparedLane
	RightUncontainedLanes   []PreparedLane
	Edges                   []RoutingEdge
	TransversalGroups       []PreparedTransversalGroup
	TransversalCount        int
	SuppressedTotal         int
	SuppressedTypes         int
	SuppressedSummary       string
	Footnotes               []PreparedFootnote
	Excluded                []manifest.ExcludedResource
}

// StageBanner labels a topological column in a stage_pipeline view.
type StageBanner struct {
	Label string
}

// PreparedContainerNode represents a nested Flexbox container box and its children/lanes.
type PreparedContainerNode struct {
	ID                string
	Title             string
	Subtitle          string
	Kind              manifest.ContainerKind
	KindPill          string
	ExposureLower     string
	Depth             int
	BackedAnchor      bool
	Badges            []string
	StackVertical     bool
	SplitTwoColumn    bool
	SplitTopBottom    bool
	SplitSideStack    bool
	DirectLanes       []PreparedLane
	Children          []PreparedContainerNode
	PrimaryChildren   []PreparedContainerNode
	SecondaryChildren []PreparedContainerNode
}

// PreparedLane represents a vertical column of cards sharing a topological band inside a container.
type PreparedLane struct {
	Cards []PreparedCard
}

// PreparedCard holds resolved display fields for a rendered component card.
type PreparedCard struct {
	ComponentID string
	Title       string
	Subtitle    string
	IconKey     string
	RoleTag     string
	Wide        bool
	ExtraWide   bool
	TopoRank    int
	SubRank     int
	RawName     string
	Members     []string
	Badges      []manifest.ComponentBadge
	InlineMeta  []manifest.InlineMeta
	OffPageRefs []manifest.OffPageRef
}

// RoutingEdge is serialized to JSON for router.js to measure and route orthogonal SVG connectors.
type RoutingEdge struct {
	From         string            `json:"from"`
	To           string            `json:"to"`
	Verb         string            `json:"verb"`
	FlowKind     manifest.FlowKind `json:"flow_kind"`
	SemanticTag  string            `json:"semantic_tag,omitempty"`
	IsRetryOrDLQ bool              `json:"is_retry_or_dlq,omitempty"`
	Heuristic    bool              `json:"heuristic,omitempty"`
}

// PreparedFootnote records a numbered inference rationale rendered below a view sheet.
type PreparedFootnote struct {
	Number    int
	EdgeLabel string
	Rationale string
}

// PreparedTransversalGroup groups transversal items for the right-hand governance panel.
type PreparedTransversalGroup struct {
	Title string
	Items []PreparedTransversalItem
}

// PreparedTransversalItem holds display details for one transversal control-plane entry.
type PreparedTransversalItem struct {
	DisplayName string
	Meta        string
}

// PrepareDocument resolves hierarchical containers, topological card lanes, transversal side-bands,
// and routing edges for all views in m.
func PrepareDocument(m *manifest.Manifest, reg *icons.Registry) (*PreparedDocument, error) {
	if m == nil {
		return nil, fmt.Errorf("manifest must not be nil")
	}
	if reg == nil {
		return nil, fmt.Errorf("icons registry must not be nil")
	}

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
		views = append(views, prepareView(i, v, m, compByID, contByID, detailByID, reg, usedIcons))
	}

	if len(views) > 0 && len(m.ExcludedResources) > 0 {
		excl := slices.Clone(m.ExcludedResources)
		slices.SortFunc(excl, func(a, b manifest.ExcludedResource) int {
			return cmp.Compare(a.URI, b.URI)
		})
		views[0].Excluded = excl
	}

	iconKeys := make([]string, 0, len(usedIcons))
	for k := range usedIcons {
		iconKeys = append(iconKeys, k)
	}
	slices.Sort(iconKeys)

	return &PreparedDocument{
		Views:        views,
		UsedIconKeys: iconKeys,
	}, nil
}

func prepareView(
	index int,
	v manifest.View,
	m *manifest.Manifest,
	compByID map[string]manifest.Component,
	contByID map[string]manifest.Container,
	detailByID map[string]manifest.View,
	reg *icons.Registry,
	usedIcons map[string]bool,
) PreparedView {
	isOverview := index == 0 || v.ID == manifest.SystemIndexViewID
	viewParent := buildViewParents(v.ContainerIDs, contByID)

	placedSet, activeContainers, directCardsByCont, uncontainedCards := collectViewPlacements(
		v, compByID, contByID, detailByID, viewParent, reg, usedIcons,
	)
	activateEdgeContainers(v.Edges, contByID, viewParent, activeContainers)

	maxCardsPerLane := 4
	if len(v.Placements) > 28 {
		maxCardsPerLane = 6
	}

	rootContainers := buildContainerTree(v.ContainerIDs, activeContainers, viewParent, contByID, directCardsByCont, maxCardsPerLane)
	leftLanes, rightLanes := splitUncontainedLanes(uncontainedCards, maxCardsPerLane)
	routingEdges, footnotes := buildPreparedEdges(v.Edges, placedSet, activeContainers, compByID)
	banners := buildStageBanners(v.Archetype, v.Placements, compByID)

	includePanel := v.IncludeTransversalPanel || (isOverview && (len(m.TransversalItems) > 0 || len(m.SuppressedPlumbingCounts) > 0))
	var transGroups []PreparedTransversalGroup
	var transCount, suppressedTotal, suppressedTypes int
	var suppressedSummary string
	if includePanel {
		transGroups, transCount = buildTransversalGroups(m.TransversalItems, placedSet, activeContainers, isOverview)
		if isOverview && len(m.SuppressedPlumbingCounts) > 0 {
			suppressedTypes = len(m.SuppressedPlumbingCounts)
			suppressedSummary = formatSuppressedSummary(m.SuppressedPlumbingCounts, &suppressedTotal)
		}
	}

	return PreparedView{
		Index:                   index,
		ID:                      v.ID,
		Title:                   v.Title,
		Subtitle:                v.Subtitle,
		FocusSummary:            v.FocusSummary,
		IncludeTransversalPanel: includePanel && (transCount > 0 || suppressedTotal > 0),
		IsDense:                 isOverview && len(v.Placements) >= 15,
		StageBanners:            banners,
		RootContainers:          rootContainers,
		LeftUncontainedLanes:    leftLanes,
		RightUncontainedLanes:   rightLanes,
		Edges:                   routingEdges,
		TransversalGroups:       transGroups,
		TransversalCount:        transCount,
		SuppressedTotal:         suppressedTotal,
		SuppressedTypes:         suppressedTypes,
		SuppressedSummary:       suppressedSummary,
		Footnotes:               footnotes,
	}
}

func collectViewPlacements(
	v manifest.View,
	compByID map[string]manifest.Component,
	contByID map[string]manifest.Container,
	detailByID map[string]manifest.View,
	viewParent map[string]string,
	reg *icons.Registry,
	usedIcons map[string]bool,
) (
	placedSet map[string]bool,
	activeContainers map[string]bool,
	directCardsByCont map[string][]PreparedCard,
	uncontainedCards []PreparedCard,
) {
	placedSet = make(map[string]bool, len(v.Placements))
	activeContainers = make(map[string]bool, len(v.ContainerIDs))
	directCardsByCont = make(map[string][]PreparedCard)

	for pIdx, p := range v.Placements {
		placedSet[p.ComponentID] = true
		card := resolveCard(v.ID, p, pIdx, compByID, detailByID, reg)
		usedIcons[card.IconKey] = true

		if p.ContainerID != "" {
			if _, ok := contByID[p.ContainerID]; ok {
				directCardsByCont[p.ContainerID] = append(directCardsByCont[p.ContainerID], card)
				for cur := p.ContainerID; cur != ""; cur = viewParent[cur] {
					activeContainers[cur] = true
				}
				continue
			}
		}
		uncontainedCards = append(uncontainedCards, card)
	}
	return placedSet, activeContainers, directCardsByCont, uncontainedCards
}

func activateEdgeContainers(
	edges []manifest.Edge,
	contByID map[string]manifest.Container,
	viewParent map[string]string,
	activeContainers map[string]bool,
) {
	// Ensure containers explicitly referenced as edge endpoints remain active if present in contByID.
	for _, e := range edges {
		for _, id := range []string{e.From, e.To} {
			if _, ok := contByID[id]; !ok {
				continue
			}
			for cur := id; cur != ""; cur = viewParent[cur] {
				activeContainers[cur] = true
			}
		}
	}
}

func splitUncontainedLanes(uncontainedCards []PreparedCard, maxCardsPerLane int) ([]PreparedLane, []PreparedLane) {
	var leftUncontained, rightUncontained []PreparedCard
	for _, card := range uncontainedCards {
		if card.RoleTag == "external_caller" || (card.TopoRank <= 1 && card.RoleTag != "external_target") {
			leftUncontained = append(leftUncontained, card)
		} else {
			rightUncontained = append(rightUncontained, card)
		}
	}
	return groupCardsIntoLanes(leftUncontained, maxCardsPerLane), groupCardsIntoLanes(rightUncontained, maxCardsPerLane)
}

// buildViewParents maps every view container to its nearest ancestor also listed in
// view.container_ids ("" for top-level frames).
func buildViewParents(containerIDs []string, contByID map[string]manifest.Container) map[string]string {
	inView := make(map[string]bool, len(containerIDs))
	for _, cid := range containerIDs {
		inView[cid] = true
	}
	viewParent := make(map[string]string, len(containerIDs))
	for _, cid := range containerIDs {
		parent := contByID[cid].ParentContainerID
		seen := map[string]bool{cid: true}
		for parent != "" && !inView[parent] && !seen[parent] {
			seen[parent] = true
			parent = contByID[parent].ParentContainerID
		}
		if seen[parent] {
			parent = ""
		}
		viewParent[cid] = parent
	}
	return viewParent
}

func structuralDepth(cid string, viewParent map[string]string) int {
	depth := 1
	seen := map[string]bool{cid: true}
	for p := viewParent[cid]; p != "" && !seen[p]; p = viewParent[p] {
		seen[p] = true
		depth++
	}
	return depth
}

func buildContainerTree(
	containerIDs []string,
	active map[string]bool,
	viewParent map[string]string,
	contByID map[string]manifest.Container,
	directCards map[string][]PreparedCard,
	maxCardsPerLane int,
) []PreparedContainerNode {
	rootsRaw, childRaw := collectContainerHierarchy(containerIDs, active, viewParent, contByID)
	vpcCache := make(map[string]bool, len(active))
	sortContainersForLayout(rootsRaw, childRaw, vpcCache)
	for pid := range childRaw {
		sortContainersForLayout(childRaw[pid], childRaw, vpcCache)
	}

	roots := make([]PreparedContainerNode, 0, len(rootsRaw))
	for _, r := range rootsRaw {
		roots = append(roots, buildPreparedContainerNode(r, childRaw, viewParent, directCards, maxCardsPerLane, vpcCache))
	}
	return roots
}

func collectContainerHierarchy(
	containerIDs []string,
	active map[string]bool,
	viewParent map[string]string,
	contByID map[string]manifest.Container,
) ([]manifest.Container, map[string][]manifest.Container) {
	childRaw := make(map[string][]manifest.Container)
	var rootsRaw []manifest.Container
	seen := make(map[string]bool, len(containerIDs))

	addCID := func(cid string) {
		if seen[cid] {
			return
		}
		seen[cid] = true
		ct, ok := contByID[cid]
		if !ok {
			return
		}
		pid := viewParent[cid]
		if pid == "" || !active[pid] {
			rootsRaw = append(rootsRaw, ct)
		} else {
			childRaw[pid] = append(childRaw[pid], ct)
		}
	}

	for _, cid := range containerIDs {
		if active[cid] {
			addCID(cid)
		}
	}
	// Also capture any active container reached via placements even if omitted from container_ids.
	for cid := range active {
		addCID(cid)
	}
	return rootsRaw, childRaw
}

func buildPreparedContainerNode(
	ct manifest.Container,
	childRaw map[string][]manifest.Container,
	viewParent map[string]string,
	directCards map[string][]PreparedCard,
	maxCardsPerLane int,
	vpcCache map[string]bool,
) PreparedContainerNode {
	rawKids := childRaw[ct.ID]
	kids := make([]PreparedContainerNode, 0, len(rawKids))
	for _, ch := range rawKids {
		kids = append(kids, buildPreparedContainerNode(ch, childRaw, viewParent, directCards, maxCardsPerLane, vpcCache))
	}

	expLower := strings.ToLower(strings.TrimSpace(ct.Exposure))
	pill := strings.ReplaceAll(string(ct.Kind), "_", " ")
	if ct.Kind == manifest.ContainerExposureZone && expLower != "" {
		pill = strings.ToUpper(strings.ReplaceAll(expLower, "_", " "))
	}

	node := PreparedContainerNode{
		ID:            ct.ID,
		Title:         ct.Title,
		Subtitle:      ct.Subtitle,
		Kind:          ct.Kind,
		KindPill:      pill,
		ExposureLower: expLower,
		Depth:         structuralDepth(ct.ID, viewParent),
		BackedAnchor:  len(ct.BackedBy) > 0,
		Badges:        slices.Clone(ct.Badges),
		DirectLanes:   groupCardsIntoLanes(directCards[ct.ID], maxCardsPerLane),
		Children:      kids,
	}
	configureContainerLayoutMode(&node, ct, kids, childRaw, vpcCache)
	return node
}

func configureContainerLayoutMode(
	node *PreparedContainerNode,
	ct manifest.Container,
	kids []PreparedContainerNode,
	childRaw map[string][]manifest.Container,
	vpcCache map[string]bool,
) {
	switch ct.Kind {
	case manifest.ContainerVPC:
		configureVPCContainerLayout(node, kids)
	case manifest.ContainerRegion:
		configureRegionContainerLayout(node, kids)
	case manifest.ContainerFolder:
		configureFolderContainerLayout(node, ct.ID, kids, childRaw, vpcCache)
	case manifest.ContainerOrg:
		configureOrgContainerLayout(node, ct.ID, kids, childRaw, vpcCache)
	case manifest.ContainerProject:
		if hasVPCDescendant(ct.ID, childRaw, vpcCache) && len(kids) > 1 {
			node.StackVertical = true
		}
	}
}

func configureVPCContainerLayout(node *PreparedContainerNode, kids []PreparedContainerNode) {
	if !hasPublicChildNode(kids) {
		node.StackVertical = true
		return
	}
	node.SplitTwoColumn = true
	for _, k := range kids {
		if k.ExposureLower == "public" {
			node.PrimaryChildren = append(node.PrimaryChildren, k)
		} else {
			node.SecondaryChildren = append(node.SecondaryChildren, k)
		}
	}
}

func configureRegionContainerLayout(node *PreparedContainerNode, kids []PreparedContainerNode) {
	if len(kids) < 4 {
		return
	}
	var computeKids, zoneKids []PreparedContainerNode
	for _, k := range kids {
		if isVPCZoneContainerKind(k.Kind) {
			zoneKids = append(zoneKids, k)
		} else {
			computeKids = append(computeKids, k)
		}
	}
	if len(computeKids) >= 1 && len(zoneKids) >= 2 {
		node.SplitTwoColumn = true
		node.PrimaryChildren = computeKids
		node.SecondaryChildren = zoneKids
	}
}

func isVPCZoneContainerKind(kind manifest.ContainerKind) bool {
	return kind == manifest.ContainerExposureZone || kind == manifest.ContainerPSAZone || kind == manifest.ContainerHybridZone
}

func configureFolderContainerLayout(
	node *PreparedContainerNode,
	id string,
	kids []PreparedContainerNode,
	childRaw map[string][]manifest.Container,
	vpcCache map[string]bool,
) {
	if !hasVPCDescendant(id, childRaw, vpcCache) {
		node.StackVertical = true
		return
	}
	vpcBearing, nonVPC := splitFolderProjectChildren(kids, childRaw, vpcCache)
	if len(vpcBearing) == 1 && len(nonVPC) >= 2 {
		node.StackVertical = true
		node.SplitTopBottom = true
		node.PrimaryChildren = vpcBearing
		node.SecondaryChildren = nonVPC
		return
	}
	if len(vpcBearing) >= 1 && len(nonVPC) >= 2 {
		node.SplitSideStack = true
		node.PrimaryChildren = vpcBearing
		node.SecondaryChildren = nonVPC
		return
	}
	if countDirectVPCChildren(kids) >= 2 || len(vpcBearing) >= 2 {
		node.StackVertical = true
	}
}

func configureOrgContainerLayout(
	node *PreparedContainerNode,
	id string,
	kids []PreparedContainerNode,
	childRaw map[string][]manifest.Container,
	vpcCache map[string]bool,
) {
	if !hasVPCDescendant(id, childRaw, vpcCache) {
		return
	}
	vpcBearing, nonVPC := splitFolderProjectChildren(kids, childRaw, vpcCache)
	if len(vpcBearing) == 1 && len(nonVPC) >= 2 {
		node.StackVertical = true
		node.SplitTopBottom = true
		node.PrimaryChildren = vpcBearing
		node.SecondaryChildren = nonVPC
	}
}

func sortContainersForLayout(list []manifest.Container, children map[string][]manifest.Container, vpcCache map[string]bool) {
	slices.SortStableFunc(list, func(a, b manifest.Container) int {
		// External source zones always far left; external target zones always far right.
		aExtLeft := a.Kind == manifest.ContainerExternalZone && a.OrderHint < 0
		bExtLeft := b.Kind == manifest.ContainerExternalZone && b.OrderHint < 0
		if aExtLeft != bExtLeft {
			if aExtLeft {
				return -1
			}
			return 1
		}
		aExtRight := a.Kind == manifest.ContainerExternalZone && a.OrderHint >= 500
		bExtRight := b.Kind == manifest.ContainerExternalZone && b.OrderHint >= 500
		if aExtRight != bExtRight {
			if aExtRight {
				return 1
			}
			return -1
		}
		// Inside ORG, folders hosting VPCs come before non-VPC folders.
		if a.Kind == manifest.ContainerFolder && b.Kind == manifest.ContainerFolder {
			aHasVPC := hasVPCDescendant(a.ID, children, vpcCache)
			bHasVPC := hasVPCDescendant(b.ID, children, vpcCache)
			if aHasVPC != bHasVPC {
				if aHasVPC {
					return -1
				}
				return 1
			}
		}
		return cmp.Or(cmp.Compare(a.OrderHint, b.OrderHint), cmp.Compare(a.Title, b.Title), cmp.Compare(a.ID, b.ID))
	})
}

func hasVPCDescendant(id string, children map[string][]manifest.Container, cache map[string]bool) bool {
	if v, ok := cache[id]; ok {
		return v
	}
	for _, ch := range children[id] {
		if ch.Kind == manifest.ContainerVPC || ch.Kind == manifest.ContainerSharedHub || hasVPCDescendant(ch.ID, children, cache) {
			cache[id] = true
			return true
		}
	}
	cache[id] = false
	return false
}

func hasPublicChildNode(children []PreparedContainerNode) bool {
	for _, ch := range children {
		if ch.ExposureLower == "public" {
			return true
		}
	}
	return false
}

func countDirectVPCChildren(children []PreparedContainerNode) int {
	n := 0
	for _, ch := range children {
		if ch.Kind == manifest.ContainerVPC || ch.Kind == manifest.ContainerSharedHub {
			n++
		}
	}
	return n
}

func splitFolderProjectChildren(children []PreparedContainerNode, childRaw map[string][]manifest.Container, vpcCache map[string]bool) (vpcBearing, nonVPC []PreparedContainerNode) {
	for _, ch := range children {
		if ch.Kind == manifest.ContainerVPC || ch.Kind == manifest.ContainerSharedHub || hasVPCDescendant(ch.ID, childRaw, vpcCache) {
			vpcBearing = append(vpcBearing, ch)
		} else {
			nonVPC = append(nonVPC, ch)
		}
	}
	return vpcBearing, nonVPC
}

func groupCardsIntoLanes(cards []PreparedCard, maxCardsPerLane int) []PreparedLane {
	if len(cards) == 0 {
		return nil
	}
	sorted := slices.Clone(cards)
	slices.SortStableFunc(sorted, func(a, b PreparedCard) int {
		return cmp.Or(
			cmp.Compare(a.TopoRank, b.TopoRank),
			cmp.Compare(a.SubRank, b.SubRank),
			cmp.Compare(a.Title, b.Title),
			cmp.Compare(a.ComponentID, b.ComponentID),
		)
	})

	var lanes []PreparedLane
	for i := 0; i < len(sorted); {
		rank := sorted[i].TopoRank
		j := i + 1
		for j < len(sorted) && sorted[j].TopoRank == rank {
			j++
		}
		group := sorted[i:j]
		for start := 0; start < len(group); start += maxCardsPerLane {
			end := min(len(group), start+maxCardsPerLane)
			lanes = append(lanes, PreparedLane{
				Cards: group[start:end],
			})
		}
		i = j
	}
	return lanes
}

func resolveCard(
	viewID string,
	p manifest.Placement,
	placementIdx int,
	compByID map[string]manifest.Component,
	detailByID map[string]manifest.View,
	reg *icons.Registry,
) PreparedCard {
	if c, ok := compByID[p.ComponentID]; ok {
		return resolveComponentCard(c, p, placementIdx)
	}
	if viewID == manifest.SystemIndexViewID {
		if dv, ok := detailByID[p.ComponentID]; ok {
			return resolveDetailViewCard(dv, p, placementIdx, reg)
		}
	}
	return PreparedCard{
		ComponentID: p.ComponentID,
		Title:       p.ComponentID,
		IconKey:     "family_compute",
		RoleTag:     "compute",
		TopoRank:    p.Column,
		SubRank:     placementIdx,
		OffPageRefs: slices.Clone(p.OffPageRefs),
	}
}

func resolveComponentCard(c manifest.Component, p manifest.Placement, placementIdx int) PreparedCard {
	rank := p.Column
	if p.Column == 0 && c.TopoRank > 0 {
		rank = c.TopoRank
	}
	subRank := c.SubRank
	if subRank == 0 {
		subRank = placementIdx
	}
	members := slices.Clone(c.Members)
	rawName := ""
	if len(members) == 1 && members[0] != "" && members[0] != c.Title {
		rawName = members[0]
	}
	badges := appendProjectBadge(slices.Clone(c.Badges), c.ServiceProjectID)
	sub := strings.Join(slices.Compact(strings.Fields(c.Subtitle)), " ")
	return PreparedCard{
		ComponentID: c.ID,
		Title:       c.Title,
		Subtitle:    sub,
		IconKey:     c.IconKey,
		RoleTag:     deriveRoleTag(c),
		Wide:        isCardWide(c.Title, rawName, members, badges),
		ExtraWide:   isCardExtraWide(c.Title, rawName),
		TopoRank:    rank,
		SubRank:     subRank,
		RawName:     rawName,
		Members:     members,
		Badges:      badges,
		InlineMeta:  slices.Clone(c.InlineMeta),
		OffPageRefs: slices.Clone(p.OffPageRefs),
	}
}

func appendProjectBadge(badges []manifest.ComponentBadge, serviceProjectID string) []manifest.ComponentBadge {
	if serviceProjectID == "" {
		return badges
	}
	for _, b := range badges {
		if b.Kind == "project" && b.Label == serviceProjectID {
			return badges
		}
	}
	return append(badges, manifest.ComponentBadge{Kind: "project", Label: serviceProjectID})
}

func isCardWide(title, rawName string, members []string, badges []manifest.ComponentBadge) bool {
	if utf8.RuneCountInString(title) >= 34 || utf8.RuneCountInString(rawName) >= 34 || len(members) >= 5 || len(badges) >= 5 {
		return true
	}
	for _, m := range members {
		if utf8.RuneCountInString(m) >= 34 {
			return true
		}
	}
	return false
}

func maxTokenLen(s string) int {
	maxL := 0
	for _, tok := range strings.Fields(s) {
		if n := utf8.RuneCountInString(tok); n > maxL {
			maxL = n
		}
	}
	return maxL
}

func isCardExtraWide(title, rawName string) bool {
	return maxTokenLen(title) >= 36 || maxTokenLen(rawName) >= 36
}

func resolveDetailViewCard(dv manifest.View, p manifest.Placement, placementIdx int, reg *icons.Registry) PreparedCard {
	sub := dv.Subtitle
	if sub == "" {
		sub = fmt.Sprintf("%d components · %s", len(dv.Placements), dv.Archetype)
	}
	return PreparedCard{
		ComponentID: dv.ID,
		Title:       dv.Title,
		Subtitle:    sub,
		IconKey:     systemIndexViewIcon(dv, reg),
		RoleTag:     "compute",
		TopoRank:    p.Column,
		SubRank:     placementIdx,
		InlineMeta: []manifest.InlineMeta{
			{Label: "Archetype", Value: string(dv.Archetype)},
			{Label: "Blocks", Value: fmt.Sprintf("%d", len(dv.Placements))},
		},
		OffPageRefs: slices.Clone(p.OffPageRefs),
	}
}

func deriveRoleTag(c manifest.Component) string {
	low := strings.ToLower(c.ID + " " + c.Title)
	if strings.Contains(low, "dlq") || strings.Contains(low, "dead_letter") || strings.Contains(low, "dead-letter") {
		return "dlq"
	}
	if strings.Contains(low, "replay") || strings.Contains(low, "reprocessor") || strings.Contains(low, "remediation") {
		return "retry"
	}
	switch c.IconKey {
	case "cloud_vpn", "cloud_interconnect", "network_connectivity_center":
		return "hybrid"
	case "vertexai", "vertex_agent_engine", "vertex_search", "foundation_model", "vertex_vector_search":
		return "ai"
	case "cloud_build", "cloud_scheduler":
		return "cicd"
	case "pubsub", "eventarc", "cloud_tasks", "workflows":
		return "async"
	case "dataflow", "datastream", "cloud_composer", "dataproc", "dataplex":
		return "data"
	}
	switch c.ComponentKind {
	case manifest.KindExternalCaller:
		return "external_caller"
	case manifest.KindExternalTarget:
		return "external_target"
	case manifest.KindIngress:
		return "ingress"
	case manifest.KindMessaging:
		return "async"
	case manifest.KindDatastore:
		return "data"
	case manifest.KindPipeline:
		return "cicd"
	default:
		return "compute"
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
		if reg.Has(k) {
			return k
		}
	}
	return candidates[len(candidates)-1]
}

func buildPreparedEdges(
	edges []manifest.Edge,
	placedSet map[string]bool,
	activeContainers map[string]bool,
	compByID map[string]manifest.Component,
) ([]RoutingEdge, []PreparedFootnote) {
	routingEdges := make([]RoutingEdge, 0, len(edges))
	var footnotes []PreparedFootnote

	for _, e := range edges {
		fromOK := placedSet[e.From] || activeContainers[e.From]
		toOK := placedSet[e.To] || activeContainers[e.To]
		if !fromOK || !toOK {
			continue
		}

		re := RoutingEdge{
			From:         e.From,
			To:           e.To,
			Verb:         e.Verb,
			FlowKind:     e.FlowKind,
			SemanticTag:  deriveSemanticTag(e, compByID),
			IsRetryOrDLQ: e.IsRetryOrDLQ,
			Heuristic:    e.Provenance == manifest.ProvenanceInferred,
		}
		if e.Provenance == manifest.ProvenanceInferred && strings.TrimSpace(e.InferenceRationale) != "" {
			footnotes = append(footnotes, PreparedFootnote{
				Number:    len(footnotes) + 1,
				EdgeLabel: e.From + " -> " + e.To,
				Rationale: e.InferenceRationale,
			})
		}
		routingEdges = append(routingEdges, re)
	}
	return routingEdges, footnotes
}

func deriveSemanticTag(e manifest.Edge, compByID map[string]manifest.Component) string {
	if tag := strings.ToLower(strings.TrimSpace(e.SemanticTag)); tag != "" {
		return tag
	}
	lowTo := strings.ToLower(e.To + " " + compByID[e.To].Title + " " + e.Verb)
	if e.IsRetryOrDLQ {
		if strings.Contains(lowTo, "dlq") || strings.Contains(lowTo, "dead") {
			return "dlq"
		}
		return "retry"
	}
	if e.FlowKind == manifest.FlowCICD {
		return "cicd"
	}
	if dst, ok := compByID[e.To]; ok {
		if dst.ComponentKind == manifest.KindMessaging {
			return "async"
		}
		if dst.ComponentKind == manifest.KindDatastore {
			return "data"
		}
	}
	if src, ok := compByID[e.From]; ok {
		if src.ComponentKind == manifest.KindExternalCaller || src.ComponentKind == manifest.KindIngress {
			return "ingress"
		}
	}
	return "traffic"
}

func buildStageBanners(archetype manifest.Archetype, placements []manifest.Placement, compByID map[string]manifest.Component) []StageBanner {
	if archetype != manifest.ArchetypeStagePipeline {
		return nil
	}
	colsMap := make(map[int][]manifest.Component)
	var cols []int
	for _, p := range placements {
		if _, seen := colsMap[p.Column]; !seen {
			cols = append(cols, p.Column)
		}
		if comp, ok := compByID[p.ComponentID]; ok {
			colsMap[p.Column] = append(colsMap[p.Column], comp)
		}
	}
	slices.Sort(cols)
	banners := make([]StageBanner, 0, len(cols))
	for i, col := range cols {
		banners = append(banners, StageBanner{
			Label: stageBannerLabel(i, colsMap[col]),
		})
	}
	return banners
}

func stageBannerLabel(stageIdx int, comps []manifest.Component) string {
	counts := make(map[string]int)
	for _, c := range comps {
		switch c.IconKey {
		case "dataflow", "datastream", "cloud_composer", "dataproc", "workflows", "data_transfer":
			counts["Pipelines & ETL"]++
		case "vertexai", "vertex_agent_engine", "vertex_search", "foundation_model", "vertex_vector_search":
			counts["AI & ML"]++
		case "artifact_registry":
			counts["Artifacts & Registry"]++
		case "cloud_build", "cloud_scheduler":
			counts["Build & CI"]++
		default:
			switch c.ComponentKind {
			case manifest.KindExternalCaller:
				counts["Source"]++
			case manifest.KindIngress:
				counts["Ingress"]++
			case manifest.KindPipeline:
				counts["Pipelines & ETL"]++
			case manifest.KindWorkload:
				counts["Workloads"]++
			case manifest.KindMessaging:
				counts["Messaging"]++
			case manifest.KindDatastore:
				counts["Data & Storage"]++
			case manifest.KindCohort:
				counts["Services"]++
			case manifest.KindExternalTarget:
				counts["Deploy Targets"]++
			}
		}
	}
	bestRole := ""
	bestCount := 0
	for _, role := range []string{
		"Source", "Ingress", "Build & CI", "Pipelines & ETL",
		"Workloads", "Messaging", "AI & ML", "Artifacts & Registry",
		"Data & Storage", "Services", "Deploy Targets",
	} {
		if counts[role] > bestCount {
			bestCount = counts[role]
			bestRole = role
		}
	}
	if bestRole == "" {
		return fmt.Sprintf("Stage %d", stageIdx+1)
	}
	return fmt.Sprintf("Stage %d · %s", stageIdx+1, bestRole)
}

var transversalCategoryOrder = []struct {
	Key   string
	Title string
}{
	{Key: "IAM_SA", Title: "Identity & IAM"},
	{Key: "KMS", Title: "Encryption (Cloud KMS)"},
	{Key: "SECRET", Title: "Secrets (Secret Manager)"},
	{Key: "FIREWALL_ARMOR", Title: "Edge Security & Firewall"},
	{Key: "NAT_ROUTER", Title: "Cloud Router & NAT"},
	{Key: "VPC_ACCESS", Title: "Serverless VPC Access"},
	{Key: "OBSERVABILITY", Title: "Observability (Logging & Monitoring)"},
}

func buildTransversalGroups(
	items []manifest.TransversalItem,
	placedSet map[string]bool,
	activeContainers map[string]bool,
	isOverview bool,
) ([]PreparedTransversalGroup, int) {
	if len(items) == 0 {
		return nil, 0
	}

	byCat := make(map[string][]PreparedTransversalItem)
	total := 0
	for _, item := range items {
		if !isTransversalItemRelevant(item, placedSet, activeContainers, isOverview) {
			continue
		}
		cat := cmp.Or(item.Category, "OTHER")
		byCat[cat] = append(byCat[cat], formatTransversalItem(item))
		total++
	}
	return orderTransversalGroups(byCat), total
}

func isTransversalItemRelevant(
	item manifest.TransversalItem,
	placedSet map[string]bool,
	activeContainers map[string]bool,
	isOverview bool,
) bool {
	if isOverview || (len(item.AttachedNodeIDs) == 0 && item.OwnerContainerID == "") {
		return true
	}
	if item.OwnerContainerID != "" && activeContainers[item.OwnerContainerID] {
		return true
	}
	for _, nid := range item.AttachedNodeIDs {
		if placedSet[nid] {
			return true
		}
	}
	return false
}

func formatTransversalItem(item manifest.TransversalItem) PreparedTransversalItem {
	name := cmp.Or(item.DisplayName, item.ID)
	var metaParts []string
	if item.ProjectID != "" {
		metaParts = append(metaParts, item.ProjectID)
	}
	if item.Detail != "" {
		metaParts = append(metaParts, item.Detail)
	}
	if len(item.Roles) > 0 {
		metaParts = append(metaParts, strings.Join(item.Roles, ", "))
	}
	if len(item.AttachedNodeIDs) > 0 {
		metaParts = append(metaParts, fmt.Sprintf("attached: %d", len(item.AttachedNodeIDs)))
	}
	return PreparedTransversalItem{
		DisplayName: name,
		Meta:        strings.Join(metaParts, " · "),
	}
}

func orderTransversalGroups(byCat map[string][]PreparedTransversalItem) []PreparedTransversalGroup {
	var groups []PreparedTransversalGroup
	seenCat := make(map[string]bool)
	for _, spec := range transversalCategoryOrder {
		seenCat[spec.Key] = true
		if list := byCat[spec.Key]; len(list) > 0 {
			groups = append(groups, PreparedTransversalGroup{
				Title: spec.Title,
				Items: list,
			})
		}
	}
	var extraCats []string
	for cat := range byCat {
		if !seenCat[cat] {
			extraCats = append(extraCats, cat)
		}
	}
	slices.Sort(extraCats)
	for _, cat := range extraCats {
		groups = append(groups, PreparedTransversalGroup{
			Title: strings.ReplaceAll(cat, "_", " "),
			Items: byCat[cat],
		})
	}
	return groups
}

func formatSuppressedSummary(counts map[string]int, totalOut *int) string {
	keys := make([]string, 0, len(counts))
	total := 0
	for k, c := range counts {
		keys = append(keys, k)
		total += c
	}
	slices.Sort(keys)
	*totalOut = total
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		short := k
		if idx := strings.LastIndex(k, "/"); idx >= 0 {
			short = k[idx+1:]
		}
		parts = append(parts, fmt.Sprintf("%s: %d", short, counts[k]))
	}
	return strings.Join(parts, ", ")
}
