package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestSupervisorFrozenPortCanonicalCopyAndWire(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	input := []SupervisorFrozenPort{
		{TaskName: "z", Port: 1, ArgvSHA256: a},
		{TaskName: "a", Port: 65535, ArgvSHA256: a},
		{TaskName: "a", Port: 1, ArgvSHA256: b},
		{TaskName: "a", Port: 1, ArgvSHA256: a},
	}
	preimage := append([]SupervisorFrozenPort(nil), input...)
	canonical := CanonicalSupervisorFrozenPorts(input)
	want := []SupervisorFrozenPort{input[3], input[2], input[1], input[0]}
	if !reflect.DeepEqual(canonical, want) || !reflect.DeepEqual(input, preimage) {
		t.Fatalf("canonical=%v input=%v", canonical, input)
	}
	if err := ValidateSupervisorFrozenPorts(canonical); err != nil {
		t.Fatal(err)
	}
	canonical[0].TaskName = "changed"
	if !reflect.DeepEqual(input, preimage) {
		t.Fatal("canonical rows alias input")
	}
	if empty := CanonicalSupervisorFrozenPorts(nil); empty == nil || len(empty) != 0 {
		t.Fatal("canonical empty rows are not nonnull")
	}
	raw, err := json.Marshal(input[3])
	expected := []byte(`{"task_name":"a","port":1,"argv_sha256":"` + a + `"}`)
	if err != nil || !bytes.Equal(raw, expected) {
		t.Fatalf("row wire=%q err=%v", raw, err)
	}
}

func TestSupervisorFrozenPortValidation(t *testing.T) {
	valid := SupervisorFrozenPort{TaskName: "task", Port: 1, ArgvSHA256: strings.Repeat("a", 64)}
	for _, tc := range []struct {
		name string
		rows []SupervisorFrozenPort
		want error
	}{
		{name: "nil-rows"},
		{name: "empty-rows", rows: []SupervisorFrozenPort{}},
		{name: "blank-task", rows: []SupervisorFrozenPort{{TaskName: " \t", Port: valid.Port, ArgvSHA256: valid.ArgvSHA256}}, want: ErrInvalidSupervisorFrozenPort},
		{name: "zero-port", rows: []SupervisorFrozenPort{{TaskName: valid.TaskName, ArgvSHA256: valid.ArgvSHA256}}, want: ErrInvalidSupervisorFrozenPort},
		{name: "above-range", rows: []SupervisorFrozenPort{{TaskName: valid.TaskName, Port: 65536, ArgvSHA256: valid.ArgvSHA256}}, want: ErrInvalidSupervisorFrozenPort},
		{name: "missing-digest", rows: []SupervisorFrozenPort{{TaskName: valid.TaskName, Port: valid.Port}}, want: ErrInvalidSupervisorFrozenPort},
		{name: "uppercase-digest", rows: []SupervisorFrozenPort{{TaskName: valid.TaskName, Port: valid.Port, ArgvSHA256: strings.ToUpper(valid.ArgvSHA256)}}, want: ErrInvalidSupervisorFrozenPort},
		{name: "duplicate", rows: []SupervisorFrozenPort{valid, valid}, want: ErrUnsortedSupervisorFrozenPorts},
		{name: "unsorted", rows: []SupervisorFrozenPort{{TaskName: "z", Port: valid.Port, ArgvSHA256: valid.ArgvSHA256}, valid}, want: ErrUnsortedSupervisorFrozenPorts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateSupervisorFrozenPorts(tc.rows); !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want=%v", err, tc.want)
			}
		})
	}
}
