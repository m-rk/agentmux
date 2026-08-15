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
hot-reload it. On Linux, a naive `systemctl restart` can lose this update:
if the *old* process is still alive when the update is applied, it can
flush its own last-used model back to disk on shutdown, clobbering the
change you just made, moments after you made it. agentmux's provisioner
avoids this by stopping an existing instance's unit *before* writing the
new registry/config, then starting it fresh — not relying on
`systemctl restart`'s single-shot semantics. (macOS's LaunchAgent path
was never affected: it already fully unloads and reloads the agent on every
provision, not just on the first one.)
