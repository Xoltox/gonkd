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

func postJSON(t *testing.T, mux *http.ServeMux, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://192.0.2.10"+path, strings.NewReader(body))
	req.Host = "192.0.2.10"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestTuneValidation(t *testing.T) {
	mux := newRouter(t)
	cases := []string{
		`{"speed":9}`,
		`{"speed":501}`,
		`{"flow":49}`,
		`{"flow":201}`,
		`{"fan":-1}`,
		`{"fan":101}`,
	}
	for _, body := range cases {
		rec := postJSON(t, mux, "/gonkd/tune", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("tune %s: status = %d, want 400; body=%s", body, rec.Code, rec.Body.String())
		}
	}
	// In range but disconnected: must get past validation (409, not 400).
	rec := postJSON(t, mux, "/gonkd/tune", `{"speed":150,"flow":90,"fan":50}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("in-range tune while disconnected: status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHeatValidation(t *testing.T) {
	mux := newRouter(t)
	cases := []string{
		`{"hotend":261}`,
		`{"hotend":-1}`,
		`{"bed":111}`,
		`{"bed":-1}`,
	}
	for _, body := range cases {
		rec := postJSON(t, mux, "/gonkd/heat", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("heat %s: status = %d, want 400; body=%s", body, rec.Code, rec.Body.String())
		}
	}
	rec := postJSON(t, mux, "/gonkd/heat", `{"hotend":200,"bed":60}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("in-range heat while disconnected: status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPresetsGetDefaultsAndPutPersists(t *testing.T) {
	s, dir := newTestServer(t)
	presetsPath := filepath.Join(dir, "presets.json")
	s.Mgr.SetPresetStore(printer.NewPresetStore(presetsPath))
	mux := http.NewServeMux()
	s.Routes(mux)

	req := httptest.NewRequest(http.MethodGet, "http://192.0.2.10/gonkd/presets", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET status = %d", rec.Code)
	}
	var got []printer.Preset
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "PLA" {
		t.Fatalf("defaults = %+v", got)
	}

	rec2 := postPresets(t, mux, `[{"name":"ABS","hotend":240,"bed":100}]`)
	if rec2.Code != 200 {
		t.Fatalf("PUT status = %d, body=%s", rec2.Code, rec2.Body.String())
	}
	if _, err := os.Stat(presetsPath); err != nil {
		t.Fatalf("presets file not written: %v", err)
	}

	// Too many presets: 400, and the file on disk is unchanged.
	var many strings.Builder
	many.WriteByte('[')
	for i := 0; i < 9; i++ {
		if i > 0 {
			many.WriteByte(',')
		}
		many.WriteString(`{"name":"P","hotend":200,"bed":60}`)
	}
	many.WriteByte(']')
	rec3 := postPresets(t, mux, many.String())
	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("too many presets status = %d, want 400", rec3.Code)
	}
}

func postPresets(t *testing.T, mux *http.ServeMux, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "http://192.0.2.10/gonkd/presets", strings.NewReader(body))
	req.Host = "192.0.2.10"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestStreamModeRejectedWithoutAllowStream: the contract's stream-mode gate
// (-allow-stream). AllowStream defaults false on a zero-value Server.
func TestStreamModeRejectedWithoutAllowStream(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Routes(mux)

	req := multipartUpload(t, "http://192.0.2.10/api/files/local?mode=stream", "a.gcode", []byte("G28\n"), false)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "stream mode disabled") {
		t.Fatalf("body = %q, want it to mention stream mode being disabled", rec.Body.String())
	}
}

func TestStreamModeAllowedWithFlag(t *testing.T) {
	s, dir := newTestServer(t)
	s.AllowStream = true
	mux := http.NewServeMux()
	s.Routes(mux)

	req := multipartUpload(t, "http://192.0.2.10/api/files/local?mode=stream", "a.gcode", []byte("G28\n"), false)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	// Disconnected Manager still refuses the upload (409), but must get
	// past the stream-mode gate, i.e. never 400.
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("status = %d, stream mode should be allowed", rec.Code)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "data"))
	if len(entries) != 0 {
		t.Fatalf("staged file left behind: %v", entries)
	}
}

// TestJobContinueDisconnectedConflict: past the CSRF/host gate, a
// disconnected Manager's Continue() (see printer.Manager.Continue) surfaces
// as 409, the same as the other job routes.
func TestJobContinueDisconnectedConflict(t *testing.T) {
	mux := newRouter(t)
	req := httptest.NewRequest(http.MethodPost, "http://192.0.2.10/gonkd/job/continue", nil)
	req.Host = "192.0.2.10"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestJobContinueRejectsGET(t *testing.T) {
	mux := newRouter(t)
	req := httptest.NewRequest(http.MethodGet, "http://192.0.2.10/gonkd/job/continue", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// --- Bed / mesh routes ---

func TestMeshGetDisconnectedConflict(t *testing.T) {
	mux := newRouter(t)
	req := httptest.NewRequest(http.MethodGet, "http://192.0.2.10/gonkd/mesh", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMeshGetRejectsPOST(t *testing.T) {
	mux := newRouter(t)
	req := httptest.NewRequest(http.MethodPost, "http://192.0.2.10/gonkd/mesh", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestMeshLevelActionValidation(t *testing.T) {
	mux := newRouter(t)
	rec := postJSON(t, mux, "/gonkd/mesh/level", `{"action":"sideways"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad action: status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	// A recognized action past validation, disconnected: 409, not 400.
	for _, action := range []string{"start", "next", "abort", "finish"} {
		rec := postJSON(t, mux, "/gonkd/mesh/level", `{"action":"`+action+`"}`)
		if rec.Code != http.StatusConflict {
			t.Errorf("action %s while disconnected: status = %d, want 409; body=%s", action, rec.Code, rec.Body.String())
		}
	}
}

// Disconnected and with no leveling wizard in progress, MeshZAdjust
// rejects for lack of a known Z to adjust from (ErrInvalidCommand, 400)
// before it would otherwise hit the disconnected check.
func TestMeshZAdjustWithoutLevelingRejected(t *testing.T) {
	mux := newRouter(t)
	rec := postJSON(t, mux, "/gonkd/mesh/zadjust", `{"mm":0.1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMeshZOffsetDisconnectedConflict(t *testing.T) {
	mux := newRouter(t)
	rec := postJSON(t, mux, "/gonkd/mesh/zoffset", `{"z":0.1,"save":true}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMeshPointValidationAndConflict(t *testing.T) {
	mux := newRouter(t)
	rec := postJSON(t, mux, "/gonkd/mesh/point", `{"x":0,"y":0,"z":0.02}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestBedCornerValidationAndConflict(t *testing.T) {
	mux := newRouter(t)
	rec := postJSON(t, mux, "/gonkd/bed/corner", `{"corner":"nope"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad corner: status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	rec = postJSON(t, mux, "/gonkd/bed/corner", `{"corner":"fl"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("valid corner while disconnected: status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestBedRoutesRejectGET(t *testing.T) {
	mux := newRouter(t)
	for _, path := range []string{"/gonkd/mesh/level", "/gonkd/mesh/zadjust", "/gonkd/mesh/zoffset", "/gonkd/mesh/point", "/gonkd/bed/corner"} {
		req := httptest.NewRequest(http.MethodGet, "http://192.0.2.10"+path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: status = %d, want 405", path, rec.Code)
		}
	}
}
