package printer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestShortNameBasic(t *testing.T) {
	m := NewNameMap(filepath.Join(t.TempDir(), "names.json"))
	short := m.Assign("Benchy - Test Print (v2).gcode")
	if len(short) > 12 { // 8 + '.' + 3
		t.Fatalf("short name too long: %q", short)
	}
	if filepath.Ext(short) != ".GCO" {
		t.Fatalf("expected .GCO extension, got %q", short)
	}
}

func TestShortNameCollisionsGetSuffixed(t *testing.T) {
	m := NewNameMap(filepath.Join(t.TempDir(), "names.json"))
	a := m.Assign("very long benchy name one.gcode")
	b := m.Assign("very long benchy name two.gcode")
	if a == b {
		t.Fatalf("collision not resolved: both got %q", a)
	}
}

func TestNameMapPersistsOnlyOnChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "names.json")
	m := NewNameMap(path)
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("expected no file written when nothing changed")
	}
	m.Assign("test.gcode")
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file to exist after change+save: %v", err)
	}
}

func TestNameMapReloadsLongName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "names.json")
	m := NewNameMap(path)
	short := m.Assign("my cool print.gcode")
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	m2 := NewNameMap(path)
	if got := m2.LongFor(short); got != "my cool print.gcode" {
		t.Fatalf("LongFor = %q, want original long name", got)
	}
}
