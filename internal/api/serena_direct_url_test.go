package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func directSpecimen(t *testing.T, raw string) preparedSerenaDistribution {
	t.Helper()
	var d preparedSerenaDistribution
	frame := `{"name":"dependency","version":"1","direct_url_present":true,"direct_url":` + raw + `,"files":[],"entrypoints":[]}`
	if err := json.Unmarshal([]byte(frame), &d); err != nil {
		t.Fatal(err)
	}
	return d
}
func TestFixedDirectURLKnownOriginsAndPresence(t *testing.T) {
	specimens := []string{
		`{"url":"file:///home/user/project","dir_info":{}}`,
		`{"url":"https://example.invalid/archive","archive_info":{"hash":"SHA256=ABCD","hashes":{"sha256":"abcd"}}}`,
		`{"url":"https://example.invalid/repo","vcs_info":{"vcs":"git","commit_id":"` + strings.Repeat("a", 40) + `","git_lfs":false},"subdirectory":"."}`,
	}
	for _, raw := range specimens {
		d := directSpecimen(t, raw)
		if d.DirectURL == nil || d.DirectURLPresent == nil || !*d.DirectURLPresent {
			t.Fatal("provenance omitted")
		}
	}
	var absent preparedSerenaDistribution
	if err := json.Unmarshal([]byte(`{"name":"index-dependency","version":"1","direct_url_present":false,"direct_url":null,"files":[],"entrypoints":[]}`), &absent); err != nil || absent.DirectURL != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"direct_url":null}`,
		`{"direct_url_present":null,"direct_url":null}`,
		`{"direct_url_present":true,"direct_url":null}`,
		`{"direct_url_present":false,"direct_url":{}}`,
		`{"direct_url_present":true,"direct_url":{"url":"file:///home/user/project","dir_info":{"editable":null}}}`,
		`{"direct_url_present":true,"direct_url":{"url":"https://example.invalid/archive","archive_info":{},"vcs_info":{"vcs":"git","commit_id":"` + strings.Repeat("a", 40) + `"}}}`,
		`{"direct_url_present":true,"direct_url":{"url":"https://example.invalid/archive","archive_info":{"hash":"sha256=abcd","hashes":{"sha256":"ffff"}}}}`,
		`{"direct_url_present":true,"direct_url":{"url":"https://user:password@example.invalid/archive","archive_info":{}}}`,
		`{"direct_url_present":true,"direct_url":{"url":"https://example.invalid/archive","archive_info":{"hashes":{"SHA256":"abcd","sha256":"abcd"}}}}`,
		`{"direct_url_present":true,"direct_url":{"url":"file:///home/user/project","dir_info":{},"subdirectory":"../outside"}}`,
		`{"direct_url_present":true,"direct_url":{"url":"https://example.invalid/archive","archive_info":{"unknown":1}}}`,
		`{"direct_url_present":true,"direct_url":{"url":"https://example.invalid/archive","archive_info":{"hashes":{"sha256":"abcd","sha256":"ffff"}}}}`,
	} {
		var d preparedSerenaDistribution
		if err := json.Unmarshal([]byte(raw), &d); err == nil {
			t.Fatal("degraded provenance admitted", raw)
		}
	}
}
func TestFixedResolutionProjectionBytesAndOrdering(t *testing.T) {
	first := directSpecimen(t, `{"url":"file:///home/user/project","dir_info":{}}`)
	raw, err := serializeSerenaResolutionV2([]preparedSerenaDistribution{first})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema":"serena-installed-resolution-v2","version":2,"distributions":[{"name":"dependency","version":"1","direct_url_present":true,"direct_url":{"url":"file:///home/user/project","origin":"directory","subdirectory":null,"info":{"editable":null}}}]}`
	if string(raw) != want {
		t.Fatalf("projection bytes %s", raw)
	}
	explicitFalse := directSpecimen(t, `{"dir_info":{"editable":false},"url":"file:///home/user/project"}`)
	changed, _ := serializeSerenaResolutionV2([]preparedSerenaDistribution{explicitFalse})
	if string(changed) == string(raw) {
		t.Fatal("optional presence lost")
	}
	a := directSpecimen(t, `{"url":"https://example.invalid/a","archive_info":{"hashes":{"sha512":"BBBB","sha256":"AAAA"}}}`)
	b := directSpecimen(t, `{"archive_info":{"hashes":{"sha256":"aaaa","sha512":"bbbb"}},"url":"https://example.invalid/a"}`)
	a.Name = "archive"
	b.Name = "archive"
	x, _ := serializeSerenaResolutionV2([]preparedSerenaDistribution{a, first})
	y, _ := serializeSerenaResolutionV2([]preparedSerenaDistribution{first, b})
	if string(x) != string(y) {
		t.Fatal("semantic order changed resolution")
	}
}
