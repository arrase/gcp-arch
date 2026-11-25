package synth

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/manifest"
)

// Pass1SystemPrompt builds the closed-world Pass 1 system instruction including valid icon_key slugs.
func Pass1SystemPrompt(validIconKeys []string) string {
	keys := slices.Clone(validIconKeys)
	slices.Sort(keys)
	return strings.TrimSpace(fmt.Sprintf(`
You are a Principal GCP Cloud Architect decomposing verified Cloud Asset Inventory facts into focused, readable architecture diagrams.
Operate strictly in a closed world: only reference interned resource handles (r_XXXX) present in the input JSON. Never invent resources or IDs.

Mandatory rules:
1. 100%% Resource Accounting: Every r_XXXX in input.resources MUST appear in at least one of components[].backed_by_ids, container_defs[].backed_by_ids, or excluded_resources[].resource_id.
2. 1:1 Folding: Fold dedicated 1:1 ServiceAccount, Secret, CryptoKey, Ingress, k8s.io/Service, and ForwardingRule resources into their owning workload/ingress component's backed_by_ids and surface concise details in inline_meta (max 4 rows).
3. Stateful Datastore Homogeneity: Never mix distinct stateful datastore asset types (Cloud SQL, AlloyDB, Spanner, Bigtable, Firestore, Memorystore, Cloud Storage, BigQuery Dataset, Artifact Registry, Filestore, Pub/Sub Topic) inside one component's backed_by_ids.
4. Focused Detail Views: Partition into 1..M cohesive detail views targeting 6-12 components each (hard cap 12). Never emit view_0_system_index (Go generates View 0 deterministically). Every component must appear in at least one detail view.
5. Containers: Define flat background containers (PROJECT, VPC, GKE_CLUSTER, PSA_ZONE, EXTERNAL_ZONE, SHARED_HUB) with parent nesting depth <= 2. Account for VPC/GKE Cluster r_XXXX handles via container_defs[].backed_by_ids or dedicated hub components.
6. Archetypes: Choose stage_pipeline for CI/CD/ETL flows, hub_spoke for VPC/PSA networking topologies, or landscape_matrix for application/ingress landscapes.
7. Valid icon_key values (choose strictly from this list): %s
`, strings.Join(keys, ", ")))
}

// Pass2SystemPrompt builds the closed-world Pass 2 per-view population instruction.
func Pass2SystemPrompt() string {
	return strings.TrimSpace(`
You are populating layout placements and provenance-checked edges for one locked GCP architecture view.
Operate strictly in a closed world using only the locked component_ids, container_ids, and interned edge handles (e_XXXX) supplied in the input JSON.

Mandatory rules:
1. Placements: Place every locked component_id once (1-12 placements). Assign column within 0..max_column (0..4 for landscape_matrix/stage_pipeline; 0..2 for hub_spoke). Order placements in preferred vertical top-to-bottom order per column.
2. Containers: Only include container_ids whose nesting depth within this view is <= 2.
3. Off-Page Refs: When a placed component connects to components in another detail view listed in cross_view_neighbors, attach concise off_page_refs (direction IN or OUT).
4. Edge Verbs: Keep verb concise and <= 16 characters.
5. Directed-Walk Provenance: Set provenance to VERIFIED ONLY when evidence_edge_ids (e_XXXX handles) form a connected directed walk starting at a resource in from.backed_by_ids and ending at a resource in to.backed_by_ids. Otherwise set provenance to INFERRED with a concise inference_rationale.
6. Shared Bus Spines: When >= 2 edges share the same source (or same target) and flow_kind, you may assign a shared via_bus_id; never mix multiple distinct sources AND multiple distinct targets on one via_bus_id.
`)
}

// ReconcileSystemPrompt builds the closed-world Day-2 --reconcile delta patching instruction.
func ReconcileSystemPrompt(validIconKeys []string) string {
	keys := slices.Clone(validIconKeys)
	slices.Sort(keys)
	return strings.TrimSpace(fmt.Sprintf(`
You are incrementally reconciling new GCP resources and inter-component FactEdges into an existing human-curated architecture.yaml.
Operate strictly in a closed world using only the r_XXXX and e_XXXX handles in the input JSON.

Mandatory rules:
1. Freeze Existing Curation: Never alter existing view IDs, archetypes, curated component titles, notes, or rejected_edges.
2. Cover Every Unclaimed Resource: Every r_XXXX in unclaimed_resources MUST be accounted for via append_backed_by, append_container_backed_by, new_components, or new_excluded_resources.
3. Respect View Capacity: Only add new_placements to existing views with remaining_capacity > 0 (never exceed 12 total placements). If existing views lack capacity, create overflow new_views (1-12 placements, never view_0_system_index).
4. Respect Rejected Edges: Never emit an edge whose (from, to) pair appears in rejected_edges.
5. Directed-Walk Provenance & Homogeneity: Enforce <= 16-char verbs, single-source/single-target via_bus_id, stateful datastore homogeneity, and VERIFIED directed walks over e_XXXX handles.
6. Valid icon_key values: %s
`, strings.Join(keys, ", ")))
}

type internedResourceDesc struct {
	ID                  string         `json:"id"`
	AssetType           string         `json:"asset_type"`
	ProjectID           string         `json:"project_id"`
	Location            string         `json:"location,omitempty"`
	DisplayName         string         `json:"display_name"`
	ParentID            string         `json:"parent_id,omitempty"`
	IsExternal          bool           `json:"is_external,omitempty"`
	Attributes          map[string]any `json:"attributes,omitempty"`
	ProjectCapabilities []string       `json:"project_capabilities,omitempty"`
}

type internedEdgeDesc struct {
	ID     string               `json:"id"`
	FromID string               `json:"from_id"`
	ToID   string               `json:"to_id"`
	Kind   extract.FactEdgeKind `json:"kind"`
	Detail string               `json:"detail,omitempty"`
}

func makeResourceDesc(id string, r extract.FactResource, table *InternTable) internedResourceDesc {
	parentID := ""
	if r.ParentURI != "" {
		if pid, ok := table.ResourceID(r.ParentURI); ok {
			parentID = pid
		}
	}
	return internedResourceDesc{
		ID:                  id,
		AssetType:           r.AssetType,
		ProjectID:           r.ProjectID,
		Location:            r.Location,
		DisplayName:         r.DisplayName,
		ParentID:            parentID,
		IsExternal:          r.IsExternal,
		Attributes:          r.Attributes,
		ProjectCapabilities: r.ProjectCapabilities,
	}
}

// BuildPass1UserPrompt builds the deterministic JSON prompt for Pass 1 planning.
func BuildPass1UserPrompt(facts *extract.FactsFile, table *InternTable, feedback []string) (string, error) {
	resIDs := table.ResourceIDs()
	descs := make([]internedResourceDesc, 0, len(resIDs))
	for _, id := range resIDs {
		r, _ := table.ResourceByID(id)
		descs = append(descs, makeResourceDesc(id, r, table))
	}

	payload := struct {
		Scopes             []string                    `json:"scopes"`
		Hierarchy          extract.Hierarchy           `json:"hierarchy"`
		Resources          []internedResourceDesc      `json:"resources"`
		Adjacency          map[string][]AdjacencyEntry `json:"adjacency"`
		ValidationFeedback []string                    `json:"validation_feedback,omitempty"`
	}{
		Scopes:             facts.Scopes,
		Hierarchy:          facts.Hierarchy,
		Resources:          descs,
		Adjacency:          BuildAdjacency(facts, table),
		ValidationFeedback: feedback,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal pass 1 prompt: %w", err)
	}
	return string(data), nil
}

type crossNeighbor struct {
	LocalComponentID  string   `json:"local_component_id"`
	RemoteComponentID string   `json:"remote_component_id"`
	RemoteViewIDs     []string `json:"remote_view_ids"`
	Direction         string   `json:"direction"`
}

// BuildPass2UserPrompt builds the deterministic JSON prompt for one Pass 2 detail view.
func BuildPass2UserPrompt(view Pass1DetailView, plan *Pass1Plan, table *InternTable, feedback []string) (string, error) {
	maxCol := 4
	if view.Archetype == manifest.ArchetypeHubSpoke {
		maxCol = 2
	}

	compSet := make(map[string]bool, len(view.ComponentIDs))
	for _, cid := range view.ComponentIDs {
		compSet[cid] = true
	}

	compViews := buildOtherViewMemberships(view.ID, plan.DetailViews)
	viewComps, viewResSet, resToComp := collectPass2ViewComponents(plan.Components, compSet)
	bridgeSet := buildPass1BridgeSet(plan)
	collector := &pass2Collector{
		table:        table,
		compSet:      compSet,
		compViews:    compViews,
		viewResSet:   viewResSet,
		resToComp:    resToComp,
		bridgeSet:    bridgeSet,
		neighborSeen: make(map[string]bool),
		neededRes:    maps.Clone(viewResSet),
	}
	collector.collect(view.ComponentIDs)

	resDescs := buildSortedResourceDescs(collector.neededRes, table)
	otherViewIDs := collectOtherViewIDs(view.ID, plan.DetailViews)

	payload := struct {
		View               Pass1DetailView        `json:"view"`
		MaxColumn          int                    `json:"max_column"`
		Components         []Pass1Component       `json:"components"`
		ContainerDefs      []Pass1ContainerDef    `json:"container_defs"`
		Resources          []internedResourceDesc `json:"resources"`
		IncidentEdges      []internedEdgeDesc     `json:"incident_edges"`
		OtherViewIDs       []string               `json:"other_view_ids,omitempty"`
		CrossViewNeighbors []crossNeighbor        `json:"cross_view_neighbors,omitempty"`
		ValidationFeedback []string               `json:"validation_feedback,omitempty"`
	}{
		View:               view,
		MaxColumn:          maxCol,
		Components:         viewComps,
		ContainerDefs:      plan.ContainerDefs,
		Resources:          resDescs,
		IncidentEdges:      collector.incidentEdges,
		OtherViewIDs:       otherViewIDs,
		CrossViewNeighbors: collector.crossNeighbors,
		ValidationFeedback: feedback,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal pass 2 prompt for view %q: %w", view.ID, err)
	}
	return string(data), nil
}

func buildOtherViewMemberships(currentViewID string, detailViews []Pass1DetailView) map[string][]string {
	compViews := make(map[string][]string)
	for _, dv := range detailViews {
		if dv.ID == currentViewID {
			continue
		}
		for _, cid := range dv.ComponentIDs {
			compViews[cid] = append(compViews[cid], dv.ID)
		}
	}
	for cid := range compViews {
		slices.Sort(compViews[cid])
	}
	return compViews
}

func collectPass2ViewComponents(components []Pass1Component, compSet map[string]bool) ([]Pass1Component, map[string]bool, map[string][]string) {
	viewComps := make([]Pass1Component, 0, len(compSet))
	viewResSet := make(map[string]bool)
	resToComp := make(map[string][]string)
	for _, c := range components {
		for _, rid := range c.BackedByIDs {
			resToComp[rid] = append(resToComp[rid], c.ID)
		}
		if compSet[c.ID] {
			viewComps = append(viewComps, c)
			for _, rid := range c.BackedByIDs {
				viewResSet[rid] = true
			}
		}
	}
	slices.SortFunc(viewComps, func(a, b Pass1Component) int { return cmp.Compare(a.ID, b.ID) })
	return viewComps, viewResSet, resToComp
}

func buildPass1BridgeSet(plan *Pass1Plan) map[string]bool {
	bridgeSet := make(map[string]bool, len(plan.ExcludedResources)+len(plan.ContainerDefs))
	for _, ex := range plan.ExcludedResources {
		bridgeSet[ex.ResourceID] = true
	}
	for _, ct := range plan.ContainerDefs {
		for _, rid := range ct.BackedByIDs {
			bridgeSet[rid] = true
		}
	}
	return bridgeSet
}

type pass2Collector struct {
	table          *InternTable
	compSet        map[string]bool
	compViews      map[string][]string
	viewResSet     map[string]bool
	resToComp      map[string][]string
	bridgeSet      map[string]bool
	neighborSeen   map[string]bool
	incidentEdges  []internedEdgeDesc
	crossNeighbors []crossNeighbor
	neededRes      map[string]bool
}

func (c *pass2Collector) collect(componentIDs []string) {
	for _, eid := range c.table.EdgeIDs() {
		c.inspectEdge(eid)
	}
	c.addSharedViewNeighbors(componentIDs)
	slices.SortFunc(c.crossNeighbors, func(a, b crossNeighbor) int {
		return cmp.Or(
			cmp.Compare(a.LocalComponentID, b.LocalComponentID),
			cmp.Compare(a.Direction, b.Direction),
			cmp.Compare(a.RemoteComponentID, b.RemoteComponentID),
		)
	})
}

func (c *pass2Collector) inspectEdge(eid string) {
	fe, _ := c.table.EdgeByID(eid)
	fromRID, okF := c.table.ResourceID(fe.FromURI)
	toRID, okT := c.table.ResourceID(fe.ToURI)
	if !okF || !okT {
		return
	}
	inViewFrom := c.viewResSet[fromRID]
	inViewTo := c.viewResSet[toRID]
	if isIncidentEdge(inViewFrom, inViewTo, c.bridgeSet[fromRID], c.bridgeSet[toRID]) {
		c.incidentEdges = append(c.incidentEdges, internedEdgeDesc{
			ID:     eid,
			FromID: fromRID,
			ToID:   toRID,
			Kind:   fe.Kind,
			Detail: fe.Detail,
		})
		c.neededRes[fromRID] = true
		c.neededRes[toRID] = true
	}
	if inViewFrom && !inViewTo {
		c.addCrossNeighbors(c.resToComp[fromRID], c.resToComp[toRID], "OUT")
	} else if inViewTo && !inViewFrom {
		c.addCrossNeighbors(c.resToComp[toRID], c.resToComp[fromRID], "IN")
	}
}

func isIncidentEdge(inViewFrom, inViewTo, bridgeFrom, bridgeTo bool) bool {
	if inViewFrom {
		return inViewTo || bridgeTo
	}
	return inViewTo && bridgeFrom
}

func (c *pass2Collector) addSharedViewNeighbors(componentIDs []string) {
	for _, cid := range componentIDs {
		if len(c.compViews[cid]) == 0 {
			continue
		}
		key := cid + "|SHARED|" + cid
		if c.neighborSeen[key] {
			continue
		}
		c.neighborSeen[key] = true
		c.crossNeighbors = append(c.crossNeighbors, crossNeighbor{
			LocalComponentID:  cid,
			RemoteComponentID: cid,
			RemoteViewIDs:     c.compViews[cid],
			Direction:         "OUT",
		})
	}
}

func (c *pass2Collector) addCrossNeighbors(localComps, remoteComps []string, dir string) {
	for _, localCID := range localComps {
		if !c.compSet[localCID] {
			continue
		}
		c.addCrossNeighborsForLocal(localCID, remoteComps, dir)
	}
}

func (c *pass2Collector) addCrossNeighborsForLocal(localCID string, remoteComps []string, dir string) {
	for _, remoteCID := range remoteComps {
		if c.compSet[remoteCID] || len(c.compViews[remoteCID]) == 0 {
			continue
		}
		key := localCID + "|" + dir + "|" + remoteCID
		if c.neighborSeen[key] {
			continue
		}
		c.neighborSeen[key] = true
		c.crossNeighbors = append(c.crossNeighbors, crossNeighbor{
			LocalComponentID:  localCID,
			RemoteComponentID: remoteCID,
			RemoteViewIDs:     c.compViews[remoteCID],
			Direction:         dir,
		})
	}
}

func buildSortedResourceDescs(neededRes map[string]bool, table *InternTable) []internedResourceDesc {
	neededIDs := make([]string, 0, len(neededRes))
	for rid := range neededRes {
		neededIDs = append(neededIDs, rid)
	}
	slices.Sort(neededIDs)
	resDescs := make([]internedResourceDesc, 0, len(neededIDs))
	for _, rid := range neededIDs {
		r, _ := table.ResourceByID(rid)
		resDescs = append(resDescs, makeResourceDesc(rid, r, table))
	}
	return resDescs
}

func collectOtherViewIDs(currentViewID string, detailViews []Pass1DetailView) []string {
	otherViewIDs := make([]string, 0, len(detailViews))
	for _, dv := range detailViews {
		if dv.ID != currentViewID {
			otherViewIDs = append(otherViewIDs, dv.ID)
		}
	}
	slices.Sort(otherViewIDs)
	return otherViewIDs
}

// ReconcileDelta summarizes the unclaimed resources and newly projected inter-component edges
// passed to Vertex AI during Day-2 --reconcile.
type ReconcileDelta struct {
	UnclaimedResourceIDs []string
	NewEdgeIDs           []string
}

type reconcileCompHeader struct {
	ID            string                 `json:"id"`
	Title         string                 `json:"title"`
	ComponentKind manifest.ComponentKind `json:"component_kind"`
	ProjectID     string                 `json:"project_id"`
	BackedByIDs   []string               `json:"backed_by_ids"`
}

type reconcileContHeader struct {
	ID                string                 `json:"id"`
	Title             string                 `json:"title"`
	Kind              manifest.ContainerKind `json:"kind"`
	ParentContainerID string                 `json:"parent_container_id,omitempty"`
}

type reconcileViewCapacity struct {
	ID                string             `json:"id"`
	Title             string             `json:"title"`
	Archetype         manifest.Archetype `json:"archetype"`
	MaxColumn         int                `json:"max_column"`
	RemainingCapacity int                `json:"remaining_capacity"`
	ContainerIDs      []string           `json:"container_ids,omitempty"`
	PlacedComponents  []string           `json:"placed_components"`
}

// BuildReconcileUserPrompt builds the deterministic JSON prompt for Day-2 reconciliation.
func BuildReconcileUserPrompt(existing *manifest.Manifest, delta ReconcileDelta, table *InternTable, feedback []string) (string, error) {
	payload := struct {
		FrozenComponents   []reconcileCompHeader   `json:"frozen_components"`
		FrozenContainers   []reconcileContHeader   `json:"frozen_containers"`
		ExistingViews      []reconcileViewCapacity `json:"existing_views"`
		RejectedEdges      []manifest.RejectedEdge `json:"rejected_edges,omitempty"`
		UnclaimedResources []internedResourceDesc  `json:"unclaimed_resources,omitempty"`
		DeltaEdges         []internedEdgeDesc      `json:"delta_edges,omitempty"`
		ValidationFeedback []string                `json:"validation_feedback,omitempty"`
	}{
		FrozenComponents:   buildReconcileCompHeaders(existing.Components, table),
		FrozenContainers:   buildReconcileContHeaders(existing.Containers),
		ExistingViews:      buildReconcileViewCapacities(existing.Views),
		RejectedEdges:      existing.RejectedEdges,
		UnclaimedResources: buildReconcileUnclaimedDescs(delta.UnclaimedResourceIDs, table),
		DeltaEdges:         buildReconcileDeltaEdges(delta.NewEdgeIDs, table),
		ValidationFeedback: feedback,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal reconcile prompt: %w", err)
	}
	return string(data), nil
}

func buildReconcileCompHeaders(components []manifest.Component, table *InternTable) []reconcileCompHeader {
	comps := make([]reconcileCompHeader, 0, len(components))
	for _, c := range components {
		var rids []string
		for _, br := range c.BackedBy {
			if rid, ok := table.ResourceID(br.URI); ok {
				rids = append(rids, rid)
			}
		}
		slices.Sort(rids)
		comps = append(comps, reconcileCompHeader{
			ID:            c.ID,
			Title:         c.Title,
			ComponentKind: c.ComponentKind,
			ProjectID:     c.ProjectID,
			BackedByIDs:   rids,
		})
	}
	slices.SortFunc(comps, func(a, b reconcileCompHeader) int { return cmp.Compare(a.ID, b.ID) })
	return comps
}

func buildReconcileContHeaders(containers []manifest.Container) []reconcileContHeader {
	conts := make([]reconcileContHeader, 0, len(containers))
	for _, ct := range containers {
		conts = append(conts, reconcileContHeader{
			ID:                ct.ID,
			Title:             ct.Title,
			Kind:              ct.Kind,
			ParentContainerID: ct.ParentContainerID,
		})
	}
	slices.SortFunc(conts, func(a, b reconcileContHeader) int { return cmp.Compare(a.ID, b.ID) })
	return conts
}

func buildReconcileViewCapacities(manifestViews []manifest.View) []reconcileViewCapacity {
	var views []reconcileViewCapacity
	for _, v := range manifestViews {
		if v.ID == manifest.SystemIndexViewID {
			continue
		}
		maxCol := 4
		if v.Archetype == manifest.ArchetypeHubSpoke {
			maxCol = 2
		}
		placed := make([]string, 0, len(v.Placements))
		for _, p := range v.Placements {
			placed = append(placed, p.ComponentID)
		}
		slices.Sort(placed)
		views = append(views, reconcileViewCapacity{
			ID:                v.ID,
			Title:             v.Title,
			Archetype:         v.Archetype,
			MaxColumn:         maxCol,
			RemainingCapacity: max(0, 12-len(v.Placements)),
			ContainerIDs:      slices.Clone(v.ContainerIDs),
			PlacedComponents:  placed,
		})
	}
	slices.SortFunc(views, func(a, b reconcileViewCapacity) int { return cmp.Compare(a.ID, b.ID) })
	return views
}

func buildReconcileUnclaimedDescs(ids []string, table *InternTable) []internedResourceDesc {
	unclaimedIDs := slices.Clone(ids)
	slices.Sort(unclaimedIDs)
	unclaimed := make([]internedResourceDesc, 0, len(unclaimedIDs))
	for _, rid := range unclaimedIDs {
		if r, ok := table.ResourceByID(rid); ok {
			unclaimed = append(unclaimed, makeResourceDesc(rid, r, table))
		}
	}
	return unclaimed
}

func buildReconcileDeltaEdges(ids []string, table *InternTable) []internedEdgeDesc {
	newEdgeIDs := slices.Clone(ids)
	slices.Sort(newEdgeIDs)
	newEdges := make([]internedEdgeDesc, 0, len(newEdgeIDs))
	for _, eid := range newEdgeIDs {
		fe, ok := table.EdgeByID(eid)
		if !ok {
			continue
		}
		fromRID, okF := table.ResourceID(fe.FromURI)
		toRID, okT := table.ResourceID(fe.ToURI)
		if okF && okT {
			newEdges = append(newEdges, internedEdgeDesc{
				ID:     eid,
				FromID: fromRID,
				ToID:   toRID,
				Kind:   fe.Kind,
				Detail: fe.Detail,
			})
		}
	}
	return newEdges
}
