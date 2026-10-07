# Codex as an agentmux runner — spike findings

Status: plan only, no product code. Probed with `codex-cli 0.160.1` on a
Linux test host (aarch64, Ubuntu 24.04), logged in with a ChatGPT account.
Probes were small prompts in a scratch git repo; no auth files were read.

## 1. How agentmux models a runner today

There is no single "runner" type. An agent kind is a string in the instance
registry (`AGENTMUX_AGENT`; empty means `claude-code`) and each concern
switches on it separately:

| Concern | Where | Notes |
|---|---|---|
| Kind default | `session.agentOf`, `discovery` (`internal/discovery`), `ops.Create` | empty → `claude-code` |
| Create / provision | `internal/provision/provision.go`, `provision/amp*.go`, `cmd/agentmux/wizard.go`, `wizardui` | per-agent units, launch command, config files |
| Start / update / stop | `session.Run/Update/Stop` (`session/session.go`) → `RunClaudeCode`, `RunAgentmux` (zero, opencode, kilo), `RunAmp` | amp's `RunAmp` is a *headless runner*, not a TUI |
| Task instances | `ops.Create` (worktree + registry from a template), `session.taskSessionEnvArgs`, `taskAmpStub*` | |
| Allow-files | `allowfiles.Supported`, `allowfiles/{claude,opencode,kilo}.go`, `session.prepareClaudeAllowSettings`, `allowFilesEnv` | codex unsupported → provisioning warns |
| Send | `ops.Send` → `sendTmux` (TUI agents, `safesend.Classify` with per-agent footer markers) or `sendAmpResume` (amp) | amp send = `ops.Run` continue |
| Headless run | `ops.Run`, `session/amprun.go` (`StartAmpRun`, `AmpRunStateOf`, `AmpRunLogStalled`, `StopAmpRun`) | log-file based state: running/done/failed/waiting |
| Status / transcript | `internal/transcript` (`register("claude-code", ...)`, opencode, amp readers) | |
| Thread watch | `threadwatch.newCollectorForAgent` (claude-code, amp, opencode) | others fall back to the pane |
| Health | `dailycheck/health.go` per-agent pane checks, `processIdentityIssue` (process name must contain agent name) | |
| Retire | `retire.State.Plan`, `retire/env.go`, `retire/retire.go` | amp archives threads; others stop + remove |
| Wire | `daemon/proto/agentmuxd.proto` agent comment, `pb` | |
| Dispatch side | mergentic `internal/dispatch/run.go` `agentmuxAgents`, `Efforts` (`low…max`), `CreateSpec.Model` | |

The amp runner is the closest template for codex task instances: a detached
non-interactive CLI writing a JSONL stream to a log, state read from the log,
resume by thread id, send = new turn.

## 2. What codex offers (probed)

- **Non-interactive**: `codex exec [PROMPT|-]`, `--json` emits JSONL:
  `thread.started{thread_id}`, `turn.started`, `item.completed{item:{type:
  agent_message|file_change|error|…}}`, `turn.completed{usage}`,
  `turn.failed{error}` / `error`. Exit code is non-zero on failure (a bad
  model exits 1 with `turn.failed`). `-o FILE` writes the last message;
  `--output-schema` constrains it. stdin is read when no prompt is given, so
  always pass `</dev/null` or a prompt.
- **Resume**: `codex exec resume <thread_id> "prompt"` (also `--last`)
  continues with context and emits a fresh `thread.started` with the same id —
  so a log with several segments looks exactly like amp's (init per segment).
  Verified. `codex exec fork` exists. Interactive: `codex resume`, `codex fork`.
- **Interactive**: `codex` TUI (`--no-alt-screen` keeps scrollback in tmux).
  Footer shows `<model> <effort> · <cwd>`. In a tmux probe a pasted prompt did
  not submit on a same-instant `Enter` (paste-burst handling), so sends need a
  short delay before Enter. Busy/prompt footer markers were not captured —
  that needs a real turn in a follow-up.
- **Queue**: `codex queue --thread <id> --message TEXT` queues a message for an
  existing session (talks to the shared app-server daemon; `codex agents`,
  `app-server`, `remote-control` exist). Not tried; a candidate for a real
  steer/queue send instead of abort-and-resume. Needs a proof task.
- **Model / effort**: `-m MODEL`; effort via `-c model_reasoning_effort=low|
  medium|high|xhigh|max|…` (works; the TUI footer reflected it). Valid levels
  and model ids are per-model, listed in codex's own models cache; the
  dispatch level set `low/medium/high/max` maps onto it, with `xhigh`/`ultra`
  out of reach unless mergentic widens `Efforts`.
- **Sandbox/approvals**: `-s read-only|workspace-write|danger-full-access`,
  `-a/--ask-for-approval`, `--add-dir DIR` (extra *writable* dir),
  `--dangerously-bypass-approvals-and-sandbox`, `--approve-for-me`, `-C DIR`,
  `--worktree`, `--ephemeral`, `--ignore-user-config`, `--ignore-rules`,
  `--strict-config`, `-p PROFILE` (`$CODEX_HOME/<name>.config.toml`).
- **Config**: `$CODEX_HOME/config.toml` (absent on the test host; defaults
  apply), `-c key=value` overrides (TOML), execpolicy `.rules` files, project
  `AGENTS.md`. No per-file read/write allowlist like Claude settings or the
  opencode config — only directory-level `--add-dir` and sandbox mode.
- **State**: `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<time>-<id>.jsonl` plus
  sqlite stores. `codex archive|delete|unarchive <id>` manage saved sessions.
- **Done / error / rate limit**: done = `turn.completed` (usage per turn);
  failure = `turn.failed` with a JSON error string (status + type + message);
  a model not allowed for the account is a 400 `invalid_request_error`.
  A rate-limit event was not provoked (would burn quota); its shape must be
  captured from a real occurrence or documented source before the state
  parser maps it to a retryable `rate_limited`.
- **Auth**: `codex login status` → "Logged in using ChatGPT"; `codex login
  --device-auth`, `--with-api-key`, `--with-access-token`. Credentials live in
  `$CODEX_HOME/auth.json` (never read here).

### Finding that blocks task instances on the test host

The bwrap sandbox does not work there: `bwrap: loopback: Failed RTM_NEWADDR:
Operation not permitted`. With `read-only` and `workspace-write` every shell
command and every file write failed ("Operation not permitted"). Ubuntu 24.04
restricts unprivileged user namespaces (AppArmor); confirmed and fixed in
[codex-sandbox.md](../codex-sandbox.md) (AMUX-54).
Until fixed, codex there can only talk, not work, unless run with the
sandbox bypass flag.

## 3. Gaps and decisions

1. **Shape of a task instance.** Recommend the amp pattern: `ops.Run` launches
   `codex exec --json` detached in the worktree, logging JSONL under the
   instance state dir; continue = `codex exec resume <id>`. State machine over
   the log: `turn.completed` → done, `turn.failed`/`error` → failed (with
   reason), process gone with no terminal event → failed, no event for 10 min
   → stalled (reuse the amp mtime check). Human-facing instances use a tmux
   TUI like kilo/opencode, as a second step.
2. **Prompt.** Pass via stdin/`-` or file (avoid argv size and `ps` exposure),
   as amp does with `-file`.
3. **Allowlist.** No per-file mechanism. Options: (a) leave allow-files
   unsupported for codex at first (provision warns) and give task workers
   `--add-dir` for the one directory they must write (the task-note vault
   writes go through the `mergentic` CLI, which runs *inside* the sandbox, so
   the vault directory must be writable or the CLI must be allowed to run
   outside it); (b) per-instance `config.toml` profile generated like the
   opencode config. Decision for Mark: bypass the sandbox for task workers
   (external isolation by run user, worktree, liveguard) or grant `--add-dir`.
   Recommended: sandboxed `workspace-write` plus `--add-dir` for the vault
   directory; bypass is a last resort.
4. **Stall and run status.** Read the JSONL log, as for amp. For TUI
   instances, extend `safesend.busyMarkers`, `dailycheck`, and
   `processIdentityIssue` (binary name `codex`).
5. **Send / nudge.** Resume path = abort a stalled turn and start a new one
   (matches AMUX-46's behaviour). `codex queue` may give true queueing; prove
   it before relying on it. `-doorbell` stays unsupported for headless.
6. **Thread watch / transcript.** Add a codex reader over the exec log (or the
   rollout files) and a collector; until then pane fallback applies.
7. **Retire.** Stop in-flight `codex exec` children (cmdline + env match like
   `ampRunCmdlineMatch`), keep the branch, optionally `codex archive` the
   thread. Keep rollout files like Claude transcripts.
8. **Auth ownership.** One login per run user; instances share
   `$CODEX_HOME`. Concurrent refresh of one `auth.json` by several processes
   is unproven. Decide: shared `CODEX_HOME` for the run user (simple) or one
   per instance (isolated, needs login per instance — worse). Recommend
   shared, plus an `agentmux auth status` line for codex (login status only).
9. **mergentic.** Add `codex` to `agentmuxAgents`; model is a free string
   (`-m`, from codex's list, no names in code); effort maps to
   `model_reasoning_effort`; the dispatch ask needs a codex option and the
   `Efforts` set may need `xhigh`; a brief line "no live model calls in tests"
   mirrors AMUX-49.

## 4. Risks

- **Auth**: tokens in `auth.json` must never be printed or committed; tests and
  docs use `codex login status` only. Task instances must not get
  `OPENAI_API_KEY` implicitly (it can switch the account to metered billing).
  Re-auth needs a browser: `--device-auth` fits the existing helper-tmux
  re-auth flow in `AGENTS.md`.
- **Spend**: the test host is logged in with a ChatGPT account, so usage draws
  on that plan's quota and rate limits rather than per-token credits — but it
  is still a shared, finite allowance. Add usage from `turn.completed` to the
  run record, and refuse API-key mode unless explicitly configured.
- **Sandbox escape**: sandbox is the only guard; bypass flag = full user
  access. The bwrap failure above pushes people toward the bypass flag, so fix
  the host first and make `danger-full-access` and the bypass flag a refused
  value unless explicitly allowed per instance. Reuse liveguard env
  (`taskSessionEnvArgs`) for task instances so they cannot hit the live
  Discord.
- **Public repo**: no model ids, hostnames, user paths or thread ids in code,
  tests or docs; fixtures must be synthetic JSONL.
- **Tests**: a fake `codex` binary on `PATH` that emits canned JSONL
  (success, failure, rate limit, resume, hang) and honours the same flags.
  The wrapper refuses real `codex exec` in task instances unless
  `AGENTMUX_ALLOW_LIVE_CODEX=1` (like AMUX-49). No live calls in `go test`.
  Implemented (AMUX-55): the fake and its synthetic fixtures are in
  `daemon/testdata/fakecodex/` (scenarios via `FAKE_CODEX_SCENARIO`), and the
  refusing wrapper is `daemon/internal/session/codexguard.go`, installed next
  to the amp wrapper for `task-*` instances. A refusal is one line on stderr
  and in `$AGENTMUX_TASK_LOG`.

## 5. Implemented runner (AMUX-56)

The `codex` agent kind is a headless runner shaped like amp (sections 1 and 3).

- **Kind**: `AGENTMUX_AGENT=codex`. `agentmux new -agent codex` (and
  `sessions create` from a codex template) provisions a registry entry and the
  oneshot/tick units; `session run|update|stop` keep a placeholder tmux
  session alive (no resident codex process). `-model` is the instance's
  `AGENTMUX_MODEL`.
- **Run**: `agentmux sessions run -file PROMPT [-model M] [-effort E]
  [-sandbox MODE] [-thread ID] <instance>@<host>` launches
  `codex exec --json -C <worktree> -s <sandbox> [-m M] [-c
  model_reasoning_effort=E] -` detached, with the prompt on stdin from a
  private file (never argv). Continue is the same with `resume <thread_id>`
  before the trailing `-`. Output is appended to
  `~/.local/state/agentmux/sessions/<instance>/codex-run-<thread>.jsonl`,
  bracketed per run by `agentmux.start` / `agentmux.exit` records the launcher
  writes. `OPENAI_API_KEY`/`CODEX_API_KEY` are dropped from the child.
- **State** (`sessions status <instance>@<host>#<thread>`): each
  `thread.started`/`agentmux.start` begins a segment; `turn.completed` is
  done, `turn.failed` is failed with the flattened error (and `rate_limited`
  for 429 / rate or usage limit), a bare `error` is not terminal, an
  `agentmux.exit` with no result is failed ("process exited ... without a
  result"), and no event for 10 minutes while running reports `stalled`.
- **Continue vs busy**: a continue of a still-working thread is refused
  (`busy`), since two `codex exec` on one rollout would interleave; a `send`
  that finds the turn stalled stops it first and starts a new turn.
- **Sandbox**: default `workspace-write`; `read-only` allowed;
  `danger-full-access` and the bypass flags are refused unless the instance
  registry has `AGENTMUX_CODEX_ALLOW_UNSAFE_SANDBOX=1` (set by hand; not copied
  from a template).
- **Not done here**: TUI instances, thread-watch collector, `codex queue`,
  usage accounting from `turn.completed`, mergentic dispatch support.

Live proof (test host, read-only sandbox, default model, effort low): a new
thread (`Reply with exactly the word PONG`) ended `done` with message `PONG`;
`sessions run -thread` on the same id started a second segment that ended
`done` with `PING`; the log held two `thread.started` segments.
