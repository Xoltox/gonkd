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

func TestParseFileListLine(t *testing.T) {
	f, ok := ParseFileListLine(`BENCHY~1.GCO 123456 "Benchy Test.gcode"`)
	if !ok {
		t.Fatal("expected ok")
	}
	if f.Short != "BENCHY~1.GCO" || f.Bytes != 123456 || f.Long != "Benchy Test.gcode" {
		t.Fatalf("f = %+v", f)
	}
}

func TestParseFileListLineSkipsHeaders(t *testing.T) {
	if _, ok := ParseFileListLine("Begin file list"); ok {
		t.Fatal("should skip header")
	}
	if _, ok := ParseFileListLine("End file list"); ok {
		t.Fatal("should skip footer")
	}
}

func TestParseCapability(t *testing.T) {
	name, enabled, ok := ParseCapability("Cap:AUTOREPORT_TEMP:1")
	if !ok || name != "AUTOREPORT_TEMP" || !enabled {
		t.Fatalf("name=%q enabled=%v ok=%v", name, enabled, ok)
	}
}
