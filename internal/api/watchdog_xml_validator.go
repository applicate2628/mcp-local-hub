// Package api — Task 6 hardened owned-task XML validator + structural
// ownership (watchdog plan v13 §5, §5b, §47).
//
// This file owns ValidateOwnedTaskXML and the snapshot-bound and plain
// OwnedXMLValidator constructors. The validator is the watchdog's last
// security gate before issuing a restart on a foreign / suspicious /
// adversarially-registered Task Scheduler entry.
//
// Threat model (plan §5):
//
//   - Adversary registers \mcp-local-hub-fake-default as a real Task
//     Scheduler entry pointing at attacker.exe. Task Scheduler accepts
//     the registration; Status() reports the row. The validator MUST
//     refuse: the (fake, default) pair is not in the manifest snapshot,
//     so manifestHasServerDaemon → false → ErrUnstructuredOwnership.
//
//   - Adversary tampers with an existing task XML to swap Command to
//     attacker.exe or change Arguments. The validator MUST refuse:
//     filepath.SameFile(canonicalMcphubPath, parsedCommand) is the
//     command gate.
//
//   - Adversary feeds the validator a malicious XML payload (DOCTYPE
//     entity bomb, deeply-nested elements, multi-megabyte buffer). The
//     validator MUST refuse: 64KB+1 size cap (LimitReader-style), 32-
//     element depth cap, byte-level DOCTYPE rejection, strict mode
//     decoder with Entity=nil + CharsetReader=nil, 2s context deadline
//     on schtasks /Query.
//
// Two constructors per plan §32:
//
//   - NewOwnedXMLValidator() OwnedXMLValidator: reads manifest fresh.
//     For non-watchdog callers (tests, future tools).
//
//   - NewOwnedXMLValidatorFromSnapshot(snap OwnershipSnapshot) OwnedXMLValidator:
//     wraps a frozen OwnershipSnapshot. The watchdog driver uses this
//     so structural ownership checks within one tick are tick-stable
//     (defeats the mid-tick rotation race per §47).
//
// Structural ownership (plan §5 v7-8):
//
//   - \mcp-local-hub-{server}-{daemon} (global): {server} MUST appear in
//     the manifest set AND the manifest's Daemons slice MUST contain a
//     DaemonSpec.Name == {daemon} entry. Args MUST include "daemon
//     --server {server} --daemon {daemon}".
//   - \mcp-local-hub-lsp-{wskey}-{lang} (lazy proxy): {wskey}-{lang}
//     MUST resolve in the workspace registry; the entry's TaskName
//     MUST equal the validated task name byte-for-byte. Args MUST
//     include "daemon workspace-proxy ..." shape.
//   - \mcp-local-hub-watchdog: maintenance — args "watchdog --once".
//   - \mcp-local-hub-workspace-weekly-refresh: maintenance — args
//     "daemon workspace-weekly-refresh" shape.
//   - Anything else with \mcp-local-hub-* prefix → ErrUnstructuredOwnership.
//
// Test seams: schtasksQueryXMLFn, canonicalMcphubPathFn, currentWindowsUserFn.
// Production wires defaults via package-level init; tests override.
//
// Platform note: the production schtasksQueryXMLFn default uses
// os/exec.CommandContext to invoke the real schtasks.exe. On non-Windows
// hosts schtasks doesn't exist; the exec returns ErrNotExist and the
// caller surfaces ErrSchtasksUnavailable. The pure-Go pieces (XML
// parsing, ownership classification, manifest lookup) are platform-
// independent and tested on all platforms via injected seams.

package api

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Constants (plan §5).
// ---------------------------------------------------------------------------

const (
	// xmlSizeLimit caps the schtasks /Query /XML payload size at 64 KiB.
	// Real Task Scheduler XML for a daemon is ~3 KiB; this is generous
	// headroom against malicious oversize attacks. The reader allocates
	// xmlSizeLimit+1 bytes so an exact-size payload is accepted while
	// any byte beyond the cap trips ErrXMLOversize.
	xmlSizeLimit = 64 * 1024
	// xmlDepthLimit caps element nesting depth. Task Scheduler XML
	// nests at most ~6 deep; 32 is generous against deep-nesting attacks
	// (billion-laughs without DOCTYPE, deeply-nested element bombs).
	xmlDepthLimit = 32
	// schtasksTimeout caps the time spent inside schtasks /Query /XML.
	// Production value; tests can override via schtasksTimeoutForTest.
	schtasksTimeout = 2 * time.Second
)

// schtasksTimeoutForTest, when non-zero, overrides schtasksTimeout for
// the duration of a unit test. Lets tests prove ctx-deadline behaviour
// without baking real seconds into the test runtime.
var schtasksTimeoutForTest time.Duration

// ---------------------------------------------------------------------------
// Error sentinels (plan §5 + §47).
// ---------------------------------------------------------------------------

var (
	// ErrNotOwnedTask is returned when classifyOwnedTaskName receives a
	// name that is not under the \mcp-local-hub- prefix.
	ErrNotOwnedTask = errors.New("api: task name not owned by mcp-local-hub")
	// ErrUnstructuredOwnership is returned when a name passes the
	// classifier but the structural lookup (manifest / registry) does
	// not place the name in the legitimate ownership universe — the
	// adversary scenario from plan §5 v7-8.
	ErrUnstructuredOwnership = errors.New("api: task name has mcp-local-hub prefix but is not structurally owned (manifest/registry mismatch)")
	// ErrXMLOversize is returned when the schtasks payload exceeds
	// xmlSizeLimit bytes.
	ErrXMLOversize = errors.New("api: task XML exceeds 64KB size limit")
	// ErrXMLDoctypeRejected is returned when the schtasks payload
	// contains a <!DOCTYPE declaration. Defends against entity
	// expansion (billion-laughs) attacks.
	ErrXMLDoctypeRejected = errors.New("api: task XML contains DOCTYPE (rejected)")
	// ErrXMLTooDeep is returned when the parser observes element
	// nesting beyond xmlDepthLimit. Defends against deep-nesting
	// element bombs.
	ErrXMLTooDeep = errors.New("api: task XML exceeds nesting depth limit (32)")
	// ErrXMLMalformed is returned when the encoding/xml decoder
	// rejects the payload. Generic catch-all for parser-level errors
	// (truncated payload, garbage tail, invalid encoding).
	ErrXMLMalformed = errors.New("api: task XML is malformed")
	// ErrSchtasksTimeout is returned when schtasks /Query /XML exceeds
	// schtasksTimeout.
	ErrSchtasksTimeout = errors.New("api: schtasks /Query /XML exceeded 2s deadline")
	// ErrSchtasksUnavailable is returned when schtasks.exe cannot be
	// invoked (non-Windows host, missing PATH entry, permission denied).
	ErrSchtasksUnavailable = errors.New("api: schtasks.exe not available on this host")
	// ErrCommandMismatch is returned when the parsed Command field does
	// not point at canonicalMcphubPath().
	ErrCommandMismatch = errors.New("api: task <Command> does not match canonicalMcphubPath")
	// ErrPrincipalMismatch is returned when the parsed UserId field is
	// not the current Windows user.
	ErrPrincipalMismatch = errors.New("api: task <UserId> does not match current Windows user")
	// ErrUnexpectedRunLevel is returned when the parsed RunLevel is not
	// the canonical install value (LeastPrivilege).
	ErrUnexpectedRunLevel = errors.New("api: task <RunLevel> not canonical")
	// ErrUnexpectedLogonType is returned when the parsed LogonType is
	// not the canonical install value (InteractiveToken).
	ErrUnexpectedLogonType = errors.New("api: task <LogonType> not canonical")
	// ErrArgsMismatch is returned when the parsed Arguments do not
	// match the structural shape declared by the task name (global vs
	// lazy-proxy vs maintenance).
	ErrArgsMismatch = errors.New("api: task <Arguments> do not match structural ownership shape")
)

// ---------------------------------------------------------------------------
// Test seams (package-level fn vars).
// ---------------------------------------------------------------------------

// schtasksQueryXMLFn is the platform-specific schtasks /Query /XML
// invoker. Production: bound to defaultSchtasksQueryXML (uses os/exec).
// Tests: inject deterministic payloads.
var schtasksQueryXMLFn func(ctx context.Context, taskName string) ([]byte, error) = defaultSchtasksQueryXML

// canonicalMcphubPathFn is the canonical-mcphub-path resolver used by
// the Command field assertion. Production: thin adapter over the
// existing internal canonicalMcphubPath() in install.go. Tests: inject
// a stub path that resolves to a real on-disk file (so
// filepath.SameFile can succeed).
var canonicalMcphubPathFn func() (string, error) = func() (string, error) { return canonicalMcphubPath() }

// currentWindowsUserFn is the current-user resolver used by the
// principal field assertion. Production: defaultCurrentWindowsUser()
// (strips DOMAIN\\ prefix from user.Current().Username). Tests: inject
// a deterministic name.
var currentWindowsUserFn func() (string, error) = defaultCurrentWindowsUser

// defaultCurrentWindowsUser returns the bare username of the running
// process, stripping the DOMAIN\\ prefix Windows attaches to
// user.Current().Username.
func defaultCurrentWindowsUser() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("user.Current: %w", err)
	}
	name := u.Username
	if i := strings.LastIndex(name, "\\"); i >= 0 {
		name = name[i+1:]
	}
	return name, nil
}

// defaultSchtasksQueryXML invokes schtasks /Query /XML /TN <name> via
// os/exec with the supplied context. On non-Windows hosts schtasks
// is unavailable and the function returns ErrSchtasksUnavailable.
//
// Caller wraps in a context.WithTimeout(schtasksTimeout) so a hung
// schtasks process is killed at the deadline; the goroutine running
// CombinedOutput observes ctx.Done via the exec.CommandContext kill
// path.
func defaultSchtasksQueryXML(ctx context.Context, taskName string) ([]byte, error) {
	if runtime.GOOS != "windows" {
		return nil, ErrSchtasksUnavailable
	}
	cmd := exec.CommandContext(ctx, "schtasks.exe", "/Query", "/TN", taskName, "/XML")
	out, err := cmd.CombinedOutput()
	if err != nil {
		// If ctx fired, surface the deadline error rather than the
		// schtasks killed-by-signal noise.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("schtasks /Query /XML: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Ownership classification (plan §5).
// ---------------------------------------------------------------------------

// Ownership kinds — string constants kept package-private (the public
// surface is the kind comparison done inside ValidateOwnedTaskXML).
const (
	ownershipGlobal      = "global"
	ownershipLazyProxy   = "lazy-proxy"
	ownershipMaintenance = "maintenance"
)

// taskOwnership is the structured form returned by classifyOwnedTaskName.
// Different fields populate based on Kind:
//
//   - global: Server, Daemon
//   - lazy-proxy: WorkspaceKey, Language
//   - maintenance: MaintenanceKind ("watchdog" | "workspace-weekly-refresh")
type taskOwnership struct {
	Kind            string
	Server          string
	Daemon          string
	WorkspaceKey    string
	Language        string
	MaintenanceKind string
}

// classifyOwnedTaskName parses a Task Scheduler task name into its
// ownership classification. Returns ErrNotOwnedTask if the name is
// not under the \mcp-local-hub- prefix.
//
// Ordering matters: maintenance suffixes (watchdog, workspace-weekly-
// refresh) are checked first, then the lazy-proxy lsp- prefix, then
// the global pattern. This avoids classifying \mcp-local-hub-watchdog
// as a global (server="watchdog", daemon="") through the trailing-
// segment split.
func classifyOwnedTaskName(name string) (taskOwnership, error) {
	const prefix = "\\mcp-local-hub-"
	if !strings.HasPrefix(name, prefix) {
		return taskOwnership{}, ErrNotOwnedTask
	}
	rest := strings.TrimPrefix(name, prefix)
	if rest == "" {
		return taskOwnership{}, ErrNotOwnedTask
	}

	// Maintenance: \mcp-local-hub-watchdog (exact match).
	if rest == "watchdog" {
		return taskOwnership{Kind: ownershipMaintenance, MaintenanceKind: "watchdog"}, nil
	}
	// Maintenance: \mcp-local-hub-workspace-weekly-refresh (exact match
	// — this is the hub-wide maintenance task; per-server weekly-refresh
	// daemons fall through to the global path with daemon="weekly-refresh").
	if rest == "workspace-weekly-refresh" {
		return taskOwnership{Kind: ownershipMaintenance, MaintenanceKind: "workspace-weekly-refresh"}, nil
	}

	// Lazy proxy: \mcp-local-hub-lsp-<wskey>-<lang>. Split on the LAST
	// hyphen to separate language from a possibly-hyphenated wskey
	// (workspace keys are short hashes today, but be defensive).
	if strings.HasPrefix(rest, "lsp-") {
		body := strings.TrimPrefix(rest, "lsp-")
		idx := strings.LastIndex(body, "-")
		if idx <= 0 || idx == len(body)-1 {
			return taskOwnership{}, ErrNotOwnedTask
		}
		return taskOwnership{
			Kind:         ownershipLazyProxy,
			WorkspaceKey: body[:idx],
			Language:     body[idx+1:],
		}, nil
	}

	// Global: \mcp-local-hub-<server>-<daemon>. The last segment is the
	// daemon; the rest is the server (which may contain '-'). Mirrors
	// status_enrich.go::parseTaskName so a name installed by mcphub
	// roundtrips through both paths identically.
	idx := strings.LastIndex(rest, "-")
	if idx <= 0 || idx == len(rest)-1 {
		return taskOwnership{}, ErrNotOwnedTask
	}
	return taskOwnership{
		Kind:   ownershipGlobal,
		Server: rest[:idx],
		Daemon: rest[idx+1:],
	}, nil
}

// ---------------------------------------------------------------------------
// XML extraction (plan §5).
// ---------------------------------------------------------------------------

// xmlTask is the minimal subset of Task Scheduler XML we need to
// validate. Strict + Entity=nil + CharsetReader=nil keep the decoder
// from following entities or charset hooks; only the structural fields
// below are extracted.
type xmlTask struct {
	XMLName    xml.Name      `xml:"Task"`
	Principals xmlPrincipals `xml:"Principals"`
	Actions    xmlActions    `xml:"Actions"`
}

type xmlPrincipals struct {
	Principal []xmlPrincipal `xml:"Principal"`
}

type xmlPrincipal struct {
	UserId    string `xml:"UserId"`
	LogonType string `xml:"LogonType"`
	RunLevel  string `xml:"RunLevel"`
}

type xmlActions struct {
	Exec []xmlExec `xml:"Exec"`
}

type xmlExec struct {
	Command   string `xml:"Command"`
	Arguments string `xml:"Arguments"`
}

// extractedFields is the parsed-and-validated form of a Task Scheduler XML.
type extractedFields struct {
	Command   string
	Arguments string
	UserId    string
	RunLevel  string
	LogonType string
}

// decodeAndExtract parses the XML payload with the hardened decoder
// settings and extracts the fields the validator asserts against.
// Returns ErrXMLMalformed for any structural error and ErrXMLTooDeep
// when the depth cap trips.
//
// schtasks /Query /XML emits a `<?xml version="1.0" encoding="UTF-16"?>`
// declaration even though the redirected stdout bytes are ASCII (Windows
// cmd quirk: the declaration mirrors the in-memory wide-char form, but
// the on-the-wire bytes are not UTF-16 wide chars). Setting
// CharsetReader=nil rejects the "UTF-16" name as unknown. To hardened-
// parse the real ASCII bytes we strip the XML declaration before
// decoding — the decoder falls back to UTF-8, which is a strict
// superset for the ASCII subset schtasks emits.
func decodeAndExtract(raw []byte) (extractedFields, error) {
	raw = stripXMLDeclaration(raw)
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = true
	dec.Entity = nil
	dec.CharsetReader = nil

	// Depth-limited token walk. We could use dec.Decode(&xmlTask{}) for
	// extraction, but Decode does not enforce a depth cap. Instead
	// stream tokens to enforce depth, then decode the captured
	// elements opportunistically.
	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return extractedFields{}, ErrXMLMalformed
		}
		switch tok.(type) {
		case xml.StartElement:
			depth++
			if depth > xmlDepthLimit {
				return extractedFields{}, ErrXMLTooDeep
			}
		case xml.EndElement:
			depth--
		}
	}

	// Re-decode for extraction. Using a fresh decoder + Unmarshal-via-
	// Decode keeps the depth check above as the gating step; this
	// second pass is purely structural. The same decoder hardening
	// (Strict / Entity nil / CharsetReader nil) applies.
	var t xmlTask
	dec2 := xml.NewDecoder(bytes.NewReader(raw))
	dec2.Strict = true
	dec2.Entity = nil
	dec2.CharsetReader = nil
	if err := dec2.Decode(&t); err != nil && err != io.EOF {
		return extractedFields{}, ErrXMLMalformed
	}

	out := extractedFields{}
	if len(t.Principals.Principal) > 0 {
		out.UserId = t.Principals.Principal[0].UserId
		out.RunLevel = t.Principals.Principal[0].RunLevel
		out.LogonType = t.Principals.Principal[0].LogonType
	}
	if len(t.Actions.Exec) > 0 {
		out.Command = t.Actions.Exec[0].Command
		out.Arguments = t.Actions.Exec[0].Arguments
	}
	return out, nil
}

// stripXMLDeclaration drops a leading `<?xml ... ?>` processing
// instruction (and any leading BOM / whitespace) so the decoder runs
// in default UTF-8 mode. The body of the document is left untouched.
//
// schtasks-on-Windows emits `<?xml version="1.0" encoding="UTF-16"?>`
// even though the redirected stdout bytes are ASCII; the validator's
// Decode-with-CharsetReader-nil hardening rejects the unknown
// "UTF-16" charset name. Stripping the declaration is safe because
// (a) the body is pure 7-bit ASCII for legitimate Task Scheduler
// payloads (Command, Args, names) and (b) the byte-level DOCTYPE
// scan still runs against the original raw bytes upstream of this
// helper.
func stripXMLDeclaration(raw []byte) []byte {
	// Skip a possible UTF-8 BOM.
	if len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF {
		raw = raw[3:]
	}
	// Skip leading whitespace (declarations sometimes start at offset
	// >0 if the producer emits leading newlines; defensive only).
	i := 0
	for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\r' || raw[i] == '\n') {
		i++
	}
	if i+5 > len(raw) || string(raw[i:i+5]) != "<?xml" {
		return raw
	}
	// Find the matching ?>.
	end := bytes.Index(raw[i:], []byte("?>"))
	if end < 0 {
		return raw
	}
	return raw[i+end+2:]
}

// ---------------------------------------------------------------------------
// Args structural verification (plan §5 v7-8).
// ---------------------------------------------------------------------------

// globalArgsMatch returns true if args is the canonical global-daemon
// invocation shape "daemon --server <server> --daemon <daemon> ...".
// Order of --server / --daemon is fixed because the installer (Task
// Scheduler XML producer) emits them in a deterministic order. Extra
// trailing flags (--port, etc.) are permitted.
func globalArgsMatch(args, server, daemon string) bool {
	tokens := strings.Fields(args)
	if len(tokens) == 0 || tokens[0] != "daemon" {
		return false
	}
	// Walk for --server <X> --daemon <Y> in any order, but both must be
	// present and exact-match.
	gotServer, gotDaemon := "", ""
	for i := 1; i < len(tokens)-1; i++ {
		switch tokens[i] {
		case "--server":
			gotServer = tokens[i+1]
		case "--daemon":
			gotDaemon = tokens[i+1]
		}
	}
	return gotServer == server && gotDaemon == daemon
}

// lazyProxyArgsMatch returns true if args is the canonical lazy-proxy
// invocation shape "daemon workspace-proxy ...".
func lazyProxyArgsMatch(args string) bool {
	tokens := strings.Fields(args)
	if len(tokens) < 2 {
		return false
	}
	return tokens[0] == "daemon" && tokens[1] == "workspace-proxy"
}

// maintenanceArgsMatch returns true if args is the canonical maintenance
// invocation for the given kind. Two recognised kinds:
//
//   - "watchdog" → "watchdog --once" (or any args starting with "watchdog").
//   - "workspace-weekly-refresh" → "daemon workspace-weekly-refresh ..."
func maintenanceArgsMatch(args, kind string) bool {
	tokens := strings.Fields(args)
	switch kind {
	case "watchdog":
		return len(tokens) >= 1 && tokens[0] == "watchdog"
	case "workspace-weekly-refresh":
		if len(tokens) < 2 {
			return false
		}
		return tokens[0] == "daemon" && tokens[1] == "workspace-weekly-refresh"
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// OwnedXMLValidator implementation.
// ---------------------------------------------------------------------------

// ownedXMLValidator is the snapshot-bound and plain validator
// implementation. The snap field is consulted for structural
// (manifest/registry) lookups; for the plain constructor a snapshot
// is built fresh on each IsOwnedAndValid call.
//
// This struct also satisfies the OwnedXMLValidator interface defined
// in api_surfaces.go and replaces the Task 0 stub via the shared
// constructor NewOwnedXMLValidatorFromSnapshot (declared below).
type ownedXMLValidator struct {
	snap OwnershipSnapshot
	// freshSnap is non-nil for the plain constructor; when set the
	// validator rebuilds the snapshot per-call instead of using the
	// captured snap. Watchdog callers (snapshot constructor) leave it nil.
	freshSnap func() OwnershipSnapshot
}

// IsOwnedAndValid is the OwnedXMLValidator interface method. Returns
// true iff ValidateOwnedTaskXML succeeds.
func (v *ownedXMLValidator) IsOwnedAndValid(taskName string) bool {
	return v.validate(taskName) == nil
}

// IsOwnedAndValidErr exposes the underlying validation error for tests
// and audit-log callers that need the precise reason for rejection.
// Not part of the OwnedXMLValidator interface.
func (v *ownedXMLValidator) IsOwnedAndValidErr(taskName string) error {
	return v.validate(taskName)
}

// validate performs the full hardened check chain per plan §5:
//
//  1. Classify the task name (ErrNotOwnedTask if not mcp-local-hub-).
//  2. Run schtasks /Query /XML with a 2s ctx deadline.
//  3. Enforce the 64KB+1 size cap.
//  4. Reject DOCTYPE.
//  5. Hard-decode with depth cap (32).
//  6. Extract and assert (Command, Args, UserId, RunLevel, LogonType).
//  7. Run kind-specific structural ownership check against the snapshot.
func (v *ownedXMLValidator) validate(taskName string) error {
	ownership, err := classifyOwnedTaskName(taskName)
	if err != nil {
		return err
	}

	timeout := schtasksTimeout
	if schtasksTimeoutForTest > 0 {
		timeout = schtasksTimeoutForTest
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	raw, err := schtasksQueryXMLFn(ctx, taskName)
	if err != nil {
		// ctx timeout takes precedence over a generic exec error.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ErrSchtasksTimeout
		}
		return fmt.Errorf("schtasks query: %w", err)
	}

	// Size cap: read up to xmlSizeLimit+1 bytes. If the source
	// produced more, reject before any parsing work.
	buf := make([]byte, xmlSizeLimit+1)
	n, err := io.ReadFull(io.LimitReader(bytes.NewReader(raw), int64(xmlSizeLimit)+1), buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && err != io.EOF {
		return ErrXMLMalformed
	}
	if n > xmlSizeLimit {
		return ErrXMLOversize
	}
	raw = buf[:n]

	// DOCTYPE byte-level rejection. Catches entity-expansion attacks
	// before the parser sees the input. Case-insensitive — XML allows
	// <!DOCTYPE in any letter case.
	if bytes.Contains(bytes.ToLower(raw), []byte("<!doctype")) {
		return ErrXMLDoctypeRejected
	}

	fields, err := decodeAndExtract(raw)
	if err != nil {
		return err
	}

	// Command field gate.
	canonical, err := canonicalMcphubPathFn()
	if err != nil {
		return fmt.Errorf("resolve canonical mcphub path: %w", err)
	}
	if !commandPathsEqual(fields.Command, canonical) {
		return ErrCommandMismatch
	}

	// Principal gate.
	currentUser, err := currentWindowsUserFn()
	if err != nil {
		return fmt.Errorf("resolve current windows user: %w", err)
	}
	if !sameWindowsUser(fields.UserId, currentUser) {
		return ErrPrincipalMismatch
	}

	// RunLevel / LogonType gates. Compare against the canonical XML
	// schema enum strings written by buildCreateXML in
	// internal/scheduler/scheduler_windows.go. These are also the
	// well-known Task Scheduler schema values for the user-facing
	// "Limited" / "Interactive" UI labels per the TaskScheduler XSD.
	if fields.RunLevel != "LeastPrivilege" {
		return ErrUnexpectedRunLevel
	}
	if fields.LogonType != "InteractiveToken" {
		return ErrUnexpectedLogonType
	}

	// Structural arg verification per ownership kind.
	snap := v.snapshot()
	switch ownership.Kind {
	case ownershipGlobal:
		if !globalArgsMatch(fields.Arguments, ownership.Server, ownership.Daemon) {
			return ErrArgsMismatch
		}
		if !manifestHasServerDaemon(snap, ownership.Server, ownership.Daemon) {
			return ErrUnstructuredOwnership
		}
	case ownershipLazyProxy:
		if !lazyProxyArgsMatch(fields.Arguments) {
			return ErrArgsMismatch
		}
		if !workspaceRegistryHas(snap, ownership.WorkspaceKey, ownership.Language, taskName) {
			return ErrUnstructuredOwnership
		}
	case ownershipMaintenance:
		if !maintenanceArgsMatch(fields.Arguments, ownership.MaintenanceKind) {
			return ErrArgsMismatch
		}
		// No structural snapshot check for maintenance: there is one
		// canonical task per maintenance kind, validated via name +
		// args alone. The recovery filter (§21) skips them anyway.
	}

	return nil
}

// snapshot returns the validator's effective snapshot. Snapshot-bound
// validators return v.snap; plain validators rebuild via the freshSnap
// factory captured at construction time.
func (v *ownedXMLValidator) snapshot() OwnershipSnapshot {
	if v.freshSnap != nil {
		return v.freshSnap()
	}
	return v.snap
}

// commandPathsEqual compares two file paths for "same on-disk file"
// semantics. os.SameFile is the gold standard but requires both paths
// to exist on disk; if either Stat fails we fall back to a case-
// insensitive string compare on the cleaned absolute forms (Windows
// paths are case-insensitive).
func commandPathsEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	// Defensive cleanup: trim quotes some Task Scheduler clients add.
	a = strings.Trim(a, "\"")
	b = strings.Trim(b, "\"")
	// Try os.SameFile first — handles symlinks + hardlinks + 8.3
	// shortname collisions.
	if infoA, errA := os.Stat(a); errA == nil {
		if infoB, errB := os.Stat(b); errB == nil {
			return os.SameFile(infoA, infoB)
		}
	}
	// Fallback: case-insensitive cleaned-path equality. Either file
	// is missing or unreadable; we still want a deterministic answer
	// for forensic / log-only callers.
	ca, _ := filepath.Abs(a)
	cb, _ := filepath.Abs(b)
	return strings.EqualFold(filepath.Clean(ca), filepath.Clean(cb))
}

// sameWindowsUser compares two Windows usernames for equality, ignoring
// optional DOMAIN\\ prefix and case.
func sameWindowsUser(a, b string) bool {
	stripDomain := func(s string) string {
		if i := strings.LastIndex(s, "\\"); i >= 0 {
			return s[i+1:]
		}
		return s
	}
	return strings.EqualFold(stripDomain(a), stripDomain(b))
}

// ---------------------------------------------------------------------------
// Snapshot lookup helpers (plan §5 v7-8).
// ---------------------------------------------------------------------------

// manifestHasServerDaemon answers "does the snapshot's
// ManifestDaemons place (server, daemon) in the legitimate set?"
// Implementation deriving a map[string]bool per the plan note: the
// per-server map already is map[string]bool, so we can do a direct
// O(1) lookup without rebuilding.
func manifestHasServerDaemon(snap OwnershipSnapshot, server, daemon string) bool {
	if !snap.ManifestServers[server] {
		return false
	}
	daemons, ok := snap.ManifestDaemons[server]
	if !ok {
		return false
	}
	return daemons[daemon]
}

// workspaceRegistryHas answers "is (wskey, lang) registered AND does
// the registered TaskName match the validated task name byte-for-byte?"
// per plan §5 v7-8.
func workspaceRegistryHas(snap OwnershipSnapshot, wskey, lang, taskName string) bool {
	registered, ok := snap.WorkspaceTasksByKey[wskey+"-"+lang]
	if !ok {
		return false
	}
	return registered == taskName
}

// ---------------------------------------------------------------------------
// Constructors.
// ---------------------------------------------------------------------------

// NewOwnedXMLValidator returns an OwnedXMLValidator that reads the
// manifest fresh on each IsOwnedAndValid call. Per plan §32: used by
// non-watchdog callers (tests, future tools). The watchdog driver
// MUST use NewOwnedXMLValidatorFromSnapshot instead so structural
// checks within one tick are tick-stable.
func NewOwnedXMLValidator() OwnedXMLValidator {
	a := &API{}
	return &ownedXMLValidator{
		freshSnap: func() OwnershipSnapshot {
			return a.LoadOwnershipSnapshot()
		},
	}
}

// (NewOwnedXMLValidatorFromSnapshot is declared in api_surfaces.go and
// returns a new ownedXMLValidator wrapping the supplied snapshot. Task
// 6 leaves that constructor in place — its body is updated in
// api_surfaces.go to construct the Task 6 implementation rather than
// the Task 0 ownership-only stub.)

// ---------------------------------------------------------------------------
// ValidateOwnedTaskXML (public function form per plan §5).
// ---------------------------------------------------------------------------

// ValidateOwnedTaskXML is the standalone function form of the validator.
// Used by callers that prefer a one-shot call over an interface
// implementation. Internally constructs a plain OwnedXMLValidator
// (manifest read fresh) and delegates.
func ValidateOwnedTaskXML(taskName string) error {
	a := &API{}
	v := &ownedXMLValidator{
		freshSnap: func() OwnershipSnapshot { return a.LoadOwnershipSnapshot() },
	}
	return v.validate(taskName)
}

// Compile-time assertion: ownedXMLValidator satisfies the
// OwnedXMLValidator interface declared in api_surfaces.go. Refactors
// that drop or rename IsOwnedAndValid fail to compile rather than
// silently break the watchdog driver's last-gate guarantee.
var _ OwnedXMLValidator = (*ownedXMLValidator)(nil)
