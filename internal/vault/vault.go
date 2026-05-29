// Package vault reads secrets from HashiCorp Vault, replacing the use of
// argocd-vault-plugin. It understands both KV v1 and KV v2 mounts and caches
// reads so each secret path is fetched at most once per run.
package vault

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	vaultapi "github.com/hashicorp/vault/api"
)

// Client wraps the Vault API client with a per-path read cache.
type Client struct {
	api   *vaultapi.Client
	mu    sync.Mutex
	cache map[string]map[string]any
}

// New builds a client from the standard Vault environment variables
// (VAULT_ADDR, VAULT_TOKEN, VAULT_SKIP_VERIFY, ...). If VAULT_TOKEN is not
// set it falls back to ~/.vault-token.
func New() (*Client, error) {
	cfg := vaultapi.DefaultConfig()
	if cfg.Error != nil {
		return nil, cfg.Error
	}
	c, err := vaultapi.NewClient(cfg)
	if err != nil {
		return nil, err
	}

	if c.Token() == "" {
		if tok := os.Getenv("VAULT_TOKEN"); tok != "" {
			c.SetToken(tok)
		}
	}
	if c.Token() == "" {
		if home, err := os.UserHomeDir(); err == nil {
			if b, err := os.ReadFile(filepath.Join(home, ".vault-token")); err == nil {
				c.SetToken(strings.TrimSpace(string(b)))
			}
		}
	}
	if c.Token() == "" {
		return nil, fmt.Errorf("no vault token found: set VAULT_TOKEN or create ~/.vault-token")
	}

	return &Client{api: c, cache: map[string]map[string]any{}}, nil
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
