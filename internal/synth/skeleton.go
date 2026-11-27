package synth

import (
	"cmp"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/icons"
	"github.com/arrase/gcp-arch/internal/manifest"
)

type skeletonNode struct {
	ID               string
	Title            string
	Subtitle         string
	IconKey          string
	ComponentKind    manifest.ComponentKind
	RoleTag          string
	AssetType        string
	ProjectID        string
	ServiceProjectID string
	ContainerID      string
	IsExternal       bool
	Exposure         string // PUBLIC | PRIVATE | INTERNAL | EXTERNAL
	TopoRank         int
	SubRank          int
	CohortCount      int
	Members          []string
	BackedBy         []manifest.BackedResource
	Badges           []manifest.ComponentBadge
	FoldedEdgeIDs    []string
	InlineMeta       []manifest.InlineMeta
}

type skeletonEdge struct {
	ID                 string
	FromNodeID         string
	ToNodeID           string
	Verb               string
	FlowKind           manifest.FlowKind
	SemanticTag        string
	Provenance         manifest.Provenance
	EvidenceEdgeIDs    []string
	InferenceRationale string
	Kinds              []extract.FactEdgeKind
	Details            []string
	IsRetryOrDLQ       bool
}

type skeleton struct {
	Scopes                   []string
	Containers               []manifest.Container
	Nodes                    []skeletonNode
	Edges                    []skeletonEdge
	TransversalItems         []manifest.TransversalItem
	StructuralEdges          []manifest.StructuralEdge
	SuppressedPlumbingCounts map[string]int
}

// buildSkeleton deterministically constructs the containment tree, 1:1 plumbing folds,
// homogeneous cohorts, transversal separation, topological ranks, and barycenter sub-ranks
// from a v3 FactsFile without calling any LLM.
func buildSkeleton(facts *extract.FactsFile, reg *icons.Registry) *skeleton {
	sortedRes, resByURI, sortedEdges := sortFacts(facts)
	transversalCatByURI, structuralByURI := classifySkeletonResources(sortedRes)

	foldTargetURI, foldedEdgesByRep := detectOneToOneFolds(sortedRes, sortedEdges, resByURI)
	resolveRep := makeFoldResolver(foldTargetURI)
	foldedChildrenByRep := groupFoldedChildren(foldTargetURI, resolveRep, resByURI)

	primaryCandidates := collectPrimaryCandidates(sortedRes, transversalCatByURI, structuralByURI, foldTargetURI)
	cb := newContainerBuilder(facts, resByURI)
	candContainerID, candExposure := placePrimaryCandidates(cb, primaryCandidates, reg)

	cohorts, nonCohorts := buildHomogeneousCohorts(primaryCandidates, sortedEdges, candContainerID, resByURI)
	nodes, uriToNodeID, structuralEdges := materializeSkeletonNodes(
		cohorts, nonCohorts, foldedChildrenByRep, foldedEdgesByRep,
		candContainerID, candExposure, cb, reg,
	)

	nodeIdxByID := indexNodePositions(nodes)
	transversalItems, transversalIdxByURI := buildSkeletonTransversalItems(sortedRes, transversalCatByURI, cb)

	edges, structuralEdges := buildSkeletonEdges(
		sortedEdges, structuralEdges,
		transversalItems, transversalIdxByURI, uriToNodeID, nodeIdxByID, nodes, cb, resByURI, resolveRep,
	)
	edges = connectOrphanSkeletonNodes(nodes, edges, sortedEdges, transversalItems, uriToNodeID, nodeIdxByID, cb, resByURI, resolveRep)

	attachResourceBadges(nodes, resByURI)
	markIngressBuckets(nodes, edges)
	computeTopoRanks(nodes, edges)
	refineSubRanks(nodes, edges)

	containers := cb.finalizeContainers(nodes)
	slices.SortFunc(transversalItems, func(a, b manifest.TransversalItem) int {
		return cmp.Or(cmp.Compare(a.Category, b.Category), cmp.Compare(a.ProjectID, b.ProjectID), cmp.Compare(a.ID, b.ID))
	})

	return &skeleton{
		Scopes:                   slices.Clone(facts.Scopes),
		Containers:               containers,
		Nodes:                    nodes,
		Edges:                    edges,
		TransversalItems:         transversalItems,
		StructuralEdges:          structuralEdges,
		SuppressedPlumbingCounts: maps.Clone(facts.SuppressedPlumbingCounts),
	}
}

func sortFacts(facts *extract.FactsFile) ([]extract.FactResource, map[string]extract.FactResource, []extract.FactEdge) {
	sortedRes := slices.Clone(facts.Resources)
	slices.SortFunc(sortedRes, func(a, b extract.FactResource) int {
		return cmp.Or(
			cmp.Compare(a.URI, b.URI),
			cmp.Compare(a.AssetType, b.AssetType),
			cmp.Compare(a.ProjectID, b.ProjectID),
		)
	})
	resByURI := make(map[string]extract.FactResource, len(sortedRes))
	for _, r := range sortedRes {
		resByURI[r.URI] = r
	}

	sortedEdges := slices.Clone(facts.Edges)
	for i := range sortedEdges {
		if sortedEdges[i].ID == "" {
			sortedEdges[i].ID = extract.EdgeID(sortedEdges[i].Kind, sortedEdges[i].FromURI, sortedEdges[i].ToURI, sortedEdges[i].Detail)
		}
	}
	slices.SortFunc(sortedEdges, func(a, b extract.FactEdge) int {
		return cmp.Or(
			cmp.Compare(a.FromURI, b.FromURI),
			cmp.Compare(a.ToURI, b.ToURI),
			cmp.Compare(a.Kind, b.Kind),
			cmp.Compare(a.Detail, b.Detail),
			cmp.Compare(a.ID, b.ID),
		)
	})
	return sortedRes, resByURI, sortedEdges
}

func classifySkeletonResources(sortedRes []extract.FactResource) (map[string]string, map[string]extract.FactResource) {
	transversalCatByURI := make(map[string]string)
	structuralByURI := make(map[string]extract.FactResource)
	for _, r := range sortedRes {
		if cat, ok := classifyTransversalAsset(r.AssetType); ok {
			transversalCatByURI[r.URI] = cat
			continue
		}
		if isStructuralContainerAsset(r) {
			structuralByURI[r.URI] = r
		}
	}
	return transversalCatByURI, structuralByURI
}

func makeFoldResolver(foldTargetURI map[string]string) func(string) string {
	return func(uri string) string {
		for range 4 {
			next, ok := foldTargetURI[uri]
			if !ok || next == uri {
				break
			}
			uri = next
		}
		return uri
	}
}

func groupFoldedChildren(
	foldTargetURI map[string]string,
	resolveRep func(string) string,
	resByURI map[string]extract.FactResource,
) map[string][]extract.FactResource {
	grouped := make(map[string][]extract.FactResource)
	for foldedURI := range foldTargetURI {
		rep := resolveRep(foldedURI)
		grouped[rep] = append(grouped[rep], resByURI[foldedURI])
	}
	for rep := range grouped {
		slices.SortFunc(grouped[rep], func(a, b extract.FactResource) int {
			return cmp.Compare(a.URI, b.URI)
		})
	}
	return grouped
}

func collectPrimaryCandidates(
	sortedRes []extract.FactResource,
	transversalCatByURI map[string]string,
	structuralByURI map[string]extract.FactResource,
	foldTargetURI map[string]string,
) []extract.FactResource {
	var candidates []extract.FactResource
	for _, r := range sortedRes {
		if transversalCatByURI[r.URI] != "" || structuralByURI[r.URI].URI != "" || foldTargetURI[r.URI] != "" {
			continue
		}
		candidates = append(candidates, r)
	}
	return candidates
}

func placePrimaryCandidates(cb *containerBuilder, candidates []extract.FactResource, reg *icons.Registry) (map[string]string, map[string]string) {
	candContainerID := make(map[string]string, len(candidates))
	candExposure := make(map[string]string, len(candidates))
	for _, r := range candidates {
		cid, exp := cb.placePrimaryResource(r, reg)
		candContainerID[r.URI] = cid
		candExposure[r.URI] = exp
	}
	return candContainerID, candExposure
}

func materializeSkeletonNodes(
	cohorts []cohortGroup,
	nonCohorts []extract.FactResource,
	foldedChildrenByRep map[string][]extract.FactResource,
	foldedEdgesByRep map[string][]extract.FactEdge,
	candContainerID map[string]string,
	candExposure map[string]string,
	cb *containerBuilder,
	reg *icons.Registry,
) ([]skeletonNode, map[string]string, []manifest.StructuralEdge) {
	var nodes []skeletonNode
	uriToNodeID := make(map[string]string)
	var structuralEdges []manifest.StructuralEdge
	emittedNodeIDs := make(map[string]bool)

	nextNodeID := func(prefix, proj, name string) string {
		base := fmt.Sprintf("%s_%s_%s", prefix, sanitizeID(proj), sanitizeID(name))
		if len(base) > 72 {
			base = strings.TrimRight(base[:72], "_")
		}
		return uniqueID(base, emittedNodeIDs)
	}

	for _, coh := range cohorts {
		node, edges := buildCohortNode(coh, foldedChildrenByRep, foldedEdgesByRep, candContainerID, candExposure, cb, reg, nextNodeID, uriToNodeID)
		nodes = append(nodes, node)
		structuralEdges = append(structuralEdges, edges...)
	}
	for _, r := range nonCohorts {
		node, edges := buildSingleNode(r, foldedChildrenByRep[r.URI], foldedEdgesByRep[r.URI], candContainerID[r.URI], candExposure[r.URI], cb, reg, nextNodeID, uriToNodeID)
		nodes = append(nodes, node)
		structuralEdges = append(structuralEdges, edges...)
	}
	return nodes, uriToNodeID, structuralEdges
}

func uniqueID(base string, emitted map[string]bool) string {
	id := base
	for n := 2; emitted[id]; n++ {
		id = fmt.Sprintf("%s_%d", base, n)
	}
	emitted[id] = true
	return id
}

func buildCohortNode(
	coh cohortGroup,
	foldedChildrenByRep map[string][]extract.FactResource,
	foldedEdgesByRep map[string][]extract.FactEdge,
	candContainerID, candExposure map[string]string,
	cb *containerBuilder,
	reg *icons.Registry,
	nextNodeID func(prefix, proj, name string) string,
	uriToNodeID map[string]string,
) (skeletonNode, []manifest.StructuralEdge) {
	first := coh.members[0]
	spec := classifyAssetSpec(first, reg)
	nid := nextNodeID("cohort", first.ProjectID, coh.title)

	backed := make([]manifest.BackedResource, 0, len(coh.members))
	memberNames := make([]string, 0, len(coh.members))
	var foldedEdgeIDs []string
	var structEdges []manifest.StructuralEdge
	for _, m := range coh.members {
		backed = append(backed, manifest.BackedResource{URI: m.URI, AssetType: m.AssetType, ProjectID: m.ProjectID})
		memberNames = append(memberNames, m.DisplayName)
		uriToNodeID[m.URI] = nid
		for _, child := range foldedChildrenByRep[m.URI] {
			backed = append(backed, manifest.BackedResource{URI: child.URI, AssetType: child.AssetType, ProjectID: child.ProjectID})
			memberNames = append(memberNames, child.DisplayName)
			uriToNodeID[child.URI] = nid
		}
		for _, fe := range foldedEdgesByRep[m.URI] {
			foldedEdgeIDs = append(foldedEdgeIDs, fe.ID)
			structEdges = append(structEdges, manifest.StructuralEdge{
				EdgeID:   fe.ID,
				Kind:     string(fe.Kind),
				FromURI:  fe.FromURI,
				ToURI:    fe.ToURI,
				Reason:   "1to1_plumbing_fold",
				TargetID: nid,
			})
		}
	}
	slices.Sort(memberNames)
	memberNames = slices.Compact(memberNames)

	var badges []manifest.ComponentBadge
	svcProj := cb.serviceProjectID(first.ProjectID)
	if svcProj != "" {
		badges = append(badges, manifest.ComponentBadge{Kind: "project", Label: svcProj})
	}

	return skeletonNode{
		ID:               nid,
		Title:            coh.title,
		Subtitle:         fmt.Sprintf("%s · %d members", shortAssetType(first.AssetType), len(coh.members)),
		IconKey:          spec.iconKey,
		ComponentKind:    manifest.KindCohort,
		RoleTag:          spec.roleTag,
		AssetType:        first.AssetType,
		ProjectID:        first.ProjectID,
		ServiceProjectID: svcProj,
		ContainerID:      candContainerID[first.URI],
		Exposure:         candExposure[first.URI],
		TopoRank:         spec.baseLayer,
		CohortCount:      len(coh.members),
		Members:          memberNames,
		BackedBy:         backed,
		Badges:           badges,
		FoldedEdgeIDs:    foldedEdgeIDs,
		InlineMeta: []manifest.InlineMeta{
			{Label: "Members", Value: fmt.Sprintf("%d homogeneous resources", len(coh.members))},
		},
	}, structEdges
}

func buildSingleNode(
	r extract.FactResource,
	foldedChildren []extract.FactResource,
	foldedEdges []extract.FactEdge,
	containerID, exposure string,
	cb *containerBuilder,
	reg *icons.Registry,
	nextNodeID func(prefix, proj, name string) string,
	uriToNodeID map[string]string,
) (skeletonNode, []manifest.StructuralEdge) {
	spec := classifyAssetSpec(r, reg)
	nid := nextNodeID("n", r.ProjectID, r.DisplayName)

	backed := []manifest.BackedResource{{URI: r.URI, AssetType: r.AssetType, ProjectID: r.ProjectID}}
	memberNames := []string{r.DisplayName}
	uriToNodeID[r.URI] = nid

	var badges []manifest.ComponentBadge
	if r.IsExternal {
		badges = append(badges, manifest.ComponentBadge{Kind: "exposure", Label: "EXTERNAL"})
	} else if exposure == "PUBLIC" || exposure == "INTERNAL" {
		badges = append(badges, manifest.ComponentBadge{Kind: "exposure", Label: exposure})
	}

	svcProj := cb.serviceProjectID(r.ProjectID)
	if svcProj != "" {
		badges = append(badges, manifest.ComponentBadge{Kind: "project", Label: svcProj})
	}

	for _, child := range foldedChildren {
		backed = append(backed, manifest.BackedResource{URI: child.URI, AssetType: child.AssetType, ProjectID: child.ProjectID})
		memberNames = append(memberNames, child.DisplayName)
		uriToNodeID[child.URI] = nid
		if badge, ok := foldedChildBadge(child); ok {
			badges = append(badges, badge)
		}
	}

	var foldedEdgeIDs []string
	var structEdges []manifest.StructuralEdge
	for _, fe := range foldedEdges {
		foldedEdgeIDs = append(foldedEdgeIDs, fe.ID)
		structEdges = append(structEdges, manifest.StructuralEdge{
			EdgeID:   fe.ID,
			Kind:     string(fe.Kind),
			FromURI:  fe.FromURI,
			ToURI:    fe.ToURI,
			Reason:   "1to1_plumbing_fold",
			TargetID: nid,
		})
	}

	slices.Sort(memberNames)
	memberNames = slices.Compact(memberNames)

	return skeletonNode{
		ID:               nid,
		Title:            r.DisplayName,
		Subtitle:         buildNodeSubtitle(r, foldedChildren),
		IconKey:          spec.iconKey,
		ComponentKind:    spec.componentKind,
		RoleTag:          spec.roleTag,
		AssetType:        r.AssetType,
		ProjectID:        r.ProjectID,
		ServiceProjectID: svcProj,
		ContainerID:      containerID,
		IsExternal:       r.IsExternal,
		Exposure:         exposure,
		TopoRank:         spec.baseLayer,
		Members:          memberNames,
		BackedBy:         backed,
		Badges:           badges,
		FoldedEdgeIDs:    foldedEdgeIDs,
		InlineMeta:       extractInlineMeta(r, foldedChildren),
	}, structEdges
}

func foldedChildBadge(child extract.FactResource) (manifest.ComponentBadge, bool) {
	switch child.AssetType {
	case "k8s.io/Service":
		return manifest.ComponentBadge{Kind: "svc", Label: "svc: " + child.DisplayName}, true
	case "networking.k8s.io/Ingress":
		return manifest.ComponentBadge{Kind: "ingress", Label: "ing: " + child.DisplayName}, true
	case "compute.googleapis.com/BackendService":
		return manifest.ComponentBadge{Kind: "backend", Label: "be: " + child.DisplayName}, true
	case "pubsub.googleapis.com/Subscription":
		return manifest.ComponentBadge{Kind: "sub", Label: "sub: " + child.DisplayName}, true
	case "compute.googleapis.com/VpnGateway":
		return manifest.ComponentBadge{Kind: "vpn", Label: "gw: " + child.DisplayName}, true
	default:
		return manifest.ComponentBadge{}, false
	}
}

func indexNodePositions(nodes []skeletonNode) map[string]int {
	idxByID := make(map[string]int, len(nodes))
	for i := range nodes {
		idxByID[nodes[i].ID] = i
	}
	return idxByID
}

func buildSkeletonTransversalItems(
	sortedRes []extract.FactResource,
	transversalCatByURI map[string]string,
	cb *containerBuilder,
) ([]manifest.TransversalItem, map[string]int) {
	var items []manifest.TransversalItem
	emittedIDs := make(map[string]bool)
	for _, r := range sortedRes {
		cat := transversalCatByURI[r.URI]
		if cat == "" {
			continue
		}
		base := fmt.Sprintf("tr_%s_%s_%s", sanitizeID(r.ProjectID), sanitizeID(shortAssetType(r.AssetType)), sanitizeID(r.DisplayName))
		if len(base) > 64 {
			base = strings.TrimRight(base[:64], "_")
		}
		tid := uniqueID(base, emittedIDs)
		items = append(items, manifest.TransversalItem{
			ID:               tid,
			Category:         cat,
			AssetType:        r.AssetType,
			DisplayName:      r.DisplayName,
			Detail:           summarizeTransversal(r),
			ProjectID:        r.ProjectID,
			OwnerContainerID: cb.resolveTransversalOwnerContainer(r),
			BackedBy:         []manifest.BackedResource{{URI: r.URI, AssetType: r.AssetType, ProjectID: r.ProjectID}},
		})
	}
	idxByURI := make(map[string]int, len(items))
	for i := range items {
		idxByURI[items[i].BackedBy[0].URI] = i
	}
	return items, idxByURI
}

func attachResourceBadges(nodes []skeletonNode, resByURI map[string]extract.FactResource) {
	for i := range nodes {
		n := &nodes[i]
		if len(n.BackedBy) == 0 {
			continue
		}
		if r, ok := resByURI[n.BackedBy[0].URI]; ok {
			if sa := attrStr(r.Attributes, "service_account"); sa != "" {
				addUniqueBadge(n, manifest.ComponentBadge{Kind: "sa", Label: "SA: " + shortSA(sa)})
			}
			if kms := attrStr(r.Attributes, "kms_key_uri"); kms != "" {
				addUniqueBadge(n, manifest.ComponentBadge{Kind: "kms", Label: "KMS: " + path.Base(kms)})
			}
		}
	}
}

func markIngressBuckets(nodes []skeletonNode, edges []skeletonEdge) {
	outEdges := make(map[string][]skeletonEdge)
	for _, e := range edges {
		if !e.IsRetryOrDLQ {
			outEdges[e.FromNodeID] = append(outEdges[e.FromNodeID], e)
		}
	}
	nodeIdxByID := indexNodePositions(nodes)
	for i := range nodes {
		n := &nodes[i]
		if n.AssetType != "storage.googleapis.com/Bucket" {
			continue
		}
		low := strings.ToLower(n.Title)
		if strings.Contains(low, "dlq") || strings.Contains(low, "archive") || strings.Contains(low, "backup") {
			continue
		}
		for _, e := range outEdges[n.ID] {
			if toIdx, ok := nodeIdxByID[e.ToNodeID]; ok {
				if nodes[toIdx].AssetType == "eventarc.googleapis.com/Trigger" || slices.Contains(e.Details, "eventarc.bucket") {
					n.TopoRank = 1
					break
				}
			}
		}
	}
}

func detectOneToOneFolds(
	sortedRes []extract.FactResource,
	sortedEdges []extract.FactEdge,
	resByURI map[string]extract.FactResource,
) (map[string]string, map[string][]extract.FactEdge) {
	foldTarget := make(map[string]string)
	foldedEdges := make(map[string][]extract.FactEdge)

	foldK8sServicesToDeployments(sortedEdges, resByURI, foldTarget, foldedEdges)
	foldK8sIngressToWorkloads(sortedEdges, resByURI, foldTarget, foldedEdges)
	foldBackendServicesToForwardingRules(sortedEdges, resByURI, foldTarget, foldedEdges)
	foldVpnGatewaysToTunnels(sortedRes, resByURI, foldTarget)
	foldPubSubSubscriptionsToTopics(sortedEdges, resByURI, foldTarget, foldedEdges)

	return foldTarget, foldedEdges
}

func foldK8sServicesToDeployments(
	sortedEdges []extract.FactEdge,
	resByURI map[string]extract.FactResource,
	foldTarget map[string]string,
	foldedEdges map[string][]extract.FactEdge,
) {
	svcToDeps := make(map[string][]extract.FactEdge)
	for _, e := range sortedEdges {
		if e.Kind != extract.EdgeK8sServiceSelector {
			continue
		}
		fr, okF := resByURI[e.FromURI]
		to, okT := resByURI[e.ToURI]
		if okF && okT && fr.AssetType == "k8s.io/Service" && to.AssetType == "apps.k8s.io/Deployment" {
			svcToDeps[fr.URI] = append(svcToDeps[fr.URI], e)
		}
	}
	for svcURI, es := range svcToDeps {
		if len(es) == 1 {
			depURI := es[0].ToURI
			foldTarget[svcURI] = depURI
			foldedEdges[depURI] = append(foldedEdges[depURI], es[0])
		}
	}
}

func foldK8sIngressToWorkloads(
	sortedEdges []extract.FactEdge,
	resByURI map[string]extract.FactResource,
	foldTarget map[string]string,
	foldedEdges map[string][]extract.FactEdge,
) {
	ingToBackends := make(map[string][]extract.FactEdge)
	for _, e := range sortedEdges {
		if e.Kind != extract.EdgeK8sIngressBackend {
			continue
		}
		if fr, ok := resByURI[e.FromURI]; ok && fr.AssetType == "networking.k8s.io/Ingress" {
			ingToBackends[fr.URI] = append(ingToBackends[fr.URI], e)
		}
	}
	for ingURI, es := range ingToBackends {
		if len(es) != 1 {
			continue
		}
		targetURI := es[0].ToURI
		if depURI, ok := foldTarget[targetURI]; ok {
			foldTarget[ingURI] = depURI
			foldedEdges[depURI] = append(foldedEdges[depURI], es[0])
		} else if toR, ok := resByURI[targetURI]; ok && toR.AssetType == "apps.k8s.io/Deployment" {
			foldTarget[ingURI] = targetURI
			foldedEdges[targetURI] = append(foldedEdges[targetURI], es[0])
		}
	}
}

func foldBackendServicesToForwardingRules(
	sortedEdges []extract.FactEdge,
	resByURI map[string]extract.FactResource,
	foldTarget map[string]string,
	foldedEdges map[string][]extract.FactEdge,
) {
	frOutBS := make(map[string][]extract.FactEdge)
	bsInFR := make(map[string][]extract.FactEdge)
	for _, e := range sortedEdges {
		if e.Kind != extract.EdgeConfigRef {
			continue
		}
		fr := resByURI[e.FromURI]
		to := resByURI[e.ToURI]
		isFR := fr.AssetType == "compute.googleapis.com/GlobalForwardingRule" || fr.AssetType == "compute.googleapis.com/ForwardingRule"
		if isFR && to.AssetType == "compute.googleapis.com/BackendService" {
			frOutBS[fr.URI] = append(frOutBS[fr.URI], e)
			bsInFR[to.URI] = append(bsInFR[to.URI], e)
		}
	}
	for bsURI, inEs := range bsInFR {
		if len(inEs) == 1 && len(frOutBS[inEs[0].FromURI]) == 1 {
			frURI := inEs[0].FromURI
			foldTarget[bsURI] = frURI
			foldedEdges[frURI] = append(foldedEdges[frURI], inEs[0])
		}
	}
}

func foldVpnGatewaysToTunnels(
	sortedRes []extract.FactResource,
	resByURI map[string]extract.FactResource,
	foldTarget map[string]string,
) {
	tunnelsByGW := make(map[string][]extract.FactResource)
	for _, r := range sortedRes {
		if r.AssetType != "compute.googleapis.com/VpnTunnel" || r.ParentURI == "" {
			continue
		}
		if pr, ok := resByURI[r.ParentURI]; ok && pr.AssetType == "compute.googleapis.com/VpnGateway" {
			tunnelsByGW[pr.URI] = append(tunnelsByGW[pr.URI], r)
		}
	}
	for gwURI, tunnels := range tunnelsByGW {
		if len(tunnels) == 1 {
			foldTarget[gwURI] = tunnels[0].URI
		}
	}
}

func foldPubSubSubscriptionsToTopics(
	sortedEdges []extract.FactEdge,
	resByURI map[string]extract.FactResource,
	foldTarget map[string]string,
	foldedEdges map[string][]extract.FactEdge,
) {
	subToTopic := make(map[string][]extract.FactEdge)
	subsByTopic := make(map[string][]string)
	for _, e := range sortedEdges {
		if e.Kind != extract.EdgeConfigRef || e.Detail != "subscription" {
			continue
		}
		fr := resByURI[e.FromURI]
		to := resByURI[e.ToURI]
		if fr.AssetType != "pubsub.googleapis.com/Topic" || to.AssetType != "pubsub.googleapis.com/Subscription" {
			continue
		}
		subToTopic[to.URI] = append(subToTopic[to.URI], e)
		if !slices.Contains(subsByTopic[fr.URI], to.URI) {
			subsByTopic[fr.URI] = append(subsByTopic[fr.URI], to.URI)
		}
	}
	for subURI, es := range subToTopic {
		if len(es) == 1 && len(subsByTopic[es[0].FromURI]) == 1 {
			topicURI := es[0].FromURI
			foldTarget[subURI] = topicURI
			foldedEdges[topicURI] = append(foldedEdges[topicURI], es[0])
		}
	}
}

type cohortGroup struct {
	title   string
	members []extract.FactResource
}

func buildHomogeneousCohorts(
	candidates []extract.FactResource,
	edges []extract.FactEdge,
	candContainerID map[string]string,
	resByURI map[string]extract.FactResource,
) ([]cohortGroup, []extract.FactResource) {
	inSig := make(map[string][]string)
	outSig := make(map[string][]string)
	for _, e := range edges {
		if e.Heuristic {
			continue
		}
		if _, isTransTo := classifyTransversalAsset(resByURI[e.ToURI].AssetType); !isTransTo {
			outSig[e.FromURI] = append(outSig[e.FromURI], string(e.Kind)+":"+e.ToURI)
		}
		if _, isTransFrom := classifyTransversalAsset(resByURI[e.FromURI].AssetType); !isTransFrom {
			inSig[e.ToURI] = append(inSig[e.ToURI], string(e.Kind)+":"+e.FromURI)
		}
	}
	sigOf := func(r extract.FactResource) string {
		ins := slices.Clone(inSig[r.URI])
		outs := slices.Clone(outSig[r.URI])
		slices.Sort(ins)
		slices.Sort(outs)
		ins = slices.Compact(ins)
		outs = slices.Compact(outs)
		return fmt.Sprintf("%s|%s|%s|in=%s|out=%s",
			r.ProjectID, r.AssetType, candContainerID[r.URI],
			strings.Join(ins, ","), strings.Join(outs, ","))
	}

	triggerCountByProj := make(map[string]int)
	for _, r := range candidates {
		if r.AssetType == "cloudbuild.googleapis.com/BuildTrigger" {
			triggerCountByProj[r.ProjectID]++
		}
	}

	groups := make(map[string][]extract.FactResource)
	var passthrough []extract.FactResource
	for _, r := range candidates {
		if r.IsExternal {
			passthrough = append(passthrough, r)
			continue
		}
		switch r.AssetType {
		case "cloudbuild.googleapis.com/BuildTrigger",
			"apps.k8s.io/Deployment",
			"run.googleapis.com/Service",
			"run.googleapis.com/Job",
			"cloudfunctions.googleapis.com/CloudFunction",
			"batch.k8s.io/CronJob":
			groups[sigOf(r)] = append(groups[sigOf(r)], r)
		default:
			passthrough = append(passthrough, r)
		}
	}

	var cohorts []cohortGroup
	for _, k := range slices.Sorted(maps.Keys(groups)) {
		members := groups[k]
		slices.SortFunc(members, func(a, b extract.FactResource) int {
			return cmp.Compare(a.URI, b.URI)
		})
		first := members[0]
		minCohort := 3
		if first.AssetType == "cloudbuild.googleapis.com/BuildTrigger" && triggerCountByProj[first.ProjectID] >= 3 {
			minCohort = 2
		}
		if len(members) < minCohort {
			passthrough = append(passthrough, members...)
			continue
		}
		title := buildCohortTitle(first, members, outSig[first.URI], resByURI)
		cohorts = append(cohorts, cohortGroup{title: title, members: members})
	}

	slices.SortFunc(passthrough, func(a, b extract.FactResource) int {
		return cmp.Or(cmp.Compare(a.URI, b.URI), cmp.Compare(a.AssetType, b.AssetType))
	})
	return cohorts, passthrough
}

func buildCohortTitle(first extract.FactResource, members []extract.FactResource, outSigs []string, resByURI map[string]extract.FactResource) string {
	if first.AssetType == "cloudbuild.googleapis.com/BuildTrigger" {
		prefix := "Build"
		lowerName := strings.ToLower(first.DisplayName)
		if strings.Contains(lowerName, "deploy") {
			prefix = "App Deploy"
		} else if strings.Contains(lowerName, "terraform") {
			prefix = "Terraform"
		}
		if prefix != "Build" {
			if targetProj := singleExternalTargetProject(first.ProjectID, outSigs, resByURI); targetProj != "" {
				return fmt.Sprintf("%s %s Triggers (%dx)", targetProj, prefix, len(members))
			}
		}
		return fmt.Sprintf("CI %s Triggers (%dx)", prefix, len(members))
	}
	if common := commonPrefixName(members); common != "" {
		return fmt.Sprintf("%s (%dx %s)", common, len(members), shortAssetType(first.AssetType))
	}
	return fmt.Sprintf("%s (%dx)", shortAssetType(first.AssetType), len(members))
}

func singleExternalTargetProject(srcProj string, outSigs []string, resByURI map[string]extract.FactResource) string {
	var target string
	for _, sig := range outSigs {
		_, uri, ok := strings.Cut(sig, ":")
		if !ok {
			continue
		}
		r, exists := resByURI[uri]
		if !exists || r.ProjectID == "" || r.ProjectID == srcProj || strings.EqualFold(r.ProjectID, "external") {
			continue
		}
		if target == "" {
			target = r.ProjectID
		} else if target != r.ProjectID {
			return ""
		}
	}
	return target
}

func commonPrefixName(members []extract.FactResource) string {
	if len(members) == 0 {
		return ""
	}
	parts := strings.Split(members[0].DisplayName, "-")
	for len(parts) > 1 {
		prefix := strings.Join(parts, "-")
		all := true
		for _, m := range members[1:] {
			if !strings.HasPrefix(m.DisplayName, prefix) {
				all = false
				break
			}
		}
		if all && len(prefix) >= 4 {
			return prefix
		}
		parts = parts[:len(parts)-1]
	}
	return ""
}

func buildNodeSubtitle(r extract.FactResource, folded []extract.FactResource) string {
	parts := []string{shortAssetType(r.AssetType)}
	if r.Location != "" && r.Location != "global" {
		parts = append(parts, r.Location)
	}
	if len(folded) > 0 {
		parts = append(parts, fmt.Sprintf("+%d folded", len(folded)))
	}
	return strings.Join(parts, " · ")
}

func extractInlineMeta(r extract.FactResource, folded []extract.FactResource) []manifest.InlineMeta {
	var meta []manifest.InlineMeta
	add := func(k, v string) {
		v = strings.TrimSpace(v)
		if v != "" && len(meta) < 4 {
			meta = append(meta, manifest.InlineMeta{Label: k, Value: v})
		}
	}
	switch r.AssetType {
	case "compute.googleapis.com/GlobalForwardingRule", "compute.googleapis.com/ForwardingRule":
		if ip := attrStr(r.Attributes, "ip_address"); ip != "" {
			if port := attrStr(r.Attributes, "port_range"); port != "" {
				add("VIP", ip+":"+port)
			} else {
				add("VIP", ip)
			}
		}
		if scheme := attrStr(r.Attributes, "load_balancing_scheme"); scheme != "" {
			add("Scheme", scheme)
		}
	case "apps.k8s.io/Deployment":
		add("Namespace", attrStr(r.Attributes, "namespace"))
	case "run.googleapis.com/Service":
		if ing := attrStr(r.Attributes, "ingress"); ing != "" {
			add("Ingress", strings.TrimPrefix(ing, "INGRESS_TRAFFIC_"))
		}
	case "cloudfunctions.googleapis.com/CloudFunction":
		add("Runtime", attrStr(r.Attributes, "runtime"))
	case "sqladmin.googleapis.com/Instance", "alloydb.googleapis.com/Cluster":
		add("Engine", attrStr(r.Attributes, "database_version"))
		add("Private IP", attrStr(r.Attributes, "private_ip"))
	case "redis.googleapis.com/Instance", "redis.googleapis.com/Cluster":
		add("Redis", attrStr(r.Attributes, "redis_version"))
	case "bigquery.googleapis.com/Dataset":
		add("Location", attrStr(r.Attributes, "location"))
	case "storage.googleapis.com/Bucket":
		add("Class", attrStr(r.Attributes, "storage_class"))
	case "artifactregistry.googleapis.com/Repository":
		add("Format", attrStr(r.Attributes, "format"))
	case "pubsub.googleapis.com/Topic":
		if len(folded) > 0 {
			add("Subscriptions", fmt.Sprintf("%d", len(folded)))
		}
	}
	return meta
}

func summarizeTransversal(r extract.FactResource) string {
	var parts []string
	if r.Location != "" {
		parts = append(parts, r.Location)
	}
	for _, k := range []string{"purpose", "cidr", "replication", "direction"} {
		if v := attrStr(r.Attributes, k); v != "" {
			parts = append(parts, v)
		}
	}
	if len(parts) == 0 {
		return shortAssetType(r.AssetType)
	}
	return strings.Join(parts, " | ")
}

func attrStr(attrs map[string]any, key string) string {
	if attrs == nil {
		return ""
	}
	v, ok := attrs[key]
	if !ok || v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func regionFromLocation(loc string) string {
	loc = strings.TrimSpace(loc)
	if loc == "" {
		return "global"
	}
	if len(loc) > 3 && loc[len(loc)-2] == '-' {
		last := loc[len(loc)-1]
		if last >= 'a' && last <= 'z' {
			return loc[:len(loc)-2]
		}
	}
	return loc
}

func shortAssetType(at string) string {
	if idx := strings.LastIndex(at, "/"); idx >= 0 {
		return at[idx+1:]
	}
	return at
}

func shortSA(email string) string {
	if idx := strings.IndexByte(email, '@'); idx > 0 {
		return email[:idx]
	}
	return email
}

func sanitizeID(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevUnderscore := true
	for _, ch := range strings.ToLower(s) {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') {
			b.WriteRune(ch)
			prevUnderscore = false
		} else if !prevUnderscore {
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	res := strings.TrimSuffix(b.String(), "_")
	if res == "" {
		return "item"
	}
	return res
}

func addUniqueBadge(n *skeletonNode, b manifest.ComponentBadge) {
	for _, ex := range n.Badges {
		if ex.Kind == b.Kind && ex.Label == b.Label {
			return
		}
	}
	n.Badges = append(n.Badges, b)
}

func appendUniqueStr(dst *[]string, val string) {
	if val == "" || slices.Contains(*dst, val) {
		return
	}
	*dst = append(*dst, val)
}
