package manifest

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/arrase/gcp-arch/internal/ioutil"
)

// Schema and reserved view identifiers for architecture.yaml manifests.
const (
	SchemaVersion     = "v1"
	SystemIndexViewID = "view_0_system_index"
)

// ComponentKind classifies the architectural role of a Component.
type ComponentKind string

// Supported ComponentKind values.
const (
	KindExternalCaller ComponentKind = "EXTERNAL_CALLER"
	KindIngress        ComponentKind = "INGRESS"
	KindWorkload       ComponentKind = "WORKLOAD"
	KindPipeline       ComponentKind = "PIPELINE"
	KindMessaging      ComponentKind = "MESSAGING"
	KindDatastore      ComponentKind = "DATASTORE"
	KindCohort         ComponentKind = "COHORT"
	KindExternalTarget ComponentKind = "EXTERNAL_TARGET"
)

// ContainerKind classifies the grouping boundary of a Container.
type ContainerKind string

// Supported ContainerKind values.
const (
	ContainerProject      ContainerKind = "PROJECT"
	ContainerVPC          ContainerKind = "VPC"
	ContainerGKECluster   ContainerKind = "GKE_CLUSTER"
	ContainerPSAZone      ContainerKind = "PSA_ZONE"
	ContainerExternalZone ContainerKind = "EXTERNAL_ZONE"
	ContainerSharedHub    ContainerKind = "SHARED_HUB"
)

// Archetype selects the visual grid layout for a View.
type Archetype string

// Supported Archetype values.
const (
	ArchetypeLandscapeMatrix Archetype = "landscape_matrix"
	ArchetypeStagePipeline   Archetype = "stage_pipeline"
	ArchetypeHubSpoke        Archetype = "hub_spoke"
)

// AccentMode selects the visual color palette emphasis of a View.
type AccentMode string

// Supported AccentMode values.
const (
	AccentRuntime AccentMode = "RUNTIME"
	AccentCICD    AccentMode = "CICD"
	AccentMixed   AccentMode = "MIXED"
)

// FlowKind categorizes the operational domain of an Edge.
type FlowKind string

// Supported FlowKind values.
const (
	FlowRuntime FlowKind = "RUNTIME"
	FlowCICD    FlowKind = "CICD"
	FlowAdmin   FlowKind = "ADMIN"
)

// Provenance indicates whether an Edge is backed by extracted facts or inferred.
type Provenance string

// Supported Provenance values.
const (
	ProvenanceVerified Provenance = "VERIFIED"
	ProvenanceInferred Provenance = "INFERRED"
)

// Manifest is the root schema for a Git-tracked architecture.yaml file.
type Manifest struct {
	SchemaVersion     string             `yaml:"schema_version"`
	Metadata          Metadata           `yaml:"metadata"`
	Components        []Component        `yaml:"components"`
	Containers        []Container        `yaml:"containers,omitempty"`
	ExcludedResources []ExcludedResource `yaml:"excluded_resources,omitempty"`
	RejectedEdges     []RejectedEdge     `yaml:"rejected_edges,omitempty"`
	Views             []View             `yaml:"views"`
}

// Metadata records generation timestamp and scanned GCP scopes.
type Metadata struct {
	GeneratedAt string   `yaml:"generated_at"`
	Scopes      []string `yaml:"scopes"`
}

// BackedResource links a Component or Container to an underlying GCP CAI resource.
type BackedResource struct {
	URI       string `yaml:"uri"`
	AssetType string `yaml:"asset_type"`
	ProjectID string `yaml:"project_id"`
}

// InlineMeta is a key-value detail row rendered inside a Component card.
type InlineMeta struct {
	Label string `yaml:"label"`
	Value string `yaml:"value"`
}

// Component represents a logical architectural node backed by one or more GCP resources.
type Component struct {
	ID            string           `yaml:"id"`
	Title         string           `yaml:"title"`
	Subtitle      string           `yaml:"subtitle,omitempty"`
	IconKey       string           `yaml:"icon_key"`
	ComponentKind ComponentKind    `yaml:"component_kind"`
	ProjectID     string           `yaml:"project_id"`
	Notes         string           `yaml:"notes,omitempty"`
	BackedBy      []BackedResource `yaml:"backed_by"`
	InlineMeta    []InlineMeta     `yaml:"inline_meta,omitempty"`
}

// Container represents a visual grouping boundary (project, VPC, GKE cluster, etc.).
type Container struct {
	ID                string           `yaml:"id"`
	Title             string           `yaml:"title"`
	Subtitle          string           `yaml:"subtitle,omitempty"`
	Kind              ContainerKind    `yaml:"kind"`
	ParentContainerID string           `yaml:"parent_container_id,omitempty"`
	BackedBy          []BackedResource `yaml:"backed_by,omitempty"`
}

// ExcludedResource records a GCP resource deliberately omitted from diagram components.
type ExcludedResource struct {
	URI       string `yaml:"uri"`
	AssetType string `yaml:"asset_type"`
	ProjectID string `yaml:"project_id"`
	Reason    string `yaml:"reason"`
}

// RejectedEdge records a curated tombstone suppressing a directed component edge.
type RejectedEdge struct {
	From   string `yaml:"from"`
	To     string `yaml:"to"`
	Reason string `yaml:"reason"`
}

// View defines one rendered diagram page and its placements, containers, and edges.
type View struct {
	ID           string      `yaml:"id"`
	Title        string      `yaml:"title"`
	Subtitle     string      `yaml:"subtitle,omitempty"`
	Archetype    Archetype   `yaml:"archetype"`
	AccentMode   AccentMode  `yaml:"accent_mode"`
	Notes        string      `yaml:"notes,omitempty"`
	ContainerIDs []string    `yaml:"container_ids,omitempty"`
	Placements   []Placement `yaml:"placements"`
	Edges        []Edge      `yaml:"edges,omitempty"`
}

// Placement positions a Component (or detail View on View 0) within a View column and optional Container.
type Placement struct {
	ComponentID string       `yaml:"component_id"`
	ContainerID string       `yaml:"container_id,omitempty"`
	Column      int          `yaml:"column"`
	OffPageRefs []OffPageRef `yaml:"off_page_refs,omitempty"`
}

// OffPageRef annotates a Placement with a cross-view navigation link.
type OffPageRef struct {
	TargetViewID string `yaml:"target_view_id"`
	Label        string `yaml:"label"`
	Direction    string `yaml:"direction"` // "IN" | "OUT"
}

// Edge represents a directed relationship rendered between two placed components in a View.
type Edge struct {
	From               string     `yaml:"from"`
	To                 string     `yaml:"to"`
	Verb               string     `yaml:"verb"`
	FlowKind           FlowKind   `yaml:"flow_kind"`
	Provenance         Provenance `yaml:"provenance"`
	EvidenceEdgeIDs    []string   `yaml:"evidence_edge_ids,omitempty"`
	InferenceRationale string     `yaml:"inference_rationale,omitempty"`
	ViaBusID           string     `yaml:"via_bus_id,omitempty"`
}

// ValidationReport is the JSON diagnostic schema emitted by `gcp-arch validate --json`.
type ValidationReport struct {
	Valid  bool              `json:"valid"`
	Errors []ValidationIssue `json:"errors,omitempty"`
}

// ValidationIssue describes a single manifest validation failure.
type ValidationIssue struct {
	Code    string `json:"code"`
	ViewID  string `json:"view_id,omitempty"`
	Subject string `json:"subject,omitempty"`
	Message string `json:"message"`
}

// ValidationError wraps one or more manifest validation issues (mapped to CLI exit code 2).
type ValidationError struct {
	Issues []ValidationIssue
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "manifest validation failed (%d issue(s)):", len(e.Issues))
	for _, iss := range e.Issues {
		b.WriteString("\n  - [")
		b.WriteString(iss.Code)
		b.WriteByte(']')
		if iss.ViewID != "" {
			fmt.Fprintf(&b, " view=%s", iss.ViewID)
		}
		if iss.Subject != "" {
			fmt.Fprintf(&b, " subject=%s", iss.Subject)
		}
		b.WriteString(": ")
		b.WriteString(iss.Message)
	}
	return b.String()
}

// Report returns the JSON-serializable ValidationReport for this error.
func (e *ValidationError) Report() ValidationReport {
	return ValidationReport{
		Valid:  false,
		Errors: slices.Clone(e.Issues),
	}
}

// Load decodes an architecture.yaml manifest using strict KnownFields(true) YAML parsing.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest %q: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest %q: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode manifest %q: unexpected trailing YAML document", path)
	}
	return &m, nil
}

// Marshal serializes m into deterministic, 2-space indented canonical YAML without mutating m.
// Placements order, InlineMeta order, EvidenceEdgeIDs order, and view_0_system_index at views[0]
// are preserved.
func Marshal(m *Manifest) ([]byte, error) {
	if m == nil {
		return nil, fmt.Errorf("manifest must not be nil")
	}
	out := normalizedManifestCopy(m)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(out); err != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close yaml encoder: %w", err)
	}
	return buf.Bytes(), nil
}

// Write deterministically marshals m and writes it atomically with 0644 permissions (0755 dir).
func Write(path string, m *Manifest) error {
	data, err := Marshal(m)
	if err != nil {
		return err
	}
	return ioutil.WriteFileAtomic(path, data, 0o644, 0o755)
}

func normalizedManifestCopy(m *Manifest) Manifest {
	out := Manifest{
		SchemaVersion: m.SchemaVersion,
		Metadata: Metadata{
			GeneratedAt: m.Metadata.GeneratedAt,
			Scopes:      slices.Clone(m.Metadata.Scopes),
		},
		Components:        normalizeComponents(m.Components),
		Containers:        normalizeContainers(m.Containers),
		ExcludedResources: normalizeExcludedResources(m.ExcludedResources),
		RejectedEdges:     normalizeRejectedEdges(m.RejectedEdges),
		Views:             normalizeViews(m.Views),
	}
	slices.Sort(out.Metadata.Scopes)
	return out
}

func normalizeComponents(in []Component) []Component {
	if len(in) == 0 {
		return nil
	}
	out := make([]Component, len(in))
	for i, c := range in {
		cc := c
		cc.BackedBy = sortedBackedResources(c.BackedBy)
		cc.InlineMeta = slices.Clone(c.InlineMeta)
		out[i] = cc
	}
	slices.SortFunc(out, func(a, b Component) int {
		return cmp.Compare(a.ID, b.ID)
	})
	return out
}

func normalizeContainers(in []Container) []Container {
	if len(in) == 0 {
		return nil
	}
	out := make([]Container, len(in))
	for i, ct := range in {
		ctc := ct
		ctc.BackedBy = sortedBackedResources(ct.BackedBy)
		out[i] = ctc
	}
	slices.SortFunc(out, func(a, b Container) int {
		return cmp.Compare(a.ID, b.ID)
	})
	return out
}

func normalizeExcludedResources(in []ExcludedResource) []ExcludedResource {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b ExcludedResource) int {
		return cmp.Or(
			cmp.Compare(a.URI, b.URI),
			cmp.Compare(a.AssetType, b.AssetType),
			cmp.Compare(a.ProjectID, b.ProjectID),
			cmp.Compare(a.Reason, b.Reason),
		)
	})
	return out
}

func normalizeRejectedEdges(in []RejectedEdge) []RejectedEdge {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b RejectedEdge) int {
		return cmp.Or(
			cmp.Compare(a.From, b.From),
			cmp.Compare(a.To, b.To),
			cmp.Compare(a.Reason, b.Reason),
		)
	})
	return out
}

func normalizeViews(in []View) []View {
	if len(in) == 0 {
		return nil
	}
	out := make([]View, len(in))
	for i, v := range in {
		out[i] = normalizeView(v)
	}
	slices.SortFunc(out, func(a, b View) int {
		if a.ID == SystemIndexViewID && b.ID != SystemIndexViewID {
			return -1
		}
		if b.ID == SystemIndexViewID && a.ID != SystemIndexViewID {
			return 1
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out
}

func normalizeView(v View) View {
	vc := v
	vc.ContainerIDs = slices.Clone(v.ContainerIDs)
	slices.Sort(vc.ContainerIDs)
	vc.Placements = normalizePlacements(v.Placements)
	vc.Edges = normalizeEdges(v.Edges)
	return vc
}

func normalizePlacements(in []Placement) []Placement {
	if len(in) == 0 {
		return nil
	}
	out := make([]Placement, len(in))
	for i, p := range in {
		pc := p
		if len(p.OffPageRefs) > 0 {
			pc.OffPageRefs = slices.Clone(p.OffPageRefs)
			slices.SortFunc(pc.OffPageRefs, func(a, b OffPageRef) int {
				return cmp.Or(
					cmp.Compare(a.TargetViewID, b.TargetViewID),
					cmp.Compare(a.Direction, b.Direction),
					cmp.Compare(a.Label, b.Label),
				)
			})
		}
		out[i] = pc
	}
	return out
}

func normalizeEdges(in []Edge) []Edge {
	if len(in) == 0 {
		return nil
	}
	out := make([]Edge, len(in))
	for i, e := range in {
		ec := e
		ec.EvidenceEdgeIDs = slices.Clone(e.EvidenceEdgeIDs)
		out[i] = ec
	}
	slices.SortFunc(out, func(a, b Edge) int {
		return cmp.Or(
			cmp.Compare(a.From, b.From),
			cmp.Compare(a.To, b.To),
			cmp.Compare(a.FlowKind, b.FlowKind),
			cmp.Compare(a.Verb, b.Verb),
			cmp.Compare(a.Provenance, b.Provenance),
			cmp.Compare(a.ViaBusID, b.ViaBusID),
			cmp.Compare(a.InferenceRationale, b.InferenceRationale),
			slices.Compare(a.EvidenceEdgeIDs, b.EvidenceEdgeIDs),
		)
	})
	return out
}

func sortedBackedResources(in []BackedResource) []BackedResource {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b BackedResource) int {
		return cmp.Or(
			cmp.Compare(a.URI, b.URI),
			cmp.Compare(a.AssetType, b.AssetType),
			cmp.Compare(a.ProjectID, b.ProjectID),
		)
	})
	return out
}
