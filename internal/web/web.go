// Package web embeds the server-rendered pages shipped inside the binary: a small landing
// page, the first-run setup wizard (which doubles as the instance settings page), and the
// pages email links open (password reset and email verification).
package web

import (
	"embed"
	"net/http"
)

//go:embed static/*.html
var static embed.FS

// ServePage writes an embedded HTML page.
func ServePage(w http.ResponseWriter, name string) {
	data, err := static.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	// Pages only load their own inline assets and call this origin's API.
	h.Set("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src 'self' https: data: blob:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	_, _ = w.Write(data)
}
