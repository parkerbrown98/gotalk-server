# Places, roles and boards

[← API overview](api-overview.md) · [Documentation](README.md)

## On this page

- [Places](#places)
- [Roles and permissions](#roles-and-permissions)
- [Boards](#boards)
- [Reading without an account](#reading-without-an-account)

## Places

Communities are *places* with `public` (listed, open to join), `invite_only`
(listed, join by invite) or `private` (unlisted, hidden from non-members) visibility. Places
are addressed by ID or slug: `/places/{place}`.

## Roles and permissions

Each place has an implicit `@everyone` role plus custom roles
ordered by `position`. Permissions are a bitfield (list them at `GET /permissions`); a
member's permissions are the union of their roles. Owners and `ADMINISTRATOR` hold
everything. Members can only manage roles and members ranked below their highest role, and
can only grant permissions they hold.

## Boards

A place's forum is a tree of boards: top-level `category` entries group
`board`s, and boards can nest one more level. Each board can be `flat` or `threaded`, and
can enable Q&A *solutions*. Boards may carry permission *overwrites* that allow or deny
forum permissions (`VIEW_BOARDS`, `CREATE_TOPICS`, `REPLY_TO_TOPICS`, `ADD_REACTIONS`,
`ATTACH_FILES`, `MANAGE_BOARDS`, `MANAGE_POSTS`) for a role. Overwrites apply from the root
board down: at each level the `@everyone` overwrite applies first, then the member's other
roles, so a role allow beats an `@everyone` deny. This covers staff-only boards
(deny `VIEW_BOARDS` for `@everyone`, allow it for staff), announcement boards
(deny `CREATE_TOPICS`), and per-board moderators (allow `MANAGE_POSTS`).

## Reading without an account

Anyone, signed in or not, can read the boards of a
`public` place that `@everyone` can see. `invite_only` and `private` places are
members-only. Only members can post, react, report users, or subscribe.
