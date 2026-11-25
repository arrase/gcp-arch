package synth

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"

	"github.com/arrase/gcp-arch/internal/extract"
	"github.com/arrase/gcp-arch/internal/icons"
	"github.com/arrase/gcp-arch/internal/manifest"
)

// Synthesis scale ceiling and default worker concurrency.
const (
	MaxActiveResources   = 500
	DefaultConcurrency   = 4
	maxValidationRetries = 2
)

// ScaleLimitError is returned when facts.json exceeds MaxActiveResources (mapped to CLI exit code 2).
type ScaleLimitError struct {
	ResourceCount int
	MaxAllowed    int
}

func (e *ScaleLimitError) Error() string {
	return fmt.Sprintf("facts file contains %d active resources, exceeding the %d-resource synthesis ceiling; narrow --projects or --folder to a single application domain", e.ResourceCount, e.MaxAllowed)
}

// Options configures Synthesize and Reconcile execution.
type Options struct {
	Concurrency int
	GeneratedAt string
}

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

// Synthesize runs two-pass closed-world synthesis over facts, projecting View 0 when >= 2 detail
// views exist and validating the final manifest with manifest.ValidateWithFacts.
func Synthesize(ctx context.Context, facts *extract.FactsFile, client ModelClient, reg *icons.Registry, opts Options) (*manifest.Manifest, error) {
	if err := validateSynthesizeInputs(facts, client, reg); err != nil {
		return nil, err
	}

	concurrency, generatedAt := resolveSynthesizeOptions(facts, opts)
	table := NewInternTable(facts)

	// Pass 1: Plan suite + Pre-Pass-2 Gate (up to 2 retries with exact offending IDs).
	plan, err := runPass1(ctx, facts, table, client, reg)
	if err != nil {
		return nil, err
	}

	components, containers, excluded := materializePlanGlobals(plan, table)

	// Pass 2: Populate each detail view concurrently with bounded errgroup + per-view retries.
	runner := newPass2Runner(plan, table, components, containers, client)
	detailViews, err := runner.populateViews(ctx, concurrency)
	if err != nil {
		return nil, err
	}

	m := &manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Metadata: manifest.Metadata{
			GeneratedAt: generatedAt,
			Scopes:      slices.Clone(facts.Scopes),
		},
		Components:        components,
		Containers:        containers,
		ExcludedResources: excluded,
		Views:             detailViews,
	}

	if v0 := BuildSystemIndexView(m); v0 != nil {
		m.Views = append([]manifest.View{*v0}, m.Views...)
	}

	if err := manifest.ValidateWithFacts(m, facts, reg); err != nil {
		return nil, err
	}
	return m, nil
}

func validateSynthesizeInputs(facts *extract.FactsFile, client ModelClient, reg *icons.Registry) error {
	if facts == nil {
		return fmt.Errorf("facts file must not be nil")
	}
	if client == nil {
		return fmt.Errorf("model client must not be nil")
	}
	if reg == nil {
		return fmt.Errorf("icons registry must not be nil")
	}
	if len(facts.Resources) > MaxActiveResources {
		return &ScaleLimitError{ResourceCount: len(facts.Resources), MaxAllowed: MaxActiveResources}
	}
	return nil
}

func resolveSynthesizeOptions(facts *extract.FactsFile, opts Options) (int, string) {
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}
	generatedAt := strings.TrimSpace(opts.GeneratedAt)
	if generatedAt != "" {
		return concurrency, generatedAt
	}
	if facts.ReadTime != "" {
		return concurrency, facts.ReadTime
	}
	return concurrency, time.Now().UTC().Format(time.RFC3339)
}

func runPass1(ctx context.Context, facts *extract.FactsFile, table *InternTable, client ModelClient, reg *icons.Registry) (*Pass1Plan, error) {
	sysPrompt := Pass1SystemPrompt(reg.ValidKeys())
	schema := Pass1PlanSchema()
	var feedback []string

	for attempt := 0; attempt <= maxValidationRetries; attempt++ {
		userPrompt, err := BuildPass1UserPrompt(facts, table, feedback)
		if err != nil {
			return nil, err
		}
		raw, err := client.GenerateJSON(ctx, sysPrompt, userPrompt, schema)
		if err != nil {
			return nil, fmt.Errorf("pass 1 attempt %d: %w", attempt+1, err)
		}
		var plan Pass1Plan
		if err := json.Unmarshal(raw, &plan); err != nil {
			feedback = []string{fmt.Sprintf("invalid JSON response: %v", err)}
			continue
		}
		issues := validatePass1Plan(&plan, table, reg)
		if len(issues) == 0 {
			return &plan, nil
		}
		feedback = issues
	}

	return nil, fmt.Errorf("pass 1 failed pre-pass-2 gate after %d retries: %w", maxValidationRetries, validationErrorFromFeedback("PASS1_VALIDATION_FAILED", "", feedback))
}

func validationErrorFromFeedback(code, viewID string, feedback []string) *manifest.ValidationError {
	issues := make([]manifest.ValidationIssue, 0, len(feedback))
	for _, msg := range feedback {
		issues = append(issues, manifest.ValidationIssue{
			Code:    code,
			ViewID:  viewID,
			Message: msg,
		})
	}
	return &manifest.ValidationError{Issues: issues}
}

func validatePass1Plan(plan *Pass1Plan, table *InternTable, reg *icons.Registry) []string {
	accounted := make(map[string]bool, len(table.ResourceIDs()))
	compIDs, issues := validatePass1Components(plan.Components, table, reg, accounted)
	issues = append(issues, validatePass1Containers(plan.ContainerDefs, table, accounted)...)
	issues = append(issues, validatePass1Excluded(plan.ExcludedResources, table, accounted)...)
	issues = append(issues, validatePass1ResourceCoverage(table, accounted)...)
	issues = append(issues, validatePass1Views(plan.DetailViews, compIDs)...)
	return issues
}

func validatePass1Components(components []Pass1Component, table *InternTable, reg *icons.Registry, accounted map[string]bool) (map[string]bool, []string) {
	var issues []string
	compIDs := make(map[string]bool, len(components))
	for _, c := range components {
		if strings.TrimSpace(c.ID) == "" {
			issues = append(issues, "component id must not be empty")
			continue
		}
		if compIDs[c.ID] {
			issues = append(issues, fmt.Sprintf("duplicate component id %q", c.ID))
		}
		compIDs[c.ID] = true
		issues = append(issues, validateSinglePass1Component(c, table, reg, accounted)...)
	}
	return compIDs, issues
}

func validateSinglePass1Component(c Pass1Component, table *InternTable, reg *icons.Registry, accounted map[string]bool) []string {
	var issues []string
	if strings.TrimSpace(c.Title) == "" {
		issues = append(issues, fmt.Sprintf("component %q has empty title", c.ID))
	}
	if !reg.Has(c.IconKey) {
		issues = append(issues, fmt.Sprintf("component %q has invalid icon_key %q", c.ID, c.IconKey))
	}
	if len(c.BackedByIDs) == 0 {
		issues = append(issues, fmt.Sprintf("component %q has empty backed_by_ids", c.ID))
	}
	if len(c.InlineMeta) > 4 {
		issues = append(issues, fmt.Sprintf("component %q has %d inline_meta rows (max 4)", c.ID, len(c.InlineMeta)))
	}
	datastoreTypes := make(map[string]bool)
	for _, rid := range c.BackedByIDs {
		r, ok := table.ResourceByID(rid)
		if !ok {
			issues = append(issues, fmt.Sprintf("component %q references unknown resource id %q", c.ID, rid))
			continue
		}
		accounted[rid] = true
		if statefulDatastoreAssetTypes[r.AssetType] {
			datastoreTypes[r.AssetType] = true
		}
	}
	if len(datastoreTypes) > 1 {
		types := make([]string, 0, len(datastoreTypes))
		for dt := range datastoreTypes {
			types = append(types, dt)
		}
		slices.Sort(types)
		issues = append(issues, fmt.Sprintf("component %q mixes heterogeneous stateful datastores: %s", c.ID, strings.Join(types, ", ")))
	}
	return issues
}

func validatePass1Containers(containers []Pass1ContainerDef, table *InternTable, accounted map[string]bool) []string {
	var issues []string
	contByID := make(map[string]Pass1ContainerDef, len(containers))
	for _, ct := range containers {
		if strings.TrimSpace(ct.ID) == "" {
			issues = append(issues, "container id must not be empty")
			continue
		}
		if _, dup := contByID[ct.ID]; dup {
			issues = append(issues, fmt.Sprintf("duplicate container id %q", ct.ID))
		}
		contByID[ct.ID] = ct
		for _, rid := range ct.BackedByIDs {
			if _, ok := table.ResourceByID(rid); !ok {
				issues = append(issues, fmt.Sprintf("container %q references unknown resource id %q", ct.ID, rid))
				continue
			}
			accounted[rid] = true
		}
	}
	for _, ct := range containers {
		if msg := validatePass1ContainerParent(ct, contByID); msg != "" {
			issues = append(issues, msg)
		}
	}
	return issues
}

func validatePass1ContainerParent(ct Pass1ContainerDef, contByID map[string]Pass1ContainerDef) string {
	if ct.ParentContainerID == "" {
		return ""
	}
	if _, ok := contByID[ct.ParentContainerID]; !ok {
		return fmt.Sprintf("container %q references unknown parent_container_id %q", ct.ID, ct.ParentContainerID)
	}
	depth := 1
	seen := map[string]bool{ct.ID: true}
	cur := ct.ParentContainerID
	for cur != "" {
		if seen[cur] {
			return fmt.Sprintf("container %q has parent cycle via %q", ct.ID, cur)
		}
		seen[cur] = true
		depth++
		cur = contByID[cur].ParentContainerID
	}
	if depth > 2 {
		return fmt.Sprintf("container %q nesting depth %d exceeds max 2", ct.ID, depth)
	}
	return ""
}

func validatePass1Excluded(excluded []Pass1ExcludedResource, table *InternTable, accounted map[string]bool) []string {
	var issues []string
	for _, ex := range excluded {
		if _, ok := table.ResourceByID(ex.ResourceID); !ok {
			issues = append(issues, fmt.Sprintf("excluded_resources references unknown resource id %q", ex.ResourceID))
			continue
		}
		if strings.TrimSpace(ex.Reason) == "" {
			issues = append(issues, fmt.Sprintf("excluded resource %q missing reason", ex.ResourceID))
		}
		accounted[ex.ResourceID] = true
	}
	return issues
}

func validatePass1ResourceCoverage(table *InternTable, accounted map[string]bool) []string {
	var missingRes []string
	for _, rid := range table.ResourceIDs() {
		if !accounted[rid] {
			missingRes = append(missingRes, rid)
		}
	}
	if len(missingRes) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("unaccounted resource IDs (must appear in components, container_defs, or excluded_resources): %s", strings.Join(missingRes, ", "))}
}

func validatePass1Views(views []Pass1DetailView, compIDs map[string]bool) []string {
	var issues []string
	if len(views) == 0 {
		issues = append(issues, "detail_views must contain at least one view")
	}
	viewIDs := make(map[string]bool, len(views))
	placedComp := make(map[string]bool, len(compIDs))
	for _, dv := range views {
		issues = append(issues, validateSinglePass1View(dv, compIDs, viewIDs, placedComp)...)
	}

	var unplaced []string
	for cid := range compIDs {
		if !placedComp[cid] {
			unplaced = append(unplaced, cid)
		}
	}
	slices.Sort(unplaced)
	if len(unplaced) > 0 {
		issues = append(issues, fmt.Sprintf("components not assigned to any detail view: %s", strings.Join(unplaced, ", ")))
	}
	return issues
}

func validateSinglePass1View(dv Pass1DetailView, compIDs, viewIDs, placedComp map[string]bool) []string {
	var issues []string
	if strings.TrimSpace(dv.ID) == "" || dv.ID == manifest.SystemIndexViewID {
		issues = append(issues, fmt.Sprintf("invalid detail view id %q", dv.ID))
	} else if viewIDs[dv.ID] {
		issues = append(issues, fmt.Sprintf("duplicate detail view id %q", dv.ID))
	}
	viewIDs[dv.ID] = true
	if len(dv.ComponentIDs) < 1 || len(dv.ComponentIDs) > 12 {
		issues = append(issues, fmt.Sprintf("detail view %q has %d components (allowed 1..12)", dv.ID, len(dv.ComponentIDs)))
	}
	for _, cid := range dv.ComponentIDs {
		if !compIDs[cid] {
			issues = append(issues, fmt.Sprintf("detail view %q references unknown component_id %q", dv.ID, cid))
			continue
		}
		placedComp[cid] = true
	}
	return issues
}

func materializePlanGlobals(plan *Pass1Plan, table *InternTable) ([]manifest.Component, []manifest.Container, []manifest.ExcludedResource) {
	components := make([]manifest.Component, 0, len(plan.Components))
	for _, c := range plan.Components {
		components = append(components, materializeComponent(c, table))
	}
	containers := make([]manifest.Container, 0, len(plan.ContainerDefs))
	for _, ct := range plan.ContainerDefs {
		containers = append(containers, materializeContainer(ct, table))
	}
	excluded := make([]manifest.ExcludedResource, 0, len(plan.ExcludedResources))
	for _, ex := range plan.ExcludedResources {
		if r, ok := table.ResourceByID(ex.ResourceID); ok {
			excluded = append(excluded, manifest.ExcludedResource{
				URI:       r.URI,
				AssetType: r.AssetType,
				ProjectID: r.ProjectID,
				Reason:    ex.Reason,
			})
		}
	}
	return components, containers, excluded
}

func materializeComponent(c Pass1Component, table *InternTable) manifest.Component {
	backed := make([]manifest.BackedResource, 0, len(c.BackedByIDs))
	seen := make(map[string]bool, len(c.BackedByIDs))
	for _, rid := range c.BackedByIDs {
		r, ok := table.ResourceByID(rid)
		if !ok || seen[r.URI] {
			continue
		}
		seen[r.URI] = true
		backed = append(backed, manifest.BackedResource{
			URI:       r.URI,
			AssetType: r.AssetType,
			ProjectID: r.ProjectID,
		})
	}
	meta := make([]manifest.InlineMeta, 0, len(c.InlineMeta))
	for _, im := range c.InlineMeta {
		meta = append(meta, manifest.InlineMeta{Label: im.Label, Value: im.Value})
	}
	return manifest.Component{
		ID:            c.ID,
		Title:         c.Title,
		Subtitle:      c.Subtitle,
		IconKey:       c.IconKey,
		ComponentKind: c.ComponentKind,
		ProjectID:     c.ProjectID,
		BackedBy:      backed,
		InlineMeta:    meta,
	}
}

func materializeContainer(ct Pass1ContainerDef, table *InternTable) manifest.Container {
	backed := make([]manifest.BackedResource, 0, len(ct.BackedByIDs))
	seen := make(map[string]bool, len(ct.BackedByIDs))
	for _, rid := range ct.BackedByIDs {
		r, ok := table.ResourceByID(rid)
		if !ok || seen[r.URI] {
			continue
		}
		seen[r.URI] = true
		backed = append(backed, manifest.BackedResource{
			URI:       r.URI,
			AssetType: r.AssetType,
			ProjectID: r.ProjectID,
		})
	}
	return manifest.Container{
		ID:                ct.ID,
		Title:             ct.Title,
		Subtitle:          ct.Subtitle,
		Kind:              ct.Kind,
		ParentContainerID: ct.ParentContainerID,
		BackedBy:          backed,
	}
}

type pass2Runner struct {
	plan             *Pass1Plan
	table            *InternTable
	compByID         map[string]manifest.Component
	contByID         map[string]manifest.Container
	factEdgeByID     map[string]extract.FactEdge
	allDetailViewIDs map[string]bool
	client           ModelClient
}

func newPass2Runner(
	plan *Pass1Plan,
	table *InternTable,
	components []manifest.Component,
	containers []manifest.Container,
	client ModelClient,
) *pass2Runner {
	compByID := make(map[string]manifest.Component, len(components))
	for _, c := range components {
		compByID[c.ID] = c
	}
	contByID := make(map[string]manifest.Container, len(containers))
	for _, ct := range containers {
		contByID[ct.ID] = ct
	}
	factEdgeByID := make(map[string]extract.FactEdge, len(table.EdgeIDs()))
	for _, eid := range table.EdgeIDs() {
		fe, _ := table.EdgeByID(eid)
		factEdgeByID[fe.ID] = fe
	}
	allDetailViewIDs := make(map[string]bool, len(plan.DetailViews))
	for _, dv := range plan.DetailViews {
		allDetailViewIDs[dv.ID] = true
	}
	return &pass2Runner{
		plan:             plan,
		table:            table,
		compByID:         compByID,
		contByID:         contByID,
		factEdgeByID:     factEdgeByID,
		allDetailViewIDs: allDetailViewIDs,
		client:           client,
	}
}

func (r *pass2Runner) populateViews(ctx context.Context, concurrency int) ([]manifest.View, error) {
	detailViews := make([]manifest.View, len(r.plan.DetailViews))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)

	for i, dv := range r.plan.DetailViews {
		g.Go(func() error {
			populated, err := r.populateView(gctx, dv)
			if err != nil {
				return err
			}
			detailViews[i] = populated
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	slices.SortFunc(detailViews, func(a, b manifest.View) int {
		return cmp.Compare(a.ID, b.ID)
	})
	return detailViews, nil
}

func (r *pass2Runner) populateView(ctx context.Context, dv Pass1DetailView) (manifest.View, error) {
	sysPrompt := Pass2SystemPrompt()
	schema := Pass2ViewSchema()
	var feedback []string

	for attempt := 0; attempt <= maxValidationRetries; attempt++ {
		userPrompt, err := BuildPass2UserPrompt(dv, r.plan, r.table, feedback)
		if err != nil {
			return manifest.View{}, err
		}
		raw, err := r.client.GenerateJSON(ctx, sysPrompt, userPrompt, schema)
		if err != nil {
			return manifest.View{}, fmt.Errorf("pass 2 view %q attempt %d: %w", dv.ID, attempt+1, err)
		}
		var pv Pass2View
		if err := json.Unmarshal(raw, &pv); err != nil {
			feedback = []string{fmt.Sprintf("invalid JSON response: %v", err)}
			continue
		}

		mv, convIssues := materializePass2View(dv, pv, r.table)
		issues := slices.Concat(convIssues, validatePopulatedView(mv, dv.ComponentIDs, r.compByID, r.contByID, r.factEdgeByID, r.allDetailViewIDs))
		if len(issues) == 0 {
			return mv, nil
		}
		feedback = issues
	}

	return manifest.View{}, fmt.Errorf("pass 2 view %q failed validation after %d retries: %w", dv.ID, maxValidationRetries, validationErrorFromFeedback("PASS2_VALIDATION_FAILED", dv.ID, feedback))
}

func materializePass2View(dv Pass1DetailView, pv Pass2View, table *InternTable) (manifest.View, []string) {
	var issues []string
	placements := make([]manifest.Placement, 0, len(pv.Placements))
	for _, p := range pv.Placements {
		placements = append(placements, materializePlacement(p))
	}
	edges := make([]manifest.Edge, 0, len(pv.Edges))
	for _, e := range pv.Edges {
		me, edgeIssues := materializeEdge(e, table)
		issues = append(issues, edgeIssues...)
		edges = append(edges, me)
	}
	return manifest.View{
		ID:           dv.ID,
		Title:        dv.Title,
		Subtitle:     dv.Subtitle,
		Archetype:    dv.Archetype,
		AccentMode:   dv.AccentMode,
		ContainerIDs: slices.Clone(pv.ContainerIDs),
		Placements:   placements,
		Edges:        edges,
	}, issues
}

func materializePlacement(p Pass2Placement) manifest.Placement {
	refs := make([]manifest.OffPageRef, 0, len(p.OffPageRefs))
	for _, r := range p.OffPageRefs {
		refs = append(refs, manifest.OffPageRef{
			TargetViewID: r.TargetViewID,
			Label:        r.Label,
			Direction:    r.Direction,
		})
	}
	return manifest.Placement{
		ComponentID: p.ComponentID,
		ContainerID: p.ContainerID,
		Column:      p.Column,
		OffPageRefs: refs,
	}
}

func materializeEdge(e Pass2Edge, table *InternTable) (manifest.Edge, []string) {
	var issues []string
	var evIDs []string
	for _, shortID := range e.EvidenceEdgeIDs {
		fe, ok := table.EdgeByID(shortID)
		if !ok {
			issues = append(issues, fmt.Sprintf("edge (%s -> %s) cites unknown evidence edge handle %q", e.From, e.To, shortID))
			continue
		}
		evIDs = append(evIDs, fe.ID)
	}
	return manifest.Edge{
		From:               e.From,
		To:                 e.To,
		Verb:               e.Verb,
		FlowKind:           e.FlowKind,
		Provenance:         e.Provenance,
		EvidenceEdgeIDs:    evIDs,
		InferenceRationale: e.InferenceRationale,
		ViaBusID:           e.ViaBusID,
	}, issues
}

func validatePopulatedView(
	v manifest.View,
	expectedCompIDs []string,
	compByID map[string]manifest.Component,
	contByID map[string]manifest.Container,
	factEdgeByID map[string]extract.FactEdge,
	allDetailViewIDs map[string]bool,
) []string {
	maxCol := 4
	if v.Archetype == manifest.ArchetypeHubSpoke {
		maxCol = 2
	}

	viewContSet, issues := validatePopulatedViewContainers(v.ContainerIDs, contByID)
	placed, placementIssues := validatePopulatedViewPlacements(v, expectedCompIDs, maxCol, compByID, viewContSet, allDetailViewIDs)
	issues = append(issues, placementIssues...)
	issues = append(issues, validatePopulatedViewEdges(v.Edges, placed, compByID, factEdgeByID)...)
	return issues
}

func validatePopulatedViewContainers(containerIDs []string, contByID map[string]manifest.Container) (map[string]bool, []string) {
	var issues []string
	viewContSet := make(map[string]bool, len(containerIDs))
	for _, cid := range containerIDs {
		if _, ok := contByID[cid]; !ok {
			issues = append(issues, fmt.Sprintf("unknown container_id %q", cid))
			continue
		}
		viewContSet[cid] = true
	}
	for _, cid := range containerIDs {
		ct, ok := contByID[cid]
		if !ok {
			continue
		}
		if depth := populatedContainerDepth(cid, ct, contByID, viewContSet); depth > 2 {
			issues = append(issues, fmt.Sprintf("container %q nesting depth %d exceeds max 2", cid, depth))
		}
	}
	return viewContSet, issues
}

func populatedContainerDepth(cid string, ct manifest.Container, contByID map[string]manifest.Container, viewContSet map[string]bool) int {
	depth := 1
	cur := ct.ParentContainerID
	seen := map[string]bool{cid: true}
	for cur != "" && !seen[cur] {
		seen[cur] = true
		if viewContSet[cur] {
			depth++
		}
		cur = contByID[cur].ParentContainerID
	}
	return depth
}

func validatePopulatedViewPlacements(
	v manifest.View,
	expectedCompIDs []string,
	maxCol int,
	compByID map[string]manifest.Component,
	viewContSet, allDetailViewIDs map[string]bool,
) (map[string]bool, []string) {
	var issues []string
	if len(v.Placements) < 1 || len(v.Placements) > 12 {
		issues = append(issues, fmt.Sprintf("view %q has %d placements (allowed 1..12)", v.ID, len(v.Placements)))
	}

	placed := make(map[string]bool, len(v.Placements))
	for _, p := range v.Placements {
		issues = append(issues, validateSinglePopulatedPlacement(p, v.ID, maxCol, compByID, placed, viewContSet, allDetailViewIDs)...)
	}
	for _, cid := range expectedCompIDs {
		if !placed[cid] {
			issues = append(issues, fmt.Sprintf("locked component %q is missing from placements", cid))
		}
	}
	return placed, issues
}

func validateSinglePopulatedPlacement(
	p manifest.Placement,
	viewID string,
	maxCol int,
	compByID map[string]manifest.Component,
	placed, viewContSet, allDetailViewIDs map[string]bool,
) []string {
	var issues []string
	if placed[p.ComponentID] {
		issues = append(issues, fmt.Sprintf("duplicate placement for component %q", p.ComponentID))
	}
	placed[p.ComponentID] = true
	if _, ok := compByID[p.ComponentID]; !ok {
		issues = append(issues, fmt.Sprintf("placement references unknown component %q", p.ComponentID))
	}
	if p.ContainerID != "" && !viewContSet[p.ContainerID] {
		issues = append(issues, fmt.Sprintf("placement %q container_id %q not in container_ids", p.ComponentID, p.ContainerID))
	}
	if p.Column < 0 || p.Column > maxCol {
		issues = append(issues, fmt.Sprintf("placement %q column %d out of bounds 0..%d", p.ComponentID, p.Column, maxCol))
	}
	for _, ref := range p.OffPageRefs {
		if !allDetailViewIDs[ref.TargetViewID] || ref.TargetViewID == viewID {
			issues = append(issues, fmt.Sprintf("placement %q off_page_ref target_view_id %q invalid", p.ComponentID, ref.TargetViewID))
		}
	}
	return issues
}

func validatePopulatedViewEdges(
	edges []manifest.Edge,
	placed map[string]bool,
	compByID map[string]manifest.Component,
	factEdgeByID map[string]extract.FactEdge,
) []string {
	type busGroup struct {
		froms map[string]bool
		tos   map[string]bool
	}
	var issues []string
	buses := make(map[string]*busGroup)

	for _, e := range edges {
		issues = append(issues, validateSinglePopulatedEdge(e, placed, compByID, factEdgeByID)...)
		if e.ViaBusID != "" {
			bg, ok := buses[e.ViaBusID]
			if !ok {
				bg = &busGroup{froms: map[string]bool{}, tos: map[string]bool{}}
				buses[e.ViaBusID] = bg
			}
			bg.froms[e.From] = true
			bg.tos[e.To] = true
		}
	}
	for bid, bg := range buses {
		if len(bg.froms) > 1 && len(bg.tos) > 1 {
			issues = append(issues, fmt.Sprintf("via_bus_id %q mixes multiple sources and multiple targets", bid))
		}
	}
	return issues
}

func validateSinglePopulatedEdge(
	e manifest.Edge,
	placed map[string]bool,
	compByID map[string]manifest.Component,
	factEdgeByID map[string]extract.FactEdge,
) []string {
	var issues []string
	label := e.From + " -> " + e.To
	if !placed[e.From] || !placed[e.To] {
		issues = append(issues, fmt.Sprintf("edge (%s) references unplaced endpoint", label))
	}
	if strings.TrimSpace(e.Verb) == "" || utf8.RuneCountInString(e.Verb) > 16 {
		issues = append(issues, fmt.Sprintf("edge (%s) verb %q must be 1..16 chars", label, e.Verb))
	}
	switch e.Provenance {
	case manifest.ProvenanceVerified:
		if msg := validateVerifiedEdgeWalk(e, label, compByID, factEdgeByID); msg != "" {
			issues = append(issues, msg)
		}
	case manifest.ProvenanceInferred:
		if strings.TrimSpace(e.InferenceRationale) == "" {
			issues = append(issues, fmt.Sprintf("INFERRED edge (%s) requires non-empty inference_rationale", label))
		}
	default:
		issues = append(issues, fmt.Sprintf("edge (%s) invalid provenance %q", label, e.Provenance))
	}
	return issues
}

func validateVerifiedEdgeWalk(
	e manifest.Edge,
	label string,
	compByID map[string]manifest.Component,
	factEdgeByID map[string]extract.FactEdge,
) string {
	if len(e.EvidenceEdgeIDs) == 0 {
		return fmt.Sprintf("VERIFIED edge (%s) requires evidence_edge_ids", label)
	}
	fc, okF := compByID[e.From]
	tc, okT := compByID[e.To]
	if okF && okT && !hasDirectedWalk(e.EvidenceEdgeIDs, factEdgeByID, fc.BackedBy, tc.BackedBy) {
		return fmt.Sprintf("VERIFIED edge (%s) evidence_edge_ids do not form a connected directed walk from %s to %s", label, e.From, e.To)
	}
	return ""
}

// Reconcile diffs facts against an existing curated manifest. When zero URIs and zero projected
// inter-component FactEdges changed, it completes in 0 LLM calls (allowing client == nil).
// Otherwise it prunes vanished URIs/edges, calls client with ReconcilePatchSchema to place deltas
// while freezing existing titles, archetypes, notes, and rejected_edges, recomputes View 0, and
// validates via manifest.ValidateWithFacts.
func Reconcile(ctx context.Context, facts *extract.FactsFile, existing *manifest.Manifest, client ModelClient, reg *icons.Registry, opts Options) (*manifest.Manifest, error) {
	if err := validateReconcileInputs(facts, existing, reg); err != nil {
		return nil, err
	}

	cur := cloneManifest(existing)
	table := NewInternTable(facts)
	factEdgeByID, allFactEdges := indexFactEdges(facts.Edges)

	addedURIs, vanishedURIs := diffManifestURIs(cur, facts.Resources)
	invalidatedEdgeCount := refreshVerifiedEdges(cur, factEdgeByID, allFactEdges)
	newEdgeIDs := findUnrepresentedInterComponentEdges(cur, table)

	// Zero-LLM Fast Path: zero URI delta, zero invalidated edges, zero newly projected edges.
	if len(addedURIs) == 0 && len(vanishedURIs) == 0 && invalidatedEdgeCount == 0 && len(newEdgeIDs) == 0 {
		if err := manifest.ValidateWithFacts(cur, facts, reg); err != nil {
			return nil, err
		}
		return cur, nil
	}

	pruneManifest(cur, vanishedURIs, factEdgeByID, allFactEdges)

	unclaimedRIDs := findUnclaimedResourceIDs(cur, table)
	deltaEdgeIDs := findReconcileDeltaEdgeIDs(cur, unclaimedRIDs, table)
	if len(unclaimedRIDs) == 0 && len(deltaEdgeIDs) == 0 {
		rebuildSystemIndex(cur)
		if opts.GeneratedAt != "" {
			cur.Metadata.GeneratedAt = opts.GeneratedAt
		}
		if err := manifest.ValidateWithFacts(cur, facts, reg); err != nil {
			return nil, err
		}
		return cur, nil
	}

	if client == nil {
		return nil, fmt.Errorf("reconcile detected %d unclaimed resource(s) and %d new inter-component edge(s); vertex AI model client is required", len(unclaimedRIDs), len(deltaEdgeIDs))
	}
	delta := ReconcileDelta{UnclaimedResourceIDs: unclaimedRIDs, NewEdgeIDs: deltaEdgeIDs}
	runner := &reconcileRunner{
		cur:    cur,
		facts:  facts,
		delta:  delta,
		table:  table,
		client: client,
		reg:    reg,
		opts:   opts,
	}
	return runner.run(ctx)
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

func indexFactEdges(edges []extract.FactEdge) (map[string]extract.FactEdge, []extract.FactEdge) {
	factEdgeByID := make(map[string]extract.FactEdge, len(edges))
	for _, fe := range edges {
		if fe.ID == "" {
			fe.ID = extract.EdgeID(fe.Kind, fe.FromURI, fe.ToURI, fe.Detail)
		}
		factEdgeByID[fe.ID] = fe
	}
	allFactEdges := make([]extract.FactEdge, 0, len(factEdgeByID))
	for _, fe := range factEdgeByID {
		allFactEdges = append(allFactEdges, fe)
	}
	slices.SortFunc(allFactEdges, func(a, b extract.FactEdge) int {
		return cmp.Or(cmp.Compare(a.FromURI, b.FromURI), cmp.Compare(a.ToURI, b.ToURI), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ID, b.ID))
	})
	return factEdgeByID, allFactEdges
}

func diffManifestURIs(m *manifest.Manifest, resources []extract.FactResource) ([]string, map[string]bool) {
	factByURI := make(map[string]bool, len(resources))
	for _, r := range resources {
		factByURI[r.URI] = true
	}
	existingURIs := accountedURISet(m)

	var addedURIs []string
	for _, r := range resources {
		if !existingURIs[r.URI] {
			addedURIs = append(addedURIs, r.URI)
		}
	}
	slices.Sort(addedURIs)

	vanishedURIs := make(map[string]bool)
	for uri := range existingURIs {
		if !factByURI[uri] {
			vanishedURIs[uri] = true
		}
	}
	return addedURIs, vanishedURIs
}

func accountedURISet(m *manifest.Manifest) map[string]bool {
	out := make(map[string]bool)
	for _, c := range m.Components {
		for _, br := range c.BackedBy {
			out[br.URI] = true
		}
	}
	for _, ct := range m.Containers {
		for _, br := range ct.BackedBy {
			out[br.URI] = true
		}
	}
	for _, ex := range m.ExcludedResources {
		out[ex.URI] = true
	}
	return out
}

func refreshVerifiedEdges(cur *manifest.Manifest, factEdgeByID map[string]extract.FactEdge, allFactEdges []extract.FactEdge) int {
	compByID := make(map[string]manifest.Component, len(cur.Components))
	for _, c := range cur.Components {
		compByID[c.ID] = c
	}

	invalidated := 0
	for vi := range cur.Views {
		if cur.Views[vi].ID == manifest.SystemIndexViewID {
			continue
		}
		for ei := range cur.Views[vi].Edges {
			if !refreshSingleVerifiedEdge(&cur.Views[vi].Edges[ei], compByID, factEdgeByID, allFactEdges) {
				invalidated++
			}
		}
	}
	return invalidated
}

func refreshSingleVerifiedEdge(
	e *manifest.Edge,
	compByID map[string]manifest.Component,
	factEdgeByID map[string]extract.FactEdge,
	allFactEdges []extract.FactEdge,
) bool {
	if e.Provenance != manifest.ProvenanceVerified {
		return true
	}
	fc, okF := compByID[e.From]
	tc, okT := compByID[e.To]
	if !okF || !okT {
		return false
	}
	if hasDirectedWalk(e.EvidenceEdgeIDs, factEdgeByID, fc.BackedBy, tc.BackedBy) {
		return true
	}
	if walkIDs := findShortestWalkIDs(allFactEdges, fc.BackedBy, tc.BackedBy); len(walkIDs) > 0 {
		e.EvidenceEdgeIDs = walkIDs
		return true
	}
	return false
}

type reconcileRunner struct {
	cur    *manifest.Manifest
	facts  *extract.FactsFile
	delta  ReconcileDelta
	table  *InternTable
	client ModelClient
	reg    *icons.Registry
	opts   Options
}

func (r *reconcileRunner) run(ctx context.Context) (*manifest.Manifest, error) {
	sysPrompt := ReconcileSystemPrompt(r.reg.ValidKeys())
	schema := ReconcilePatchSchema()

	var feedback []string
	var lastValErr *manifest.ValidationError
	for attempt := 0; attempt <= maxValidationRetries; attempt++ {
		userPrompt, err := BuildReconcileUserPrompt(r.cur, r.delta, r.table, feedback)
		if err != nil {
			return nil, err
		}
		raw, err := r.client.GenerateJSON(ctx, sysPrompt, userPrompt, schema)
		if err != nil {
			return nil, fmt.Errorf("reconcile attempt %d: %w", attempt+1, err)
		}
		var patch ReconcilePatch
		if err := json.Unmarshal(raw, &patch); err != nil {
			feedback = []string{fmt.Sprintf("invalid JSON patch: %v", err)}
			lastValErr = validationErrorFromFeedback("RECONCILE_INVALID_JSON", "", feedback)
			continue
		}

		candidate, nextFeedback, valErr := r.tryApplyAndValidate(&patch)
		if candidate != nil {
			return candidate, nil
		}
		feedback = nextFeedback
		lastValErr = valErr
	}

	return nil, fmt.Errorf("reconcile failed validation after %d retries: %w", maxValidationRetries, lastValErr)
}

func (r *reconcileRunner) tryApplyAndValidate(patch *ReconcilePatch) (*manifest.Manifest, []string, *manifest.ValidationError) {
	candidate := cloneManifest(r.cur)
	mergeIssues := applyReconcilePatch(candidate, patch, r.table)
	rebuildSystemIndex(candidate)
	if r.opts.GeneratedAt != "" {
		candidate.Metadata.GeneratedAt = r.opts.GeneratedAt
	}

	if len(mergeIssues) > 0 {
		return nil, mergeIssues, validationErrorFromFeedback("RECONCILE_MERGE_FAILED", "", mergeIssues)
	}
	if err := manifest.ValidateWithFacts(candidate, r.facts, r.reg); err != nil {
		feedback := []string{err.Error()}
		var ve *manifest.ValidationError
		if errors.As(err, &ve) {
			return nil, feedback, ve
		}
		return nil, feedback, validationErrorFromFeedback("RECONCILE_VALIDATION_FAILED", "", feedback)
	}
	return candidate, nil, nil
}

func pruneManifest(m *manifest.Manifest, vanished map[string]bool, factEdgeByID map[string]extract.FactEdge, allFactEdges []extract.FactEdge) {
	removedComps := pruneComponents(m, vanished)
	pruneContainersAndExcluded(m, vanished)

	compByID := make(map[string]manifest.Component, len(m.Components))
	for _, c := range m.Components {
		compByID[c.ID] = c
	}

	keptViews, removedViews := pruneDetailViews(m.Views, removedComps, compByID, factEdgeByID, allFactEdges)
	if len(removedViews) > 0 {
		pruneRemovedOffPageRefs(keptViews, removedViews)
	}
	m.Views = keptViews
}

func pruneComponents(m *manifest.Manifest, vanished map[string]bool) map[string]bool {
	removedComps := make(map[string]bool)
	keptComps := make([]manifest.Component, 0, len(m.Components))
	for _, c := range m.Components {
		keptBacked := filterBackedResources(c.BackedBy, vanished)
		if len(keptBacked) == 0 {
			removedComps[c.ID] = true
			continue
		}
		c.BackedBy = keptBacked
		keptComps = append(keptComps, c)
	}
	m.Components = keptComps
	return removedComps
}

func pruneContainersAndExcluded(m *manifest.Manifest, vanished map[string]bool) {
	for i := range m.Containers {
		if len(m.Containers[i].BackedBy) > 0 {
			m.Containers[i].BackedBy = filterBackedResources(m.Containers[i].BackedBy, vanished)
		}
	}
	keptExcl := make([]manifest.ExcludedResource, 0, len(m.ExcludedResources))
	for _, ex := range m.ExcludedResources {
		if !vanished[ex.URI] {
			keptExcl = append(keptExcl, ex)
		}
	}
	m.ExcludedResources = keptExcl
}

func filterBackedResources(in []manifest.BackedResource, vanished map[string]bool) []manifest.BackedResource {
	kept := make([]manifest.BackedResource, 0, len(in))
	for _, br := range in {
		if !vanished[br.URI] {
			kept = append(kept, br)
		}
	}
	return kept
}

func pruneDetailViews(
	views []manifest.View,
	removedComps map[string]bool,
	compByID map[string]manifest.Component,
	factEdgeByID map[string]extract.FactEdge,
	allFactEdges []extract.FactEdge,
) ([]manifest.View, map[string]bool) {
	keptViews := make([]manifest.View, 0, len(views))
	removedViews := make(map[string]bool)
	for _, v := range views {
		if v.ID == manifest.SystemIndexViewID {
			continue
		}
		keptPlacements, placed := pruneViewPlacements(v.Placements, removedComps)
		if len(keptPlacements) == 0 {
			removedViews[v.ID] = true
			continue
		}
		v.Placements = keptPlacements
		v.Edges = pruneViewEdges(v.Edges, placed, compByID, factEdgeByID, allFactEdges)
		keptViews = append(keptViews, v)
	}
	return keptViews, removedViews
}

func pruneViewPlacements(placements []manifest.Placement, removedComps map[string]bool) ([]manifest.Placement, map[string]bool) {
	kept := make([]manifest.Placement, 0, len(placements))
	placed := make(map[string]bool, len(placements))
	for _, p := range placements {
		if removedComps[p.ComponentID] {
			continue
		}
		kept = append(kept, p)
		placed[p.ComponentID] = true
	}
	return kept, placed
}

func pruneViewEdges(
	edges []manifest.Edge,
	placed map[string]bool,
	compByID map[string]manifest.Component,
	factEdgeByID map[string]extract.FactEdge,
	allFactEdges []extract.FactEdge,
) []manifest.Edge {
	kept := make([]manifest.Edge, 0, len(edges))
	for _, e := range edges {
		if !placed[e.From] || !placed[e.To] {
			continue
		}
		if e.Provenance == manifest.ProvenanceVerified {
			if !refreshSingleVerifiedEdge(&e, compByID, factEdgeByID, allFactEdges) {
				continue
			}
		}
		kept = append(kept, e)
	}
	return kept
}

func pruneRemovedOffPageRefs(views []manifest.View, removedViews map[string]bool) {
	for vi := range views {
		for pi := range views[vi].Placements {
			refs := views[vi].Placements[pi].OffPageRefs
			if len(refs) == 0 {
				continue
			}
			keptRefs := make([]manifest.OffPageRef, 0, len(refs))
			for _, r := range refs {
				if !removedViews[r.TargetViewID] {
					keptRefs = append(keptRefs, r)
				}
			}
			views[vi].Placements[pi].OffPageRefs = keptRefs
		}
	}
}

func applyReconcilePatch(m *manifest.Manifest, patch *ReconcilePatch, table *InternTable) []string {
	compIdx := make(map[string]int, len(m.Components))
	for i, c := range m.Components {
		compIdx[c.ID] = i
	}
	contIdx := make(map[string]int, len(m.Containers))
	for i, ct := range m.Containers {
		contIdx[ct.ID] = i
	}
	viewIdx := make(map[string]int, len(m.Views))
	for i, v := range m.Views {
		viewIdx[v.ID] = i
	}
	rejected := make(map[[2]string]bool, len(m.RejectedEdges))
	for _, re := range m.RejectedEdges {
		rejected[[2]string{re.From, re.To}] = true
	}

	var issues []string
	issues = append(issues, applyAppendBackedBy(m, patch, compIdx, contIdx, table)...)
	issues = append(issues, applyNewEntities(m, patch, compIdx, contIdx, table)...)
	issues = append(issues, applyNewViewItems(m, patch, viewIdx, rejected, table)...)
	issues = append(issues, applyNewViews(m, patch.NewViews, viewIdx, rejected, table)...)
	return issues
}

func appendUniqueBackedResources(dst *[]manifest.BackedResource, resourceIDs []string, kind, targetID string, table *InternTable) []string {
	have := make(map[string]bool, len(*dst))
	for _, br := range *dst {
		have[br.URI] = true
	}
	var issues []string
	for _, rid := range resourceIDs {
		r, ok := table.ResourceByID(rid)
		if !ok {
			issues = append(issues, fmt.Sprintf("%s %q unknown resource %q", kind, targetID, rid))
			continue
		}
		if !have[r.URI] {
			have[r.URI] = true
			*dst = append(*dst, manifest.BackedResource{
				URI:       r.URI,
				AssetType: r.AssetType,
				ProjectID: r.ProjectID,
			})
		}
	}
	return issues
}

func applyAppendBackedBy(m *manifest.Manifest, patch *ReconcilePatch, compIdx, contIdx map[string]int, table *InternTable) []string {
	var issues []string
	for _, ab := range patch.AppendBackedBy {
		idx, ok := compIdx[ab.ComponentID]
		if !ok {
			issues = append(issues, fmt.Sprintf("append_backed_by references unknown component %q", ab.ComponentID))
			continue
		}
		issues = append(issues, appendUniqueBackedResources(&m.Components[idx].BackedBy, ab.ResourceIDs, "append_backed_by component", ab.ComponentID, table)...)
	}
	for _, ab := range patch.AppendContainerBackedBy {
		idx, ok := contIdx[ab.ContainerID]
		if !ok {
			issues = append(issues, fmt.Sprintf("append_container_backed_by references unknown container %q", ab.ContainerID))
			continue
		}
		issues = append(issues, appendUniqueBackedResources(&m.Containers[idx].BackedBy, ab.ResourceIDs, "append_container_backed_by container", ab.ContainerID, table)...)
	}
	return issues
}

func applyNewEntities(m *manifest.Manifest, patch *ReconcilePatch, compIdx, contIdx map[string]int, table *InternTable) []string {
	var issues []string
	issues = append(issues, applyNewComponents(m, patch.NewComponents, compIdx, table)...)
	issues = append(issues, applyNewContainers(m, patch.NewContainers, contIdx, table)...)
	issues = append(issues, applyNewExcludedResources(m, patch.NewExcludedResources, table)...)
	return issues
}

func checkKnownResourceIDs(kind, id string, resourceIDs []string, table *InternTable) []string {
	var issues []string
	for _, rid := range resourceIDs {
		if _, ok := table.ResourceByID(rid); !ok {
			issues = append(issues, fmt.Sprintf("%s %q references unknown resource %q", kind, id, rid))
		}
	}
	return issues
}

func applyNewComponents(m *manifest.Manifest, newComps []Pass1Component, compIdx map[string]int, table *InternTable) []string {
	var issues []string
	for _, nc := range newComps {
		if _, dup := compIdx[nc.ID]; dup {
			issues = append(issues, fmt.Sprintf("new_components id %q collides with existing component", nc.ID))
			continue
		}
		issues = append(issues, checkKnownResourceIDs("new_components", nc.ID, nc.BackedByIDs, table)...)
		compIdx[nc.ID] = len(m.Components)
		m.Components = append(m.Components, materializeComponent(nc, table))
	}
	return issues
}

func applyNewContainers(m *manifest.Manifest, newConts []Pass1ContainerDef, contIdx map[string]int, table *InternTable) []string {
	var issues []string
	for _, nct := range newConts {
		if _, dup := contIdx[nct.ID]; dup {
			issues = append(issues, fmt.Sprintf("new_containers id %q collides with existing container", nct.ID))
			continue
		}
		issues = append(issues, checkKnownResourceIDs("new_containers", nct.ID, nct.BackedByIDs, table)...)
		contIdx[nct.ID] = len(m.Containers)
		m.Containers = append(m.Containers, materializeContainer(nct, table))
	}
	return issues
}

func applyNewExcludedResources(m *manifest.Manifest, newExcl []Pass1ExcludedResource, table *InternTable) []string {
	var issues []string
	for _, nex := range newExcl {
		r, ok := table.ResourceByID(nex.ResourceID)
		if !ok {
			issues = append(issues, fmt.Sprintf("new_excluded_resources unknown resource %q", nex.ResourceID))
			continue
		}
		m.ExcludedResources = append(m.ExcludedResources, manifest.ExcludedResource{
			URI:       r.URI,
			AssetType: r.AssetType,
			ProjectID: r.ProjectID,
			Reason:    nex.Reason,
		})
	}
	return issues
}

func applyNewViewItems(m *manifest.Manifest, patch *ReconcilePatch, viewIdx map[string]int, rejected map[[2]string]bool, table *InternTable) []string {
	var issues []string
	for _, np := range patch.NewPlacements {
		idx, ok := viewIdx[np.ViewID]
		if !ok || np.ViewID == manifest.SystemIndexViewID {
			issues = append(issues, fmt.Sprintf("new_placements references unknown or index view %q", np.ViewID))
			continue
		}
		mp := materializePlacement(np.Placement)
		if mp.ContainerID != "" && !slices.Contains(m.Views[idx].ContainerIDs, mp.ContainerID) {
			m.Views[idx].ContainerIDs = append(m.Views[idx].ContainerIDs, mp.ContainerID)
		}
		m.Views[idx].Placements = append(m.Views[idx].Placements, mp)
	}
	for _, ne := range patch.NewEdges {
		idx, ok := viewIdx[ne.ViewID]
		if !ok || ne.ViewID == manifest.SystemIndexViewID {
			issues = append(issues, fmt.Sprintf("new_edges references unknown or index view %q", ne.ViewID))
			continue
		}
		if rejected[[2]string{ne.Edge.From, ne.Edge.To}] {
			issues = append(issues, fmt.Sprintf("new_edges (%s -> %s) is forbidden by rejected_edges", ne.Edge.From, ne.Edge.To))
			continue
		}
		me, edgeIssues := materializeEdge(ne.Edge, table)
		issues = append(issues, edgeIssues...)
		m.Views[idx].Edges = append(m.Views[idx].Edges, me)
	}
	return issues
}

func applyNewViews(m *manifest.Manifest, newViews []ReconcileNewView, viewIdx map[string]int, rejected map[[2]string]bool, table *InternTable) []string {
	var issues []string
	for _, nv := range newViews {
		if nv.ID == manifest.SystemIndexViewID {
			issues = append(issues, "new_views must not define view_0_system_index")
			continue
		}
		if _, dup := viewIdx[nv.ID]; dup {
			issues = append(issues, fmt.Sprintf("new_views id %q collides with existing view", nv.ID))
			continue
		}
		placements := make([]manifest.Placement, 0, len(nv.Placements))
		for _, p := range nv.Placements {
			placements = append(placements, materializePlacement(p))
		}
		edges := make([]manifest.Edge, 0, len(nv.Edges))
		for _, e := range nv.Edges {
			if rejected[[2]string{e.From, e.To}] {
				issues = append(issues, fmt.Sprintf("new_views %q edge (%s -> %s) is forbidden by rejected_edges", nv.ID, e.From, e.To))
				continue
			}
			me, edgeIssues := materializeEdge(e, table)
			issues = append(issues, edgeIssues...)
			edges = append(edges, me)
		}
		viewIdx[nv.ID] = len(m.Views)
		m.Views = append(m.Views, manifest.View{
			ID:           nv.ID,
			Title:        nv.Title,
			Subtitle:     nv.Subtitle,
			Archetype:    nv.Archetype,
			AccentMode:   nv.AccentMode,
			ContainerIDs: slices.Clone(nv.ContainerIDs),
			Placements:   placements,
			Edges:        edges,
		})
	}
	return issues
}

func rebuildSystemIndex(m *manifest.Manifest) {
	detail := make([]manifest.View, 0, len(m.Views))
	for _, v := range m.Views {
		if v.ID != manifest.SystemIndexViewID {
			detail = append(detail, v)
		}
	}
	m.Views = detail
	if v0 := BuildSystemIndexView(m); v0 != nil {
		m.Views = append([]manifest.View{*v0}, m.Views...)
	}
}

func findUnclaimedResourceIDs(m *manifest.Manifest, table *InternTable) []string {
	accounted := accountedURISet(m)
	var unclaimed []string
	for _, rid := range table.ResourceIDs() {
		r, _ := table.ResourceByID(rid)
		if !accounted[r.URI] {
			unclaimed = append(unclaimed, rid)
		}
	}
	return unclaimed
}

func findReconcileDeltaEdgeIDs(m *manifest.Manifest, unclaimedRIDs []string, table *InternTable) []string {
	included := make(map[string]bool)
	for _, eid := range findUnrepresentedInterComponentEdges(m, table) {
		included[eid] = true
	}
	if len(unclaimedRIDs) > 0 {
		addUnclaimedIncidentEdges(m, unclaimedRIDs, table, included)
	}

	out := make([]string, 0, len(included))
	for eid := range included {
		out = append(out, eid)
	}
	slices.Sort(out)
	return out
}

func addUnclaimedIncidentEdges(m *manifest.Manifest, unclaimedRIDs []string, table *InternTable, included map[string]bool) {
	unclaimedURIs := resolveResourceURISet(unclaimedRIDs, table)
	bridgeURIs := manifestBridgeURISet(m)

	activeBridges := make(map[string]bool)
	for _, eid := range table.EdgeIDs() {
		fe, _ := table.EdgeByID(eid)
		fromUnc, toUnc := unclaimedURIs[fe.FromURI], unclaimedURIs[fe.ToURI]
		if !fromUnc && !toUnc {
			continue
		}
		included[eid] = true
		markActiveBridge(fe, fromUnc, toUnc, bridgeURIs, activeBridges)
	}
	if len(activeBridges) > 0 {
		includeEdgesTouchingURIs(table, activeBridges, included)
	}
}

func resolveResourceURISet(rids []string, table *InternTable) map[string]bool {
	out := make(map[string]bool, len(rids))
	for _, rid := range rids {
		if r, ok := table.ResourceByID(rid); ok {
			out[r.URI] = true
		}
	}
	return out
}

func markActiveBridge(fe extract.FactEdge, fromUnc, toUnc bool, bridgeURIs, activeBridges map[string]bool) {
	if fromUnc && bridgeURIs[fe.ToURI] {
		activeBridges[fe.ToURI] = true
	}
	if toUnc && bridgeURIs[fe.FromURI] {
		activeBridges[fe.FromURI] = true
	}
}

func includeEdgesTouchingURIs(table *InternTable, uris, included map[string]bool) {
	for _, eid := range table.EdgeIDs() {
		fe, _ := table.EdgeByID(eid)
		if uris[fe.FromURI] || uris[fe.ToURI] {
			included[eid] = true
		}
	}
}

func manifestBridgeURISet(m *manifest.Manifest) map[string]bool {
	bridgeURIs := make(map[string]bool, len(m.ExcludedResources)+len(m.Containers))
	for _, ex := range m.ExcludedResources {
		bridgeURIs[ex.URI] = true
	}
	for _, ct := range m.Containers {
		for _, br := range ct.BackedBy {
			bridgeURIs[br.URI] = true
		}
	}
	return bridgeURIs
}

func findUnrepresentedInterComponentEdges(m *manifest.Manifest, table *InternTable) []string {
	uriToComps := make(map[string][]string)
	for _, c := range m.Components {
		for _, br := range c.BackedBy {
			uriToComps[br.URI] = append(uriToComps[br.URI], c.ID)
		}
	}
	rejected := make(map[[2]string]bool, len(m.RejectedEdges))
	for _, re := range m.RejectedEdges {
		rejected[[2]string{re.From, re.To}] = true
	}
	represented := make(map[[2]string]bool)
	for _, v := range m.Views {
		if v.ID == manifest.SystemIndexViewID {
			continue
		}
		for _, e := range v.Edges {
			represented[[2]string{e.From, e.To}] = true
		}
	}

	var delta []string
	for _, eid := range table.EdgeIDs() {
		fe, _ := table.EdgeByID(eid)
		if hasUnrepresentedPair(uriToComps[fe.FromURI], uriToComps[fe.ToURI], rejected, represented) {
			delta = append(delta, eid)
		}
	}
	return delta
}

func hasUnrepresentedPair(fromComps, toComps []string, rejected, represented map[[2]string]bool) bool {
	for _, fc := range fromComps {
		for _, tc := range toComps {
			if fc == tc {
				continue
			}
			pair := [2]string{fc, tc}
			if !rejected[pair] && !represented[pair] {
				return true
			}
		}
	}
	return false
}

func hasDirectedWalk(edgeIDs []string, factEdgeByID map[string]extract.FactEdge, fromBacked, toBacked []manifest.BackedResource) bool {
	if len(edgeIDs) == 0 {
		return false
	}
	cited := make([]extract.FactEdge, 0, len(edgeIDs))
	for _, id := range edgeIDs {
		fe, ok := factEdgeByID[id]
		if !ok {
			return false
		}
		cited = append(cited, fe)
	}
	fromSet := backedURISet(fromBacked)
	toSet := backedURISet(toBacked)

	fwd := make(map[string][]string, len(cited))
	rev := make(map[string][]string, len(cited))
	for _, fe := range cited {
		fwd[fe.FromURI] = append(fwd[fe.FromURI], fe.ToURI)
		rev[fe.ToURI] = append(rev[fe.ToURI], fe.FromURI)
	}
	fwdReach := bfsURI(fromSet, fwd)
	bwdReach := bfsURI(toSet, rev)
	for _, fe := range cited {
		if !fwdReach[fe.FromURI] || !bwdReach[fe.ToURI] {
			return false
		}
	}
	return true
}

type walkStep struct {
	fromURI string
	edgeID  string
}

func backedURISet(backed []manifest.BackedResource) map[string]bool {
	out := make(map[string]bool, len(backed))
	for _, br := range backed {
		out[br.URI] = true
	}
	return out
}

func sortedMapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func buildFactEdgeAdjacency(edges []extract.FactEdge) map[string][]extract.FactEdge {
	adj := make(map[string][]extract.FactEdge)
	for _, fe := range edges {
		adj[fe.FromURI] = append(adj[fe.FromURI], fe)
	}
	return adj
}

func reconstructWalkIDs(lastEdgeID, startURI string, parent map[string]walkStep) []string {
	path := []string{lastEdgeID}
	for step, ok := parent[startURI]; ok; step, ok = parent[step.fromURI] {
		path = append(path, step.edgeID)
	}
	slices.Reverse(path)
	return path
}

func findShortestWalkIDs(edges []extract.FactEdge, fromBacked, toBacked []manifest.BackedResource) []string {
	fromSet := backedURISet(fromBacked)
	toSet := backedURISet(toBacked)
	adj := buildFactEdgeAdjacency(edges)

	visited := maps.Clone(fromSet)
	parent := make(map[string]walkStep)
	queue := sortedMapKeys(fromSet)

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, fe := range adj[cur] {
			if toSet[fe.ToURI] {
				return reconstructWalkIDs(fe.ID, cur, parent)
			}
			if !visited[fe.ToURI] {
				visited[fe.ToURI] = true
				parent[fe.ToURI] = walkStep{fromURI: cur, edgeID: fe.ID}
				queue = append(queue, fe.ToURI)
			}
		}
	}
	return nil
}

func bfsURI(seeds map[string]bool, adj map[string][]string) map[string]bool {
	visited := make(map[string]bool, len(seeds))
	queue := make([]string, 0, len(seeds))
	for u := range seeds {
		visited[u] = true
		queue = append(queue, u)
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

func cloneManifest(m *manifest.Manifest) *manifest.Manifest {
	return &manifest.Manifest{
		SchemaVersion: m.SchemaVersion,
		Metadata: manifest.Metadata{
			GeneratedAt: m.Metadata.GeneratedAt,
			Scopes:      slices.Clone(m.Metadata.Scopes),
		},
		Components:        cloneComponents(m.Components),
		Containers:        cloneContainers(m.Containers),
		ExcludedResources: slices.Clone(m.ExcludedResources),
		RejectedEdges:     slices.Clone(m.RejectedEdges),
		Views:             cloneViews(m.Views),
	}
}

func cloneComponents(in []manifest.Component) []manifest.Component {
	if len(in) == 0 {
		return nil
	}
	out := make([]manifest.Component, len(in))
	for i, c := range in {
		c.BackedBy = slices.Clone(c.BackedBy)
		c.InlineMeta = slices.Clone(c.InlineMeta)
		out[i] = c
	}
	return out
}

func cloneContainers(in []manifest.Container) []manifest.Container {
	if len(in) == 0 {
		return nil
	}
	out := make([]manifest.Container, len(in))
	for i, ct := range in {
		ct.BackedBy = slices.Clone(ct.BackedBy)
		out[i] = ct
	}
	return out
}

func cloneViews(in []manifest.View) []manifest.View {
	if len(in) == 0 {
		return nil
	}
	out := make([]manifest.View, len(in))
	for i, v := range in {
		out[i] = cloneView(v)
	}
	return out
}

func cloneView(v manifest.View) manifest.View {
	v.ContainerIDs = slices.Clone(v.ContainerIDs)
	if len(v.Placements) > 0 {
		placements := make([]manifest.Placement, len(v.Placements))
		for i, p := range v.Placements {
			p.OffPageRefs = slices.Clone(p.OffPageRefs)
			placements[i] = p
		}
		v.Placements = placements
	}
	if len(v.Edges) > 0 {
		edges := make([]manifest.Edge, len(v.Edges))
		for i, e := range v.Edges {
			e.EvidenceEdgeIDs = slices.Clone(e.EvidenceEdgeIDs)
			edges[i] = e
		}
		v.Edges = edges
	}
	return v
}
