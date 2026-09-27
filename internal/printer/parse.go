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
