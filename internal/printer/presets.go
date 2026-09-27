package printer

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Preset is one named hotend/bed temperature pair offered as a quick-heat
// shortcut in the UI.
type Preset struct {
	Name   string  `json:"name"`
	Hotend float64 `json:"hotend"`
	Bed    float64 `json:"bed"`
}

const (
	maxPresets       = 8
	maxPresetNameLen = 24
)

var (
	ErrTooManyPresets = errors.New("too many presets")
	ErrInvalidPreset  = errors.New("invalid preset")
)

// defaultPresets seeds a fresh install; the file on flash is written only
// once the user actually changes something (PUT /gonkd/presets).
func defaultPresets() []Preset {
	return []Preset{
		{Name: "PLA", Hotend: 200, Bed: 60},
		{Name: "PETG", Hotend: 235, Bed: 80},
	}
}

// PresetStore persists the heat presets shown in the UI. Flash is jffs2 on
// the target box, so it is only written when the set actually changes.
type PresetStore struct {
	mu    sync.Mutex
	path  string
	list  []Preset
	dirty bool
}

func NewPresetStore(path string) *PresetStore {
	p := &PresetStore{path: path, list: defaultPresets()}
	p.load()
	return p
}

func (p *PresetStore) load() {
	data, err := os.ReadFile(p.path)
	if err != nil {
		return
	}
	var list []Preset
	if err := json.Unmarshal(data, &list); err != nil {
		return
	}
	p.list = list
}

// List returns the current presets.
func (p *PresetStore) List() []Preset {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Preset, len(p.list))
	copy(out, p.list)
	return out
}

// Set validates and replaces the whole preset list, then saves it to disk
// if anything actually changed.
func (p *PresetStore) Set(list []Preset) error {
	if len(list) > maxPresets {
		return ErrTooManyPresets
	}
	clean := make([]Preset, len(list))
	for i, pr := range list {
		name := strings.TrimSpace(pr.Name)
		if name == "" || len(name) > maxPresetNameLen {
			return ErrInvalidPreset
		}
		if pr.Hotend < 0 || pr.Hotend > hotendMaxC || pr.Bed < 0 || pr.Bed > bedMaxC {
			return ErrInvalidPreset
		}
		clean[i] = Preset{Name: name, Hotend: pr.Hotend, Bed: pr.Bed}
	}
	p.mu.Lock()
	p.list = clean
	p.dirty = true
	p.mu.Unlock()
	return p.save()
}

func (p *PresetStore) save() error {
	p.mu.Lock()
	if !p.dirty {
		p.mu.Unlock()
		return nil
	}
	data, err := json.Marshal(p.list)
	p.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0755); err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := writeSynced(tmp, data); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p.path); err != nil {
		return err
	}
	p.mu.Lock()
	p.dirty = false
	p.mu.Unlock()
	return nil
}
