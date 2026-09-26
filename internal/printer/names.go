package printer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// NameEntry maps one long (uploaded) filename to the 8.3 short name Marlin
// actually stores it under. Marlin's SDSUPPORT + LONG_FILENAME_HOST_SUPPORT
// lets it report long names for files, but *creates* only 8.3 short names,
// so forge picks the short name on upload and remembers the mapping.
type NameEntry struct {
	Long  string `json:"long"`
	Short string `json:"short"` // 8.3, upper-case, as sent in M23/M30
}

// NameMap persists Long<->Short mappings to a small JSON file. Flash is
// jffs2 on the target box, so it only writes when the map actually changes.
type NameMap struct {
	mu      sync.Mutex
	path    string
	entries map[string]NameEntry // key: Short (unique on the SD card)
	dirty   bool
}

func NewNameMap(path string) *NameMap {
	m := &NameMap{path: path, entries: map[string]NameEntry{}}
	m.load()
	return m
}

func (m *NameMap) load() {
	data, err := os.ReadFile(m.path)
	if err != nil {
		return
	}
	var list []NameEntry
	if err := json.Unmarshal(data, &list); err != nil {
		return
	}
	for _, e := range list {
		m.entries[e.Short] = e
	}
}

// Save writes the map to disk only if it has changed since the last save.
func (m *NameMap) Save() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.dirty {
		return nil
	}
	list := make([]NameEntry, 0, len(m.entries))
	for _, e := range m.entries {
		list = append(list, e)
	}
	data, err := json.Marshal(list)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0755); err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := writeSynced(tmp, data); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, m.path); err != nil {
		return err
	}
	m.dirty = false
	return nil
}

// writeSynced writes data to path and fsyncs it before returning, so a power
// loss on jffs2 cannot leave the renamed file empty (SEC-33).
func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// LongFor returns the remembered long name for a short (8.3) name, or the
// short name itself if none is known.
func (m *NameMap) LongFor(short string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[strings.ToUpper(short)]; ok {
		return e.Long
	}
	return short
}

// Assign picks (and remembers) an 8.3 short name for a long name, avoiding
// collisions with names already known to this map.
func (m *NameMap) Assign(long string) string {
	return m.AssignAvoiding(long, nil)
}

// AssignAvoiding is Assign that also skips any candidate for which taken
// returns true, e.g. files already on the SD card that forge did not upload
// (SEC-16). taken may be nil. It runs with the map's lock held, so it must
// not call back into the NameMap.
func (m *NameMap) AssignAvoiding(long string, taken func(string) bool) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	short := shortNameFor(long, func(candidate string) bool {
		if _, exists := m.entries[candidate]; exists {
			return true
		}
		return taken != nil && taken(candidate)
	})
	m.entries[short] = NameEntry{Long: long, Short: short}
	m.dirty = true
	return short
}

// Forget removes a mapping, e.g. after the file is deleted from the SD card.
func (m *NameMap) Forget(short string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[strings.ToUpper(short)]; ok {
		delete(m.entries, strings.ToUpper(short))
		m.dirty = true
	}
}

// shortNameFor implements a DOS 8.3 "tilde" short-name algorithm similar to
// what Marlin/FatFs itself would generate: base name uppercased, invalid
// characters stripped, truncated to 6 chars + "~N" + a (possibly changed)
// 3-char extension, with N bumped on collision.
func shortNameFor(long string, taken func(string) bool) string {
	base := filepath.Base(long)
	ext := sanitize83(strings.TrimPrefix(filepath.Ext(base), "."))
	if len(ext) > 3 {
		ext = ext[:3]
	}
	// Marlin only lists files whose extension starts with G, and anything
	// odd here would make M28 and M23 target different names (SEC-16), so
	// only the usual G-code spellings survive; everything else becomes GCO.
	if ext != "G" && ext != "GCO" {
		ext = "GCO"
	}
	name := strings.TrimSuffix(base, filepath.Ext(base))
	name = sanitize83(name)
	if name == "" {
		name = "FILE"
	}

	// If it already fits and has no invalid chars, still enforce upper-case
	// and 8.3 length; only add ~N when it doesn't fit or collides.
	plain := fmt.Sprintf("%s.%s", truncate(name, 8), ext)
	if len(name) <= 8 && !taken(plain) {
		return plain
	}

	for n := 1; n < 10000; n++ {
		suffix := fmt.Sprintf("~%d", n)
		base := truncate(name, 8-len(suffix)) + suffix
		candidate := fmt.Sprintf("%s.%s", base, ext)
		if !taken(candidate) {
			return candidate
		}
	}
	// Extremely unlikely fallback.
	return fmt.Sprintf("F%07X.GCO", hash32(long)&0xFFFFFFF)
}

func sanitize83(s string) string {
	s = strings.ToUpper(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '-':
			b.WriteRune('_')
		default:
			// drop spaces, dots, unicode, punctuation, etc.
		}
	}
	return b.String()
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func hash32(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}
