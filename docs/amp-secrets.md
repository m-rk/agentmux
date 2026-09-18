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
