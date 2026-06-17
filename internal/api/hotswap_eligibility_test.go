package api

import "testing"

// TestClassifyHotSwapEligibility_ProxyVsGlobal pins the core pure-function
// verdict: a daemon with a non-nil RuntimeSpec (a materialized proxy — serena
// dynamic-pool, workspace-scoped LSP) is hot-swap eligible (the proxy fronts a
// stable external port); a generic global daemon (nil RuntimeSpec) is not.
func TestClassifyHotSwapEligibility_ProxyVsGlobal(t *testing.T) {
	cases := []struct {
		name         string
		daemon       SupervisorDaemon
		wantEligible bool
		wantReason   string
	}{
		{
			name: "proxy-fronted daemon (RuntimeSpec set) is eligible",
			daemon: SupervisorDaemon{
				TaskName:    `\mcp-local-hub-serena-claude`,
				Server:      "serena",
				Port:        9121,
				RuntimeSpec: &DaemonRuntimeSpec{SpecVersion: DaemonRuntimeSpecVersion, ExternalPort: 9121, UpstreamPort: 19121},
			},
			wantEligible: true,
			wantReason:   HotSwapReasonProxyFronted,
		},
		{
			name: "global direct-port daemon (nil RuntimeSpec) is NOT eligible",
			daemon: SupervisorDaemon{
				TaskName: `\mcp-local-hub-memory-default`,
				Server:   "memory",
				Port:     9123,
				// RuntimeSpec nil: generic `mcphub daemon --server memory`.
			},
			wantEligible: false,
			wantReason:   HotSwapReasonDirectPort,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyHotSwapEligibility(tc.daemon)
			if got.Eligible != tc.wantEligible {
				t.Errorf("Eligible = %v, want %v", got.Eligible, tc.wantEligible)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
		})
	}
}

// TestClassifyHotSwapEligibility_IsPure asserts the classifier does not mutate
// its input descriptor (no I/O, no side effects) — it is safe to call inside
// the reconcile drift loop on the live descriptor.
func TestClassifyHotSwapEligibility_IsPure(t *testing.T) {
	d := SupervisorDaemon{TaskName: `\mcp-local-hub-time-default`, Server: "time", Port: 9128}
	_ = ClassifyHotSwapEligibility(d)
	if d.RuntimeSpec != nil {
		t.Error("classifier mutated the descriptor's RuntimeSpec")
	}
	if d.Server != "time" || d.Port != 9128 {
		t.Error("classifier mutated the descriptor's fields")
	}
}
