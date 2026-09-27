package printer

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SDMeta is metadata parsed from a sliced G-code file's own comments during
// upload, stored alongside the file (see MetaStore) so the UI can show it
// without asking the printer. Every field is omitempty: a slicer that
// didn't write a given comment just leaves it out.
type SDMeta struct {
	Thumb       bool    `json:"thumb,omitempty"`
	EstSec      int     `json:"estSec,omitempty"`
	FilamentG   float64 `json:"filamentG,omitempty"`
	FilamentMm  float64 `json:"filamentMm,omitempty"`
	LayerHeight float64 `json:"layerHeight,omitempty"`
	Nozzle      float64 `json:"nozzle,omitempty"`
	HotendC     float64 `json:"hotendC,omitempty"`
	BedC        float64 `json:"bedC,omitempty"`
	Slicer      string  `json:"slicer,omitempty"`

	// Pauses lists where this file asks the printer to stop for the user
	// (M0/M1/M600/M601/M25/@pause), in file order, capped at maxPauses.
	Pauses []Pause `json:"pauses,omitempty"`
	// Unsupported lists which pause commands this firmware build ignores
	// (no ADVANCED_PAUSE_FEATURE: M600/M601 print "Unknown command" and the
	// print keeps going), deduplicated in first-seen order.
	Unsupported []string `json:"unsupported,omitempty"`

	// LayerCount is the total number of layers found during the scan; it
	// is small and safe to show in the /gonkd/files listing, unlike the
	// Layers/M73 tables below.
	LayerCount int `json:"layerCount,omitempty"`

	// Layers and M73 are the server-side ETA/layer tables recorded during
	// the upload scan (see metaParser.addLayer/addM73). They can hold up
	// to layerTableCap/m73TableCap entries and are only ever read
	// server-side (Manager.applyPrintProgress): the /gonkd/files listing
	// strips them before responding (Manager.SDFiles) to keep that
	// response small.
	Layers []LayerPoint `json:"layers,omitempty"`
	M73    []M73Point   `json:"m73,omitempty"`
}

// LayerPoint is one layer boundary found during the upload scan: the
// card-space byte offset (matching M27 "SD printing byte X/Y") where the
// layer begins, and its Z height.
type LayerPoint struct {
	O int64   `json:"o"`
	Z float64 `json:"z"`
}

// M73Point is one "M73 P.. R.." progress checkpoint found during the
// upload scan: the card-space byte offset, the slicer's reported percent
// and its remaining time in seconds (R is minutes in the G-code).
type M73Point struct {
	O int64 `json:"o"`
	P int   `json:"p"`
	R int   `json:"r"`
}

// Pause is one point in a sliced file where it asks the printer to stop for
// the user, found during the upload scan (metaParser.feedCommand).
type Pause struct {
	Offset int64  `json:"offset"`          // byte offset of the command's line
	Layer  int    `json:"layer,omitempty"` // slicer layer number, if seen
	Cmd    string `json:"cmd"`             // M0, M1, M600, M601, M25 or @pause
	Msg    string `json:"msg,omitempty"`   // the M0/M1 argument, or the preceding M117 text
}

// maxPauses caps how many Pause entries a file's metadata keeps; a file
// with more pause commands than this is unusual and the rest add little.
const maxPauses = 50

// thumbBlock accumulates one "; thumbnail begin WxH LEN" .. "; thumbnail
// end" block's base64 body while it is being read.
type thumbBlock struct {
	w, h int
	b64  strings.Builder
}

// metaParser is fed every raw source line of an uploading file, in order,
// and extracts SDMeta plus (at most) one thumbnail PNG. It costs almost
// nothing on the vast majority of lines (plain G-code body): feed()
// bails out after a cheap prefix check, and once both heater temperatures
// are known the command-line fallback path is skipped entirely.
type metaParser struct {
	meta      SDMeta
	curThumb  *thumbBlock
	bestThumb *thumbBlock
	hotendSet bool
	bedSet    bool

	offset             int64  // card-space bytes written so far, for Pause.Offset/Layers/M73 (set by feed's cardOffset)
	layer              int    // current slicer layer, tracked from LAYER_CHANGE/LAYER:/Z: comments
	pendingLayerChange bool   // a LAYER_CHANGE or LAYER: comment was just seen, awaiting its Z:
	pendingLayerZ      bool   // a layer boundary was seen; the next Z: (or first G1 Z) records it
	lastM117           string // most recent M117 text, used as the pause message fallback

	layerStride, layerSeen int // addLayer's decimation state (see appendLayer)
	m73Stride, m73Seen     int // addM73's decimation state (see appendM73)
}

// layerTableCap and m73TableCap bound SDMeta.Layers/M73 (contract: 3000/
// 1000 entries, thinning evenly once full).
const (
	layerTableCap = 3000
	m73TableCap   = 1000
)

// maxThumbDim is the largest thumbnail width/height gonkd will keep; the UI
// only ever shows it at file-list thumbnail size.
const maxThumbDim = 300

func (p *metaParser) feed(line string, cardOffset int64) {
	p.offset = cardOffset // card-space, matching M27 "SD printing byte X/Y" (see driver.go's metaFeed)
	if p.curThumb != nil {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, ";") && strings.Contains(t, "thumbnail") && strings.Contains(t, "end") {
			p.finishThumb()
			return
		}
		p.curThumb.b64.WriteString(stripCommentPrefix(t))
		return
	}
	t := strings.TrimSpace(line)
	if len(t) == 0 || t[0] != ';' {
		p.feedCommand(t)
		return
	}
	body := stripCommentPrefix(t)
	if w, h, ok := parseThumbBegin(body); ok {
		p.curThumb = &thumbBlock{w: w, h: h}
		return
	}
	p.feedComment(body)
}

// parseThumbBegin parses "thumbnail begin WxH LEN" (the comment body,
// already stripped of its leading ';'). Variants like "thumbnail_QOI
// begin" (a non-PNG preview some slicers also emit) are deliberately not
// matched: the contract only asks for the PNG block.
func parseThumbBegin(body string) (w, h int, ok bool) {
	fields := strings.Fields(body)
	if len(fields) < 3 || fields[0] != "thumbnail" || fields[1] != "begin" {
		return 0, 0, false
	}
	x := strings.IndexByte(fields[2], 'x')
	if x < 0 {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(fields[2][:x])
	h, err2 := strconv.Atoi(fields[2][x+1:])
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

func (p *metaParser) finishThumb() {
	cur := p.curThumb
	p.curThumb = nil
	if cur.w > maxThumbDim || cur.h > maxThumbDim {
		return
	}
	if p.bestThumb != nil && p.bestThumb.w*p.bestThumb.h >= cur.w*cur.h {
		return
	}
	p.bestThumb = cur
}

func stripCommentPrefix(t string) string {
	return strings.TrimSpace(strings.TrimPrefix(t, ";"))
}

// feedComment reads one "; key = value" (or "; generated by X on ...")
// header/footer comment line. Slicer comments are consistently lower-case
// with a bare "=" separator, so a simple split covers Orca/Prusa/Cura.
func (p *metaParser) feedComment(body string) {
	if body == "" {
		return
	}
	// Layer markers: Orca/PrusaSlicer emit "LAYER_CHANGE" immediately
	// followed by "Z:<height>"; Cura emits "LAYER:<n>" (and its own
	// "Z:<height>") without a LAYER_CHANGE marker at all. pendingLayerChange
	// avoids double-counting a layer when both a change marker and its Z:
	// line are present.
	switch {
	case body == "LAYER_CHANGE":
		p.layer++
		p.pendingLayerChange = true
		p.pendingLayerZ = true
		return
	case strings.HasPrefix(body, "LAYER:"):
		if v, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(body, "LAYER:"))); err == nil {
			p.layer = v
		}
		p.pendingLayerChange = true
		p.pendingLayerZ = true
		return
	case strings.HasPrefix(body, "Z:"):
		if p.pendingLayerChange {
			p.pendingLayerChange = false
		} else {
			p.layer++
		}
		if p.pendingLayerZ {
			if v, ok := firstFloat(strings.TrimPrefix(body, "Z:")); ok {
				p.addLayer(v)
			}
			p.pendingLayerZ = false
		}
		return
	}
	if p.meta.Slicer == "" && strings.HasPrefix(body, "generated by") {
		rest := strings.TrimSpace(strings.TrimPrefix(body, "generated by"))
		if i := strings.Index(rest, " on "); i >= 0 {
			rest = rest[:i]
		}
		p.meta.Slicer = strings.TrimSpace(rest)
		return
	}
	eq := strings.IndexByte(body, '=')
	if eq < 0 {
		return
	}
	key := strings.ToLower(strings.TrimSpace(body[:eq]))
	val := strings.TrimSpace(body[eq+1:])
	switch {
	case strings.HasPrefix(key, "estimated printing time") && strings.Contains(key, "normal"):
		if s, ok := parseSlicerDuration(val); ok {
			p.meta.EstSec = s
		}
	case key == "filament used [g]":
		if v, ok := firstFloat(val); ok {
			p.meta.FilamentG = v
		}
	case key == "filament used [mm]":
		if v, ok := firstFloat(val); ok {
			p.meta.FilamentMm = v
		}
	case key == "layer_height":
		if v, ok := firstFloat(val); ok {
			p.meta.LayerHeight = v
		}
	case key == "nozzle_diameter":
		if v, ok := firstFloat(val); ok {
			p.meta.Nozzle = v
		}
	case key == "nozzle_temperature" || key == "temperature":
		if v, ok := firstFloat(val); ok && !p.hotendSet {
			p.meta.HotendC = v
			p.hotendSet = true
		}
	case key == "bed_temperature" || key == "bed_temperature_initial_layer":
		if v, ok := firstFloat(val); ok && !p.bedSet {
			p.meta.BedC = v
			p.bedSet = true
		}
	}
}

// feedCommand handles one non-comment source line: it records M117 as the
// pending pause message, records a pause point for M0/M1/M600/M601/M25/
// @pause, and -- as a fallback for files (or slicer versions) whose header
// comments don't carry a temperature -- the first M104/M109 sets the
// hotend target and the first M140/M190 the bed target, matching what
// Marlin itself would actually heat to.
func (p *metaParser) feedCommand(t string) {
	t = stripComment(t) // "M0 ; Pause and wait for click" has no message
	if t == "" {
		return
	}
	fields := strings.Fields(t)
	if len(fields) == 0 {
		return
	}
	word := strings.ToUpper(fields[0])
	rest := strings.TrimSpace(t[len(fields[0]):])
	switch {
	case word == "M117":
		p.lastM117 = rest
	case word == "M0" || word == "M1":
		if rest == "" {
			rest = p.lastM117
		}
		p.addPause(word, rest)
	case word == "M600" || word == "M601":
		p.addPause(word, p.lastM117)
		p.addUnsupported(word)
	case word == "M25":
		p.addPause(word, p.lastM117)
	case fields[0] == "@pause" || word == "@PAUSE":
		p.addPause("@pause", p.lastM117)
	case word == "M73":
		p.feedM73(fields)
	}

	// Cura's "LAYER:<n>" carries no Z: comment of its own; the layer's Z
	// is instead read off its first G1/G0 move that sets Z, same as the
	// slicer's own LCD progress display would.
	if p.pendingLayerZ && (word == "G1" || word == "G0") {
		if z, ok := gcodeZParam(fields); ok {
			p.addLayer(z)
			p.pendingLayerZ = false
		}
	}

	if p.hotendSet && p.bedSet {
		return
	}
	switch word {
	case "M104", "M109":
		if !p.hotendSet {
			if v, ok := sValue(fields); ok {
				p.meta.HotendC = v
				p.hotendSet = true
			}
		}
	case "M140", "M190":
		if !p.bedSet {
			if v, ok := sValue(fields); ok {
				p.meta.BedC = v
				p.bedSet = true
			}
		}
	}
}

// addPause appends one Pause at the parser's current offset/layer, capped
// at maxPauses.
func (p *metaParser) addPause(cmd, msg string) {
	if len(p.meta.Pauses) >= maxPauses {
		return
	}
	p.meta.Pauses = append(p.meta.Pauses, Pause{Offset: p.offset, Layer: p.layer, Cmd: cmd, Msg: msg})
}

// addUnsupported records cmd in Unsupported once, in first-seen order.
func (p *metaParser) addUnsupported(cmd string) {
	for _, c := range p.meta.Unsupported {
		if c == cmd {
			return
		}
	}
	p.meta.Unsupported = append(p.meta.Unsupported, cmd)
}

// addLayer records one LayerPoint at the parser's current (card-space)
// offset, decimated once meta.Layers passes layerTableCap: every stride-th
// point is kept, and the stride doubles (halving the buffer) each time it
// would otherwise overflow, so the table stays representative of the
// whole file rather than just its first layerTableCap layers.
func (p *metaParser) addLayer(z float64) {
	if p.layerStride == 0 {
		p.layerStride = 1
	}
	keep := p.layerSeen%p.layerStride == 0
	p.layerSeen++
	if !keep {
		return
	}
	p.meta.Layers = append(p.meta.Layers, LayerPoint{O: p.offset, Z: z})
	if len(p.meta.Layers) > layerTableCap {
		thinned := p.meta.Layers[:0]
		for i, v := range p.meta.Layers {
			if i%2 == 0 {
				thinned = append(thinned, v)
			}
		}
		p.meta.Layers = thinned
		p.layerStride *= 2
	}
}

// feedM73 parses "M73 P<percent> R<minutes>" and records an M73Point at
// the parser's current offset. P alone (no R) is still recorded with
// R left at 0 rather than skipped: some slicers only emit P.
func (p *metaParser) feedM73(fields []string) {
	var pct int
	var rem int
	haveP := false
	for _, f := range fields[1:] {
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case 'P', 'p':
			if v, err := strconv.Atoi(f[1:]); err == nil {
				pct, haveP = v, true
			}
		case 'R', 'r':
			if v, err := strconv.Atoi(f[1:]); err == nil {
				rem = v * 60
			}
		}
	}
	if !haveP {
		return
	}
	p.addM73(pct, rem)
}

// addM73 records one M73Point, decimated exactly like addLayer but against
// m73TableCap.
func (p *metaParser) addM73(pct, remSec int) {
	if p.m73Stride == 0 {
		p.m73Stride = 1
	}
	keep := p.m73Seen%p.m73Stride == 0
	p.m73Seen++
	if !keep {
		return
	}
	p.meta.M73 = append(p.meta.M73, M73Point{O: p.offset, P: pct, R: remSec})
	if len(p.meta.M73) > m73TableCap {
		thinned := p.meta.M73[:0]
		for i, v := range p.meta.M73 {
			if i%2 == 0 {
				thinned = append(thinned, v)
			}
		}
		p.meta.M73 = thinned
		p.m73Stride *= 2
	}
}

// gcodeZParam returns the Z parameter of a G0/G1 command, if any.
func gcodeZParam(fields []string) (float64, bool) {
	for _, f := range fields[1:] {
		if len(f) > 1 && (f[0] == 'Z' || f[0] == 'z') {
			v, err := strconv.ParseFloat(f[1:], 64)
			return v, err == nil
		}
	}
	return 0, false
}

func sValue(fields []string) (float64, bool) {
	for _, f := range fields[1:] {
		if len(f) > 1 && (f[0] == 'S' || f[0] == 's') {
			v, err := strconv.ParseFloat(f[1:], 64)
			return v, err == nil
		}
	}
	return 0, false
}

func firstFloat(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, ", \t"); i >= 0 {
		s = s[:i]
	}
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil
}

// parseSlicerDuration parses a slicer's "2h 1m 0s" style duration into
// seconds. It returns false if it found no unit at all.
func parseSlicerDuration(s string) (int, bool) {
	total, num, found := 0, 0, false
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= '0' && r <= '9':
			num = num*10 + int(r-'0')
		case r == 'h':
			total += num * 3600
			num, found = 0, true
		case r == 'm':
			total += num * 60
			num, found = 0, true
		case r == 's':
			total += num
			num, found = 0, true
		case r == ' ':
		default:
			return total, found
		}
	}
	return total, found
}

// result finalizes the parse: the accumulated metadata, and the decoded
// thumbnail PNG bytes if a usable one was found.
func (p *metaParser) result() (SDMeta, []byte) {
	meta := p.meta
	meta.LayerCount = p.layer
	if p.bestThumb == nil {
		return meta, nil
	}
	data, err := base64.StdEncoding.DecodeString(p.bestThumb.b64.String())
	if err != nil || len(data) == 0 {
		return meta, nil
	}
	meta.Thumb = true
	return meta, data
}

// metaCapBytes is the total on-flash budget for stored metadata + thumbnail
// PNGs (contract: 1MB, oldest evicted first). Flash on the target box is
// small (about 4.8MB free), so this is kept modest and enforced eagerly.
const metaCapBytes = 1 << 20

// MetaStore persists SDMeta (and an optional thumbnail PNG) per uploaded
// file, keyed by its 8.3 short name, under metaCapBytes total.
type MetaStore struct {
	mu  sync.Mutex
	dir string
}

func NewMetaStore(dir string) *MetaStore {
	return &MetaStore{dir: dir}
}

func (s *MetaStore) jsonPath(short string) string { return filepath.Join(s.dir, short+".json") }
func (s *MetaStore) pngPath(short string) string  { return filepath.Join(s.dir, short+".png") }

// Get returns the stored metadata for short, or nil if there is none (or it
// cannot be read).
func (s *MetaStore) Get(short string) *SDMeta {
	data, err := os.ReadFile(s.jsonPath(short))
	if err != nil {
		return nil
	}
	var m SDMeta
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	return &m
}

// ThumbPath returns the on-disk PNG path for short and true, if one is
// stored.
func (s *MetaStore) ThumbPath(short string) (string, bool) {
	p := s.pngPath(short)
	if _, err := os.Stat(p); err != nil {
		return "", false
	}
	return p, true
}

// Save stores meta (and png, if non-empty) for short, then evicts the
// oldest stored files until the directory is back under metaCapBytes.
func (s *MetaStore) Save(short string, meta SDMeta, png []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return err
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := writeSynced(s.jsonPath(short), data); err != nil {
		return err
	}
	if len(png) > 0 {
		if err := writeSynced(s.pngPath(short), png); err != nil {
			return err
		}
	} else {
		os.Remove(s.pngPath(short))
	}
	s.evictLocked()
	return nil
}

// Delete removes any stored metadata/thumbnail for short, e.g. once the
// file itself is deleted from the SD card.
func (s *MetaStore) Delete(short string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	os.Remove(s.jsonPath(short))
	os.Remove(s.pngPath(short))
}

// evictLocked removes the least-recently-written files until the directory
// is back under metaCapBytes. Called with mu held.
func (s *MetaStore) evictLocked() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	type fi struct {
		path string
		size int64
		mod  time.Time
	}
	var files []fi
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		total += info.Size()
		files = append(files, fi{filepath.Join(s.dir, e.Name()), info.Size(), info.ModTime()})
	}
	if total <= metaCapBytes {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files {
		if total <= metaCapBytes {
			break
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}
