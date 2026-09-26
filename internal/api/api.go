// Package api implements forge's HTTP surface: a small OctoPrint-compatible
// subset (enough for OrcaSlicer's "Octo/Klipper" host type to test the
// connection and do one-click upload+print) plus forge's own JSON API used
// by the embedded UI.
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"forge/internal/printer"
)

const forgeVersion = "0.1.0"

// Server wires the Manager to HTTP handlers.
type Server struct {
	Mgr     *printer.Manager
	DataDir string // for staging uploads before transfer
	MaxBody int64  // upload size cap in bytes
	Mode    printer.UploadMode // default upload mode (sd or stream)
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
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

	// --- forge's own API ---
	mux.HandleFunc("/forge/status", s.handleStatus)
	mux.HandleFunc("/forge/send", s.handleSend)
	mux.HandleFunc("/forge/console", s.handleConsole)
	mux.HandleFunc("/forge/files", s.handleSDFiles)
	mux.HandleFunc("/forge/files/delete", s.handleSDDelete)
	mux.HandleFunc("/forge/job/pause", s.handleJobPause)
	mux.HandleFunc("/forge/job/resume", s.handleJobResume)
	mux.HandleFunc("/forge/job/cancel", s.handleJobCancel)
	mux.HandleFunc("/forge/job/print", s.handleJobPrint)
	mux.HandleFunc("/forge/emergency", s.handleEmergency)
	mux.HandleFunc("/forge/babystep", s.handleBabystep)
}

// --- OctoPrint subset ---
// OrcaSlicer's OctoPrint host probes GET /api/version to confirm the target
// looks like OctoPrint (it checks the response has "api"/"server" fields),
// then uses POST /api/files/local with multipart fields file/print/select
// to upload+print, and polls /api/job + /api/printer for status. Accepting
// any (or no) X-Api-Key matches the task's "accept any" requirement, and
// keeps setup friction near zero on a LAN-only box.

type versionResp struct {
	API    string `json:"api"`
	Server string `json:"server"`
	Text   string `json:"text"`
}

func (s *Server) handleAPIVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, versionResp{API: "0.1", Server: forgeVersion, Text: "forge " + forgeVersion})
}

func (s *Server) handleAPIServer(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{
		"version": forgeVersion,
		"safemode": nil,
	})
}

func (s *Server) handleConnection(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		// OrcaSlicer may POST to (re)connect; forge is always connected
		// once the service is up, so just acknowledge.
		writeJSON(w, 204, nil)
		return
	}
	state := "Operational"
	switch s.Mgr.State() {
	case printer.StatePrinting:
		state = "Printing"
	case printer.StatePaused:
		state = "Paused"
	case printer.StateError, printer.StateDisconnected:
		state = "Closed"
	}
	writeJSON(w, 200, map[string]interface{}{
		"current": map[string]interface{}{
			"state":       state,
			"port":        "/dev/ttyUSB0",
			"baudrate":    250000,
			"printerProfile": "default",
		},
		"options": map[string]interface{}{
			"ports":     []string{"/dev/ttyUSB0"},
			"baudrates": []int{250000},
		},
	})
}

func (s *Server) handlePrinterState(w http.ResponseWriter, r *http.Request) {
	temps := s.Mgr.Driver.Temps()
	snap := s.Mgr.Snapshot()
	flags := map[string]bool{
		"operational": true,
		"paused":      snap.State == printer.StatePaused,
		"printing":    snap.State == printer.StatePrinting,
		"cancelling":  snap.State == printer.StateCancelling,
		"pausing":     false,
		"error":       snap.State == printer.StateError,
		"ready":       snap.State == printer.StateIdle,
		"closedOrError": snap.State == printer.StateDisconnected,
	}
	writeJSON(w, 200, map[string]interface{}{
		"state": map[string]interface{}{
			"text":  string(snap.State),
			"flags": flags,
		},
		"temperature": map[string]interface{}{
			"tool0": map[string]interface{}{"actual": temps.HotendActual, "target": temps.HotendTarget, "offset": 0},
			"bed":   map[string]interface{}{"actual": temps.BedActual, "target": temps.BedTarget, "offset": 0},
		},
	})
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		snap := s.Mgr.Snapshot()
		resp := map[string]interface{}{
			"job": map[string]interface{}{"file": map[string]interface{}{}},
			"progress": map[string]interface{}{
				"completion":  0.0,
				"printTime":   0,
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
		var body struct {
			Command string `json:"command"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body.Command {
		case "cancel":
			s.cancelActive()
		case "pause":
			s.pauseActive()
		case "start":
			// no-op: OrcaSlicer starts via the upload's print=true instead
		}
		writeJSON(w, 204, nil)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) pauseActive() {
	if s.Mgr.State() == printer.StatePaused {
		if s.Mode == printer.ModeStream {
			s.Mgr.ResumeStream()
		} else {
			s.Mgr.ResumeSD()
		}
		return
	}
	if s.Mode == printer.ModeStream {
		s.Mgr.PauseStream()
	} else {
		s.Mgr.PauseSD()
	}
}

func (s *Server) cancelActive() {
	if s.Mode == printer.ModeStream {
		s.Mgr.CancelStream()
	} else {
		s.Mgr.CancelSD()
	}
}

// handleFilesLocal implements POST /api/files/local, the endpoint
// OrcaSlicer's OctoPrint uploader hits with a multipart "file" field plus
// optional "print"/"select" fields.
func (s *Server) handleFilesLocal(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]interface{}{"files": []interface{}{}})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.handleUploadCommon(w, r, r.URL.Query().Get("mode"))
}

// --- forge's own API ---

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.Mgr.Snapshot())
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct{ Cmd string `json:"cmd"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Cmd == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.Mgr.Driver.Send(body.Cmd)
	writeJSON(w, 204, nil)
}

func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{"lines": s.Mgr.Driver.Console()})
}

func (s *Server) handleSDFiles(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("refresh") == "1" {
		s.Mgr.Driver.Send("M20 L")
	}
	writeJSON(w, 200, map[string]interface{}{"files": s.Mgr.Driver.SDFiles()})
}

func (s *Server) handleSDDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct{ Name string `json:"name"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.Mgr.Driver.Send(fmt.Sprintf("M30 %s", strings.ToUpper(body.Name)))
	s.Mgr.Names.Forget(strings.ToUpper(body.Name))
	_ = s.Mgr.Names.Save()
	writeJSON(w, 204, nil)
}

func (s *Server) handleJobPause(w http.ResponseWriter, r *http.Request)  { s.pauseActive(); writeJSON(w, 204, nil) }
func (s *Server) handleJobResume(w http.ResponseWriter, r *http.Request) { s.pauseActive(); writeJSON(w, 204, nil) }
func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request) { s.cancelActive(); writeJSON(w, 204, nil) }

func (s *Server) handleJobPrint(w http.ResponseWriter, r *http.Request) {
	s.handleUploadCommon(w, r, string(s.Mode))
}

func (s *Server) handleEmergency(w http.ResponseWriter, r *http.Request) {
	s.Mgr.EmergencyStop()
	writeJSON(w, 204, nil)
}

func (s *Server) handleBabystep(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeltaMM float64 `json:"deltaMm"`
		Save    bool    `json:"save"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.DeltaMM != 0 {
		// M290 Z<mm>: Marlin interprets this in mm directly.
		s.Mgr.Driver.Send(fmt.Sprintf("M290 Z%.3f", body.DeltaMM))
	}
	if body.Save {
		s.Mgr.Driver.Send("M500")
	}
	writeJSON(w, 204, nil)
}

// handleUploadCommon does the multipart parse + staging-to-DataDir +
// Manager dispatch shared by both the OctoPrint endpoint and forge's own.
func (s *Server) handleUploadCommon(w http.ResponseWriter, r *http.Request, modeParam string) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		// Fall back to a larger in-memory threshold only if needed; actual
		// file bytes still stream to disk via the standard library.
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	if s.MaxBody > 0 && header.Size > s.MaxBody {
		http.Error(w, fmt.Sprintf("file too large: %d bytes (cap %d)", header.Size, s.MaxBody), http.StatusRequestEntityTooLarge)
		return
	}
	if free, err := freeSpace(s.DataDir); err == nil && header.Size > free {
		http.Error(w, "insufficient space in data dir", http.StatusInsufficientStorage)
		return
	}

	if err := os.MkdirAll(s.DataDir, 0755); err != nil {
		http.Error(w, "server storage error", http.StatusInternalServerError)
		return
	}
	longName := header.Filename
	stagePath := filepath.Join(s.DataDir, fmt.Sprintf("upload-%d-%s", time.Now().UnixNano(), sanitizeStageName(longName)))
	out, err := os.Create(stagePath)
	if err != nil {
		http.Error(w, "server storage error", http.StatusInternalServerError)
		return
	}
	written, err := io.Copy(out, io.LimitReader(file, s.MaxBody+1))
	out.Close()
	if err != nil {
		os.Remove(stagePath)
		http.Error(w, "upload read error", http.StatusInternalServerError)
		return
	}
	if s.MaxBody > 0 && written > s.MaxBody {
		os.Remove(stagePath)
		http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
		return
	}

	print := r.FormValue("print") == "true" || r.FormValue("print") == "1"
	mode := printer.UploadMode(modeParam)
	if mode == "" {
		mode = s.Mode
	}

	var short string
	if mode == printer.ModeStream {
		if err := s.Mgr.StreamPrint(stagePath, longName); err != nil {
			os.Remove(stagePath)
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	} else {
		short, err = s.Mgr.UploadAndMaybePrint(stagePath, longName, print)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	log.Printf("forge: uploaded %q (%d bytes) as %q mode=%s print=%v", longName, written, short, mode, print)
	writeJSON(w, 201, map[string]interface{}{
		"done": true,
		"files": map[string]interface{}{
			"local": map[string]interface{}{"name": short, "origin": "local"},
		},
	})
}

func sanitizeStageName(name string) string {
	name = filepath.Base(name)
	var b strings.Builder
	for _, r := range name {
		if r == '/' || r == '\\' || r == 0 {
			continue
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
