# Add a repository and instance

`agentmux repo add OWNER/REPO` clones a GitHub repository and provisions an
agentmux instance in one command:

```sh
agentmux repo add tan-gl/n -agent amp
```

By default, checkouts live under `~/Projects/<owner>/<repo>` and the instance
is named `<repo>-<agent>`. Set `repos_root` in
`~/.config/agentmux/repos.yaml` to choose a host-specific root:

```yaml
repos_root: ~/Projects
```

An existing checkout is reused if it is a Git repository. An existing
instance with the requested name is reused only when its agent and workdir
match. Otherwise the command reports the conflict. Clone authentication uses
the host's normal Git credential helper; agentmux stores no credentials.

The checkout's adjacent `<repo>-worktrees` directory is reported for use by
task sessions. Repository cloning runs on the local host; invoke the command
on a remote host where its checkout should live.
