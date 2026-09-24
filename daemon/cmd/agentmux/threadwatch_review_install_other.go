//go:build !linux

package main

import "fmt"

// installThreadwatchReviewTimer is only implemented on Linux (systemd).
// See threadwatch_review_install_linux.go.
func installThreadwatchReviewTimer(runUser, bin, at string, print bool) error {
	return fmt.Errorf("agentmux threadwatch review install is only supported on Linux (systemd)")
}
