package api_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/parkerbrown98/gotalk-server/internal/permissions"
)

func (e *env) board(token, place string, body map[string]any) map[string]any {
	e.t.Helper()
	return e.expect(201, e.do("POST", "/api/v1/places/"+place+"/boards", token, body)).obj(e.t)
}

// topic starts a topic and returns its ID and opening post ID.
func (e *env) topic(token string, board map[string]any, title, content string, tags ...string) (topicID, postID string) {
	e.t.Helper()
	body := map[string]any{"title": title, "content": content}
	if len(tags) > 0 {
		body["tags"] = tags
	}
	res := e.expect(201, e.do("POST", "/api/v1/boards/"+board["id"].(string)+"/topics", token, body)).obj(e.t)
	return res["topic"].(map[string]any)["id"].(string), res["post"].(map[string]any)["id"].(string)
}

func (e *env) reply(token, topicID, content, parentID string) map[string]any {
	e.t.Helper()
	body := map[string]any{"content": content}
	if parentID != "" {
		body["parent_id"] = parentID
	}
	return e.expect(201, e.do("POST", "/api/v1/topics/"+topicID+"/posts", token, body)).obj(e.t)
}

func (e *env) items(r resp) []map[string]any {
	e.t.Helper()
	raw := r.obj(e.t)["items"].([]any)
	out := make([]map[string]any, len(raw))
	for i, v := range raw {
		out[i] = v.(map[string]any)
	}
	return out
}

func (e *env) notifications(token string) []map[string]any {
	e.t.Helper()
	return e.items(e.expect(200, e.do("GET", "/api/v1/users/@me/notifications", token, nil)))
}

func (e *env) defaultRoleID(token, place string) string {
	e.t.Helper()
	for _, r := range e.expect(200, e.do("GET", "/api/v1/places/"+place+"/roles", token, nil)).list(e.t) {
		if role := r.(map[string]any); role["is_default"] == true {
			return role["id"].(string)
		}
	}
	e.t.Fatal("no default role")
	return ""
}

func field[T any](items []map[string]any, key string) []T {
	out := make([]T, len(items))
	for i, it := range items {
		out[i], _ = it[key].(T)
	}
	return out
}

func kinds(items []map[string]any) []string { return field[string](items, "kind") }

func TestForumLifecycle(t *testing.T) {
	e := newEnv(t)
	owner := e.setup()
	bob, _ := e.register("bob")
	carol, _ := e.register("carol")
	e.createPlace(owner, "gophers", "public")
	e.expect(200, e.do("POST", "/api/v1/places/gophers/join", bob, nil))

	// Board tree: category > board > sub-board, at most three levels.
	cat := e.board(owner, "gophers", map[string]any{"slug": "general", "name": "General", "kind": "category"})
	lobby := e.board(owner, "gophers", map[string]any{"slug": "lobby", "name": "Lobby", "parent_id": cat["id"]})
	help := e.board(owner, "gophers", map[string]any{
		"slug": "help", "name": "Help", "parent_id": lobby["id"], "reply_mode": "threaded", "solutions_enabled": true,
	})
	e.expect(422, e.do("POST", "/api/v1/places/gophers/boards", owner, map[string]any{"slug": "deep", "name": "Deep", "parent_id": help["id"]}))
	e.expect(422, e.do("POST", "/api/v1/places/gophers/boards", owner, map[string]any{"slug": "subcat", "name": "x", "kind": "category", "parent_id": cat["id"]}))
	e.expect(409, e.do("POST", "/api/v1/places/gophers/boards", owner, map[string]any{"slug": "lobby", "name": "dup"}))
	e.expect(403, e.do("POST", "/api/v1/places/gophers/boards", bob, map[string]any{"slug": "mine", "name": "Mine"}))
	boards := e.expect(200, e.do("GET", "/api/v1/places/gophers/boards", "", nil)).list(t)
	require.Len(t, boards, 3)
	require.Equal(t, "help", boards[2].(map[string]any)["slug"], "depth-first order")
	require.Equal(t, true, boards[1].(map[string]any)["is_public"])
	require.EqualValues(t, permissions.ViewBoards, boards[1].(map[string]any)["my_permissions"], "guests can only read")
	e.expect(422, e.do("POST", "/api/v1/boards/"+cat["id"].(string)+"/topics", owner, map[string]any{"title": "x", "content": "y"}))

	// Topics: anyone can read a public place; only members may post.
	topicID, firstID := e.topic(bob, lobby, "Hello, Gophers!", "First post", "Go", "go", "intro")
	topic := e.expect(200, e.do("GET", "/api/v1/topics/"+topicID, "", nil)).obj(t)
	require.Equal(t, "hello-gophers", topic["slug"])
	require.Equal(t, []any{"go", "intro"}, topic["tags"])
	require.Equal(t, "bob", topic["author"].(map[string]any)["username"])
	require.NotContains(t, topic, "unread_count", "no caller state for anonymous readers")
	e.expect(401, e.do("POST", "/api/v1/boards/"+lobby["id"].(string)+"/topics", "", map[string]any{"title": "x", "content": "y"}))
	e.expect(403, e.do("POST", "/api/v1/boards/"+lobby["id"].(string)+"/topics", carol, map[string]any{"title": "x", "content": "y"}))
	e.expect(200, e.do("GET", "/api/v1/topics/"+topicID+"/posts", carol, nil))
	e.expect(422, e.do("POST", "/api/v1/boards/"+lobby["id"].(string)+"/topics", bob, map[string]any{"title": "x", "content": "   "}))

	// Replies, edits and revisions.
	welcome := e.reply(owner, topicID, "Welcome!", "")
	require.EqualValues(t, 2, welcome["post_number"])
	e.expect(403, e.do("PATCH", "/api/v1/posts/"+welcome["id"].(string), bob, map[string]any{"content": "hijack"}))
	edited := e.expect(200, e.do("PATCH", "/api/v1/posts/"+firstID, bob, map[string]any{"content": "First post, edited"})).obj(t)
	require.EqualValues(t, 1, edited["edit_count"])
	e.expect(200, e.do("PATCH", "/api/v1/posts/"+firstID, owner, map[string]any{"content": "Moderated"}))
	revs := e.expect(200, e.do("GET", "/api/v1/posts/"+firstID+"/revisions", "", nil)).list(t)
	require.Len(t, revs, 2)
	require.Equal(t, "First post, edited", revs[0].(map[string]any)["content"], "newest first")
	require.Equal(t, "First post", revs[1].(map[string]any)["content"])

	// Reactions.
	thumbs := "/api/v1/posts/" + firstID + "/reactions/" + url.PathEscape("👍")
	e.expect(204, e.do("PUT", thumbs, owner, nil))
	e.expect(204, e.do("PUT", thumbs, owner, nil))
	e.expect(204, e.do("PUT", "/api/v1/posts/"+firstID+"/reactions/heart", owner, nil))
	e.expect(422, e.do("PUT", "/api/v1/posts/"+firstID+"/reactions/"+url.PathEscape("not valid"), owner, nil))
	post := e.expect(200, e.do("GET", "/api/v1/posts/"+firstID, owner, nil)).obj(t)
	require.EqualValues(t, 2, post["reaction_count"])
	require.Equal(t, []any{
		map[string]any{"emoji": "👍", "count": float64(1), "me": true},
		map[string]any{"emoji": "heart", "count": float64(1), "me": true},
	}, post["reactions"])
	anonPost := e.expect(200, e.do("GET", "/api/v1/posts/"+firstID, "", nil)).obj(t)
	require.Equal(t, false, anonPost["reactions"].([]any)[0].(map[string]any)["me"])
	reactors := e.items(e.expect(200, e.do("GET", thumbs, "", nil)))
	require.Equal(t, []string{"owner"}, field[string](reactors, "username"))
	e.expect(204, e.do("DELETE", "/api/v1/posts/"+firstID+"/reactions/heart", owner, nil))
	require.EqualValues(t, 1, e.expect(200, e.do("GET", "/api/v1/posts/"+firstID, "", nil)).obj(t)["reaction_count"])

	// Read state: posting marks your own post read; reads never move backwards.
	listed := e.items(e.expect(200, e.do("GET", "/api/v1/boards/"+lobby["id"].(string)+"/topics", bob, nil)))
	require.EqualValues(t, 1, listed[0]["unread_count"])
	e.expect(204, e.do("PUT", "/api/v1/topics/"+topicID+"/read", bob, map[string]any{"post_number": 99}))
	listed = e.items(e.expect(200, e.do("GET", "/api/v1/boards/"+lobby["id"].(string)+"/topics", bob, nil)))
	require.EqualValues(t, 0, listed[0]["unread_count"])
	require.EqualValues(t, 2, listed[0]["last_read_post_number"])
	require.Equal(t, "watching", listed[0]["subscription"], "authors watch their topics")

	// Deleted replies leave tombstones; only moderators still see the content.
	e.expect(422, e.do("DELETE", "/api/v1/posts/"+firstID, bob, nil))
	oops := e.reply(bob, topicID, "Oops", "")
	e.expect(204, e.do("DELETE", "/api/v1/posts/"+oops["id"].(string), bob, nil))
	e.expect(404, e.do("DELETE", "/api/v1/posts/"+oops["id"].(string), bob, nil))
	posts := e.items(e.expect(200, e.do("GET", "/api/v1/topics/"+topicID+"/posts", bob, nil)))
	require.Len(t, posts, 3)
	require.Equal(t, true, posts[2]["deleted"])
	require.Equal(t, "", posts[2]["content"])
	require.Equal(t, "Oops", e.items(e.expect(200, e.do("GET", "/api/v1/topics/"+topicID+"/posts", owner, nil)))[2]["content"])
	require.EqualValues(t, 2, e.expect(200, e.do("GET", "/api/v1/topics/"+topicID, "", nil)).obj(t)["post_count"])

	// Locking and archiving.
	e.expect(403, e.do("PATCH", "/api/v1/topics/"+topicID, bob, map[string]any{"is_locked": true}))
	locked := e.expect(200, e.do("PATCH", "/api/v1/topics/"+topicID, owner, map[string]any{"is_locked": true, "is_pinned": true})).obj(t)
	require.Equal(t, true, locked["is_pinned"])
	e.expect(403, e.do("POST", "/api/v1/topics/"+topicID+"/posts", bob, map[string]any{"content": "let me in"}))
	e.expect(403, e.do("PATCH", "/api/v1/topics/"+topicID, bob, map[string]any{"title": "Renamed"}))
	e.reply(owner, topicID, "Moderators can still reply", "")
	e.expect(200, e.do("PATCH", "/api/v1/topics/"+topicID, owner, map[string]any{"is_archived": true}))
	e.expect(409, e.do("POST", "/api/v1/topics/"+topicID+"/posts", owner, map[string]any{"content": "too late"}))
	require.Empty(t, e.items(e.expect(200, e.do("GET", "/api/v1/boards/"+lobby["id"].(string)+"/topics", "", nil))))
	require.Len(t, e.items(e.expect(200, e.do("GET", "/api/v1/boards/"+lobby["id"].(string)+"/topics?archived=true", "", nil))), 1)

	// Threaded Q&A board.
	qID, qFirst := e.topic(bob, help, "How do I X?", "Help please")
	a := e.reply(owner, qID, "Try A", "")
	e.reply(bob, qID, "Thanks, but why?", a["id"].(string))
	e.reply(owner, qID, "Or C", "")
	e.reply(owner, qID, "Because of B", a["id"].(string))
	thread := e.items(e.expect(200, e.do("GET", "/api/v1/topics/"+qID+"/posts", "", nil)))
	require.Equal(t, []float64{1, 2, 3, 5, 4}, field[float64](thread, "post_number"))
	require.Equal(t, []float64{0, 0, 1, 1, 0}, field[float64](thread, "depth"))
	e.expect(422, e.do("POST", "/api/v1/topics/"+qID+"/posts", owner, map[string]any{"content": "x", "parent_id": firstID}))

	e.expect(422, e.do("PUT", "/api/v1/topics/"+qID+"/solution", bob, map[string]any{"post_id": qFirst}))
	e.expect(403, e.do("PUT", "/api/v1/topics/"+qID+"/solution", carol, map[string]any{"post_id": a["id"]}))
	solved := e.expect(200, e.do("PUT", "/api/v1/topics/"+qID+"/solution", bob, map[string]any{"post_id": a["id"]})).obj(t)
	require.Equal(t, a["id"], solved["solution_post_id"])
	e.expect(422, e.do("PUT", "/api/v1/topics/"+topicID+"/solution", owner, map[string]any{"post_id": welcome["id"]}))
	cleared := e.expect(200, e.do("DELETE", "/api/v1/topics/"+qID+"/solution", bob, nil)).obj(t)
	require.Nil(t, cleared["solution_post_id"])

	// Moving topics keeps board counters in step.
	other := e.board(owner, "gophers", map[string]any{"slug": "other", "name": "Other"})
	e.expect(403, e.do("PATCH", "/api/v1/topics/"+qID, bob, map[string]any{"board_id": other["id"]}))
	e.expect(422, e.do("PATCH", "/api/v1/topics/"+qID, owner, map[string]any{"board_id": cat["id"]}))
	moved := e.expect(200, e.do("PATCH", "/api/v1/topics/"+qID, owner, map[string]any{"board_id": other["id"]})).obj(t)
	require.Equal(t, other["id"], moved["board_id"])
	otherBoard := e.expect(200, e.do("GET", "/api/v1/boards/"+other["id"].(string), "", nil)).obj(t)
	require.EqualValues(t, 1, otherBoard["topic_count"])
	require.EqualValues(t, 5, otherBoard["post_count"])
	require.EqualValues(t, 0, e.expect(200, e.do("GET", "/api/v1/boards/"+help["id"].(string), "", nil)).obj(t)["topic_count"])
	latest := e.items(e.expect(200, e.do("GET", "/api/v1/places/gophers/topics", "", nil)))
	require.Equal(t, []string{qID}, field[string](latest, "id"), "latest excludes archived topics")
	tags := e.expect(200, e.do("GET", "/api/v1/places/gophers/tags?q=in", "", nil)).list(t)
	require.Equal(t, "intro", tags[0].(map[string]any)["tag"])

	// Deleting topics: authors only until someone replies; moderators always.
	e.expect(409, e.do("DELETE", "/api/v1/topics/"+qID, bob, nil))
	e.expect(204, e.do("DELETE", "/api/v1/topics/"+qID+"?reason=off-topic", owner, nil))
	e.expect(404, e.do("GET", "/api/v1/topics/"+qID, owner, nil))
	e.expect(404, e.do("GET", "/api/v1/posts/"+a["id"].(string), owner, nil))
	n := e.notifications(bob)
	require.Equal(t, "moderation", n[0]["kind"])
	require.Equal(t, "topic.delete", n[0]["data"].(map[string]any)["action"])
	require.Equal(t, "off-topic", n[0]["data"].(map[string]any)["reason"])
	soloID, _ := e.topic(bob, other, "Never mind", "nothing")
	e.expect(204, e.do("DELETE", "/api/v1/topics/"+soloID, bob, nil))

	// Boards must be emptied before deletion.
	e.expect(409, e.do("DELETE", "/api/v1/boards/"+lobby["id"].(string), owner, nil))
	e.expect(204, e.do("DELETE", "/api/v1/boards/"+help["id"].(string), owner, nil))
	e.expect(409, e.do("DELETE", "/api/v1/boards/"+lobby["id"].(string), owner, nil))

	audit := e.items(e.expect(200, e.do("GET", "/api/v1/places/gophers/audit-log?action=topic", owner, nil)))
	require.Contains(t, field[string](audit, "action"), "topic.delete")
	require.Contains(t, field[string](audit, "action"), "topic.update")
	postAudit := e.items(e.expect(200, e.do("GET", "/api/v1/places/gophers/audit-log?action=post.edit", owner, nil)))
	require.Len(t, postAudit, 1, "only edits of other people's posts are audited")
}

func TestBoardPermissionOverwrites(t *testing.T) {
	e := newEnv(t)
	owner := e.setup()
	bob, _ := e.register("bob")
	mod, modID := e.register("maria")
	e.createPlace(owner, "club", "public")
	for _, tok := range []string{bob, mod} {
		e.expect(200, e.do("POST", "/api/v1/places/club/join", tok, nil))
	}
	everyone := e.defaultRoleID(owner, "club")
	modRole := e.expect(201, e.do("POST", "/api/v1/places/club/roles", owner, map[string]any{
		"name": "Mods", "permissions": permissions.ManageBoards | permissions.ManagePosts,
	})).obj(t)["id"].(string)
	e.expect(200, e.do("PUT", "/api/v1/places/club/members/"+modID+"/roles/"+modRole, owner, nil))

	staff := e.board(owner, "club", map[string]any{"slug": "staff", "name": "Staff"})
	news := e.board(owner, "club", map[string]any{"slug": "news", "name": "News"})
	ow := func(token string, board map[string]any, role string, allow, deny permissions.Permission) resp {
		return e.do("PUT", "/api/v1/boards/"+board["id"].(string)+"/overwrites/"+role, token, map[string]any{"allow": allow, "deny": deny})
	}
	e.expect(200, ow(owner, staff, everyone, 0, permissions.ViewBoards))
	e.expect(200, ow(owner, staff, modRole, permissions.ViewBoards, 0))
	e.expect(200, ow(owner, news, everyone, 0, permissions.CreateTopics))
	e.expect(422, ow(owner, news, everyone, permissions.KickMembers, 0))
	e.expect(422, ow(owner, news, everyone, permissions.ViewBoards, permissions.ViewBoards))
	e.expect(403, ow(bob, news, everyone, 0, permissions.ViewBoards))
	// Mods cannot edit overwrites of their own top role.
	e.expect(403, ow(mod, news, modRole, 0, permissions.ViewBoards))
	ows := e.expect(200, e.do("GET", "/api/v1/boards/"+staff["id"].(string)+"/overwrites", owner, nil)).list(t)
	require.Len(t, ows, 2)

	slugs := func(token string) []string {
		var out []string
		for _, b := range e.expect(200, e.do("GET", "/api/v1/places/club/boards", token, nil)).list(t) {
			out = append(out, b.(map[string]any)["slug"].(string))
		}
		return out
	}
	require.Equal(t, []string{"news"}, slugs(bob))
	require.Equal(t, []string{"news"}, slugs(""))
	require.Equal(t, []string{"staff", "news"}, slugs(mod))
	e.expect(404, e.do("GET", "/api/v1/boards/"+staff["id"].(string), bob, nil))

	staffTopic, staffPost := e.topic(mod, staff, "Secret plans", "zebra crossing strategy")
	e.expect(404, e.do("GET", "/api/v1/topics/"+staffTopic, bob, nil))
	e.expect(404, e.do("GET", "/api/v1/topics/"+staffTopic, "", nil))
	e.expect(404, e.do("GET", "/api/v1/posts/"+staffPost, bob, nil))
	e.expect(404, e.do("POST", "/api/v1/places/club/reports", bob, map[string]any{"post_id": staffPost, "reason": "spam"}))

	// Announcements: members can reply but not start topics.
	e.expect(403, e.do("POST", "/api/v1/boards/"+news["id"].(string)+"/topics", bob, map[string]any{"title": "x", "content": "y"}))
	annID, _ := e.topic(owner, news, "Rules", "Be nice to everyone")
	e.reply(bob, annID, "Got it", "")

	// Guest visibility drives instance-wide search.
	search := func(q string) int {
		return len(e.items(e.expect(200, e.do("GET", "/api/v1/search?q="+url.QueryEscape(q), "", nil))))
	}
	require.Equal(t, 0, search("zebra"))
	require.Equal(t, 1, search("nice"))
	require.Len(t, e.items(e.expect(200, e.do("GET", "/api/v1/places/club/search?q=zebra", mod, nil))), 1)
	require.Empty(t, e.items(e.expect(200, e.do("GET", "/api/v1/places/club/search?q=zebra", bob, nil))))

	// Removing VIEW_BOARDS from @everyone hides the place's boards from guests and members alike.
	e.expect(200, e.do("PATCH", "/api/v1/places/club/roles/"+everyone, owner, map[string]any{
		"permissions": permissions.Default &^ permissions.ViewBoards,
	}))
	require.Empty(t, slugs(""))
	require.Equal(t, 0, search("nice"))
	e.expect(200, e.do("PATCH", "/api/v1/places/club/roles/"+everyone, owner, map[string]any{"permissions": permissions.Default}))
	require.Equal(t, 1, search("nice"))

	e.expect(204, e.do("DELETE", "/api/v1/boards/"+staff["id"].(string)+"/overwrites/"+everyone, owner, nil))
	e.expect(404, e.do("DELETE", "/api/v1/boards/"+staff["id"].(string)+"/overwrites/"+everyone, owner, nil))
	require.Equal(t, []string{"staff", "news"}, slugs(bob))

	// Content in non-public places is members-only.
	e.createPlace(owner, "inner", "invite_only")
	e.expect(403, e.do("GET", "/api/v1/places/inner/boards", bob, nil))
	e.createPlace(owner, "secret", "private")
	e.expect(404, e.do("GET", "/api/v1/places/secret/boards", bob, nil))
}

func TestSearch(t *testing.T) {
	e := newEnv(t)
	owner := e.setup()
	bob, _ := e.register("bob")
	e.createPlace(owner, "devs", "public")
	e.expect(200, e.do("POST", "/api/v1/places/devs/join", bob, nil))
	talk := e.board(owner, "devs", map[string]any{"slug": "talk", "name": "Talk", "solutions_enabled": true})

	t1, _ := e.topic(owner, talk, "Deploying with Kubernetes", "We use helm charts in production.", "k8s")
	t2, t2First := e.topic(bob, talk, "Docker tips", "Multi-stage builds keep images small. Kubernetes optional.")
	r := e.reply(bob, t1, "Kubernetes operators are great", "")
	e.createPlace(owner, "hidden", "private")
	hidden := e.board(owner, "hidden", map[string]any{"slug": "vault", "name": "Vault"})
	e.topic(owner, hidden, "Kubernetes secrets", "classified")

	search := func(token, path string) []map[string]any {
		return e.items(e.expect(200, e.do("GET", path, token, nil)))
	}
	hits := search("", "/api/v1/search?q=kubernetes")
	require.Len(t, hits, 3, "private places are never searched instance-wide")
	require.Equal(t, "Deploying with Kubernetes", hits[0]["topic"].(map[string]any)["title"], "title matches rank highest")
	require.Equal(t, "devs", hits[0]["place"].(map[string]any)["slug"])
	snippets := strings.Join(field[string](hits, "snippet"), " ")
	require.Contains(t, snippets, "**Kubernetes**")

	require.Len(t, search("", "/api/v1/search?q=kubernetes&author=bob"), 2)
	require.Len(t, search("", "/api/v1/search?q=kubernetes&tag=k8s"), 2)
	require.Len(t, search("", "/api/v1/search?q=kubernetes&topics_only=true"), 2)
	require.Empty(t, search("", "/api/v1/search?q=kubernetes&solved=true"))
	require.Empty(t, search("", "/api/v1/search?q=kubernetes&author=nobody"))
	require.Equal(t, r["id"], search("", "/api/v1/search?q=kubernetes&sort=newest")[0]["post_id"])
	require.Len(t, search("", "/api/v1/search?q="+url.QueryEscape("kubernetes -docker")), 2)
	require.Len(t, search("", "/api/v1/search?q=kubernetes&after="+url.QueryEscape(time.Now().Add(time.Hour).Format(time.RFC3339))), 0)
	e.expect(422, e.do("GET", "/api/v1/search", "", nil))
	e.expect(422, e.do("GET", "/api/v1/search?q=x&after=yesterday", "", nil))

	require.Len(t, search(owner, "/api/v1/places/hidden/search?q=kubernetes"), 1)
	e.expect(404, e.do("GET", "/api/v1/places/hidden/search?q=kubernetes", bob, nil))
	require.Len(t, search("", "/api/v1/places/devs/search?q=kubernetes&board="+talk["id"].(string)), 3)

	// The index follows edits, title changes and deletions.
	e.expect(200, e.do("PATCH", "/api/v1/posts/"+t2First, bob, map[string]any{"content": "Multi-stage builds only."}))
	e.expect(200, e.do("PATCH", "/api/v1/topics/"+t1, owner, map[string]any{"title": "Deploying with Nomad"}))
	e.expect(204, e.do("DELETE", "/api/v1/posts/"+r["id"].(string), bob, nil))
	require.Empty(t, search("", "/api/v1/search?q=kubernetes"))
	require.Equal(t, t1, search("", "/api/v1/search?q=nomad")[0]["topic"].(map[string]any)["id"])
	require.Equal(t, t2, search("", "/api/v1/search?q=multi")[0]["topic"].(map[string]any)["id"])

	// Leaving public visibility removes a place from instance-wide search.
	e.expect(200, e.do("PATCH", "/api/v1/places/devs", owner, map[string]any{"visibility": "invite_only"}))
	require.Empty(t, search("", "/api/v1/search?q=nomad"))
	require.Len(t, search(bob, "/api/v1/places/devs/search?q=nomad"), 1)
}

func TestNotifications(t *testing.T) {
	e := newEnv(t)
	owner := e.setup()
	bob, _ := e.register("bob")
	carol, _ := e.register("carol")
	dave, _ := e.register("dave")
	e.createPlace(owner, "news", "public")
	for _, tok := range []string{bob, carol, dave} {
		e.expect(200, e.do("POST", "/api/v1/places/news/join", tok, nil))
	}
	general := e.board(owner, "news", map[string]any{"slug": "general", "name": "General"})
	staff := e.board(owner, "news", map[string]any{"slug": "staff", "name": "Staff"})
	e.expect(200, e.do("PUT", "/api/v1/boards/"+staff["id"].(string)+"/overwrites/"+e.defaultRoleID(owner, "news"), owner,
		map[string]any{"allow": 0, "deny": permissions.ViewBoards}))

	e.expect(204, e.do("PUT", "/api/v1/boards/"+general["id"].(string)+"/subscription", bob, map[string]any{"level": "watching"}))
	e.expect(204, e.do("PUT", "/api/v1/places/news/subscription", dave, map[string]any{"level": "muted"}))
	e.expect(422, e.do("PUT", "/api/v1/places/news/subscription", dave, map[string]any{"level": "loud"}))
	require.Equal(t, "watching", e.expect(200, e.do("GET", "/api/v1/boards/"+general["id"].(string), bob, nil)).obj(t)["subscription"])

	t1, _ := e.topic(owner, general, "Launch", "Hey @carol and @dave, look! Not `@bob` though.")
	require.Equal(t, []string{"new_topic"}, kinds(e.notifications(bob)), "code spans are not mentions")
	carolN := e.notifications(carol)
	require.Equal(t, []string{"mention"}, kinds(carolN))
	require.Equal(t, "Launch", carolN[0]["data"].(map[string]any)["topic_title"])
	require.Equal(t, "owner", carolN[0]["actor"].(map[string]any)["username"])
	require.Equal(t, t1, carolN[0]["topic_id"])
	require.Empty(t, e.notifications(dave), "muted place")

	// Topic authors hear about replies; watchers hear about everything.
	e.expect(204, e.do("PUT", "/api/v1/topics/"+t1+"/subscription", carol, map[string]any{"level": "watching"}))
	bobReply := e.reply(bob, t1, "Congrats!", "")
	require.Equal(t, []string{"reply"}, kinds(e.notifications(owner)))
	require.Equal(t, []string{"topic_reply", "mention"}, kinds(e.notifications(carol)))
	e.reply(carol, t1, "Agreed", bobReply["id"].(string))
	require.Equal(t, []string{"reply", "new_topic"}, kinds(e.notifications(bob)), "direct replies notify the parent's author")
	require.Equal(t, []string{"topic_reply", "reply"}, kinds(e.notifications(owner)))

	// Muting a topic silences it, mentions included.
	e.expect(204, e.do("PUT", "/api/v1/topics/"+t1+"/subscription", carol, map[string]any{"level": "muted"}))
	e.reply(owner, t1, "@carol thanks", "")
	require.Len(t, e.notifications(carol), 2)

	// Mentions never leak boards the mentioned user cannot see.
	e.topic(owner, staff, "Staff only", "ping @bob")
	require.Len(t, e.notifications(bob), 2)

	// One reaction notification per reactor, however many emoji.
	e.expect(204, e.do("PUT", "/api/v1/posts/"+bobReply["id"].(string)+"/reactions/"+url.PathEscape("🎉"), owner, nil))
	e.expect(204, e.do("PUT", "/api/v1/posts/"+bobReply["id"].(string)+"/reactions/heart", owner, nil))
	bobN := e.notifications(bob)
	require.Equal(t, []string{"reaction", "reply", "new_topic"}, kinds(bobN))
	require.Equal(t, "🎉", bobN[0]["data"].(map[string]any)["emoji"])

	// Unread counts, marking read, dismissing.
	count := func() float64 {
		return e.expect(200, e.do("GET", "/api/v1/users/@me/notifications/unread-count", bob, nil)).obj(t)["count"].(float64)
	}
	require.EqualValues(t, 3, count())
	e.expect(204, e.do("POST", "/api/v1/users/@me/notifications/"+bobN[0]["id"].(string)+"/read", bob, nil))
	require.EqualValues(t, 2, count())
	require.Len(t, e.items(e.expect(200, e.do("GET", "/api/v1/users/@me/notifications?unread=true", bob, nil))), 2)
	e.expect(404, e.do("POST", "/api/v1/users/@me/notifications/"+bobN[0]["id"].(string)+"/read", carol, nil))
	e.expect(204, e.do("POST", "/api/v1/users/@me/notifications/read-all", bob, nil))
	require.EqualValues(t, 0, count())
	e.expect(204, e.do("DELETE", "/api/v1/users/@me/notifications/"+bobN[0]["id"].(string), bob, nil))
	e.expect(404, e.do("DELETE", "/api/v1/users/@me/notifications/"+bobN[0]["id"].(string), bob, nil))
	require.Len(t, e.notifications(bob), 2)

	// Deleted accounts keep their posts, unattributed.
	e.expect(204, e.do("DELETE", "/api/v1/users/@me", bob, map[string]any{"password": "bob-password-1"}))
	got := e.expect(200, e.do("GET", "/api/v1/posts/"+bobReply["id"].(string), "", nil)).obj(t)
	require.Nil(t, got["author"])
	require.Equal(t, "Congrats!", got["content"])
}

func TestModerationTools(t *testing.T) {
	e := newEnv(t)
	owner := e.setup()
	ownerID := e.userID(owner)
	bob, bobID := e.register("bob")
	carol, carolID := e.register("carol")
	mod, modID := e.register("maria")
	e.createPlace(owner, "town", "public")
	for _, tok := range []string{bob, carol, mod} {
		e.expect(200, e.do("POST", "/api/v1/places/town/join", tok, nil))
	}
	modRole := e.expect(201, e.do("POST", "/api/v1/places/town/roles", owner, map[string]any{
		"name": "Mods", "permissions": permissions.ModerateMembers | permissions.ManageReports |
			permissions.ViewAuditLog | permissions.BanMembers | permissions.KickMembers,
	})).obj(t)["id"].(string)
	e.expect(200, e.do("PUT", "/api/v1/places/town/members/"+modID+"/roles/"+modRole, owner, nil))
	square := e.board(owner, "town", map[string]any{"slug": "square", "name": "Square"})
	topicID, _ := e.topic(bob, square, "Buy cheap stuff", "spam spam spam")
	spam := e.reply(bob, topicID, "more spam", "")

	// Reports.
	report := e.expect(201, e.do("POST", "/api/v1/places/town/reports", carol, map[string]any{
		"post_id": spam["id"], "reason": "spam", "details": "obvious"})).obj(t)
	require.Equal(t, "open", report["status"])
	e.expect(409, e.do("POST", "/api/v1/places/town/reports", carol, map[string]any{"post_id": spam["id"], "reason": "spam"}))
	e.expect(422, e.do("POST", "/api/v1/places/town/reports", bob, map[string]any{"post_id": spam["id"], "reason": "spam"}))
	e.expect(422, e.do("POST", "/api/v1/places/town/reports", carol, map[string]any{"reason": "spam"}))
	e.expect(201, e.do("POST", "/api/v1/places/town/reports", carol, map[string]any{"user_id": bobID, "reason": "harassment"}))
	e.expect(403, e.do("GET", "/api/v1/places/town/reports", bob, nil))
	queue := e.items(e.expect(200, e.do("GET", "/api/v1/places/town/reports?status=open", mod, nil)))
	require.Len(t, queue, 2)
	postReport := queue[1]
	require.Equal(t, "more spam", postReport["content_snapshot"])
	require.Equal(t, "bob", postReport["target_user"].(map[string]any)["username"])
	require.Equal(t, topicID, postReport["topic_id"])
	resolved := e.expect(200, e.do("PATCH", "/api/v1/places/town/reports/"+report["id"].(string), mod,
		map[string]any{"status": "resolved", "resolution_note": "removed"})).obj(t)
	require.Equal(t, "resolved", resolved["status"])
	require.Equal(t, modID, resolved["resolved_by"])
	require.Len(t, e.items(e.expect(200, e.do("GET", "/api/v1/places/town/reports?status=open", mod, nil))), 1)

	// Timeouts: members can still read but not participate.
	e.expect(403, e.do("PUT", "/api/v1/places/town/members/"+carolID+"/timeout", bob, map[string]any{"duration": 600}))
	e.expect(403, e.do("PUT", "/api/v1/places/town/members/"+ownerID+"/timeout", mod, map[string]any{"duration": 600}))
	e.expect(422, e.do("PUT", "/api/v1/places/town/members/"+bobID+"/timeout", mod, map[string]any{"duration": 5}))
	member := e.expect(200, e.do("PUT", "/api/v1/places/town/members/"+bobID+"/timeout", mod,
		map[string]any{"duration": 600, "reason": "cool off"})).obj(t)
	require.NotNil(t, member["timeout_until"])
	denied := e.expect(403, e.do("POST", "/api/v1/topics/"+topicID+"/posts", bob, map[string]any{"content": "still here"}))
	require.Contains(t, string(denied.Raw), "timed out")
	e.expect(403, e.do("PUT", "/api/v1/posts/"+spam["id"].(string)+"/reactions/heart", bob, nil))
	e.expect(200, e.do("GET", "/api/v1/topics/"+topicID, bob, nil))
	perms := e.expect(200, e.do("GET", "/api/v1/places/town/permissions/@me", bob, nil)).obj(t)
	require.NotContains(t, perms["permission_names"], "REPLY_TO_TOPICS")
	require.Contains(t, perms["permission_names"], "VIEW_BOARDS")
	cleared := e.expect(200, e.do("DELETE", "/api/v1/places/town/members/"+bobID+"/timeout", mod, nil)).obj(t)
	require.Nil(t, cleared["timeout_until"])
	e.reply(bob, topicID, "sorry", "")

	// Warnings.
	e.expect(422, e.do("POST", "/api/v1/places/town/members/"+bobID+"/warnings", mod, map[string]any{"reason": ""}))
	e.expect(204, e.do("POST", "/api/v1/places/town/members/"+bobID+"/warnings", mod, map[string]any{"reason": "Please stop"}))
	var actions []string
	for _, n := range e.notifications(bob) {
		if n["kind"] == "moderation" {
			actions = append(actions, n["data"].(map[string]any)["action"].(string))
		}
	}
	require.Equal(t, []string{"member.warn", "member.timeout"}, actions)

	// Temporary bans lift themselves.
	e.expect(204, e.do("PUT", "/api/v1/places/town/bans/"+carolID, mod, map[string]any{"reason": "break", "duration": 1}))
	bans := e.items(e.expect(200, e.do("GET", "/api/v1/places/town/bans", mod, nil)))
	require.NotNil(t, bans[0]["expires_at"])
	e.expect(403, e.do("POST", "/api/v1/places/town/join", carol, nil))
	time.Sleep(2 * time.Second)
	e.expect(200, e.do("POST", "/api/v1/places/town/join", carol, nil))
	require.Empty(t, e.items(e.expect(200, e.do("GET", "/api/v1/places/town/bans", mod, nil))))
	e.expect(204, e.do("DELETE", "/api/v1/places/town/members/"+carolID+"?reason=bye", mod, nil))

	// Audit log.
	e.expect(403, e.do("GET", "/api/v1/places/town/audit-log", bob, nil))
	entries := e.items(e.expect(200, e.do("GET", "/api/v1/places/town/audit-log?action=member", mod, nil)))
	got := field[string](entries, "action")
	for _, want := range []string{"member.role_add", "member.timeout", "member.timeout_clear", "member.warn", "member.ban", "member.kick"} {
		require.Contains(t, got, want)
	}
	require.Equal(t, "member.kick", entries[0]["action"])
	require.Equal(t, "bye", entries[0]["reason"])
	require.Equal(t, "maria", entries[0]["actor"].(map[string]any)["username"])
	bobHistory := e.items(e.expect(200, e.do("GET", "/api/v1/places/town/audit-log?target_id="+bobID, mod, nil)))
	for _, entry := range bobHistory {
		require.Equal(t, bobID, entry["target_id"])
	}
	require.Len(t, bobHistory, 3)
	require.Len(t, e.items(e.expect(200, e.do("GET", "/api/v1/places/town/audit-log?action=report.resolve", mod, nil))), 1)
	e.expect(422, e.do("GET", "/api/v1/places/town/audit-log?action=%25", mod, nil))
}

func TestDrafts(t *testing.T) {
	e := newEnv(t)
	owner := e.setup()
	bob, _ := e.register("bob")

	saved := e.expect(200, e.do("PUT", "/api/v1/users/@me/drafts/reply:abc", owner, map[string]any{
		"data": map[string]any{"content": "half a thought"}})).obj(t)
	require.Equal(t, "half a thought", saved["data"].(map[string]any)["content"])
	e.expect(200, e.do("PUT", "/api/v1/users/@me/drafts/reply:abc", owner, map[string]any{"data": map[string]any{"content": "a whole thought"}}))
	drafts := e.expect(200, e.do("GET", "/api/v1/users/@me/drafts", owner, nil)).list(t)
	require.Len(t, drafts, 1)
	got := e.expect(200, e.do("GET", "/api/v1/users/@me/drafts/reply:abc", owner, nil)).obj(t)
	require.Equal(t, "a whole thought", got["data"].(map[string]any)["content"])
	e.expect(404, e.do("GET", "/api/v1/users/@me/drafts/reply:abc", bob, nil)) // drafts are private
	e.expect(422, e.do("PUT", "/api/v1/users/@me/drafts/"+url.PathEscape("bad key"), owner, map[string]any{"data": map[string]any{}}))
	e.expect(204, e.do("DELETE", "/api/v1/users/@me/drafts/reply:abc", owner, nil))
	e.expect(404, e.do("GET", "/api/v1/users/@me/drafts/reply:abc", owner, nil))
}
