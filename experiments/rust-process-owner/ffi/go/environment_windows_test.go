//go:build windows && amd64 && cgo

package caller

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"
)

// The finite Rust fixture emits one cwd followed by the requested values. A nil
// field means absent; a non-nil empty string is a present, empty value.
func readEnvironmentReport(t *testing.T, path string, fields int) []*string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]*string, 0, fields)
	for len(data) != 0 {
		if len(data) < 4 || len(out) >= fields {
			t.Fatal("malformed fixture field count")
		}
		n := binary.LittleEndian.Uint32(data[:4])
		data = data[4:]
		if n == ^uint32(0) {
			out = append(out, nil)
			continue
		}
		if n > 32768 || uint64(n)*2 > uint64(len(data)) {
			t.Fatal("invalid bounded fixture field")
		}
		units := make([]uint16, int(n))
		for i := range units {
			units[i] = binary.LittleEndian.Uint16(data[i*2:])
		}
		value := string(utf16.Decode(units))
		out = append(out, &value)
		data = data[2*int(n):]
	}
	if len(out) != fields {
		t.Fatalf("fields=%d want=%d", len(out), fields)
	}
	return out
}
func assertValue(t *testing.T, got *string, want string) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("environment value got=%v want length=%d", got, len(want))
	}
}
func finishZero(t *testing.T, o *Owner) {
	t.Helper()
	out, err := o.Finish(3000)
	if err != nil || out.State != complete || out.RootExit != 0 {
		t.Fatalf("fixture outcome %+v %v", out, err)
	}
}

func TestExplicitEnvironmentCopiesValuesWithoutParentInheritance(t *testing.T) {
	const parentKey = "MCPHUB_ENV_PARENT_SENTINEL"
	t.Setenv(parentKey, "parent-only-test-value")
	lib, child, dir := setup(t)
	marker := filepath.Join(dir, "environment.bin")
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	entries := []EnvironmentEntry{{"MCPHUB_A", "λ=value=🦀"}, {"MCPHUB_EMPTY", ""}, {"MCPHUB_ТЕСТ", "значение"}}
	owner, initial, err := lib.NewWithEnvironment(child, dir, []string{"context-relative", marker, "mcphub_a", "MCPHUB_EMPTY", "mcphub_тест", parentKey}, entries)
	if owner != nil {
		own(t, owner)
	}
	if err != nil {
		t.Fatal(err)
	}
	absent(t, marker)
	entries[0].Value = "mutated after creation"
	entries[2].Name = "renamed"
	entries = nil
	runtime.GC()
	if _, err = owner.AdmitChecked(func(info ProcessObservation) error {
		if info.PID != initial.PID {
			return errors.New("wrong original process")
		}
		if _, e := os.Stat(marker); !os.IsNotExist(e) {
			return errors.New("effect before admission")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	finishZero(t, owner)
	got := readEnvironmentReport(t, marker, 5)
	if got[0] == nil || !strings.EqualFold(filepath.Clean(*got[0]), filepath.Clean(dir)) {
		t.Fatal("wrong child directory")
	}
	assertValue(t, got[1], "λ=value=🦀")
	assertValue(t, got[2], "")
	assertValue(t, got[3], "значение")
	if got[4] != nil {
		t.Fatal("explicit child inherited parent sentinel")
	}
	proof, err := os.ReadFile(filepath.Join(dir, "relative-proof.bin"))
	if err != nil || string(proof) != "owned-context" {
		t.Fatal("child cwd was not effective")
	}
	after, _ := os.Getwd()
	if before != after || os.Getenv(parentKey) != "parent-only-test-value" {
		t.Fatal("parent process global state changed")
	}
}

func TestExplicitEnvironmentEmptyRemainsEmptyAndLegacyCompatible(t *testing.T) {
	const key = "MCPHUB_ENV_EMPTY_SENTINEL"
	t.Setenv(key, "parent-only")
	for _, mode := range []string{"nil", "empty", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			lib, child, dir := setup(t)
			marker := filepath.Join(dir, "empty.bin")
			var o *Owner
			var err error
			switch mode {
			case "nil":
				o, _, err = lib.NewWithEnvironment(child, dir, []string{"context", marker, key}, nil)
			case "empty":
				o, _, err = lib.NewWithEnvironment(child, dir, []string{"context", marker, key}, []EnvironmentEntry{})
			default:
				o, _, err = lib.New(child, dir, []string{"context", marker, key})
			}
			if o != nil {
				own(t, o)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = o.AdmitChecked(func(ProcessObservation) error { return nil }); err != nil {
				t.Fatal(err)
			}
			finishZero(t, o)
			got := readEnvironmentReport(t, marker, 2)
			if got[1] != nil {
				t.Fatal("empty block became inherited block")
			}
		})
	}
}

func TestExplicitEnvironmentRejectsInvalidBeforeNativeEffect(t *testing.T) {
	oversized := make([]EnvironmentEntry, 257)
	for i := range oversized {
		oversized[i] = EnvironmentEntry{fmt.Sprintf("K%03d", i), "v"}
	}
	cases := map[string][]EnvironmentEntry{
		"empty-key":          {{"", "test-private-value"}},
		"equals-key":         {{"BAD=NAME", "test-private-value"}},
		"drive-key":          {{"=D:", "test-private-value"}},
		"key-nul":            {{"A\x00B", "test-private-value"}},
		"value-nul":          {{"A", "test-private-value\x00"}},
		"duplicate-case":     {{"A", "test-private-value"}, {"a", "other"}},
		"duplicate-unicode":  {{"ТЕСТ", "test-private-value"}, {"тест", "other"}},
		"wrong-order":        {{"Z", "test-private-value"}, {"A", "other"}},
		"invalid-key-utf8":   {{string([]byte{0xff}), "test-private-value"}},
		"invalid-value-utf8": {{"A", string([]byte{0xfe, 0xff})}},
		"entry-count":        oversized,
		"block-budget":       {{"A", strings.Repeat("v", 16383)}, {"B", strings.Repeat("v", 16383)}},
		"string-budget":      {{"A", strings.Repeat("v", 32767)}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			lib, child, dir := setup(t)
			marker := filepath.Join(dir, "invalid")
			o, out, err := lib.NewWithEnvironment(child, dir, []string{"exit", marker, "0", "no"}, entries)
			if o != nil {
				own(t, o)
				t.Fatal("invalid environment acquired an owner")
			}
			requireStatus(t, err, argument)
			if out.PID != 0 {
				t.Fatal("validation error exposed a created process")
			}
			if strings.Contains(err.Error(), "test-private-value") {
				t.Fatal("input value leaked through validation error")
			}
			absent(t, marker)
		})
	}
}

func TestExplicitEnvironmentConcurrentChildrenStaySeparated(t *testing.T) {
	const key = "MCPHUB_ENV_CHILD_ID"
	t.Setenv(key, "parent")
	lib, child, root := setup(t)
	type created struct {
		o             *Owner
		err           error
		marker, value string
	}
	results := make(chan created, 2)
	var wg sync.WaitGroup
	for _, value := range []string{"left", "right"} {
		dir := filepath.Join(root, value)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(value, dir string) {
			defer wg.Done()
			marker := filepath.Join(dir, "env.bin")
			o, _, err := lib.NewWithEnvironment(child, dir, []string{"context", marker, key}, []EnvironmentEntry{{key, value}})
			results <- created{o, err, marker, value}
		}(value, dir)
	}
	wg.Wait()
	close(results)
	items := []created{}
	for item := range results {
		if item.o != nil {
			own(t, item.o)
		}
		items = append(items, item)
	}
	for _, item := range items {
		if item.err != nil {
			t.Fatal(item.err)
		}
		if _, err := item.o.AdmitChecked(func(ProcessObservation) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range items {
		finishZero(t, item.o)
		got := readEnvironmentReport(t, item.marker, 2)
		assertValue(t, got[1], item.value)
	}
	if os.Getenv(key) != "parent" {
		t.Fatal("child configuration modified parent")
	}
}

func TestExplicitEnvironmentDenialRetainsOwnerWithoutEffect(t *testing.T) {
	lib, child, dir := setup(t)
	marker := filepath.Join(dir, "denied-env")
	o, _, err := lib.NewWithEnvironment(child, dir, []string{"exit", marker, "0", "no"}, []EnvironmentEntry{{"A", "v"}})
	if o != nil {
		own(t, o)
	}
	if err != nil {
		t.Fatal(err)
	}
	rejection := errors.New("test policy denied")
	_, err = o.AdmitChecked(func(ProcessObservation) error { return rejection })
	requireStatus(t, err, denied)
	if !errors.Is(err, rejection) {
		t.Fatal("policy error lost")
	}
	absent(t, marker)
	if _, err = o.Stop(84); err != nil {
		t.Fatal(err)
	}
	out, err := o.Finish(3000)
	if err != nil || out.State != complete || out.RootExit != 84 {
		t.Fatalf("denial cleanup %+v %v", out, err)
	}
	absent(t, marker)
}

func TestExplicitEnvironmentPreservesNativeFailure(t *testing.T) {
	lib, _, dir := setup(t)
	missing := filepath.Join(dir, "absent.exe")
	o, out, err := lib.NewWithEnvironment(missing, dir, nil, []EnvironmentEntry{{"A", "v"}})
	if o != nil {
		own(t, o)
		t.Fatal("missing executable acquired owner")
	}
	requireStatus(t, err, nativeFailure)
	if out.NativeError == 0 {
		t.Fatal("native failure lost original code")
	}
	o, _, err = lib.NewWithEnvironment(missing, dir, nil, []EnvironmentEntry{{"", "invalid"}})
	if o != nil {
		own(t, o)
		t.Fatal("invalid input acquired owner")
	}
	requireStatus(t, err, argument)
}

func TestExplicitEnvironmentBudgetCountsAllTerminators(t *testing.T) {
	lib, child, dir := setup(t)
	marker := filepath.Join(dir, "edge.bin")
	// A + '=' + value + entry NUL + block NUL = 32766 UTF-16 units.
	value := strings.Repeat("v", 32762)
	o, _, err := lib.NewWithEnvironment(child, dir, []string{"context", marker, "A"}, []EnvironmentEntry{{"A", value}})
	if o != nil {
		own(t, o)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err = o.AdmitChecked(func(ProcessObservation) error { return nil }); err != nil {
		t.Fatal(err)
	}
	finishZero(t, o)
	assertValue(t, readEnvironmentReport(t, marker, 2)[1], value)
	bad, _, err := lib.NewWithEnvironment(child, dir, nil, []EnvironmentEntry{{"A", value + "v"}})
	if bad != nil {
		own(t, bad)
		t.Fatal("block overrun accepted")
	}
	requireStatus(t, err, argument)
}

func TestTextBoundaryRejectsInvalidUTF8InsteadOfReplacing(t *testing.T) {
	lib, child, dir := setup(t)
	o, _, err := lib.New(child, dir, []string{"exit", filepath.Join(dir, "invalid-text"), "0", string([]byte{0xff})})
	if o != nil {
		own(t, o)
		t.Fatal("invalid UTF-8 silently replaced in native input")
	}
	requireStatus(t, err, argument)
}
