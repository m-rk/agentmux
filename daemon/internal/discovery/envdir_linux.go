package discovery

// defaultEnvDir is where backends/agentmux/install.sh and
// backends/claude-code/install.sh (and, going forward, the native Go
// provisioner) write each instance's registry file on Linux: a root-owned
// system directory, matching the root-owned systemd units those instances
// run as.
func defaultEnvDir() string {
	return "/etc/agentmux"
}
