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
