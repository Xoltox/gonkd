package printer

import "testing"

func TestParseTemps(t *testing.T) {
	temps, ok := ParseTemps("T:210.12 /210.00 B:60.03 /60.00 @:127 B@:80")
	if !ok {
		t.Fatal("expected ok")
	}
	if temps.HotendActual != 210.12 || temps.HotendTarget != 210.00 {
		t.Fatalf("hotend = %+v", temps)
	}
	if temps.BedActual != 60.03 || temps.BedTarget != 60.00 {
		t.Fatalf("bed = %+v", temps)
	}
}

func TestParseTempsNoMatch(t *testing.T) {
	if _, ok := ParseTemps("ok\n"); ok {
		t.Fatal("expected no match")
	}
}

func TestParseSDStatusPrinting(t *testing.T) {
	s, ok := ParseSDStatus("SD printing byte 12345/67890")
	if !ok || !s.Printing || s.Current != 12345 || s.Total != 67890 {
		t.Fatalf("s = %+v ok=%v", s, ok)
	}
}

func TestParseSDStatusNotPrinting(t *testing.T) {
	s, ok := ParseSDStatus("Not SD printing")
	if !ok || !s.NotSD {
		t.Fatalf("s = %+v ok=%v", s, ok)
	}
}

func TestParseTempsAutoreportLeadingSpace(t *testing.T) {
	temps, ok := ParseTemps(" T:20.00 /0.00 B:21.50 /0.00 @:0 B@:0\n")
	if !ok || temps.HotendActual != 20 || temps.BedActual != 21.5 {
		t.Fatalf("temps=%+v ok=%v", temps, ok)
	}
}

func TestParseTempsM105Reply(t *testing.T) {
	temps, ok := ParseTemps("ok T:210.00 /210.00 B:60.00 /60.00 @:127 B@:80")
	if !ok || temps.HotendTarget != 210 || temps.BedTarget != 60 {
		t.Fatalf("temps=%+v ok=%v", temps, ok)
	}
}

func TestParseTempsIgnoresEmbeddedT(t *testing.T) {
	for _, in := range []string{
		"echo:Now fresh file: T:1.GCO",
		"BENCHY~1.GCO 123 My T:1 B:2 part.gcode",
		"ok N5 P15 B16",
	} {
		if _, ok := ParseTemps(in); ok {
			t.Fatalf("%q must not parse as temps", in)
		}
	}
}

func TestParseFileListLineUnquotedLongName(t *testing.T) {
	f, ok := ParseFileListLine("BENCHY~1.GCO 123456 Benchy Test.gcode\n")
	if !ok || f.Short != "BENCHY~1.GCO" || f.Bytes != 123456 || f.Long != "Benchy Test.gcode" {
		t.Fatalf("f=%+v ok=%v", f, ok)
	}
	f, ok = ParseFileListLine("cube.gco 2048 CUBE.GCO")
	if !ok || f.Short != "CUBE.GCO" || f.Long != "CUBE.GCO" {
		t.Fatalf("f=%+v ok=%v", f, ok)
	}
	f, ok = ParseFileListLine("NOEXT 10")
	if !ok || f.Short != "NOEXT" || f.Long != "" {
		t.Fatalf("f=%+v ok=%v", f, ok)
	}
}

func TestParseFileListLineRejectsNonFiles(t *testing.T) {
	for _, in := range []string{
		"Begin file list",
		"End file list",
		"",
		"T:210.00 /210.00 B:60.00 /60.00 @:127 B@:80",
		" T:20.00 /0.00 B:20.00 /0.00 @:0 B@:0",
		"ok N5 P15 B16",
		"ok",
		"SD printing byte 10/200",
		"SUBDIR/PART.GCO 99 Sub Dir/part.gcode",
		"TOOLONGNAME.GCO 10 x",
		"NAME.GCODE 10 x",
		"echo:busy: processing",
	} {
		if f, ok := ParseFileListLine(in); ok {
			t.Fatalf("%q parsed as file %+v", in, f)
		}
	}
}

func TestParseCapability(t *testing.T) {
	name, enabled, ok := ParseCapability("Cap:AUTOREPORT_TEMP:1")
	if !ok || name != "AUTOREPORT_TEMP" || !enabled {
		t.Fatalf("name=%q enabled=%v ok=%v", name, enabled, ok)
	}
}

func TestParseFirmwareName(t *testing.T) {
	line := "FIRMWARE_NAME:Marlin 2.1.2.7 (Github) SOURCE_CODE_URL:https://github.com/MarlinFirmware/Marlin PROTOCOL_VERSION:1.0 MACHINE_TYPE:Creality EXTRUDER_COUNT:1 UUID:cede2a2f-41a2-4748-9b12-c55c62f367ff"
	name, ok := ParseFirmwareName(line)
	if !ok || name != "Marlin 2.1.2.7" {
		t.Fatalf("name=%q ok=%v", name, ok)
	}
}

func TestParseFirmwareNameNoMatch(t *testing.T) {
	if _, ok := ParseFirmwareName("ok"); ok {
		t.Fatal("expected no match")
	}
}
