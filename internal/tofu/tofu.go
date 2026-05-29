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
const locksRel = "locks"

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

// Lock generates a cross-platform provider lock file.
func Lock(bin, workDir string) error {
	args := []string{"-chdir=" + workDir, "providers", "lock"}
	for _, p := range LockPlatforms {
		args = append(args, "-platform="+p)
	}
	return run(bin, args)
}

// Init runs `tofu init -reconfigure` with the variable file (needed because
// the backend path is derived from variables). upgrade adds -upgrade.
func Init(bin, workDir, varFile string, upgrade bool) error {
	args := []string{"-chdir=" + workDir, "init", "-reconfigure", "-input=false", "-var-file=" + varFile}
	if upgrade {
		args = append(args, "-upgrade")
	}
	return run(bin, args)
}

// Action runs an arbitrary tofu subcommand (plan, apply, destroy, ...) with the
// variable file and any passthrough arguments.
func Action(bin, workDir, action, varFile string, extra []string) error {
	args := []string{"-chdir=" + workDir, action, "-var-file=" + varFile}
	args = append(args, extra...)
	return run(bin, args)
}

func run(bin string, args []string) error {
	cmd := exec.Command(bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	return cmd.Run()
}
