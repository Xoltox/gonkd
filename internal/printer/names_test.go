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

func TestShortNameExtensionSanitized(t *testing.T) {
	cases := map[string]string{
		"noext":        "NOEXT.GCO",
		"a.g c":        "A.GCO",
		"part.gcode":   "PART.GCO",
		"part.g":       "PART.G",
		"part.txt":     "PART.GCO",
		"part.gco":     "PART.GCO",
		"dir/x.GCODE":  "X.GCO",
		"weird.g*o":    "WEIRD.GCO",
		"trailingdot.": "TRAILI~1.GCO",
	}
	for long, want := range cases {
		m := NewNameMap(filepath.Join(t.TempDir(), "names.json"))
		if got := m.Assign(long); got != want {
			t.Errorf("Assign(%q) = %q, want %q", long, got, want)
		}
	}
}

func TestAssignAvoidingSkipsTakenNames(t *testing.T) {
	m := NewNameMap(filepath.Join(t.TempDir(), "names.json"))
	onCard := map[string]bool{"BENCHY.GCO": true}
	got := m.AssignAvoiding("benchy.gcode", func(s string) bool { return onCard[s] })
	if got == "BENCHY.GCO" {
		t.Fatalf("AssignAvoiding reused a name already on the card")
	}
	if got := m.Assign("other.gcode"); got != "OTHER.GCO" {
		t.Fatalf("Assign = %q, want OTHER.GCO", got)
	}
}
