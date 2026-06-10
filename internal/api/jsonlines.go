// Package api — shared JSON-Lines tokenizer.
//
// splitJSONLines is the one JSON-Lines line splitter used across the
// api package's tail readers (gui-events.log via gui_event_log.go).
// It was migrated here from watchdog_log.go when the v0.6 redesign
// (spec §5 Phase D) deleted the watchdog engine — the splitter is
// neutral infrastructure shared with the surviving GUI-event log, so it
// outlives the watchdog code that originally hosted it.
package api

// splitJSONLines splits raw on '\n', returning each non-empty line as a
// fresh slice (no aliasing of raw). Cheap O(n) walk; avoids bufio.Scanner
// to keep the hot path allocation-light.
func splitJSONLines(raw []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\n' {
			if i > start {
				cp := make([]byte, i-start)
				copy(cp, raw[start:i])
				out = append(out, cp)
			}
			start = i + 1
		}
	}
	if start < len(raw) {
		cp := make([]byte, len(raw)-start)
		copy(cp, raw[start:])
		out = append(out, cp)
	}
	return out
}
