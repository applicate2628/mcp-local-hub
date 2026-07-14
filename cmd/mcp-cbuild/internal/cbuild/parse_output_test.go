package cbuild

import "testing"

func TestParseCTest(t *testing.T) {
	out := "Test project C:/proj/build\n" +
		"    Start 1: hello_pass\n" +
		"1/4 Test #1: hello_pass ..................   Passed    0.01 sec\n" +
		"    Start 2: hello_fail\n" +
		"2/4 Test #2: hello_fail ..................***Failed    0.02 sec\n" +
		"    Start 3: slow_one\n" +
		"3/4 Test #3: slow_one ...................***Timeout   1.50 sec\n" +
		"    Start 4: skip_one\n" +
		"4/4 Test #4: skip_one ...................***Skipped    0.00 sec\n" +
		"\n" +
		"50% tests passed, 2 tests failed out of 4\n"

	tests := parseCTest(out)
	if len(tests) != 4 {
		t.Fatalf("got %d tests, want 4: %+v", len(tests), tests)
	}
	want := []testCase{
		{Name: "hello_pass", Status: "passed", WallMs: 10},
		{Name: "hello_fail", Status: "failed", WallMs: 20},
		{Name: "slow_one", Status: "timeout", WallMs: 1500},
		{Name: "skip_one", Status: "skipped", WallMs: 0},
	}
	for i, w := range want {
		if tests[i] != w {
			t.Errorf("test[%d]\n got: %+v\nwant: %+v", i, tests[i], w)
		}
	}
}

func TestParseCTestEmpty(t *testing.T) {
	if got := parseCTest("no tests were found!!!\n"); got == nil || len(got) != 0 {
		t.Errorf("want non-nil empty slice, got %+v", got)
	}
}

func TestParseVcpkgList(t *testing.T) {
	out := "fmt:x64-windows          10.1.1#1     Formatting library for C++\n" +
		"zlib:x64-windows         1.3.1        A compression library\n" +
		"random noise line\n"
	pkgs := parseVcpkgList(out)
	if len(pkgs) != 2 {
		t.Fatalf("got %d packages, want 2: %+v", len(pkgs), pkgs)
	}
	if pkgs[0] != (installedPackage{Name: "fmt", Triplet: "x64-windows", Version: "10.1.1#1"}) {
		t.Errorf("pkg[0] = %+v", pkgs[0])
	}
	if pkgs[1] != (installedPackage{Name: "zlib", Triplet: "x64-windows", Version: "1.3.1"}) {
		t.Errorf("pkg[1] = %+v", pkgs[1])
	}
}

func TestParseVcpkgInstalled(t *testing.T) {
	out := "The following packages will be built and installed:\n" +
		"    fmt:x64-windows -> 10.1.1\n" +
		"  * vcpkg-cmake:x64-windows -> 2024-04-23\n" +
		"    spdlog[core]:x64-windows -> 1.14.1\n" +
		"    fmt:x64-windows -> 10.1.1\n" + // duplicate is collapsed
		"Elapsed time to handle install: 2 s\n"
	pkgs := parseVcpkgInstalled(out)
	if len(pkgs) != 3 {
		t.Fatalf("got %d packages, want 3: %+v", len(pkgs), pkgs)
	}
	want := []installedPackage{
		{Name: "fmt", Triplet: "x64-windows", Version: "10.1.1"},
		{Name: "vcpkg-cmake", Triplet: "x64-windows", Version: "2024-04-23"},
		{Name: "spdlog", Triplet: "x64-windows", Version: "1.14.1"},
	}
	for i, w := range want {
		if pkgs[i] != w {
			t.Errorf("pkg[%d]\n got: %+v\nwant: %+v", i, pkgs[i], w)
		}
	}
}

func TestParseVcpkgSearch(t *testing.T) {
	out := "fmt                  10.1.1#1         {Formatting library for C++}\n" +
		"fmt[core]                             The core feature\n" +
		"zlib                 1.3.1            A compression library\n"
	pkgs := parseVcpkgSearch(out)
	if len(pkgs) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(pkgs), pkgs)
	}
	if pkgs[0].Name != "fmt" || pkgs[0].Version != "10.1.1#1" || pkgs[0].Description != "{Formatting library for C++}" {
		t.Errorf("row[0] = %+v", pkgs[0])
	}
	// A feature row has no version column; the whole tail is the description.
	if pkgs[1].Name != "fmt[core]" || pkgs[1].Version != "" || pkgs[1].Description != "The core feature" {
		t.Errorf("row[1] = %+v", pkgs[1])
	}
	if pkgs[2].Name != "zlib" || pkgs[2].Version != "1.3.1" {
		t.Errorf("row[2] = %+v", pkgs[2])
	}
}
