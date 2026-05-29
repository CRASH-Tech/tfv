package secrets

import (
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"tfv/internal/vault"
)

// avpAliases maps argocd-vault-plugin function names to the equivalent
// sprig/Helm function so existing YAML using "| base64encode" keeps working.
// Add new aliases here as needed.
var avpAliases = map[string]string{
	"base64encode": "b64enc",
	"base64decode": "b64dec",
	"sha256":       "sha256sum",
	"sha1":         "sha1sum",
	"sha512":       "sha512sum",
}

// FuncMap returns the template functions available inside YAML values: every
// Helm/sprig function (b64enc, htpasswd, sha256sum, quote, ...), the custom
// "vault" lookup, and the AVP-compatible aliases above.
func FuncMap(vc *vault.Client) template.FuncMap {
	fm := sprig.TxtFuncMap()

	fm["vault"] = func(ref string) (string, error) {
		return vc.Field(ref)
	}

	for alias, target := range avpAliases {
		if f, ok := fm[target]; ok {
			fm[alias] = f
		}
	}

	return fm
}
