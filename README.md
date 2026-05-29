# tfv

`tfv` is a small CLI for managing many similar OpenTofu deployments from plain
YAML.

Each deployment is described by one YAML file. `tfv`:

1. **Resolves secrets** — values may reference [HashiCorp Vault](https://www.vaultproject.io)
   and be transformed with [Helm/sprig](https://masterminds.github.io/sprig/)
   template functions (`b64enc`, `htpasswd`, …), so no secret material is stored
   in the file.
2. **Fetches the module** — the OpenTofu module is pulled from a git URL + ref
   declared in the YAML and cached locally, instead of being vendored into the
   repository.
3. **Runs OpenTofu** — the resolved values are passed as variables to `plan`,
   `apply`, `destroy`, or any other OpenTofu subcommand.

This keeps every environment as a single readable, secret-free file while the
actual infrastructure code lives in a versioned module shared across them.

## Install

Download a prebuilt binary for your platform from the
[Releases](../../releases) page (Linux/macOS/Windows, amd64/arm64), or build
from source:

```sh
go build -o tfv .
```

Requires an [OpenTofu](https://opentofu.org) binary on `PATH` at runtime, and Go
to build from source. Check the version with `tfv --version`.

## Usage

```
tfv [flags] <pattern>... <command> [command args]
```

- `<pattern>...` — the leading arguments that match existing files: one or more
  env-file paths or globs. Globs are expanded by `tfv` itself, so quote them to
  bypass the shell: `tfv 'prod-*.yaml' plan`.
- `<command>` — any OpenTofu command and its arguments, passed through verbatim.
  Multi-word commands and flags work: `plan`, `apply -auto-approve`,
  `state list`, `output -json`, `import <addr> <id>`, … Not needed with
  `--render`.
- An optional `--` can force the pattern/command boundary if a command token
  would otherwise look like a file.

Variables are sent via `-var-file` to commands that accept it (`plan`, `apply`,
`destroy`, `refresh`, `import`, `console`, `test`). For every command,
string-valued variables are also exported as `TF_VAR_<name>`, so commands like
`state list` or `output` can read the backend and decrypt state.

### Flags

| Flag | Meaning |
| --- | --- |
| `--no-update`, `--offline` | Use the cached module without `git fetch` (errors if not yet cached). |
| `--render`, `--debug` | Resolve and print the variables, then exit — no OpenTofu. |
| `--format json\|yaml` | Output format for `--render` (default `json`). |
| `--env-file PATH` | Load environment variables from a dotenv file before reading credentials. |
| `--parallel N` | Run up to N environments at once (default 1). Output is grouped per environment; interactive prompting is disabled. |
| `--keep-going` | Don't stop at the first failing environment; report a summary at the end. |
| `-h`, `--help` | Show full help with examples. |
| `-V`, `--version` | Print the tfv version. |

### Examples

```sh
# inspect the fully resolved variables, without running OpenTofu
tfv --render dev.yaml
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

# load credentials from a dotenv file first
tfv --env-file .env dev.yaml destroy
```

## Environment YAML

See [`envs/example.yaml`](envs/example.yaml) for a fully commented, publishable
example.

`module_source` is tool metadata and is stripped before the file is handed to
OpenTofu; every other key becomes a tfvar. It is written as `url#ref` — the git
URL of the module and the branch or tag to use:

```yaml
module_source: https://git.example/modules/app.git#master

# optional: pin the OpenTofu binary for this environment (default: "tofu")
tofu_bin: tofu-1.11.1

tenant: acme
env: dev
region: eu-1
instance: app.example
# ... any other variables your module expects
```

`tofu_bin` is optional tool metadata (stripped before OpenTofu runs): if set it
selects the OpenTofu binary for that environment, otherwise `tofu` from `PATH`
is used.

On each run `tfv` clones/updates the module into its own
`.tfv/cache/<repo>-<ref>-<hash>/` directory and runs OpenTofu there, so several
versions — different branches or tags across environments — coexist with their
own provider downloads. A `.tfv/cache/.tstate` symlink makes a module that
stores state at `${path.module}/../.tstate/...` resolve to the project's real
`.tstate/`. Providers are shared through a single cache
(`TF_PLUGIN_CACHE_DIR`, defaulting to `.tfv/plugin-cache`), so they download
once across all checkouts. The `.tfv/` cache is disposable and gitignored.

### Lock files

Provider lock files are committed in `.lock/`, one per environment, named like
the state file (`<tenant>-<env>-<region>-<instance>.terraform.lock.hcl`). Before
`init` the committed lock is copied into the working directory; after `init` it
is copied back. The first time an environment is seen, a cross-platform lock
(`linux_amd64`, `linux_arm64`, `darwin_arm64`) is generated. Commit `.lock/`.

## Secret / template syntax

Any string value may pull secrets from Vault and transform them, in either form:

```yaml
# shortcut (the <path:...> prefix is also accepted)
client_id:  "<vault:common/data/oauth/app#client_id>"
client_b64: "<vault:common/data/oauth/app#client_id | b64enc>"

# full Go template — needed for functions taking more than one argument
htpasswd: '{{ htpasswd "admin" (vault "common/data/app#password") }}'
```

`<vault:PATH#KEY | f | g>` is rewritten to `{{ vault "PATH#KEY" | f | g }}` and
executed as a Go `text/template`. All
[sprig](https://masterminds.github.io/sprig/) (Helm) functions are available —
`b64enc`, `htpasswd`, `sha256sum`, `quote`, … — plus compatibility aliases
(`base64encode`, `base64decode`, `sha256`, …) defined in
`internal/secrets/funcs.go`. Add new aliases there.

Both KV v1 and KV v2 mounts are supported (KV v2 paths include the `/data/`
segment).

### Multiple Vault servers

A reference may target a specific Vault server by prefixing the path with its
URL; otherwise `VAULT_ADDR` is used. Clients are created lazily and cached per
address, so a run touching several servers reuses one connection each.

```yaml
a: "<vault:common/data/app#x>"                                  # default VAULT_ADDR
b: "<vault:https://vault-b.example/common/data/app#token>"      # specific server
```

The token for a prefixed server is read from `VAULT_TOKEN_<HOST>` (the host
upper-cased, non-alphanumerics replaced with `_`; e.g. `vault-b.example` →
`VAULT_TOKEN_VAULT_B_EXAMPLE`), falling back to `VAULT_TOKEN`, then
`~/.vault-token`.

### Per-environment state passphrase

If the module uses an encrypted backend, keep its passphrase in Vault and
reference it like any other variable — each environment then has its own key
with no env var and no prompt:

```yaml
encryption_key: "<vault:secret/data/tfstate/dev#key>"
```

## Credentials

Read from the environment (optionally loaded via `--env-file`):

- `VAULT_ADDR` — default Vault address for references without a URL prefix
- `VAULT_TOKEN` / `VAULT_TOKEN_<HOST>` — token (falls back to `~/.vault-token`);
  see [Multiple Vault servers](#multiple-vault-servers)

Any variable the module requires (declared without a default) that is not in
the env file or a `TF_VAR_<name>` environment variable is prompted for
interactively before OpenTofu runs (for example an encrypted-backend
passphrase), and then passed to every command.

The OpenTofu binary is the env file's `tofu_bin` if set, else `$TFV_TOFU_BIN`,
else `tofu` from `PATH`.
