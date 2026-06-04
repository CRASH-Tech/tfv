// Package secrets resolves Vault references inside a parsed YAML structure.
//
// Only the explicit placeholder is evaluated:
//
//	"<vault:common/data/oauth/app#client_id>"
//	"<vault:common/data/oauth/app#client_id | b64enc>"
//	"<vault:common/data/app#password | htpasswd \"admin\">"   (piped value is the
//	                                                            last argument)
//
// Each placeholder is turned into a small "{{ vault ... }}" template and
// executed with the sprig + vault function map; the surrounding text is left
// untouched. Any other "{{ ... }}" in a value is deliberately NOT evaluated, so
// it passes through to downstream templating (Helm, Vector, ...).
package secrets

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"text/template"

	"tfv/internal/vault"
)

// shortcutRe matches `<vault:PATH#KEY>` and `<vault:PATH#KEY | f1 | f2>`.
// The legacy argocd-vault-plugin prefix `<path:...>` is accepted as an alias.
// Group 1 is the reference; group 2 (optional) is the pipe chain including
// its leading '|'.
var shortcutRe = regexp.MustCompile(`<(?:vault|path):\s*([^>|]+?)\s*(\|\s*[^>]+?)?\s*>`)

// Renderer resolves values using a fixed function map.
type Renderer struct {
	funcs template.FuncMap
}

// NewRenderer builds a renderer backed by the given Vault pool.
func NewRenderer(vp *vault.Pool) *Renderer {
	return &Renderer{funcs: FuncMap(vp)}
}

// renderString replaces every <vault:...> / <path:...> placeholder with its
// resolved value, leaving the rest of the string — including any other
// "{{ ... }}" — exactly as written.
func (r *Renderer) renderString(s string) (string, error) {
	var firstErr error
	out := shortcutRe.ReplaceAllStringFunc(s, func(m string) string {
		if firstErr != nil {
			return m
		}
		sub := shortcutRe.FindStringSubmatch(m)
		ref := strings.TrimSpace(sub[1])
		pipes := strings.TrimSpace(sub[2]) // e.g. "| b64enc" or ""
		val, err := r.evalToken(ref, pipes)
		if err != nil {
			firstErr = err
			return m
		}
		return val
	})
	return out, firstErr
}

// evalToken evaluates a single placeholder as "{{ vault \"ref\" <pipes> }}".
func (r *Renderer) evalToken(ref, pipes string) (string, error) {
	text := fmt.Sprintf(`{{ vault %q }}`, ref)
	if pipes != "" {
		text = fmt.Sprintf(`{{ vault %q %s }}`, ref, pipes)
	}
	t, err := template.New("v").Funcs(r.funcs).Parse(text)
	if err != nil {
		return "", fmt.Errorf("placeholder %q: %w", ref, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, nil); err != nil {
		return "", fmt.Errorf("placeholder %q: %w", ref, err)
	}
	return buf.String(), nil
}

// Resolve recursively renders every string scalar in v, returning the
// resolved structure. Maps and slices are walked in place.
func (r *Renderer) Resolve(v any) (any, error) {
	switch val := v.(type) {
	case map[string]any:
		for k, item := range val {
			nv, err := r.Resolve(item)
			if err != nil {
				return nil, err
			}
			val[k] = nv
		}
		return val, nil
	case []any:
		for i, item := range val {
			nv, err := r.Resolve(item)
			if err != nil {
				return nil, err
			}
			val[i] = nv
		}
		return val, nil
	case string:
		return r.renderString(val)
	default:
		return v, nil
	}
}
