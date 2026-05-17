package migration

import (
	"encoding/xml"
	"strings"

	"mcp-local-hub/internal/scheduler"
)

// DeviationKind enumerates the six classification buckets defined in
// docs/superpowers/specs/2026-05-16-v0.5.0-supervisor-architecture.md
// §"XML deviation-only classification" (lines 236–248). Default-match
// (bucket 1) does NOT produce a Deviation entry — silent.
type DeviationKind int

const (
	// KindKnownPreserveIntent — bucket 2. Known deviation preserved through
	// supervisor-intent + autostart shim. Example: RunOnlyIfNetworkAvailable=true.
	KindKnownPreserveIntent DeviationKind = iota
	// KindKnownPreserveShim — bucket 3. Known deviation preserved through
	// autostart shim only (no intent-file mapping). Example: CalendarTrigger
	// in place of LogonTrigger.
	KindKnownPreserveShim
	// KindUnsupportedAbort — bucket 4. Known deviation that aborts migration
	// unless --discard-scheduler-customizations is set. Examples: wrong
	// Principal.UserId, wrong LogonType, custom <Actions>, StopOnIdleEnd=true,
	// WakeToRun=true, RunOnlyIfIdle=true.
	KindUnsupportedAbort
	// KindKnownDrop — bucket 5. Known deviation silently dropped with a
	// warning. Example: Priority != 7.
	KindKnownDrop
	// KindUnknownDrift — bucket 6. Unknown XML element OR unknown
	// default-value drift (field present but value not in the pinned
	// defaults table). --migration-strict-template flips this to abort
	// in the migration driver (NOT this package's responsibility).
	KindUnknownDrift
)

// Deviation describes ONE XML field that does not match the pinned baseline.
type Deviation struct {
	XPath string        // e.g. "Settings.MultipleInstancesPolicy"
	Got   string        // observed value
	Want  string        // pinned default (empty for KindUnknownDrift unknown elements)
	Kind  DeviationKind
}

// DeviationReport is the classifier output.
type DeviationReport struct {
	Deviations          []Deviation
	HasUnsupportedAbort bool // convenience flag — true iff any Deviation.Kind == KindUnsupportedAbort
}

// taskXML mirrors the Task Scheduler XML schema fields the classifier
// inspects. Unknown fields are captured via InnerXML on the parents so the
// unknown-element bucket can detect them by serializing and re-scanning.
type taskXML struct {
	XMLName          xml.Name           `xml:"Task"`
	RegistrationInfo *regInfoXML        `xml:"RegistrationInfo"`
	Triggers         *triggersXML       `xml:"Triggers"`
	Principals       *principalsXML     `xml:"Principals"`
	Settings         *settingsXML       `xml:"Settings"`
	Actions          *actionsXML        `xml:"Actions"`
}

type regInfoXML struct {
	XMLName xml.Name `xml:"RegistrationInfo"`
	// We intentionally do NOT inspect Date/Author/Description — they drift
	// naturally and are not part of the deviation baseline.
}

type triggersXML struct {
	XMLName          xml.Name              `xml:"Triggers"`
	LogonTrigger     *logonTriggerXML      `xml:"LogonTrigger"`
	CalendarTrigger  *calendarTriggerXML   `xml:"CalendarTrigger"`
	BootTrigger      *anyTriggerXML        `xml:"BootTrigger"`
	TimeTrigger      *anyTriggerXML        `xml:"TimeTrigger"`
	EventTrigger     *anyTriggerXML        `xml:"EventTrigger"`
	SessionStateTrigger *anyTriggerXML     `xml:"SessionStateTrigger"`
	RegistrationTrigger *anyTriggerXML     `xml:"RegistrationTrigger"`
	IdleTrigger      *anyTriggerXML        `xml:"IdleTrigger"`
	InnerXML         string                `xml:",innerxml"`
}

type logonTriggerXML struct {
	XMLName  xml.Name `xml:"LogonTrigger"`
	UserId   string   `xml:"UserId"`
	Enabled  string   `xml:"Enabled"`
	InnerXML string   `xml:",innerxml"`
}

type calendarTriggerXML struct {
	XMLName  xml.Name `xml:"CalendarTrigger"`
	Enabled  string   `xml:"Enabled"`
	InnerXML string   `xml:",innerxml"`
}

type anyTriggerXML struct {
	InnerXML string `xml:",innerxml"`
}

type principalsXML struct {
	XMLName   xml.Name        `xml:"Principals"`
	Principal *principalXML   `xml:"Principal"`
	InnerXML  string          `xml:",innerxml"`
}

type principalXML struct {
	XMLName   xml.Name `xml:"Principal"`
	UserId    string   `xml:"UserId"`
	LogonType string   `xml:"LogonType"`
	RunLevel  string   `xml:"RunLevel"`
	InnerXML  string   `xml:",innerxml"`
}

type settingsXML struct {
	XMLName                    xml.Name             `xml:"Settings"`
	AllowHardTerminate         string               `xml:"AllowHardTerminate"`
	StartWhenAvailable         string               `xml:"StartWhenAvailable"`
	RunOnlyIfNetworkAvailable  string               `xml:"RunOnlyIfNetworkAvailable"`
	MultipleInstancesPolicy    string               `xml:"MultipleInstancesPolicy"`
	DisallowStartIfOnBatteries string               `xml:"DisallowStartIfOnBatteries"`
	StopIfGoingOnBatteries     string               `xml:"StopIfGoingOnBatteries"`
	IdleSettings               *idleSettingsXML     `xml:"IdleSettings"`
	AllowStartOnDemand         string               `xml:"AllowStartOnDemand"`
	Enabled                    string               `xml:"Enabled"`
	Hidden                     string               `xml:"Hidden"`
	RunOnlyIfIdle              string               `xml:"RunOnlyIfIdle"`
	WakeToRun                  string               `xml:"WakeToRun"`
	ExecutionTimeLimit         string               `xml:"ExecutionTimeLimit"`
	Priority                   string               `xml:"Priority"`
	RestartOnFailure           *restartOnFailureXML `xml:"RestartOnFailure"`
	InnerXML                   string               `xml:",innerxml"`
}

type idleSettingsXML struct {
	XMLName       xml.Name `xml:"IdleSettings"`
	StopOnIdleEnd string   `xml:"StopOnIdleEnd"`
	RestartOnIdle string   `xml:"RestartOnIdle"`
	InnerXML      string   `xml:",innerxml"`
}

type restartOnFailureXML struct {
	XMLName  xml.Name `xml:"RestartOnFailure"`
	Interval string   `xml:"Interval"`
	Count    string   `xml:"Count"`
	InnerXML string   `xml:",innerxml"`
}

type actionsXML struct {
	XMLName  xml.Name  `xml:"Actions"`
	Exec     *execXML  `xml:"Exec"`
	InnerXML string    `xml:",innerxml"`
}

type execXML struct {
	XMLName          xml.Name `xml:"Exec"`
	Command          string   `xml:"Command"`
	Arguments        string   `xml:"Arguments"`
	WorkingDirectory string   `xml:"WorkingDirectory"`
	InnerXML         string   `xml:",innerxml"`
}

// ClassifyXMLDeviations parses observed XML and classifies each field
// against the pinned v0.4.x baseline derived from spec + currentUser.
//
// Contract: this function NEVER returns an error and NEVER panics. Malformed
// XML surfaces as a single Deviation with XPath="(parse error)" and
// Kind=KindUnknownDrift — the migration driver decides what to do with
// that signal (typically: fall through to --discard-scheduler-customizations
// or abort).
func ClassifyXMLDeviations(observedXML string, spec scheduler.TaskSpec, currentUser string) DeviationReport {
	var report DeviationReport
	defer func() {
		// Belt-and-braces panic guard. The schema below is deliberately
		// permissive (InnerXML catch-alls + pointer subtrees), but a future
		// edit could regress. A panic here would abort migration mid-flight;
		// surfacing it as KindUnknownDrift keeps the loop alive.
		if r := recover(); r != nil {
			report.Deviations = append(report.Deviations, Deviation{
				XPath: "(parse error)",
				Got:   "panic recovered during classification",
				Kind:  KindUnknownDrift,
			})
		}
	}()

	// Strip the leading `<?xml ... ?>` PI before parsing. v0.4.x XML
	// declares encoding="UTF-16" (the format Task Scheduler requires when
	// piped via /Create /XML), but classifier inputs are Go strings — text
	// that has already been read off disk and decoded into UTF-8. Without
	// stripping the declaration, encoding/xml fails with
	//   xml: encoding "UTF-16" declared but Decoder.CharsetReader is nil
	// because it tries to install a UTF-16 charset reader at the byte level.
	// The body is pure 7-bit ASCII (paths, identifiers, schema tags), so
	// stripping the PI and treating the remainder as UTF-8 is safe.
	// Mirrors internal/scheduler/enumerate_all_windows.go's
	// stripEnumerateXMLDeclaration — same rationale, same cheap helper.
	body := stripXMLDeclaration(observedXML)

	var doc taskXML
	dec := xml.NewDecoder(strings.NewReader(body))
	dec.Strict = true
	dec.Entity = nil
	dec.CharsetReader = nil
	if err := dec.Decode(&doc); err != nil {
		report.Deviations = append(report.Deviations, Deviation{
			XPath: "(parse error)",
			Got:   err.Error(),
			Kind:  KindUnknownDrift,
		})
		return report
	}

	// --- Principals.Principal ---------------------------------------------
	if doc.Principals != nil && doc.Principals.Principal != nil {
		p := doc.Principals.Principal

		// UserId is the user-specific anchor — must equal currentUser.
		if p.UserId != "" && p.UserId != currentUser {
			report.add(Deviation{
				XPath: "Principals.Principal.UserId",
				Got:   p.UserId,
				Want:  currentUser,
				Kind:  KindUnsupportedAbort,
			})
		}

		// LogonType must be InteractiveToken.
		if p.LogonType != "" {
			classifyAgainstPinned(&report,
				"Principals.Principal.LogonType",
				p.LogonType,
				KindUnsupportedAbort, // wrong value here aborts
			)
		}

		// RunLevel — drift here is not an explicit bucket-4 trigger per
		// spec; treat off-pinned values as unknown drift (warn+preserve).
		if p.RunLevel != "" {
			classifyAgainstPinned(&report,
				"Principals.Principal.RunLevel",
				p.RunLevel,
				KindUnknownDrift,
			)
		}

		// Catch unknown elements inside <Principal>.
		appendUnknownElements(&report, "Principals.Principal", p.InnerXML, []string{
			"UserId", "LogonType", "RunLevel",
		})
	}

	// --- Settings ---------------------------------------------------------
	if doc.Settings != nil {
		s := doc.Settings

		// Scalar settings whose drift is bucket-4 (abort).
		classifyAgainstPinned(&report, "Settings.RunOnlyIfIdle", s.RunOnlyIfIdle, KindUnsupportedAbort)
		classifyAgainstPinned(&report, "Settings.WakeToRun", s.WakeToRun, KindUnsupportedAbort)

		// Scalar settings whose drift is bucket-2 (intent + shim).
		// Concrete spec mapping: RunOnlyIfNetworkAvailable=true → supervisor
		// delays spawn until network probe passes.
		classifyAgainstPinned(&report, "Settings.RunOnlyIfNetworkAvailable", s.RunOnlyIfNetworkAvailable, KindKnownPreserveIntent)

		// Scalar settings whose drift is bucket-5 (drop with warning).
		classifyAgainstPinned(&report, "Settings.Priority", s.Priority, KindKnownDrop)

		// Everything else under <Settings> defaults to bucket-6 (unknown drift)
		// when the observed value is not in the pinned table.
		classifyAgainstPinned(&report, "Settings.AllowHardTerminate", s.AllowHardTerminate, KindUnknownDrift)
		classifyAgainstPinned(&report, "Settings.StartWhenAvailable", s.StartWhenAvailable, KindUnknownDrift)
		classifyAgainstPinned(&report, "Settings.MultipleInstancesPolicy", s.MultipleInstancesPolicy, KindUnknownDrift)
		classifyAgainstPinned(&report, "Settings.DisallowStartIfOnBatteries", s.DisallowStartIfOnBatteries, KindUnknownDrift)
		classifyAgainstPinned(&report, "Settings.StopIfGoingOnBatteries", s.StopIfGoingOnBatteries, KindUnknownDrift)
		classifyAgainstPinned(&report, "Settings.AllowStartOnDemand", s.AllowStartOnDemand, KindUnknownDrift)
		classifyAgainstPinned(&report, "Settings.Enabled", s.Enabled, KindUnknownDrift)
		classifyAgainstPinned(&report, "Settings.Hidden", s.Hidden, KindUnknownDrift)
		classifyAgainstPinned(&report, "Settings.ExecutionTimeLimit", s.ExecutionTimeLimit, KindUnknownDrift)

		// IdleSettings sub-tree.
		if s.IdleSettings != nil {
			// StopOnIdleEnd=true is bucket-4 (abort).
			classifyAgainstPinned(&report, "Settings.IdleSettings.StopOnIdleEnd", s.IdleSettings.StopOnIdleEnd, KindUnsupportedAbort)
			classifyAgainstPinned(&report, "Settings.IdleSettings.RestartOnIdle", s.IdleSettings.RestartOnIdle, KindUnknownDrift)
			appendUnknownElements(&report, "Settings.IdleSettings", s.IdleSettings.InnerXML, []string{
				"StopOnIdleEnd", "RestartOnIdle",
			})
		}

		// RestartOnFailure sub-tree (optional — only emitted by spec.RestartOnFailure).
		if s.RestartOnFailure != nil {
			classifyAgainstPinned(&report, "Settings.RestartOnFailure.Interval", s.RestartOnFailure.Interval, KindUnknownDrift)
			classifyAgainstPinned(&report, "Settings.RestartOnFailure.Count", s.RestartOnFailure.Count, KindUnknownDrift)
			appendUnknownElements(&report, "Settings.RestartOnFailure", s.RestartOnFailure.InnerXML, []string{
				"Interval", "Count",
			})
		}

		// Catch unknown elements directly under <Settings>.
		appendUnknownElements(&report, "Settings", s.InnerXML, []string{
			"AllowHardTerminate", "StartWhenAvailable", "RunOnlyIfNetworkAvailable",
			"MultipleInstancesPolicy", "DisallowStartIfOnBatteries", "StopIfGoingOnBatteries",
			"IdleSettings", "AllowStartOnDemand", "Enabled", "Hidden", "RunOnlyIfIdle",
			"WakeToRun", "ExecutionTimeLimit", "Priority", "RestartOnFailure",
		})
	}

	// --- Triggers ---------------------------------------------------------
	// Spec bucket 3: non-LogonTrigger triggers ride the autostart shim.
	// Spec bucket 1: matching LogonTrigger (with correct UserId) is silent.
	// The trigger comparison depends on what the original spec expected.
	if doc.Triggers != nil {
		t := doc.Triggers

		// If spec expected LogonTrigger but observed has none (or has only
		// non-LogonTrigger entries), surface that as bucket-3 preserve-shim.
		if spec.LogonTrigger {
			if t.LogonTrigger == nil {
				report.add(Deviation{
					XPath: "Triggers.LogonTrigger",
					Got:   "missing",
					Want:  "<LogonTrigger>...</LogonTrigger>",
					Kind:  KindKnownPreserveShim,
				})
			} else {
				// LogonTrigger present — verify UserId + Enabled.
				if t.LogonTrigger.UserId != "" && t.LogonTrigger.UserId != currentUser {
					report.add(Deviation{
						XPath: "Triggers.LogonTrigger.UserId",
						Got:   t.LogonTrigger.UserId,
						Want:  currentUser,
						Kind:  KindUnsupportedAbort,
					})
				}
				if t.LogonTrigger.Enabled != "" {
					classifyAgainstPinned(&report, "Triggers.LogonTrigger.Enabled", t.LogonTrigger.Enabled, KindUnknownDrift)
				}
				appendUnknownElements(&report, "Triggers.LogonTrigger", t.LogonTrigger.InnerXML, []string{
					"UserId", "Enabled",
				})
			}
		}

		// Any non-LogonTrigger trigger (CalendarTrigger, BootTrigger, etc.)
		// that the spec did NOT expect is bucket-3 preserve-shim.
		if t.CalendarTrigger != nil && spec.WeeklyTrigger == nil {
			report.add(Deviation{
				XPath: "Triggers.CalendarTrigger",
				Got:   "<CalendarTrigger>...</CalendarTrigger>",
				Want:  "",
				Kind:  KindKnownPreserveShim,
			})
		}
		// BootTrigger / TimeTrigger / EventTrigger / SessionStateTrigger /
		// RegistrationTrigger / IdleTrigger — all preserve-shim.
		preserveShimIfPresent := func(p *anyTriggerXML, xpath string) {
			if p != nil {
				report.add(Deviation{
					XPath: xpath,
					Got:   "present",
					Want:  "",
					Kind:  KindKnownPreserveShim,
				})
			}
		}
		preserveShimIfPresent(t.BootTrigger, "Triggers.BootTrigger")
		preserveShimIfPresent(t.TimeTrigger, "Triggers.TimeTrigger")
		preserveShimIfPresent(t.EventTrigger, "Triggers.EventTrigger")
		preserveShimIfPresent(t.SessionStateTrigger, "Triggers.SessionStateTrigger")
		preserveShimIfPresent(t.RegistrationTrigger, "Triggers.RegistrationTrigger")
		preserveShimIfPresent(t.IdleTrigger, "Triggers.IdleTrigger")
	}

	// --- Actions ----------------------------------------------------------
	// Custom <Actions> = bucket-4 (abort). "Custom" = Exec.Command not
	// mcphub.exe (case-insensitive, path-suffix match) or argv missing the
	// `daemon` token.
	if doc.Actions != nil && doc.Actions.Exec != nil {
		ex := doc.Actions.Exec
		if !isMcphubExe(ex.Command) {
			report.add(Deviation{
				XPath: "Actions.Exec.Command",
				Got:   ex.Command,
				Want:  spec.Command,
				Kind:  KindUnsupportedAbort,
			})
		}
		if ex.Arguments != "" && !isDaemonArgv(ex.Arguments) {
			report.add(Deviation{
				XPath: "Actions.Exec.Arguments",
				Got:   ex.Arguments,
				Want:  "daemon ...",
				Kind:  KindUnsupportedAbort,
			})
		}
		// WorkingDirectory is informational — no fixed pin, accept any value.

		appendUnknownElements(&report, "Actions.Exec", ex.InnerXML, []string{
			"Command", "Arguments", "WorkingDirectory",
		})
	}

	return report
}

// classifyAgainstPinned compares an observed scalar against the pinnedDefaults
// table at the given xpath. If the field is unset in the observed XML (empty
// string), no deviation is recorded. If it matches the pinned value, no
// deviation is recorded. Otherwise the supplied driftKind is used.
func classifyAgainstPinned(report *DeviationReport, xpath, observed string, driftKind DeviationKind) {
	if observed == "" {
		return
	}
	want, ok := pinnedDefaults[xpath]
	if !ok {
		// No pinned default — surface as unknown drift since we have no
		// authority to claim what the canonical value is. (Defensive guard;
		// in practice every xpath the classifier calls into has an entry.)
		report.add(Deviation{
			XPath: xpath,
			Got:   observed,
			Kind:  KindUnknownDrift,
		})
		return
	}
	if observed == want {
		return
	}
	report.add(Deviation{
		XPath: xpath,
		Got:   observed,
		Want:  want,
		Kind:  driftKind,
	})
}

// appendUnknownElements scans an InnerXML body for top-level elements not
// in the knownNames list and records each as a KindUnknownDrift deviation.
// Whitespace text nodes between elements are ignored. Nested element bodies
// are not recursed (callers attach catch-alls on the children they care
// about).
func appendUnknownElements(report *DeviationReport, parentXPath, innerXML string, knownNames []string) {
	if innerXML == "" {
		return
	}
	known := make(map[string]bool, len(knownNames))
	for _, n := range knownNames {
		known[n] = true
	}
	dec := xml.NewDecoder(strings.NewReader(innerXML))
	dec.Strict = true
	for {
		tok, err := dec.Token()
		if err != nil {
			// io.EOF or parse error — bounded by InnerXML length, safe to bail.
			return
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		name := start.Name.Local
		// Skip the rest of this element's subtree (don't recurse into
		// children — caller-attached catch-alls cover sub-levels).
		if err := dec.Skip(); err != nil {
			return
		}
		if !known[name] {
			report.add(Deviation{
				XPath: parentXPath + "." + name,
				Got:   "(unknown element)",
				Kind:  KindUnknownDrift,
			})
		}
	}
}

// isMcphubExe checks whether the <Command> value points to an mcphub
// executable. Case-insensitive on Windows; matches both bare `mcphub.exe`
// and absolute paths whose basename is `mcphub.exe` (or `mcphub` on POSIX).
func isMcphubExe(cmd string) bool {
	if cmd == "" {
		return false
	}
	// Strip path separators, take the last segment.
	last := cmd
	if i := strings.LastIndexAny(cmd, `\/`); i >= 0 {
		last = cmd[i+1:]
	}
	last = strings.ToLower(last)
	return last == "mcphub.exe" || last == "mcphub"
}

// isDaemonArgv checks that the joined argv string starts with the `daemon`
// subcommand (the only entrypoint v0.4.x used for scheduled-task launches).
// Leading whitespace tolerated.
func isDaemonArgv(args string) bool {
	args = strings.TrimSpace(args)
	if args == "daemon" {
		return true
	}
	return strings.HasPrefix(args, "daemon ") || strings.HasPrefix(args, "daemon\t")
}

// add appends a deviation and updates the convenience flag.
func (r *DeviationReport) add(d Deviation) {
	r.Deviations = append(r.Deviations, d)
	if d.Kind == KindUnsupportedAbort {
		r.HasUnsupportedAbort = true
	}
}

// stripXMLDeclaration drops a leading `<?xml ... ?>` processing instruction
// (and any leading BOM / whitespace) so encoding/xml's default UTF-8 reader
// handles the ASCII body even when the declaration claims a non-UTF-8
// encoding. Mirrors internal/scheduler/enumerate_all_windows.go's
// stripEnumerateXMLDeclaration — duplicated here so the migration package
// stays free of cross-package coupling for what is otherwise a 30-line
// helper.
func stripXMLDeclaration(raw string) string {
	// UTF-8 BOM.
	if len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF {
		raw = raw[3:]
	}
	// Leading whitespace.
	i := 0
	for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\r' || raw[i] == '\n') {
		i++
	}
	if i+5 > len(raw) || raw[i:i+5] != "<?xml" {
		return raw
	}
	end := strings.Index(raw[i:], "?>")
	if end < 0 {
		return raw
	}
	return raw[i+end+2:]
}
