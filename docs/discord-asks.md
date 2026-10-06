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
   a far broader permission than anything else here. Tags describe the
   thread, not the ask (see MERG-37): every task thread carries exactly one
   type tag and exactly one state tag.

   - Type (one per thread, set at creation): `task`, `epic`, `idea`
   - State (one per thread, kept current): `needs me`, `working`, `blocked`,
     `parked`, `not now`, `done`, `failed`
   - optionally one per project; pass it with `-tag NAME`
   - Retired: `ask`, `pending`, `launched`, `answered`. Threads created
     before the retag may still carry them; agentmux honours the `ask` tag
     in its forum gate until Mark deletes the old tags from the forum.

   A command that needs a tag the forum lacks fails naming it and the valid
   ones (agentmux re-reads the forum once first, in case the tag was just
   created).

3. Give the bot **Manage Threads**, on the collaboration forum only, as a
   channel permission override (not server-wide). This is needed for
   `asks close`, `asks post -thread` and `asks tag`, which set tags and
   archive, unarchive or lock the thread. A webhook
   can't edit threads, and Manage Threads is the smallest permission that
   can. It lets the bot manage every thread in that forum, so agentmux
   refuses to touch anything that is not a task thread (a type tag, or the
   retired `ask` tag) in the configured forum. Post, reply and read need no
   extra permission. Posting
   and replying use the webhook and reading uses the bot's existing View
   Channels and Read Message History. `asks react` needs **Add Reactions**,
   and `asks edit` needs **Send Messages in Threads** (message edits go
   through the bot), both as channel overrides on the forum.

## Test thread

Task sessions must never touch live asks, but dry-run can't show emoji
resolution, tags or buttons as Discord renders them. The answer is one
reusable thread in the asks forum — `🧪 agent test thread` — that task
sessions post into instead of the live forum. No new channel, no
permission changes, no thread litter.

1. Create the thread once (a plain `agentmux asks post` outside a task
   session, tagged `idea`), and put its id in the host's `discord.yaml`:

   ```yaml
   collaboration:
     test_thread: "<thread id from the forum>"
   ```

   The id stays in host config, never in a repo. Until it is configured,
   task sessions refuse every sending command.

2. From a task session (any process with `AGENTMUX_TASK_SESSION=1`, with
   `AGENTMUX_INSTANCE_NAME` starting with `task-`, or inside a
   `*-worktrees/task-*` directory — see below) the commands reroute:
   `post` becomes a reply into the test thread, never a new forum post;
   `reply`, `react` and `edit` act only on messages in that thread;
   `close`, `tag` and `list` are dry-run style no-ops that print what
   they would do. Every line of output says `test thread: …`.

   This covers body rendering, custom emoji resolution, buttons,
   reactions, edits and the settled ✓ state, all as Discord really
   renders them. It doesn't cover creating a forum post, its tags, or
   closing it — test those with the fake gateway; `-dry-run` prints the
   full creation payload.

3. Test sends are harmless by construction: each message is prefixed
   `[<task id>]` so interleaved workers stay readable, and sent with
   allowed mentions off (`<@…>`, `@everyone` and `@here` are stripped,
   so nothing pasted into a test body can ever ping anyone). The bot
   deletes its own test messages older than 24 hours (`agentmux asks
   prune`, which the reconcile job calls) but keeps the starter message,
   and nothing in a task session can retag, rename, close, lock, archive
   or delete the thread. Clicks on test buttons are answered
   ephemerally ("test button") and recorded nowhere — they can never
   answer a real ask.

4. There is deliberately no override. A worker that can set an
   environment variable on its own command could also set the override,
   so an override the restrained agent can set itself isn't a guard.
   Live posting is for non-task callers only (a person, the
   orchestrator, `asks serve`). This stops well-meaning agents, not a
   determined one: workers run as the same Unix user and can read the
   bot token. A real boundary means running task sessions as a separate
   user without the Discord token (`sessions run -run-user` exists).

   To test Discord rendering from a task session, run `agentmux asks
   post` as normal — it replies into the test thread. Never set
   `AGENTMUX_ALLOW_LIVE` (it does nothing).

All test sends go through the bot token, never the webhook, so `post`,
`reply`, `react` and `edit` act as one Discord identity: a worker that
posts a test message can edit it afterwards. (A webhook-posted message
is authored by a different app identity than a bot-posted one, and
Discord only lets an app edit its own messages.)

## Commands

```sh
agentmux asks post -title T -body-file F [-tag NAME ...] [-react 1️⃣,2️⃣,⏸️] [-button LABEL ...] [-buttons-json FILE] [-embeds] -json
# {"thread_id":"…","message_id":"…"}  (link unfurls suppressed unless -embeds)

agentmux asks post -thread ID [-title T] -body-file F [-tag NAME ...] [-embeds] -json
# {"thread_id":"…","message_id":"…"}  (thread_id is the one passed in)

agentmux asks reply -thread ID -body-file F [-mention] [-embeds]

agentmux asks read -thread ID [-after MESSAGE_ID] -json
# [{"id","author_id","author_name","author_is_configured_user","text","timestamp","answers":[…]}, …] oldest first

agentmux asks react -thread ID -message ID -emoji 🤖   # bot adds a reaction to a posted message

agentmux asks edit -thread ID -message ID [-body-file F] [-disable-buttons] [-chosen LABEL] [-embeds]
# replace the body and/or settle the buttons: -disable-buttons greys them all
# out, -chosen LABEL keeps that one highlighted (success style) like a click.
# Edits keep link unfurls suppressed (an edit never re-enables cards)
# unless -embeds.

agentmux asks serve   # long-running: records button clicks (buttons only)

agentmux asks tag -thread ID -set "task,working" [-unarchive]
# replace the thread's tags with exactly these (a retag never posts)

agentmux asks close -thread ID [-tag NAME] [-lock]   # default tag: done

agentmux asks prune [-thread ID] [-older-than DUR] [-dry-run] [-json]
# delete the bot's own messages older than DUR (default 24h), keeping the
# starter message; default thread is the test thread
```

- `list` shows the asks forum's threads — thread id, title, applied tags
  (by name), created and last-message time, archived and locked flags, and
  the starter message id (a forum post's first message shares the thread
  id). `-open` (the default), `-archived`, or `-all` picks the state;
  `-tag NAME` keeps only threads carrying that tag and `-since DUR`
  (e.g. `24h`) keeps only threads active since then. It reads the guild's
  active threads (filtered to the forum) plus the forum's public archived
  threads, paged to the end, so an old stray post (the "t" post that
  prompted this command) shows up with its thread id. From a task session
  it is a no-op that names the test thread instead of enumerating live
  asks (see above).

- `post` and `reply` take `-dry-run`, which prints the Discord payload
  (title, content with the mention, tags, buttons, reactions) instead of
  sending it — the safe way to check real rendering from a test or task
  session. `post -thread ID -dry-run` names the target thread without
  renaming, reopening, or posting into it. The sending commands
  (`post`, `reply`, `react`, `edit`, `close`, `tag`), `sessions send`, `sessions
  run` (except its own `-dry-run`), `deploy`, and `daemon install` refuse
  inside a task session with "task sessions can't touch live Discord or
  other sessions; use fakes or -dry-run" — except that `asks post`,
  `reply`, `react` and `edit` reroute into the test thread (see above)
  and `asks close`, `tag` and `list` are no-ops there, so task sessions
  can test real rendering without touching live asks. A task session is
  any process with `AGENTMUX_TASK_SESSION=1`, with
  `AGENTMUX_INSTANCE_NAME` starting with `task-`, or running inside a
  `*-worktrees/task-*` directory — `sessions run` stamps the first two
  on every amp run child (task claude-code panes get the same pair), so
  the guard fires inside the agent's own runs even though real `amp -x`
  processes inherit no agentmux environment of their own. The directory
  fallback covers runs whose environment was scrubbed. There is no
  override: live posting is for non-task callers only.

- `-body-file -` reads the body from stdin. Bodies are limited to 2000
  characters including the mention.
- `post` applies `task` and `needs me` plus any `-tag`s (Discord allows five
  per post), and opens the body with `<@user>`. A `-tag` naming a state tag
  (e.g. `-tag blocked`) replaces the default `needs me`; type and project
  tags are kept alongside. New posts also get the longest auto-archive
  duration (7 days), so live task threads stay open.
- `post -thread ID` adds an ask message to an existing task thread
  instead of creating a post. It @-mentions the configured user, unarchives
  the thread if needed (unlocked only; archived threads reopen with the
  longest auto-archive duration, and are left unlocked so the next ask can
  reopen them), swaps any state tag for `needs me` (plus
  any `-tag`s), and returns the new `message_id`. `-title` renames the thread;
  without it the name is kept. The thread edit needs Manage Threads. Use the
  returned `message_id` as `read -after` to get the replies to that ask.
- `tag` replaces the thread's applied tags with exactly `-set` (e.g.
  `-set "task,working"`), resolved by name from the forum's available tags.
  An unknown name fails with the list of valid ones. The thread must be open:
  applying tags to an archived thread fails (Discord error 50083), so add
  `-unarchive` to unarchive it first (a retag posts nothing, so this needs
  Manage Threads) and re-archive after. A locked thread can't be unarchived
  this way — unlock it by hand first. Without Manage Threads the only way to
  unarchive an unlocked thread is to post a reply in it (Send Messages in
  Threads auto-unarchives); `asks post -thread` already does this, and
  mergentic's reconciler uses reply-then-close for archived orphans.
- On create, `-title` is the thread name (up to 100 characters), e.g.
  `MERG-4 combine related ready tasks…`.
- `reply` only mentions with `-mention`.
- `read` also returns the opening post and any replies from the webhook;
  `author_is_configured_user` is true only for a real message from the
  configured user, so mergentic can pick out replies after the opening post
  with `-after <message_id>`.
- `close` swaps any existing state tag (or retired outcome tag) for the one
  given — any state tag by name, default `done` — keeps type and project
  tags, then archives
  the thread. It does not lock it, so the next `post -thread` can reopen it;
  add `-lock` for a task that's finished.
- `post -thread`, `reply`, `read`, `react`, `edit`, `close` and `tag` refuse
  threads that aren't task threads (a type tag, or the retired `ask` tag) in
  the configured forum.
- `react` has the bot add one emoji to a posted message (e.g. 🤖 once an
  autopilot or orchestrator has answered the ask outside Discord), so the
  post shows the choice with no person clicking. Needs Add Reactions, like
  seeding.
- `edit` changes a posted message in place: `-body-file F` (`-` for stdin)
  replaces the text, `-disable-buttons` greys every button out, and
  `-chosen LABEL` keeps that one highlighted (success style, trailing ✓)
  with the rest grey — the same settled look a click gets. Re-settling is
  idempotent: the click handler and `-chosen` may run in either order, and
  `-chosen` matches the settled label too (the trailing ✓ is stripped when
  matching), always leaving exactly one ✓ at the end, after the label, so
  the button reads `[emoji label ✓]`. `-chosen` must match a
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

`-button LABEL` posts a plain grey button; `-buttons-json FILE` (`-` for
stdin) appends richer buttons from a JSON array of `{label, emoji, style}`
objects, e.g.:

```json
[
  {"label": "amp · medium", "emoji": ":amp:", "style": "primary"},
  {"label": "claude · medium", "emoji": ":claude:", "style": "primary"},
  {"label": "Not now", "emoji": "⏸️"}
]
```

- **Emoji** is a unicode emoji (⭐, ⏸️) or a custom server emoji by name
  (`:amp:`, `:claude:`, `:opencode:`, `:muse:`). Custom names resolve
  against the guild's emoji list on first use and are cached per guild;
  ids are never hard-coded. An unknown name posts the button without an
  emoji and logs once.
- **Style** is `primary` (blue), `secondary` (grey, the default),
  `success`, or `danger`. An unknown style fails the post.
- **Labels** allow up to 80 characters (Discord's limit), and duplicate
  labels on one message fail the post with a clear error rather than
  falling back to numbers.

With buttons the **bot** posts the message, because webhooks
can't send interactive components. Discord delivers a click only as an
interaction that must be acked within 3 seconds, so something must hold a
Gateway connection open: `agentmux asks serve`. It records the click in
`~/.local/state/agentmux/asks/clicks.jsonl` (`read` reads the same file, so
run `serve` and `read` as the same user) and acks by editing the message:
every button is disabled and the chosen one turns green with a trailing ✓
(`[emoji label ✓]`), keeping
each button's emoji and style. A click
from anyone else, a second click on the same ask, or any click on a
test-thread button gets a private (ephemeral) refusal and records
nothing: test buttons can never answer a real ask. The first click wins. `asks edit
-disable-buttons` and `-chosen LABEL` settle buttons the same way: only
`disabled`, `style` and the trailing ✓ change, so emoji survive and clicks
still map by label.

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

`agentmux collab read` and the collaboration digest skip every thread
carrying a task type tag (`task`, `epic`, `idea`, or the retired `ask` tag),
and `collab read -thread` refuses one, so sessions never pick asks up
as project context. Collab threads share the same forum but carry no type
tag, so they still flow through.

## Not included

An HTTPS interactions endpoint (the alternative to the Gateway connection for
buttons). Reactions and replies are unaffected by it.
