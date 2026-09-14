# Point a zero/opencode/kilo instance at a custom provider

By default `agentmux new` wires a zero/opencode/kilo instance up to a local
Ollama install. You can instead point one at any OpenAI-compatible
endpoint — a paid gateway, a self-hosted router, whatever — via `-provider`
and `-provider-base-url`:

```sh
agentmux new -y -instance my-kilo -agent kilo -run-user myuser \
  -provider my-gateway -provider-base-url https://gateway.example/v1 -model some-model
```

`-provider` can be any identifier; only `"ollama"` has a built-in default
base URL, so anything else requires `-provider-base-url` explicitly.
Re-running the same command against an **existing** instance (same
`-instance`/`-agent`) updates its provider/model in place — this is the
supported "change settings" workflow, not just first-time creation. Because
applying a provider/model change requires the instance's live process to
actually restart, re-running this **does interrupt whatever that instance is
currently doing** (see "Why this needs a clean restart" below).

## Authentication: the part agentmux can't do for you

If the endpoint needs an API key, add `-provider-api-key-env`:

```sh
agentmux new -y -instance my-kilo -agent kilo -run-user myuser \
  -provider my-gateway -provider-base-url https://gateway.example/v1 \
  -model some-model -provider-api-key-env MY_GATEWAY_API_KEY
```

This records which environment variable *name* holds the key (never the key
value itself — nothing secret goes through agentmux's registry or RPC
layer). `-provider-api-key-env` supports `kilo` and `opencode`; it's not
wired up for `zero` yet — its config format's support for a templated
`"{env:VAR}"` value hasn't been confirmed, so agentmux doesn't guess at it.

**Opencode** is the simpler case: its project-level config (the
`opencode.json` agentmux regenerates on every `session run`) accepts a
`"{env:VAR}"` reference directly, and agentmux writes one there itself once
`-provider-api-key-env` is set. The only thing left to do by hand, once, on
the host, is put the actual key where agentmux-launched processes can see
it — in `~/.config/agentmux/kilo-env` (same file kilo uses, see below;
despite the name it's read for any zero/opencode/kilo instance), one
`NAME=VALUE` per line:

```sh
MY_GATEWAY_API_KEY=sk-...
```

Then restart the instance (re-run the `agentmux new` command above, or
`agentmux control -action restart`) for it to pick the value up.

**Kilo is a different story.** Kilo's project-level config (the `kilo.json`
agentmux regenerates on every `session run`) flatly refuses any
`"{env:VAR}"` reference — confirmed via `kilo config check` — so an API key
can never live in the config agentmux itself writes per-project. The
supported place for it is Kilo's **shared global config**,
`~/.config/kilo/kilo.jsonc`, which Kilo merges into every project
automatically and which *does* allow `"{env:VAR}"` references. So after
running the command above, do this once, by hand, on the host:

1. Add a provider block for your provider id to `~/.config/kilo/kilo.jsonc`
   (create the file if it doesn't exist yet — keep any existing
   `"permission"` block or other content, just add/merge the `"provider"`
   key):

   ```jsonc
   {
     "$schema": "https://app.kilo.ai/config.json",
     "provider": {
       "my-gateway": {
         "name": "my-gateway",
         "npm": "@ai-sdk/openai-compatible",
         "options": {
           "baseURL": "https://gateway.example/v1",
           "apiKey": "{env:MY_GATEWAY_API_KEY}"
         },
         "models": { "some-model": { "name": "some-model" } }
       }
     }
   }
   ```

2. Put the actual key in `~/.config/agentmux/kilo-env` (create it if
   absent), one `NAME=VALUE` per line:

   ```sh
   MY_GATEWAY_API_KEY=sk-...
   ```

   agentmux's own `session run` reads this file and injects its contents
   into every tmux-launched zero/opencode/kilo instance's environment.
   That's a deliberate extra step, not an oversight: agentmux-managed
   instances run non-interactively under tmux/systemd and never source a
   shell profile, so an env var exported only from `.bashrc` never reaches
   them — confirmed the hard way. If you also want an interactive session
   started directly from a terminal to see the same key, export it from
   your shell profile too (e.g. `.bashrc`); the two paths are independent.

`agentmux new`'s response prints these exact steps back at you (filled in
with your actual provider id/URL/model/env-var name) whenever
`-provider-api-key-env` is set on a kilo or opencode instance, so you don't
have to remember this doc.

Once both are in place, confirm the key actually works — this tests the
real endpoint, not just config schema validity:

```sh
cd ~/.agentmux/my-kilo && kilo roll-call some-model
```

`kilo config check` only validates shape; `kilo roll-call` is what actually
proves the key round-trips.

## Why this needs a clean restart

A provider/model change only takes effect once the instance's process
actually restarts with the new config — the running process doesn't
hot-reload it. On Linux, that alone used to be two separate bugs:

1. `systemctl enable --now` (what a fresh provision ends with) is a no-op
   on an already-active `Type=oneshot, RemainAfterExit=yes` unit, so
   simply re-running the provisioner never even *tried* to restart an
   existing instance. Fixed by having the provisioner explicitly stop the
   unit before writing the updated registry/config on a re-provision,
   rather than relying on `enable --now` alone.
2. Even an explicit stop wasn't automatically enough: `tmux kill-session`
   sends a hangup and tears down tmux's own session bookkeeping almost
   immediately, but does **not** wait for the signaled process to actually
   finish exiting — and a well-behaved agent CLI does async work in its
   own shutdown handler (flushing its last-used model/session state to its
   local database) before it actually exits. If the old process is still
   mid-flush when the new config is written and a replacement process
   launched, it can silently overwrite that fresh write with its own stale
   state a moment later. Confirmed live: a provider swap reproducibly
   failed to stick this way. `StopAgentmux` (what every instance's
   `ExecStop` runs, on every stop/restart, not just re-provisioning) now
   polls for the killed session to actually be gone plus a short grace
   period before returning, closing this for good — `agentmux control
   -action restart` benefits from this too, not just `new -y`.

(macOS's LaunchAgent path was never affected by either bug: it already
fully unloads and reloads the agent on every provision, not just the
first one.)

## Troubleshooting: opencode/kilo keeps using an old model

A model change can fail to take effect at three independent layers, and an
`agentmux control -action restart` (or a fresh `agentmux new -y`) only
ever reaches the first two:

1. **This instance's project config** (`opencode.json`/`kilo.json`,
   regenerated by `RunAgentmux`). Fixed to only rewrite when the
   registry's provider/model/baseURL/apiKeyEnv actually changed (tracked
   via `AGENTMUX_LAST_CONFIG_HASH`) or the file is missing — before this,
   it rewrote unconditionally on every restart, silently reverting any
   model switched from inside the running session.
2. **opencode's own global config**, `~/.config/opencode/opencode.jsonc`
   (kilo: `~/.config/kilo/kilo.jsonc`). `small_model` and
   `agent.<name>.model` (e.g. `agent.compaction.model`) pin auxiliary
   helpers — auto-compaction, summarization — to a specific model
   *independently* of the main chat model shown in the TUI footer. A long
   session that needs auto-compaction can fail entirely if that pinned
   model is unavailable (rate-limited, wrong provider, etc.), while the
   visible "Build · &lt;model&gt;" label looks completely fine. This is a
   host-wide setting, not per-instance — check it if multiple instances
   share the symptom.
3. **In-flight state inside opencode's shared backend server.** Every
   opencode TUI invocation on a user account is a thin client to one
   long-lived `opencode serve` process (find it: `ps aux | grep 'opencode
   serve'`; find its port: `lsof -nP -iTCP -sTCP:LISTEN | grep opencode`).
   If something else on the host manages opencode for you (e.g. the Paseo
   desktop app spawns and owns this process as its child), that process
   can auto-compact using whatever model config was in memory *when it
   last started* — completely independent of the current file on disk,
   and untouched by `agentmux control -action restart`, because that only
   restarts the tmux client, not this shared backend. A stuck request
   here retries forever, silently, invisible in the TUI, burning API
   quota. The tell: `grep session.id=<id>
   ~/.local/share/opencode/log/opencode.log` (JSONL) shows the same
   `run=<id>` retrying the same `agent=compaction` call every ~10-20s with
   an error that never changes.

   To clear one stuck session without touching anything else:

   ```sh
   curl -X POST http://localhost:<port>/session/<id>/abort -d '{}'
   ```

   (`/api/session/<id>/interrupt`, the newer v2 endpoint, did **not**
   stop a stuck retry loop in practice — `/session/<id>/abort`, the older
   v1 endpoint, did.) To clear it for every session at once, restart
   whatever owns the backend process — killing it directly is safe if
   nothing manages it; if something like Paseo spawns it as a supervised
   child, restart that supervisor instead so it respawns the child
   properly rather than leaving a duplicate, unmanaged process behind.

   When testing a fix for this class of bug, always retest against the
   **actual** session that was broken, not just a fresh one — a fresh
   session working fine doesn't rule out a zombie stuck in an old one.
