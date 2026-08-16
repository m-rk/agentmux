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
layer). For zero/opencode this is enough on its own — see each project's own
docs for how their generated project config picks up provider credentials.

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
   into every tmux-launched kilo instance's environment. That's a deliberate
   extra step, not an oversight: agentmux-managed kilo processes run
   non-interactively under tmux/systemd and never source a shell profile, so
   an env var exported only from `.bashrc` never reaches them — confirmed
   the hard way. If you also want an interactive `kilo` session started
   directly from a terminal to see the same key, export it from your shell
   profile too (e.g. `.bashrc`); the two paths are independent.

`agentmux new`'s response prints these exact steps back at you (filled in
with your actual provider id/URL/model/env-var name) whenever
`-provider-api-key-env` is set on a kilo instance, so you don't have to
remember this doc.

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
