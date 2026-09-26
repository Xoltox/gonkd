package heatshrink

import (
	"bytes"
	"math/rand"
	"testing"
)

func roundTrip(t *testing.T, data []byte) {
	t.Helper()
	cfg := DefaultConfig()
	enc := Encode(data, cfg)
	dec := Decode(enc, cfg, len(data))
	if !bytes.Equal(dec, data) {
		t.Fatalf("round trip mismatch: len(in)=%d len(out)=%d", len(data), len(dec))
	}
}

func TestRoundTripEmpty(t *testing.T) {
	roundTrip(t, []byte{})
}

func TestRoundTripShort(t *testing.T) {
	roundTrip(t, []byte("G1 X10 Y10 F1500"))
}

func TestRoundTripRepeated(t *testing.T) {
	roundTrip(t, bytes.Repeat([]byte("G1 X1.234 Y5.678 E0.03210\n"), 200))
}

func TestRoundTripBinaryLike(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	data := make([]byte, 5000)
	pattern := "G1 X0.00 Y0.00 E0.0 F1500\n"
	// Mix of repetitive g-code-ish text and random bytes, since real
	// gcode compresses well but shouldn't be *only* tested on best case.
	for i := range data {
		if i%7 == 0 {
			data[i] = byte(r.Intn(256))
		} else {
			data[i] = pattern[i%len(pattern)]
		}
	}
	roundTrip(t, data)
}

func TestEncodeShrinksRepetitiveData(t *testing.T) {
	data := bytes.Repeat([]byte("AAAAAAAAAAAAAAAA"), 500)
	enc := Encode(data, DefaultConfig())
	if len(enc) >= len(data) {
		t.Fatalf("expected compression: in=%d out=%d", len(data), len(enc))
	}
}

func TestRoundTripAllByteValues(t *testing.T) {
	data := make([]byte, 256)
	for i := range data {
		data[i] = byte(i)
	}
	roundTrip(t, data)
}
