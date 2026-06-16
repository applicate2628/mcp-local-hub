package drmemory

import (
	"testing"
)

// cannedResults is a representative Dr. Memory results.txt body covering
// the three primary error classes (UNADDRESSABLE ACCESS, UNINITIALIZED
// READ, LEAK) plus the ERRORS FOUND summary block. Lines carry the
// "~~Dr.M~~ " sentinel Dr. Memory prefixes to every results.txt line so
// the parser's prefix-stripping is exercised too.
const cannedResults = `~~Dr.M~~ Dr. Memory version 2.6.0 build 0 built on Jan 01 2024
~~Dr.M~~ Running "target.exe"
~~Dr.M~~
~~Dr.M~~ Error #1: UNADDRESSABLE ACCESS: reading 0x00000004-0x00000008 4 byte(s)
~~Dr.M~~ # 0 target.exe!do_thing            [c:\src\target.c:42]
~~Dr.M~~ # 1 target.exe!main                [c:\src\target.c:88]
~~Dr.M~~ Note: @0:00:01.234 in thread 1234
~~Dr.M~~
~~Dr.M~~ Error #2: UNINITIALIZED READ: reading register eax
~~Dr.M~~ # 0 target.exe!compute             [c:\src\target.c:55]
~~Dr.M~~ # 1 target.exe!main                [c:\src\target.c:90]
~~Dr.M~~
~~Dr.M~~ Error #3: LEAK 16 direct bytes 0x00500100-0x00500110 + 0 indirect bytes
~~Dr.M~~ # 0 replace_malloc
~~Dr.M~~ # 1 target.exe!alloc_it            [c:\src\target.c:10]
~~Dr.M~~ # 2 target.exe!main                [c:\src\target.c:80]
~~Dr.M~~
~~Dr.M~~ ERRORS FOUND:
~~Dr.M~~       1 unique,     1 total unaddressable access(es)
~~Dr.M~~       1 unique,     1 total uninitialized access(es)
~~Dr.M~~       0 unique,     0 total invalid heap argument(s)
~~Dr.M~~       1 unique,     1 total,     16 byte(s) of leak(s)
~~Dr.M~~ ERRORS IGNORED:
~~Dr.M~~       0 unique,     0 total still-reachable allocation(s)
`

func TestParseResults_StructuredErrorsAndCounts(t *testing.T) {
	res := parseResults(cannedResults)

	if len(res.Errors) != 3 {
		t.Fatalf("expected 3 parsed errors, got %d: %+v", len(res.Errors), res.Errors)
	}

	// Error #1 — UNADDRESSABLE ACCESS.
	e0 := res.Errors[0]
	if e0.Type != "UNADDRESSABLE ACCESS" {
		t.Errorf("errors[0].Type = %q, want UNADDRESSABLE ACCESS", e0.Type)
	}
	if e0.Count != 1 {
		t.Errorf("errors[0].Count = %d, want 1", e0.Count)
	}
	if e0.Location != "# 0 target.exe!do_thing            [c:\\src\\target.c:42]" {
		t.Errorf("errors[0].Location = %q, want top stack frame", e0.Location)
	}
	wantStack0 := "# 0 target.exe!do_thing            [c:\\src\\target.c:42]\n" +
		"# 1 target.exe!main                [c:\\src\\target.c:88]"
	if e0.FullStack != wantStack0 {
		t.Errorf("errors[0].FullStack =\n%q\nwant\n%q", e0.FullStack, wantStack0)
	}

	// Error #2 — UNINITIALIZED READ.
	if res.Errors[1].Type != "UNINITIALIZED READ" {
		t.Errorf("errors[1].Type = %q, want UNINITIALIZED READ", res.Errors[1].Type)
	}

	// Error #3 — LEAK.
	if res.Errors[2].Type != "LEAK" {
		t.Errorf("errors[2].Type = %q, want LEAK", res.Errors[2].Type)
	}

	// Counts: 2 non-leak errors (unaddressable + uninit), 1 leak.
	if res.ErrorCount != 2 {
		t.Errorf("ErrorCount = %d, want 2", res.ErrorCount)
	}
	if res.LeakCount != 1 {
		t.Errorf("LeakCount = %d, want 1", res.LeakCount)
	}

	// Summary must capture the ERRORS FOUND block.
	if res.Summary == "" {
		t.Fatal("Summary empty; expected ERRORS FOUND block")
	}
	for _, want := range []string{
		"ERRORS FOUND:",
		"unaddressable access(es)",
		"uninitialized access(es)",
		"byte(s) of leak(s)",
	} {
		if !contains(res.Summary, want) {
			t.Errorf("Summary missing %q:\n%s", want, res.Summary)
		}
	}
}

func TestParseResults_PossibleLeakClassifiedAsLeak(t *testing.T) {
	blob := `Error #1: POSSIBLE LEAK 8 direct bytes 0x00400000-0x00400008 + 0 indirect bytes
# 0 replace_malloc
# 1 app.exe!foo [c:\a.c:5]

ERRORS FOUND:
      1 unique,     1 total,     8 byte(s) of possible leak(s)
`
	res := parseResults(blob)
	if len(res.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(res.Errors))
	}
	if res.Errors[0].Type != "POSSIBLE LEAK" {
		t.Errorf("Type = %q, want POSSIBLE LEAK", res.Errors[0].Type)
	}
	if res.LeakCount != 1 {
		t.Errorf("LeakCount = %d, want 1 (POSSIBLE LEAK must count as leak)", res.LeakCount)
	}
	if res.ErrorCount != 0 {
		t.Errorf("ErrorCount = %d, want 0", res.ErrorCount)
	}
}

func TestParseResults_NoErrors(t *testing.T) {
	blob := `~~Dr.M~~ Dr. Memory version 2.6.0
~~Dr.M~~ Running "clean.exe"
~~Dr.M~~ NO ERRORS FOUND:
~~Dr.M~~       0 unique,     0 total unaddressable access(es)
`
	res := parseResults(blob)
	if len(res.Errors) != 0 {
		t.Errorf("expected 0 errors, got %d: %+v", len(res.Errors), res.Errors)
	}
	if res.ErrorCount != 0 || res.LeakCount != 0 {
		t.Errorf("expected zero counts, got error=%d leak=%d", res.ErrorCount, res.LeakCount)
	}
	if !contains(res.Summary, "NO ERRORS FOUND:") {
		t.Errorf("Summary missing NO ERRORS FOUND block: %q", res.Summary)
	}
}

func TestParseResults_InvalidHeapArgument(t *testing.T) {
	blob := `Error #1: INVALID HEAP ARGUMENT: allocated with malloc, freed with operator delete
# 0 app.exe!bad_free [c:\b.c:9]

ERRORS FOUND:
      1 unique,     1 total invalid heap argument(s)
`
	res := parseResults(blob)
	if len(res.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(res.Errors))
	}
	if res.Errors[0].Type != "INVALID HEAP ARGUMENT" {
		t.Errorf("Type = %q, want INVALID HEAP ARGUMENT", res.Errors[0].Type)
	}
	if res.ErrorCount != 1 {
		t.Errorf("ErrorCount = %d, want 1", res.ErrorCount)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
