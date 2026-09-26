package gcode

import "testing"

func TestChecksumKnownValue(t *testing.T) {
	// "N1 M110 N0" -> checksum 46 is a commonly cited reference value for
	// the classic "N0 M110 N0*125" style framing; verify our XOR matches
	// a hand-computed value instead of trusting an external reference.
	body := "N3 G28*"
	body = body[:len(body)-1] // "N3 G28"
	var want byte
	for i := 0; i < len(body); i++ {
		want ^= body[i]
	}
	got := Checksum(body)
	if got != want {
		t.Fatalf("Checksum(%q) = %d, want %d", body, got, want)
	}
}

func TestFrameLineRoundTrip(t *testing.T) {
	line := FrameLine(42, "G1 X10 Y10")
	// Expect "N42 G1 X10 Y10*<cs>\n"
	want := "N42 G1 X10 Y10"
	cs := Checksum(want)
	expected := want + "*" + itoa(int(cs)) + "\n"
	if line != expected {
		t.Fatalf("FrameLine = %q, want %q", line, expected)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func TestParseLineOK(t *testing.T) {
	r := ParseLine("ok N5 P30 B15\n")
	if r.Kind != KindOK {
		t.Fatalf("kind = %v, want KindOK", r.Kind)
	}
	if !r.HasAdv || r.OKLine != 5 || r.Planner != 30 || r.Buffer != 15 {
		t.Fatalf("parsed = %+v", r)
	}
}

func TestParseLinePlainOK(t *testing.T) {
	r := ParseLine("ok\n")
	if r.Kind != KindOK || r.HasAdv {
		t.Fatalf("parsed = %+v", r)
	}
}

func TestParseLineResend(t *testing.T) {
	r := ParseLine("Resend: 17\n")
	if r.Kind != KindResend || r.Resend != 17 {
		t.Fatalf("parsed = %+v", r)
	}
}

func TestParseLineError(t *testing.T) {
	r := ParseLine("Error:Line Number is not Last Line Number+1, Last Line: 4\n")
	if r.Kind != KindError {
		t.Fatalf("parsed = %+v", r)
	}
}

func TestParseLineBusy(t *testing.T) {
	r := ParseLine("echo:busy: processing\n")
	if r.Kind != KindBusy {
		t.Fatalf("parsed = %+v", r)
	}
}

func TestParseLineStart(t *testing.T) {
	r := ParseLine("start\n")
	if r.Kind != KindStart {
		t.Fatalf("parsed = %+v", r)
	}
}

func TestIsEmergency(t *testing.T) {
	cases := map[string]bool{
		"M112":        true,
		"M112 ":       true,
		"M108":        true,
		"M410":        true,
		"M876 S0":     true,
		"G28":         false,
		"M104 S200":   false,
		"M1120":       false, // must not match as prefix of M112
	}
	for cmd, want := range cases {
		if got := IsEmergency(cmd); got != want {
			t.Errorf("IsEmergency(%q) = %v, want %v", cmd, got, want)
		}
	}
}
