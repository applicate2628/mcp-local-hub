package api

import (
	"reflect"
	"testing"
)

func TestUnmanagedStdioCount(t *testing.T) {
	cases := []struct {
		name  string
		entry ScanEntry
		want  int
	}{
		{
			name: "unknown stdio is unmanaged drift",
			entry: ScanEntry{
				Name:   "local-stdio",
				Status: "unknown",
				ClientPresence: map[string]ClientEntry{
					"claude-code": {Transport: "stdio", Endpoint: "npx"},
				},
			},
			want: 1,
		},
		{
			name: "unknown with any stdio presence is unmanaged drift",
			entry: ScanEntry{
				Name:   "mixed",
				Status: "unknown",
				ClientPresence: map[string]ClientEntry{
					"claude-code": {Transport: "http", Endpoint: "https://example.test/mcp"},
					"codex-cli":   {Transport: "stdio", Endpoint: "uvx"},
				},
			},
			want: 1,
		},
		{
			name: "disabled unknown stdio is not unmanaged drift",
			entry: ScanEntry{
				Name:   "parked-cursor",
				Status: "unknown",
				ClientPresence: map[string]ClientEntry{
					"cursor": shapeCursorEntry(map[string]any{
						"command":  "npx",
						"disabled": true,
					}),
				},
			},
			want: 0,
		},
		{
			name: "enabled false unknown stdio is not unmanaged drift",
			entry: ScanEntry{
				Name:   "parked-codex",
				Status: "unknown",
				ClientPresence: map[string]ClientEntry{
					"codex-cli": shapeCodexEntry(map[string]any{
						"command": "npx",
						"enabled": false,
					}),
				},
			},
			want: 0,
		},
		{
			name: "enabled true unknown stdio remains unmanaged drift",
			entry: ScanEntry{
				Name:   "active-codex",
				Status: "unknown",
				ClientPresence: map[string]ClientEntry{
					"codex-cli": shapeCodexEntry(map[string]any{
						"command": "npx",
						"enabled": true,
					}),
				},
			},
			want: 1,
		},
		{
			name: "via hub is managed",
			entry: ScanEntry{
				Name:   "memory",
				Status: "via-hub",
				ClientPresence: map[string]ClientEntry{
					"claude-code": {Transport: "http", Endpoint: "http://127.0.0.1:9123/mcp"},
				},
			},
			want: 0,
		},
		{
			name: "can migrate is not unmanaged drift",
			entry: ScanEntry{
				Name:   "fetch",
				Status: "can-migrate",
				ClientPresence: map[string]ClientEntry{
					"claude-code": {Transport: "stdio", Endpoint: "npx"},
				},
			},
			want: 0,
		},
		{
			name: "external http is not unmanaged stdio",
			entry: ScanEntry{
				Name:   "context7",
				Status: "external",
				ClientPresence: map[string]ClientEntry{
					"claude-code": {Transport: "http", Endpoint: "https://mcp.context7.com/mcp"},
				},
			},
			want: 0,
		},
		{
			name: "unknown http only is not unmanaged stdio",
			entry: ScanEntry{
				Name:   "odd-remote",
				Status: "unknown",
				ClientPresence: map[string]ClientEntry{
					"claude-code": {Transport: "http", Endpoint: "https://example.test/mcp"},
				},
			},
			want: 0,
		},
		{
			name: "not installed is not client drift",
			entry: ScanEntry{
				Name:           "manifest-only",
				Status:         "not-installed",
				ClientPresence: map[string]ClientEntry{},
			},
			want: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UnmanagedStdioCount([]ScanEntry{tc.entry}); got != tc.want {
				t.Fatalf("UnmanagedStdioCount = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestUnmanagedStdioNamesSorted(t *testing.T) {
	entries := []ScanEntry{
		{
			Name:   "zeta",
			Status: "unknown",
			ClientPresence: map[string]ClientEntry{
				"codex-cli": {Transport: "stdio", Endpoint: "uvx"},
			},
		},
		{
			Name:   "alpha",
			Status: "unknown",
			ClientPresence: map[string]ClientEntry{
				"claude-code": {Transport: "stdio", Endpoint: "npx"},
			},
		},
		{
			Name:   "memory",
			Status: "via-hub",
			ClientPresence: map[string]ClientEntry{
				"claude-code": {Transport: "http", Endpoint: "http://127.0.0.1:9123/mcp"},
			},
		},
	}

	got := UnmanagedStdioNames(entries)
	want := []string{"alpha", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UnmanagedStdioNames = %#v, want %#v", got, want)
	}
}
