# Chat and messaging

[← API overview](api-overview.md) · [Documentation](README.md)

## On this page

- [Channels](#channels)
- [Messages](#messages)
- [Read state](#read-state)
- [Direct messages](#direct-messages)
- [Presence and typing](#presence-and-typing)

## Channels

Alongside its forum, each place has chat channels: top-level `category`
entries group `text` channels. Chat is members-only, even in public places. Categories and
text channels can carry role overwrites for the chat permissions (`VIEW_CHANNELS`,
`SEND_MESSAGES`, `MANAGE_MESSAGES`, `MANAGE_CHANNELS`, `ADD_REACTIONS`, `ATTACH_FILES`).
They apply from the category down to the channel, using the same rules as
[board overwrites](api-places.md#boards). `MANAGE_CHANNELS` creates, edits, reorders and deletes
channels. Deleting a channel removes its messages and threads, and deleting a category moves
its channels to the top level. A *thread* is a sub-channel of a text channel, optionally
started from one of its messages, and inherits the channel's permissions. Threads can be
archived by their creator or by `MANAGE_MESSAGES`, and a new message unarchives them.

Places can also have [voice channels](api-voice.md).

## Messages

Message IDs are time-ordered UUIDv7s. `GET /channels/{id}/messages` returns
history in chronological order: the latest page by default, or the page `before`, `after`
or `around` a message ID. Messages are Markdown, up to 4,000 characters, and may set
`reply_to_id`. They also accept a `nonce` that is echoed back so clients can reconcile
optimistic sends. Authors can edit their own messages (previous versions are kept for the
author and `MANAGE_MESSAGES`) and delete them. `MANAGE_MESSAGES` can delete anyone's message
in place channels. The deletion is audited, and the author is told why. Deleted messages are
removed outright; reports keep a snapshot. Messages support pins (up to 50 per channel) and
emoji reactions. `@username` mentions and replies notify members who can see the channel,
unless they muted the channel, its category, or the place.

## Read state

`PUT /channels/{id}/read` moves the caller's read position forward. Channel
listings include `read_state` (`last_read_message_id`, `mention_count`) and `unread`. In
direct messages every unread message counts toward `mention_count`; elsewhere only mentions
and replies do. Reading a channel also marks the notifications it caused as read.

## Direct messages

`POST /users/@me/channels` with one recipient returns the caller's
one-to-one conversation with them, creating it if needed. Several recipients start a group
conversation (up to 10 people, with an owner and an optional name). You can only message
people you share a place with; instance administrators are exempt. Group members can add
people they share a place with. Anyone may leave, and the owner may remove others. When the
owner leaves, the longest-standing member takes over. Participants see read receipts
(`GET /channels/{id}/receipts`) and may pin messages. The first unread message of a
conversation creates a `direct_message` notification; the read state counts the rest.

## Presence and typing

Gateway connections report `online`, `idle`, `dnd` or `invisible`.
A user's presence is their most present connection (invisible counts as offline). People who
share a place or a conversation with a user can read their presence via
`GET /presences?user_ids=…`. `POST /channels/{id}/typing` shows the caller as typing for
about 10 seconds. Live updates arrive over the [real-time gateway](gateway.md).
