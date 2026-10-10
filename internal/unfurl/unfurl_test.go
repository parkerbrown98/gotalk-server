package unfurl

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func TestFetchPage(t *testing.T) {
	pic := pngBytes(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/article", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head>
			<title>Fallback title</title>
			<meta property="og:title" content="  Hand-cut   dovetails ">
			<meta property="og:description" content="A guide to cutting joints by hand.">
			<meta property="og:site_name" content="Joinery Weekly">
			<meta property="og:image" content="/img/cover.png">
			<meta name="twitter:card" content="summary_large_image">
			<meta name="theme-color" content="#AA3300">
		</head><body><meta property="og:title" content="ignored"></body></html>`))
	})
	mux.HandleFunc("/img/cover.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pic)
	})
	mux.HandleFunc("/plain", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>Just a title</title><meta name="description" content="desc"></head></html>`))
	})
	mux.HandleFunc("/photo", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/img/cover.png", http.StatusFound)
	})
	mux.HandleFunc("/empty", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head></head><body>nothing</body></html>`))
	})
	mux.HandleFunc("/zip", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write([]byte("PK"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := New(Options{AllowPrivate: true, MaxImage: 1 << 20})
	ctx := context.Background()

	res, err := c.Fetch(ctx, srv.URL+"/article")
	require.NoError(t, err)
	assert.Equal(t, KindLink, res.Kind)
	assert.Equal(t, "Hand-cut dovetails", res.Title)
	assert.Equal(t, "A guide to cutting joints by hand.", res.Description)
	assert.Equal(t, "Joinery Weekly", res.SiteName)
	assert.Equal(t, "#aa3300", res.ThemeColor)
	assert.True(t, res.LargeImage)
	assert.Equal(t, pic, res.Image)

	res, err = c.Fetch(ctx, srv.URL+"/plain")
	require.NoError(t, err)
	assert.Equal(t, "Just a title", res.Title)
	assert.Equal(t, "desc", res.Description)
	assert.Equal(t, "127.0.0.1", res.SiteName)
	assert.Nil(t, res.Image)

	res, err = c.Fetch(ctx, srv.URL+"/photo")
	require.NoError(t, err)
	assert.Equal(t, KindImage, res.Kind)
	assert.Equal(t, srv.URL+"/img/cover.png", res.FinalURL)
	assert.Equal(t, pic, res.Image)

	_, err = c.Fetch(ctx, srv.URL+"/empty")
	require.Error(t, err)
	_, err = c.Fetch(ctx, srv.URL+"/zip")
	require.Error(t, err)
	_, err = c.Fetch(ctx, srv.URL+"/missing")
	require.Error(t, err)
	_, err = c.Fetch(ctx, "ftp://example.com/file")
	require.Error(t, err)

	small := New(Options{AllowPrivate: true, MaxImage: 10})
	_, err = small.Fetch(ctx, srv.URL+"/photo")
	require.Error(t, err)
}

func TestFetchBlocksPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<title>internal</title>`))
	}))
	t.Cleanup(srv.Close)
	_, err := New(Options{MaxImage: 1 << 20}).Fetch(context.Background(), srv.URL)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrBlocked), "got %v", err)
}

func TestPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8":           true,
		"2606:4700::1111":   true,
		"127.0.0.1":         false,
		"10.1.2.3":          false,
		"172.16.0.1":        false,
		"192.168.1.1":       false,
		"169.254.169.254":   false,
		"100.64.0.1":        false,
		"0.0.0.0":           false,
		"::1":               false,
		"fe80::1":           false,
		"fd00::1":           false,
		"::ffff:127.0.0.1":  false,
		"::ffff:8.8.8.8":    true,
		"64:ff9b::a00:1":    false,
		"2002:a00:1::1":     false,
		"224.0.0.1":         false,
		"255.255.255.255":   false,
		"198.18.0.1":        false,
		"203.0.113.9":       false,
		"2001:db8::1":       false,
		"93.184.216.34":     true,
		"2a00:1450:4001::1": true,
	} {
		assert.Equal(t, want, Public(netip.MustParseAddr(addr)), addr)
	}
}
