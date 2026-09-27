package heatshrink

import (
	"bytes"
	"math/rand"
	"testing"
)

func roundTrip(t *testing.T, data []byte) {
	t.Helper()
	cfg := DefaultConfig()
	enc, err := Encode(data, cfg)
	if err != nil {
		t.Fatal(err)
	}
	dec := Decode(enc, cfg)
	if !bytes.Equal(dec, data) {
		t.Fatalf("round trip mismatch: len(in)=%d len(out)=%d", len(data), len(dec))
	}
}

func TestRoundTripEmpty(t *testing.T) {
	roundTrip(t, []byte{})
}

func TestRoundTripShort(t *testing.T) {
	roundTrip(t, []byte("G1 X10 Y10 F1500"))
	roundTrip(t, []byte("G"))
}

func TestRoundTripRepeated(t *testing.T) {
	roundTrip(t, bytes.Repeat([]byte("G1 X1.234 Y5.678 E0.03210\n"), 200))
	roundTrip(t, bytes.Repeat([]byte("A"), 5000)) // overlapping backreferences
}

func TestRoundTripBinaryLike(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	data := make([]byte, 50000)
	pattern := "G1 X0.00 Y0.00 E0.0 F1500\n"
	for i := range data {
		if i%7 == 0 {
			data[i] = byte(r.Intn(256))
		} else {
			data[i] = pattern[i%len(pattern)]
		}
	}
	roundTrip(t, data)
	r.Read(data)
	roundTrip(t, data)
}

func TestRoundTripAllByteValues(t *testing.T) {
	data := make([]byte, 256)
	for i := range data {
		data[i] = byte(i)
	}
	roundTrip(t, data)
}

// Writes in odd pieces must give the same stream as one Write.
func TestEncoderStreaming(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	var src bytes.Buffer
	for i := 0; i < 4000; i++ {
		src.WriteString("G1 X")
		src.WriteByte(byte('0' + r.Intn(10)))
		src.WriteString(".5 Y12.25 E0.0312\n")
	}
	whole, _ := Encode(src.Bytes(), DefaultConfig())
	var out bytes.Buffer
	enc, err := NewEncoder(&out, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	for b := src.Bytes(); len(b) > 0; {
		n := min(len(b), 1+r.Intn(300))
		enc.Write(b[:n])
		b = b[n:]
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), whole) {
		t.Fatal("streamed output differs from one-shot output")
	}
	if enc.Consumed() != int64(src.Len()) {
		t.Fatalf("Consumed = %d", enc.Consumed())
	}
	if len(whole)*10 > src.Len()*6 {
		t.Fatalf("G-code compressed only to %d of %d bytes", len(whole), src.Len())
	}
}

func TestConfigValidate(t *testing.T) {
	if _, err := NewEncoder(&bytes.Buffer{}, Config{WindowBits: 8, LookaheadBits: 8}); err == nil {
		t.Fatal("lookahead >= window accepted")
	}
}
