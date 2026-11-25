//go:build windows

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/daemon"
	"mcp-local-hub/internal/process"
)

const privateSerenaNativeConsumerSchema = "private-serena-native-consumer-v1"

// These fields are DATA from the original consumer producer. Neither this
// receipt nor its validator can construct native, protocol or IO authority.
type privateSerenaNativeConsumerProtocol struct {
	QualificationID string `json:"qualification_id"`
	HostStartSHA256 string `json:"host_start_sha256"`
	EndpointSHA256  string `json:"endpoint_sha256"`
	ReceiptSHA256   string `json:"receipt_sha256"`
}

type privateSerenaNativeConsumerReceipt struct {
	Schema          string                                 `json:"schema"`
	Ready           api.PreparedSerenaRuntimeBinding       `json:"ready"`
	Confirmed       api.PreparedConsumerBindingV1          `json:"confirmed"`
	Terminal        api.PreparedConsumerBindingV1          `json:"terminal"`
	Protocol        privateSerenaNativeConsumerProtocol    `json:"protocol"`
	Native          process.ContainedNativeSnapshot        `json:"native"`
	IO              process.ContainedIOSettlement          `json:"io"`
	IOReceiptSHA256 string                                 `json:"io_receipt_sha256"`
	Order           daemon.HTTPHostQualificationEventOrder `json:"order"`
	Complete        bool                                   `json:"complete"`
}

// The wire record is the exact shape emitted by these types. Reject duplicate
// keys and absent zero-valued fields as well as unknown fields. This prevents a
// missing Error/NoChildKnown field from silently becoming a successful default.
func (r *privateSerenaNativeConsumerReceipt) UnmarshalJSON(raw []byte) error {
	if err := privateSerenaNativeConsumerUniqueJSON(raw); err != nil {
		return err
	}
	type wire privateSerenaNativeConsumerReceipt
	var value wire
	if err := privateSerenaMigrationJSON(raw, &value); err != nil {
		return err
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var supplied, emitted any
	left, right := json.NewDecoder(bytes.NewReader(raw)), json.NewDecoder(bytes.NewReader(normalized))
	left.UseNumber()
	right.UseNumber()
	if err := left.Decode(&supplied); err != nil {
		return err
	}
	if err := right.Decode(&emitted); err != nil {
		return err
	}
	if !reflect.DeepEqual(supplied, emitted) {
		return errors.New("private Serena consumer wire fields/types differ from the emitted schema")
	}
	*r = privateSerenaNativeConsumerReceipt(value)
	return nil
}

func privateSerenaNativeConsumerUniqueJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var visit func(int) error
	visit = func(depth int) error {
		if depth > 16 {
			return errors.New("private Serena consumer JSON nesting exceeds its fixed schema")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, container := token.(json.Delim)
		if !container {
			return nil
		}
		close := json.Delim(']')
		switch delim {
		case '{':
			close = '}'
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("private Serena consumer JSON repeats an object field")
				}
				seen[name] = true
				if err := visit(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := visit(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("private Serena consumer JSON delimiter differs")
		}
		last, err := d.Token()
		if err != nil || last != close {
			return errors.Join(err, errors.New("private Serena consumer JSON container is incomplete"))
		}
		return nil
	}
	if err := visit(0); err != nil {
		return err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errors.Join(err, errors.New("private Serena consumer requires one JSON value"))
	}
	return nil
}

func privateSerenaNativeConsumerID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// Called only as DATA receiving after the original producer's typed validators
// and, in the parent, after the original publisher child/native/IO boundary.
func validatePrivateSerenaNativeConsumer(r *privateSerenaNativeConsumerReceipt, input privateSerenaMigrationInput) error {
	if r == nil || r.Schema != privateSerenaNativeConsumerSchema || !r.Complete || r.Ready.Schema != api.PreparedSerenaBindingSchema {
		return errors.New("private Serena native consumer receiving is absent/incomplete")
	}
	for _, digest := range []string{r.Ready.RequestIdentitySHA256, r.Ready.RecordSHA256, r.Ready.SourceSHA256, r.Ready.InterpreterSHA256, r.Ready.EntrypointSHA256, r.Ready.ResolutionSHA256, r.Protocol.HostStartSHA256, r.Protocol.EndpointSHA256, r.Protocol.ReceiptSHA256, r.IOReceiptSHA256, r.Order.OwnedActionReceiptSHA256} {
		if !validLowerHexSHA256(digest) {
			return errors.New("private Serena native consumer receipt digest is invalid")
		}
	}
	if !privateSerenaNativeConsumerID(r.Protocol.QualificationID) || r.Ready.SourceSHA256 != input.SourceEpoch || r.Ready.InterpreterSHA256 != input.Python.SHA256 {
		return errors.New("private Serena native consumer source/interpreter binding differs")
	}
	if err := errors.Join(r.Confirmed.Validate(), r.Terminal.Validate()); err != nil {
		return err
	}
	confirmed, terminal := r.Confirmed, r.Terminal
	if confirmed.ConsumerKind != "workspace" || confirmed.Disposition != "current" || confirmed.StartDisposition != "confirmed" || confirmed.NativeBinding == nil || confirmed.ClosureReceiptSHA256 != nil || terminal.Disposition != "current" || terminal.StartDisposition != "terminal" || terminal.NativeBinding == nil || terminal.ClosureReceiptSHA256 == nil {
		return errors.New("private Serena native consumer transition is not current/confirmed to current/terminal")
	}
	expected := confirmed
	expected.StartDisposition = "terminal"
	expected.ClosureReceiptSHA256 = terminal.ClosureReceiptSHA256
	if !reflect.DeepEqual(expected, terminal) || confirmed.PackageRequestIdentitySHA256 != r.Ready.RequestIdentitySHA256 || confirmed.GenerationID != r.Ready.GenerationID || confirmed.ReadyRecordSHA256 != r.Ready.RecordSHA256 || confirmed.SourceSHA256 != r.Ready.SourceSHA256 || confirmed.EntrypointSHA256 != r.Ready.EntrypointSHA256 {
		return errors.New("private Serena native consumer immutable Ready/binding tuple differs")
	}
	native := r.Native
	id := native.RootIdentity
	if !id.Known || id.PID <= 0 || uint64(id.PID) > uint64(^uint32(0)) || id.NativeBirthFILETIME == 0 || uint32(id.PID) != confirmed.NativeBinding.PID || id.NativeBirthFILETIME != confirmed.NativeBinding.NativeBirthFILETIME || native.NoChildKnown || !native.RootWaitKnown || !native.HandlesClosed || native.FinalActive != 0 || native.Error != nil || !(native.MembersExited && native.AccountingKnown) || native.TerminalAction != "request-succeeded" {
		return errors.New("private Serena native consumer original identity/owned stop is incomplete")
	}
	identityBytes, err := json.Marshal(id)
	if err != nil {
		return err
	}
	identitySHA := privateSerenaMigrationDigest(identityBytes)
	if identitySHA != confirmed.NativeBinding.NativeIdentitySHA256 || identitySHA != r.Protocol.HostStartSHA256 {
		return errors.New("private Serena native consumer original host identity digest differs")
	}
	order := r.Order
	if order.ProtocolReceived == 0 || order.OwnedActionStarted <= order.ProtocolReceived || order.OwnedActionCompleted <= order.OwnedActionStarted || order.ResourcesClosed <= order.OwnedActionCompleted {
		return errors.New("private Serena native consumer protocol/owned-stop/closure chronology differs")
	}
	for _, slot := range []process.ContainedStreamSlot{r.IO.Stdin, r.IO.StdoutRead, r.IO.StdoutWrite, r.IO.StderrRead, r.IO.StderrWrite} {
		if !slot.Acquired || !slot.CloseAttempted || !slot.Closed || slot.CloseError != nil {
			return errors.New("private Serena native consumer original pipe close is incomplete")
		}
	}
	for _, stream := range []process.ContainedIOStream{r.IO.Stdout, r.IO.Stderr} {
		if !stream.WorkerStarted || !stream.WorkerComplete || !stream.EOF || stream.Error != nil || stream.LimitExceeded || stream.Bytes < 0 || stream.Limit < 0 {
			return errors.New("private Serena native consumer original IO worker is incomplete")
		}
	}
	ioBytes, err := json.Marshal(struct {
		Schema                    string
		IO                        process.ContainedIOSettlement
		LogClosed, RequestsJoined bool
	}{"http-host-original-io-v1", r.IO, true, true})
	if err != nil {
		return err
	}
	if privateSerenaMigrationDigest(ioBytes) != r.IOReceiptSHA256 {
		return errors.New("private Serena native consumer original IO receipt digest differs")
	}
	// A successful owned stop can have a nonzero child exit code. Its actual
	// exit remains in Native; this DATA gate requires the original stop order.
	return nil
}
