// Package gcode implements Marlin line-numbered, checksummed G-code framing:
// building "N<n> <cmd> *<cs>" lines, parsing "ok"/"Resend"/"Error" responses,
// and a small windowed send queue that tracks which numbered lines are still
// awaiting Marlin's "ok" (ADVANCED_OK: "ok N<line> P<planner> B<buffer>").
package gcode

import (
	"strconv"
	"strings"
)

// Checksum computes Marlin's XOR checksum over the given bytes, exactly as
// Marlin does: cs = cs ^ byte, for every byte in the line up to (not
// including) the "*" that introduces the checksum itself.
func Checksum(line string) byte {
	var cs byte
	for i := 0; i < len(line); i++ {
		cs ^= line[i]
	}
	return cs
}

// FrameLine builds "N<n> <cmd>*<cs>\n" the way Marlin expects it on the wire.
func FrameLine(n int64, cmd string) string {
	body := "N" + strconv.FormatInt(n, 10) + " " + cmd
	return body + "*" + strconv.Itoa(int(Checksum(body))) + "\n"
}

// MaxFramedLen is the longest framed line (without its newline) Marlin
// 2.1.2.7 keeps whole: MAX_CMD_SIZE is 96 including the NUL. From the 96th
// byte on, queue.cpp process_stream_char discards the rest of the line,
// checksum included, answers "No Checksum" and then asks for the same line
// again forever.
const MaxFramedLen = 95

// FramedLenMax returns the framed length (without the newline) of cmd for
// any line number up to maxN, counting the checksum as its widest (3
// digits), so a line that passes it fits whatever N it finally gets.
func FramedLenMax(maxN int64, cmd string) int {
	return len("N") + len(strconv.FormatInt(maxN, 10)) + len(" ") + len(cmd) + len("*255")
}

// CutComment drops everything from the first ';' on. Marlin does the same
// at receive time for every command, M117/M118 included (this build has no
// GCODE_QUOTED_STRINGS), so a ';' left in a framed line would take the
// checksum with it.
func CutComment(s string) string {
	if i := strings.IndexByte(s, ';'); i >= 0 {
		return s[:i]
	}
	return s
}

// WireSafe reports whether Marlin stores cmd exactly as sent, which the
// checksum requires: no ';' (comment), no '\' (escape, dropped by
// process_stream_char) and no control byte other than tab (0x08 erases the
// previous byte; CR/LF end the line; NUL ends the string).
func WireSafe(cmd string) bool {
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if c == ';' || c == '\\' || (c < 0x20 && c != '\t') {
			return false
		}
	}
	return true
}

// ResponseKind classifies a line received from Marlin.
type ResponseKind int

const (
	KindOther ResponseKind = iota
	KindOK
	KindResend
	KindError
	KindStart
)

// Response is a parsed line of Marlin host-protocol chatter.
type Response struct {
	Kind    ResponseKind
	Line    string // raw line, trimmed
	OKLine  int64  // for KindOK with HasN: the N<line> Marlin echoed back
	HasN    bool   // true if the ok carried N<line> (valid even for N0)
	Planner int    // ADVANCED_OK P value (planner free slots)
	Buffer  int    // ADVANCED_OK B value (command buffer free slots)
	HasAdv  bool   // true if P/B were present
	Resend  int64  // for KindResend: requested line number
	Message string // free text (error text)
}

// ParseLine classifies one line of text received from Marlin.
func ParseLine(raw string) Response {
	line := strings.TrimRight(raw, "\r\n")
	trimmed := strings.TrimSpace(line)

	switch {
	case isStart(trimmed):
		return Response{Kind: KindStart, Line: line}

	case strings.HasPrefix(trimmed, "ok"):
		r := Response{Kind: KindOK, Line: line}
		parseAdvancedOK(trimmed, &r)
		return r

	case strings.HasPrefix(trimmed, "Resend:") || strings.HasPrefix(trimmed, "rs "):
		r := Response{Kind: KindResend, Line: line}
		rest := strings.TrimPrefix(strings.TrimPrefix(trimmed, "Resend:"), "rs ")
		if f := strings.Fields(rest); len(f) > 0 {
			r.Resend, _ = strconv.ParseInt(f[0], 10, 64)
		}
		return r

	case strings.HasPrefix(trimmed, "Error:") || strings.HasPrefix(trimmed, "!!"):
		return Response{Kind: KindError, Line: line, Message: trimmed}

	default:
		return Response{Kind: KindOther, Line: line}
	}
}

// isStart matches Marlin's boot banner "start". A board reset can leave a
// few bytes of bootloader noise in front of it, so non-printable leading
// bytes are ignored, but the visible text must be exactly "start".
func isStart(trimmed string) bool {
	i := 0
	for i < len(trimmed) && (trimmed[i] < 0x20 || trimmed[i] > 0x7e) {
		i++
	}
	return trimmed[i:] == "start"
}

// parseAdvancedOK looks for "ok N123 P30 B15" style suffixes on an "ok" line.
// Marlin's ADVANCED_OK format is: "ok N<line> P<planner-free> B<buffer-free>"
// but plain "ok" and "ok N123" (no P/B) are also possible depending on config,
// so every field is optional after the leading "ok".
func parseAdvancedOK(trimmed string, r *Response) {
	fields := strings.Fields(trimmed)
	if len(fields) == 0 || fields[0] != "ok" {
		return
	}
	for _, f := range fields[1:] {
		if len(f) < 2 {
			continue
		}
		val, err := strconv.ParseInt(f[1:], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case 'N':
			r.OKLine = val
			r.HasN = true
		case 'P':
			r.Planner = int(val)
			r.HasAdv = true
		case 'B':
			r.Buffer = int(val)
			r.HasAdv = true
		}
	}
}
