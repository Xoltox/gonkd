package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Xoltox/gonkd/internal/printer"
)

// newTestServer builds a Server around a disconnected Manager (no Driver
// attached). Per the wave contract, tests that need a connected Manager are
// skipped rather than editing package printer.
func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	names := printer.NewNameMap(filepath.Join(dir, "names.json"))
	mgr := printer.NewManager(names)
	return &Server{
		Mgr:     mgr,
		DataDir: filepath.Join(dir, "data"),
		MaxBody: 1 << 20, // 1 MiB cap for tests
		Mode:    printer.ModeSDUpload,
	}, dir
}

func newRouter(t *testing.T) *http.ServeMux {
	t.Helper()
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Routes(mux)
	return mux
}

func TestVersionText(t *testing.T) {
	mux := newRouter(t)
	req := httptest.NewRequest(http.MethodGet, "http://192.0.2.10/api/version", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(body.Text, "OctoPrint") {
		t.Fatalf("text = %q, want prefix %q", body.Text, "OctoPrint")
	}
}

func multipartUpload(t *testing.T, url, fileName string, content []byte, print bool) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", fileName)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if print {
		if err := mw.WriteField("print", "true"); err != nil {
			t.Fatalf("WriteField: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, url, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Host = "192.0.2.10"
	return req
}

// TestUploadDisconnected: with no Driver attached, Manager.Upload must
// return ErrDisconnected synchronously; the handler maps that to 409 and
// must remove the staged file rather than leaking it under DataDir.
func TestUploadDisconnected(t *testing.T) {
	s, dir := newTestServer(t)
	mux := http.NewServeMux()
	s.Routes(mux)

	req := multipartUpload(t, "http://192.0.2.10/api/files/local", "benchy.gcode", []byte("G28\nG1 X10\n"), true)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "data"))
	if len(entries) != 0 {
		t.Fatalf("staged file left behind: %v", entries)
	}
}

func TestUploadOversize(t *testing.T) {
	s, dir := newTestServer(t)
	s.MaxBody = 16 // tiny cap to force overflow
	mux := http.NewServeMux()
	s.Routes(mux)

	req := multipartUpload(t, "http://192.0.2.10/api/files/local", "big.gcode", bytes.Repeat([]byte("A"), 4096), false)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "data"))
	if len(entries) != 0 {
		t.Fatalf("staged file left behind after oversize upload: %v", entries)
	}
}

func TestMutatingRouteRejectsGET(t *testing.T) {
	mux := newRouter(t)
	req := httptest.NewRequest(http.MethodGet, "http://192.0.2.10/gonkd/send", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestCrossOriginRejected(t *testing.T) {
	mux := newRouter(t)
	req := httptest.NewRequest(http.MethodPost, "http://192.0.2.10/gonkd/emergency", nil)
	req.Host = "192.0.2.10"
	req.Header.Set("Origin", "http://evil.example.com")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestJSONRouteRejectsPlainText(t *testing.T) {
	mux := newRouter(t)
	req := httptest.NewRequest(http.MethodPost, "http://192.0.2.10/gonkd/send", strings.NewReader(`{"cmd":"M105"}`))
	req.Host = "192.0.2.10"
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", rec.Code)
	}
}

func TestHostAllowlist(t *testing.T) {
	mux := newRouter(t)

	// Bad Host: not an IP literal, not localhost, not in AllowHosts.
	req := httptest.NewRequest(http.MethodPost, "http://printer.example.com/gonkd/emergency", nil)
	req.Host = "printer.example.com"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("bad host status = %d, want 421", rec.Code)
	}

	// IP literal Host is accepted (this is how OrcaSlicer addresses gonkd).
	req2 := httptest.NewRequest(http.MethodPost, "http://192.0.2.10/gonkd/emergency", nil)
	req2.Host = "192.0.2.10"
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	// Disconnected Manager -> Emergency() may still error (ErrDisconnected ->
	// 409), but it must get past the host/CSRF gate, i.e. not 421/403/405.
	if rec2.Code == http.StatusMisdirectedRequest || rec2.Code == http.StatusForbidden || rec2.Code == http.StatusMethodNotAllowed {
		t.Fatalf("IP host status = %d, want past the CSRF gate", rec2.Code)
	}
}

func TestJogValidation(t *testing.T) {
	mux := newRouter(t)
	body := `{"axis":"Q","dist":10,"feed":1000}`
	req := httptest.NewRequest(http.MethodPost, "http://192.0.2.10/gonkd/jog", strings.NewReader(body))
	req.Host = "192.0.2.10"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
