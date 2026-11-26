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
	Title       string
	FontCSS     template.CSS
	DiagramCSS  template.CSS
	IconSprite  template.HTML
	Views       []PreparedView
	RoutingJSON template.JS
	RouterJS    template.JS
}

var documentTemplate = template.Must(template.New("architecture").Funcs(template.FuncMap{
	"css": func(s string) template.CSS { return template.CSS(s) },
	"mul": func(a, b int) int { return a * b },
	"add": func(a, b int) int { return a + b },
}).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; font-src data:; img-src data:; script-src 'unsafe-inline'">
<title>{{.Title}}</title>
<style>
{{.FontCSS}}
{{.DiagramCSS}}
</style>
</head>
<body>
{{.IconSprite}}
<main class="document-root">
{{- range .Views}}
<section class="view-sheet" data-view-id="{{.ID}}" data-archetype="{{.Archetype}}" data-cols="{{.NumCols}}">
  <header class="view-header">
    <div class="view-title-group">
      <h1 class="view-title">{{.Title}}</h1>
      {{- if .Subtitle}}<p class="view-subtitle">{{.Subtitle}}</p>{{end}}
    </div>
    <div class="view-legend">
      <span class="legend-item"><span class="legend-swatch runtime"></span>Runtime</span>
      <span class="legend-item"><span class="legend-swatch cicd"></span>CI/CD</span>
      <span class="legend-item"><span class="legend-swatch admin"></span>Admin</span>
      <span class="legend-item"><span class="legend-swatch inferred"></span>Inferred</span>
    </div>
  </header>
  {{- if .StageBanners}}
  <div class="stage-banners" style="grid-template-columns: {{.ColTracksCSS | css}};">
    {{- range .StageBanners}}
    <div class="stage-banner" style="grid-column: {{.GridCol}};">{{.Label}}</div>
    {{- end}}
  </div>
  {{- end}}
  <div class="view-canvas">
    <div class="view-grid" style="grid-template-columns: {{.ColTracksCSS | css}}; grid-template-rows: {{.RowTracksCSS | css}};">
      {{- range .CorridorCols}}
      <div class="corridor-col" data-corridor-col="{{.}}" style="grid-column: {{add (mul . 2) 2}}; grid-row: 1 / -1;"></div>
      {{- end}}
      {{- range .CorridorRows}}
      <div class="corridor-row" data-corridor-row="{{.}}" style="grid-column: 1 / -1; grid-row: {{add (mul . 2) 1}};"></div>
      {{- end}}
      {{- range .Containers}}
      <div class="arch-container" data-container-id="{{.ID}}" data-depth="{{.Depth}}" data-kind="{{.Kind}}"{{if .HasInnerTop}} data-has-inner-top="1"{{end}} style="grid-column: {{.GridColSpec | css}}; grid-row: {{.GridRowSpec | css}};"></div>
      <span class="arch-container-badge" data-container-id="{{.ID}}" data-depth="{{.Depth}}"{{if .HasInnerTop}} data-has-inner-top="1"{{end}} style="grid-column: {{.BadgeGridCol}}; grid-row: {{.BadgeGridRow}};">{{.Title}}</span>
      {{- end}}
      {{- range .Cards}}
      <article class="arch-card" data-component-id="{{.ComponentID}}" data-grid-col="{{.GridCol}}" data-grid-row="{{.GridRow}}" style="grid-column: {{.CSSGridCol}}; grid-row: {{.CSSGridRow}};{{if .MinHeightPx}} min-height: {{.MinHeightPx}}px;{{end}}">
        <div class="arch-card-header">
          <svg class="arch-card-icon" viewBox="0 0 24 24" aria-hidden="true"><use href="#gcp-icon-{{.IconKey}}"></use></svg>
          <div class="arch-card-titles">
            <div class="arch-card-title">{{.Title}}</div>
            {{- if .Subtitle}}<div class="arch-card-subtitle">{{.Subtitle}}</div>{{end}}
          </div>
        </div>
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
        {{- if .OffPageRefs}}
        <div class="arch-card-refs">
          {{- range .OffPageRefs}}
          <span class="off-page-pill">{{if eq .Direction "IN"}}&larr;{{else}}&rarr;{{end}} {{.Label}}</span>
          {{- end}}
        </div>
        {{- end}}
      </article>
      {{- end}}
    </div>
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
// bundled base64 .woff2 fonts, inline SVG <symbol> sprites, escaped JSON routing metadata,
// and strict CSP.
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
	if len(m.Metadata.Scopes) > 0 {
		title = "GCP Architecture — " + strings.Join(m.Metadata.Scopes, ", ")
	}

	data := htmlTemplateData{
		Title:       title,
		FontCSS:     template.CSS(fontCSS),
		DiagramCSS:  template.CSS(diagramCSS),
		IconSprite:  template.HTML(reg.SymbolSprite(doc.UsedIconKeys)),
		Views:       doc.Views,
		RoutingJSON: template.JS(routingBytes),
		RouterJS:    template.JS(routerJS),
	}

	var buf bytes.Buffer
	if err := documentTemplate.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("render html template: %w", err)
	}
	return buf.Bytes(), nil
}
