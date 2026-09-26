package gcode

import "testing"

func TestChecksumKnownValue(t *testing.T) {
	// Hand-computed XOR over "N0 M110 N0":
	// 4E^30=7E ^20=5E ^4D=13 ^31=22 ^31=13 ^30=23 ^20=03 ^4E=4D ^30=7D = 125,
	// matching the widely used reset line "N0 M110 N0*125".
	if got := Checksum("N0 M110 N0"); got != 125 {
		t.Fatalf("Checksum = %d, want 125", got)
	}
}

func TestFrameLineLiteral(t *testing.T) {
	if got := FrameLine(0, "M110 N0"); got != "N0 M110 N0*125\n" {
		t.Fatalf("FrameLine = %q", got)
	}
}

func TestParseLineOK(t *testing.T) {
	r := ParseLine("ok N5 P30 B15\n")
	if r.Kind != KindOK {
		t.Fatalf("kind = %v, want KindOK", r.Kind)
	}
	if !r.HasN || !r.HasAdv || r.OKLine != 5 || r.Planner != 30 || r.Buffer != 15 {
		t.Fatalf("parsed = %+v", r)
	}
}

func TestParseLineOKN0(t *testing.T) {
	r := ParseLine("ok N0 P63 B15\n")
	if r.Kind != KindOK || !r.HasN || r.OKLine != 0 {
		t.Fatalf("parsed = %+v", r)
	}
}

func TestParseLinePlainOK(t *testing.T) {
	r := ParseLine("ok\n")
	if r.Kind != KindOK || r.HasAdv || r.HasN {
		t.Fatalf("parsed = %+v", r)
	}
}

func TestParseLineOKTemps(t *testing.T) {
	r := ParseLine("ok T:210.00 /210.00 B:60.00 /60.00 @:0 B@:0\n")
	if r.Kind != KindOK || r.HasN {
		t.Fatalf("parsed = %+v", r)
	}
}

func TestParseLineResend(t *testing.T) {
	for _, in := range []string{"Resend: 17\n", "Resend:17\r\n", "rs 17\n"} {
		r := ParseLine(in)
		if r.Kind != KindResend || r.Resend != 17 {
			t.Fatalf("%q parsed = %+v", in, r)
		}
	}
}

func TestParseLineError(t *testing.T) {
	r := ParseLine("Error:Line Number is not Last Line Number+1, Last Line: 4\n")
	if r.Kind != KindError {
		t.Fatalf("parsed = %+v", r)
	}
}

func TestParseLineStart(t *testing.T) {
	for _, in := range []string{"start\n", "\x00\xfestart\r\n"} {
		if r := ParseLine(in); r.Kind != KindStart {
			t.Fatalf("%q parsed = %+v", in, r)
		}
	}
	for _, in := range []string{"echo:start", "started", "START~1.GCO 12 start.gcode"} {
		if r := ParseLine(in); r.Kind == KindStart {
			t.Fatalf("%q must not be a start banner", in)
		}
	}
}

func TestIsEmergency(t *testing.T) {
	cases := map[string]bool{
		"M112":      true,
		"M112 ":     true,
		"M108":      true,
		"M410":      true,
		"M876 S0":   true,
		"G28":       false,
		"M104 S200": false,
		"M1120":     false, // must not match as prefix of M112
	}
	for cmd, want := range cases {
		if got := IsEmergency(cmd); got != want {
			t.Errorf("IsEmergency(%q) = %v, want %v", cmd, got, want)
		}
	}
}

func TestFramedLenMaxCoversFrameLine(t *testing.T) {
	cmd := "G1 X10 Y20"
	for _, n := range []int64{1, 9, 99999, 123456} {
		if got, max := len(FrameLine(n, cmd))-1, FramedLenMax(n, cmd); got > max {
			t.Fatalf("N%d: framed %d > bound %d", n, got, max)
		}
	}
}

func TestWireSafeAndCutComment(t *testing.T) {
	if CutComment("M117 a ; b") != "M117 a " || CutComment("G28") != "G28" {
		t.Fatal("CutComment")
	}
	backslash := string(rune(0x5c))
	for _, c := range []string{"M117 a;b", "M117 a" + backslash + "b", "M117 a\bb", "G1\x01"} {
		if WireSafe(c) {
			t.Errorf("WireSafe(%q) = true", c)
		}
	}
	if !WireSafe("M117 Layer\t(1/50) *x") {
		t.Error("WireSafe rejected a safe line")
	}
}
