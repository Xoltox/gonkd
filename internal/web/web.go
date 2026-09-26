// Package web embeds forge's single-page UI.
package web

import (
	"bytes"
	"embed"
	"net/http"
	"time"
)

//go:embed static/index.html
var files embed.FS

var indexHTML []byte

func init() {
	b, err := files.ReadFile("static/index.html")
	if err != nil {
		panic(err)
	}
	indexHTML = b
}

// Handler serves the embedded single-page app at "/" with explicit headers
// (QUAL-13): avoids http.FileServer's directory/redirect semantics for a
// single embedded file, and sets a real Content-Type, nosniff, and a
// cache-control that always revalidates so a redeploy is picked up.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(indexHTML))
	})
}
