package printer

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// This file implements manual mesh bed leveling (G29, MESH_BED_LEVELING)
// for the Ender 3 / Marlin 2.1.2.7 build gonkd targets: no probe,
// MANUAL_PROBE_START_Z 0.2, LCD_BED_LEVELING, a GRID_MAX_POINTS_X x
// GRID_MAX_POINTS_Y grid (firmware/Configuration.h; 5x5 on this build).
// See src/gcode/bedlevel/mbl/G29.cpp (Marlin source, read-only reference)
// for the state machine this drives.

// meshGridSize is GRID_MAX_POINTS_X (== _Y) on this firmware build
// (firmware/Configuration.h:2056-2057). It only bounds the request
// validation for MeshSetPoint; MeshReport's grid size always comes from
// parsing Marlin's own reply, so a firmware rebuild with a different grid
// is reflected there regardless of this constant.
const meshGridSize = 5

// bedSizeMM is X/Y_BED_SIZE (firmware/Configuration.h:1736-1737).
// cornerInsetMM keeps the nozzle a bit off the very edge of the glass,
// roughly where the bed's leveling screws sit.
const (
	bedSizeMM     = 235.0
	cornerInsetMM = 30.0
)

// meshCmdTimeout bounds one G29/G1 round trip: homing (part of "G29 S1 N")
// can take several seconds.
const meshCmdTimeout = 20 * time.Second

// meshSettle is how long meshSendCollect keeps listening after its done()
// condition first fires, so a position report line that follows the line
// done() matched (report_current_position runs at the very end of G29())
// is still captured.
const meshSettle = 200 * time.Millisecond

// zAdjustMin/Max bound one Z nudge during leveling (MESH_EDIT_Z_STEP is
// 0.025mm on this firmware; see firmware/Configuration.h:2070 and
// lcd/menu/menu_bed_leveling.cpp:119, the LCD's own equivalent control).
const (
	zAdjustMin = 0.025
	zAdjustMax = 1.0
)

// zOffsetRange bounds G29 S4's Z (LCD_PROBE_Z_RANGE is 4mm, so +/-2mm;
// firmware/Configuration.h:2071, menu_bed_leveling.cpp:120).
const zOffsetRange = 2.0

// MeshReport sends "G29 S0" and parses the reply (ParseMeshReport).
// Refused with ErrJobActive while a print or upload is running, like every
// other mesh/bed route.
func (m *Manager) MeshReport() (Mesh, error) {
	lines, err := m.meshSendCollect("G29 S0", meshCmdTimeout, containsAny("Mesh Bed Leveling has no data.", "ok"))
	if err != nil {
		return Mesh{}, err
	}
	active, zOffset, points, ok := ParseMeshReport(lines)
	if !ok {
		return Mesh{}, ErrTimeout
	}
	mesh := Mesh{Active: active, ZOffset: zOffset}
	if points != nil {
		mesh.Points = points
		mesh.Min, mesh.Max = meshMinMax(points)
		mesh.Range = mesh.Max - mesh.Min
	}
	return mesh, nil
}

func meshMinMax(points [][]float64) (min, max float64) {
	first := true
	for _, row := range points {
		for _, v := range row {
			if first {
				min, max, first = v, v, false
				continue
			}
			if v < min {
				min = v
			}
			if v > max {
				max = v
			}
		}
	}
	return min, max
}

// MeshLevelStart begins a fresh probe: "G29 S1 N" homes (Marlin injects
// G28 itself when N is present, then the first "G29 S2") and moves to
// point 1. It always (re)homes first: gonkd does not track "homed" state
// server-side, and re-homing is safe, just slower than skipping it.
func (m *Manager) MeshLevelStart() (Leveling, error) {
	return m.meshLevelStep("G29 S1 N", 30*time.Second)
}

// MeshLevelNext stores the Z the caller adjusted to for the current point
// and moves to the next one (or finishes on the last point).
func (m *Manager) MeshLevelNext() (Leveling, bool, error) {
	lines, err := m.meshSendCollect("G29 S2", meshCmdTimeout, containsAny("MBL G29 point", "Mesh probing done."))
	if err != nil {
		return Leveling{}, false, err
	}
	done := false
	for _, l := range lines {
		if ParseMeshDone(l) {
			done = true
			break
		}
	}
	lv := levelingFromLines(lines)
	m.mu.Lock()
	if done {
		m.leveling = nil
	} else {
		lv2 := lv
		m.leveling = &lv2
	}
	m.mu.Unlock()
	return lv, done, nil
}

func (m *Manager) meshLevelStep(cmd string, timeout time.Duration) (Leveling, error) {
	lines, err := m.meshSendCollect(cmd, timeout, containsAny("MBL G29 point"))
	if err != nil {
		return Leveling{}, err
	}
	lv := levelingFromLines(lines)
	m.mu.Lock()
	lv2 := lv
	m.leveling = &lv2
	m.mu.Unlock()
	return lv, nil
}

// levelingFromLines pulls the point/total (ParseMBLPointLine) and current
// Z (ParsePositionZ) out of a G29 S1/S2 reply. Marlin's own numbering
// ("MBL G29 point N of TOTAL") is used as-is, not a locally recomputed
// index, so it always matches the LCD/host prompt.
func levelingFromLines(lines []string) Leveling {
	var lv Leveling
	for _, l := range lines {
		if p, t, ok := ParseMBLPointLine(l); ok {
			lv.Point, lv.Total = p, t
		}
		if z, ok := ParsePositionZ(l); ok {
			lv.Z = z
		}
	}
	return lv
}

// MeshLevelAbort leaves the printer safe without finishing or discarding
// anything: soft endstops are left "loose" by Marlin mid-probe (G29.cpp),
// so this re-enables them (M211 S1) and lifts Z clear of the bed. The
// in-progress mesh data is not valid yet either way (Marlin only commits
// it, and re-enables leveling, on the call that records the last point),
// so there is nothing to discard.
func (m *Manager) MeshLevelAbort() error {
	m.mu.Lock()
	m.leveling = nil
	m.mu.Unlock()
	return m.meshSendSequence([]string{"M211 S1", "G1 Z10 F600"}, meshCmdTimeout)
}

// MeshLevelFinish ends a completed wizard. save true persists it
// (M420 S1 + M500, i.e. "keep this mesh"); save false discards the mesh
// just probed (G29 S5: reset and disable) instead, matching the wizard's
// Save/Discard choice.
func (m *Manager) MeshLevelFinish(save bool) error {
	m.mu.Lock()
	m.leveling = nil
	m.mu.Unlock()
	if !save {
		return m.meshSendSequence([]string{"G29 S5"}, meshCmdTimeout)
	}
	return m.meshSendSequence([]string{"M420 S1", "M500"}, meshCmdTimeout)
}

// MeshZAdjust nudges Z by deltaMM during leveling (a plain G1, not part of
// the G29 family, so it needs its own M114 to read the confirmed position
// back: G29 is the only command here that reports position on its own).
// deltaMM's magnitude is clamped to [zAdjustMin, zAdjustMax]; its sign is
// kept. Requires a leveling wizard in progress (Manager tracks the last Z
// G29 reported) so the move is relative to a known position.
func (m *Manager) MeshZAdjust(deltaMM float64) (float64, error) {
	if math.IsNaN(deltaMM) || deltaMM == 0 {
		return 0, ErrInvalidCommand
	}
	mag := clampF(math.Abs(deltaMM), zAdjustMin, zAdjustMax)
	if deltaMM < 0 {
		mag = -mag
	}
	m.mu.Lock()
	lv := m.leveling
	m.mu.Unlock()
	if lv == nil {
		return 0, ErrInvalidCommand
	}
	target := lv.Z + mag
	cmds := []string{fmt.Sprintf("G1 Z%s F300", formatMM(target)), "M114"}
	lines, err := m.meshSendCollectSeq(cmds, meshCmdTimeout, containsAny("X:"))
	if err != nil {
		return 0, err
	}
	z := target
	for _, l := range lines {
		if v, ok := ParsePositionZ(l); ok {
			z = v
		}
	}
	m.mu.Lock()
	if m.leveling != nil {
		m.leveling.Z = z
	}
	m.mu.Unlock()
	return z, nil
}

// MeshZOffset sets the mesh Z offset (G29 S4), optionally saving it
// (M500). The "save current babystep as Z offset" action (new zOffset =
// mesh zOffset + the job's babystep total) is computed by the caller: the
// job's babystep total only exists while a job is active, which is also
// when every mesh/bed route is refused (R-05-style), so the API layer
// reads it from the last snapshot rather than Manager reaching across
// that boundary here.
func (m *Manager) MeshZOffset(z float64, save bool) error {
	if math.IsNaN(z) {
		return ErrInvalidCommand
	}
	z = clampF(z, -zOffsetRange, zOffsetRange)
	cmds := []string{fmt.Sprintf("G29 S4 Z%s", formatMM(z))}
	if save {
		cmds = append(cmds, "M500")
	}
	return m.meshSendSequence(cmds, meshCmdTimeout)
}

// MeshSetPoint edits one mesh cell (G29 S3 Xi Yj Zz), optionally saving.
func (m *Manager) MeshSetPoint(x, y int, z float64, save bool) error {
	if x < 0 || x >= meshGridSize || y < 0 || y >= meshGridSize || math.IsNaN(z) {
		return ErrInvalidCommand
	}
	cmds := []string{fmt.Sprintf("G29 S3 I%d J%d Z%s", x, y, formatMM(z))}
	if save {
		cmds = append(cmds, "M500")
	}
	return m.meshSendSequence(cmds, meshCmdTimeout)
}

// Corner is one of the five bed-corner assistant targets.
type Corner string

const (
	CornerFrontLeft  Corner = "fl"
	CornerFrontRight Corner = "fr"
	CornerBackLeft   Corner = "bl"
	CornerBackRight  Corner = "br"
	CornerCenter     Corner = "center"
	// CornerDone ends corner leveling: lift and turn mesh compensation
	// back on.
	CornerDone Corner = "done"
)

// cornerXY returns the X/Y target for a corner, inset from the bed edge
// (bedSizeMM, cornerInsetMM) roughly to the leveling screw positions. Y=0
// is the front of the bed (nearest the display) on this machine.
func cornerXY(c Corner) (x, y float64, ok bool) {
	switch c {
	case CornerFrontLeft:
		return cornerInsetMM, cornerInsetMM, true
	case CornerFrontRight:
		return bedSizeMM - cornerInsetMM, cornerInsetMM, true
	case CornerBackLeft:
		return cornerInsetMM, bedSizeMM - cornerInsetMM, true
	case CornerBackRight:
		return bedSizeMM - cornerInsetMM, bedSizeMM - cornerInsetMM, true
	case CornerCenter:
		return bedSizeMM / 2, bedSizeMM / 2, true
	}
	return 0, 0, false
}

// BedCorner moves to one of the five corner-assistant positions: home only
// if needed (G28 O), turn mesh compensation off so Z0 is the raw bed (with
// the mesh on it would hide the very tilt the screws are meant to fix), lift
// clear, move over XY, then settle down onto the bed for the paper test.
// CornerDone lifts and turns the mesh back on (RESTORE_LEVELING_AFTER_G28
// would otherwise keep it off for the next print). Refused while a job runs,
// like every mesh/bed route.
func (m *Manager) BedCorner(c Corner) error {
	if c == CornerDone {
		return m.meshSendSequence([]string{"G1 Z5 F600", "M420 S1"}, 30*time.Second)
	}
	x, y, ok := cornerXY(c)
	if !ok {
		return ErrInvalidCommand
	}
	cmds := []string{
		"G28 O",
		"M420 S0",
		"G1 Z5 F600",
		fmt.Sprintf("G1 X%s Y%s F3000", formatMM(x), formatMM(y)),
		"G1 Z0 F300",
	}
	return m.meshSendSequence(cmds, 30*time.Second)
}

// --- low-level send/collect helpers ---

// meshSendSequence runs cmds through the normal SendSequence path (one
// caller at a time, refused with ErrJobActive during a job); it does not
// wait for or parse any reply.
func (m *Manager) meshSendSequence(cmds []string, timeout time.Duration) error {
	return m.SendSequence(cmds)
}

// meshSendCollect sends one command and polls Driver.ConsoleSince until
// done(lines-seen-so-far) reports true (plus a short settle window to
// catch a trailing line) or timeout elapses. It follows the same
// multi-line-reply pattern as SDFiles/RefreshFiles (M20 L), except that
// mesh replies are not otherwise cached by the Driver, so this collects
// them itself from the console backlog rather than a dedicated field.
func (m *Manager) meshSendCollect(cmd string, timeout time.Duration, done func([]string) bool) ([]string, error) {
	return m.meshSendCollectSeq([]string{cmd}, timeout, done)
}

func (m *Manager) meshSendCollectSeq(cmds []string, timeout time.Duration, done func([]string) bool) ([]string, error) {
	m.seq.Lock()
	defer m.seq.Unlock()
	drv, err := m.activeDriver()
	if err != nil {
		return nil, err
	}
	_, since := drv.ConsoleSince(math.MaxInt64)
	for _, c := range cmds {
		if err := drv.Send(c); err != nil {
			return nil, err
		}
	}
	deadline := time.Now().Add(timeout)
	var all []string
	settled := false
	var settleUntil time.Time
	for {
		lines, total := drv.ConsoleSince(since)
		since = total
		all = append(all, lines...)
		if !settled && done(all) {
			settled = true
			settleUntil = time.Now().Add(meshSettle)
		}
		if settled && time.Now().After(settleUntil) {
			return all, nil
		}
		if time.Now().After(deadline) {
			if settled {
				return all, nil
			}
			return all, ErrTimeout
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// containsAny returns a done() predicate for meshSendCollect(Seq): true
// once any line so far contains any of the given substrings.
func containsAny(subs ...string) func([]string) bool {
	return func(lines []string) bool {
		for _, l := range lines {
			for _, s := range subs {
				if strings.Contains(l, s) {
					return true
				}
			}
		}
		return false
	}
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
