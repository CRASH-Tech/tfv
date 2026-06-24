// Command tfv renders environment YAML files (resolving Vault references and
// Helm/sprig template functions) and drives OpenTofu against a git-sourced
// module.
//
// Usage:
//
//	tfv [flags] <pattern>... <command> [command args]
//
// Examples:
//
//	tfv dev.yaml plan
//	tfv dev.yaml apply -auto-approve
//	tfv dev.yaml state list
//	tfv 'prod-*.yaml' apply
//	tfv --parallel 4 --keep-going 'envs/*.yaml' plan
//	tfv --render dev.yaml
//
// Run "tfv --help" for the full reference.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"tfv/internal/config"
	"tfv/internal/secrets"
	"tfv/internal/source"
	"tfv/internal/tofu"
	"tfv/internal/vault"

	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

type options struct {
	noUpdate  bool
	render    bool
	help      bool
	version   bool
	keepGoing bool
	parallel  int
	format    string
	envFile   string
	patterns  []string
	action    string
	extra     []string // passthrough args after "--"
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

	renderer := secrets.NewRenderer(vault.NewPool())

	root, err := os.Getwd()
	if err != nil {
		return err
	}

	// Render-only mode never touches OpenTofu; just print each environment.
	if opts.render {
		for _, file := range envFiles {
			env, err := config.Load(file)
			if err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			resolved, err := renderer.Resolve(env.Vars, env.Anchors)
			if err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			if err := printVars(env.Name, resolved.(map[string]any), opts.format); err != nil {
				return err
			}
		}
		return nil
	}

	// Share one provider plugin cache across every module checkout so providers
	// are downloaded once, not per cache directory.
	if os.Getenv("TF_PLUGIN_CACHE_DIR") == "" {
		dir := filepath.Join(root, ".tfv", "plugin-cache")
		if err := os.MkdirAll(dir, 0o755); err == nil {
			os.Setenv("TF_PLUGIN_CACHE_DIR", dir)
		}
	}

	return runEnvs(root, envFiles, opts, renderer)
}

// runEnvs runs the OpenTofu command for every environment, optionally in
// parallel, and prints a summary. Without --keep-going a sequential run stops
// at the first failure; a parallel run always lets in-flight work finish.
func runEnvs(root string, files []string, opts options, renderer *secrets.Renderer) error {
	workers := opts.parallel
	if workers < 1 {
		workers = 1
	}

	// Serialize work that shares a module cache directory: concurrent init in
	// the same directory would corrupt it. Different modules run in parallel.
	var locksMu sync.Mutex
	locks := map[string]*sync.Mutex{}
	dirLock := func(dir string) *sync.Mutex {
		locksMu.Lock()
		defer locksMu.Unlock()
		m := locks[dir]
		if m == nil {
			m = &sync.Mutex{}
			locks[dir] = m
		}
		return m
	}

	var (
		failedMu sync.Mutex
		failed   []string
	)
	record := func(file string, err error) {
		failedMu.Lock()
		failed = append(failed, file)
		failedMu.Unlock()
		fmt.Fprintf(os.Stderr, "tfv: %s: %v\n", file, err)
	}

	if workers == 1 {
		for _, file := range files {
			if tofu.Interrupted() {
				break
			}
			// Interactive prompting is only meaningful when running serially.
			if err := processEnv(root, file, opts, renderer, tofu.DefaultIO(), true, dirLock); err != nil {
				record(file, err)
				if !opts.keepGoing {
					break
				}
			}
		}
	} else {
		var wg sync.WaitGroup
		var printMu sync.Mutex
		sem := make(chan struct{}, workers)
		for _, file := range files {
			if tofu.Interrupted() {
				break
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(file string) {
				defer wg.Done()
				defer func() { <-sem }()
				if tofu.Interrupted() {
					return
				}
				var buf bytes.Buffer
				io := tofu.IO{Stdin: nil, Stdout: &buf, Stderr: &buf}
				err := processEnv(root, file, opts, renderer, io, false, dirLock)
				printMu.Lock()
				os.Stderr.Write(buf.Bytes())
				printMu.Unlock()
				if err != nil {
					record(file, err)
				}
			}(file)
		}
		wg.Wait()
	}

	if len(failed) > 0 {
		return fmt.Errorf("%d/%d environments failed: %s", len(failed), len(files), strings.Join(failed, ", "))
	}
	if len(files) > 1 {
		fmt.Fprintf(os.Stderr, ">>> all %d environments succeeded\n", len(files))
	}
	return nil
}

func processEnv(root, file string, opts options, renderer *secrets.Renderer, io tofu.IO, interactive bool, dirLock func(string) *sync.Mutex) error {
	env, err := config.Load(file)
	if err != nil {
		return err
	}

	resolved, err := renderer.Resolve(env.Vars, env.Anchors)
	if err != nil {
		return err
	}
	vars := resolved.(map[string]any)

	cmdline := strings.TrimSpace(opts.action + " " + strings.Join(opts.extra, " "))
	fmt.Fprintf(io.Stderr, ">>> %s on %s\n", cmdline, env.Name)

	// Serialize everything that touches this module's cache directory, both
	// within this process (mutex) and across separate tfv processes (file lock),
	// so concurrent runs don't corrupt the shared checkout / backend record.
	mu := dirLock(source.CacheDir(root, env.ModuleSource, env.ModuleRef))
	mu.Lock()
	defer mu.Unlock()

	unlock, err := source.LockCache(root, env.ModuleSource, env.ModuleRef)
	if err != nil {
		return fmt.Errorf("lock cache: %w", err)
	}
	defer unlock()

	workDir, err := source.Prepare(root, env.ModuleSource, env.ModuleRef, !opts.noUpdate)
	if err != nil {
		return err
	}

	// Prompt for any variable the module requires (no default) that is not
	// supplied here or via a TF_VAR_* environment variable. Collecting them now
	// and passing them in the var-file makes the value a static source, which —
	// unlike OpenTofu's own interactive prompt — also works for variables used
	// in the state-encryption block.
	if err := promptMissingVars(workDir, vars, interactive); err != nil {
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
	varEnv := tofu.VarEnv(vars)
	lockKey := lockKey(vars, env.Name)
	if err := tofu.RestoreLock(root, lockKey, workDir); err != nil {
		return fmt.Errorf("restore lock: %w", err)
	}

	// When the command itself is "init", run it directly (with the backend
	// var-file and the user's own flags, e.g. -upgrade) rather than doing the
	// internal init dance — then make the lock cross-platform and save it.
	if opts.action == "init" {
		if err := tofu.Init(io, bin, workDir, varFile, opts.extra, varEnv); err != nil {
			return fmt.Errorf("init: %w", err)
		}
		if err := tofu.Lock(io, bin, workDir, varEnv); err != nil {
			return fmt.Errorf("providers lock: %w", err)
		}
		return tofu.SaveLock(root, lockKey, workDir)
	}

	if !tofu.HasCommittedLock(root, lockKey) {
		if err := tofu.Lock(io, bin, workDir, varEnv); err != nil {
			return fmt.Errorf("providers lock: %w", err)
		}
	}
	var initExtra []string
	if !opts.noUpdate {
		initExtra = []string{"-upgrade"}
	}
	if err := tofu.Init(io, bin, workDir, varFile, initExtra, varEnv); err != nil {
		return fmt.Errorf("init: %w", err)
	}
	if err := tofu.SaveLock(root, lockKey, workDir); err != nil {
		return fmt.Errorf("save lock: %w", err)
	}

	extra := opts.extra
	if opts.action == "apply" && !hasParallelism(extra) {
		extra = append(slices.Clone(extra), "-parallelism=1")
	}
	if err := tofu.Action(io, bin, workDir, opts.action, varFile, extra, varEnv); err != nil {
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

// stdin is shared across prompts so buffered input is not lost between them.
var stdin = bufio.NewReader(os.Stdin)

// promptMissingVars asks the user for every required module variable that is
// not already provided, storing the answers in vars. When interactive is false
// (parallel runs) a missing variable is an error instead of a prompt.
func promptMissingVars(workDir string, vars map[string]any, interactive bool) error {
	required, err := tofu.RequiredVars(workDir)
	if err != nil {
		return fmt.Errorf("scan module variables: %w", err)
	}
	for _, name := range required {
		if _, ok := vars[name]; ok {
			continue // supplied in the env file
		}
		if os.Getenv("TF_VAR_"+name) != "" {
			continue // supplied via the environment
		}
		if !interactive {
			return fmt.Errorf("variable %q is required but not set; provide it in the env file, via TF_VAR_%s, or run without --parallel to be prompted", name, name)
		}
		val, err := promptVar(name)
		if err != nil {
			return err
		}
		vars[name] = val
	}
	return nil
}

// promptVar reads a value for one variable from the terminal (hidden) or, when
// stdin is not a terminal, from a single line of input.
func promptVar(name string) (string, error) {
	fmt.Fprintf(os.Stderr, "var.%s\n  Enter a value: ", name)
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		return strings.TrimSpace(string(b)), err
	}
	line, err := stdin.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
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

const usageLine = "usage: tfv [flags] <pattern>... <command> [command args]"

// parseArgs reads leading tfv flags, then splits the remaining arguments into
// environment patterns (leading tokens that match existing files) and the
// OpenTofu command with its own arguments (everything after). An explicit "--"
// may be used to force the boundary.
func parseArgs(args []string) (options, error) {
	opts := options{format: "json"}
	var err error

	// Phase 1: tfv's own flags, which come first.
	i := 0
flags:
	for ; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--no-update" || a == "--offline":
			opts.noUpdate = true
		case a == "--render" || a == "--debug":
			opts.render = true
		case a == "--keep-going":
			opts.keepGoing = true
		case a == "--parallel":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("--parallel requires a value")
			}
			if opts.parallel, err = strconv.Atoi(args[i]); err != nil {
				return opts, fmt.Errorf("--parallel: %w", err)
			}
		case strings.HasPrefix(a, "--parallel="):
			if opts.parallel, err = strconv.Atoi(strings.TrimPrefix(a, "--parallel=")); err != nil {
				return opts, fmt.Errorf("--parallel: %w", err)
			}
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
			break flags
		}
	}
	rest := args[i:]

	// Phase 2: split env patterns from the OpenTofu command.
	var command []string
	if sep := slices.Index(rest, "--"); sep >= 0 {
		opts.patterns = rest[:sep]
		command = rest[sep+1:]
	} else {
		split := 0
		for split < len(rest) && isEnvFile(rest[split]) {
			split++
		}
		opts.patterns = rest[:split]
		command = rest[split:]
	}

	if len(opts.patterns) == 0 {
		return opts, fmt.Errorf("%s\nno environment files matched", usageLine)
	}

	// --render only resolves and prints variables, so it takes no command.
	if opts.render {
		return opts, nil
	}
	if len(command) == 0 {
		return opts, fmt.Errorf("%s\nno OpenTofu command given", usageLine)
	}
	opts.action = command[0]
	opts.extra = command[1:]
	return opts, nil
}

// isEnvFile reports whether a token refers to an existing file (directly or via
// a glob), and therefore is an environment pattern rather than a command token.
func isEnvFile(s string) bool {
	if m, _ := filepath.Glob(s); len(m) > 0 {
		return true
	}
	_, err := os.Stat(s)
	return err == nil
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
    tfv [flags] <pattern>... <command> [command args]

    <pattern>   one or more env-file paths or globs (the leading arguments that
                match existing files). Quote globs so the shell does not expand
                them first, e.g. 'prod-*.yaml'.
    <command>   any OpenTofu command and its arguments, passed through verbatim:
                "plan", "apply -auto-approve", "state list", "output -json",
                "import <addr> <id>", ...
    --          optional separator forcing the pattern/command boundary, e.g.
                when a command token would otherwise look like a file.

    Variables are passed to commands that accept -var-file (plan, apply,
    destroy, refresh, import, console, test); for every command, string-valued
    variables are also exported as TF_VAR_<name> so commands like "state list"
    or "output" can read the backend and decrypt state.

FLAGS:
    --no-update, --offline   use the cached module without "git fetch"
                             (errors if that version is not cached yet)
    --render, --debug        resolve variables and print them, then exit
                             (does not run OpenTofu)
    --format json|yaml       output format for --render (default: json)
    --env-file PATH          load environment variables from a dotenv file
                             before reading credentials
    --parallel N             run up to N environments at once (default: 1);
                             output is grouped per environment, no prompting
    --keep-going             do not stop at the first failing environment
    -h, --help               show this help
    -V, --version            print the tfv version

CREDENTIALS (from the environment, optionally loaded via --env-file):
    VAULT_ADDR               default Vault address when a reference has no
                             address prefix, e.g. https://vault.example
    VAULT_TOKEN              default Vault token (falls back to ~/.vault-token)
    VAULT_TOKEN_<HOST>       token for a specific server, e.g.
                             VAULT_TOKEN_VAULT_A_EXAMPLE for vault-a.example
    TFV_TOFU_BIN             OpenTofu binary when "tofu_bin" is not set in the
                             env file (default: "tofu")
    TF_PLUGIN_CACHE_DIR      shared provider cache (defaults to .tfv/plugin-cache)

    Any variable the module requires (no default) that is not in the env file or
    a TF_VAR_<name> environment variable is prompted for interactively before
    OpenTofu runs, then passed to every command.

ENV FILE:
    module_source is "<git-url>#<ref>" — the module to deploy and its branch or
    tag. It is removed before the remaining keys are handed to OpenTofu as
    tfvars. The module is cloned per (url, ref) under .tfv/cache, so different
    branches/tags coexist. Lock files are committed under .lock/.

    tofu_bin (optional) overrides the OpenTofu binary for this environment;
    when omitted, "tofu" from PATH is used. It is also stripped from the tfvars.

    include (optional) is a list of YAML files merged in before this one, in
    the order listed (a later file overrides an earlier one); this file's own
    keys then override the includes. Maps merge deeply, lists/scalars replace
    (like passing several -f files to Helm), but a list tagged !append/!prepend
    is combined with the base list instead of replacing it. Paths are relative to
    this file; includes may nest. YAML anchors defined here are visible to the
    included files (so &anchor in an env file can be used as *anchor elsewhere).

    Every included file is rendered as a Go template (sprig functions; inherited
    anchors and any passed "values" bound as $variables) before being parsed —
    so it can use loops, conditionals, etc. (The root env file is not a template.)

        include:
          - 1_cilium.yaml                   # plain — still a template
          - file: default.yaml              # with parameters
            values: { hosts: { a: {ip: 10.0.0.1} }, enabled: true }

        module_source: https://git.example/modules/app.git#master
        tofu_bin: tofu          # optional, e.g. a pinned "tofu-1.11.1"
        include:                # optional defaults merged in first
          - default.yaml
        tenant: acme
        env: dev
        region: eu-1
        instance: app.example

SECRET / TEMPLATE SYNTAX:
    Any string value may pull secrets from Vault and transform them:

      "<vault:PATH#FIELD>"             a secret field
      "<vault:PATH#FIELD | fn | fn>"   transformed by pipe functions
      (the legacy "<path:...>" prefix is also accepted)

    PATH is the Vault path (KV v2 paths include "/data/"); FIELD is the key.
    Multi-argument functions take the secret as the piped (last) argument, e.g.
    "<vault:PATH#pw | htpasswd \"admin\">" is htpasswd "admin" <pw>.

    A YAML anchor can be used as a variable anywhere in a string via its name
    (the name after &), and may be piped through any sprig/Helm function:

      region: &region ru-msk-0
      env: &env dev
      location: "my-{{ $region }}-{{ $env }}"     ->  my-ru-msk-0-dev
      upper:    "{{ $region | upper }}"           ->  RU-MSK-0
      id:       "{{ $cluster_id | default 0 }}"   ->  0   (a number, type kept)

    When the whole value is a single "{{ ... }}" expression, the result type is
    inferred (number/bool/string). Anchors are visible across the include chain.

    Apart from anchor expressions and the placeholders above, any other
    "{{ ... }}" is left exactly as written and passed through (a bare unknown
    "{{ $x }}" too), so templating meant for Helm, Vector, etc. is not disturbed.

    To read from a specific Vault server, prefix PATH with its URL; the token is
    then taken from VAULT_TOKEN_<HOST> (falling back to VAULT_TOKEN). Without a
    prefix, VAULT_ADDR is used.

      "<vault:https://vault-b.example/common/data/app#token>"

    Tip: keep a per-environment state passphrase in Vault and reference it as a
    normal variable, e.g.  encryption_key: "<vault:secret/data/tfstate/dev#key>"

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

    # plan / apply a single environment
    tfv dev.yaml plan
    tfv dev.yaml apply -auto-approve

    # any OpenTofu command, including multi-word ones and flags
    tfv dev.yaml state list
    tfv dev.yaml output -json
    tfv dev.yaml destroy -target grafana_dashboard.dashboard

    # act on every matching environment (glob expanded by tfv)
    tfv 'prod-*.yaml' apply

    # plan the whole fleet, 4 at a time, without stopping on failures
    tfv --parallel 4 --keep-going 'envs/*.yaml' plan

    # reuse the already-downloaded module, skip git fetch
    tfv --no-update dev.yaml plan

    # destroy, loading credentials from a dotenv file
    tfv --env-file .env dev.yaml destroy

    Secret examples inside an env YAML:
      client_id:  "<vault:common/data/oauth/app#client_id>"
      client_b64: "<vault:common/data/oauth/app#client_id | b64enc>"
      htpasswd:   '<vault:common/data/app#password | htpasswd "admin">'
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
