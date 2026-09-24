# Thread watch (design)

Status: phases 1-4 implemented, except the TUI badge for instances with open
signals. Deterministic watch, Jev in shadow *and* live mode, the nightly
review, the opencode collector, and the pane-hash fallback for other agents
are all in place. Quiet hours and the dedup/merge Jev question below are
**not implemented** — see "Open questions". See
[docs/thread-watch.md](../thread-watch.md) for the operator's guide —
installing it, `threadwatch.yaml`, the optional TypeSafe key, and
`threadwatch serve`/`status`/`review`.

agentmux already knows whether a session is *up* (`agentmux list`, the daily
doctor). It does not know whether the work inside a session is *going well*.
Thread watch adds that: it follows each instance's turns and sends two kinds
of output.

- **Intervene alerts** go to Discord straight away, and only when a human
  action would change the outcome: the session is waiting on you, stuck, or
  failing in a loop.
- **Insights** such as slow turns, wasted tokens, recovered errors and repeated
  friction are only logged. A nightly review turns them into at most one
  Discord digest.

The bar for an immediate message is "you should act now". Everything else
waits for the digest.

## Sources

Each agent keeps its own record of turns. Collectors turn these into one event
stream. Where structured data exists they read it and never scrape it from the
pane.

| Agent | Source | Useful fields |
|---|---|---|
| claude-code | `~/.claude/projects/<workdir-slug>/*.jsonl` | `system/turn_duration` (`durationMs`), `isApiErrorMessage`, `tool_result.is_error`, assistant `usage`, `cost-state`, compaction and bridge records |
| amp | `~/.cache/amp/logs/no-tui.log` (JSON lines) | `level`, `threadId`, auth/`Session expired` errors, reconnects, runner registration. Thread content is server-side, so amp gets error and liveness signals only |
| opencode | `~/.local/share/opencode/opencode.db` (read-only SQLite: `session`, `message`, `part`) | message times, token counts, tool errors |
| any (fallback) | `ViewPane` + discovery status | running/idle transitions come from discovery status separately; the `ViewPane` side is a pane-hash heartbeat only — one `activity` event whenever the rendered pane changes since the last poll, nothing else. It does not parse pane content, so it cannot tell a visible menu from any other change |

The collectors keep a small per-file byte or row offset, so a restart resumes
where it left off and never re-alerts. Normalised events are appended to
`~/.local/state/agentmux/threadwatch/events-YYYY-MM-DD.jsonl` (kept 14 days).
Events store metrics and short capped excerpts, not whole transcripts.

```go
type Event struct {
    Time     time.Time
    Instance string
    Thread   string        // Claude session id, amp threadId, opencode session id
    Kind     string        // turn_end, api_error, tool_error, auth_error, awaiting_user, stall, compaction, ...
    Duration time.Duration // turn_end
    Tokens   Usage         // turn_end, when known
    Excerpt  string        // capped (≤2 KiB), secret-redacted
}
```

## Detectors

Detectors are deterministic Go and run as events arrive. Numbers, counts and
timing stay in code. Each detector emits a `Signal{Instance, Thread, Code,
Tier, Evidence}` where `Tier` is `intervene` or `insight`.

Intervene candidates:

- `awaiting_user`: the session has sat idle ≥ 10 min (configurable) on a
  question, permission prompt, plan approval or menu. It fires after a turn
  ends, and also while a turn is still open (a prompt or menu blocks a turn
  without ending it) for the instance's most recently active thread; open
  turns quiet for more than 6 h are treated as abandoned. The runner appends
  the last 20 pane lines to the evidence before judging. Unless live Jev has
  a verdict, it pages only when the agent's last message ends in a question
  or the message or pane shows a prompt or menu; otherwise it is kept as an
  insight. See the Jev gate below.
- `error_loop`: at least 3 consecutive API errors, or the same tool error at
  least 4 times in one turn.
- `auth_failed`: amp `Session expired`, 401s, or Claude login errors. The
  doctor already repairs some of these, so this signal only shows the problem
  earlier.
- `stalled_turn`: status is `running`, but for more than 15 min there has been
  no new transcript event and the pane hash has not changed.
- `died_mid_turn`: the session exited or restarted while a turn was open.

Insight candidates (logged, never paged):

- `slow_turn`: a turn over the instance's rolling p90 and over 5 min.
- `expensive_turn`: tokens or cost over the instance's p95.
- `recovered_errors`: tool or API errors that the turn got past.
- `compaction_churn`: more than 2 compactions in one thread per day.
- `retry_thrash`: the same command or file operation repeated with no progress (for
  example tests failing the same way N times).
- `long_wait`: a question answered hours later. This is a signal about the
  workflow, not the agent.

Thresholds live in `~/.config/agentmux/threadwatch.yaml`, with per-instance
overrides.

## Alerting

Alerts reuse `discordnotify` and the webhook from
`agentmux notify discord setup`, so there is no new credential.

- One message per signal: instance, thread, a one-line reason, a capped
  evidence excerpt, and what to do (`agentmux` → select → `a`). Mentions are
  neutralised, as `dailycheck.FormatNotification` already does.
- Dedup on `(instance, thread, code)` with a 1 h cooldown. The limit is 6
  intervene messages per hour per host; after that they roll up into one
  "N more" line.
- When a condition clears (the session is running again), a short "resolved"
  message is sent only if the alert was sent in the last hour.
- Quiet hours are **not implemented**: every alert pages immediately
  regardless of time of day (see "Open questions").

## Nightly review and digest

`agentmux threadwatch review` runs on its own timer (default 07:00, set with
`agentmux threadwatch review install -at HH:MM`). It:

1. Aggregates the last 24 h of events and signals into per-instance stats: turn
   count, p50/p90 turn time, errors, tokens, and alerts sent.
2. Ranks insight clusters in code (by frequency × cost × recency), using the Jev
   tags below.
3. Sends the top ≤ 8 clusters with their stats to the same bounded escalation as
   the doctor: `claude -p` with no tools, a JSON-schema output and a system
   prompt that treats excerpts as untrusted data. It returns ≤ 5 insights, each
   with evidence and a concrete suggestion (an AGENTS.md line, a permission
   allow-rule, a threshold change, a flaky test to fix).
4. Writes the full report to `~/.local/state/agentmux/reviews/YYYY-MM-DD.md` and
   posts a digest of at most 1,900 characters to Discord. If nothing clears the
   bar, nothing is sent; an optional weekly "all quiet" line with headline
   stats is sent instead.

The review only suggests changes and never makes them. A later phase could
open PRs for suggested AGENTS.md changes, but only after you approve.

## Where Jev fits

Jev (TypeSafe's System One model) returns typed judgments and probabilities
rather than text. That fits the places where code needs judgment about
untrusted session text quickly and cheaply, many times a day. Durations and
counts stay in code, and prose for the digest stays with Claude.

| Use | Primitive(s) | Why Jev and not code or Claude |
|---|---|---|
| **Awaiting-user gate** (highest value). Runs when a session goes idle. State: last assistant message tail + pane tail | Choice `waiting_kind` ∈ {question to user, permission/approval prompt, plan awaiting approval, finished/report, error/blocked, none}; Noul `needs_human_now` | Regexes cannot tell "Done, here's a summary" from "Should I also migrate the DB?". Claude on every idle transition is too slow and costly |
| **Alert gate**. Before paging on any intervene candidate | Score `intervention_urgency` (1–5, with concrete levels); Noul `likely_transient` for errors | Confidence-gated routing: page only if urgency ≥ 4 and confidence is high. Uncertain candidates become insights, which is the main noise control |
| **Insight tagging** at write time | Choice `category` over a fixed taxonomy (missing permission, flaky test, env/tooling, context bloat, repeated instruction, unclear task, external outage, other); Noul `fixable_by_config` | Turns the log into structured data, so the nightly ranking is code (composite scoring) and Claude only writes up the top clusters |
| **Dedup/merge** (not implemented — future work) | Noul "same underlying issue as open alert X?" | Would stop one outage from causing one alert per instance; today dedup is purely code-based, per (instance, thread, code) with a cooldown (see "Alerting") |

All questions for one event go in one request, because they run in parallel.
Typed output also limits prompt injection: pane text that says "ignore previous
instructions" can at most move a probability, and it cannot trigger an action,
because detectors and repairs stay deterministic. Before Jev may suppress or
send anything, it runs in **shadow mode** for a week. Verdicts are logged next
to the deterministic outcome, and the thresholds are tuned against your real
sessions, not cookbook numbers.

Call shape (`POST https://api.typesafe.ai/v1/systemone`, `model: "jev-latest"`),
with a small `internal/typesafe` HTTP client. There is no Go SDK. The client
backs off on 429 and 529. If TypeSafe is unavailable, detectors fall back to
deterministic-only behaviour and never alert *less* because Jev is down.

## Running it and secrets

Thread watch runs as its own unit, `agentmux-threadwatch.service`, **as the
operator user**, not inside root `agentmuxd`. That lets it read the user's
transcripts without root. It talks to `agentmuxd` over the existing gRPC
socket (`ListInstances`, `ViewPane`) for status and panes.

The TypeSafe key is optional and set per host in `threadwatch.yaml`: either
`jev.api_key` (the key itself, honoured only from a mode-600 file) or
`jev.api_key_ref` (an `op://<vault-id>/<item-id>/<field>` reference). A
reference is resolved once at startup with the host's service account token,
in-process: thread watch is the consumer, so unlike amp there is no child to
hand the value to. Any key problem is logged and thread watch continues
deterministic-only, so a 1Password outage can't stop alerting.
`agentmux threadwatch jev-test` checks the key with one synthetic judgment.

Data leaving the host: capped, redacted excerpts go to TypeSafe (Jev gates)
and, for the nightly review, to Claude under your existing login. Per-instance
`jev: false` / `review: false` in `threadwatch.yaml` keeps a sensitive project
local-only.

## Phases

1. **Deterministic watch.** Claude-code and amp collectors, event log, the
   intervene detectors, Discord alerts with dedup, and
   `agentmux threadwatch status` to show open signals. There is no model in
   this phase. It is useful on its own and gives a baseline to compare Jev
   against.
2. **Jev in shadow mode.** Add the `internal/typesafe` client and the
   awaiting-user gate, urgency gate and tagging. Verdicts are logged only.
   After a week, compare them with what you actually needed to know.
3. **Nightly review and digest.** Add `agentmux threadwatch review`, the timer, the report
   file and the Discord digest.
4. **Jev live, plus the remaining collectors.** `jev.mode: live` lets the
   gates actually control paging; the opencode collector and the pane
   fallback for `zero` and others are implemented too. The one piece still
   outstanding from this phase is a TUI badge for instances with open
   signals.

## Open questions

- Whether digests and alerts use the same webhook or separate channels.
- Quiet hours: not implemented. Every alert pages immediately regardless of
  time of day; a future change could hold alerts during a configured window
  and fold them into the digest instead.
- Dedup/merge via Jev ("is this the same underlying issue as open alert X?"):
  not implemented. Today's dedup is purely code-based, per
  (instance, thread, code) with a cooldown.
- A TUI badge for instances with open intervene signals.
- Whether any instances should be local-only (no excerpts to TypeSafe or
  Claude) beyond the existing per-instance `jev: false` / `review: false`.
