package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
)

const PreparedSerenaConsumersSchema = "serena-prepared-consumers-v1"

type PreparedSerenaConsumersV1 struct {
	Schema   string                      `json:"schema"`
	Bindings []PreparedConsumerBindingV1 `json:"bindings"`
}
type PreparedConsumerNativeBindingV1 struct {
	CreatorOperationID   string `json:"creator_operation_id"`
	PID                  uint32 `json:"pid"`
	NativeBirthFILETIME  uint64 `json:"native_birth_filetime"`
	NativeIdentitySHA256 string `json:"native_identity_sha256"`
}
type PreparedConsumerBindingV1 struct {
	OperationID                  string                           `json:"operation_id"`
	TaskName                     string                           `json:"task_name"`
	ConsumerKind                 string                           `json:"consumer_kind"`
	PackageRequestIdentitySHA256 string                           `json:"package_request_identity_sha256"`
	GenerationID                 string                           `json:"generation_id"`
	ReadyRecordSHA256            string                           `json:"ready_record_sha256"`
	SourceSHA256                 string                           `json:"source_sha256"`
	EntrypointSHA256             string                           `json:"entrypoint_sha256"`
	DescriptorSHA256             string                           `json:"descriptor_sha256"`
	Disposition                  string                           `json:"disposition"`
	StartDisposition             string                           `json:"start_disposition"`
	NativeBinding                *PreparedConsumerNativeBindingV1 `json:"native_binding"`
	ClosureReceiptSHA256         *string                          `json:"closure_receipt_sha256"`
}

func decodePreparedExactObject(raw []byte, fields []string, target any) error {
	allowed := map[string]bool{}
	for _, field := range fields {
		allowed[field] = true
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return errors.New("SERENA_PREPARED_BINDING_OBJECT_INVALID")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok || !allowed[key] || seen[key] {
			return errors.New("SERENA_PREPARED_BINDING_FIELDS_INVALID")
		}
		seen[key] = true
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			return err
		}
	}
	if _, err = decoder.Token(); err != nil {
		return err
	}
	if len(seen) != len(allowed) {
		return errors.New("SERENA_PREPARED_BINDING_FIELDS_MISSING")
	}
	if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("SERENA_PREPARED_BINDING_TRAILING_DATA")
	}
	return json.Unmarshal(raw, target)
}
func (b *PreparedSerenaConsumersV1) UnmarshalJSON(raw []byte) error {
	type plain PreparedSerenaConsumersV1
	var value plain
	if err := decodePreparedExactObject(raw, []string{"schema", "bindings"}, &value); err != nil {
		return err
	}
	*b = PreparedSerenaConsumersV1(value)
	return nil
}
func (b *PreparedConsumerNativeBindingV1) UnmarshalJSON(raw []byte) error {
	type plain PreparedConsumerNativeBindingV1
	var value plain
	if err := decodePreparedExactObject(raw, []string{"creator_operation_id", "pid", "native_birth_filetime", "native_identity_sha256"}, &value); err != nil {
		return err
	}
	*b = PreparedConsumerNativeBindingV1(value)
	return nil
}
func (b *PreparedConsumerBindingV1) UnmarshalJSON(raw []byte) error {
	type plain PreparedConsumerBindingV1
	var value plain
	if err := decodePreparedExactObject(raw, []string{"operation_id", "task_name", "consumer_kind", "package_request_identity_sha256", "generation_id", "ready_record_sha256", "source_sha256", "entrypoint_sha256", "descriptor_sha256", "disposition", "start_disposition", "native_binding", "closure_receipt_sha256"}, &value); err != nil {
		return err
	}
	*b = PreparedConsumerBindingV1(value)
	return nil
}
func (b PreparedConsumerBindingV1) Validate() error {
	if !preparedSerenaID(b.OperationID) || b.TaskName == "" || !preparedSerenaID(b.GenerationID) || (b.ConsumerKind != "global" && b.ConsumerKind != "workspace") {
		return errors.New("SERENA_PREPARED_CONSUMER_IDENTITY_INVALID")
	}
	for _, digest := range []string{b.PackageRequestIdentitySHA256, b.ReadyRecordSHA256, b.SourceSHA256, b.EntrypointSHA256, b.DescriptorSHA256} {
		if !preparedSerenaDigest(digest) {
			return errors.New("SERENA_PREPARED_CONSUMER_DIGEST_INVALID")
		}
	}
	if b.Disposition != "pending" && b.Disposition != "current" && b.Disposition != "rollback" {
		return errors.New("SERENA_PREPARED_CONSUMER_DISPOSITION_INVALID")
	}
	switch b.StartDisposition {
	case "not_attempted":
		if b.NativeBinding != nil || b.ClosureReceiptSHA256 != nil || b.Disposition != "pending" {
			return errors.New("SERENA_PREPARED_CONSUMER_UNDEFINED_COMBINATION")
		}
	case "attempted_unknown":
		if b.Disposition != "pending" || b.ClosureReceiptSHA256 != nil {
			return errors.New("SERENA_PREPARED_CONSUMER_UNDEFINED_COMBINATION")
		}
	case "confirmed":
		if b.NativeBinding == nil || b.ClosureReceiptSHA256 != nil {
			return errors.New("SERENA_PREPARED_CONSUMER_UNDEFINED_COMBINATION")
		}
	case "no_child":
		if b.NativeBinding != nil || b.ClosureReceiptSHA256 == nil || b.Disposition != "pending" {
			return errors.New("SERENA_PREPARED_CONSUMER_UNDEFINED_COMBINATION")
		}
	case "terminal":
		if b.ClosureReceiptSHA256 == nil {
			return errors.New("SERENA_PREPARED_CONSUMER_UNDEFINED_COMBINATION")
		}
	default:
		return errors.New("SERENA_PREPARED_CONSUMER_START_DISPOSITION_INVALID")
	}
	if b.NativeBinding != nil {
		n := b.NativeBinding
		if n.CreatorOperationID != b.OperationID || n.PID == 0 || n.NativeBirthFILETIME == 0 || !preparedSerenaDigest(n.NativeIdentitySHA256) {
			return errors.New("SERENA_PREPARED_CONSUMER_NATIVE_BINDING_INVALID")
		}
	}
	if b.ClosureReceiptSHA256 != nil && !preparedSerenaDigest(*b.ClosureReceiptSHA256) {
		return errors.New("SERENA_PREPARED_CONSUMER_CLOSURE_DIGEST_INVALID")
	}
	return nil
}
func (b *PreparedSerenaConsumersV1) Validate() error {
	if b == nil || b.Schema != PreparedSerenaConsumersSchema || b.Bindings == nil {
		return errors.New("SERENA_PREPARED_CONSUMERS_UNAVAILABLE")
	}
	previous := ""
	seen := map[string]bool{}
	for _, row := range b.Bindings {
		if err := row.Validate(); err != nil {
			return err
		}
		key := row.OperationID + "\x00" + row.TaskName
		if seen[key] || key < previous {
			return errors.New("SERENA_PREPARED_CONSUMERS_ORDER_OR_DUPLICATE")
		}
		seen[key] = true
		previous = key
	}
	return nil
}
func (b *PreparedSerenaConsumersV1) canonicalize() {
	if b == nil {
		return
	}
	sort.Slice(b.Bindings, func(i, j int) bool {
		a, c := b.Bindings[i], b.Bindings[j]
		if a.OperationID != c.OperationID {
			return a.OperationID < c.OperationID
		}
		return a.TaskName < c.TaskName
	})
}
func clonePreparedSerenaConsumers(in *PreparedSerenaConsumersV1) *PreparedSerenaConsumersV1 {
	if in == nil {
		return nil
	}
	out := *in
	out.Bindings = make([]PreparedConsumerBindingV1, len(in.Bindings))
	copy(out.Bindings, in.Bindings)
	for i := range out.Bindings {
		if in.Bindings[i].NativeBinding != nil {
			native := *in.Bindings[i].NativeBinding
			out.Bindings[i].NativeBinding = &native
		}
		if in.Bindings[i].ClosureReceiptSHA256 != nil {
			digest := *in.Bindings[i].ClosureReceiptSHA256
			out.Bindings[i].ClosureReceiptSHA256 = &digest
		}
	}
	return &out
}
