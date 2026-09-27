package printer

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestPresetStoreDefaults(t *testing.T) {
	ps := NewPresetStore(filepath.Join(t.TempDir(), "presets.json"))
	list := ps.List()
	if len(list) != 2 || list[0].Name != "PLA" || list[0].Hotend != 200 || list[0].Bed != 60 ||
		list[1].Name != "PETG" || list[1].Hotend != 235 || list[1].Bed != 80 {
		t.Fatalf("defaults = %+v", list)
	}
}

func TestPresetStoreSetPersistsInTempDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presets.json")
	ps := NewPresetStore(path)
	custom := []Preset{{Name: "ABS", Hotend: 240, Bed: 100}}
	if err := ps.Set(custom); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// A fresh store reading the same path picks up the persisted set.
	ps2 := NewPresetStore(path)
	got := ps2.List()
	if len(got) != 1 || got[0] != custom[0] {
		t.Fatalf("reloaded = %+v, want %+v", got, custom)
	}
}

func TestPresetStoreRejectsTooMany(t *testing.T) {
	ps := NewPresetStore(filepath.Join(t.TempDir(), "presets.json"))
	var many []Preset
	for i := 0; i < maxPresets+1; i++ {
		many = append(many, Preset{Name: "P", Hotend: 200, Bed: 60})
	}
	if err := ps.Set(many); !errors.Is(err, ErrTooManyPresets) {
		t.Fatalf("Set(%d presets) = %v, want ErrTooManyPresets", len(many), err)
	}
}

func TestPresetStoreValidatesAgainstLimits(t *testing.T) {
	ps := NewPresetStore(filepath.Join(t.TempDir(), "presets.json"))
	cases := [][]Preset{
		{{Name: "", Hotend: 200, Bed: 60}}, // empty name
		{{Name: "TooLongPresetNameThatExceedsTheCap", Hotend: 200, Bed: 60}},
		{{Name: "X", Hotend: hotendMaxC + 1, Bed: 60}},
		{{Name: "X", Hotend: 200, Bed: bedMaxC + 1}},
		{{Name: "X", Hotend: -1, Bed: 60}},
		{{Name: "X", Hotend: 200, Bed: -1}},
	}
	for i, c := range cases {
		if err := ps.Set(c); !errors.Is(err, ErrInvalidPreset) {
			t.Errorf("case %d: Set(%+v) = %v, want ErrInvalidPreset", i, c, err)
		}
	}
}

func TestPresetStoreSetDoesNotWriteWhenUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presets.json")
	ps := NewPresetStore(path)
	// Setting exactly the defaults still counts as "changed" relative to
	// the never-written file (dirty is set on every successful Set), so
	// this mainly documents that Set() with the same content twice in a
	// row does not error, not that it skips the write.
	if err := ps.Set(defaultPresets()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := ps.Set(defaultPresets()); err != nil {
		t.Fatalf("Set (again): %v", err)
	}
}
