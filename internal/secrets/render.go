// Package secrets resolves Vault references inside a parsed YAML structure.
//
// Two equivalent forms are supported for every string value:
//
//	shortcut:     "<vault:common/data/oauth/app#client_id | b64enc>"
//	Go template:  "{{ htpasswd \"admin\" (vault \"common/data/app#pw\") }}"
//
// The shortcut is rewritten into the template form, then the whole string is
// executed as a text/template with the sprig + vault function map.
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

// rewriteShortcuts turns shortcut tokens into Go template expressions.
func rewriteShortcuts(s string) string {
	return shortcutRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := shortcutRe.FindStringSubmatch(m)
		ref := strings.TrimSpace(sub[1])
		pipes := strings.TrimSpace(sub[2]) // e.g. "| b64enc" or ""
		if pipes != "" {
			return fmt.Sprintf(`{{ vault %q %s }}`, ref, pipes)
		}
		return fmt.Sprintf(`{{ vault %q }}`, ref)
	})
}

func (r *Renderer) renderString(s string) (string, error) {
	s = rewriteShortcuts(s)
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	t, err := template.New("value").Funcs(r.funcs).Parse(s)
	if err != nil {
		return "", fmt.Errorf("template parse %q: %w", s, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, nil); err != nil {
		return "", fmt.Errorf("template execute %q: %w", s, err)
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
