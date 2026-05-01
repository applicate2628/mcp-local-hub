// Package api — typed schedule parser for daemons.weekly_schedule.
// Memo D7: only `weekly DAY HH:MM` is accepted today; daily/cron Kinds
// extend the parser with new cases without callsite changes.
package api

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type ScheduleKind string

const (
	ScheduleWeekly ScheduleKind = "weekly"
)

type ScheduleSpec struct {
	Kind      ScheduleKind
	DayOfWeek int // 0=Sunday..6=Saturday (Kind == ScheduleWeekly)
	Hour      int // 0..23
	Minute    int // 0..59
}

const canonicalExample = "weekly Sun 03:00"

var dayLookup = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// weeklyRE mirrors (and tightens) the registry pattern. Parser is the
// authoritative validator — registry pattern is an early UI rejection.
var weeklyRE = regexp.MustCompile(`^weekly\s+([A-Za-z]{3})\s+([01]\d|2[0-3]):([0-5]\d)$`)

// ParseSchedule converts a settings-string schedule into a typed
// ScheduleSpec. Returns a typed error with the canonical example on
// rejection.
func ParseSchedule(s string) (*ScheduleSpec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty schedule (canonical example: %q)", canonicalExample)
	}
	m := weeklyRE.FindStringSubmatch(s)
	if m == nil {
		return nil, fmt.Errorf("schedule %q not in form `weekly DAY HH:MM` (canonical example: %q)", s, canonicalExample)
	}
	dow, ok := dayLookup[strings.ToLower(m[1])]
	if !ok {
		return nil, fmt.Errorf("schedule %q: unknown day %q (canonical example: %q)", s, m[1], canonicalExample)
	}
	hh, _ := strconv.Atoi(m[2])
	mm, _ := strconv.Atoi(m[3])
	return &ScheduleSpec{
		Kind:      ScheduleWeekly,
		DayOfWeek: dow,
		Hour:      hh,
		Minute:    mm,
	}, nil
}
