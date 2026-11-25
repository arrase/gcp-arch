package manifest

import (
	"cmp"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/icons"
)

// Validation error codes emitted in ValidationIssue.Code.
const (
	ErrInvalidSchemaVersion        = "INVALID_SCHEMA_VERSION"
	ErrEmptyComponentID            = "EMPTY_COMPONENT_ID"
	ErrDuplicateComponentID        = "DUPLICATE_COMPONENT_ID"
	ErrEmptyComponentTitle         = "EMPTY_COMPONENT_TITLE"
	ErrInvalidComponentKind        = "INVALID_COMPONENT_KIND"
	ErrEmptyProjectID              = "EMPTY_PROJECT_ID"
	ErrInvalidIconKey              = "INVALID_ICON_KEY"
	ErrEmptyBackedBy               = "EMPTY_BACKED_BY"
	ErrInvalidBackedResource       = "INVALID_BACKED_RESOURCE"
	ErrInlineMetaLimit             = "INLINE_META_LIMIT"
	ErrInvalidInlineMeta           = "INVALID_INLINE_META"
	ErrHeterogeneousDatastore      = "HETEROGENEOUS_DATASTORE"
	ErrEmptyContainerID            = "EMPTY_CONTAINER_ID"
	ErrDuplicateContainerID        = "DUPLICATE_CONTAINER_ID"
	ErrEmptyContainerTitle         = "EMPTY_CONTAINER_TITLE"
	ErrInvalidContainerKind        = "INVALID_CONTAINER_KIND"
	ErrUnknownParentContainer      = "UNKNOWN_PARENT_CONTAINER"
	ErrContainerCycle              = "CONTAINER_CYCLE"
	ErrContainerDepthExceeded      = "CONTAINER_DEPTH_EXCEEDED"
	ErrInvalidExcludedResource     = "INVALID_EXCLUDED_RESOURCE"
	ErrMissingExclusionReason      = "MISSING_EXCLUSION_REASON"
	ErrInvalidRejectedEdge         = "INVALID_REJECTED_EDGE"
	ErrNoViews                     = "NO_VIEWS"
	ErrEmptyViewID                 = "EMPTY_VIEW_ID"
	ErrDuplicateViewID             = "DUPLICATE_VIEW_ID"
	ErrEmptyViewTitle              = "EMPTY_VIEW_TITLE"
	ErrMissingSystemIndexView      = "MISSING_SYSTEM_INDEX_VIEW"
	ErrUnexpectedSystemIndexView   = "UNEXPECTED_SYSTEM_INDEX_VIEW"
	ErrMisplacedSystemIndexView    = "MISPLACED_SYSTEM_INDEX_VIEW"
	ErrInvalidArchetype            = "INVALID_ARCHETYPE"
	ErrInvalidAccentMode           = "INVALID_ACCENT_MODE"
	ErrUnknownContainerRef         = "UNKNOWN_CONTAINER_REF"
	ErrViewPlacementCount          = "VIEW_PLACEMENT_COUNT"
	ErrDuplicatePlacement          = "DUPLICATE_PLACEMENT"
	ErrUnknownComponentRef         = "UNKNOWN_COMPONENT_REF"
	ErrPlacementContainerNotInView = "PLACEMENT_CONTAINER_NOT_IN_VIEW"
	ErrColumnOutOfBounds           = "COLUMN_OUT_OF_BOUNDS"
	ErrInvalidOffPageRef           = "INVALID_OFF_PAGE_REF"
	ErrEdgeEndpointNotPlaced       = "EDGE_ENDPOINT_NOT_PLACED"
	ErrRejectedEdgePresent         = "REJECTED_EDGE_PRESENT"
	ErrEmptyEdgeVerb               = "EMPTY_EDGE_VERB"
	ErrEdgeVerbTooLong             = "EDGE_VERB_TOO_LONG"
	ErrInvalidFlowKind             = "INVALID_FLOW_KIND"
	ErrInvalidProvenance           = "INVALID_PROVENANCE"
	ErrMissingEvidenceEdgeIDs      = "MISSING_EVIDENCE_EDGE_IDS"
	ErrMissingInferenceRationale   = "MISSING_INFERENCE_RATIONALE"
	ErrInvalidBusGroup             = "INVALID_BUS_GROUP"
	ErrUnplacedComponent           = "UNPLACED_COMPONENT"
	ErrNilFacts                    = "NIL_FACTS"
	ErrUnaccountedFactResource     = "UNACCOUNTED_FACT_RESOURCE"
	ErrUnknownEvidenceEdge         = "UNKNOWN_EVIDENCE_EDGE"
	ErrInvalidEdgeProvenance       = "INVALID_EDGE_PROVENANCE"
)

const (
	maxInlineMetaRows        = 4
	maxDetailViewPlacements  = 12
	maxSystemIndexPlacements = 16
	maxContainerNestingDepth = 2
	maxEdgeVerbRunes         = 16
)

var statefulDatastoreAssetTypes = map[string]bool{
	"sqladmin.googleapis.com/Instance":           true,
	"alloydb.googleapis.com/Cluster":             true,
	"alloydb.googleapis.com/Instance":            true,
	"spanner.googleapis.com/Instance":            true,
	"spanner.googleapis.com/Database":            true,
	"bigtableadmin.googleapis.com/Instance":      true,
	"firestore.googleapis.com/Database":          true,
	"redis.googleapis.com/Instance":              true,
	"redis.googleapis.com/Cluster":               true,
	"memcache.googleapis.com/Instance":           true,
	"storage.googleapis.com/Bucket":              true,
	"bigquery.googleapis.com/Dataset":            true,
	"artifactregistry.googleapis.com/Repository": true,
	"file.googleapis.com/Instance":               true,
	"pubsub.googleapis.com/Topic":                true,
}

// ValidateStructure verifies all offline structural invariants of m without facts.json.
// Returns *ValidationError when one or more issues are found.
func ValidateStructure(m *Manifest, reg *icons.Registry) error {
	issues := validateStructureIssues(m, reg)
	if len(issues) > 0 {
		return &ValidationError{Issues: issues}
	}
	return nil
}

// ValidateWithFacts runs ValidateStructure plus --facts checks: 100% FactsFile resource accounting
// and directed-walk FactEdge provenance on all detail-view VERIFIED edges.
// Returns *ValidationError when one or more issues are found.
func ValidateWithFacts(m *Manifest, facts *extract.FactsFile, reg *icons.Registry) error {
	issues := validateStructureIssues(m, reg)
	if facts == nil {
		issues = append(issues, ValidationIssue{
			Code:    ErrNilFacts,
			Message: "facts file must not be nil",
		})
		return &ValidationError{Issues: issues}
	}
	if m == nil {
		return &ValidationError{Issues: issues}
	}

	compByID := make(map[string]Component, len(m.Components))
	for _, c := range m.Components {
		compByID[c.ID] = c
	}

	issues = append(issues, validateFactResourceAccounting(m, facts)...)
	issues = append(issues, validateFactEdgeProvenance(m, facts, compByID)...)

	if len(issues) > 0 {
		return &ValidationError{Issues: issues}
	}
	return nil
}

func validateFactResourceAccounting(m *Manifest, facts *extract.FactsFile) []ValidationIssue {
	accounted := make(map[string]bool, len(facts.Resources))
	for _, c := range m.Components {
		for _, br := range c.BackedBy {
			if br.URI != "" {
				accounted[br.URI] = true
			}
		}
	}
	for _, ct := range m.Containers {
		for _, br := range ct.BackedBy {
			if br.URI != "" {
				accounted[br.URI] = true
			}
		}
	}
	for _, ex := range m.ExcludedResources {
		if ex.URI != "" {
			accounted[ex.URI] = true
		}
	}

	var unaccounted []string
	for _, r := range facts.Resources {
		if !accounted[r.URI] {
			unaccounted = append(unaccounted, r.URI)
		}
	}
	sort.Strings(unaccounted)

	issues := make([]ValidationIssue, 0, len(unaccounted))
	for _, uri := range unaccounted {
		issues = append(issues, ValidationIssue{
			Code:    ErrUnaccountedFactResource,
			Subject: uri,
			Message: fmt.Sprintf("facts resource %q is not accounted for in components[].backed_by, containers[].backed_by, or excluded_resources", uri),
		})
	}
	return issues
}

func validateFactEdgeProvenance(m *Manifest, facts *extract.FactsFile, compByID map[string]Component) []ValidationIssue {
	factEdgeByID := make(map[string]extract.FactEdge, len(facts.Edges))
	for _, fe := range facts.Edges {
		factEdgeByID[fe.ID] = fe
	}

	var issues []ValidationIssue
	for _, v := range m.Views {
		if v.ID == SystemIndexViewID {
			continue
		}
		for _, e := range v.Edges {
			issues = append(issues, validateSingleEdgeProvenance(v.ID, e, compByID, factEdgeByID)...)
		}
	}
	return issues
}

func validateSingleEdgeProvenance(viewID string, e Edge, compByID map[string]Component, factEdgeByID map[string]extract.FactEdge) []ValidationIssue {
	if e.Provenance != ProvenanceVerified || len(e.EvidenceEdgeIDs) == 0 {
		return nil
	}
	fromComp, okFrom := compByID[e.From]
	toComp, okTo := compByID[e.To]
	if !okFrom || !okTo {
		return nil
	}

	edgeLabel := e.From + " -> " + e.To
	cited := make([]extract.FactEdge, 0, len(e.EvidenceEdgeIDs))
	var issues []ValidationIssue
	missingAny := false
	for _, eid := range e.EvidenceEdgeIDs {
		if strings.TrimSpace(eid) == "" {
			missingAny = true
			continue
		}
		fe, ok := factEdgeByID[eid]
		if !ok {
			missingAny = true
			issues = append(issues, ValidationIssue{
				Code:    ErrUnknownEvidenceEdge,
				ViewID:  viewID,
				Subject: edgeLabel,
				Message: fmt.Sprintf("evidence edge ID %q does not exist in facts.json", eid),
			})
			continue
		}
		cited = append(cited, fe)
	}
	if missingAny {
		return issues
	}

	fromURIs := backedURISet(fromComp.BackedBy)
	toURIs := backedURISet(toComp.BackedBy)
	if !isValidDirectedWalk(cited, fromURIs, toURIs) {
		issues = append(issues, ValidationIssue{
			Code:    ErrInvalidEdgeProvenance,
			ViewID:  viewID,
			Subject: edgeLabel,
			Message: fmt.Sprintf("VERIFIED edge %s evidence_edge_ids %v do not form a connected directed walk from %s.backed_by to %s.backed_by", edgeLabel, e.EvidenceEdgeIDs, e.From, e.To),
		})
	}
	return issues
}

// ComputeCoverage builds the sorted Inventory Coverage Table directly from architecture.yaml
// with zero dependency on facts.json.
func ComputeCoverage(m *Manifest) []CoverageRow {
	if m == nil {
		return nil
	}

	type covEntry struct {
		assetType    string
		projectID    string
		dispositions map[string]bool
	}
	byURI := make(map[string]*covEntry)
	record := func(uri, assetType, projectID, disp string) {
		e, ok := byURI[uri]
		if !ok {
			e = &covEntry{
				assetType:    assetType,
				projectID:    projectID,
				dispositions: make(map[string]bool),
			}
			byURI[uri] = e
		}
		if e.assetType == "" && assetType != "" {
			e.assetType = assetType
		}
		if e.projectID == "" && projectID != "" {
			e.projectID = projectID
		}
		e.dispositions[disp] = true
	}

	for _, c := range m.Components {
		label := c.Title
		if label == "" {
			label = c.ID
		}
		for _, br := range c.BackedBy {
			record(br.URI, br.AssetType, br.ProjectID, "Component: "+label)
		}
	}
	for _, ct := range m.Containers {
		label := ct.Title
		if label == "" {
			label = ct.ID
		}
		for _, br := range ct.BackedBy {
			record(br.URI, br.AssetType, br.ProjectID, "Container: "+label)
		}
	}
	for _, ex := range m.ExcludedResources {
		record(ex.URI, ex.AssetType, ex.ProjectID, "Excluded: "+ex.Reason)
	}

	rows := make([]CoverageRow, 0, len(byURI))
	for uri, e := range byURI {
		disps := make([]string, 0, len(e.dispositions))
		for d := range e.dispositions {
			disps = append(disps, d)
		}
		sort.Strings(disps)
		rows = append(rows, CoverageRow{
			URI:         uri,
			AssetType:   e.assetType,
			ProjectID:   e.projectID,
			Disposition: strings.Join(disps, "; "),
		})
	}
	slices.SortFunc(rows, func(a, b CoverageRow) int {
		return cmp.Or(
			cmp.Compare(a.URI, b.URI),
			cmp.Compare(a.AssetType, b.AssetType),
			cmp.Compare(a.ProjectID, b.ProjectID),
			cmp.Compare(a.Disposition, b.Disposition),
		)
	})
	return rows
}

type issueCollector struct {
	issues []ValidationIssue
}

func (ic *issueCollector) add(code, viewID, subject, msg string) {
	ic.issues = append(ic.issues, ValidationIssue{
		Code:    code,
		ViewID:  viewID,
		Subject: subject,
		Message: msg,
	})
}

func validateStructureIssues(m *Manifest, reg *icons.Registry) []ValidationIssue {
	var ic issueCollector
	if m == nil {
		ic.add(ErrInvalidSchemaVersion, "", "", "manifest must not be nil")
		return ic.issues
	}
	if m.SchemaVersion != SchemaVersion {
		ic.add(ErrInvalidSchemaVersion, "", m.SchemaVersion, fmt.Sprintf("unsupported schema_version %q (want %q)", m.SchemaVersion, SchemaVersion))
	}

	compByID := validateComponents(m.Components, reg, &ic)
	contByID := validateContainers(m.Containers, &ic)
	validateExcludedResources(m.ExcludedResources, &ic)
	rejectedPairs := validateRejectedEdges(m.RejectedEdges, &ic)

	if len(m.Views) == 0 {
		ic.add(ErrNoViews, "", "", "manifest must define at least one view")
		return ic.issues
	}

	viewIDs, detailViewIDs := validateViewIndex(m.Views, &ic)
	placedInAnyDetailView := make(map[string]bool, len(compByID))
	for _, v := range m.Views {
		validateView(v, compByID, contByID, viewIDs, detailViewIDs, rejectedPairs, placedInAnyDetailView, &ic)
	}
	validateAllComponentsPlaced(compByID, placedInAnyDetailView, &ic)
	return ic.issues
}

func validateComponents(components []Component, reg *icons.Registry, ic *issueCollector) map[string]Component {
	compByID := make(map[string]Component, len(components))
	for i, c := range components {
		subj := c.ID
		if strings.TrimSpace(subj) == "" {
			subj = fmt.Sprintf("components[%d]", i)
			ic.add(ErrEmptyComponentID, "", subj, "component id must not be empty")
		} else if _, dup := compByID[c.ID]; dup {
			ic.add(ErrDuplicateComponentID, "", c.ID, fmt.Sprintf("duplicate component id %q", c.ID))
		} else {
			compByID[c.ID] = c
		}

		validateComponentFields(c, subj, reg, ic)
		validateComponentBackedBy(c.BackedBy, subj, ic)
		validateComponentInlineMeta(c.InlineMeta, subj, ic)
	}
	return compByID
}

func validateComponentFields(c Component, subj string, reg *icons.Registry, ic *issueCollector) {
	if strings.TrimSpace(c.Title) == "" {
		ic.add(ErrEmptyComponentTitle, "", subj, "component title must not be empty")
	}
	if !isValidComponentKind(c.ComponentKind) {
		ic.add(ErrInvalidComponentKind, "", subj, fmt.Sprintf("invalid component_kind %q", c.ComponentKind))
	}
	if strings.TrimSpace(c.ProjectID) == "" {
		ic.add(ErrEmptyProjectID, "", subj, "component project_id must not be empty")
	}
	if strings.TrimSpace(c.IconKey) == "" || (reg != nil && !reg.Has(c.IconKey)) {
		ic.add(ErrInvalidIconKey, "", subj, fmt.Sprintf("unknown or empty icon_key %q", c.IconKey))
	}
}

func validateComponentBackedBy(backed []BackedResource, subj string, ic *issueCollector) {
	if len(backed) == 0 {
		ic.add(ErrEmptyBackedBy, "", subj, "component backed_by must contain at least one resource")
		return
	}
	datastoreTypes := make(map[string]bool)
	for _, br := range backed {
		if strings.TrimSpace(br.URI) == "" || strings.TrimSpace(br.AssetType) == "" || strings.TrimSpace(br.ProjectID) == "" {
			ic.add(ErrInvalidBackedResource, "", subj, "backed_by entry requires non-empty uri, asset_type, and project_id")
			continue
		}
		if statefulDatastoreAssetTypes[br.AssetType] {
			datastoreTypes[br.AssetType] = true
		}
	}
	if len(datastoreTypes) > 1 {
		types := make([]string, 0, len(datastoreTypes))
		for dt := range datastoreTypes {
			types = append(types, dt)
		}
		sort.Strings(types)
		ic.add(ErrHeterogeneousDatastore, "", subj, fmt.Sprintf("component mixes heterogeneous stateful datastore asset types: %s", strings.Join(types, ", ")))
	}
}

func validateComponentInlineMeta(rows []InlineMeta, subj string, ic *issueCollector) {
	if len(rows) > maxInlineMetaRows {
		ic.add(ErrInlineMetaLimit, "", subj, fmt.Sprintf("inline_meta has %d rows (max %d)", len(rows), maxInlineMetaRows))
	}
	for _, meta := range rows {
		if strings.TrimSpace(meta.Label) == "" || strings.TrimSpace(meta.Value) == "" {
			ic.add(ErrInvalidInlineMeta, "", subj, "inline_meta row requires non-empty label and value")
		}
	}
}

func validateContainers(containers []Container, ic *issueCollector) map[string]Container {
	contByID := make(map[string]Container, len(containers))
	for i, ct := range containers {
		subj := ct.ID
		if strings.TrimSpace(subj) == "" {
			subj = fmt.Sprintf("containers[%d]", i)
			ic.add(ErrEmptyContainerID, "", subj, "container id must not be empty")
		} else if _, dup := contByID[ct.ID]; dup {
			ic.add(ErrDuplicateContainerID, "", ct.ID, fmt.Sprintf("duplicate container id %q", ct.ID))
		} else {
			contByID[ct.ID] = ct
		}

		if strings.TrimSpace(ct.Title) == "" {
			ic.add(ErrEmptyContainerTitle, "", subj, "container title must not be empty")
		}
		if !isValidContainerKind(ct.Kind) {
			ic.add(ErrInvalidContainerKind, "", subj, fmt.Sprintf("invalid container kind %q", ct.Kind))
		}
		for _, br := range ct.BackedBy {
			if strings.TrimSpace(br.URI) == "" || strings.TrimSpace(br.AssetType) == "" || strings.TrimSpace(br.ProjectID) == "" {
				ic.add(ErrInvalidBackedResource, "", subj, "container backed_by entry requires non-empty uri, asset_type, and project_id")
			}
		}
	}
	for _, ct := range containers {
		validateContainerParentChain(ct, contByID, ic)
	}
	return contByID
}

func validateContainerParentChain(ct Container, contByID map[string]Container, ic *issueCollector) {
	if ct.ParentContainerID == "" {
		return
	}
	if _, ok := contByID[ct.ParentContainerID]; !ok {
		ic.add(ErrUnknownParentContainer, "", ct.ID, fmt.Sprintf("parent_container_id %q does not exist", ct.ParentContainerID))
		return
	}
	seen := map[string]bool{ct.ID: true}
	cur := ct.ParentContainerID
	for cur != "" {
		if seen[cur] {
			ic.add(ErrContainerCycle, "", ct.ID, fmt.Sprintf("parent_container_id cycle detected involving %q", cur))
			return
		}
		seen[cur] = true
		parent, ok := contByID[cur]
		if !ok {
			return
		}
		cur = parent.ParentContainerID
	}
}

func validateExcludedResources(excluded []ExcludedResource, ic *issueCollector) {
	for i, ex := range excluded {
		subj := ex.URI
		if subj == "" {
			subj = fmt.Sprintf("excluded_resources[%d]", i)
		}
		if strings.TrimSpace(ex.URI) == "" || strings.TrimSpace(ex.AssetType) == "" || strings.TrimSpace(ex.ProjectID) == "" {
			ic.add(ErrInvalidExcludedResource, "", subj, "excluded_resources entry requires non-empty uri, asset_type, and project_id")
		}
		if strings.TrimSpace(ex.Reason) == "" {
			ic.add(ErrMissingExclusionReason, "", subj, "excluded_resources entry requires non-empty reason")
		}
	}
}

func validateRejectedEdges(rejected []RejectedEdge, ic *issueCollector) map[[2]string]bool {
	rejectedPairs := make(map[[2]string]bool, len(rejected))
	for i, re := range rejected {
		if strings.TrimSpace(re.From) == "" || strings.TrimSpace(re.To) == "" || strings.TrimSpace(re.Reason) == "" {
			ic.add(ErrInvalidRejectedEdge, "", fmt.Sprintf("rejected_edges[%d]", i), "rejected_edges entry requires non-empty from, to, and reason")
			continue
		}
		rejectedPairs[[2]string{re.From, re.To}] = true
	}
	return rejectedPairs
}

func validateViewIndex(views []View, ic *issueCollector) (map[string]bool, map[string]bool) {
	viewIDs := make(map[string]bool, len(views))
	detailViewIDs := make(map[string]bool, len(views))
	systemIndexIdx := -1
	for i, v := range views {
		switch {
		case strings.TrimSpace(v.ID) == "":
			ic.add(ErrEmptyViewID, "", fmt.Sprintf("views[%d]", i), "view id must not be empty")
		case viewIDs[v.ID]:
			ic.add(ErrDuplicateViewID, v.ID, v.ID, fmt.Sprintf("duplicate view id %q", v.ID))
		default:
			viewIDs[v.ID] = true
			if v.ID == SystemIndexViewID {
				systemIndexIdx = i
			} else {
				detailViewIDs[v.ID] = true
			}
		}
		if strings.TrimSpace(v.Title) == "" {
			ic.add(ErrEmptyViewTitle, v.ID, v.ID, "view title must not be empty")
		}
	}

	if systemIndexIdx > 0 {
		ic.add(ErrMisplacedSystemIndexView, SystemIndexViewID, SystemIndexViewID, fmt.Sprintf("%s must be views[0], found at views[%d]", SystemIndexViewID, systemIndexIdx))
	}
	if len(detailViewIDs) >= 2 && systemIndexIdx == -1 {
		ic.add(ErrMissingSystemIndexView, "", SystemIndexViewID, fmt.Sprintf("views[0].id must be %s when %d detail views exist", SystemIndexViewID, len(detailViewIDs)))
	}
	if len(detailViewIDs) < 2 && systemIndexIdx != -1 {
		ic.add(ErrUnexpectedSystemIndexView, SystemIndexViewID, SystemIndexViewID, fmt.Sprintf("%s must be omitted when fewer than 2 detail views exist (found %d)", SystemIndexViewID, len(detailViewIDs)))
	}
	return viewIDs, detailViewIDs
}

func validateView(
	v View,
	compByID map[string]Component,
	contByID map[string]Container,
	viewIDs map[string]bool,
	detailViewIDs map[string]bool,
	rejectedPairs map[[2]string]bool,
	placedInAnyDetailView map[string]bool,
	ic *issueCollector,
) {
	isSystemIndex := v.ID == SystemIndexViewID
	maxCol, validArch := archetypeMaxColumn(v.Archetype)
	if !validArch {
		ic.add(ErrInvalidArchetype, v.ID, string(v.Archetype), fmt.Sprintf("invalid archetype %q", v.Archetype))
	}
	if !isValidAccentMode(v.AccentMode) {
		ic.add(ErrInvalidAccentMode, v.ID, string(v.AccentMode), fmt.Sprintf("invalid accent_mode %q", v.AccentMode))
	}

	viewContSet := validateViewContainers(v, contByID, ic)
	placedInView := validateViewPlacements(v, isSystemIndex, maxCol, validArch, compByID, detailViewIDs, viewContSet, viewIDs, placedInAnyDetailView, ic)
	validateViewEdges(v, isSystemIndex, placedInView, rejectedPairs, ic)
}

func validateViewContainers(v View, contByID map[string]Container, ic *issueCollector) map[string]bool {
	viewContSet := make(map[string]bool, len(v.ContainerIDs))
	for _, cid := range v.ContainerIDs {
		if _, ok := contByID[cid]; !ok {
			ic.add(ErrUnknownContainerRef, v.ID, cid, fmt.Sprintf("container_ids references unknown container %q", cid))
			continue
		}
		viewContSet[cid] = true
	}
	for _, cid := range v.ContainerIDs {
		ct, ok := contByID[cid]
		if !ok {
			continue
		}
		if depth := viewContainerDepth(cid, ct, contByID, viewContSet); depth > maxContainerNestingDepth {
			ic.add(ErrContainerDepthExceeded, v.ID, cid, fmt.Sprintf("container %q nesting depth %d exceeds max %d", cid, depth, maxContainerNestingDepth))
		}
	}
	return viewContSet
}

func viewContainerDepth(cid string, ct Container, contByID map[string]Container, viewContSet map[string]bool) int {
	depth := 1
	seen := map[string]bool{cid: true}
	cur := ct.ParentContainerID
	for cur != "" && !seen[cur] {
		seen[cur] = true
		if viewContSet[cur] {
			depth++
		}
		parent, ok := contByID[cur]
		if !ok {
			break
		}
		cur = parent.ParentContainerID
	}
	return depth
}

func validateViewPlacements(
	v View,
	isSystemIndex bool,
	maxCol int,
	validArch bool,
	compByID map[string]Component,
	detailViewIDs map[string]bool,
	viewContSet map[string]bool,
	viewIDs map[string]bool,
	placedInAnyDetailView map[string]bool,
	ic *issueCollector,
) map[string]bool {
	maxPlacements := maxDetailViewPlacements
	if isSystemIndex {
		maxPlacements = maxSystemIndexPlacements
	}
	if len(v.Placements) < 1 || len(v.Placements) > maxPlacements {
		ic.add(ErrViewPlacementCount, v.ID, v.ID, fmt.Sprintf("view has %d placements (allowed 1..%d)", len(v.Placements), maxPlacements))
	}

	placedInView := make(map[string]bool, len(v.Placements))
	for _, p := range v.Placements {
		if placedInView[p.ComponentID] {
			ic.add(ErrDuplicatePlacement, v.ID, p.ComponentID, fmt.Sprintf("component %q placed more than once in view", p.ComponentID))
		}
		placedInView[p.ComponentID] = true

		validatePlacementTarget(v.ID, p.ComponentID, isSystemIndex, compByID, detailViewIDs, placedInAnyDetailView, ic)
		if p.ContainerID != "" && !viewContSet[p.ContainerID] {
			ic.add(ErrPlacementContainerNotInView, v.ID, p.ComponentID, fmt.Sprintf("placement container_id %q is not listed in view.container_ids", p.ContainerID))
		}
		if validArch && (p.Column < 0 || p.Column > maxCol) {
			ic.add(ErrColumnOutOfBounds, v.ID, p.ComponentID, fmt.Sprintf("placement column %d out of bounds 0..%d for archetype %q", p.Column, maxCol, v.Archetype))
		}
		validateOffPageRefs(v.ID, p, viewIDs, ic)
	}
	return placedInView
}

func validatePlacementTarget(
	viewID, compID string,
	isSystemIndex bool,
	compByID map[string]Component,
	detailViewIDs map[string]bool,
	placedInAnyDetailView map[string]bool,
	ic *issueCollector,
) {
	_, inComponents := compByID[compID]
	_, inDetailViews := detailViewIDs[compID]
	if isSystemIndex {
		if !inComponents && !inDetailViews {
			ic.add(ErrUnknownComponentRef, viewID, compID, fmt.Sprintf("system index placement %q matches neither a component nor a detail view", compID))
		}
		return
	}
	if !inComponents {
		ic.add(ErrUnknownComponentRef, viewID, compID, fmt.Sprintf("placement references unknown component %q", compID))
		return
	}
	placedInAnyDetailView[compID] = true
}

func validateOffPageRefs(viewID string, p Placement, viewIDs map[string]bool, ic *issueCollector) {
	for _, ref := range p.OffPageRefs {
		if !viewIDs[ref.TargetViewID] || ref.TargetViewID == viewID {
			ic.add(ErrInvalidOffPageRef, viewID, p.ComponentID, fmt.Sprintf("off_page_refs target_view_id %q must reference another existing view", ref.TargetViewID))
		}
		if ref.Direction != "IN" && ref.Direction != "OUT" {
			ic.add(ErrInvalidOffPageRef, viewID, p.ComponentID, fmt.Sprintf("off_page_refs direction %q must be IN or OUT", ref.Direction))
		}
		if strings.TrimSpace(ref.Label) == "" {
			ic.add(ErrInvalidOffPageRef, viewID, p.ComponentID, "off_page_refs label must not be empty")
		}
	}
}

func validateViewEdges(v View, isSystemIndex bool, placedInView map[string]bool, rejectedPairs map[[2]string]bool, ic *issueCollector) {
	type busEndpoints struct {
		froms map[string]bool
		tos   map[string]bool
	}
	buses := make(map[string]*busEndpoints)

	for _, e := range v.Edges {
		validateSingleViewEdge(v.ID, e, isSystemIndex, placedInView, rejectedPairs, ic)
		if e.ViaBusID != "" {
			b, ok := buses[e.ViaBusID]
			if !ok {
				b = &busEndpoints{
					froms: make(map[string]bool),
					tos:   make(map[string]bool),
				}
				buses[e.ViaBusID] = b
			}
			b.froms[e.From] = true
			b.tos[e.To] = true
		}
	}

	busIDs := make([]string, 0, len(buses))
	for id := range buses {
		busIDs = append(busIDs, id)
	}
	sort.Strings(busIDs)
	for _, bid := range busIDs {
		b := buses[bid]
		if len(b.froms) > 1 && len(b.tos) > 1 {
			ic.add(ErrInvalidBusGroup, v.ID, bid, fmt.Sprintf("via_bus_id %q mixes %d distinct sources and %d distinct targets (must share one from or one to)", bid, len(b.froms), len(b.tos)))
		}
	}
}

func validateSingleViewEdge(viewID string, e Edge, isSystemIndex bool, placedInView map[string]bool, rejectedPairs map[[2]string]bool, ic *issueCollector) {
	edgeLabel := e.From + " -> " + e.To
	if !placedInView[e.From] {
		ic.add(ErrEdgeEndpointNotPlaced, viewID, edgeLabel, fmt.Sprintf("edge from %q is not placed in view", e.From))
	}
	if !placedInView[e.To] {
		ic.add(ErrEdgeEndpointNotPlaced, viewID, edgeLabel, fmt.Sprintf("edge to %q is not placed in view", e.To))
	}
	if rejectedPairs[[2]string{e.From, e.To}] {
		ic.add(ErrRejectedEdgePresent, viewID, edgeLabel, fmt.Sprintf("edge (%s -> %s) is forbidden by rejected_edges", e.From, e.To))
	}
	if strings.TrimSpace(e.Verb) == "" {
		ic.add(ErrEmptyEdgeVerb, viewID, edgeLabel, "edge verb must not be empty")
	} else if utf8.RuneCountInString(e.Verb) > maxEdgeVerbRunes {
		ic.add(ErrEdgeVerbTooLong, viewID, edgeLabel, fmt.Sprintf("edge verb %q has %d chars (max %d)", e.Verb, utf8.RuneCountInString(e.Verb), maxEdgeVerbRunes))
	}
	if !isValidFlowKind(e.FlowKind) {
		ic.add(ErrInvalidFlowKind, viewID, edgeLabel, fmt.Sprintf("invalid flow_kind %q", e.FlowKind))
	}
	validateEdgeProvenanceField(viewID, edgeLabel, e, isSystemIndex, ic)
}

func validateEdgeProvenanceField(viewID, edgeLabel string, e Edge, isSystemIndex bool, ic *issueCollector) {
	switch e.Provenance {
	case ProvenanceVerified:
		if isSystemIndex {
			return
		}
		if len(e.EvidenceEdgeIDs) == 0 {
			ic.add(ErrMissingEvidenceEdgeIDs, viewID, edgeLabel, "VERIFIED edge on detail view requires non-empty evidence_edge_ids")
			return
		}
		for _, eid := range e.EvidenceEdgeIDs {
			if strings.TrimSpace(eid) == "" {
				ic.add(ErrMissingEvidenceEdgeIDs, viewID, edgeLabel, "VERIFIED edge evidence_edge_ids must not contain empty entries")
				return
			}
		}
	case ProvenanceInferred:
		if strings.TrimSpace(e.InferenceRationale) == "" {
			ic.add(ErrMissingInferenceRationale, viewID, edgeLabel, "INFERRED edge requires non-empty inference_rationale")
		}
	default:
		ic.add(ErrInvalidProvenance, viewID, edgeLabel, fmt.Sprintf("invalid provenance %q", e.Provenance))
	}
}

func validateAllComponentsPlaced(compByID map[string]Component, placedInAnyDetailView map[string]bool, ic *issueCollector) {
	var unplaced []string
	for id := range compByID {
		if !placedInAnyDetailView[id] {
			unplaced = append(unplaced, id)
		}
	}
	sort.Strings(unplaced)
	for _, id := range unplaced {
		ic.add(ErrUnplacedComponent, "", id, fmt.Sprintf("component %q is not placed in any detail view", id))
	}
}

func backedURISet(backed []BackedResource) map[string]bool {
	out := make(map[string]bool, len(backed))
	for _, br := range backed {
		out[br.URI] = true
	}
	return out
}

// isValidDirectedWalk checks that the subgraph induced by edges is non-empty and that every cited
// edge lies on a directed walk starting at fromURIs and ending at toURIs.
func isValidDirectedWalk(edges []extract.FactEdge, fromURIs, toURIs map[string]bool) bool {
	if len(edges) == 0 {
		return false
	}
	fwdAdj := make(map[string][]string, len(edges))
	revAdj := make(map[string][]string, len(edges))
	for _, fe := range edges {
		fwdAdj[fe.FromURI] = append(fwdAdj[fe.FromURI], fe.ToURI)
		revAdj[fe.ToURI] = append(revAdj[fe.ToURI], fe.FromURI)
	}

	fwdReach := bfsReachable(fromURIs, fwdAdj)
	bwdReach := bfsReachable(toURIs, revAdj)

	for _, fe := range edges {
		if !fwdReach[fe.FromURI] || !bwdReach[fe.ToURI] {
			return false
		}
	}
	return true
}

func bfsReachable(seeds map[string]bool, adj map[string][]string) map[string]bool {
	visited := make(map[string]bool, len(seeds))
	queue := make([]string, 0, len(seeds))
	for uri := range seeds {
		visited[uri] = true
		queue = append(queue, uri)
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, nxt := range adj[cur] {
			if !visited[nxt] {
				visited[nxt] = true
				queue = append(queue, nxt)
			}
		}
	}
	return visited
}

func isValidComponentKind(k ComponentKind) bool {
	switch k {
	case KindExternalCaller, KindIngress, KindWorkload, KindPipeline,
		KindMessaging, KindDatastore, KindCohort, KindExternalTarget:
		return true
	}
	return false
}

func isValidContainerKind(k ContainerKind) bool {
	switch k {
	case ContainerProject, ContainerVPC, ContainerGKECluster,
		ContainerPSAZone, ContainerExternalZone, ContainerSharedHub:
		return true
	}
	return false
}

func archetypeMaxColumn(a Archetype) (int, bool) {
	switch a {
	case ArchetypeLandscapeMatrix, ArchetypeStagePipeline:
		return 4, true
	case ArchetypeHubSpoke:
		return 2, true
	}
	return 0, false
}

func isValidAccentMode(a AccentMode) bool {
	switch a {
	case AccentRuntime, AccentCICD, AccentMixed:
		return true
	}
	return false
}

func isValidFlowKind(k FlowKind) bool {
	switch k {
	case FlowRuntime, FlowCICD, FlowAdmin:
		return true
	}
	return false
}
