package printer

import (
	"strconv"
	"strings"
)

// ParseTemps parses a Marlin temperature report line, either from an M105
// reply or an AUTO_REPORT_TEMPERATURES (M155 S<n>) push, e.g.:
//
//	"T:210.12 /210.00 B:60.03 /60.00 @:127 B@:80"
//
// It returns ok=false if the line has no recognizable T: field.
// ParseTemps handles Marlin's space-separated actual/target format, e.g.
// "T:210.12 /210.00 B:60.03 /60.00 @:127 B@:80", where the "/target" for a
// given sensor is a separate whitespace-delimited token immediately
// following its "X:actual" token (not glued to it).
func ParseTemps(line string) (Temps, bool) {
	var t Temps
	found := false
	fields := strings.Fields(line)
	for i, f := range fields {
		var actual *float64
		var target *float64
		switch {
		case strings.HasPrefix(f, "T:"):
			actual, target = &t.HotendActual, &t.HotendTarget
			*actual, _ = strconv.ParseFloat(f[2:], 64)
		case strings.HasPrefix(f, "B:"):
			actual, target = &t.BedActual, &t.BedTarget
			*actual, _ = strconv.ParseFloat(f[2:], 64)
		default:
			continue
		}
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
// Marlin's LONG_FILENAME_HOST_SUPPORT format is:
//
//	Begin file list
//	shortname.gco 12345 "Long File Name.gcode"
//	End file list
//
// (quoting of the long name and exact spacing has varied across Marlin
// versions; this accepts the common form and degrades gracefully.)
func ParseFileListLine(line string) (SDFile, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || trimmed == "Begin file list" || trimmed == "End file list" {
		return SDFile{}, false
	}
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return SDFile{}, false
	}
	f := SDFile{Short: strings.ToUpper(fields[0])}
	if len(fields) >= 2 {
		if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
			f.Bytes = n
		}
	}
	if qi := strings.Index(trimmed, "\""); qi >= 0 {
		if qj := strings.LastIndex(trimmed, "\""); qj > qi {
			f.Long = trimmed[qi+1 : qj]
		}
	}
	return f, true
}

// Capabilities parses M115 EXTENDED_CAPABILITIES_REPORT lines of the form
// "Cap:AUTOREPORT_TEMP:1".
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
