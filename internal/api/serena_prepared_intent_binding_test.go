package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func syntheticPreparedConsumer() PreparedConsumerBindingV1 {
	return PreparedConsumerBindingV1{OperationID: "start-one", TaskName: `\synthetic-task`, ConsumerKind: "workspace", PackageRequestIdentitySHA256: strings.Repeat("a", 64), GenerationID: "generation-one", ReadyRecordSHA256: strings.Repeat("b", 64), SourceSHA256: strings.Repeat("c", 64), EntrypointSHA256: strings.Repeat("d", 64), DescriptorSHA256: strings.Repeat("e", 64), Disposition: "pending", StartDisposition: "not_attempted"}
}
func TestPreparedConsumerWireUndefinedAndStrictFields(t *testing.T) {
	row := syntheticPreparedConsumer()
	raw, _ := json.Marshal(row)
	var decoded PreparedConsumerBindingV1
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Validate() != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"native_binding":null`) || !strings.Contains(string(raw), `"closure_receipt_sha256":null`) {
		t.Fatal("undefined evidence was omitted or defaulted")
	}
	for _, bad := range []string{strings.Replace(string(raw), `"operation_id"`, `"Operation_ID"`, 1), strings.Replace(string(raw), `"operation_id":"start-one"`, `"operation_id":"start-one","operation_id":"start-two"`, 1), strings.Replace(string(raw), `"native_binding":null,`, "", 1)} {
		if json.Unmarshal([]byte(bad), &decoded) == nil {
			t.Fatal("invalid field boundary admitted")
		}
	}
	row.StartDisposition = "confirmed"
	if row.Validate() == nil {
		t.Fatal("default native identity confirmed a consumer")
	}
	row.NativeBinding = &PreparedConsumerNativeBindingV1{CreatorOperationID: row.OperationID, PID: 7, NativeBirthFILETIME: 42, NativeIdentitySHA256: strings.Repeat("f", 64)}
	row.Disposition = "current"
	if err := row.Validate(); err != nil {
		t.Fatal(err)
	}
}
func TestPreparedConsumerBlockCannotLoseReferencesOrAliasClone(t *testing.T) {
	empty := PreparedSerenaConsumersV1{Schema: PreparedSerenaConsumersSchema, Bindings: []PreparedConsumerBindingV1{}}
	if empty.Validate() != nil {
		t.Fatal("explicit empty array refused")
	}
	empty.Bindings = nil
	if empty.Validate() == nil {
		t.Fatal("missing/null block treated as empty")
	}
	row := syntheticPreparedConsumer()
	block := &PreparedSerenaConsumersV1{Schema: PreparedSerenaConsumersSchema, Bindings: []PreparedConsumerBindingV1{row, row}}
	if block.Validate() == nil {
		t.Fatal("duplicate reference admitted")
	}
	row.StartDisposition = "confirmed"
	row.Disposition = "rollback"
	row.NativeBinding = &PreparedConsumerNativeBindingV1{CreatorOperationID: row.OperationID, PID: 7, NativeBirthFILETIME: 42, NativeIdentitySHA256: strings.Repeat("f", 64)}
	block.Bindings = []PreparedConsumerBindingV1{row}
	clone := clonePreparedSerenaConsumers(block)
	clone.Bindings[0].NativeBinding.PID = 8
	if block.Bindings[0].NativeBinding.PID != 7 {
		t.Fatal("clone changed original custody")
	}
}
