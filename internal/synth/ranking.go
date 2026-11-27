package synth

import (
	"cmp"
	"slices"

	"github.com/arrase/gcp-arch/internal/manifest"
)

func computeTopoRanks(nodes []skeletonNode, edges []skeletonEdge) {
	nodeByID := make(map[string]skeletonNode, len(nodes))
	baseRank := make(map[string]int, len(nodes))
	rank := make(map[string]int, len(nodes))
	nodeIDs := make([]string, len(nodes))
	for i, n := range nodes {
		nodeByID[n.ID] = n
		baseRank[n.ID] = n.TopoRank
		rank[n.ID] = n.TopoRank
		nodeIDs[i] = n.ID
	}
	slices.Sort(nodeIDs)

	activeEdges := filterActiveTopoEdges(edges, nodeByID)
	activeEdges = breakTopoCycles(nodeIDs, activeEdges, nodeByID, baseRank)
	propagateLongestTopoRank(nodeIDs, activeEdges, rank)

	for i := range nodes {
		nodes[i].TopoRank = rank[nodes[i].ID]
	}
}

func filterActiveTopoEdges(edges []skeletonEdge, nodeByID map[string]skeletonNode) []skeletonEdge {
	var active []skeletonEdge
	for _, e := range edges {
		if e.IsRetryOrDLQ || e.FromNodeID == e.ToNodeID {
			continue
		}
		if _, okF := nodeByID[e.FromNodeID]; !okF {
			continue
		}
		if _, okT := nodeByID[e.ToNodeID]; !okT {
			continue
		}
		active = append(active, e)
	}
	slices.SortFunc(active, func(a, b skeletonEdge) int {
		return cmp.Or(cmp.Compare(a.FromNodeID, b.FromNodeID), cmp.Compare(a.ToNodeID, b.ToNodeID), cmp.Compare(a.ID, b.ID))
	})
	return active
}

type indexedEdge struct {
	idx  int
	edge skeletonEdge
}

// breakTopoCycles breaks non-DLQ cycles by running Tarjan SCC once over the full graph and
// iteratively breaking feedback edges scoped strictly within each cyclic SCC.
func breakTopoCycles(nodeIDs []string, activeEdges []skeletonEdge, nodeByID map[string]skeletonNode, baseRank map[string]int) []skeletonEdge {
	type subgraph struct {
		nodes []string
		edges []indexedEdge
	}

	initial := make([]indexedEdge, len(activeEdges))
	for i, e := range activeEdges {
		initial[i] = indexedEdge{idx: i, edge: e}
	}

	removed := make([]bool, len(activeEdges))
	anyRemoved := false
	queue := []subgraph{{nodes: nodeIDs, edges: initial}}

	for len(queue) > 0 {
		sub := queue[0]
		queue = queue[1:]

		for _, comp := range tarjanCyclicSCCs(sub.nodes, sub.edges) {
			breakEdgeIdx, remaining := splitSCCFeedbackEdge(comp, sub.edges, nodeByID, baseRank)
			if breakEdgeIdx < 0 {
				continue
			}
			removed[breakEdgeIdx] = true
			anyRemoved = true
			if len(remaining) >= 2 {
				slices.Sort(comp)
				queue = append(queue, subgraph{nodes: comp, edges: remaining})
			}
		}
	}

	if !anyRemoved {
		return activeEdges
	}
	out := make([]skeletonEdge, 0, len(activeEdges))
	for i, e := range activeEdges {
		if !removed[i] {
			out = append(out, e)
		}
	}
	return out
}

func splitSCCFeedbackEdge(
	comp []string,
	edges []indexedEdge,
	nodeByID map[string]skeletonNode,
	baseRank map[string]int,
) (int, []indexedEdge) {
	inComp := make(map[string]bool, len(comp))
	for _, id := range comp {
		inComp[id] = true
	}
	var compEdges []indexedEdge
	breakPos := -1
	for _, ie := range edges {
		if !inComp[ie.edge.FromNodeID] || !inComp[ie.edge.ToNodeID] {
			continue
		}
		if breakPos == -1 || betterFeedbackEdge(ie.edge, compEdges[breakPos].edge, nodeByID, baseRank) {
			breakPos = len(compEdges)
		}
		compEdges = append(compEdges, ie)
	}
	if breakPos == -1 {
		return -1, nil
	}
	breakEdgeIdx := compEdges[breakPos].idx
	return breakEdgeIdx, slices.Delete(compEdges, breakPos, breakPos+1)
}

func tarjanCyclicSCCs(nodeIDs []string, edges []indexedEdge) [][]string {
	adj := make(map[string][]string, len(nodeIDs))
	for _, ie := range edges {
		adj[ie.edge.FromNodeID] = append(adj[ie.edge.FromNodeID], ie.edge.ToNodeID)
	}
	index := 0
	indices := make(map[string]int, len(nodeIDs))
	lowlink := make(map[string]int, len(nodeIDs))
	onStack := make(map[string]bool, len(nodeIDs))
	var stack []string
	var cyclic [][]string

	var strongConnect func(v string)
	strongConnect = func(v string) {
		index++
		indices[v] = index
		lowlink[v] = index
		stack = append(stack, v)
		onStack[v] = true

		for _, w := range adj[v] {
			if indices[w] == 0 {
				strongConnect(w)
				if lowlink[w] < lowlink[v] {
					lowlink[v] = lowlink[w]
				}
			} else if onStack[w] && indices[w] < lowlink[v] {
				lowlink[v] = indices[w]
			}
		}

		if lowlink[v] == indices[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			if len(comp) > 1 {
				cyclic = append(cyclic, comp)
			}
		}
	}

	for _, id := range nodeIDs {
		if indices[id] == 0 {
			strongConnect(id)
		}
	}
	return cyclic
}

func betterFeedbackEdge(cand, best skeletonEdge, nodeByID map[string]skeletonNode, baseRank map[string]int) bool {
	score := func(e skeletonEdge) (isIngressReadBack int, isPushCallback int, isBackward int, baseDiff int) {
		fn, tn := nodeByID[e.FromNodeID], nodeByID[e.ToNodeID]
		if tn.ComponentKind == manifest.KindDatastore && baseRank[e.ToNodeID] <= 1 {
			isIngressReadBack = 1
		}
		if fn.ComponentKind == manifest.KindMessaging && tn.ComponentKind != manifest.KindMessaging && tn.ComponentKind != manifest.KindDatastore {
			isPushCallback = 1
		}
		diff := baseRank[e.FromNodeID] - baseRank[e.ToNodeID]
		if diff >= 0 {
			isBackward = 1
		}
		return isIngressReadBack, isPushCallback, isBackward, diff
	}
	cr, cp, cb, cd := score(cand)
	br, bp, bb, bd := score(best)
	if cr != br {
		return cr > br
	}
	if cp != bp {
		return cp > bp
	}
	if cb != bb {
		return cb > bb
	}
	if cd != bd {
		return cd > bd
	}
	return cmp.Or(cmp.Compare(cand.ID, best.ID), cmp.Compare(cand.FromNodeID, best.FromNodeID), cmp.Compare(cand.ToNodeID, best.ToNodeID)) < 0
}

func propagateLongestTopoRank(nodeIDs []string, activeEdges []skeletonEdge, rank map[string]int) {
	adj := make(map[string][]string, len(nodeIDs))
	inDeg := make(map[string]int, len(nodeIDs))
	for _, e := range activeEdges {
		adj[e.FromNodeID] = append(adj[e.FromNodeID], e.ToNodeID)
		inDeg[e.ToNodeID]++
	}
	var queue []string
	for _, id := range nodeIDs {
		if inDeg[id] == 0 {
			queue = append(queue, id)
		}
	}
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		for _, v := range adj[u] {
			if rank[u]+1 > rank[v] {
				rank[v] = rank[u] + 1
			}
			inDeg[v]--
			if inDeg[v] == 0 {
				queue = append(queue, v)
			}
		}
	}
}

func refineSubRanks(nodes []skeletonNode, edges []skeletonEdge) {
	slices.SortFunc(nodes, func(a, b skeletonNode) int {
		return cmp.Or(
			cmp.Compare(a.TopoRank, b.TopoRank),
			cmp.Compare(a.ContainerID, b.ContainerID),
			cmp.Compare(a.Title, b.Title),
			cmp.Compare(a.ID, b.ID),
		)
	})
	idxByID := assignSequentialSubRanks(nodes)
	out, in := buildTopoAdjacency(edges)

	for range 3 {
		scores := computeBarycenterScores(nodes, idxByID, out, in)
		slices.SortStableFunc(nodes, func(a, b skeletonNode) int {
			return cmp.Or(
				cmp.Compare(a.TopoRank, b.TopoRank),
				cmp.Compare(a.ContainerID, b.ContainerID),
				cmp.Compare(scores[a.ID], scores[b.ID]),
				cmp.Compare(a.Title, b.Title),
				cmp.Compare(a.ID, b.ID),
			)
		})
		idxByID = assignSequentialSubRanks(nodes)
	}
}

func assignSequentialSubRanks(nodes []skeletonNode) map[string]int {
	idxByID := make(map[string]int, len(nodes))
	for i := range nodes {
		nodes[i].SubRank = i
		idxByID[nodes[i].ID] = i
	}
	return idxByID
}

func buildTopoAdjacency(edges []skeletonEdge) (map[string][]string, map[string][]string) {
	out := make(map[string][]string)
	in := make(map[string][]string)
	for _, e := range edges {
		if e.IsRetryOrDLQ {
			continue
		}
		out[e.FromNodeID] = append(out[e.FromNodeID], e.ToNodeID)
		in[e.ToNodeID] = append(in[e.ToNodeID], e.FromNodeID)
	}
	return out, in
}

func computeBarycenterScores(nodes []skeletonNode, idxByID map[string]int, out, in map[string][]string) map[string]float64 {
	scores := make(map[string]float64, len(nodes))
	for _, n := range nodes {
		var sum, cnt float64
		for _, pid := range in[n.ID] {
			if pi, ok := idxByID[pid]; ok {
				sum += float64(nodes[pi].SubRank)
				cnt++
			}
		}
		for _, cid := range out[n.ID] {
			if ci, ok := idxByID[cid]; ok {
				sum += float64(nodes[ci].SubRank)
				cnt++
			}
		}
		if cnt > 0 {
			scores[n.ID] = sum / cnt
		} else {
			scores[n.ID] = float64(n.SubRank)
		}
	}
	return scores
}
