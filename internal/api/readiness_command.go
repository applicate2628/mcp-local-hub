package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

type CommandOperationV1 string

const (
	CommandOperationInstall CommandOperationV1 = "install"
	CommandOperationRestart CommandOperationV1 = "restart"
)

// ReceivingReadinessRequestV1 is frozen by the command adapter before its
// mutation runs; the receiver never re-plans or re-reads client intent.
type ReceivingReadinessRequestV1 struct {
	Operation           CommandOperationV1
	ExpectedTasks       []string
	BindingExpectations []bindingExpectationV1
	RequireBindings     bool
	CanMigrate          bool
	StartupBindDeadline time.Duration
}

type CommandSettlementV1 struct {
	SchemaVersion           string                     `json:"schema_version"`
	Operation               CommandOperationV1         `json:"operation"`
	CommitState             string                     `json:"commit_state"`
	MutationRows            []CommandMutationRowV1     `json:"mutation_rows"`
	Snapshot                ReadinessSnapshotV1        `json:"snapshot"`
	PrimaryFailure          *ReadinessFailureV1        `json:"primary_failure,omitempty"`
	Failures                []ReadinessFailureV1       `json:"failures"`
	ClientConfigSettlements []ClientConfigSettlementV1 `json:"client_config_settlements,omitempty"`
}

// CommandMutationRowV1 is the portable, stable-ID-only projection of one
// command mutation row. Raw scheduler and client errors remain on their
// legacy rows and never enter a settlement wire projection.
type CommandMutationRowV1 struct {
	Server    string `json:"server,omitempty"`
	TaskName  string `json:"task_name,omitempty"`
	FailureID string `json:"failure_id,omitempty"`
}

// InstallMutationReceiptV1 is made from the exact plan instance passed to the
// low-level core before it is applied. A receiver must never create a plan.
type InstallMutationReceiptV1 struct {
	DryRun                  bool
	Committed               bool
	ExpectedTasks           []string
	bindingExpectations     []bindingExpectationV1
	RequireBindings         bool
	CanMigrate              bool
	StartupBindDeadline     time.Duration
	MutationRows            []CommandMutationRowV1
	ClientConfigSettlements []ClientConfigSettlementV1
}

// bindingExpectationV1 is deliberately in-memory only. It contains the exact
// adapter-normalized identity from the plan before mutation, never a later
// observation and never a JSON, event, log, or settlement field.
type bindingExpectationV1 struct {
	Client   string
	Expected clients.MCPEntry
}

type RestartCommandRequestV1 struct {
	Server       string
	DaemonFilter string
	All          bool
}

type RestartMutationReceiptV1 struct {
	Committed     bool
	Results       []RestartResult
	ExpectedTasks []string
	MutationRows  []CommandMutationRowV1
}

// CommandMutationPort is intentionally receiver-free so legacy API callers
// preserve their mutation-only contract.
type CommandMutationPort interface {
	Install(context.Context, InstallOpts) (InstallMutationReceiptV1, error)
	Restart(context.Context, RestartCommandRequestV1) (RestartMutationReceiptV1, error)
}

type ReceivingReadinessPort interface {
	Await(context.Context, ReceivingReadinessRequestV1) (CommandSettlementV1, error)
}

type apiCommandMutationAdapter struct{ api *API }

func (p apiCommandMutationAdapter) Install(ctx context.Context, opts InstallOpts) (InstallMutationReceiptV1, error) {
	return p.api.installCommandMutation(ctx, opts)
}

func (p apiCommandMutationAdapter) Restart(ctx context.Context, req RestartCommandRequestV1) (RestartMutationReceiptV1, error) {
	return p.api.restartCommandMutation(ctx, req)
}

func (a *API) installCommandMutation(ctx context.Context, opts InstallOpts) (InstallMutationReceiptV1, error) {
	var receipt InstallMutationReceiptV1
	err := a.installWithFrozenPlan(ctx, opts, func(frozen InstallMutationReceiptV1) { receipt = frozen })
	if err != nil {
		return receipt, err
	}
	if !receipt.DryRun {
		receipt.Committed = true
	}
	return receipt, nil
}

func (a *API) restartCommandMutation(ctx context.Context, req RestartCommandRequestV1) (RestartMutationReceiptV1, error) {
	var (
		results []RestartResult
		err     error
	)
	if req.All {
		results, err = a.restartAllWithFrozenDispatch(ctx)
	} else {
		results, err = a.restartWithFrozenDispatch(ctx, req.Server, req.DaemonFilter)
	}
	receipt := RestartMutationReceiptV1{Results: append([]RestartResult(nil), results...)}
	for _, result := range results {
		row := CommandMutationRowV1{TaskName: result.TaskName}
		if result.Err != "" {
			row.FailureID = result.Code
			if row.FailureID == "" {
				row.FailureID = "restart_dispatch_failed"
			}
		}
		receipt.MutationRows = append(receipt.MutationRows, row)
	}
	if err != nil {
		return receipt, err
	}
	receipt.Committed = true
	for _, result := range results {
		if result.Err == "" && strings.TrimSpace(result.TaskName) != "" {
			receipt.ExpectedTasks = append(receipt.ExpectedTasks, result.TaskName)
		}
	}
	return receipt, nil
}

func frozenInstallMutationReceipt(plan *Plan, opts InstallOpts) InstallMutationReceiptV1 {
	receipt := InstallMutationReceiptV1{DryRun: opts.DryRun, CanMigrate: plan.CanMigrate}
	seen := make(map[string]struct{})
	appendTask := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if !strings.HasPrefix(name, `\`) {
			name = `\` + name
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		receipt.ExpectedTasks = append(receipt.ExpectedTasks, name)
		receipt.MutationRows = append(receipt.MutationRows, CommandMutationRowV1{Server: opts.Server, TaskName: name})
	}
	for _, task := range plan.SupervisorIntent {
		appendTask(task.Name)
		if deadline := time.Duration(task.StartupBindDeadlineSeconds) * time.Second; deadline > receipt.StartupBindDeadline {
			receipt.StartupBindDeadline = deadline
		}
	}
	for _, task := range plan.SchedulerTasks {
		appendTask(task.Name)
	}
	return receipt
}

func commandSettlementV1(operation CommandOperationV1, commitState string, rows []CommandMutationRowV1, snapshot ReadinessSnapshotV1) CommandSettlementV1 {
	settlement := CommandSettlementV1{
		SchemaVersion: "command-settlement-v1", Operation: operation, CommitState: commitState,
		MutationRows: append([]CommandMutationRowV1(nil), rows...), Snapshot: snapshot,
		Failures: append([]ReadinessFailureV1(nil), snapshot.Failures...),
	}
	if snapshot.PrimaryFailure != nil {
		failure := *snapshot.PrimaryFailure
		settlement.PrimaryFailure = &failure
	}
	return settlement
}

func freezeInstallBindingExpectations(plan *Plan, server, relayExePath string) ([]bindingExpectationV1, error) {
	allClients := clients.AllClients()
	expectations := make([]bindingExpectationV1, 0, len(plan.ClientUpdates))
	for _, update := range plan.ClientUpdates {
		client := allClients[update.Client]
		if client == nil {
			return nil, &ReadinessErrorV1{Stage: ReadinessStageClientBinding, FailureID: "client_binding_unreadable"}
		}
		requested := installClientEntryV1(server, update, relayExePath)
		expectations = append(expectations, bindingExpectationV1{
			Client: update.Client, Expected: intendedEntryReadbackProjection(client, requested),
		})
	}
	return expectations, nil
}

// CommandCoordinator is deliberately above legacy API cores. CLI/GUI call it
// after one committed mutation receipt; low-level callers remain receiver-free.
type CommandCoordinator struct {
	Mutation CommandMutationPort
	Receiver ReceivingReadinessPort
}

func NewCommandCoordinator(a *API) CommandCoordinator {
	receiver, err := NewReceivingReadinessPort(ReceivingReadinessDepsV1{
		Reader: apiReceivingStateReaderV1{}, Clock: systemUTCClockV1{}, Waiter: timerWaiterV1{}, Deadline: commandSettlementDeadlineV1{},
	})
	if err != nil {
		return CommandCoordinator{Mutation: apiCommandMutationAdapter{api: a}}
	}
	return CommandCoordinator{Mutation: apiCommandMutationAdapter{api: a}, Receiver: receiver}
}

func (c CommandCoordinator) Settle(ctx context.Context, req ReceivingReadinessRequestV1) (CommandSettlementV1, error) {
	if c.Receiver == nil {
		return commandSettlementV1(req.Operation, "committed_unverified", nil, ReadinessSnapshotV1{}), &ReadinessErrorV1{Stage: ReadinessStageSupervisorBootstrap, FailureID: "supervisor_bootstrap_failed"}
	}
	return c.Receiver.Await(ctx, req)
}

// Install applies one low-level mutation and then, only for a committed
// non-dry-run receipt, calls the sole readiness receiver exactly once.
func (c CommandCoordinator) Install(ctx context.Context, opts InstallOpts) (CommandSettlementV1, error) {
	if c.Mutation == nil {
		return commandSettlementV1(CommandOperationInstall, "not_committed", nil, ReadinessSnapshotV1{}), fmt.Errorf("command install mutation port is unavailable")
	}
	receipt, err := c.Mutation.Install(ctx, opts)
	if err != nil {
		commitState := "not_committed"
		if receipt.Committed {
			commitState = "committed_unverified"
		}
		settlement := commandSettlementV1(CommandOperationInstall, commitState, receipt.MutationRows, ReadinessSnapshotV1{})
		settlement.ClientConfigSettlements = append([]ClientConfigSettlementV1(nil), receipt.ClientConfigSettlements...)
		return settlement, err
	}
	if receipt.DryRun {
		return commandSettlementV1(CommandOperationInstall, "dry_run", receipt.MutationRows, ReadinessSnapshotV1{}), nil
	}
	if !receipt.Committed || len(receipt.ExpectedTasks) == 0 {
		settlement := commandSettlementV1(CommandOperationInstall, "committed_unverified", receipt.MutationRows, ReadinessSnapshotV1{})
		settlement.ClientConfigSettlements = append([]ClientConfigSettlementV1(nil), receipt.ClientConfigSettlements...)
		return settlement, fmt.Errorf("install committed without a frozen task receipt")
	}
	settlement, err := c.Settle(ctx, ReceivingReadinessRequestV1{Operation: CommandOperationInstall, ExpectedTasks: receipt.ExpectedTasks, BindingExpectations: receipt.bindingExpectations, RequireBindings: receipt.RequireBindings, CanMigrate: receipt.CanMigrate, StartupBindDeadline: receipt.StartupBindDeadline})
	settlement.MutationRows = append([]CommandMutationRowV1(nil), receipt.MutationRows...)
	settlement.Failures = append([]ReadinessFailureV1(nil), settlement.Snapshot.Failures...)
	settlement.ClientConfigSettlements = append([]ClientConfigSettlementV1(nil), receipt.ClientConfigSettlements...)
	return settlement, err
}

// InstallBatch preserves per-server command semantics for GUI install-all.
// It never routes an applied row through the legacy bulk mutation path.
func (c CommandCoordinator) InstallBatch(ctx context.Context, names []string, opts InstallAllOpts) []InstallResult {
	results := make([]InstallResult, 0, len(names))
	for _, name := range names {
		settlement, err := c.Install(ctx, InstallOpts{
			Server:            name,
			ClientsInclude:    opts.ClientsInclude,
			IncludeAllClients: opts.IncludeAllClients,
			DryRun:            opts.DryRun,
			Writer:            opts.Writer,
			GUIPort:           opts.GUIPort,
			RoutingTarget:     opts.RoutingTarget,
		})
		results = append(results, InstallResult{Server: name, Err: err, Settlement: settlement, ClientConfigSettlements: append([]ClientConfigSettlementV1(nil), settlement.ClientConfigSettlements...)})
	}
	return results
}

// InstallAll resolves the same manifest population as the legacy bulk API but
// settles every applied server through this coordinator.
func (c CommandCoordinator) InstallAll(ctx context.Context, opts InstallAllOpts) []InstallResult {
	names, err := listManifestNamesEmbedFirst()
	if err != nil {
		return []InstallResult{{Err: err}}
	}
	installable := make([]string, 0, len(names))
	var skipped []string
	for _, name := range names {
		if data, readErr := loadManifestYAMLEmbedFirst(name); readErr == nil {
			if manifest, parseErr := parseManifestForName(name, data); parseErr == nil && manifest.Kind == config.KindWorkspaceScoped {
				skipped = append(skipped, name)
				continue
			}
		}
		installable = append(installable, name)
	}
	results := c.InstallBatch(ctx, installable, opts)
	if len(skipped) > 0 && opts.Writer != nil {
		fmt.Fprintf(opts.Writer, "Skipped %d workspace-scoped manifest(s); use `mcphub register` instead: %v\n", len(skipped), skipped)
	}
	return results
}

// Restart keeps every low-level result row intact, settles only successful
// dispatches, and never overwrites a row error with a shared readiness error.
func (c CommandCoordinator) Restart(ctx context.Context, request RestartCommandRequestV1) (RestartMutationReceiptV1, CommandSettlementV1, error) {
	if c.Mutation == nil {
		return RestartMutationReceiptV1{}, commandSettlementV1(CommandOperationRestart, "not_committed", nil, ReadinessSnapshotV1{}), fmt.Errorf("command restart mutation port is unavailable")
	}
	receipt, err := c.Mutation.Restart(ctx, request)
	if err != nil {
		return receipt, commandSettlementV1(CommandOperationRestart, "not_committed", receipt.MutationRows, ReadinessSnapshotV1{}), err
	}
	if !receipt.Committed {
		return receipt, commandSettlementV1(CommandOperationRestart, "not_committed", receipt.MutationRows, ReadinessSnapshotV1{}), fmt.Errorf("restart did not commit")
	}
	settlement := commandSettlementV1(CommandOperationRestart, "settled", receipt.MutationRows, ReadinessSnapshotV1{})
	if len(receipt.ExpectedTasks) > 0 {
		settlement, err = c.Settle(ctx, ReceivingReadinessRequestV1{Operation: CommandOperationRestart, ExpectedTasks: receipt.ExpectedTasks})
		if err != nil {
			settlement.MutationRows = append([]CommandMutationRowV1(nil), receipt.MutationRows...)
			settlement.Failures = append([]ReadinessFailureV1(nil), settlement.Snapshot.Failures...)
			return receipt, settlement, err
		}
	}
	for _, row := range receipt.Results {
		if row.Err != "" {
			settlement.CommitState = "committed_unverified"
			return receipt, settlement, fmt.Errorf("one or more daemons failed to restart")
		}
	}
	return receipt, settlement, nil
}
