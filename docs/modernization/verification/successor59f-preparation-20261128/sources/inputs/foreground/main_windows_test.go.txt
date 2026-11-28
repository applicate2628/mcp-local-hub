//go:build windows

package main

import (
    "strings"
    "testing"
)

func newParser() *testFrameParser {
    counts := map[string]testCounts{}
    for _, name := range cleanupTests { counts[name] = testCounts{} }
    return &testFrameParser{counts: counts}
}
func allEvents() []byte {
    var text strings.Builder
    for _, name := range cleanupTests {
        text.WriteString("\x16=== RUN   " + name + "\n")
        text.WriteString("\x16--- PASS: " + name + " (0.01s)\n")
    }
    return []byte(text.String())
}

func TestForegroundCleanupParserRejectsUnframedText(t *testing.T) {
    parser := newParser()
    parser.feed([]byte(strings.ReplaceAll(string(allEvents()), "\x16", "")))
    if cleanupPassed(parser.counts) { t.Fatal("unframed output was accepted as test evidence") }
    for name, counts := range parser.counts {
        if counts != (testCounts{}) { t.Fatalf("ordinary text changed %s: %+v", name, counts) }
    }
}

func TestForegroundCleanupParserAcceptsPromptAndOneByteChunks(t *testing.T) {
    parser := newParser()
    raw := append([]byte("ordinary prompt without newline> "), allEvents()...)
    for _, value := range raw { parser.feed([]byte{value}) }
    if !cleanupPassed(parser.counts) { t.Fatalf("split framed events lost: %+v", parser.counts) }
}

func TestForegroundCleanupParserIgnoresLongOrdinaryLineAndOversizedFrame(t *testing.T) {
    parser := newParser()
    parser.feed([]byte(strings.Repeat("ordinary", 20000)))
    parser.feed([]byte("\x16" + strings.Repeat("x", 4097) + "\n"))
    if len(parser.frame) > 4096 { t.Fatal("event buffer exceeded its bound") }
    for name, counts := range parser.counts {
        if counts != (testCounts{}) { t.Fatalf("oversized/ordinary data changed %s: %+v", name, counts) }
    }
    parser.feed(allEvents())
    if !cleanupPassed(parser.counts) { t.Fatalf("valid events after oversized data lost: %+v", parser.counts) }
}

func TestForegroundCleanupParserRejectsDuplicatePass(t *testing.T) {
    parser := newParser()
    parser.feed(allEvents())
    parser.feed([]byte("\x16--- PASS: " + cleanupTests[0] + " (0.01s)\n"))
    if cleanupPassed(parser.counts) { t.Fatal("duplicate successful event was accepted") }
}

func TestForegroundCleanupParserRejectsFailedOrSkippedRequiredRoot(t *testing.T) {
    for _, terminal := range []string{"FAIL", "SKIP"} {
        t.Run(terminal, func(t *testing.T) {
            parser := newParser()
            text := strings.Replace(string(allEvents()),
                "\x16--- PASS: " + cleanupTests[0] + " (",
                "\x16--- " + terminal + ": " + cleanupTests[0] + " (", 1)
            parser.feed([]byte(text))
            if cleanupPassed(parser.counts) { t.Fatal("required cleanup failure or skip was accepted") }
        })
    }
}
