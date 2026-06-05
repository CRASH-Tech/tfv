//go:build unix

package tofu

import (
	"os"
	"syscall"
)

// shutdownSignals are the signals that should stop tfv and its OpenTofu child.
// SIGINT (Ctrl-C) is already delivered to the whole foreground process group by
// the terminal, so it reaches OpenTofu directly; SIGTERM is not, and is
// forwarded explicitly (see the handler in tofu.go).
var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}
