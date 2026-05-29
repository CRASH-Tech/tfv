package tofu

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	varDeclRe   = regexp.MustCompile(`variable\s+"([^"]+)"\s*\{`)
	hasDefaultRe = regexp.MustCompile(`(?m)^\s*default\s*=`)
)

// RequiredVars scans the module's .tf files and returns the names of variables
// declared without a default value — the ones OpenTofu requires a value for.
// It is a best-effort parse: anything it misses simply falls back to OpenTofu's
// own handling.
func RequiredVars(workDir string) ([]string, error) {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return nil, err
	}
	var required []string
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tf") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(workDir, e.Name()))
		if err != nil {
			return nil, err
		}
		for _, name := range scanRequired(string(b)) {
			if !seen[name] {
				seen[name] = true
				required = append(required, name)
			}
		}
	}
	return required, nil
}

func scanRequired(src string) []string {
	var names []string
	for _, m := range varDeclRe.FindAllStringSubmatchIndex(src, -1) {
		name := src[m[2]:m[3]]
		open := m[1] - 1 // index of the '{' captured at the end of the match
		body, ok := braceBody(src, open)
		if !ok {
			continue
		}
		if !hasDefaultRe.MatchString(body) {
			names = append(names, name)
		}
	}
	return names
}

// braceBody returns the text between the brace at index open and its match.
func braceBody(s string, open int) (string, bool) {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[open+1 : i], true
			}
		}
	}
	return "", false
}
