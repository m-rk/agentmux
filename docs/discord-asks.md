# Discord asks

Asks let another system (mergentic's ask service) put a question in front of
one person through Discord: each ask is a message in a thread in the collaboration
forum (a new post, or a new message in an existing ask thread so one task keeps
one thread), it @-mentions that person, and they answer by replying in the thread,
by tapping an emoji reaction, or by clicking a button (see
[One-tap answers](#one-tap-answers-reactions-and-buttons)).
Callers go through `agentmux asks`, so Discord credentials stay in agentmux's
config. This reuses the collaboration forum, bot and webhook from
[discord-collaboration.md](discord-collaboration.md); there is no new channel
or credential.

## Setup

1. Add the user to mention under `collaboration:` in
   `~/.config/agentmux/discord.yaml`:

   ```yaml
   collaboration:
     ask_mention_user_id: "123456789012345678"
   ```

   Only this user can ever be pinged by an ask (`allowed_mentions` lists just
   them; `@everyone`, roles and other users in the text stay inert). Replies
   from this user are what `asks read` flags.

2. Create these forum tags by hand (**Edit Channel → Tags**). agentmux does
   not create them, because that would need the bot to have Manage Channels,
   a far broader permission than anything else here:

   - `ask` (required: it marks ask posts)
   - `pending`, `launched`, `not now`, `failed`, `answered` (outcome tags)
   - optionally one per project; pass it with `-tag NAME`

   A command that needs a tag the forum lacks fails with a message naming it.

3. Give the bot **Manage Threads**, on the collaboration forum only, as a
   channel permission override (not server-wide). This is needed only for
   `asks close` and `asks post -thread`, which set tags and archive, unarchive
   or lock the thread. A webhook
   can't edit threads, and Manage Threads is the smallest permission that
   can. It lets the bot manage every thread in that forum, so agentmux
   refuses to touch anything that is not an `ask`-tagged post in the
   configured forum. Post, reply and read need no extra permission. Posting
   and replying use the webhook and reading uses the bot's existing View
   Channels and Read Message History. `asks react` needs **Add Reactions**,
   and `asks edit` needs **Send Messages in Threads** (message edits go
   through the bot), both as channel overrides on the forum.

## Commands

```sh
agentmux asks post -title T -body-file F [-tag NAME ...] [-react 1️⃣,2️⃣,⏸️] [-button LABEL ...] -json
# {"thread_id":"…","message_id":"…"}

agentmux asks post -thread ID [-title T] -body-file F [-tag NAME ...] -json
# {"thread_id":"…","message_id":"…"}  (thread_id is the one passed in)

agentmux asks reply -thread ID -body-file F [-mention]

agentmux asks read -thread ID [-after MESSAGE_ID] -json
# [{"id","author_id","author_name","author_is_configured_user","text","timestamp","answers":[…]}, …] oldest first

agentmux asks react -thread ID -message ID -emoji 🤖   # bot adds a reaction to a posted message

agentmux asks edit -thread ID -message ID [-body-file F] [-disable-buttons] [-chosen LABEL]
# replace the body and/or settle the buttons: -disable-buttons greys them all
# out, -chosen LABEL keeps that one highlighted (success style) like a click

agentmux asks serve   # long-running: records button clicks (buttons only)

agentmux asks close -thread ID [-tag NAME] [-lock]   # default tag: answered
```

- `-body-file -` reads the body from stdin. Bodies are limited to 2000
  characters including the mention.
- `post` applies `ask` and `pending` plus any `-tag`s (Discord allows five
  per post), and opens the body with `<@user>`.
- `post -thread ID` adds an ask message to an existing `ask`-tagged thread
  instead of creating a post. It @-mentions the configured user, unarchives
  and unlocks the thread if needed, swaps any outcome tag for `pending` (plus
  any `-tag`s), and returns the new `message_id`. `-title` renames the thread;
  without it the name is kept. The thread edit needs Manage Threads. Use the
  returned `message_id` as `read -after` to get the replies to that ask.
- On create, `-title` is the thread name (up to 100 characters), e.g.
  `MERG-4 combine related ready tasks…`.
- `reply` only mentions with `-mention`.
- `read` also returns the opening post and any replies from the webhook;
  `author_is_configured_user` is true only for a real message from the
  configured user, so mergentic can pick out replies after the opening post
  with `-after <message_id>`.
- `close` swaps any existing outcome tag (`pending`, `launched`, `not now`,
  `failed`, `answered`) for the one given, keeps other tags, then archives
  the thread. It does not lock it, so the next `post -thread` can reopen it;
  add `-lock` for a task that's finished.
- `post -thread`, `reply`, `read`, `react`, `edit` and `close` refuse threads
  that aren't `ask`-tagged posts in the configured forum.
- `react` has the bot add one emoji to a posted message (e.g. 🤖 once an
  autopilot or orchestrator has answered the ask outside Discord), so the
  post shows the choice with no person clicking. Needs Add Reactions, like
  seeding.
- `edit` changes a posted message in place: `-body-file F` (`-` for stdin)
  replaces the text, `-disable-buttons` greys every button out, and
  `-chosen LABEL` keeps that one highlighted (success style, ✓ prefix) with
  the rest grey — the same settled look a click gets. `-chosen` must match a
  button on the message or the edit fails, so a typo can't silently grey
  everything. Only the body is rewritten, never re-mentioned, so an edit
  pings nobody. Typical autopilot settle: `asks react -emoji 🤖` then
  `asks edit -disable-buttons -chosen "<label>"`. Message edits need the bot
  (Send Messages in Threads on the forum).

## One-tap answers: reactions and buttons

Two ways to answer without typing. Both are implemented; **buttons were
picked** (2026-10-05) as the one MERG-9 consumes, and reactions remain
available. A typed reply still counts as "Other" either way. Both show up in
`asks read -json` as an `answers` array on the message that was reacted to or
clicked:

```json
{"id":"500", …, "answers":[{"kind":"reaction","value":"1️⃣","message_id":"500"}]}
{"id":"500", …, "answers":[{"kind":"button","value":"Ship it","message_id":"500","timestamp":"…"}]}
```

Only the configured user's answers are reported; the bot's own seed
reactions and anyone else's reactions or clicks are ignored. `value` is the
emoji or the button label. Answers to the ask itself sit on the ask message,
which `-after <ask message_id>` would normally exclude, so when that message
has answers `read -after` returns it first (a bot/webhook message, never
`author_is_configured_user`). A caller can pick out answers with
`answers != null`.

### Reactions

`asks post -react 1️⃣,2️⃣,3️⃣,⏸️` posts as usual (via the webhook), then the bot adds
each emoji in order. The caller supplies the list, so the convention is
1️⃣ 2️⃣ 3️⃣ 4️⃣ for options and ⏸️ for Not now. Works with `-thread` too.

- **Permissions:** the bot needs **Add Reactions** on the forum (a channel
  override, like Manage Threads). Reading uses the existing View Channels and
  Read Message History.
- If seeding fails after the ask posted (e.g. no Add Reactions), `post` still
  prints the ids, adds `"reactions_error"` to the JSON and warns on stderr,
  exiting 0 so a retry doesn't duplicate the ask.
- No process needs to be running: Discord stores the reactions, and `read`
  fetches them on demand.

### Buttons

`asks post -button "Ship it" -button "Not now"` (up to 25 labels of 80
characters) makes the **bot** post the message with buttons, because webhooks
can't send interactive components. Discord delivers a click only as an
interaction that must be acked within 3 seconds, so something must hold a
Gateway connection open: `agentmux asks serve`. It records the click in
`~/.local/state/agentmux/asks/clicks.jsonl` (`read` reads the same file, so
run `serve` and `read` as the same user) and acks by editing the message:
every button is disabled and the chosen one turns green with a ✓. A click
from anyone else, or a second click on the same ask, gets a private
(ephemeral) refusal and records nothing. The first click wins.

- **Permissions:** the bot needs **Create Posts** (new asks) and **Send
  Messages in Threads** (`post -thread`) on the forum, in addition to what
  reactions need if used together. No privileged Gateway intents are needed.
- Run `asks serve` under a service manager (it exits only on SIGINT/SIGTERM
  and reconnects with backoff on its own). For example a systemd user unit:

  ```ini
  [Service]
  ExecStart=/usr/local/bin/agentmux asks serve
  Restart=always
  ```
- Buttons carry their label in the component's `custom_id` (`ask:<label>`),
  so no ask state is stored beyond the click.

### Trade-offs

| | Reactions | Buttons |
|---|---|---|
| Extra permissions | Add Reactions | Create Posts, Send Messages in Threads |
| Always-on process | none | `asks serve` with an open Gateway connection |
| Latency to the caller | none beyond polling `read` | none beyond polling `read`; the tap is acked in well under 3s |
| Tap feedback | emoji shows up | buttons disable and the pick turns green |
| Daemon down | taps still stored by Discord and read later | taps while down fail ("This interaction failed") and are lost; buttons stay live, so the user can tap again once it is back |
| Message author | webhook, named "agentmux asks" | the bot |
| Option text | any emoji, meaning lives in the body | the labels themselves |

## Keeping asks out of session context

`agentmux collab read` and the collaboration digest skip any thread tagged
`ask`, and `collab read -thread` refuses one, so sessions never pick asks up
as project context. This only works while the forum has an `ask` tag.

## Not included

An HTTPS interactions endpoint (the alternative to the Gateway connection for
buttons). Reactions and replies are unaffected by it.
