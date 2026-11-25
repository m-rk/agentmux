# Provider probe

`agentmux models list` says an offering is available when its runner and
config are present. That does not mean the provider answers. `agentmux models
probe` checks that directly. It sends each provider tank on this host one tiny
request ("Reply with the single word OK.") on its cheapest model and records
the result. Amp is the exception: it gets no request (see below).

```sh
agentmux models probe [-json] [-host H] [-catalogue PATH]
```

## What is probed

The probe reads the catalogue and makes one probe for each tank configured
on this host (`local` or the host's own name):

| Tank | Runner | Model |
|---|---|---|
| `claude` | claude-code | `haiku`, the CLI's alias for the current Haiku |
| `chatgpt` | codex | the cheapest codex entry, at its lowest effort |
| `amp` | amp | none: `amp usage` only |
| an opencode or kilo provider (such as `ollama`) | opencode or kilo | the cheapest entry for that provider |

"Cheapest" means the lowest tier (easy, then hard, then hardest), and the first
entry in the file breaks a tie.

## How each probe runs

Each try is one headless runner process, run as the current user in a new
temporary directory that is deleted afterwards:

- claude: `claude -p` with `--no-session-persistence`, no tools, no MCP
  servers, no slash commands, and a one-line system prompt.
- codex: `codex exec --json --ephemeral --sandbox read-only`.
- amp: `amp usage`, which runs no model and makes no thread. The Amp phone
  app pushes a notification for every thread turn and cannot mute one
  thread, so an hourly model run would ping the phone every hour. Amp is
  `ok` when it says "Signed in as" and any credit balance it lists is above
  zero. It is `down` with `auth` when it is not signed in or exits with a
  login error, and `down` with `quota` when every balance is at or below
  zero. The "Signed in as" line carries the account email; only the line's
  presence is read and none of the output is kept.
- opencode and kilo: `opencode run --format json` (or `kilo run`). An opencode
  or kilo provider is defined per instance, so the probe borrows the provider
  config of the first instance the catalogue lists for that entry. It reads
  that instance's registry and the provider env overlay (`kilo-env`), then
  writes the config into the temporary directory. It sets
  `OPENCODE_CONFIG` (or `KILO_CONFIG`) to point there, and keeps the run's data
  and state inside that directory too. The instance, its workdir and its
  sessions are never touched.

The probe never launches an instance and never touches a tmux session.

Before amp was checked this way, amp probes continued one standing thread,
named in `~/.local/state/agentmux/amp-probe-thread.json`. If that file is
there, the next probe archives the thread (`amp threads archive`) and
deletes the file. If archiving fails, the file stays and the next probe
tries again.

## Retries and status

A failed try is retried twice, after 5 seconds and then 15 seconds. Each try
times out after 90 seconds. A provider counts as `down` only when all three
tries fail. If any try answers OK, the provider is `ok`.

Each failure is recorded in plain words. The words come from a fixed list:
runner output, paths, hostnames and keys are never stored. The kinds are:

| `error_kind` | `error` |
|---|---|
| `auth` | not signed in, or the login was refused |
| `quota` | usage limit or quota reached |
| `network` | provider unreachable over the network |
| `timeout` | no reply within 90s / the provider timed out |
| `other` | runner exited with status N, reply was not OK, runner is not installed, ... |

## State and `models list`

The results go to `~/.local/state/agentmux/provider-health.json`, one row per
provider and host. `agentmux models list` reads them:

- In the plain-text view, a `probe:` column on each catalogue line shows the
  last probe of that entry's tank on that host.
- `-json` returns them as `providers`, one row for each tank on each host in
  the catalogue:

```json
{"provider": "chatgpt", "label": "ChatGPT", "runner": "codex", "model": "<codex model>",
 "host": "local", "host_label": "hub", "status": "down",
 "checked_at": "2026-11-24T12:00:31Z", "last_ok_at": "2026-11-24T10:00:09Z",
 "down_since": "2026-11-24T11:00:27Z", "error_kind": "quota",
 "error": "usage limit or quota reached", "latency_ms": 900, "attempts": 3}
```

- `status` is `ok`, `down` or `unknown`. `unknown` means never probed, or the
  host could not report; `error` then says why (for example "not probed yet",
  "host does not report probes yet" or "host check failed: forbidden").
- `checked_at` is when the last probe finished, and `last_ok_at` is the last
  time a probe passed.
- `down_since` is the first probe of the current down run. It is cleared when
  the provider is `ok` again.
- `error_kind` and `error` describe the last probe, and are empty when it
  passed.
- `latency_ms` is the time the last try took. `attempts` is how many tries the
  last probe took (1-3).
- A row is never dropped. Like the catalogue, the shape only gains fields.

A remote host probes itself with its own timer. Its gateway returns its rows
in the `runners` op reply (`probes`), so the hub needs only the `runners`
grant it already uses for the catalogue. A gateway older than this feature
returns no `probes`; the hub then shows that host's tanks as `unknown`, with
the error "host does not report probes yet".

## Usage

Every try that reports tokens appends one line to the usage file
(`~/.agents/local/metrics/turn-completions.jsonl`, see
[completed-turn-usage.md](completed-turn-usage.md)). The line has instance
`models-probe`, the runner, the model and the effort, so probe cost shows up in
fuel. Its `turn_id` is `probe:<provider>:<host>:<unix time>:<try>`.

## Timer

`agentmux daemon install` installs the hourly probe:

- Linux: `agentmuxd-probe.service` and `agentmuxd-probe.timer`
  (`OnCalendar=hourly`, with up to 5 minutes of random delay, and no
  catch-up). The service runs as `SUDO_USER`, whose provider logins it checks.
  Install writes and enables these units only when it runs under sudo for a
  non-root user. `agentmux deploy` enables the timer once its unit exists. A
  root gateway reads the probe state of that unit's `User=`.
- macOS: the LaunchAgent `com.m-rk.agentmux.probe`, with `StartInterval` 3600.
