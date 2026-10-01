# Paseo daemon updates

`agentmux paseo update` keeps the Paseo daemon (the `@getpaseo/cli` npm
package) on the latest **stable** release, and undoes the update if the daemon
doesn't come back healthy.

```sh
agentmux paseo update -check     # report only, change nothing
agentmux paseo update            # update now (restarts the daemon)
sudo agentmux paseo update install -run-user USER   # Linux: daily systemd timer
agentmux paseo update install                       # macOS: daily LaunchAgent (no sudo)
```

## What one run does

1. Compare `paseo --version` with `npm view @getpaseo/cli@latest version`.
   Equal or older: silent no-op. Prereleases (`-beta`) are never followed, and
   it never downgrades.
2. `npm install -g @getpaseo/cli@<new>`, then restart the daemon: the
   `paseo-daemon.service` unit on Linux if it exists (the timer runs as root
   for this; npm/paseo still run as `-run-user`), otherwise
   `paseo daemon stop && start`.
3. Healthy means `paseo daemon status --json` reports `localDaemon: running`
   **and** `daemonVersion` equal to the new version, then still running after a
   25 s settle window (this catches a systemd crash loop).
4. On any failure: reinstall the exact previous version, restart, re-verify.
5. Discord message (the run user's webhook, same as other agentmux alerts) on
   update, on rollback, and loudly if the rollback also fails. Nothing is sent
   when already current. A missing webhook is logged, never fatal.

A daemon managed by the Paseo desktop app (`desktopManaged: true`) is skipped;
update it from the app.

## Caveats

- The restart interrupts running Paseo agents. The default time is 04:00
  (`-at HH:MM`; Linux uses the same Australia/Perth convention as the doctor).
- Rollback restarts the *previous* version under the *current* service unit.
  Paseo 0.10 removed `paseo daemon start --foreground`, so the unit must use
  `paseo daemon run --home ~/.paseo`; check that form also works on the old
  version before relying on rollback across that boundary.
- Exit code is non-zero when a rollback happened, so `systemctl status
  agentmux-paseo-update.service` shows the last failure.
