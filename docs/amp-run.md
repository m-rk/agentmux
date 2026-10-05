# Starting amp threads

`agentmux sessions run` starts an amp thread on an amp instance — a prompt
run through the amp CLI in the instance's workdir — and returns as soon as
the thread exists, while the agent keeps working. Continuing a thread
relaunches it the same way:

```sh
agentmux sessions run -file prompt.md site-amp@build-box
agentmux sessions run -file followup.md -thread T-11111111-1111-4111-8111-111111111111 site-amp@build-box
echo "do the thing" | agentmux sessions run -file - site-amp@build-box
```

The prompt always comes from `-file` (`-` for stdin), never from the
command line, so shell history can't leak it. The reply names the thread:

```text
thread   site-amp@build-box#T-11111111-1111-4111-8111-111111111111
url      https://ampcode.com/threads/T-11111111-1111-4111-8111-111111111111
state    running
```

`state` is always `running` here: the agent was just launched (or
relaunched for a continue). Poll `sessions status` for what happens next —
`running`, `done`, or `failed` with a reason — and `sessions read` for the
transcript. Both read the thread's own stream log under the state dir
(`~/.local/state/agentmux/sessions/<instance>/`), so they work even when
the amp CLI or its auth is gone.

A refusal exits 1 (`-json` reports `{"ok": false, ...}` instead): `invalid`
(bad thread id, empty prompt, or a mode amp rejects — quoted from amp's
own error), `not_found`, `unsupported` (not an amp instance), `failed`
(the CLI died before printing its init record). Exit 2 is usage.

Through the gateway it is the `run` op, granted like `send` (it starts an
agent that will act on text it is given, and shares `send`'s rate bucket).
The grant is checked against the session without its `#thread`: a thread
suffix never widens access. See [Gateway](gateway.md).

## The amp mode

Every run uses one amp mode (`amp -m`), chosen per host and kept out of
the repo. The value is a built-in (`low`, `medium`, `high`, `ultra`) or a
plugin mode by key or label; amp validates it at run time, and an unknown
mode refuses the run quoting amp's error. With no mode configured anywhere,
runs omit `-m` and amp uses whatever it would by default.

- Host default: `~/.config/agentmux/amp.yaml`, with `mode: <amp mode key or
  label>`. Docs and tests use placeholders such as `high` — never put a
  real user's mode name in the repo.
- Per-instance override: `AGENTMUX_AMP_MODE`, set by
  `agentmux new -amp-mode <mode>`. A task instance created by
  `sessions create` copies the template's override. Wins over the host
  file.

`sessions status -json` on an amp instance reports the effective mode as
`amp_mode: {"mode": ..., "source": "instance"|"host"}` (absent when none is
configured anywhere); the human-readable status prints it as
`amp_mode <mode> (from <source>)`.

The long-lived `--no-tui` runners do not get `-m`: amp has no default-mode
setting for remote threads (its `--no-tui` takes no `-m`), so runs start
their own CLI with the mode rather than reconfiguring the runner.
