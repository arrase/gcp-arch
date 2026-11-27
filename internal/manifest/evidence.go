package manifest

import (
	"cmp"
	"fmt"
	"maps"
	"slices"

	"github.com/arrase/gcp-arch/internal/extract"
)

// permissionEdgeKinds are FactEdge kinds that prove access or identity, not traffic.
var permissionEdgeKinds = map[extract.FactEdgeKind]bool{
	extract.EdgeIAMResourceBinding: true,
	extract.EdgeWorkloadIdentity:   true,
}

// EvidenceIndex resolves VERIFIED edge evidence against facts.json FactEdges and the manifest's
// resource ownership. A walk may only pass through URIs owned by its two endpoint components or
// by no component/container at all (excluded resources), so it can never shortcut a third
// component or route through a VPC/cluster container.
type EvidenceIndex struct {
	factEdges     map[string]extract.FactEdge
	sortedEdges   []extract.FactEdge
	adjByFrom     map[string][]extract.FactEdge
	owner         map[string]string
	containerURIs map[string]bool
	backed        map[string]map[string]bool
}

// NewEvidenceIndex indexes components, containers, and facts edges for evidence checks.
func NewEvidenceIndex(components []Component, containers []Container, edges []extract.FactEdge) *EvidenceIndex {
	x := &EvidenceIndex{
		factEdges:     make(map[string]extract.FactEdge, len(edges)),
		adjByFrom:     make(map[string][]extract.FactEdge),
		owner:         make(map[string]string),
		containerURIs: make(map[string]bool),
		backed:        make(map[string]map[string]bool, len(components)),
	}
	for _, fe := range edges {
		x.factEdges[fe.ID] = fe
	}
	x.sortedEdges = slices.SortedFunc(maps.Values(x.factEdges), func(a, b extract.FactEdge) int {
		return cmp.Or(cmp.Compare(a.FromURI, b.FromURI), cmp.Compare(a.ToURI, b.ToURI), cmp.Compare(a.ID, b.ID))
	})
	for _, fe := range x.sortedEdges {
		if !fe.Heuristic {
			x.adjByFrom[fe.FromURI] = append(x.adjByFrom[fe.FromURI], fe)
		}
	}
	for _, c := range components {
		set := make(map[string]bool, len(c.BackedBy))
		for _, br := range c.BackedBy {
			set[br.URI] = true
			x.owner[br.URI] = c.ID
		}
		x.backed[c.ID] = set
	}
	for _, ct := range containers {
		for _, br := range ct.BackedBy {
			x.containerURIs[br.URI] = true
		}
	}
	return x
}

// blocked reports whether uri may not appear on an evidence walk between from and to.
func (x *EvidenceIndex) blocked(uri, from, to string) bool {
	if x.containerURIs[uri] {
		return true
	}
	owner, owned := x.owner[uri]
	return owned && owner != from && owner != to
}

// usable reports whether fe may serve as VERIFIED evidence between from and to.
func (x *EvidenceIndex) usable(fe extract.FactEdge, from, to string) bool {
	return !fe.Heuristic && !x.blocked(fe.FromURI, from, to) && !x.blocked(fe.ToURI, from, to)
}

// WalkIssue returns empty strings when e's evidence_edge_ids form a valid VERIFIED walk, or a
// validation error code and human-readable reason otherwise. Non-VERIFIED edges always pass.
func (x *EvidenceIndex) WalkIssue(e Edge) (code, msg string) {
	if e.Provenance != ProvenanceVerified {
		return "", ""
	}
	if len(e.EvidenceEdgeIDs) == 0 {
		return ErrMissingEvidenceEdgeIDs, "VERIFIED edge requires evidence_edge_ids"
	}
	cited := make([]extract.FactEdge, 0, len(e.EvidenceEdgeIDs))
	for _, id := range e.EvidenceEdgeIDs {
		fe, ok := x.factEdges[id]
		if !ok {
			return ErrUnknownEvidenceEdge, fmt.Sprintf("evidence edge ID %q does not exist in facts.json", id)
		}
		if fe.Heuristic {
			return ErrInvalidEdgeProvenance, fmt.Sprintf("evidence edge %q is heuristic (%s) and can only back an INFERRED edge", id, fe.Kind)
		}
		if !x.usable(fe, e.From, e.To) {
			return ErrInvalidEdgeProvenance, fmt.Sprintf("evidence edge %q passes through a resource owned by another component or a container", id)
		}
		cited = append(cited, fe)
	}
	if !isValidDirectedWalk(cited, x.backed[e.From], x.backed[e.To]) {
		return ErrInvalidEdgeProvenance, fmt.Sprintf("evidence_edge_ids %v do not form a connected directed walk from %s.backed_by to %s.backed_by", e.EvidenceEdgeIDs, e.From, e.To)
	}
	if e.FlowKind != FlowAdmin && permissionOnly(cited) {
		return ErrInvalidEdgeProvenance, fmt.Sprintf("evidence %v only proves IAM access or identity, so flow_kind must be ADMIN (got %s)", e.EvidenceEdgeIDs, e.FlowKind)
	}
	return "", ""
}

func permissionOnly(edges []extract.FactEdge) bool {
	for _, fe := range edges {
		if !permissionEdgeKinds[fe.Kind] {
			return false
		}
	}
	return true
}

// ShortestWalk returns the evidence edge IDs of the shortest usable directed walk from component
// from to component to, or nil when none exists.
func (x *EvidenceIndex) ShortestWalk(from, to string) []string {
	type step struct{ fromURI, edgeID string }
	queue := slices.Sorted(maps.Keys(x.backed[from]))
	visited := maps.Clone(x.backed[from])
	parent := make(map[string]step)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, fe := range x.adjByFrom[cur] {
			if !x.usable(fe, from, to) {
				continue
			}
			if x.backed[to][fe.ToURI] {
				path := []string{fe.ID}
				for s, ok := parent[cur]; ok; s, ok = parent[s.fromURI] {
					path = append(path, s.edgeID)
				}
				slices.Reverse(path)
				return path
			}
			if !visited[fe.ToURI] {
				visited[fe.ToURI] = true
				parent[fe.ToURI] = step{fromURI: cur, edgeID: fe.ID}
				queue = append(queue, fe.ToURI)
			}
		}
	}
	return nil
}

// componentPair returns the ordered (from, to) pair of distinct components owning fe's endpoints.
func (x *EvidenceIndex) componentPair(fe extract.FactEdge) ([2]string, bool) {
	from, okFrom := x.owner[fe.FromURI]
	to, okTo := x.owner[fe.ToURI]
	return [2]string{from, to}, okFrom && okTo && from != to
}

// ProjectedPairs returns every ordered (from, to) component pair directly linked by a
// non-heuristic FactEdge.
func (x *EvidenceIndex) ProjectedPairs() map[[2]string]bool {
	pairs := make(map[[2]string]bool)
	for _, fe := range x.sortedEdges {
		if fe.Heuristic {
			continue
		}
		if pair, ok := x.componentPair(fe); ok {
			pairs[pair] = true
		}
	}
	return pairs
}
