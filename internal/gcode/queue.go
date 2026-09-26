package gcode

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

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

// resendIgnoreFor bounds the duplicate-resend budget in time. Marlin's
// gcode_line_error drains its RX buffer on every error, so the lines behind
// a bad one do not reliably draw their own "Resend: n"; a budget left
// standing would swallow a genuine request (a lost or eaten replay).
const resendIgnoreFor = 2 * time.Second

// Window implements Marlin's line-numbered send-ahead protocol:
//   - Every non-emergency command gets a monotonically increasing N.
//   - Up to `capacity` (Marlin's BUFSIZE) lines may be outstanding (sent,
//     not yet ok'd) at once, keeping Marlin's command buffer full without
//     overrunning it.
//   - On Resend:<n>, every outstanding line from n onward is replayed.
//   - An M110 is a barrier: Marlin sets its line counter from M110 both on
//     receipt and again when the queued command executes, so a numbered
//     line received in between would be rewound (and a later replay could
//     run twice). Nothing else is written until the M110's ok arrives.
//
// Window is safe for concurrent use (the driver's read and pump goroutines
// both touch it); every method takes the internal mutex.
type Window struct {
	mu       sync.Mutex
	nextN    int64
	capacity int
	outstand []Sent

	// seenN is set once any "ok N<line>" arrived: from then on an ok
	// without N is not an ack of a numbered line (see Ack).
	seenN bool
	// skipBare swallows the bare "ok" Marlin prints right after
	// "Resend: <n>" (queue.cpp flush_and_request_resend).
	skipBare bool
	// Duplicate resend suppression: lines already in flight behind a
	// corrupted one may draw their own "Resend: <n>" for the same n.
	// Replaying on each of them snowballs, so for a short time after one
	// replay up to resendIgnore further requests for the same n are ignored.
	resendN      int64
	resendIgnore int
	resendAt     time.Time

	// barrier: outstand[0] is an M110 whose ok must arrive before any
	// other line is written. heldUntil > 0: the lines after it up to that
	// N were not written yet (stale-resend resync) and are released into
	// replay by the M110's ack, for the driver to write (TakeReplay).
	barrier   bool
	heldUntil int64
	replay    []Sent

	// progress is when the oldest outstanding line last changed (an ack,
	// a line sent into an empty window, a replay, or Touch).
	progress time.Time
}

// NewWindow creates a Window. capacity should be Marlin's BUFSIZE (16 for
// this build).
func NewWindow(capacity int) *Window {
	return &Window{nextN: 1, capacity: capacity}
}

// Reset clears all outstanding state, e.g. after a board reset + M110 N0.
// The next Prepare gets line number startN.
func (w *Window) Reset(startN int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.nextN = startN
	w.outstand = nil
	w.skipBare = false
	w.resendN = 0
	w.resendIgnore = 0
	w.barrier = false
	w.heldUntil = 0
	w.replay = nil
}

// CanSend reports whether a new line may be written now: the window has
// room, no M110 barrier is outstanding and no released lines are waiting
// to be replayed.
func (w *Window) CanSend() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.outstand) < w.capacity && !w.barrier && len(w.replay) == 0
}

// NextN returns the line number the next Prepare will use.
func (w *Window) NextN() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.nextN
}

// Prepare assigns the next line number and framed text for cmd, and records
// it as outstanding. Caller must actually write the returned text to the
// wire only when CanSend() was true.
func (w *Window) Prepare(cmd string) (n int64, framed string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.prepareLocked(cmd)
}

func (w *Window) prepareLocked(cmd string) (int64, string) {
	n := w.nextN
	w.nextN++
	if len(w.outstand) == 0 {
		w.progress = time.Now()
	}
	w.outstand = append(w.outstand, Sent{N: n, Cmd: cmd})
	return n, FrameLine(n, cmd)
}

// PrepareBarrier is Prepare for an M110 on an empty window (right after
// Reset): until its ok arrives CanSend is false.
func (w *Window) PrepareBarrier(cmd string) (n int64, framed string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, framed = w.prepareLocked(cmd)
	w.barrier = len(w.outstand) == 1
	return n, framed
}

// drop removes the k oldest outstanding lines. Caller holds w.mu.
func (w *Window) drop(k int) {
	if k <= 0 {
		return
	}
	if w.barrier {
		w.barrier = false
		for _, s := range w.outstand[k:] {
			if s.N <= w.heldUntil {
				w.replay = append(w.replay, s)
			}
		}
		w.heldUntil = 0
	}
	w.outstand = w.outstand[k:]
	w.progress = time.Now()
}

// Ack applies one "ok" line to the outstanding set.
//
// "ok N<k>" drops every entry with N <= k (Marlin acks in order, so a lost
// ok self-heals on the next one). An ok without N is handled as:
//   - the bare "ok" right after a Resend is ignored (it acks nothing);
//   - once line-numbered oks have been seen (ADVANCED_OK), every numbered
//     line's ok carries its N except M105's: it prints its own "ok T:..."
//     and skips ok_to_send (gcode.cpp). Such an ok acks the oldest
//     outstanding M105 and, since Marlin executes in order, everything
//     before it. Any other ok without N is ignored;
//   - without ADVANCED_OK it acks the oldest outstanding line.
//
// After an ack that clears a resync barrier, TakeReplay returns the held
// lines to write.
func (w *Window) Ack(r Response) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !r.HasN {
		if w.skipBare {
			w.skipBare = false
			return
		}
		if !w.seenN {
			if len(w.outstand) > 0 {
				w.drop(1)
			}
			return
		}
		if !strings.Contains(r.Line, "T:") || w.barrier {
			return
		}
		for i, s := range w.outstand {
			if firstWord(s.Cmd) == "M105" {
				w.drop(i + 1)
				return
			}
		}
		return
	}
	w.seenN = true
	i := 0
	for ; i < len(w.outstand); i++ {
		if w.outstand[i].N > r.OKLine {
			break
		}
	}
	w.drop(i)
	if w.resendIgnore > 0 && r.OKLine >= w.resendN {
		w.resendN, w.resendIgnore = 0, 0
	}
}

// TakeReplay returns (once) the lines released by the ack of a resync
// barrier. They are still outstanding; the caller writes them in order,
// holding its write lock, before any new line (CanSend stays false until
// they are taken).
func (w *Window) TakeReplay() []Sent {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := w.replay
	w.replay = nil
	return r
}

// Resend handles "Resend: <n>" and returns what to put back on the wire.
//
// Normally lines holds the outstanding lines from n onward, to be
// retransmitted verbatim (they stay outstanding until acked). If n is not a
// line this window can replay (printer reset, stale numbering) resync is
// true and lines is a single "N<k> M110 N<k>" barrier, which makes Marlin
// expect the oldest outstanding line next; the outstanding lines are held
// and released by its ack (TakeReplay). Looping on the stale request would
// never converge. While a barrier is outstanding only the barrier is
// replayed. A duplicate request for a line that was just handled returns
// nothing.
func (w *Window) Resend(n int64) (lines []Sent, resync bool, k int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.skipBare = true
	if w.barrier {
		return []Sent{w.outstand[0]}, false, 0
	}
	if len(w.replay) > 0 {
		// The released lines are about to be written anyway.
		return nil, false, 0
	}
	if n == w.resendN && w.resendIgnore > 0 && time.Since(w.resendAt) < resendIgnoreFor {
		w.resendIgnore--
		return nil, false, 0
	}
	first := w.nextN
	if len(w.outstand) > 0 {
		first = w.outstand[0].N
	}
	if n < first || n > w.nextN {
		k = first - 1
		b := Sent{N: k, Cmd: "M110 N" + strconv.FormatInt(k, 10)}
		if len(w.outstand) > 0 {
			w.heldUntil = w.outstand[len(w.outstand)-1].N
		}
		w.outstand = append([]Sent{b}, w.outstand...)
		w.barrier = true
		w.resendN, w.resendIgnore = 0, 0
		w.progress = time.Now()
		return []Sent{b}, true, k
	}
	for _, s := range w.outstand {
		if s.N >= n {
			lines = append(lines, s)
		}
	}
	w.resendN = n
	w.resendIgnore = len(lines) - 1
	if w.resendIgnore < 0 {
		w.resendIgnore = 0
	}
	w.resendAt = time.Now()
	return lines, false, 0
}

// Replay is the ack-stall recovery: it clears the duplicate-resend budget,
// restarts the stall clock and returns every outstanding line to write
// again (only the barrier while one is outstanding). Marlin silently drops
// a duplicate of its last two accepted lines and answers an older one with
// "Resend: last+1", so replaying lines it already has is harmless.
func (w *Window) Replay() []Sent {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.resendN, w.resendIgnore = 0, 0
	w.progress = time.Now()
	if w.barrier {
		return []Sent{w.outstand[0]}
	}
	if len(w.replay) > 0 {
		return nil
	}
	return append([]Sent(nil), w.outstand...)
}

// Touch restarts the stall clock (Marlin reported it is busy executing).
func (w *Window) Touch() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.progress = time.Now()
}

// Stalled reports whether lines are outstanding and the oldest one has not
// changed for at least d.
func (w *Window) Stalled(d time.Duration) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.outstand) > 0 && time.Since(w.progress) >= d
}

// Outstanding returns the count of unacknowledged lines.
func (w *Window) Outstanding() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.outstand)
}
