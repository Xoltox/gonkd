package printer

import (
	"strconv"
	"strings"
)

// ParseTemps parses a Marlin temperature report, either an M105 reply
// ("ok T:210.12 /210.00 B:60.03 /60.00 @:127 B@:80") or an
// AUTO_REPORT_TEMPERATURES (M155 S<n>) push (" T:210.12 /210.00 B:..."),
// where the "/target" for a sensor is a separate whitespace-delimited token
// right after its "X:actual" token. Only lines that start with "T:" or
// "ok T:" are accepted, so a file name or echo that happens to contain
// "T:" cannot clobber the readings. (ADVANCED_OK never adds N/P/B to the
// M105 reply: M105 prints its own "ok" and skips ok_to_send.)
func ParseTemps(line string) (Temps, bool) {
	var t Temps
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "T:") && !strings.HasPrefix(trimmed, "ok T:") {
		return t, false
	}
	found := false
	fields := strings.Fields(trimmed)
	for i, f := range fields {
		var actual, target *float64
		switch {
		case strings.HasPrefix(f, "T:"):
			actual, target = &t.HotendActual, &t.HotendTarget
		case strings.HasPrefix(f, "B:"):
			actual, target = &t.BedActual, &t.BedTarget
		default:
			continue
		}
		v, err := strconv.ParseFloat(f[2:], 64)
		if err != nil {
			continue
		}
		*actual = v
		found = true
		if i+1 < len(fields) && strings.HasPrefix(fields[i+1], "/") {
			*target, _ = strconv.ParseFloat(fields[i+1][1:], 64)
		}
	}
	return t, found
}

// SDStatus is the parsed result of an M27 report, either polled or pushed
// by AUTO_REPORT_SD_STATUS (M27 S<n>): "SD printing byte 12345/67890".
type SDStatus struct {
	Printing bool
	Current  int64
	Total    int64
	NotSD    bool // "Not SD printing"
}

func ParseSDStatus(line string) (SDStatus, bool) {
	trimmed := strings.TrimSpace(line)
	if strings.Contains(trimmed, "Not SD printing") {
		return SDStatus{NotSD: true}, true
	}
	const marker = "SD printing byte "
	idx := strings.Index(trimmed, marker)
	if idx < 0 {
		return SDStatus{}, false
	}
	rest := trimmed[idx+len(marker):]
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		return SDStatus{}, false
	}
	cur, err1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	tot, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err1 != nil || err2 != nil {
		return SDStatus{}, false
	}
	return SDStatus{Printing: true, Current: cur, Total: tot}, true
}

// ParseFileListLine parses one line of an M20 L (long filename) listing.
// This firmware (Marlin 2.1.2.7 cardreader.cpp printListing) prints the
// long name unquoted, and for files without a long name repeats the short
// name:
//
//	Begin file list
//	BENCHY~1.GCO 123456 Benchy Test.gcode
//	CUBE.GCO 2048 CUBE.GCO
//	SUBDIR/PART.GCO 99 Sub Dir/part.gcode
//	End file list
//
// Subdirectory entries (anything containing "/") are skipped: gonkd only
// prints and uploads in the card root. Only 8.3-shaped short names are
// accepted, so an autoreport or "ok" line interleaved with a long listing
// is never mistaken for a file.
func ParseFileListLine(line string) (SDFile, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || strings.Contains(line, "/") {
		return SDFile{}, false
	}
	if !valid83(fields[0]) {
		return SDFile{}, false
	}
	size, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || size < 0 {
		return SDFile{}, false
	}
	f := SDFile{Short: strings.ToUpper(fields[0]), Bytes: size}
	if len(fields) > 2 {
		f.Long = strings.Join(fields[2:], " ")
	}
	return f, true
}

// valid83 reports whether s looks like an 8.3 short name:
// [A-Z0-9_~]{1,8} optionally followed by "." and [A-Z0-9_]{1,3}
// (case-insensitive).
func valid83(s string) bool {
	base, ext := s, ""
	hasDot := false
	if i := strings.IndexByte(s, '.'); i >= 0 {
		base, ext, hasDot = s[:i], s[i+1:], true
	}
	if len(base) < 1 || len(base) > 8 || (hasDot && (len(ext) < 1 || len(ext) > 3)) {
		return false
	}
	for i := 0; i < len(base); i++ {
		if !is83Char(base[i]) {
			return false
		}
	}
	for i := 0; i < len(ext); i++ {
		if ext[i] == '~' || !is83Char(ext[i]) {
			return false
		}
	}
	return true
}

func is83Char(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '~'
}

// ParseFirmwareName extracts the FIRMWARE_NAME value from an M115 reply
// line, e.g.
//
//	FIRMWARE_NAME:Marlin 2.1.2.7 (Github) SOURCE_CODE_URL:https://github.com/MarlinFirmware/Marlin PROTOCOL_VERSION:1.0 MACHINE_TYPE:... EXTRUDER_COUNT:1 UUID:...
//
// yields "Marlin 2.1.2.7": Marlin packs several KEY:value fields on one
// line, so the value runs until the next recognized key, and a trailing
// "(Github)"-style parenthetical is dropped too.
func ParseFirmwareName(line string) (string, bool) {
	const marker = "FIRMWARE_NAME:"
	idx := strings.Index(line, marker)
	if idx < 0 {
		return "", false
	}
	rest := line[idx+len(marker):]
	for _, key := range []string{"SOURCE_CODE_URL:", "PROTOCOL_VERSION:", "MACHINE_TYPE:", "EXTRUDER_COUNT:", "UUID:"} {
		if i := strings.Index(rest, key); i >= 0 {
			rest = rest[:i]
		}
	}
	if i := strings.IndexByte(rest, '('); i >= 0 {
		rest = rest[:i]
	}
	name := strings.TrimSpace(rest)
	if name == "" {
		return "", false
	}
	return name, true
}

// BusyPausedForUser reports whether line is Marlin's HOST_KEEPALIVE
// keepalive sent every 2s while blocked in M0/M1's wait_for_user loop:
// "echo:busy: paused for user". This firmware build has
// ADVANCED_PAUSE_FEATURE off, so M600/M601 are not supported and never
// produce this line; M0/M1 (with EMERGENCY_PARSER on) are the only pause
// path gonkd can detect this way.
func BusyPausedForUser(line string) bool {
	return strings.Contains(line, "busy: paused for user")
}

// ParsePromptBegin parses a HOST_PROMPT_SUPPORT "//action:prompt_begin
// <msg>" line (sent only if that Marlin feature is enabled), returning the
// message, which may be empty.
func ParsePromptBegin(line string) (string, bool) {
	const marker = "//action:prompt_begin"
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, marker) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(trimmed, marker)), true
}

// IsPromptShow reports whether line is a "//action:prompt_show", sent once
// a HOST_PROMPT_SUPPORT dialog's buttons are ready.
func IsPromptShow(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "//action:prompt_show")
}

// IsPromptEnd reports whether line is a "//action:prompt_end", sent when a
// HOST_PROMPT_SUPPORT dialog is dismissed (by M876 or otherwise).
func IsPromptEnd(line string) bool {
	return strings.TrimSpace(line) == "//action:prompt_end"
}

// ParseCapability parses M115 EXTENDED_CAPABILITIES_REPORT lines of the
// form "Cap:AUTOREPORT_TEMP:1".
func ParseCapability(line string) (name string, enabled bool, ok bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "Cap:") {
		return "", false, false
	}
	rest := strings.TrimPrefix(trimmed, "Cap:")
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 {
		return "", false, false
	}
	return parts[0], parts[1] == "1", true
}

// ParseMBLPointLine parses Marlin's "MBL G29 point N of TOTAL" line
// (src/gcode/bedlevel/mbl/G29.cpp:260), printed after every G29 S1/S2 call
// that leaves state MeshNext. N is 1-based and is the point about to be
// probed (or, on the call that finishes the last point, -1: see
// ParseMeshDone for the real end-of-sequence signal).
func ParseMBLPointLine(line string) (point, total int, ok bool) {
	trimmed := strings.TrimSpace(line)
	const prefix = "MBL G29 point "
	if !strings.HasPrefix(trimmed, prefix) {
		return 0, 0, false
	}
	fields := strings.Fields(strings.TrimPrefix(trimmed, prefix))
	if len(fields) != 3 || fields[1] != "of" {
		return 0, 0, false
	}
	p, err1 := strconv.Atoi(fields[0])
	t, err2 := strconv.Atoi(fields[2])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return p, t, true
}

// ParseMeshDone reports whether line is Marlin's "Mesh probing done."
// (G29.cpp:198), sent once by the G29 S2 call that records the last point.
// This, not the point number, is the reliable end-of-sequence signal: that
// same call's own "MBL G29 point" line reports mbl_probe_index after it is
// reset to -1 ("MBL G29 point -1 of N").
func ParseMeshDone(line string) bool {
	return strings.TrimSpace(line) == "Mesh probing done."
}

// ParsePositionZ extracts the Z value from a report_current_position()
// line, e.g. "X:0.00 Y:0.00 Z:0.20 E:0.00 Count X:0 Y:0 Z:80 E:0"
// (module/motion.cpp). Everything from "Count" on is stepper counts, not
// logical position, so it is stripped before looking for "Z:" to avoid
// picking up "Count ... Z:80" instead of the real value.
func ParsePositionZ(line string) (float64, bool) {
	trimmed := strings.TrimSpace(line)
	if i := strings.Index(trimmed, "Count"); i >= 0 {
		trimmed = trimmed[:i]
	}
	for _, f := range strings.Fields(trimmed) {
		if strings.HasPrefix(f, "Z:") {
			v, err := strconv.ParseFloat(f[2:], 64)
			if err == nil {
				return v, true
			}
		}
	}
	return 0, false
}

// ParsePosition extracts X, Y, Z and E from a report_current_position()
// line (see ParsePositionZ for the "Count" stripping this shares), e.g.
// "X:0.00 Y:0.00 Z:0.20 E:0.00 Count X:0 Y:0 Z:80 E:0". ok is true only if
// all four axes were found, which every M114 reply on this firmware
// build provides.
func ParsePosition(line string) (Position, bool) {
	var p Position
	trimmed := strings.TrimSpace(line)
	if i := strings.Index(trimmed, "Count"); i >= 0 {
		trimmed = trimmed[:i]
	}
	found := 0
	for _, f := range strings.Fields(trimmed) {
		var dst *float64
		switch {
		case strings.HasPrefix(f, "X:"):
			dst = &p.X
		case strings.HasPrefix(f, "Y:"):
			dst = &p.Y
		case strings.HasPrefix(f, "Z:"):
			dst = &p.Z
		case strings.HasPrefix(f, "E:"):
			dst = &p.E
		default:
			continue
		}
		v, err := strconv.ParseFloat(f[2:], 64)
		if err != nil {
			continue
		}
		*dst = v
		found++
	}
	return p, found == 4
}

// ParseMeshReport parses the full multi-line reply to "G29 S0"
// (feature/bedlevel/mbl/mesh_bed_leveling.cpp report_mesh, and
// feature/bedlevel/bedlevel.cpp print_2d_array with SCAD_MESH_OUTPUT
// undefined, the default):
//
//	Mesh Bed Leveling ON
//	5x5 mesh. Z offset: 0.00000
//
//	Measured points:
//	  0    1    2    3    4
//	 0 +0.00000 +0.02500 +0.00000 -0.01000 +0.00000
//	 1 ...
//
// or a single "Mesh Bed Leveling has no data." line before the first
// complete probe. ok is false only if a mesh was reported (ON/OFF) but the
// grid that followed could not be parsed. Points is row-major, [y][x].
func ParseMeshReport(lines []string) (active bool, zOffset float64, points [][]float64, ok bool) {
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "Mesh Bed Leveling "):
			rest := strings.TrimPrefix(line, "Mesh Bed Leveling ")
			if strings.HasPrefix(rest, "has no data") {
				return false, 0, nil, true
			}
			active = rest == "ON"
		case strings.Contains(line, "Z offset:"):
			if idx := strings.Index(line, "Z offset:"); idx >= 0 {
				v, err := strconv.ParseFloat(strings.TrimSpace(line[idx+len("Z offset:"):]), 64)
				if err == nil {
					zOffset = v
				}
			}
		case line == "Measured points:":
			grid, gok := parseMeshGrid(lines[i+1:])
			if !gok {
				return false, 0, nil, false
			}
			return active, zOffset, grid, true
		}
	}
	return false, 0, nil, false
}

// parseMeshGrid reads the header row (used only for its column count) and
// the data rows that follow ("<row index> <value>..."), stopping at the
// first line that does not fit the pattern (typically "ok").
func parseMeshGrid(rest []string) ([][]float64, bool) {
	if len(rest) == 0 {
		return nil, false
	}
	cols := len(strings.Fields(rest[0]))
	if cols == 0 {
		return nil, false
	}
	var grid [][]float64
	for _, raw := range rest[1:] {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != cols+1 {
			break
		}
		row := make([]float64, cols)
		for i, f := range fields[1:] {
			v, err := strconv.ParseFloat(f, 64)
			if err != nil {
				return nil, false
			}
			row[i] = v
		}
		grid = append(grid, row)
		if len(grid) == cols {
			break
		}
	}
	if len(grid) == 0 {
		return nil, false
	}
	return grid, true
}
