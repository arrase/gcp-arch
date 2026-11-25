package synth

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/arrase/gcp-arch/internal/extract"
)

// InternTable provides deterministic bidirectional short-ID mapping for FactResource (r_0001..)
// and FactEdge (e_0001..) records without mutating the source FactsFile.
type InternTable struct {
	resIDs           []string
	resByID          map[string]extract.FactResource
	resIDByURI       map[string]string
	edgeIDs          []string
	edgeByID         map[string]extract.FactEdge
	edgeIDByFactEdge map[string]string
}

// AdjacencyEntry represents one deduplicated directed short-ID edge for Pass 1 planning.
type AdjacencyEntry struct {
	To   string               `json:"to"`
	Kind extract.FactEdgeKind `json:"kind"`
}

// NewInternTable sorts copies of facts.Resources and facts.Edges and assigns deterministic
// r_0001..r_NNNN and e_0001..e_MMMM handles.
func NewInternTable(facts *extract.FactsFile) *InternTable {
	t := &InternTable{
		resByID:          make(map[string]extract.FactResource, len(facts.Resources)),
		resIDByURI:       make(map[string]string, len(facts.Resources)),
		edgeByID:         make(map[string]extract.FactEdge, len(facts.Edges)),
		edgeIDByFactEdge: make(map[string]string, len(facts.Edges)),
	}

	resources := slices.Clone(facts.Resources)
	slices.SortFunc(resources, func(a, b extract.FactResource) int {
		return cmp.Or(
			cmp.Compare(a.URI, b.URI),
			cmp.Compare(a.AssetType, b.AssetType),
			cmp.Compare(a.ProjectID, b.ProjectID),
		)
	})
	t.resIDs = make([]string, len(resources))
	for i, r := range resources {
		id := fmt.Sprintf("r_%04d", i+1)
		t.resIDs[i] = id
		t.resByID[id] = r
		t.resIDByURI[r.URI] = id
	}

	edges := slices.Clone(facts.Edges)
	for i := range edges {
		if edges[i].ID == "" {
			edges[i].ID = extract.EdgeID(edges[i].Kind, edges[i].FromURI, edges[i].ToURI, edges[i].Detail)
		}
	}
	slices.SortFunc(edges, func(a, b extract.FactEdge) int {
		return cmp.Or(
			cmp.Compare(a.FromURI, b.FromURI),
			cmp.Compare(a.ToURI, b.ToURI),
			cmp.Compare(a.Kind, b.Kind),
			cmp.Compare(a.Detail, b.Detail),
			cmp.Compare(a.ID, b.ID),
		)
	})
	t.edgeIDs = make([]string, len(edges))
	for i, e := range edges {
		id := fmt.Sprintf("e_%04d", i+1)
		t.edgeIDs[i] = id
		t.edgeByID[id] = e
		t.edgeIDByFactEdge[e.ID] = id
	}

	return t
}

// ResourceByID returns the FactResource associated with a short r_XXXX handle.
func (t *InternTable) ResourceByID(shortID string) (extract.FactResource, bool) {
	r, ok := t.resByID[shortID]
	return r, ok
}

// ResourceID returns the short r_XXXX handle for a canonical resource URI.
func (t *InternTable) ResourceID(uri string) (string, bool) {
	id, ok := t.resIDByURI[uri]
	return id, ok
}

// ResourceIDs returns all interned resource IDs in deterministic sorted order.
func (t *InternTable) ResourceIDs() []string {
	return slices.Clone(t.resIDs)
}

// EdgeByID returns the FactEdge associated with a short e_XXXX handle.
func (t *InternTable) EdgeByID(shortID string) (extract.FactEdge, bool) {
	e, ok := t.edgeByID[shortID]
	return e, ok
}

// EdgeID returns the short e_XXXX handle for a FactEdge.ID hash.
func (t *InternTable) EdgeID(factEdgeID string) (string, bool) {
	id, ok := t.edgeIDByFactEdge[factEdgeID]
	return id, ok
}

// EdgeIDs returns all interned edge IDs in deterministic sorted order.
func (t *InternTable) EdgeIDs() []string {
	return slices.Clone(t.edgeIDs)
}

// BuildAdjacency returns a deduplicated, sorted short-ID adjacency list (r_XXXX -> [{to, kind}])
// covering every FactEdge whose endpoints are present in table.
func BuildAdjacency(facts *extract.FactsFile, table *InternTable) map[string][]AdjacencyEntry {
	type pairKey struct {
		from string
		to   string
		kind extract.FactEdgeKind
	}
	seen := make(map[pairKey]bool, len(facts.Edges))
	adj := make(map[string][]AdjacencyEntry)

	for _, eid := range table.edgeIDs {
		fe := table.edgeByID[eid]
		fromID, okFrom := table.resIDByURI[fe.FromURI]
		toID, okTo := table.resIDByURI[fe.ToURI]
		if !okFrom || !okTo {
			continue
		}
		k := pairKey{from: fromID, to: toID, kind: fe.Kind}
		if seen[k] {
			continue
		}
		seen[k] = true
		adj[fromID] = append(adj[fromID], AdjacencyEntry{To: toID, Kind: fe.Kind})
	}

	for fromID := range adj {
		slices.SortFunc(adj[fromID], func(a, b AdjacencyEntry) int {
			return cmp.Or(cmp.Compare(a.To, b.To), cmp.Compare(a.Kind, b.Kind))
		})
	}
	return adj
}
