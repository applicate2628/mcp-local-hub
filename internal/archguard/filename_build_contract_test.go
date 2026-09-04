package archguard

import (
	"fmt"
	"go/build"
	"go/build/constraint"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// go/build is the oracle: expectations are not a copy of archcheck's matcher.
// File collection already handles leading dot/underscore names; these tests
// exercise constraints on Go files admitted by that collection layer.
func TestFilenameBuildConstraintMatchesGoBuild(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		"source.go", "windows.go", "amd64.go", "source_unix.go", "source_custom.go",
		"source_windows.go", "source_linux.go", "source_android.go", "source_darwin.go",
		"source_ios.go", "source_solaris.go", "source_illumos.go", "source_arm64.go",
		"source_amd64.go", "source_windows_arm64.go", "windows_arm64.go",
		"source_windows_test.go", "source_windows_arm64_test.go",
		"source_windows.extra.go", "source_windows_test.extra.go", "source.extra_windows.go",
		"source_nacl.go", "source_hurd.go", "source_zos.go", "source_amd64p32.go",
		"source_armbe.go", "source_arm64be.go", "source_mips64p32.go", "source_mips64p32le.go",
		"source_ppc.go", "source_riscv.go", "source_s390.go", "source_sparc.go",
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("package example\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	targets := []goTarget{
		{GOOS: "linux", GOARCH: "amd64"}, {GOOS: "windows", GOARCH: "arm64"},
		{GOOS: "darwin", GOARCH: "arm64"}, {GOOS: "android", GOARCH: "arm64"},
		{GOOS: "ios", GOARCH: "arm64"}, {GOOS: "illumos", GOARCH: "amd64"},
		{GOOS: "js", GOARCH: "wasm"},
	}
	tagSets := [][]string{
		nil, {"windows"}, {"arm64"}, {"windows", "arm64"}, {"linux", "amd64"},
		{"nacl", "hurd", "zos"},
		{"amd64p32", "armbe", "arm64be", "mips64p32", "mips64p32le", "ppc", "riscv", "s390", "sparc"},
	}
	for _, target := range targets {
		for i, tags := range tagSets {
			t.Run(fmt.Sprintf("%s-%s/tags-%d", target.GOOS, target.GOARCH, i), func(t *testing.T) {
				ctx := build.Context{GOOS: target.GOOS, GOARCH: target.GOARCH, Compiler: "gc", BuildTags: tags}
				for _, name := range names {
					want, err := ctx.MatchFile(dir, name)
					if err != nil {
						t.Fatal(err)
					}
					expr := filenameBuildConstraint("internal/example/" + name)
					if got := filenameContractEval(expr, ctx); got != want {
						t.Errorf("%s tags=%v: archcheck=%v go/build=%v", name, tags, got, want)
					}
				}
			})
		}
	}
}

func TestFilenameAndDirectiveShareTagIdentity(t *testing.T) {
	for _, directive := range []string{"windows", "!windows", "windows || linux", "!(windows && arm64)"} {
		t.Run(directive, func(t *testing.T) {
			dir := t.TempDir()
			name := "source_windows_arm64.go"
			if err := os.WriteFile(filepath.Join(dir, name), []byte("//go:build "+directive+"\n\npackage example\n"), 0600); err != nil {
				t.Fatal(err)
			}
			parsed, err := constraint.Parse("//go:build " + directive)
			if err != nil {
				t.Fatal(err)
			}
			expr := &constraint.AndExpr{X: filenameBuildConstraint(name), Y: allowExplicitAutomaticTagOverrides(parsed)}
			for _, tags := range [][]string{nil, {"windows"}, {"arm64"}, {"windows", "arm64"}} {
				ctx := build.Context{GOOS: "linux", GOARCH: "amd64", Compiler: "gc", BuildTags: tags}
				want, err := ctx.MatchFile(dir, name)
				if err != nil {
					t.Fatal(err)
				}
				if got := filenameContractEval(expr, ctx); got != want {
					t.Errorf("directive=%s tags=%v: archcheck=%v go/build=%v", directive, tags, got, want)
				}
			}
		})
	}
}

// Evaluate archcheck's symbolic variables under a concrete build context.
func filenameContractEval(expr constraint.Expr, ctx build.Context) bool {
	return expr == nil || expr.Eval(func(tag string) bool {
		if additional, ok := strings.CutPrefix(tag, explicitAdditionalTagPrefix); ok {
			return slices.Contains(ctx.BuildTags, additional)
		}
		return osTagEnabled(ctx.GOOS, tag) || ctx.GOARCH == tag
	})
}
