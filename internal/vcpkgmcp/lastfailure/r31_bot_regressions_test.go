package lastfailure

import "testing"

func TestR31CMakeInstallFailureKeepsInstallPhase(t *testing.T) {
	root := writePortLogs(t, "installfailure", map[string]string{
		"cl.vcpkg_abi_info.txt":  "abi\n",
		"install-cl-rel-err.log": "CMake Error at cmake_install.cmake:54 (file):\n  file INSTALL cannot copy file\n",
	})
	result := LastFailure(Args{Port: "installfailure", BuildtreesRoot: root}, testDeps())
	if result.Phase != PhaseInstall {
		t.Fatalf("phase=%q, want %q for a genuine CMake install-step failure; result=%+v", result.Phase, PhaseInstall, result)
	}
}

func TestR31LatestCommandOwnsInferredTripletUnlessHeaderOwnsIt(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "latest command-derived value",
			data: "command: vcpkg install first --triplet=x64-windows\n" +
				"command: vcpkg install second --triplet=arm64-windows\n",
			want: "arm64-windows",
		},
		{
			name: "latest command without triplet clears inferred value",
			data: "command: vcpkg install first --triplet=x64-windows\n" +
				"command: vcpkg install second\n",
			want: "",
		},
		{
			name: "header remains authoritative",
			data: "[2026-08-09 12:00:00] triplet=cl\n" +
				"command: vcpkg install first --triplet=x64-windows\n" +
				"command: vcpkg install second --triplet=arm64-windows\n",
			want: "cl",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info, ok, err := ParseWrapperContent([]byte(test.data))
			if err != nil || !ok {
				t.Fatalf("ParseWrapperContent() = ok=%v err=%v", ok, err)
			}
			if info.Triplet != test.want {
				t.Fatalf("Triplet=%q, want %q", info.Triplet, test.want)
			}
		})
	}
}
