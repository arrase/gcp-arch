package icons

import (
	"encoding/xml"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestValidateSVGRejections(t *testing.T) {
	badSVGs := []struct {
		name string
		svg  string
	}{
		{name: "empty", svg: "   "},
		{name: "malformed xml", svg: `<svg viewBox="0 0 24 24"><rect></svg>`},
		{name: "xml doctype directive", svg: `<!DOCTYPE svg [<!ENTITY xxe "boom">]><svg viewBox="0 0 24 24"><rect/></svg>`},
		{name: "xml processing instruction", svg: `<?xml-stylesheet type="text/xsl" href="evil.xsl"?><svg viewBox="0 0 24 24"><rect/></svg>`},
		{name: "non svg root", svg: `<div/>`},
		{name: "multiple roots", svg: `<svg viewBox="0 0 24 24"><rect/></svg><svg/>`},
		{name: "trailing text outside root", svg: `<svg viewBox="0 0 24 24"><rect/></svg>trailing`},
		{name: "script element", svg: `<svg viewBox="0 0 24 24"><script>alert(1)</script></svg>`},
		{name: "foreignObject element", svg: `<svg viewBox="0 0 24 24"><foreignObject><div/></foreignObject></svg>`},
		{name: "iframe element", svg: `<svg viewBox="0 0 24 24"><iframe/></svg>`},
		{name: "embed element", svg: `<svg viewBox="0 0 24 24"><embed/></svg>`},
		{name: "object element", svg: `<svg viewBox="0 0 24 24"><object/></svg>`},
		{name: "onclick handler", svg: `<svg viewBox="0 0 24 24"><rect onclick="alert(1)"/></svg>`},
		{name: "onload handler", svg: `<svg viewBox="0 0 24 24" onload="alert(1)"><rect/></svg>`},
		{name: "unsupported xmlns", svg: `<svg xmlns="http://evil.example/ns" viewBox="0 0 24 24"><rect/></svg>`},
		{name: "unsupported prefixed xmlns", svg: `<svg xmlns="http://www.w3.org/2000/svg" xmlns:foo="http://evil.example/foo"><rect/></svg>`},
		{name: "javascript scheme", svg: `<svg viewBox="0 0 24 24"><a href="javascript:alert(1)"><rect/></a></svg>`},
		{name: "whitespace obfuscated javascript scheme", svg: `<svg viewBox="0 0 24 24"><a href="java script:alert(1)"><rect/></a></svg>`},
		{name: "entity encoded data scheme", svg: `<svg viewBox="0 0 24 24"><image href="data&#58;image/svg+xml,foo"/></svg>`},
		{name: "external http url", svg: `<svg viewBox="0 0 24 24"><image href="https://evil.example/a.png"/></svg>`},
		{name: "protocol relative href", svg: `<svg viewBox="0 0 24 24"><image href="//evil.example/a.png"/></svg>`},
		{name: "entity encoded protocol relative href", svg: `<svg viewBox="0 0 24 24"><image href="&#x2f;&#x2f;evil.example/a.png"/></svg>`},
		{name: "css @import in style element", svg: `<svg viewBox="0 0 24 24"><style>@import "//evil.example/a.css";</style><rect/></svg>`},
		{name: "css expression in style attr", svg: `<svg viewBox="0 0 24 24"><rect style="width:expression(alert(1))"/></svg>`},
		{name: "css backslash escape", svg: `<svg viewBox="0 0 24 24"><rect style="fill:\75 rl(//evil.example/x)"/></svg>`},
		{name: "css url external", svg: `<svg viewBox="0 0 24 24"><rect style="fill:url(//evil.example/x.svg#a)"/></svg>`},
		{name: "css url malformed", svg: `<svg viewBox="0 0 24 24"><rect fill="url(#broken"/></svg>`},
		{name: "css url empty hash", svg: `<svg viewBox="0 0 24 24"><rect fill="url('#')"/></svg>`},
	}

	for _, tc := range badSVGs {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateSVG(tc.svg); err == nil {
				t.Fatalf("ValidateSVG(%q) succeeded, expected error", tc.svg)
			}
		})
	}
}

func TestValidateSVGAcceptsSafeInlineSVG(t *testing.T) {
	valid := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><defs><filter id="A"><feFlood flood-color="#fff"/></filter></defs><style>.cls{fill:url(#A);}</style><rect style="fill:#4285F4;stroke:#1A73E8;" filter="url('#A')" width="20" height="20"/></svg>`
	if err := ValidateSVG(valid); err != nil {
		t.Fatalf("ValidateSVG failed on valid SVG: %v", err)
	}
}

type parsedSprite struct {
	ids       map[string]int
	defsIDs   map[string]bool
	symbolIDs map[string]string
	hrefRefs  []string
	urlRefs   []string
}

type spriteParser struct {
	res      parsedSprite
	urlRefRe *regexp.Regexp
	depth    int
	inDefs   bool
	inSymbol bool
}

func (p *spriteParser) handleStart(tk xml.StartElement) {
	local := strings.ToLower(tk.Name.Local)
	p.depth++
	if p.depth == 2 && local == "defs" {
		p.inDefs = true
	}
	if local == "symbol" {
		p.inSymbol = true
	}
	recordSpriteStartAttrs(&p.res, local, tk.Attr, p.inDefs && !p.inSymbol, p.urlRefRe)
}

func (p *spriteParser) handleEnd(tk xml.EndElement) {
	local := strings.ToLower(tk.Name.Local)
	if local == "symbol" {
		p.inSymbol = false
	}
	if p.depth == 2 && local == "defs" {
		p.inDefs = false
	}
	p.depth--
}

func parseSpriteForTest(t *testing.T, sprite string) parsedSprite {
	t.Helper()
	p := spriteParser{
		res: parsedSprite{
			ids:       make(map[string]int),
			defsIDs:   make(map[string]bool),
			symbolIDs: make(map[string]string),
		},
		urlRefRe: regexp.MustCompile(`(?i)url\(\s*['"]?#([^)'"\s]+)['"]?\s*\)`),
	}
	dec := xml.NewDecoder(strings.NewReader(sprite))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("sprite XML decode error: %v", err)
		}
		switch tk := tok.(type) {
		case xml.StartElement:
			p.handleStart(tk)
		case xml.EndElement:
			p.handleEnd(tk)
		}
	}
	return p.res
}

func recordSpriteStartAttrs(res *parsedSprite, local string, attrs []xml.Attr, inRootDefs bool, urlRefRe *regexp.Regexp) {
	var elemID, elemVB string
	for _, a := range attrs {
		switch a.Name.Local {
		case "id":
			elemID = a.Value
			res.ids[a.Value]++
			if inRootDefs {
				res.defsIDs[a.Value] = true
			}
		case "viewBox":
			elemVB = a.Value
		case "href":
			if strings.HasPrefix(a.Value, "#") {
				res.hrefRefs = append(res.hrefRefs, strings.TrimPrefix(a.Value, "#"))
			}
		}
		for _, m := range urlRefRe.FindAllStringSubmatch(a.Value, -1) {
			res.urlRefs = append(res.urlRefs, m[1])
		}
	}
	if local == "symbol" {
		res.symbolIDs[elemID] = elemVB
	}
}

func TestLoadRegistryKeysAndFullSprite(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	keys := reg.ValidKeys()
	// 80 salvaged legacy icons + 5 newly appended 24x24 pipeline icons.
	const wantCount = 85
	if len(keys) != wantCount {
		t.Fatalf("len(ValidKeys()) = %d, want %d", len(keys), wantCount)
	}
	if !slices.IsSorted(keys) {
		t.Fatal("ValidKeys() is not sorted")
	}

	// Mutating returned slice must not affect Registry internals.
	origFirst := keys[0]
	keys[0] = "mutated"
	if reg.ValidKeys()[0] != origFirst {
		t.Fatal("ValidKeys() returned uncloned backing slice")
	}

	requiredNew := []string{
		"github", "external_client", "private_services_access",
		"k8s_workload", "k8s_ingress", "pubsub",
		"cloud_build", "cloud_sql", "google_kubernetes_engine",
	}
	for _, k := range requiredNew {
		if !reg.Has(k) {
			t.Errorf("Has(%q) = false, want true", k)
		}
	}
	if reg.Has("nonexistent_icon") {
		t.Error("Has(nonexistent_icon) = true, want false")
	}

	// Compile ALL 85 icons into one sprite and verify XML validity, ID uniqueness,
	// symbol IDs, and <defs> hoisting for <filter>/<mask>.
	allKeys := reg.ValidKeys()
	sprite := reg.SymbolSprite(allKeys)
	if !strings.HasPrefix(sprite, `<svg style="display:none"><defs>`) {
		t.Fatalf("unexpected sprite prefix: %.80s", sprite)
	}

	parsed := parseSpriteForTest(t, sprite)
	assertSpriteValid(t, parsed, allKeys)
}

func assertSpriteValid(t *testing.T, parsed parsedSprite, allKeys []string) {
	t.Helper()
	assertSpriteSymbols(t, parsed.symbolIDs, allKeys)
	assertSpriteIDsAndRefs(t, parsed)
	// Verify pubsub <filter id="A"> and <mask id="B"> were hoisted into <defs> with icon-pubsub- prefix.
	for _, hoisted := range []string{"icon-pubsub-A", "icon-pubsub-B"} {
		if !parsed.defsIDs[hoisted] {
			t.Errorf("expected %q to be hoisted into root <defs>", hoisted)
		}
	}
}

func assertSpriteSymbols(t *testing.T, symbolIDs map[string]string, allKeys []string) {
	t.Helper()
	if len(symbolIDs) != len(allKeys) {
		t.Fatalf("compiled symbol count = %d, want %d", len(symbolIDs), len(allKeys))
	}
	for _, k := range allKeys {
		symID := "gcp-icon-" + k
		vb, ok := symbolIDs[symID]
		if !ok {
			t.Errorf("missing symbol %q in sprite", symID)
		}
		if vb == "" {
			t.Errorf("symbol %q has empty viewBox", symID)
		}
	}
}

func assertSpriteIDsAndRefs(t *testing.T, parsed parsedSprite) {
	t.Helper()
	for id, count := range parsed.ids {
		if count > 1 {
			t.Errorf("duplicate ID %q appeared %d times in sprite", id, count)
		}
	}
	for _, ref := range parsed.hrefRefs {
		if parsed.ids[ref] == 0 {
			t.Errorf("dangling href=#%s in sprite", ref)
		}
	}
	for _, ref := range parsed.urlRefs {
		if parsed.ids[ref] == 0 {
			t.Errorf("dangling url(#%s) in sprite", ref)
		}
	}
}

func TestSymbolSpriteDedupOrderAndEdgeCases(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}

	// Duplicate, unsorted, and unknown keys must produce identical deterministic output.
	s1 := reg.SymbolSprite([]string{"github", "unknown_key", "cloud_sql", "github"})
	s2 := reg.SymbolSprite([]string{"cloud_sql", "github"})
	if s1 != s2 {
		t.Fatalf("SymbolSprite not deterministic/deduplicated:\ns1=%s\ns2=%s", s1, s2)
	}
	if strings.Contains(s1, "<defs>") {
		t.Fatalf("expected no <defs> block when selected icons have no hoisted defs: %s", s1)
	}
	if strings.Index(s1, `id="gcp-icon-cloud_sql"`) > strings.Index(s1, `id="gcp-icon-github"`) {
		t.Fatalf("expected sorted symbol order in sprite: %s", s1)
	}

	// Empty / all-unknown keys produces minimal hidden <svg>.
	if got := reg.SymbolSprite([]string{"missing"}); got != `<svg style="display:none"></svg>` {
		t.Fatalf("empty sprite = %q, want minimal hidden svg", got)
	}

	// Concurrent safety under -race.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = reg.Has("github")
			_ = reg.ValidKeys()
			_ = reg.SymbolSprite([]string{"pubsub", "github", "k8s_workload"})
		}()
	}
	wg.Wait()
}

func TestLoadFromYAMLCustomRewritingAndErrors(t *testing.T) {
	customYAML := `schema_version: "v1"
icons:
  grad_icon:
    svg: '<svg xmlns="http://www.w3.org/2000/svg" width="32px" height="16px" fill="url(#g1)" preserveAspectRatio="none"><linearGradient id="g1"><stop offset="0%" stop-color="#4285f4"/></linearGradient><style>.c{fill:url(#g1);}</style><g id="shape" xml:space="preserve"><rect width="32" height="16"/></g><use href="#shape"/><use href="ignored"/></svg>'
  novb_icon:
    svg: '<svg xmlns="http://www.w3.org/2000/svg"><circle cx="12" cy="12" r="10"/></svg>'
`
	reg, err := loadFromYAML([]byte(customYAML))
	if err != nil {
		t.Fatalf("loadFromYAML valid error: %v", err)
	}

	sprite := reg.SymbolSprite([]string{"grad_icon", "novb_icon"})
	checks := []string{
		`<defs><linearGradient id="icon-grad_icon-g1"><stop offset="0%" stop-color="#4285f4"></stop></linearGradient></defs>`,
		`<symbol id="gcp-icon-grad_icon" viewBox="0 0 32 16" fill="url(#icon-grad_icon-g1)" preserveAspectRatio="none">`,
		`.c{fill:url(#icon-grad_icon-g1);}`,
		`id="icon-grad_icon-shape" xml:space="preserve"`,
		`<use href="#icon-grad_icon-shape"></use>`,
		`<symbol id="gcp-icon-novb_icon" viewBox="0 0 24 24" preserveAspectRatio="xMidYMid meet">`,
	}
	for _, want := range checks {
		if !strings.Contains(sprite, want) {
			t.Errorf("sprite missing %q\ngot: %s", want, sprite)
		}
	}

	badYAMLs := []struct {
		name string
		data string
	}{
		{name: "unknown yaml field", data: "schema_version: \"v1\"\nbogus: 1\n"},
		{name: "wrong schema version", data: "schema_version: \"v2\"\nicons:\n  a:\n    svg: '<svg/>'\n"},
		{name: "empty icons map", data: "schema_version: \"v1\"\nicons: {}\n"},
		{name: "empty icon key", data: "schema_version: \"v1\"\nicons:\n  \" \":\n    svg: '<svg/>'\n"},
		{name: "unsafe svg", data: "schema_version: \"v1\"\nicons:\n  bad:\n    svg: '<svg><script/></svg>'\n"},
	}
	for _, tc := range badYAMLs {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadFromYAML([]byte(tc.data)); err == nil {
				t.Fatalf("loadFromYAML(%s) succeeded, want error", tc.name)
			}
		})
	}

	// validateAndCompileSVG must fail cleanly on malformed XML rather than spinning.
	if _, err := validateAndCompileSVG("bad", `<svg viewBox="0 0 24 24"><rect></svg>`); err == nil {
		t.Fatal("validateAndCompileSVG succeeded on malformed XML, want error")
	}
}
