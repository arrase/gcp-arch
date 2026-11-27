package render

import (
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"strings"
	"sync"

	"github.com/arrase/gcp-arch/internal/icons"
	"github.com/arrase/gcp-arch/internal/manifest"
)

//go:embed assets/diagram.css assets/router.js assets/fonts/*.woff2
var embeddedAssets embed.FS

// ContentSecurityPolicy blocks all external network fetches while permitting inline styles,
// embedded base64 fonts/data URIs, and the in-page orthogonal router script.
const ContentSecurityPolicy = "default-src 'none'; style-src 'unsafe-inline'; font-src data:; img-src data:; script-src 'unsafe-inline'"

type fontAsset struct {
	Family string
	Weight int
	Path   string
}

var bundledFonts = []fontAsset{
	{Family: "Inter", Weight: 400, Path: "assets/fonts/Inter-Regular.woff2"},
	{Family: "Inter", Weight: 500, Path: "assets/fonts/Inter-Medium.woff2"},
	{Family: "Inter", Weight: 600, Path: "assets/fonts/Inter-SemiBold.woff2"},
	{Family: "Inter", Weight: 700, Path: "assets/fonts/Inter-Bold.woff2"},
	{Family: "JetBrains Mono", Weight: 400, Path: "assets/fonts/JetBrainsMono-Regular.woff2"},
	{Family: "JetBrains Mono", Weight: 500, Path: "assets/fonts/JetBrainsMono-Medium.woff2"},
}

var (
	staticAssetsOnce sync.Once
	cachedFontCSS    string
	cachedDiagramCSS string
	cachedRouterJS   string
	staticAssetsErr  error
)

func loadStaticAssets() (fontCSS, diagramCSS, routerJS string, err error) {
	staticAssetsOnce.Do(func() {
		var fb strings.Builder
		for _, f := range bundledFonts {
			raw, readErr := embeddedAssets.ReadFile(f.Path)
			if readErr != nil {
				staticAssetsErr = fmt.Errorf("read embedded font %q: %w", f.Path, readErr)
				return
			}
			b64 := base64.StdEncoding.EncodeToString(raw)
			fmt.Fprintf(&fb, "@font-face{font-family:'%s';font-style:normal;font-weight:%d;font-display:block;src:url(data:font/woff2;base64,%s) format('woff2');}\n", f.Family, f.Weight, b64)
		}
		cssBytes, readErr := embeddedAssets.ReadFile("assets/diagram.css")
		if readErr != nil {
			staticAssetsErr = fmt.Errorf("read embedded diagram.css: %w", readErr)
			return
		}
		jsBytes, readErr := embeddedAssets.ReadFile("assets/router.js")
		if readErr != nil {
			staticAssetsErr = fmt.Errorf("read embedded router.js: %w", readErr)
			return
		}
		cachedFontCSS = fb.String()
		cachedDiagramCSS = string(cssBytes)
		cachedRouterJS = string(jsBytes)
	})
	return cachedFontCSS, cachedDiagramCSS, cachedRouterJS, staticAssetsErr
}

type routingViewJSON struct {
	ID    string        `json:"id"`
	Edges []RoutingEdge `json:"edges"`
}

type htmlTemplateData struct {
	CSPAttr     template.HTMLAttr
	Title       string
	ScopeKicker string
	FontCSS     template.CSS
	DiagramCSS  template.CSS
	IconSprite  template.HTML
	Views       []PreparedView
	RoutingJSON template.JS
	RouterJS    template.JS
}

func formatBreakableText(s string) template.HTML {
	escaped := template.HTMLEscapeString(s)
	escaped = strings.ReplaceAll(escaped, "_", "_<wbr>")
	escaped = strings.ReplaceAll(escaped, "-", "-<wbr>")
	return template.HTML(escaped)
}

var documentTemplate = template.Must(template.New("architecture").Funcs(template.FuncMap{
	"wbr": formatBreakableText,
}).Parse(`{{- define "card" -}}
<article class="arch-card" data-component-id="{{.ComponentID}}" data-role="{{.RoleTag}}"{{if .ExtraWide}} data-wide="2"{{else if .Wide}} data-wide="1"{{end}}>
  <div class="arch-card-header">
    <svg class="arch-card-icon" viewBox="0 0 24 24" aria-hidden="true"><use href="#gcp-icon-{{.IconKey}}"></use></svg>
    <div class="arch-card-titles">
      <div class="arch-card-title">{{wbr .Title}}</div>
      {{- if .Subtitle}}<div class="arch-card-subtitle">{{.Subtitle}}</div>{{end}}
    </div>
  </div>
  {{- if gt (len .Members) 1}}
  <div class="arch-card-members">
    <div class="arch-card-members-hdr">{{len .Members}} resources</div>
    <div class="arch-card-members-list">
      {{- range .Members}}<span class="member-chip">{{.}}</span>{{end}}
    </div>
  </div>
  {{- else if .RawName}}
  <div class="arch-card-raw">{{wbr .RawName}}</div>
  {{- end}}
  {{- if .InlineMeta}}
  <div class="arch-card-meta">
    {{- range .InlineMeta}}
    <div class="arch-card-meta-row">
      <span class="arch-card-meta-label">{{.Label}}</span>
      <span class="arch-card-meta-value">{{.Value}}</span>
    </div>
    {{- end}}
  </div>
  {{- end}}
  {{- if .Badges}}
  <div class="arch-card-badges">
    {{- range .Badges}}<span class="chip {{.Kind}}">{{.Label}}</span>{{end}}
  </div>
  {{- end}}
  {{- if .OffPageRefs}}
  <div class="arch-card-refs">
    {{- range .OffPageRefs}}<span class="off-page-pill">{{if eq .Direction "IN"}}&larr;{{else}}&rarr;{{end}} {{.Label}}</span>{{end}}
  </div>
  {{- end}}
</article>
{{- end -}}

{{- define "lane" -}}
<div class="node-lane">
  {{- range .Cards}}{{template "card" .}}{{end}}
</div>
{{- end -}}

{{- define "box" -}}
<div class="arch-box" data-kind="{{.Kind}}" data-depth="{{.Depth}}"{{if .ExposureLower}} data-exposure="{{.ExposureLower}}"{{end}}{{if .BackedAnchor}} data-anchor-id="{{.ID}}"{{end}} data-container-id="{{.ID}}">
  <div class="arch-box-header" data-container-id="{{.ID}}">
    <span class="box-kind-pill">{{.KindPill}}</span>
    <span class="box-title">{{.Title}}</span>
    {{- if .Subtitle}}<span class="box-subtitle">{{.Subtitle}}</span>{{end}}
    {{- range .Badges}}<span class="box-badge">{{.}}</span>{{end}}
  </div>
  <div class="arch-box-body{{if .StackVertical}} stack-vertical{{end}}">
    {{- range .DirectLanes}}{{template "lane" .}}{{end}}
    {{- if .SplitTwoColumn}}
      {{- range .PrimaryChildren}}{{template "box" .}}{{end}}
      {{- if .SecondaryChildren}}
      <div class="vpc-regions-stack">
        {{- range .SecondaryChildren}}{{template "box" .}}{{end}}
      </div>
      {{- end}}
    {{- else if .SplitTopBottom}}
      {{- range .PrimaryChildren}}{{template "box" .}}{{end}}
      {{- if .SecondaryChildren}}
      <div class="projects-row">
        {{- range .SecondaryChildren}}{{template "box" .}}{{end}}
      </div>
      {{- end}}
    {{- else if .SplitSideStack}}
      {{- range .PrimaryChildren}}{{template "box" .}}{{end}}
      {{- if .SecondaryChildren}}
      <div class="projects-stack">
        {{- range .SecondaryChildren}}{{template "box" .}}{{end}}
      </div>
      {{- end}}
    {{- else}}
      {{- range .Children}}{{template "box" .}}{{end}}
    {{- end}}
  </div>
</div>
{{- end -}}

<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" {{.CSPAttr}}>
<title>{{.Title}}</title>
<style>
{{.FontCSS}}
{{.DiagramCSS}}
</style>
</head>
<body>
{{.IconSprite}}
<main class="document-root">
{{- $kicker := .ScopeKicker -}}
{{- range .Views}}
<section class="view-sheet" data-view-id="{{.ID}}" data-view-index="{{.Index}}"{{if .IsDense}} data-dense="1"{{end}}>
  <header class="view-header">
    <div class="view-title-group">
      {{- if $kicker}}<div class="view-kicker">{{$kicker}}</div>{{end}}
      <h1 class="view-title">{{.Title}}</h1>
      {{- if .FocusSummary}}
      <p class="view-subtitle">{{.FocusSummary}}</p>
      {{- else if .Subtitle}}
      <p class="view-subtitle">{{.Subtitle}}</p>
      {{- end}}
    </div>
    <div class="view-legend">
      <span class="legend-item"><span class="legend-line ingress"></span>Ingress</span>
      <span class="legend-item"><span class="legend-line traffic"></span>Traffic</span>
      <span class="legend-item"><span class="legend-line data"></span>Data / AI</span>
      <span class="legend-item"><span class="legend-line async"></span>Async</span>
      <span class="legend-item"><span class="legend-line retry"></span>Retry</span>
      <span class="legend-item"><span class="legend-line dlq"></span>DLQ</span>
      <span class="legend-item"><span class="legend-line cicd"></span>CI/CD</span>
      <span class="legend-item"><span class="legend-line peering"></span>Peering</span>
      <span class="legend-item"><span class="legend-line inferred"></span>Inferred</span>
    </div>
  </header>
  {{- if .StageBanners}}
  <div class="stage-banners">
    {{- range .StageBanners}}<div class="stage-banner">{{.Label}}</div>{{end}}
  </div>
  {{- end}}
  <div class="view-body">
    <div class="diagram-stage">
      <svg class="connector-layer"></svg>
      {{- range .LeftUncontainedLanes}}{{template "lane" .}}{{end}}
      {{- range .RootContainers}}{{template "box" .}}{{end}}
      {{- range .RightUncontainedLanes}}{{template "lane" .}}{{end}}
    </div>
    {{- if .IncludeTransversalPanel}}
    <aside class="governance-panel">
      <div class="gov-header">
        <h2 class="gov-title">Governance &amp; Transversal Control Plane</h2>
        <p class="gov-subtitle">{{.TransversalCount}} control, identity, security, and observability resource(s) separated from traffic edges.</p>
      </div>
      <div class="gov-sections">
        {{- range .TransversalGroups}}
        <div class="gov-group">
          <div class="gov-group-title">{{.Title}} ({{len .Items}})</div>
          {{- range .Items}}
          <div class="gov-item">
            <div class="gov-item-name">{{.DisplayName}}</div>
            {{- if .Meta}}<div class="gov-item-meta">{{.Meta}}</div>{{end}}
          </div>
          {{- end}}
        </div>
        {{- end}}
      </div>
      {{- if gt .SuppressedTotal 0}}
      <div class="gov-footer">Stage 1 suppressed plumbing: {{.SuppressedTotal}} resource(s) across {{.SuppressedTypes}} asset type(s) ({{.SuppressedSummary}}).</div>
      {{- end}}
    </aside>
    {{- end}}
  </div>
  {{- if or .Footnotes .Excluded}}
  <footer class="view-footnotes">
    {{- range .Footnotes}}
    <div class="view-footnote-item"><span class="view-footnote-tag">[{{.Number}}]</span><strong>{{.EdgeLabel}}</strong>: {{.Rationale}}</div>
    {{- end}}
    {{- if .Excluded}}
    <div class="view-footnote-item"><span class="view-footnote-tag">Excluded ({{len .Excluded}})</span>{{- range $i, $ex := .Excluded}}{{if $i}}; {{end}}<strong>{{$ex.URI}}</strong>: {{$ex.Reason}}{{- end}}</div>
    {{- end}}
  </footer>
  {{- end}}
</section>
{{- end}}
</main>
<script id="gcp-arch-routing-data" type="application/json">{{.RoutingJSON}}</script>
<script>{{.RouterJS}}</script>
</body>
</html>
`))

// BuildHTML compiles a validated Manifest into a self-contained offline HTML dossier with
// nested Flexbox containers, bundled base64 .woff2 fonts, inline SVG <symbol> sprites,
// escaped JSON routing metadata, transversal control-plane panels, and strict CSP.
func BuildHTML(m *manifest.Manifest, reg *icons.Registry) ([]byte, error) {
	if m == nil {
		return nil, fmt.Errorf("manifest must not be nil")
	}
	if reg == nil {
		return nil, fmt.Errorf("icons registry must not be nil")
	}

	fontCSS, diagramCSS, routerJS, err := loadStaticAssets()
	if err != nil {
		return nil, err
	}

	doc, err := PrepareDocument(m, reg)
	if err != nil {
		return nil, err
	}

	routingViews := make([]routingViewJSON, len(doc.Views))
	for i, v := range doc.Views {
		routingViews[i] = routingViewJSON{ID: v.ID, Edges: v.Edges}
	}
	// encoding/json escapes '<', '>', and '&' as \u003c, \u003e, \u0026 preventing </script> breakouts.
	routingBytes, err := json.Marshal(routingViews)
	if err != nil {
		return nil, fmt.Errorf("marshal routing data: %w", err)
	}

	title := "GCP Architecture"
	kicker := ""
	if len(m.Metadata.Scopes) > 0 {
		kicker = strings.Join(m.Metadata.Scopes, ", ")
		title = "GCP Architecture — " + kicker
	}

	data := htmlTemplateData{
		CSPAttr:     template.HTMLAttr(`content="` + ContentSecurityPolicy + `"`),
		Title:       title,
		ScopeKicker: kicker,
		FontCSS:     template.CSS(fontCSS),
		DiagramCSS:  template.CSS(diagramCSS),
		IconSprite:  template.HTML(reg.SymbolSprite(doc.UsedIconKeys)),
		Views:       doc.Views,
		RoutingJSON: template.JS(routingBytes),
		RouterJS:    template.JS(routerJS),
	}

	var buf bytes.Buffer
	buf.Grow(len(fontCSS) + len(diagramCSS) + len(routerJS) + len(routingBytes) + 64*1024)
	if err := documentTemplate.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("render html template: %w", err)
	}
	return buf.Bytes(), nil
}
