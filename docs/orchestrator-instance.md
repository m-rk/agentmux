# Orchestrator instance

A long-lived claude-code instance, `orchestrator`, that drains an event queue
kept in the vault. Senders ring a doorbell; the instance reads its state on
each wake, so a missed or merged doorbell loses nothing.

## Create

Run on the host that should own it, from a scratch directory that is not a
repository:

```sh
mkdir -p ~/scratch/orchestrator
agentmux new -y -agent claude-code -instance orchestrator \
  -workdir ~/scratch/orchestrator -run-user "$USER"   # -run-user on Linux only
```

Then, in that workdir:

- install the orchestrator skill;
- in `.claude/settings.json`, set the default mode to auto and allow only
  `mergentic`, `agentmux sessions read`, `agentmux sessions send` and
  `git fetch`. Broader permissions are a decision for the operator.

## Doorbell

```sh
agentmux sessions send -by PRINCIPAL -via sent -doorbell orchestrator@HOST "wake"
```

- Idle: delivered like any send.
- Busy, or an earlier doorbell is still pasted in the input box: exits 0 with
  `coalesced: true` and sends nothing. The running turn or the pending
  doorbell reads the queue anyway.
- Claude Code's greyed prompt suggestion does not count as a draft.
- A permission prompt or a person's typed draft still refuses.

## Restart

The daemon keeps the instance running. A crash or restart is safe because
its state is in the vault, not the conversation.

## Context hygiene

Not automated yet. The skill re-reads state on every wake, so clearing the
session is safe. Until a scheduled clear exists, check the pane is idle with
`agentmux view -instance orchestrator`, then run
`agentmux send-keys -instance orchestrator -- /clear Enter`, for example from
cron after N events. (`sessions send` can't carry a slash command: it puts a
provenance line before the text.)
