package cmaketrace

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
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
	// sawAnyContent records whether ANY non-blank line was observed at all,
	// so an empty (or whitespace-only) trace stays distinguishable from one
	// whose every line was malformed — without buffering the file to run a
	// whole-content TrimSpace over it.
	sawAnyContent bool
	// The three ceilings, each recorded independently: they are different
	// facts and a caller may need to act on them differently.
	hitByteLimit   bool
	hitLineLimit   bool
	hitRecordLimit bool
}

func (p parseResult) sawNoContent() bool { return !p.sawAnyContent }

func (p parseResult) incomplete() bool {
	return p.malformedCount > 0 || p.hitByteLimit || p.hitLineLimit || p.hitRecordLimit
}

// incompleteReasons returns every reason this parse is incomplete, in a
// fixed (declaration) order so the wire field is deterministic.
func (p parseResult) incompleteReasons() []Reason {
	var out []Reason
	if p.malformedCount > 0 {
		out = append(out, ReasonInputMalformed)
	}
	if p.hitByteLimit {
		out = append(out, ReasonByteLimit)
	}
	if p.hitLineLimit {
		out = append(out, ReasonLineLimit)
	}
	if p.hitRecordLimit {
		out = append(out, ReasonRecordLimit)
	}
	return out
}

// cancellationCheckInterval is how often (in lines) the streaming parser
// consults the context. Checking every line would put a channel read in the
// hot loop for no benefit; a few thousand lines is far below human-perceptible
// latency while keeping a canceled request from parsing to the end of a
// hundred-megabyte file.
const cancellationCheckInterval = 4096

// parseTraceStream parses json-v1 JSON Lines defensively AS IT READS, under
// the MaxTraceBytes / MaxLineBytes / MaxParsedRecords ceilings.
//
// Defensive, as before: a line that is neither valid JSON, nor a recognized
// version-header shape, nor a command record carrying a non-empty cmd, is
// counted as malformed and parsing CONTINUES — a trace truncated by a killed
// build is the NORMAL case, never an abort condition. Blank lines (including
// a trailing newline at end of file) are skipped silently: they are not
// malformed, they are not lines at all.
//
// The version header is recognized by shape (a "version" key with no "cmd"
// key) wherever it appears, not only at line 0 — a concatenated or
// hand-trimmed trace file is exactly the malformed input this parser must
// tolerate, per the package doc.
//
// A ceiling stops the parse and is RECORDED (see parseResult.incompleteReasons)
// so the caller can never mistake a bounded read for a complete one. Only a
// context cancellation and a genuine read error are returned as errors: both
// mean we have no idea what we did not see, which is not a result worth
// returning.
func parseTraceStream(ctx context.Context, r io.Reader, lim Limits) (parseResult, error) {
	var res parseResult
	lim = lim.normalized()

	// +1 so consuming exactly MaxTraceBytes is not itself reported as a trip.
	limited := &io.LimitedReader{R: r, N: lim.MaxTraceBytes + 1}
	br := bufio.NewReader(limited)
	consumed := int64(0)
	lineNo := 0

	for {
		if lineNo%cancellationCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return parseResult{}, err
			}
		}
		lineNo++

		raw, n, tooLong, err := readLine(br, lim.MaxLineBytes)
		consumed += int64(n)
		if consumed > lim.MaxTraceBytes {
			res.hitByteLimit = true
			return res, nil
		}
		if tooLong {
			// The line was skipped without being parsed; it is both a
			// ceiling trip and, for counting purposes, unusable input.
			res.hitLineLimit = true
			res.malformedCount++
			res.sawAnyContent = true
			if errors.Is(err, io.EOF) {
				return res, nil
			}
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return parseResult{}, err
		}

		if stop := res.consumeLine(raw, lim.MaxParsedRecords); stop {
			return res, nil
		}

		if errors.Is(err, io.EOF) {
			return res, nil
		}
	}
}

// consumeLine folds one raw line into res. It returns stop=true when the
// record ceiling was reached, so the caller abandons the rest of the file.
func (p *parseResult) consumeLine(raw string, maxRecords int) (stop bool) {
	line := strings.TrimRight(raw, "\r\n")
	line = strings.TrimRight(line, "\r")
	if strings.TrimSpace(line) == "" {
		return false
	}
	p.sawAnyContent = true

	var tl traceLine
	if err := json.Unmarshal([]byte(line), &tl); err != nil {
		p.malformedCount++
		return false
	}

	if tl.Version != nil && tl.Cmd == "" {
		p.versionHeaderPresent = true
		return false
	}

	if tl.File == "" || tl.Line <= 0 || tl.Cmd == "" {
		// A real command record must identify the source location and
		// command. Anything less is malformed input, never positive
		// execution evidence (in particular, never line 0 evidence).
		p.malformedCount++
		return false
	}

	if len(p.records) >= maxRecords {
		p.hitRecordLimit = true
		return true
	}

	p.records = append(p.records, Record{
		File:        tl.File,
		Line:        tl.Line,
		Cmd:         tl.Cmd,
		Args:        tl.Args,
		Time:        tl.Time,
		Frame:       tl.Frame,
		GlobalFrame: tl.GlobalFrame,
		Defer:       len(tl.Defer) > 0 && string(tl.Defer) != "null",
	})
	return false
}

// readLine reads one newline-terminated line. When the line exceeds maxBytes
// it is DRAINED to its newline and tooLong is set — the bytes are consumed
// (so parsing stays in sync with record boundaries) but never accumulated,
// which is the whole point of the ceiling.
//
// consumed counts every byte read from br, INCLUDING a drained oversized
// line: it feeds the whole-file byte ceiling, which would otherwise be
// trivially bypassed by one enormous line.
func readLine(br *bufio.Reader, maxBytes int) (line string, consumed int, tooLong bool, err error) {
	var b strings.Builder
	overflowed := false
	for {
		chunk, readErr := br.ReadString('\n')
		consumed += len(chunk)
		if !overflowed {
			if b.Len()+len(chunk) > maxBytes {
				overflowed = true
				b.Reset()
			} else {
				b.WriteString(chunk)
			}
		}
		if readErr != nil {
			return b.String(), consumed, overflowed, readErr
		}
		if strings.HasSuffix(chunk, "\n") {
			return b.String(), consumed, overflowed, nil
		}
	}
}
