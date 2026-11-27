package manifest

import (
	"fmt"
	"maps"
	"slices"
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
	ErrExcludedDatastore           = "EXCLUDED_DATASTORE"
	ErrDuplicateAccountedURI       = "DUPLICATE_ACCOUNTED_URI"
	ErrTooManyDetailViews          = "TOO_MANY_DETAIL_VIEWS"
	ErrViewComponentIDCollision    = "VIEW_COMPONENT_ID_COLLISION"
	ErrUnknownPairComponent        = "UNKNOWN_PAIR_COMPONENT"
	ErrInconsistentCrossViewEdge   = "INCONSISTENT_CROSS_VIEW_EDGE"
	ErrPlacementContainerMismatch  = "PLACEMENT_CONTAINER_MISMATCH"
)

const (
	maxInlineMetaRows = 4
	maxDetailViews    = 16
	maxEdgeVerbRunes  = 16
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
	issues, ok := validateBaseWithFactsPrecheck(m, facts, reg)
	if !ok {
		return &ValidationError{Issues: issues}
	}
	evIdx := NewEvidenceIndex(m.Components, m.Containers, facts.Edges)
	issues = append(issues, validateFactsIssues(m, facts, evIdx)...)
	if len(issues) > 0 {
		return &ValidationError{Issues: issues}
	}
	return nil
}

// ValidateWithFactsAndConservation runs ValidateWithFacts and VerifyConservation sharing a single
// EvidenceIndex build. Returns the conservation report alongside any *ValidationError.
func ValidateWithFactsAndConservation(m *Manifest, facts *extract.FactsFile, reg *icons.Registry) (ConservationReport, error) {
	issues, ok := validateBaseWithFactsPrecheck(m, facts, reg)
	if !ok {
		return failConservationReport(), &ValidationError{Issues: issues}
	}
	evIdx := NewEvidenceIndex(m.Components, m.Containers, facts.Edges)
	issues = append(issues, validateFactsIssues(m, facts, evIdx)...)
	rep := verifyConservationWithIndex(m, facts, evIdx)
	if len(issues) > 0 {
		return rep, &ValidationError{Issues: issues}
	}
	return rep, nil
}

func validateBaseWithFactsPrecheck(m *Manifest, facts *extract.FactsFile, reg *icons.Registry) ([]ValidationIssue, bool) {
	issues := validateStructureIssues(m, reg)
	if facts == nil {
		issues = append(issues, ValidationIssue{
			Code:    ErrNilFacts,
			Message: "facts file must not be nil",
		})
		return issues, false
	}
	if m == nil {
		return issues, false
	}
	return issues, true
}

func validateFactsIssues(m *Manifest, facts *extract.FactsFile, evIdx *EvidenceIndex) []ValidationIssue {
	var issues []ValidationIssue
	issues = append(issues, validateFactResourceAccounting(m, facts)...)
	issues = append(issues, validateFactEdgeProvenance(m, evIdx)...)
	issues = append(issues, validatePlacementContainers(m, facts)...)
	return issues
}

// validatePlacementContainers rejects placements inside a resource-backed container (VPC, GKE
// cluster) when the component's resources declare a parent_uri chain that never reaches it.
func validatePlacementContainers(m *Manifest, facts *extract.FactsFile) []ValidationIssue {
	parentOf := ParentIndex(facts)
	contURIsByID := make(map[string]map[string]bool, len(m.Containers))
	for _, ct := range m.Containers {
		if len(ct.BackedBy) > 0 {
			contURIsByID[ct.ID] = backedURISet(ct.BackedBy)
		}
	}
	compByID := make(map[string]Component, len(m.Components))
	for _, c := range m.Components {
		compByID[c.ID] = c
	}

	var issues []ValidationIssue
	for _, v := range m.Views {
		for _, p := range v.Placements {
			comp, okComp := compByID[p.ComponentID]
			if !okComp {
				continue
			}
			if containerURIs := contURIsByID[p.ContainerID]; len(containerURIs) > 0 && !componentInsideBackedURIs(comp, containerURIs, parentOf) {
				issues = append(issues, ValidationIssue{
					Code:    ErrPlacementContainerMismatch,
					ViewID:  v.ID,
					Subject: p.ComponentID,
					Message: fmt.Sprintf("component %q is placed in container %q but none of its resources has that container's resource in its parent_uri chain", p.ComponentID, p.ContainerID),
				})
			}
		}
	}
	return issues
}

// ParentIndex maps each facts resource URI to its parent_uri.
func ParentIndex(facts *extract.FactsFile) map[string]string {
	parentOf := make(map[string]string, len(facts.Resources))
	for _, r := range facts.Resources {
		if r.ParentURI != "" {
			parentOf[r.URI] = r.ParentURI
		}
	}
	return parentOf
}

// ComponentInsideContainer reports whether c may be drawn inside ct. Containers without backing
// resources (PROJECT, PSA_ZONE, EXTERNAL_ZONE, ...) accept any component; so do components whose
// resources carry no parent_uri information, since they cannot be checked.
func ComponentInsideContainer(c Component, ct Container, parentOf map[string]string) bool {
	if len(ct.BackedBy) == 0 {
		return true
	}
	return componentInsideBackedURIs(c, backedURISet(ct.BackedBy), parentOf)
}

func componentInsideBackedURIs(c Component, containerURIs map[string]bool, parentOf map[string]string) bool {
	hasParentInfo := false
	for _, br := range c.BackedBy {
		if containerURIs[br.URI] {
			return true
		}
		seen := map[string]bool{br.URI: true}
		for cur, ok := parentOf[br.URI]; ok && !seen[cur]; cur, ok = parentOf[cur] {
			hasParentInfo = true
			if containerURIs[cur] {
				return true
			}
			seen[cur] = true
		}
	}
	return !hasParentInfo
}

func forEachAccountedURI(m *Manifest, fn func(uri, owner string)) {
	for _, c := range m.Components {
		for _, br := range c.BackedBy {
			if br.URI != "" {
				fn(br.URI, "component "+c.ID)
			}
		}
	}
	for _, ct := range m.Containers {
		for _, br := range ct.BackedBy {
			if br.URI != "" {
				fn(br.URI, "container "+ct.ID)
			}
		}
	}
	for _, ti := range m.TransversalItems {
		for _, br := range ti.BackedBy {
			if br.URI != "" {
				fn(br.URI, "transversal "+ti.ID)
			}
		}
	}
	for _, ex := range m.ExcludedResources {
		if ex.URI != "" {
			fn(ex.URI, "excluded_resources")
		}
	}
}

func validateFactResourceAccounting(m *Manifest, facts *extract.FactsFile) []ValidationIssue {
	accounted := make(map[string]bool, len(facts.Resources))
	forEachAccountedURI(m, func(uri, _ string) {
		accounted[uri] = true
	})

	var unaccounted []string
	for _, r := range facts.Resources {
		if !accounted[r.URI] {
			unaccounted = append(unaccounted, r.URI)
		}
	}
	slices.Sort(unaccounted)

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

func validateFactEdgeProvenance(m *Manifest, idx *EvidenceIndex) []ValidationIssue {
	var issues []ValidationIssue
	for _, v := range m.Views {
		for _, e := range v.Edges {
			if v.ID == SystemIndexViewID && len(e.EvidenceEdgeIDs) == 0 && v.Archetype != ArchetypeLayeredFlow {
				continue
			}
			if code, msg := idx.WalkIssue(e); code != "" {
				issues = append(issues, ValidationIssue{
					Code:    code,
					ViewID:  v.ID,
					Subject: e.From + " -> " + e.To,
					Message: "VERIFIED edge " + e.From + " -> " + e.To + ": " + msg,
				})
			}
		}
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
	placedInView0         map[string]bool
	placedInAnyDetailView map[string]bool
	crossViewEdges        map[crossViewEdgeKey]crossViewEdge
}

type crossViewEdgeKey struct {
	from, to string
	flowKind FlowKind
}

type crossViewEdge struct {
	viewID     string
	verb       string
	provenance Provenance
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
	sv.validateAccountedURIUniqueness(m)
	sv.rejectedPairs = sv.validateRejectedEdges(m.RejectedEdges)
	sv.validateAcknowledgedEdges(m.AcknowledgedEdges)

	if len(m.Views) == 0 {
		sv.add(ErrNoViews, "", "", "manifest must define at least one view")
		return sv.issues
	}

	sv.viewIDs, sv.detailViewIDs = sv.validateViewIndex(m.Views)
	sv.placedInView0 = make(map[string]bool, len(sv.compByID))
	sv.placedInAnyDetailView = make(map[string]bool, len(sv.compByID))
	sv.crossViewEdges = make(map[crossViewEdgeKey]crossViewEdge)
	for _, v := range m.Views {
		sv.validateView(v)
	}
	sv.validateAllComponentsPlaced()
	return sv.issues
}

// validateAccountedURIUniqueness requires every URI to be accounted exactly once across
// components[].backed_by, containers[].backed_by, transversal_items[].backed_by, and excluded_resources.
func (sv *structureValidator) validateAccountedURIUniqueness(m *Manifest) {
	owners := make(map[string][]string)
	forEachAccountedURI(m, func(uri, owner string) {
		owners[uri] = append(owners[uri], owner)
	})
	for _, uri := range slices.Sorted(maps.Keys(owners)) {
		if len(owners[uri]) > 1 {
			sv.add(ErrDuplicateAccountedURI, "", uri, fmt.Sprintf("resource %q is accounted more than once (%s); each URI must appear exactly once", uri, strings.Join(owners[uri], ", ")))
		}
	}
}

func (sv *structureValidator) validateAcknowledgedEdges(pairs []ComponentPair) {
	for i, p := range pairs {
		sv.validatePairComponents(fmt.Sprintf("acknowledged_edges[%d]", i), p.From, p.To)
	}
}

func (sv *structureValidator) validatePairComponents(subj, from, to string) {
	for _, id := range []string{from, to} {
		if _, ok := sv.compByID[id]; !ok {
			sv.add(ErrUnknownPairComponent, "", subj, fmt.Sprintf("%s references unknown component %q", subj, id))
		}
	}
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
		types := slices.Sorted(maps.Keys(datastoreTypes))
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
		if statefulDatastoreAssetTypes[ex.AssetType] {
			sv.add(ErrExcludedDatastore, "", subj, fmt.Sprintf("stateful datastore %q (%s) must be diagrammed in a component (fold several into a COHORT if needed), not excluded", ex.URI, ex.AssetType))
		}
	}
}

func (sv *structureValidator) validateRejectedEdges(rejected []RejectedEdge) map[[2]string]bool {
	rejectedPairs := make(map[[2]string]bool, len(rejected))
	for i, re := range rejected {
		subj := fmt.Sprintf("rejected_edges[%d]", i)
		if strings.TrimSpace(re.From) == "" || strings.TrimSpace(re.To) == "" || strings.TrimSpace(re.Reason) == "" {
			sv.add(ErrInvalidRejectedEdge, "", subj, "rejected_edges entry requires non-empty from, to, and reason")
			continue
		}
		sv.validatePairComponents(subj, re.From, re.To)
		rejectedPairs[[2]string{re.From, re.To}] = true
	}
	return rejectedPairs
}

func (sv *structureValidator) validateViewIndex(views []View) (map[string]bool, map[string]bool) {
	viewIDs := make(map[string]bool, len(views))
	detailViewIDs := make(map[string]bool, len(views))
	view0Idx := -1
	view0ID := ""
	for i, v := range views {
		switch {
		case strings.TrimSpace(v.ID) == "":
			sv.add(ErrEmptyViewID, "", fmt.Sprintf("views[%d]", i), "view id must not be empty")
		case viewIDs[v.ID]:
			sv.add(ErrDuplicateViewID, v.ID, v.ID, fmt.Sprintf("duplicate view id %q", v.ID))
		default:
			viewIDs[v.ID] = true
			if v.ID == SystemIndexViewID {
				view0Idx = i
				view0ID = v.ID
			} else {
				detailViewIDs[v.ID] = true
			}
		}
		if strings.TrimSpace(v.Title) == "" {
			sv.add(ErrEmptyViewTitle, v.ID, v.ID, "view title must not be empty")
		}
		if _, clash := sv.compByID[v.ID]; clash {
			sv.add(ErrViewComponentIDCollision, v.ID, v.ID, fmt.Sprintf("view id %q collides with a component id; View 0 placements reference both namespaces", v.ID))
		}
	}
	if len(detailViewIDs) > maxDetailViews {
		sv.add(ErrTooManyDetailViews, "", "", fmt.Sprintf("manifest has %d detail views (max %d so View 0 can index every view)", len(detailViewIDs), maxDetailViews))
	}
	sv.validateSystemIndexPosition(view0Idx, view0ID, len(detailViewIDs))
	return viewIDs, detailViewIDs
}

func (sv *structureValidator) validateSystemIndexPosition(view0Idx int, view0ID string, detailCount int) {
	if view0Idx > 0 {
		sv.add(ErrMisplacedSystemIndexView, view0ID, view0ID, fmt.Sprintf("%s must be views[0], found at views[%d]", view0ID, view0Idx))
	}
	if detailCount >= 2 && view0Idx == -1 {
		sv.add(ErrMissingSystemIndexView, "", SystemIndexViewID, fmt.Sprintf("views[0].id must be %s when %d detail views exist", SystemIndexViewID, detailCount))
	}
	if detailCount == 1 && view0ID == SystemIndexViewID {
		sv.add(ErrUnexpectedSystemIndexView, SystemIndexViewID, SystemIndexViewID, fmt.Sprintf("%s must be omitted when only 1 detail view exists", SystemIndexViewID))
	}
}

func (sv *structureValidator) validateView(v View) {
	isView0 := v.ID == SystemIndexViewID
	maxCol, validArch := archetypeMaxColumn(v.Archetype)
	if !validArch {
		sv.add(ErrInvalidArchetype, v.ID, string(v.Archetype), fmt.Sprintf("invalid archetype %q", v.Archetype))
	}
	if !isValidAccentMode(v.AccentMode) {
		sv.add(ErrInvalidAccentMode, v.ID, string(v.AccentMode), fmt.Sprintf("invalid accent_mode %q", v.AccentMode))
	}

	viewContSet := sv.validateViewContainers(v)
	placedInView := sv.validateViewPlacements(v, isView0, validArch, maxCol, viewContSet)
	sv.validateViewEdges(v, isView0, placedInView)
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
	return viewContSet
}

func (sv *structureValidator) validateViewPlacements(
	v View,
	isView0, validArch bool,
	maxCol int,
	viewContSet map[string]bool,
) map[string]bool {
	sv.validatePlacementCount(v.ID, len(v.Placements))

	placedInView := make(map[string]bool, len(v.Placements))
	for _, p := range v.Placements {
		if placedInView[p.ComponentID] {
			sv.add(ErrDuplicatePlacement, v.ID, p.ComponentID, fmt.Sprintf("component %q placed more than once in view", p.ComponentID))
		}
		placedInView[p.ComponentID] = true

		sv.validatePlacementTarget(v.ID, p.ComponentID, isView0)
		if p.ContainerID != "" && !viewContSet[p.ContainerID] {
			sv.add(ErrPlacementContainerNotInView, v.ID, p.ComponentID, fmt.Sprintf("placement container_id %q is not listed in view.container_ids", p.ContainerID))
		}
		if validArch && (p.Column < 0 || (maxCol >= 0 && p.Column > maxCol)) {
			sv.add(ErrColumnOutOfBounds, v.ID, p.ComponentID, fmt.Sprintf("placement column %d out of bounds 0..%d for archetype %q", p.Column, maxCol, v.Archetype))
		}
		sv.validateOffPageRefs(v.ID, p)
	}
	return placedInView
}

func (sv *structureValidator) validatePlacementCount(viewID string, count int) {
	if count < 1 {
		sv.add(ErrViewPlacementCount, viewID, viewID, fmt.Sprintf("view has %d placements (must be >= 1)", count))
	}
}

func (sv *structureValidator) validatePlacementTarget(viewID, compID string, isView0 bool) {
	_, inComponents := sv.compByID[compID]
	_, inDetailViews := sv.detailViewIDs[compID]
	if isView0 {
		if !inComponents && !inDetailViews {
			sv.add(ErrUnknownComponentRef, viewID, compID, fmt.Sprintf("system index placement %q matches neither a component nor a detail view", compID))
			return
		}
		if inComponents {
			sv.placedInView0[compID] = true
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

func (sv *structureValidator) validateViewEdges(v View, isView0 bool, placedInView map[string]bool) {
	type busEndpoints struct {
		froms map[string]bool
		tos   map[string]bool
	}
	buses := make(map[string]*busEndpoints)

	for _, e := range v.Edges {
		sv.validateSingleViewEdge(v, e, isView0, placedInView)
		if !isView0 {
			sv.validateCrossViewConsistency(v.ID, e)
		}
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

	for _, bid := range slices.Sorted(maps.Keys(buses)) {
		b := buses[bid]
		if len(b.froms) > 1 && len(b.tos) > 1 {
			sv.add(ErrInvalidBusGroup, v.ID, bid, fmt.Sprintf("via_bus_id %q mixes %d distinct sources and %d distinct targets (must share one from or one to)", bid, len(b.froms), len(b.tos)))
		}
	}
}

// validateCrossViewConsistency requires the same (from, to, flow_kind) edge drawn in several
// detail views to carry the same verb and provenance everywhere.
func (sv *structureValidator) validateCrossViewConsistency(viewID string, e Edge) {
	k := crossViewEdgeKey{from: e.From, to: e.To, flowKind: e.FlowKind}
	prev, seen := sv.crossViewEdges[k]
	if !seen {
		sv.crossViewEdges[k] = crossViewEdge{viewID: viewID, verb: e.Verb, provenance: e.Provenance}
		return
	}
	if prev.viewID != viewID && (prev.verb != e.Verb || prev.provenance != e.Provenance) {
		sv.add(ErrInconsistentCrossViewEdge, viewID, e.From+" -> "+e.To, fmt.Sprintf("edge is drawn as %q/%s here but %q/%s in view %q; the same edge must read identically in every view", e.Verb, e.Provenance, prev.verb, prev.provenance, prev.viewID))
	}
}

func (sv *structureValidator) validateSingleViewEdge(v View, e Edge, isView0 bool, placedInView map[string]bool) {
	viewID := v.ID
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
	skipLegacyIndexEvidence := isView0 && v.ID == SystemIndexViewID && v.Archetype != ArchetypeLayeredFlow && len(sv.detailViewIDs) >= 2 && len(e.EvidenceEdgeIDs) == 0
	sv.validateEdgeProvenanceField(viewID, edgeLabel, e, skipLegacyIndexEvidence)
}

func (sv *structureValidator) validateEdgeProvenanceField(viewID, edgeLabel string, e Edge, skipLegacyIndexEvidence bool) {
	switch e.Provenance {
	case ProvenanceVerified:
		if skipLegacyIndexEvidence {
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
	placed := sv.placedInAnyDetailView
	scope := "any detail view"
	if len(sv.detailViewIDs) == 0 {
		placed = sv.placedInView0
		scope = SystemIndexViewID
	}
	var unplaced []string
	for id := range sv.compByID {
		if !placed[id] {
			unplaced = append(unplaced, id)
		}
	}
	slices.Sort(unplaced)
	for _, id := range unplaced {
		sv.add(ErrUnplacedComponent, "", id, fmt.Sprintf("component %q is not placed in %s", id, scope))
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
	case ContainerOrg, ContainerFolder, ContainerProject, ContainerVPC,
		ContainerRegion, ContainerSubnet, ContainerExposureZone, ContainerGKECluster,
		ContainerPSAZone, ContainerExternalZone, ContainerSharedHub, ContainerHybridZone:
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
	case ArchetypeLayeredFlow:
		return -1, true
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
