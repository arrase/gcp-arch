package render

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/arrase/gcp-arch/internal/manifest"
)

// Minimum PNG dimensions and aspect-ratio bounds enforced for rendered views.
const (
	PNGMinWidth       = 600
	PNGMinHeight      = 400
	PNGMinAspectRatio = 0.35
	PNGMaxAspectRatio = 2.60
)

// Collision records a strict layout collision detected by router.js after corridor reflow passes.
type Collision struct {
	ViewID   string   `json:"view_id"`
	Kind     string   `json:"kind"`
	Subjects []string `json:"subjects"`
	Detail   string   `json:"detail"`
}

// LayoutCollisionError is returned when router.js detects unresolved card, badge, pill, wire,
// or text clipping/overflow issues (mapped to CLI exit code 2).
type LayoutCollisionError struct {
	Collisions []Collision
}

func (e *LayoutCollisionError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "layout collision verification failed (%d collision(s)):", len(e.Collisions))
	for _, c := range e.Collisions {
		fmt.Fprintf(&b, "\n  - [%s] view=%s subjects=%v: %s", c.Kind, c.ViewID, c.Subjects, c.Detail)
	}
	b.WriteString("\nfix: in architecture.yaml, move one of the listed components to another column (placements[].column) " +
		"or into the right container (placements[].container_id), or split the view into smaller views, then re-run gcp-arch render")
	return b.String()
}

// GeometryReportError is returned when one or more rendered views fail aspect-ratio,
// minimum-dimension, pixel quality, unplaced-label, or conservation checks.
type GeometryReportError struct {
	Path   string
	Report *GeometryReport
	Detail string
}

func (e *GeometryReportError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("geometry verification failed (%s): %s", e.Path, e.Detail)
	}
	return fmt.Sprintf("geometry verification failed: %s", e.Detail)
}

// Box represents an axis-aligned bounding box in geometry_report.json (PNG pixel coordinates).
type Box struct {
	ID     string  `json:"id"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// ViewReport captures rendered PNG dimensions, bounding boxes, and geometry checks for one view.
type ViewReport struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	PNGPath        string   `json:"png_path"`
	WidthPx        int      `json:"width_px"`
	HeightPx       int      `json:"height_px"`
	AspectRatio    float64  `json:"aspect_ratio"`
	NodeBoxes      []Box    `json:"node_boxes"`
	ContainerBoxes []Box    `json:"container_boxes"`
	LabelBoxes     []Box    `json:"label_boxes"`
	BadgeBoxes     []Box    `json:"badge_boxes"`
	Overlaps       []string `json:"overlaps"`
	ClippedTexts   []string `json:"clipped_texts"`
	OverflowTexts  []string `json:"overflow_texts"`
	UnplacedLabels []string `json:"unplaced_labels"`
	Status         string   `json:"status"`
}

// GeometryReport is the machine-readable geometry verification report for rendered views.
type GeometryReport struct {
	Views        []ViewReport                 `json:"views"`
	Conservation *manifest.ConservationReport `json:"conservation,omitempty"`
}

// CheckViewGeometry validates a single ViewReport's status, layout diagnostics, minimum PNG
// dimensions, and aspect-ratio bounds, returning any failure descriptions.
func CheckViewGeometry(vr *ViewReport) []string {
	if vr == nil {
		return []string{"nil view report"}
	}
	var issues []string
	st := strings.TrimSpace(vr.Status)
	if !strings.EqualFold(st, "PASS") && !strings.EqualFold(st, "OK") {
		issues = append(issues, fmt.Sprintf("status=%s", vr.Status))
	}
	if len(vr.Overlaps) > 0 {
		issues = append(issues, fmt.Sprintf("overlaps=%d", len(vr.Overlaps)))
	}
	if len(vr.ClippedTexts) > 0 {
		issues = append(issues, fmt.Sprintf("clipped_texts=%d", len(vr.ClippedTexts)))
	}
	if len(vr.OverflowTexts) > 0 {
		issues = append(issues, fmt.Sprintf("overflow_texts=%d", len(vr.OverflowTexts)))
	}
	if len(vr.UnplacedLabels) > 0 {
		issues = append(issues, fmt.Sprintf("unplaced_labels=%d", len(vr.UnplacedLabels)))
	}
	if vr.WidthPx > 0 || vr.HeightPx > 0 {
		ar := vr.AspectRatio
		if ar == 0 && vr.HeightPx > 0 {
			ar = math.Round((float64(vr.WidthPx)/float64(vr.HeightPx))*1000) / 1000
		}
		if vr.WidthPx < PNGMinWidth || vr.HeightPx < PNGMinHeight {
			issues = append(issues, fmt.Sprintf("png_dimensions %dx%d below %dx%d", vr.WidthPx, vr.HeightPx, PNGMinWidth, PNGMinHeight))
		}
		if ar < PNGMinAspectRatio || ar > PNGMaxAspectRatio {
			issues = append(issues, fmt.Sprintf("aspect_ratio %.3f outside [%.2f, %.2f]", ar, PNGMinAspectRatio, PNGMaxAspectRatio))
		}
	}
	return issues
}

// AllPass reports whether every view in the geometry report passed and conservation (if present) passed.
func (r *GeometryReport) AllPass() bool {
	if r == nil || len(r.Views) == 0 {
		return false
	}
	for i := range r.Views {
		if len(CheckViewGeometry(&r.Views[i])) > 0 {
			return false
		}
	}
	if r.Conservation != nil && !strings.EqualFold(strings.TrimSpace(r.Conservation.Status), "PASS") {
		return false
	}
	return true
}

func summarizeGeometryFailures(rep *GeometryReport) string {
	var parts []string
	for i := range rep.Views {
		v := &rep.Views[i]
		if len(CheckViewGeometry(v)) > 0 {
			parts = append(parts, fmt.Sprintf("view %s (%dx%d ar=%.3f status=%s overlaps=%v unplaced=%v)",
				v.ID, v.WidthPx, v.HeightPx, v.AspectRatio, v.Status, v.Overlaps, v.UnplacedLabels))
		}
	}
	if rep.Conservation != nil && !strings.EqualFold(strings.TrimSpace(rep.Conservation.Status), "PASS") {
		parts = append(parts, fmt.Sprintf("conservation=%s", rep.Conservation.Status))
	}
	if len(parts) == 0 {
		return "unknown geometry failure"
	}
	return strings.Join(parts, "; ")
}

func recordViewFailure(vr *ViewReport, msg string) {
	if !slices.Contains(vr.Overlaps, msg) {
		vr.Overlaps = append(vr.Overlaps, msg)
	}
	vr.Status = "FAIL"
}

func applyViewDimensionChecks(vr *ViewReport) {
	if vr.WidthPx == 0 && vr.HeightPx == 0 {
		return
	}
	if vr.AspectRatio == 0 && vr.HeightPx > 0 {
		vr.AspectRatio = math.Round((float64(vr.WidthPx)/float64(vr.HeightPx))*1000) / 1000
	}
	if vr.WidthPx < PNGMinWidth || vr.HeightPx < PNGMinHeight {
		recordViewFailure(vr, fmt.Sprintf("png_dimensions %dx%d below %dx%d", vr.WidthPx, vr.HeightPx, PNGMinWidth, PNGMinHeight))
	}
	if vr.AspectRatio < PNGMinAspectRatio || vr.AspectRatio > PNGMaxAspectRatio {
		recordViewFailure(vr, fmt.Sprintf("aspect_ratio %.3f outside [%.2f, %.2f]", vr.AspectRatio, PNGMinAspectRatio, PNGMaxAspectRatio))
	}
	if len(vr.UnplacedLabels) > 0 {
		vr.Status = "FAIL"
	}
}

func normalizeViewReport(vr ViewReport) ViewReport {
	if vr.NodeBoxes == nil {
		vr.NodeBoxes = []Box{}
	}
	if vr.ContainerBoxes == nil {
		vr.ContainerBoxes = []Box{}
	}
	if vr.LabelBoxes == nil {
		vr.LabelBoxes = []Box{}
	}
	if vr.BadgeBoxes == nil {
		vr.BadgeBoxes = []Box{}
	}
	if vr.Overlaps == nil {
		vr.Overlaps = []string{}
	}
	if vr.ClippedTexts == nil {
		vr.ClippedTexts = []string{}
	}
	if vr.OverflowTexts == nil {
		vr.OverflowTexts = []string{}
	}
	if vr.UnplacedLabels == nil {
		vr.UnplacedLabels = []string{}
	}
	if vr.Status == "" {
		vr.Status = "PASS"
	}
	return vr
}

func normalizeConservationCopy(c *manifest.ConservationReport) *manifest.ConservationReport {
	if c == nil {
		return nil
	}
	cp := *c
	if cp.MissingPrimaryURIs == nil {
		cp.MissingPrimaryURIs = []string{}
	}
	if cp.HallucinatedPrimaryURIs == nil {
		cp.HallucinatedPrimaryURIs = []string{}
	}
	if cp.MissingEdgeIDs == nil {
		cp.MissingEdgeIDs = []string{}
	}
	if cp.UnrepresentedTransversalURIs == nil {
		cp.UnrepresentedTransversalURIs = []string{}
	}
	if cp.MissingTransversalEdgeIDs == nil {
		cp.MissingTransversalEdgeIDs = []string{}
	}
	if cp.View0MissingNodeIDs == nil {
		cp.View0MissingNodeIDs = []string{}
	}
	return &cp
}

func updateViewReportGeometryFromPNG(vr *ViewReport, rect ViewRect, w, h int, pngPath string) {
	vr.PNGPath = pngPath
	vr.WidthPx = w
	vr.HeightPx = h
	if h > 0 {
		vr.AspectRatio = math.Round((float64(w)/float64(h))*1000) / 1000
	}
	if rect.Width > 0 && rect.Height > 0 {
		sx := float64(w) / rect.Width
		sy := float64(h) / rect.Height
		vr.NodeBoxes = scaleBoxesToPNG(vr.NodeBoxes, sx, sy, w, h)
		vr.ContainerBoxes = scaleBoxesToPNG(vr.ContainerBoxes, sx, sy, w, h)
		vr.LabelBoxes = scaleBoxesToPNG(vr.LabelBoxes, sx, sy, w, h)
		vr.BadgeBoxes = scaleBoxesToPNG(vr.BadgeBoxes, sx, sy, w, h)
	}
}

func scaleBoxesToPNG(boxes []Box, sx, sy float64, maxW, maxH int) []Box {
	if len(boxes) == 0 {
		return []Box{}
	}
	fw, fh := float64(maxW), float64(maxH)
	out := make([]Box, len(boxes))
	for i, b := range boxes {
		x := math.Max(0, math.Round(b.X*sx*100)/100)
		y := math.Max(0, math.Round(b.Y*sy*100)/100)
		if x >= fw {
			x = math.Max(0, fw-1)
		}
		if y >= fh {
			y = math.Max(0, fh-1)
		}
		w := math.Max(1, math.Round(b.Width*sx*100)/100)
		h := math.Max(1, math.Round(b.Height*sy*100)/100)
		if x+w > fw {
			w = math.Max(1, fw-x)
		}
		if y+h > fh {
			h = math.Max(1, fh-y)
		}
		out[i] = Box{
			ID:     b.ID,
			X:      x,
			Y:      y,
			Width:  w,
			Height: h,
		}
	}
	return out
}
