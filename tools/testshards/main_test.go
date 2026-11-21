package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func TestInventoryCountsTestsExamplesFuzzAndBenchmarks(t *testing.T) {
	list := []byte("TestOne\nExample\nFuzzCorpus\nBenchmarkParse\nTestTwo\nok  example/internal/api 0.003s\n")
	names, benchmarks, err := parseInventory(list)
	if err != nil {
		t.Fatal(err)
	}
	if benchmarks != 1 || strings.Join(names, ",") != "TestOne,Example,FuzzCorpus,TestTwo" {
		t.Fatalf("inventory names=%q benchmarks=%d", names, benchmarks)
	}
}

func TestInventoryRejectsAmbiguousOrMissingRoots(t *testing.T) {
	for _, raw := range []string{"", "BenchmarkOnly\n", "TestOne\nTestOne\n", "TestIllegal/Bad\n"} {
		if _, _, err := parseInventory([]byte(raw)); err == nil {
			t.Errorf("accepted invalid inventory %q", raw)
		}
	}
}

func TestPartitionIsCompleteAndDisjoint(t *testing.T) {
	roots := []string{"TestZ", "TestC", "Example", "TestA", "FuzzOne", "TestB", "TestY"}
	parts, err := partition(roots, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 {
		t.Fatalf("shards=%d, want 3", len(parts))
	}
	counts := map[string]int{}
	for i, part := range parts {
		if part.Index != i || len(part.Roots) > 3 {
			t.Fatalf("bad shard %d: %+v", i, part)
		}
		re, err := regexp.Compile(part.Selector)
		if err != nil {
			t.Fatal(err)
		}
		for _, root := range roots {
			got := re.MatchString(root)
			if got {
				counts[root]++
			}
		}
		if re.MatchString("TestAA") || re.MatchString("TestB/subtest") {
			t.Fatal("unintended root or subtest selector match")
		}
	}
	for _, root := range roots {
		if counts[root] != 1 {
			t.Errorf("root %q selected %d times", root, counts[root])
		}
	}
}

func TestPartitionRefusesInvalidAndRepeatedInputs(t *testing.T) {
	for _, tc := range []struct {
		roots []string
		max   int
	}{
		{[]string{"TestA", "TestA"}, 1},
		{[]string{"TestA"}, 0},
		{[]string{"TestA"}, 151},
		{[]string{"TestUnknown/child"}, 2},
		{nil, 2},
	} {
		if _, err := partition(tc.roots, tc.max); err == nil {
			t.Errorf("accepted invalid partition %+v", tc)
		}
	}
}

func eventLine(action, name string) string {
	raw, _ := json.Marshal(event{Action: action, Test: name})
	return string(raw) + "\n"
}

func TestReportRejectsMissingDuplicateAndSkip(t *testing.T) {
	s := shard{Index: 2, Roots: []string{"TestOne", "TestTwo"}, Selector: "^(TestOne|TestTwo)$"}
	complete := []byte(eventLine("pass", "TestOne") + eventLine("pass", "TestTwo") +
		eventLine("pass", ""))
	if got := analyzeLog(complete, s, 0); !got.Valid || got.Completed != 2 || !got.PackagePass {
		t.Fatalf("valid receipt refused: %+v", got)
	}
	fixtures := map[string][]byte{
		"missing": []byte(eventLine("pass", "TestOne") + eventLine("pass", "")),
		"duplicate": []byte(eventLine("pass", "TestOne") + eventLine("pass", "TestOne") +
			eventLine("pass", "TestTwo") + eventLine("pass", "")),
		"unknown": []byte(eventLine("pass", "TestOne") + eventLine("pass", "TestOther") +
			eventLine("pass", "TestTwo") + eventLine("pass", "")),
		"nested_skip": []byte(eventLine("pass", "TestOne") + eventLine("skip", "TestTwo/child") +
			eventLine("pass", "TestTwo") + eventLine("pass", "")),
		"root_skip": []byte(eventLine("pass", "TestOne") + eventLine("skip", "TestTwo") +
			eventLine("pass", "")),
		"missing_package_pass": []byte(eventLine("pass", "TestOne") +
			eventLine("pass", "TestTwo")),
		"non_json": append(bytes.Clone(complete), []byte("not-json\n")...),
	}
	for name, log := range fixtures {
		t.Run(name, func(t *testing.T) {
			if got := analyzeLog(log, s, 0); got.Valid {
				t.Fatalf("invalid log accepted: %s", name)
			}
		})
	}
	if got := analyzeLog(complete, s, 1); got.Valid {
		t.Fatal("nonzero process exit accepted")
	}
}

func TestReportCountsNestedPassWithoutConfusingRoot(t *testing.T) {
	s := shard{Index: 0, Roots: []string{"TestParent"}}
	log := []byte(eventLine("run", "TestParent") + eventLine("pass", "TestParent/child") +
		eventLine("pass", "TestParent") + eventLine("pass", ""))
	got := analyzeLog(log, s, 0)
	if !got.Valid || got.Completed != 1 {
		t.Fatalf("nested event corrupted root accounting: %+v", got)
	}
}

func TestEnvironmentIsolatesTestMainAndDisablesNetworkFetch(t *testing.T) {
	dir := t.TempDir()
	env, err := isolatedEnv(dir)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[strings.ToUpper(key)] = value
		}
	}
	for _, key := range []string{"TEMP", "TMP", "TMPDIR", "GOTMPDIR", "HOME",
		"USERPROFILE", "APPDATA", "LOCALAPPDATA"} {
		if !strings.HasPrefix(strings.ToUpper(values[key]), strings.ToUpper(dir)) {
			t.Errorf("%s escaped sandbox: %q", key, values[key])
		}
	}
	for key, want := range map[string]string{
		"GOPROXY": "off", "GOTOOLCHAIN": "local", "GOSUMDB": "off",
		"GOENV": "off", "GOVCS": "*:off", "GOFLAGS": "", "MCPHUB_E2E_SCHEDULER": "none",
	} {
		if values[key] != want {
			t.Errorf("%s=%q want %q", key, values[key], want)
		}
	}
}
