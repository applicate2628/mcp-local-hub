package lastfailure

import (
	"strings"
	"testing"
)

// An interrupt marker is producer NARRATION and arrives as a complete line.
// A mid-line occurrence is text the producer QUOTED, and must not be read as
// an interrupt.
//
// This matters more than any other predicate in the package because
// DetectInterrupted's verdict is the HIGHEST-precedence branch in LastFailure
// (lastfailure.go:511 — above even an unreadable log), so a false positive
// converts a genuine build failure into unknown(build_interrupted) and
// suppresses the real diagnostic entirely. The old predicate was
// strings.Contains over the WHOLE FILE, so any of the lines below tripped it.
//
// Every fixture is a shape a real phase log produces:
//   - ninja -v echoes each command line in full (the package's own interrupted
//     fixture shows `[1/3] cl.exe /c ... a.cpp`), so a source path or -D value
//     containing the phrase lands in the log verbatim;
//   - clang/gcc echo the offending SOURCE LINE under each diagnostic, so a
//     comment or string literal mentioning it lands there too;
//   - a project's own CMake message()/check_symbol_exists output passes
//     straight through.
func TestDetectInterrupted_MarkerMustBeAWholeLine(t *testing.T) {
	notInterrupts := []struct{ name, content string }{
		{
			"MSVC diagnostic whose path contains the phrase",
			"C:\\dev\\User interrupt\\src.cpp(12): error C2065: 'x': undeclared identifier\n",
		},
		{
			"ninja -v echo of a command line carrying the phrase",
			"[2/3] cl.exe /c /DFEATURE=\"User interrupt\" src.cpp\nFAILED: [code=2] src.obj\n",
		},
		{
			"clang echoing the offending source line",
			"src.cpp:5:9: error: expected ';'\n    // User interrupt handling below\n        ^\n",
		},
		{
			"the project's own CMake status line",
			"-- Looking for User interrupt support\n-- Looking for User interrupt support - found\n",
		},
		{
			"ninja's own narration quoted inside a longer message",
			"cmake: the child said \"ninja: build stopped: interrupted by user.\" but kept going\n",
		},
	}
	for _, tc := range notInterrupts {
		t.Run(tc.name, func(t *testing.T) {
			// Precondition: the fixture MUST contain the marker as a
			// substring, otherwise it proves nothing about the anchoring.
			var carries bool
			for _, m := range interruptMarkers {
				if strings.Contains(tc.content, m) {
					carries = true
				}
			}
			if !carries {
				t.Fatalf("precondition failed: fixture must contain an interrupt marker as a SUBSTRING, or this "+
					"case cannot distinguish the two predicates and would pass vacuously; content=%q", tc.content)
			}
			if DetectInterrupted([]byte(tc.content)) {
				t.Fatalf("a QUOTED marker was read as an interrupt. build_interrupted outranks every other verdict, "+
					"so this silently converts a real build failure into \"the build was stopped, not broken\" and "+
					"drops the diagnostic; content=%q", tc.content)
			}
		})
	}
}

// The complement: every whole-line spelling the producer actually emits must
// still be detected, or the fix would have bought correctness by never
// recognizing an interrupt at all — which fails in the other direction,
// reporting a stopped build as a defect.
func TestDetectInterrupted_RealProducerLinesAreStillDetected(t *testing.T) {
	realShapes := []struct{ name, content string }{
		{
			"the observed real sequence (scout pass boost-thread; fixture interruptedlib)",
			"[2/3] cl.exe /c ... b.cpp\nFAILED: [code=1] CMakeFiles/x.dir/b.cpp.obj\nUser interrupt\nninja: build stopped: interrupted by user.\n",
		},
		{"subprocess line alone", "FAILED: [code=1] x.obj\nUser interrupt\n"},
		{"ninja narration alone", "ninja: build stopped: interrupted by user.\n"},
		{"CRLF log", "FAILED: [code=1] x.obj\r\nUser interrupt\r\nninja: build stopped: interrupted by user.\r\n"},
		{"indented by a capture wrapper", "FAILED: [code=1] x.obj\n   User interrupt   \n"},
		{"no trailing newline at end of file", "FAILED: [code=1] x.obj\nUser interrupt"},
		{"carriage-return separated (terminal overwrite retained in a capture)", "FAILED: [code=1] x.obj\rUser interrupt\r"},
	}
	for _, tc := range realShapes {
		t.Run(tc.name, func(t *testing.T) {
			if !DetectInterrupted([]byte(tc.content)) {
				t.Fatalf("a real producer-emitted interrupt line was missed, so a stopped build will be reported as "+
					"a defect; content=%q", tc.content)
			}
		})
	}
}
