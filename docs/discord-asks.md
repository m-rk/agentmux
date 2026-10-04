# Discord asks

Asks let another system (mergentic's ask service) put a question in front of
one person through Discord: each ask is its own post in the collaboration
forum, it @-mentions that person, and they answer by replying in the thread.
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
   `asks close`, which sets tags and archives/locks the thread. A webhook
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

agentmux asks reply -thread ID -body-file F [-mention]

agentmux asks read -thread ID [-after MESSAGE_ID] -json
# [{"id","author_id","author_name","author_is_configured_user","text","timestamp"}, …] oldest first

agentmux asks close -thread ID [-tag NAME]   # default tag: answered
```

- `-body-file -` reads the body from stdin. Bodies are limited to 2000
  characters including the mention.
- `post` applies `ask` and `pending` plus any `-tag`s (Discord allows five
  per post), and opens the body with `<@user>`.
- `reply` only mentions with `-mention`.
- `read` also returns the opening post and any replies from the webhook;
  `author_is_configured_user` is true only for a real message from the
  configured user, so mergentic can pick out replies after the opening post
  with `-after <message_id>`.
- `close` swaps any existing outcome tag (`pending`, `launched`, `not now`,
  `failed`, `answered`) for the one given, keeps other tags, then archives
  and locks the thread.
- `reply`, `read` and `close` refuse threads that aren't `ask`-tagged posts
  in the configured forum.

## Keeping asks out of session context

`agentmux collab read` and the collaboration digest skip any thread tagged
`ask`, and `collab read -thread` refuses one, so sessions never pick asks up
as project context. This only works while the forum has an `ask` tag.

## Not included

Buttons would need an interactions endpoint or Gateway connection. Answering
is by reply.
