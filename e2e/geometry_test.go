package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/arrase/gcp-arch/internal/manifest"
	"github.com/arrase/gcp-arch/internal/render"
)

const (
	aabbOverlapEpsilon  = 1.0
	aspectRatioDeltaMax = 0.02
	minStabilityRuns    = 3
	minContainerAgree   = 0.90
	minCoGroupJaccard   = 0.85
)

func verifyGeometryCase(caseDir string) error {
	var errs []error
	for _, req := range []string{"architecture.yaml", "architecture.html"} {
		p := filepath.Join(caseDir, req)
		info, err := os.Stat(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("required artifact %q: %w", p, err))
			continue
		}
		if info.Size() == 0 {
			errs = append(errs, fmt.Errorf("required artifact %q is empty", p))
		}
	}

	reportPath := filepath.Join(caseDir, "geometry_report.json")
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		errs = append(errs, fmt.Errorf("read geometry report %q: %w", reportPath, err))
		return errors.Join(errs...)
	}

	var report render.GeometryReport
	if err := json.Unmarshal(raw, &report); err != nil {
		errs = append(errs, fmt.Errorf("decode geometry report %q: %w", reportPath, err))
		return errors.Join(errs...)
	}

	errs = append(errs, verifyGeometryReport(caseDir, report)...)
	errs = append(errs, verifyPNGCoverage(caseDir, report.Views)...)
	return errors.Join(errs...)
}

func verifyGeometryReport(caseDir string, report render.GeometryReport) []error {
	if len(report.Views) == 0 {
		return []error{errors.New("geometry report has zero views")}
	}
	var errs []error
	if report.Conservation != nil {
		errs = append(errs, verifyConservation(*report.Conservation)...)
	}
	for i := range report.Views {
		errs = append(errs, verifyViewReport(caseDir, &report.Views[i])...)
	}
	return errs
}

func verifyConservation(c manifest.ConservationReport) []error {
	var errs []error
	if !strings.EqualFold(strings.TrimSpace(c.Status), "PASS") {
		errs = append(errs, fmt.Errorf("conservation status %q is not PASS", c.Status))
	}
	if c.FactsPrimaryCount <= 0 {
		errs = append(errs, fmt.Errorf("facts_primary_count must be > 0, got %d", c.FactsPrimaryCount))
	}
	if c.RepresentedPrimaryCount != c.FactsPrimaryCount {
		errs = append(errs, fmt.Errorf("represented_primary_count %d != facts_primary_count %d", c.RepresentedPrimaryCount, c.FactsPrimaryCount))
	}
	for _, iss := range c.Issues() {
		errs = append(errs, fmt.Errorf("%s (%s): %s", iss.Code, iss.Subject, iss.Message))
	}
	return errs
}

func verifyViewReport(caseDir string, v *render.ViewReport) []error {
	var errs []error
	for _, msg := range render.CheckViewGeometry(v) {
		errs = append(errs, fmt.Errorf("view %q: %s", v.ID, msg))
	}

	pngPath, err := resolveViewPNGPath(caseDir, v.PNGPath)
	if err != nil {
		return append(errs, fmt.Errorf("view %q png_path: %w", v.ID, err))
	}

	w, h, issues := render.InspectPNGFile(pngPath, v)
	if len(issues) > 0 {
		for _, msg := range issues {
			errs = append(errs, fmt.Errorf("view %q png check: %s", v.ID, msg))
		}
	}
	if w > 0 && h > 0 {
		if v.WidthPx != w || v.HeightPx != h {
			errs = append(errs, fmt.Errorf("view %q reported %dx%d != png %dx%d", v.ID, v.WidthPx, v.HeightPx, w, h))
		}
		ar := float64(w) / float64(h)
		if math.Abs(v.AspectRatio-ar) > aspectRatioDeltaMax {
			errs = append(errs, fmt.Errorf("view %q reported aspect_ratio %.3f != png %.3f", v.ID, v.AspectRatio, ar))
		}
	}

	errs = append(errs, checkBoxBounds(v.ID, "node", v.NodeBoxes, v.WidthPx, v.HeightPx)...)
	errs = append(errs, checkBoxBounds(v.ID, "container", v.ContainerBoxes, v.WidthPx, v.HeightPx)...)
	errs = append(errs, checkBoxBounds(v.ID, "label", v.LabelBoxes, v.WidthPx, v.HeightPx)...)
	errs = append(errs, checkBoxBounds(v.ID, "badge", v.BadgeBoxes, v.WidthPx, v.HeightPx)...)
	errs = append(errs, checkSelfOverlaps(v.ID, "node", v.NodeBoxes)...)
	errs = append(errs, checkSelfOverlaps(v.ID, "label", v.LabelBoxes)...)
	errs = append(errs, checkSelfOverlaps(v.ID, "badge", v.BadgeBoxes)...)
	errs = append(errs, checkContainerOverlaps(v.ID, v.ContainerBoxes)...)
	errs = append(errs, checkNodeContainerStraddles(v.ID, v.ContainerBoxes, v.NodeBoxes)...)
	errs = append(errs, checkCrossOverlaps(v.ID, "label", v.LabelBoxes, "node", v.NodeBoxes)...)
	errs = append(errs, checkCrossOverlaps(v.ID, "badge", v.BadgeBoxes, "node", v.NodeBoxes)...)
	errs = append(errs, checkCrossOverlaps(v.ID, "label", v.LabelBoxes, "badge", v.BadgeBoxes)...)
	return errs
}

func resolveViewPNGPath(caseDir, pngPath string) (string, error) {
	if pngPath == "" {
		return "", errors.New("empty png_path")
	}
	if filepath.IsAbs(pngPath) {
		if _, err := os.Stat(pngPath); err != nil {
			return "", fmt.Errorf("stat %q: %w", pngPath, err)
		}
		return filepath.Clean(pngPath), nil
	}
	cleaned := filepath.Clean(pngPath)
	direct := filepath.Join(caseDir, cleaned)
	if _, err := os.Stat(direct); err == nil {
		return filepath.Clean(direct), nil
	}
	if filepath.Dir(cleaned) == "." {
		viewsFallback := filepath.Join(caseDir, "views", cleaned)
		if _, err := os.Stat(viewsFallback); err == nil {
			return filepath.Clean(viewsFallback), nil
		}
	}
	return "", fmt.Errorf("png file %q not found under %q", pngPath, caseDir)
}

func verifyPNGCoverage(caseDir string, views []render.ViewReport) []error {
	pngs, err := filepath.Glob(filepath.Join(caseDir, "views", "*.png"))
	if err != nil {
		return []error{fmt.Errorf("glob views pngs: %w", err)}
	}
	if len(pngs) == 0 {
		pngs, err = filepath.Glob(filepath.Join(caseDir, "*.png"))
		if err != nil {
			return []error{fmt.Errorf("glob case pngs: %w", err)}
		}
	}

	var errs []error
	if len(pngs) != len(views) {
		errs = append(errs, fmt.Errorf("rendered png count %d != geometry_report views count %d", len(pngs), len(views)))
	}
	reported := make(map[string]bool, len(views))
	for _, v := range views {
		if p, err := resolveViewPNGPath(caseDir, v.PNGPath); err == nil {
			reported[p] = true
		}
	}
	for _, p := range pngs {
		cp := filepath.Clean(p)
		if !reported[cp] {
			errs = append(errs, fmt.Errorf("rendered png %q is missing from geometry_report.json", cp))
		}
	}
	return errs
}

func checkBoxBounds(viewID, kind string, boxes []render.Box, w, h int) []error {
	var errs []error
	fw, fh := float64(w), float64(h)
	for _, b := range boxes {
		if b.Width <= 0 || b.Height <= 0 {
			errs = append(errs, fmt.Errorf("view %q %s box %q has non-positive size %.1fx%.1f", viewID, kind, b.ID, b.Width, b.Height))
			continue
		}
		if b.X < 0 || b.Y < 0 || b.X+b.Width > fw || b.Y+b.Height > fh {
			errs = append(errs, fmt.Errorf("view %q %s box %q (%.1f,%.1f %.1fx%.1f) exceeds %dx%d", viewID, kind, b.ID, b.X, b.Y, b.Width, b.Height, w, h))
		}
	}
	return errs
}

func checkSelfOverlaps(viewID, kind string, boxes []render.Box) []error {
	var errs []error
	for i := range boxes {
		for j := i + 1; j < len(boxes); j++ {
			if boxesOverlap(boxes[i], boxes[j], aabbOverlapEpsilon) {
				errs = append(errs, fmt.Errorf("view %q %s overlap between %q and %q", viewID, kind, boxes[i].ID, boxes[j].ID))
			}
		}
	}
	return errs
}

func checkCrossOverlaps(viewID, kindA string, a []render.Box, kindB string, b []render.Box) []error {
	var errs []error
	for _, ba := range a {
		for _, bb := range b {
			if boxesOverlap(ba, bb, aabbOverlapEpsilon) {
				errs = append(errs, fmt.Errorf("view %q %s %q overlaps %s %q", viewID, kindA, ba.ID, kindB, bb.ID))
			}
		}
	}
	return errs
}

func checkContainerOverlaps(viewID string, boxes []render.Box) []error {
	var errs []error
	for i := range boxes {
		for j := i + 1; j < len(boxes); j++ {
			a, b := boxes[i], boxes[j]
			if !boxesOverlap(a, b, aabbOverlapEpsilon) {
				continue
			}
			if boxContains(a, b, aabbOverlapEpsilon) && boxContains(b, a, aabbOverlapEpsilon) {
				errs = append(errs, fmt.Errorf("view %q coincident container overlap between %q and %q", viewID, a.ID, b.ID))
				continue
			}
			if boxContains(a, b, aabbOverlapEpsilon) || boxContains(b, a, aabbOverlapEpsilon) {
				continue
			}
			errs = append(errs, fmt.Errorf("view %q container partial overlap between %q and %q", viewID, a.ID, b.ID))
		}
	}
	return errs
}

func checkNodeContainerStraddles(viewID string, containers, nodes []render.Box) []error {
	var errs []error
	for _, c := range containers {
		for _, n := range nodes {
			if boxesOverlap(c, n, aabbOverlapEpsilon) && !boxContains(c, n, aabbOverlapEpsilon) {
				errs = append(errs, fmt.Errorf("view %q node %q straddles container %q boundary", viewID, n.ID, c.ID))
			}
		}
	}
	return errs
}

func boxContains(outer, inner render.Box, eps float64) bool {
	return inner.X >= outer.X-eps &&
		inner.Y >= outer.Y-eps &&
		inner.X+inner.Width <= outer.X+outer.Width+eps &&
		inner.Y+inner.Height <= outer.Y+outer.Height+eps
}

func boxesOverlap(a, b render.Box, eps float64) bool {
	dx := math.Min(a.X+a.Width, b.X+b.Width) - math.Max(a.X, b.X)
	dy := math.Min(a.Y+a.Height, b.Y+b.Height) - math.Max(a.Y, b.Y)
	return dx > eps && dy > eps
}

// checkStability verifies repeated synthesis runs (>= 3) preserve container hierarchy,
// detail view count, primary resource placement, and co-grouping structure.
func checkStability(archPaths []string) error {
	if len(archPaths) < minStabilityRuns {
		return fmt.Errorf("need at least %d architecture manifests for stability check, got %d", minStabilityRuns, len(archPaths))
	}

	sigs := make([]stabilitySignature, 0, len(archPaths))
	for _, p := range archPaths {
		sig, err := buildStabilitySignature(p)
		if err != nil {
			return err
		}
		sigs = append(sigs, sig)
	}

	var errs []error
	base := sigs[0]
	for i := 1; i < len(sigs); i++ {
		cur := sigs[i]
		if !mapsEqual(base.hierPaths, cur.hierPaths) {
			errs = append(errs, fmt.Errorf("run %d container hierarchy paths %v differ from run 0 %v", i, sortedKeys(cur.hierPaths), sortedKeys(base.hierPaths)))
		}
		if cur.detailViewCount != base.detailViewCount {
			errs = append(errs, fmt.Errorf("run %d detail view count %d differs from run 0 %d", i, cur.detailViewCount, base.detailViewCount))
		}
		if agree, shared := placementAgreement(base.uriPlacements, cur.uriPlacements); shared > 0 && agree < minContainerAgree {
			errs = append(errs, fmt.Errorf("run %d primary URI container-kind agreement %.3f < %.2f (%d shared URIs)", i, agree, minContainerAgree, shared))
		}
		if jaccard, union := pairJaccard(base.coGroupPairs, cur.coGroupPairs); union > 0 && jaccard < minCoGroupJaccard {
			errs = append(errs, fmt.Errorf("run %d co-grouping Jaccard %.3f < %.2f", i, jaccard, minCoGroupJaccard))
		}
		if jaccard, union := pairJaccard(base.containerCoGroupPairs, cur.containerCoGroupPairs); union > 0 && jaccard < minCoGroupJaccard {
			errs = append(errs, fmt.Errorf("run %d container co-grouping Jaccard %.3f < %.2f", i, jaccard, minCoGroupJaccard))
		}
	}
	return errors.Join(errs...)
}

type stabilitySignature struct {
	hierPaths             map[string]bool
	detailViewCount       int
	uriPlacements         map[string]string
	coGroupPairs          map[string]bool
	containerCoGroupPairs map[string]bool
}

func buildStabilitySignature(path string) (stabilitySignature, error) {
	m, err := manifest.Load(path)
	if err != nil {
		return stabilitySignature{}, err
	}

	cByID := make(map[string]manifest.Container, len(m.Containers))
	for _, c := range m.Containers {
		cByID[c.ID] = c
	}

	hierPaths := make(map[string]bool, len(m.Containers))
	for _, c := range m.Containers {
		hierPaths[resolveContainerPath(cByID, c.ID)] = true
	}

	compByID := make(map[string]manifest.Component, len(m.Components))
	coGroups := make(map[string]bool)
	for _, comp := range m.Components {
		compByID[comp.ID] = comp
		addPairs(coGroups, extractBackedURIs(comp.BackedBy))
	}

	detailViews := 0
	uriPlaceLists := make(map[string][]string)
	containerCoGroups := make(map[string]bool)
	for _, v := range m.Views {
		if v.ID == manifest.SystemIndexViewID {
			continue
		}
		detailViews++
		collectViewStability(v, cByID, compByID, uriPlaceLists, coGroups, containerCoGroups)
	}

	uriPlacements := make(map[string]string, len(uriPlaceLists))
	for uri, list := range uriPlaceLists {
		slices.Sort(list)
		uriPlacements[uri] = strings.Join(list, "|")
	}

	return stabilitySignature{
		hierPaths:             hierPaths,
		detailViewCount:       detailViews,
		uriPlacements:         uriPlacements,
		coGroupPairs:          coGroups,
		containerCoGroupPairs: containerCoGroups,
	}, nil
}

func resolveContainerPath(cByID map[string]manifest.Container, id string) string {
	if id == "" {
		return "ROOT"
	}
	var parts []string
	seen := make(map[string]bool)
	cur := id
	for cur != "" && !seen[cur] {
		seen[cur] = true
		c, ok := cByID[cur]
		if !ok {
			break
		}
		parts = append(parts, string(c.Kind))
		cur = c.ParentContainerID
	}
	slices.Reverse(parts)
	if len(parts) == 0 {
		return "UNKNOWN"
	}
	return strings.Join(parts, ">")
}

func collectViewStability(
	v manifest.View,
	cByID map[string]manifest.Container,
	compByID map[string]manifest.Component,
	uriPlaceLists map[string][]string,
	coGroups map[string]bool,
	containerCoGroups map[string]bool,
) {
	var viewURIs []string
	byContainer := make(map[string][]string)
	for _, pl := range v.Placements {
		comp, ok := compByID[pl.ComponentID]
		if !ok {
			continue
		}
		tag := string(comp.ComponentKind) + "@" + resolveContainerPath(cByID, pl.ContainerID)
		for _, b := range comp.BackedBy {
			if b.URI == "" {
				continue
			}
			uriPlaceLists[b.URI] = append(uriPlaceLists[b.URI], tag)
			viewURIs = append(viewURIs, b.URI)
			if pl.ContainerID != "" {
				byContainer[pl.ContainerID] = append(byContainer[pl.ContainerID], b.URI)
			}
		}
	}
	addPairs(coGroups, viewURIs)
	for _, cURIs := range byContainer {
		addPairs(containerCoGroups, cURIs)
	}
}

func extractBackedURIs(backed []manifest.BackedResource) []string {
	uris := make([]string, 0, len(backed))
	for _, b := range backed {
		if b.URI != "" {
			uris = append(uris, b.URI)
		}
	}
	return uris
}

func addPairs(dst map[string]bool, uris []string) {
	sorted := slices.Clone(uris)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	for i := range sorted {
		for j := i + 1; j < len(sorted); j++ {
			dst[sorted[i]+"\x00"+sorted[j]] = true
		}
	}
}

func mapsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func placementAgreement(a, b map[string]string) (float64, int) {
	shared, matches := 0, 0
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			continue
		}
		shared++
		if va == vb {
			matches++
		}
	}
	if shared == 0 {
		return 1, 0
	}
	return float64(matches) / float64(shared), shared
}

func pairJaccard(a, b map[string]bool) (float64, int) {
	inter, union := 0, len(b)
	for k := range a {
		if b[k] {
			inter++
		} else {
			union++
		}
	}
	if union == 0 {
		return 1, 0
	}
	return float64(inter) / float64(union), union
}

func TestExamplesPNGStreamingRegression(t *testing.T) {
	root := repoRoot(t)
	slugs := []string{
		"platform-cicd",
		"retail-cloud-native",
		"fintech-global-payments",
		"enterprise-lakehouse-mlops",
		"agentic-ai-document-platform",
		"enterprise-zero-trust-mesh",
	}

	for _, slug := range slugs {
		t.Run(slug, func(t *testing.T) {
			dir := filepath.Join(root, "examples", slug)
			pngs, err := filepath.Glob(filepath.Join(dir, "views", "*.png"))
			if err != nil {
				t.Fatal(err)
			}
			if len(pngs) == 0 {
				t.Fatalf("no rendered view PNGs in %s", dir)
			}
			for _, p := range pngs {
				w, h, issues := render.InspectPNGFile(p, nil)
				if len(issues) > 0 {
					t.Errorf("%s (%dx%d) failed streaming PNG inspection: %v", filepath.Base(p), w, h, issues)
				}
			}
		})
	}
}

func TestGeometryVerificationMatrix(t *testing.T) {
	t.Run("valid_case_passes", func(t *testing.T) {
		dir := writeValidGeometryCase(t)
		if err := verifyGeometryCase(dir); err != nil {
			t.Fatalf("valid case failed: %v", err)
		}
	})

	t.Run("touching_boxes_within_epsilon_pass", func(t *testing.T) {
		dir := writeValidGeometryCase(t)
		mutateReport(t, dir, func(r *render.GeometryReport) {
			r.Views[0].NodeBoxes = []render.Box{
				{ID: "n1", X: 40, Y: 40, Width: 90, Height: 80},
				{ID: "n2", X: 130, Y: 40, Width: 90, Height: 80},
			}
		})
		if err := verifyGeometryCase(dir); err != nil {
			t.Fatalf("touching boxes failed: %v", err)
		}
	})

	t.Run("node_overlap_fails", func(t *testing.T) {
		dir := writeValidGeometryCase(t)
		mutateReport(t, dir, func(r *render.GeometryReport) {
			r.Views[0].NodeBoxes = []render.Box{
				{ID: "n1", X: 40, Y: 40, Width: 90, Height: 80},
				{ID: "n2", X: 100, Y: 40, Width: 90, Height: 80},
			}
		})
		if err := verifyGeometryCase(dir); err == nil || !strings.Contains(err.Error(), "node overlap") {
			t.Fatalf("expected node overlap error, got %v", err)
		}
	})

	t.Run("label_and_badge_overlaps_fail", func(t *testing.T) {
		dir := writeValidGeometryCase(t)
		mutateReport(t, dir, func(r *render.GeometryReport) {
			r.Views[0].LabelBoxes = []render.Box{{ID: "l1", X: 50, Y: 50, Width: 40, Height: 20}}
		})
		if err := verifyGeometryCase(dir); err == nil || !strings.Contains(err.Error(), "overlaps node") {
			t.Fatalf("expected label-node overlap error, got %v", err)
		}
	})

	t.Run("container_partial_and_coincident_overlap_fail_nested_passes", func(t *testing.T) {
		dir := writeValidGeometryCase(t)
		mutateReport(t, dir, func(r *render.GeometryReport) {
			r.Views[0].ContainerBoxes = []render.Box{
				{ID: "outer", X: 20, Y: 20, Width: 500, Height: 400},
				{ID: "inner", X: 30, Y: 30, Width: 300, Height: 250},
			}
		})
		if err := verifyGeometryCase(dir); err != nil {
			t.Fatalf("nested containers should pass, got %v", err)
		}

		mutateReport(t, dir, func(r *render.GeometryReport) {
			r.Views[0].ContainerBoxes = []render.Box{
				{ID: "c1", X: 20, Y: 20, Width: 300, Height: 250},
				{ID: "c2", X: 20, Y: 20, Width: 300, Height: 250},
			}
		})
		if err := verifyGeometryCase(dir); err == nil || !strings.Contains(err.Error(), "coincident") {
			t.Fatalf("expected coincident container overlap error, got %v", err)
		}

		mutateReport(t, dir, func(r *render.GeometryReport) {
			r.Views[0].ContainerBoxes = []render.Box{
				{ID: "c1", X: 20, Y: 20, Width: 300, Height: 250},
				{ID: "c2", X: 200, Y: 20, Width: 300, Height: 250},
			}
		})
		if err := verifyGeometryCase(dir); err == nil || !strings.Contains(err.Error(), "partial overlap") {
			t.Fatalf("expected partial container overlap error, got %v", err)
		}
	})

	t.Run("node_straddling_container_fails", func(t *testing.T) {
		dir := writeValidGeometryCase(t)
		mutateReport(t, dir, func(r *render.GeometryReport) {
			r.Views[0].ContainerBoxes = []render.Box{{ID: "c1", X: 20, Y: 20, Width: 80, Height: 200}}
			r.Views[0].NodeBoxes = []render.Box{{ID: "n1", X: 40, Y: 40, Width: 90, Height: 80}}
		})
		if err := verifyGeometryCase(dir); err == nil || !strings.Contains(err.Error(), "straddles") {
			t.Fatalf("expected node straddle error, got %v", err)
		}
	})

	t.Run("partial_tile_box_blanking_fails", func(t *testing.T) {
		dir := writeValidGeometryCase(t)
		mutateReport(t, dir, func(r *render.GeometryReport) {
			r.Views[0].NodeBoxes = append(r.Views[0].NodeBoxes, render.Box{
				ID: "blank_node", X: 700, Y: 400, Width: 200, Height: 120,
			})
		})
		if err := verifyGeometryCase(dir); err == nil || !strings.Contains(err.Error(), "png_box_blank") {
			t.Fatalf("expected png_box_blank error, got %v", err)
		}
	})

	t.Run("conservation_failure_rejected", func(t *testing.T) {
		dir := writeValidGeometryCase(t)
		mutateReport(t, dir, func(r *render.GeometryReport) {
			r.Conservation.Status = "FAIL"
			r.Conservation.MissingPrimaryURIs = []string{"//storage.googleapis.com/missing"}
		})
		if err := verifyGeometryCase(dir); err == nil || !strings.Contains(err.Error(), "CONSERVATION_MISSING_PRIMARY") {
			t.Fatalf("expected conservation error, got %v", err)
		}
	})
}

func TestCheckStability(t *testing.T) {
	dir := t.TempDir()
	baseYAML := `schema_version: v1
metadata:
  generated_at: "2026-11-27T00:00:00Z"
  scopes: [projects/p1]
containers:
  - id: proj_p1
    title: p1
    kind: PROJECT
components:
  - id: api
    title: API
    icon_key: cloud_run
    component_kind: WORKLOAD
    project_id: p1
    backed_by:
      - uri: //run.googleapis.com/projects/p1/locations/us-central1/services/api
        asset_type: run.googleapis.com/Service
        project_id: p1
views:
  - id: view_0_system_index
    title: System Index
    archetype: layered_flow
    accent_mode: RUNTIME
    container_ids: [proj_p1]
    placements:
      - component_id: api
        container_id: proj_p1
        column: 0
  - id: view_1
    title: Detail
    archetype: layered_flow
    accent_mode: RUNTIME
    container_ids: [proj_p1]
    placements:
      - component_id: api
        container_id: proj_p1
        column: 0
`
	paths := make([]string, 3)
	for i := range 3 {
		p := filepath.Join(dir, fmt.Sprintf("run_%d.yaml", i))
		writeFileStr(t, p, baseYAML)
		paths[i] = p
	}
	if err := checkStability(paths); err != nil {
		t.Fatalf("identical stability runs failed: %v", err)
	}

	drifted := strings.Replace(baseYAML, "kind: PROJECT", "kind: VPC", 1)
	writeFileStr(t, paths[2], drifted)
	if err := checkStability(paths); err == nil || !strings.Contains(err.Error(), "hierarchy paths") {
		t.Fatalf("expected hierarchy drift error, got %v", err)
	}
}

func writeValidGeometryCase(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	viewsDir := filepath.Join(dir, "views")
	if err := os.MkdirAll(viewsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileStr(t, filepath.Join(dir, "architecture.yaml"), "schema_version: v1\n")
	writeFileStr(t, filepath.Join(dir, "architecture.html"), "<!doctype html><html></html>\n")

	pngPath := filepath.Join(viewsDir, "00-view_0_system_index.png")
	img := image.NewNRGBA(image.Rect(0, 0, 1200, 800))
	fillRect(img, 0, 0, 1200, 800, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	drawPaletteBoxes(img, 1200, 800)
	// Ensure node box at (40,40,90,80) and (160,40,90,80) have >= 3 distinct colors for per-box checks.
	fillRect(img, 45, 45, 80, 70, color.NRGBA{R: 20, G: 20, B: 20, A: 255})
	fillRect(img, 85, 55, 115, 95, color.NRGBA{R: 234, G: 67, B: 53, A: 255})
	fillRect(img, 165, 45, 200, 70, color.NRGBA{R: 20, G: 20, B: 20, A: 255})
	fillRect(img, 205, 55, 235, 95, color.NRGBA{R: 52, G: 168, B: 83, A: 255})
	savePNG(t, pngPath, img)

	rep := render.GeometryReport{
		Views: []render.ViewReport{{
			ID:             manifest.SystemIndexViewID,
			Title:          "System Index",
			PNGPath:        "views/00-view_0_system_index.png",
			WidthPx:        1200,
			HeightPx:       800,
			AspectRatio:    1.5,
			NodeBoxes:      []render.Box{{ID: "n1", X: 40, Y: 40, Width: 90, Height: 80}},
			ContainerBoxes: []render.Box{{ID: "c1", X: 20, Y: 20, Width: 520, Height: 300}},
			LabelBoxes:     []render.Box{},
			BadgeBoxes:     []render.Box{},
			Overlaps:       []string{},
			ClippedTexts:   []string{},
			OverflowTexts:  []string{},
			UnplacedLabels: []string{},
			Status:         "PASS",
		}},
		Conservation: &manifest.ConservationReport{
			Status:                   "PASS",
			FactsPrimaryCount:        2,
			RepresentedPrimaryCount:  2,
			FactsFunctionalEdgeCount: 1,
			View0Coverage:            1.0,
		},
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFileStr(t, filepath.Join(dir, "geometry_report.json"), string(b))
	return dir
}

func mutateReport(t *testing.T, dir string, fn func(*render.GeometryReport)) {
	t.Helper()
	p := filepath.Join(dir, "geometry_report.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var r render.GeometryReport
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	fn(&r)
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFileStr(t, p, string(b))
}

func writeFileStr(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func drawPaletteBoxes(img *image.NRGBA, w, h int) {
	palette := []color.NRGBA{
		{R: 26, G: 115, B: 232, A: 255},
		{R: 52, G: 168, B: 83, A: 255},
		{R: 251, G: 188, B: 4, A: 255},
		{R: 234, G: 67, B: 53, A: 255},
		{R: 147, G: 52, B: 230, A: 255},
		{R: 0, G: 172, B: 193, A: 255},
		{R: 60, G: 64, B: 67, A: 255},
		{R: 232, G: 240, B: 254, A: 255},
	}
	for i, c := range palette {
		x0 := 40 + (i%4)*120
		y0 := 40 + (i/4)*110
		if x0+90 < w-20 && y0+80 < h-20 {
			fillRect(img, x0, y0, x0+90, y0+80, c)
		}
	}
}

func fillRect(img *image.NRGBA, x0, y0, x1, y1 int, c color.NRGBA) {
	for y := max(0, y0); y < min(img.Bounds().Max.Y, y1); y++ {
		for x := max(0, x0); x < min(img.Bounds().Max.X, x1); x++ {
			img.SetNRGBA(x, y, c)
		}
	}
}

func savePNG(t *testing.T, path string, img image.Image) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, ".."))
}
