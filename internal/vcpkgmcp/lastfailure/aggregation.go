package lastfailure

import "sort"

type diagnosticCandidate struct {
	diagnostic Diagnostic
	file       phaseLogFile
}

type diagnosticAccumulator struct {
	limit     int
	cells     [severityBudgetClasses][tierBudgetClasses][]diagnosticCandidate
	dropped   int
	textCut   bool
	valueCut  bool
	highWater int
	commands  []phaseCommand
}

type phaseCommand struct{ config, command string }

func newDiagnosticAccumulator(limit int) *diagnosticAccumulator {
	return &diagnosticAccumulator{limit: limit}
}

func (a *diagnosticAccumulator) addCommand(file phaseLogFile, buildCommand string) {
	if buildCommand == "" {
		return
	}
	for _, command := range a.commands {
		if command.config == file.Config {
			return
		}
	}
	a.commands = append(a.commands, phaseCommand{config: file.Config, command: buildCommand})
}

func (a *diagnosticAccumulator) addDropped(count int) {
	a.dropped += count
}

// addDiagnostic caps one parsed diagnostic before it enters retained phase
// state, then spends the phase-cell budget. The streaming scanner calls this
// once per match; no per-log diagnostic slice is materialized.
func (a *diagnosticAccumulator) addDiagnostic(file phaseLogFile, diagnostic Diagnostic) {
	bounded, textCut, valueCut := truncateDiagnostic(diagnostic)
	a.textCut = a.textCut || textCut
	a.valueCut = a.valueCut || valueCut
	severity, tier := severityBudgetClass(bounded), tierRank(bounded.Tier)
	if len(a.cells[severity][tier]) >= a.limit {
		a.dropped++
		return
	}
	a.cells[severity][tier] = append(a.cells[severity][tier], diagnosticCandidate{
		diagnostic: bounded, file: file,
	})
	retained := 0
	for i := range a.cells {
		for j := range a.cells[i] {
			retained += len(a.cells[i][j])
		}
	}
	if retained > a.highWater {
		a.highWater = retained
	}
}

type diagnosticAccumulatorCheckpoint struct {
	cellLengths [severityBudgetClasses][tierBudgetClasses]int
	dropped     int
	textCut     bool
	valueCut    bool
	highWater   int
	commands    int
}

func (a *diagnosticAccumulator) checkpoint() diagnosticAccumulatorCheckpoint {
	checkpoint := diagnosticAccumulatorCheckpoint{
		dropped: a.dropped, textCut: a.textCut, valueCut: a.valueCut,
		highWater: a.highWater, commands: len(a.commands),
	}
	for severity := range a.cells {
		for tier := range a.cells[severity] {
			checkpoint.cellLengths[severity][tier] = len(a.cells[severity][tier])
		}
	}
	return checkpoint
}

func (a *diagnosticAccumulator) restore(checkpoint diagnosticAccumulatorCheckpoint) {
	for severity := range a.cells {
		for tier := range a.cells[severity] {
			a.cells[severity][tier] = a.cells[severity][tier][:checkpoint.cellLengths[severity][tier]]
		}
	}
	a.dropped = checkpoint.dropped
	a.textCut = checkpoint.textCut
	a.valueCut = checkpoint.valueCut
	a.highWater = checkpoint.highWater
	a.commands = a.commands[:checkpoint.commands]
}

func (a *diagnosticAccumulator) ranked() []diagnosticCandidate {
	out := make([]diagnosticCandidate, 0, a.highWater)
	for s := range a.cells {
		for tier := range a.cells[s] {
			out = append(out, a.cells[s][tier]...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return diagnosticOutranks(out[i].diagnostic, out[j].diagnostic)
	})
	return out
}

type phaseSummary struct {
	phase      Phase
	candidates []diagnosticCandidate
	dropped    int
	textCut    bool
	valueCut   bool
	highWater  int
	commands   []phaseCommand
}

func (s phaseSummary) diagnostics() []Diagnostic {
	out := make([]Diagnostic, len(s.candidates))
	for i := range s.candidates {
		out[i] = s.candidates[i].diagnostic
	}
	return out
}

func (s phaseSummary) hasFailure() bool {
	for _, candidate := range s.candidates {
		if candidate.diagnostic.Severity == SeverityError {
			return true
		}
	}
	return false
}

func (s phaseSummary) headline() (diagnosticCandidate, bool) {
	if len(s.candidates) == 0 {
		return diagnosticCandidate{}, false
	}
	return s.candidates[0], true
}

func (s phaseSummary) commandFor(config string) string {
	for _, command := range s.commands {
		if command.config == config {
			return command.command
		}
	}
	return ""
}
