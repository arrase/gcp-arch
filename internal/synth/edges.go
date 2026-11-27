package synth

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/manifest"
)

const labelVPCPeering = "VPC Peering"

type edgePairKey struct {
	from string
	to   string
}

type edgeBuilder struct {
	transversalItems    []manifest.TransversalItem
	transversalIdxByURI map[string]int
	uriToNodeID         map[string]string
	nodeIdxByID         map[string]int
	nodes               []skeletonNode
	cb                  *containerBuilder
	resByURI            map[string]extract.FactResource
	resolveRep          func(string) string
}

func (eb *edgeBuilder) nodeAt(id string) *skeletonNode {
	if idx, ok := eb.nodeIdxByID[id]; ok {
		return &eb.nodes[idx]
	}
	return nil
}

func (eb *edgeBuilder) buildSkeletonEdges(
	sortedEdges []extract.FactEdge,
	initialStructural []manifest.StructuralEdge,
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
		if se, hasStructural := eb.processFactEdge(e, edgeAgg, &edgeOrder); hasStructural {
			structuralOut = append(structuralOut, se)
		}
	}

	return finalizeSkeletonEdges(edgeAgg, edgeOrder), structuralOut
}

func (eb *edgeBuilder) processFactEdge(
	e extract.FactEdge,
	edgeAgg map[edgePairKey]*skeletonEdge,
	edgeOrder *[]edgePairKey,
) (manifest.StructuralEdge, bool) {
	if eb.handleTransversalFactEdge(e) {
		return manifest.StructuralEdge{}, false
	}
	if se, handled := eb.handleSpecialStructuralEdge(e); handled {
		return se, true
	}

	fromNID := eb.uriToNodeID[eb.resolveRep(e.FromURI)]
	toNID := eb.uriToNodeID[eb.resolveRep(e.ToURI)]
	if fromNID != "" && fromNID == toNID {
		if fn := eb.nodeAt(fromNID); fn != nil {
			appendUniqueStr(&fn.FoldedEdgeIDs, e.ID)
		}
		return manifest.StructuralEdge{
			EdgeID: e.ID, Kind: string(e.Kind), FromURI: e.FromURI, ToURI: e.ToURI,
			Reason: "intra_component_fold", TargetID: fromNID,
		}, true
	}
	if fromNID == "" || toNID == "" {
		return eb.handleUnmappedEndpointEdge(e, fromNID, toNID), true
	}
	return eb.accumulateSkeletonEdge(e, fromNID, toNID, edgeAgg, edgeOrder)
}

func finalizeSkeletonEdges(edgeAgg map[edgePairKey]*skeletonEdge, edgeOrder []edgePairKey) []skeletonEdge {
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
	return edges
}

func (eb *edgeBuilder) handleTransversalFactEdge(e extract.FactEdge) bool {
	fromIdx, hasFrom := eb.transversalIdxByURI[e.FromURI]
	toIdx, hasTo := eb.transversalIdxByURI[e.ToURI]
	if !hasFrom && !hasTo {
		return false
	}
	if hasTo {
		eb.attachTransversalEdge(&eb.transversalItems[toIdx], e, e.FromURI)
		eb.foldContainerEdge(e.FromURI, e.ID, "")
	}
	if hasFrom {
		fromTI := &eb.transversalItems[fromIdx]
		eb.attachTransversalEdge(fromTI, e, e.ToURI)
		natBadge := ""
		if fromTI.Category == "NAT_ROUTER" {
			natBadge = "NAT: " + fromTI.DisplayName
		}
		eb.foldContainerEdge(e.ToURI, e.ID, natBadge)
	}
	return true
}

func (eb *edgeBuilder) foldContainerEdge(uri, edgeID, badge string) {
	if cid := eb.cb.containerByBackedURI[uri]; cid != "" {
		if ct, ok := eb.cb.byID[cid]; ok {
			appendUniqueStr(&ct.FoldedEdgeIDs, edgeID)
			appendUniqueStr(&ct.Badges, badge)
		}
	}
}

func (eb *edgeBuilder) attachTransversalEdge(
	ti *manifest.TransversalItem,
	e extract.FactEdge,
	otherURI string,
) {
	appendUniqueStr(&ti.BackedEdgeIDs, e.ID)
	if strings.HasPrefix(e.Detail, "roles/") {
		appendUniqueStr(&ti.Roles, e.Detail)
	}
	nid, ok := eb.uriToNodeID[otherURI]
	if !ok {
		return
	}
	appendUniqueStr(&ti.AttachedNodeIDs, nid)
	cn := eb.nodeAt(nid)
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

func (eb *edgeBuilder) handleSpecialStructuralEdge(e extract.FactEdge) (manifest.StructuralEdge, bool) {
	if e.Kind == extract.EdgePSAPeering {
		targetNID := eb.uriToNodeID[eb.resolveRep(e.ToURI)]
		targetCID := ""
		if tn := eb.nodeAt(targetNID); tn != nil {
			targetCID = tn.ContainerID
			addUniqueBadge(tn, manifest.ComponentBadge{Kind: "psa", Label: "PSA Peering"})
		}
		if ct, ok := eb.cb.byID[targetCID]; ok {
			appendUniqueStr(&ct.FoldedEdgeIDs, e.ID)
			appendUniqueStr(&ct.Badges, "PSA Peering")
		}
		eb.foldContainerEdge(e.FromURI, e.ID, "")
		return manifest.StructuralEdge{
			EdgeID: e.ID, Kind: string(e.Kind), FromURI: e.FromURI, ToURI: e.ToURI,
			Reason: "psa_container_boundary", TargetID: cmp.Or(targetCID, targetNID),
		}, true
	}
	if e.Kind != extract.EdgeVPCPeering {
		return manifest.StructuralEdge{}, false
	}
	fromR, toR := eb.resByURI[e.FromURI], eb.resByURI[e.ToURI]
	srcCID, dstCID := eb.cb.containerByBackedURI[e.FromURI], eb.cb.containerByBackedURI[e.ToURI]
	fromNID, targetNID := eb.uriToNodeID[eb.resolveRep(e.FromURI)], eb.uriToNodeID[eb.resolveRep(e.ToURI)]
	if tn := eb.nodeAt(targetNID); tn != nil {
		addUniqueBadge(tn, manifest.ComponentBadge{Kind: "peering", Label: labelVPCPeering})
	}
	if fn := eb.nodeAt(fromNID); fn != nil {
		addUniqueBadge(fn, manifest.ComponentBadge{Kind: "peering", Label: labelVPCPeering})
	}
	eb.attachVPCPeeringContainer(srcCID, toR.DisplayName, e.ID)
	eb.attachVPCPeeringContainer(dstCID, fromR.DisplayName, e.ID)
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

func (eb *edgeBuilder) attachVPCPeeringContainer(cid, peerName, edgeID string) {
	if ct, ok := eb.cb.byID[cid]; ok {
		appendUniqueStr(&ct.FoldedEdgeIDs, edgeID)
		if peerName != "" {
			appendUniqueStr(&ct.Badges, "Peering: "+peerName)
		} else {
			appendUniqueStr(&ct.Badges, labelVPCPeering)
		}
	}
}

func (eb *edgeBuilder) handleUnmappedEndpointEdge(
	e extract.FactEdge,
	fromNID, toNID string,
) manifest.StructuralEdge {
	fromCID := cmp.Or(eb.cb.containerByBackedURI[eb.resolveRep(e.FromURI)], eb.cb.containerByBackedURI[e.FromURI])
	toCID := cmp.Or(eb.cb.containerByBackedURI[eb.resolveRep(e.ToURI)], eb.cb.containerByBackedURI[e.ToURI])
	eb.attachContainerEndpointEdge(e, eb.nodeAt(fromNID), eb.nodeAt(toNID), fromCID, toCID)
	reason := "structural_link"
	if fromCID != "" || toCID != "" {
		reason = "container_attachment"
	}
	return manifest.StructuralEdge{
		EdgeID: e.ID, Kind: string(e.Kind), FromURI: e.FromURI, ToURI: e.ToURI,
		Reason: reason, TargetID: cmp.Or(toCID, fromCID, toNID, fromNID),
	}
}

func (eb *edgeBuilder) attachContainerEndpointEdge(
	e extract.FactEdge,
	fromNode, toNode *skeletonNode,
	fromCID, toCID string,
) {
	fromR, toR := eb.resByURI[e.FromURI], eb.resByURI[e.ToURI]
	det := strings.ToLower(e.Detail)
	attachToContainer := func(cid string, peerR extract.FactResource) {
		ct, ok := eb.cb.byID[cid]
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

func (eb *edgeBuilder) accumulateSkeletonEdge(
	e extract.FactEdge,
	fromNID, toNID string,
	edgeAgg map[edgePairKey]*skeletonEdge,
	edgeOrder *[]edgePairKey,
) (manifest.StructuralEdge, bool) {
	key := edgePairKey{from: fromNID, to: toNID}
	agg, exists := edgeAgg[key]
	fromR, toR := eb.resByURI[e.FromURI], eb.resByURI[e.ToURI]
	retryDLQ := isRetryOrDLQEdge(e, fromR, toR)
	if !exists {
		agg = eb.newSkeletonEdge(e, fromNID, toNID, retryDLQ)
		edgeAgg[key] = agg
		*edgeOrder = append(*edgeOrder, key)
	} else {
		mergeSkeletonEdge(agg, e, toR, retryDLQ)
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

func (eb *edgeBuilder) newSkeletonEdge(e extract.FactEdge, fromNID, toNID string, retryDLQ bool) *skeletonEdge {
	verb, semTag, flowKind := defaultEdgeAnnotation(e, eb.nodeAt(fromNID), eb.nodeAt(toNID), retryDLQ)
	prov := manifest.ProvenanceVerified
	rationale := ""
	if e.Heuristic {
		prov = manifest.ProvenanceInferred
		rationale = buildInferenceRationale(e)
	}
	return &skeletonEdge{
		FromNodeID: fromNID, ToNodeID: toNID, Verb: verb, FlowKind: flowKind, SemanticTag: semTag,
		Provenance: prov, InferenceRationale: rationale, IsRetryOrDLQ: retryDLQ,
	}
}

func mergeSkeletonEdge(agg *skeletonEdge, e extract.FactEdge, toR extract.FactResource, retryDLQ bool) {
	if !e.Heuristic && agg.Provenance == manifest.ProvenanceInferred {
		agg.Provenance = manifest.ProvenanceVerified
		agg.InferenceRationale = ""
	}
	if !retryDLQ {
		return
	}
	agg.IsRetryOrDLQ = true
	if strings.Contains(strings.ToLower(e.Detail), "dead_letter") || strings.Contains(strings.ToLower(toR.DisplayName), "dlq") {
		agg.SemanticTag = "dlq"
	} else if agg.SemanticTag != "dlq" {
		agg.SemanticTag = "retry"
	}
}

// connectOrphanSkeletonNodes adds evidence-backed INFERRED edges for container-bridged
// resources (VPC peering hybrid gateways, Private DNS zones, NCC hub spokes, and shared-SA
// build staging buckets) so zero primary nodes float disconnected.
func (eb *edgeBuilder) connectOrphanSkeletonNodes(
	edges []skeletonEdge,
	sortedEdges []extract.FactEdge,
) []skeletonEdge {
	adder := newOrphanEdgeAdder(eb, edges)
	adder.connectContainerBridgedEdges(sortedEdges)
	adder.connectSharedSAOrphans()
	return adder.edges
}

type orphanEdgeAdder struct {
	eb          *edgeBuilder
	edges       []skeletonEdge
	pairSeen    map[edgePairKey]bool
	deg         map[string]int
	nodesInCont map[string][]skeletonNode
}

func newOrphanEdgeAdder(eb *edgeBuilder, edges []skeletonEdge) *orphanEdgeAdder {
	pairSeen := make(map[edgePairKey]bool, len(edges))
	deg := make(map[string]int, len(eb.nodes))
	for _, e := range edges {
		pairSeen[edgePairKey{from: e.FromNodeID, to: e.ToNodeID}] = true
		deg[e.FromNodeID]++
		deg[e.ToNodeID]++
	}
	return &orphanEdgeAdder{
		eb:          eb,
		edges:       edges,
		pairSeen:    pairSeen,
		deg:         deg,
		nodesInCont: indexNodesUnderContainers(eb.nodes, eb.cb),
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
	fromNID := a.eb.uriToNodeID[a.eb.resolveRep(e.FromURI)]
	toCID := a.eb.cb.containerByBackedURI[e.ToURI]
	if fromNID == "" || toCID == "" {
		return
	}
	if rep := pickRepresentativeNode(a.nodesInCont[toCID], fromNID); rep != nil {
		a.addInferred(fromNID, rep.ID, "NCC spoke", "peering", manifest.FlowRuntime,
			fmt.Sprintf("NCC hub spoke attachment to VPC %s", a.eb.resByURI[e.ToURI].DisplayName))
	}
}

func (a *orphanEdgeAdder) connectVPCPeeringGateways(e extract.FactEdge) {
	fromCID := a.eb.cb.containerByBackedURI[e.FromURI]
	toNID := a.eb.uriToNodeID[a.eb.resolveRep(e.ToURI)]
	if fromCID == "" || toNID == "" {
		return
	}
	for _, hn := range gatewayNodesInContainer(a.nodesInCont[fromCID]) {
		verb := "Hybrid VPN"
		if hn.AssetType == "compute.googleapis.com/InterconnectAttachment" {
			verb = "Interconnect"
		}
		a.addInferred(hn.ID, toNID, verb, "hybrid", manifest.FlowRuntime,
			fmt.Sprintf("Hybrid gateway %s in peered VPC %s (%s)", hn.Title, a.eb.resByURI[e.FromURI].DisplayName, cmp.Or(e.Detail, "vpc_peering")))
	}
}

func (a *orphanEdgeAdder) connectPrivateDNSZone(e extract.FactEdge) {
	zoneNID := a.eb.uriToNodeID[a.eb.resolveRep(e.FromURI)]
	vpcCID := a.eb.cb.containerByBackedURI[e.ToURI]
	if zoneNID == "" || vpcCID == "" || a.deg[zoneNID] > 0 {
		return
	}
	rep := pickRepresentativeNode(a.nodesInCont[vpcCID], zoneNID)
	if rep == nil {
		return
	}
	zoneNode := a.eb.nodeAt(zoneNID)
	if zoneNode == nil {
		return
	}
	fromID, toID := zoneNID, rep.ID
	if rep.TopoRank < zoneNode.TopoRank {
		fromID, toID = rep.ID, zoneNID
	}
	a.addInferred(fromID, toID, "Private DNS", "traffic", manifest.FlowRuntime,
		fmt.Sprintf("Private DNS zone %s attached to VPC %s", zoneNode.Title, a.eb.resByURI[e.ToURI].DisplayName))
}

func (a *orphanEdgeAdder) connectSharedSAOrphans() {
	tiByNode := make(map[string][]manifest.TransversalItem)
	for _, ti := range a.eb.transversalItems {
		for _, nid := range ti.AttachedNodeIDs {
			tiByNode[nid] = append(tiByNode[nid], ti)
		}
	}
	for _, n := range a.eb.nodes {
		if a.deg[n.ID] > 0 {
			continue
		}
		a.connectOrphanViaSharedSA(n, tiByNode[n.ID])
	}
}

func (a *orphanEdgeAdder) connectOrphanViaSharedSA(n skeletonNode, items []manifest.TransversalItem) {
	for _, ti := range items {
		for _, peerID := range ti.AttachedNodeIDs {
			peer := a.eb.nodeAt(peerID)
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
