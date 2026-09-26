package gcode

import "sync"

// EmergencyCommands are sent raw, immediately, bypassing line numbering and
// the send window entirely, because Marlin's EMERGENCY_PARSER intercepts
// them out-of-band from the normal command stream.
var EmergencyCommands = map[string]bool{
	"M112": true, // emergency stop
	"M108": true, // break out of heat-and-wait
	"M410": true, // quickstop
	"M876": true, // handle prompt response
}

// IsEmergency reports whether cmd (already trimmed, first word compared
// case-sensitively as Marlin expects G-code words) is an EMERGENCY_PARSER
// command that must skip the queue.
func IsEmergency(cmd string) bool {
	word := firstWord(cmd)
	return EmergencyCommands[word]
}

func firstWord(s string) string {
	i := 0
	for i < len(s) && s[i] != ' ' && s[i] != '\t' {
		i++
	}
	return s[:i]
}

// Sent is a line that has been written to the wire and is awaiting "ok".
type Sent struct {
	N   int64
	Cmd string
}

// Window implements Marlin's line-numbered send-ahead protocol:
//   - Every non-emergency command gets a monotonically increasing N.
//   - Up to `capacity` (bounded by ADVANCED_OK's B, and BUFSIZE as fallback)
//     lines may be outstanding (sent, not yet ok'd) at once, keeping
//     Marlin's serial/planner buffers full without overrunning them.
//   - On Resend:<n>, every outstanding line from n onward is replayed.
//
// Window is safe for single-goroutine use; the printer driver owns it.
type Window struct {
	mu       sync.Mutex
	nextN    int64
	capacity int // default outstanding-line budget, e.g. BUFSIZE=16
	buffer   int // last-seen ADVANCED_OK "B" (serial RX free slots), -1 = unknown
	planner  int // last-seen ADVANCED_OK "P" (planner free slots), -1 = unknown
	outstand []Sent
}

// NewWindow creates a Window. capacity should be Marlin's BUFSIZE (16 for
// this build) as a safe default before any ADVANCED_OK has been seen.
func NewWindow(capacity int) *Window {
	return &Window{
		nextN:    1,
		capacity: capacity,
		buffer:   -1,
		planner:  -1,
	}
}

// Reset clears all outstanding state, e.g. after a board reset + M110 N0.
func (w *Window) Reset(startN int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.nextN = startN
	w.outstand = nil
	w.buffer = -1
	w.planner = -1
}

// CanSend reports whether the window has room for one more outstanding line.
func (w *Window) CanSend() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.canSendLocked()
}

func (w *Window) canSendLocked() bool {
	return len(w.outstand) < w.capacity
}

// Prepare assigns the next line number and framed text for cmd, and records
// it as outstanding. Caller must actually write the returned text to the
// wire only when CanSend() was true.
func (w *Window) Prepare(cmd string) (n int64, framed string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n = w.nextN
	w.nextN++
	framed = FrameLine(n, cmd)
	w.outstand = append(w.outstand, Sent{N: n, Cmd: cmd})
	return n, framed
}

// Ack removes the acknowledged line (and any earlier ones, which Marlin
// acks in order) from the outstanding set, and records any ADVANCED_OK
// buffer/planner counts.
func (w *Window) Ack(r Response) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if r.HasAdv {
		w.buffer = r.Buffer
		w.planner = r.Planner
	}
	if r.OKLine == 0 {
		// Plain "ok" with no line number: acks the oldest outstanding line.
		if len(w.outstand) > 0 {
			w.outstand = w.outstand[1:]
		}
		return
	}
	// Drop every outstanding entry with N <= r.OKLine.
	i := 0
	for ; i < len(w.outstand); i++ {
		if w.outstand[i].N > r.OKLine {
			break
		}
	}
	w.outstand = w.outstand[i:]
}

// Resend returns the outstanding lines from n onward, in order, to be
// retransmitted verbatim (same line numbers, same checksum) after a
// "Resend:<n>" from Marlin. It does not remove them from the outstanding
// set -- they remain outstanding until acked normally.
func (w *Window) Resend(n int64) []Sent {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]Sent, 0, len(w.outstand))
	for _, s := range w.outstand {
		if s.N >= n {
			out = append(out, s)
		}
	}
	return out
}

// Outstanding returns the count of unacknowledged lines.
func (w *Window) Outstanding() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.outstand)
}

// LastBuffer returns the most recent ADVANCED_OK B value, or -1 if unknown.
func (w *Window) LastBuffer() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer
}
