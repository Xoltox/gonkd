// Package web embeds gonkd's single-page UI: a plain HTML/CSS/JS static
// site (no build step, no hashed filenames) written directly under
// static/.
package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
)

//go:embed all:static
var embedded embed.FS

// asset is one served file: its original bytes, an optional smaller
// gzipped copy (kept only when it actually helps), a strong ETag over the
// original content, and its Content-Type.
type asset struct {
	data  []byte
	gzip  []byte // nil if compression wasn't tried or didn't help
	etag  string
	ctype string
}

// assets is built once at init from the embedded tree, keyed by path
// relative to static/ ("index.html", "app.css", "screens/now.js", ...).
var assets map[string]*asset

func init() {
	sub, err := fs.Sub(embedded, "static")
	if err != nil {
		panic(err)
	}
	assets = map[string]*asset{}
	err = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(sub, p)
		if err != nil {
			return err
		}
		assets[p] = buildAsset(p, data)
		return nil
	})
	if err != nil {
		panic(err)
	}
	if _, ok := assets["index.html"]; !ok {
		panic("web: static/index.html missing from the embed")
	}
}

func buildAsset(p string, data []byte) *asset {
	sum := sha256.Sum256(data)
	a := &asset{
		data:  data,
		ctype: contentTypeFor(p),
		etag:  `"` + hex.EncodeToString(sum[:])[:16] + `"`,
	}
	if compressible(p) {
		if gz, ok := gzipBytes(data); ok && len(gz) < len(data) {
			a.gzip = gz
		}
	}
	return a
}

// gzipBytes compresses data at the best ratio (this runs once per asset at
// startup, not on the request path, so the extra CPU is free). ok is false
// if compression itself failed, not merely if it didn't help.
func gzipBytes(data []byte) (out []byte, ok bool) {
	var buf bytes.Buffer
	gw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, false
	}
	if _, err := gw.Write(data); err != nil {
		return nil, false
	}
	if err := gw.Close(); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

func compressible(p string) bool {
	switch path.Ext(p) {
	case ".html", ".css", ".js", ".svg", ".json":
		return true
	}
	return false
}

func contentTypeFor(p string) string {
	switch path.Ext(p) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".woff2":
		return "font/woff2"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".json":
		return "application/json; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// Handler serves the embedded static site: "/" and any unknown extension-
// less GET path fall back to index.html (so client-side routing works),
// a path with an extension that isn't a known asset is a real 404, and
// every response gets an explicit Content-Type, nosniff, a strong ETag and
// Cache-Control: no-cache (QUAL-13: explicit headers rather than
// http.FileServer's directory/redirect semantics; no-cache means the
// browser always revalidates and gets a cheap 304 when nothing changed,
// so a redeploy is still picked up immediately).
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if p == "." {
			p = ""
		}
		a, ok := assets[p]
		if !ok {
			if p != "" && path.Ext(p) != "" {
				http.NotFound(w, r)
				return
			}
			p = "index.html"
			a = assets[p]
		}
		serveAsset(w, r, a)
	})
}

func serveAsset(w http.ResponseWriter, r *http.Request, a *asset) {
	h := w.Header()
	h.Set("ETag", a.etag)
	h.Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == a.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	data := a.data
	gz := a.gzip != nil && acceptsGzip(r)
	if gz {
		data = a.gzip
	}
	if a.gzip != nil {
		h.Set("Vary", "Accept-Encoding")
	}
	h.Set("Content-Type", a.ctype)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	if gz {
		h.Set("Content-Encoding", "gzip")
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(data); err != nil {
		log.Printf("gonkd: write static asset: %v", err)
	}
}

func acceptsGzip(r *http.Request) bool {
	for _, enc := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if strings.TrimSpace(enc) == "gzip" {
			return true
		}
	}
	return false
}
