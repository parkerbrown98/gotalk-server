package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"

	"github.com/parkerbrown98/gotalk-server/internal/config"
	"github.com/parkerbrown98/gotalk-server/internal/indexsig"
	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/service"
)

// The index protocol's JSON Schemas, copied from gotalk-platform's
// packages/index-protocol (see testdata/index-protocol/README.md).
const schemaDir = "testdata/index-protocol/schemas"

var (
	schemaOnce sync.Once
	schemas    *jsonschema.Compiler
	schemaErr  error
)

// validate checks a response body against one of the index protocol's schemas.
func validate(t *testing.T, schema string, raw []byte) {
	t.Helper()
	schemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.AssertFormat()
		entries, err := os.ReadDir(schemaDir)
		if err != nil {
			schemaErr = err
			return
		}
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(schemaDir, e.Name()))
			if err != nil {
				schemaErr = err
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
			if err != nil {
				schemaErr = err
				return
			}
			id, _ := doc.(map[string]any)["$id"].(string)
			if err := c.AddResource(id, doc); err != nil {
				schemaErr = err
				return
			}
		}
		schemas = c
	})
	require.NoError(t, schemaErr)
	sch, err := schemas.Compile("https://gotalk.sh/schemas/index/v1/" + schema + ".schema.json")
	require.NoError(t, err)
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	require.NoError(t, err)
	require.NoError(t, sch.Validate(doc), "%s: %s", schema, raw)
}

type indexEnv struct {
	*env
	owner string
}

// listing switches directory listing through the settings API.
func (e *indexEnv) listing(on bool) {
	e.t.Helper()
	e.expect(200, e.do("PATCH", "/api/v1/instance/config", e.owner, map[string]any{
		"directory": map[string]any{"listed": on, "contact_email": "ops@forum.example"},
	}))
}

// get reads an index endpoint anonymously and validates it against its schema.
func (e *indexEnv) get(path, schema string, status int) resp {
	e.t.Helper()
	r := e.expect(status, e.do("GET", "/api/v1/index"+path, "", nil))
	if status == 200 {
		validate(e.t, schema, r.Raw)
		require.Equal(e.t, "no-cache", r.Header.Get("Cache-Control"))
		require.NotEmpty(e.t, r.Header.Get("ETag"))
	} else {
		validate(e.t, "problem", r.Raw)
	}
	return r
}

type event struct{ Type, Place, Board, Topic, User string }

// feed reads the change feed after cursor ("" for the start) and returns the events and the
// next cursor.
func (e *indexEnv) feed(cursor string) ([]event, string) {
	e.t.Helper()
	path := "/changes?limit=500"
	if cursor != "" {
		path += "&cursor=" + cursor
	}
	m := e.get(path, "changes-page", 200).obj(e.t)
	var out []event
	for _, raw := range m["events"].([]any) {
		ev := raw.(map[string]any)
		s := func(k string) string { v, _ := ev[k].(string); return v }
		out = append(out, event{Type: s("type"), Place: s("place_id"), Board: s("board_id"), Topic: s("topic_id"), User: s("user_id")})
	}
	return out, m["next_cursor"].(string)
}

func (e *indexEnv) latest() string {
	e.t.Helper()
	m := e.get("/changes?cursor=latest", "changes-page", 200).obj(e.t)
	require.Empty(e.t, m["events"])
	return m["next_cursor"].(string)
}

func (e *indexEnv) places() []map[string]any {
	e.t.Helper()
	return e.items(e.get("/places", "places-page", 200))
}

func newIndexEnv(t *testing.T, opts ...func(*envOpts)) *indexEnv {
	e := &indexEnv{env: newEnv(t, opts...)}
	e.owner = e.setup()
	return e
}

func TestIndexOptInAndReadEndpoints(t *testing.T) {
	e := newIndexEnv(t)
	alice, aliceID := e.register("alice")
	bob, _ := e.register("bob")

	// Nothing is listed or indexable by default.
	wk := e.expect(200, e.do("GET", "/.well-known/gotalk-instance", "", nil))
	validate(t, "well-known", wk.Raw)
	require.NotContains(t, wk.obj(t), "directory")
	require.Empty(t, e.places())

	place := e.createPlace(e.owner, "rust", "public")
	require.Equal(t, false, place["indexable"])
	require.Equal(t, false, place["indexed"])
	require.NotEmpty(t, place["index_notice"])
	for _, tok := range []string{alice, bob} {
		e.expect(200, e.do("POST", "/api/v1/places/rust/join", tok, nil))
	}
	help := e.board(e.owner, "rust", map[string]any{"slug": "help", "name": "Help", "kind": "category"})
	beginners := e.board(e.owner, "rust", map[string]any{"slug": "beginners", "name": "Beginners", "parent_id": help["id"]})
	staff := e.board(e.owner, "rust", map[string]any{"slug": "staff", "name": "Staff"})
	nsfw := e.board(e.owner, "rust", map[string]any{"slug": "after-dark", "name": "After dark", "is_nsfw": true})
	optout := e.board(e.owner, "rust", map[string]any{"slug": "meta", "name": "Meta"})
	under := e.board(e.owner, "rust", map[string]any{"slug": "meta-sub", "name": "Meta sub", "parent_id": optout["id"]})
	everyone := e.defaultRoleID(e.owner, "rust")
	e.expect(200, e.do("PUT", "/api/v1/boards/"+staff["id"].(string)+"/overwrites/"+everyone, e.owner,
		map[string]any{"allow": 0, "deny": permissions.ViewBoards}))
	e.expect(422, e.do("PATCH", "/api/v1/boards/"+nsfw["id"].(string), e.owner, map[string]any{"indexable": true}))
	b := e.expect(200, e.do("PATCH", "/api/v1/boards/"+optout["id"].(string), e.owner, map[string]any{"indexable": false})).obj(t)
	require.Equal(t, false, b["indexable"])
	require.Contains(t, b["index_notice"], "excluded")

	topicID, _ := e.topic(alice, beginners, "Why does the borrow checker complain?", "I get error E0502.", "lifetimes")
	reply := e.reply(bob, topicID, "Because of the **mutable borrow**.", "")
	staffTopic, _ := e.topic(e.owner, staff, "Staff only", "secret")
	underTopic, _ := e.topic(e.owner, under, "Opted out", "not public")

	// Indexable places other than public, non-NSFW ones are never listed.
	e.createPlace(e.owner, "secret", "private")
	e.expect(200, e.do("PATCH", "/api/v1/places/secret", e.owner, map[string]any{"indexable": true}))
	e.createPlace(e.owner, "club", "invite_only")
	e.expect(200, e.do("PATCH", "/api/v1/places/club", e.owner, map[string]any{"indexable": true}))
	e.expect(201, e.do("POST", "/api/v1/places", e.owner, map[string]any{"slug": "spicy", "name": "Spicy", "is_nsfw": true}))
	e.expect(422, e.do("PATCH", "/api/v1/places/spicy", e.owner, map[string]any{"indexable": true}))
	e.expect(422, e.do("PATCH", "/api/v1/places/rust", e.owner, map[string]any{"indexable": true, "is_nsfw": true}))
	e.expect(403, e.do("PATCH", "/api/v1/places/rust", alice, map[string]any{"indexable": true}))

	// Opting the place in does nothing until the operator lists the instance.
	p := e.expect(200, e.do("PATCH", "/api/v1/places/rust", e.owner, map[string]any{"indexable": true})).obj(t)
	require.Equal(t, true, p["indexable"])
	require.Equal(t, false, p["indexed"])
	require.Contains(t, p["index_notice"], "not listed")
	require.Empty(t, e.places())
	e.get("/topics/"+topicID, "", 404)

	e.listing(true)
	wk = e.expect(200, e.do("GET", "/.well-known/gotalk-instance", "", nil))
	validate(t, "well-known", wk.Raw)
	dir := wk.obj(t)["directory"].(map[string]any)
	require.Equal(t, true, dir["listed"])
	require.EqualValues(t, 1, dir["protocol"])
	require.Equal(t, e.srv.URL+"/api/v1/index", dir["index_url"])
	require.Equal(t, "ops@forum.example", dir["contact_email"])
	require.Len(t, dir["keys"], 1)
	info := e.expect(200, e.do("GET", "/api/v1/instance", "", nil)).obj(t)
	require.Equal(t, map[string]any{"listed": true, "places_indexed": float64(1)}, info["directory"])
	p = e.expect(200, e.do("GET", "/api/v1/places/rust", alice, nil)).obj(t)
	require.Equal(t, true, p["indexed"])

	places := e.places()
	require.Len(t, places, 1)
	require.Equal(t, "rust", places[0]["slug"])
	var boardSlugs []string
	for _, b := range places[0]["boards"].([]any) {
		boardSlugs = append(boardSlugs, b.(map[string]any)["slug"].(string))
	}
	require.Equal(t, []string{"help", "beginners"}, boardSlugs)

	topics := e.items(e.get("/boards/"+beginners["id"].(string)+"/topics", "board-topics-page", 200))
	require.Len(t, topics, 1)
	require.Equal(t, topicID, topics[0]["id"])
	require.EqualValues(t, 1, topics[0]["reply_count"])
	require.Equal(t, []any{"lifetimes"}, topics[0]["tags"])
	require.Equal(t, map[string]any{"id": aliceID, "username": "alice", "display_name": "alice"}, topics[0]["author"])
	require.Empty(t, e.items(e.get("/boards/"+help["id"].(string)+"/topics", "board-topics-page", 200)))
	for _, hidden := range []map[string]any{staff, nsfw, optout, under} {
		e.get("/boards/"+hidden["id"].(string)+"/topics", "", 404)
	}
	e.get("/boards/not-a-uuid/topics", "", 404)
	for _, hidden := range []string{staffTopic, underTopic} {
		e.get("/topics/"+hidden, "", 404)
	}

	topic := e.get("/topics/"+topicID, "topic", 200).obj(t)
	posts := topic["posts"].([]any)
	require.Len(t, posts, 2)
	require.Equal(t, "I get error E0502.", posts[0].(map[string]any)["content"])
	require.Equal(t, reply["id"], posts[1].(map[string]any)["id"])
	require.EqualValues(t, 2, posts[1].(map[string]any)["number"])
	require.Equal(t, beginners["id"], topic["board_id"])

	// Paging: one post per page.
	page1 := e.get("/topics/"+topicID+"?limit=1", "topic", 200).obj(t)
	require.Len(t, page1["posts"], 1)
	page2 := e.get("/topics/"+topicID+"?limit=1&cursor="+page1["next_cursor"].(string), "topic", 200).obj(t)
	require.Len(t, page2["posts"], 1)
	require.Nil(t, page2["next_cursor"])
	require.Equal(t, "invalid_cursor", e.get("/topics/"+topicID+"?cursor=garbage", "", 400).obj(t)["code"])
	require.Equal(t, "invalid_parameter", e.get("/boards/"+beginners["id"].(string)+"/topics?updated_since=yesterday", "", 400).obj(t)["code"])

	// Anonymous parity: everything the index returns is readable by a signed-out visitor.
	for _, pl := range places {
		e.expect(200, e.do("GET", "/api/v1/places/"+pl["id"].(string), "", nil))
		for _, b := range pl["boards"].([]any) {
			id := b.(map[string]any)["id"].(string)
			e.expect(200, e.do("GET", "/api/v1/boards/"+id, "", nil))
			for _, tp := range e.items(e.get("/boards/"+id+"/topics", "board-topics-page", 200)) {
				e.expect(200, e.do("GET", "/api/v1/topics/"+tp["id"].(string), "", nil))
				public := map[string]bool{}
				for _, post := range e.items(e.expect(200, e.do("GET", "/api/v1/topics/"+tp["id"].(string)+"/posts", "", nil))) {
					public[post["id"].(string)] = true
				}
				for _, post := range e.get("/topics/"+tp["id"].(string), "topic", 200).obj(t)["posts"].([]any) {
					require.True(t, public[post.(map[string]any)["id"].(string)])
				}
			}
		}
	}

	// ETags: unchanged pages answer 304.
	first := e.do("GET", "/api/v1/index/places", "", nil)
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/v1/index/places", nil)
	req.Header.Set("If-None-Match", first.Header.Get("ETag"))
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	require.Equal(t, http.StatusNotModified, res.StatusCode)
	require.Empty(t, body)
	e.expect(200, e.do("PATCH", "/api/v1/places/rust", e.owner, map[string]any{"description": "Rust Q&A"}))
	res, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)

	// The feed only ever names objects that were indexable.
	evs, _ := e.feed("")
	for _, ev := range evs {
		require.NotContains(t, []string{staff["id"].(string), nsfw["id"].(string), optout["id"].(string), under["id"].(string)}, ev.Board)
		require.NotContains(t, []string{staffTopic, underTopic}, ev.Topic)
		if ev.Place != "" {
			require.Equal(t, place["id"], ev.Place)
		}
	}
	// Content that existed before listing is covered by place.upsert (the reader re-reads
	// the place); changes after it name the topic.
	require.Equal(t, []event{{Type: "place.upsert", Place: place["id"].(string)}}, evs)
	e.reply(alice, topicID, "Thanks!", "")
	e.reply(e.owner, staffTopic, "staff reply", "")
	evs, _ = e.feed("")
	require.Equal(t, []event{
		{Type: "place.upsert", Place: place["id"].(string)},
		{Type: "topic.upsert", Place: place["id"].(string), Board: beginners["id"].(string), Topic: topicID},
	}, evs)

	// Settings changes are audited.
	audit := e.items(e.expect(200, e.do("GET", "/api/v1/places/rust/audit-log", e.owner, nil)))
	var sawIndexable bool
	for _, a := range audit {
		if a["action"] == "place.update" && a["metadata"].(map[string]any)["indexable"] == true {
			sawIndexable = true
		}
	}
	require.True(t, sawIndexable)
	inst := e.expect(200, e.do("GET", "/api/v1/instance/audit-log", e.owner, nil)).list(t)
	require.Equal(t, "directory.update", inst[0].(map[string]any)["action"])
	e.expect(403, e.do("GET", "/api/v1/instance/audit-log", alice, nil))
}

func TestIndexChangeFeedEvents(t *testing.T) {
	e := newIndexEnv(t)
	alice, aliceID := e.register("alice")
	bob, bobID := e.register("bob")
	place := e.createPlace(e.owner, "rust", "public")
	placeID := place["id"].(string)
	for _, tok := range []string{alice, bob} {
		e.expect(200, e.do("POST", "/api/v1/places/rust/join", tok, nil))
	}
	general := e.board(e.owner, "rust", map[string]any{"slug": "general", "name": "General"})
	gen := general["id"].(string)
	hidden := e.board(e.owner, "rust", map[string]any{"slug": "hidden", "name": "Hidden"})
	everyone := e.defaultRoleID(e.owner, "rust")
	overwrite := func(board string, deny permissions.Permission) {
		e.expect(200, e.do("PUT", "/api/v1/boards/"+board+"/overwrites/"+everyone, e.owner, map[string]any{"allow": 0, "deny": deny}))
	}
	overwrite(hidden["id"].(string), permissions.ViewBoards)
	e.listing(true)
	cur := e.latest()

	// Opting a place in announces it.
	e.expect(200, e.do("PATCH", "/api/v1/places/rust", e.owner, map[string]any{"indexable": true}))
	evs, cur := e.feed(cur)
	require.Equal(t, []event{{Type: "place.upsert", Place: placeID}}, evs)

	// Topics, replies, edits and solutions are topic.upsert; repeated events coalesce.
	t1, _ := e.topic(alice, general, "First", "Opening post by alice")
	r1 := e.reply(bob, t1, "A reply by bob", "")
	e.expect(200, e.do("PATCH", "/api/v1/posts/"+r1["id"].(string), bob, map[string]any{"content": "Edited reply"}))
	t2, _ := e.topic(bob, general, "Second", "Opening post by bob")
	evs, cur = e.feed(cur)
	require.Equal(t, []event{
		{Type: "topic.upsert", Place: placeID, Board: gen, Topic: t1},
		{Type: "topic.upsert", Place: placeID, Board: gen, Topic: t2},
	}, evs)

	// Deleting a reply re-announces the topic without the post.
	e.expect(204, e.do("DELETE", "/api/v1/posts/"+r1["id"].(string), bob, nil))
	evs, cur = e.feed(cur)
	require.Equal(t, []event{{Type: "topic.upsert", Place: placeID, Board: gen, Topic: t1}}, evs)
	require.Len(t, e.get("/topics/"+t1, "topic", 200).obj(t)["posts"], 1)

	// A person excluding their posts: user.exclude, and their topics leave the index.
	e.reply(alice, t2, "alice replies to bob", "")
	_, cur = e.feed(cur)
	me := e.expect(200, e.do("PATCH", "/api/v1/users/@me", alice, map[string]any{"exclude_from_index": true})).obj(t)
	require.Equal(t, true, me["exclude_from_index"])
	require.Contains(t, me["index_notice"], "excluded")
	evs, cur = e.feed(cur)
	require.Equal(t, []event{
		{Type: "user.exclude", User: aliceID},
		{Type: "topic.delete", Place: placeID, Board: gen, Topic: t1},
	}, evs)
	e.get("/topics/"+t1, "", 404)
	topic2 := e.get("/topics/"+t2, "topic", 200).obj(t)
	require.Len(t, topic2["posts"], 1)
	require.EqualValues(t, 0, topic2["reply_count"])
	listed := e.items(e.get("/boards/"+gen+"/topics", "board-topics-page", 200))
	require.Equal(t, []string{t2}, field[string](listed, "id"))

	// Including them again: user.include plus topic.upsert for each affected topic.
	e.expect(200, e.do("PATCH", "/api/v1/users/@me", alice, map[string]any{"exclude_from_index": false}))
	evs, cur = e.feed(cur)
	require.Equal(t, "user.include", evs[0].Type)
	require.ElementsMatch(t, []event{
		{Type: "topic.upsert", Place: placeID, Board: gen, Topic: t1},
		{Type: "topic.upsert", Place: placeID, Board: gen, Topic: t2},
	}, evs[1:])
	inst := e.expect(200, e.do("GET", "/api/v1/instance/audit-log", e.owner, nil)).list(t)
	require.Equal(t, "user.index_exclusion", inst[0].(map[string]any)["action"])

	// Moving a topic to a board that is not indexable removes it from the old board.
	e.expect(200, e.do("PATCH", "/api/v1/topics/"+t2, e.owner, map[string]any{"board_id": hidden["id"]}))
	evs, cur = e.feed(cur)
	require.Equal(t, []event{{Type: "topic.delete", Place: placeID, Board: gen, Topic: t2}}, evs)
	e.get("/topics/"+t2, "", 404)
	e.expect(200, e.do("PATCH", "/api/v1/topics/"+t2, e.owner, map[string]any{"board_id": gen}))
	evs, cur = e.feed(cur)
	require.Equal(t, []event{{Type: "topic.upsert", Place: placeID, Board: gen, Topic: t2}}, evs)

	// Deleting a topic.
	e.expect(204, e.do("DELETE", "/api/v1/topics/"+t2, e.owner, nil))
	evs, cur = e.feed(cur)
	require.Equal(t, []event{{Type: "topic.delete", Place: placeID, Board: gen, Topic: t2}}, evs)

	// A board renamed is board.upsert; opted out is board.delete; NSFW is board.delete.
	e.expect(200, e.do("PATCH", "/api/v1/boards/"+gen, e.owner, map[string]any{"name": "General chat"}))
	evs, cur = e.feed(cur)
	require.Equal(t, []event{{Type: "board.upsert", Place: placeID, Board: gen}}, evs)
	e.expect(200, e.do("PATCH", "/api/v1/boards/"+gen, e.owner, map[string]any{"indexable": false}))
	evs, cur = e.feed(cur)
	require.Equal(t, []event{{Type: "board.delete", Place: placeID, Board: gen}}, evs)
	e.get("/boards/"+gen+"/topics", "", 404)
	e.get("/topics/"+t1, "", 404)
	e.expect(200, e.do("PATCH", "/api/v1/boards/"+gen, e.owner, map[string]any{"indexable": true}))
	_, cur = e.feed(cur)
	e.expect(200, e.do("PATCH", "/api/v1/boards/"+gen, e.owner, map[string]any{"is_nsfw": true}))
	evs, cur = e.feed(cur)
	require.Equal(t, []event{{Type: "board.delete", Place: placeID, Board: gen}}, evs)
	e.expect(200, e.do("PATCH", "/api/v1/boards/"+gen, e.owner, map[string]any{"is_nsfw": false}))
	_, cur = e.feed(cur)

	// Permission changes that hide a board from @everyone fan out per board.
	overwrite(gen, permissions.ViewBoards)
	evs, cur = e.feed(cur)
	require.Equal(t, []event{{Type: "board.delete", Place: placeID, Board: gen}}, evs)
	e.expect(204, e.do("DELETE", "/api/v1/boards/"+gen+"/overwrites/"+everyone, e.owner, nil))
	evs, cur = e.feed(cur)
	require.Equal(t, []event{{Type: "board.upsert", Place: placeID, Board: gen}}, evs)
	e.expect(200, e.do("PATCH", "/api/v1/places/rust/roles/"+everyone, e.owner,
		map[string]any{"permissions": permissions.Default &^ permissions.ViewBoards}))
	evs, cur = e.feed(cur)
	require.Equal(t, []event{{Type: "board.delete", Place: placeID, Board: gen}}, evs)
	e.expect(200, e.do("PATCH", "/api/v1/places/rust/roles/"+everyone, e.owner, map[string]any{"permissions": permissions.Default}))
	evs, cur = e.feed(cur)
	require.Equal(t, []event{{Type: "board.upsert", Place: placeID, Board: gen}}, evs)

	// A place made private, then public; opted out; NSFW.
	for _, change := range []map[string]any{{"visibility": "private"}, {"visibility": "invite_only"}, {"indexable": false}, {"is_nsfw": true}} {
		e.expect(200, e.do("PATCH", "/api/v1/places/rust", e.owner, change))
		evs, cur = e.feed(cur)
		require.Equal(t, []event{{Type: "place.delete", Place: placeID}}, evs, "%v", change)
		require.Empty(t, e.places())
		e.get("/topics/"+t1, "", 404)
		e.expect(200, e.do("PATCH", "/api/v1/places/rust", e.owner, map[string]any{"visibility": "public", "indexable": true, "is_nsfw": false}))
		evs, cur = e.feed(cur)
		require.Equal(t, []event{{Type: "place.upsert", Place: placeID}}, evs)
	}

	// Turning listing off removes everything; on again re-announces indexable places.
	e.listing(false)
	evs, cur = e.feed(cur)
	require.Equal(t, []event{{Type: "place.delete", Place: placeID}}, evs)
	require.NotContains(t, e.expect(200, e.do("GET", "/.well-known/gotalk-instance", "", nil)).obj(t), "directory")
	require.Empty(t, e.places())
	e.get("/topics/"+t1, "", 404)
	e.listing(true)
	evs, cur = e.feed(cur)
	require.Equal(t, []event{{Type: "place.upsert", Place: placeID}}, evs)

	// Deleting an account excludes its posts.
	e.reply(bob, t1, "bob is still here", "")
	_, cur = e.feed(cur)
	e.expect(204, e.do("DELETE", "/api/v1/users/@me", bob, map[string]any{"password": "bob-password-1"}))
	evs, cur = e.feed(cur)
	require.Equal(t, []event{{Type: "user.exclude", User: bobID}}, evs)
	require.Len(t, e.get("/topics/"+t1, "topic", 200).obj(t)["posts"], 1)

	// Deleting the place.
	e.expect(204, e.do("DELETE", "/api/v1/places/rust", e.owner, nil))
	evs, _ = e.feed(cur)
	require.Equal(t, []event{{Type: "place.delete", Place: placeID}}, evs)
	require.Empty(t, e.places())
}

func TestIndexFeedPagingRetentionAndCursorExpiry(t *testing.T) {
	e := newIndexEnv(t)
	e.createPlace(e.owner, "rust", "public")
	board := e.board(e.owner, "rust", map[string]any{"slug": "general", "name": "General"})
	e.expect(200, e.do("PATCH", "/api/v1/places/rust", e.owner, map[string]any{"indexable": true}))
	e.listing(true)
	start := e.latest()
	var ids []string
	for i := range 5 {
		id, _ := e.topic(e.owner, board, "Topic "+string(rune('A'+i)), "content")
		ids = append(ids, id)
	}

	// Pages of two, in order, with has_more until the end.
	var got []string
	cur := start
	for {
		m := e.get("/changes?limit=2&cursor="+cur, "changes-page", 200).obj(t)
		for _, ev := range m["events"].([]any) {
			got = append(got, ev.(map[string]any)["topic_id"].(string))
		}
		cur = m["next_cursor"].(string)
		if m["has_more"] != true {
			break
		}
	}
	require.Equal(t, ids, got)
	evs, _ := e.feed(cur)
	require.Empty(t, evs)

	// Reading from the start returns every retained event, in seq order.
	m := e.get("/changes", "changes-page", 200).obj(t)
	var last float64
	for _, ev := range m["events"].([]any) {
		seq := ev.(map[string]any)["seq"].(float64)
		require.Greater(t, seq, last)
		last = seq
	}

	// Events older than the retention window are pruned, and older cursors expire.
	prev := service.IndexRetention
	service.IndexRetention = -time.Hour
	t.Cleanup(func() { service.IndexRetention = prev })
	require.NoError(t, e.svc.Maintain(context.Background()))
	r := e.get("/changes?cursor="+start, "", 410)
	problem := r.obj(t)
	require.Equal(t, "cursor_expired", problem["code"])
	require.True(t, strings.HasSuffix(problem["type"].(string), "/cursor_expired"))
	require.Equal(t, "application/problem+json", r.Header.Get("Content-Type"))
	evs, _ = e.feed(cur)
	require.Empty(t, evs)
	fresh := e.latest()
	service.IndexRetention = prev
	id, _ := e.topic(e.owner, board, "After pruning", "content")
	evs, _ = e.feed(fresh)
	require.Equal(t, []string{id}, func() []string {
		out := []string{}
		for _, ev := range evs {
			out = append(out, ev.Topic)
		}
		return out
	}())

	e.get("/changes?cursor=nonsense", "", 400)
	future := base64.RawURLEncoding.EncodeToString([]byte("1:s999999"))
	e.get("/changes?cursor="+future, "", 410)
}

func TestIndexRateLimitTier(t *testing.T) {
	e := newIndexEnv(t, func(o *envOpts) {
		o.mutate = func(c *config.Config) { c.RateLimit.Index = "3-M" }
	})
	for range 3 {
		r := e.expect(200, e.do("GET", "/api/v1/index/places", "", nil))
		require.Equal(t, "index", r.Header.Get("X-RateLimit-Tier"))
	}
	r := e.expect(429, e.do("GET", "/api/v1/index/changes", "", nil))
	validate(t, "problem", r.Raw)
	require.Equal(t, "rate_limited", r.obj(t)["code"])
	require.NotEmpty(t, r.Header.Get("Retry-After"))
	// Other tiers are unaffected, even for the same IP.
	e.expect(200, e.do("GET", "/api/v1/instance", "", nil))
}

func TestDirectorySettingsKeysAndNotices(t *testing.T) {
	e := newIndexEnv(t)
	alice, _ := e.register("alice")

	cfg := e.expect(200, e.do("GET", "/api/v1/instance/config", e.owner, nil)).obj(t)
	dir := cfg["directory"].(map[string]any)
	require.Equal(t, "default", dir["source"])
	require.Equal(t, "directory.listed", dir["config_key"])
	require.Equal(t, false, dir["settings"].(map[string]any)["listed"])

	st := e.expect(200, e.do("GET", "/api/v1/instance/directory", e.owner, nil)).obj(t)
	require.Equal(t, false, st["listed"])
	require.Equal(t, true, st["license_pending"])
	require.Contains(t, st["notice"], "Pending legal review")
	require.Contains(t, st["policy_text"].(map[string]any)["terms"], "Pending legal review")
	require.Equal(t, "none", st["key_source"])
	e.expect(403, e.do("GET", "/api/v1/instance/directory", alice, nil))
	e.expect(403, e.do("PATCH", "/api/v1/instance/config", alice, map[string]any{"directory": map[string]any{"listed": true}}))
	e.expect(422, e.do("PATCH", "/api/v1/instance/config", e.owner, map[string]any{"directory": map[string]any{"listed": true, "signing_key": "short"}}))

	saved := e.expect(200, e.do("PATCH", "/api/v1/instance/config", e.owner, map[string]any{"directory": map[string]any{"listed": true}})).obj(t)
	checks := saved["checks"].([]any)
	require.Equal(t, "directory", checks[0].(map[string]any)["name"])
	require.Equal(t, "warning", checks[0].(map[string]any)["status"], "no public_url in tests")

	st = e.expect(200, e.do("GET", "/api/v1/instance/directory", e.owner, nil)).obj(t)
	require.Equal(t, true, st["listed"])
	require.Equal(t, "generated", st["key_source"])
	require.Len(t, st["public_keys"], 1)
	require.Contains(t, st["platform_status_url"], "https://gotalk.sh/api/index/v1/instances?origin=")
	first := st["public_keys"].([]any)[0].(map[string]any)["public_key"]

	// Rotation publishes both keys until the old one is revoked.
	st = e.expect(200, e.do("POST", "/api/v1/instance/directory/keys/rotate", e.owner, nil)).obj(t)
	require.Len(t, st["public_keys"], 2)
	require.Equal(t, first, st["previous_key"])
	keys := e.expect(200, e.do("GET", "/.well-known/gotalk-instance", "", nil)).obj(t)["directory"].(map[string]any)["keys"].([]any)
	require.Len(t, keys, 2)
	require.Equal(t, first, keys[1].(map[string]any)["public_key"])
	st = e.expect(200, e.do("DELETE", "/api/v1/instance/directory/keys/previous", e.owner, nil)).obj(t)
	require.Len(t, st["public_keys"], 1)
	require.NotEqual(t, first, st["public_keys"].([]any)[0].(map[string]any)["public_key"])
	e.expect(404, e.do("DELETE", "/api/v1/instance/directory/keys/previous", e.owner, nil))

	// A configured key is used as-is and cannot be rotated here.
	seed := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	e.expect(200, e.do("PATCH", "/api/v1/instance/config", e.owner, map[string]any{"directory": map[string]any{"listed": true, "signing_key": seed}}))
	st = e.expect(200, e.do("GET", "/api/v1/instance/directory", e.owner, nil)).obj(t)
	require.Equal(t, "configured", st["key_source"])
	pub := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32)).Public().(ed25519.PublicKey)
	require.Equal(t, base64.RawURLEncoding.EncodeToString(pub), st["public_keys"].([]any)[0].(map[string]any)["public_key"])
	e.expect(409, e.do("POST", "/api/v1/instance/directory/keys/rotate", e.owner, nil))
	cfg = e.expect(200, e.do("GET", "/api/v1/instance/config", e.owner, nil)).obj(t)
	require.Equal(t, []any{"signing_key"}, cfg["directory"].(map[string]any)["secrets_set"])
	require.NotContains(t, cfg["directory"].(map[string]any)["settings"], "signing_key")

	// Resetting the section turns listing back off.
	e.expect(200, e.do("DELETE", "/api/v1/instance/config/directory", e.owner, nil))
	require.NotContains(t, e.expect(200, e.do("GET", "/.well-known/gotalk-instance", "", nil)).obj(t), "directory")

	// Account notice.
	me := e.expect(200, e.do("GET", "/api/v1/users/@me", alice, nil)).obj(t)
	require.Equal(t, false, me["exclude_from_index"])
	require.Contains(t, me["index_notice"], "within an hour")
}

func TestDirectoryListingFromConfig(t *testing.T) {
	e := newIndexEnv(t, func(o *envOpts) {
		o.mutate = func(c *config.Config) {
			c.Directory.Listed = true
			c.MarkSet("directory.listed")
		}
	})
	dir := e.expect(200, e.do("GET", "/api/v1/instance/config", e.owner, nil)).obj(t)["directory"].(map[string]any)
	require.Equal(t, "config", dir["source"])
	require.Equal(t, false, dir["editable"])
	e.expect(409, e.do("PATCH", "/api/v1/instance/config", e.owner, map[string]any{"directory": map[string]any{"listed": false}}))
	require.Contains(t, e.expect(200, e.do("GET", "/.well-known/gotalk-instance", "", nil)).obj(t), "directory")
}

// fakeIndex stands in for gotalk.sh: it checks ping signatures against the instance's
// well-known keys and records each accepted ping.
type fakeIndex struct {
	t        *testing.T
	mu       sync.Mutex
	pings    []time.Time
	wellKnow func() map[string]any
}

func (f *fakeIndex) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.Method != http.MethodPost || r.URL.Path != service.IndexPingPath {
		http.NotFound(w, r)
		return
	}
	var in struct{ Origin string }
	if err := json.Unmarshal(body, &in); err != nil || in.Origin != "https://forum.example" {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	keyID := r.Header.Get(indexsig.HeaderKeyID)
	var published bool
	for _, k := range f.wellKnow()["directory"].(map[string]any)["keys"].([]any) {
		published = published || k.(map[string]any)["public_key"] == keyID
	}
	pub, err := base64.RawURLEncoding.DecodeString(keyID)
	if !published || err != nil {
		http.Error(w, "unknown key", http.StatusUnauthorized)
		return
	}
	stamp, err := strconv.ParseInt(r.Header.Get(indexsig.HeaderTimestamp), 10, 64)
	if err != nil || time.Since(time.Unix(stamp, 0)).Abs() > 300*time.Second {
		http.Error(w, "stale timestamp", http.StatusUnauthorized)
		return
	}
	req := indexsig.Request{
		Method: r.Method, Host: r.Host, Target: r.URL.RequestURI(), Timestamp: stamp,
		Nonce: r.Header.Get(indexsig.HeaderNonce), KeyID: keyID, Body: body,
	}
	if r.Header.Get(indexsig.HeaderContentDigest) != indexsig.ContentDigest(body) ||
		!indexsig.Verify(pub, req, r.Header.Get(indexsig.HeaderSignature)) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	f.pings = append(f.pings, time.Now())
	f.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

func (f *fakeIndex) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pings)
}

func TestIndexPingsAreSignedAndDebounced(t *testing.T) {
	prevInterval, prevSettle := service.IndexPingInterval, service.IndexPingSettle
	service.IndexPingInterval, service.IndexPingSettle = 1500*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { service.IndexPingInterval, service.IndexPingSettle = prevInterval, prevSettle })

	idx := &fakeIndex{t: t}
	platform := httptest.NewServer(idx)
	t.Cleanup(platform.Close)
	e := newIndexEnv(t, func(o *envOpts) {
		o.mutate = func(c *config.Config) {
			c.Server.PublicURL = "https://forum.example"
			c.Directory.PlatformURL = platform.URL
		}
	})
	idx.wellKnow = func() map[string]any {
		return e.expect(200, e.do("GET", "/.well-known/gotalk-instance", "", nil)).obj(t)
	}
	e.createPlace(e.owner, "rust", "public")
	board := e.board(e.owner, "rust", map[string]any{"slug": "general", "name": "General"})

	// No pings while the instance is not listed.
	e.expect(200, e.do("PATCH", "/api/v1/places/rust", e.owner, map[string]any{"indexable": true}))
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, 0, idx.count())

	// The settings check may fail on the made-up public URL, so force the save.
	e.expect(200, e.do("PATCH", "/api/v1/instance/config?force=true", e.owner, map[string]any{"directory": map[string]any{"listed": true}}))
	require.Eventually(t, func() bool { return idx.count() == 1 }, 5*time.Second, 20*time.Millisecond)

	// A burst of changes within the interval produces one more ping, not one per change.
	for i := range 4 {
		e.topic(e.owner, board, "Topic "+string(rune('A'+i)), "content")
	}
	require.Eventually(t, func() bool { return idx.count() == 2 }, 5*time.Second, 20*time.Millisecond)
	time.Sleep(service.IndexPingInterval + 300*time.Millisecond)
	require.Equal(t, 2, idx.count())
	idx.mu.Lock()
	gap := idx.pings[1].Sub(idx.pings[0])
	idx.mu.Unlock()
	require.GreaterOrEqual(t, gap, service.IndexPingInterval-100*time.Millisecond)

	st := e.expect(200, e.do("GET", "/api/v1/instance/directory", e.owner, nil)).obj(t)
	require.NotNil(t, st["last_ping_at"])

	// Pings stop when they are disabled.
	e.expect(200, e.do("PATCH", "/api/v1/instance/config?force=true", e.owner, map[string]any{
		"directory": map[string]any{"listed": true, "pings_disabled": true},
	}))
	time.Sleep(service.IndexPingInterval)
	e.topic(e.owner, board, "Quiet", "content")
	time.Sleep(service.IndexPingInterval + 300*time.Millisecond)
	require.Equal(t, 2, idx.count())
}

// TestIndexProtocolFixtures checks that this validator agrees with the platform's on every
// conformance fixture.
func TestIndexProtocolFixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/index-protocol/fixtures/manifest.json")
	require.NoError(t, err)
	var manifest struct {
		Fixtures []struct{ File, Schema string }
	}
	require.NoError(t, json.Unmarshal(raw, &manifest))
	require.NotEmpty(t, manifest.Fixtures)
	for _, f := range manifest.Fixtures {
		if f.Schema == "" {
			continue
		}
		doc, err := os.ReadFile(filepath.Join("testdata/index-protocol/fixtures", f.File))
		require.NoError(t, err)
		validate(t, f.Schema, doc)
	}
}

func TestIndexOpenAPI(t *testing.T) {
	e := newEnv(t)
	e.setup()
	doc := e.expect(200, e.do("GET", "/api/v1/openapi.json", "", nil)).obj(t)
	paths := doc["paths"].(map[string]any)
	for _, p := range []string{"/index/places", "/index/boards/{boardID}/topics", "/index/topics/{topicID}", "/index/changes"} {
		require.Contains(t, paths, p)
	}
}
