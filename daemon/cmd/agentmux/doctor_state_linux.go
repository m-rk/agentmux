package main

import (
	"os"
	"os/user"
	"path/filepath"
)

// Root's doctor state belongs under a root-controlled directory, not inside
// the selected user's symlink-controllable home. The UID keeps the filename
// path-safe even on systems with unusual account names.
func defaultDoctorStatePath(identity *user.User) string {
	if os.Geteuid() != 0 {
		return filepath.Join(identity.HomeDir, ".local", "state", "agentmux", "doctor.json")
	}
	return filepath.Join("/var/lib/agentmux", "doctor-"+identity.Uid+".json")
}
