//go:build !linux

package main

import "errors"

func servePreviewBroker() error {
	return errors.New("preview broker is supported only on Linux systemd hosts")
}
