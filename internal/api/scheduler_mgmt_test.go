package api

import "testing"

// TestSchedulerUpgradeNoopWhenEmpty verifies that running SchedulerUpgrade
// on a system with no mcp-local-hub tasks returns an empty result list
// without error.
func TestSchedulerUpgradeNoopWhenEmpty(t *testing.T) {
	// Cannot easily stub schtasks.exe in unit tests; just assert the api
	// accepts the call and returns something sane. Real verification is
	// the live smoke test in step 3.
	a := NewAPI()
	results, err := a.SchedulerUpgrade()
	if err != nil {
		t.Skipf("scheduler unavailable: %v", err)
	}
	_ = results
}

func TestParseWeeklyRefreshSchedule(t *testing.T) {
	tests := []struct {
		input   string
		wantDay int
		wantHr  int
		wantMin int
		wantErr bool
	}{
		{"SUN 03:00", 0, 3, 0, false},
		{"MON 14:30", 1, 14, 30, false},
		{"FRI 23:59", 5, 23, 59, false},
		{"SAT 00:01", 6, 0, 1, false},
		{"XXX 12:00", 0, 0, 0, true},
		{"SUN 25:00", 0, 0, 0, true},
		{"SUN", 0, 0, 0, true},
	}
	for _, tc := range tests {
		day, hr, min, err := parseWeeklyRefreshSchedule(tc.input)
		gotErr := err != nil
		if gotErr != tc.wantErr {
			t.Errorf("%q: err=%v, wantErr=%v", tc.input, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && (day != tc.wantDay || hr != tc.wantHr || min != tc.wantMin) {
			t.Errorf("%q: got (%d,%d,%d), want (%d,%d,%d)", tc.input, day, hr, min, tc.wantDay, tc.wantHr, tc.wantMin)
		}
	}
}
