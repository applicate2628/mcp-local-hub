package patchesapply

import (
	"fmt"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR17WalkRejectsDuplicateElseAndElseifAfterElse(t *testing.T) {
	env := newVarEnv("", "", "", nil, nil)
	for name, source := range map[string]string{
		"duplicate_else":    "if(ON)\nelse()\nelse()\nendif()\n",
		"elseif_after_else": "if(OFF)\nelse()\nelseif(ON)\nendif()\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, structural := walkPortfile(source, env)
			if structural != parserStructuralExpressionUnparsable {
				t.Fatalf("structural=%v, want expression_unparsable", structural)
			}
		})
	}
}

func TestR17UnquotedEscapesRemainInsideOnePatchArgument(t *testing.T) {
	tokens := tokenize(`fix\ one.patch patch\#1.patch`)
	want := []string{"fix one.patch", "patch#1.patch"}
	if len(tokens) != len(want) {
		t.Fatalf("tokens=%+v, want %q", tokens, want)
	}
	for i := range want {
		if tokens[i].Text != want[i] {
			t.Fatalf("tokens[%d]=%q, want %q", i, tokens[i].Text, want[i])
		}
	}
	portfile := "vcpkg_from_github(REPO owner/repo PATCHES fix\\ one.patch patch\\#1.patch)\n"
	dir := writePort(t, portfile, want...)
	result := ApplyOrder(Args{PortDir: dir, Triplet: "x64-windows"})
	if result.Status != evidence.StatusOK || len(result.Applied) != len(want) || len(result.Missing) != 0 {
		t.Fatalf("result=%+v, want exactly two existing escaped-name patches", result)
	}
	for i, patch := range result.Applied {
		if !strings.HasSuffix(patch.ResolvedPath, want[i]) {
			t.Fatalf("applied[%d]=%+v, want suffix %q", i, patch, want[i])
		}
	}
}

func TestR17VarOverridesAreAdmittedBeforeFilesystemWork(t *testing.T) {
	tooMany := make(map[string]string, MaxVarOverrides+1)
	for i := 0; i < MaxVarOverrides+1; i++ {
		tooMany[fmt.Sprintf("K%03d", i)] = "v"
	}
	for name, overrides := range map[string]map[string]string{
		"count":     tooMany,
		"name":      {strings.Repeat("n", MaxVarOverrideNameBytes+1): "v"},
		"value":     {"K": strings.Repeat("v", MaxVarOverrideValueBytes+1)},
		"aggregate": {"A": strings.Repeat("a", MaxVarOverrideTotalBytes/2), "B": strings.Repeat("b", MaxVarOverrideTotalBytes/2)},
	} {
		t.Run(name, func(t *testing.T) {
			result := ApplyOrder(Args{VarOverrides: overrides})
			if result.Status != evidence.StatusFailed || result.Reason != ReasonVarOverridesLimitExceeded {
				t.Fatalf("status/reason=%s/%s, want failed/%s", result.Status, result.Reason, ReasonVarOverridesLimitExceeded)
			}
		})
	}
}
