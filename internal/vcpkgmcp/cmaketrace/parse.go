package cmaketrace

import (
	"encoding/json"
	"strings"
)

// traceLine is the raw json-v1 shape this package reads off one line.
// Defensive by design: only the fields this tool needs are declared, so an
// operator's cmake version emitting additional fields never breaks the
// parse (encoding/json silently ignores unknown fields).
type traceLine struct {
	// Version is non-nil only on the header line ({"version":{"major":1,
	// "minor":0}}); a command record never carries this key.
	Version *struct {
		Major int `json:"major"`
		Minor int `json:"minor"`
	} `json:"version"`
	File        string          `json:"file"`
	Line        int             `json:"line"`
	Cmd         string          `json:"cmd"`
	Args        []string        `json:"args"`
	Time        float64         `json:"time"`
	Frame       int             `json:"frame"`
	GlobalFrame int             `json:"global_frame"`
	Defer       json.RawMessage `json:"defer"`
}

// parseResult is the intermediate parse outcome, before the File/Command
// filters and the MaxRecords cap are applied.
type parseResult struct {
	records              []Record
	malformedCount       int
	versionHeaderPresent bool
}

// parseTraceLines parses json-v1 JSON Lines content defensively: a line that
// is neither valid JSON, nor a recognized version-header shape, nor a
// command record carrying a non-empty cmd, is counted as malformed and
// parsing CONTINUES — a trace truncated by a killed build is the NORMAL
// case, never an abort condition. Blank lines (including a trailing newline
// at end of file) are skipped silently: they are not malformed, they are not
// lines at all.
//
// The version header is recognized by shape (a "version" key with no "cmd"
// key) wherever it appears, not only at line 0 — a concatenated or
// hand-trimmed trace file is exactly the malformed input this parser must
// tolerate, per the package doc.
func parseTraceLines(data []byte) parseResult {
	var res parseResult

	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(rawLine, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}

		var tl traceLine
		if err := json.Unmarshal([]byte(line), &tl); err != nil {
			res.malformedCount++
			continue
		}

		if tl.Version != nil && tl.Cmd == "" {
			res.versionHeaderPresent = true
			continue
		}

		if tl.File == "" || tl.Line <= 0 || tl.Cmd == "" {
			// A real command record must identify the source location and
			// command. Anything less is malformed input, never positive
			// execution evidence (in particular, never line 0 evidence).
			res.malformedCount++
			continue
		}

		res.records = append(res.records, Record{
			File:        tl.File,
			Line:        tl.Line,
			Cmd:         tl.Cmd,
			Args:        tl.Args,
			Time:        tl.Time,
			Frame:       tl.Frame,
			GlobalFrame: tl.GlobalFrame,
			Defer:       len(tl.Defer) > 0 && string(tl.Defer) != "null",
		})
	}

	return res
}
