// Package ui serves the operator page (ADR 010, ADR 015).
//
// The page is plain HTML, CSS and JavaScript in the web folder, compiled into the engine
// binary. It talks to the engine's own API from the browser; nothing here touches Postgres.
package ui

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
)

//go:embed web
var files embed.FS

func init() {
	// The page's scripts are ES modules (.mjs). Browsers refuse a module that is not served as JavaScript.
	mime.AddExtensionType(".mjs", "text/javascript; charset=utf-8")
}

// csp lets the page load scripts and styles from its own address only and call only its own
// address (the API). It forbids inline script and inline style, so the HTML has none, and it
// stops the page being shown inside another site's frame (ADR 015, decision 3).
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
	"img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Handler serves the files under the prefix "/ui/". Mount it at "/ui/".
func Handler() http.Handler {
	sub, err := fs.Sub(files, "web")
	if err != nil {
		panic(err) // the folder is compiled in; this cannot happen
	}
	static := http.StripPrefix("/ui/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Frame-Options", "DENY") // the older header for the same rule, for old browsers
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store") // a changed page is never served stale
		static.ServeHTTP(w, r)
	})
}

// Files exposes the embedded folder to tests.
func Files() fs.FS {
	sub, _ := fs.Sub(files, "web")
	return sub
}
