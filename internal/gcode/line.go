// Package gcode implements Marlin line-numbered, checksummed G-code framing:
// building "N<n> <cmd> *<cs>" lines, parsing "ok"/"Resend"/"Error" responses,
// and a small windowed send queue that tracks Marlin's planner/serial buffer
// state via ADVANCED_OK ("ok N<line> P<planner> B<buffer>").
package gcode

import (
	"fmt"
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
	body := fmt.Sprintf("N%d %s", n, cmd)
	cs := Checksum(body)
	return fmt.Sprintf("%s*%d\n", body, cs)
}

// ResponseKind classifies a line received from Marlin.
type ResponseKind int

const (
	KindOther ResponseKind = iota
	KindOK
	KindResend
	KindError
	KindStart
	KindBusy
	KindEcho
)

// Response is a parsed line of Marlin host-protocol chatter.
type Response struct {
	Kind    ResponseKind
	Line    string // raw line, trimmed
	OKLine  int64  // for KindOK with ADVANCED_OK: N<line>
	Planner int    // ADVANCED_OK P value (planner free slots)
	Buffer  int    // ADVANCED_OK B value (serial RX buffer free slots)
	HasAdv  bool   // true if P/B were present
	Resend  int64  // for KindResend: requested line number
	Message string // free text (error text, busy reason, echo text)
}

// ParseLine classifies one line of text received from Marlin.
func ParseLine(raw string) Response {
	line := strings.TrimRight(raw, "\r\n")
	trimmed := strings.TrimSpace(line)

	switch {
	case strings.HasPrefix(trimmed, "start"):
		return Response{Kind: KindStart, Line: line}

	case strings.HasPrefix(trimmed, "ok"):
		r := Response{Kind: KindOK, Line: line}
		parseAdvancedOK(trimmed, &r)
		return r

	case strings.HasPrefix(trimmed, "Resend:") || strings.HasPrefix(trimmed, "rs "):
		r := Response{Kind: KindResend, Line: line}
		fields := strings.Fields(trimmed)
		for _, f := range fields {
			if n, err := strconv.ParseInt(f, 10, 64); err == nil {
				r.Resend = n
				break
			}
		}
		return r

	case strings.HasPrefix(trimmed, "Error:") || strings.HasPrefix(trimmed, "!!"):
		return Response{Kind: KindError, Line: line, Message: trimmed}

	case strings.HasPrefix(trimmed, "echo:busy:"):
		return Response{Kind: KindBusy, Line: line, Message: strings.TrimPrefix(trimmed, "echo:busy:")}

	case strings.HasPrefix(trimmed, "echo:"):
		return Response{Kind: KindEcho, Line: line, Message: strings.TrimPrefix(trimmed, "echo:")}

	default:
		return Response{Kind: KindOther, Line: line}
	}
}

// parseAdvancedOK looks for "ok N123 P30 B15" style suffixes on an "ok" line.
// Marlin's ADVANCED_OK format is: "ok N<line> P<planner-free> B<buffer-free>"
// but plain "ok" and "ok N123" (no P/B) are also possible depending on config,
// so every field is optional after the leading "ok".
func parseAdvancedOK(trimmed string, r *Response) {
	fields := strings.Fields(trimmed)
	for _, f := range fields[minInt(1, len(fields)):] {
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
			r.HasAdv = true
		case 'P':
			r.Planner = int(val)
			r.HasAdv = true
		case 'B':
			r.Buffer = int(val)
			r.HasAdv = true
		}
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
