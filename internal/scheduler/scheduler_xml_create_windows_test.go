//go:build windows

package scheduler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"unicode/utf16"

	"mcp-local-hub/internal/process"
)

const xmlCreateFixture = `<?xml version="1.0" encoding="UTF-16"?><Task><RegistrationInfo><Description>§-Привет-🙂 &amp; $(throw 'data')</Description></RegistrationInfo><Principals><Principal id="P"><UserId>S-1-5-21-fixture</UserId><LogonType>Password</LogonType></Principal></Principals><Settings><Enabled>false</Enabled><Hidden>true</Hidden></Settings><Actions Context="P"><Exec><Command>D:\fixture\never.exe</Command><Arguments>&quot;literal\value&quot;</Arguments></Exec></Actions></Task>`

func xmlCreateUTF16Fixture(text string) []byte {
	units := utf16.Encode([]rune(text))
	raw := make([]byte, 2+2*len(units))
	raw[0], raw[1] = 0xff, 0xfe
	for i, unit := range units {
		binary.LittleEndian.PutUint16(raw[2+2*i:], unit)
	}
	return raw
}

func TestCreateXMLIfAbsentPreservesDefinitionInBase64Request(t *testing.T) {
	want := xmlCreateUTF16Fixture(xmlCreateFixture)
	for _, raw := range [][]byte{[]byte(xmlCreateFixture), append([]byte{0xef, 0xbb, 0xbf}, []byte(xmlCreateFixture)...), want} {
		calls := 0
		withCOMRunner(t, func(ctx context.Context, request schedulerCOMRequest) (schedulerCOMResponse, error) {
			calls++
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("registration request has no deadline")
			}
			if request.Operation != "create_xml_if_absent" || request.Name != `\fixture-$('literal')` || !bytes.Equal(request.XMLUTF16LE, want) {
				t.Fatalf("definition or literal name changed: %+v", request)
			}
			encoded, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				XML string `json:"xml_utf16le"`
			}
			if err := json.Unmarshal(encoded, &wire); err != nil {
				t.Fatal(err)
			}
			decoded, err := base64.StdEncoding.DecodeString(wire.XML)
			if err != nil || !bytes.Equal(decoded, want) {
				t.Fatalf("wire XML differs: %v", err)
			}
			return schedulerCOMResponse{OK: true}, nil
		})
		if err := (&windowsScheduler{}).CreateXMLIfAbsent(`\fixture-$('literal')`, raw); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Fatalf("registration calls=%d, want one", calls)
		}
	}
}

func TestCreateXMLIfAbsentRejectsInputBeforeDispatch(t *testing.T) {
	withCOMRunner(t, func(context.Context, schedulerCOMRequest) (schedulerCOMResponse, error) {
		t.Fatal("invalid input reached scheduler bridge")
		return schedulerCOMResponse{}, nil
	})
	for _, name := range []string{"", " ", `\`, `\\name`, `\folder\name`, "folder/name", ".", "..", " name", "name ", "name*", "name?", "name\x00", "name\n", string([]byte{0xff})} {
		if err := (&windowsScheduler{}).CreateXMLIfAbsent(name, []byte(xmlCreateFixture)); !errors.Is(err, ErrTaskCorrupt) {
			t.Fatalf("name=%q error=%v", name, err)
		}
	}
	for _, raw := range [][]byte{nil, {}, {0xef, 0xbb, 0xbf}, {0xff, 0xfe}, {0xff, 0xfe, 0x41}, {0xff, 0xfe, 0x00, 0xd8}, {0xff}, {0xfe, 0xff, 0x00, 0x41}, []byte(" \t\r\n"), xmlCreateUTF16Fixture(" \t\r\n")} {
		if err := (&windowsScheduler{}).CreateXMLIfAbsent(`\fixture`, raw); !errors.Is(err, ErrTaskCorrupt) {
			t.Fatalf("XML=%x error=%v", raw, err)
		}
	}
}

func TestCreateXMLIfAbsentReturnsConflictAndUncertainTransportWithoutRetry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response schedulerCOMResponse
		cause    error
		want     error
	}{
		{"conflict", schedulerCOMResponse{Kind: "task_already_exists", Phase: "xml_register", HRESULT: 0x800700b7}, nil, ErrTaskAlreadyExists},
		{"permission", schedulerCOMResponse{Kind: "permission_denied", HRESULT: 0x80070005}, nil, ErrPermissionDenied},
		{"corrupt", schedulerCOMResponse{Kind: "task_corrupt", HRESULT: 0x8004131a}, nil, ErrTaskCorrupt},
		{"unconfirmed", schedulerCOMResponse{Kind: "scheduler_unavailable", Phase: "xml_register_unconfirmed"}, nil, ErrUnavailable},
		{"lost reply", schedulerCOMResponse{OK: true}, context.DeadlineExceeded, ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			withCOMRunner(t, func(context.Context, schedulerCOMRequest) (schedulerCOMResponse, error) {
				calls++
				return tc.response, tc.cause
			})
			err := (&windowsScheduler{}).CreateXMLIfAbsent(`\fixture`, []byte(xmlCreateFixture))
			if !errors.Is(err, tc.want) || calls != 1 {
				t.Fatalf("error=%v calls=%d", err, calls)
			}
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Fatalf("transport cause lost: %v", err)
			}
			if tc.want == ErrUnavailable && !strings.Contains(err.Error(), "outcome uncertain") {
				t.Fatalf("mutation uncertainty omitted: %v", err)
			}
		})
	}
}

// Replace the entire connection statement with a fake object before executing
// the production script. These tests exercise PowerShell/JSON/exception behavior
// but never connect to or mutate Windows Task Scheduler.
func runXMLCreateScriptFixture(t *testing.T, request schedulerCOMRequest, registerBody string) schedulerCOMResponse {
	t.Helper()
	const connect = `$s=New-Object -ComObject 'Schedule.Service';$s.Connect();$f=$s.GetFolder('\');`
	if strings.Count(schedulerCOMScript, connect) != 1 {
		t.Fatal("COM connection fixture boundary changed")
	}
	fake := `$f=[pscustomobject]@{};$f|Add-Member ScriptMethod GetTasks {throw 'enumeration forbidden'};$f|Add-Member ScriptMethod RegisterTask {param($name,$xml,$flags,$userId,$password,$logonType,$sddl);` + registerBody + `};`
	script := schedulerCOMUTF8Preamble + strings.Replace(schedulerCOMScript, connect, fake, 1)
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), schedulerCOMDeadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	process.NoConsole(cmd)
	cmd.Stdin = bytes.NewReader(raw)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("script fixture failed: %v", err)
	}
	var response schedulerCOMResponse
	if err := json.Unmarshal(out, &response); err != nil {
		t.Fatalf("script fixture JSON=%q: %v", out, err)
	}
	return response
}

func TestXMLCreateCOMScriptPassesExactDataAndCreateOnlyFlags(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte(xmlCreateFixture))
	body := `$expected=[System.Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('` + encoded + `'));if($xml -cne $expected -or $name -cne [string]$r.name){throw 'data changed'};if($flags -ne 34 -or $logonType -ne 0 -or $null -ne $userId -or $null -ne $password -or $null -ne $sddl){throw 'registration policy changed'};return [pscustomobject]@{Path=$name}`
	response := runXMLCreateScriptFixture(t, schedulerCOMRequest{
		Operation: "create_xml_if_absent", Name: `\fixture-$('literal')`, XMLUTF16LE: xmlCreateUTF16Fixture(xmlCreateFixture),
	}, body)
	if !response.OK {
		t.Fatalf("create-only request failed: %+v", response)
	}
}

func TestXMLCreateCOMScriptUnwrapsAndClassifiesNativeHRESULT(t *testing.T) {
	for _, tc := range []struct {
		hresult uint32
		kind    string
	}{
		{0x800700b7, "task_already_exists"},
		{0x80070005, "permission_denied"},
		{0x80070057, "task_corrupt"},
		{0x80041316, "task_corrupt"},
		{0x80041317, "task_corrupt"},
		{0x80041318, "task_corrupt"},
		{0x80041319, "task_corrupt"},
		{0x8004131a, "task_corrupt"},
		{0x8004131d, "task_corrupt"},
		{0x80041322, "scheduler_unavailable"},
	} {
		t.Run(fmt.Sprintf("%08x", tc.hresult), func(t *testing.T) {
			body := fmt.Sprintf(`$native=[System.Runtime.InteropServices.COMException]::new('ОШИБКА: file not found — untrusted message',%d);throw [System.Management.Automation.MethodInvocationException]::new('outer wrapper',$native)`, int32(tc.hresult))
			response := runXMLCreateScriptFixture(t, schedulerCOMRequest{
				Operation: "create_xml_if_absent", Name: `\fixture`, XMLUTF16LE: xmlCreateUTF16Fixture(xmlCreateFixture),
			}, body)
			if response.OK || response.Kind != tc.kind || response.HRESULT != tc.hresult || response.Phase != "xml_register" {
				t.Fatalf("wrapped native result=%+v", response)
			}
		})
	}
}

func TestXMLCreateCOMScriptRejectsMalformedEncodingAndUnconfirmedRegistration(t *testing.T) {
	request := schedulerCOMRequest{Operation: "create_xml_if_absent", Name: `\fixture`, XMLUTF16LE: []byte{0xff, 0xfe, 0x00, 0xd8}}
	response := runXMLCreateScriptFixture(t, request, `throw 'registration must not be called'`)
	if response.OK || response.Kind != "task_corrupt" || response.Phase != "xml_decode" {
		t.Fatalf("malformed input result=%+v", response)
	}
	request.XMLUTF16LE = xmlCreateUTF16Fixture(xmlCreateFixture)
	response = runXMLCreateScriptFixture(t, request, `return $null`)
	if response.OK || response.Kind != "scheduler_unavailable" || response.Phase != "xml_register_unconfirmed" {
		t.Fatalf("unconfirmed mutation result=%+v", response)
	}
}
