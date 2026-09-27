package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Xoltox/gonkd/internal/printer"
)

// maxSSEClients caps concurrent /gonkd/events connections; the 9th gets 503
// rather than gonkd holding an unbounded number of open response writers
// (each one is a goroutine plus a small buffer) on a 128MB box.
const maxSSEClients = 8

// sseClientBuf is the per-client outgoing event buffer. A slow client (a
// phone that locked its screen mid-stream) has its oldest queued event
// dropped rather than ever blocking the sender.
const sseClientBuf = 8

// statusPollInterval is how often the status pump checks Manager.Snapshot
// for a change. It, together with statusHeartbeat, implements the
// contract's "throttled to at most 2/s, and at least every 5s".
const statusPollInterval = 500 * time.Millisecond
const statusHeartbeat = 5 * time.Second

// consolePollInterval is how often the console pump checks for new lines.
const consolePollInterval = 150 * time.Millisecond

// pingInterval is the SSE comment ping sent to keep idle connections (and
// any intermediate proxy) alive.
const pingInterval = 15 * time.Second

type sseEvent struct {
	event string
	data  []byte
}

// frame renders the event in text/event-stream wire format.
func (e sseEvent) frame() []byte {
	var b bytes.Buffer
	b.WriteString("event: ")
	b.WriteString(e.event)
	b.WriteByte('\n')
	for _, line := range strings.Split(string(e.data), "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.Bytes()
}

type sseClient struct {
	ch chan sseEvent
}

// hub fans out status/console events to every connected /gonkd/events
// client. broadcast is non-blocking by construction (buffered channel,
// drop-oldest-then-push on overflow), so it is always safe to call from a
// polling goroutine without risking a stall anywhere upstream (the driver
// and Manager never call into it directly - see runStatusPump/
// runConsolePump).
type hub struct {
	mu      sync.Mutex
	clients map[*sseClient]struct{}
}

func newHub() *hub {
	return &hub{clients: map[*sseClient]struct{}{}}
}

// add registers a new client, or refuses it (false) once maxSSEClients are
// already connected.
func (h *hub) add() (*sseClient, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) >= maxSSEClients {
		return nil, false
	}
	c := &sseClient{ch: make(chan sseEvent, sseClientBuf)}
	h.clients[c] = struct{}{}
	return c, true
}

func (h *hub) remove(c *sseClient) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// broadcast delivers ev to every client, never blocking: a full client
// buffer has its oldest event dropped to make room.
func (h *hub) broadcast(ev sseEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c.ch <- ev:
		default:
			select {
			case <-c.ch:
			default:
			}
			select {
			case c.ch <- ev:
			default:
			}
		}
	}
}

func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// getHub lazily initializes the server's event hub, so a zero-value Server
// (as tests build with a struct literal) works without an explicit
// constructor call.
func (s *Server) getHub() *hub {
	s.hubOnce.Do(func() { s.hub = newHub() })
	return s.hub
}

// handleEvents implements GET /gonkd/events: a Snapshot on connect and on
// every change (throttled), a console line on every new console line, and a
// ": ping" comment every 15s so idle connections (and any proxy in
// between) stay open.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	client, ok := s.getHub().add()
	if !ok {
		http.Error(w, "too many event stream clients", http.StatusServiceUnavailable)
		return
	}
	defer s.getHub().remove(client)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	writeSSEStatus(w, s.Mgr.Snapshot())
	flusher.Flush()

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-client.ch:
			if _, err := w.Write(ev.frame()); err != nil {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeSSEStatus(w io.Writer, snap printer.Snapshot) {
	b, err := json.Marshal(snap)
	if err != nil {
		return
	}
	w.Write(sseEvent{event: "status", data: b}.frame())
}

type consoleLineEvent struct {
	Line string `json:"line"`
	TS   int64  `json:"ts"`
}

// RunEvents drives the status and console SSE feeds until ctx is done. Call
// it once, in its own goroutine, after Routes.
func (s *Server) RunEvents(ctx context.Context) {
	go s.runStatusPump(ctx)
	go s.runConsolePump(ctx)
}

// runStatusPump polls Manager.Snapshot at statusPollInterval and broadcasts
// it whenever it changed, or at least every statusHeartbeat regardless.
// Polling (rather than a callback hook into Manager) means this can never
// block the driver or Manager: the worst case is a slightly stale event.
func (s *Server) runStatusPump(ctx context.Context) {
	t := time.NewTicker(statusPollInterval)
	defer t.Stop()
	var last []byte
	var lastSent time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		b, err := json.Marshal(s.Mgr.Snapshot())
		if err != nil {
			continue
		}
		if bytes.Equal(b, last) && time.Since(lastSent) < statusHeartbeat {
			continue
		}
		last = b
		lastSent = time.Now()
		s.getHub().broadcast(sseEvent{event: "status", data: b})
	}
}

// runConsolePump polls Manager.ConsoleSince at consolePollInterval and
// broadcasts each new line as its own event, oldest first.
func (s *Server) runConsolePump(ctx context.Context) {
	t := time.NewTicker(consolePollInterval)
	defer t.Stop()
	var since int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		lines, next := s.Mgr.ConsoleSince(since)
		since = next
		for _, line := range lines {
			b, err := json.Marshal(consoleLineEvent{Line: line, TS: time.Now().UnixMilli()})
			if err != nil {
				continue
			}
			s.getHub().broadcast(sseEvent{event: "console", data: b})
		}
	}
}
