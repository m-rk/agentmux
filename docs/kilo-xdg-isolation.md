# Isolate Kilo data and state per instance

Kilo normally stores every session for one OS user in the same SQLite database
under `XDG_DATA_HOME`, with locks and other process state under
`XDG_STATE_HOME`. Two agentmux instances running as that user therefore write
to the same database even when their workdirs are different.

agentmux can give each prepared Kilo instance private data and state roots:

```text
~/.agentmux/<instance>/.kilo-home/data
~/.agentmux/<instance>/.kilo-home/state
```

Kilo may create its own `kilo/` subdirectory beneath each XDG root. Global
configuration and cache remain shared: agentmux does not set
`XDG_CONFIG_HOME` or `XDG_CACHE_HOME`.

## Why cutover is explicit

The shared Kilo database contains both session history and login credentials.
Pointing an existing instance at an empty directory would make it look like a
fresh, logged-out installation. It could also cause agentmux to seed a new
session instead of resuming the conversation the operator intended.

Installing a newer agentmux binary does not enable isolation by itself. An
instance keeps using Kilo's legacy shared paths until this regular file exists:

```text
~/.agentmux/<instance>/.kilo-home/.xdg-isolation-ready
```

Once the marker exists, session lookup, first-session seeding, scheduled
version/update checks, and the long-running tmux server all receive the same
per-instance `XDG_DATA_HOME` and `XDG_STATE_HOME`. The directories are forced
to mode `0700` before use.

## Migrate one instance

Do this on the host as the instance's run user, one instance at a time. The
commands below use shell variables to keep the paths readable; replace the
placeholder values before running them.

1. Pick the instance and confirm its workdir and run user from `agentmux list
   -json`. Use `agentmux view -instance NAME` to make sure it is not in the
   middle of work.

2. Stop the instance's automatic updater before deploying the new binary.
   On Linux:

   ```sh
   instance_name="NAME"
   sudo systemctl disable --now "agentmux-${instance_name}-update.timer"
   ```

   This does not stop the live tmux session. Leave the timer disabled until the
   isolated instance has been verified.

   On macOS, unload only the update LaunchAgent:

   ```sh
   instance_name="NAME"
   launch_domain="gui/$(id -u)"
   update_plist="$HOME/Library/LaunchAgents/com.agentmux.${instance_name}.update.plist"
   launchctl bootout "$launch_domain" "$update_plist"
   ```

3. Export the session or sessions belonging to this instance from the shared
   database using Kilo's supported commands. Treat the export as sensitive
   conversation data:

   ```sh
   instance_workdir="/absolute/path/from-agentmux-list"
   migration_dir="$(mktemp -d)"
   chmod 700 "$migration_dir"
   cd "$instance_workdir"
   kilo session list --format json
   kilo export SESSION_ID >"$migration_dir/thread.json"
   chmod 600 "$migration_dir/thread.json"
   ```

   Select sessions whose `directory` matches `instance_workdir`. Export every
   conversation that must remain available after cutover.

4. Prepare private XDG roots and populate the isolated login. Re-authenticating
   under the isolated environment is the safest supported route:

   ```sh
   kilo_root="$HOME/.agentmux/${instance_name}/.kilo-home"
   install -d -m 700 "$kilo_root" "$kilo_root/data" "$kilo_root/state"
   env XDG_DATA_HOME="$kilo_root/data" \
     XDG_STATE_HOME="$kilo_root/state" \
     kilo auth login
   ```

   If interactive re-authentication is not acceptable, use a separately
   reviewed procedure for the installed Kilo version. Do not paste tokens into
   shell commands or copy private credential-table rows with ad hoc SQL: those
   schemas can change, and failures may print credential values. agentmux does
   not automate that unsafe transfer.

5. Import and verify the exported sessions under the isolated environment:

   ```sh
   env XDG_DATA_HOME="$kilo_root/data" \
     XDG_STATE_HOME="$kilo_root/state" \
     kilo import "$migration_dir/thread.json"
   env XDG_DATA_HOME="$kilo_root/data" \
     XDG_STATE_HOME="$kilo_root/state" \
     kilo session list --format json
   env XDG_DATA_HOME="$kilo_root/data" \
     XDG_STATE_HOME="$kilo_root/state" \
     kilo debug paths
   ```

   Do not continue until authentication succeeds, the intended session IDs are
   present, and the debug output points data and state at this instance's
   `.kilo-home` directory.

6. Create the readiness marker, then restart only this instance:

   ```sh
   install -m 600 /dev/null "$kilo_root/.xdg-isolation-ready"
   agentmux control -instance "$instance_name" -action restart
   ```

   The Remote Control client will disconnect briefly. Confirm the same session
   and history are available with `agentmux view -instance NAME` and the Kilo
   client before touching another instance.

7. Restore the automatic updater after the instance is healthy. On Linux:

   ```sh
   sudo systemctl enable --now "agentmux-${instance_name}-update.timer"
   ```

   On macOS:

   ```sh
   launchctl bootstrap "$launch_domain" "$update_plist"
   ```

Keep the protected export until the instance has run successfully through a
normal maintenance restart, then remove it using your usual secure-data
handling practice.

## Roll back

The legacy shared database is not modified by activating isolation. To return
an instance to it, first export any conversations created after cutover, remove
the readiness marker, and restart that instance. The isolated database also
remains on disk, so neither side is deleted automatically; recent writes are
not synchronized between the two databases.
