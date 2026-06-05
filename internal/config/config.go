// Package config loads an environment YAML file, separating tool metadata
// (the git module source) from the variables that are handed to OpenTofu.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Env is a single parsed environment definition.
type Env struct {
	Path         string         // original file path
	Name         string         // base name without extension, used for log headers
	ModuleSource string         // git URL of the OpenTofu module
	ModuleRef    string         // git branch or tag
	TofuBin      string         // optional OpenTofu binary override ("" = default)
	Vars         map[string]any // everything else, passed to OpenTofu as tfvars
}

// Load reads and parses an environment YAML file.
func Load(path string) (*Env, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	doc, err := nodeToValue(&root)
	if err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	m, ok := doc.(map[string]any)
	if !ok {
		if doc == nil {
			m = map[string]any{}
		} else {
			return nil, fmt.Errorf("top-level YAML must be a mapping")
		}
	}

	// module_source carries both the git URL and the ref as "url#ref".
	src, _ := m["module_source"].(string)
	src = strings.TrimSpace(src)
	if src == "" {
		return nil, fmt.Errorf("missing required key 'module_source'")
	}
	delete(m, "module_source") // tool metadata, not an OpenTofu variable

	i := strings.LastIndex(src, "#")
	if i < 0 {
		return nil, fmt.Errorf("module_source must be 'url#ref' (e.g. https://git.example/repo.git#master), got %q", src)
	}
	url := strings.TrimSpace(src[:i])
	ref := strings.TrimSpace(src[i+1:])
	if url == "" || ref == "" {
		return nil, fmt.Errorf("module_source must be 'url#ref', got %q", src)
	}

	// Optional OpenTofu binary override; also tool metadata, not a tfvar.
	tofuBin, _ := m["tofu_bin"].(string)
	tofuBin = strings.TrimSpace(tofuBin)
	delete(m, "tofu_bin")

	base := filepath.Base(path)
	name := strings.TrimSuffix(base, filepath.Ext(base))

	return &Env{
		Path:         path,
		Name:         name,
		ModuleSource: url,
		ModuleRef:    ref,
		TofuBin:      tofuBin,
		Vars:         m,
	}, nil
}

// nodeToValue converts a parsed YAML node tree into plain Go values. Crucially,
// scalars keep their original source text (only integers, floats, booleans and
// null are converted to native types), so values such as dates are preserved
// exactly as written instead of being rewritten — e.g. "2024-04-01" stays the
// string "2024-04-01" rather than becoming a timestamp. Mapping keys are always
// taken verbatim as strings, which also handles numeric keys like "1:".
func nodeToValue(n *yaml.Node) (any, error) {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return nodeToValue(n.Content[0])
	case yaml.AliasNode:
		return nodeToValue(n.Alias)
	case yaml.MappingNode:
		m := map[string]any{}
		if err := decodeMapping(n, m); err != nil {
			return nil, err
		}
		return m, nil
	case yaml.SequenceNode:
		arr := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := nodeToValue(c)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		return arr, nil
	case yaml.ScalarNode:
		return scalarValue(n), nil
	default:
		return nil, fmt.Errorf("unsupported YAML node")
	}
}

// decodeMapping fills m from a mapping node. Explicit keys are set first; merge
// keys ("<<") then provide values for any keys not already present.
func decodeMapping(n *yaml.Node, m map[string]any) error {
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if isMerge(k) {
			continue
		}
		val, err := nodeToValue(v)
		if err != nil {
			return err
		}
		m[keyString(k)] = val
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if !isMerge(k) {
			continue
		}
		if err := applyMerge(v, m); err != nil {
			return err
		}
	}
	return nil
}

// applyMerge adds keys from a merge value (a mapping, an alias to one, or a
// sequence of them) without overriding keys already present.
func applyMerge(v *yaml.Node, m map[string]any) error {
	switch v.Kind {
	case yaml.AliasNode:
		return applyMerge(v.Alias, m)
	case yaml.SequenceNode:
		for _, c := range v.Content {
			if err := applyMerge(c, m); err != nil {
				return err
			}
		}
		return nil
	case yaml.MappingNode:
		merged := map[string]any{}
		if err := decodeMapping(v, merged); err != nil {
			return err
		}
		for k, val := range merged {
			if _, ok := m[k]; !ok {
				m[k] = val
			}
		}
		return nil
	default:
		return fmt.Errorf("invalid merge value")
	}
}

func isMerge(k *yaml.Node) bool {
	return k.Tag == "!!merge" || (k.Kind == yaml.ScalarNode && k.Value == "<<")
}

func keyString(k *yaml.Node) string {
	if k.Kind == yaml.ScalarNode {
		return k.Value
	}
	v, _ := nodeToValue(k)
	return fmt.Sprint(v)
}

// scalarValue converts a scalar node, keeping the literal text for everything
// except the unambiguous int/float/bool/null types.
func scalarValue(n *yaml.Node) any {
	switch n.ShortTag() {
	case "!!null":
		return nil
	case "!!bool":
		var b bool
		if n.Decode(&b) == nil {
			return b
		}
	case "!!int":
		var i int64
		if n.Decode(&i) == nil {
			return i
		}
		var u uint64
		if n.Decode(&u) == nil {
			return u
		}
	case "!!float":
		var f float64
		if n.Decode(&f) == nil {
			return f
		}
	}
	// !!str, !!timestamp and anything else: keep the source text verbatim.
	return n.Value
}
