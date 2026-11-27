package synth

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/genai"

	"github.com/arrase/gcp-arch/internal/icons"
	"github.com/arrase/gcp-arch/internal/manifest"
)

const defaultOverviewTitle = "System Architecture Overview"

type annotationPromptPayload struct {
	Scopes                []string               `json:"scopes"`
	TargetDetailViewCount int                    `json:"target_detail_view_count"`
	TransversalCount      int                    `json:"transversal_count"`
	Nodes                 []annotationPromptNode `json:"nodes"`
	Edges                 []annotationPromptEdge `json:"edges"`
}

type annotationPromptNode struct {
	NodeID      string `json:"node_id"`
	Title       string `json:"title"`
	AssetType   string `json:"asset_type"`
	ProjectID   string `json:"project_id"`
	ContainerID string `json:"container_id"`
	TopoRank    int    `json:"topo_rank"`
	Exposure    string `json:"exposure,omitempty"`
	CohortCount int    `json:"cohort_count,omitempty"`
}

type annotationPromptEdge struct {
	EdgeID       string   `json:"edge_id"`
	FromNodeID   string   `json:"from_node_id"`
	ToNodeID     string   `json:"to_node_id"`
	Kinds        []string `json:"kinds"`
	Details      []string `json:"details,omitempty"`
	IsRetryOrDLQ bool     `json:"is_retry_or_dlq,omitempty"`
}

type annotationResponse struct {
	FocusSummary    string                 `json:"focus_summary"`
	DetailViews     []annotationDetailView `json:"detail_views"`
	NodeEnrichments []annotationNodeEnrich `json:"node_enrichments"`
	EdgeEnrichments []annotationEdgeEnrich `json:"edge_enrichments"`
}

type annotationDetailView struct {
	ViewSlot     string   `json:"view_slot"`
	Title        string   `json:"title"`
	Subtitle     string   `json:"subtitle"`
	FocusSummary string   `json:"focus_summary"`
	NodeIDs      []string `json:"node_ids"`
}

type annotationNodeEnrich struct {
	NodeID   string `json:"node_id"`
	Subtitle string `json:"subtitle"`
}

type annotationEdgeEnrich struct {
	EdgeID      string `json:"edge_id"`
	Verb        string `json:"verb"`
	FlowKind    string `json:"flow_kind"`
	SemanticTag string `json:"semantic_tag"`
}

var flowKindEnum = []string{
	string(manifest.FlowRuntime),
	string(manifest.FlowCICD),
	string(manifest.FlowAdmin),
}

var semanticTagEnum = []string{
	"ingress", "traffic", "data", "async", "retry", "dlq", "cicd", "peering", "hybrid",
}

func annotationSchema() *genai.Schema {
	viewSchema := &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"view_slot":     {Type: genai.TypeString, Enum: []string{"view_1", "view_2", "view_3"}},
			"title":         {Type: genai.TypeString, Description: "Short cohesive subsystem title"},
			"subtitle":      {Type: genai.TypeString, Description: "Concise scope summary (<=60 chars)"},
			"focus_summary": {Type: genai.TypeString, Description: "One-line architectural focus"},
			"node_ids":      {Type: genai.TypeArray, Items: &genai.Schema{Type: genai.TypeString}},
		},
		Required: []string{"view_slot", "title", "subtitle", "focus_summary", "node_ids"},
	}

	nodeEnrichSchema := &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"node_id":  {Type: genai.TypeString},
			"subtitle": {Type: genai.TypeString, Description: "Concise runtime/role label (<=44 chars)"},
		},
		Required: []string{"node_id", "subtitle"},
	}

	edgeEnrichSchema := &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"edge_id":      {Type: genai.TypeString},
			"verb":         {Type: genai.TypeString, Description: "Concise action verb <= 14 chars"},
			"flow_kind":    {Type: genai.TypeString, Enum: flowKindEnum},
			"semantic_tag": {Type: genai.TypeString, Enum: semanticTagEnum},
		},
		Required: []string{"edge_id", "verb", "flow_kind", "semantic_tag"},
	}

	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"focus_summary":    {Type: genai.TypeString, Description: "One-line end-to-end architecture summary"},
			"detail_views":     {Type: genai.TypeArray, Items: viewSchema},
			"node_enrichments": {Type: genai.TypeArray, Items: nodeEnrichSchema},
			"edge_enrichments": {Type: genai.TypeArray, Items: edgeEnrichSchema},
		},
		Required: []string{"focus_summary", "detail_views", "node_enrichments", "edge_enrichments"},
	}
}

const annotationSystemPrompt = `You are a Principal Google Cloud Platform architect annotating a pre-computed deterministic GCP architecture skeleton.
Topology, containers, node titles, icons, and view IDs are ALREADY locked in Go. NEVER invent or omit node_id or edge_id entries.

Rules:
1. focus_summary:
   - Concise one-line overview of the end-to-end architecture.
2. detail_views:
   - Emit EXACTLY target_detail_view_count entries.
   - If target_detail_view_count == 0, return an empty array [].
   - If target_detail_view_count == 2, emit slots "view_1" and "view_2" partitioning nodes deterministically by architectural stage: view_1 = upstream ingress, compute & primary runtime (topo_rank <= 2), view_2 = async messaging, pipelines, datastores, AI & hybrid/external targets (topo_rank >= 3).
   - If target_detail_view_count == 3, emit slots "view_1", "view_2", "view_3" partitioning nodes deterministically by architectural stage (every node assigned to its primary stage slot; do NOT invent ad-hoc splits):
     * view_1 = Edge Ingress, External Callers & Core Runtime Compute (topo_rank <= 2, excluding messaging/pipelines/datastores/AI/DLQ/retry)
     * view_2 = Async Messaging, Event Pipelines, CI/CD & Downstream/Retry Workers (topo_rank >= 3 or messaging/pipeline/retry/dlq/cicd, excluding datastores/AI/external targets)
     * view_3 = Persistent Datastores, Analytics, AI/ML Engines & External/Hybrid Targets (all Datastores, BigQuery, Cloud SQL, AlloyDB, Redis, Firestore, GCS, Artifact Registry, Vertex AI, Discovery Engine, and external targets)
   - Use ONLY exact node_id values from input.nodes. Keep partitioning deterministic across runs.
3. node_enrichments:
   - Provide one entry for EVERY node_id in input.nodes.
   - subtitle: concise <= 44 chars describing runtime/role/exposure (never use "...").
4. edge_enrichments:
   - Provide one entry for EVERY edge_id in input.edges.
   - verb: ultra-concise action verb <= 14 characters (e.g. "HTTPS :443", "Publishes", "Dead-letter", "Retry push", "Queries SQL", "VPC peering").
   - flow_kind: RUNTIME, CICD, or ADMIN.
   - semantic_tag: ingress, traffic, data, async, retry, dlq, cicd, peering, or hybrid ("dlq" / "retry" when is_retry_or_dlq is true).`

func targetDetailViewCount(nodeCount int) int {
	switch {
	case nodeCount <= 14:
		return 0
	case nodeCount <= 28:
		return 2
	default:
		return 3
	}
}

func overviewSubtitle(compCount, edgeCount, transCount int) string {
	return fmt.Sprintf("%d components · %d flows · %d governance items", compCount, edgeCount, transCount)
}

func annotateSkeleton(
	ctx context.Context,
	client ModelClient,
	skel *skeleton,
) (annotationResponse, UsageStats, error) {
	pNodes := make([]annotationPromptNode, len(skel.Nodes))
	for i, n := range skel.Nodes {
		pNodes[i] = annotationPromptNode{
			NodeID:      n.ID,
			Title:       n.Title,
			AssetType:   n.AssetType,
			ProjectID:   n.ProjectID,
			ContainerID: n.ContainerID,
			TopoRank:    n.TopoRank,
			Exposure:    n.Exposure,
			CohortCount: n.CohortCount,
		}
	}

	pEdges := make([]annotationPromptEdge, len(skel.Edges))
	for i, e := range skel.Edges {
		kinds := make([]string, len(e.Kinds))
		for j, k := range e.Kinds {
			kinds[j] = string(k)
		}
		details := e.Details
		if len(details) > 2 {
			details = details[:2]
		}
		pEdges[i] = annotationPromptEdge{
			EdgeID:       e.ID,
			FromNodeID:   e.FromNodeID,
			ToNodeID:     e.ToNodeID,
			Kinds:        kinds,
			Details:      details,
			IsRetryOrDLQ: e.IsRetryOrDLQ,
		}
	}

	payload := annotationPromptPayload{
		Scopes:                skel.Scopes,
		TargetDetailViewCount: targetDetailViewCount(len(skel.Nodes)),
		TransversalCount:      len(skel.TransversalItems),
		Nodes:                 pNodes,
		Edges:                 pEdges,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return annotationResponse{}, UsageStats{}, fmt.Errorf("marshal annotation prompt payload: %w", err)
	}
	userPrompt := "Annotate this deterministic GCP architecture skeleton:\n" + string(payloadBytes)

	var (
		raw   []byte
		stats UsageStats
	)
	if uc, ok := client.(UsageModelClient); ok {
		raw, stats, err = uc.GenerateJSONWithUsage(ctx, annotationSystemPrompt, userPrompt, annotationSchema())
	} else {
		start := time.Now()
		raw, err = client.GenerateJSON(ctx, annotationSystemPrompt, userPrompt, annotationSchema())
		stats = UsageStats{
			DurationMs:    time.Since(start).Milliseconds(),
			ResponseBytes: len(raw),
		}
	}
	if err != nil {
		return annotationResponse{}, stats, err
	}

	var parsed annotationResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return annotationResponse{}, stats, fmt.Errorf("decode annotation JSON: %w", err)
	}
	return parsed, stats, nil
}

func assembleManifest(
	skel *skeleton,
	llm annotationResponse,
	reg *icons.Registry,
	generatedAt string,
) *manifest.Manifest {
	components := buildManifestComponents(skel.Nodes, llm.NodeEnrichments, reg)
	edges, accent := buildManifestEdges(skel.Edges, llm.EdgeEnrichments)

	contByID := make(map[string]manifest.Container, len(skel.Containers))
	allContIDs := make([]string, len(skel.Containers))
	for i, ct := range skel.Containers {
		contByID[ct.ID] = ct
		allContIDs[i] = ct.ID
	}

	// Build deterministic View 0 covering 100% of primary components.
	orderedNodes := slices.Clone(skel.Nodes)
	slices.SortFunc(orderedNodes, func(a, b skeletonNode) int {
		return cmp.Or(
			cmp.Compare(a.TopoRank, b.TopoRank),
			cmp.Compare(a.SubRank, b.SubRank),
			cmp.Compare(a.ID, b.ID),
		)
	})
	v0Placements := make([]manifest.Placement, len(orderedNodes))
	for i, n := range orderedNodes {
		v0Placements[i] = manifest.Placement{
			ComponentID: n.ID,
			ContainerID: n.ContainerID,
			Column:      n.TopoRank,
		}
	}

	focusSummary := cleanAnnotation(llm.FocusSummary)
	if focusSummary == "" {
		focusSummary = fmt.Sprintf("End-to-end architecture across %d components and %d functional flows", len(components), len(edges))
	}

	views := []manifest.View{{
		ID:                      manifest.SystemIndexViewID,
		Title:                   defaultOverviewTitle,
		Subtitle:                overviewSubtitle(len(components), len(edges), len(skel.TransversalItems)),
		FocusSummary:            focusSummary,
		IncludeTransversalPanel: true,
		Archetype:               manifest.ArchetypeLayeredFlow,
		AccentMode:              accent,
		ContainerIDs:            allContIDs,
		Placements:              v0Placements,
		Edges:                   edges,
	}}

	// Build deterministic-ID detail views when > 14 primary nodes.
	if targetViews := targetDetailViewCount(len(orderedNodes)); targetViews > 0 {
		views = append(views, buildDetailViews(targetViews, orderedNodes, llm.DetailViews, contByID, edges, accent)...)
	}

	return &manifest.Manifest{
		SchemaVersion:            manifest.SchemaVersion,
		Metadata:                 manifest.Metadata{GeneratedAt: generatedAt, Scopes: slices.Clone(skel.Scopes)},
		Components:               components,
		Containers:               slices.Clone(skel.Containers),
		TransversalItems:         slices.Clone(skel.TransversalItems),
		StructuralEdges:          slices.Clone(skel.StructuralEdges),
		SuppressedPlumbingCounts: maps.Clone(skel.SuppressedPlumbingCounts),
		Views:                    views,
	}
}

func buildManifestComponents(
	nodes []skeletonNode,
	enrichments []annotationNodeEnrich,
	reg *icons.Registry,
) []manifest.Component {
	nodeEnrichByID := make(map[string]annotationNodeEnrich, len(enrichments))
	for _, ne := range enrichments {
		nodeEnrichByID[ne.NodeID] = ne
	}

	components := make([]manifest.Component, len(nodes))
	for i, cn := range nodes {
		subtitle := cn.Subtitle
		if ne, ok := nodeEnrichByID[cn.ID]; ok {
			if s := cleanAnnotation(ne.Subtitle); s != "" {
				subtitle = s
			}
		}
		iconKey := cn.IconKey
		if reg != nil && !reg.Has(iconKey) {
			iconKey = "family_compute"
		}
		components[i] = manifest.Component{
			ID:               cn.ID,
			Title:            cn.Title,
			Subtitle:         subtitle,
			IconKey:          iconKey,
			ComponentKind:    cn.ComponentKind,
			ProjectID:        cn.ProjectID,
			ServiceProjectID: cn.ServiceProjectID,
			TopoRank:         cn.TopoRank,
			SubRank:          cn.SubRank,
			BackedBy:         slices.Clone(cn.BackedBy),
			Members:          slices.Clone(cn.Members),
			Badges:           slices.Clone(cn.Badges),
			FoldedEdgeIDs:    slices.Clone(cn.FoldedEdgeIDs),
			InlineMeta:       slices.Clone(cn.InlineMeta),
		}
	}
	return components
}

func buildManifestEdges(
	skelEdges []skeletonEdge,
	enrichments []annotationEdgeEnrich,
) ([]manifest.Edge, manifest.AccentMode) {
	edgeEnrichByID := make(map[string]annotationEdgeEnrich, len(enrichments))
	for _, ee := range enrichments {
		edgeEnrichByID[ee.EdgeID] = ee
	}
	validSemanticTag := make(map[string]bool, len(semanticTagEnum))
	for _, s := range semanticTagEnum {
		validSemanticTag[s] = true
	}

	edges := make([]manifest.Edge, len(skelEdges))
	var hasCICD, hasRuntime bool
	for i, ce := range skelEdges {
		me := buildManifestEdge(ce, edgeEnrichByID, validSemanticTag)
		if me.FlowKind == manifest.FlowCICD {
			hasCICD = true
		} else {
			hasRuntime = true
		}
		edges[i] = me
	}

	accent := manifest.AccentRuntime
	if hasCICD && hasRuntime {
		accent = manifest.AccentMixed
	} else if hasCICD {
		accent = manifest.AccentCICD
	}
	return edges, accent
}

func buildManifestEdge(
	ce skeletonEdge,
	edgeEnrichByID map[string]annotationEdgeEnrich,
	validSemanticTag map[string]bool,
) manifest.Edge {
	verb, flowKind, semTag := ce.Verb, ce.FlowKind, ce.SemanticTag
	if ee, ok := edgeEnrichByID[ce.ID]; ok {
		if v := clampVerb(ee.Verb, 14); v != "" {
			verb = v
		}
		switch manifest.FlowKind(ee.FlowKind) {
		case manifest.FlowRuntime, manifest.FlowCICD, manifest.FlowAdmin:
			flowKind = manifest.FlowKind(ee.FlowKind)
		}
		if validSemanticTag[ee.SemanticTag] {
			semTag = ee.SemanticTag
		}
	}
	if ce.IsRetryOrDLQ && semTag != "dlq" && semTag != "retry" {
		semTag = ce.SemanticTag
	}
	if ce.Provenance == manifest.ProvenanceVerified && allKindsPermissionOnly(ce.Kinds) {
		flowKind = manifest.FlowAdmin
	}
	return manifest.Edge{
		From:               ce.FromNodeID,
		To:                 ce.ToNodeID,
		Verb:               clampVerb(verb, 14),
		FlowKind:           flowKind,
		SemanticTag:        semTag,
		IsRetryOrDLQ:       ce.IsRetryOrDLQ,
		Provenance:         ce.Provenance,
		EvidenceEdgeIDs:    slices.Clone(ce.EvidenceEdgeIDs),
		InferenceRationale: ce.InferenceRationale,
	}
}

func buildDetailViews(
	targetViews int,
	orderedNodes []skeletonNode,
	llmViews []annotationDetailView,
	contByID map[string]manifest.Container,
	allEdges []manifest.Edge,
	accent manifest.AccentMode,
) []manifest.View {
	nodeByID := make(map[string]skeletonNode, len(orderedNodes))
	for _, n := range orderedNodes {
		nodeByID[n.ID] = n
	}

	llmBySlot := make(map[string]annotationDetailView, len(llmViews))
	for i, lv := range llmViews {
		slot := lv.ViewSlot
		if slot == "" && i < targetViews {
			slot = fmt.Sprintf("view_%d", i+1)
		}
		if _, exists := llmBySlot[slot]; !exists {
			llmBySlot[slot] = lv
		}
	}

	pickedByView := partitionDetailViewNodes(targetViews, orderedNodes, nodeByID, llmBySlot)

	out := make([]manifest.View, 0, targetViews)
	for i := range targetViews {
		slotID := fmt.Sprintf("view_%d", i+1)
		lv := llmBySlot[slotID]
		picked := pickedByView[i]

		slices.SortFunc(picked, func(a, b string) int {
			na, nb := nodeByID[a], nodeByID[b]
			return cmp.Or(
				cmp.Compare(na.TopoRank, nb.TopoRank),
				cmp.Compare(na.SubRank, nb.SubRank),
				cmp.Compare(na.ID, nb.ID),
			)
		})

		contIDs, placements := buildDetailPlacements(picked, nodeByID, contByID)
		viewEdges := collectDetailViewEdges(picked, allEdges)

		title := fmt.Sprintf("Subsystem View %d", i+1)
		if t := cleanAnnotation(lv.Title); t != "" {
			title = t
		}
		out = append(out, manifest.View{
			ID:                      slotID,
			Title:                   title,
			Subtitle:                cleanAnnotation(lv.Subtitle),
			FocusSummary:            cleanAnnotation(lv.FocusSummary),
			IncludeTransversalPanel: true,
			Archetype:               manifest.ArchetypeLayeredFlow,
			AccentMode:              accent,
			ContainerIDs:            contIDs,
			Placements:              placements,
			Edges:                   viewEdges,
		})
	}

	populateOffPageRefs(out, allEdges, nodeByID)
	return out
}

func partitionDetailViewNodes(
	targetViews int,
	orderedNodes []skeletonNode,
	nodeByID map[string]skeletonNode,
	llmBySlot map[string]annotationDetailView,
) [][]string {
	fallbackSlices := make([][]string, targetViews)
	for i, n := range orderedNodes {
		bucket := (i * targetViews) / len(orderedNodes)
		if bucket >= targetViews {
			bucket = targetViews - 1
		}
		fallbackSlices[bucket] = append(fallbackSlices[bucket], n.ID)
	}

	pickedByView := make([][]string, targetViews)
	assigned := make(map[string]bool, len(orderedNodes))
	for i := range targetViews {
		slotID := fmt.Sprintf("view_%d", i+1)
		picked := filterValidNodeIDs(llmBySlot[slotID].NodeIDs, nodeByID)
		if len(picked) < 2 {
			picked = slices.Clone(fallbackSlices[i])
		}
		pickedByView[i] = picked
		for _, nid := range picked {
			assigned[nid] = true
		}
	}

	// Guarantee 100% component coverage across view_1..view_N when targetViews > 0.
	for _, n := range orderedNodes {
		if assigned[n.ID] {
			continue
		}
		slotIdx := stageDetailViewBucket(n, targetViews)
		pickedByView[slotIdx] = append(pickedByView[slotIdx], n.ID)
		assigned[n.ID] = true
	}
	return pickedByView
}

func filterValidNodeIDs(nodeIDs []string, nodeByID map[string]skeletonNode) []string {
	seen := make(map[string]bool, len(nodeIDs))
	var picked []string
	for _, nid := range nodeIDs {
		if _, ok := nodeByID[nid]; ok && !seen[nid] {
			seen[nid] = true
			picked = append(picked, nid)
		}
	}
	return picked
}

func buildDetailPlacements(
	picked []string,
	nodeByID map[string]skeletonNode,
	contByID map[string]manifest.Container,
) ([]string, []manifest.Placement) {
	contSet := make(map[string]bool)
	placements := make([]manifest.Placement, len(picked))
	for idx, nid := range picked {
		n := nodeByID[nid]
		placements[idx] = manifest.Placement{
			ComponentID: n.ID,
			ContainerID: n.ContainerID,
			Column:      n.TopoRank,
		}
		cur := n.ContainerID
		for cur != "" && !contSet[cur] {
			contSet[cur] = true
			if ct, ok := contByID[cur]; ok {
				cur = ct.ParentContainerID
			} else {
				break
			}
		}
	}
	contIDs := slices.Sorted(maps.Keys(contSet))
	return contIDs, placements
}

func collectDetailViewEdges(picked []string, allEdges []manifest.Edge) []manifest.Edge {
	inView := make(map[string]bool, len(picked))
	for _, nid := range picked {
		inView[nid] = true
	}
	var viewEdges []manifest.Edge
	for _, e := range allEdges {
		if e.From != e.To && inView[e.From] && inView[e.To] {
			viewEdges = append(viewEdges, e)
		}
	}
	return viewEdges
}

type offPageRefKey struct {
	targetViewID string
	direction    string
}

func populateOffPageRefs(views []manifest.View, allEdges []manifest.Edge, nodeByID map[string]skeletonNode) {
	viewIDsByNode := indexViewIDsByNode(views)
	outEdges := make(map[string][]manifest.Edge)
	inEdges := make(map[string][]manifest.Edge)
	for _, e := range allEdges {
		outEdges[e.From] = append(outEdges[e.From], e)
		inEdges[e.To] = append(inEdges[e.To], e)
	}

	for vi := range views {
		v := &views[vi]
		inView := make(map[string]bool, len(v.Placements))
		for _, p := range v.Placements {
			inView[p.ComponentID] = true
		}
		for pi := range v.Placements {
			nid := v.Placements[pi].ComponentID
			v.Placements[pi].OffPageRefs = buildPlacementOffPageRefs(
				v.ID, inView, outEdges[nid], inEdges[nid], nodeByID, viewIDsByNode,
			)
		}
	}
}

func indexViewIDsByNode(views []manifest.View) map[string][]string {
	viewIDsByNode := make(map[string][]string)
	for _, v := range views {
		for _, p := range v.Placements {
			viewIDsByNode[p.ComponentID] = append(viewIDsByNode[p.ComponentID], v.ID)
		}
	}
	for nid := range viewIDsByNode {
		slices.Sort(viewIDsByNode[nid])
		viewIDsByNode[nid] = slices.Compact(viewIDsByNode[nid])
	}
	return viewIDsByNode
}

func buildPlacementOffPageRefs(
	viewID string,
	inView map[string]bool,
	outList, inList []manifest.Edge,
	nodeByID map[string]skeletonNode,
	viewIDsByNode map[string][]string,
) []manifest.OffPageRef {
	peersByKey := make(map[offPageRefKey][]string)
	addPeer := func(other, dir string) {
		if inView[other] {
			return
		}
		otherTitle := other
		if cn, exists := nodeByID[other]; exists && cn.Title != "" {
			otherTitle = cn.Title
		}
		for _, targetVID := range viewIDsByNode[other] {
			if targetVID == viewID {
				continue
			}
			k := offPageRefKey{targetViewID: targetVID, direction: dir}
			if !slices.Contains(peersByKey[k], otherTitle) {
				peersByKey[k] = append(peersByKey[k], otherTitle)
			}
		}
	}
	for _, e := range outList {
		addPeer(e.To, "OUT")
	}
	for _, e := range inList {
		addPeer(e.From, "IN")
	}
	return formatOffPageRefs(peersByKey)
}

func formatOffPageRefs(peersByKey map[offPageRefKey][]string) []manifest.OffPageRef {
	if len(peersByKey) == 0 {
		return nil
	}
	refs := make([]manifest.OffPageRef, 0, len(peersByKey))
	for k, peers := range peersByKey {
		slices.Sort(peers)
		label := fmt.Sprintf("%s (%s)", peers[0], k.targetViewID)
		if len(peers) > 1 {
			label = fmt.Sprintf("%s (+%d in %s)", peers[0], len(peers)-1, k.targetViewID)
		}
		refs = append(refs, manifest.OffPageRef{
			TargetViewID: k.targetViewID,
			Direction:    k.direction,
			Label:        label,
		})
	}
	slices.SortFunc(refs, func(a, b manifest.OffPageRef) int {
		return cmp.Or(
			cmp.Compare(a.Direction, b.Direction),
			cmp.Compare(a.TargetViewID, b.TargetViewID),
			cmp.Compare(a.Label, b.Label),
		)
	})
	return refs
}

func stageDetailViewBucket(n skeletonNode, targetViews int) int {
	if targetViews <= 1 {
		return 0
	}
	if targetViews == 2 {
		if n.TopoRank <= 2 && n.ComponentKind != manifest.KindDatastore && n.ComponentKind != manifest.KindExternalTarget {
			return 0
		}
		return 1
	}
	if targetViews == 3 {
		if n.ComponentKind == manifest.KindDatastore || n.ComponentKind == manifest.KindExternalTarget || n.RoleTag == "ai" {
			return 2
		}
		if n.TopoRank >= 3 || n.ComponentKind == manifest.KindMessaging || n.ComponentKind == manifest.KindPipeline || n.RoleTag == "retry" || n.RoleTag == "dlq" || n.RoleTag == "cicd" {
			return 1
		}
		return 0
	}
	bucket := (n.TopoRank * targetViews) / 7
	if bucket >= targetViews {
		return targetViews - 1
	}
	if bucket < 0 {
		return 0
	}
	return bucket
}

func cleanAnnotation(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.Contains(s, "...") || strings.ContainsRune(s, '\u2026') {
		return ""
	}
	fields := strings.Fields(s)
	out := fields[:0]
	for _, f := range fields {
		if len(out) > 0 && out[len(out)-1] == f && isParentheticalCount(f) {
			continue
		}
		out = append(out, f)
	}
	return stripTrailingStrayPunct(strings.Join(out, " "))
}

func isParentheticalCount(tok string) bool {
	if len(tok) < 4 || tok[0] != '(' || tok[len(tok)-1] != ')' {
		return false
	}
	if tok[len(tok)-2] != 'x' && tok[len(tok)-2] != 'X' {
		return false
	}
	for i := 1; i < len(tok)-2; i++ {
		if tok[i] < '0' || tok[i] > '9' {
			return false
		}
	}
	return true
}

func stripTrailingStrayPunct(s string) string {
	for {
		prev := s
		s = strings.TrimSpace(s)
		s = strings.TrimRight(s, ",;")
		s = strings.TrimSpace(s)
		for _, q := range []string{"'", `"`} {
			if strings.HasSuffix(s, q) && strings.Count(s, q)%2 == 1 {
				s = strings.TrimSuffix(s, q)
			}
		}
		if s == prev {
			break
		}
	}
	return strings.TrimSpace(s)
}

func clampVerb(s string, maxRunes int) string {
	s = strings.ReplaceAll(s, "...", "")
	s = strings.ReplaceAll(s, "\u2026", "")
	s = stripTrailingStrayPunct(s)
	if s == "" {
		return "Calls"
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	cut := string(runes[:maxRunes])
	if sp := strings.LastIndex(cut, " "); sp >= 4 {
		cut = cut[:sp]
	}
	cut = stripTrailingStrayPunct(cut)
	if cut == "" {
		cut = stripTrailingStrayPunct(string(runes[:maxRunes]))
	}
	if cut == "" {
		return "Calls"
	}
	return cut
}
