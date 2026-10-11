# Moderation and policies

[← API overview](api-overview.md) · [Documentation](README.md)

## On this page

- [Moderation](#moderation)
- [Policies and consent](#policies-and-consent)
- [Transparency](#transparency)

## Moderation

Members report posts, chat messages, or other members to
`POST /places/{place}/reports`. Reports snapshot the content and land in a queue
(`MANAGE_REPORTS`) to be resolved or dismissed. `MODERATE_MEMBERS` can warn members or time
them out for up to 28 days. Timed-out members keep read access but lose posting, replying,
messaging, reacting, nickname and invite rights.
Bans may be temporary (`duration` in seconds). Every moderation and administrative action,
including role, board and place changes, is recorded in the audit log (`VIEW_AUDIT_LOG`).
Filter the log by action (`member.ban`, or a whole category such as `member`), actor, or
target. Optional `?reason=` on kick/delete requests is recorded and shown to the affected
user.

## Policies and consent

Instance administrators publish versioned policies
(`POST /policies/{kind}` for `terms`, `privacy` and `guidelines`), optionally scheduled up to a
year ahead and marked as requiring consent. `GET /policies` lists the versions in effect,
`/policies/{kind}/versions` is the changelog (each version can carry a summary of what
changed), and `/instance` links to them. Consent is an append-only log per purpose:
`POST /users/@me/consents` records accepting a policy version or giving or withdrawing
another purpose such as `analytics`. `GET /users/@me/consents` returns the latest decision per
purpose plus the `outstanding` policies whose current version the user has not accepted, which
clients should show. Registration accepts `accept_policies: true` to record consent to every
current policy that requires it. Consent records survive account deletion as proof, without
IP address or user agent.

## Transparency

`GET /transparency` (instance-wide) and `GET /places/{place}/transparency`
(anyone who can see the place) report, for a period of up to 366 days (default: the last 30),
how many reports were filed by reason and their status, how many moderation actions were
taken (warnings, timeouts, kicks, bans, unbans, voice disconnects, deletions), and how much
content moderators removed. Reports contain counts only.
