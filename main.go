// Command tfv renders environment YAML files (resolving Vault references and
// Helm/sprig template functions) and drives OpenTofu against a git-sourced
// module.
//
// Usage:
//
//	tfv [flags] <pattern>... <action> [-- <extra tofu args>]
//
// Examples:
//
//	tfv dev.yaml plan
//	tfv 'prod-*.yaml' apply
//	tfv --render dev.yaml
//	tfv --no-update dev.yaml plan
//
// Flags:
//
//	--no-update, --offline   use the cached module without git-fetching
//	--render, --debug        print resolved vars and exit (no tofu)
//	--format json|yaml       output format for --render (default json)
//	--env-file PATH          load environment variables from a dotenv file first
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"tfv/internal/config"
	"tfv/internal/secrets"
	"tfv/internal/source"
	"tfv/internal/tofu"
	"tfv/internal/vault"

	"gopkg.in/yaml.v3"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

type options struct {
	noUpdate bool
	render   bool
	help     bool
	version  bool
	format   string
	envFile  string
	patterns []string
	action   string
	extra    []string // passthrough args after "--"
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "tfv: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		return err
	}
	if opts.help {
		fmt.Print(helpText)
		return nil
	}
	if opts.version {
		fmt.Println("tfv " + version)
		return nil
	}

	if opts.envFile != "" {
		if err := loadEnvFile(opts.envFile); err != nil {
			return fmt.Errorf("env-file: %w", err)
		}
	}

	envFiles, err := expandPatterns(opts.patterns)
	if err != nil {
		return err
	}
	if len(envFiles) == 0 {
		return fmt.Errorf("no environment files matched: %s", strings.Join(opts.patterns, " "))
	}

	vc, err := vault.New()
	if err != nil {
		return err
	}
	renderer := secrets.NewRenderer(vc)

	root, err := os.Getwd()
	if err != nil {
		return err
	}

	for _, file := range envFiles {
		if err := process(root, file, opts, renderer); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
	}
	return nil
}

func process(root, file string, opts options, renderer *secrets.Renderer) error {
	env, err := config.Load(file)
	if err != nil {
		return err
	}

	resolved, err := renderer.Resolve(env.Vars)
	if err != nil {
		return err
	}
	vars := resolved.(map[string]any)

	if opts.render {
		return printVars(env.Name, vars, opts.format)
	}

	fmt.Fprintf(os.Stderr, ">>> %s on %s\n", opts.action, env.Name)

	workDir, err := source.Prepare(root, env.ModuleSource, env.ModuleRef, !opts.noUpdate)
	if err != nil {
		return err
	}

	varFile, err := tofu.WriteVars(vars)
	if err != nil {
		return err
	}
	defer os.Remove(varFile)

	// Restore the committed lock for this environment, generating a fresh
	// cross-platform one the first time it is seen.
	bin := tofu.Binary(env.TofuBin)
	lockKey := lockKey(vars, env.Name)
	if err := tofu.RestoreLock(root, lockKey, workDir); err != nil {
		return fmt.Errorf("restore lock: %w", err)
	}
	if !tofu.HasCommittedLock(root, lockKey) {
		if err := tofu.Lock(bin, workDir); err != nil {
			return fmt.Errorf("providers lock: %w", err)
		}
	}
	if err := tofu.Init(bin, workDir, varFile, !opts.noUpdate); err != nil {
		return fmt.Errorf("init: %w", err)
	}
	if err := tofu.SaveLock(root, lockKey, workDir); err != nil {
		return fmt.Errorf("save lock: %w", err)
	}

	extra := opts.extra
	if opts.action == "apply" && !hasParallelism(extra) {
		extra = append(slices.Clone(extra), "-parallelism=1")
	}
	if err := tofu.Action(bin, workDir, opts.action, varFile, extra); err != nil {
		return fmt.Errorf("%s: %w", opts.action, err)
	}
	return nil
}

func printVars(name string, vars map[string]any, format string) error {
	fmt.Printf("# %s\n", name)
	switch format {
	case "yaml", "yml":
		out, err := yaml.Marshal(vars)
		if err != nil {
			return err
		}
		os.Stdout.Write(out)
	default: // json
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(vars); err != nil {
			return err
		}
	}
	return nil
}

// lockKey builds the committed-lock key from the same identity the module's
// backend uses for the state file: tenant-env-region-instance. Falls back to
// the env file name if those variables are absent.
func lockKey(vars map[string]any, fallback string) string {
	parts := make([]string, 0, 4)
	for _, k := range []string{"tenant", "env", "region", "instance"} {
		v, ok := vars[k]
		if !ok {
			return fallback
		}
		parts = append(parts, fmt.Sprintf("%v", v))
	}
	return strings.Join(parts, "-")
}

func hasParallelism(args []string) bool {
	for _, a := range args {
		if a == "-parallelism" || strings.HasPrefix(a, "-parallelism=") {
			return true
		}
	}
	return false
}

// parseArgs splits flags, positional patterns/action, and passthrough args.
func parseArgs(args []string) (options, error) {
	opts := options{format: "json"}

	// Everything after a standalone "--" goes straight to tofu.
	if i := slices.Index(args, "--"); i >= 0 {
		opts.extra = args[i+1:]
		args = args[:i]
	}

	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--no-update" || a == "--offline":
			opts.noUpdate = true
		case a == "--render" || a == "--debug":
			opts.render = true
		case a == "--format":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("--format requires a value")
			}
			opts.format = args[i]
		case strings.HasPrefix(a, "--format="):
			opts.format = strings.TrimPrefix(a, "--format=")
		case a == "--env-file":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("--env-file requires a value")
			}
			opts.envFile = args[i]
		case strings.HasPrefix(a, "--env-file="):
			opts.envFile = strings.TrimPrefix(a, "--env-file=")
		case a == "-h" || a == "--help":
			opts.help = true
			return opts, nil
		case a == "--version" || a == "-V":
			opts.version = true
			return opts, nil
		default:
			positional = append(positional, a)
		}
	}

	// --render only resolves and prints variables, so it takes no action.
	if opts.render {
		if len(positional) < 1 {
			return opts, fmt.Errorf("usage: tfv --render [flags] <pattern>...")
		}
		opts.patterns = positional
		return opts, nil
	}

	if len(positional) < 2 {
		return opts, fmt.Errorf("usage: tfv [flags] <pattern>... <action> [-- <tofu args>]")
	}
	opts.action = positional[len(positional)-1]
	opts.patterns = positional[:len(positional)-1]
	return opts, nil
}

// expandPatterns expands globs/paths into a sorted, deduplicated list of files.
func expandPatterns(patterns []string) ([]string, error) {
	seen := map[string]bool{}
	var files []string
	for _, p := range patterns {
		matches, err := filepath.Glob(p)
		if err != nil {
			return nil, fmt.Errorf("bad pattern %q: %w", p, err)
		}
		if len(matches) == 0 {
			if _, err := os.Stat(p); err == nil {
				matches = []string{p}
			} else {
				return nil, fmt.Errorf("no files match %q", p)
			}
		}
		for _, m := range matches {
			if fi, err := os.Stat(m); err != nil || fi.IsDir() {
				continue
			}
			if !seen[m] {
				seen[m] = true
				files = append(files, m)
			}
		}
	}
	slices.Sort(files)
	return files, nil
}

const helpText = `tfv — render environment YAML (Vault + Helm/sprig templating) and drive OpenTofu.

USAGE:
    tfv [flags] <pattern>... <action> [-- <tofu args>]

    <pattern>   one or more env-file paths or globs. Quote globs so the shell
                does not expand them first, e.g. 'prod-*.yaml'.
    <action>    final positional: any OpenTofu subcommand (plan, apply,
                destroy, init, ...).
    -- ...      everything after "--" is passed straight to the tofu action.

FLAGS:
    --no-update, --offline   use the cached module without "git fetch"
                             (errors if that version is not cached yet)
    --render, --debug        resolve variables and print them, then exit
                             (does not run OpenTofu)
    --format json|yaml       output format for --render (default: json)
    --env-file PATH          load environment variables from a dotenv file
                             before reading credentials
    -h, --help               show this help
    -V, --version            print the tfv version

CREDENTIALS (from the environment, optionally loaded via --env-file):
    VAULT_ADDR               Vault address, e.g. https://vault.example
    VAULT_TOKEN              Vault token (falls back to ~/.vault-token)
    TF_VAR_encryption_key    passphrase for the encrypted local state backend
    TFV_TOFU_BIN             OpenTofu binary when "tofu_bin" is not set in the
                             env file (default: "tofu")

ENV FILE:
    module_source is "<git-url>#<ref>" — the module to deploy and its branch or
    tag. It is removed before the remaining keys are handed to OpenTofu as
    tfvars. The module is cloned per (url, ref) under .tfv/cache, so different
    branches/tags coexist. Lock files are committed under locks/.

    tofu_bin (optional) overrides the OpenTofu binary for this environment;
    when omitted, "tofu" from PATH is used. It is also stripped from the tfvars.

        module_source: https://git.example/modules/app.git#master
        tofu_bin: tofu          # optional, e.g. a pinned "tofu-1.11.1"
        tenant: acme
        env: dev
        region: eu-1
        instance: app.example

SECRET / TEMPLATE SYNTAX:
    Any string value may pull secrets from Vault and transform them. Two
    equivalent forms are supported:

      shortcut    "<vault:PATH#FIELD>"   or   "<vault:PATH#FIELD | fn | fn>"
                  (the legacy "<path:...>" prefix is also accepted)
      template    "{{ vault \"PATH#FIELD\" | fn }}"   — a full Go text/template,
                  needed for functions taking more than one argument

    PATH is the Vault path (KV v2 paths include "/data/"); FIELD is the key.

FUNCTIONS:
    vault "PATH#FIELD"   read a secret field from Vault

    Every Helm/sprig function is available. Frequently used:
      b64enc / b64dec    base64 encode / decode
      sha256sum          sha-256 hex digest
      htpasswd u p       Apache "user:bcrypt-hash" line
      upper / lower      change case
      trim / quote       trim whitespace / wrap in quotes
      randAlphaNum n     random alphanumeric string of length n
    AVP-compatible aliases: base64encode, base64decode, sha256, sha1, sha512.
    Full reference: https://masterminds.github.io/sprig/
    Add project-specific aliases in internal/secrets/funcs.go.

EXAMPLES:
    # show resolved variables as JSON, without running tofu
    tfv --render dev.yaml

    # same, as YAML
    tfv --render --format yaml dev.yaml

    # plan a single environment
    tfv dev.yaml plan

    # apply every matching environment (glob expanded by tfv)
    tfv 'prod-*.yaml' apply

    # apply non-interactively, passing a flag through to tofu
    tfv dev.yaml apply -- -auto-approve

    # reuse the already-downloaded module, skip git fetch
    tfv --no-update dev.yaml plan

    # destroy, loading credentials from a dotenv file
    tfv --env-file .env dev.yaml destroy

    Secret examples inside an env YAML:
      client_id:  "<vault:common/data/oauth/app#client_id>"
      client_b64: "<vault:common/data/oauth/app#client_id | b64enc>"
      htpasswd:   '{{ htpasswd "admin" (vault "common/data/app#password") }}'
`

// loadEnvFile reads a dotenv-style file (KEY=VALUE, optional "export", #comments)
// and sets the variables in the process environment.
func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		if err := os.Setenv(key, val); err != nil {
			return err
		}
	}
	return sc.Err()
}
