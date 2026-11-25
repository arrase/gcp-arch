package manifest

import (
	"fmt"
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
	accounted := collectAccountedURIs(m, len(facts.Resources))

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

func collectAccountedURIs(m *Manifest, capHint int) map[string]bool {
	accounted := make(map[string]bool, capHint)
	for _, c := range m.Components {
		markBackedURIs(accounted, c.BackedBy)
	}
	for _, ct := range m.Containers {
		markBackedURIs(accounted, ct.BackedBy)
	}
	for _, ex := range m.ExcludedResources {
		if ex.URI != "" {
			accounted[ex.URI] = true
		}
	}
	return accounted
}

func markBackedURIs(dst map[string]bool, backed []BackedResource) {
	for _, br := range backed {
		if br.URI != "" {
			dst[br.URI] = true
		}
	}
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

type structureValidator struct {
	issues                []ValidationIssue
	compByID              map[string]Component
	contByID              map[string]Container
	viewIDs               map[string]bool
	detailViewIDs         map[string]bool
	rejectedPairs         map[[2]string]bool
	placedInAnyDetailView map[string]bool
}

func (sv *structureValidator) add(code, viewID, subject, msg string) {
	sv.issues = append(sv.issues, ValidationIssue{
		Code:    code,
		ViewID:  viewID,
		Subject: subject,
		Message: msg,
	})
}

func validateStructureIssues(m *Manifest, reg *icons.Registry) []ValidationIssue {
	var sv structureValidator
	if m == nil {
		sv.add(ErrInvalidSchemaVersion, "", "", "manifest must not be nil")
		return sv.issues
	}
	if m.SchemaVersion != SchemaVersion {
		sv.add(ErrInvalidSchemaVersion, "", m.SchemaVersion, fmt.Sprintf("unsupported schema_version %q (want %q)", m.SchemaVersion, SchemaVersion))
	}

	sv.compByID = sv.validateComponents(m.Components, reg)
	sv.contByID = sv.validateContainers(m.Containers)
	sv.validateExcludedResources(m.ExcludedResources)
	sv.rejectedPairs = sv.validateRejectedEdges(m.RejectedEdges)

	if len(m.Views) == 0 {
		sv.add(ErrNoViews, "", "", "manifest must define at least one view")
		return sv.issues
	}

	sv.viewIDs, sv.detailViewIDs = sv.validateViewIndex(m.Views)
	sv.placedInAnyDetailView = make(map[string]bool, len(sv.compByID))
	for _, v := range m.Views {
		sv.validateView(v)
	}
	sv.validateAllComponentsPlaced()
	return sv.issues
}

func (sv *structureValidator) validateComponents(components []Component, reg *icons.Registry) map[string]Component {
	compByID := make(map[string]Component, len(components))
	for i, c := range components {
		subj := c.ID
		if strings.TrimSpace(subj) == "" {
			subj = fmt.Sprintf("components[%d]", i)
			sv.add(ErrEmptyComponentID, "", subj, "component id must not be empty")
		} else if _, dup := compByID[c.ID]; dup {
			sv.add(ErrDuplicateComponentID, "", c.ID, fmt.Sprintf("duplicate component id %q", c.ID))
		} else {
			compByID[c.ID] = c
		}

		sv.validateComponentFields(c, subj, reg)
		sv.validateComponentBackedBy(c.BackedBy, subj)
		sv.validateComponentInlineMeta(c.InlineMeta, subj)
	}
	return compByID
}

func (sv *structureValidator) validateComponentFields(c Component, subj string, reg *icons.Registry) {
	if strings.TrimSpace(c.Title) == "" {
		sv.add(ErrEmptyComponentTitle, "", subj, "component title must not be empty")
	}
	if !isValidComponentKind(c.ComponentKind) {
		sv.add(ErrInvalidComponentKind, "", subj, fmt.Sprintf("invalid component_kind %q", c.ComponentKind))
	}
	if strings.TrimSpace(c.ProjectID) == "" {
		sv.add(ErrEmptyProjectID, "", subj, "component project_id must not be empty")
	}
	if strings.TrimSpace(c.IconKey) == "" || (reg != nil && !reg.Has(c.IconKey)) {
		sv.add(ErrInvalidIconKey, "", subj, fmt.Sprintf("unknown or empty icon_key %q", c.IconKey))
	}
}

func (sv *structureValidator) validateComponentBackedBy(backed []BackedResource, subj string) {
	if len(backed) == 0 {
		sv.add(ErrEmptyBackedBy, "", subj, "component backed_by must contain at least one resource")
		return
	}
	datastoreTypes := make(map[string]bool)
	for _, br := range backed {
		if !hasBackedResourceFields(br) {
			sv.add(ErrInvalidBackedResource, "", subj, "backed_by entry requires non-empty uri, asset_type, and project_id")
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
		sv.add(ErrHeterogeneousDatastore, "", subj, fmt.Sprintf("component mixes heterogeneous stateful datastore asset types: %s", strings.Join(types, ", ")))
	}
}

func (sv *structureValidator) validateComponentInlineMeta(rows []InlineMeta, subj string) {
	if len(rows) > maxInlineMetaRows {
		sv.add(ErrInlineMetaLimit, "", subj, fmt.Sprintf("inline_meta has %d rows (max %d)", len(rows), maxInlineMetaRows))
	}
	for _, meta := range rows {
		if strings.TrimSpace(meta.Label) == "" || strings.TrimSpace(meta.Value) == "" {
			sv.add(ErrInvalidInlineMeta, "", subj, "inline_meta row requires non-empty label and value")
		}
	}
}

func (sv *structureValidator) validateContainers(containers []Container) map[string]Container {
	contByID := make(map[string]Container, len(containers))
	for i, ct := range containers {
		subj := ct.ID
		if strings.TrimSpace(subj) == "" {
			subj = fmt.Sprintf("containers[%d]", i)
			sv.add(ErrEmptyContainerID, "", subj, "container id must not be empty")
		} else if _, dup := contByID[ct.ID]; dup {
			sv.add(ErrDuplicateContainerID, "", ct.ID, fmt.Sprintf("duplicate container id %q", ct.ID))
		} else {
			contByID[ct.ID] = ct
		}
		sv.validateContainerFields(ct, subj)
	}
	for _, ct := range containers {
		sv.validateContainerParentChain(ct, contByID)
	}
	return contByID
}

func (sv *structureValidator) validateContainerFields(ct Container, subj string) {
	if strings.TrimSpace(ct.Title) == "" {
		sv.add(ErrEmptyContainerTitle, "", subj, "container title must not be empty")
	}
	if !isValidContainerKind(ct.Kind) {
		sv.add(ErrInvalidContainerKind, "", subj, fmt.Sprintf("invalid container kind %q", ct.Kind))
	}
	for _, br := range ct.BackedBy {
		if !hasBackedResourceFields(br) {
			sv.add(ErrInvalidBackedResource, "", subj, "container backed_by entry requires non-empty uri, asset_type, and project_id")
		}
	}
}

func hasBackedResourceFields(br BackedResource) bool {
	return strings.TrimSpace(br.URI) != "" && strings.TrimSpace(br.AssetType) != "" && strings.TrimSpace(br.ProjectID) != ""
}

func (sv *structureValidator) validateContainerParentChain(ct Container, contByID map[string]Container) {
	if ct.ParentContainerID == "" {
		return
	}
	if _, ok := contByID[ct.ParentContainerID]; !ok {
		sv.add(ErrUnknownParentContainer, "", ct.ID, fmt.Sprintf("parent_container_id %q does not exist", ct.ParentContainerID))
		return
	}
	seen := map[string]bool{ct.ID: true}
	cur := ct.ParentContainerID
	for cur != "" {
		if seen[cur] {
			sv.add(ErrContainerCycle, "", ct.ID, fmt.Sprintf("parent_container_id cycle detected involving %q", cur))
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

func (sv *structureValidator) validateExcludedResources(excluded []ExcludedResource) {
	for i, ex := range excluded {
		subj := ex.URI
		if subj == "" {
			subj = fmt.Sprintf("excluded_resources[%d]", i)
		}
		if strings.TrimSpace(ex.URI) == "" || strings.TrimSpace(ex.AssetType) == "" || strings.TrimSpace(ex.ProjectID) == "" {
			sv.add(ErrInvalidExcludedResource, "", subj, "excluded_resources entry requires non-empty uri, asset_type, and project_id")
		}
		if strings.TrimSpace(ex.Reason) == "" {
			sv.add(ErrMissingExclusionReason, "", subj, "excluded_resources entry requires non-empty reason")
		}
	}
}

func (sv *structureValidator) validateRejectedEdges(rejected []RejectedEdge) map[[2]string]bool {
	rejectedPairs := make(map[[2]string]bool, len(rejected))
	for i, re := range rejected {
		if strings.TrimSpace(re.From) == "" || strings.TrimSpace(re.To) == "" || strings.TrimSpace(re.Reason) == "" {
			sv.add(ErrInvalidRejectedEdge, "", fmt.Sprintf("rejected_edges[%d]", i), "rejected_edges entry requires non-empty from, to, and reason")
			continue
		}
		rejectedPairs[[2]string{re.From, re.To}] = true
	}
	return rejectedPairs
}

func (sv *structureValidator) validateViewIndex(views []View) (map[string]bool, map[string]bool) {
	viewIDs := make(map[string]bool, len(views))
	detailViewIDs := make(map[string]bool, len(views))
	systemIndexIdx := -1
	for i, v := range views {
		switch {
		case strings.TrimSpace(v.ID) == "":
			sv.add(ErrEmptyViewID, "", fmt.Sprintf("views[%d]", i), "view id must not be empty")
		case viewIDs[v.ID]:
			sv.add(ErrDuplicateViewID, v.ID, v.ID, fmt.Sprintf("duplicate view id %q", v.ID))
		default:
			viewIDs[v.ID] = true
			if v.ID == SystemIndexViewID {
				systemIndexIdx = i
			} else {
				detailViewIDs[v.ID] = true
			}
		}
		if strings.TrimSpace(v.Title) == "" {
			sv.add(ErrEmptyViewTitle, v.ID, v.ID, "view title must not be empty")
		}
	}
	sv.validateSystemIndexPosition(systemIndexIdx, len(detailViewIDs))
	return viewIDs, detailViewIDs
}

func (sv *structureValidator) validateSystemIndexPosition(systemIndexIdx, detailCount int) {
	if systemIndexIdx > 0 {
		sv.add(ErrMisplacedSystemIndexView, SystemIndexViewID, SystemIndexViewID, fmt.Sprintf("%s must be views[0], found at views[%d]", SystemIndexViewID, systemIndexIdx))
	}
	if detailCount >= 2 && systemIndexIdx == -1 {
		sv.add(ErrMissingSystemIndexView, "", SystemIndexViewID, fmt.Sprintf("views[0].id must be %s when %d detail views exist", SystemIndexViewID, detailCount))
	}
	if detailCount < 2 && systemIndexIdx != -1 {
		sv.add(ErrUnexpectedSystemIndexView, SystemIndexViewID, SystemIndexViewID, fmt.Sprintf("%s must be omitted when fewer than 2 detail views exist (found %d)", SystemIndexViewID, detailCount))
	}
}

func (sv *structureValidator) validateView(v View) {
	isSystemIndex := v.ID == SystemIndexViewID
	maxCol, validArch := archetypeMaxColumn(v.Archetype)
	if !validArch {
		sv.add(ErrInvalidArchetype, v.ID, string(v.Archetype), fmt.Sprintf("invalid archetype %q", v.Archetype))
	}
	if !isValidAccentMode(v.AccentMode) {
		sv.add(ErrInvalidAccentMode, v.ID, string(v.AccentMode), fmt.Sprintf("invalid accent_mode %q", v.AccentMode))
	}

	viewContSet := sv.validateViewContainers(v)
	placedInView := sv.validateViewPlacements(v, isSystemIndex, validArch, maxCol, viewContSet)
	sv.validateViewEdges(v, isSystemIndex, placedInView)
}

func (sv *structureValidator) validateViewContainers(v View) map[string]bool {
	viewContSet := make(map[string]bool, len(v.ContainerIDs))
	for _, cid := range v.ContainerIDs {
		if _, ok := sv.contByID[cid]; !ok {
			sv.add(ErrUnknownContainerRef, v.ID, cid, fmt.Sprintf("container_ids references unknown container %q", cid))
			continue
		}
		viewContSet[cid] = true
	}
	for _, cid := range v.ContainerIDs {
		ct, ok := sv.contByID[cid]
		if !ok {
			continue
		}
		if depth := viewContainerDepth(cid, ct, sv.contByID, viewContSet); depth > maxContainerNestingDepth {
			sv.add(ErrContainerDepthExceeded, v.ID, cid, fmt.Sprintf("container %q nesting depth %d exceeds max %d", cid, depth, maxContainerNestingDepth))
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

func (sv *structureValidator) validateViewPlacements(
	v View,
	isSystemIndex, validArch bool,
	maxCol int,
	viewContSet map[string]bool,
) map[string]bool {
	sv.validatePlacementCount(v.ID, len(v.Placements), isSystemIndex)

	placedInView := make(map[string]bool, len(v.Placements))
	for _, p := range v.Placements {
		if placedInView[p.ComponentID] {
			sv.add(ErrDuplicatePlacement, v.ID, p.ComponentID, fmt.Sprintf("component %q placed more than once in view", p.ComponentID))
		}
		placedInView[p.ComponentID] = true

		sv.validatePlacementTarget(v.ID, p.ComponentID, isSystemIndex)
		if p.ContainerID != "" && !viewContSet[p.ContainerID] {
			sv.add(ErrPlacementContainerNotInView, v.ID, p.ComponentID, fmt.Sprintf("placement container_id %q is not listed in view.container_ids", p.ContainerID))
		}
		if validArch && (p.Column < 0 || p.Column > maxCol) {
			sv.add(ErrColumnOutOfBounds, v.ID, p.ComponentID, fmt.Sprintf("placement column %d out of bounds 0..%d for archetype %q", p.Column, maxCol, v.Archetype))
		}
		sv.validateOffPageRefs(v.ID, p)
	}
	return placedInView
}

func (sv *structureValidator) validatePlacementCount(viewID string, count int, isSystemIndex bool) {
	maxPlacements := maxDetailViewPlacements
	if isSystemIndex {
		maxPlacements = maxSystemIndexPlacements
	}
	if count < 1 || count > maxPlacements {
		sv.add(ErrViewPlacementCount, viewID, viewID, fmt.Sprintf("view has %d placements (allowed 1..%d)", count, maxPlacements))
	}
}

func (sv *structureValidator) validatePlacementTarget(viewID, compID string, isSystemIndex bool) {
	_, inComponents := sv.compByID[compID]
	_, inDetailViews := sv.detailViewIDs[compID]
	if isSystemIndex {
		if !inComponents && !inDetailViews {
			sv.add(ErrUnknownComponentRef, viewID, compID, fmt.Sprintf("system index placement %q matches neither a component nor a detail view", compID))
		}
		return
	}
	if !inComponents {
		sv.add(ErrUnknownComponentRef, viewID, compID, fmt.Sprintf("placement references unknown component %q", compID))
		return
	}
	sv.placedInAnyDetailView[compID] = true
}

func (sv *structureValidator) validateOffPageRefs(viewID string, p Placement) {
	for _, ref := range p.OffPageRefs {
		if !sv.viewIDs[ref.TargetViewID] || ref.TargetViewID == viewID {
			sv.add(ErrInvalidOffPageRef, viewID, p.ComponentID, fmt.Sprintf("off_page_refs target_view_id %q must reference another existing view", ref.TargetViewID))
		}
		if ref.Direction != "IN" && ref.Direction != "OUT" {
			sv.add(ErrInvalidOffPageRef, viewID, p.ComponentID, fmt.Sprintf("off_page_refs direction %q must be IN or OUT", ref.Direction))
		}
		if strings.TrimSpace(ref.Label) == "" {
			sv.add(ErrInvalidOffPageRef, viewID, p.ComponentID, "off_page_refs label must not be empty")
		}
	}
}

func (sv *structureValidator) validateViewEdges(v View, isSystemIndex bool, placedInView map[string]bool) {
	type busEndpoints struct {
		froms map[string]bool
		tos   map[string]bool
	}
	buses := make(map[string]*busEndpoints)

	for _, e := range v.Edges {
		sv.validateSingleViewEdge(v.ID, e, isSystemIndex, placedInView)
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
			sv.add(ErrInvalidBusGroup, v.ID, bid, fmt.Sprintf("via_bus_id %q mixes %d distinct sources and %d distinct targets (must share one from or one to)", bid, len(b.froms), len(b.tos)))
		}
	}
}

func (sv *structureValidator) validateSingleViewEdge(viewID string, e Edge, isSystemIndex bool, placedInView map[string]bool) {
	edgeLabel := e.From + " -> " + e.To
	if !placedInView[e.From] {
		sv.add(ErrEdgeEndpointNotPlaced, viewID, edgeLabel, fmt.Sprintf("edge from %q is not placed in view", e.From))
	}
	if !placedInView[e.To] {
		sv.add(ErrEdgeEndpointNotPlaced, viewID, edgeLabel, fmt.Sprintf("edge to %q is not placed in view", e.To))
	}
	if sv.rejectedPairs[[2]string{e.From, e.To}] {
		sv.add(ErrRejectedEdgePresent, viewID, edgeLabel, fmt.Sprintf("edge (%s -> %s) is forbidden by rejected_edges", e.From, e.To))
	}
	if strings.TrimSpace(e.Verb) == "" {
		sv.add(ErrEmptyEdgeVerb, viewID, edgeLabel, "edge verb must not be empty")
	} else if utf8.RuneCountInString(e.Verb) > maxEdgeVerbRunes {
		sv.add(ErrEdgeVerbTooLong, viewID, edgeLabel, fmt.Sprintf("edge verb %q has %d chars (max %d)", e.Verb, utf8.RuneCountInString(e.Verb), maxEdgeVerbRunes))
	}
	if !isValidFlowKind(e.FlowKind) {
		sv.add(ErrInvalidFlowKind, viewID, edgeLabel, fmt.Sprintf("invalid flow_kind %q", e.FlowKind))
	}
	sv.validateEdgeProvenanceField(viewID, edgeLabel, e, isSystemIndex)
}

func (sv *structureValidator) validateEdgeProvenanceField(viewID, edgeLabel string, e Edge, isSystemIndex bool) {
	switch e.Provenance {
	case ProvenanceVerified:
		if isSystemIndex {
			return
		}
		if len(e.EvidenceEdgeIDs) == 0 {
			sv.add(ErrMissingEvidenceEdgeIDs, viewID, edgeLabel, "VERIFIED edge on detail view requires non-empty evidence_edge_ids")
			return
		}
		for _, eid := range e.EvidenceEdgeIDs {
			if strings.TrimSpace(eid) == "" {
				sv.add(ErrMissingEvidenceEdgeIDs, viewID, edgeLabel, "VERIFIED edge evidence_edge_ids must not contain empty entries")
				return
			}
		}
	case ProvenanceInferred:
		if strings.TrimSpace(e.InferenceRationale) == "" {
			sv.add(ErrMissingInferenceRationale, viewID, edgeLabel, "INFERRED edge requires non-empty inference_rationale")
		}
	default:
		sv.add(ErrInvalidProvenance, viewID, edgeLabel, fmt.Sprintf("invalid provenance %q", e.Provenance))
	}
}

func (sv *structureValidator) validateAllComponentsPlaced() {
	var unplaced []string
	for id := range sv.compByID {
		if !sv.placedInAnyDetailView[id] {
			unplaced = append(unplaced, id)
		}
	}
	sort.Strings(unplaced)
	for _, id := range unplaced {
		sv.add(ErrUnplacedComponent, "", id, fmt.Sprintf("component %q is not placed in any detail view", id))
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
