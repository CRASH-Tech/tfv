// Package config loads an environment YAML file, separating tool metadata
// (the git module source) from the variables that are handed to OpenTofu.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
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
	Anchors      map[string]any // YAML anchor values, usable as "{{ $name }}"
}

// Load reads and parses an environment YAML file, applying any "include" files
// it references (see loadMerged).
func Load(path string) (*Env, error) {
	collected := map[string]*yaml.Node{}
	m, err := loadMerged(path, map[string]bool{}, nil, collected, nil, false)
	if err != nil {
		return nil, err
	}
	unwrapOps(m) // resolve any leftover !append/!prepend markers

	// Anchor values, exposed to templating as "{{ $name }}".
	anchors := make(map[string]any, len(collected))
	for name, node := range collected {
		if v, err := nodeToValue(node); err == nil {
			anchors[name] = unwrapOps(v)
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
		Anchors:      anchors,
	}, nil
}

// loadMerged parses the file at path and applies its "include" list, returning
// the merged variables. Includes are merged in the order listed (a later file
// overrides an earlier one), and the file's own keys override its includes —
// so an included "default.yaml" provides defaults that the env file refines,
// like passing several -f files to Helm. Include paths are relative to the
// file that lists them; includes may nest, and cycles are rejected.
//
// anchors holds the YAML anchors defined by the ancestor files (the file that
// included this one, and its ancestors). They are made available when parsing
// this file, so an anchor defined in an env file can be used by the files it
// includes.
//
// tmpl marks a file that should be rendered as a Go template before being
// parsed as YAML (sprig functions, with any passed values and the inherited
// anchors bound as $variables) — supporting loops, conditionals, etc., like a
// Helm template. Every included file is a template; the root env file is not.
// values are the optional template parameters passed to this file by its
// includer.
func loadMerged(path string, seen map[string]bool, anchors, collected map[string]*yaml.Node, values map[string]any, tmpl bool) (map[string]any, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if seen[abs] {
		return nil, fmt.Errorf("include cycle through %s", path)
	}
	seen[abs] = true
	defer delete(seen, abs)

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if tmpl {
		ctx := anchorValues(anchors)
		for k, v := range values {
			ctx[k] = v
		}
		raw, err = renderTemplate(raw, ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
	}

	m, fileAnchors, err := parseBytes(raw, anchors)
	if err != nil {
		return nil, err
	}

	// Accumulate anchors globally; the root file is processed first, so its
	// anchors win on a name clash.
	for name, node := range fileAnchors {
		if _, ok := collected[name]; !ok {
			collected[name] = node
		}
	}

	includes, err := parseIncludes(m["include"])
	if err != nil {
		return nil, err
	}
	delete(m, "include") // tool metadata, not an OpenTofu variable

	merged := map[string]any{}
	dir := filepath.Dir(path)
	for _, inc := range includes {
		incPath := inc.file
		if !filepath.IsAbs(incPath) {
			incPath = filepath.Join(dir, inc.file)
		}
		// Resolve template expressions in the parameters using this file's
		// context, so a value like "{{ $base_domain }}-dash" is evaluated here
		// before being passed down.
		vals, err := renderValues(inc.values, anchorValues(fileAnchors))
		if err != nil {
			return nil, fmt.Errorf("include %q values: %w", inc.file, err)
		}

		// Anchors defined here (and by our ancestors) are visible to includes,
		// and every included file is rendered as a template.
		sub, err := loadMerged(incPath, seen, fileAnchors, collected, vals, true)
		if err != nil {
			return nil, fmt.Errorf("include %q: %w", inc.file, err)
		}
		deepMerge(merged, sub)
	}
	deepMerge(merged, m)
	return merged, nil
}

// includeItem is one parsed "include" entry: a file and optional template
// parameters.
type includeItem struct {
	file   string
	values map[string]any
}

// parseIncludes accepts the "include" value, which may be a single string or a
// list whose entries are either a filename string or a map of the form
// {file: NAME, values: {...}} (the filename may also be given as a bare key).
func parseIncludes(v any) ([]includeItem, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		return []includeItem{{file: x}}, nil
	case []any:
		var out []includeItem
		for _, it := range x {
			switch e := it.(type) {
			case string:
				out = append(out, includeItem{file: e})
			case map[string]any:
				item, err := includeFromMap(e)
				if err != nil {
					return nil, err
				}
				out = append(out, item)
			default:
				return nil, fmt.Errorf("invalid include entry: %T", it)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("'include' must be a string or a list")
	}
}

func includeFromMap(e map[string]any) (includeItem, error) {
	var item includeItem
	item.values, _ = e["values"].(map[string]any)
	if f, ok := e["file"].(string); ok {
		item.file = f
		return item, nil
	}
	for k, val := range e {
		if k == "values" || k == "file" {
			continue
		}
		item.file = k
		// Also support the nested form {NAME: {values: {...}}}.
		if item.values == nil {
			if vm, ok := val.(map[string]any); ok {
				item.values, _ = vm["values"].(map[string]any)
			}
		}
		return item, nil
	}
	return item, fmt.Errorf("include entry has no file name")
}

// anchorsKey is the synthetic top-level key under which ancestor anchor
// definitions are injected before parsing an included file. It is stripped from
// the result.
const anchorsKey = "__tfv_anchors__"

// templateFuncs is the function set available when rendering a parameterized
// include: all of sprig (Helm) plus toYaml (which sprig lacks) and lowercase
// aliases for the JSON/YAML helpers.
func templateFuncs() template.FuncMap {
	fm := sprig.TxtFuncMap()
	toYAML := func(v any) string {
		b, err := yaml.Marshal(v)
		if err != nil {
			return ""
		}
		return strings.TrimRight(string(b), "\n")
	}
	fm["toYaml"] = toYAML
	fm["toyaml"] = toYAML
	if f, ok := fm["toJson"]; ok {
		fm["tojson"] = f
	}
	return fm
}

var (
	// refVarRe finds every $name variable reference.
	refVarRe = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)
	// declVarRe finds $name (and "$a, $name") declared with ":=", e.g. by range.
	declVarRe = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)(?:\s*,\s*\$([A-Za-z_][A-Za-z0-9_]*))?\s*:=`)
)

// renderTemplate renders raw as a Go template so the file can use {{ $name }},
// {{ range $x := $list }}, {{ if ... }}, etc. Every $name referenced in the
// file is bound to its value from ctx (the passed values and inherited anchors),
// or to nil when absent — so "{{ $x | default ... }}" works for an unset
// variable instead of failing as "undefined". Variables the template declares
// itself (range/with/assignment) are left alone.
func renderTemplate(raw []byte, ctx map[string]any) ([]byte, error) {
	src := string(raw)

	declared := map[string]bool{}
	for _, m := range declVarRe.FindAllStringSubmatch(src, -1) {
		declared[m[1]] = true
		if m[2] != "" {
			declared[m[2]] = true
		}
	}

	var b strings.Builder
	seen := map[string]bool{}
	for _, m := range refVarRe.FindAllStringSubmatch(src, -1) {
		name := m[1]
		if seen[name] || declared[name] {
			continue
		}
		seen[name] = true
		fmt.Fprintf(&b, "{{- $%s := index $ %q -}}", name, name)
	}
	b.WriteString(src)

	t, err := template.New("include").Funcs(templateFuncs()).Parse(b.String())
	if err != nil {
		return nil, fmt.Errorf("template parse: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, ctx); err != nil {
		return nil, fmt.Errorf("template execute: %w", err)
	}
	return buf.Bytes(), nil
}

// renderValues evaluates template expressions in include parameters using ctx
// (the including file's anchors), so values written in a non-templated env file
// — like "{{ $base_domain }}-dash" — are resolved before being passed to the
// included template.
func renderValues(values map[string]any, ctx map[string]any) (map[string]any, error) {
	if len(values) == 0 {
		return values, nil
	}
	raw, err := yaml.Marshal(values)
	if err != nil {
		return nil, err
	}
	rendered, err := renderTemplate(raw, ctx)
	if err != nil {
		return nil, err
	}
	m, _, err := parseBytes(rendered, nil)
	return m, err
}

// anchorValues converts anchor nodes to plain values, for use as template
// variables.
func anchorValues(nodes map[string]*yaml.Node) map[string]any {
	out := make(map[string]any, len(nodes))
	for name, n := range nodes {
		if v, err := nodeToValue(n); err == nil {
			out[name] = unwrapOps(v)
		}
	}
	return out
}

// parseBytes parses YAML bytes into a map. Any anchors passed in are injected so
// the content may reference them; the returned anchor map holds every anchor
// visible to this file (the injected ones plus those it defines), for passing on
// to the files it includes.
func parseBytes(raw []byte, anchors map[string]*yaml.Node) (map[string]any, map[string]*yaml.Node, error) {
	// Inject ancestor anchors as a prelude so the content may reference them. The
	// content is appended verbatim (not re-indented), so input without ancestor
	// anchors is parsed exactly as written.
	text := raw
	if len(anchors) > 0 {
		text = append(buildAnchorPrelude(anchors), raw...)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(text, &root); err != nil {
		if strings.Contains(err.Error(), "unknown anchor") {
			return nil, nil, fmt.Errorf("%w — define the anchor (&name) in this file or in a file that includes it", err)
		}
		return nil, nil, fmt.Errorf("parse yaml: %w", err)
	}

	visible := collectAnchors(&root)

	doc, err := nodeToValue(&root)
	if err != nil {
		return nil, nil, fmt.Errorf("parse yaml: %w", err)
	}

	var m map[string]any
	switch v := doc.(type) {
	case nil:
		m = map[string]any{}
	case map[string]any:
		m = v
	default:
		return nil, nil, fmt.Errorf("top-level YAML must be a mapping")
	}
	delete(m, anchorsKey) // drop the injected prelude
	return m, visible, nil
}

// buildAnchorPrelude renders the given anchors as an indented YAML block under
// anchorsKey, so that parsing "<prelude>\n<file>" makes the anchors available
// to the file's aliases.
func buildAnchorPrelude(anchors map[string]*yaml.Node) []byte {
	body := &yaml.Node{Kind: yaml.MappingNode}
	i := 0
	for _, node := range anchors {
		body.Content = append(body.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprintf("a%d", i)},
			node)
		i++
	}
	out, err := yaml.Marshal(body)
	if err != nil {
		return nil
	}
	var b strings.Builder
	b.WriteString(anchorsKey)
	b.WriteString(":\n")
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// collectAnchors returns every anchor defined anywhere in the node tree.
func collectAnchors(n *yaml.Node) map[string]*yaml.Node {
	res := map[string]*yaml.Node{}
	var walk func(*yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil {
			return
		}
		if n.Anchor != "" {
			res[n.Anchor] = n
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(n)
	return res
}

// listOp is a list value tagged with a merge operation (!append / !prepend /
// !replace). It is produced by nodeToValue and resolved by deepMerge.
type listOp struct {
	mode  string // "append", "prepend" or "replace"
	items []any
}

// deepMerge merges src into dst: nested maps are merged recursively; a list
// tagged !append/!prepend is combined with the base list, otherwise lists and
// scalars from src replace those in dst (src wins).
func deepMerge(dst, src map[string]any) {
	for k, sv := range src {
		if op, ok := sv.(listOp); ok {
			dst[k] = applyListOp(dst[k], op)
			continue
		}
		if dv, ok := dst[k]; ok {
			if dm, ok := dv.(map[string]any); ok {
				if sm, ok := sv.(map[string]any); ok {
					deepMerge(dm, sm)
					continue
				}
			}
		}
		dst[k] = sv
	}
}

// applyListOp combines a tagged list with the base value already present.
func applyListOp(base any, op listOp) any {
	baseList, _ := base.([]any) // nil if absent or not a list
	switch op.mode {
	case "append":
		return append(append([]any{}, baseList...), op.items...)
	case "prepend":
		return append(append([]any{}, op.items...), baseList...)
	default: // replace
		return op.items
	}
}

// unwrapOps removes any leftover listOp markers (e.g. one used where there was
// no base list, or nested inside a list), replacing each with its plain items.
func unwrapOps(v any) any {
	switch x := v.(type) {
	case listOp:
		items := make([]any, len(x.items))
		for i, it := range x.items {
			items[i] = unwrapOps(it)
		}
		return items
	case map[string]any:
		for k, val := range x {
			x[k] = unwrapOps(val)
		}
		return x
	case []any:
		for i, val := range x {
			x[i] = unwrapOps(val)
		}
		return x
	default:
		return v
	}
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
		switch n.Tag {
		case "!append":
			return listOp{mode: "append", items: arr}, nil
		case "!prepend":
			return listOp{mode: "prepend", items: arr}, nil
		case "!replace":
			return listOp{mode: "replace", items: arr}, nil
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
