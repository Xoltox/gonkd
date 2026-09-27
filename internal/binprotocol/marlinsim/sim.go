// Package marlinsim is a test double of Marlin 2.1.2.7's binary transfer
// receiver: BinaryStream::receive and SDFileTransferProtocol from
// src/feature/binary_stream.h, state for state, with the same replies. It
// is fed the bytes Marlin would read from its RX buffer and writes reply
// lines through Out. Faults (lost or flipped bytes, lost replies) are the
// caller's job: it only has to damage what it feeds in or passes out.
package marlinsim

import (
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/Xoltox/gonkd/internal/binprotocol"
	"github.com/Xoltox/gonkd/internal/heatshrink"
)

type state int

const (
	stWait state = iota
	stHeader
	stData
	stFooter
)

// Sim is one serial port's binary stream plus the SD card behind it.
type Sim struct {
	// Out receives each reply line, newline included.
	Out func(line string)
	// BufSize is the receive buffer (serial line_buffer, MAX_CMD_SIZE 96).
	BufSize int
	// FailOpen makes OPEN answer "PFT:fail" (no card, bad name).
	FailOpen bool

	mu      sync.Mutex
	binary  bool
	st      state
	hdr     [binprotocol.HeaderSize]byte
	n       int
	cs, hcs uint16
	payload []byte
	footer  [2]byte
	sync    uint8
	retries int

	active     bool
	compressed bool
	name       string
	selected   string // card.filename: the last file opened
	data       []byte
	files      map[string][]byte
	opens      int
	aborts     int
	resends    int
}

// New returns a Sim in ASCII mode with an empty card.
func New(out func(string)) *Sim {
	return &Sim{Out: out, BufSize: 96, files: map[string][]byte{}}
}

// EnterBinary is what "M28 B1" does (card.flag.binary_mode = true).
func (s *Sim) EnterBinary() {
	s.mu.Lock()
	s.binary = true
	s.mu.Unlock()
}

// Binary reports whether the stream is in binary mode.
func (s *Sim) Binary() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.binary
}

// InPacket reports whether a packet is partly received; only then does
// data starvation count (stream_read, PACKET_MAX_WAIT).
func (s *Sim) InPacket() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.binary && s.st != stWait
}

// Starved is PACKET_TIMEOUT: no byte for 500 ms inside a packet.
func (s *Sim) Starved() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st == stWait {
		return
	}
	s.echo("Datastream timeout")
	s.resend()
}

// File returns a file on the card.
func (s *Sim) File(name string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.files[name]
	return append([]byte(nil), b...), ok
}

// PutFile places a file on the card.
func (s *Sim) PutFile(name string, b []byte) {
	s.mu.Lock()
	s.files[name] = b
	s.mu.Unlock()
}

// Names lists the card.
func (s *Sim) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.files {
		out = append(out, k)
	}
	return out
}

// Counters returns OPENs, ABORTs and resend requests so far.
func (s *Sim) Counters() (opens, aborts, resends int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens, s.aborts, s.resends
}

// TransferActive reports an open file.
func (s *Sim) TransferActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

func (s *Sim) line(format string, a ...any) { s.Out(fmt.Sprintf(format, a...) + "\n") }
func (s *Sim) echo(msg string)              { s.line("echo:%s", msg) }

func (s *Sim) reset() {
	s.st = stWait
	s.n = 0
	s.cs, s.hcs = 0, 0
	s.hdr = [binprotocol.HeaderSize]byte{}
	s.payload = s.payload[:0]
}

// resend is PACKET_RESEND (MAX_RETRIES 0: unlimited).
func (s *Sim) resend() {
	s.retries++
	s.resends++
	s.reset()
	s.echo(fmt.Sprintf("Resend request %d", s.retries))
	s.line("rs%d", s.sync)
}

// Byte feeds one received byte. It returns false once the byte was not
// consumed because the stream is in ASCII mode (after CONTROL CLOSE).
func (s *Sim) Byte(c byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.binary {
		return false
	}
	switch s.st {
	case stWait:
		// PACKET_WAIT: slide a 2-byte window until it reads the token.
		s.hdr[0], s.hdr[1] = s.hdr[1], c
		if binary.LittleEndian.Uint16(s.hdr[:2]) == binprotocol.Token {
			s.st = stHeader
			s.n = 2
			s.cs = 0
		}
	case stHeader:
		s.hdr[s.n] = c
		s.n++
		s.cs = binprotocol.Fletcher16(s.cs, c)
		if s.n == 6 {
			s.hcs = s.cs
		}
		if s.n < binprotocol.HeaderSize {
			return true
		}
		if binary.LittleEndian.Uint16(s.hdr[6:]) != s.hcs {
			s.echo(fmt.Sprintf("Packet header(%d?) corrupt", s.hdr[2]))
			s.resend()
			return true
		}
		sync, meta := s.hdr[2], s.hdr[3]
		size := int(binary.LittleEndian.Uint16(s.hdr[4:]))
		if meta>>4 == binprotocol.ProtoControl && meta&0xF == binprotocol.ControlSync {
			s.line("ss%d,%d,0.1.0", s.sync, s.BufSize)
			s.reset()
			return true
		}
		switch {
		case sync == s.sync:
			s.n = 0
			if size > 0 {
				s.st = stData
			} else {
				s.process()
			}
		case int(sync) == int(s.sync)-1: // int, as in C: no wrap at 0
			s.line("ok%d", sync)
			s.reset()
		case s.retries > 0:
			s.reset()
		default:
			s.echo("Datastream packet out of order")
			s.resend()
		}
	case stData:
		if len(s.payload) >= s.BufSize {
			s.echo("Datastream packet data buffer overrun")
			s.line("fe%d", s.hdr[2])
			s.sync, s.retries = 0, 0
			s.reset()
			return true
		}
		s.payload = append(s.payload, c)
		s.cs = binprotocol.Fletcher16(s.cs, c)
		if len(s.payload) == int(binary.LittleEndian.Uint16(s.hdr[4:])) {
			s.st = stFooter
			s.n = 0
		}
	case stFooter:
		s.footer[s.n] = c
		s.n++
		if s.n < 2 {
			return true
		}
		if binary.LittleEndian.Uint16(s.footer[:]) != s.cs {
			s.echo(fmt.Sprintf("Packet(%d) payload corrupt", s.hdr[2]))
			s.resend()
			return true
		}
		s.process()
	}
	return true
}

// process is PACKET_PROCESS + dispatch: ok first, then the action.
func (s *Sim) process() {
	sync, meta := s.hdr[2], s.hdr[3]
	s.sync++
	s.retries = 0
	s.line("ok%d", sync)
	p := s.payload
	switch meta >> 4 {
	case binprotocol.ProtoControl:
		if meta&0xF == binprotocol.ControlClose {
			s.binary = false
		} else {
			s.echo("Unknown BinaryProtocolControl Packet")
		}
	case binprotocol.ProtoFile:
		s.file(meta&0xF, p)
	default:
		s.echo("Unsupported Binary Protocol")
	}
	s.reset()
}

func (s *Sim) file(typ uint8, p []byte) {
	switch typ {
	case binprotocol.FileQuery:
		s.line("PFT:version:0.1.0:compression:heatshrink,8,4")
	case binprotocol.FileOpen:
		s.opens++
		switch {
		case s.active:
			s.line("PFT:busy")
		case len(p) > 2 && p[len(p)-1] == 0 && !s.FailOpen:
			s.compressed = p[1]&1 == 1
			s.name = string(p[2 : len(p)-1])
			s.data = s.data[:0]
			s.active = true
			s.selected = s.name
			s.line("PFT:success")
		default:
			s.line("PFT:fail")
		}
	case binprotocol.FileClose:
		if !s.active {
			s.line("PFT:invalid")
			return
		}
		b := append([]byte(nil), s.data...)
		if s.compressed {
			b = heatshrink.Decode(b, heatshrink.DefaultConfig())
		}
		s.files[s.name] = b
		s.active = false
		s.line("PFT:success")
	case binprotocol.FileWrite:
		if !s.active {
			s.line("PFT:invalid")
			return
		}
		s.data = append(s.data, p...)
	case binprotocol.FileAbort:
		// transfer_abort removes card.filename whether or not a transfer
		// is active: after a finished upload that is the new file.
		s.aborts++
		delete(s.files, s.selected)
		s.active = false
		s.line("PFT:success")
	default:
		s.line("PTF:invalid")
	}
}
