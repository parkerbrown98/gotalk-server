# Forum

[← API overview](api-overview.md) · [Documentation](README.md)

## On this page

- [Topics and posts](#topics-and-posts)
- [Read state, subscriptions, notifications](#read-state-subscriptions-notifications)
- [Feeds](#feeds)
- [Search](#search)
- [Drafts](#drafts)

## Topics and posts

Content is Markdown. A topic is created together with its opening
post (post number 1); replies get stable, increasing post numbers. Replies may set
`parent_id`; threaded boards list posts depth-first with a `depth`. Edits keep the previous
version (`/posts/{id}/revisions`). Deleted replies stay in the thread as tombstones with
empty content, which only `MANAGE_POSTS` can still read. Authors can edit their posts and
delete their replies. They can also delete their topics until someone replies. `MANAGE_POSTS`
can do all of this to anyone's content, and can pin, lock, archive or move topics. Archived
topics are read-only and hidden from listings unless `archived=true`. Reactions accept a
Unicode emoji (URL-encoded in the path) or a shortcode, up to 20 distinct per post.
`@username` mentions notify the mentioned user if they can see the board.

## Read state, subscriptions, notifications

`PUT /topics/{id}/read` records that the
caller opened a topic and, with an optional `post_number`, how far they have read (positions
never move backwards). Authenticated topic responses include a `viewer` object: `read` (opened,
and not marked unread since), `has_new_replies` and `new_reply_count` (posts since the last open),
`unread_count` (posts after the read position), `last_read_post_number`, `vote` and `subscription`; the older top-level
`last_read_post_number` and `unread_count` fields remain. `DELETE /topics/{id}/read` marks a
topic unread again but keeps the position. Read changes reach the user's other sessions as
`TOPIC_READ_STATE_UPDATE` events. Authors have read their own topics and replies.
Places, boards and topics can be set to `watching`, `normal` or `muted`. Watching a place
or board notifies about new topics, and watching a topic notifies about every reply. Authors
watch their own topics automatically. The most specific setting wins, so watching a topic
inside a muted place still notifies. Notifications (`mention`, `reply`, `topic_reply`,
`new_topic`, `reaction`, `solution`, `moderation`, `direct_message`) carry a `data` snapshot
for rendering and are pushed to the recipient's gateway sessions as they are created.
Moderation notices ignore mutes.

## Feeds

`GET /places/{place}/feed` ranks topics from every board of a place the caller
can read; `GET /feed` does the same across places, with `scope=home` (the caller's places, the
default when signed in, leaving out muted places, boards and topics) or `scope=all` (public
content on the instance, the only scope when signed out). `sort` is `hot` (default; score and
replies weighed against age), `new`, `active` (latest reply), `top`, `rising` (fastest-growing
topics of the last 48 hours) or `controversial` (many votes split between up and down);
`top` and `controversial` take a window `t` of `hour`, `day`, `week` (default), `month`,
`year` or `all`. Filters: `tag`, `solved`, `hide_read` (leave out opened topics without new
posts), `nsfw` (NSFW boards, and on instance feeds NSFW places, are left out unless true),
`include_archived`, and on place feeds `board` (a board or category and everything below it)
and `pinned=first` (pinned topics returned separately on the first page). Items are topics
plus their `board`, `place`, an `is_nsfw` flag and a plain-text `excerpt` of the opening post.
Pages are cursor-based: pass `next_cursor` back as `cursor` with the same parameters. A cursor
fixes the time the first page was loaded (`as_of`), so windows do not slide while paging. Keys
that change while someone pages (such as scores on `hot` and `top`) can move a topic across
the cursor, so clients should drop duplicate IDs. Members vote with
`PUT /topics/{id}/vote` (`{"value": 1}` or `-1`, needs `ADD_REACTIONS`, not on their own
topics) and `DELETE /topics/{id}/vote`. Places can turn voting off (`voting_enabled`), and
feeds then rank by the reactions on each opening post. `POST /feed/read` records up to 100
opens at once (offline replay, or importing what someone read while signed out), skipping
topics the caller cannot see. `POST /places/{place}/feed/read` marks a place (or a `board_id`)
read up to `before`, usually the feed's `as_of`, touching at most the 5000 most recently
active topics.

## Search

`GET /search` searches everything signed-out visitors can read across the
instance; `GET /places/{place}/search` searches every board the caller can read. Queries use
web-search syntax (`"exact phrase"`, `or`, `-exclude`). They can be filtered by `author`,
`tag`, `board`, `solved`, `after`/`before` and `topics_only`, and sorted by `relevance`
(text rank blended with reactions and recency), `newest` or `oldest`. Snippets wrap
matches in Markdown `**bold**`. Indexing uses PostgreSQL full-text search (language-neutral
`simple` configuration) and happens in the same transaction as each write, so results are
never stale.

## Drafts

Clients can sync unfinished posts across devices as JSON objects under
`/users/@me/drafts/{key}` (for example `topic:<boardID>` or `reply:<topicID>`).
