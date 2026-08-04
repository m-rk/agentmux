package main

import (
	"os/user"
	"path/filepath"
)

func defaultDoctorStatePath(identity *user.User) string {
	return filepath.Join(identity.HomeDir, ".local", "state", "agentmux", "doctor.json")
}
