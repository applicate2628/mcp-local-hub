//go:build windows

package cli

import (
	"reflect"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// realCommandLineToArgv parses cmd via the actual shell32 CommandLineToArgvW,
// the ground truth the pure-Go tokenizeWindowsCommandLine must match for the
// security-relevant argument tokens (F3).
func realCommandLineToArgv(t *testing.T, cmd string) []string {
	t.Helper()
	p, err := windows.UTF16PtrFromString(cmd)
	if err != nil {
		t.Fatalf("UTF16PtrFromString(%q): %v", cmd, err)
	}
	var argc int32
	argv, err := windows.CommandLineToArgv(p, &argc)
	if err != nil {
		t.Fatalf("CommandLineToArgv(%q): %v", cmd, err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(argv)))
	out := make([]string, argc)
	for i := int32(0); i < argc; i++ {
		out[i] = windows.UTF16ToString((*argv)[i][:])
	}
	return out
}

// dropLeadingEmpty neutralizes the ONLY intentional divergence between our
// parser and shell32: CommandLineToArgvW's special program-name (argv[0]) rule
// can emit an empty argv[0] on a leading-whitespace command line, while our
// parser skips leading whitespace. The classifier never inspects argv[0] (it
// anchors on tokens[1:]), and a legitimate argument is never an empty string, so
// dropping a single leading "" aligns both slices without masking any
// argument-parsing divergence.
func dropLeadingEmpty(xs []string) []string {
	if len(xs) > 0 && xs[0] == "" {
		return xs[1:]
	}
	return xs
}

// TestTokenizeWindowsCommandLine_DifferentialVsShell32 is the differential test
// the security review asked for: for a table of tricky inputs (the `""`
// double-quote-inside-quotes case, leading + internal whitespace, trailing
// backslashes, escaped quotes, quoted embedded spaces) our pure-Go tokenizer
// must reproduce shell32 CommandLineToArgvW's ARGUMENT tokens exactly. It runs
// and must PASS on this Windows host.
func TestTokenizeWindowsCommandLine_DifferentialVsShell32(t *testing.T) {
	inputs := []string{
		`C:\mcphub.exe daemon --server memory --daemon default`,
		`C:\mcphub.exe daemon serena-proxy --workspace "C:\My Proj\ws" --task-name \mcp-local-hub-serena-b133f336`,
		// The empirically-divergent "" case: a double-double-quote inside a
		// quoted span emits one literal " and closes the span, so the following
		// space splits and the reopened quote swallows the tail.
		`C:\mcphub.exe --workspace "C:\Program"" Files\ws" --language go`,
		`C:\mcphub.exe --path C:\dir\\ --x y`,       // trailing backslashes in a value
		`C:\mcphub.exe --q \"quoted\" --z 1`,        // escaped quotes
		`   C:\mcphub.exe daemon --server memory`,   // leading whitespace
		"C:\\mcphub.exe\tdaemon\t--server\tmemory",  // tab whitespace
		`C:\mcphub.exe    daemon     --server    x`, // runs of internal whitespace
		`C:\mcphub.exe "a b" "c\\d" e`,              // quoted spaces + escaped backslashes
		`C:\mcphub.exe daemon workspace-proxy --port 9401 --workspace "C:\Users\me\OneDrive\Documents\proj" --language go`,
	}
	for _, in := range inputs {
		mine := dropLeadingEmpty(tokenizeWindowsCommandLine(in))
		real := dropLeadingEmpty(realCommandLineToArgv(t, in))
		if !reflect.DeepEqual(mine, real) {
			t.Fatalf("tokenizer divergence for %q\n mine = %#v\n real = %#v", in, mine, real)
		}
	}
}
