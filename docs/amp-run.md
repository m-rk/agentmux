# Starting amp threads

`agentmux sessions run` starts an amp thread on an amp instance — a prompt
run through the amp CLI in the instance's workdir — and returns as soon as
the thread exists, while the agent keeps working. Continuing a thread
relaunches it the same way:

```sh
agentmux sessions run -file prompt.md site-amp@build-box
agentmux sessions run -file prompt.md -title "AMUX-17 do the thing" -label agentmux-task site-amp@build-box
agentmux sessions run -file followup.md -thread T-11111111-1111-4111-8111-111111111111 -title "AMUX-17 do the thing" -label agentmux-task site-amp@build-box
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

`-dry-run` validates everything a real run would — address, text,
instance, workdir, host config, mode shape, thread id and title — but
starts no amp thread; see [Deploy](deploy.md). `-template NAME`
(dry-run only) validates against an existing amp instance instead of
the target, for targets that don't exist yet.

## Thread titles and labels

`-title "<task id> <task name>"` names the thread in the amp sidebar
(the dispatcher passes the task's id and name, e.g. `-title "AMUX-17 do the
thing"`), where amp's own summary would otherwise say nothing about which
task the thread belongs to. amp's auto-title replaces `--title` with its
own summary while the agent works, so the title is re-applied with
`amp threads rename` after the first assistant turn, on every resume, and
when the run ends — before any archiving, since amp refuses to rename an
archived thread. The rename is best-effort and never fails the run. The
title rides `amp -x --title` on a new thread with
`--no-archive-after-execute`, so a finished thread stays unarchived —
findable and renamable — instead of vanishing into the archive. The
prompt itself also opens with the title as a one-line header, since the
kickoff notification shows the first message rather than the sidebar
title. There is no amp setting that stops auto-titling; the re-apply is
the mechanism. The launch-time rename runs before `sessions run`
returns; a second rename runs in the background once the agent's first
assistant record lands in the stream log — the point where the
overwrite happens — so the thread still reads `<ID> <title>` an hour
after launch. Archive task threads when the task note is archived, not
before.

`-label X` (repeatable) tags the thread so `amp threads list --label X`
finds it — labels, not projects, are the filter for local threads, since
`--project` only applies to orb (cloud) runs. Every `-label` rides
`amp -l` on the first run and on every `threads continue`, which adds the
label to the existing thread. Junk labels (empty, overlong, or carrying
line breaks, NUL bytes, or commas) are dropped, never a refusal. Labels
are the dispatcher's to set; the example `agentmux-task` above stands in
for whatever label the dispatcher passes.

## Threads stay unarchived

Every run — new or continued — carries `--no-archive-after-execute`, so a
finished thread stays in the active list instead of vanishing into the
archive. Continuing also unarchives the thread first (`amp threads archive
--unarchive <id>`), since amp archives a thread when an `-x` run ends and
continuing an archived thread refuses with "This thread is archived and
cannot be continued". Unarchiving an active thread succeeds, so the
continue does it unconditionally rather than detecting the archived state
first.

## Relaying into a worker thread

`sessions send` to an amp instance is a `run` continue under another
name: it resumes the instance's thread — the address's thread suffix,
else the instance's current thread (newest run log, else the newest
thread listed on its runner) — with the host/instance mode, carrying the
provenance prefix as its text. Nothing ever pastes into the runner's
terminal, so a send can never start a new thread on amp's default model
(AMUX-37). With no thread to resume the send is refused as `not_found`
with the `sessions run` to use instead; `-doorbell` is refused for amp.
The result names the resumed thread and `confirmed` means its run
reports running. For amp workers, relays and nudges must use this path
(or `sessions run` directly), never a terminal paste.

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

No amp process starts without `-m` (AMUX-36): every thread — `sessions
run` and continues, the runner unit, the nightly review, the doctor
escalation — carries the host mode, and a missing mode everywhere is a
refusal, never a silent run on amp's default model. The value is a
built-in (`low`, `medium`, `high`, `ultra`) or a
plugin mode by key or label. Malformed values (overlong, or carrying line
breaks or NUL bytes) are refused before anything spawns; anything else
rides the real run's `-m`, and an unknown mode fails there — the run is
refused quoting amp's own error, with no throwaway check thread first.

- Host default: `~/.config/agentmux/amp.yaml`, with `mode: <amp mode key or
  label>`. Docs and tests use placeholders such as `high` — never put a
  real user's mode name in the repo.
- Per-instance override: `AGENTMUX_AMP_MODE`, set by
  `agentmux new -amp-mode <mode>`. A task instance created by
  `sessions create` copies the template's override. Wins over the host
  file.
- Per-run override: `sessions run -mode <mode>`, for one run only. Wins
  over both; a person can always name the mode explicitly.

`sessions status -json` on an amp instance reports the effective mode as
`amp_mode: {"mode": ..., "source": "instance"|"host"}` (absent when none is
configured anywhere); the human-readable status prints it as
`amp_mode <mode> (from <source>)`.

The long-lived `--no-tui` runners also carry `-m` (the unit's command
line includes it): every runner process starts with the host/instance
mode. Whether threads created from that runner's terminal inherit it is
unconfirmed from the runner side — verify with `amp threads export` and
rely on `sessions run` continues, which always pass `-m` themselves.

Task sessions cannot run amp directly: a stub `amp` sits first on PATH
in task workers' panes and refuses (`agentmux starts amp for you; test
with fakes or -dry-run`). Real runs are started by agentmux, not by the
worker.

amp has no default-mode setting (checked 2026-10-06: only
`amp.updates.mode` and `amp.defaultVisibility` exist in its settings
file, and `-m` is per run). Re-check after each amp upgrade, and use a
default setting if one appears.
