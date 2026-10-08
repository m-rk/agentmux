# Codex runner

`codex` is a headless agentmux runner shaped like [amp](amp-run.md): a
detached `codex exec --json` writes a JSONL stream to a log, state is read
from the log, and a continue resumes the same thread. There is no resident
codex process and no TUI; the instance keeps only a placeholder tmux session.

## Setup

1. Install the `codex` CLI for the run user and log in (see Auth).
2. Fix the sandbox if the host is Ubuntu 24.04+ — see
   [codex-sandbox.md](codex-sandbox.md). `agentmux doctor` runs the self-test.
3. Create an instance: `agentmux new -y -agent codex -model MODEL ...`
   (`-model` becomes the instance's `AGENTMUX_MODEL`; leave it unset for
   codex's own default). Task instances come from `sessions create` with a
   codex template, like any agent.
4. Run a prompt (the prompt always comes from `-file`, never argv):

```sh
agentmux sessions run -file prompt.md [-model M] [-effort E] [-sandbox MODE] task-1@build-box
agentmux sessions run -file followup.md -thread THREAD_ID task-1@build-box
agentmux sessions status 'task-1@build-box#THREAD_ID'
agentmux sessions read   'task-1@build-box#THREAD_ID'
```

A run launches `codex exec --json -C <worktree> -s <sandbox> [-m M] [-c
model_reasoning_effort=E] -` with the prompt on stdin from a private file. A
continue is the same with `resume <thread_id>` before the trailing `-`. Output
is appended to `~/.local/state/agentmux/sessions/<instance>/codex-run-<thread>.jsonl`,
bracketed per run by `agentmux.start` / `agentmux.exit` records.

## State

Each `thread.started` / `agentmux.start` begins a segment. `turn.completed` is
`done`; `turn.failed` is `failed` with the flattened error (`rate_limited` for a
429 or a rate/usage limit); a bare `error` event is not terminal; an
`agentmux.exit` with no result is `failed`; no event for 10 minutes while
running reports `stalled`. A continue of a still-working thread is refused
(`busy`).

## Send

`agentmux send` to a codex worker never pastes into a terminal. A healthy
running turn gets the message through `codex queue` and keeps running; a stalled
turn is stopped and replaced by `codex exec resume`; an idle, done or failed
thread is resumed. A queued message is guaranteed to arrive with the next
continue (probed headless: `codex queue --thread ID --message TEXT` persists it
and the next `exec resume` answers it); whether a running `exec` picks it up
mid-turn is not proved. `-doorbell` is unsupported.

## Auth

One ChatGPT login per run user, shared by all of that user's codex instances
through `$CODEX_HOME`. `agentmux auth status -all` reports it using `codex
login status` only; agentmux never reads `auth.json`. `agentmux auth login` is
claude-only; re-auth codex with `codex login --device-auth` as the run user (it
prints a URL and a one-time code, no pasted code needed — see AGENTS.md).
`OPENAI_API_KEY` and `CODEX_API_KEY` are dropped from the run child so a run can
never silently switch the account to metered billing.

## Sandbox and approvals

Default `workspace-write`; `read-only` is allowed. `danger-full-access` and the
bypass flags are refused unless the instance registry has
`AGENTMUX_CODEX_ALLOW_UNSAFE_SANDBOX=1` (set by hand; never copied from a
template). Codex has no per-file allowlist, so `-allow-file` is unsupported
(create warns). The task-note vault directory is granted with `--add-dir` from
the registry key `AGENTMUX_CODEX_ADD_DIRS` (comma-separated absolute paths,
copied from the template by create, which also appends the git dirs a commit
writes so the worker can commit in its worktree: the worktree's own admin dir
and the shared `objects`, `refs` and `logs` dirs; put the mergentic state dir
in the template's value). Codex's workspace-write also makes the
system temp directory writable. Task instances carry the liveguard identity, so
they cannot reach live Discord or the live daemon.

### Committing from the sandbox

Codex (0.160.x) workspace-write keeps any directory named `.git` read-only
even when it is a writable root, so granting the repo's shared `.git` does not
let a worker commit (`index.lock: Read-only file system`). Proved with `codex
exec -s workspace-write` against a scratch repo: `--add-dir` of `.git` fails;
`--add-dir` of `.git/worktrees/<name>`, `.git/objects`, `.git/refs` and
`.git/logs` commits and moves the branch ref; a `git clone
--separate-git-dir=<path not named .git>` checkout with that path as the root
also commits. The subdirectory grant keeps the worktree layout, and `.git/config`
and `.git/hooks` stay read-only (a worker's write to either fails), so it
cannot change config or plant a hook. Existing instances keep the add-dirs they
were created with; recreate them to pick up the grant.

## Spend

Usage draws on the logged-in ChatGPT plan's quota and rate limits, not
per-token credits — a shared, finite allowance. Tests and deploy never make
live model calls (see the fake codex below); the only live path is the opt-in
`agentmux deploy -codex-live`. Per-turn usage appears in `turn.completed` but is
not accounted by agentmux yet.

## Retire

`agentmux retire` on a codex task instance stops its in-flight `codex exec`
children (matched by the instance identity in the process environment plus the
worktree as cwd) before the worktree goes. The branch is kept unless merged,
like any agent, and codex's own rollout files under
`$CODEX_HOME/sessions/` are kept, like Claude transcripts. Archiving the thread
with `codex archive <id>` is left to the operator.

## Deploy smoke

`agentmux deploy` includes a codex dry run on each host that has a codex
instance: a dry-run `sessions run` of the smoke name validated against that
instance. It starts no process and spends nothing. Hosts without a codex
instance, or whose gateway grants no run for the smoke name, are skipped.
`agentmux deploy -codex-live` additionally runs one tiny read-only, low-effort
turn on the local host's first codex instance and waits for it to finish `done`.

## Fake codex

`daemon/testdata/fakecodex/codex` is a test double that prints canned,
synthetic `--json` JSONL and never touches a network or a model. Select a
scenario with `FAKE_CODEX_SCENARIO=success|turn_failed|error_item|rate_limit|hang`
(`FAKE_CODEX_HANG_SECONDS`, `FAKE_CODEX_THREAD_ID`, `FAKE_CODEX_QUEUE_LOG`
tune it). Tests put it on the run PATH. Task instances also get a wrapper that
refuses a real `codex exec` unless `AGENTMUX_ALLOW_LIVE_CODEX=1`.

## Spike notes (codex-cli 0.160.x)

- `codex exec --json` emits `thread.started{thread_id}`, `turn.started`,
  `item.completed`, `turn.completed{usage}` and `turn.failed{error}`; the exit
  code is non-zero on failure. Always give a prompt or `</dev/null`; stdin is
  read when there is none.
- `codex exec resume <id>` continues with context and emits a new
  `thread.started` with the same id, so a log with several segments looks like
  amp's.
- Effort is `-c model_reasoning_effort=...`; valid models and levels are
  per-model (codex's own cache), so no model ids live in agentmux.
- Saved sessions live at `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl`;
  `codex archive|delete|unarchive <id>` manage them.
- A model the account may not use fails with a 400 `invalid_request_error`.
- A rate-limit event shape was not provoked on purpose; the parser treats 429
  and rate/usage-limit text as `rate_limited`.
- TUI codex instances are not supported (a pasted prompt needs a delay before
  Enter, and footer markers were not captured).
