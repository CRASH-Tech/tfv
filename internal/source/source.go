// Package source manages the OpenTofu module checkout. Each (url, ref) pair is
// cloned into its own directory under .tfv/cache, so several versions — e.g.
// different branches or tags — can coexist without clobbering each other.
//
// OpenTofu runs directly inside the cache directory. The module's backend.tf
// derives the state path as "${path.module}/../.tstate/<name>.tstate", i.e. it
// expects the module's parent to contain the state directory. To satisfy that
// without copying files around, a single symlink .tfv/cache/.tstate points at
// the real project .tstate, so "<cache>/<slug>/../.tstate" resolves correctly.
package source

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const cacheRel = ".tfv/cache" // relative to project root

// Slug returns the cache directory name for a (url, ref) pair: a readable
// "<repo>-<ref>-<hash>" so coexisting versions are easy to tell apart.
func Slug(url, ref string) string {
	h := sha1.Sum([]byte(url + "\x00" + ref))
	return fmt.Sprintf("%s-%s-%s", sanitize(repoName(url)), sanitize(ref), hex.EncodeToString(h[:])[:8])
}

func repoName(url string) string {
	u := strings.TrimSuffix(strings.TrimRight(url, "/"), ".git")
	if i := strings.LastIndexAny(u, "/:"); i >= 0 {
		u = u[i+1:]
	}
	return u
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '.' || r == '_' || r == '-',
			r >= '0' && r <= '9',
			r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// Prepare ensures the module (url@ref) is cloned/updated and returns the
// working directory tofu should run in. When update is false the git fetch is
// skipped and the existing cache is reused (erroring if absent).
func Prepare(root, url, ref string, update bool) (string, error) {
	cacheBase := filepath.Join(root, cacheRel)
	cacheDir := filepath.Join(cacheBase, Slug(url, ref))
	gitDir := filepath.Join(cacheDir, ".git")

	if _, err := os.Stat(gitDir); err != nil {
		if !update {
			return "", fmt.Errorf("module %s@%s is not cached and --no-update is set; run once without --no-update first", url, ref)
		}
		if err := os.MkdirAll(cacheBase, 0o755); err != nil {
			return "", err
		}
		_ = os.RemoveAll(cacheDir) // clear any partial leftovers
		if err := git(root, "clone", "--depth", "1", "--branch", ref, url, cacheDir); err != nil {
			return "", fmt.Errorf("git clone %s@%s: %w", url, ref, err)
		}
	} else if update {
		if err := git(cacheDir, "fetch", "--depth", "1", "origin", ref); err != nil {
			return "", fmt.Errorf("git fetch %s@%s: %w", url, ref, err)
		}
		if err := git(cacheDir, "reset", "--hard", "FETCH_HEAD"); err != nil {
			return "", fmt.Errorf("git reset %s@%s: %w", url, ref, err)
		}
	}

	if err := ensureStateLink(root, cacheBase); err != nil {
		return "", err
	}
	return cacheDir, nil
}

// ensureStateLink creates/refreshes .tfv/cache/.tstate -> ../../.tstate so the
// backend's relative state path resolves to the real project state directory.
func ensureStateLink(root, cacheBase string) error {
	if err := os.MkdirAll(filepath.Join(root, ".tstate"), 0o755); err != nil {
		return err
	}
	link := filepath.Join(cacheBase, ".tstate")
	const target = "../../.tstate"

	if cur, err := os.Readlink(link); err == nil {
		if cur == target {
			return nil
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	} else if _, err := os.Lstat(link); err == nil {
		return fmt.Errorf("%s exists and is not a symlink; remove it", link)
	}
	return os.Symlink(target, link)
}

func git(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
