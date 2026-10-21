# Headless Claude re-auth runbook

`agentmux auth login` runs `claude auth login` under a PTY as the
instance's run user, prints the authorize URL, and feeds pasted codes
back to claude. The operator completes the URL on another computer and
pastes the code back. This runbook is the agent-side procedure for
driving that exchange through a non-interactive shell, learned from the
first live run (Oct 2026).

## Rules

1. **Check the installed binary first.** `/usr/local/bin/agentmux` may
   predate the `auth` command. If `agentmux auth -h` fails, build and
   install first (from `daemon/`):
   ```sh
   go build -o agentmux ./cmd/agentmux
   sudo ./agentmux daemon install
   sudo systemctl restart agentmuxd.service   # safe: doesn't touch sessions
   ```
2. **Go straight to a waiting tmux session.** Backgrounding the login
   with redirected stdin (`nohup`, `setsid … </dev/null`) cannot hold
   an interactive prompt — the read side hits EOF and the session
   dies, orphaning its verifier. Instead:
   ```sh
   tmux new-session -d -s claude-relogin -c ~ -x 200 -y 50
   tmux send-keys -t claude-relogin "/usr/local/bin/agentmux auth login -instance <NAME>" Enter
   sleep 20
   tmux capture-pane -p -t claude-relogin   # read the URL from here
   ```
3. **One URL per attempt.** Each login run mints a fresh PKCE
   verifier. A code pasted from an earlier URL is dead on arrival —
   the session that printed it has already exited. Always use the URL
   from the currently waiting session.
4. **Paste fast.** Codes expire within minutes, and every agent
   round-trip burns some of that. Feed the operator's paste straight
   into the waiting prompt with a literal send (never log the code):
   ```sh
   tmux send-keys -t claude-relogin -l "<PASTED_CODE>" && tmux send-keys -t claude-relogin Enter
   ```
   Then confirm (`agentmux auth status -instance <NAME>`) and clean up
   (`tmux kill-session -t claude-relogin`).
5. **Verify and report.** Success looks like `opc: logged in (method
   claude.ai, refresh expires <date>)` from `auth status`. The
   expiry/expired Discord warnings clear on the next 5-minute tick.

## What not to do

- Don't run `auth login` with stdin redirected from `/dev/null` or a
  closed FIFO — it will print a URL whose verifier dies immediately.
- Don't reuse a URL across attempts, and don't quote an old code back
  to the operator as if it were still valid.
- Don't paste codes into any log, transcript, or report — they are
  single-use, but treat them as secrets anyway.
