//go:build !linux

package main

import (
	"os"
	"path/filepath"
)

// deployBinaryPath is the installed binary every service execs: the same
// path daemon install pins on macOS (~/.agentmux/bin/agentmux).
func deployBinaryPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".agentmux", "bin", "agentmux")
	}
	return filepath.Join(home, ".agentmux", "bin", "agentmux")
}
