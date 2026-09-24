# Thread watch: nightly review and digest

See [`docs/design/thread-watch.md`](design/thread-watch.md) for the overall
design. This page documents `agentmux threadwatch review`: the nightly job
that turns a day of thread-watch events and signals into per-instance stats,
ranked insight clusters, a Discord digest, and a full Markdown report.

It reads from the same store thread watch's collectors and detectors write
to (`~/.local/state/agentmux/threadwatch/events-*.jsonl` and
`signals-*.jsonl`) and never mutates anything itself — it only suggests.

## Running it

```sh
agentmux threadwatch review [-since 24h] [-run-user USER] [-model MODEL]
                             [-dry-run] [-no-model] [-timeout 10m]
                             [-weekly-quiet]
```

- `-since` — how far back to aggregate (default 24h).
- `-run-user` — whose threadwatch state, Claude login, and Discord webhook
  to use. Left unset, it resolves the same way `agentmux doctor` does: the
  current user when not root, otherwise the first `claude-code` instance
  owner (see `doctorIdentity` in `cmd/agentmux/daily_check_cmd.go`).
- `-model` — optional Claude model override for the review escalation.
- `-dry-run` — print the digest; write nothing (no report file, no
  Discord post).
- `-no-model` — skip the bounded Claude escalation entirely and build the
  digest straight from stats and cluster titles. This is also the automatic
  fallback whenever the Claude review call fails, in which case the digest
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
3. Escalates the clusters to a single bounded `claude -p` call — no tools,
   a JSON schema for the reply, `--permission-mode dontAsk`,
   `--no-session-persistence` — the same pattern `agentmux doctor` uses for
   its own escalation (`daemon/internal/dailycheck/claude.go`). Excerpts are
   passed as untrusted data; the system prompt tells Claude never to treat
   them as instructions.
4. Writes the full report to `~/.local/state/agentmux/reviews/YYYY-MM-DD.md`
   (mode 0600).
5. Posts a digest (≤ 1,900 characters) to the run user's configured Discord
   webhook, but only when the review is notable (at least one insight), or
   on a Sunday with `-weekly-quiet` and nothing notable.

Per-instance `review: false` in `~/.config/agentmux/threadwatch.yaml`
excludes an instance from the review entirely (same file, same per-instance
override style as `jev: false`; see the design doc's "Instances" config).

## Installing the timer (Linux)

```sh
sudo agentmux threadwatch review install [-at 07:00] [-run-user USER] [-print]
```

- `-at` — local time (`HH:MM`) to run daily, after the doctor's own timer
  (default 07:00; the doctor itself defaults to 03:30).
- `-run-user` — who the systemd unit runs as. Unlike
  `agentmuxd-doctor.service` (which stays root and drops privilege
  internally via `runas` for the Claude subprocess only), the review timer
  runs **as this user directly** (`User=` in the unit), matching the design
  doc's "Running it and secrets": thread watch reads the operator's own
  transcripts and posts through the operator's own Discord webhook, so it
  does not need to run as root at all. Left unset it is auto-detected the
  same way `-run-user` on `agentmux doctor` is.
- `-print` — print the rendered `agentmux-threadwatch-review.service` and
  `.timer` units without touching the system or requiring root; useful to
  review before installing.

Installing writes
`/etc/systemd/system/agentmux-threadwatch-review.{service,timer}`, runs
`systemctl daemon-reload`, and enables + starts the timer
(`Persistent=true`, so a missed run — e.g. the host was off — catches up on
next boot).

## Safety notes

- The review only ever suggests; it has no repair actions like
  `agentmux doctor`'s `start`/`restart`/`send_escape`.
- All excerpts sent to Claude are already capped and redacted by the
  collectors/detectors before they ever reach the store (see the design
  doc's `Event`/`Signal` contract), and are capped again to a smaller size
  when assembled into a cluster.
- A failed Claude call never suppresses the review: it falls back to a
  stats-and-titles-only digest instead of sending nothing.
