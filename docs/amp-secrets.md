# Headless amp auth with a 1Password access token

`amp login` stores an OAuth session that eventually expires. When its refresh
token is rejected, an amp runner keeps running (agentmux still reports it
`running`) but every thread fails with `Session expired and could not be
refreshed`, and each error is logged to `~/.cache/amp/logs/no-tui.log`.

An Amp access token (`AMP_API_KEY`, `sgamp_…`) does not expire that way and
takes precedence over the stored login. agentmux can inject it into an amp
runner from 1Password without the value touching the registry, tmux, `ps`
output, or the runner's own tool environment.

## Set up an instance

1. Put the access token (Amp → Settings → Security → Access Token) in a
   1Password item and note its vault and item IDs. The field is normally
   `credential`.
2. Confirm the host has the service account token
   (`~/.config/op/service_account_token`, mode 600) and `op` on the run
   user's PATH. See [AGENTS.md](../AGENTS.md#reading-secrets-from-1password).
3. Write an env-file of references, not values, at
   `~/.agentmux/env/<instance>.env`:

   ```sh
   mkdir -p ~/.agentmux/env
   echo 'AMP_API_KEY=op://<vault-id>/<item-id>/<field>' > ~/.agentmux/env/<instance>.env
   chmod 600 ~/.agentmux/env/<instance>.env
   ```

4. Restart the instance: `agentmux control -action restart -instance <instance>`.
   The restart drops any threads the runner is serving; amp has no resume.

Instances without an env-file launch exactly as before.

Provisioning checks the env-file too: on both Linux and macOS, creating (or
re-provisioning) an amp instance whose env-file exists probes `amp usage`
through `op run` with that file, so an instance authenticated by an injected
`AMP_API_KEY` no longer needs a stored `amp login` at creation time either.
The probe needs `op` on the run user's PATH (including `~/.npm-global/bin`,
where `amp` itself lives on hosts that installed it there) and the service
account token at `~/.config/op/service_account_token`.

## Task instances inherit the template's env-file

`sessions create` (a task instance dispatched from a template, e.g.
`task-mergentic-merg-13` from `mergentic-amp`) copies the template's env-file
to `~/.agentmux/env/<new-instance>.env` when the template is an amp instance
with one. The copy holds the same `op://` references, never secret values,
and is written mode 600. That way a per-project amp template authenticated
by 1Password dispatches task instances that authenticate the same way, with
no `amp login` to babysit. The template's amp serving knobs (`--dir`,
`--discover-dirs`, update mode, mode override) are carried over as well.

- A template without an env-file leaves the new instance exactly as before.
- An env-file already present under the new instance's name is never
  overwritten: with a reused instance name `sessions create` reuses the
  instance, and a file already there belongs to whoever put it there.
- Copies are made only when the instance is created, not on reuse.
- Removing a task instance is manual (there is no instance-removal RPC):
  alongside its units and registrations, delete its env-file too —
  `rm ~/.agentmux/env/<instance>.env` as the run user — or the next
  instance to reuse that name inherits the file on disk (creation never
  overwrites it).

## Runner IDs carry the host name

The `--runner-id` is the instance name minus any trailing `-amp` suffix,
plus the host name: `mergentic-amp` on `host-a` registers as
`mergentic-host-a`. Runner IDs must be unique across hosts because
amp's runner registry is shared — a second host registering the same ID
sees its runner exit about a second after starting (code 130, nothing
logged). The ID is computed and stored once, at creation time; runner IDs
stored by earlier agentmux versions keep working unchanged.

## How it works

`agentmux session run` checks for `~/.agentmux/env/<instance>.env`. If present,
it verifies the service account token and `op` exist (failing the unit with a
message if not, rather than starting a session that dies immediately), then
starts tmux with `agentmux session exec --instance <instance>`. That step reads
the service account token and replaces itself with:

```
op run --env-file=<file> -- /usr/bin/env -u OP_SERVICE_ACCOUNT_TOKEN amp --no-tui …
```

- The service account token is passed in the process environment only, never
  through tmux (argv is visible to `ps`; the tmux server's environment
  outlives sessions and would go stale across restarts).
- `env -u` removes the token before amp starts, so amp and the tools it spawns
  do not see it. `op run` itself holds it while it runs.
- The env-file lives outside the registry because `agentmux new -y` rewrites
  the registry wholesale and would drop a hand-added field.

## Troubleshooting

- Instance fails to start with `1Password service account token …`: the token
  file is missing or empty.
- Session exits immediately after starting: the `op://` reference is wrong
  (vault, item, or field) or the item is not shared with the service account.
  Check it without printing the value:
  `OP_SERVICE_ACCOUNT_TOKEN=$(cat ~/.config/op/service_account_token) op run --env-file="$HOME/.agentmux/env/<instance>.env" -- sh -c 'test -n "$AMP_API_KEY" && echo ok'`.
- Do not run `agentmux session exec` by hand to debug: it starts a real
  runner.
