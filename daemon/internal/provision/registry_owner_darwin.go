//go:build darwin

package provision

// SelfHealRegistryOwnership is a no-op on macOS: the daemon and its
// provisioners run as the current user there (see createAgentmux's
// os.Geteuid() == 0 checks), so a registry file's owner already matches
// the LaunchAgent that reads and self-corrects it — see the Linux
// counterpart in registry_owner_linux.go for the incident this guards
// against on Linux, where the provisioner runs as root but instance ticks
// run as the instance's own run user.
func SelfHealRegistryOwnership() {}
