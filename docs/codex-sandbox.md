# Codex sandbox on Ubuntu hosts

## Symptom

On an Ubuntu 24.04+ host, `codex exec -s read-only` and `-s workspace-write`
cannot run any shell command or write any file:

```
bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted
```

`codex sandbox -- true` reproduces it without a model turn, and `agentmux
doctor` runs that same self-test and reports a problem when it fails.

## Cause

Codex sandboxes commands with its bundled `bwrap` (bubblewrap), which needs an
unprivileged user namespace with its own network namespace. Ubuntu 24.04 sets
`kernel.apparmor_restrict_unprivileged_userns=1`: an unconfined program may
create a user namespace, but the capabilities inside it are stripped, so
bwrap's loopback setup fails. `unshare -Ur true` fails the same way
(`write failed /proc/self/uid_map`).

## Fix (narrowest)

Leave the sysctl on and grant `userns` to the codex `bwrap` binary only, with
an AppArmor profile, as Ubuntu does for other bubblewrap users. As root:

```sh
sudo tee /etc/apparmor.d/codex-bwrap >/dev/null <<'PROFILE'
abi <abi/4.0>,
include <tunables/global>

profile codex-bwrap /home/*/.codex/packages/standalone/releases/*/codex-resources/bwrap flags=(unconfined) {
  userns,
}
PROFILE
sudo apparmor_parser -r /etc/apparmor.d/codex-bwrap
```

The glob covers every user's installed release, so codex upgrades keep
working. If codex is installed elsewhere, change the path to wherever
`codex-resources/bwrap` lives (`readlink -f` the `codex` binary). Avoid
`sysctl kernel.apparmor_restrict_unprivileged_userns=0`: it opens user
namespaces to every program on the host.

## Verify

As the run user, in a scratch git repo:

```sh
codex sandbox -- true                      # exits 0
codex exec -s workspace-write "write inside.txt here, then write ~/outside.txt"
#   inside.txt is created; the home-directory write fails: Read-only file system
codex exec -s read-only "cat a file, then write ro.txt"
#   the read works; the write fails
```

Note `workspace-write` also allows `/tmp` and `$TMPDIR` by default, so test the
"cannot write outside" case against a path under the home directory.
