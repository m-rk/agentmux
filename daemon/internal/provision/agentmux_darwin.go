package provision

import "fmt"

// createAgentmux on macOS is not implemented yet (see claudecode_darwin.go
// for the same caveat) — stubbed so the package still cross-compiles.
func createAgentmux(opts Options) (string, error) {
	return "", fmt.Errorf("creating %s instances on macOS isn't implemented yet", opts.Agent)
}
