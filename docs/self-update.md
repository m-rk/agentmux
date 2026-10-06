# Pull-based self-update (Mac)

The Mac pulls its own builds: the hub gets no access into it. A
per-user launchd job, `com.m-rk.agentmux.self-update`, runs
`agentmux self-update run` every 10 minutes (600 seconds). The run
fetches `origin/main` for agentmux and mergentic, and when the
orchestrator's shipped commit moved, builds in a temporary worktree
and installs atomically (temp file, then rename). See AMUX-29.

- agentmux: install to the pinned daemon bin (`~/.agentmux/bin`),
  rewrite the daemon and gateway plists, kickstart both, then smoke
  check. mergentic: install into `~/.local/bin` and the agents local
  bin, then `--version` check. No restart: it is a CLI, not a service.
- The ship gate: install only a commit the orchestrator recorded as
  shipped, not every push to main. The hub publishes it with
  `agentmux sessions ship <host> agentmux@<sha> mergentic@<sha>`
  (through the gateway), and the run installs up to that commit.
- Safety: only commits on `origin/main` (`merge-base --is-ancestor`
  proves it); only when `go vet` and the build pass; the previous
  binary stays as `<bin>.prev` for rollback; each event logs one line
  to `~/.agentmux/self-update/update.log`; a local session that is
  mid-deploy holds `deploy.lock` (`agentmux deploy begin|end`) and the
  run skips while it exists.
- Smoke test and rollback: after installing and restarting, the same
  dry-run create+run the hub deploy uses must pass. On failure the
  kept previous binary goes back, services restart, and the failure
  logs. The Mac is never left on a failed build.
- Visibility: the installed commit per repo lives in
  `~/.agentmux/self-update/versions.json`; the gate in `shipped.json`.
  The gateway serves both plus the log tail, so the orchestrator can
  confirm "the Mac is on `<sha>`" or warn when it lags by more than 30
  minutes — with no access into the Mac:
  `agentmux sessions versions <host>`,
  `agentmux sessions selfupdate-log <host>`.
  On success the run writes one `deployed <repo>@<sha>` line the hub
  reads.

## Host setup (once, by hand on the Mac)

No personal paths, hostnames or usernames live in the repo; configure
them on the host. The URLs and checkout dirs below are examples —
substitute the real ones:

```sh
agentmux self-update install \
  -agentmux-url https://github.com/example/agentmux.git \
  -mergentic-url https://github.com/example/mergentic.git \
  -agentmux-dir /home/me/src/agentmux \
  -mergentic-dir /home/me/src/mergentic \
  -agents-bin-dir /home/me/.local/bin
```

`-print` shows the plist without installing it. `self-update status`
prints installed vs shipped per repo on the host itself.

## Hub flow (after every merge to main)

1. The orchestrator records the shipped commit per repo:
   `agentmux sessions ship <host> agentmux@<sha> mergentic@<sha>`.
2. Within 10 minutes the launchd job installs it and writes
   `deployed <repo>@<sha>` to its log.
3. The orchestrator confirms with `agentmux sessions versions <host>`
   (or greps `sessions selfupdate-log <host>` for the deployed line),
   and warns when shipped stays ahead of installed for over 30
   minutes.

Follow-up once this works (not here): remove the "deploy pending"
lines and the console observer's Mac-deploy watch.
