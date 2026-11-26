package cmaketrace

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
	// unsupportedVersion stops the parser on an explicit header whose major
	// is not json-v1. This is a whole-input failure, not partial-v1 evidence.
	unsupportedVersion bool
	// sawAnyContent records whether ANY non-blank line was observed at all,
	// so an empty (or whitespace-only) trace stays distinguishable from one
	// whose every line was malformed — without buffering the file to run a
	// whole-content TrimSpace over it.
	sawAnyContent bool
	// The four ceilings, each recorded independently: they are different
	// facts and a caller may need to act on them differently.
	hitByteLimit           bool
	hitLineLimit           bool
	hitRecordLimit         bool
	hitRetainedRecordLimit bool
	retainedRecordBytes    int64
}

func (p parseResult) sawNoContent() bool { return !p.sawAnyContent }

func (p parseResult) incomplete() bool {
	return p.malformedCount > 0 || p.hitByteLimit || p.hitLineLimit || p.hitRecordLimit || p.hitRetainedRecordLimit
}

// incompleteReasons returns every reason this parse is incomplete, in a
// fixed (declaration) order so the wire field is deterministic.
//
// Consumer-sweep note (ReasonTracePathNotSupplied, 2026-07-27): the not-supplied
// member is deliberately NOT reachable here. This list qualifies evidence that
// WAS parsed; a call with no trace_path is refused before deps.FS.Open, so
// there is no parse and nothing to qualify. Every OTHER member of Reason that
// can describe a partial read is already listed below.
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
	if p.hitRetainedRecordLimit {
		out = append(out, ReasonRetainedRecordLimit)
	}
	return out
}

// cancellationCheckInterval is how often (in lines) the streaming parser
// consults the context. Checking every line would put a channel read in the
// hot loop for no benefit; a few thousand lines is far below human-perceptible
// latency while keeping a canceled request from parsing to the end of a
// hundred-megabyte file.
const cancellationCheckInterval = 4096

// readChunkBytes is the size of the bufio.Reader buffer, and therefore the
// largest single slice one ReadSlice can hand back. It is stated
// explicitly rather than left to bufio's default so the memory ceiling
// readLine documents (maxBytes + readChunkBytes) is a property of this file
// and not of a library default that could change.
const readChunkBytes = 64 << 10 // 64 KiB

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
	br := bufio.NewReaderSize(limited, readChunkBytes)
	consumed := int64(0)
	lineNo := 0

	for {
		if lineNo%cancellationCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return parseResult{}, err
			}
		}
		lineNo++

		raw, n, tooLong, err := readLine(ctx, br, lim.MaxLineBytes)
		consumed += int64(n)
		// A read failure is not a successful bounded read. Preserve it even
		// when the same read also crosses a byte or line ceiling.
		if err != nil && !errors.Is(err, io.EOF) {
			return parseResult{}, err
		}
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
		if stop := res.consumeLine(raw, lim.MaxParsedRecords, lim.MaxRetainedRecordBytes); stop {
			return res, nil
		}

		if errors.Is(err, io.EOF) {
			return res, nil
		}
	}
}

// consumeLine decodes the raw line before the next read. Retained records own
// their decoded fields and never hold the reader's byte slice. It returns true
// when a retention ceiling or an unsupported version stops the parse.
func (p *parseResult) consumeLine(raw []byte, maxRecords int, maxRetainedRecordBytes int64) (stop bool) {
	line := bytes.TrimRight(raw, "\r\n")
	line = bytes.TrimRight(line, "\r")
	if len(bytes.TrimSpace(line)) == 0 {
		return false
	}
	p.sawAnyContent = true

	var tl traceLine
	if err := json.Unmarshal(line, &tl); err != nil {
		p.malformedCount++
		return false
	}

	if tl.Version != nil && tl.Cmd == "" {
		p.versionHeaderPresent = true
		if tl.Version.Major != 1 {
			p.unsupportedVersion = true
			return true
		}
		return false
	}

	if tl.File == "" || tl.Line <= 0 || tl.Cmd == "" || tl.Args == nil {
		// A real command record must identify the source location and
		// command, and json-v1 always carries an args array (including [] for
		// zero arguments). Anything less is malformed input, never positive
		// execution evidence (in particular, never line 0 evidence).
		p.malformedCount++
		return false
	}

	if len(p.records) >= maxRecords {
		p.hitRecordLimit = true
		return true
	}

	retainedBytes := retainedTraceRecordBytes(tl)
	if retainedBytes > maxRetainedRecordBytes-p.retainedRecordBytes {
		p.hitRetainedRecordLimit = true
		return true
	}
	p.retainedRecordBytes += retainedBytes

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

// retainedTraceRecordBytes conservatively accounts for the retained Record
// object, string/slice headers, and decoded backing bytes. It deliberately
// does not use unsafe.Sizeof: the fixed allowance is safe on both supported
// pointer widths and keeps the resource contract portable.
func retainedTraceRecordBytes(tl traceLine) int64 {
	const recordAndHeaders = 128
	const argumentCell = 16
	n := int64(recordAndHeaders + len(tl.File) + len(tl.Cmd))
	for _, arg := range tl.Args {
		n += int64(argumentCell + len(arg))
	}
	return n
}

// readLine reads one newline-terminated line. When the line exceeds maxBytes
// it is DRAINED to its newline and tooLong is set — the bytes are consumed
// (so parsing stays in sync with record boundaries) but never accumulated,
// which is the whole point of the ceiling.
//
// consumed counts every byte read from br, INCLUDING a drained oversized
// line: it feeds the whole-file byte ceiling, which would otherwise be
// trivially bypassed by one enormous line.
//
// The read is bounded BEFORE the line is materialized. bufio.Reader.ReadString
// (what this used to call) has no ceiling of its own: it appends until it finds
// the delimiter, so a single 256 MiB line was fully allocated and only THEN
// compared against maxBytes — the cap could report the overflow but could not
// bound the memory it was there to bound. ReadSlice instead returns at most one
// reader-buffer's worth and reports bufio.ErrBufferFull, so an oversized line
// arrives as a sequence of readChunkBytes-sized pieces that are counted,
// discarded once the ceiling trips, and never concatenated. Multi-buffer
// lines copy accepted chunks into storage capped at maxBytes. A single-buffer
// line borrows the reader buffer and is usable only until its next read.
func readLine(ctx context.Context, br *bufio.Reader, maxBytes int) (line []byte, consumed int, tooLong bool, err error) {
	var accumulated []byte
	for {
		if err := ctx.Err(); err != nil {
			return accumulated, consumed, tooLong, err
		}
		chunk, readErr := br.ReadSlice('\n')
		consumed += len(chunk)
		if !tooLong {
			if len(chunk) > maxBytes-len(accumulated) {
				tooLong = true
				accumulated = nil
			} else if accumulated == nil && !errors.Is(readErr, bufio.ErrBufferFull) {
				// The caller finishes decoding before the next bufio read.
				return chunk, consumed, false, readErr
			} else {
				needed := len(accumulated) + len(chunk)
				if needed > cap(accumulated) {
					capacity := cap(accumulated) + cap(accumulated)/2
					capacity = min(maxBytes, max(needed, capacity))
					grown := make([]byte, len(accumulated), capacity)
					copy(grown, accumulated)
					accumulated = grown
				}
				accumulated = append(accumulated, chunk...)
			}
		}
		if !errors.Is(readErr, bufio.ErrBufferFull) {
			return accumulated, consumed, tooLong, readErr
		}
	}
}
