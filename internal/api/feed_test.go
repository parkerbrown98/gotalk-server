package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

// age moves a topic (and its latest activity) back in time and re-ranks it.
func (e *env) age(topicID string, d time.Duration) {
	e.t.Helper()
	ctx := context.Background()
	_, err := e.svc.Pool().Exec(ctx, `UPDATE topics
		SET created_at = created_at - make_interval(secs => $2), last_post_at = last_post_at - make_interval(secs => $2)
		WHERE id = $1`, topicID, d.Seconds())
	require.NoError(e.t, err)
	require.NoError(e.t, store.New(e.svc.Pool()).RefreshTopicRanks(ctx, store.RefreshTopicRanksParams{
		TopicIds: []uuid.UUID{uuid.MustParse(topicID)},
	}))
}

func (e *env) feed(token, path string) map[string]any {
	e.t.Helper()
	return e.expect(200, e.do("GET", path, token, nil)).obj(e.t)
}

func feedItems(page map[string]any) []map[string]any { return toMaps(page["items"]) }

func titles(items []map[string]any) []string { return field[string](items, "title") }

func viewer(item map[string]any) map[string]any {
	v, _ := item["viewer"].(map[string]any)
	return v
}

func itemByTitle(t *testing.T, items []map[string]any, title string) map[string]any {
	t.Helper()
	for _, it := range items {
		if it["title"] == title {
			return it
		}
	}
	t.Fatalf("no item titled %q in %v", title, titles(items))
	return nil
}

func TestTopicFeeds(t *testing.T) {
	e := newEnv(t)
	owner := e.setup()
	bob, _ := e.register("bob")
	carol, _ := e.register("carol")
	dave, _ := e.register("dave")
	e.createPlace(owner, "news", "public")
	e.expect(200, e.do("POST", "/api/v1/places/news/join", bob, nil))
	e.expect(200, e.do("POST", "/api/v1/places/news/join", carol, nil))

	general := e.board(owner, "news", map[string]any{"slug": "general", "name": "General"})
	staff := e.board(owner, "news", map[string]any{"slug": "staff", "name": "Staff"})
	spicy := e.board(owner, "news", map[string]any{"slug": "spicy", "name": "Spicy", "is_nsfw": true})
	def := e.defaultRoleID(owner, "news")
	e.expect(200, e.do("PUT", "/api/v1/boards/"+id(staff)+"/overwrites/"+def, owner, map[string]any{"allow": 0, "deny": permissions.ViewBoards}))

	a, aPost := e.topic(bob, general, "Alpha", "**Alpha** body with a [link](https://example.com)")
	b, _ := e.topic(carol, general, "Bravo", "Bravo body")
	c, _ := e.topic(owner, general, "Charlie", "Charlie body")
	d, _ := e.topic(owner, staff, "Delta", "Staff only")
	ex, _ := e.topic(owner, spicy, "Echo", "Spicy")
	f, _ := e.topic(owner, general, "Foxtrot", "Old news")
	e.age(a, 2*time.Hour)
	e.age(b, 30*time.Minute)
	e.age(c, 5*time.Hour)
	e.age(d, time.Hour)
	e.age(ex, time.Hour)
	e.age(f, 72*time.Hour)

	vote := func(token, topic string, value int, status int) map[string]any {
		t.Helper()
		return e.expect(status, e.do("PUT", "/api/v1/topics/"+topic+"/vote", token, map[string]any{"value": value})).obj(t)
	}
	vote(owner, a, 1, 200)
	got := vote(carol, a, 1, 200)
	require.EqualValues(t, 2, got["score"])
	require.EqualValues(t, 2, got["upvotes"])
	require.EqualValues(t, 1, viewer(got)["vote"])
	vote(bob, c, 1, 200)
	vote(carol, c, -1, 200)

	// Every sort, as a member: no hidden board, no NSFW board by default.
	base := "/api/v1/places/news/feed?sort="
	for sort, want := range map[string][]string{
		"new":                 {"Bravo", "Alpha", "Charlie", "Foxtrot"},
		"active":              {"Bravo", "Alpha", "Charlie", "Foxtrot"},
		"hot":                 {"Alpha", "Bravo", "Charlie", "Foxtrot"},
		"top&t=all":           {"Alpha", "Foxtrot", "Charlie", "Bravo"},
		"top&t=day":           {"Alpha", "Charlie", "Bravo"},
		"top&t=hour":          {"Bravo"},
		"controversial&t=all": {"Charlie"},
		"rising":              {"Alpha", "Bravo", "Charlie"},
	} {
		require.Equal(t, want, titles(feedItems(e.feed(bob, base+sort))), "sort=%s", sort)
	}
	require.Equal(t, "week", e.feed(bob, base+"top")["t"], "top defaults to a week")
	e.expect(422, e.do("GET", base+"best", bob, nil))
	e.expect(422, e.do("GET", base+"top&t=decade", bob, nil))
	require.Equal(t, []string{"Bravo", "Echo", "Delta", "Alpha", "Charlie", "Foxtrot"},
		titles(feedItems(e.feed(owner, base+"new&nsfw=true"))), "the owner sees every board")
	require.Equal(t, []string{"Echo"}, titles(feedItems(e.feed(owner, base+"new&nsfw=true&board="+id(spicy)))))
	e.expect(404, e.do("GET", base+"new&board="+id(staff), bob, nil))

	// Feed items carry what a card needs.
	page := e.feed(bob, base+"new")
	require.NotEmpty(t, page["as_of"])
	alpha := itemByTitle(t, feedItems(page), "Alpha")
	require.Equal(t, "Alpha body with a link", alpha["excerpt"])
	require.Equal(t, "general", alpha["board"].(map[string]any)["slug"])
	require.Equal(t, "news", alpha["place"].(map[string]any)["slug"])
	require.Equal(t, true, alpha["place"].(map[string]any)["voting_enabled"])
	require.Equal(t, false, alpha["is_nsfw"])
	require.Equal(t, "bob", alpha["author"].(map[string]any)["username"])
	anon := feedItems(e.feed("", base+"new&nsfw=true"))
	require.Equal(t, []string{"Bravo", "Echo", "Alpha", "Charlie", "Foxtrot"}, titles(anon), "guests see public boards only")
	require.Nil(t, viewer(anon[0]), "no caller state for anonymous readers")
	require.Equal(t, true, itemByTitle(t, anon, "Echo")["is_nsfw"])

	// Cursor paging: pages do not overlap, the last page has no cursor, and cursors are tied
	// to their sort.
	p1 := e.feed(bob, base+"new&limit=2")
	require.Equal(t, []string{"Bravo", "Alpha"}, titles(feedItems(p1)))
	cursor := p1["next_cursor"].(string)
	p2 := e.feed(bob, base+"new&limit=2&cursor="+cursor)
	require.Equal(t, []string{"Charlie", "Foxtrot"}, titles(feedItems(p2)))
	require.NotContains(t, p2, "next_cursor")
	require.Equal(t, p1["as_of"], p2["as_of"], "later pages keep the first page's time")
	e.expect(422, e.do("GET", base+"hot&limit=2&cursor="+cursor, bob, nil))
	e.expect(422, e.do("GET", base+"new&cursor=garbage", bob, nil))
	for _, sort := range []string{"hot", "top&t=all", "rising", "active"} {
		var all []string
		next := ""
		for range 10 {
			pg := e.feed(bob, base+sort+"&limit=1"+next)
			all = append(all, titles(feedItems(pg))...)
			c, more := pg["next_cursor"].(string)
			if !more {
				break
			}
			next = "&cursor=" + c
		}
		require.Equal(t, titles(feedItems(e.feed(bob, base+sort))), all, "paging one by one through sort=%s", sort)
	}

	// Pinned topics can be lifted out of the ranking.
	e.expect(200, e.do("PATCH", "/api/v1/topics/"+c, owner, map[string]any{"is_pinned": true}))
	pinned := e.feed(bob, base+"new&pinned=first&limit=2")
	require.Equal(t, []string{"Charlie"}, titles(toMaps(pinned["pinned"])))
	require.Equal(t, []string{"Bravo", "Alpha"}, titles(feedItems(pinned)))
	rest := e.feed(bob, base+"new&pinned=first&limit=2&cursor="+pinned["next_cursor"].(string))
	require.NotContains(t, rest, "pinned")
	require.Equal(t, []string{"Foxtrot"}, titles(feedItems(rest)))
	require.Contains(t, titles(feedItems(e.feed(bob, base+"new"))), "Charlie", "pinned topics stay inline by default")

	// Voting rules.
	vote(bob, a, 1, 403)   // own topic
	vote(dave, a, 1, 403)  // not a member
	vote(carol, a, 2, 422) // not a vote
	e.expect(401, e.do("PUT", "/api/v1/topics/"+a+"/vote", "", map[string]any{"value": 1}))
	require.EqualValues(t, 0, vote(carol, a, -1, 200)["score"], "changing a vote replaces it")
	got = e.expect(200, e.do("DELETE", "/api/v1/topics/"+a+"/vote", carol, nil)).obj(t)
	require.EqualValues(t, 1, got["score"])
	require.EqualValues(t, 0, viewer(got)["vote"])
	e.expect(200, e.do("DELETE", "/api/v1/topics/"+a+"/vote", carol, nil))
	require.EqualValues(t, 2, vote(carol, a, 1, 200)["score"])
	require.EqualValues(t, 1, viewer(e.expect(200, e.do("GET", "/api/v1/topics/"+a, carol, nil)).obj(t))["vote"])

	// With voting off, scores fall back to reactions on the opening post.
	placeRes := e.expect(200, e.do("PATCH", "/api/v1/places/news", owner, map[string]any{"voting_enabled": false})).obj(t)
	require.Equal(t, false, placeRes["voting_enabled"])
	vote(carol, b, 1, 409)
	e.expect(422, e.do("GET", base+"controversial", bob, nil))
	e.expect(204, e.do("PUT", "/api/v1/posts/"+aPost+"/reactions/heart", carol, nil))
	require.EqualValues(t, 1, itemByTitle(t, feedItems(e.feed(bob, base+"new")), "Alpha")["score"])
	e.expect(200, e.do("PATCH", "/api/v1/places/news", owner, map[string]any{"voting_enabled": true}))
	require.EqualValues(t, 2, itemByTitle(t, feedItems(e.feed(bob, base+"new")), "Alpha")["score"])

	// Read state: authors have read their topics; opening one marks it read.
	items := feedItems(e.feed(bob, base+"new"))
	require.Equal(t, true, viewer(itemByTitle(t, items, "Alpha"))["read"])
	require.Equal(t, false, viewer(itemByTitle(t, items, "Bravo"))["read"])
	bc := e.connect(bob)
	e.expect(204, e.do("PUT", "/api/v1/topics/"+b+"/read", bob, nil))
	ev := bc.expect("TOPIC_READ_STATE_UPDATE", nil)
	state := toMaps(ev["topics"])[0]
	require.Equal(t, b, state["topic_id"])
	require.Equal(t, true, state["read"])
	bravo := viewer(itemByTitle(t, feedItems(e.feed(bob, base+"new")), "Bravo"))
	require.Equal(t, true, bravo["read"])
	require.Equal(t, false, bravo["has_new_replies"])
	require.EqualValues(t, 1, bravo["last_read_post_number"])

	// A reply after the open flags the topic without making it unread.
	e.reply(carol, b, "A new reply", "")
	bravo = viewer(itemByTitle(t, feedItems(e.feed(bob, base+"new")), "Bravo"))
	require.Equal(t, true, bravo["read"])
	require.Equal(t, true, bravo["has_new_replies"])
	require.EqualValues(t, 1, bravo["new_reply_count"])
	require.EqualValues(t, 1, bravo["unread_count"])
	require.Equal(t, []string{"Bravo", "Charlie", "Foxtrot"}, titles(feedItems(e.feed(bob, base+"new&hide_read=true"))),
		"hide_read keeps read topics with new replies")
	e.expect(204, e.do("PUT", "/api/v1/topics/"+b+"/read", bob, map[string]any{"post_number": 2}))
	bravo = viewer(itemByTitle(t, feedItems(e.feed(bob, base+"new")), "Bravo"))
	require.Equal(t, false, bravo["has_new_replies"])
	require.EqualValues(t, 0, bravo["unread_count"])
	require.Equal(t, []string{"Charlie", "Foxtrot"}, titles(feedItems(e.feed(bob, base+"new&hide_read=true"))))

	// Marking unread keeps the read position, and other sessions hear about it.
	e.expect(204, e.do("DELETE", "/api/v1/topics/"+b+"/read", bob, nil))
	ev = bc.expect("TOPIC_READ_STATE_UPDATE", func(d map[string]any) bool {
		ts := toMaps(d["topics"])
		return len(ts) == 1 && ts[0]["read"] == false
	})
	require.EqualValues(t, 2, toMaps(ev["topics"])[0]["last_read_post_number"])
	bravo = viewer(itemByTitle(t, feedItems(e.feed(bob, base+"new")), "Bravo"))
	require.Equal(t, false, bravo["read"])
	require.EqualValues(t, 2, bravo["last_read_post_number"])
	require.Equal(t, true, viewer(e.expect(200, e.do("GET", "/api/v1/topics/"+a, bob, nil)).obj(t))["read"],
		"single topics carry the same viewer state")

	// Batch opens skip what the caller cannot see.
	states := e.expect(200, e.do("POST", "/api/v1/feed/read", bob, map[string]any{
		"topic_ids": []string{b, c, d, uuid.NewString()},
	})).list(t)
	require.Len(t, states, 2)
	e.expect(401, e.do("POST", "/api/v1/feed/read", "", map[string]any{"topic_ids": []string{b}}))
	require.Equal(t, []string{"Foxtrot"}, titles(feedItems(e.feed(bob, base+"new&hide_read=true"))))

	// Mark all as read, bounded by the time the feed was loaded.
	e.expect(204, e.do("DELETE", "/api/v1/topics/"+b+"/read", bob, nil))
	marked := e.expect(200, e.do("POST", "/api/v1/places/news/feed/read", bob, map[string]any{
		"before": time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
	})).obj(t)
	require.EqualValues(t, 1, marked["marked"], "only Foxtrot was last active before then")
	require.Equal(t, []string{"Bravo"}, titles(feedItems(e.feed(bob, base+"new&hide_read=true"))))
	e.expect(200, e.do("POST", "/api/v1/places/news/feed/read", bob, nil))
	bc.expect("TOPIC_READ_STATE_UPDATE", func(d map[string]any) bool { return d["all"] == true })
	require.Empty(t, feedItems(e.feed(bob, base+"new&hide_read=true")))

	// Active order follows replies.
	e.reply(bob, c, "Bump", "")
	require.Equal(t, []string{"Charlie", "Bravo", "Alpha", "Foxtrot"}, titles(feedItems(e.feed(bob, base+"active"))))

	// Instance feeds: home covers joined places, all covers public content.
	e.createPlace(owner, "other", "public")
	misc := e.board(owner, "other", map[string]any{"slug": "misc", "name": "Misc"})
	e.topic(owner, misc, "Golf", "Elsewhere")
	e.createPlace(owner, "secret", "private")
	hidden := e.board(owner, "secret", map[string]any{"slug": "hidden", "name": "Hidden"})
	e.topic(owner, hidden, "Hotel", "Private")

	all := feedItems(e.feed("", "/api/v1/feed?sort=new"))
	require.ElementsMatch(t, []string{"Alpha", "Bravo", "Charlie", "Foxtrot", "Golf"}, titles(all))
	require.Equal(t, "other", itemByTitle(t, all, "Golf")["place"].(map[string]any)["slug"])
	require.Contains(t, titles(feedItems(e.feed("", "/api/v1/feed?sort=new&nsfw=true"))), "Echo")
	e.expect(401, e.do("GET", "/api/v1/feed?scope=home", "", nil))
	require.ElementsMatch(t, []string{"Alpha", "Bravo", "Charlie", "Foxtrot"}, titles(feedItems(e.feed(bob, "/api/v1/feed?sort=new"))))
	require.Contains(t, titles(feedItems(e.feed(bob, "/api/v1/feed?scope=all&sort=new"))), "Golf")
	require.ElementsMatch(t, []string{"Alpha", "Bravo", "Charlie", "Delta", "Foxtrot", "Golf", "Hotel"},
		titles(feedItems(e.feed(owner, "/api/v1/feed?sort=new"))), "home includes the owner's private place")

	// Muting leaves topics out of home (not the place feed); watched topics stay, since the
	// most specific preference wins (authors watch their own topics).
	e.expect(204, e.do("PUT", "/api/v1/boards/"+id(general)+"/subscription", bob, map[string]any{"level": "muted"}))
	require.Equal(t, []string{"Alpha"}, titles(feedItems(e.feed(bob, "/api/v1/feed"))))
	require.Len(t, feedItems(e.feed(bob, base+"new")), 4)
	e.expect(204, e.do("PUT", "/api/v1/topics/"+b+"/subscription", bob, map[string]any{"level": "watching"}))
	require.ElementsMatch(t, []string{"Alpha", "Bravo"}, titles(feedItems(e.feed(bob, "/api/v1/feed"))))
	e.expect(204, e.do("PUT", "/api/v1/topics/"+a+"/subscription", bob, map[string]any{"level": "muted"}))
	require.Equal(t, []string{"Bravo"}, titles(feedItems(e.feed(bob, "/api/v1/feed"))))

	// Deleting an account takes its votes off.
	e.expect(204, e.do("DELETE", "/api/v1/users/@me", carol, map[string]any{"password": "carol-password-1"}))
	alpha = e.expect(200, e.do("GET", "/api/v1/topics/"+a, owner, nil)).obj(t)
	require.EqualValues(t, 1, alpha["upvotes"])
	require.EqualValues(t, 1, alpha["score"])

	// Capabilities are advertised.
	inst := e.expect(200, e.do("GET", "/api/v1/instance", "", nil)).obj(t)
	require.Equal(t, true, inst["features"].(map[string]any)["feed"])
	require.Equal(t, true, inst["features"].(map[string]any)["topic_votes"])
	require.Equal(t, "hot", inst["feed"].(map[string]any)["sorts"].([]any)[0])
	require.EqualValues(t, 100, inst["feed"].(map[string]any)["read_batch"])
}
