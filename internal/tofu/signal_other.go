//go:build !unix

package tofu

import "os"

// shutdownSignals are the signals that should stop tfv and its OpenTofu child.
var shutdownSignals = []os.Signal{os.Interrupt}
