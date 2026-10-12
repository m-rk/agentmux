//go:build !linux

package main

import "errors"

func serveTaskcheckBroker() error {
	return errors.New("task-check broker is supported only on Linux systemd hosts")
}
