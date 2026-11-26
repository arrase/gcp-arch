package synth

import (
	"cmp"
	"maps"
	"slices"
	"strings"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/manifest"
)

const lostEvidenceRationalePrefix = "Not verifiable from facts.json: "

// finalizeManifest applies every deterministic post-synthesis step shared by Day-1 synthesis and
// Day-2 reconciliation: provenance refresh, cross-view edge harmonization, off-page reference
// derivation, acknowledged-pair bookkeeping, and View 0 projection.
func finalizeManifest(m *manifest.Manifest, facts *extract.FactsFile) {
	idx := manifest.NewEvidenceIndex(m.Components, m.Containers, facts.Edges)
	refreshEdgeProvenance(m, idx)
	harmonizeCrossViewEdges(m)
	deriveOffPageRefs(m, idx)
	m.AcknowledgedEdges = undrawnProjectedPairs(m, idx)
	rebuildSystemIndex(m)
}

// refreshEdgeProvenance re-derives provenance on every detail edge from the current facts:
// a VERIFIED edge whose evidence vanished is re-pointed to the shortest valid walk or downgraded
// to INFERRED (never deleted), and an INFERRED edge that now has a valid walk is upgraded.
func refreshEdgeProvenance(m *manifest.Manifest, idx *manifest.EvidenceIndex) {
	for vi := range m.Views {
		if m.Views[vi].ID == manifest.SystemIndexViewID {
			continue
		}
		for ei := range m.Views[vi].Edges {
			m.Views[vi].Edges[ei] = refreshedEdge(m.Views[vi].Edges[ei], idx)
		}
	}
}

func refreshedEdge(e manifest.Edge, idx *manifest.EvidenceIndex) manifest.Edge {
	if e.Provenance == manifest.ProvenanceVerified {
		if _, msg := idx.WalkIssue(e); msg == "" {
			return e
		}
	} else if !strings.HasPrefix(e.InferenceRationale, lostEvidenceRationalePrefix) {
		return e
	}
	verified := e
	verified.Provenance = manifest.ProvenanceVerified
	verified.EvidenceEdgeIDs = idx.ShortestWalk(e.From, e.To)
	verified.InferenceRationale = ""
	_, msg := idx.WalkIssue(verified)
	if msg == "" {
		return verified
	}
	if e.Provenance == manifest.ProvenanceInferred {
		return e
	}
	e.Provenance = manifest.ProvenanceInferred
	e.EvidenceEdgeIDs = nil
	e.InferenceRationale = lostEvidenceRationalePrefix + msg
	return e
}

type edgeKey struct {
	from, to string
	flowKind manifest.FlowKind
}

// harmonizeCrossViewEdges makes an edge drawn in several detail views read identically everywhere,
// copying verb, provenance, evidence, and rationale from its first occurrence in view-ID order.
func harmonizeCrossViewEdges(m *manifest.Manifest) {
	order := detailViewIndexesByID(m)
	canonical := make(map[edgeKey]manifest.Edge)
	for _, vi := range order {
		for ei, e := range m.Views[vi].Edges {
			k := edgeKey{from: e.From, to: e.To, flowKind: e.FlowKind}
			first, seen := canonical[k]
			if !seen {
				canonical[k] = e
				continue
			}
			e.Verb = first.Verb
			e.Provenance = first.Provenance
			e.EvidenceEdgeIDs = slices.Clone(first.EvidenceEdgeIDs)
			e.InferenceRationale = first.InferenceRationale
			m.Views[vi].Edges[ei] = e
		}
	}
}

func detailViewIndexesByID(m *manifest.Manifest) []int {
	var order []int
	for i, v := range m.Views {
		if v.ID != manifest.SystemIndexViewID {
			order = append(order, i)
		}
	}
	slices.SortFunc(order, func(a, b int) int { return cmp.Compare(m.Views[a].ID, m.Views[b].ID) })
	return order
}

// relatedPairs returns every non-rejected directed component pair that is either drawn in a
// detail view or projected from facts, labelled with the drawn verb or the target's title.
func relatedPairs(m *manifest.Manifest, idx *manifest.EvidenceIndex) map[[2]string]string {
	rejected := rejectedPairSet(m)
	labels := make(map[[2]string]string)
	for _, vi := range detailViewIndexesByID(m) {
		for _, e := range m.Views[vi].Edges {
			pair := [2]string{e.From, e.To}
			if _, seen := labels[pair]; !seen && !rejected[pair] {
				labels[pair] = e.Verb
			}
		}
	}
	titles := make(map[string]string, len(m.Components))
	for _, c := range m.Components {
		titles[c.ID] = c.Title
	}
	for pair := range idx.ProjectedPairs() {
		if _, seen := labels[pair]; !seen && !rejected[pair] {
			labels[pair] = titles[pair[1]]
		}
	}
	return labels
}

// deriveOffPageRefs rebuilds every detail placement's off_page_refs: component c in view V gets an
// OUT (IN) reference to every other detail view W that places a component d not shown in V for a
// related pair (c, d) ((d, c)), so references are symmetric by construction.
func deriveOffPageRefs(m *manifest.Manifest, idx *manifest.EvidenceIndex) {
	labels := relatedPairs(m, idx)
	placedIn := make(map[string][]string)
	order := detailViewIndexesByID(m)
	for _, vi := range order {
		for _, p := range m.Views[vi].Placements {
			placedIn[p.ComponentID] = append(placedIn[p.ComponentID], m.Views[vi].ID)
		}
	}
	pairs := slices.SortedFunc(maps.Keys(labels), func(a, b [2]string) int {
		return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1]))
	})

	for _, vi := range order {
		v := &m.Views[vi]
		inView := make(map[string]bool, len(v.Placements))
		for _, p := range v.Placements {
			inView[p.ComponentID] = true
		}
		for pi := range v.Placements {
			v.Placements[pi].OffPageRefs = offPageRefsFor(v.ID, v.Placements[pi], inView, pairs, labels, placedIn)
		}
	}
}

func offPageRefsFor(viewID string, p manifest.Placement, inView map[string]bool, pairs [][2]string, labels map[[2]string]string, placedIn map[string][]string) []manifest.OffPageRef {
	type refKey struct{ target, direction string }
	curated := make(map[refKey]string, len(p.OffPageRefs))
	for _, r := range p.OffPageRefs {
		if strings.TrimSpace(r.Label) != "" {
			curated[refKey{target: r.TargetViewID, direction: r.Direction}] = r.Label
		}
	}
	seen := make(map[refKey]bool)
	var refs []manifest.OffPageRef
	for _, pair := range pairs {
		other, direction := pair[1], "OUT"
		if pair[1] == p.ComponentID {
			other, direction = pair[0], "IN"
		} else if pair[0] != p.ComponentID {
			continue
		}
		if inView[other] {
			continue
		}
		for _, target := range placedIn[other] {
			k := refKey{target: target, direction: direction}
			if target == viewID || seen[k] {
				continue
			}
			seen[k] = true
			label := curated[k]
			if label == "" {
				label = truncateVerb(labels[pair])
			}
			refs = append(refs, manifest.OffPageRef{TargetViewID: target, Label: label, Direction: direction})
		}
	}
	return refs
}

// undrawnProjectedPairs lists projected component pairs that synthesis saw but left undrawn and
// unrejected; Day-2 reconciliation only asks the model about pairs outside this set.
func undrawnProjectedPairs(m *manifest.Manifest, idx *manifest.EvidenceIndex) []manifest.ComponentPair {
	settled := drawnOrRejectedPairs(m)
	var out []manifest.ComponentPair
	for pair := range idx.ProjectedPairs() {
		if !settled[pair] {
			out = append(out, manifest.ComponentPair{From: pair[0], To: pair[1]})
		}
	}
	slices.SortFunc(out, func(a, b manifest.ComponentPair) int {
		return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.To, b.To))
	})
	return out
}

func drawnOrRejectedPairs(m *manifest.Manifest) map[[2]string]bool {
	settled := rejectedPairSet(m)
	for _, v := range m.Views {
		if v.ID == manifest.SystemIndexViewID {
			continue
		}
		for _, e := range v.Edges {
			settled[[2]string{e.From, e.To}] = true
		}
	}
	return settled
}

func rejectedPairSet(m *manifest.Manifest) map[[2]string]bool {
	rejected := make(map[[2]string]bool, len(m.RejectedEdges))
	for _, re := range m.RejectedEdges {
		rejected[[2]string{re.From, re.To}] = true
	}
	return rejected
}
