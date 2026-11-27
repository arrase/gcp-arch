package manifest

import (
	"fmt"
	"slices"

	"github.com/arrase/gcp-arch/internal/extract"
)

// TransversalAccountingAssetTypes lists the 5 core governance/transversal asset types
// excluded from FactsPrimaryCount and tracked in UnrepresentedTransversalURIs,
// matching e2e/catalog_fixtures_test.go.
var TransversalAccountingAssetTypes = map[string]bool{
	"iam.googleapis.com/ServiceAccount":     true,
	"cloudkms.googleapis.com/CryptoKey":     true,
	"secretmanager.googleapis.com/Secret":   true,
	"logging.googleapis.com/LogSink":        true,
	"monitoring.googleapis.com/AlertPolicy": true,
}

var transversalAccountingEdgeDetails = map[string]bool{
	"cmek":            true,
	"secret_ref":      true,
	"service_account": true,
}

var structuralContainerAssetTypes = map[string]bool{
	"compute.googleapis.com/Network":    true,
	"compute.googleapis.com/Subnetwork": true,
	"container.googleapis.com/Cluster":  true,
}

var transversalEligiblePrimaryAssetTypes = map[string]bool{
	"compute.googleapis.com/Router":         true,
	"vpcaccess.googleapis.com/Connector":    true,
	"compute.googleapis.com/Firewall":       true,
	"compute.googleapis.com/Policy":         true,
	"compute.googleapis.com/SecurityPolicy": true,
	"compute.googleapis.com/Route":          true,
	"logging.googleapis.com/LogBucket":      true,
}

var validStructuralEdgeReasons = map[string]bool{
	"1to1_plumbing_fold":         true,
	"intra_component_fold":       true,
	"psa_container_boundary":     true,
	"vpc_peering":                true,
	"vpc_peering_container_link": true,
	"container_attachment":       true,
	"heuristic_correlation":      true,
	"structural_link":            true,
}

// ConservationReport captures deterministic conservation invariants between a FactsFile
// and a synthesized Manifest.
type ConservationReport struct {
	Status                       string   `json:"status"`
	FactsPrimaryCount            int      `json:"facts_primary_count"`
	RepresentedPrimaryCount      int      `json:"represented_primary_count"`
	MissingPrimaryURIs           []string `json:"missing_primary_uris"`
	HallucinatedPrimaryURIs      []string `json:"hallucinated_primary_uris"`
	FactsFunctionalEdgeCount     int      `json:"facts_functional_edge_count"`
	MissingEdgeIDs               []string `json:"missing_edge_ids"`
	UnrepresentedTransversalURIs []string `json:"unrepresented_transversal_uris"`
	MissingTransversalEdgeIDs    []string `json:"missing_transversal_edge_ids"`
	View0Coverage                float64  `json:"view0_coverage"`
	View0MissingNodeIDs          []string `json:"view0_missing_node_ids"`
}

// Issues converts all failed conservation invariants in r into structured ValidationIssue entries.
// Returns nil when r.Status == "PASS".
func (r ConservationReport) Issues() []ValidationIssue {
	if r.Status == "PASS" {
		return nil
	}
	var issues []ValidationIssue
	for _, uri := range r.MissingPrimaryURIs {
		issues = append(issues, ValidationIssue{
			Code:    "CONSERVATION_MISSING_PRIMARY",
			Subject: uri,
			Message: fmt.Sprintf("primary resource %q is not represented in components/containers/transversals", uri),
		})
	}
	for _, uri := range r.HallucinatedPrimaryURIs {
		issues = append(issues, ValidationIssue{
			Code:    "CONSERVATION_HALLUCINATED_PRIMARY",
			Subject: uri,
			Message: fmt.Sprintf("manifest references hallucinated URI %q absent from facts", uri),
		})
	}
	for _, eid := range r.MissingEdgeIDs {
		issues = append(issues, ValidationIssue{
			Code:    "CONSERVATION_MISSING_EDGE",
			Subject: eid,
			Message: fmt.Sprintf("functional edge %q is missing from View 0 / valid structural representation", eid),
		})
	}
	for _, uri := range r.UnrepresentedTransversalURIs {
		issues = append(issues, ValidationIssue{
			Code:    "CONSERVATION_UNREPRESENTED_TRANSVERSAL",
			Subject: uri,
			Message: fmt.Sprintf("transversal resource %q is not represented in transversal_items", uri),
		})
	}
	for _, eid := range r.MissingTransversalEdgeIDs {
		issues = append(issues, ValidationIssue{
			Code:    "CONSERVATION_MISSING_TRANSVERSAL_EDGE",
			Subject: eid,
			Message: fmt.Sprintf("transversal edge %q is missing from transversal_items[].backed_edge_ids", eid),
		})
	}
	for _, nid := range r.View0MissingNodeIDs {
		issues = append(issues, ValidationIssue{
			Code:    "CONSERVATION_VIEW0_COVERAGE",
			ViewID:  SystemIndexViewID,
			Subject: nid,
			Message: fmt.Sprintf("component %q is missing from View 0 placements", nid),
		})
	}
	if len(issues) == 0 {
		issues = append(issues, ValidationIssue{
			Code:    "CONSERVATION_FAILED",
			Message: "deterministic conservation verification failed",
		})
	}
	return issues
}

// IsManifestFunctionalEdge reports whether e is a non-heuristic functional edge
// required by conservation accounting (matching e2e/catalog_fixtures_test.go).
func IsManifestFunctionalEdge(e extract.FactEdge, resByURI map[string]extract.FactResource) bool {
	if e.Heuristic {
		return false
	}
	switch e.Kind {
	case extract.EdgeIAMResourceBinding, extract.EdgeWorkloadIdentity, extract.EdgeMonitoringTraffic:
		return false
	}
	fromRes, okFrom := resByURI[e.FromURI]
	toRes, okTo := resByURI[e.ToURI]
	if !okFrom || !okTo {
		return false
	}
	if TransversalAccountingAssetTypes[fromRes.AssetType] || TransversalAccountingAssetTypes[toRes.AssetType] {
		return false
	}
	if e.Kind == extract.EdgeConfigRef && transversalAccountingEdgeDetails[e.Detail] {
		return false
	}
	return true
}

type conservationFactsIndex struct {
	resByURI            map[string]extract.FactResource
	expectedPrimary     map[string]bool
	expectedTransversal map[string]bool
	transversalURIs     map[string]bool
	factEdgeByID        map[string]extract.FactEdge
	expectedFuncEdges   map[string]bool
	expectedTransEdges  map[string]bool
}

type conservationManifestIndex struct {
	representedPrimary     map[string]bool
	representedTransversal map[string]bool
	transversalURIs        map[string]bool
	hallucinated           map[string]bool
	compOwnerByURI         map[string]string
	validContainers        map[string]bool
	validTransversals      map[string]bool
}

func failConservationReport() ConservationReport {
	return ConservationReport{
		Status:                       "FAIL",
		MissingPrimaryURIs:           []string{},
		HallucinatedPrimaryURIs:      []string{},
		MissingEdgeIDs:               []string{},
		UnrepresentedTransversalURIs: []string{},
		MissingTransversalEdgeIDs:    []string{},
		View0MissingNodeIDs:          []string{},
	}
}

// VerifyConservation deterministically verifies that every primary resource, transversal
// resource, non-heuristic functional edge, and transversal edge from facts is represented
// in m without hallucinations, and that View 0 covers 100% of primary Components.
func VerifyConservation(m *Manifest, facts *extract.FactsFile) ConservationReport {
	if m == nil || facts == nil {
		return failConservationReport()
	}
	evIdx := NewEvidenceIndex(m.Components, m.Containers, facts.Edges)
	return verifyConservationWithIndex(m, facts, evIdx)
}

func verifyConservationWithIndex(m *Manifest, facts *extract.FactsFile, evIdx *EvidenceIndex) ConservationReport {
	fi := buildConservationFactsIndex(facts)
	mi := buildConservationManifestIndex(m, fi)

	missingPrimary := sortedMissingURIs(fi.expectedPrimary, mi.representedPrimary)
	hallucinated := sortedTrueKeys(mi.hallucinated)
	unrepresentedTransversal := sortedMissingURIs(fi.expectedTransversal, mi.representedTransversal)

	accountedView0 := collectAccountedView0Edges(m, fi.factEdgeByID, mi.compOwnerByURI, evIdx)
	accountedStructural := collectAccountedStructuralEdges(m, fi, mi)
	accountedTransEdges := collectAccountedTransEdges(m, fi.factEdgeByID, mi.compOwnerByURI, accountedStructural)

	missingEdgeIDs := collectMissingFunctionalEdges(fi, mi.compOwnerByURI, accountedView0, accountedStructural)
	missingTransEdgeIDs := sortedMissingEdgeIDs(fi.expectedTransEdges, accountedTransEdges, accountedView0)
	view0Missing := collectView0MissingNodes(m)

	view0Coverage := 1.0
	if len(m.Components) > 0 {
		view0Coverage = float64(len(m.Components)-len(view0Missing)) / float64(len(m.Components))
	}

	status := "PASS"
	if len(missingPrimary) > 0 ||
		len(hallucinated) > 0 ||
		len(missingEdgeIDs) > 0 ||
		len(unrepresentedTransversal) > 0 ||
		len(missingTransEdgeIDs) > 0 ||
		len(view0Missing) > 0 {
		status = "FAIL"
	}

	return ConservationReport{
		Status:                       status,
		FactsPrimaryCount:            len(fi.expectedPrimary),
		RepresentedPrimaryCount:      len(mi.representedPrimary),
		MissingPrimaryURIs:           missingPrimary,
		HallucinatedPrimaryURIs:      hallucinated,
		FactsFunctionalEdgeCount:     len(fi.expectedFuncEdges),
		MissingEdgeIDs:               missingEdgeIDs,
		UnrepresentedTransversalURIs: unrepresentedTransversal,
		MissingTransversalEdgeIDs:    missingTransEdgeIDs,
		View0Coverage:                view0Coverage,
		View0MissingNodeIDs:          view0Missing,
	}
}

func buildConservationFactsIndex(facts *extract.FactsFile) conservationFactsIndex {
	fi := conservationFactsIndex{
		resByURI:            make(map[string]extract.FactResource, len(facts.Resources)),
		expectedPrimary:     make(map[string]bool),
		expectedTransversal: make(map[string]bool),
		transversalURIs:     make(map[string]bool),
		factEdgeByID:        make(map[string]extract.FactEdge, len(facts.Edges)),
		expectedFuncEdges:   make(map[string]bool),
		expectedTransEdges:  make(map[string]bool),
	}
	for _, r := range facts.Resources {
		fi.resByURI[r.URI] = r
		if TransversalAccountingAssetTypes[r.AssetType] {
			fi.expectedTransversal[r.URI] = true
			fi.transversalURIs[r.URI] = true
		} else {
			fi.expectedPrimary[r.URI] = true
			if transversalEligiblePrimaryAssetTypes[r.AssetType] {
				fi.transversalURIs[r.URI] = true
			}
		}
	}
	for _, e := range facts.Edges {
		id := e.ID
		if id == "" {
			id = extract.EdgeID(e.Kind, e.FromURI, e.ToURI, e.Detail)
		}
		e.ID = id
		fi.factEdgeByID[id] = e
		if e.Heuristic {
			continue
		}
		if IsManifestFunctionalEdge(e, fi.resByURI) {
			fi.expectedFuncEdges[id] = true
		} else if e.Kind != extract.EdgeMonitoringTraffic {
			fi.expectedTransEdges[id] = true
		}
	}
	return fi
}

type uriOwnerKind int

const (
	uriOwnerComponent uriOwnerKind = iota
	uriOwnerContainer
	uriOwnerTransversal
)

func buildConservationManifestIndex(m *Manifest, fi conservationFactsIndex) conservationManifestIndex {
	mi := conservationManifestIndex{
		representedPrimary:     make(map[string]bool),
		representedTransversal: make(map[string]bool),
		transversalURIs:        make(map[string]bool),
		hallucinated:           make(map[string]bool),
		compOwnerByURI:         make(map[string]string),
		validContainers:        make(map[string]bool, len(m.Containers)),
		validTransversals:      make(map[string]bool, len(m.TransversalItems)),
	}
	for _, c := range m.Components {
		mi.recordBackedBy(c.BackedBy, uriOwnerComponent, c.ID, fi)
	}
	for _, ct := range m.Containers {
		if ct.ID != "" {
			mi.validContainers[ct.ID] = true
		}
		mi.recordBackedBy(ct.BackedBy, uriOwnerContainer, "", fi)
	}
	for _, ti := range m.TransversalItems {
		if ti.ID != "" {
			mi.validTransversals[ti.ID] = true
		}
		mi.recordBackedBy(ti.BackedBy, uriOwnerTransversal, "", fi)
	}
	return mi
}

func (mi *conservationManifestIndex) recordBackedBy(backed []BackedResource, owner uriOwnerKind, ownerID string, fi conservationFactsIndex) {
	for _, br := range backed {
		if br.URI == "" {
			continue
		}
		switch owner {
		case uriOwnerComponent:
			mi.compOwnerByURI[br.URI] = ownerID
		case uriOwnerTransversal:
			mi.transversalURIs[br.URI] = true
		}
		recordConservedURI(br.URI, owner, fi, mi)
	}
}

func recordConservedURI(uri string, owner uriOwnerKind, fi conservationFactsIndex, mi *conservationManifestIndex) {
	if uri == "" {
		return
	}
	r, exists := fi.resByURI[uri]
	if !exists {
		mi.hallucinated[uri] = true
		return
	}
	if fi.expectedTransversal[uri] {
		if owner == uriOwnerTransversal {
			mi.representedTransversal[uri] = true
		}
		return
	}
	switch owner {
	case uriOwnerComponent:
		mi.representedPrimary[uri] = true
	case uriOwnerContainer:
		if structuralContainerAssetTypes[r.AssetType] && !r.IsExternal {
			mi.representedPrimary[uri] = true
		}
	case uriOwnerTransversal:
		if transversalEligiblePrimaryAssetTypes[r.AssetType] && !r.IsExternal {
			mi.representedPrimary[uri] = true
		}
	}
}

func collectAccountedView0Edges(
	m *Manifest,
	factEdgeByID map[string]extract.FactEdge,
	compOwnerByURI map[string]string,
	evIdx *EvidenceIndex,
) map[string]bool {
	accounted := make(map[string]bool)
	if len(m.Views) == 0 {
		return accounted
	}
	for _, e := range m.Views[0].Edges {
		if e.Provenance != ProvenanceVerified {
			continue
		}
		if code, _ := evIdx.WalkIssue(e); code != "" {
			continue
		}
		recordView0EdgeEvidence(e, factEdgeByID, compOwnerByURI, accounted)
	}
	return accounted
}

func recordView0EdgeEvidence(
	e Edge,
	factEdgeByID map[string]extract.FactEdge,
	compOwnerByURI map[string]string,
	accounted map[string]bool,
) {
	for _, eid := range e.EvidenceEdgeIDs {
		fe, ok := factEdgeByID[eid]
		if !ok {
			continue
		}
		compFrom := compOwnerByURI[fe.FromURI]
		compTo := compOwnerByURI[fe.ToURI]
		if compFrom != "" && compTo != "" && compFrom != compTo && (e.From != compFrom || e.To != compTo) {
			continue
		}
		accounted[eid] = true
	}
}

func collectAccountedStructuralEdges(
	m *Manifest,
	fi conservationFactsIndex,
	mi conservationManifestIndex,
) map[string]bool {
	accounted := make(map[string]bool)
	collectComponentFoldedEdges(m.Components, fi.factEdgeByID, mi.compOwnerByURI, accounted)
	collectContainerFoldedEdges(m.Containers, fi.factEdgeByID, mi.compOwnerByURI, accounted)
	for _, se := range m.StructuralEdges {
		if isValidStructuralEdge(se, fi, mi) {
			accounted[se.EdgeID] = true
		}
	}
	return accounted
}

func collectComponentFoldedEdges(
	components []Component,
	factEdgeByID map[string]extract.FactEdge,
	compOwnerByURI map[string]string,
	accounted map[string]bool,
) {
	for _, c := range components {
		for _, eid := range c.FoldedEdgeIDs {
			fe, ok := factEdgeByID[eid]
			if !ok {
				continue
			}
			if compOwnerByURI[fe.FromURI] == c.ID && compOwnerByURI[fe.ToURI] == c.ID {
				accounted[eid] = true
			}
		}
	}
}

func collectContainerFoldedEdges(
	containers []Container,
	factEdgeByID map[string]extract.FactEdge,
	compOwnerByURI map[string]string,
	accounted map[string]bool,
) {
	for _, ct := range containers {
		ctURIs := backedURISet(ct.BackedBy)
		for _, eid := range ct.FoldedEdgeIDs {
			fe, ok := factEdgeByID[eid]
			if !ok {
				continue
			}
			if miBothOwned(compOwnerByURI, fe) {
				continue
			}
			if ctURIs[fe.FromURI] || ctURIs[fe.ToURI] || (ct.Kind == ContainerPSAZone && fe.Kind == extract.EdgePSAPeering) {
				accounted[eid] = true
			}
		}
	}
}

func miBothOwned(compOwnerByURI map[string]string, fe extract.FactEdge) bool {
	return compOwnerByURI[fe.FromURI] != "" && compOwnerByURI[fe.ToURI] != ""
}

func isValidStructuralEdge(se StructuralEdge, fi conservationFactsIndex, mi conservationManifestIndex) bool {
	fe, ok := fi.factEdgeByID[se.EdgeID]
	if !ok || !validStructuralEdgeReasons[se.Reason] {
		return false
	}
	if se.FromURI != fe.FromURI || se.ToURI != fe.ToURI {
		return false
	}
	if se.Reason == "heuristic_correlation" && !fe.Heuristic {
		return false
	}
	compFrom := mi.compOwnerByURI[fe.FromURI]
	compTo := mi.compOwnerByURI[fe.ToURI]
	if se.Reason == "intra_component_fold" || se.Reason == "1to1_plumbing_fold" {
		return compFrom != "" && compFrom == compTo
	}
	if compFrom != "" && compTo != "" {
		return false
	}
	if se.Reason == "structural_link" {
		return isValidStructuralLink(se, fe, fi, mi)
	}
	return true
}

func isValidStructuralLink(se StructuralEdge, fe extract.FactEdge, fi conservationFactsIndex, mi conservationManifestIndex) bool {
	if se.TargetID == "" || (!mi.validContainers[se.TargetID] && !mi.validTransversals[se.TargetID]) {
		return false
	}
	return !fi.transversalURIs[fe.FromURI] && !fi.transversalURIs[fe.ToURI] &&
		!mi.transversalURIs[fe.FromURI] && !mi.transversalURIs[fe.ToURI]
}

func collectAccountedTransEdges(
	m *Manifest,
	factEdgeByID map[string]extract.FactEdge,
	compOwnerByURI map[string]string,
	accountedStructural map[string]bool,
) map[string]bool {
	accounted := make(map[string]bool)
	for _, ti := range m.TransversalItems {
		tiURIs := backedURISet(ti.BackedBy)
		for _, eid := range ti.BackedEdgeIDs {
			fe, ok := factEdgeByID[eid]
			if !ok {
				continue
			}
			if compOwnerByURI[fe.FromURI] != "" && compOwnerByURI[fe.ToURI] != "" {
				continue
			}
			if !tiURIs[fe.FromURI] && !tiURIs[fe.ToURI] {
				continue
			}
			accounted[eid] = true
			accountedStructural[eid] = true
		}
	}
	return accounted
}

func collectMissingFunctionalEdges(
	fi conservationFactsIndex,
	compOwnerByURI map[string]string,
	accountedView0, accountedStructural map[string]bool,
) []string {
	missing := make([]string, 0)
	for eid := range fi.expectedFuncEdges {
		fe := fi.factEdgeByID[eid]
		compFrom := compOwnerByURI[fe.FromURI]
		compTo := compOwnerByURI[fe.ToURI]
		if compFrom != "" && compTo != "" && compFrom != compTo {
			if !accountedView0[eid] {
				missing = append(missing, eid)
			}
			continue
		}
		if !accountedView0[eid] && !accountedStructural[eid] {
			missing = append(missing, eid)
		}
	}
	slices.Sort(missing)
	return missing
}

func sortedMissingEdgeIDs(expected, a, b map[string]bool) []string {
	missing := make([]string, 0)
	for eid := range expected {
		if !a[eid] && !b[eid] {
			missing = append(missing, eid)
		}
	}
	slices.Sort(missing)
	return missing
}

func collectView0MissingNodes(m *Manifest) []string {
	placed := make(map[string]bool)
	if len(m.Views) > 0 {
		for _, p := range m.Views[0].Placements {
			placed[p.ComponentID] = true
		}
	}
	missing := make([]string, 0)
	for _, c := range m.Components {
		if !placed[c.ID] {
			missing = append(missing, c.ID)
		}
	}
	slices.Sort(missing)
	return missing
}

func sortedMissingURIs(expected, present map[string]bool) []string {
	out := make([]string, 0)
	for uri := range expected {
		if !present[uri] {
			out = append(out, uri)
		}
	}
	slices.Sort(out)
	return out
}

func sortedTrueKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
