package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

func TestPreparedSerenaBuildConstraintReceipt(t *testing.T) {
	for _, tc := range []struct {
		name, dependencies string
		wantError          bool
	}{
		{"actual-fixed-policy", `[{ name = "setuptools", specifier = "<84" }]`, false},
		{"missing", `[]`, true},
		{"wrong-package", `[{ name = "other", specifier = "<84" }]`, true},
		{"wrong-range", `[{ name = "setuptools", specifier = "<85" }]`, true},
		{"missing-range", `[{ name = "setuptools" }]`, true},
		{"wrong-type", `[{ name = "setuptools", specifier = 84 }]`, true},
		{"extra-source", `[{ name = "setuptools", specifier = "<84", url = "https://example.invalid/backend" }]`, true},
		{"extra-marker", `[{ name = "setuptools", specifier = "<84", marker = "python_version < '3.14'" }]`, true},
		{"duplicate", `[{ name = "setuptools", specifier = "<84" }, { name = "setuptools", specifier = "<84" }]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var receipt preparedSerenaUVReceipt
			if err := toml.Unmarshal([]byte("[tool]\nbuild-constraint-dependencies = "+tc.dependencies+"\n"), &receipt); err != nil {
				t.Fatal(err)
			}
			err := validatePreparedSerenaBuildConstraintReceipt(receipt)
			if (err != nil) != tc.wantError {
				t.Fatalf("receipt validation = %v, want error %v", err, tc.wantError)
			}
			if err != nil && !strings.Contains(err.Error(), "SERENA_BUILD_CONSTRAINT_RECEIPT_MISMATCH") {
				t.Fatal(err)
			}
		})
	}
}

func TestPreparedSerenaBuildConstraintRequiresOriginalAttempt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, tree := range []*SerenaOwnedTree{nil, {}, {request: SerenaOwnedTreeRequest{Kind: "generation"}}} {
		constraint, err := acquirePreparedSerenaBuildConstraint(ctx, tree)
		if err == nil || constraint != nil {
			t.Fatal("unissued attempt manufactured a constraint capability")
		}
	}
}

func TestPreparedSerenaInvocationPreflightPreservesMissingClosure(t *testing.T) {
	result, err := runPreparedSerenaInvocation(context.Background(), preparedSerenaInvocation{})
	if err == nil || result.containedAttempted || result.Closure.ReceiptSHA256 != "" || result.Closure.NoChildKnown || result.Closure.NoChildReleased {
		t.Fatal("local preflight invented contained entry or child settlement", err)
	}
}
