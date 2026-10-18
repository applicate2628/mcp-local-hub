package clients

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCodexConfiguredAuthUsesNativeRefreshBranches(t *testing.T) {
	clock := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	jwt := func(payload string) string {
		return "e30." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".synthetic"
	}
	for _, row := range []struct {
		name, access, plan string
		last               *time.Time
		nullLast           bool
		invalidLast        string
		accessLineBreak    string
		budget             time.Duration
		refuse             bool
	}{
		{name: "expiry equality at budget", access: fmt.Sprintf(`{"exp":%d}`, clock.Add(5*time.Minute+120*time.Second).Unix()), plan: "plus", budget: 120 * time.Second, refuse: true},
		{name: "expiry beyond budget", access: fmt.Sprintf(`{"exp":%d}`, clock.Add(5*time.Minute+121*time.Second).Unix()), plan: "plus", budget: 120 * time.Second},
		{name: "fallback equality", access: `{}`, plan: "plus", last: configuredAuthTime(clock.Add(-8 * 24 * time.Hour))},
		{name: "fallback stale", access: `{"exp":9223372036854775807}`, plan: "plus", last: configuredAuthTime(clock.Add(-8*24*time.Hour - time.Second)), refuse: true},
		{name: "native lower expiry bound", access: `{"exp":-8334601228800}`, plan: "plus", refuse: true},
		{name: "below native lower bound", access: `{"exp":-8334601228801}`, plan: "plus"},
		{name: "native upper expiry bound", access: `{"exp":8210266876799}`, plan: "plus"},
		{name: "above native upper bound", access: `{"exp":8210266876800}`, plan: "plus", last: configuredAuthTime(clock.Add(-9 * 24 * time.Hour)), refuse: true},
		{name: "missing fallback refresh time", access: `{"exp":"invalid"}`, plan: "plus"},
		{name: "null fallback refresh time", access: `{}`, plan: "plus", nullLast: true},
		{name: "native invalid access payload retains known None", access: fmt.Sprintf(`{"exp":%d}`, clock.Add(time.Hour).Unix()), plan: "plus", nullLast: true, accessLineBreak: "\r\n"},
		{name: "malformed supplied refresh date", access: fmt.Sprintf(`{"exp":%d}`, clock.Add(time.Hour).Unix()), plan: "plus", invalidLast: "not-a-date", refuse: true},
		{name: "unknown plan case", access: fmt.Sprintf(`{"exp":%d}`, clock.Add(time.Hour).Unix()), plan: "Plus", refuse: true},
		{name: "unknown duplicate cannot hide near expiry", access: fmt.Sprintf(`{"exp":%d,"unknown":1,"unknown":2}`, clock.Add(time.Minute).Unix()), plan: "plus", refuse: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			accessToken := jwt(row.access)
			if row.accessLineBreak != "" {
				accessToken = strings.Replace(accessToken, "e30.", "e30."+row.accessLineBreak, 1)
			}
			object := map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{
				"id_token":     jwt(fmt.Sprintf(`{"https://api.openai.com/auth":{"chatgpt_plan_type":%q}}`, row.plan)),
				"access_token": accessToken, "refresh_token": "synthetic-never-used",
			}}
			if row.last != nil {
				object["last_refresh"] = row.last.Format(time.RFC3339Nano)
			}
			if row.nullLast {
				object["last_refresh"] = nil
			}
			if row.invalidLast != "" {
				object["last_refresh"] = row.invalidLast
			}
			raw, err := json.Marshal(object)
			if err != nil {
				t.Fatal(err)
			}
			if err := codexConfiguredAuthNonrefreshing(raw, clock, row.budget); (err != nil) != row.refuse {
				t.Fatalf("native branch refusal=%t, expected=%t", err != nil, row.refuse)
			}
		})
	}
}

func TestCodexConfiguredJWTEnvelopeMatchesNativeOrdinaryClaims(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1800000000}`))
	if expiry, present := codexConfiguredJWTExpiry("header." + payload + ".signature.extra"); !present || expiry != 1800000000 {
		t.Fatal("ordinary native claims incorrectly used membership's component count")
	}
	if _, present := codexConfiguredJWTExpiry("header." + payload + "=.signature"); present {
		t.Fatal("native RequireNone padding accepted")
	}
	// e31 decodes the same last byte as e30 only when nonzero trailing bits
	// are tolerated. Native base640.22.1 disallows those bits.
	if _, err := codexConfiguredJWTPayload("header.e31.signature"); err == nil {
		t.Fatal("noncanonical base64 trailing bits accepted")
	}
	for _, character := range []string{"\r", "\n"} {
		if _, err := codexConfiguredJWTPayload("header." + character + payload + ".signature"); err == nil {
			t.Fatal("native-invalid payload CR/LF was normalized")
		}
	}
}

func configuredAuthTime(value time.Time) *time.Time { return &value }
