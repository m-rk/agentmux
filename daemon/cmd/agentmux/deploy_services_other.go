//go:build !linux

package main

import (
	"context"
	"fmt"
	"time"
)

// deployOwnedServices is only implemented on Linux (systemd); see
// deploy_services_linux.go. macOS (launchd) support is future work.
func deployOwnedServices() []deployService {
	return nil
}

func deployRestartServices(ctx context.Context, services []deployService) error {
	return fmt.Errorf("agentmux deploy is only supported on Linux (systemd)")
}

func deployWaitActive(ctx context.Context, unit string, timeout time.Duration) error {
	return fmt.Errorf("agentmux deploy is only supported on Linux (systemd)")
}

func deployServiceInstalled(ctx context.Context, unit string) bool { return false }

func deployDoctorTimeFromTimer(ctx context.Context) (string, error) {
	return "", nil
}

func deployServiceMainPID(ctx context.Context, unit string) uint32 { return 0 }

func deployBinaryStartTime(pid uint32) string { return "" }

func deployCheckRootOwnedRepos(ctx context.Context, repo string) error { return nil }
