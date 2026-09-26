package printer

import "time"

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
	HotendActual float64
	HotendTarget float64
	BedActual    float64
	BedTarget    float64
	FanPercent   int
	UpdatedAt    time.Time
}

// SDFile is one entry from an M20 L (long-name) listing, cross-referenced
// against the local NameMap so the UI can show the original long name.
type SDFile struct {
	Short string `json:"short"`
	Long  string `json:"long"`
	Bytes int64  `json:"bytes"`
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
}

// Snapshot is the full point-in-time state returned by the JSON API.
type Snapshot struct {
	State        State    `json:"state"`
	Connected    bool     `json:"connected"`
	Temps        Temps    `json:"temps"`
	Job          *Job     `json:"job,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	LastError    string   `json:"lastError,omitempty"`
}
