# Thread watch

Thread watch follows each instance's own turns — the same JSONL/log/SQLite
records Claude Code, amp, and opencode already keep for themselves — and
tells you when a session needs you: waiting on a question, stuck in a loop,
failed mid-turn, or blocked by a usage/credit/rate limit. Everything else (a
slow turn, a recovered error, a context-bloat pattern) is only logged, and
rolls up into at most one nightly Discord digest instead of paging you. See
[docs/design/thread-watch.md](design/thread-watch.md) for the full design
and rationale; this page is the operator's guide to running it, including
the nightly review.

Thread watch is optional and off until you install it. It reuses the
`agentmux notify discord setup` webhook, so there's no separate credential to
configure for alerting.

## Install

Thread watch runs as its own systemd unit, `agentmux-threadwatch.service`,
**as your own operator user**, not as root inside `agentmuxd` — that's what
lets it read your own `~/.claude/projects/...` transcripts without any
privilege escalation. It is installed per agentmux install (i.e. per host).

```sh
sudo agentmux threadwatch install -run-user YOUR_USER
```

Review the unit before installing it with `-print`:

```sh
agentmux threadwatch install -run-user YOUR_USER -print
```

This is Linux-only for now; running it on macOS prints "not supported yet"
and exits 1.

Uninstall it the ordinary systemd way:

```sh
sudo systemctl disable --now agentmux-threadwatch.service
sudo rm /etc/systemd/system/agentmux-threadwatch.service
sudo systemctl daemon-reload
```

## Config

`~/.config/agentmux/threadwatch.yaml` is optional — a missing file just
means every default below applies. Only the fields you want to change need
to be present; anything left out keeps its default. Every field and its
default (from `DefaultConfig` in
[`daemon/internal/threadwatch/config.go`](../daemon/internal/threadwatch/config.go)):

```yaml
thresholds:
  awaiting_user_after: 10m   # page if idle this long on a question/prompt
  stalled_turn_after: 15m    # page if a running turn has gone silent this long
  api_error_loop: 3          # consecutive API errors before paging
  tool_error_loop: 4         # same tool error, same turn, before paging
  slow_turn_min: 5m          # insight floor for a slow turn
  compactions_per_day: 2     # insight floor for compaction churn
  long_wait_after: 2h        # insight: a question answered hours later
  retry_thrash_repeats: 3    # insight: the same failing command repeated

alerts:
  cooldown: 1h          # per (instance, thread, code) dedup window
  max_per_hour: 6       # host-wide page budget; the rest roll into "N more"
  resolved_window: 1h   # only send a "resolved" notice within this window
                         # (never sent at all for awaiting_user/usage_limit:
                         # the operator resolving a wait themselves, or a
                         # limit clearing on its own, isn't news — see
                         # docs/design/thread-watch.md's "Alerting")

jev:
  mode: shadow            # off | shadow | live — see "Shadow vs. live Jev" below
  model: jev-latest       # TypeSafe model name
  page_urgency: 4         # live mode: minimum Score (1-5) to page
  page_confidence: 0.6    # live mode: minimum confidence to page
  awaiting_min_prob: 0.5  # live mode: minimum needs_human_now to page

  # The TypeSafe key is entirely optional, and set per agentmux install
  # (i.e. per host). Give at most one of the two below; TYPESAFE_API_KEY in
  # the environment, if set, overrides both. See "TypeSafe key" below.
  # api_key_ref: op://<vault-id>/<item-id>/<field>
  # api_key: ts_...

review:
  agent: claude          # claude (default) | amp — see "Review backend: claude vs. amp"
  model:                  # optional Claude model override; unused for agent: amp
  amp:
    executor: local        # local (default, tools disabled) | runner:<id> (see below)
    workdir:                # local executor's cwd; default the run user's home
    runner_dir:              # runner executor's --runner-dir; optional
    mode:                    # optional amp -m/--mode override
    label: agentmux-review  # amp thread label
    # At most one of the two below; AMP_API_KEY in the environment, if set,
    # overrides both — same pattern as jev's key above.
    # api_key_ref: op://<vault-id>/<item-id>/<field>
    # api_key: sgamp_...

instances:
  some-sensitive-project:
    jev: false       # never send this instance's excerpts to TypeSafe
    review: false    # exclude it from the nightly review too
    disabled: true   # or skip it entirely
  noisy-instance:
    thresholds:
      slow_turn_min: 15m   # per-instance threshold override
```

Every duration accepts either a Go duration string (`10m`, `2h30m`) or a bare
number of seconds.

## TypeSafe key (optional)

Jev needs a TypeSafe API key. Without one, thread watch runs on its
deterministic rules alone. The key is entirely optional and set per
agentmux install — i.e. per host, in that host's own
`~/.config/agentmux/threadwatch.yaml` — in one of two ways:

```yaml
jev:
  # A 1Password secret reference: op://<vault-id>/<item-id>/<field>.
  api_key_ref: op://<vault-id>/<item-id>/credential
  # Or the key itself. Only honoured when threadwatch.yaml is mode 600.
  # api_key: ts_...
```

Prefer `api_key_ref`: the file then holds no secret. For a 1Password share
URL (`...&v=<vault-id>&i=<item-id>...`) the vault is `v=` and the item is
`i=`; the field is normally `credential`. The reference is resolved once at
startup with the host's service account token
(`~/.config/op/service_account_token`, mode 600), so `op` must be on the run
user's PATH. `TYPESAFE_API_KEY` in the environment, if set, overrides both.

Check the key with one synthetic judgment (it prints Jev's verdict, never
the key):

```sh
agentmux threadwatch jev-test
```

Then restart thread watch: `sudo systemctl restart agentmux-threadwatch`.
Its log says where the key came from, or why Jev is off.

If the setting is wrong (both keys set, a malformed reference, a literal key
in a file others can read) or 1Password can't resolve the reference, thread
watch logs why and runs deterministic-only; it never alerts *less* because
Jev is unavailable. To keep one project's excerpts away from TypeSafe, set
`jev: false` on that instance (see [Config](#config)).

## Shadow vs. live Jev

`jev.mode` controls how much Jev's typed judgments (the awaiting-user gate,
the alert urgency gate, insight tagging) actually influence paging:

- **`off`** — no Jev calls at all.
- **`shadow`** (the default once a key is configured) — every intervene
  signal is still scored, and the verdict is logged and shown by `threadwatch
  status`, but it never suppresses or changes a page. This is how you build
  confidence in the gate against your own real sessions before trusting it.
- **`live`** — the urgency/awaiting-user gates actually control paging. A
  signal Jev would have suppressed is still recorded, just re-tiered from an
  intervene alert to a logged insight instead of silently disappearing.

Run in shadow mode for a while, compare `threadwatch status` and the event
log against what you actually needed to know, then switch to `live` once
you're comfortable.

Without a live Jev verdict (shadow or off mode, no key, or a failed call), a
"waiting on you" alert pages only when the agent's last message ends in a
question, or the message or the pane tail shows a prompt or menu ("Do you
want…", "(y/n)", a `❯ 1.` cursor, and similar). A finished summary is kept
as an insight instead. In shadow mode the log records where Jev and this rule
disagree (`shadow: jev says waiting …`), which is what to look at before
switching to `live`. A turn that ends without a fresh assistant message at
all (an API error, an interrupt) never pages as "waiting on you" either — an
older message from earlier in the thread is never reused as evidence for a
later, unrelated turn. A usage/session/credit/rate limit is its own
`usage_limit` signal (🪫), never `awaiting_user`, even in live mode.

## Alerts

Each intervene signal (`awaiting_user` ⏳, `error_loop` 🔁, `auth_failed` 🔑,
`stalled_turn` 🧊, `died_mid_turn` 💥, `usage_limit` 🪫) becomes at most one
Discord message: instance, thread, a one-line reason, evidence, and how to
attach. Evidence is shown in up to two parts — the agent's own last words
(cut to a sentence/line boundary, not mid-word), and, when a pane tail was
captured, a `Pane:` block with box-drawing rules, status/keybinding chrome
and bare prompt lines stripped out, keeping just the last few real lines —
so an alert reads as what the agent actually said or was showing, not a
cut-off fragment glued to its own input box. See
[docs/design/thread-watch.md](design/thread-watch.md)'s "Alerting" for the
full dedup/rate-limit/resolved rules, including why `awaiting_user` and
`usage_limit` never get a "resolved" follow-up message.

## Running it

```sh
agentmux threadwatch serve                # run forever (what the unit does)
agentmux threadwatch serve -once          # one poll cycle, then exit
agentmux threadwatch serve -dry-run       # print decisions, never post to Discord
agentmux threadwatch serve -dry-run -once # combine both, e.g. to sanity-check a config change
```

`-interval` overrides the default 20s poll interval. `-socket`/`-config`
override the daemon socket and the config file path, for testing against a
non-default setup.

## Status

```sh
agentmux threadwatch status              # last 24h, human table
agentmux threadwatch status -since 48h
agentmux threadwatch status -json        # for scripts
```

Lists every intervene signal that's still open (fired and not since
resolved) in the window, one row per `(instance, thread, code)`, with the
most recent Jev verdict if one was scored. An empty result means nothing is
currently waiting on you — it does not mean nothing happened; insights (slow
turns, recovered errors, and the rest) are logged but only ever show up in
the nightly digest, not here.

## Nightly review

`agentmux threadwatch review` is the nightly job that turns a day of thread
watch's events and signals into per-instance stats, ranked insight clusters,
a Discord digest, and a full Markdown report. It reads from the same store
thread watch's collectors and detectors write to
(`~/.local/state/agentmux/threadwatch/events-*.jsonl` and `signals-*.jsonl`)
and never mutates anything itself — it only suggests.

```sh
agentmux threadwatch review [-since 24h] [-run-user USER] [-config PATH]
                             [-agent claude|amp] [-model MODEL] [-dry-run]
                             [-no-model] [-timeout 10m] [-weekly-quiet]
```

- `-since` — how far back to aggregate (default 24h).
- `-run-user` — whose threadwatch state, Claude/amp login, and Discord
  webhook to use. Left unset, it resolves the same way `agentmux doctor`
  does: the current user when not root, otherwise the first `claude-code`
  instance owner (see `doctorIdentity` in `cmd/agentmux/daily_check_cmd.go`).
- `-config` — `threadwatch.yaml` path, overriding the resolved run user's
  default (`~/.config/agentmux/threadwatch.yaml`); mirrors `threadwatch
  serve`'s own `-config`. Mainly useful for trying a config change with
  `-dry-run` before touching the real file.
- `-agent` — analysis backend for the escalation: `claude` or `amp`.
  Overrides `review.agent` in `threadwatch.yaml`; left unset it uses that
  (default `claude`). See "Review backend: claude vs. amp" below.
- `-model` — optional Claude model override for the review escalation.
  Unused for `-agent amp`.
- `-dry-run` — print the digest; write nothing (no report file, no
  Discord post).
- `-no-model` — skip the bounded model escalation entirely and build the
  digest straight from stats and cluster titles. This is also the automatic
  fallback whenever the review call fails, in which case the digest
  is prefixed with a `⚠️ review model failed` line so a broken model step
  is visible rather than silently degrading to less information.
- `-timeout` — overall run timeout (default 10m).
- `-weekly-quiet` — on Sundays, if nothing is notable, send a one-line
  "all quiet" summary with the same headline stats instead of sending
  nothing at all.

Each run, unless `-dry-run`:

1. Reads events/signals for the window from the threadwatch store.
2. Aggregates per-instance stats and ranks up to 8 insight clusters
   (`threadwatch.BuildReview`).
3. Escalates the clusters to a single bounded model call — no tools, a
   strict JSON-only reply contract, and excerpts passed as untrusted data
   the system prompt tells the model never to treat as instructions. With
   the default `claude` backend that's `claude -p --json-schema ... --tools
   "" --permission-mode dontAsk --no-session-persistence`, the same pattern
   `agentmux doctor` uses for its own escalation
   (`daemon/internal/dailycheck/claude.go`). With `-agent amp` it's a single
   `amp -x` call instead — see "Review backend: claude vs. amp" below.
4. Writes the full report to `~/.local/state/agentmux/reviews/YYYY-MM-DD.md`
   (mode 0600).
5. Posts a digest (≤ 1,900 characters) to the run user's configured Discord
   webhook, but only when the review is notable (at least one insight), or
   on a Sunday with `-weekly-quiet` and nothing notable.

### Review backend: claude vs. amp

`review.agent` (or `-agent`) picks which CLI runs the bounded escalation.
Both get the identical stats/clusters payload and the identical
`reviewSystemPrompt`; only the transport differs.

- **`claude`** (default) — `claude -p --json-schema ... --tools ""
  --permission-mode dontAsk --no-session-persistence`. Claude enforces the
  JSON reply shape itself via `--json-schema`.
- **`amp`** — a single `amp -x` call (`daemon/internal/ampexec`), reading
  `review.amp` from `threadwatch.yaml` (see [Config](#config)). amp has no
  `--json-schema` equivalent, so the JSON-only contract is spelled out in
  the message itself, and the reply is parsed after stripping an optional
  ` ```json ` fence. **Every run creates a real, visible amp thread on
  ampcode.com** (labeled `agentmux-review` by default,
  `review.amp.label` to change it) — that's intentional, not a leak to
  guard against.

  `review.amp.executor` chooses where that thread runs:
  - **`local`** (the default) runs amp directly on this host. Before
    calling amp, agentmux writes a temporary, 0600 settings file that
    disables every tool two independent ways
    (`amp.tools.disable: ["*"]` plus a catch-all `amp.permissions` reject
    rule) and passes it via `--settings-file`, removing it afterward. This
    is the only executor that can make that guarantee, which is why it's
    the default: the review payload embeds untrusted transcript/pane
    excerpts, and the whole point of disabling tools is that amp must not
    be able to act on anything hiding in them.
  - **`runner:<id>`** runs on an already-started `amp --no-tui` runner
    (`amp runner list` shows what's available; `review.amp.runner_dir`
    selects which served directory). **agentmux cannot disable tools on a
    runner** — that runner's own settings decide what it can call, and a
    startup log line warns about this every time it's selected. Only use
    a runner executor for a runner you've configured with permissions you
    trust against a message built from untrusted session excerpts.

  The optional amp access token follows the exact same precedence and
  1Password pattern as the TypeSafe key above: `AMP_API_KEY` in the
  environment, else `review.amp.api_key` (only honoured from a mode-600
  file), else `review.amp.api_key_ref` resolved via `session.ReadOpRef`
  with the host's service account token. No key at all just means amp
  relies on its own stored `amp login` session. The key is passed to the
  amp child through its environment only, never argv.

  `agentmux doctor -checker amp` reuses this exact `review.amp` block (see
  [docs/doctor.md](doctor.md)) rather than having its own separate amp
  config.

Per-instance `review: false` in `~/.config/agentmux/threadwatch.yaml`
excludes an instance from the review entirely (same file, same per-instance
override style as `jev: false`; see [Config](#config)).

### Installing the timer (Linux)

```sh
sudo agentmux threadwatch review install [-at 07:00] [-run-user USER] [-print]
```

- `-at` — local time (`HH:MM`) to run daily, after the doctor's own timer
  (default 07:00; the doctor itself defaults to 03:30).
- `-run-user` — who the systemd unit runs as. Unlike
  `agentmuxd-doctor.service` (which stays root and drops privilege
  internally via `runas` for the Claude subprocess only), the review timer
  runs **as this user directly** (`User=` in the unit): thread watch reads
  the operator's own transcripts and posts through the operator's own
  Discord webhook, so it does not need to run as root at all. Left unset it
  is auto-detected the same way `-run-user` on `agentmux doctor` is.
- `-print` — print the rendered `agentmux-threadwatch-review.service` and
  `.timer` units without touching the system or requiring root; useful to
  review before installing.

Installing writes
`/etc/systemd/system/agentmux-threadwatch-review.{service,timer}`, runs
`systemctl daemon-reload`, and enables + starts the timer
(`Persistent=true`, so a missed run — e.g. the host was off — catches up on
next boot).

### Nightly review safety notes

- The review only ever suggests; it has no repair actions like
  `agentmux doctor`'s `start`/`restart`/`send_escape`.
- All excerpts sent to Claude are already capped and redacted by the
  collectors/detectors before they ever reach the store (see the design
  doc's `Event`/`Signal` contract), and are capped again to a smaller size
  when assembled into a cluster.
- A failed model call (Claude or amp) never suppresses the review: it falls
  back to a stats-and-titles-only digest instead of sending nothing.
- With `-agent amp` and the default `local` executor, tool access is
  disabled for the escalation the same way the `claude` backend's `--tools
  ""` disables it — see "Review backend: claude vs. amp" above for exactly
  how, and why `runner:<id>` cannot make the same guarantee.

## Data leaving the host

Capped, redacted excerpts (secrets are stripped by pattern before anything is
sent — see `Redact`/`Excerpt` in
[`daemon/internal/threadwatch/config.go`](../daemon/internal/threadwatch/config.go))
go to TypeSafe for the Jev gates, and to Claude (under your existing login)
or, with `review.agent: amp`, to amp (under your existing `amp login` or a
configured access token — **and visible as a real thread on ampcode.com**,
see "Review backend: claude vs. amp" above) for the nightly review's
writeup. Set `jev: false` and/or `review: false` on an instance in
`threadwatch.yaml` to keep a sensitive project entirely local-only — its
signals still page you deterministically, they just never leave the host as
excerpts.

## Troubleshooting

- **Nothing gets installed / "must be run as root"** — `threadwatch install`
  writes to `/etc/systemd/system`, so it needs `sudo`, same as `agentmux
  daemon install`.
- **No alerts, ever** — check `agentmux notify discord setup` has a webhook
  configured for the run user (`~/.config/agentmux/discord.yaml`); thread
  watch logs a one-line note and runs without sending if it doesn't.
- **Alerts stopped after a burst** — that's `alerts.max_per_hour`; the rest
  are rolled up into one "N more held" line, sent on the next page or the
  hourly flush.
- **Want to see what a config change would do first** — `agentmux threadwatch
  serve -dry-run -once` runs exactly one cycle against the real daemon and
  prints every decision instead of posting or suppressing silently.
