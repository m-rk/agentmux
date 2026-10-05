# Starting amp threads

`agentmux sessions run` starts an amp thread on an amp instance — a prompt
run through the amp CLI in the instance's workdir — and returns as soon as
the thread exists, while the agent keeps working. Continuing a thread
relaunches it the same way:

```sh
agentmux sessions run -file prompt.md site-amp@build-box
agentmux sessions run -file prompt.md -title "AMUX-17 do the thing" site-amp@build-box
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
`running`, `done`, `waiting` (with the pending question under
`waiting_on`), or `failed` with a reason — and `sessions read` for the
transcript. Both read the thread's own stream log under the state dir
(`~/.local/state/agentmux/sessions/<instance>/`), so they work even when
the amp CLI or its auth is gone.

A refusal exits 1 (`-json` reports `{"ok": false, ...}` instead): `invalid`
(bad thread id, empty prompt, a bad title, or a mode amp rejects — quoted
from amp's own error), `not_found`, `unsupported` (not an amp instance),
`failed` (the CLI died before printing its init record). Exit 2 is usage.

## Thread titles

`-title "<task id> <task name>"` names a new thread in the amp sidebar
(the dispatcher passes the task's id and name, e.g. `-title "AMUX-17 do the
thing"`), where amp's own summary would otherwise say nothing about which
task the thread belongs to. It is ignored when continuing a thread. The
title rides `amp -x --title` with `--no-archive-after-execute`, so a
finished thread stays unarchived — findable and renamable — instead of
vanishing into the archive. Once the thread exists the title is re-applied
with `amp threads rename`, since amp may retitle the thread itself while
the agent works; the rename is best-effort and never fails the run. amp
refuses to rename archived threads, so an archived task thread keeps
whatever title it had. Archive task threads when the task note is
archived, not before.

## Threads stay unarchived

Every run — new or continued — carries `--no-archive-after-execute`, so a
finished thread stays in the active list instead of vanishing into the
archive. Continuing also unarchives the thread first (`amp threads archive
--unarchive <id>`), since amp archives a thread when an `-x` run ends and
continuing an archived thread refuses with "This thread is archived and
cannot be continued". Unarchiving an active thread succeeds, so the
continue does it unconditionally rather than detecting the archived state
first.

## Waiting on a question

An amp agent can call amp's built-in `ask_user_choice` tool to ask a
multiple-choice question. In `-x` mode nothing can answer that dialog, so
the run just waits: the stream log ends at the assistant's `tool_use`
record with no result record after it. `sessions status -json` on the
thread then reports the thread-level `state` as `running` with the run as
`waiting` and the question under `waiting_on`:

```json
{"state": "running", "run": {"state": "waiting", "waiting_on": {
  "tool": "ask_user_choice", "tool_use_id": "TU-…",
  "question": "Tabs or spaces?", "options": ["Tabs", "Spaces"],
  "allow_other": true}}}
```

The human-readable status prints the question with its numbered options,
and `sessions read` appends a closing assistant message naming the
question and options. Answer with a continue — `sessions run -thread
<id> -file <answer>`, where the file holds the choice (or free text when
`allow_other` is set). The continue stops the stuck run process first
(nothing else can answer the pending dialog) and unarchives the thread,
then relaunches it with the answer.

## Failed runs

A run whose stream log ends in a `result` record with subtype
`error_during_execution` (or `error_max_turns`) — `is_error: true` with
the message in the record's `error` field — reports `state: failed` with
that error as the reason, even while amp itself still shows the thread as
`running_tools`: the stream log's final record is authoritative, not
amp's last-known agent state. A run killed by hand (for example answering
a pending question by killing the stuck process, unarchiving, and
continuing) ends the same way, with an error like `User cancelled
(SIGINT/SIGTERM)`. Poll `sessions status` for `running` / `done` /
`waiting` / `failed` rather than trusting the thread's agent state.

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
