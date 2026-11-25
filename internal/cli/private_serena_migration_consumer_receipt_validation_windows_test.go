//go:build windows

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/daemon"
	"mcp-local-hub/internal/process"
)

// This is synthetic DATA for parser/predicate tests. It contains no original
// native, protocol or IO observation and cannot qualify a live consumer.
func privateSerenaConsumerPredicateData(t *testing.T) (*privateSerenaNativeConsumerReceipt, privateSerenaMigrationInput) {
	t.Helper()
	input := privateSerenaMigrationInput{SourceEpoch: strings.Repeat("a", 64), SourceCommit: strings.Repeat("1", 40), Python: privateSerenaMigrationArtifact{SHA256: strings.Repeat("b", 64)}}
	ready := api.PreparedSerenaRuntimeBinding{Schema: api.PreparedSerenaBindingSchema, RequestIdentitySHA256: strings.Repeat("c", 64), GenerationID: "generation", RecordSHA256: strings.Repeat("d", 64), SourceSHA256: input.SourceEpoch, InterpreterSHA256: input.Python.SHA256, EntrypointSHA256: strings.Repeat("e", 64), ResolutionSHA256: strings.Repeat("f", 64)}
	id := process.ContainedRootIdentity{Known: true, PID: 4242, StartedAt: "2026-11-23T00:00:00Z", NativeBirthFILETIME: 123456789}
	identityBytes, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	identitySHA := privateSerenaMigrationDigest(identityBytes)
	confirmed := api.PreparedConsumerBindingV1{OperationID: "consumer", TaskName: "serena-task", ConsumerKind: "workspace", PackageRequestIdentitySHA256: ready.RequestIdentitySHA256, GenerationID: ready.GenerationID, ReadyRecordSHA256: ready.RecordSHA256, SourceSHA256: ready.SourceSHA256, EntrypointSHA256: ready.EntrypointSHA256, DescriptorSHA256: strings.Repeat("2", 64), Disposition: "current", StartDisposition: "confirmed", NativeBinding: &api.PreparedConsumerNativeBindingV1{CreatorOperationID: "consumer", PID: uint32(id.PID), NativeBirthFILETIME: id.NativeBirthFILETIME, NativeIdentitySHA256: identitySHA}}
	terminal := confirmed
	nativeCopy := *confirmed.NativeBinding
	terminal.NativeBinding = &nativeCopy
	terminal.StartDisposition = "terminal"
	closure := strings.Repeat("3", 64)
	terminal.ClosureReceiptSHA256 = &closure
	slot := process.ContainedStreamSlot{Acquired: true, CloseAttempted: true, Closed: true}
	stream := process.ContainedIOStream{WorkerStarted: true, WorkerComplete: true, EOF: true, Bytes: 17}
	streams := process.ContainedIOSettlement{Stdin: slot, StdoutRead: slot, StdoutWrite: slot, StderrRead: slot, StderrWrite: slot, Stdout: stream, Stderr: stream}
	ioBytes, err := json.Marshal(struct {
		Schema                    string
		IO                        process.ContainedIOSettlement
		LogClosed, RequestsJoined bool
	}{"http-host-original-io-v1", streams, true, true})
	if err != nil {
		t.Fatal(err)
	}
	return &privateSerenaNativeConsumerReceipt{
		Schema: privateSerenaNativeConsumerSchema, Ready: ready, Confirmed: confirmed, Terminal: terminal,
		Protocol: privateSerenaNativeConsumerProtocol{QualificationID: "qualification", HostStartSHA256: identitySHA, EndpointSHA256: strings.Repeat("4", 64), ReceiptSHA256: strings.Repeat("5", 64)},
		Native:   process.ContainedNativeSnapshot{RootIdentity: id, RootWaitKnown: true, RootExitCode: 17, TerminalAction: "request-succeeded", MembersExited: true, AccountingKnown: true, InitialTotal: 1, FinalActive: 0, CapturedMemberPIDs: []uint32{uint32(id.PID)}, HandlesClosed: true},
		IO:       streams, IOReceiptSHA256: privateSerenaMigrationDigest(ioBytes),
		Order:    daemon.HTTPHostQualificationEventOrder{ProtocolReceived: 1, OwnedActionStarted: 2, OwnedActionCompleted: 3, ResourcesClosed: 4, OwnedActionReceiptSHA256: strings.Repeat("6", 64)},
		Complete: true,
	}, input
}

func TestPrivateSerenaNativeConsumerDataPredicates(t *testing.T) {
	for _, exit := range []int{0, 17} {
		r, input := privateSerenaConsumerPredicateData(t)
		r.Native.RootExitCode = exit
		if err := validatePrivateSerenaNativeConsumer(r, input); err != nil {
			t.Fatalf("internally consistent owned-stop DATA with exit %d: %v", exit, err)
		}
	}
	tests := []struct {
		name string
		edit func(*privateSerenaNativeConsumerReceipt, *privateSerenaMigrationInput)
	}{
		{"incomplete", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) { r.Complete = false }},
		{"old_schema", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Schema = "private-serena-native-consumer-v0"
		}},
		{"ready_only", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			*r = privateSerenaNativeConsumerReceipt{Schema: r.Schema, Ready: r.Ready, Complete: true}
		}},
		{"missing_protocol", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Protocol.ReceiptSHA256 = ""
		}},
		{"malformed_digest", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Protocol.EndpointSHA256 = strings.Repeat("G", 64)
		}},
		{"foreign_source", func(r *privateSerenaNativeConsumerReceipt, input *privateSerenaMigrationInput) {
			input.SourceEpoch = strings.Repeat("7", 64)
		}},
		{"commit_is_not_epoch", func(r *privateSerenaNativeConsumerReceipt, input *privateSerenaMigrationInput) {
			input.SourceEpoch = input.SourceCommit
		}},
		{"foreign_interpreter", func(r *privateSerenaNativeConsumerReceipt, input *privateSerenaMigrationInput) {
			input.Python.SHA256 = strings.Repeat("7", 64)
		}},
		{"pending_start", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Confirmed.Disposition = "pending"
		}},
		{"unconfirmed_start", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Confirmed.StartDisposition = "attempted_unknown"
		}},
		{"premature_closure", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Confirmed.ClosureReceiptSHA256 = r.Terminal.ClosureReceiptSHA256
		}},
		{"missing_terminal", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Terminal.StartDisposition = "confirmed"
		}},
		{"missing_closure", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Terminal.ClosureReceiptSHA256 = nil
		}},
		{"foreign_terminal_task", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Terminal.TaskName = "another-task"
		}},
		{"foreign_ready", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Ready.GenerationID = "another-generation"
		}},
		{"no_child", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Native.NoChildKnown = true
		}},
		{"foreign_root_pid", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Native.RootIdentity.PID++
		}},
		{"foreign_root_birth", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Native.RootIdentity.NativeBirthFILETIME++
		}},
		{"foreign_root_fingerprint", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Native.RootIdentity.StartedAt = "2026-11-23T00:00:01Z"
		}},
		{"foreign_protocol_host", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Protocol.HostStartSHA256 = strings.Repeat("7", 64)
		}},
		{"unknown_wait", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Native.RootWaitKnown = false
		}},
		{"live_members", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) { r.Native.FinalActive = 1 }},
		{"unclosed_handles", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Native.HandlesClosed = false
		}},
		{"native_error", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Native.Error = errors.New("native error")
		}},
		{"no_owned_stop", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Native.TerminalAction = "not-requested"
		}},
		{"protocol_not_received", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Order.ProtocolReceived = 0
		}},
		{"stop_before_protocol", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Order.OwnedActionStarted = r.Order.ProtocolReceived
		}},
		{"closure_before_stop", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.Order.ResourcesClosed = r.Order.OwnedActionCompleted
		}},
		{"vacuous_io", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.IO = process.ContainedIOSettlement{}
		}},
		{"unjoined_stream", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.IO.Stdout.WorkerComplete = false
		}},
		{"unclosed_pipe", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.IO.StderrRead.Closed = false
		}},
		{"output_limit_is_not_eof", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) {
			r.IO.Stdout.LimitExceeded = true
		}},
		{"io_digest_drift", func(r *privateSerenaNativeConsumerReceipt, _ *privateSerenaMigrationInput) { r.IO.Stdout.Bytes++ }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, input := privateSerenaConsumerPredicateData(t)
			tc.edit(r, &input)
			if err := validatePrivateSerenaNativeConsumer(r, input); err == nil {
				t.Fatal("malformed consumer DATA was accepted")
			}
		})
	}
	_, input := privateSerenaConsumerPredicateData(t)
	if err := validatePrivateSerenaNativeConsumer(nil, input); err == nil {
		t.Fatal("missing consumer DATA was accepted")
	}
}

func TestPrivateSerenaNativeConsumerWireSchema(t *testing.T) {
	r, input := privateSerenaConsumerPredicateData(t)
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var decoded privateSerenaNativeConsumerReceipt
	if err := privateSerenaMigrationJSON(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateSerenaNativeConsumer(&decoded, input); err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, old, replacement string }{
		{"missing_no_child_field", `"NoChildKnown":false,`, ""},
		{"missing_error_field", `"HandlesClosed":true,"Error":null`, `"HandlesClosed":true`},
		{"duplicate_complete", `"complete":true`, `"complete":true,"complete":true`},
		{"duplicate_native_identity_field", `"PID":4242`, `"PID":4242,"PID":4242`},
		{"unknown_field", `"complete":true`, `"extra":0,"complete":true`},
		{"boolean_as_integer", `"FinalActive":0`, `"FinalActive":false`},
		{"absent_protocol", `"protocol":{`, `"unexpected_protocol":{`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if bytes.Count(raw, []byte(tc.old)) != 1 {
				t.Fatal("wire mutation did not select one exact field")
			}
			bad := bytes.Replace(raw, []byte(tc.old), []byte(tc.replacement), 1)
			var value privateSerenaNativeConsumerReceipt
			if err := privateSerenaMigrationJSON(bad, &value); err == nil {
				t.Fatal("malformed wire record was accepted")
			}
		})
	}
}
