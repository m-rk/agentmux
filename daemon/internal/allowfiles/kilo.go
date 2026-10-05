package allowfiles

// KiloConfigEnv is the environment variable kilo reads an inline JSON config
// from. It is merged after the project's kilo.json and the global
// ~/.config/kilo config, so its permission rules win over theirs.
const KiloConfigEnv = "KILO_CONFIG_CONTENT"

// KiloAllowConfig returns the inline JSON config that lets a kilo session
// started in workdir read and edit exactly the given files outside workdir
// without a permission prompt. paths and workdir must be absolute and
// symlink-resolved.
//
// The launcher applies it by setting KiloConfigEnv=<returned bytes> in the
// environment of every kilo process for the instance, next to the per-instance
// XDG_DATA_HOME/XDG_STATE_HOME from kiloInstanceXDGEnv. agentmux does not
// isolate XDG_CONFIG_HOME, so a per-instance config file would mean sharing or
// faking the global config; the env var avoids both and writes nothing into
// the worktree. The content carries no "{env:...}" references, which kilo
// rejects in project-level config.
//
// kilo 7.4.22 shares opencode's permission schema and behaviour (verified with
// `kilo run`: the same external_directory gate, workdir-relative read/edit
// patterns, last-match-wins), so the rules and limits are those documented on
// OpencodeAllowConfig, including rejection of paths containing "*" or "?".
func KiloAllowConfig(workdir string, paths []string) ([]byte, error) {
	return opencodeStyleAllowConfig(workdir, paths)
}
