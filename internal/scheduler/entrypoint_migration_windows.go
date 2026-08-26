//go:build windows

package scheduler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os/user"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const ownedEntrypointTaskPrefix = "mcp-local-hub-"

// OwnedEntrypointTaskTxn serializes the lossless replacement of the command
// receiver in every current-user mcp-local-hub Task Scheduler task. It owns the
// lock from inventory through forward verification or symmetric restoration.
// Callers must Close it after a successful forward verification or restore.
type OwnedEntrypointTaskTxn struct {
	backend ownedEntrypointTaskBackend
	store   RetainedTaskXMLStore
	ctx     context.Context
	owner   string
	prior   string
	runtime string
	lock    *flock.Flock

	inventory map[string]ownedEntrypointTaskSnapshot
	closed    bool
}

type ownedEntrypointTaskBackend interface {
	List(prefix string) ([]TaskStatus, error)
	ExportXML(name string) ([]byte, error)
	ImportXML(name string, xml []byte) error
}

type ownedEntrypointTaskSnapshot struct {
	name   string
	xml    []byte
	backup OwnedEntrypointTaskBackup
}

// RetainedTaskXMLStore is the secure, owner-DACL artifact boundary. Refs are
// opaque to Scheduler and are the only values suitable for JournalV1.Backups.
type RetainedTaskXMLStore interface {
	Retain(ctx context.Context, xml []byte) (opaqueRef string, err error)
	Load(ctx context.Context, opaqueRef string) ([]byte, error)
}

// OwnedEntrypointTaskBackup is journal-safe task recovery metadata. It never
// carries XML bytes, arguments, paths, or other task content.
type OwnedEntrypointTaskBackup struct {
	TaskName string
	Ref      string
	SHA256   string
}

// OwnedEntrypointTaskReference is the read-only inventory record for a task
// whose executable remains under mcphub's managed entrypoint pair.  It carries
// no XML or arguments, so callers cannot use the scan as a task mutation path.
type OwnedEntrypointTaskReference struct {
	TaskName string
	Command  string
}

// ScanOwnedEntrypointTaskReferences returns only current-user hub task
// commands that equal either member of the admitted CLI/runtime pair.  Other
// current-user tasks using the hub name prefix but an operator executable are
// explicitly outside this transaction and are not returned or changed.
func ScanOwnedEntrypointTaskReferences(ctx context.Context, operatorCLI, runtime string) ([]OwnedEntrypointTaskReference, error) {
	backend, err := New()
	if err != nil {
		return nil, fmt.Errorf("entrypoint task scan: scheduler: %w", err)
	}
	owner, err := currentWindowsTaskOwner()
	if err != nil {
		return nil, err
	}
	return scanOwnedEntrypointTaskReferences(ctx, backend, owner, operatorCLI, runtime)
}

func scanOwnedEntrypointTaskReferences(ctx context.Context, backend ownedEntrypointTaskBackend, owner, operatorCLI, runtime string) ([]OwnedEntrypointTaskReference, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if backend == nil || strings.TrimSpace(owner) == "" || strings.TrimSpace(operatorCLI) == "" || strings.TrimSpace(runtime) == "" || operatorCLI == runtime {
		return nil, errors.New("entrypoint task scan: invalid admitted pair")
	}
	statuses, err := backend.List(ownedEntrypointTaskPrefix)
	if err != nil {
		return nil, fmt.Errorf("entrypoint task scan: list: %w", err)
	}
	sort.Slice(statuses, func(i, j int) bool {
		return canonicalOwnedEntrypointTaskName(statuses[i].Name) < canonicalOwnedEntrypointTaskName(statuses[j].Name)
	})
	refs := make([]OwnedEntrypointTaskReference, 0, len(statuses))
	seen := make(map[string]struct{}, len(statuses))
	for _, status := range statuses {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := canonicalOwnedEntrypointTaskName(status.Name)
		if !strings.HasPrefix(name, `\mcp-local-hub-`) || !sameWindowsUser(status.Owner, owner) {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("entrypoint task scan: duplicate task %q", name)
		}
		seen[name] = struct{}{}
		raw, err := backend.ExportXML(status.Name)
		if err != nil {
			return nil, fmt.Errorf("entrypoint task scan: export %q: %w", name, err)
		}
		shape, err := parseOwnedEntrypointTaskXML(raw)
		if err != nil || shape.uri != name {
			return nil, fmt.Errorf("entrypoint task scan: invalid owned task %q", name)
		}
		if shape.command == operatorCLI || shape.command == runtime {
			refs = append(refs, OwnedEntrypointTaskReference{TaskName: name, Command: shape.command})
		}
	}
	return refs, nil
}

// BeginOwnedEntrypointTaskTxn acquires the one lock used for the complete
// current-user task migration. A secure retained-artifact store is mandatory:
// no production transaction can retain XML only in memory.
func BeginOwnedEntrypointTaskTxn(ctx context.Context, lockPath, priorCLI, runtime string, store RetainedTaskXMLStore) (*OwnedEntrypointTaskTxn, error) {
	backend, err := New()
	if err != nil {
		return nil, fmt.Errorf("entrypoint task scheduler: %w", err)
	}
	owner, err := currentWindowsTaskOwner()
	if err != nil {
		return nil, err
	}
	return beginOwnedEntrypointTaskTxnWithStore(ctx, lockPath, priorCLI, runtime, backend, owner, store)
}

// BeginOwnedEntrypointTaskTxnWithStore is the durable production entrypoint.
// The caller supplies the secure retained-artifact owner; Scheduler retains and
// reloads only opaque references and SHA-256 digests.
func BeginOwnedEntrypointTaskTxnWithStore(ctx context.Context, lockPath, priorCLI, runtime string, store RetainedTaskXMLStore) (*OwnedEntrypointTaskTxn, error) {
	return BeginOwnedEntrypointTaskTxn(ctx, lockPath, priorCLI, runtime, store)
}

func beginOwnedEntrypointTaskTxnWith(ctx context.Context, lockPath, priorCLI, runtime string, backend ownedEntrypointTaskBackend, owner string) (*OwnedEntrypointTaskTxn, error) {
	if backend == nil {
		return nil, errors.New("entrypoint task transaction: scheduler backend is required")
	}
	if strings.TrimSpace(lockPath) == "" {
		return nil, errors.New("entrypoint task transaction: lock path is required")
	}
	if priorCLI == "" || runtime == "" || priorCLI == runtime {
		return nil, errors.New("entrypoint task transaction: distinct prior and runtime paths are required")
	}
	if strings.TrimSpace(owner) == "" {
		return nil, errors.New("entrypoint task transaction: current owner is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	lock := flock.New(lockPath)
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("entrypoint task transaction lock: %w", err)
	}
	if !locked {
		_ = lock.Close()
		return nil, fmt.Errorf("entrypoint task transaction lock is held: %s", lockPath)
	}
	return &OwnedEntrypointTaskTxn{
		backend: backend,
		ctx:     ctx,
		owner:   owner,
		prior:   priorCLI,
		runtime: runtime,
		lock:    lock,
	}, nil
}

func beginOwnedEntrypointTaskTxnWithStore(ctx context.Context, lockPath, priorCLI, runtime string, backend ownedEntrypointTaskBackend, owner string, store RetainedTaskXMLStore) (*OwnedEntrypointTaskTxn, error) {
	if store == nil {
		return nil, errors.New("entrypoint task transaction: retained XML store is required")
	}
	txn, err := beginOwnedEntrypointTaskTxnWith(ctx, lockPath, priorCLI, runtime, backend, owner)
	if err != nil {
		return nil, err
	}
	txn.store = store
	return txn, nil
}

// ReopenOwnedEntrypointTaskTxnWithStore reacquires the same lock after a crash,
// validates retained XML bodies against their journal metadata, and returns a
// transaction that may only restore through the ordinary Import/readback path.
func ReopenOwnedEntrypointTaskTxnWithStore(ctx context.Context, lockPath, priorCLI, runtime string, store RetainedTaskXMLStore, backups []OwnedEntrypointTaskBackup) (*OwnedEntrypointTaskTxn, error) {
	backend, err := New()
	if err != nil {
		return nil, fmt.Errorf("entrypoint task scheduler: %w", err)
	}
	owner, err := currentWindowsTaskOwner()
	if err != nil {
		return nil, err
	}
	return reopenOwnedEntrypointTaskTxnWithStore(ctx, lockPath, priorCLI, runtime, backend, owner, store, backups)
}

func reopenOwnedEntrypointTaskTxnWithStore(ctx context.Context, lockPath, priorCLI, runtime string, backend ownedEntrypointTaskBackend, owner string, store RetainedTaskXMLStore, backups []OwnedEntrypointTaskBackup) (*OwnedEntrypointTaskTxn, error) {
	txn, err := beginOwnedEntrypointTaskTxnWithStore(ctx, lockPath, priorCLI, runtime, backend, owner, store)
	if err != nil {
		return nil, err
	}
	inventory := make(map[string]ownedEntrypointTaskSnapshot, len(backups))
	for _, backup := range backups {
		name := canonicalOwnedEntrypointTaskName(backup.TaskName)
		if !strings.HasPrefix(name, `\mcp-local-hub-`) || backup.Ref == "" || backup.SHA256 == "" {
			_ = txn.Close()
			return nil, errors.New("entrypoint task reopen: invalid backup metadata")
		}
		if _, exists := inventory[name]; exists {
			_ = txn.Close()
			return nil, fmt.Errorf("entrypoint task reopen: duplicate backup %q", name)
		}
		raw, loadErr := store.Load(txn.ctx, backup.Ref)
		if loadErr != nil || sha256Hex(raw) != backup.SHA256 {
			_ = txn.Close()
			return nil, fmt.Errorf("entrypoint task reopen: retained XML verification failed for %q", name)
		}
		shape, parseErr := parseOwnedEntrypointTaskXML(raw)
		if parseErr != nil || shape.uri != name || shape.command != priorCLI {
			_ = txn.Close()
			return nil, fmt.Errorf("entrypoint task reopen: retained XML is invalid for %q", name)
		}
		inventory[name] = ownedEntrypointTaskSnapshot{name: backup.TaskName, xml: append([]byte(nil), raw...), backup: backup}
	}
	txn.inventory = inventory
	return txn, nil
}

func currentWindowsTaskOwner() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("entrypoint task current user: %w", err)
	}
	name := strings.TrimSpace(u.Username)
	if i := strings.LastIndex(name, `\`); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		return "", errors.New("entrypoint task current user is empty")
	}
	return name, nil
}

// InventoryExport snapshots every owned task before mutation. It refuses a
// duplicate, foreign, unreadable, malformed, or non-prior command receiver.
func (t *OwnedEntrypointTaskTxn) InventoryExport() error {
	if err := t.requireOpen(); err != nil {
		return err
	}
	if t.inventory != nil {
		return errors.New("entrypoint task transaction: inventory already captured")
	}
	statuses, err := t.backend.List(ownedEntrypointTaskPrefix)
	if err != nil {
		return fmt.Errorf("entrypoint task inventory: %w", err)
	}
	sort.Slice(statuses, func(i, j int) bool {
		return canonicalOwnedEntrypointTaskName(statuses[i].Name) < canonicalOwnedEntrypointTaskName(statuses[j].Name)
	})

	inventory := make(map[string]ownedEntrypointTaskSnapshot, len(statuses))
	for _, status := range statuses {
		name := canonicalOwnedEntrypointTaskName(status.Name)
		if !strings.HasPrefix(name, `\mcp-local-hub-`) {
			return fmt.Errorf("entrypoint task inventory: non-hub task %q", name)
		}
		if _, exists := inventory[name]; exists {
			return fmt.Errorf("entrypoint task inventory: duplicate task %q", name)
		}
		if !sameWindowsUser(status.Owner, t.owner) {
			return fmt.Errorf("entrypoint task inventory: foreign owner for %q", name)
		}
		raw, err := t.backend.ExportXML(status.Name)
		if err != nil {
			return fmt.Errorf("entrypoint task inventory export %q: %w", name, err)
		}
		shape, err := parseOwnedEntrypointTaskXML(raw)
		if err != nil {
			return fmt.Errorf("entrypoint task inventory parse %q: %w", name, err)
		}
		if shape.uri != name {
			return fmt.Errorf("entrypoint task inventory: XML URI mismatch for %q", name)
		}
		if shape.command != t.prior {
			return fmt.Errorf("entrypoint task inventory: command is not exact prior CLI path for %q", name)
		}
		snapshot := ownedEntrypointTaskSnapshot{name: status.Name, xml: append([]byte(nil), raw...)}
		if t.store != nil {
			ref, retainErr := t.store.Retain(t.ctx, snapshot.xml)
			if retainErr != nil || strings.TrimSpace(ref) == "" {
				return fmt.Errorf("entrypoint task inventory retain %q: retained XML unavailable", name)
			}
			snapshot.backup = OwnedEntrypointTaskBackup{TaskName: status.Name, Ref: ref, SHA256: sha256Hex(snapshot.xml)}
		}
		inventory[name] = snapshot
	}
	t.inventory = inventory
	return nil
}

// Backups returns the deterministic opaque refs/hashes that the caller may
// store in JournalV1. Nil is returned for the non-durable test seam.
func (t *OwnedEntrypointTaskTxn) Backups() []OwnedEntrypointTaskBackup {
	if t == nil || t.inventory == nil {
		return nil
	}
	backups := make([]OwnedEntrypointTaskBackup, 0, len(t.inventory))
	for _, name := range t.sortedInventoryNames() {
		backup := t.inventory[name].backup
		if backup.Ref != "" {
			backups = append(backups, backup)
		}
	}
	return backups
}

// RewriteCommand updates only the text inside the one Exec Command XML node.
// A failed write can have partially applied, so it returns a typed error and
// leaves the captured state untouched for the bundle coordinator to restore.
func (t *OwnedEntrypointTaskTxn) RewriteCommand() error {
	if err := t.requireInventory(); err != nil {
		return err
	}
	for _, key := range t.sortedInventoryNames() {
		snapshot := t.inventory[key]
		rewritten, err := rewriteOwnedEntrypointTaskCommand(snapshot.xml, t.prior, t.runtime)
		if err != nil {
			return &EntrypointTaskPartialProgressError{Cause: fmt.Errorf("entrypoint task rewrite %q: %w", key, err)}
		}
		if err := t.backend.ImportXML(snapshot.name, rewritten); err != nil {
			return &EntrypointTaskPartialProgressError{Cause: fmt.Errorf("entrypoint task import %q: %w", key, err)}
		}
	}
	return nil
}

// VerifyRuntime requires the exact task set, the runtime command in each task,
// and byte-equivalence outside that Command node. A failed verification leaves
// restoration to the bundle coordinator that owns the transaction settlement.
func (t *OwnedEntrypointTaskTxn) VerifyRuntime() error {
	if err := t.requireInventory(); err != nil {
		return err
	}
	if err := t.verify(func(shape ownedEntrypointTaskShape, snapshot ownedEntrypointTaskSnapshot, raw []byte) error {
		if shape.command != t.runtime {
			return errors.New("runtime command mismatch")
		}
		expected, err := rewriteOwnedEntrypointTaskCommand(snapshot.xml, t.prior, t.runtime)
		if err != nil {
			return err
		}
		if !bytes.Equal(raw, expected) {
			return errors.New("XML differs outside the Command node")
		}
		return nil
	}); err != nil {
		return &EntrypointTaskPartialProgressError{Cause: fmt.Errorf("entrypoint task runtime verification: %w", err)}
	}
	return nil
}

// RestoreImport imports every captured raw XML body, then performs the
// symmetric set/readback check. It deliberately continues after an import
// failure so a later task is never abandoned without a restore attempt.
func (t *OwnedEntrypointTaskTxn) RestoreImport() error {
	if err := t.requireInventory(); err != nil {
		return err
	}
	var errs []error
	for _, key := range t.sortedInventoryNames() {
		snapshot := t.inventory[key]
		if err := t.backend.ImportXML(snapshot.name, snapshot.xml); err != nil {
			errs = append(errs, fmt.Errorf("entrypoint task restore import %q: %w", key, err))
		}
	}
	if err := t.verify(func(_ ownedEntrypointTaskShape, snapshot ownedEntrypointTaskSnapshot, raw []byte) error {
		if !bytes.Equal(raw, snapshot.xml) {
			return errors.New("restored XML is not byte-exact")
		}
		return nil
	}); err != nil {
		errs = append(errs, fmt.Errorf("entrypoint task restore verification: %w", err))
	}
	return errors.Join(errs...)
}

// Close releases the cross-process lock. It is idempotent and must be called
// only after the caller has reached a verified forward or restore terminal path.
func (t *OwnedEntrypointTaskTxn) Close() error {
	if t == nil || t.closed {
		return nil
	}
	t.closed = true
	if err := t.lock.Close(); err != nil {
		return fmt.Errorf("entrypoint task transaction unlock: %w", err)
	}
	return nil
}

// EntrypointTaskPartialProgressError tells the transaction coordinator that a
// receiver write may already have changed one or more owned task rows. The
// owner deliberately performs no compensation itself: exactly one coordinator
// issues the guarded restore directive and records its receipt.
type EntrypointTaskPartialProgressError struct{ Cause error }

func (e *EntrypointTaskPartialProgressError) Error() string {
	if e == nil || e.Cause == nil {
		return "entrypoint task transaction partially progressed"
	}
	return e.Cause.Error()
}

func (e *EntrypointTaskPartialProgressError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (t *OwnedEntrypointTaskTxn) verify(check func(ownedEntrypointTaskShape, ownedEntrypointTaskSnapshot, []byte) error) error {
	statuses, err := t.backend.List(ownedEntrypointTaskPrefix)
	if err != nil {
		return fmt.Errorf("list owned tasks: %w", err)
	}
	live := make(map[string]TaskStatus, len(statuses))
	for _, status := range statuses {
		name := canonicalOwnedEntrypointTaskName(status.Name)
		if !strings.HasPrefix(name, `\mcp-local-hub-`) {
			return fmt.Errorf("non-hub task %q", name)
		}
		if _, exists := live[name]; exists {
			return fmt.Errorf("duplicate task %q", name)
		}
		if !sameWindowsUser(status.Owner, t.owner) {
			return fmt.Errorf("foreign owner for %q", name)
		}
		live[name] = status
	}
	if len(live) != len(t.inventory) {
		return errors.New("task set changed")
	}
	for _, key := range t.sortedInventoryNames() {
		snapshot := t.inventory[key]
		status, exists := live[key]
		if !exists {
			return fmt.Errorf("task %q is missing", key)
		}
		raw, err := t.backend.ExportXML(status.Name)
		if err != nil {
			return fmt.Errorf("export %q: %w", key, err)
		}
		shape, err := parseOwnedEntrypointTaskXML(raw)
		if err != nil {
			return fmt.Errorf("parse %q: %w", key, err)
		}
		if shape.uri != key {
			return fmt.Errorf("XML URI mismatch for %q", key)
		}
		if err := check(shape, snapshot, raw); err != nil {
			return fmt.Errorf("task %q: %w", key, err)
		}
	}
	return nil
}

func (t *OwnedEntrypointTaskTxn) sortedInventoryNames() []string {
	names := make([]string, 0, len(t.inventory))
	for name := range t.inventory {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (t *OwnedEntrypointTaskTxn) requireOpen() error {
	if t == nil || t.closed {
		return errors.New("entrypoint task transaction is closed")
	}
	return nil
}

func (t *OwnedEntrypointTaskTxn) requireInventory() error {
	if err := t.requireOpen(); err != nil {
		return err
	}
	if t.inventory == nil {
		return errors.New("entrypoint task transaction inventory is required")
	}
	return nil
}

func canonicalOwnedEntrypointTaskName(name string) string {
	name = strings.TrimSpace(name)
	if !strings.HasPrefix(name, `\`) {
		name = `\` + name
	}
	return name
}

type ownedEntrypointTaskShape struct {
	uri          string
	command      string
	commandStart int
	commandEnd   int
}

func parseOwnedEntrypointTaskXML(raw []byte) (ownedEntrypointTaskShape, error) {
	body, base, err := taskXMLBody(raw)
	if err != nil {
		return ownedEntrypointTaskShape{}, err
	}
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.Strict = true
	dec.Entity = nil
	dec.CharsetReader = nil

	var (
		stack                    []xml.Name
		actionsElements          int
		actionCount              int
		execCount                int
		commandCount             int
		uriCount                 int
		uriActive, commandActive bool
		commandNested            bool
		uri, command             strings.Builder
		shape                    ownedEntrypointTaskShape
	)
	for {
		token, tokenErr := dec.Token()
		if tokenErr == nil {
			switch value := token.(type) {
			case xml.StartElement:
				parent := xml.Name{}
				if len(stack) > 0 {
					parent = stack[len(stack)-1]
				}
				if parent.Local == "Task" && value.Name.Local == "Actions" {
					actionsElements++
				}
				if parent.Local == "Actions" {
					actionCount++
					if value.Name.Local == "Exec" {
						execCount++
					}
				}
				if parent.Local == "RegistrationInfo" && value.Name.Local == "URI" {
					uriCount++
					uriActive = true
				}
				if parent.Local == "Exec" && value.Name.Local == "Command" {
					commandCount++
					commandActive = true
					shape.commandStart = base + int(dec.InputOffset())
				} else if commandActive {
					commandNested = true
				}
				stack = append(stack, value.Name)
			case xml.CharData:
				if uriActive {
					uri.Write([]byte(value))
				}
				if commandActive {
					command.Write([]byte(value))
					shape.commandEnd = base + int(dec.InputOffset())
				}
			case xml.EndElement:
				if value.Name.Local == "URI" && uriActive {
					uriActive = false
				}
				if value.Name.Local == "Command" && commandActive {
					commandActive = false
				}
				if len(stack) == 0 || stack[len(stack)-1] != value.Name {
					return ownedEntrypointTaskShape{}, errors.New("malformed XML nesting")
				}
				stack = stack[:len(stack)-1]
			}
		} else if errors.Is(tokenErr, io.EOF) {
			break
		} else {
			return ownedEntrypointTaskShape{}, fmt.Errorf("invalid XML: %w", tokenErr)
		}
	}
	if len(stack) != 0 {
		return ownedEntrypointTaskShape{}, errors.New("malformed XML nesting")
	}
	if actionsElements != 1 || actionCount != 1 || execCount != 1 {
		return ownedEntrypointTaskShape{}, errors.New("unknown action count")
	}
	if uriCount != 1 || strings.TrimSpace(uri.String()) == "" {
		return ownedEntrypointTaskShape{}, errors.New("missing task URI")
	}
	if commandCount != 1 || commandNested || shape.commandStart >= shape.commandEnd {
		return ownedEntrypointTaskShape{}, errors.New("missing or ambiguous Exec Command")
	}
	shape.uri = canonicalOwnedEntrypointTaskName(uri.String())
	shape.command = command.String()
	return shape, nil
}

func rewriteOwnedEntrypointTaskCommand(raw []byte, prior, runtime string) ([]byte, error) {
	shape, err := parseOwnedEntrypointTaskXML(raw)
	if err != nil {
		return nil, err
	}
	if shape.command != prior {
		return nil, errors.New("command is not exact prior CLI path")
	}
	var escaped bytes.Buffer
	if err := xml.EscapeText(&escaped, []byte(runtime)); err != nil {
		return nil, fmt.Errorf("escape runtime command: %w", err)
	}
	out := make([]byte, 0, len(raw)-shape.commandEnd+shape.commandStart+escaped.Len())
	out = append(out, raw[:shape.commandStart]...)
	out = append(out, escaped.Bytes()...)
	out = append(out, raw[shape.commandEnd:]...)
	return out, nil
}

func taskXMLBody(raw []byte) ([]byte, int, error) {
	if len(raw) >= 2 && ((raw[0] == 0xff && raw[1] == 0xfe) || (raw[0] == 0xfe && raw[1] == 0xff)) {
		return nil, 0, errors.New("UTF-16 task XML bytes are unsupported")
	}
	start := 0
	if len(raw) >= 3 && bytes.Equal(raw[:3], []byte{0xef, 0xbb, 0xbf}) {
		start = 3
	}
	for start < len(raw) && (raw[start] == ' ' || raw[start] == '\t' || raw[start] == '\r' || raw[start] == '\n') {
		start++
	}
	if bytes.HasPrefix(raw[start:], []byte("<?xml")) {
		end := bytes.Index(raw[start:], []byte("?>"))
		if end < 0 {
			return nil, 0, errors.New("unterminated XML declaration")
		}
		start += end + len("?>")
	}
	if len(bytes.TrimSpace(raw[start:])) == 0 {
		return nil, 0, errors.New("empty task XML")
	}
	return raw[start:], start, nil
}

func sha256Hex(body []byte) string {
	digest := sha256.Sum256(body)
	return fmt.Sprintf("%x", digest[:])
}
