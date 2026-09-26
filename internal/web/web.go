// Package web embeds forge's single-page UI.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static/index.html
var files embed.FS

// Handler serves the embedded single-page app at "/".
func Handler() http.Handler {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(sub))
}
