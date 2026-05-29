// Package vault reads secrets from HashiCorp Vault, replacing the use of
// argocd-vault-plugin. It understands both KV v1 and KV v2 mounts and caches
// reads so each secret path is fetched at most once per run.
//
// A single reference may target a specific Vault server by prefixing the path
// with the server URL, e.g. "https://vault-a.example/common/data/app#field".
// Without a prefix the default VAULT_ADDR is used. Clients are created lazily
// and cached per address by a Pool, so a run touching several Vault servers
// reuses one connection each.
package vault

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	vaultapi "github.com/hashicorp/vault/api"
)

// Pool lazily creates and caches one Client per Vault address.
type Pool struct {
	defaultAddr string
	mu          sync.Mutex
	clients     map[string]*Client
}

// NewPool builds an empty pool. The default address comes from VAULT_ADDR.
func NewPool() *Pool {
	return &Pool{
		defaultAddr: strings.TrimSpace(os.Getenv("VAULT_ADDR")),
		clients:     map[string]*Client{},
	}
}

// Field resolves a reference of the form "[scheme://host/]path#field",
// selecting the Vault server from the optional address prefix.
func (p *Pool) Field(ref string) (string, error) {
	addr, rest := splitAddr(ref)
	c, err := p.client(addr)
	if err != nil {
		return "", err
	}
	return c.Field(rest)
}

func (p *Pool) client(addr string) (*Client, error) {
	if addr == "" {
		addr = p.defaultAddr
	}
	if addr == "" {
		return nil, fmt.Errorf("no Vault address: set VAULT_ADDR or prefix the reference with one (https://host/...)")
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[addr]; ok {
		return c, nil
	}
	c, err := newClient(addr)
	if err != nil {
		return nil, err
	}
	p.clients[addr] = c
	return c, nil
}

// Client wraps the Vault API client with a per-path read cache.
type Client struct {
	api   *vaultapi.Client
	mu    sync.Mutex
	cache map[string]map[string]any
}

func newClient(addr string) (*Client, error) {
	cfg := vaultapi.DefaultConfig()
	if cfg.Error != nil {
		return nil, cfg.Error
	}
	cfg.Address = addr
	c, err := vaultapi.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	tok := tokenFor(addr)
	if tok == "" {
		return nil, fmt.Errorf("no Vault token for %s: set %s, VAULT_TOKEN, or create ~/.vault-token", addr, tokenEnvName(addr))
	}
	c.SetToken(tok)
	return &Client{api: c, cache: map[string]map[string]any{}}, nil
}

// tokenFor resolves the token for an address: the address-specific
// VAULT_TOKEN_<HOST>, then VAULT_TOKEN, then ~/.vault-token.
func tokenFor(addr string) string {
	if t := os.Getenv(tokenEnvName(addr)); t != "" {
		return t
	}
	if t := os.Getenv("VAULT_TOKEN"); t != "" {
		return t
	}
	if home, err := os.UserHomeDir(); err == nil {
		if b, err := os.ReadFile(filepath.Join(home, ".vault-token")); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

// tokenEnvName maps an address to VAULT_TOKEN_<HOST>: the host is upper-cased
// and every non-alphanumeric character is replaced with '_'. For example
// https://vault-a.uis.dev -> VAULT_TOKEN_VAULT_A_UIS_DEV.
func tokenEnvName(addr string) string {
	host := addr
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	var b strings.Builder
	b.WriteString("VAULT_TOKEN_")
	for _, r := range strings.ToUpper(host) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// splitAddr separates an optional "scheme://host" prefix from the rest of a
// reference. Without a recognized scheme the address is empty (use the default).
func splitAddr(ref string) (addr, rest string) {
	for _, scheme := range []string{"https://", "http://"} {
		if strings.HasPrefix(ref, scheme) {
			after := ref[len(scheme):]
			if i := strings.IndexByte(after, '/'); i >= 0 {
				return scheme + after[:i], after[i+1:]
			}
			return ref, "" // no path; Field will report the missing '#field'
		}
	}
	return "", ref
}

// Read fetches all fields at a Vault path, transparently handling KV v2
// (where the response nests the data under data.data).
func (c *Client) Read(path string) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if v, ok := c.cache[path]; ok {
		return v, nil
	}

	sec, err := c.api.Logical().Read(path)
	if err != nil {
		return nil, fmt.Errorf("vault read %q: %w", path, err)
	}
	if sec == nil || sec.Data == nil {
		return nil, fmt.Errorf("vault: no data at %q", path)
	}

	data := sec.Data
	// KV v2 responses wrap the real fields in data.data with a sibling
	// metadata map. KV v1 responses hold the fields directly.
	if inner, ok := data["data"].(map[string]any); ok {
		if _, hasMeta := data["metadata"]; hasMeta {
			data = inner
		}
	}

	c.cache[path] = data
	return data, nil
}

// Field reads a single field using the "path#field" reference syntax.
func (c *Client) Field(ref string) (string, error) {
	i := strings.LastIndex(ref, "#")
	if i < 0 {
		return "", fmt.Errorf("vault ref %q: expected 'path#field'", ref)
	}
	path := strings.TrimSpace(ref[:i])
	field := strings.TrimSpace(ref[i+1:])

	data, err := c.Read(path)
	if err != nil {
		return "", err
	}
	v, ok := data[field]
	if !ok {
		return "", fmt.Errorf("vault: field %q not found at %q", field, path)
	}
	return fmt.Sprintf("%v", v), nil
}
