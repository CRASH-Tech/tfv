// Package secrets resolves Vault references and anchor-variable templates inside
// a parsed YAML structure.
//
// Vault secrets use the explicit placeholder:
//
//	"<vault:common/data/oauth/app#client_id>"
//	"<vault:common/data/oauth/app#client_id | b64enc>"
//	"<vault:common/data/app#password | htpasswd \"admin\">"   (piped value is the
//	                                                            last argument)
//
// YAML anchor values are available as variables in "{{ $name }}" expressions,
// which are evaluated as Go templates with the full sprig (Helm) function set:
//
//	"{{ $region }}"                 -> the anchor's value (type preserved)
//	"my-{{ $region }}-{{ $env }}"   -> string interpolation
//	"{{ $cluster_id | default 0 }}" -> 0 (a number) when $cluster_id is unset
//
// When the whole value is a single "{{ ... }}" expression, the result type is
// inferred (number/bool/string). A bare "{{ $x }}" whose name is not an anchor,
// and any "{{ ... }}" not starting with "$", are left exactly as written, so
// templating meant for Helm, Vector, etc. passes through untouched.
package secrets

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"text/template"

	"tfv/internal/vault"

	"gopkg.in/yaml.v3"
)

// shortcutRe matches `<vault:PATH#KEY>` and `<vault:PATH#KEY | f1 | f2>`.
// The legacy argocd-vault-plugin prefix `<path:...>` is accepted as an alias.
var shortcutRe = regexp.MustCompile(`<(?:vault|path):\s*([^>|]+?)\s*(\|\s*[^>]+?)?\s*>`)

// blockRe matches a single `{{ ... }}` template block (non-greedy, newlines ok).
var blockRe = regexp.MustCompile(`(?s)\{\{(.*?)\}\}`)

// wholeBlockRe matches a string that is exactly one `{{ ... }}` block.
var wholeBlockRe = regexp.MustCompile(`(?s)^\{\{(.*?)\}\}$`)

// bareVarRe matches an expression that is just a variable reference, e.g. `$x`.
var bareVarRe = regexp.MustCompile(`^\$[A-Za-z_][A-Za-z0-9_]*$`)

// varNameRe finds `$name` variable references inside an expression.
var varNameRe = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)

// singleQuotedRe matches a single-quoted segment, e.g. 'text'.
var singleQuotedRe = regexp.MustCompile(`'[^']*'`)

// normalizeQuotes rewrites single-quoted string literals to double-quoted ones,
// so values like "{{ $x | default 'text' }}" work despite Go templates treating
// '...' as a (single-character) rune constant. Convenient because the YAML value
// itself is usually already wrapped in double quotes.
func normalizeQuotes(s string) string {
	return singleQuotedRe.ReplaceAllStringFunc(s, func(q string) string {
		inner := strings.ReplaceAll(q[1:len(q)-1], `"`, `\"`)
		return `"` + inner + `"`
	})
}

// Renderer resolves values using a fixed function map.
type Renderer struct {
	funcs template.FuncMap
}

// NewRenderer builds a renderer backed by the given Vault pool.
func NewRenderer(vp *vault.Pool) *Renderer {
	return &Renderer{funcs: FuncMap(vp)}
}

// renderString resolves anchor-variable templates and Vault placeholders in s.
// It may return a non-string value when the whole input is a single anchor
// expression, so that the original type (number/bool) is preserved.
func (r *Renderer) renderString(s string, anchors map[string]any) (any, error) {
	// Whole-value anchor expression: preserve the result's type.
	if m := wholeBlockRe.FindStringSubmatch(strings.TrimSpace(s)); m != nil {
		expr := strings.TrimSpace(m[1])
		if strings.HasPrefix(expr, "$") {
			if bareVarRe.MatchString(expr) {
				if v, ok := anchors[expr[1:]]; ok {
					return v, nil // keep the anchor's native type
				}
				return s, nil // unknown bare var: leave untouched (Helm passthrough)
			}
			out, err := r.execTemplate(expr, anchors)
			if err != nil {
				return nil, err
			}
			out, err = r.resolveVault(out)
			if err != nil {
				return nil, err
			}
			return typedScalar(out), nil
		}
	}

	// Otherwise interpolate anchor expressions into the string, then resolve
	// Vault placeholders.
	var firstErr error
	out := blockRe.ReplaceAllStringFunc(s, func(block string) string {
		if firstErr != nil {
			return block
		}
		expr := strings.TrimSpace(blockRe.FindStringSubmatch(block)[1])
		if !strings.HasPrefix(expr, "$") {
			return block // not an anchor expression: passthrough
		}
		if bareVarRe.MatchString(expr) {
			if v, ok := anchors[expr[1:]]; ok {
				return fmt.Sprintf("%v", v)
			}
			return block // unknown bare var: passthrough
		}
		res, err := r.execTemplate(expr, anchors)
		if err != nil {
			firstErr = err
			return block
		}
		return res
	})
	if firstErr != nil {
		return nil, firstErr
	}
	return r.resolveVault(out)
}

// execTemplate evaluates an anchor expression (e.g. `$cluster_id | default 0`)
// as a Go template with the sprig function set, binding each `$name` to the
// matching anchor value (or nil when undefined).
func (r *Renderer) execTemplate(expr string, anchors map[string]any) (string, error) {
	var b strings.Builder
	for _, name := range uniqueVarNames(expr) {
		fmt.Fprintf(&b, "{{- $%s := index $ %q -}}", name, name)
	}
	b.WriteString("{{")
	b.WriteString(normalizeQuotes(expr))
	b.WriteString("}}")

	t, err := template.New("anchor").Funcs(r.funcs).Parse(b.String())
	if err != nil {
		return "", fmt.Errorf("template {{%s}}: %w", expr, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, anchors); err != nil {
		return "", fmt.Errorf("template {{%s}}: %w", expr, err)
	}
	return buf.String(), nil
}

// resolveVault replaces every <vault:...> / <path:...> placeholder in s.
func (r *Renderer) resolveVault(s string) (string, error) {
	var firstErr error
	out := shortcutRe.ReplaceAllStringFunc(s, func(m string) string {
		if firstErr != nil {
			return m
		}
		sub := shortcutRe.FindStringSubmatch(m)
		ref := strings.TrimSpace(sub[1])
		pipes := normalizeQuotes(strings.TrimSpace(sub[2])) // e.g. "| b64enc" or ""
		text := fmt.Sprintf(`{{ vault %q }}`, ref)
		if pipes != "" {
			text = fmt.Sprintf(`{{ vault %q %s }}`, ref, pipes)
		}
		val, err := r.eval(text)
		if err != nil {
			firstErr = err
			return m
		}
		return val
	})
	return out, firstErr
}

func (r *Renderer) eval(text string) (string, error) {
	t, err := template.New("v").Funcs(r.funcs).Parse(text)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, nil); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func uniqueVarNames(expr string) []string {
	var names []string
	seen := map[string]bool{}
	for _, m := range varNameRe.FindAllStringSubmatch(expr, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			names = append(names, m[1])
		}
	}
	return names
}

// typedScalar reinterprets a rendered string as a YAML/JSON value, so a numeric,
// boolean, list or map result keeps that type rather than becoming a string.
// Plain strings (and bare dates) stay strings. Serialize lists/maps with toJson
// (or toYaml) so they round-trip cleanly, e.g.
// "{{ $cidrs | default (list '10.0.0.0/8') | toJson }}".
func typedScalar(s string) any {
	var v any
	if err := yaml.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	switch v.(type) {
	case bool, int, int64, uint64, float64, []any, map[string]any:
		return v
	default:
		return s // string, null, timestamp -> keep the rendered text
	}
}

// Resolve recursively renders every string scalar in v, returning the resolved
// structure. anchors holds YAML anchor values usable as "{{ $name }}". Maps and
// slices are walked in place.
func (r *Renderer) Resolve(v any, anchors map[string]any) (any, error) {
	switch val := v.(type) {
	case map[string]any:
		for k, item := range val {
			nv, err := r.Resolve(item, anchors)
			if err != nil {
				return nil, err
			}
			val[k] = nv
		}
		return val, nil
	case []any:
		for i, item := range val {
			nv, err := r.Resolve(item, anchors)
			if err != nil {
				return nil, err
			}
			val[i] = nv
		}
		return val, nil
	case string:
		return r.renderString(val, anchors)
	default:
		return v, nil
	}
}
