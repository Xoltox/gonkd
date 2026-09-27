// Package binprotocol is a host for Marlin 2.1's BINARY_FILE_TRANSFER
// protocol, written against Marlin 2.1.2.7's src/feature/binary_stream.h
// (the only reference; line numbers below are from that file) and checked
// against the reference client buildroot/share/scripts/MarlinBinaryProtocol.py.
//
// Marlin enters the protocol on "M28 B1" (gcode/sd/M28_M29.cpp) and from
// then on feeds every received byte to BinaryStream::receive instead of the
// G-code parser (gcode/queue.cpp get_serial_commands), until a CONTROL
// CLOSE packet arrives. Host to printer is binary packets; printer to host
// stays ASCII lines ("ok<n>", "rs<n>", "ss...", "fe<n>", "PFT:..."),
// interleaved with the usual autoreports, so the caller keeps its line
// reader and hands this package the reply lines.
//
// Packet (little-endian, h:209-220):
//
//	u16 token 0xB5AD (bytes AD B5, not checksummed)
//	u8  sync      stream sequence number, learned from "ss", +1 per packet
//	u8  meta      protocol<<4 | type
//	u16 size      payload length, at most the buffer size from "ss"
//	u16 header checksum: Fletcher-16 over sync, meta, size (h:314-319)
//	payload[size], then u16 payload checksum, only if size > 0: the same
//	Fletcher-16 continued over the header checksum bytes and the payload
//	(h:314-316 keep summing through the checksum bytes; h:368, 382)
//
// Marlin answers each accepted packet with "ok<sync>" before acting on it
// (h:390-397), a duplicate of the previous packet with "ok<sync>" again
// (h:339-342), a corrupt or out-of-order packet or a 500 ms stall inside
// a packet with "rs<expected sync>" (h:343-354, 400-413), and drops every
// out-of-order packet silently while a resend is pending (h:343-345). So
// several packets may be in flight and a resend is go-back-N. "fe" (h:414)
// means the stream was reset and needs a new SYNC.
package binprotocol

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Token starts every packet (h:212), little-endian on the wire.
const Token = 0xB5AD

// Protocols and packet types (h:202-204, 129).
const (
	ProtoControl = 0
	ProtoFile    = 1

	ControlSync  = 1 // answered by "ss<sync>,<buffer size>,<version>"
	ControlClose = 2 // leave binary mode (h:429-431)

	FileQuery = 0 // "PFT:version:0.1.0:compression:heatshrink,8,4" or ":none"
	FileOpen  = 1 // payload: dummy u8, compression u8, name, NUL (h:45-59)
	FileClose = 2
	FileWrite = 3
	FileAbort = 4 // closes and deletes the file (h:113-122)
)

// HeaderSize and FooterSize are the fixed packet overhead: 10 bytes for a
// packet with payload.
const (
	HeaderSize = 8
	FooterSize = 2
)

// Fletcher16 adds one byte to a running checksum exactly as
// BinaryStream::checksum does (h:260-263): both halves modulo 255.
func Fletcher16(cs uint16, b byte) uint16 {
	lo := (uint32(cs&0xFF) + uint32(b)) % 255
	hi := (uint32(cs>>8) + lo) % 255
	return uint16(hi<<8 | lo)
}

// Packet builds one packet as Marlin expects it.
func Packet(sync uint8, protocol, typ uint8, payload []byte) []byte {
	b := make([]byte, 0, HeaderSize+len(payload)+FooterSize)
	b = binary.LittleEndian.AppendUint16(b, Token)
	b = append(b, sync, protocol<<4|typ&0xF)
	b = binary.LittleEndian.AppendUint16(b, uint16(len(payload)))
	var cs uint16
	for _, c := range b[2:6] {
		cs = Fletcher16(cs, c)
	}
	b = binary.LittleEndian.AppendUint16(b, cs)
	if len(payload) == 0 {
		return b
	}
	for _, c := range b[6:8] {
		cs = Fletcher16(cs, c)
	}
	for _, c := range payload {
		cs = Fletcher16(cs, c)
	}
	b = append(b, payload...)
	return binary.LittleEndian.AppendUint16(b, cs)
}

// ReplyKind classifies a line from the printer during a binary session.
type ReplyKind int

const (
	ReplyNone   ReplyKind = iota // not part of the protocol (autoreport, echo:...)
	ReplyOK                      // "ok<sync>": packet accepted
	ReplyResend                  // "rs<sync>": resend from this sync
	ReplySync                    // "ss<sync>,<buffer size>,<version>"
	ReplyFatal                   // "fe<sync>": stream reset, SYNC again
	ReplyPFT                     // "PFT:..." (and Marlin's misspelt "PTF:invalid")
)

// Reply is one parsed protocol line.
type Reply struct {
	Kind    ReplyKind
	Sync    uint8
	BufSize int    // ReplySync: Marlin's receive buffer, the payload limit
	Version string // ReplySync: stream protocol version
	Text    string // ReplyPFT: everything after "PFT:"
}

// ParseReply recognises the protocol's reply lines. An "ok" must be
// followed by digits only, so ASCII "ok N12 P15 B3" and "ok T:..." never
// match.
func ParseReply(line string) (Reply, bool) {
	s := strings.TrimSpace(line)
	if t, ok := strings.CutPrefix(s, "PFT:"); ok {
		return Reply{Kind: ReplyPFT, Text: t}, true
	}
	if t, ok := strings.CutPrefix(s, "PTF:"); ok {
		return Reply{Kind: ReplyPFT, Text: t}, true
	}
	if len(s) < 3 {
		return Reply{}, false
	}
	var kind ReplyKind
	switch s[:2] {
	case "ok":
		kind = ReplyOK
	case "rs":
		kind = ReplyResend
	case "fe":
		kind = ReplyFatal
	case "ss":
		kind = ReplySync
	default:
		return Reply{}, false
	}
	rest := s[2:]
	if kind == ReplySync {
		f := strings.Split(rest, ",")
		if len(f) != 3 {
			return Reply{}, false
		}
		n, err1 := strconv.ParseUint(f[0], 10, 8)
		size, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil || size <= 0 {
			return Reply{}, false
		}
		return Reply{Kind: ReplySync, Sync: uint8(n), BufSize: size, Version: f[2]}, true
	}
	n, err := strconv.ParseUint(rest, 10, 8)
	if err != nil {
		return Reply{}, false
	}
	return Reply{Kind: kind, Sync: uint8(n)}, true
}

var (
	ErrTimeout = errors.New("binary transfer: no answer from printer")
	ErrFatal   = errors.New("binary transfer: printer reset the stream (fe)")
	ErrRetries = errors.New("binary transfer: too many resends")
)

// PFTError is a file-transfer failure the printer reported, e.g.
// "fail" (cannot create the file), "ioerror" (SD write), "invalid" (no
// file open, e.g. after Marlin's 10 s idle abort), "busy".
type PFTError struct{ Status string }

func (e *PFTError) Error() string { return "binary transfer: printer answered PFT:" + e.Status }

// Config is what a Session needs from its caller.
type Config struct {
	// Write puts one whole packet on the wire (one call per packet so
	// nothing can be interleaved inside it).
	Write func([]byte) error
	// Lines delivers the printer's lines; non-protocol lines are skipped.
	Lines <-chan string

	Timeout time.Duration // per reply before a resend; default 1 s (py:62)
	Retries int           // resends without progress before giving up; default 20
	Window  int           // WRITE packets in flight; default 4, 1 = stop-and-wait
	// OpenTimeout bounds the PFT answer to OPEN and CLOSE: Marlin mounts the
	// card and creates or flushes the file first; default 10 s.
	OpenTimeout time.Duration
}

// Stats counts recoveries over a session.
type Stats struct {
	Packets  int   // packets accepted
	Resends  int   // packets written again (rs or timeout)
	Timeouts int   // reply timeouts
	Wire     int64 // bytes written, retransmissions included
}

// Session is one "M28 B1" binary session. Not safe for concurrent use.
type Session struct {
	cfg Config

	sync       uint8 // sync of the next new packet
	bufSize    int
	Version    string // stream version from "ss"
	FTVersion  string // file transfer version from PFT:version
	Compressor string // "none" or e.g. "heatshrink"
	HSWindow   int    // heatshrink window bits (0 without heatshrink)
	HSLook     int    // heatshrink lookahead bits

	open     bool // OPEN succeeded, CLOSE not yet
	inflight []flight
	resent   bool // rs answered for inflight[0] and no ack since
	stats    Stats
}

type flight struct {
	sync uint8
	pkt  []byte
}

// Start synchronises with a printer that has just answered "M28 B1":
// SYNC learns the stream sync and buffer size, QUERY the transfer
// version and compression.
func Start(ctx context.Context, cfg Config) (*Session, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = time.Second
	}
	if cfg.Retries <= 0 {
		cfg.Retries = 20
	}
	if cfg.Window <= 0 {
		cfg.Window = 4
	}
	if cfg.Window > 64 {
		cfg.Window = 64
	}
	if cfg.OpenTimeout <= 0 {
		cfg.OpenTimeout = 10 * time.Second
	}
	s := &Session{cfg: cfg}
	if err := s.Sync(ctx); err != nil {
		return nil, err
	}
	pft, err := s.control(ctx, ProtoFile, FileQuery, nil, cfg.Timeout)
	if err != nil {
		return s, fmt.Errorf("query: %w", err)
	}
	if err := s.parseVersion(pft); err != nil {
		return s, err
	}
	return s, nil
}

// Sync sends SYNC (accepted with any sync number, h:322-328) until "ss"
// arrives and adopts the sync and buffer size it reports.
func (s *Session) Sync(ctx context.Context) error {
	pkt := Packet(0, ProtoControl, ControlSync, nil)
	for try := 0; try < 3; try++ {
		if err := s.write(pkt); err != nil {
			return err
		}
		deadline := time.Now().Add(s.cfg.Timeout)
		for {
			r, err := s.next(ctx, deadline)
			if err == ErrTimeout {
				s.stats.Timeouts++
				break
			}
			if err != nil {
				return err
			}
			if r.Kind == ReplySync {
				s.sync = r.Sync
				s.bufSize = r.BufSize
				s.Version = r.Version
				s.inflight = s.inflight[:0]
				s.resent = false
				return nil
			}
		}
	}
	return fmt.Errorf("sync: %w", ErrTimeout)
}

func (s *Session) parseVersion(pft string) error {
	// "version:0.1.0:compression:heatshrink,8,4" or "...:compression:none"
	f := strings.Split(pft, ":")
	if len(f) < 4 || f[0] != "version" || f[2] != "compression" {
		return fmt.Errorf("query: unexpected answer PFT:%s", pft)
	}
	s.FTVersion = f[1]
	c := strings.Split(f[3], ",")
	s.Compressor = c[0]
	if c[0] == "heatshrink" && len(c) == 3 {
		s.HSWindow, _ = strconv.Atoi(c[1])
		s.HSLook, _ = strconv.Atoi(c[2])
	}
	return nil
}

// MaxPayload is the largest WRITE payload: Marlin's receive buffer
// (serial line_buffer, MAX_CMD_SIZE = 96 here); a larger packet is an
// overrun and draws "fe" (h:360-365).
func (s *Session) MaxPayload() int { return s.bufSize }

// Heatshrink reports the compression Marlin decodes, if any.
func (s *Session) Heatshrink() (window, lookahead int, ok bool) {
	return s.HSWindow, s.HSLook, s.Compressor == "heatshrink" && s.HSWindow > 0 && s.HSLook > 0
}

// Stats returns the counters so far.
func (s *Session) Stats() Stats { return s.stats }

// Open creates name (8.3 unless LONG_FILENAME_WRITE_SUPPORT) on the card,
// truncating an existing file. compressed must only be set when Heatshrink
// reports ok. A "PFT:busy" (a transfer left open) is answered with ABORT
// and one more OPEN, as the reference client does.
func (s *Session) Open(ctx context.Context, name string, compressed bool) error {
	p := []byte{0, 0}
	if compressed {
		p[1] = 1
	}
	p = append(append(p, name...), 0)
	for try := 0; ; try++ {
		pft, err := s.control(ctx, ProtoFile, FileOpen, p, s.cfg.OpenTimeout)
		if err != nil {
			return fmt.Errorf("open: %w", err)
		}
		switch {
		case pft == "success":
			s.open = true
			return nil
		case pft == "busy" && try == 0:
			if _, err := s.control(ctx, ProtoFile, FileAbort, nil, s.cfg.OpenTimeout); err != nil {
				return fmt.Errorf("abort stale transfer: %w", err)
			}
		default:
			return fmt.Errorf("open %s: %w", name, &PFTError{pft})
		}
	}
}

// Write sends data as WRITE packets of at most MaxPayload bytes, keeping
// up to Window packets in flight. It returns once every packet is queued;
// Flush (or Close) waits for the acks.
func (s *Session) Write(ctx context.Context, data []byte) error {
	for len(data) > 0 {
		n := min(len(data), s.bufSize)
		for len(s.inflight) >= s.cfg.Window {
			if err := s.await(ctx); err != nil {
				return err
			}
		}
		f := flight{sync: s.sync, pkt: Packet(s.sync, ProtoFile, FileWrite, data[:n])}
		s.sync++
		s.inflight = append(s.inflight, f)
		if err := s.write(f.pkt); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// Flush waits until every WRITE packet is acked.
func (s *Session) Flush(ctx context.Context) error {
	for len(s.inflight) > 0 {
		if err := s.await(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Close flushes and closes the file ("PFT:success" once it is on the card).
func (s *Session) Close(ctx context.Context) error {
	if err := s.Flush(ctx); err != nil {
		return err
	}
	pft, err := s.control(ctx, ProtoFile, FileClose, nil, s.cfg.OpenTimeout)
	if err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if pft != "success" {
		return fmt.Errorf("close: %w", &PFTError{pft})
	}
	s.open = false
	return nil
}

// Abort drops whatever is in flight, re-synchronises (the stream state is
// unknown after an error) and, while a file this session opened is still
// open, sends ABORT, which closes and deletes it. Never otherwise:
// transfer_abort removes card.filename even with no transfer active
// (h:113-122), which after a finished upload is the new file and after a
// failed OPEN whatever file was selected before.
func (s *Session) Abort(ctx context.Context) error {
	s.inflight = s.inflight[:0]
	s.resent = false
	if err := s.Sync(ctx); err != nil {
		return err
	}
	if !s.open {
		return nil
	}
	_, err := s.control(ctx, ProtoFile, FileAbort, nil, s.cfg.OpenTimeout)
	if err == nil {
		s.open = false
	}
	return err
}

// Exit sends CONTROL CLOSE: Marlin acks it, then goes back to ASCII G-code
// (h:390-397, 429-431). If the ack does not come, a SYNC tells the two
// cases apart: only binary mode answers it with "ss" (and the sync to
// use for the next CLOSE); silence means the CLOSE went through and its
// ok was lost. Either way stray packet bytes may now sit in Marlin's ASCII
// line buffer, so the caller must follow Exit with a bare newline, which
// ends such a line (Marlin ignores an empty line).
func (s *Session) Exit(ctx context.Context) error {
	s.inflight = s.inflight[:0]
	for round := 0; round < 3; round++ {
		closeSync := s.sync
		acked, err := s.exitOnce(ctx)
		if err != nil || acked {
			return err
		}
		binary, err := s.probe(ctx, closeSync)
		if err != nil || !binary {
			return err
		}
	}
	return fmt.Errorf("exit: %w", ErrTimeout)
}

// probe asks with SYNC whether Marlin is still in binary mode after a
// CONTROL CLOSE (sync closeSync) went unanswered. Any protocol reply
// proves binary mode and tells the sync to use next; a late ok for the
// CLOSE itself means it went through. Two SYNCs without any answer mean
// ASCII mode.
func (s *Session) probe(ctx context.Context, closeSync uint8) (bool, error) {
	for try := 0; try < 2; try++ {
		if err := s.write(Packet(0, ProtoControl, ControlSync, nil)); err != nil {
			return false, err
		}
		deadline := time.Now().Add(s.cfg.Timeout)
		for {
			r, err := s.next(ctx, deadline)
			if err == ErrTimeout {
				break
			}
			if err != nil {
				return false, err
			}
			switch r.Kind {
			case ReplySync, ReplyResend:
				s.sync = r.Sync
				return true, nil
			case ReplyFatal:
				s.sync = 0
				return true, nil
			case ReplyOK:
				if r.Sync == closeSync {
					return false, nil
				}
			}
		}
	}
	return false, nil
}

// exitOnce sends one CONTROL CLOSE and waits for its ok, resending it on
// a resend request.
func (s *Session) exitOnce(ctx context.Context) (bool, error) {
	pkt := Packet(s.sync, ProtoControl, ControlClose, nil)
	for tries := 0; tries <= s.cfg.Retries; tries++ {
		if err := s.write(pkt); err != nil {
			return false, err
		}
		deadline := time.Now().Add(s.cfg.Timeout)
	wait:
		for {
			r, err := s.next(ctx, deadline)
			if err == ErrTimeout {
				s.stats.Timeouts++
				return false, nil
			}
			if err != nil {
				return false, err
			}
			switch {
			case r.Kind == ReplyOK && r.Sync == s.sync:
				s.sync++
				s.stats.Packets++
				return true, nil
			case r.Kind == ReplyResend && r.Sync == s.sync:
				s.stats.Resends++
				break wait
			case r.Kind == ReplyFatal:
				return false, ErrFatal
			}
		}
	}
	return false, ErrRetries
}

// control sends one packet stop-and-wait. FILE packets other than WRITE
// get a PFT line after their ok (h:147-195); that line is returned and
// also counts as the ack if the ok itself was lost.
func (s *Session) control(ctx context.Context, proto, typ uint8, payload []byte, pftTimeout time.Duration) (string, error) {
	if err := s.Flush(ctx); err != nil {
		return "", err
	}
	wantPFT := proto == ProtoFile
	sync := s.sync
	pkt := Packet(sync, proto, typ, payload)
	acked := false
	tries := 0
	if err := s.write(pkt); err != nil {
		return "", err
	}
	deadline := time.Now().Add(s.cfg.Timeout)
	for {
		r, err := s.next(ctx, deadline)
		if err == ErrTimeout {
			if acked {
				// ok arrived, the PFT answer did not in pftTimeout.
				return "", err
			}
			s.stats.Timeouts++
			if tries++; tries > s.cfg.Retries {
				return "", err
			}
			s.stats.Resends++
			if err := s.write(pkt); err != nil {
				return "", err
			}
			deadline = time.Now().Add(s.cfg.Timeout)
			continue
		}
		if err != nil {
			return "", err
		}
		switch r.Kind {
		case ReplyOK:
			if r.Sync != sync || acked {
				continue // stale duplicate
			}
			acked = true
			s.sync++
			s.stats.Packets++
			if !wantPFT {
				return "", nil
			}
			deadline = time.Now().Add(pftTimeout)
		case ReplyPFT:
			if !wantPFT {
				continue
			}
			if !acked {
				acked = true
				s.sync++
				s.stats.Packets++
			}
			return r.Text, nil
		case ReplyResend:
			if acked || r.Sync != sync {
				continue
			}
			if tries++; tries > s.cfg.Retries {
				return "", ErrRetries
			}
			s.stats.Resends++
			if err := s.write(pkt); err != nil {
				return "", err
			}
			deadline = time.Now().Add(s.cfg.Timeout)
		case ReplyFatal:
			return "", ErrFatal
		}
	}
}

// await handles replies until at least one in-flight WRITE is acked.
func (s *Session) await(ctx context.Context) error {
	tries := 0
	deadline := time.Now().Add(s.cfg.Timeout)
	for {
		r, err := s.next(ctx, deadline)
		if err == ErrTimeout {
			// Lost packet or lost ok: go back and send everything again.
			// Marlin acks a duplicate of its last packet and asks for the
			// right one on anything older (h:339-354).
			s.stats.Timeouts++
			if tries++; tries > s.cfg.Retries {
				return err
			}
			if err := s.resendAll(); err != nil {
				return err
			}
			deadline = time.Now().Add(s.cfg.Timeout)
			continue
		}
		if err != nil {
			return err
		}
		switch r.Kind {
		case ReplyOK:
			if s.ackThrough(r.Sync+1) > 0 {
				return nil
			}
		case ReplyResend:
			// Everything before the requested sync was accepted.
			progressed := s.ackThrough(r.Sync) > 0
			if len(s.inflight) == 0 || r.Sync != s.inflight[0].sync {
				if progressed {
					return nil
				}
				continue // stale
			}
			if s.resent && !progressed {
				// Another rs for the same packet from the same burst
				// (h:343-345 drops the rest silently); the copy we
				// already sent answers it. A lost copy is caught by
				// the timeout.
				continue
			}
			if tries++; tries > s.cfg.Retries {
				return ErrRetries
			}
			if err := s.resendAll(); err != nil {
				return err
			}
			s.resent = true
			deadline = time.Now().Add(s.cfg.Timeout)
			if progressed {
				return nil
			}
		case ReplyFatal:
			return ErrFatal
		case ReplyPFT:
			// A WRITE only answers on failure: "ioerror" or "invalid"
			// (no file open any more, e.g. Marlin's 10 s idle abort).
			return &PFTError{r.Text}
		}
	}
}

// ackThrough drops the in-flight packets before sync next (modulo 256)
// and returns how many.
func (s *Session) ackThrough(next uint8) int {
	if len(s.inflight) == 0 {
		return 0
	}
	d := int(next - s.inflight[0].sync)
	if d > len(s.inflight) {
		return 0 // not in the window: a stale reply
	}
	s.inflight = s.inflight[d:]
	s.stats.Packets += d
	if d > 0 {
		s.resent = false
	}
	return d
}

func (s *Session) resendAll() error {
	for _, f := range s.inflight {
		s.stats.Resends++
		if err := s.write(f.pkt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) write(p []byte) error {
	s.stats.Wire += int64(len(p))
	return s.cfg.Write(p)
}

// next returns the next protocol reply, or ErrTimeout at deadline.
func (s *Session) next(ctx context.Context, deadline time.Time) (Reply, error) {
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	for {
		select {
		case line := <-s.cfg.Lines:
			if r, ok := ParseReply(line); ok {
				return r, nil
			}
		case <-t.C:
			return Reply{}, ErrTimeout
		case <-ctx.Done():
			return Reply{}, ctx.Err()
		}
	}
}
