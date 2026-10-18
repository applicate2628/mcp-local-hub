package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

type recoveryTestReader struct {
	listCalls, inspectCalls int
	id                      string
	summaries               []api.SecretsInitRecoverySummary
	report                  api.SecretsInitRecoveryReport
	err                     error
}

func (r *recoveryTestReader) ListSecretsInitRecovery() ([]api.SecretsInitRecoverySummary, error) {
	r.listCalls++
	return r.summaries, r.err
}
func (r *recoveryTestReader) ReadSecretsInitRecovery(id string) (api.SecretsInitRecoveryReport, error) {
	r.inspectCalls++
	r.id = id
	return r.report, r.err
}
func executeRecoveryUnit(reader *recoveryTestReader, out io.Writer, args ...string) error {
	root := &cobra.Command{Use: "mcphub", SilenceErrors: true, SilenceUsage: true}
	root.SetOut(out)
	root.SetErr(out)
	root.AddCommand(newSecretsRecoveryCmdWithReader(reader))
	root.SetArgs(append([]string{"recovery"}, args...))
	return root.Execute()
}

func TestSecretsRecoveryGrammarAndHelp(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		message string
	}{
		{"group", nil, ""},
		{"help", []string{"--help"}, ""},
		{"inspect-help", []string{"inspect", "--help"}, ""},
		{"unknown-command", []string{"private-token"}, recoveryUsageInvalid},
		{"list-positional", []string{"list", "private-token"}, recoveryUsageInvalid},
		{"inspect-positional", []string{"inspect", "private-token", "--attempt-id", "id"}, recoveryUsageInvalid},
		{"group-flag", []string{"--root=/private/token"}, recoveryUsageInvalid},
		{"list-flag", []string{"list", "--apply=/private/token"}, recoveryUsageInvalid},
		{"inspect-flag", []string{"inspect", "--reset=/private/token"}, recoveryUsageInvalid},
		{"flag-without-value", []string{"inspect", "--attempt-id"}, recoveryUsageInvalid},
		{"missing-flag", []string{"inspect"}, recoveryAttemptRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &recoveryTestReader{}
			var out bytes.Buffer
			err := executeRecoveryUnit(reader, &out, tc.args...)
			if tc.message == "" {
				if err != nil || !strings.Contains(out.String(), "historical") {
					t.Fatalf("help: %v, %q", err, out.String())
				}
			} else {
				if err == nil || err.Error() != tc.message || out.Len() != 0 {
					t.Fatalf("grammar: %v, %q", err, out.String())
				}
				if errors.Unwrap(err) == nil {
					t.Fatal("grammar cause lost")
				}
			}
			if reader.listCalls != 0 || reader.inspectCalls != 0 {
				t.Fatal("grammar/help called API")
			}
		})
	}
}

func TestSecretsRecoveryRegistration(t *testing.T) {
	secrets := newSecretsCmdReal()
	want := []string{"delete", "edit", "get", "init", "list", "migrate", "recovery", "set"}
	var got []string
	for _, cmd := range secrets.Commands() {
		got = append(got, cmd.Name())
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("children: %v", got)
	}
	for _, args := range [][]string{{"recovery", "list"}, {"recovery", "inspect"}} {
		cmd, rest, err := secrets.Find(args)
		if err != nil || len(rest) != 0 || cmd.Name() != args[1] {
			t.Fatalf("registration: %v %v %v", cmd, rest, err)
		}
	}
}

func TestSecretsRecoveryProjection(t *testing.T) {
	const reportJSON = `{"attempt_id":"11111111111111111111111111111111","state":"prepared","key":{"selected_path":"relative\n\"key","absolute_path":"/synthetic/key","outcome":"not_attempted","remove_error_class":"none"},"vault":{"selected_path":"vault","absolute_path":"/synthetic/vault","outcome":"not_attempted","remove_error_class":"none"},"authority":"historical_attempt_only","current_state":"not_inspected"}`
	var report api.SecretsInitRecoveryReport
	if err := json.Unmarshal([]byte(reportJSON), &report); err != nil {
		t.Fatal(err)
	}
	reader := &recoveryTestReader{report: report}
	var out bytes.Buffer
	if err := executeRecoveryUnit(reader, &out, "inspect", "--attempt-id", report.AttemptID); err != nil {
		t.Fatal(err)
	}
	var got api.SecretsInitRecoveryReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || !reflect.DeepEqual(got, report) {
		t.Fatalf("projection: %v %v", got, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 6 || fields["resolution_cwd"] != nil || fields["cause"] != nil {
		t.Fatalf("private projection: %s", out.Bytes())
	}
	if reader.inspectCalls != 1 || reader.listCalls != 0 || reader.id != report.AttemptID || !bytes.HasSuffix(out.Bytes(), []byte("\n")) {
		t.Fatal("inspect invocation/newline")
	}
	for _, summaries := range [][]api.SecretsInitRecoverySummary{{}, {{AttemptID: "b", State: "retired"}, {AttemptID: "a", State: "prepared"}}} {
		reader = &recoveryTestReader{summaries: summaries}
		out.Reset()
		if err := executeRecoveryUnit(reader, &out, "list"); err != nil {
			t.Fatal(err)
		}
		expected, err := json.Marshal(summaries)
		if err != nil {
			t.Fatal(err)
		}
		if out.String() != string(expected)+"\n" || reader.listCalls != 1 || reader.inspectCalls != 0 {
			t.Fatalf("list projection: %q", out.String())
		}
	}
}

func TestSecretsRecoveryIDDelegationAndCause(t *testing.T) {
	cause := &os.PathError{Op: "open", Path: "/synthetic/private-token", Err: os.ErrPermission}
	for _, id := range []string{"", "malformed", strings.Repeat("0", 32), strings.Repeat("A", 32), " /synthetic/path "} {
		reader := &recoveryTestReader{err: cause}
		var out bytes.Buffer
		err := executeRecoveryUnit(reader, &out, "inspect", "--attempt-id="+id)
		var got *os.PathError
		if err == nil || err.Error() != recoveryInspectFailed || errors.Unwrap(err) != cause || !errors.Is(err, os.ErrPermission) || !errors.As(err, &got) || got != cause {
			t.Fatalf("cause: %v", err)
		}
		if reader.id != id || reader.inspectCalls != 1 || reader.listCalls != 0 || out.Len() != 0 {
			t.Fatal("ID policy duplicated or output emitted")
		}
	}
	reader := &recoveryTestReader{err: cause, summaries: []api.SecretsInitRecoverySummary{{AttemptID: "partial", State: "prepared"}}}
	var out bytes.Buffer
	err := executeRecoveryUnit(reader, &out, "list")
	if err == nil || err.Error() != recoveryListFailed || errors.Unwrap(err) != cause || out.Len() != 0 || reader.listCalls != 1 {
		t.Fatalf("list failure: %v %q", err, out.String())
	}
}

type recoveryFailWriter struct {
	cause  error
	prefix []byte
}

func (w *recoveryFailWriter) Write(p []byte) (int, error) {
	w.prefix = append(w.prefix, p[0])
	return 1, w.cause
}
func TestSecretsRecoveryOutputFailure(t *testing.T) {
	cause := errors.New("synthetic-private-writer-detail")
	for _, args := range [][]string{{"list"}, {"inspect", "--attempt-id", "id"}} {
		reader := &recoveryTestReader{summaries: []api.SecretsInitRecoverySummary{}}
		writer := &recoveryFailWriter{cause: cause}
		err := executeRecoveryUnit(reader, writer, args...)
		if err == nil || err.Error() != recoveryOutputFailed || !errors.Is(err, cause) || errors.Unwrap(err) != cause || len(writer.prefix) != 1 {
			t.Fatalf("writer failure: %v", err)
		}
		if reader.listCalls+reader.inspectCalls != 1 {
			t.Fatal("output failure retried API")
		}
	}
}
