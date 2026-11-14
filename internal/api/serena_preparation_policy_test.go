package api

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestSerenaPreparationPolicySelection(t *testing.T) {
	p, e := ResolveSerenaPreparationPolicy(nil)
	if e != nil || p != DefaultSerenaPreparationPolicy() {
		t.Fatal("nil policy", e)
	}
	zero := SerenaPreparationPolicyV2{}
	if _, e = ResolveSerenaPreparationPolicy(&zero); e == nil {
		t.Fatal("zero override merged")
	}
	cases := []struct {
		name   string
		change func(*SerenaPreparationPolicyV2)
	}{
		{"uv missing", func(p *SerenaPreparationPolicyV2) { p.UVMinimumVersion = "" }},
		{"uv below", func(p *SerenaPreparationPolicyV2) { p.UVMinimumVersion = "0.13.3" }},
		{"uv malformed", func(p *SerenaPreparationPolicyV2) { p.UVMinimumVersion = ">=0.13.4" }},
		{"uv zero", func(p *SerenaPreparationPolicyV2) { p.UVMinimumVersion = "0.0.0" }},
		{"python missing", func(p *SerenaPreparationPolicyV2) { p.PythonMinimumVersion = "" }},
		{"python below", func(p *SerenaPreparationPolicyV2) { p.PythonMinimumVersion = "3.10.99" }},
		{"python malformed", func(p *SerenaPreparationPolicyV2) { p.PythonMinimumVersion = "03.11" }},
		{"work missing", func(p *SerenaPreparationPolicyV2) { p.WorkLimit = 0 }},
		{"tree missing", func(p *SerenaPreparationPolicyV2) { p.OwnedTreeLimits = SerenaTreeLimits{} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := p
			c.change(&v)
			if _, e := ResolveSerenaPreparationPolicy(&v); e == nil {
				t.Fatal("incomplete/weakened override admitted")
			}
		})
	}
	if same, e := ResolveSerenaPreparationPolicy(&p); e != nil || same != p {
		t.Fatal("equal complete override", e)
	}
	raised := p
	raised.PythonMinimumVersion = "3.12"
	raised.UVMinimumVersion = "0.14.0"
	raised.OwnedTreeLimits.Entries++
	if same, e := ResolveSerenaPreparationPolicy(&raised); e != nil || same != raised {
		t.Fatal("raised complete override", e)
	}
	args := raised.pythonFindArgs()
	if !reflect.DeepEqual(args, []string{"python", "find", "--no-project", "--system", "--no-config", "--no-python-downloads", "--offline", ">=3.12"}) {
		t.Fatal(args)
	}
	if p.pythonFindArgs()[7] != ">=3.11" {
		t.Fatal("default discovery floor")
	}
	baseline, _ := p.IdentitySHA256()
	other, _ := raised.IdentitySHA256()
	if other == baseline {
		t.Fatal("raised policy lost identity")
	}
	future := p
	future.PythonMinimumVersion = "4.0"
	future.UVMinimumVersion = "2.0.0"
	if e := future.Validate(); e != nil {
		t.Fatal("upper/family gate", e)
	}
}

func TestSerenaPreparationMinimumVersions(t *testing.T) {
	p := DefaultSerenaPreparationPolicy()
	for _, c := range []struct {
		banner string
		ok     bool
	}{
		{"uv 0.13.3", false}, {"uv 0.13.4", true}, {"uv 0.13.6 (abc 2026-11-01)\n", true},
		{"uv 1.0.0", true}, {"uv 0.13.4rc1", false}, {"uv 0.13.4-rc.1", false}, {"uv 0.14.0rc1", true},
		{"uv 0.13.4+vendor.1", true}, {"uv 0.13.4 (build)", true}, {"", false}, {"uv", false},
		{"uv unknown", false}, {"uv 0.0.0", false}, {"uv 0.13.4\nforeign", false}, {"uv 0.13.4 bogus", false},
	} {
		t.Run(c.banner, func(t *testing.T) {
			if e := p.validateUVVersion([]byte(c.banner)); (e == nil) != c.ok {
				t.Fatalf("%q: %v", c.banner, e)
			}
		})
	}
	for _, c := range []struct {
		v  []int
		ok bool
	}{
		{nil, false}, {[]int{}, false}, {[]int{0, 0, 0}, false}, {[]int{3, 11}, false}, {[]int{3, 11, 0, 0}, false},
		{[]int{3, 10, 99}, false}, {[]int{3, 11, 0}, true}, {[]int{3, 11, 9}, true}, {[]int{3, 15, 0}, true}, {[]int{4, 0, 0}, true}, {[]int{3, -1, 0}, false},
	} {
		if e := p.validatePythonVersion(c.v); (e == nil) != c.ok {
			t.Fatalf("%v: %v", c.v, e)
		}
	}
	p.PythonMinimumVersion = "3.12.1"
	if p.validatePythonVersion([]int{3, 12, 0}) == nil || p.validatePythonVersion([]int{3, 12, 1}) != nil {
		t.Fatal("raised numeric floor")
	}
}

func TestSerenaPreparationHistoricalPolicyWire(t *testing.T) {
	p := historicalSerenaPreparationPolicy()
	raw, e := json.Marshal(p)
	if e != nil {
		t.Fatal(e)
	}
	want := `{"version":1,"uv_version":"0.9.21","interpreter_selector":"3.13.11","work_limit_ns":300000000000,"cleanup_limit_ns":45000000000,"owned_tree_limits":{"entries":131072,"depth":64,"record_bytes":67108864,"encoded_path_bytes":67108864,"logical_bytes":34359738368,"open_handles":256}}`
	if string(raw) != want {
		t.Fatalf("historical bytes changed: %s", raw)
	}
	sha, e := p.IdentitySHA256()
	if e != nil || sha != "6d3bc7eeb560fa529f85d93f68c5f4a8d794e0e141450cf6ecf049bfe69ed49b" {
		t.Fatal(sha, e)
	}
}

func TestSerenaPreparationPartialOutcomePreservesCauses(t *testing.T) {
	primary := errors.New("original execution")
	cleanup := errors.New("original close")
	v := preparedSerenaCommandResult{Stdout: []byte("uv 0.13.6"), Stderr: []byte("partial")}
	e := validateSerenaUVProbe(v, "op", DefaultSerenaPreparationPolicy(), errors.Join(primary, cleanup))
	if e == nil || !errors.Is(e, primary) || !errors.Is(e, cleanup) {
		t.Fatal("lost joined causes", e)
	}
	partial := SerenaPreparationInputs{uvVersionProbe: &v, interpreterProbe: &v, Policy: DefaultSerenaPreparationPolicy()}
	carrier := &serenaPreparationInputsError{cause: e, inputs: partial}
	var found *serenaPreparationInputsError
	if !errors.As(carrier, &found) || found.inputs.uvVersionProbe != &v || string(found.inputs.interpreterProbe.Stderr) != "partial" || !errors.Is(carrier, cleanup) {
		t.Fatal("partial result erased")
	}
	raw, _ := json.Marshal(carrier)
	if string(raw) != "{}" {
		t.Fatal("private raw results serialized", string(raw))
	}
}
