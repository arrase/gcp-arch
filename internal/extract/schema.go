package extract

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"

	"github.com/arrase/gcp-arch/internal/ioutil"
)

// FactsSchemaVersion is the current schema version for facts.json files.
const FactsSchemaVersion = "v3"

// FactEdgeKind classifies the extraction source and semantics of a FactEdge.
type FactEdgeKind string

// Supported FactEdgeKind values.
const (
	EdgeConfigRef          FactEdgeKind = "CONFIG_REF"
	EdgeK8sIngressBackend  FactEdgeKind = "K8S_INGRESS_BACKEND"
	EdgeK8sNEGBackend      FactEdgeKind = "K8S_NEG_BACKEND"
	EdgeK8sServiceSelector FactEdgeKind = "K8S_SERVICE_SELECTOR"
	EdgeWorkloadIdentity   FactEdgeKind = "WORKLOAD_IDENTITY"
	EdgeCloudBuildRepo     FactEdgeKind = "CLOUDBUILD_TRIGGER_REPO"
	EdgeCloudBuildStepRef  FactEdgeKind = "CLOUDBUILD_STEP_REF"
	EdgeIAMResourceBinding FactEdgeKind = "IAM_RESOURCE_BINDING"
	EdgePSAPeering         FactEdgeKind = "PSA_PEERING"
	EdgeVPCPeering         FactEdgeKind = "VPC_PEERING"
	EdgeBQLineage          FactEdgeKind = "BQ_LINEAGE"
	EdgeMonitoringTraffic  FactEdgeKind = "MONITORING_TRAFFIC"
)

// CoverageState reports whether a discovery API call succeeded or degraded.
type CoverageState string

// Supported CoverageState values.
const (
	CoverageOK              CoverageState = "OK"
	CoverageIAMDenied       CoverageState = "IAM_DENIED"
	CoverageVPCSCBlocked    CoverageState = "VPC_SC_BLOCKED"
	CoverageServiceDisabled CoverageState = "SERVICE_DISABLED"
	CoverageTruncatingCap   CoverageState = "TRUNCATING_CAP"
)

// FactsFile is the on-disk v3 contract emitted by Stage 1 extraction.
type FactsFile struct {
	SchemaVersion            string           `json:"schema_version"`
	ReadTime                 string           `json:"read_time"`
	Scopes                   []string         `json:"scopes"`
	Hierarchy                Hierarchy        `json:"hierarchy"`
	Resources                []FactResource   `json:"resources"`
	Edges                    []FactEdge       `json:"edges"`
	SuppressedPlumbingCounts map[string]int   `json:"suppressed_plumbing_counts"`
	OutOfScopeTargets        []OutOfScopeHint `json:"out_of_scope_targets,omitempty"`
	CoverageLedger           []CoverageEntry  `json:"coverage_ledger"`
}

// Hierarchy captures discovered GCP organizations, folders, and projects.
type Hierarchy struct {
	Organizations []OrgNode     `json:"organizations,omitempty"`
	Folders       []FolderNode  `json:"folders,omitempty"`
	Projects      []ProjectNode `json:"projects"`
}

// OrgNode represents a GCP Organization in the resource hierarchy.
type OrgNode struct {
	OrgID       string `json:"org_id"`
	DisplayName string `json:"display_name,omitempty"`
}

// FolderNode represents a GCP Folder in the resource hierarchy.
type FolderNode struct {
	FolderID    string `json:"folder_id"`
	DisplayName string `json:"display_name,omitempty"`
	Parent      string `json:"parent,omitempty"`
}

// ProjectNode represents a GCP Project in the resource hierarchy.
type ProjectNode struct {
	ProjectID            string `json:"project_id"`
	ProjectNumber        string `json:"project_number,omitempty"`
	DisplayName          string `json:"display_name,omitempty"`
	Parent               string `json:"parent,omitempty"`
	IsPSATenant          bool   `json:"is_psa_tenant,omitempty"`
	SharedVPCHostProject string `json:"shared_vpc_host_project,omitempty"`
}

// FactResource represents one sanitized active GCP or external resource.
type FactResource struct {
	URI                 string         `json:"uri"`
	AssetType           string         `json:"asset_type"`
	ProjectID           string         `json:"project_id"`
	Location            string         `json:"location,omitempty"`
	DisplayName         string         `json:"display_name"`
	ParentURI           string         `json:"parent_uri,omitempty"`
	IsExternal          bool           `json:"is_external,omitempty"`
	Attributes          map[string]any `json:"attributes,omitempty"`
	ProjectCapabilities []string       `json:"project_capabilities,omitempty"`
}

// FactEdge represents one directed dependency or runtime relationship between two resources.
type FactEdge struct {
	ID      string       `json:"id"`
	FromURI string       `json:"from_uri"`
	ToURI   string       `json:"to_uri"`
	Kind    FactEdgeKind `json:"kind"`
	Detail  string       `json:"detail,omitempty"`
}

// OutOfScopeHint records a project referenced by extracted resources outside the scanned scopes.
type OutOfScopeHint struct {
	TargetProjectID  string   `json:"target_project_id"`
	SyntheticURI     string   `json:"synthetic_uri"`
	ReferencedByURIs []string `json:"referenced_by_uris"`
	SourceKind       string   `json:"source_kind"`
}

// CoverageEntry records the outcome of one discovery API source against a scope.
type CoverageEntry struct {
	Scope  string        `json:"scope"`
	Source string        `json:"source"`
	State  CoverageState `json:"state"`
	Detail string        `json:"detail,omitempty"`
}

// EdgeID returns the deterministic 16-hex-char SHA-256 identifier for a FactEdge.
func EdgeID(kind FactEdgeKind, fromURI, toURI, detail string) string {
	sum := sha256.Sum256([]byte(string(kind) + ":" + fromURI + ":" + toURI + ":" + detail))
	return hex.EncodeToString(sum[:])[:16]
}

// LoadFacts reads and decodes a v3 facts.json file.
func LoadFacts(path string) (*FactsFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read facts file %q: %w", path, err)
	}
	var f FactsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("decode facts file %q: %w", path, err)
	}
	if f.SchemaVersion != FactsSchemaVersion {
		return nil, fmt.Errorf("unsupported facts schema_version %q (want %q)", f.SchemaVersion, FactsSchemaVersion)
	}
	return &f, nil
}

// WriteFacts deterministically sorts all slices in f and atomically writes indented UTF-8 JSON
// with 0600 file permissions (0700 parent directory).
func WriteFacts(path string, f *FactsFile) error {
	if f == nil {
		return fmt.Errorf("facts file must not be nil")
	}
	out := normalizedFactsCopy(f)
	if out.SchemaVersion == "" {
		out.SchemaVersion = FactsSchemaVersion
	} else if out.SchemaVersion != FactsSchemaVersion {
		return fmt.Errorf("unsupported facts schema_version %q (want %q)", out.SchemaVersion, FactsSchemaVersion)
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal facts file: %w", err)
	}
	data = append(data, '\n')
	return ioutil.WriteFileAtomic(path, data, 0o600, 0o700)
}

func normalizedFactsCopy(f *FactsFile) FactsFile {
	out := FactsFile{
		SchemaVersion:            f.SchemaVersion,
		ReadTime:                 f.ReadTime,
		Scopes:                   nonNilSortedStrings(f.Scopes),
		SuppressedPlumbingCounts: f.SuppressedPlumbingCounts,
	}
	if out.SuppressedPlumbingCounts == nil {
		out.SuppressedPlumbingCounts = map[string]int{}
	}

	if len(f.Hierarchy.Organizations) > 0 {
		orgs := slices.Clone(f.Hierarchy.Organizations)
		slices.SortFunc(orgs, func(a, b OrgNode) int {
			return cmp.Or(cmp.Compare(a.OrgID, b.OrgID), cmp.Compare(a.DisplayName, b.DisplayName))
		})
		out.Hierarchy.Organizations = orgs
	}

	if len(f.Hierarchy.Folders) > 0 {
		folders := slices.Clone(f.Hierarchy.Folders)
		slices.SortFunc(folders, func(a, b FolderNode) int {
			return cmp.Or(
				cmp.Compare(a.FolderID, b.FolderID),
				cmp.Compare(a.Parent, b.Parent),
				cmp.Compare(a.DisplayName, b.DisplayName),
			)
		})
		out.Hierarchy.Folders = folders
	}

	projects := make([]ProjectNode, len(f.Hierarchy.Projects))
	copy(projects, f.Hierarchy.Projects)
	slices.SortFunc(projects, func(a, b ProjectNode) int {
		return cmp.Or(
			cmp.Compare(a.ProjectID, b.ProjectID),
			cmp.Compare(a.ProjectNumber, b.ProjectNumber),
			cmp.Compare(a.Parent, b.Parent),
			cmp.Compare(a.DisplayName, b.DisplayName),
		)
	})
	out.Hierarchy.Projects = projects

	resources := make([]FactResource, len(f.Resources))
	for i, r := range f.Resources {
		rc := r
		if len(r.ProjectCapabilities) > 0 {
			rc.ProjectCapabilities = slices.Clone(r.ProjectCapabilities)
			slices.Sort(rc.ProjectCapabilities)
		}
		resources[i] = rc
	}
	slices.SortFunc(resources, func(a, b FactResource) int {
		return cmp.Or(
			cmp.Compare(a.URI, b.URI),
			cmp.Compare(a.AssetType, b.AssetType),
			cmp.Compare(a.ProjectID, b.ProjectID),
		)
	})
	out.Resources = resources

	edges := make([]FactEdge, len(f.Edges))
	for i, e := range f.Edges {
		ec := e
		if ec.ID == "" {
			ec.ID = EdgeID(ec.Kind, ec.FromURI, ec.ToURI, ec.Detail)
		}
		edges[i] = ec
	}
	slices.SortFunc(edges, func(a, b FactEdge) int {
		return cmp.Or(
			cmp.Compare(a.FromURI, b.FromURI),
			cmp.Compare(a.ToURI, b.ToURI),
			cmp.Compare(a.Kind, b.Kind),
			cmp.Compare(a.Detail, b.Detail),
			cmp.Compare(a.ID, b.ID),
		)
	})
	out.Edges = edges

	if len(f.OutOfScopeTargets) > 0 {
		hints := make([]OutOfScopeHint, len(f.OutOfScopeTargets))
		for i, h := range f.OutOfScopeTargets {
			hc := h
			hc.ReferencedByURIs = nonNilSortedStrings(h.ReferencedByURIs)
			hints[i] = hc
		}
		slices.SortFunc(hints, func(a, b OutOfScopeHint) int {
			return cmp.Or(
				cmp.Compare(a.TargetProjectID, b.TargetProjectID),
				cmp.Compare(a.SourceKind, b.SourceKind),
				cmp.Compare(a.SyntheticURI, b.SyntheticURI),
			)
		})
		out.OutOfScopeTargets = hints
	}

	ledger := make([]CoverageEntry, len(f.CoverageLedger))
	copy(ledger, f.CoverageLedger)
	slices.SortFunc(ledger, func(a, b CoverageEntry) int {
		return cmp.Or(
			cmp.Compare(a.Scope, b.Scope),
			cmp.Compare(a.Source, b.Source),
			cmp.Compare(a.State, b.State),
			cmp.Compare(a.Detail, b.Detail),
		)
	})
	out.CoverageLedger = ledger

	return out
}

func nonNilSortedStrings(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	slices.Sort(out)
	return out
}
