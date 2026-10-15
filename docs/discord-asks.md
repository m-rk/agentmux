# Discord deferred panel buttons

Use persistent buttons when a click must trigger application work before the
message is refreshed. In `-buttons-json`, mark each panel control with
`"persistent": true`:

```json
[{"label":"Refresh","style":"primary","persistent":true}]
```

The bot posts these with `panel:<label>` component IDs. The
`agentmux discord serve` process acknowledges an authorized click with
Discord's deferred message update before recording or waking a client. The
source message stays unchanged and its controls remain active until the client
completes the update. Other users receive an ephemeral refusal. Re-delivery of
the same interaction ID is acknowledged but recorded only once; distinct taps on the same panel are
recorded independently. Ordinary buttons keep their existing one-shot
behavior.

## Interaction API

The stable client interface is the JSONL file at
`~/.local/state/agentmux/asks/clicks.jsonl`, shared by `discord serve` and the
configured run user. Persistent-button records have this shape:

```json
{"interaction_id":"…","thread_id":"…","message_id":"…","label":"Refresh","user_id":"…","timestamp":"…"}
```

`interaction_id` uniquely identifies one delivery; `thread_id` and
`message_id` identify the original message; `user_id` is the configured
Discord user's identity; `label` is the button value. The record has no bot
credential or interaction token. Clients can tail this file and use its
interaction ID to finish the deferred update:

```sh
agentmux discord complete -interaction INTERACTION_ID \
  -body-file body.txt -buttons-json buttons.json
```

`complete` resolves the source message from the local interaction record and
edits it using the bot credentials already configured for agentmux. Replacement
buttons with `persistent: true` stay enabled for later taps. The client should
apply the selected setting before calling `complete`; Discord keeps the
original message pending until the edit succeeds. The service manager must run
`agentmux discord serve` continuously, and the configured bot needs permission
to send messages in the ask thread. Setup and the one-shot button behavior are
documented in [discord.md](discord.md#buttons).
