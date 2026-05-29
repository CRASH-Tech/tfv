// Package tofu locates and runs the OpenTofu binary.
package tofu

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// locksRel is the repository-committed directory holding per-environment lock
// files, keyed like the state file (tenant-env-region-instance).
const locksRel = ".lock"

// LockPlatforms mirrors the cross-platform lock the original script generated.
var LockPlatforms = []string{"linux_amd64", "linux_arm64", "darwin_arm64"}

// Binary resolves the OpenTofu binary: the per-environment override (tofu_bin)
// if set, then $TFV_TOFU_BIN, then plain "tofu" from PATH.
func Binary(override string) string {
	if override != "" {
		return override
	}
	if b := os.Getenv("TFV_TOFU_BIN"); b != "" {
		return b
	}
	return "tofu"
}

// WriteVars writes vars to a temporary JSON tfvars file and returns its path.
// The caller is responsible for removing it.
func WriteVars(vars map[string]any) (string, error) {
	f, err := os.CreateTemp("", "tfv-*.tfvars.json")
	if err != nil {
		return "", err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(vars); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// LockPath is the committed lock file for an environment key.
func LockPath(root, key string) string {
	return filepath.Join(root, locksRel, key+".terraform.lock.hcl")
}

// HasCommittedLock reports whether a lock for the key is stored in the repo.
func HasCommittedLock(root, key string) bool {
	_, err := os.Stat(LockPath(root, key))
	return err == nil
}

// RestoreLock copies the committed lock (if any) into the working directory so
// init reuses the recorded provider versions/checksums.
func RestoreLock(root, key, workDir string) error {
	src := LockPath(root, key)
	if _, err := os.Stat(src); err != nil {
		return nil // nothing committed yet
	}
	return copyFile(src, filepath.Join(workDir, ".terraform.lock.hcl"))
}

// SaveLock copies the working directory's lock back into the committed location
// so it can be version-controlled.
func SaveLock(root, key, workDir string) error {
	src := filepath.Join(workDir, ".terraform.lock.hcl")
	if _, err := os.Stat(src); err != nil {
		return nil
	}
	dst := LockPath(root, key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return copyFile(src, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// varsCommands lists the OpenTofu subcommands that accept -var-file. Any other
// command (state, output, show, workspace, ...) is run without it; such
// commands still receive variables they need via the TF_VAR_* environment (see
// VarEnv), which works universally.
var varsCommands = map[string]bool{
	"plan":    true,
	"apply":   true,
	"destroy": true,
	"refresh": true,
	"import":  true,
	"console": true,
	"test":    true,
}

// AcceptsVarFile reports whether the given subcommand takes -var-file.
func AcceptsVarFile(action string) bool {
	return varsCommands[action]
}

// VarEnv returns TF_VAR_<name>=<value> entries for every string-valued variable.
// This exposes the backend identity (tenant/env/region/instance) and any
// passphrase to every OpenTofu command, including those that cannot take a
// -var-file. Complex-typed variables are left to the -var-file.
func VarEnv(vars map[string]any) []string {
	var env []string
	for k, v := range vars {
		if s, ok := v.(string); ok {
			env = append(env, "TF_VAR_"+k+"="+s)
		}
	}
	return env
}

// IO holds the streams an OpenTofu invocation uses. Sequential runs use the
// process streams (see DefaultIO); parallel runs buffer output so each
// environment's logs stay grouped.
type IO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// DefaultIO connects OpenTofu to the process's own streams.
func DefaultIO() IO {
	return IO{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
}

// Lock generates a cross-platform provider lock file.
func Lock(io IO, bin, workDir string, extraEnv []string) error {
	args := []string{"-chdir=" + workDir, "providers", "lock"}
	for _, p := range LockPlatforms {
		args = append(args, "-platform="+p)
	}
	return run(io, bin, args, extraEnv)
}

// Init runs `tofu init -reconfigure` with the variable file (needed because
// the backend path is derived from variables). upgrade adds -upgrade.
// Input is left enabled so OpenTofu can prompt for any variable that is not
// supplied in the env file or the environment (e.g. an encryption passphrase).
func Init(io IO, bin, workDir, varFile string, upgrade bool, extraEnv []string) error {
	args := []string{"-chdir=" + workDir, "init", "-reconfigure", "-var-file=" + varFile}
	if upgrade {
		args = append(args, "-upgrade")
	}
	return run(io, bin, args, extraEnv)
}

// Action runs an OpenTofu subcommand. The variable file is added only for
// commands that accept it; extra holds the remaining command arguments
// (sub-subcommands like "list", resource addresses, flags such as -target, ...).
func Action(io IO, bin, workDir, action, varFile string, extra, extraEnv []string) error {
	args := []string{"-chdir=" + workDir, action}
	if AcceptsVarFile(action) {
		args = append(args, "-var-file="+varFile)
	}
	args = append(args, extra...)
	return run(io, bin, args, extraEnv)
}

func run(io IO, bin string, args, extraEnv []string) error {
	cmd := exec.Command(bin, args...)
	cmd.Stdin = io.Stdin
	cmd.Stdout = io.Stdout
	cmd.Stderr = io.Stderr
	cmd.Env = append(os.Environ(), extraEnv...)
	return cmd.Run()
}
