package api

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/lldb"
	"mcp-local-hub/internal/secrets"
)

type AdmissionFinding struct {
	ID       string
	Name     string
	Reason   string
	Fix      string
	Optional bool
}

type AdmissionScope struct {
	DaemonFilter string
}

func AdmissionCheck(m *config.ServerManifest, scope AdmissionScope) []AdmissionFinding {
	if m == nil {
		return nil
	}

	var findings []AdmissionFinding
	add := func(id, name, reason, fix string, optional bool) {
		findings = append(findings, AdmissionFinding{
			ID:       id,
			Name:     name,
			Reason:   reason,
			Fix:      fix,
			Optional: optional,
		})
	}

	if m.Transport == config.TransportRemoteHTTP {
		if _, err := ensureCanonicalMcphubPresent(); err != nil {
			_, fix := LauncherGuidance("mcphub")
			add("canonical-mcphub", "mcphub binary", err.Error(), fix, false)
		}
		if _, err := ExpandSecrets(m.URL, nil); err != nil {
			add("remote-url-secret", "remote URL secrets", fmt.Sprintf("install remote-http manifest %s: expand url: %v", m.Name, err), "Set the missing remote URL secret or fix the malformed ${secret:KEY} placeholder.", false)
		}
		if _, err := ExpandSecretsMap(m.Headers, nil); err != nil {
			add("remote-headers-secret", "remote header secrets", fmt.Sprintf("install remote-http manifest %s: expand headers: %v", m.Name, err), "Set the missing remote header secret or fix the malformed ${secret:KEY} placeholder.", false)
		}
		return findings
	}

	launcherOptional := m.Kind == config.KindWorkspaceScoped && m.DaemonTemplate == nil
	if _, err := exec.LookPath(m.Command); err != nil {
		_, fix := LauncherGuidance(m.Command)
		add("command-on-path", "launcher: "+filepath.Base(m.Command), fmt.Sprintf("command %q not found on PATH — %s: %v", m.Command, fix, err), fix, launcherOptional)
	} else if rt := runtimeBehindLauncher(m.Command); rt != "" {
		if _, err := exec.LookPath(rt); err != nil {
			_, fix := LauncherGuidance(rt)
			add("runtime-behind-launcher", "runtime: "+rt, fmt.Sprintf("runtime %q (needed by %q) not found on PATH — %s: %v", rt, m.Command, fix, err), fix, launcherOptional)
		}
	}

	for _, bin := range m.RequiredBinaries {
		if !binaryAvailable(bin) {
			_, fix := LauncherGuidance(bin)
			add("required-binary", "binary: "+filepath.Base(bin), fmt.Sprintf("required binary %q not found — %s", filepath.Base(bin), fix), fix, false)
		}
	}
	seenLanguageBin := map[string]bool{}
	for _, lang := range m.Languages {
		for _, bin := range lang.RequiredBinaries {
			if bin == "" || seenLanguageBin[bin] {
				continue
			}
			seenLanguageBin[bin] = true
			if !binaryAvailable(bin) {
				_, fix := LauncherGuidance(bin)
				add("language-required-binary", "binary: "+filepath.Base(bin), fmt.Sprintf("required binary %q not found — %s", filepath.Base(bin), fix), fix, true)
			}
		}
	}
	if manifestNeedsGit(m) && !binaryAvailable("git") {
		_, fix := LauncherGuidance("git")
		add("git-for-uvx-git-source", "binary: git", fmt.Sprintf("git is required to fetch the uvx git+ source but is not on PATH — %s", fix), fix, false)
	}

	if m.Transport == config.TransportStdioBridge && len(m.BaseArgs) >= 2 && m.BaseArgs[0] == "lldb-bridge" {
		addr := m.BaseArgs[1]
		if _, _, err := lldb.ParseHostPort(addr); err != nil {
			add("lldb-bridge-address", "debugger: lldb", fmt.Sprintf("lldb-bridge address %q is not a valid host:port (e.g. localhost:47000): %v", addr, err), "Set base_args[1] to a valid host:port (e.g. localhost:47000) in the manifest.", false)
		} else if !bridgeListenerUp(addr) && !binaryAvailable("lldb") {
			_, fix := LauncherGuidance("lldb")
			add("lldb-bridge-listener-or-binary", "debugger: lldb", fmt.Sprintf("lldb-bridge: no MCP listener on %s and no lldb binary found — %s, or start an lldb MCP listener on %s first", addr, fix, addr), fix+" — OR start an lldb MCP listener on "+addr+" first, then re-run install.", false)
		}
	}

	if !launcherOptional {
		for _, c := range entryScriptCheckTargets(m) {
			if scope.DaemonFilter != "" && c.daemon != "" && c.daemon != scope.DaemonFilter {
				continue
			}
			if !c.resolvable {
				add("entry-script-unresolvable", "script: "+c.label, "relative entry script with no absolute daemon cwd — the daemon inherits an unpredictable working directory, so the script cannot be verified here", "Make base_args[0] absolute, or set an absolute daemon cwd, so readiness can verify the entry script exists.", true)
				continue
			}
			if ok, reason := entryScriptStatus(c.path); !ok {
				add("entry-script-present", "script: "+c.label, fmt.Sprintf("entry script %q for %q %s — install/clone the server so base_args[0] points at the file, then re-run install", filepath.Base(c.path), normalizeLauncher(m.Command), reason), "Install/clone the server so the manifest's base_args[0] script path exists and points at a file (not a directory), then re-run install.", false)
			}
		}
	}

	if _, err := ensureCanonicalMcphubPresent(); err != nil {
		_, fix := LauncherGuidance("mcphub")
		add("canonical-mcphub", "mcphub binary", err.Error(), fix, false)
	}

	if err := validateDynamicPoolManifest(m); err != nil {
		add("dynamic-pool", "dynamic-pool", err.Error(), "Fix the daemon_template manifest: native-http transport, a non-empty daemon_template.context, and no --context token in base_args/extra_args_template.", false)
	}

	for _, d := range m.Daemons {
		if scope.DaemonFilter != "" && d.Name != scope.DaemonFilter {
			continue
		}
		if d.Port < 1 || d.Port > 65535 {
			add("external-port-range", fmt.Sprintf("port %d (%s)", d.Port, d.Name), fmt.Sprintf("daemon %s/%s: port %d is outside the valid range 1..65535", m.Name, d.Name, d.Port), "Set a valid free fixed port (1..65535) for this daemon in the manifest.", false)
			continue
		}
		if preflightPortInUse(d.Port) && !portHeldByOurDaemonForPortArm(d.Port, m.Name, d.Name, false) {
			add("external-port-free", fmt.Sprintf("port %d (%s)", d.Port, d.Name), fmt.Sprintf("port %d already in use (needed for daemon %s/%s)", d.Port, m.Name, d.Name), "Free the port or change the daemon port in the manifest.", false)
		}
		if m.Transport != config.TransportNativeHTTP {
			continue
		}
		internal := d.Port + config.NativeHTTPInternalPortOffset
		if internal < 1 || internal > 65535 {
			add("native-http-internal-range", fmt.Sprintf("internal port %d (%s)", internal, d.Name), fmt.Sprintf("daemon %s/%s native-http upstream port %d is outside the valid range 1..65535 (external=%d, internal=external+%d)", m.Name, d.Name, internal, d.Port, config.NativeHTTPInternalPortOffset), "Free the port or change the daemon port (internal = external + offset, both must be 1..65535).", false)
			continue
		}
		if preflightPortInUse(internal) && !portHeldByOurDaemonForPortArm(internal, m.Name, d.Name, true) {
			add("native-http-internal-free", fmt.Sprintf("internal port %d (%s)", internal, d.Name), fmt.Sprintf("internal port %d already in use (needed for native-http upstream of %s/%s; external=%d, internal=external+%d)", internal, m.Name, d.Name, d.Port, config.NativeHTTPInternalPortOffset), "Free the port or change the daemon port (internal = external + offset, both must be 1..65535).", false)
		}
	}

	findings = append(findings, admissionPortPoolFindings(m)...)

	if secrets.HasSecretRef(m.Env) {
		if _, err := secrets.OpenVaultOptional(secrets.DefaultKeyPath(), secrets.DefaultVaultPath()); err != nil {
			add("secrets-vault-readable", "secrets vault", fmt.Sprintf("manifest %s uses secret refs but the vault is unreadable: %v", m.Name, err), "Fix or remove the corrupt vault — a secret-using server fails to start when it cannot be read.", false)
		}
	}
	for k, v := range m.Env {
		if strings.HasPrefix(v, "file:") {
			add("file-env-ref", "env: "+k, fmt.Sprintf("manifest %s env[%s] uses a file: ref, which the daemon launch path cannot resolve (mcphub has no local config map); replace it with a secret: ref or a literal value", m.Name, k), "Replace the file: env ref with a secret: ref (vault) or a literal value in the manifest.", false)
		}
	}

	return findings
}

func containsNonOptional(findings []AdmissionFinding) bool {
	for _, f := range findings {
		if !f.Optional {
			return true
		}
	}
	return false
}

func scopeForPreflight(daemonFilter string) AdmissionScope {
	return AdmissionScope{DaemonFilter: daemonFilter}
}

type manifestPortPool struct {
	pool           *config.PortPool
	daemonTemplate bool
}

func manifestPortPools(m *config.ServerManifest) []manifestPortPool {
	var pools []manifestPortPool
	if m.PortPool != nil {
		pools = append(pools, manifestPortPool{pool: m.PortPool})
	}
	if m.DaemonTemplate != nil && m.DaemonTemplate.PortPool != nil {
		pools = append(pools, manifestPortPool{pool: m.DaemonTemplate.PortPool, daemonTemplate: true})
	}
	return pools
}

func portPoolName(p *config.PortPool) string {
	return fmt.Sprintf("port pool %d-%d", p.Start, p.End)
}

func nativeHTTPPoolExternalCeiling() int {
	return 65535 - config.NativeHTTPInternalPortOffset
}

func nativeHTTPPoolOverflows(p *config.PortPool, nativeHTTP bool) bool {
	return nativeHTTP && p != nil && p.End >= p.Start && p.End > nativeHTTPPoolExternalCeiling()
}

func nativeHTTPPoolOverflowReason(p *config.PortPool) string {
	highestUpstream := int64(p.End) + int64(config.NativeHTTPInternalPortOffset)
	return fmt.Sprintf("native-http port pool %d-%d exceeds the external port ceiling %d; the highest upstream port would be %d (external+%d) outside 1..65535", p.Start, p.End, nativeHTTPPoolExternalCeiling(), highestUpstream, config.NativeHTTPInternalPortOffset)
}

func nativeHTTPPoolOverflowFix() string {
	return fmt.Sprintf("Keep the pool's ports at or below %d so external+%d stays within 1..65535.", nativeHTTPPoolExternalCeiling(), config.NativeHTTPInternalPortOffset)
}

func admissionPortPoolFindings(m *config.ServerManifest) []AdmissionFinding {
	pools := manifestPortPools(m)
	if len(pools) == 0 {
		return nil
	}

	var findings []AdmissionFinding
	add := func(id, name, reason, fix string, optional bool) {
		findings = append(findings, AdmissionFinding{ID: id, Name: name, Reason: reason, Fix: fix, Optional: optional})
	}

	nativeHTTP := m.Transport == config.TransportNativeHTTP
	for _, pp := range pools {
		p := pp.pool
		if p == nil || p.End < p.Start {
			continue
		}
		if nativeHTTPPoolOverflows(p, nativeHTTP) {
			add("port-pool-native-overflow", portPoolName(p), nativeHTTPPoolOverflowReason(p), nativeHTTPPoolOverflowFix(), false)
		}
	}

	var registryTaken map[int]bool
	if regPath, err := DefaultRegistryPath(); err != nil {
		for _, pp := range pools {
			p := pp.pool
			if p == nil || p.End < p.Start {
				continue
			}
			add("port-pool-registry", portPoolName(p), "the workspace registry could not be read or resolved (register reads it before allocating a pool port)", "Fix or remove the corrupt workspaces.yaml registry (register reads it before allocating a pool port).", false)
		}
		return findings
	} else {
		reg := NewRegistry(regPath)
		if err := reg.Load(); err != nil {
			for _, pp := range pools {
				p := pp.pool
				if p == nil || p.End < p.Start {
					continue
				}
				add("port-pool-registry", portPoolName(p), "the workspace registry could not be read or resolved (register reads it before allocating a pool port)", "Fix or remove the corrupt workspaces.yaml registry (register reads it before allocating a pool port).", false)
			}
			return findings
		}
		registryTaken = reg.AllocatedPorts()
	}

	portTaken := func(port int) bool { return !portAvailable(port) || registryTaken[port] }
	for _, pp := range pools {
		p := pp.pool
		if p == nil || p.End < p.Start {
			continue
		}
		if nativeHTTPPoolOverflows(p, nativeHTTP) {
			continue
		}
		name := portPoolName(p)
		free := false
		for port := p.Start; port <= p.End; port++ {
			if portTaken(port) {
				continue
			}
			if nativeHTTP && portTaken(port+config.NativeHTTPInternalPortOffset) {
				continue
			}
			free = true
			break
		}
		if !free {
			optional := pp.daemonTemplate
			reason := "no port in the workspace pool is free (OS-bound or registry-allocated)"
			if optional {
				// ADVISORY only for daemon-template dynamic-pool installs/reinstalls:
				// those allocate no new pool port; workspace registration does.
				reason = "no port in the workspace pool is free for a NEW workspace (OS-bound or registry-allocated); existing workspaces and reinstall are unaffected"
			}
			add("port-pool-free", name, reason, "Free a pool port (or its native-http +offset upstream), or widen the pool in the manifest, before registering a new workspace.", optional)
		}
	}
	return findings
}
