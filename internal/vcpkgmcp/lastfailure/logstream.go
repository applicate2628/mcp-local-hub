package lastfailure

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	phaseLogReadChunkBytes = 64 << 10
)

// phaseLogScanResult contains bounded facts from one phase log. Raw log bytes
// never escape the scanner.
type phaseLogScanResult struct {
	bytesRead             int64
	truncated             bool
	interrupted           bool
	diagnosticsIncomplete bool
	buildCommand          string
	logBufferBytes        int
}

// phaseLogStreamScanner owns the only buffers used to read phase logs. One
// instance is reused for every admitted log in a last-failure call.
type phaseLogStreamScanner struct {
	readBuffer []byte
	lineBuffer []byte
}

func newPhaseLogStreamScanner() *phaseLogStreamScanner {
	return &phaseLogStreamScanner{readBuffer: make([]byte, phaseLogReadChunkBytes)}
}

type phaseLogParser struct {
	scanner      *phaseLogStreamScanner
	file         phaseLogFile
	accumulator  *diagnosticAccumulator
	commandBytes int
	lineBytes    int
	budget       [severityBudgetClasses][tierBudgetClasses]int
	result       phaseLogScanResult

	interruptStart int
	lineOverlong   bool
	interrupt      interruptStreamMatcher
}

func newPhaseLogParser(scanner *phaseLogStreamScanner, file phaseLogFile, perCellLimit, commandBytes, lineBytes int, accumulator *diagnosticAccumulator) *phaseLogParser {
	p := &phaseLogParser{
		scanner: scanner, file: file, accumulator: accumulator,
		commandBytes: commandBytes, lineBytes: lineBytes,
	}
	p.interrupt.reset()
	for severity := range p.budget {
		for tier := range p.budget[severity] {
			p.budget[severity][tier] = perCellLimit
		}
	}
	return p
}

// scan reads at most readLimit+1 bytes, using the sentinel byte only to prove
// truncation. It checks cancellation before open, before every read, and after
// a read returns, and closes the file exactly once on every post-open path.
func (s *phaseLogStreamScanner) scan(ctx context.Context, fsys FS, file phaseLogFile, readLimit int64, perCellLimit, commandBytes, lineBytes int, accumulator *diagnosticAccumulator) (result phaseLogScanResult, err error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if readLimit < 0 {
		return result, errors.New("negative phase-log read limit")
	}
	if lineBytes <= 0 {
		return result, errors.New("non-positive phase-log line limit")
	}
	info, err := fsys.Stat(file.Path)
	if err != nil {
		return result, err
	}
	if info == nil || !info.Mode().IsRegular() {
		return result, errors.New("phase log is not a regular file")
	}
	rc, err := fsys.Open(file.Path)
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := rc.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	s.lineBuffer = s.lineBuffer[:0]
	parser := newPhaseLogParser(s, file, perCellLimit, commandBytes, lineBytes, accumulator)
	for result.bytesRead <= readLimit {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		remaining := readLimit + 1 - result.bytesRead
		if remaining <= 0 {
			break
		}
		chunk := s.readBuffer
		if int64(len(chunk)) > remaining {
			chunk = chunk[:remaining]
		}
		n, readErr := rc.Read(chunk)
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if n > 0 {
			admitted := n
			if result.bytesRead+int64(admitted) > readLimit {
				admitted = int(readLimit - result.bytesRead)
				result.truncated = true
			}
			if admitted > 0 {
				parser.consume(chunk[:admitted])
				result.bytesRead += int64(admitted)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return result, readErr
		}
		if result.truncated {
			break
		}
		if n == 0 {
			return result, io.ErrNoProgress
		}
	}
	parser.finish()
	result.interrupted = parser.result.interrupted
	result.diagnosticsIncomplete = parser.result.diagnosticsIncomplete
	result.buildCommand = parser.result.buildCommand
	result.logBufferBytes = cap(s.readBuffer) + cap(s.lineBuffer)
	return result, nil
}

func (p *phaseLogParser) consume(data []byte) {
	for len(data) > 0 {
		i := bytes.IndexAny(data, "\r\n")
		if i < 0 {
			p.append(data)
			return
		}
		p.append(data[:i])
		switch data[i] {
		case '\r':
			p.finishInterruptSegment()
			p.interrupt.reset()
			if p.lineOverlong {
				p.scanner.lineBuffer = p.scanner.lineBuffer[:0]
				p.interruptStart = 0
			} else {
				p.append([]byte{'\r'})
				p.interruptStart = len(p.scanner.lineBuffer)
			}
		case '\n':
			p.finishInterruptSegment()
			p.interrupt.reset()
			p.finishLFLine()
		}
		data = data[i+1:]
	}
}

func (p *phaseLogParser) append(data []byte) {
	if len(data) == 0 {
		return
	}
	p.interrupt.feed(data)
	if p.lineOverlong {
		return
	}
	remaining := p.lineBytes - len(p.scanner.lineBuffer)
	if remaining >= len(data) {
		p.growLine(len(p.scanner.lineBuffer) + len(data))
		p.scanner.lineBuffer = append(p.scanner.lineBuffer, data...)
		return
	}
	if remaining > 0 {
		p.growLine(p.lineBytes)
		p.scanner.lineBuffer = append(p.scanner.lineBuffer, data[:remaining]...)
	}
	p.lineOverlong = true
	p.result.diagnosticsIncomplete = true
}

func (p *phaseLogParser) growLine(needed int) {
	if needed <= cap(p.scanner.lineBuffer) {
		return
	}
	capacity := cap(p.scanner.lineBuffer)
	if capacity == 0 {
		capacity = min(phaseLogReadChunkBytes, p.lineBytes)
	}
	for capacity < needed {
		capacity *= 2
		if capacity >= p.lineBytes {
			capacity = p.lineBytes
			break
		}
	}
	next := make([]byte, len(p.scanner.lineBuffer), capacity)
	copy(next, p.scanner.lineBuffer)
	p.scanner.lineBuffer = next
}

func (p *phaseLogParser) finishInterruptSegment() {
	matched := p.interrupt.matches()
	if !p.lineOverlong && p.interruptStart <= len(p.scanner.lineBuffer) {
		matched = isInterruptLogLine(p.scanner.lineBuffer[p.interruptStart:])
	}
	if matched {
		p.result.interrupted = true
	}
}

func (p *phaseLogParser) finishLFLine() {
	if !p.lineOverlong {
		line := normalizeLFLogLine(string(p.scanner.lineBuffer))
		if p.result.buildCommand == "" {
			if command, ok := buildCommandFromNormalizedLine(line); ok {
				p.result.buildCommand = boundedValue(command, p.commandBytes)
				p.accumulator.addCommand(p.file, p.result.buildCommand)
			}
		}
		if diagnostic, ok := matchDiagnosticLine(line); ok {
			severity, tier := severityBudgetClass(diagnostic), tierRank(diagnostic.Tier)
			if p.budget[severity][tier] == 0 {
				p.accumulator.addDropped(1)
			} else {
				p.budget[severity][tier]--
				p.accumulator.addDiagnostic(p.file, diagnostic)
			}
		}
	}
	p.scanner.lineBuffer = p.scanner.lineBuffer[:0]
	p.interruptStart = 0
	p.lineOverlong = false
}

func (p *phaseLogParser) finish() {
	p.finishInterruptSegment()
	if len(p.scanner.lineBuffer) > 0 || p.lineOverlong {
		p.finishLFLine()
	}
}

func normalizeLFLogLine(line string) string {
	return normalizeLogLine(strings.TrimRight(line, "\r"))
}

// interruptStreamMatcher recognizes the two exact ASCII interrupt markers
// without retaining their surrounding line. It exists for an LF line that is
// too long for the diagnostic framing buffer: DetectInterrupted historically
// scans CR/LF segments independently of the diagnostic line ceiling, so an
// overlong whitespace-padded marker must still win verdict precedence.
type interruptStreamMatcher struct {
	candidates uint8
	position   int
	started    bool
	matched    bool
	invalid    bool
	normalizer logLineNormalizer
	runeBytes  [utf8.UTFMax]byte
	runeLen    int
}

func (m *interruptStreamMatcher) reset() {
	m.candidates = uint8((1 << len(interruptMarkers)) - 1)
	m.position = 0
	m.started = false
	m.matched = false
	m.invalid = false
	m.normalizer.reset()
	m.runeLen = 0
}

func (m *interruptStreamMatcher) matches() bool {
	if !m.invalid {
		m.normalizer.finish(m.feedVisible)
	}
	if m.runeLen != 0 {
		m.invalid = true
	}
	return m.matched && !m.invalid
}

func (m *interruptStreamMatcher) feed(data []byte) {
	if m.invalid {
		return
	}
	m.normalizer.feed(data, m.feedVisible)
}

func (m *interruptStreamMatcher) feedVisible(b byte) {
	if m.runeLen > 0 || b >= utf8.RuneSelf {
		m.runeBytes[m.runeLen] = b
		m.runeLen++
		if !utf8.FullRune(m.runeBytes[:m.runeLen]) {
			return
		}
		r, size := utf8.DecodeRune(m.runeBytes[:m.runeLen])
		m.runeLen = 0
		if r == utf8.RuneError && size == 1 {
			m.invalid = true
			return
		}
		if unicode.IsSpace(r) {
			if m.started && !m.matched {
				m.invalid = true
			}
			return
		}
		m.invalid = true
		return
	}
	m.feedASCII(b)
}

func (m *interruptStreamMatcher) feedASCII(b byte) {
	whitespace := b == ' ' || b == '\t'
	if !m.started {
		if whitespace {
			return
		}
		m.started = true
	}
	if m.matched {
		if !whitespace {
			m.invalid = true
		}
		return
	}

	var remaining uint8
	complete := false
	for i, marker := range interruptMarkers {
		bit := uint8(1 << i)
		if m.candidates&bit == 0 || m.position >= len(marker) || marker[m.position] != b {
			continue
		}
		remaining |= bit
		if m.position+1 == len(marker) {
			complete = true
		}
	}
	if remaining == 0 {
		m.invalid = true
		return
	}
	m.candidates = remaining
	m.position++
	if complete {
		m.matched = true
	}
}
