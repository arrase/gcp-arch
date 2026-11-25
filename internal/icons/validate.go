package icons

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var (
	forbiddenSVGElements = map[string]bool{
		"script":        true,
		"foreignobject": true,
		"iframe":        true,
		"embed":         true,
		"object":        true,
	}
	forbiddenSchemeRe = regexp.MustCompile(`(?i)(?:javascript|vbscript|data)\s*:`)
	httpURLRe         = regexp.MustCompile(`(?i)https?://`)
	cssCommentRe      = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssImportRe       = regexp.MustCompile(`(?i)@import\b`)
	cssExpressionRe   = regexp.MustCompile(`(?i)expression\s*\(`)
	cssURLStartRe     = regexp.MustCompile(`(?i)url\s*\(`)
)

const svgXMLNS = "http://www.w3.org/2000/svg"

type svgValidator struct {
	depth    int
	rootSeen bool
	inStyle  int
}

func (v *svgValidator) handleStart(elem xml.StartElement) error {
	local := strings.ToLower(elem.Name.Local)
	if v.depth == 0 {
		if v.rootSeen {
			return fmt.Errorf("inline SVG must have a single <svg> root element")
		}
		if local != "svg" {
			return fmt.Errorf("inline SVG root element must be <svg>, got <%s>", elem.Name.Local)
		}
		v.rootSeen = true
	}
	v.depth++

	if forbiddenSVGElements[local] {
		return fmt.Errorf("inline SVG contains forbidden element <%s>", elem.Name.Local)
	}
	if local == "style" {
		v.inStyle++
	}
	for _, attr := range elem.Attr {
		if err := validateSVGAttr(attr); err != nil {
			return err
		}
	}
	return nil
}

func (v *svgValidator) handleCharData(data xml.CharData) error {
	if v.depth == 0 {
		if strings.TrimSpace(string(data)) != "" {
			return fmt.Errorf("inline SVG must not contain trailing content outside <svg>")
		}
		return nil
	}
	if v.inStyle > 0 {
		return validateCSSContent(string(data))
	}
	return nil
}

// ValidateSVG strictly parses an inline SVG string with encoding/xml and verifies it is safe
// for embedded symbol sprites: single <svg> root, no XML directives/processing instructions,
// no forbidden elements, no on* event handlers, no external/protocol-relative URLs or dangerous
// URI schemes, no CSS @import/expression/backslash escapes, and only local #id url(...) references.
func ValidateSVG(svg string) error {
	s := strings.TrimSpace(svg)
	if s == "" {
		return fmt.Errorf("inline SVG must not be empty")
	}

	dec := xml.NewDecoder(strings.NewReader(s))
	var v svgValidator

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("inline SVG is not well-formed XML: %w", err)
		}

		switch t := tok.(type) {
		case xml.Directive:
			return fmt.Errorf("inline SVG must not contain XML directives")
		case xml.ProcInst:
			return fmt.Errorf("inline SVG must not contain XML processing instructions")
		case xml.StartElement:
			if err := v.handleStart(t); err != nil {
				return err
			}
		case xml.EndElement:
			if strings.EqualFold(t.Name.Local, "style") && v.inStyle > 0 {
				v.inStyle--
			}
			v.depth--
		case xml.CharData:
			if err := v.handleCharData(t); err != nil {
				return err
			}
		}
	}

	if !v.rootSeen || v.depth != 0 {
		return fmt.Errorf("inline SVG must contain a complete <svg> root element")
	}
	return nil
}

func validateSVGAttr(attr xml.Attr) error {
	local := strings.ToLower(attr.Name.Local)
	space := strings.ToLower(attr.Name.Space)
	val := attr.Value

	if strings.HasPrefix(local, "on") {
		return fmt.Errorf("inline SVG contains forbidden event handler %q", attr.Name.Local)
	}
	if space == "" && local == "xmlns" {
		if strings.TrimSpace(val) != svgXMLNS {
			return fmt.Errorf("inline SVG has unsupported xmlns %q", val)
		}
		return nil
	}
	if space == "xmlns" {
		return fmt.Errorf("inline SVG has unsupported xmlns:%s %q", attr.Name.Local, val)
	}

	if err := validateValueSchemesAndURLs(val); err != nil {
		return err
	}
	if local == "href" {
		if strings.HasPrefix(strings.TrimSpace(val), "//") || strings.HasPrefix(stripControlAndSpace(val), "//") {
			return fmt.Errorf("inline SVG must not contain protocol-relative external URLs")
		}
	}
	if local == "style" {
		if err := validateCSSContent(val); err != nil {
			return err
		}
	}
	return nil
}

func validateCSSContent(css string) error {
	if strings.Contains(css, `\`) {
		return fmt.Errorf("inline SVG CSS must not contain backslash escapes")
	}
	cleaned := cssCommentRe.ReplaceAllString(css, "")
	compact := stripControlAndSpace(cleaned)
	if cssImportRe.MatchString(cleaned) || cssImportRe.MatchString(compact) {
		return fmt.Errorf("inline SVG CSS must not contain @import")
	}
	if cssExpressionRe.MatchString(cleaned) || cssExpressionRe.MatchString(compact) {
		return fmt.Errorf("inline SVG CSS must not contain expression()")
	}
	return validateValueSchemesAndURLs(cleaned)
}

func validateValueSchemesAndURLs(val string) error {
	compact := stripControlAndSpace(val)
	if m := forbiddenSchemeRe.FindString(val); m != "" {
		return fmt.Errorf("inline SVG contains forbidden URI scheme %q", m)
	}
	if m := forbiddenSchemeRe.FindString(compact); m != "" {
		return fmt.Errorf("inline SVG contains forbidden URI scheme %q", m)
	}
	if httpURLRe.MatchString(val) || httpURLRe.MatchString(compact) {
		return fmt.Errorf("inline SVG must not contain external http:// or https:// URLs")
	}
	return validateLocalCSSURLs(val)
}

func validateLocalCSSURLs(s string) error {
	matches := cssURLStartRe.FindAllStringIndex(s, -1)
	for _, loc := range matches {
		rest := s[loc[1]:]
		end := strings.IndexByte(rest, ')')
		if end < 0 {
			return fmt.Errorf("inline SVG contains malformed url() reference")
		}
		target := strings.TrimSpace(rest[:end])
		if len(target) >= 2 && ((target[0] == '"' && target[len(target)-1] == '"') || (target[0] == '\'' && target[len(target)-1] == '\'')) {
			target = strings.TrimSpace(target[1 : len(target)-1])
		}
		if !strings.HasPrefix(target, "#") || len(target) < 2 {
			return fmt.Errorf("inline SVG url() reference %q must target a local #id", target)
		}
	}
	return nil
}

func stripControlAndSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r > ' ' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
