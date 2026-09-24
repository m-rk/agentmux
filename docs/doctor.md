# Session doctor

`agentmux daemon install` also installs one daily doctor for the host: a
systemd timer on Linux and a per-user LaunchAgent on macOS. It runs at 03:30
(Australia/Perth on Linux, local time on macOS), giving the default 03:00
per-instance refresh half an hour to finish. Choose another post-refresh time
when installing with `agentmux daemon install -doctor-time HH:MM`. Run the
same pass whenever you want with:

```sh
agentmux doctor -dry-run   # preview locally; no repair or Discord post
agentmux doctor            # diagnose, recover safely, and verify
```

The first stage is deterministic and backend-aware: it checks the service
manager, tmux/process identity, the latest refresh result, whether the pane is
interactive, and Claude/Kilo remote-control indicators where applicable. If
those checks are healthy, no model is called and no pane content leaves the
host.

When the first stage finds trouble, Claude Code is the escalation agent by
default and uses the existing Claude login for the session owner; `-model` can
pin a model when that is useful. On Linux, agentmux drops privileges before
launching Claude and normally chooses the owner of the first Claude Code
instance. `-run-user USER` makes that explicit on an unusual multi-user host.
`-checker amp` uses amp instead — see "Amp as the checker" below.

Claude gets no tools and never types into a session itself. It receives only
the affected sessions' structured findings and capped pane snapshots, then
returns JSON for agentmux to validate. A dead session may be started; Escape
may be sent only when the visible pane advertises an Escape action; an idle
session may be restarted only with a verbatim pane excerpt as evidence. A
running session cannot be restarted, and no lifecycle repair runs while the
daily refresh is still active. agentmux refreshes the affected session's state
immediately before acting, then probes again afterward rather than treating a
successful command as proof of recovery. Pane text is still session content,
so use an account you trust with that small excerpt.

## Amp as the checker

`agentmux doctor -checker amp` runs the same escalation through a single
bounded `amp -x` call (`daemon/internal/ampexec`) instead of `claude -p`. It
gets the identical findings/snapshots payload and the identical validation —
only the transport differs, and amp gets no tools here either.

Amp settings come from the run user's own `~/.config/agentmux/
threadwatch.yaml`, specifically the **same `review.amp` block** the nightly
review's `-agent amp` uses (see [docs/thread-watch.md](thread-watch.md#review-backend-claude-vs-amp)
for the full field list, the `local` vs. `runner:<id>` executor tradeoff,
and the amp key precedence). Doctor does not have its own separate amp
config section — it deliberately reuses that one, since both are "the
occasional bounded escalation for this host's operator." Two flags let a
doctor invocation override it without touching `threadwatch.yaml`:

- `-amp-executor` — override `review.amp.executor` (`local` or
  `runner:<id>`).
- `-amp-workdir` — override `review.amp.workdir` (default: the run user's
  home).

With the default `local` executor, agentmux generates a temporary,
0600 settings file that disables every amp tool (belt and braces:
`amp.tools.disable: ["*"]` plus a catch-all `amp.permissions` reject
rule) before the call and removes it afterward — the same tool-safety
guarantee `--tools ""` gives the claude path. A `runner:<id>` executor
cannot be given that guarantee (that runner's own settings decide what it
can call), so choosing one logs a startup warning; only point doctor at a
runner you've configured with permissions you trust against pane content.

Every `-checker amp` run creates a real, visible amp thread on
ampcode.com (labeled by `review.amp.label`, default `agentmux-review`) —
that is intentional, not a leak to guard against.

On Linux, root-run doctor drops privileges for the amp subprocess exactly
like it does for claude: via `runas`, as the same session owner
`doctorIdentity` resolves for the claude path (or `-run-user`).

No Discord message is sent for an uneventful pass. Repairs, meaningful
observations, escalation failures, and inspection failures go to the webhook
configured for the same OS user with `agentmux notify discord setup`. Each
message includes the before/after state, including successful auto-recovery.
An unchanged unresolved incident is debounced; recovery or a changed/new
incident produces a fresh message. If the same repair leaves exactly the same
problem behind twice, later attempts are suppressed until the session state
changes, and that suppression is reported once rather than causing silent
daily restart churn.

## Without a Claude login

The deterministic first stage always runs, but repair escalation shells out
to Claude Code — so a host with no Claude login (for example, an amp-only or
zero-only box) gets diagnosis without repair: escalation fails loudly as
`doctor escalation failed: ...` and the deterministic findings stand. Point
`-checker` at another analysis CLI, or add a Claude login, to get repairs
back.
