// Package heatshrink implements a small pure-Go encoder (and a decoder used
// only by tests) for the heatshrink compression format used by Marlin's
// BINARY_FILE_TRANSFER feature (src/feature/binary_stream.h wraps a
// heatshrink_encoder/decoder pair from https://github.com/atomicobject/heatshrink).
//
// Wire format (bit-packed, MSB-first within each byte):
//
//	tag bit: 1 = literal byte follows (8 bits), 0 = backreference follows
//	backreference: window_bits bits of (offset-1), then lookahead_bits bits
//	               of (length-1)
//
// NOTE: the window/lookahead sizes here (8/4, heatshrink's common embedded
// defaults) are an assumption -- Marlin's exact HEATSHRINK_STATIC_* build
// configuration was not verified against real device firmware output as
// part of this work. If the real printer fails to decode uploaded files,
// this is the first thing to check (see README "Known gaps").
package heatshrink

const (
	DefaultWindowBits    = 8
	DefaultLookaheadBits = 4
)

// Config holds the two heatshrink parameters that must match on both ends.
type Config struct {
	WindowBits    uint // e.g. 8 => 256-byte window
	LookaheadBits uint // e.g. 4 => matches up to 16 bytes
}

// DefaultConfig returns heatshrink's common embedded-target configuration.
func DefaultConfig() Config {
	return Config{WindowBits: DefaultWindowBits, LookaheadBits: DefaultLookaheadBits}
}

type bitWriter struct {
	out     []byte
	cur     byte
	nbits   uint
}

func (b *bitWriter) writeBit(bit uint8) {
	b.cur = (b.cur << 1) | (bit & 1)
	b.nbits++
	if b.nbits == 8 {
		b.out = append(b.out, b.cur)
		b.cur = 0
		b.nbits = 0
	}
}

func (b *bitWriter) writeBits(v uint32, n uint) {
	for i := int(n) - 1; i >= 0; i-- {
		b.writeBit(uint8((v >> uint(i)) & 1))
	}
}

func (b *bitWriter) flush() []byte {
	if b.nbits > 0 {
		b.cur <<= (8 - b.nbits)
		b.out = append(b.out, b.cur)
		b.cur = 0
		b.nbits = 0
	}
	return b.out
}

// Encode compresses src using the heatshrink LZSS-style format described
// above. The match finder is a simple brute-force search over the window
// (fine given the small 256-byte window this format uses).
func Encode(src []byte, cfg Config) []byte {
	window := 1 << cfg.WindowBits
	maxLen := (1 << cfg.LookaheadBits) + minMatchExtra(cfg)
	minMatch := 2 // shortest backref worth encoding: 1(tag)+winBits+lookBits vs 1(tag)+8 per literal

	bw := &bitWriter{}
	i := 0
	for i < len(src) {
		bestLen, bestOff := 0, 0
		start := i - window
		if start < 0 {
			start = 0
		}
		limit := len(src) - i
		if limit > maxLen {
			limit = maxLen
		}
		for j := start; j < i; j++ {
			l := 0
			for l < limit && src[j+l] == src[i+l] {
				l++
			}
			if l > bestLen {
				bestLen = l
				bestOff = i - j
			}
		}
		if bestLen >= minMatch {
			bw.writeBit(0)
			bw.writeBits(uint32(bestOff-1), cfg.WindowBits)
			bw.writeBits(uint32(bestLen-1), cfg.LookaheadBits)
			i += bestLen
		} else {
			bw.writeBit(1)
			bw.writeBits(uint32(src[i]), 8)
			i++
		}
	}
	return bw.flush()
}

func minMatchExtra(cfg Config) int { return 0 }

type bitReader struct {
	in    []byte
	pos   int // byte index
	bit   uint
}

func (r *bitReader) readBit() (uint8, bool) {
	if r.pos >= len(r.in) {
		return 0, false
	}
	b := (r.in[r.pos] >> (7 - r.bit)) & 1
	r.bit++
	if r.bit == 8 {
		r.bit = 0
		r.pos++
	}
	return b, true
}

func (r *bitReader) readBits(n uint) (uint32, bool) {
	var v uint32
	for i := uint(0); i < n; i++ {
		b, ok := r.readBit()
		if !ok {
			return 0, false
		}
		v = (v << 1) | uint32(b)
	}
	return v, true
}

// Decode reverses Encode. It exists so the encoder can be verified by
// round-trip in tests; Marlin's own heatshrink_decoder is the real
// consumer on the device.
func Decode(compressed []byte, cfg Config, outSizeHint int) []byte {
	r := &bitReader{in: compressed}
	out := make([]byte, 0, outSizeHint)
	for outSizeHint <= 0 || len(out) < outSizeHint {
		tag, ok := r.readBit()
		if !ok {
			break
		}
		if tag == 1 {
			v, ok := r.readBits(8)
			if !ok {
				break
			}
			out = append(out, byte(v))
		} else {
			off, ok := r.readBits(cfg.WindowBits)
			if !ok {
				break
			}
			ln, ok := r.readBits(cfg.LookaheadBits)
			if !ok {
				break
			}
			offset := int(off) + 1
			length := int(ln) + 1
			start := len(out) - offset
			if start < 0 {
				break
			}
			for k := 0; k < length; k++ {
				out = append(out, out[start+k])
			}
		}
	}
	if outSizeHint > 0 && len(out) > outSizeHint {
		out = out[:outSizeHint]
	}
	return out
}
