# Discord asks

Asks let another system (mergentic's ask service) put a question in front of
one person through Discord: each ask is a message in a thread in the collaboration
forum (a new post, or a new message in an existing ask thread so one task keeps
one thread), it @-mentions that person, and they answer by replying in the thread.
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
   Channels and Read Message History.

## Commands

```sh
agentmux asks post -title T -body-file F [-tag NAME ...] -json
# {"thread_id":"…","message_id":"…"}

agentmux asks post -thread ID [-title T] -body-file F [-tag NAME ...] -json
# {"thread_id":"…","message_id":"…"}  (thread_id is the one passed in)

agentmux asks reply -thread ID -body-file F [-mention]

agentmux asks read -thread ID [-after MESSAGE_ID] -json
# [{"id","author_id","author_name","author_is_configured_user","text","timestamp"}, …] oldest first

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
- `post -thread`, `reply`, `read` and `close` refuse threads that aren't `ask`-tagged posts
  in the configured forum.

## Keeping asks out of session context

`agentmux collab read` and the collaboration digest skip any thread tagged
`ask`, and `collab read -thread` refuses one, so sessions never pick asks up
as project context. This only works while the forum has an `ask` tag.

## Not included

Buttons would need an interactions endpoint or Gateway connection. Answering
is by reply.
