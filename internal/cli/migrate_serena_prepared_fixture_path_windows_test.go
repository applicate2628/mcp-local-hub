//go:build windows && serena_qualification && test_state_path_env

package cli

import (
	"strings"
	"testing"
)

func TestSerenaQualificationWorkingDirectoryBudget(t *testing.T) {
	for _, test := range []struct {
		name     string
		stateDir string
		wantErr  bool
	}{
		{name: "short-retained-case", stateDir: `D:\dev\ChatGPT\q9189\s-1234567890`},
		{name: "ascii-at-limit", stateDir: `D:\` + strings.Repeat("a", 96)},
		{name: "ascii-over-limit", stateDir: `D:\` + strings.Repeat("a", 97), wantErr: true},
		{name: "surrogate-pair-at-limit", stateDir: `D:\` + strings.Repeat("a", 94) + "\U0001F642"},
		{name: "surrogate-pair-over-limit", stateDir: `D:\` + strings.Repeat("a", 95) + "\U0001F642", wantErr: true},
		{
			name: "short-run-root-with-original-temp-name-still-too-long",
			stateDir: `D:\dev\ChatGPT\q9189\tmp\` +
				"TestMigrateSerena_CatalogDynamicPool_RuntimeLegacyMissing_Proceeds"[:64] +
				`4294967295\002\hardened-parent`,
			wantErr: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateSerenaQualificationWorkingDirectoryBudget(test.stateDir)
			if (err != nil) != test.wantErr {
				t.Fatalf("working-directory budget error = %v; want error = %t", err, test.wantErr)
			}
		})
	}
}
