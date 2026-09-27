package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIndexServed(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("content-type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("cache-control = %q, want no-cache", cc)
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("expected an ETag")
	}
}

// TestSPAFallback: an unknown extensionless path (client-side routing)
// falls back to index.html rather than 404ing.
func TestSPAFallback(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/control", nil)
	Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("content-type = %q, want index.html's", ct)
	}
}

// TestUnknownAssetIs404: a path that looks like a real asset (has an
// extension) but isn't embedded must 404, not fall back to index.html.
func TestUnknownAssetIs404(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/missing.js", nil)
	Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestETagRevalidation(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on first response")
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("If-None-Match", etag)
	Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rec2.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// TestGzipRoundTrip exercises the gzip/caching machinery directly (rather
// than depending on the real static/index.html happening to be large
// enough to compress smaller, which would make this test flaky against
// whatever the frontend currently ships).
func TestGzipRoundTrip(t *testing.T) {
	body := make([]byte, 4096)
	for i := range body {
		body[i] = byte('a' + i%3) // compresses well, unlike random bytes
	}
	a := buildAsset("app.css", body)
	if a.gzip == nil {
		t.Fatal("expected gzip to help on this compressible payload")
	}
	assets["__test.css"] = a
	defer delete(assets, "__test.css")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/__test.css", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("content-encoding = %q, want gzip", enc)
	}
	if v := rec.Header().Get("Vary"); v != "Accept-Encoding" {
		t.Fatalf("vary = %q", v)
	}
	if rec.Body.Len() >= len(body) {
		t.Fatalf("gzipped body (%d) not smaller than original (%d)", rec.Body.Len(), len(body))
	}

	// Without Accept-Encoding, the same asset is served uncompressed but
	// still advertises Vary.
	rec2 := httptest.NewRecorder()
	Handler().ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/__test.css", nil))
	if rec2.Header().Get("Content-Encoding") != "" {
		t.Fatal("must not gzip without Accept-Encoding")
	}
	if rec2.Body.Len() != len(body) {
		t.Fatalf("uncompressed body length = %d, want %d", rec2.Body.Len(), len(body))
	}
}
