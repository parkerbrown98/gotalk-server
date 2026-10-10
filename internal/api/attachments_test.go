package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/parkerbrown98/gotalk-server/internal/config"
)

func (e *env) attach(token, filename, contentType string, body []byte) map[string]any {
	e.t.Helper()
	return e.expect(201, e.raw("POST", "/api/v1/attachments?filename="+filename, token, contentType, body)).obj(e.t)
}

func TestMessageAttachments(t *testing.T) {
	e := newEnv(t)
	owner := e.setup()
	bob, _ := e.register("bob")
	e.createPlace(owner, "shop", "public")
	e.expect(200, e.do("POST", "/api/v1/places/shop/join", bob, nil))
	ch := e.channel(owner, "shop", map[string]any{"name": "bench", "kind": "text"})

	img := e.attach(owner, "dovetail.png", "image/png", pngBytes(t, 40, 30))
	require.Equal(t, "dovetail.png", img["filename"])
	require.Equal(t, "image/png", img["content_type"])
	require.EqualValues(t, 40, img["width"])
	require.EqualValues(t, 30, img["height"])
	require.Contains(t, img["url"], "/media/attachments/")

	pdf := e.attach(owner, "..%2F..%2Fplans%20v2.pdf", "application/pdf", []byte("%PDF-1.4 fake"))
	require.Equal(t, "plans v2.pdf", pdf["filename"], "path elements are dropped")
	require.Equal(t, "application/pdf", pdf["content_type"])
	require.Nil(t, pdf["width"])
	require.True(t, strings.HasSuffix(pdf["url"].(string), ".pdf"), pdf["url"])

	// Images are served inline; other files only as downloads, in a sandbox.
	got := e.expect(200, e.get(strings.TrimPrefix(img["url"].(string), e.srv.URL), nil))
	require.Equal(t, "image/png", got.Header.Get("Content-Type"))
	require.Equal(t, `inline; filename=dovetail.png`, got.Header.Get("Content-Disposition"))
	got = e.expect(200, e.get(strings.TrimPrefix(pdf["url"].(string), e.srv.URL), nil))
	require.Equal(t, "application/pdf", got.Header.Get("Content-Type"))
	require.Equal(t, `attachment; filename="plans v2.pdf"`, got.Header.Get("Content-Disposition"))
	require.Equal(t, "default-src 'none'; sandbox", got.Header.Get("Content-Security-Policy"))
	require.Equal(t, "%PDF-1.4 fake", string(got.Raw))

	html := e.attach(owner, "x.html", "text/html", []byte("<script>alert(1)</script>"))
	got = e.expect(200, e.get(strings.TrimPrefix(html["url"].(string), e.srv.URL), nil))
	require.True(t, strings.HasPrefix(got.Header.Get("Content-Disposition"), "attachment"))

	// Uploads need an account, a body and must fit the limit.
	e.expect(401, e.raw("POST", "/api/v1/attachments?filename=a.txt", "", "text/plain", []byte("hi")))
	e.expect(400, e.raw("POST", "/api/v1/attachments?filename=a.txt", owner, "text/plain", nil))
	require.Equal(t, http.StatusRequestEntityTooLarge,
		e.raw("POST", "/api/v1/attachments", owner, "application/zip", make([]byte, e.cfg.Uploads.MaxSize+1)).Status)

	// A message may be only files; they come back in order.
	m := e.send(owner, ch, "", map[string]any{"attachment_ids": []string{id(pdf), id(img)}})
	files := toMaps(m["attachments"])
	require.Equal(t, []string{id(pdf), id(img)}, []string{id(files[0]), id(files[1])})
	require.Empty(t, m["embeds"])
	listed := e.messages(bob, ch, "")
	require.Len(t, toMaps(listed[len(listed)-1]["attachments"]), 2)

	// Files cannot be reused, borrowed, repeated or missing.
	e.expect(422, e.do("POST", "/api/v1/channels/"+id(ch)+"/messages", owner, map[string]any{"attachment_ids": []string{id(img)}}))
	mine := e.attach(owner, "a.txt", "text/plain", []byte("hello"))
	e.expect(422, e.do("POST", "/api/v1/channels/"+id(ch)+"/messages", bob, map[string]any{"attachment_ids": []string{id(mine)}}))
	e.expect(422, e.do("POST", "/api/v1/channels/"+id(ch)+"/messages", owner, map[string]any{"attachment_ids": []string{id(mine), id(mine)}}))
	e.expect(422, e.do("POST", "/api/v1/channels/"+id(ch)+"/messages", owner, map[string]any{"content": ""}))
	unsent := e.attach(owner, "n.txt", "text/plain", []byte("n"))
	many := []string{id(unsent)}
	for len(many) < 11 {
		many = append(many, id(e.attach(owner, "n.txt", "text/plain", []byte("n"))))
	}
	e.expect(422, e.do("POST", "/api/v1/channels/"+id(ch)+"/messages", owner, map[string]any{"attachment_ids": many}))

	// Messages with files may be edited down to no text; plain messages may not.
	withText := e.send(owner, ch, "see attached", map[string]any{"attachment_ids": []string{id(mine)}})
	e.expect(200, e.do("PATCH", "/api/v1/messages/"+id(withText), owner, map[string]any{"content": ""}))
	plain := e.send(owner, ch, "plain")
	e.expect(422, e.do("PATCH", "/api/v1/messages/"+id(plain), owner, map[string]any{"content": " "}))

	// Unsent uploads and files of deleted messages are swept once old enough.
	e.expect(204, e.do("DELETE", "/api/v1/messages/"+id(m), owner, nil))
	_, err := e.svc.Pool().Exec(context.Background(), `UPDATE uploads SET created_at = now() - interval '2 hours'`)
	require.NoError(t, err)
	require.NoError(t, e.svc.Maintain(context.Background()))
	e.expect(404, e.get(strings.TrimPrefix(img["url"].(string), e.srv.URL), nil))
	e.expect(404, e.get(strings.TrimPrefix(unsent["url"].(string), e.srv.URL), nil))
	e.expect(200, e.get(strings.TrimPrefix(mine["url"].(string), e.srv.URL), nil))

	info := e.expect(200, e.do("GET", "/api/v1/instance", "", nil)).obj(t)
	require.EqualValues(t, 10, info["limits"].(map[string]any)["attachments"])
}

func TestPostAttachmentsAndFeedImages(t *testing.T) {
	e := newEnv(t)
	owner := e.setup()
	e.createPlace(owner, "gallery", "public")
	b := e.board(owner, "gallery", map[string]any{"slug": "builds", "name": "Builds"})

	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, id(e.attach(owner, "photo.png", "image/png", pngBytes(t, 20+i, 10))))
	}
	notes := e.attach(owner, "notes.txt", "text/plain", []byte("cut list"))
	res := e.expect(201, e.do("POST", "/api/v1/boards/"+id(b)+"/topics", owner, map[string]any{
		"title": "My workbench", "attachment_ids": append([]string{id(notes)}, ids...),
	})).obj(t)
	post := res["post"].(map[string]any)
	require.Empty(t, post["content"])
	require.Len(t, toMaps(post["attachments"]), 6)
	topicID := res["topic"].(map[string]any)["id"].(string)

	reply := e.expect(201, e.do("POST", "/api/v1/topics/"+topicID+"/posts", owner, map[string]any{
		"content": "Finished!", "attachment_ids": []string{id(e.attach(owner, "done.png", "image/png", pngBytes(t, 8, 8)))},
	})).obj(t)
	require.Len(t, toMaps(reply["attachments"]), 1)
	posts := e.items(e.expect(200, e.do("GET", "/api/v1/topics/"+topicID+"/posts", owner, nil)))
	require.Len(t, toMaps(posts[0]["attachments"]), 6)

	// Feed items carry the opening post's first images, not files or reply images.
	item := itemByTitle(t, feedItems(e.feed(owner, "/api/v1/places/gallery/feed")), "My workbench")
	images := toMaps(item["images"])
	require.Len(t, images, 4)
	require.EqualValues(t, 5, item["image_count"])
	require.Equal(t, ids[:4], []string{id(images[0]), id(images[1]), id(images[2]), id(images[3])})
	require.EqualValues(t, 20, images[0]["width"])
	require.Nil(t, item["embed"])

	e.topic(owner, b, "Text only", "No pictures")
	item = itemByTitle(t, feedItems(e.feed(owner, "/api/v1/places/gallery/feed")), "Text only")
	require.Empty(t, item["images"])
	require.EqualValues(t, 0, item["image_count"])
}

func TestLinkPreviews(t *testing.T) {
	pic := pngBytes(t, 120, 60)
	var hits atomic.Int32
	site := http.NewServeMux()
	site.HandleFunc("/article", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>Dovetails</title>
			<meta property="og:title" content="Cutting dovetails by hand">
			<meta property="og:description" content="Saw, chisel, repeat.">
			<meta property="og:site_name" content="Joinery Weekly">
			<meta property="og:image" content="/cover.png">
			<meta name="theme-color" content="#336699">
			</head><body></body></html>`))
	})
	site.HandleFunc("/cover.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pic)
	})
	ext := httptest.NewServer(site)
	t.Cleanup(ext.Close)

	e := newEnv(t, func(o *envOpts) { o.mutate = func(c *config.Config) { c.Embeds.AllowPrivateNetworks = true } })
	owner := e.setup()
	e.createPlace(owner, "shop", "public")
	ch := e.channel(owner, "shop", map[string]any{"name": "bench", "kind": "text"})
	ws := e.connect(owner)

	link := ext.URL + "/article"
	content := "Read " + link + ". Not <" + ext.URL + "/hidden> or `" + ext.URL + "/code`"
	m := e.send(owner, ch, content)
	require.Empty(t, m["embeds"], "previews are fetched after sending")

	upd := ws.expect("MESSAGE_UPDATE", func(d map[string]any) bool { return d["id"] == id(m) })
	embeds := toMaps(upd["embeds"])
	require.Len(t, embeds, 1)
	em := embeds[0]
	require.Equal(t, link, em["url"])
	require.Equal(t, "link", em["kind"])
	require.Equal(t, "Cutting dovetails by hand", em["title"])
	require.Equal(t, "Saw, chisel, repeat.", em["description"])
	require.Equal(t, "Joinery Weekly", em["site_name"])
	require.Equal(t, "#336699", em["color"])
	image := em["image"].(map[string]any)
	require.EqualValues(t, 120, image["width"])
	require.Contains(t, image["url"], e.srv.URL+"/media/embeds/", "the image is copied to this instance")
	e.expect(200, e.get(strings.TrimPrefix(image["url"].(string), e.srv.URL), nil))

	// Listing includes the cached preview; posting the link again reuses it.
	listed := e.messages(owner, ch, "")
	require.Len(t, toMaps(listed[len(listed)-1]["embeds"]), 1)
	again := e.send(owner, ch, "again: "+link)
	require.Len(t, toMaps(again["embeds"]), 1)
	require.EqualValues(t, 1, hits.Load())

	// Images linked directly preview as images; forum posts get previews too.
	direct := e.send(owner, ch, ext.URL+"/cover.png")
	upd = ws.expect("MESSAGE_UPDATE", func(d map[string]any) bool { return d["id"] == id(direct) })
	em = toMaps(upd["embeds"])[0]
	require.Equal(t, "image", em["kind"])
	require.Equal(t, true, em["large_image"])

	b := e.board(owner, "shop", map[string]any{"slug": "builds", "name": "Builds"})
	topicID, _ := e.topic(owner, b, "Worth a read", "See "+ext.URL+"/article?ref=forum")
	require.Eventually(t, func() bool {
		item := itemByTitle(t, feedItems(e.feed(owner, "/api/v1/places/shop/feed")), "Worth a read")
		em, _ := item["embed"].(map[string]any)
		return em != nil && em["title"] == "Cutting dovetails by hand"
	}, 10*time.Second, 50*time.Millisecond)
	posts := e.items(e.expect(200, e.do("GET", "/api/v1/topics/"+topicID+"/posts", owner, nil)))
	require.Len(t, toMaps(posts[0]["embeds"]), 1)
}

func TestLinkPreviewsSkipPrivateAddresses(t *testing.T) {
	ext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<title>Internal admin</title>`))
	}))
	t.Cleanup(ext.Close)

	e := newEnv(t)
	owner := e.setup()
	e.createPlace(owner, "shop", "public")
	ch := e.channel(owner, "shop", map[string]any{"name": "bench", "kind": "text"})
	m := e.send(owner, ch, ext.URL)
	require.Eventually(t, func() bool {
		var status string
		err := e.svc.Pool().QueryRow(context.Background(), `SELECT status FROM link_previews WHERE url = $1`, ext.URL).Scan(&status)
		return err == nil && status == "failed"
	}, 10*time.Second, 50*time.Millisecond)
	got := e.expect(200, e.do("GET", "/api/v1/messages/"+id(m), owner, nil)).obj(t)
	require.Empty(t, got["embeds"])
}
