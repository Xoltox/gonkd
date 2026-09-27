// Package api implements gonkd's HTTP surface: a small OctoPrint-compatible
// subset (enough for OrcaSlicer's "Octo/Klipper" host type to test the
// connection and do one-click upload+print) plus gonkd's own JSON API used
// by the embedded UI.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Xoltox/gonkd/internal/printer"
)

const gonkdVersion = printer.Version

// Server wires the Manager to HTTP handlers.
type Server struct {
	Mgr         *printer.Manager
	DataDir     string             // for staging uploads before transfer
	MaxBody     int64              // upload size cap in bytes; <= 0 means no cap
	Mode        printer.UploadMode // default upload mode (sd or stream)
	AllowHosts  []string           // extra Host header values allowed besides IP literals and localhost
	AllowStream bool               // gate on stream-mode uploads/prints (-allow-stream)

	hubOnce sync.Once
	hub     *hub
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("gonkd: write response: %v", err)
	}
}

// noContent writes a bare 204: no body, so net/http never has to reconcile
// a Content-Length with an encoded "null".
func noContent(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// writeErr maps a Manager/Driver error to an HTTP status and writes a short,
// human-readable body (OrcaSlicer surfaces this text in its error dialog).
func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, printer.ErrDisconnected):
		code = http.StatusConflict
	case errors.Is(err, printer.ErrJobActive):
		code = http.StatusConflict
	case errors.Is(err, printer.ErrBusy):
		code = http.StatusConflict
	case errors.Is(err, printer.ErrNoJob):
		code = http.StatusConflict
	case errors.Is(err, printer.ErrNotPrinting):
		code = http.StatusConflict
	case errors.Is(err, printer.ErrClosed):
		code = http.StatusConflict
	case errors.Is(err, printer.ErrInvalidCommand), errors.Is(err, printer.ErrInvalidMode),
		errors.Is(err, printer.ErrTooManyPresets), errors.Is(err, printer.ErrInvalidPreset):
		code = http.StatusBadRequest
	case errors.Is(err, printer.ErrUnknownFile):
		code = http.StatusNotFound
	case errors.Is(err, printer.ErrTimeout):
		code = http.StatusServiceUnavailable
	}
	http.Error(w, err.Error(), code)
}

// Routes registers every handler on mux.
func (s *Server) Routes(mux *http.ServeMux) {
	// --- OctoPrint-compatible subset (for OrcaSlicer) ---
	mux.HandleFunc("/api/version", s.handleAPIVersion)
	mux.HandleFunc("/api/server", s.handleAPIServer)
	mux.HandleFunc("/api/connection", s.handleConnection)
	mux.HandleFunc("/api/printer", s.handlePrinterState)
	mux.HandleFunc("/api/job", s.handleJob)
	mux.HandleFunc("/api/files/local", s.handleFilesLocal)

	// --- gonkd's own API ---
	mux.HandleFunc("/gonkd/status", s.handleStatus)
	mux.HandleFunc("/gonkd/send", s.handleSend)
	mux.HandleFunc("/gonkd/console", s.handleConsole)
	mux.HandleFunc("/gonkd/files", s.handleSDFiles)
	mux.HandleFunc("/gonkd/files/delete", s.handleSDDelete)
	mux.HandleFunc("/gonkd/files/rename", s.handleSDRename)
	mux.HandleFunc("/gonkd/files/thumb", s.handleSDThumb)
	mux.HandleFunc("/gonkd/job/pause", s.handleJobPause)
	mux.HandleFunc("/gonkd/job/resume", s.handleJobResume)
	mux.HandleFunc("/gonkd/job/cancel", s.handleJobCancel)
	mux.HandleFunc("/gonkd/job/print", s.handleJobPrint)
	mux.HandleFunc("/gonkd/emergency", s.handleEmergency)
	mux.HandleFunc("/gonkd/babystep", s.handleBabystep)
	mux.HandleFunc("/gonkd/jog", s.handleJog)
	mux.HandleFunc("/gonkd/tune", s.handleTune)
	mux.HandleFunc("/gonkd/heat", s.handleHeat)
	mux.HandleFunc("/gonkd/presets", s.handlePresets)
	mux.HandleFunc("/gonkd/events", s.handleEvents)
}

// --- CSRF / Host hardening (SEC-5, SEC-25) ---
//
// Every mutating route runs through checkMutation: POST only, and if an
// Origin header (or Sec-Fetch-Site: cross-site) is present it must name the
// same host this request arrived on. The Host header itself must be an IP
// literal, "localhost", or one of Server.AllowHosts -- this is what lets
// OrcaSlicer (which addresses gonkd by IP and sends no Origin at all) through
// while blocking a browser tab on some unrelated DNS name from driving gonkd
// via a cross-site fetch/form.
func (s *Server) checkMutation(w http.ResponseWriter, r *http.Request, requireJSON bool) bool {
	return s.checkMutationMethod(w, r, http.MethodPost, requireJSON)
}

// checkMutationMethod is checkMutation generalized to a method other than
// POST, for /gonkd/presets's PUT.
func (s *Server) checkMutationMethod(w http.ResponseWriter, r *http.Request, method string, requireJSON bool) bool {
	if r.Method != method {
		w.Header().Set("Allow", method)
		http.Error(w, "method not allowed, "+method+" required", http.StatusMethodNotAllowed)
		return false
	}
	if !isAllowedHost(r.Host, s.AllowHosts) {
		http.Error(w, "host not allowed", http.StatusMisdirectedRequest)
		return false
	}
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs == "cross-site" {
		http.Error(w, "cross-site request rejected", http.StatusForbidden)
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return false
	}
	if requireJSON {
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "application/json") {
			http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
			return false
		}
	}
	return true
}

func stripHostPort(hostport string) string {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		h = hostport
	}
	h = strings.TrimPrefix(h, "[")
	h = strings.TrimSuffix(h, "]")
	return h
}

func isAllowedHost(host string, allow []string) bool {
	h := stripHostPort(host)
	if strings.EqualFold(h, "localhost") {
		return true
	}
	if net.ParseIP(h) != nil {
		return true
	}
	for _, a := range allow {
		if strings.EqualFold(stripHostPort(a), h) {
			return true
		}
	}
	return false
}

func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(stripHostPort(u.Host), stripHostPort(host))
}

// --- OctoPrint subset ---
// OrcaSlicer's OctoPrint host probes GET /api/version to confirm the target
// looks like OctoPrint - it requires "text" to start with "OctoPrint", else
// it fails with "Mismatched type of print host". Its uploader then uses
// POST /api/files/local with multipart fields file/print/path (not select)
// to upload+print, and polls /api/job + /api/printer for status. Accepting
// any (or no) X-Api-Key matches the task's "accept any" requirement, and
// keeps setup friction near zero on a LAN-only box.

type versionResp struct {
	API    string `json:"api"`
	Server string `json:"server"`
	Text   string `json:"text"`
}

func (s *Server) handleAPIVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, versionResp{API: "0.1", Server: "1.9.0", Text: "OctoPrint 1.9.0 (gonkd " + gonkdVersion + ")"})
}

func (s *Server) handleAPIServer(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{
		"version":  gonkdVersion,
		"safemode": nil,
	})
}

func (s *Server) handleConnection(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		snap := s.Mgr.Snapshot()
		state := "Closed"
		if snap.Connected {
			state = "Operational"
			switch snap.State {
			case printer.StatePrinting, printer.StateUploading:
				state = "Printing"
			case printer.StatePaused:
				state = "Paused"
			case printer.StateError:
				state = "Closed"
			}
		}
		writeJSON(w, 200, map[string]interface{}{
			"current": map[string]interface{}{
				"state":          state,
				"port":           "/dev/ttyUSB0",
				"baudrate":       250000,
				"printerProfile": "default",
			},
			"options": map[string]interface{}{
				"ports":     []string{"/dev/ttyUSB0"},
				"baudrates": []int{250000},
			},
		})
		return
	}
	// OrcaSlicer may POST to (re)connect; gonkd manages its own serial link,
	// so just acknowledge.
	if !s.checkMutation(w, r, false) {
		return
	}
	noContent(w)
}

func (s *Server) handlePrinterState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	snap := s.Mgr.Snapshot()
	if !snap.Connected {
		http.Error(w, "printer not connected", http.StatusConflict)
		return
	}
	flags := map[string]bool{
		"operational":   true,
		"paused":        snap.State == printer.StatePaused,
		"printing":      snap.State == printer.StatePrinting,
		"cancelling":    snap.State == printer.StateCancelling,
		"pausing":       false,
		"error":         snap.State == printer.StateError,
		"ready":         snap.State == printer.StateIdle,
		"closedOrError": false,
	}
	writeJSON(w, 200, map[string]interface{}{
		"state": map[string]interface{}{
			"text":  string(snap.State),
			"flags": flags,
		},
		"temperature": map[string]interface{}{
			"tool0": map[string]interface{}{"actual": snap.Temps.HotendActual, "target": snap.Temps.HotendTarget, "offset": 0},
			"bed":   map[string]interface{}{"actual": snap.Temps.BedActual, "target": snap.Temps.BedTarget, "offset": 0},
		},
	})
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		snap := s.Mgr.Snapshot()
		if !snap.Connected {
			http.Error(w, "printer not connected", http.StatusConflict)
			return
		}
		resp := map[string]interface{}{
			"job": map[string]interface{}{"file": map[string]interface{}{}},
			"progress": map[string]interface{}{
				"completion":    0.0,
				"printTime":     0,
				"printTimeLeft": 0,
			},
			"state": string(snap.State),
		}
		if snap.Job != nil {
			resp["job"] = map[string]interface{}{
				"file": map[string]interface{}{"name": snap.Job.Filename},
			}
			resp["progress"] = map[string]interface{}{
				"completion":    snap.Job.Progress,
				"printTime":     snap.Job.ElapsedSec,
				"printTimeLeft": snap.Job.ETASec,
			}
		}
		writeJSON(w, 200, resp)
	case http.MethodPost:
		if !s.checkMutation(w, r, true) {
			return
		}
		var body struct {
			Command string `json:"command"`
			Action  string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var err error
		switch body.Command {
		case "cancel":
			err = s.Mgr.Cancel()
		case "pause":
			switch body.Action {
			case "resume":
				err = s.Mgr.Resume()
			default:
				err = s.Mgr.Pause()
			}
		case "start":
			// no-op: OrcaSlicer starts via the upload's print=true instead
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		noContent(w)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleFilesLocal implements GET/POST /api/files/local, the endpoint
// OrcaSlicer's OctoPrint uploader hits with a multipart "file" field plus
// optional "print"/"select" fields.
func (s *Server) handleFilesLocal(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		files := s.Mgr.SDFiles()
		out := make([]map[string]interface{}, 0, len(files))
		for _, f := range files {
			name := f.Long
			if name == "" {
				name = f.Short
			}
			out = append(out, map[string]interface{}{
				"name":   name,
				"origin": "local",
				"size":   f.Bytes,
				"type":   "machinecode",
			})
		}
		writeJSON(w, 200, map[string]interface{}{"files": out})
		return
	}
	if !s.checkMutation(w, r, false) {
		return
	}
	s.handleUploadCommon(w, r, r.URL.Query().Get("mode"))
}

// --- gonkd's own API ---

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.Mgr.Snapshot())
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	if !s.checkMutation(w, r, true) {
		return
	}
	var body struct {
		Cmd string `json:"cmd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Cmd == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.Mgr.Send(body.Cmd); err != nil {
		writeErr(w, err)
		return
	}
	noContent(w)
}

func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{"lines": s.Mgr.Console()})
}

func (s *Server) handleSDFiles(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("refresh") == "1" {
		if err := s.Mgr.RefreshFiles(); err != nil {
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, 200, map[string]interface{}{"files": s.Mgr.SDFiles()})
}

func (s *Server) handleSDDelete(w http.ResponseWriter, r *http.Request) {
	if !s.checkMutation(w, r, true) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.Mgr.DeleteFile(strings.ToUpper(body.Name)); err != nil {
		writeErr(w, err)
		return
	}
	noContent(w)
}

func (s *Server) handleSDRename(w http.ResponseWriter, r *http.Request) {
	if !s.checkMutation(w, r, true) {
		return
	}
	var body struct {
		Name string `json:"name"`
		Long string `json:"long"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" || body.Long == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.Mgr.RenameFile(body.Name, body.Long); err != nil {
		writeErr(w, err)
		return
	}
	noContent(w)
}

// handleSDThumb implements GET /gonkd/files/thumb?name=SHORT: the stored
// thumbnail PNG for one SD file, or 404 if none was parsed for it.
func (s *Server) handleSDThumb(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	short := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("name")))
	path, ok := s.Mgr.ThumbPath(short)
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	// no-cache: a new upload can reuse the same 8.3 name.
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, short+".png", time.Time{}, bytes.NewReader(data))
}

func (s *Server) handleJobPause(w http.ResponseWriter, r *http.Request) {
	if !s.checkMutation(w, r, false) {
		return
	}
	if err := s.Mgr.Pause(); err != nil {
		writeErr(w, err)
		return
	}
	noContent(w)
}

func (s *Server) handleJobResume(w http.ResponseWriter, r *http.Request) {
	if !s.checkMutation(w, r, false) {
		return
	}
	if err := s.Mgr.Resume(); err != nil {
		writeErr(w, err)
		return
	}
	noContent(w)
}

func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	if !s.checkMutation(w, r, false) {
		return
	}
	if err := s.Mgr.Cancel(); err != nil {
		writeErr(w, err)
		return
	}
	noContent(w)
}

func (s *Server) handleJobPrint(w http.ResponseWriter, r *http.Request) {
	if !s.checkMutation(w, r, true) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.Mgr.StartSDPrint(strings.ToUpper(body.Name)); err != nil {
		writeErr(w, err)
		return
	}
	noContent(w)
}

func (s *Server) handleEmergency(w http.ResponseWriter, r *http.Request) {
	if !s.checkMutation(w, r, false) {
		return
	}
	if err := s.Mgr.Emergency(); err != nil {
		writeErr(w, err)
		return
	}
	noContent(w)
}

func (s *Server) handleBabystep(w http.ResponseWriter, r *http.Request) {
	if !s.checkMutation(w, r, true) {
		return
	}
	var body struct {
		DeltaMM float64 `json:"deltaMm"`
		Save    bool    `json:"save"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.DeltaMM != 0 {
		if err := s.Mgr.Babystep(body.DeltaMM); err != nil {
			writeErr(w, err)
			return
		}
	}
	if body.Save {
		if err := s.Mgr.SaveSettings(); err != nil {
			writeErr(w, err)
			return
		}
	}
	noContent(w)
}

// handleJog implements POST /gonkd/jog: a single validated, ordered
// G91/G1/G90 sequence, replacing the UI's old three independent requests
// (SEC-8) whose ordering across separate HTTP connections was never
// guaranteed.
func (s *Server) handleJog(w http.ResponseWriter, r *http.Request) {
	if !s.checkMutation(w, r, true) {
		return
	}
	var body struct {
		Axis string  `json:"axis"`
		Dist float64 `json:"dist"`
		Feed float64 `json:"feed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// R-05: a move must not land while a job owns the port or the head
	// mid-print; babystep/pause/resume/cancel/E-stop go through other
	// routes and are unaffected.
	switch s.Mgr.Snapshot().State {
	case printer.StateUploading, printer.StatePrinting, printer.StatePaused:
		http.Error(w, "move disabled while a job is active", http.StatusConflict)
		return
	}
	axis := strings.ToUpper(strings.TrimSpace(body.Axis))
	if axis != "X" && axis != "Y" && axis != "Z" && axis != "E" {
		http.Error(w, "axis must be one of X, Y, Z, E", http.StatusBadRequest)
		return
	}
	limit := 100.0
	if axis == "E" {
		limit = 50.0
	}
	dist := clamp(body.Dist, -limit, limit)
	feed := body.Feed
	if feed <= 0 {
		feed = 3000
		if axis == "E" {
			feed = 300
		}
	}
	feed = clamp(feed, 1, 10000)

	cmds := []string{"G91", fmt.Sprintf("G1 %s%.4f F%.0f", axis, dist, feed), "G90"}
	if err := s.Mgr.SendSequence(cmds); err != nil {
		writeErr(w, err)
		return
	}
	noContent(w)
}

// handleTune implements POST /gonkd/tune {"speed"?,"flow"?,"fan"?}: live
// M220/M221/M106-M107 overrides. Each field is optional; an out-of-range
// value is rejected (400) without touching the others.
func (s *Server) handleTune(w http.ResponseWriter, r *http.Request) {
	if !s.checkMutation(w, r, true) {
		return
	}
	var body struct {
		Speed *int `json:"speed"`
		Flow  *int `json:"flow"`
		Fan   *int `json:"fan"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.Speed != nil && (*body.Speed < 10 || *body.Speed > 500) {
		http.Error(w, "speed must be 10-500", http.StatusBadRequest)
		return
	}
	if body.Flow != nil && (*body.Flow < 50 || *body.Flow > 200) {
		http.Error(w, "flow must be 50-200", http.StatusBadRequest)
		return
	}
	if body.Fan != nil && (*body.Fan < 0 || *body.Fan > 100) {
		http.Error(w, "fan must be 0-100", http.StatusBadRequest)
		return
	}
	if err := s.Mgr.Tune(body.Speed, body.Flow, body.Fan); err != nil {
		writeErr(w, err)
		return
	}
	noContent(w)
}

// handleHeat implements POST /gonkd/heat {"hotend"?,"bed"?}: M104/M140, no
// wait. 0 turns a heater off; sending both 0 is a full cooldown. Heater
// commands are only ever sent in response to this explicit call.
func (s *Server) handleHeat(w http.ResponseWriter, r *http.Request) {
	if !s.checkMutation(w, r, true) {
		return
	}
	var body struct {
		Hotend *float64 `json:"hotend"`
		Bed    *float64 `json:"bed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.Hotend != nil && (*body.Hotend < 0 || *body.Hotend > 260) {
		http.Error(w, "hotend must be 0-260", http.StatusBadRequest)
		return
	}
	if body.Bed != nil && (*body.Bed < 0 || *body.Bed > 110) {
		http.Error(w, "bed must be 0-110", http.StatusBadRequest)
		return
	}
	if err := s.Mgr.Heat(body.Hotend, body.Bed); err != nil {
		writeErr(w, err)
		return
	}
	noContent(w)
}

// handlePresets implements GET/PUT /gonkd/presets.
func (s *Server) handlePresets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.Mgr.Presets())
	case http.MethodPut:
		if !s.checkMutationMethod(w, r, http.MethodPut, true) {
			return
		}
		var list []printer.Preset
		if err := json.NewDecoder(r.Body).Decode(&list); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := s.Mgr.SetPresets(list); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, s.Mgr.Presets())
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPut)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// handleUploadCommon streams the multipart "file" part straight into a
// staging file under DataDir (SEC-4, SEC-26), enforcing MaxBody as it goes,
// then hands the staged path to Manager.Upload. Upload is async: it returns
// once the request is validated (connected, no active job) and the transfer
// itself runs in the background, so the HTTP request completes quickly and
// failures during the actual transfer surface via /gonkd/status.lastError
// (G5).
func (s *Server) handleUploadCommon(w http.ResponseWriter, r *http.Request, modeParam string) {
	mode := printer.UploadMode(modeParam)
	if mode == "" {
		mode = s.Mode
	}
	if mode != printer.ModeSDUpload && mode != printer.ModeStream {
		http.Error(w, fmt.Sprintf("unknown upload mode %q", modeParam), http.StatusBadRequest)
		return
	}
	if mode == printer.ModeStream && !s.AllowStream {
		http.Error(w, "stream mode disabled", http.StatusBadRequest)
		return
	}

	capBytes := s.MaxBody // <= 0 means "no cap", handled consistently below
	if capBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, capBytes+64*1024)
	}

	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "expected multipart/form-data", http.StatusBadRequest)
		return
	}

	if err := os.MkdirAll(s.DataDir, 0755); err != nil {
		http.Error(w, "server storage error", http.StatusInternalServerError)
		return
	}
	// R-06: the old check demanded the *whole* MaxBody free, regardless of
	// how big this upload actually is, so a small upload could be rejected
	// on a mostly-full tmpfs. Use the real request size when we have one
	// (Content-Length; Orca sends it, and it already includes the small
	// multipart overhead, so it is a safe overestimate of the file size),
	// capped at MaxBody since that is all this upload can ever write. When
	// the size isn't known up front, fall back to a small startup margin and
	// keep checking free space as bytes actually land on disk (spaceGuard).
	const spaceMargin int64 = 2 << 20 // 2 MiB headroom kept free
	needed := spaceMargin
	if r.ContentLength > 0 {
		want := r.ContentLength
		if capBytes > 0 && want > capBytes {
			want = capBytes
		}
		needed = want + spaceMargin
	}
	if free, err := freeSpace(s.DataDir); err != nil || free < needed {
		http.Error(w, "insufficient space in data dir", http.StatusInsufficientStorage)
		return
	}

	var (
		stagePath string
		longName  string
		written   int64
		gotFile   bool
		print     bool
	)
	cleanup := func() {
		if stagePath != "" {
			os.Remove(stagePath)
		}
	}

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			cleanup()
			// http.MaxBytesReader surfaces overflow here as a body-read error.
			http.Error(w, "upload read error", http.StatusRequestEntityTooLarge)
			return
		}
		name := part.FormName()
		switch {
		case name == "file" && part.FileName() != "":
			if gotFile {
				part.Close()
				continue // ignore extra file parts
			}
			gotFile = true
			longName = part.FileName()
			stagePath = filepath.Join(s.DataDir, fmt.Sprintf("upload-%d-%s", time.Now().UnixNano(), sanitizeStageName(longName)))
			out, err := os.Create(stagePath)
			if err != nil {
				part.Close()
				http.Error(w, "server storage error", http.StatusInternalServerError)
				return
			}
			// When the request had no usable Content-Length, the up-front
			// check above only guarantees a small margin; keep checking
			// actual free space against the running byte count as the file
			// streams in, so a large upload on a nearly-full tmpfs fails
			// fast instead of filling it (R-06).
			src := io.Reader(part)
			if r.ContentLength <= 0 {
				src = &spaceCheckReader{r: part, dir: s.DataDir, margin: spaceMargin, every: 4 << 20}
			}
			written, err = copyLimited(out, src, capBytes)
			out.Close()
			part.Close()
			if err != nil {
				cleanup()
				switch err {
				case errTooLarge:
					http.Error(w, fmt.Sprintf("file too large (cap %d bytes)", capBytes), http.StatusRequestEntityTooLarge)
				case errInsufficientSpace:
					http.Error(w, "insufficient space in data dir", http.StatusInsufficientStorage)
				default:
					http.Error(w, "upload read error", http.StatusInternalServerError)
				}
				return
			}
		case name == "print":
			v, _ := readSmallField(part)
			print = v == "true" || v == "1"
			part.Close()
		default:
			// "path", "select", "plateindex", anything else: drain and ignore.
			io.Copy(io.Discard, part)
			part.Close()
		}
	}

	if !gotFile {
		cleanup()
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}

	if err := s.Mgr.Upload(stagePath, longName, mode, print); err != nil {
		cleanup()
		writeErr(w, err)
		return
	}

	log.Printf("gonkd: upload staged %q (%d bytes) mode=%s print=%v", longName, written, mode, print)
	writeJSON(w, 201, map[string]interface{}{
		"done": true,
		"files": map[string]interface{}{
			"local": map[string]interface{}{"name": longName, "origin": "local"},
		},
	})
}

var errTooLarge = errors.New("upload exceeds cap")
var errInsufficientSpace = errors.New("insufficient space in data dir")

// spaceCheckReader re-checks free disk space every `every` bytes read,
// failing closed (a statfs error counts as "unknown, reject") rather than
// only trusting a single check made before the transfer started (R-06).
type spaceCheckReader struct {
	r         io.Reader
	dir       string
	margin    int64
	every     int64
	read      int64
	lastCheck int64
}

func (s *spaceCheckReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.read += int64(n)
		if s.read-s.lastCheck >= s.every {
			s.lastCheck = s.read
			free, ferr := freeSpace(s.dir)
			if ferr != nil || free < s.margin {
				return n, errInsufficientSpace
			}
		}
	}
	return n, err
}

// copyLimited copies src into dst, enforcing cap bytes when cap > 0 (cap <=
// 0 means no cap, consistent with Server.MaxBody). Returns errTooLarge on
// overflow without leaving the caller guessing from a generic io error.
func copyLimited(dst io.Writer, src io.Reader, cap int64) (int64, error) {
	if cap <= 0 {
		return io.Copy(dst, src)
	}
	n, err := io.Copy(dst, io.LimitReader(src, cap+1))
	if err != nil {
		return n, err
	}
	if n > cap {
		return n, errTooLarge
	}
	return n, nil
}

func readSmallField(p *multipart.Part) (string, error) {
	b, err := io.ReadAll(io.LimitReader(p, 4096))
	return strings.TrimSpace(string(b)), err
}

// stageNameMax caps the sanitized name so "upload-<nanos>-<name>" stays well
// under common filesystem NAME_MAX (255 bytes), even for multi-byte UTF-8
// names (R-20).
const stageNameMax = 120

func sanitizeStageName(name string) string {
	name = filepath.Base(name)
	var b strings.Builder
	for _, r := range name {
		if r == '/' || r == '\\' || r == 0 {
			continue
		}
		if b.Len()+len(string(r)) > stageNameMax {
			break
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "upload.gcode"
	}
	return b.String()
}

func freeSpace(dir string) (int64, error) {
	return statfsFree(dir)
}
