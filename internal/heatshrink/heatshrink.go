// Package heatshrink is a streaming encoder (and a decoder for tests) for
// the heatshrink LZSS format that Marlin's BINARY_FILE_TRANSFER decodes
// (Marlin/src/libs/heatshrink/heatshrink_decoder.cpp, built static with
// HEATSHRINK_STATIC_WINDOW_BITS 8 and LOOKAHEAD_BITS 4 in
// heatshrink_config.h; Marlin reports both in its "PFT:version" reply, so
// callers should take them from there).
//
// Bit stream, MSB first within each byte:
//
//	1, 8 bits              literal byte
//	0, window bits, look bits   backreference: (offset-1), (count-1)
//
// The decoder copies a backreference byte by byte through its 2^window
// byte history (decoder.cpp st_yield_backref), so count may exceed offset.
// The last byte is padded with zero bits, which the decoder never turns
// into output (a backreference needs more bits than the padding holds).
package heatshrink

import (
	"fmt"
	"io"
)

// Config holds the two parameters that must match the decoder.
type Config struct {
	WindowBits    uint // history 2^WindowBits bytes
	LookaheadBits uint // longest match 2^LookaheadBits bytes
}

// Marlin 2.1.2.7's static configuration (heatshrink_config.h:17-19).
func DefaultConfig() Config { return Config{WindowBits: 8, LookaheadBits: 4} }

// Validate applies heatshrink's own limits (decoder.cpp heatshrink_decoder_alloc).
func (c Config) Validate() error {
	if c.WindowBits < 4 || c.WindowBits > 15 || c.LookaheadBits < 3 || c.LookaheadBits >= c.WindowBits {
		return fmt.Errorf("heatshrink: unsupported window %d, lookahead %d", c.WindowBits, c.LookaheadBits)
	}
	return nil
}

// minMatch: a backreference costs 1+window+lookahead bits, a literal 9, so
// with 8/4 a 2-byte match (13 bits) already beats two literals (18).
func (c Config) minMatch() int { return int(1+c.WindowBits+c.LookaheadBits)/9 + 1 }

// Encoder compresses everything written to it into w. Close flushes the
// final bits. Memory is a few times the window size regardless of input.
type Encoder struct {
	cfg    Config
	w      io.Writer
	window int
	maxLen int
	buf    []byte // history (up to window bytes) followed by pending input
	pos    int    // first pending byte in buf

	out   []byte
	cur   byte
	nbits uint
	in    int64 // bytes consumed
}

// NewEncoder returns an encoder writing to w. cfg must Validate.
func NewEncoder(w io.Writer, cfg Config) (*Encoder, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Encoder{
		cfg:    cfg,
		w:      w,
		window: 1 << cfg.WindowBits,
		maxLen: 1 << cfg.LookaheadBits,
	}, nil
}

// Write encodes p, holding back the last maxLen bytes until more input (or
// Close) shows how far a match can run.
func (e *Encoder) Write(p []byte) (int, error) {
	e.buf = append(e.buf, p...)
	e.in += int64(len(p))
	e.encode(false)
	if len(e.out) >= 512 {
		if err := e.emit(); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Close encodes the remaining input, pads the last byte and writes it all.
func (e *Encoder) Close() error {
	e.encode(true)
	if e.nbits > 0 {
		e.out = append(e.out, e.cur<<(8-e.nbits))
		e.cur, e.nbits = 0, 0
	}
	return e.emit()
}

// Consumed returns the number of input bytes written so far.
func (e *Encoder) Consumed() int64 { return e.in }

func (e *Encoder) emit() error {
	if len(e.out) == 0 {
		return nil
	}
	_, err := e.w.Write(e.out)
	e.out = e.out[:0]
	return err
}

func (e *Encoder) encode(final bool) {
	minLen := e.cfg.minMatch()
	for {
		avail := len(e.buf) - e.pos
		if avail == 0 || (!final && avail < e.maxLen) {
			break
		}
		limit := min(avail, e.maxLen)
		bestLen, bestOff := 0, 0
		cur := e.buf[e.pos:]
		for j := max(0, e.pos-e.window); j < e.pos; j++ {
			if e.buf[j] != cur[0] {
				continue
			}
			l := 1
			for l < limit && e.buf[j+l] == cur[l] {
				l++
			}
			// >= prefers the nearest of equally long matches.
			if l >= bestLen {
				bestLen, bestOff = l, e.pos-j
			}
		}
		if bestLen >= minLen {
			e.bits(0, 1)
			e.bits(uint32(bestOff-1), e.cfg.WindowBits)
			e.bits(uint32(bestLen-1), e.cfg.LookaheadBits)
			e.pos += bestLen
		} else {
			e.bits(1, 1)
			e.bits(uint32(cur[0]), 8)
			e.pos++
		}
	}
	// Keep only the history the window can still reach.
	if drop := e.pos - e.window; drop >= 4096 {
		n := copy(e.buf, e.buf[drop:])
		e.buf = e.buf[:n]
		e.pos -= drop
	}
}

func (e *Encoder) bits(v uint32, n uint) {
	for i := int(n) - 1; i >= 0; i-- {
		e.cur = e.cur<<1 | byte(v>>uint(i)&1)
		if e.nbits++; e.nbits == 8 {
			e.out = append(e.out, e.cur)
			e.cur, e.nbits = 0, 0
		}
	}
}

// Encode compresses src in one go.
func Encode(src []byte, cfg Config) ([]byte, error) {
	var out sliceWriter
	enc, err := NewEncoder(&out, cfg)
	if err != nil {
		return nil, err
	}
	if _, err := enc.Write(src); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.b, nil
}

type sliceWriter struct{ b []byte }

func (s *sliceWriter) Write(p []byte) (int, error) {
	s.b = append(s.b, p...)
	return len(p), nil
}

// Decode reverses Encode the way Marlin's decoder does: a history of
// 2^WindowBits bytes that starts zeroed (heatshrink_decoder_reset), trailing
// bits that do not make a whole literal or backreference are ignored.
func Decode(src []byte, cfg Config) []byte {
	win := make([]byte, 1<<cfg.WindowBits)
	mask := len(win) - 1
	head := 0
	var out []byte
	pos, bit := 0, uint(0)
	read := func(n uint) (uint32, bool) {
		var v uint32
		for i := uint(0); i < n; i++ {
			if pos >= len(src) {
				return 0, false
			}
			v = v<<1 | uint32(src[pos]>>(7-bit)&1)
			if bit++; bit == 8 {
				bit, pos = 0, pos+1
			}
		}
		return v, true
	}
	push := func(c byte) {
		win[head&mask] = c
		head++
		out = append(out, c)
	}
	for {
		tag, ok := read(1)
		if !ok {
			return out
		}
		if tag == 1 {
			c, ok := read(8)
			if !ok {
				return out
			}
			push(byte(c))
			continue
		}
		idx, ok1 := read(cfg.WindowBits)
		cnt, ok2 := read(cfg.LookaheadBits)
		if !ok1 || !ok2 {
			return out
		}
		for i := 0; i <= int(cnt); i++ {
			push(win[(head-int(idx)-1)&mask])
		}
	}
}
