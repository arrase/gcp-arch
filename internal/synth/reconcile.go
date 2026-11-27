package synth

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/icons"
	"github.com/arrase/gcp-arch/internal/manifest"
)

const lostEvidenceRationalePrefix = "Not verifiable from facts.json: "

// Reconcile synchronizes an existing architecture manifest against facts.
// When all active resource URIs and functional/transversal edges match existing, Reconcile takes a
// zero-LLM fast path (< 5ms, allowing client == nil) that refreshes edge provenance in place.
// When topology or edge deltas exist, Reconcile runs Synthesize and preserves curated human edits
// (matching component and view Title, Subtitle, Notes) from existing.
func Reconcile(
	ctx context.Context,
	facts *extract.FactsFile,
	existing *manifest.Manifest,
	client ModelClient,
	reg *icons.Registry,
) (*manifest.Manifest, error) {
	if err := validateReconcileInputs(facts, existing, reg); err != nil {
		return nil, err
	}

	if fast, ok, err := tryZeroDeltaFastPath(facts, existing, reg); err != nil {
		return nil, err
	} else if ok {
		return fast, nil
	}

	if client == nil {
		return nil, fmt.Errorf("reconcile detected architecture deltas; vertex AI model client is required")
	}

	res, err := Synthesize(ctx, facts, client, reg)
	if err != nil {
		return nil, err
	}
	preserveCuratedEdits(res.Manifest, existing)

	rep, err := manifest.ValidateWithFactsAndConservation(res.Manifest, facts, reg)
	if err != nil {
		return nil, err
	}
	if rep.Status != "PASS" {
		return nil, &manifest.ValidationError{Issues: rep.Issues()}
	}
	return res.Manifest, nil
}

func validateReconcileInputs(facts *extract.FactsFile, existing *manifest.Manifest, reg *icons.Registry) error {
	if facts == nil {
		return fmt.Errorf("facts file must not be nil")
	}
	if existing == nil {
		return fmt.Errorf("existing manifest must not be nil")
	}
	if reg == nil {
		return fmt.Errorf("icons registry must not be nil")
	}
	if len(facts.Resources) > MaxActiveResources {
		return &ScaleLimitError{ResourceCount: len(facts.Resources), MaxAllowed: MaxActiveResources}
	}
	return nil
}

func tryZeroDeltaFastPath(
	facts *extract.FactsFile,
	existing *manifest.Manifest,
	reg *icons.Registry,
) (*manifest.Manifest, bool, error) {
	if !resourcesMatch(existing, facts) {
		return nil, false, nil
	}

	cur := cloneManifest(existing)
	evIdx := manifest.NewEvidenceIndex(cur.Components, cur.Containers, facts.Edges)
	refreshEdgeProvenance(cur, evIdx)

	if cur.IsLayeredFlow() {
		rep, valErr := manifest.ValidateWithFactsAndConservation(cur, facts, reg)
		if rep.Status != "PASS" {
			return nil, false, nil
		}
		if valErr != nil {
			return nil, false, valErr
		}
		return cur, true, nil
	}

	if hasUnsettledProjectedPair(cur, evIdx) {
		return nil, false, nil
	}
	if err := manifest.ValidateWithFacts(cur, facts, reg); err != nil {
		return nil, false, err
	}
	return cur, true, nil
}

func resourcesMatch(m *manifest.Manifest, facts *extract.FactsFile) bool {
	factURIs := make(map[string]bool, len(facts.Resources))
	for _, r := range facts.Resources {
		factURIs[r.URI] = true
	}
	existingURIs := accountedURISet(m)
	if len(factURIs) != len(existingURIs) {
		return false
	}
	for uri := range factURIs {
		if !existingURIs[uri] {
			return false
		}
	}
	return true
}

func accountedURISet(m *manifest.Manifest) map[string]bool {
	out := make(map[string]bool)
	for _, c := range m.Components {
		for _, br := range c.BackedBy {
			if br.URI != "" {
				out[br.URI] = true
			}
		}
	}
	for _, ct := range m.Containers {
		for _, br := range ct.BackedBy {
			if br.URI != "" {
				out[br.URI] = true
			}
		}
	}
	for _, ti := range m.TransversalItems {
		for _, br := range ti.BackedBy {
			if br.URI != "" {
				out[br.URI] = true
			}
		}
	}
	for _, ex := range m.ExcludedResources {
		if ex.URI != "" {
			out[ex.URI] = true
		}
	}
	return out
}

func hasUnsettledProjectedPair(m *manifest.Manifest, idx *manifest.EvidenceIndex) bool {
	settled := make(map[[2]string]bool, len(m.RejectedEdges)+len(m.AcknowledgedEdges))
	for _, re := range m.RejectedEdges {
		settled[[2]string{re.From, re.To}] = true
	}
	for _, ae := range m.AcknowledgedEdges {
		settled[[2]string{ae.From, ae.To}] = true
	}
	for _, v := range m.Views {
		if v.ID == manifest.SystemIndexViewID && v.Archetype != manifest.ArchetypeLayeredFlow {
			continue
		}
		for _, e := range v.Edges {
			settled[[2]string{e.From, e.To}] = true
		}
	}
	for pair := range idx.ProjectedPairs() {
		if !settled[pair] {
			return true
		}
	}
	return false
}

func refreshEdgeProvenance(m *manifest.Manifest, idx *manifest.EvidenceIndex) {
	for vi := range m.Views {
		if m.Views[vi].ID == manifest.SystemIndexViewID && m.Views[vi].Archetype != manifest.ArchetypeLayeredFlow {
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

func preserveCuratedEdits(dst, existing *manifest.Manifest) {
	prevComp := make(map[string]manifest.Component, len(existing.Components))
	for _, c := range existing.Components {
		prevComp[c.ID] = c
	}
	for i := range dst.Components {
		prev, ok := prevComp[dst.Components[i].ID]
		if !ok {
			continue
		}
		if strings.TrimSpace(prev.Title) != "" {
			dst.Components[i].Title = prev.Title
		}
		if strings.TrimSpace(prev.Subtitle) != "" {
			dst.Components[i].Subtitle = prev.Subtitle
		}
		if strings.TrimSpace(prev.Notes) != "" {
			dst.Components[i].Notes = prev.Notes
		}
	}

	prevView := make(map[string]manifest.View, len(existing.Views))
	for _, v := range existing.Views {
		prevView[v.ID] = v
	}
	prevAutoOverviewSub := ""
	if v0, ok := prevView[manifest.SystemIndexViewID]; ok {
		prevAutoOverviewSub = overviewSubtitle(len(existing.Components), len(v0.Edges), len(existing.TransversalItems))
	}

	for i := range dst.Views {
		prev, ok := prevView[dst.Views[i].ID]
		if !ok {
			continue
		}
		if strings.TrimSpace(prev.Title) != "" {
			dst.Views[i].Title = prev.Title
		}
		if strings.TrimSpace(prev.Subtitle) != "" {
			if dst.Views[i].ID != manifest.SystemIndexViewID || prev.Subtitle != prevAutoOverviewSub {
				dst.Views[i].Subtitle = prev.Subtitle
			}
		}
		if strings.TrimSpace(prev.Notes) != "" {
			dst.Views[i].Notes = prev.Notes
		}
	}
}

func cloneManifest(m *manifest.Manifest) *manifest.Manifest {
	out := &manifest.Manifest{
		SchemaVersion: m.SchemaVersion,
		Metadata: manifest.Metadata{
			GeneratedAt: m.Metadata.GeneratedAt,
			Scopes:      slices.Clone(m.Metadata.Scopes),
		},
		Components:               make([]manifest.Component, len(m.Components)),
		Containers:               make([]manifest.Container, len(m.Containers)),
		TransversalItems:         make([]manifest.TransversalItem, len(m.TransversalItems)),
		StructuralEdges:          slices.Clone(m.StructuralEdges),
		SuppressedPlumbingCounts: maps.Clone(m.SuppressedPlumbingCounts),
		ExcludedResources:        slices.Clone(m.ExcludedResources),
		RejectedEdges:            slices.Clone(m.RejectedEdges),
		AcknowledgedEdges:        slices.Clone(m.AcknowledgedEdges),
		Views:                    make([]manifest.View, len(m.Views)),
	}
	for i, c := range m.Components {
		cc := c
		cc.BackedBy = slices.Clone(c.BackedBy)
		cc.Members = slices.Clone(c.Members)
		cc.Badges = slices.Clone(c.Badges)
		cc.FoldedEdgeIDs = slices.Clone(c.FoldedEdgeIDs)
		cc.InlineMeta = slices.Clone(c.InlineMeta)
		out.Components[i] = cc
	}
	for i, ct := range m.Containers {
		ctc := ct
		ctc.Badges = slices.Clone(ct.Badges)
		ctc.FoldedEdgeIDs = slices.Clone(ct.FoldedEdgeIDs)
		ctc.BackedBy = slices.Clone(ct.BackedBy)
		out.Containers[i] = ctc
	}
	for i, ti := range m.TransversalItems {
		tic := ti
		tic.AttachedNodeIDs = slices.Clone(ti.AttachedNodeIDs)
		tic.BackedBy = slices.Clone(ti.BackedBy)
		tic.BackedEdgeIDs = slices.Clone(ti.BackedEdgeIDs)
		tic.Roles = slices.Clone(ti.Roles)
		out.TransversalItems[i] = tic
	}
	for i, v := range m.Views {
		vc := v
		vc.ContainerIDs = slices.Clone(v.ContainerIDs)
		vc.Placements = make([]manifest.Placement, len(v.Placements))
		for pi, p := range v.Placements {
			pc := p
			pc.OffPageRefs = slices.Clone(p.OffPageRefs)
			vc.Placements[pi] = pc
		}
		vc.Edges = make([]manifest.Edge, len(v.Edges))
		for ei, e := range v.Edges {
			ec := e
			ec.EvidenceEdgeIDs = slices.Clone(e.EvidenceEdgeIDs)
			vc.Edges[ei] = ec
		}
		out.Views[i] = vc
	}
	return out
}
