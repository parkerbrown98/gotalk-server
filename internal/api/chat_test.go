package api_test

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/parkerbrown98/gotalk-server/internal/config"
	"github.com/parkerbrown98/gotalk-server/internal/permissions"
)

func (e *env) channel(token, place string, body map[string]any) map[string]any {
	e.t.Helper()
	return e.expect(201, e.do("POST", "/api/v1/places/"+place+"/channels", token, body)).obj(e.t)
}

func (e *env) send(token string, channel map[string]any, content string, extra ...map[string]any) map[string]any {
	e.t.Helper()
	body := map[string]any{"content": content}
	for _, x := range extra {
		for k, v := range x {
			body[k] = v
		}
	}
	return e.expect(201, e.do("POST", "/api/v1/channels/"+id(channel)+"/messages", token, body)).obj(e.t)
}

func (e *env) messages(token string, channel map[string]any, query string) []map[string]any {
	e.t.Helper()
	raw := e.expect(200, e.do("GET", "/api/v1/channels/"+id(channel)+"/messages"+query, token, nil)).list(e.t)
	out := make([]map[string]any, len(raw))
	for i, v := range raw {
		out[i] = v.(map[string]any)
	}
	return out
}

func (e *env) listChannels(token, place string) []map[string]any {
	e.t.Helper()
	raw := e.expect(200, e.do("GET", "/api/v1/places/"+place+"/channels", token, nil)).list(e.t)
	out := make([]map[string]any, len(raw))
	for i, v := range raw {
		out[i] = v.(map[string]any)
	}
	return out
}

func id(m map[string]any) string { return m["id"].(string) }

func byID(items []map[string]any, want string) map[string]any {
	for _, it := range items {
		if it["id"] == want {
			return it
		}
	}
	return nil
}

func TestChatChannelsAndPermissions(t *testing.T) {
	e := newEnv(t)
	owner := e.setup()
	bob, _ := e.register("bob")
	carol, _ := e.register("carol")
	e.createPlace(owner, "chatters", "public")
	e.expect(200, e.do("POST", "/api/v1/places/chatters/join", bob, nil))

	// Chat is members-only, even in public places.
	e.expect(401, e.do("GET", "/api/v1/places/chatters/channels", "", nil))
	e.expect(403, e.do("GET", "/api/v1/places/chatters/channels", carol, nil))

	cat := e.channel(owner, "chatters", map[string]any{"name": "Text", "kind": "category"})
	general := e.channel(owner, "chatters", map[string]any{"name": "general", "parent_id": id(cat), "topic": "Anything goes"})
	staff := e.channel(owner, "chatters", map[string]any{"name": "staff"})
	require.Equal(t, id(cat), general["parent_id"])
	require.Equal(t, false, general["unread"])
	e.expect(403, e.do("POST", "/api/v1/places/chatters/channels", bob, map[string]any{"name": "mine"}))
	e.expect(422, e.do("POST", "/api/v1/places/chatters/channels", owner, map[string]any{"name": "x", "kind": "category", "parent_id": id(cat)}))
	e.expect(422, e.do("POST", "/api/v1/places/chatters/channels", owner, map[string]any{"name": "x", "parent_id": id(general)}))

	// Staff-only channel: deny VIEW_CHANNELS to @everyone. Overwrites only take chat bits.
	def := e.defaultRoleID(owner, "chatters")
	e.expect(422, e.do("PUT", "/api/v1/channels/"+id(staff)+"/overwrites/"+def, owner, map[string]any{"allow": 0, "deny": permissions.ViewBoards}))
	e.expect(200, e.do("PUT", "/api/v1/channels/"+id(staff)+"/overwrites/"+def, owner, map[string]any{"allow": 0, "deny": permissions.ViewChannels}))
	e.expect(403, e.do("GET", "/api/v1/channels/"+id(general)+"/overwrites", bob, nil))
	require.Equal(t, []string{"Text", "general", "staff"}, field[string](e.listChannels(owner, "chatters"), "name"))
	bobView := e.listChannels(bob, "chatters")
	require.Equal(t, []string{"Text", "general"}, field[string](bobView, "name"))
	perms := int64(bobView[1]["my_permissions"].(float64))
	require.NotZero(t, perms&int64(permissions.SendMessages))
	require.Zero(t, perms&int64(permissions.ManageMessages))
	e.expect(404, e.do("GET", "/api/v1/channels/"+id(staff), bob, nil))
	e.expect(404, e.do("POST", "/api/v1/channels/"+id(staff)+"/messages", bob, map[string]any{"content": "sneaky"}))

	// A role overwrite beats the @everyone deny.
	role := e.expect(201, e.do("POST", "/api/v1/places/chatters/roles", owner, map[string]any{"name": "Staff"})).obj(t)
	e.expect(200, e.do("PUT", "/api/v1/channels/"+id(staff)+"/overwrites/"+id(role), owner, map[string]any{"allow": permissions.ViewChannels, "deny": 0}))
	bobID := e.expect(200, e.do("GET", "/api/v1/users/@me", bob, nil)).obj(t)["id"].(string)
	e.expect(200, e.do("PUT", "/api/v1/places/chatters/members/"+bobID+"/roles/"+id(role), owner, nil))
	e.expect(200, e.do("GET", "/api/v1/channels/"+id(staff), bob, nil))
	require.Len(t, e.expect(200, e.do("GET", "/api/v1/channels/"+id(staff)+"/overwrites", owner, nil)).list(t), 2)
	e.expect(204, e.do("DELETE", "/api/v1/channels/"+id(staff)+"/overwrites/"+id(role), owner, nil))
	e.expect(404, e.do("GET", "/api/v1/channels/"+id(staff), bob, nil))

	// Updating and moving.
	e.expect(403, e.do("PATCH", "/api/v1/channels/"+id(general), bob, map[string]any{"name": "mine"}))
	updated := e.expect(200, e.do("PATCH", "/api/v1/channels/"+id(general), owner, map[string]any{"name": "lobby", "topic": "Say hi"})).obj(t)
	require.Equal(t, "lobby", updated["name"])
	e.expect(422, e.do("PATCH", "/api/v1/channels/"+id(general), owner, map[string]any{"is_archived": true}))

	// Deleting a category moves its channels to the top level.
	e.expect(204, e.do("DELETE", "/api/v1/channels/"+id(cat), owner, nil))
	moved := e.expect(200, e.do("GET", "/api/v1/channels/"+id(general), owner, nil)).obj(t)
	require.Nil(t, moved["parent_id"])
	e.expect(403, e.do("DELETE", "/api/v1/channels/"+id(general), bob, nil))
	e.expect(204, e.do("DELETE", "/api/v1/channels/"+id(staff), owner, nil))
	require.Equal(t, []string{"lobby"}, field[string](e.listChannels(owner, "chatters"), "name"))

	audit := e.items(e.expect(200, e.do("GET", "/api/v1/places/chatters/audit-log?action=channel", owner, nil)))
	require.Contains(t, field[string](audit, "action"), "channel.create")
	require.Contains(t, field[string](audit, "action"), "channel.overwrite_update")
	require.Contains(t, field[string](audit, "action"), "channel.delete")
}

func TestChatMessages(t *testing.T) {
	e := newEnv(t)
	owner := e.setup()
	ownerID := e.expect(200, e.do("GET", "/api/v1/users/@me", owner, nil)).obj(t)["id"].(string)
	bob, bobID := e.register("bob")
	e.createPlace(owner, "talk", "public")
	e.expect(200, e.do("POST", "/api/v1/places/talk/join", bob, nil))
	general := e.channel(owner, "talk", map[string]any{"name": "general"})

	m1 := e.send(owner, general, "hello everyone")
	m2 := e.send(bob, general, "hi @owner", map[string]any{"nonce": "abc"})
	require.Equal(t, "abc", m2["nonce"])
	require.Equal(t, []any{ownerID}, m2["mentions"])
	m3 := e.send(owner, general, "welcome", map[string]any{"reply_to_id": id(m2)})
	require.Equal(t, id(m2), m3["reply_to"].(map[string]any)["id"])
	require.Equal(t, []any{bobID}, m3["mentions"], "replies notify the replied-to author")
	e.expect(422, e.do("POST", "/api/v1/channels/"+id(general)+"/messages", bob, map[string]any{"content": "   "}))

	// History is chronological, paged with message-ID cursors.
	require.Equal(t, []string{id(m1), id(m2), id(m3)}, field[string](e.messages(bob, general, ""), "id"))
	require.Equal(t, []string{id(m2), id(m3)}, field[string](e.messages(bob, general, "?limit=2"), "id"))
	require.Equal(t, []string{id(m1), id(m2)}, field[string](e.messages(bob, general, "?before="+id(m3)), "id"))
	require.Equal(t, []string{id(m2), id(m3)}, field[string](e.messages(bob, general, "?after="+id(m1)), "id"))
	require.Equal(t, []string{id(m1), id(m2), id(m3)}, field[string](e.messages(bob, general, "?limit=3&around="+id(m2)), "id"))
	e.expect(422, e.do("GET", "/api/v1/channels/"+id(general)+"/messages?before="+id(m1)+"&after="+id(m1), bob, nil))

	// Mentions and replies notify, count as unread mentions, and are read by acknowledging.
	notes := e.notifications(bob)
	require.Equal(t, []string{"reply"}, kinds(notes))
	require.Equal(t, id(general), notes[0]["channel_id"])
	require.Equal(t, "mention", kinds(e.notifications(owner))[0])
	bobGeneral := byID(e.listChannels(bob, "talk"), id(general))
	require.Equal(t, true, bobGeneral["unread"])
	require.EqualValues(t, 1, bobGeneral["read_state"].(map[string]any)["mention_count"])
	read := e.expect(200, e.do("PUT", "/api/v1/channels/"+id(general)+"/read", bob, map[string]any{"message_id": id(m3)})).obj(t)
	require.EqualValues(t, 0, read["mention_count"])
	require.Equal(t, id(m3), read["last_read_message_id"])
	require.Equal(t, true, e.notifications(bob)[0]["read"])
	require.Equal(t, false, byID(e.listChannels(bob, "talk"), id(general))["unread"])
	// Read positions never move backwards.
	e.expect(200, e.do("PUT", "/api/v1/channels/"+id(general)+"/read", bob, map[string]any{"message_id": id(m1)}))
	require.Equal(t, id(m3), byID(e.listChannels(bob, "talk"), id(general))["read_state"].(map[string]any)["last_read_message_id"])
	// Mentioning someone in an already-read message notifies them without an unread badge.
	e.expect(200, e.do("PATCH", "/api/v1/messages/"+id(m1), owner, map[string]any{"content": "hello everyone, @bob"}))
	require.Equal(t, "mention", kinds(e.notifications(bob))[0])
	require.EqualValues(t, 0, byID(e.listChannels(bob, "talk"), id(general))["read_state"].(map[string]any)["mention_count"])
	e.expect(422, e.do("GET", "/api/v1/channels/"+id(general)+"/receipts", bob, nil))

	// Edits keep history; only the author may edit.
	e.expect(403, e.do("PATCH", "/api/v1/messages/"+id(m2), owner, map[string]any{"content": "hijack"}))
	edited := e.expect(200, e.do("PATCH", "/api/v1/messages/"+id(m2), bob, map[string]any{"content": "hi @owner!"})).obj(t)
	require.EqualValues(t, 1, edited["edit_count"])
	revs := e.expect(200, e.do("GET", "/api/v1/messages/"+id(m2)+"/revisions", bob, nil)).list(t)
	require.Equal(t, "hi @owner", revs[0].(map[string]any)["content"])
	require.Len(t, e.notifications(owner), 1, "unchanged mentions do not notify again")

	// Reactions.
	thumbs := "/api/v1/messages/" + id(m1) + "/reactions/" + url.PathEscape("👍")
	e.expect(204, e.do("PUT", thumbs, bob, nil))
	e.expect(204, e.do("PUT", thumbs, bob, nil))
	e.expect(422, e.do("PUT", "/api/v1/messages/"+id(m1)+"/reactions/"+url.PathEscape("not valid"), bob, nil))
	got := e.expect(200, e.do("GET", "/api/v1/messages/"+id(m1), bob, nil)).obj(t)
	require.Equal(t, []any{map[string]any{"emoji": "👍", "count": float64(1), "me": true}}, got["reactions"])
	require.Equal(t, []string{"bob"}, field[string](e.items(e.expect(200, e.do("GET", thumbs, owner, nil))), "username"))
	e.expect(204, e.do("DELETE", thumbs, bob, nil))

	// Pins need MANAGE_MESSAGES in place channels.
	pin := "/api/v1/channels/" + id(general) + "/pins/" + id(m1)
	e.expect(403, e.do("PUT", pin, bob, nil))
	e.expect(204, e.do("PUT", pin, owner, nil))
	pins := e.expect(200, e.do("GET", "/api/v1/channels/"+id(general)+"/pins", bob, nil)).list(t)
	require.Len(t, pins, 1)
	require.Equal(t, true, pins[0].(map[string]any)["is_pinned"])
	e.expect(204, e.do("DELETE", pin, owner, nil))
	require.Empty(t, e.expect(200, e.do("GET", "/api/v1/channels/"+id(general)+"/pins", bob, nil)).list(t))

	// Reports snapshot the message.
	e.expect(201, e.do("POST", "/api/v1/places/talk/reports", owner, map[string]any{"message_id": id(m2), "reason": "spam"}))
	e.expect(422, e.do("POST", "/api/v1/places/talk/reports", bob, map[string]any{"message_id": id(m2), "reason": "spam"}))
	queue := e.items(e.expect(200, e.do("GET", "/api/v1/places/talk/reports", owner, nil)))
	require.Equal(t, "message", queue[0]["target_type"])
	require.Equal(t, "hi @owner!", queue[0]["content_snapshot"])
	require.Equal(t, id(general), queue[0]["channel_id"])

	// Moderators delete with a reason; the author is told and the action is audited.
	e.expect(403, e.do("DELETE", "/api/v1/messages/"+id(m1), bob, nil))
	e.expect(204, e.do("DELETE", "/api/v1/messages/"+id(m2)+"?reason=spam", owner, nil))
	e.expect(404, e.do("GET", "/api/v1/messages/"+id(m2), bob, nil))
	require.Nil(t, e.expect(200, e.do("GET", "/api/v1/messages/"+id(m3), bob, nil)).obj(t)["reply_to"])
	mod := e.notifications(bob)[0]
	require.Equal(t, "moderation", mod["kind"])
	require.Equal(t, "message.delete", mod["data"].(map[string]any)["action"])
	audit := e.items(e.expect(200, e.do("GET", "/api/v1/places/talk/audit-log?action=message.delete", owner, nil)))
	require.Len(t, audit, 1)
	require.Equal(t, "spam", audit[0]["reason"])
	ch := e.expect(200, e.do("GET", "/api/v1/channels/"+id(general), owner, nil)).obj(t)
	require.EqualValues(t, 2, ch["message_count"])
	require.Equal(t, id(m3), ch["last_message_id"])

	// Timed-out members can read but not send, type or react.
	e.expect(200, e.do("PUT", "/api/v1/places/talk/members/"+bobID+"/timeout", owner, map[string]any{"duration": 600}))
	e.expect(403, e.do("POST", "/api/v1/channels/"+id(general)+"/messages", bob, map[string]any{"content": "let me talk"}))
	e.expect(403, e.do("POST", "/api/v1/channels/"+id(general)+"/typing", bob, nil))
	e.expect(403, e.do("PUT", thumbs, bob, nil))
	e.messages(bob, general, "")
	e.expect(200, e.do("DELETE", "/api/v1/places/talk/members/"+bobID+"/timeout", owner, nil))
	e.expect(204, e.do("POST", "/api/v1/channels/"+id(general)+"/typing", bob, nil))

	// Threads branch off a message and inherit the channel's permissions.
	thread := e.expect(201, e.do("POST", "/api/v1/channels/"+id(general)+"/threads", bob, map[string]any{"message_id": id(m1)})).obj(t)
	require.Equal(t, "thread", thread["kind"])
	require.Equal(t, "hello everyone, @bob", thread["name"])
	require.Equal(t, id(general), thread["parent_id"])
	e.expect(409, e.do("POST", "/api/v1/channels/"+id(general)+"/threads", bob, map[string]any{"message_id": id(m1)}))
	e.expect(422, e.do("POST", "/api/v1/channels/"+id(general)+"/threads", bob, map[string]any{}))
	e.send(owner, thread, "in the thread")
	starter := e.expect(200, e.do("GET", "/api/v1/messages/"+id(m1), bob, nil)).obj(t)
	require.Equal(t, id(thread), starter["thread"].(map[string]any)["id"])
	require.EqualValues(t, 1, starter["thread"].(map[string]any)["message_count"])
	require.Len(t, e.items(e.expect(200, e.do("GET", "/api/v1/channels/"+id(general)+"/threads", bob, nil))), 1)
	e.expect(200, e.do("PATCH", "/api/v1/channels/"+id(thread), bob, map[string]any{"is_archived": true}))
	require.Empty(t, e.items(e.expect(200, e.do("GET", "/api/v1/channels/"+id(general)+"/threads", bob, nil))))
	require.Len(t, e.items(e.expect(200, e.do("GET", "/api/v1/channels/"+id(general)+"/threads?archived=true", bob, nil))), 1)
	e.send(bob, thread, "bump")
	require.Equal(t, false, e.expect(200, e.do("GET", "/api/v1/channels/"+id(thread), bob, nil)).obj(t)["is_archived"])

	// Muting a channel silences its mentions.
	e.expect(204, e.do("PUT", "/api/v1/channels/"+id(general)+"/subscription", bob, map[string]any{"level": "muted"}))
	before := len(e.notifications(bob))
	e.send(owner, general, "@bob are you there?")
	require.Len(t, e.notifications(bob), before)
	require.Equal(t, "muted", byID(e.listChannels(bob, "talk"), id(general))["subscription"])

	// Several open reports about one author's messages do not block deleting them.
	r1, r2 := e.send(owner, general, "spam one"), e.send(owner, general, "spam two")
	e.expect(201, e.do("POST", "/api/v1/places/talk/reports", bob, map[string]any{"message_id": id(r1), "reason": "spam"}))
	e.expect(201, e.do("POST", "/api/v1/places/talk/reports", bob, map[string]any{"message_id": id(r2), "reason": "spam"}))
	e.expect(409, e.do("POST", "/api/v1/places/talk/reports", bob, map[string]any{"message_id": id(r2), "reason": "spam"}))
	e.expect(204, e.do("DELETE", "/api/v1/messages/"+id(r1), owner, nil))
	e.expect(204, e.do("DELETE", "/api/v1/messages/"+id(r2), owner, nil))
}

func TestDirectMessages(t *testing.T) {
	e := newEnv(t)
	owner := e.setup()
	ownerID := e.expect(200, e.do("GET", "/api/v1/users/@me", owner, nil)).obj(t)["id"].(string)
	bob, bobID := e.register("bob")
	carol, carolID := e.register("carol")
	dave, daveID := e.register("dave")
	_, eveID := e.register("eve")
	e.createPlace(owner, "hub", "public")
	e.expect(200, e.do("POST", "/api/v1/places/hub/join", bob, nil))
	e.expect(200, e.do("POST", "/api/v1/places/hub/join", carol, nil))

	// Conversations need a shared place.
	e.expect(403, e.do("POST", "/api/v1/users/@me/channels", bob, map[string]any{"recipient_ids": []string{daveID}}))
	e.expect(422, e.do("POST", "/api/v1/users/@me/channels", bob, map[string]any{"recipient_ids": []string{bobID}}))
	dm := e.expect(200, e.do("POST", "/api/v1/users/@me/channels", bob, map[string]any{"recipient_ids": []string{carolID}})).obj(t)
	require.Equal(t, "dm", dm["kind"])
	require.Nil(t, dm["place_id"])
	require.ElementsMatch(t, []string{"bob", "carol"}, field[string](toMaps(dm["recipients"]), "username"))
	again := e.expect(200, e.do("POST", "/api/v1/users/@me/channels", carol, map[string]any{"recipient_ids": []string{bobID}})).obj(t)
	require.Equal(t, id(dm), id(again), "one conversation per pair")
	e.expect(404, e.do("GET", "/api/v1/channels/"+id(dm), dave, nil))
	e.expect(422, e.do("PATCH", "/api/v1/channels/"+id(dm), bob, map[string]any{"name": "x"}))
	e.expect(422, e.do("DELETE", "/api/v1/channels/"+id(dm), bob, nil))

	// Each unread conversation produces one notification; the read state counts messages.
	e.send(bob, dm, "hey carol")
	last := e.send(bob, dm, "you there?")
	notes := e.notifications(carol)
	require.Equal(t, []string{"direct_message"}, kinds(notes))
	require.Equal(t, id(dm), notes[0]["channel_id"])
	carolDMs := e.items(e.expect(200, e.do("GET", "/api/v1/users/@me/channels", carol, nil)))
	require.Len(t, carolDMs, 1)
	require.EqualValues(t, 2, carolDMs[0]["read_state"].(map[string]any)["mention_count"])
	require.Equal(t, true, carolDMs[0]["unread"])

	// Reading clears the count and shares a receipt.
	e.expect(200, e.do("PUT", "/api/v1/channels/"+id(dm)+"/read", carol, map[string]any{"message_id": id(last)}))
	require.Equal(t, true, e.notifications(carol)[0]["read"])
	receipts := e.expect(200, e.do("GET", "/api/v1/channels/"+id(dm)+"/receipts", bob, nil)).list(t)
	byUser := map[string]any{}
	for _, r := range receipts {
		byUser[r.(map[string]any)["user_id"].(string)] = r.(map[string]any)["last_read_message_id"]
	}
	require.Equal(t, id(last), byUser[carolID])
	require.Equal(t, id(last), byUser[bobID], "senders have read their own messages")

	// Any participant may pin; only authors delete in direct messages.
	e.expect(204, e.do("PUT", "/api/v1/channels/"+id(dm)+"/pins/"+id(last), carol, nil))
	e.expect(403, e.do("DELETE", "/api/v1/messages/"+id(last), carol, nil))

	// Muted conversations do not notify.
	e.expect(204, e.do("PUT", "/api/v1/channels/"+id(dm)+"/subscription", carol, map[string]any{"level": "muted"}))
	e.send(bob, dm, "ping")
	require.Len(t, e.notifications(carol), 1)

	// Group conversations.
	group := e.expect(200, e.do("POST", "/api/v1/users/@me/channels", bob, map[string]any{
		"recipient_ids": []string{carolID, ownerID}, "name": "Trio",
	})).obj(t)
	require.Equal(t, "group_dm", group["kind"])
	require.Equal(t, "Trio", group["name"])
	require.Equal(t, bobID, group["owner_id"])
	e.expect(403, e.do("PUT", "/api/v1/channels/"+id(group)+"/recipients/"+daveID, bob, nil))
	e.expect(200, e.do("POST", "/api/v1/places/hub/join", dave, nil))
	withDave := e.expect(200, e.do("PUT", "/api/v1/channels/"+id(group)+"/recipients/"+daveID, carol, nil)).obj(t)
	require.Len(t, withDave["recipients"], 4)
	e.expect(200, e.do("PATCH", "/api/v1/channels/"+id(group), dave, map[string]any{"name": "Quartet"}))
	e.expect(403, e.do("DELETE", "/api/v1/channels/"+id(group)+"/recipients/"+daveID, carol, nil))
	e.expect(204, e.do("DELETE", "/api/v1/channels/"+id(group)+"/recipients/"+daveID, bob, nil))
	e.expect(404, e.do("GET", "/api/v1/channels/"+id(group), dave, nil))

	// When the owner leaves, someone else takes over.
	e.expect(204, e.do("DELETE", "/api/v1/channels/"+id(group)+"/recipients/"+bobID, bob, nil))
	after := e.expect(200, e.do("GET", "/api/v1/channels/"+id(group), carol, nil)).obj(t)
	require.Equal(t, "Quartet", after["name"])
	require.Contains(t, []string{carolID, ownerID}, after["owner_id"])
	require.Len(t, after["recipients"], 2)

	// Presence is visible to people sharing a place or conversation, and offline by default.
	pres := e.expect(200, e.do("GET", "/api/v1/presences?user_ids="+carolID+","+eveID, bob, nil)).list(t)
	require.Equal(t, []any{map[string]any{"user_id": carolID, "status": "offline"}}, pres)
}

func toMaps(v any) []map[string]any {
	raw := v.([]any)
	out := make([]map[string]any, len(raw))
	for i, it := range raw {
		out[i] = it.(map[string]any)
	}
	return out
}

func TestChatRateLimitTier(t *testing.T) {
	e := newEnv(t, func(o *envOpts) {
		o.mutate = func(c *config.Config) { c.RateLimit.Chat = "2-M" }
	})
	owner := e.setup()
	e.createPlace(owner, "quick", "public")
	general := e.channel(owner, "quick", map[string]any{"name": "general"})
	e.send(owner, general, "one")
	r := e.expect(201, e.do("POST", "/api/v1/channels/"+id(general)+"/messages", owner, map[string]any{"content": "two"}))
	require.Equal(t, "2", r.Header.Get("X-RateLimit-Limit"))
	e.expect(429, e.do("POST", "/api/v1/channels/"+id(general)+"/messages", owner, map[string]any{"content": "three"}))
	// Other endpoints use their own tier.
	e.expect(200, e.do("GET", "/api/v1/channels/"+id(general)+"/messages", owner, nil))
}
