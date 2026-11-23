package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

type mcpFrontRouteAdmissionReceipt struct {
	JournalBeforeSHA256       string                  `json:"journal_before_sha256"`
	SettingsSHA256            string                  `json:"settings_sha256"`
	IntentSHA256              string                  `json:"intent_sha256"`
	IncumbentDescriptorSHA256 string                  `json:"incumbent_descriptor_sha256"`
	Incumbent                 api.SupervisorLockOwner `json:"incumbent"`
}

func (r mcpFrontRouteAdmissionReceipt) valid(selection api.RouteActivationSelection) bool {
	for _, digest := range []string{r.JournalBeforeSHA256, r.SettingsSHA256, r.IntentSHA256, r.IncumbentDescriptorSHA256} {
		raw, err := hex.DecodeString(digest)
		if err != nil || len(raw) != sha256.Size || hex.EncodeToString(raw) != digest {
			return false
		}
	}
	birth, err := time.Parse(time.RFC3339Nano, r.Incumbent.StartedAt)
	return err == nil && !birth.IsZero() && r.Incumbent.PID > 0 && r.Incumbent.PID != selection.Supervisor.PID
}

type routeAdmissionPreimage struct {
	report    *mcpFrontReconcileReport
	raw       []byte
	selection api.RouteActivationSelection
	receipt   mcpFrontRouteAdmissionReceipt
	incumbent *daemonCreatorOperation
	binding   *routeAggregateBinding
	readers   []*api.StateFileReadObservation
}

type routeAdmissionLease struct {
	verify      func(context.Context) error
	release     func() routeAdmissionLeaseClose
	state       *api.StateFileObservedLease
	acquisition flock.AcquisitionObservation
}

type routeAdmissionLeaseClose struct {
	closed    bool
	err       error
	primitive flock.ReleaseObservation
	observed  *api.StateFileLeaseCloseObservation
}

type routeAdmissionLeaseReceipt struct {
	owner *routeAdmissionLease
	close routeAdmissionLeaseClose
}

type routeAdmissionLeaseRole uint8

const (
	routeAdmissionReconcile routeAdmissionLeaseRole = iota
	routeAdmissionRouting
	routeAdmissionSettings
	routeAdmissionIntent
	routeAdmissionReport
	routeAdmissionLeaseCount
)

const routeAdmissionPending = "pending"

var errRouteAdmissionNativePipeCloseUnavailable = errors.New("original native pipe close result unavailable")

type routeAdmissionWrite struct {
	entered                                         bool
	validate                                        func(context.Context, string) error
	capture                                         *clients.ConfigWriteCaptureTransaction
	publication                                     *api.StateFilePublicationObservation
	close                                           *clients.ConfigWriteParentCloseObservation
	captureErr, writeErr, closeErr, receiveCloseErr error
}

type routeAdmissionRead struct {
	report      *mcpFrontReconcileReport
	raw         []byte
	observation *api.StateFileReadObservation
	err         error
}

type routeAdmissionOps struct {
	observe func(context.Context) (routeAdmissionPreimage, error)
	lock    func(context.Context, routeAdmissionLeaseRole) (*routeAdmissionLease, error)
	write   func(context.Context, *routeAdmissionLease, string, []byte, func(context.Context) error) (routeAdmissionWrite, error)
	read    func(context.Context) routeAdmissionRead
	live    func(context.Context, routeAdmissionPreimage) error
	after   func(context.Context, routeAdmissionPreimage) (routeAdmissionPreimage, error)
}

type routeAdmissionResult struct {
	Phase         string `json:"phase"`
	JournalSHA256 string `json:"journal_sha256"`
}

type routeAdmissionReservation struct {
	closed           atomic.Bool
	conflicted       atomic.Bool
	permitted        atomic.Bool
	used             bool                            // controller FIFO only
	received         bool                            // controller FIFO only
	operationReceipt *routeAdmissionOperationReceipt // controller FIFO only
	finalized        bool                            // controller FIFO only
	check            func(context.Context) error
	current          func() bool // short in-memory comparison for the FIFO
	ops              routeAdmissionOps
	before           routeAdmissionPreimage // immutable after posting confirmation
	selection        atomic.Pointer[routeAdmissionPreimage]
	outcome          atomic.Pointer[routeAdmissionOutcome] // retains unresolved native owners even if final FIFO delivery fails
	transport        atomic.Pointer[routeAdmissionTransport]
	request          context.Context
	wire             routeAdmissionWire
}

// The original reader and final projection each enter this memory-only
// guard. No pipe or filesystem operation runs while it is held.
type routeAdmissionWire struct {
	mu        sync.Mutex
	readerErr error
	projected bool
}

func (w *routeAdmissionWire) readerEnded(err error, cancel context.CancelFunc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.projected {
		w.readerErr = errors.Join(err, errors.New("original Route admission reader ended"))
		cancel()
	}
}

func (w *routeAdmissionWire) requestError(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return errors.Join(ctx.Err(), w.readerErr)
}

func (w *routeAdmissionWire) project(ctx context.Context, reply routeAdmissionReply) routeAdmissionOutcome {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := reply.out
	if w.projected || !reply.accepted {
		out.err = errors.Join(out.err, errors.New("exact Route admission terminal FIFO acknowledgement unavailable"))
	}
	w.projected = true
	return routeAdmissionPendingOnError(out, errors.Join(ctx.Err(), w.readerErr))
}

func routeAdmissionPendingOnError(out routeAdmissionOutcome, err error) routeAdmissionOutcome {
	out.err = errors.Join(out.err, err)
	if out.err != nil && out.result.Phase != "" {
		out.result.Phase = routeAdmissionPending
	}
	return out
}

type routeAdmissionOutcome struct {
	result                       routeAdmissionResult
	err                          error
	write                        routeAdmissionWrite
	readback, finalReadback      *routeAdmissionRead
	leases                       [routeAdmissionLeaseCount]routeAdmissionLeaseReceipt
	callerCloseErr               error
	provedClosedNoEffect         bool // received before handing the sealed outcome to the FIFO
	operationResourcesClosed     bool
	preflight, locked, rechecked *routeAdmissionPreimage
	operator                     *routeAdmissionOperator
	operatorClose                routeAdmissionOperatorClose
	native                       routeAdmissionNativeObservations
}

type routeAdmissionNativeEvidence interface{ ResourcesFinalized() bool }

type routeAdmissionNativeObservation struct {
	stage      string
	evidence   routeAdmissionNativeEvidence
	err        error
	notEntered bool
}

// Only the original pipe worker appends native attempt outcomes. A later
// successful query cannot erase an earlier unresolved close.
type routeAdmissionNativeObservations []routeAdmissionNativeObservation

func (o *routeAdmissionNativeObservations) add(stage string, evidence routeAdmissionNativeEvidence, err error) {
	*o = append(*o, routeAdmissionNativeObservation{stage: stage, evidence: evidence, err: err})
}

func (o *routeAdmissionNativeObservations) addNotEntered(stage string, err error) {
	*o = append(*o, routeAdmissionNativeObservation{stage: stage, err: err, notEntered: true})
}

func (o routeAdmissionNativeObservations) ResourcesFinalized() bool {
	for _, observation := range o {
		if !observation.notEntered && (observation.evidence == nil || !observation.evidence.ResourcesFinalized()) {
			return false
		}
	}
	return true
}

type routeAdmissionOperator struct {
	check        func(context.Context) error
	productClose func() error
	callerClose  func() error
}

type routeAdmissionOperatorClose struct {
	callerAttempted, productAttempted bool
	callerErr, productErr             error
}

func (o *routeAdmissionOperator) close() (result routeAdmissionOperatorClose) {
	if o != nil && o.callerClose != nil {
		result.callerAttempted = true
		result.callerErr = o.callerClose()
	}
	if o != nil && o.productClose != nil {
		result.productAttempted = true
		result.productErr = o.productClose()
	}
	return result
}

func (out routeAdmissionOutcome) leasesClosed() bool {
	for _, lease := range out.leases {
		if lease.owner != nil && !lease.close.closed {
			return false
		}
	}
	return true
}

func (out routeAdmissionOutcome) sealForFIFO(callerCloseErr error) routeAdmissionOutcome {
	out.callerCloseErr = callerCloseErr
	out.err = errors.Join(out.err, callerCloseErr)
	w := out.write
	out.operationResourcesClosed = out.resourcesFinalized()
	out.provedClosedNoEffect = out.result.Phase == "" && out.operationResourcesClosed && !w.entered &&
		(w.publication == nil || w.publication.SettledWithoutRename())
	return out
}

func (out routeAdmissionOutcome) resourcesFinalized() bool {
	w := out.write
	return out.callerCloseErr == nil && out.leasesClosed() && out.readersFinalized() && out.native.ResourcesFinalized() &&
		(w.capture == nil || w.close.ValidateTransaction(w.capture) == nil) &&
		(w.publication == nil || w.publication.ResourcesClosed() || w.publication.SettledWithoutRename())
}

func (out routeAdmissionOutcome) readersFinalized() bool {
	for _, source := range []*routeAdmissionPreimage{out.preflight, out.locked, out.rechecked} {
		if source != nil {
			for _, reader := range source.readers {
				if !reader.ResourcesFinalized() {
					return false
				}
			}
		}
	}
	for _, read := range []*routeAdmissionRead{out.readback, out.finalReadback} {
		if read != nil && !read.observation.ResourcesFinalized() {
			return false
		}
	}
	return true
}

func quietRouteAdmissionLease(ctx context.Context, path string) (*routeAdmissionLease, error) {
	owned, before, err := api.AcquireSupervisorLockQuietObserved(ctx, path)
	if owned == nil {
		return nil, err
	}
	return &routeAdmissionLease{
		verify:      func(ctx context.Context) error { return owned.ValidateQuietObservedHeld(ctx, path) },
		acquisition: before,
		release: func() routeAdmissionLeaseClose {
			after, err := owned.ReleaseQuietObserved()
			closed := after.Generation == before.Generation && after.AcquisitionReturned &&
				after.AllocatedFilesClosed && !after.Pending && (!before.Held || after.Complete)
			return routeAdmissionLeaseClose{closed: closed, err: err, primitive: after}
		},
	}, err
}

func acquireRouteAdmissionLease(ctx context.Context, role routeAdmissionLeaseRole, reconcile, settings, intent, journal string) (*routeAdmissionLease, error) {
	var lease *api.StateFileObservedLease
	var err error
	path := settings
	switch role {
	case routeAdmissionReconcile:
		return quietRouteAdmissionLease(ctx, reconcile)
	case routeAdmissionRouting:
		lease, err = api.TryAcquireMCPFrontRoutingTargetObserved(ctx, settings)
	case routeAdmissionSettings:
		lease, err = api.TryAcquireBoundSettingsObserved(ctx, settings)
	case routeAdmissionIntent:
		path = intent
		lease, err = api.TryAcquireStateFileObservedLease(ctx, path)
	case routeAdmissionReport:
		path = journal
		lease, err = api.TryAcquireStateFileObservedLease(ctx, path)
	default:
		return nil, errors.New("unknown Route admission lease role")
	}
	if lease == nil {
		return nil, err
	}
	acquired := lease.AcquisitionObservation()
	return &routeAdmissionLease{
		state:       lease,
		acquisition: acquired,
		verify: func(ctx context.Context) error {
			if role == routeAdmissionRouting {
				return api.ValidateMCPFrontRoutingTargetObserved(ctx, path, lease)
			}
			return lease.ValidateHeld(ctx, path)
		},
		release: func() routeAdmissionLeaseClose {
			close, err := lease.CloseObserved()
			actual := close.Snapshot()
			closed := actual.Generation == acquired.Generation && actual.AcquisitionReturned &&
				actual.AllocatedFilesClosed && !actual.Pending && (!acquired.Held || actual.Complete)
			if acquired.Held {
				err = errors.Join(err, close.Validate(lease))
			}
			return routeAdmissionLeaseClose{closed: closed, err: err, primitive: actual, observed: close}
		},
	}, err
}

func writeRouteAdmissionJournal(ctx context.Context, path string, lease *routeAdmissionLease, before string, data []byte, guard func(context.Context) error) (result routeAdmissionWrite, err error) {
	transaction, captureErr := clients.CaptureStateFileWriteSnapshotObserved(ctx, path, mcpFrontReconcileReportMaxBytes, before)
	result.capture, result.captureErr = transaction, captureErr
	defer func() {
		if transaction != nil {
			result.close, result.closeErr = transaction.CloseObserved()
			if result.publication != nil {
				result.receiveCloseErr = result.publication.ReceiveBorrowedParentClose(result.close)
			}
			err = errors.Join(err, result.closeErr, result.receiveCloseErr)
		}
	}()
	if captureErr != nil || transaction == nil {
		return result, errors.Join(errors.New("original Route journal snapshot unavailable"), captureErr)
	}
	req := clients.ConfigWriteRequest{Context: ctx, Path: path, Contents: data, ExpectedSnapshot: transaction.Snapshot(), BeforeObservedReplace: guard}
	if req.ExpectedSnapshot == nil || lease == nil || lease.state == nil || guard == nil {
		return result, errors.New("original Route journal writer unavailable")
	}
	publication, writeErr := api.WriteStateFileRouteAdmissionLockHeldObserved(req, lease.state)
	result.publication, result.writeErr = publication, writeErr
	result.entered = publication.RenameEntered()
	result.validate = func(ctx context.Context, digest string) error {
		return publication.Validate(ctx, req, digest)
	}
	return result, writeErr
}

func admitRouteActivation(ctx context.Context, reservation *routeAdmissionReservation) (out routeAdmissionOutcome) {
	if !reservation.permitted.CompareAndSwap(true, false) || reservation.before.report == nil {
		out.err = errors.New("original Route admission permit required")
		return out
	}
	if err := checkRouteAdmissionLive(ctx, reservation); err != nil {
		out.err = err
		return out
	}
	before := reservation.before
	out.preflight = &before
	defer func() {
		for n := len(out.leases) - 1; n >= 0; n-- {
			if lease := out.leases[n].owner; lease != nil {
				closed := lease.release()
				out.leases[n].close = closed
				releaseErr := closed.err
				if !closed.closed {
					releaseErr = errors.Join(releaseErr, errors.New("Route admission lock release pending"))
				}
				out.err = errors.Join(out.err, releaseErr)
			}
		}
		if out.result.JournalSHA256 != "" {
			if out.write.validate == nil {
				out.err = errors.Join(out.err, errors.New("original publication close unavailable"))
			} else {
				out.err = errors.Join(out.err, out.write.validate(ctx, out.result.JournalSHA256))
			}
			rechecked, recheckErr := reservation.ops.after(ctx, before)
			out.rechecked = &rechecked
			out.err = errors.Join(out.err, checkRouteAdmissionLive(ctx, reservation), recheckErr)
			readback := reservation.ops.read(ctx)
			out.finalReadback = &readback
			if readback.err != nil || fmt.Sprintf("%x", sha256.Sum256(readback.raw)) != out.result.JournalSHA256 {
				out.err = errors.Join(out.err, errors.New("Route admission changed during final settlement"), readback.err)
			}
		}
		if out.write.entered && out.err != nil {
			out.result.Phase = routeAdmissionPending
		}
	}()
	for role := routeAdmissionReconcile; role < routeAdmissionLeaseCount; role++ {
		lease, err := reservation.ops.lock(ctx, role)
		out.leases[role].owner = lease
		if err != nil || lease == nil {
			out.err = errors.Join(fmt.Errorf("Route admission lease %d unavailable", role), err)
			return out
		}
		if err := lease.verify(ctx); err != nil {
			out.err = err
			return out
		}
	}
	actual, err := reservation.ops.observe(ctx)
	out.locked = &actual
	if err != nil || !sameRouteAdmissionInputs(actual, before) || !bytes.Equal(actual.raw, before.raw) ||
		before.receipt.JournalBeforeSHA256 != fmt.Sprintf("%x", sha256.Sum256(before.raw)) {
		out.err = errors.Join(errors.New("Route admission preimage changed"), err)
		return out
	}
	proposed := *before.report
	proposed.Version = mcpFrontReconcileVersion5
	proposed.RouteActivation = &mcpFrontRouteActivation{
		Phase: mcpFrontRouteActivationAdmitted, FromEpoch: before.selection.FromEpoch, ToEpoch: before.selection.ToEpoch,
		Selection: &before.selection, Admission: &before.receipt,
	}
	if err := validateMCPFrontReconcileReport(&proposed, mcpFrontReconcileSerenaReportLeaf); err != nil {
		out.err = err
		return out
	}
	expected, err := marshalMCPFrontReconcileReport(&proposed)
	if err != nil {
		out.err = err
		return out
	}
	if err := checkRouteAdmissionLive(ctx, reservation); err != nil {
		out.err = err
		return out
	}
	out.write, out.err = reservation.ops.write(ctx, out.leases[routeAdmissionReport].owner, before.receipt.JournalBeforeSHA256, expected,
		func(ctx context.Context) error { return checkRouteAdmissionLive(ctx, reservation) })
	if !out.write.entered {
		out.err = errors.Join(out.err, errors.New("native Route admission replacement did not enter"))
		return out
	}
	readback := reservation.ops.read(ctx)
	out.readback = &readback
	if readback.err != nil || readback.report == nil || !bytes.Equal(readback.raw, expected) {
		out.err = errors.Join(out.err, errors.New("Route admission readback pending"), readback.err)
		return out
	}
	out.result = routeAdmissionResult{Phase: mcpFrontRouteActivationAdmitted, JournalSHA256: fmt.Sprintf("%x", sha256.Sum256(readback.raw))}
	return out
}

func sameRouteAdmissionInputs(actual, expected routeAdmissionPreimage) bool {
	return actual.incumbent == expected.incumbent && actual.binding == expected.binding &&
		actual.selection == expected.selection && actual.receipt == expected.receipt
}

func checkRouteAdmissionLive(ctx context.Context, reservation *routeAdmissionReservation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if reservation.closed.Load() || reservation.conflicted.Load() {
		return errors.New("original Route admission reservation changed")
	}
	if err := reservation.check(ctx); err != nil {
		return err
	}
	if err := reservation.ops.live(ctx, reservation.before); err != nil {
		return err
	}
	if reservation.closed.Load() || reservation.conflicted.Load() {
		return errors.New("original Route admission reservation changed")
	}
	return ctx.Err()
}

func (c *supervisorController) observeRouteAdmissionIncumbent(ctx context.Context, creators *daemonCreatorRecords, port int,
	phases *routeAdmissionNativeObservations) (*routeAggregateBinding, *daemonCreatorOperation, int, api.SupervisorLockOwner, error) {
	var zero api.SupervisorLockOwner
	creators.mu.Lock()
	b := creators.routeAggregate.binding
	if b == nil || !b.ready || creators.routeAggregateOperation() != b.operation || b.operation.starting {
		creators.mu.Unlock()
		return nil, nil, 0, zero, errAggregateUnavailable
	}
	creators.mu.Unlock()
	op, generation, err := creators.observeRouteAggregatePeerWithReceipt(ctx, b.conn, c.tracker, phases, "incumbent")
	if err != nil || op != b.operation || generation != b.generation {
		return nil, nil, 0, zero, errors.Join(errAggregatePeerChanged, err)
	}
	row, found := c.tracker.Get(api.BuiltinRouteTaskName)
	pid, listening, originalListener, err := api.ObserveOriginalLoopbackPortOwnerObserved(ctx, port)
	phases.add("incumbent-loopback-frame", originalListener, err)
	if err != nil || !found || !listening || row.State != daemonRuntimeStateRunning ||
		row.CurrentPID != pid || row.PIDGeneration != generation || row.StartedAt.IsZero() {
		return nil, nil, 0, zero, errors.Join(errAggregatePeerChanged, err)
	}
	if err := creators.checkRouteAggregateBindingWithReceipt(ctx, b, c.tracker, phases); err != nil {
		return nil, nil, 0, zero, err
	}
	return b, op, generation, api.SupervisorLockOwner{PID: pid, StartedAt: row.StartedAt.UTC().Format(time.RFC3339Nano)}, nil
}

// The FIFO and the final dispatch guard share only this short metadata
// comparison. Native creator, listener and pipe observations stay outside.
func (c *supervisorController) sameRouteAdmissionIncumbent(creators *daemonCreatorRecords, before routeAdmissionPreimage) bool {
	creators.mu.Lock()
	defer creators.mu.Unlock()
	op := before.incumbent
	b := creators.routeAggregate.binding
	if op == nil || b == nil || b != before.binding || !b.ready || b.operation != op ||
		b.generation != before.selection.PriorGeneration || creators.routeAggregateOperation() != op ||
		op.owner == nil || op.starting || op.startupErr != nil || op.root.Known || op.terminal != nil ||
		op.generation != before.selection.PriorGeneration || op.routeDescriptorSHA256 != before.receipt.IncumbentDescriptorSHA256 {
		return false
	}
	row, found := c.tracker.Get(api.BuiltinRouteTaskName)
	return found && row.State == daemonRuntimeStateRunning && row.CurrentPID == before.receipt.Incumbent.PID &&
		row.PIDGeneration == before.selection.PriorGeneration &&
		row.StartedAt.UTC().Format(time.RFC3339Nano) == before.receipt.Incumbent.StartedAt
}

func readRouteAdmissionJournal(ctx context.Context, path string) (result routeAdmissionRead) {
	raw, observation, err := api.ReadStateFileRouteAdmissionObserved(ctx, path, mcpFrontReconcileReportMaxBytes)
	result.raw, result.observation, result.err = raw, observation, err
	if err != nil {
		return result
	}
	if err := observation.Validate(ctx, path, mcpFrontReconcileReportMaxBytes, raw, fmt.Sprintf("%x", sha256.Sum256(raw))); err != nil {
		result.err = err
		return result
	}
	result.report, result.err = decodeMCPFrontReconcileReport(raw, path)
	if result.err == nil {
		result.err = validateMCPFrontReconcileReport(result.report, path)
	}
	return result
}

func (c *supervisorController) routeAdmissionOps(creators *daemonCreatorRecords, phases *routeAdmissionNativeObservations) routeAdmissionOps {
	s := creators.routeAggregate
	journal := filepath.Join(s.launch.StateDir, mcpFrontReconcileSerenaReportLeaf)
	lockPath := filepath.Join(s.launch.StateDir, mcpFrontReconcileLockLeaf)
	settings := api.SettingsPath()
	intent := filepath.Join(s.launch.StateDir, "supervisor-intent.json")
	read := func(ctx context.Context) routeAdmissionRead {
		return readRouteAdmissionJournal(ctx, journal)
	}
	return routeAdmissionOps{
		read: read,
		lock: func(ctx context.Context, role routeAdmissionLeaseRole) (*routeAdmissionLease, error) {
			return acquireRouteAdmissionLease(ctx, role, lockPath, settings, intent, journal)
		},
		write: func(ctx context.Context, lease *routeAdmissionLease, before string, data []byte, guard func(context.Context) error) (routeAdmissionWrite, error) {
			return writeRouteAdmissionJournal(ctx, journal, lease, before, data, guard)
		},
		observe: func(ctx context.Context) (routeAdmissionPreimage, error) {
			preimage := read(ctx)
			report := preimage.report
			journalReader := []*api.StateFileReadObservation{preimage.observation}
			if preimage.err != nil || report == nil || !report.Settled || report.RouteActivation != nil || report.GenerationAdmission != nil ||
				report.RouteOriginal != nil || hasMCPFrontRecoveryDisposition(report) {
				return routeAdmissionPreimage{readers: journalReader}, errors.Join(errors.New("fresh Route admission requires a settled unlinked predecessor"), preimage.err)
			}
			actual, err := c.observeRouteAdmissionInputs(ctx, creators, settings, intent, report, preimage.raw, phases)
			actual.readers = append(journalReader, actual.readers...)
			return actual, err
		},
		live: func(ctx context.Context, before routeAdmissionPreimage) error {
			b, op, generation, incumbent, err := c.observeRouteAdmissionIncumbent(ctx, creators, before.selection.FromEpoch.Port, phases)
			if err != nil || b != before.binding || op != before.incumbent || generation != before.selection.PriorGeneration ||
				incumbent != before.receipt.Incumbent || !c.sameRouteAdmissionIncumbent(creators, before) {
				return errors.Join(errAggregatePeerChanged, err)
			}
			return ctx.Err()
		},
		after: func(ctx context.Context, before routeAdmissionPreimage) (routeAdmissionPreimage, error) {
			actual, err := c.observeRouteAdmissionInputs(ctx, creators, settings, intent, before.report, before.raw, phases)
			if err != nil || !sameRouteAdmissionInputs(actual, before) {
				return actual, errors.Join(errors.New("Route admission inputs changed during settlement"), err)
			}
			return actual, nil
		},
	}
}

func (c *supervisorController) observeRouteAdmissionInputs(ctx context.Context, creators *daemonCreatorRecords, settings, intentPath string,
	report *mcpFrontReconcileReport, raw []byte, phases *routeAdmissionNativeObservations) (routeAdmissionPreimage, error) {
	var zero routeAdmissionPreimage
	s := creators.routeAggregate
	from, port, settingsDigest, settingsReader, err := api.ObserveMCPFrontRouteAdmissionSettings(ctx, settings)
	zero.readers = append(zero.readers, settingsReader)
	if err != nil {
		return zero, err
	}
	decision, err := classifyMCPFrontGeneration(report, from, port)
	if err != nil || !decision.Admission || from.State != api.MCPFrontRoutingTargetFront {
		return zero, errors.Join(errors.New("no exact fresh V5 Route activation"), err)
	}
	intent, intentDigest, intentReader, err := api.ObserveSupervisorIntentForRouteAdmission(ctx, intentPath)
	zero.readers = append(zero.readers, intentReader)
	if err != nil || intent == nil {
		return zero, errors.Join(errors.New("original Route intent unavailable"), err)
	}
	var command string
	for _, saved := range intent.Daemons {
		if saved.TaskName == api.BuiltinRouteTaskName {
			command = saved.Command
			break
		}
	}
	if !samePath(command, s.launch.Peer.ImagePath) ||
		computeIntentDesired(api.BuiltinRouteTaskName, api.UnifiedStopsFile(intent, nil).Tasks, time.Now().UTC()) != api.ReconcileIntentDesiredRunning {
		return zero, errors.New("original Route intent is not runnable for the current Product")
	}
	if err := validateMCPFrontRouteDescriptorAtPorts(intent, command, from.Port); err != nil {
		return zero, err
	}
	digest, err := mcpFrontRouteDescriptorDigest(api.BuildBuiltinRouteDaemon(command, port))
	if err != nil {
		return zero, err
	}
	oldDigest, err := mcpFrontRouteDescriptorDigest(api.BuildBuiltinRouteDaemon(command, from.Port))
	if err != nil {
		return zero, err
	}
	binding, op, generation, incumbent, err := c.observeRouteAdmissionIncumbent(ctx, creators, from.Port, phases)
	if err != nil {
		return zero, err
	}
	creators.mu.Lock()
	sameDescriptor := op.routeDescriptorSHA256 == oldDigest
	creators.mu.Unlock()
	if !sameDescriptor {
		return zero, errors.New("incumbent Route creator did not receive the current descriptor")
	}
	selection := api.RouteActivationSelection{FromEpoch: from, ToEpoch: decision.ToEpoch, DescriptorSHA256: digest,
		Supervisor: s.launch.Owner, PriorGeneration: generation}
	receipt := mcpFrontRouteAdmissionReceipt{
		JournalBeforeSHA256: fmt.Sprintf("%x", sha256.Sum256(raw)), SettingsSHA256: settingsDigest,
		IntentSHA256: intentDigest, IncumbentDescriptorSHA256: oldDigest, Incumbent: incumbent,
	}
	if !selection.Valid() || !receipt.valid(selection) {
		return zero, errors.New("original Route admission preimage unavailable")
	}
	return routeAdmissionPreimage{report: report, raw: raw, selection: selection, receipt: receipt, incumbent: op, binding: binding,
		readers: zero.readers}, ctx.Err()
}

const evRouteActivationAdmission api.SMEvent = "route-activation-admission"
const routeAdmissionRequestKey = "route_admission"

type routeAdmissionPhase uint8

const (
	reserveRouteAdmission routeAdmissionPhase = iota
	confirmRouteAdmission
	receiveRouteAdmissionResult
	finishRouteAdmission
)

type routeAdmissionReader struct {
	done chan struct{}
	n    int
	err  error
}

type routeAdmissionTransport struct {
	openFrame, finalFrame *ipcFrameWriteObservation
	firstClose            *aggregateIPCClose
	closeReturned         bool
	closeErr              error
	nativeClose           routeAdmissionNativePipeClose
	reader                *routeAdmissionReader
	readerJoined          bool
	readerJoinErr         error
	readerProtocolErr     error
	projectionErr         error
	cleanProjection       bool
	operationReceipt      *routeAdmissionOperationReceipt
}

type routeAdmissionNativePipeReceipt interface{ NativeResult() (error, bool) }

type routeAdmissionNativePipeClose struct {
	receipt   routeAdmissionNativePipeReceipt
	waitErr   error
	nativeErr error
	observed  bool
}

func receiveRouteAdmissionNativePipeClose(receipt routeAdmissionNativePipeReceipt, waitErr error) (out routeAdmissionNativePipeClose) {
	out.receipt, out.waitErr = receipt, waitErr
	if receipt != nil {
		out.nativeErr, out.observed = receipt.NativeResult()
	}
	return out
}

func (o routeAdmissionNativePipeClose) failure() error {
	if !o.observed {
		return errors.Join(errRouteAdmissionNativePipeCloseUnavailable, o.waitErr, o.nativeErr)
	}
	return errors.Join(o.waitErr, o.nativeErr)
}

func (t *routeAdmissionTransport) resourcesClosed() bool {
	return t != nil && t.firstClose != nil && t.closeReturned && t.closeErr == nil &&
		t.nativeClose.receipt != nil && t.nativeClose.observed && t.nativeClose.failure() == nil &&
		(t.reader == nil || t.readerJoined && t.readerJoinErr == nil)
}

func routeAdmissionFrameError(frame *ipcFrameWriteObservation) error {
	if frame == nil {
		return nil // the original selected no write, e.g. a pre-admit EOF
	}
	err := errors.Join(frame.encodeErr, frame.writeErr)
	if frame.attempted && frame.written != frame.encoded {
		err = errors.Join(err, io.ErrShortWrite)
	}
	return err
}

func routeAdmissionFrameComplete(frame *ipcFrameWriteObservation) bool {
	return frame != nil && frame.attempted && frame.encoded > 0 && frame.written == frame.encoded &&
		frame.encodeErr == nil && frame.writeErr == nil
}

func (t *routeAdmissionTransport) deliveryError() error {
	if t == nil {
		return errors.New("original Route transport disposition unavailable")
	}
	return errors.Join(t.projectionErr, routeAdmissionFrameError(t.openFrame), routeAdmissionFrameError(t.finalFrame),
		t.closeErr, t.nativeClose.failure(), t.readerJoinErr, t.readerProtocolErr)
}

func (t *routeAdmissionTransport) receiveClose(ctx context.Context, conn *aggregateIPCConn) {
	t.firstClose = conn.startClose()
	if awaitRouteAdmissionReceipt(ctx, t.firstClose.done) {
		t.closeReturned, t.closeErr = true, t.firstClose.err
		if t.closeErr == nil {
			receipt, err := observeRouteAdmissionNativePipeClose(ctx, conn.Conn)
			t.nativeClose = receiveRouteAdmissionNativePipeClose(receipt, err)
		}
	} else {
		t.closeErr = errors.Join(ctx.Err(), errors.New("original pipe close receipt pending"))
	}
	if t.reader == nil {
		return
	}
	if !awaitRouteAdmissionReceipt(ctx, t.reader.done) {
		t.readerJoinErr = errors.Join(ctx.Err(), errors.New("original pipe reader join pending"))
		return
	}
	t.readerJoined = true
	t.readerProtocolErr = t.reader.protocolError()
}

func (r *routeAdmissionReader) protocolError() error {
	err := r.err
	if r.n != 0 || err == nil || !(errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) || routeAdmissionPlatformClosedRead(err)) {
		return errors.Join(err, errors.New("original pipe reader did not return a closed stream"))
	}
	return nil
}

func awaitRouteAdmissionReceipt(ctx context.Context, done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
	}
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

type routeAdmissionReply struct {
	accepted bool
	out      routeAdmissionOutcome
	receipt  *routeAdmissionOperationReceipt
}

type routeAdmissionOperationReceipt struct {
	original *routeAdmissionOutcome
	result   routeAdmissionResult
}

type routeAdmissionLoopRequest struct {
	ctx         context.Context
	reservation *routeAdmissionReservation
	phase       routeAdmissionPhase
	reply       chan routeAdmissionReply
}

func (c *supervisorController) handleRouteAdmissionOnLoop(req routeAdmissionLoopRequest) {
	req.reply <- c.routeAdmissionOnLoop(req.ctx, req.reservation, req.phase)
}

func (c *supervisorController) conflictRouteAdmission(task string) {
	if current := c.routeAdmission; current != nil && canonicalSupervisorTaskName(task) == api.BuiltinRouteTaskName {
		current.conflicted.Store(true)
	}
}

func (c *supervisorController) checkRouteAdmissionIntent(updated *api.SupervisorIntentFile, stops *api.DaemonIntentFile) {
	reservation := c.routeAdmission
	if reservation == nil || updated == nil {
		return
	}
	before := reservation.selection.Load()
	if before == nil {
		return
	}
	selected := updated.FindSupervisorDaemonByTaskName(api.BuiltinRouteTaskName)
	if selected == nil {
		c.conflictRouteAdmission(api.BuiltinRouteTaskName)
		return
	}
	digest, err := mcpFrontRouteDescriptorDigest(*selected)
	if err != nil || digest != before.receipt.IncumbentDescriptorSHA256 ||
		stops != nil && computeIntentDesired(api.BuiltinRouteTaskName, stops.Tasks, time.Now().UTC()) != api.ReconcileIntentDesiredRunning {
		c.conflictRouteAdmission(api.BuiltinRouteTaskName)
	}
}

func (r *routeAdmissionReservation) closedNoEffect() bool {
	original := r.outcome.Load()
	return r.closed.Load() && original != nil && original.provedClosedNoEffect && r.transport.Load().resourcesClosed()
}

func (c *supervisorController) routeAdmissionOnLoop(ctx context.Context, reservation *routeAdmissionReservation, phase routeAdmissionPhase) (reply routeAdmissionReply) {
	if phase == receiveRouteAdmissionResult || phase == finishRouteAdmission {
		return c.receiveRouteAdmissionOnLoop(reservation, phase)
	}
	if reservation.closed.Load() || ctx.Err() != nil {
		reply.out.err = errors.Join(errors.New("Route admission pipe closed"), ctx.Err())
		return reply
	}
	switch phase {
	case reserveRouteAdmission:
		// Reconcile the exact stale original only from its own sealed
		// no-effect and physical closure. This grants no delivery credit.
		if prior := c.routeAdmission; prior != nil && prior != reservation && prior.closedNoEffect() {
			prior.finalized = true
			prior.permitted.Store(false)
			c.routeAdmission = nil
		}
		if c.routeAdmission != nil {
			reply.out.err = errAggregateBusy
			return reply
		}
		c.routeAdmission = reservation
		reply.accepted = true
	case confirmRouteAdmission:
		if c.routeAdmission != reservation || reservation.used || reservation.received ||
			reservation.selection.Load() == nil || reservation.conflicted.Load() || !reservation.current() {
			reply.out.err = errors.New("Route admission requires the original live pipe reservation")
			return reply
		}
		reservation.used = true
		reservation.permitted.Store(true)
		reply.accepted = true
	default:
		reply.out.err = errors.New("unknown Route admission controller operation")
	}
	return reply
}

func (c *supervisorController) receiveRouteAdmissionOnLoop(reservation *routeAdmissionReservation, phase routeAdmissionPhase) routeAdmissionReply {
	original := reservation.outcome.Load()
	if original == nil {
		return routeAdmissionReply{out: routeAdmissionOutcome{err: errors.New("original Route admission outcome unavailable")}}
	}
	out := *original
	if c.routeAdmission != reservation || reservation.finalized {
		return routeAdmissionReply{out: out}
	}
	if phase == receiveRouteAdmissionResult {
		if reservation.received {
			return routeAdmissionReply{out: out}
		}
		reservation.received = true
		reservation.permitted.Store(false)
		if reservation.request != nil {
			out = routeAdmissionPendingOnError(out, reservation.wire.requestError(reservation.request))
		}
		if out.result.Phase != "" && (reservation.conflicted.Load() || !reservation.current() || !out.operationResourcesClosed) {
			out = routeAdmissionPendingOnError(out, errAggregatePeerChanged)
		}
		// This receipt acknowledges the operation only. The original
		// reservation still owns the frame, close and reader dispositions.
		reservation.operationReceipt = &routeAdmissionOperationReceipt{original: original, result: out.result}
		return routeAdmissionReply{accepted: true, out: out, receipt: reservation.operationReceipt}
	}

	transport := reservation.transport.Load()
	if transport == nil || !reservation.closed.Load() {
		return routeAdmissionReply{out: routeAdmissionPendingOnError(out, errors.New("original Route transport disposition unavailable"))}
	}
	reservation.finalized = true
	reservation.permitted.Store(false)
	out = routeAdmissionPendingOnError(out, transport.deliveryError())
	if out.result.Phase != "" && (reservation.conflicted.Load() || !reservation.current()) {
		out = routeAdmissionPendingOnError(out, errAggregatePeerChanged)
	}
	first := transport.operationReceipt
	delivered := transport.resourcesClosed() && out.err == nil && out.operationResourcesClosed &&
		out.result.Phase == mcpFrontRouteActivationAdmitted && reservation.received &&
		first != nil && first == reservation.operationReceipt && first.original == original && first.result == out.result &&
		transport.cleanProjection && transport.reader != nil &&
		routeAdmissionFrameComplete(transport.openFrame) && routeAdmissionFrameComplete(transport.finalFrame)
	settledNoEffect := reservation.closedNoEffect() && transport.readerProtocolErr == nil &&
		routeAdmissionFrameError(transport.openFrame) == nil && routeAdmissionFrameError(transport.finalFrame) == nil
	if delivered || settledNoEffect {
		c.routeAdmission = nil
	} else if out.err == nil {
		out = routeAdmissionPendingOnError(out, errors.New("original Route terminal settlement incomplete"))
	}
	return routeAdmissionReply{accepted: true, out: out}
}

func (c *supervisorController) postRouteAdmission(ctx context.Context, reservation *routeAdmissionReservation, phase routeAdmissionPhase) routeAdmissionReply {
	reply := make(chan routeAdmissionReply, 1)
	req := routeAdmissionLoopRequest{ctx: ctx, reservation: reservation, phase: phase, reply: reply}
	var out routeAdmissionOutcome
	if original := reservation.outcome.Load(); original != nil {
		out = *original
	}
	if err := ctx.Err(); err != nil {
		return routeAdmissionReply{out: routeAdmissionPendingOnError(out, err)}
	}
	if err := c.eventLoop.PostCtx(ctx, api.LoopEvent{Kind: evRouteActivationAdmission, Body: map[string]any{routeAdmissionRequestKey: req}}); err != nil {
		return routeAdmissionReply{out: routeAdmissionPendingOnError(out, err)}
	}
	select {
	case result := <-reply:
		return result
	case <-ctx.Done():
		return routeAdmissionReply{out: routeAdmissionPendingOnError(out, ctx.Err())}
	}
}

func serveRouteActivationAdmission(ctx context.Context, conn net.Conn, req api.IPCRequest, deps ipcDispatchDeps) error {
	if deps.controllerProvider == nil || deps.reconcileReady == nil || !deps.reconcileReady.Load() {
		return writeAggregateFailure(conn, req.ID, "ROUTE_ADMISSION_UNAVAILABLE", "original Route controller unavailable")
	}
	c := deps.controllerProvider()
	original, owned := conn.(*aggregateIPCConn)
	if c == nil || c.eventLoop == nil || !owned {
		return writeAggregateFailure(conn, req.ID, "ROUTE_ADMISSION_UNAVAILABLE", "original Route controller unavailable")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	creators := deps.daemonCreators
	reservation := &routeAdmissionReservation{request: ctx}
	reservation.current = func() bool {
		before := reservation.selection.Load()
		return before != nil && c.sameRouteAdmissionIncumbent(creators, *before)
	}
	transport := &routeAdmissionTransport{}
	open := c.postRouteAdmission(ctx, reservation, reserveRouteAdmission)
	out, sendFinal := open.out, true
	if open.accepted && out.err == nil {
		out, sendFinal = c.runRouteAdmissionPipe(ctx, cancel, original, req.ID, creators, reservation, transport)
	}
	if out.operator != nil {
		out.operatorClose = out.operator.close()
	}
	operation := out.sealForFIFO(errors.Join(out.operatorClose.callerErr, out.operatorClose.productErr))
	reservation.outcome.Store(&operation)
	resultCtx, stopResult := context.WithTimeout(c.ctx, api.SupervisorAggregateStatusTimeout)
	result := c.postRouteAdmission(resultCtx, reservation, receiveRouteAdmissionResult)
	stopResult()
	transport.operationReceipt = result.receipt

	if sendFinal {
		projection := reservation.wire.project(ctx, result)
		if projection.err == nil && projection.result.Phase != mcpFrontRouteActivationAdmitted {
			projection = routeAdmissionPendingOnError(projection, errors.New("durable original Route admission unavailable"))
		}
		transport.projectionErr = projection.err
		transport.cleanProjection = projection.err == nil
		frame := api.IPCResponse{ID: req.ID, OK: transport.cleanProjection, Final: true}
		if projection.result.Phase != "" {
			frame.Result = projection.result
		}
		if !frame.OK {
			frame.Error = &api.IPCErr{Code: "ROUTE_ADMISSION_PENDING", Message: "original Route admission unconfirmed"}
		}
		written := writeIPCFrameObserved(original, frame)
		transport.finalFrame = &written
	} else {
		transport.projectionErr = result.out.err
	}

	terminalCtx, stopTerminal := context.WithTimeout(c.ctx, api.SupervisorAggregateStatusTimeout)
	defer stopTerminal()
	transport.receiveClose(terminalCtx, original)
	reservation.transport.Store(transport)
	reservation.closed.Store(true)
	terminal := c.postRouteAdmission(terminalCtx, reservation, finishRouteAdmission)
	return errors.Join(operation.err, result.out.err, terminal.out.err, transport.deliveryError())
}

func (c *supervisorController) runRouteAdmissionPipe(ctx context.Context, cancel context.CancelFunc, conn *aggregateIPCConn, id int64, creators *daemonCreatorRecords,
	reservation *routeAdmissionReservation, transport *routeAdmissionTransport) (out routeAdmissionOutcome, sendFinal bool) {
	var phases routeAdmissionNativeObservations
	defer func() { out.native = phases }()
	operator, err := holdRouteAdmissionOperator(ctx, conn, creators.routeAggregate, &phases)
	out.operator = operator
	if err != nil {
		out.err = err
		return out, true
	}
	reservation.check = operator.check
	reservation.ops = c.routeAdmissionOps(creators, &phases)
	before, err := reservation.ops.observe(ctx)
	out.preflight = &before
	if err != nil {
		out.err = err
		return out, true
	}
	reservation.before = before
	reservation.selection.Store(&before)

	var nonce [api.RouteAggregateChallengeBytes]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		out.err = err
		return out, true
	}
	challenge := hex.EncodeToString(nonce[:])
	open := writeIPCFrameObserved(conn, api.IPCResponse{ID: id, OK: true,
		Result: api.RouteActivationAdmissionChallenge{Challenge: challenge, Selection: before.selection}})
	transport.openFrame = &open
	if err := routeAdmissionFrameError(&open); err != nil {
		out.err = err
		return out, false // never append another frame to an incomplete offer
	}
	if err := api.ReadRouteActivationAdmissionConfirm(conn, id, challenge); err != nil {
		out.err = err
		return out, false
	}

	reader := &routeAdmissionReader{done: make(chan struct{})}
	transport.reader = reader
	go func() {
		defer close(reader.done)
		var extra [1]byte
		reader.n, reader.err = conn.Read(extra[:])
		reservation.wire.readerEnded(reader.err, cancel)
	}()
	confirmed := c.postRouteAdmission(ctx, reservation, confirmRouteAdmission)
	if !confirmed.accepted || confirmed.out.err != nil {
		out.err = errors.Join(confirmed.out.err, errors.New("original Route admission confirmation unavailable"))
		return out, true
	}
	out = admitRouteActivation(ctx, reservation)
	out.operator, out.preflight = operator, &before
	return out, true
}
