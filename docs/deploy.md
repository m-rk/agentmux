# Deploy

`agentmux deploy` (Linux, root) is how a new binary reaches every running
service on a host. `agentmux daemon install` restarts only the daemon; the
gateway kept serving the previous day's code once (cross-host creates used
old logic and stale bases), so deploy restarts everything agentmux owns:

```sh
sudo agentmux deploy                      # pin, restart, verify, smoke-test
sudo agentmux deploy -template agentmux-amp -base main
```

1. **Pin** the invoking binary to `/usr/local/bin/agentmux` (atomically,
   via temp + rename) and hash it.
2. **Rewrite** the daemon, doctor and gc units for the new binary, keeping
   the installed doctor timer's time unless `-doctor-time HH:MM` is given.
3. **Restart** the daemon plus every installed agentmux-owned user service
   (the gateway, `asks serve`, thread watch when their units exist) and
   every installed agentmux-owned user unit in the invoking user's
   (`SUDO_USER`) manager — `agentmux-asks-serve.service` when it exists —
   via `systemctl --user -M <user>@`, and wait for each to become active.
   Timers are left alone — they run the pinned binary on their next tick.
   Instance units (`agentmux-<name>`)
   are never touched: deploy does not interrupt running agents.
4. **Verify** each restarted service's `/proc/<pid>/exe` hashes to the
   pinned binary. A service still on the old binary fails the deploy.
5. **Ownership check** (the AMUX-23 guard): no root-owned files may be left
   in the template repo's `.git` by a privileged git step. The smoke
   test's fetch runs as the template's run user even when deploy runs as
   root, so this check should always pass — it fails the deploy if it
   doesn't.
6. **Smoke test** every host in the run user's `hosts.yaml` (plus the
   local host): a dry-run `sessions create -base <base>` and a dry-run
   `sessions run`
   that starts no amp thread, and print the per-host result. Deploy runs
   as root under sudo, so it resolves the hosts file from `SUDO_USER`'s
   home, not root's; it prints which file was used
   (`deploy: hosts file /home/alice/.config/agentmux/hosts.yaml (from SUDO_USER alice)`).
   A stale
   gateway that predates `dry_run` refuses with `unknown field "dry_run"`,
   which fails the deploy and names the host — deploy the new binary
   there and re-run.

The smoke test never creates sessions or threads: it exercises the same
code paths short of the spawn. `-template` names the instance to copy on
each host (default: that host's first amp instance whose workdir is a Git
checkout, else its first instance); hosts with no instances are skipped,
and a host with no gateway fails with `not_local`. A host whose amp
instances all live outside checkouts is skipped with a reason naming
them — no checkout can supply the create's template.

The dry-run create names the `task-smoke-deploy` instance, so it fits the
fleet's gateway create grants (`task-*@<host>`); the grant stays narrow by
design (AMUX-26). `-smoke-name NAME` picks another name when a host grants
a different pattern. The dry-run run targets the smoke name too, not the
template — the fleet's run grants cover `task-*` sessions only (AMUX-27).
A host whose gateway still refuses the create as forbidden is reported as
skipped — `deploy: smoke <host> skipped (no create grant for a smoke name:
...; grant a task-* create or rerun with -smoke-name NAME)` — and its run
check still runs against the smoke name, rather than failing the deploy.
A forbidden run skips the same way; a run that finds no smoke session
after a skipped create skips as well, since the dry-run create made
nothing. Only a run that finds no session after a passed create fails —
the smoke name should exist.

```text
deploy: pinned /usr/local/bin/agentmux (5b7072e48fe8)
deploy: keeping doctor time 03:30
deploy: restarting agentmuxd.service, agentmux-gateway.service (gateway), agentmux-threadwatch.service (threadwatch)
deploy: agentmuxd.service                pid 1816454 ok 5b7072e48fe8
deploy: agentmux-gateway.service         pid 1816462 ok 5b7072e48fe8
deploy: agentmux-threadwatch.service    pid 1816478 ok 5b7072e48fe8
deploy: every service runs 5b7072e48fe8
deploy: smoke build-box   create ok (origin/main @ 9fceb02d0ae5) run ok (start amp thread on task-smoke-deploy@build-box)
deploy: smoke test passed on every host
```

The two dry runs are also available directly, for use outside deploy:

```sh
agentmux sessions create -template web@build-box -instance task-42 \
  -branch feature/task-42 -base main -dry-run
echo "smoke check" > /tmp/prompt.txt
agentmux sessions run -dry-run -file /tmp/prompt.txt 'web@build-box'
```

`-dry-run` on `create` checks names, the template, the instance clash,
and fetches `origin/<base>` (a stale or missing remote base still
refuses), but creates no worktree, branch, instance, registry entry or
env-file. `-dry-run` on `run` validates address, text, instance,
workdir, host config, mode shape and title, but starts no amp thread
(the AMUX-22 no-spawn contract, enforced by test). Both need the same
grants as the real operation.
