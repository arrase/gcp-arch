package synth

import (
	"google.golang.org/genai"

	"github.com/arrase/gcp-arch/internal/manifest"
)

// Pass1Plan is the structured JSON output produced by Pass 1 (view & component decomposition).
type Pass1Plan struct {
	Components        []Pass1Component        `json:"components"`
	ContainerDefs     []Pass1ContainerDef     `json:"container_defs"`
	ExcludedResources []Pass1ExcludedResource `json:"excluded_resources"`
	DetailViews       []Pass1DetailView       `json:"detail_views"`
}

// Pass1InlineMeta is a key-value metadata row returned inside Pass1Component.
type Pass1InlineMeta struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Pass1Component defines a logical component proposed by Pass 1 synthesis.
type Pass1Component struct {
	ID            string                 `json:"id"`
	Title         string                 `json:"title"`
	Subtitle      string                 `json:"subtitle,omitempty"`
	IconKey       string                 `json:"icon_key"`
	ComponentKind manifest.ComponentKind `json:"component_kind"`
	ProjectID     string                 `json:"project_id"`
	BackedByIDs   []string               `json:"backed_by_ids"`
	InlineMeta    []Pass1InlineMeta      `json:"inline_meta,omitempty"`
}

// Pass1ContainerDef defines a visual container proposed by Pass 1 synthesis.
type Pass1ContainerDef struct {
	ID                string                 `json:"id"`
	Title             string                 `json:"title"`
	Subtitle          string                 `json:"subtitle,omitempty"`
	Kind              manifest.ContainerKind `json:"kind"`
	ParentContainerID string                 `json:"parent_container_id,omitempty"`
	BackedByIDs       []string               `json:"backed_by_ids,omitempty"`
}

// Pass1ExcludedResource records an interned resource excluded during Pass 1 synthesis.
type Pass1ExcludedResource struct {
	ResourceID string `json:"resource_id"`
	Reason     string `json:"reason"`
}

// Pass1DetailView describes a planned detail view and its assigned component IDs.
type Pass1DetailView struct {
	ID           string              `json:"id"`
	Title        string              `json:"title"`
	Subtitle     string              `json:"subtitle,omitempty"`
	Archetype    manifest.Archetype  `json:"archetype"`
	AccentMode   manifest.AccentMode `json:"accent_mode"`
	ComponentIDs []string            `json:"component_ids"`
}

// Pass2View is the structured JSON output produced per detail view by Pass 2.
type Pass2View struct {
	ContainerIDs []string         `json:"container_ids"`
	Placements   []Pass2Placement `json:"placements"`
	Edges        []Pass2Edge      `json:"edges"`
}

// Pass2Placement positions a component within a Pass 2 detail view. Off-page references are
// derived deterministically in Go after all views are populated.
type Pass2Placement struct {
	ComponentID string `json:"component_id"`
	ContainerID string `json:"container_id,omitempty"`
	Column      int    `json:"column"`
}

// Pass2Edge describes a directed edge between two components in a Pass 2 view.
type Pass2Edge struct {
	From               string              `json:"from"`
	To                 string              `json:"to"`
	Verb               string              `json:"verb"`
	FlowKind           manifest.FlowKind   `json:"flow_kind"`
	Provenance         manifest.Provenance `json:"provenance"`
	EvidenceEdgeIDs    []string            `json:"evidence_edge_ids,omitempty"`
	InferenceRationale string              `json:"inference_rationale,omitempty"`
	ViaBusID           string              `json:"via_bus_id,omitempty"`
}

// ReconcilePatch is the structured JSON delta returned by Day-2 --reconcile synthesis.
type ReconcilePatch struct {
	AppendBackedBy          []ReconcileAppendBackedBy          `json:"append_backed_by,omitempty"`
	AppendContainerBackedBy []ReconcileAppendContainerBackedBy `json:"append_container_backed_by,omitempty"`
	NewComponents           []Pass1Component                   `json:"new_components,omitempty"`
	NewContainers           []Pass1ContainerDef                `json:"new_containers,omitempty"`
	NewExcludedResources    []Pass1ExcludedResource            `json:"new_excluded_resources,omitempty"`
	NewPlacements           []ReconcileNewPlacement            `json:"new_placements,omitempty"`
	NewEdges                []ReconcileNewEdge                 `json:"new_edges,omitempty"`
	NewViews                []ReconcileNewView                 `json:"new_views,omitempty"`
}

// ReconcileAppendBackedBy appends unclaimed resources to an existing component.
type ReconcileAppendBackedBy struct {
	ComponentID string   `json:"component_id"`
	ResourceIDs []string `json:"resource_ids"`
}

// ReconcileAppendContainerBackedBy appends unclaimed resources to an existing container.
type ReconcileAppendContainerBackedBy struct {
	ContainerID string   `json:"container_id"`
	ResourceIDs []string `json:"resource_ids"`
}

// ReconcileNewPlacement inserts a placement into an existing detail view.
type ReconcileNewPlacement struct {
	ViewID    string         `json:"view_id"`
	Placement Pass2Placement `json:"placement"`
}

// ReconcileNewEdge appends a directed edge to an existing detail view.
type ReconcileNewEdge struct {
	ViewID string    `json:"view_id"`
	Edge   Pass2Edge `json:"edge"`
}

// ReconcileNewView defines a new overflow detail view added during reconciliation.
type ReconcileNewView struct {
	ID           string              `json:"id"`
	Title        string              `json:"title"`
	Subtitle     string              `json:"subtitle,omitempty"`
	Archetype    manifest.Archetype  `json:"archetype"`
	AccentMode   manifest.AccentMode `json:"accent_mode"`
	ContainerIDs []string            `json:"container_ids,omitempty"`
	Placements   []Pass2Placement    `json:"placements"`
	Edges        []Pass2Edge         `json:"edges,omitempty"`
}

var (
	componentKindEnum = []string{
		string(manifest.KindExternalCaller),
		string(manifest.KindIngress),
		string(manifest.KindWorkload),
		string(manifest.KindPipeline),
		string(manifest.KindMessaging),
		string(manifest.KindDatastore),
		string(manifest.KindCohort),
		string(manifest.KindExternalTarget),
	}
	containerKindEnum = []string{
		string(manifest.ContainerProject),
		string(manifest.ContainerVPC),
		string(manifest.ContainerGKECluster),
		string(manifest.ContainerPSAZone),
		string(manifest.ContainerExternalZone),
		string(manifest.ContainerSharedHub),
	}
	archetypeEnum = []string{
		string(manifest.ArchetypeLandscapeMatrix),
		string(manifest.ArchetypeStagePipeline),
		string(manifest.ArchetypeHubSpoke),
	}
	accentModeEnum = []string{
		string(manifest.AccentRuntime),
		string(manifest.AccentCICD),
		string(manifest.AccentMixed),
	}
	flowKindEnum = []string{
		string(manifest.FlowRuntime),
		string(manifest.FlowCICD),
		string(manifest.FlowAdmin),
	}
	provenanceEnum = []string{
		string(manifest.ProvenanceVerified),
		string(manifest.ProvenanceInferred),
	}
)

func stringSchema(desc string) *genai.Schema {
	return &genai.Schema{Type: genai.TypeString, Description: desc}
}

func enumSchema(desc string, values []string) *genai.Schema {
	return &genai.Schema{Type: genai.TypeString, Format: "enum", Enum: values, Description: desc}
}

func arraySchema(desc string, items *genai.Schema) *genai.Schema {
	return &genai.Schema{Type: genai.TypeArray, Description: desc, Items: items}
}

func objectSchema(props map[string]*genai.Schema, required ...string) *genai.Schema {
	return &genai.Schema{Type: genai.TypeObject, Properties: props, Required: required}
}

func stringArraySchema(desc string) *genai.Schema {
	return arraySchema(desc, &genai.Schema{Type: genai.TypeString})
}

func componentSchema() *genai.Schema {
	inlineMetaItem := objectSchema(map[string]*genai.Schema{
		"label": stringSchema("Short metadata key"),
		"value": stringSchema("Short metadata value"),
	}, "label", "value")

	return objectSchema(map[string]*genai.Schema{
		"id":             stringSchema("Canonical snake_case component slug"),
		"title":          stringSchema("Human-readable block title"),
		"subtitle":       stringSchema("Concise technical sublabel"),
		"icon_key":       stringSchema("Registered icon slug validated against the allowed icon list"),
		"component_kind": enumSchema("Architectural role of the component", componentKindEnum),
		"project_id":     stringSchema("Owning GCP project ID or external"),
		"backed_by_ids":  stringArraySchema("Interned r_XXXX handles owned or 1:1-folded into this component"),
		"inline_meta":    arraySchema("Up to 4 concise key/value metadata rows displayed inside the card", inlineMetaItem),
	}, "id", "title", "icon_key", "component_kind", "project_id", "backed_by_ids")
}

func containerDefSchema() *genai.Schema {
	return objectSchema(map[string]*genai.Schema{
		"id":                  stringSchema("Canonical snake_case container slug"),
		"title":               stringSchema("Human-readable container title"),
		"subtitle":            stringSchema("Region or scope subtitle"),
		"kind":                enumSchema("Container semantic kind", containerKindEnum),
		"parent_container_id": stringSchema("Optional parent container ID (max nesting depth 2)"),
		"backed_by_ids":       stringArraySchema("Optional interned r_XXXX handles represented by this container"),
	}, "id", "title", "kind")
}

func excludedResourceSchema() *genai.Schema {
	return objectSchema(map[string]*genai.Schema{
		"resource_id": stringSchema("Interned r_XXXX handle intentionally excluded from diagram cards"),
		"reason":      stringSchema("Concise audit reason for exclusion"),
	}, "resource_id", "reason")
}

func placementSchema() *genai.Schema {
	return objectSchema(map[string]*genai.Schema{
		"component_id": stringSchema("Locked component ID placed in this view"),
		"container_id": stringSchema("Optional active container ID from container_ids"),
		"column":       {Type: genai.TypeInteger, Description: "0-based archetype column index"},
	}, "component_id", "column")
}

func edgeSchema() *genai.Schema {
	return objectSchema(map[string]*genai.Schema{
		"from":                stringSchema("Source component ID placed in this view"),
		"to":                  stringSchema("Target component ID placed in this view"),
		"verb":                stringSchema("Short action verb (max 16 chars)"),
		"flow_kind":           enumSchema("Semantic flow category", flowKindEnum),
		"provenance":          enumSchema("Evidence provenance classification", provenanceEnum),
		"evidence_edge_ids":   stringArraySchema("Interned non-heuristic e_XXXX IDs forming a directed walk from -> to (required for VERIFIED)"),
		"inference_rationale": stringSchema("Concise rationale required when provenance is INFERRED"),
		"via_bus_id":          stringSchema("Optional shared bus ID for edges sharing one source or one target"),
	}, "from", "to", "verb", "flow_kind", "provenance")
}

func viewHeaderProps(idDesc, titleDesc, subtitleDesc, accentDesc string) map[string]*genai.Schema {
	return map[string]*genai.Schema{
		"id":          stringSchema(idDesc),
		"title":       stringSchema(titleDesc),
		"subtitle":    stringSchema(subtitleDesc),
		"archetype":   enumSchema("Layout archetype", archetypeEnum),
		"accent_mode": enumSchema(accentDesc, accentModeEnum),
	}
}

func detailViewSchema() *genai.Schema {
	props := viewHeaderProps(
		"Canonical snake_case view ID (never view_0_system_index)",
		"Human-readable view title",
		"One-line view summary",
		"Primary visual accent mode",
	)
	props["component_ids"] = stringArraySchema("1-12 component IDs assigned to this view")
	return objectSchema(props, "id", "title", "archetype", "accent_mode", "component_ids")
}

func appendBackedBySchema(idKey, idDesc string) *genai.Schema {
	return objectSchema(map[string]*genai.Schema{
		idKey:          stringSchema(idDesc),
		"resource_ids": stringArraySchema("Unclaimed r_XXXX IDs to append"),
	}, idKey, "resource_ids")
}

func viewScopedSchema(itemKey string, item *genai.Schema) *genai.Schema {
	return objectSchema(map[string]*genai.Schema{
		"view_id": stringSchema("Existing detail view ID"),
		itemKey:   item,
	}, "view_id", itemKey)
}

func reconcileNewViewSchema() *genai.Schema {
	props := viewHeaderProps(
		"New snake_case detail view ID",
		"View title",
		"View subtitle",
		"Primary accent mode",
	)
	props["container_ids"] = stringArraySchema("Active container IDs")
	props["placements"] = arraySchema("", placementSchema())
	props["edges"] = arraySchema("", edgeSchema())
	return objectSchema(props, "id", "title", "archetype", "accent_mode", "placements")
}

// Pass1PlanSchema returns the non-recursive Vertex AI ResponseSchema for Pass 1 planning.
func Pass1PlanSchema() *genai.Schema {
	return objectSchema(map[string]*genai.Schema{
		"components":         arraySchema("Logical architecture blocks covering primary resources", componentSchema()),
		"container_defs":     arraySchema("Global container frame definitions (max depth 2)", containerDefSchema()),
		"excluded_resources": arraySchema("Active resources intentionally omitted from visual cards", excludedResourceSchema()),
		"detail_views":       arraySchema("1-16 focused logical views (target 6-12 components each)", detailViewSchema()),
	}, "components", "container_defs", "excluded_resources", "detail_views")
}

// Pass2ViewSchema returns the non-recursive Vertex AI ResponseSchema for Pass 2 per-view population.
func Pass2ViewSchema() *genai.Schema {
	return objectSchema(map[string]*genai.Schema{
		"container_ids": stringArraySchema("Container IDs active in this view (max nesting depth 2)"),
		"placements":    arraySchema("Ordered placements (1-12) for components in this view", placementSchema()),
		"edges":         arraySchema("Provenance-checked directed edges between placed components", edgeSchema()),
	}, "container_ids", "placements", "edges")
}

// ReconcilePatchSchema returns the non-recursive Vertex AI ResponseSchema for Day-2 delta reconciliation.
func ReconcilePatchSchema() *genai.Schema {
	return objectSchema(map[string]*genai.Schema{
		"append_backed_by":           arraySchema("Unclaimed r_XXXX resources to fold into existing components without changing titles/notes", appendBackedBySchema("component_id", "Existing component ID")),
		"append_container_backed_by": arraySchema("Unclaimed r_XXXX resources to fold into existing containers", appendBackedBySchema("container_id", "Existing container ID")),
		"new_components":             arraySchema("Brand-new components for unclaimed resources", componentSchema()),
		"new_containers":             arraySchema("Brand-new containers if required", containerDefSchema()),
		"new_excluded_resources":     arraySchema("Unclaimed resources intentionally excluded with audit reason", excludedResourceSchema()),
		"new_placements":             arraySchema("Placements for new components inside existing detail views with remaining capacity", viewScopedSchema("placement", placementSchema())),
		"new_edges":                  arraySchema("New edges to append into existing detail views", viewScopedSchema("edge", edgeSchema())),
		"new_views":                  arraySchema("Overflow detail views (1-12 placements) if existing views lack capacity", reconcileNewViewSchema()),
	})
}
