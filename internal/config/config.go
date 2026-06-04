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

	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	// YAML mappings with non-string keys (e.g. numeric keys like "1:") decode as
	// map[any]any, which neither encoding/json nor the secret resolver handle.
	// Normalize every map to map[string]any with string keys — matching what
	// tofu expects for map(object(...)) variables.
	m, ok := normalize(doc).(map[string]any)
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

// normalize recursively converts every map to map[string]any with string keys
// (stringifying non-string keys), so the result marshals cleanly to JSON and is
// walked by the secret resolver.
func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			x[k] = normalize(val)
		}
		return x
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			m[fmt.Sprint(k)] = normalize(val)
		}
		return m
	case []any:
		for i, val := range x {
			x[i] = normalize(val)
		}
		return x
	default:
		return v
	}
}
