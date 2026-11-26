package api

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRecentHubMcpEventsTailsPhysicalLines(t *testing.T) {
	hubMcpStateTestHelper(t)
	dir, err := DaemonStateDir()
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, hubMcpLogFileLeaf)
	write := func(file, body string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(log+".1", "{\"event\":\"rotated\"}\n")
	for _, tc := range []struct {
		name, body string
		present    bool
		n          int
		want       []string
	}{
		{name: "missing ignores rotation", n: 5},
		{name: "empty", present: true, n: 5, want: []string{}},
		{name: "no terminal newline", body: "{\"event\":\"a\"}\n{\"event\":\"b\"}", present: true, n: 1, want: []string{"b"}},
		{name: "one terminal newline", body: "{\"event\":\"a\"}\n{\"event\":\"b\"}\n", present: true, n: 2, want: []string{"a", "b"}},
		{name: "blank and malformed count toward limit", body: "{\"event\":\"a\"}\nmalformed\n\n{\"event\":\"b\"}\r\n", present: true, n: 3, want: []string{"b"}},
		{name: "blank last physical line", body: "{\"event\":\"a\"}\n\n", present: true, n: 1, want: []string{}},
		{name: "json null remains a nil map", body: "{\"event\":\"a\"}\nnull\n", present: true, n: 1, want: []string{"<null>"}},
		{name: "nonpositive request", body: "{\"event\":\"a\"}\n", present: true, n: 0},
		{name: "large limit", body: "{\"event\":\"a\"}\n", present: true, n: 1 << 30, want: []string{"a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.present {
				write(log, tc.body)
			}
			got, err := RecentHubMcpEvents(tc.n)
			if err != nil {
				t.Fatal(err)
			}
			var events []string
			if got != nil {
				events = make([]string, 0, len(got))
			}
			for _, m := range got {
				if m == nil {
					events = append(events, "<null>")
					continue
				}
				if s, ok := m["event"].(string); ok {
					events = append(events, s)
				} else {
					t.Fatalf("unexpected record: %#v", m)
				}
			}
			if !reflect.DeepEqual(events, tc.want) {
				t.Fatalf("events=%v (nil=%t), want %v (nil=%t)", events, events == nil, tc.want, tc.want == nil)
			}
		})
	}
}
