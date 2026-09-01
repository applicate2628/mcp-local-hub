package process

import (
	"reflect"
	"testing"
)

func TestTokenizeWindowsCommandLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want []string
	}{
		{
			name: "quoted executable and workspace",
			line: `"C:\\Program Files\\mcphub.exe" daemon --server vcpkg --daemon default --workspace "C:\\My Proj\\ws"`,
			want: []string{`C:\\Program Files\\mcphub.exe`, "daemon", "--server", "vcpkg", "--daemon", "default", "--workspace", `C:\\My Proj\\ws`},
		},
		{
			name: "npm shim",
			line: `node.exe "C:\\npm root\\mcphub.js" daemon --server vcpkg --daemon default`,
			want: []string{"node.exe", `C:\\npm root\\mcphub.js`, "daemon", "--server", "vcpkg", "--daemon", "default"},
		},
		{
			name: "escaped quote",
			line: `mcphub.exe --label \"quoted\"`,
			want: []string{"mcphub.exe", "--label", `"quoted"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TokenizeWindowsCommandLine(tc.line); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("TokenizeWindowsCommandLine(%q) = %#v, want %#v", tc.line, got, tc.want)
			}
		})
	}
}
