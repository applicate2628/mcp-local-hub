package pinstatus

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

func TestS2_PublicWireRedactionCoversEveryRemoteCarrier(t *testing.T) {
	tests := []struct {
		name             string
		repo             string
		url              string
		secret           string
		decodeIncomplete bool
	}{
		{"fragment", "group/project#token=fragment-secret", "https://github.com/group/project.git#token=fragment-secret", "fragment-secret", false},
		{"percent encoded", "group/%74oken%3Dencoded-secret", "https://github.com/group/%74oken%3Dencoded-secret.git", "encoded-secret", false},
		{"triple encoded", "group/%252574oken%25253Dtriple-secret", "https://github.com/group/%252574oken%25253Dtriple-secret.git", "triple-secret", false},
		{"nested URL", "group/project?next=https%3A%2F%2Fgit.example%2Frepo%3Ftoken%3Dnested-secret", "https://github.com/group/project?next=https%3A%2F%2Fgit.example%2Frepo%3Ftoken%3Dnested-secret", "nested-secret", false},
		{"bare opaque", "group/token=bare-secret", "https://github.com/group/token=bare-secret.git", "bare-secret", false},
		{"malformed encoded delimiter", "group/token%3Dopaque-secret%ZZ", "https://github.com/group/token%3Dopaque-secret%ZZ.git", "opaque-secret", true},
		{"fused credential key", "group/apikey=opaque-secret", "https://github.com/group/apikey=opaque-secret.git", "opaque-secret", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.decodeIncomplete {
				if _, complete := decodedRemoteMetadataSpellings(tc.repo); complete {
					t.Fatalf("invalid escape was accepted as complete: %q", tc.repo)
				}
			}
			if !hasEmbeddedCredential(tc.repo) {
				t.Fatalf("direct classifier did not reject %q", tc.repo)
			}
			if redacted := redactURL(tc.url); strings.Contains(redacted, tc.secret) {
				t.Fatalf("direct redactURL leaked %q: %s", tc.secret, redacted)
			}
			result := Result{Status: evidence.StatusUnknown, Ports: []PortResult{{
				Remote:     Remote{Kind: RemoteGitHub, Repo: tc.repo, URL: tc.url},
				Candidates: []FetchCandidate{{Remote: Remote{Kind: RemoteGitLab, Repo: tc.repo, URL: tc.url}}},
				CompareURL: tc.url + "/-/compare/a...b",
				Evidence:   evidence.Evidence{Commands: []string{"git ls-remote " + tc.url}},
				Failure:    &PublicFailure{ID: FailureRemoteQueryFailed, Detail: "remote query failed"},
			}}}
			assertSecretsAbsentFromJSON(t, result, tc.secret)
			encoded, err := publicresult.MarshalIndent(result)
			if err != nil {
				t.Fatalf("publicresult.MarshalIndent: %v", err)
			}
			assertSecretsAbsent(t, string(encoded), tc.secret)
			assertSecretsAbsentFromJSON(t, result.PublicResultProjection(), tc.secret)
		})
	}

	clean := Result{Status: evidence.StatusOK, Ports: []PortResult{{
		Remote:   Remote{Kind: RemoteGitLab, Repo: "group/sub/project", URL: "https://gitlab.example/group/sub/project.git"},
		Evidence: evidence.Evidence{Commands: []string{"git ls-remote https://gitlab.example/group/sub/project.git"}},
	}}}
	cleanDoc, err := json.Marshal(clean)
	if err != nil {
		t.Fatalf("json.Marshal clean result: %v", err)
	}
	if !strings.Contains(string(cleanDoc), "group/sub/project") {
		t.Fatalf("ordinary remote metadata was unexpectedly redacted: %s", cleanDoc)
	}
}

func TestS2_CredentialKeyRecognitionCoversDelimitedAndFusedIdentifiers(t *testing.T) {
	for _, key := range []string{"access_token", "X-Api-Key", "apikey", "clientsecret", "authToken"} {
		if !keyNamesArgvSecret(key) {
			t.Fatalf("credential identifier %q was not recognized", key)
		}
	}
	for _, key := range []string{"design", "monkey", "depth"} {
		if keyNamesArgvSecret(key) {
			t.Fatalf("safe identifier %q was classified as credential-bearing", key)
		}
	}
}

func TestS2_ProducerRejectsSecretBearingRepoBeforeRemoteCall(t *testing.T) {
	tests := []struct {
		name     string
		portfile string
		secret   string
	}{
		{"fragment", `vcpkg_from_git(URL "https://host/widget.git#token=fragment-secret" REF ` + commitA + ` SHA512 0)`, "fragment-secret"},
		{"percent encoded repo", `vcpkg_from_github(REPO group/%74oken%3Dencoded-secret REF ` + commitA + ` SHA512 0)`, "encoded-secret"},
		{"triple encoded repo", `vcpkg_from_github(REPO group/%252574oken%25253Dtriple-secret REF ` + commitA + ` SHA512 0)`, "triple-secret"},
		{"nested URL", `vcpkg_from_git(URL "https://host/widget.git?next=https%3A%2F%2Fgit.example%2Frepo%3Ftoken%3Dnested-secret" REF ` + commitA + ` SHA512 0)`, "nested-secret"},
		{"bare opaque repo", `vcpkg_from_github(REPO group/token=bare-secret REF ` + commitA + ` SHA512 0)`, "bare-secret"},
		{"malformed encoded delimiter", `vcpkg_from_github(REPO group/token%3Dopaque-secret%ZZ REF ` + commitA + ` SHA512 0)`, "opaque-secret"},
		{"fused credential key", `vcpkg_from_github(REPO group/apikey=opaque-secret REF ` + commitA + ` SHA512 0)`, "opaque-secret"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := newPort(t, "credential-"+strings.ReplaceAll(tc.name, " ", "-"), tc.portfile)
			remoteCalls := 0
			result := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
				FS:  DefaultFS(),
				Now: fixedNow(),
				RemoteRefs: func(context.Context, approvedRemoteURL) (map[string]string, error) {
					remoteCalls++
					return nil, nil
				},
			})
			if remoteCalls != 0 {
				t.Fatalf("remote calls = %d, want 0 for credential carrier", remoteCalls)
			}
			if got := result.Ports[0].Reason; got != ReasonRemoteURLCredentialBearing {
				t.Fatalf("reason = %q, want %q", got, ReasonRemoteURLCredentialBearing)
			}
			assertSecretsAbsentFromJSON(t, result, tc.secret)
		})
	}
}

func TestS2_OpaqueRemoteMetadataDecodingFailsClosedAtBound(t *testing.T) {
	const secret = "beyond-bound-secret"
	raw := "group/token%3D" + secret
	for range maxRemoteMetadataDecodePasses {
		raw = url.PathEscape(raw)
	}
	if !hasEmbeddedCredential(raw) {
		t.Fatalf("opaque metadata beyond decode bound was not refused: %q", raw)
	}
	if redacted := redactURL(raw); strings.Contains(redacted, secret) {
		t.Fatalf("opaque metadata beyond decode bound leaked %q: %q", secret, redacted)
	}
	result := Result{Status: evidence.StatusUnknown, Ports: []PortResult{{
		Remote:   Remote{Kind: RemoteGitHub, Repo: raw, URL: "https://github.com/" + raw + ".git"},
		Evidence: evidence.Evidence{Commands: []string{"git ls-remote " + raw}},
	}}}
	assertSecretsAbsentFromJSON(t, result, secret)
}

func TestS2_CanonicalCredentialCarrierCoversAuthorityAndSafeControls(t *testing.T) {
	for _, raw := range []string{
		"owner/repo",
		"group/sub/project",
		"gitlab.example:8443/group/project",
		"[2001:db8::1]:8443/group/project",
		"git@host:owner/repo.git",
		"group/%2Fproject",
		"https://host/group/repo@v1.git",
	} {
		t.Run("safe "+raw, func(t *testing.T) {
			if hasEmbeddedCredential(raw) {
				t.Fatalf("safe remote metadata %q was classified as credential-bearing", raw)
			}
			if redacted := redactURL(raw); redacted != raw {
				t.Fatalf("safe remote metadata %q redacted to %q", raw, redacted)
			}
		})
	}

	for _, carrier := range []string{"user@host", "user:password@host"} {
		if !hasEmbeddedCredential(carrier) {
			t.Fatalf("authority carrier was not classified as credential-bearing: %q", carrier)
		}
		if redacted := redactURL(carrier); strings.Contains(redacted, carrier) {
			t.Fatalf("direct redaction leaked authority carrier: %q", redacted)
		}
	}
	const carrier = "user:password@host"
	for _, tc := range []struct {
		name     string
		portfile string
	}{
		{"github", `vcpkg_from_github(REPO ` + carrier + ` REF ` + commitA + ` SHA512 0)`},
		{"gitlab", `vcpkg_from_gitlab(GITLAB_URL https://gitlab.example REPO ` + carrier + ` REF ` + commitA + ` SHA512 0)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newPort(t, "authority-"+tc.name, tc.portfile)
			var calls int
			result := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
				FS:  DefaultFS(),
				Now: fixedNow(),
				RemoteRefs: func(context.Context, approvedRemoteURL) (map[string]string, error) {
					calls++
					return nil, nil
				},
			})
			if calls != 0 || result.Ports[0].Reason != ReasonRemoteURLCredentialBearing {
				t.Fatalf("calls/reason = %d/%q, want 0/%q", calls, result.Ports[0].Reason, ReasonRemoteURLCredentialBearing)
			}
			assertSecretsAbsentFromJSON(t, result, carrier)
		})
	}
}

func TestS2_SafeRemoteSpellingsRemainAdmissible(t *testing.T) {
	for _, remote := range []string{
		"https://host/group/sub/repo.git",
		"https://host:8443/group/repo.git",
		"https://[2001:db8::1]:8443/group/repo.git",
		"git@host:owner/repo.git",
	} {
		t.Run(remote, func(t *testing.T) {
			if hasEmbeddedCredential(remote) {
				t.Fatalf("safe remote was classified as credential-bearing: %q", remote)
			}
			if redacted := redactURL(remote); redacted != remote {
				t.Fatalf("safe remote redacted to %q, want %q", redacted, remote)
			}
			approved, reason := approveRemoteURL(remote)
			if reason != "" {
				t.Fatalf("safe remote admission reason = %q", reason)
			}
			if argument, ok := approved.transportArgument(); !ok || argument != remote {
				t.Fatalf("safe remote approval argument = %q/%t, want %q/true", argument, ok, remote)
			}
		})
	}
	for _, remote := range []string{
		"https://host/group/repo.git#design=dark",
	} {
		t.Run("redaction only "+remote, func(t *testing.T) {
			if hasEmbeddedCredential(remote) {
				t.Fatalf("safe remote was classified as credential-bearing: %q", remote)
			}
			if redacted := redactURL(remote); redacted == remote || strings.Contains(redacted, "design=dark") {
				t.Fatalf("fragment value remained emittable: %q", redacted)
			}
		})
	}
}

func TestS2_URLPathAtSignIsNotAuthorityCredential(t *testing.T) {
	const remote = "https://host/group/repo@v1.git"
	const command = "git ls-remote " + remote
	if hasEmbeddedCredential(remote) {
		t.Fatalf("safe URL path was classified as credential-bearing: %q", remote)
	}
	if redacted := redactURL(remote); redacted != remote {
		t.Fatalf("safe URL path redacted to %q, want %q", redacted, remote)
	}
	approved, reason := approveRemoteURL(remote)
	if reason != "" {
		t.Fatalf("safe URL path admission reason = %q", reason)
	}
	if argument, ok := approved.transportArgument(); !ok || argument != remote {
		t.Fatalf("safe URL path approval argument = %q/%t, want %q/true", argument, ok, remote)
	}
	if redacted := redactEvidenceCommand(command); redacted != command {
		t.Fatalf("safe command redacted to %q, want %q", redacted, command)
	}
}

func TestS2_EvidenceCommandsRedactRepoOnlyCarrierOnEveryPublicBoundary(t *testing.T) {
	const carrier = "user:password@host"
	result := Result{Status: evidence.StatusUnknown, Ports: []PortResult{{
		Remote:   Remote{Kind: RemoteGitHub, Repo: carrier, URL: "https://github.com/safe/repo.git"},
		Evidence: evidence.Evidence{Commands: []string{"git ls-remote " + carrier}},
		Failure:  &PublicFailure{ID: FailureRemoteQueryFailed, Detail: "remote query failed"},
	}}}
	assertSecretsAbsentFromJSON(t, result, carrier)
	encoded, err := publicresult.MarshalIndent(result)
	if err != nil {
		t.Fatalf("publicresult.MarshalIndent: %v", err)
	}
	assertSecretsAbsent(t, string(encoded), carrier)
	assertSecretsAbsentFromJSON(t, result.PublicResultProjection(), carrier)
}

func TestS2_EvidenceCommandsAreSanitizedIndependentlyOfRemote(t *testing.T) {
	const secret = "independent-command-secret"
	unsafeCommand := "git ls-remote https://user:" + secret + "@host/repo.git"
	for _, tc := range []struct {
		name    string
		remote  string
		command string
		secret  string
	}{
		{name: "empty remote", command: unsafeCommand, secret: secret},
		{name: "different remote", remote: "https://host/safe/repo.git", command: unsafeCommand, secret: secret},
		{name: "malformed encoded delimiter", command: "git ls-remote group/token%3Dmalformed-command-secret%ZZ", secret: "malformed-command-secret"},
		{name: "fused credential key", command: "git ls-remote group/apikey=fused-command-secret", secret: "fused-command-secret"},
		{name: "safe control", command: "git ls-remote https://host/group/repo@v1.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := Result{Status: evidence.StatusUnknown, Ports: []PortResult{{
				Remote:   Remote{Kind: RemoteGitHub, URL: tc.remote},
				Evidence: evidence.Evidence{Commands: []string{tc.command}},
			}}}
			redacted := redactResult(result).Ports[0].Evidence.Commands[0]
			if tc.secret != "" {
				if strings.Contains(redacted, tc.secret) {
					t.Fatalf("independent command leaked %q: %q", tc.secret, redacted)
				}
				assertSecretsAbsentFromJSON(t, result, tc.secret)
				return
			}
			if redacted != tc.command {
				t.Fatalf("safe command redacted to %q, want %q", redacted, tc.command)
			}
		})
	}
}

func assertSecretsAbsentFromJSON(t *testing.T, value any, secrets ...string) {
	t.Helper()
	document, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	assertSecretsAbsent(t, string(document), secrets...)
}

func assertSecretsAbsent(t *testing.T, document string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(document, secret) {
			t.Fatalf("secret %q leaked into public document: %s", secret, document)
		}
	}
}
