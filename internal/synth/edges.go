package synth

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/manifest"
)

type edgePairKey struct {
	from string
	to   string
}

func nodeAt(nodes []skeletonNode, nodeIdxByID map[string]int, id string) *skeletonNode {
	if idx, ok := nodeIdxByID[id]; ok {
		return &nodes[idx]
	}
	return nil
}

func buildSkeletonEdges(
	sortedEdges []extract.FactEdge,
	initialStructural []manifest.StructuralEdge,
	transversalItems []manifest.TransversalItem,
	transversalIdxByURI map[string]int,
	uriToNodeID map[string]string,
	nodeIdxByID map[string]int,
	nodes []skeletonNode,
	cb *containerBuilder,
	resByURI map[string]extract.FactResource,
	resolveRep func(string) string,
) ([]skeletonEdge, []manifest.StructuralEdge) {
	foldedEdgeSet := make(map[string]bool, len(initialStructural))
	for _, se := range initialStructural {
		foldedEdgeSet[se.EdgeID] = true
	}
	structuralOut := slices.Clone(initialStructural)

	edgeAgg := make(map[edgePairKey]*skeletonEdge)
	var edgeOrder []edgePairKey

	for _, e := range sortedEdges {
		if foldedEdgeSet[e.ID] {
			continue
		}
		if handleTransversalFactEdge(e, transversalItems, transversalIdxByURI, uriToNodeID, nodeIdxByID, nodes, cb) {
			continue
		}
		if se, handled := handleSpecialStructuralEdge(e, uriToNodeID, nodeIdxByID, nodes, cb, resByURI, resolveRep); handled {
			structuralOut = append(structuralOut, se)
			continue
		}

		fromNID := uriToNodeID[resolveRep(e.FromURI)]
		toNID := uriToNodeID[resolveRep(e.ToURI)]
		if fromNID != "" && toNID != "" && fromNID == toNID {
			if fn := nodeAt(nodes, nodeIdxByID, fromNID); fn != nil {
				appendUniqueStr(&fn.FoldedEdgeIDs, e.ID)
			}
			structuralOut = append(structuralOut, manifest.StructuralEdge{
				EdgeID: e.ID, Kind: string(e.Kind), FromURI: e.FromURI, ToURI: e.ToURI, Reason: "intra_component_fold", TargetID: fromNID,
			})
			continue
		}
		if fromNID == "" || toNID == "" {
			structuralOut = append(structuralOut, handleUnmappedEndpointEdge(e, fromNID, toNID, nodeIdxByID, nodes, cb, resByURI, resolveRep))
			continue
		}
		if se, isHeuristic := accumulateSkeletonEdge(e, fromNID, toNID, edgeAgg, &edgeOrder, nodeIdxByID, nodes, resByURI); isHeuristic {
			structuralOut = append(structuralOut, se)
		}
	}

	edges := make([]skeletonEdge, 0, len(edgeOrder))
	for i, key := range edgeOrder {
		agg := edgeAgg[key]
		agg.ID = fmt.Sprintf("edge_%03d", i+1)
		if agg.Provenance == manifest.ProvenanceVerified && allKindsPermissionOnly(agg.Kinds) {
			agg.FlowKind = manifest.FlowAdmin
		}
		if agg.Provenance == manifest.ProvenanceInferred && agg.InferenceRationale == "" {
			agg.InferenceRationale = "Heuristic telemetry correlation"
		}
		edges = append(edges, *agg)
	}
	return edges, structuralOut
}

func handleTransversalFactEdge(
	e extract.FactEdge,
	transversalItems []manifest.TransversalItem,
	transversalIdxByURI map[string]int,
	uriToNodeID map[string]string,
	nodeIdxByID map[string]int,
	nodes []skeletonNode,
	cb *containerBuilder,
) bool {
	fromIdx, hasFrom := transversalIdxByURI[e.FromURI]
	toIdx, hasTo := transversalIdxByURI[e.ToURI]
	if !hasFrom && !hasTo {
		return false
	}
	if hasTo {
		attachTransversalEdge(&transversalItems[toIdx], e, e.FromURI, uriToNodeID, nodeIdxByID, nodes)
		if srcCID := cb.containerByBackedURI[e.FromURI]; srcCID != "" {
			if ct, ok := cb.byID[srcCID]; ok {
				appendUniqueStr(&ct.FoldedEdgeIDs, e.ID)
			}
		}
	}
	if hasFrom {
		fromTI := &transversalItems[fromIdx]
		attachTransversalEdge(fromTI, e, e.ToURI, uriToNodeID, nodeIdxByID, nodes)
		if dstCID := cb.containerByBackedURI[e.ToURI]; dstCID != "" {
			if ct, ok := cb.byID[dstCID]; ok {
				appendUniqueStr(&ct.FoldedEdgeIDs, e.ID)
				if fromTI.Category == "NAT_ROUTER" {
					appendUniqueStr(&ct.Badges, "NAT: "+fromTI.DisplayName)
				}
			}
		}
	}
	return true
}

func attachTransversalEdge(
	ti *manifest.TransversalItem,
	e extract.FactEdge,
	otherURI string,
	uriToNodeID map[string]string,
	nodeIdxByID map[string]int,
	nodes []skeletonNode,
) {
	appendUniqueStr(&ti.BackedEdgeIDs, e.ID)
	if strings.HasPrefix(e.Detail, "roles/") {
		appendUniqueStr(&ti.Roles, e.Detail)
	}
	nid, ok := uriToNodeID[otherURI]
	if !ok {
		return
	}
	appendUniqueStr(&ti.AttachedNodeIDs, nid)
	cn := nodeAt(nodes, nodeIdxByID, nid)
	if cn == nil {
		return
	}
	if badge, hasBadge := transversalBadge(ti); hasBadge {
		addUniqueBadge(cn, badge)
	}
}

func transversalBadge(ti *manifest.TransversalItem) (manifest.ComponentBadge, bool) {
	switch ti.Category {
	case "IAM_SA":
		return manifest.ComponentBadge{Kind: "sa", Label: "SA: " + shortSA(ti.DisplayName)}, true
	case "KMS":
		return manifest.ComponentBadge{Kind: "kms", Label: "KMS: " + ti.DisplayName}, true
	case "SECRET":
		return manifest.ComponentBadge{Kind: "secret", Label: "Secret: " + ti.DisplayName}, true
	case "FIREWALL_ARMOR":
		if ti.AssetType == "compute.googleapis.com/SecurityPolicy" {
			return manifest.ComponentBadge{Kind: "armor", Label: "WAF: " + ti.DisplayName}, true
		}
	case "VPC_ACCESS":
		return manifest.ComponentBadge{Kind: "vpc_conn", Label: "VPC-Conn: " + ti.DisplayName}, true
	case "NAT_ROUTER":
		return manifest.ComponentBadge{Kind: "nat", Label: "Router: " + ti.DisplayName}, true
	case "OBSERVABILITY":
		return manifest.ComponentBadge{Kind: "log", Label: "Obs: " + ti.DisplayName}, true
	}
	return manifest.ComponentBadge{}, false
}

func handleSpecialStructuralEdge(
	e extract.FactEdge,
	uriToNodeID map[string]string,
	nodeIdxByID map[string]int,
	nodes []skeletonNode,
	cb *containerBuilder,
	resByURI map[string]extract.FactResource,
	resolveRep func(string) string,
) (manifest.StructuralEdge, bool) {
	if e.Kind == extract.EdgePSAPeering {
		targetNID := uriToNodeID[resolveRep(e.ToURI)]
		targetCID := ""
		if tn := nodeAt(nodes, nodeIdxByID, targetNID); tn != nil {
			targetCID = tn.ContainerID
			addUniqueBadge(tn, manifest.ComponentBadge{Kind: "psa", Label: "PSA Peering"})
		}
		if ct, ok := cb.byID[targetCID]; ok {
			appendUniqueStr(&ct.FoldedEdgeIDs, e.ID)
			appendUniqueStr(&ct.Badges, "PSA Peering")
		}
		if srcCID := cb.containerByBackedURI[e.FromURI]; srcCID != "" {
			if srcCt, ok := cb.byID[srcCID]; ok {
				appendUniqueStr(&srcCt.FoldedEdgeIDs, e.ID)
			}
		}
		return manifest.StructuralEdge{
			EdgeID: e.ID, Kind: string(e.Kind), FromURI: e.FromURI, ToURI: e.ToURI,
			Reason: "psa_container_boundary", TargetID: cmp.Or(targetCID, targetNID),
		}, true
	}
	if e.Kind != extract.EdgeVPCPeering {
		return manifest.StructuralEdge{}, false
	}
	fromR, toR := resByURI[e.FromURI], resByURI[e.ToURI]
	srcCID, dstCID := cb.containerByBackedURI[e.FromURI], cb.containerByBackedURI[e.ToURI]
	fromNID, targetNID := uriToNodeID[resolveRep(e.FromURI)], uriToNodeID[resolveRep(e.ToURI)]
	if tn := nodeAt(nodes, nodeIdxByID, targetNID); tn != nil {
		addUniqueBadge(tn, manifest.ComponentBadge{Kind: "peering", Label: "VPC Peering"})
	}
	if fn := nodeAt(nodes, nodeIdxByID, fromNID); fn != nil {
		addUniqueBadge(fn, manifest.ComponentBadge{Kind: "peering", Label: "VPC Peering"})
	}
	attachVPCPeeringContainer(cb, srcCID, toR.DisplayName, e.ID)
	attachVPCPeeringContainer(cb, dstCID, fromR.DisplayName, e.ID)
	reason := "vpc_peering"
	targetID := cmp.Or(targetNID, dstCID, srcCID)
	if srcCID != "" && dstCID != "" && targetNID == "" {
		reason = "vpc_peering_container_link"
		targetID = dstCID
	}
	return manifest.StructuralEdge{
		EdgeID: e.ID, Kind: string(e.Kind), FromURI: e.FromURI, ToURI: e.ToURI,
		Reason: reason, TargetID: targetID,
	}, true
}

func attachVPCPeeringContainer(cb *containerBuilder, cid, peerName, edgeID string) {
	if ct, ok := cb.byID[cid]; ok {
		appendUniqueStr(&ct.FoldedEdgeIDs, edgeID)
		if peerName != "" {
			appendUniqueStr(&ct.Badges, "Peering: "+peerName)
		} else {
			appendUniqueStr(&ct.Badges, "VPC Peering")
		}
	}
}

func handleUnmappedEndpointEdge(
	e extract.FactEdge,
	fromNID, toNID string,
	nodeIdxByID map[string]int,
	nodes []skeletonNode,
	cb *containerBuilder,
	resByURI map[string]extract.FactResource,
	resolveRep func(string) string,
) manifest.StructuralEdge {
	fromCID := cmp.Or(cb.containerByBackedURI[resolveRep(e.FromURI)], cb.containerByBackedURI[e.FromURI])
	toCID := cmp.Or(cb.containerByBackedURI[resolveRep(e.ToURI)], cb.containerByBackedURI[e.ToURI])
	attachContainerEndpointEdge(cb, e, resByURI[e.FromURI], resByURI[e.ToURI], nodeAt(nodes, nodeIdxByID, fromNID), nodeAt(nodes, nodeIdxByID, toNID), fromCID, toCID)
	reason := "structural_link"
	if fromCID != "" || toCID != "" {
		reason = "container_attachment"
	}
	return manifest.StructuralEdge{
		EdgeID: e.ID, Kind: string(e.Kind), FromURI: e.FromURI, ToURI: e.ToURI,
		Reason: reason, TargetID: cmp.Or(toCID, fromCID, toNID, fromNID),
	}
}

func attachContainerEndpointEdge(
	cb *containerBuilder,
	e extract.FactEdge,
	fromR, toR extract.FactResource,
	fromNode, toNode *skeletonNode,
	fromCID, toCID string,
) {
	det := strings.ToLower(e.Detail)
	attachToContainer := func(cid string, peerR extract.FactResource) {
		ct, ok := cb.byID[cid]
		if !ok {
			return
		}
		appendUniqueStr(&ct.FoldedEdgeIDs, e.ID)
		switch {
		case det == "private_zone":
			appendUniqueStr(&ct.Badges, "DNS: "+peerR.DisplayName)
		case det == "psc_network":
			appendUniqueStr(&ct.Badges, "PSC Network")
		case det == "ncc_spoke":
			appendUniqueStr(&ct.Badges, "NCC Spoke")
		case det == "mgmt_transit":
			appendUniqueStr(&ct.Badges, "Mgmt Transit: "+peerR.DisplayName)
		case peerR.DisplayName != "":
			appendUniqueStr(&ct.Badges, shortAssetType(peerR.AssetType)+": "+peerR.DisplayName)
		}
	}
	if fromCID != "" {
		attachToContainer(fromCID, toR)
	}
	if toCID != "" {
		attachToContainer(toCID, fromR)
	}

	attachToNode := func(n *skeletonNode, contR extract.FactResource) {
		if n == nil {
			return
		}
		switch det {
		case "private_zone":
			addUniqueBadge(n, manifest.ComponentBadge{Kind: "dns", Label: "DNS: " + contR.DisplayName})
		case "psc_network":
			addUniqueBadge(n, manifest.ComponentBadge{Kind: "psc", Label: "PSC: " + contR.DisplayName})
		case "ncc_spoke":
			addUniqueBadge(n, manifest.ComponentBadge{Kind: "ncc", Label: "NCC Spoke"})
		case "mgmt_transit":
			addUniqueBadge(n, manifest.ComponentBadge{Kind: "transit", Label: "Transit: " + contR.DisplayName})
		}
	}
	if fromNode != nil && toCID != "" {
		attachToNode(fromNode, toR)
	}
	if toNode != nil && fromCID != "" {
		attachToNode(toNode, fromR)
	}
}

func accumulateSkeletonEdge(
	e extract.FactEdge,
	fromNID, toNID string,
	edgeAgg map[edgePairKey]*skeletonEdge,
	edgeOrder *[]edgePairKey,
	nodeIdxByID map[string]int,
	nodes []skeletonNode,
	resByURI map[string]extract.FactResource,
) (manifest.StructuralEdge, bool) {
	key := edgePairKey{from: fromNID, to: toNID}
	agg, exists := edgeAgg[key]
	fromR, toR := resByURI[e.FromURI], resByURI[e.ToURI]
	retryDLQ := isRetryOrDLQEdge(e, fromR, toR)
	if !exists {
		verb, semTag, flowKind := defaultEdgeAnnotation(e, nodeAt(nodes, nodeIdxByID, fromNID), nodeAt(nodes, nodeIdxByID, toNID), retryDLQ)
		prov := manifest.ProvenanceVerified
		rationale := ""
		if e.Heuristic {
			prov = manifest.ProvenanceInferred
			rationale = buildInferenceRationale(e)
		}
		agg = &skeletonEdge{
			FromNodeID: fromNID, ToNodeID: toNID, Verb: verb, FlowKind: flowKind, SemanticTag: semTag,
			Provenance: prov, InferenceRationale: rationale, IsRetryOrDLQ: retryDLQ,
		}
		edgeAgg[key] = agg
		*edgeOrder = append(*edgeOrder, key)
	} else {
		if !e.Heuristic && agg.Provenance == manifest.ProvenanceInferred {
			agg.Provenance = manifest.ProvenanceVerified
			agg.InferenceRationale = ""
		}
		if retryDLQ {
			agg.IsRetryOrDLQ = true
			if strings.Contains(strings.ToLower(e.Detail), "dead_letter") || strings.Contains(strings.ToLower(toR.DisplayName), "dlq") {
				agg.SemanticTag = "dlq"
			} else if agg.SemanticTag != "dlq" {
				agg.SemanticTag = "retry"
			}
		}
	}
	if !slices.Contains(agg.Kinds, e.Kind) {
		agg.Kinds = append(agg.Kinds, e.Kind)
	}
	if e.Detail != "" && !slices.Contains(agg.Details, e.Detail) {
		agg.Details = append(agg.Details, e.Detail)
	}
	if !e.Heuristic {
		agg.EvidenceEdgeIDs = append(agg.EvidenceEdgeIDs, e.ID)
		return manifest.StructuralEdge{}, false
	}
	return manifest.StructuralEdge{
		EdgeID: e.ID, Kind: string(e.Kind), FromURI: e.FromURI, ToURI: e.ToURI, Reason: "heuristic_correlation", TargetID: toNID,
	}, true
}

// connectOrphanSkeletonNodes adds evidence-backed INFERRED edges for container-bridged
// resources (VPC peering hybrid gateways, Private DNS zones, NCC hub spokes, and shared-SA
// build staging buckets) so zero primary nodes float disconnected.
func connectOrphanSkeletonNodes(
	nodes []skeletonNode,
	edges []skeletonEdge,
	sortedEdges []extract.FactEdge,
	transversalItems []manifest.TransversalItem,
	uriToNodeID map[string]string,
	nodeIdxByID map[string]int,
	cb *containerBuilder,
	resByURI map[string]extract.FactResource,
	resolveRep func(string) string,
) []skeletonEdge {
	adder := newOrphanEdgeAdder(nodes, edges, uriToNodeID, nodeIdxByID, cb, resByURI, resolveRep)
	adder.connectContainerBridgedEdges(sortedEdges)
	adder.connectSharedSAOrphans(nodes, transversalItems)
	return adder.edges
}

type orphanEdgeAdder struct {
	edges       []skeletonEdge
	pairSeen    map[edgePairKey]bool
	deg         map[string]int
	uriToNodeID map[string]string
	nodeIdxByID map[string]int
	nodes       []skeletonNode
	cb          *containerBuilder
	resByURI    map[string]extract.FactResource
	resolveRep  func(string) string
	nodesInCont map[string][]skeletonNode
}

func newOrphanEdgeAdder(
	nodes []skeletonNode,
	edges []skeletonEdge,
	uriToNodeID map[string]string,
	nodeIdxByID map[string]int,
	cb *containerBuilder,
	resByURI map[string]extract.FactResource,
	resolveRep func(string) string,
) *orphanEdgeAdder {
	pairSeen := make(map[edgePairKey]bool, len(edges))
	deg := make(map[string]int, len(nodes))
	for _, e := range edges {
		pairSeen[edgePairKey{from: e.FromNodeID, to: e.ToNodeID}] = true
		deg[e.FromNodeID]++
		deg[e.ToNodeID]++
	}
	return &orphanEdgeAdder{
		edges:       edges,
		pairSeen:    pairSeen,
		deg:         deg,
		uriToNodeID: uriToNodeID,
		nodeIdxByID: nodeIdxByID,
		nodes:       nodes,
		cb:          cb,
		resByURI:    resByURI,
		resolveRep:  resolveRep,
		nodesInCont: indexNodesUnderContainers(nodes, cb),
	}
}

func (a *orphanEdgeAdder) addInferred(fromID, toID, verb, semTag string, fk manifest.FlowKind, rationale string) {
	if fromID == "" || toID == "" || fromID == toID {
		return
	}
	k := edgePairKey{from: fromID, to: toID}
	if a.pairSeen[k] {
		return
	}
	a.pairSeen[k] = true
	a.deg[fromID]++
	a.deg[toID]++
	a.edges = append(a.edges, skeletonEdge{
		ID:                 fmt.Sprintf("edge_%03d", len(a.edges)+1),
		FromNodeID:         fromID,
		ToNodeID:           toID,
		Verb:               clampVerb(verb, 14),
		FlowKind:           fk,
		SemanticTag:        semTag,
		Provenance:         manifest.ProvenanceInferred,
		InferenceRationale: rationale,
	})
}

func (a *orphanEdgeAdder) connectContainerBridgedEdges(sortedEdges []extract.FactEdge) {
	for _, e := range sortedEdges {
		det := strings.ToLower(e.Detail)
		switch {
		case e.Kind == extract.EdgeConfigRef && det == "ncc_spoke":
			a.connectNCCSpoke(e)
		case e.Kind == extract.EdgeVPCPeering:
			a.connectVPCPeeringGateways(e)
		case e.Kind == extract.EdgeConfigRef && det == "private_zone":
			a.connectPrivateDNSZone(e)
		}
	}
}

func (a *orphanEdgeAdder) connectNCCSpoke(e extract.FactEdge) {
	fromNID := a.uriToNodeID[a.resolveRep(e.FromURI)]
	toCID := a.cb.containerByBackedURI[e.ToURI]
	if fromNID == "" || toCID == "" {
		return
	}
	if rep := pickRepresentativeNode(a.nodesInCont[toCID], fromNID); rep != nil {
		a.addInferred(fromNID, rep.ID, "NCC spoke", "peering", manifest.FlowRuntime,
			fmt.Sprintf("NCC hub spoke attachment to VPC %s", a.resByURI[e.ToURI].DisplayName))
	}
}

func (a *orphanEdgeAdder) connectVPCPeeringGateways(e extract.FactEdge) {
	fromCID := a.cb.containerByBackedURI[e.FromURI]
	toNID := a.uriToNodeID[a.resolveRep(e.ToURI)]
	if fromCID == "" || toNID == "" {
		return
	}
	for _, hn := range gatewayNodesInContainer(a.nodesInCont[fromCID]) {
		verb := "Hybrid VPN"
		if hn.AssetType == "compute.googleapis.com/InterconnectAttachment" {
			verb = "Interconnect"
		}
		a.addInferred(hn.ID, toNID, verb, "hybrid", manifest.FlowRuntime,
			fmt.Sprintf("Hybrid gateway %s in peered VPC %s (%s)", hn.Title, a.resByURI[e.FromURI].DisplayName, cmp.Or(e.Detail, "vpc_peering")))
	}
}

func (a *orphanEdgeAdder) connectPrivateDNSZone(e extract.FactEdge) {
	zoneNID := a.uriToNodeID[a.resolveRep(e.FromURI)]
	vpcCID := a.cb.containerByBackedURI[e.ToURI]
	if zoneNID == "" || vpcCID == "" || a.deg[zoneNID] > 0 {
		return
	}
	rep := pickRepresentativeNode(a.nodesInCont[vpcCID], zoneNID)
	if rep == nil {
		return
	}
	zoneNode := nodeAt(a.nodes, a.nodeIdxByID, zoneNID)
	if zoneNode == nil {
		return
	}
	fromID, toID := zoneNID, rep.ID
	if rep.TopoRank < zoneNode.TopoRank {
		fromID, toID = rep.ID, zoneNID
	}
	a.addInferred(fromID, toID, "Private DNS", "traffic", manifest.FlowRuntime,
		fmt.Sprintf("Private DNS zone %s attached to VPC %s", zoneNode.Title, a.resByURI[e.ToURI].DisplayName))
}

func (a *orphanEdgeAdder) connectSharedSAOrphans(nodes []skeletonNode, transversalItems []manifest.TransversalItem) {
	tiByNode := make(map[string][]manifest.TransversalItem)
	for _, ti := range transversalItems {
		for _, nid := range ti.AttachedNodeIDs {
			tiByNode[nid] = append(tiByNode[nid], ti)
		}
	}
	for _, n := range nodes {
		if a.deg[n.ID] > 0 {
			continue
		}
		for _, ti := range tiByNode[n.ID] {
			for _, peerID := range ti.AttachedNodeIDs {
				peer := nodeAt(a.nodes, a.nodeIdxByID, peerID)
				if peerID == n.ID || peer == nil {
					continue
				}
				fk, tag, verb := manifest.FlowRuntime, "traffic", "Shared SA"
				if peer.RoleTag == "cicd" {
					fk, tag, verb = manifest.FlowCICD, "cicd", "Build staging"
				}
				a.addInferred(peerID, n.ID, verb, tag, fk,
					fmt.Sprintf("Shared service account %s connects %s to %s", ti.DisplayName, peer.Title, n.Title))
			}
		}
	}
}

func indexNodesUnderContainers(nodes []skeletonNode, cb *containerBuilder) map[string][]skeletonNode {
	byCont := make(map[string][]skeletonNode)
	for _, n := range nodes {
		cur := n.ContainerID
		seen := make(map[string]bool)
		for cur != "" && !seen[cur] {
			seen[cur] = true
			byCont[cur] = append(byCont[cur], n)
			if ct, ok := cb.byID[cur]; ok {
				cur = ct.ParentContainerID
			} else {
				break
			}
		}
	}
	for cid := range byCont {
		slices.SortFunc(byCont[cid], func(a, b skeletonNode) int {
			return cmp.Or(cmp.Compare(a.TopoRank, b.TopoRank), cmp.Compare(a.ID, b.ID))
		})
	}
	return byCont
}

func pickRepresentativeNode(candidates []skeletonNode, excludeID string) *skeletonNode {
	var fallback *skeletonNode
	for i := range candidates {
		c := &candidates[i]
		if c.ID == excludeID || c.AssetType == "dns.googleapis.com/ManagedZone" {
			continue
		}
		if fallback == nil {
			fallback = c
		}
		if c.ComponentKind != manifest.KindDatastore && c.ComponentKind != manifest.KindExternalTarget {
			return c
		}
	}
	return fallback
}

func gatewayNodesInContainer(candidates []skeletonNode) []skeletonNode {
	var out []skeletonNode
	for _, c := range candidates {
		switch c.AssetType {
		case "compute.googleapis.com/VpnGateway",
			"compute.googleapis.com/VpnTunnel",
			"compute.googleapis.com/InterconnectAttachment":
			out = append(out, c)
		}
	}
	return out
}

func isRetryOrDLQEdge(e extract.FactEdge, fromR, toR extract.FactResource) bool {
	det := strings.ToLower(e.Detail)
	if strings.Contains(det, "dead_letter") || strings.Contains(det, "retry") {
		return true
	}
	toLower := strings.ToLower(toR.DisplayName)
	fromLower := strings.ToLower(fromR.DisplayName)
	if (toR.AssetType == "pubsub.googleapis.com/Topic" || toR.AssetType == "pubsub.googleapis.com/Subscription") &&
		(strings.Contains(toLower, "dlq") || strings.Contains(toLower, "dead-letter")) {
		return true
	}
	if (strings.Contains(fromLower, "dlq") || strings.Contains(fromLower, "replay")) &&
		(strings.Contains(det, "republish") || (toR.AssetType == "pubsub.googleapis.com/Topic" && strings.Contains(det, "pubsub.publish"))) {
		return true
	}
	return false
}

func defaultEdgeAnnotation(e extract.FactEdge, fromN, toN *skeletonNode, retryDLQ bool) (string, string, manifest.FlowKind) {
	if retryDLQ {
		if strings.Contains(strings.ToLower(e.Detail), "dead_letter") || (toN != nil && strings.Contains(strings.ToLower(toN.Title), "dlq")) {
			return "Dead-letter", "dlq", manifest.FlowRuntime
		}
		return "Retry publish", "retry", manifest.FlowRuntime
	}
	switch e.Kind {
	case extract.EdgeCloudBuildRepo:
		return "Git push", "cicd", manifest.FlowCICD
	case extract.EdgeCloudBuildStepRef:
		if strings.Contains(e.Detail, "--project=") {
			return "Deploys to", "cicd", manifest.FlowCICD
		}
		return "Pushes image", "cicd", manifest.FlowCICD
	case extract.EdgeK8sIngressBackend, extract.EdgeK8sNEGBackend:
		return "Routes HTTP", "ingress", manifest.FlowRuntime
	case extract.EdgeBQLineage:
		return "ETL lineage", "data", manifest.FlowRuntime
	case extract.EdgePSAPeering, extract.EdgeVPCPeering:
		return "VPC peering", "peering", manifest.FlowRuntime
	case extract.EdgeMonitoringTraffic:
		return "Invokes", "traffic", manifest.FlowRuntime
	}
	det := strings.ToLower(e.Detail)
	switch {
	case strings.Contains(det, "pubsub_publish"):
		return "Publishes", "async", manifest.FlowRuntime
	case strings.Contains(det, "pubsub_subscription"), strings.Contains(det, "pubsub.push"), strings.Contains(det, "pubsub.pull"):
		return "Delivers", "async", manifest.FlowRuntime
	case strings.Contains(det, "datastream"):
		return "CDC stream", "data", manifest.FlowRuntime
	case strings.Contains(det, "vertex"):
		return "Predicts", "data", manifest.FlowRuntime
	case strings.Contains(det, "https:"):
		return "HTTPS :443", "ingress", manifest.FlowRuntime
	case strings.Contains(det, "ipsec"), strings.Contains(det, "dedicated_10g"):
		return "Hybrid link", "hybrid", manifest.FlowRuntime
	case toN != nil && toN.ComponentKind == manifest.KindDatastore:
		return "Reads/Writes", "data", manifest.FlowRuntime
	case fromN != nil && fromN.ComponentKind == manifest.KindExternalCaller:
		return "Ingress", "ingress", manifest.FlowRuntime
	}
	return "Calls", "traffic", manifest.FlowRuntime
}

func allKindsPermissionOnly(kinds []extract.FactEdgeKind) bool {
	if len(kinds) == 0 {
		return false
	}
	for _, k := range kinds {
		if k != extract.EdgeIAMResourceBinding && k != extract.EdgeWorkloadIdentity {
			return false
		}
	}
	return true
}

func buildInferenceRationale(e extract.FactEdge) string {
	if e.Detail != "" {
		return fmt.Sprintf("Heuristic %s correlation (%s)", e.Kind, e.Detail)
	}
	return fmt.Sprintf("Heuristic %s correlation", e.Kind)
}
