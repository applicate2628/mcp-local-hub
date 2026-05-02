package api

import (
	"strings"
	"testing"
)

func TestParseSchedule_ValidWeekly(t *testing.T) {
	cases := []struct {
		in   string
		want ScheduleSpec
	}{
		{"weekly Sun 03:00", ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 0, Hour: 3, Minute: 0}},
		{"weekly Mon 14:30", ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 1, Hour: 14, Minute: 30}},
		{"weekly Sat 23:59", ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 6, Hour: 23, Minute: 59}},
		{"weekly sun 03:00", ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 0, Hour: 3, Minute: 0}},
		{"weekly TUE 09:05", ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 2, Hour: 9, Minute: 5}},
	}
	for _, c := range cases {
		got, err := ParseSchedule(c.in)
		if err != nil {
			t.Errorf("ParseSchedule(%q): %v", c.in, err)
			continue
		}
		if *got != c.want {
			t.Errorf("ParseSchedule(%q) = %+v, want %+v", c.in, *got, c.want)
		}
	}
}

func TestParseSchedule_Rejected(t *testing.T) {
	cases := []string{
		"",
		"weekly",
		"weekly Sun",
		"weekly Sun 03",
		"weekly Sun 24:00",
		"weekly Sun 99:99",
		"weekly Sun 23:60",
		"daily 03:00",
		"weekly Funday 03:00",
		"weekly Sun 3:00",
		"0 3 * * 0",
		"weekly Sun 03:00 UTC",
	}
	for _, in := range cases {
		_, err := ParseSchedule(in)
		if err == nil {
			t.Errorf("ParseSchedule(%q): expected error, got nil", in)
		}
	}
}

func TestParseSchedule_ErrorIncludesExample(t *testing.T) {
	_, err := ParseSchedule("daily 03:00")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "weekly Sun 03:00") {
		t.Errorf("error %q missing canonical example", err.Error())
	}
}
