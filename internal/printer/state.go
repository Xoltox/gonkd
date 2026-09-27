package printer

import "time"

// Version is gonkd's own version string, reported in Snapshot and in the
// OctoPrint-compatible /api/version text. Release builds set it from the git
// tag with -ldflags "-X github.com/Xoltox/gonkd/internal/printer.Version=...".
var Version = "0.1.0"

// hotendMaxC and bedMaxC are the heat limits enforced by handleHeat/handleTune
// and reported in Snapshot.Limits so the UI can validate before it even
// sends a request. They are fixed rather than read from the printer: this
// firmware build does not report its own thermal limits over serial.
const (
	hotendMaxC = 260.0
	bedMaxC    = 110.0
)

// State is the coarse job state exposed to the HTTP API and UI.
type State string

const (
	StateDisconnected State = "disconnected"
	StateConnecting   State = "connecting"
	StateIdle         State = "idle"
	StateUploading    State = "uploading"
	StatePrinting     State = "printing"
	StatePaused       State = "paused"
	StateCancelling   State = "cancelling"
	StateError        State = "error"
)

// UploadMode selects how a newly received file reaches the printer.
type UploadMode string

const (
	ModeSDUpload UploadMode = "sd"     // default: transfer to printer SD, then M23/M24
	ModeStream   UploadMode = "stream" // stream line-by-line over serial, no SD file created
)

// Temps holds the latest actual/target readings, as reported by
// AUTO_REPORT_TEMPERATURES (M155) or an M105 poll.
type Temps struct {
	HotendActual float64   `json:"hotendActual"`
	HotendTarget float64   `json:"hotendTarget"`
	BedActual    float64   `json:"bedActual"`
	BedTarget    float64   `json:"bedTarget"`
	FanPercent   int       `json:"fanPercent"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// SDFile is one entry from an M20 L (long-name) listing, cross-referenced
// against the local NameMap so the UI can show the original long name.
type SDFile struct {
	Short string  `json:"short"`
	Long  string  `json:"long"`
	Bytes int64   `json:"bytes"`
	Meta  *SDMeta `json:"meta,omitempty"` // parsed from slicer comments during upload
}

// Job describes the currently active (or most recently finished) print.
type Job struct {
	Filename   string     `json:"filename"`
	Mode       UploadMode `json:"mode"`
	TotalBytes int64      `json:"totalBytes"`
	SentBytes  int64      `json:"sentBytes"`
	Progress   float64    `json:"progress"` // 0..100
	StartedAt  time.Time  `json:"startedAt"`
	ElapsedSec float64    `json:"elapsedSec"`
	ETASec     float64    `json:"etaSec"`
	BabystepMM float64    `json:"babystepMm"`     // running total since last M500
	Note       string     `json:"note,omitempty"` // e.g. link lost while an SD print may still run

	// Layer/LayerTotal/Z/EtaSource are filled in by Snapshot from the
	// file's recorded meta tables (see MetaStore, metaParser) cross
	// referenced against the current card-space byte offset; they stay
	// zero (and omitted) for a job gonkd has no metadata for, e.g. one
	// started on the printer's own LCD/console.
	Layer      int     `json:"layer,omitempty"`      // 1-based
	LayerTotal int     `json:"layerTotal,omitempty"` // from meta.layerCount
	Z          float64 `json:"z,omitempty"`          // planned Z at the current offset, from the layer table
	EtaSource  string  `json:"etaSource,omitempty"`  // "m73", "slicer" or "live"

	// short is the SD short name of the file this job printed, used to
	// look up its recorded meta tables (layers/m73) at Snapshot time. Not
	// set for a job gonkd did not start itself (see lcdJobName).
	short string
	// firstByteAt is when SentBytes first became > 0 during an SD print
	// (setSDProgress), i.e. once Marlin is actually reading the card
	// rather than still heating; ETA's "live"/"slicer" fallbacks count
	// elapsed time from here, not from StartedAt.
	firstByteAt time.Time
}

// Tune holds the live speed/flow/fan overrides gonkd last sent, defaulting
// to 100/100/0 (Marlin's own defaults) whenever a printer connects.
type Tune struct {
	Speed int `json:"speed"`
	Flow  int `json:"flow"`
	Fan   int `json:"fan"`
}

// Limits are the heat targets handleHeat/handleTune accept, echoed in
// Snapshot so the UI never has to hardcode them.
type Limits struct {
	HotendMax float64 `json:"hotendMax"`
	BedMax    float64 `json:"bedMax"`
}

// UserWait describes Marlin blocked at an M0/M1 (or a HOST_PROMPT_SUPPORT
// dialog it opened), waiting for the user to confirm via
// POST /gonkd/job/continue. See Manager.scanUserWait.
type UserWait struct {
	Since   time.Time `json:"since"`
	Message string    `json:"message"`
}

// Leveling describes an in-progress manual mesh probe (G29 S1/S2), reported
// in Snapshot while active so any client watching /gonkd/status sees the
// wizard's progress, not just the one that started it. Point/Total come
// straight from Marlin's own "MBL G29 point N of TOTAL" line (mbl/G29.cpp),
// so they always match what the firmware thinks, including on a firmware
// rebuild with a different GRID_MAX_POINTS.
type Leveling struct {
	Point int     `json:"point"`
	Total int     `json:"total"`
	Z     float64 `json:"z"`
}

// Position is the toolhead's last confirmed position, parsed from an M114
// reply ("X:.. Y:.. Z:.. E:.. Count ..."), sent on demand by POST
// /gonkd/position and automatically after a jog, home, corner move or mesh
// Z adjust.
type Position struct {
	X  float64   `json:"x"`
	Y  float64   `json:"y"`
	Z  float64   `json:"z"`
	E  float64   `json:"e"`
	At time.Time `json:"at"`
}

// Snapshot is the full point-in-time state returned by the JSON API.
type Snapshot struct {
	Version      string    `json:"version"`
	State        State     `json:"state"`
	Connected    bool      `json:"connected"`
	Temps        Temps     `json:"temps"`
	Job          *Job      `json:"job,omitempty"`
	Capabilities []string  `json:"capabilities,omitempty"`
	LastError    string    `json:"lastError,omitempty"`
	Firmware     string    `json:"firmware,omitempty"`
	Tune         Tune      `json:"tune"`
	Limits       Limits    `json:"limits"`
	UserWait     *UserWait `json:"userWait,omitempty"`
	Leveling     *Leveling `json:"leveling,omitempty"`
	Position     *Position `json:"position,omitempty"`
}

// Mesh is the parsed reply to "G29 S0" (mbl/G29.cpp MeshReport case): the
// mesh either has no data yet (Active false, Points nil, before the first
// full G29 S1/S2 cycle finishes) or is a complete GRID_MAX_POINTS_X x
// GRID_MAX_POINTS_Y grid. Points is row-major, Points[y][x], matching the
// order Marlin's print_2d_array (bedlevel.cpp) prints it in.
type Mesh struct {
	Active  bool        `json:"active"`
	ZOffset float64     `json:"zOffset"`
	Points  [][]float64 `json:"points,omitempty"`
	Min     float64     `json:"min,omitempty"`
	Max     float64     `json:"max,omitempty"`
	Range   float64     `json:"range,omitempty"`
	// Cached is true when this is the Manager's last parsed G29 S0 result,
	// served in place of a live probe (refused with ErrJobActive) while a
	// job is running.
	Cached bool `json:"cached,omitempty"`
}
