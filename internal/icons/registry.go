package icons

import (
	"bytes"
	_ "embed"
	"encoding/xml"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed assets/icons.yaml
var embeddedIconsYAML []byte

var svgURLRefRe = regexp.MustCompile(`(?i)url\(\s*['"]?#([^)'"\s]+)['"]?\s*\)`)

type iconsFile struct {
	SchemaVersion string              `yaml:"schema_version"`
	Icons         map[string]iconSpec `yaml:"icons"`
}

type iconSpec struct {
	SVG string `yaml:"svg"`
}

type compiledSymbol struct {
	id          string
	viewBox     string
	rootAttrs   []xml.Attr
	hoistedDefs string
	body        string
}

// Registry holds validated, pre-compiled inline SVG icon symbols.
type Registry struct {
	symbols map[string]compiledSymbol
	keys    []string
}

var (
	defaultRegistryOnce sync.Once
	defaultRegistry     *Registry
	defaultRegistryErr  error
)

// Load decodes, validates, and compiles the embedded SVG icon registry.
func Load() (*Registry, error) {
	defaultRegistryOnce.Do(func() {
		defaultRegistry, defaultRegistryErr = loadFromYAML(embeddedIconsYAML)
	})
	return defaultRegistry, defaultRegistryErr
}

func loadFromYAML(data []byte) (*Registry, error) {
	var f iconsFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("decode icons yaml: %w", err)
	}
	if f.SchemaVersion != "v1" {
		return nil, fmt.Errorf("unsupported icons schema_version %q", f.SchemaVersion)
	}
	if len(f.Icons) == 0 {
		return nil, fmt.Errorf("icons registry must contain at least one icon")
	}

	keys := make([]string, 0, len(f.Icons))
	for k := range f.Icons {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	symbols := make(map[string]compiledSymbol, len(keys))
	for _, key := range keys {
		if strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("icon key must not be empty")
		}
		rawSVG := f.Icons[key].SVG
		if err := ValidateSVG(rawSVG); err != nil {
			return nil, fmt.Errorf("invalid SVG for icon %q: %w", key, err)
		}
		symbols[key] = compileIconSymbol(key, rawSVG)
	}

	return &Registry{
		symbols: symbols,
		keys:    keys,
	}, nil
}

// Has reports whether key is registered in the icon registry.
func (r *Registry) Has(key string) bool {
	_, ok := r.symbols[key]
	return ok
}

// ValidKeys returns a sorted copy of all registered icon keys.
func (r *Registry) ValidKeys() []string {
	return slices.Clone(r.keys)
}

// SymbolSprite compiles the deduplicated, sorted subset of keys into a hidden inline SVG sprite
// with hoisted root <defs> and <symbol id="gcp-icon-<key>"> elements.
func (r *Registry) SymbolSprite(keys []string) string {
	unique := make([]string, 0, len(keys))
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		if seen[k] || !r.Has(k) {
			continue
		}
		seen[k] = true
		unique = append(unique, k)
	}
	sort.Strings(unique)

	var defs bytes.Buffer
	var syms bytes.Buffer
	for _, k := range unique {
		sym := r.symbols[k]
		if sym.hoistedDefs != "" {
			defs.WriteString(sym.hoistedDefs)
		}
		fmt.Fprintf(&syms, `<symbol id="%s" viewBox="%s"`, sym.id, escapeXML(sym.viewBox))
		hasPreserveAspect := false
		for _, attr := range sym.rootAttrs {
			if strings.EqualFold(attr.Name.Local, "preserveAspectRatio") {
				hasPreserveAspect = true
			}
			fmt.Fprintf(&syms, ` %s="%s"`, attr.Name.Local, escapeXML(attr.Value))
		}
		if !hasPreserveAspect {
			syms.WriteString(` preserveAspectRatio="xMidYMid meet"`)
		}
		syms.WriteByte('>')
		syms.WriteString(sym.body)
		syms.WriteString("</symbol>")
	}

	var out bytes.Buffer
	out.WriteString(`<svg style="display:none">`)
	if defs.Len() > 0 {
		out.WriteString("<defs>")
		out.Write(defs.Bytes())
		out.WriteString("</defs>")
	}
	out.Write(syms.Bytes())
	out.WriteString("</svg>")
	return out.String()
}

type symbolCompiler struct {
	idPrefix    string
	viewBox     string
	rootW       string
	rootH       string
	rootAttrs   []xml.Attr
	hoistedDefs bytes.Buffer
	body        bytes.Buffer
	depth       int
	defsDepth   int
	hoistDepth  int
	inStyle     int
}

func (c *symbolCompiler) handleRootStart(elem xml.StartElement) {
	for _, attr := range elem.Attr {
		switch strings.ToLower(attr.Name.Local) {
		case "viewbox":
			c.viewBox = strings.TrimSpace(attr.Value)
		case "width":
			c.rootW = strings.TrimSpace(attr.Value)
		case "height":
			c.rootH = strings.TrimSpace(attr.Value)
		default:
			if isPreservedRootAttr(attr) {
				attr.Value = rewriteURLRefs(attr.Value, c.idPrefix)
				c.rootAttrs = append(c.rootAttrs, attr)
			}
		}
	}
	c.depth = 1
}

func (c *symbolCompiler) handleStart(elem xml.StartElement) {
	if c.depth == 0 {
		c.handleRootStart(elem)
		return
	}
	localLower := strings.ToLower(elem.Name.Local)
	c.depth++
	if localLower == "style" {
		c.inStyle++
	}
	if c.defsDepth == 0 && c.hoistDepth == 0 && localLower == "defs" {
		c.defsDepth = 1
		return
	}

	target := &c.body
	switch {
	case c.defsDepth > 0:
		c.defsDepth++
		target = &c.hoistedDefs
	case c.hoistDepth > 0:
		c.hoistDepth++
		target = &c.hoistedDefs
	case isHoistableDefTag(localLower):
		c.hoistDepth = 1
		target = &c.hoistedDefs
	}
	writeRewrittenStartTag(target, elem, c.idPrefix)
}

func (c *symbolCompiler) handleEnd(elem xml.EndElement) {
	if c.depth == 1 {
		c.depth = 0
		return
	}
	if strings.EqualFold(elem.Name.Local, "style") && c.inStyle > 0 {
		c.inStyle--
	}
	c.depth--

	if c.defsDepth > 0 {
		c.defsDepth--
		if c.defsDepth > 0 {
			fmt.Fprintf(&c.hoistedDefs, "</%s>", elem.Name.Local)
		}
		return
	}
	if c.hoistDepth > 0 {
		fmt.Fprintf(&c.hoistedDefs, "</%s>", elem.Name.Local)
		c.hoistDepth--
		return
	}
	fmt.Fprintf(&c.body, "</%s>", elem.Name.Local)
}

func (c *symbolCompiler) handleCharData(data xml.CharData) {
	if c.depth <= 1 || len(bytes.TrimSpace(data)) == 0 {
		return
	}
	text := string(data)
	if c.inStyle > 0 {
		text = rewriteURLRefs(text, c.idPrefix)
	}
	target := &c.body
	if c.defsDepth > 0 || c.hoistDepth > 0 {
		target = &c.hoistedDefs
	}
	target.WriteString(escapeXML(text))
}

// compileIconSymbol extracts a validated inline <svg> into a <symbol> body plus hoisted root <defs>
// (<filter>, <mask>, gradients, <clipPath>, <pattern>), prefixing every internal ID and
// url(#...)/href="#..." reference with "icon-<key>-".
func compileIconSymbol(key, rawSVG string) compiledSymbol {
	c := &symbolCompiler{idPrefix: "icon-" + key + "-"}
	dec := xml.NewDecoder(strings.NewReader(strings.TrimSpace(rawSVG)))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			c.handleStart(t)
		case xml.EndElement:
			c.handleEnd(t)
		case xml.CharData:
			c.handleCharData(t)
		}
	}
	if c.viewBox == "" {
		c.viewBox = fallbackViewBox(c.rootW, c.rootH)
	}
	return compiledSymbol{
		id:          "gcp-icon-" + key,
		viewBox:     c.viewBox,
		rootAttrs:   c.rootAttrs,
		hoistedDefs: c.hoistedDefs.String(),
		body:        c.body.String(),
	}
}

const (
	xmlNS   = "http://www.w3.org/XML/1998/namespace"
	xlinkNS = "http://www.w3.org/1999/xlink"
)

func writeRewrittenStartTag(dst *bytes.Buffer, elem xml.StartElement, idPrefix string) {
	dst.WriteByte('<')
	dst.WriteString(elem.Name.Local)
	seen := make(map[string]bool, len(elem.Attr))
	for _, attr := range elem.Attr {
		name, val, ok := rewriteStartAttr(attr, idPrefix)
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		fmt.Fprintf(dst, ` %s="%s"`, name, escapeXML(val))
	}
	dst.WriteByte('>')
}

func rewriteStartAttr(attr xml.Attr, idPrefix string) (string, string, bool) {
	localLower := strings.ToLower(attr.Name.Local)
	if localLower == "xmlns" || strings.EqualFold(attr.Name.Space, "xmlns") {
		return "", "", false
	}
	if localLower == "href" && isHrefNamespace(attr.Name.Space) {
		return rewriteHrefAttr(attr.Value, idPrefix)
	}
	name, ok := qualifyAttrName(attr.Name)
	if !ok {
		return "", "", false
	}
	if localLower == "id" && strings.TrimSpace(attr.Value) != "" {
		return name, idPrefix + strings.TrimSpace(attr.Value), true
	}
	return name, rewriteURLRefs(attr.Value, idPrefix), true
}

func isHrefNamespace(space string) bool {
	return space == "" || space == xlinkNS || strings.EqualFold(space, "xlink")
}

func rewriteHrefAttr(val, idPrefix string) (string, string, bool) {
	trimmed := strings.TrimSpace(val)
	if !strings.HasPrefix(trimmed, "#") || len(trimmed) <= 1 {
		return "", "", false
	}
	return "href", "#" + idPrefix + trimmed[1:], true
}

func qualifyAttrName(name xml.Name) (string, bool) {
	switch name.Space {
	case "":
		return name.Local, true
	case xmlNS:
		return "xml:" + name.Local, true
	default:
		return "", false
	}
}

func rewriteURLRefs(val, idPrefix string) string {
	if !strings.Contains(strings.ToLower(val), "url(") {
		return val
	}
	return svgURLRefRe.ReplaceAllString(val, "url(#"+idPrefix+"$1)")
}

func isHoistableDefTag(localLower string) bool {
	switch localLower {
	case "filter", "mask", "lineargradient", "radialgradient", "clippath", "pattern":
		return true
	}
	return false
}

func isPreservedRootAttr(attr xml.Attr) bool {
	if attr.Name.Space != "" {
		return false
	}
	switch strings.ToLower(attr.Name.Local) {
	case "fill", "fill-rule", "stroke", "stroke-width", "stroke-linecap", "stroke-linejoin", "clip-rule", "preserveaspectratio":
		return true
	}
	return false
}

func fallbackViewBox(wStr, hStr string) string {
	w, errW := strconv.ParseFloat(strings.TrimSuffix(wStr, "px"), 64)
	h, errH := strconv.ParseFloat(strings.TrimSuffix(hStr, "px"), 64)
	if errW == nil && errH == nil && w > 0 && h > 0 {
		return fmt.Sprintf("0 0 %s %s", fmtNum(w), fmtNum(h))
	}
	return "0 0 24 24"
}

func fmtNum(v float64) string {
	r := math.Round(v*100) / 100
	if r == 0 {
		return "0"
	}
	return strconv.FormatFloat(r, 'f', -1, 64)
}

func escapeXML(s string) string {
	var b bytes.Buffer
	b.Grow(len(s) + 8)
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
