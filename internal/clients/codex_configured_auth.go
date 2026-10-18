package clients

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// Protective read eligibility follows the one supported native definition.
// This parses local metadata only, never authenticates/refreshes or publishes
// token payloads. The original file bytes stay operation-local/private.
func codexConfiguredAuthNonrefreshing(raw []byte, clock time.Time, budget time.Duration) error {
	if clock.IsZero() || budget < 0 || budget > codexProviderInventoryTimeout {
		return errors.New("native authentication clock or budget invalid")
	}
	object, err := codexConfiguredObject(raw)
	if err != nil {
		return errors.New("native authentication object invalid")
	}
	for key := range object {
		switch key {
		case "auth_mode", "OPENAI_API_KEY", "tokens", "last_refresh", "agent_identity", "personal_access_token", "bedrock_api_key", "bedrock_access_keys":
		default:
			return errors.New("native authentication contributor unknown")
		}
	}
	for _, key := range []string{"OPENAI_API_KEY", "agent_identity", "personal_access_token", "bedrock_api_key", "bedrock_access_keys"} {
		if value, present := object[key]; present && !bytes.Equal(value, []byte("null")) {
			return errors.New("native authentication source unsupported")
		}
	}
	if rawMode, present := object["auth_mode"]; present && !bytes.Equal(rawMode, []byte("null")) {
		var mode string
		if json.Unmarshal(rawMode, &mode) != nil || mode != "chatgpt" {
			return errors.New("native authentication mode unsupported")
		}
	}
	tokens, err := codexConfiguredObject(object["tokens"])
	if err != nil {
		return errors.New("native authentication tokens unavailable")
	}
	for key := range tokens {
		switch key {
		case "id_token", "access_token", "refresh_token", "account_id":
		default:
			return errors.New("native token metadata contributor unknown")
		}
	}
	if accountID, present := tokens["account_id"]; present && !codexConfiguredNullableString(accountID) {
		return errors.New("native account binding type invalid")
	}
	var refreshed *time.Time
	if refreshRaw, present := object["last_refresh"]; present && !bytes.Equal(refreshRaw, []byte("null")) {
		var refreshText string
		if json.Unmarshal(refreshRaw, &refreshText) != nil {
			return errors.New("native refresh metadata invalid")
		}
		value, err := time.Parse(time.RFC3339Nano, refreshText)
		if err != nil {
			return errors.New("native refresh metadata invalid")
		}
		refreshed = &value
	}
	var idToken, accessToken, refreshToken string
	if json.Unmarshal(tokens["id_token"], &idToken) != nil || idToken == "" || json.Unmarshal(tokens["access_token"], &accessToken) != nil || accessToken == "" || json.Unmarshal(tokens["refresh_token"], &refreshToken) != nil || refreshToken == "" {
		return errors.New("native token metadata incomplete")
	}
	claims, err := codexConfiguredJWTObject(idToken)
	if err != nil {
		return errors.New("native identity metadata invalid")
	}
	if email, present := claims["email"]; present && !codexConfiguredNullableString(email) {
		return errors.New("native identity metadata type invalid")
	}
	if profileRaw, present := claims["https://api.openai.com/profile"]; present && !bytes.Equal(profileRaw, []byte("null")) {
		profile, err := codexConfiguredObject(profileRaw)
		if err != nil {
			return errors.New("native identity profile invalid")
		}
		if email, present := profile["email"]; present && !codexConfiguredNullableString(email) {
			return errors.New("native identity profile type invalid")
		}
	}
	account, err := codexConfiguredObject(claims["https://api.openai.com/auth"])
	if err != nil {
		return errors.New("native account metadata unavailable")
	}
	for _, key := range []string{"chatgpt_user_id", "user_id", "chatgpt_account_id"} {
		if value, present := account[key]; present && !codexConfiguredNullableString(value) {
			return errors.New("native identity claim type invalid")
		}
	}
	if rawBool, present := account["chatgpt_account_is_fedramp"]; present {
		var value bool
		if bytes.Equal(rawBool, []byte("null")) || json.Unmarshal(rawBool, &value) != nil {
			return errors.New("native identity claim type invalid")
		}
	}
	var plan string
	if json.Unmarshal(account["chatgpt_plan_type"], &plan) != nil {
		return errors.New("native plan metadata unavailable")
	}
	// These are exactly KnownPlan's six case-sensitive serde spellings that
	// are not workspace accounts. Unknown and workspace classes refuse.
	switch plan {
	case "free", "go", "plus", "pro", "prolite", "promax":
	default:
		return errors.New("native cloud eligibility unsupported")
	}
	if expires, present := codexConfiguredJWTExpiry(accessToken); present {
		// Both supported native source Cargo locks use Chrono0.4.43. Its
		// from_timestamp(exp,0) accepts years -262143..262142. Out-of-range
		// conversion is None and follows the same last_refresh fallback.
		if expires >= -8334601228800 && expires <= 8210266876799 {
			if !time.Unix(expires, 0).After(clock.Add(budget).Add(5 * time.Minute)) {
				return errors.New("native authentication would refresh during read")
			}
			return nil
		}
	}
	// Native missing/unparseable expiry uses last_refresh. No last_refresh is
	// nonrefreshing in the pinned producer; a malformed supplied date refuses.
	if refreshed != nil && refreshed.Before(clock.Add(budget).Add(-8*24*time.Hour)) {
		return errors.New("native authentication would refresh during read")
	}
	return nil
}

func codexConfiguredJWTObject(token string) (map[string]json.RawMessage, error) {
	payload, err := codexConfiguredJWTPayload(token)
	if err != nil {
		return nil, err
	}
	return codexConfiguredObject(payload)
}

func codexConfiguredJWTPayload(token string) ([]byte, error) {
	parts := strings.Split(token, ".")
	// The native ordinary-claims decoder consumes the first three nonempty
	// components, without membership verification's separate exact-count rule.
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" || len(parts[1]) > 1<<20 {
		return nil, errors.New("native token metadata shape invalid")
	}
	// Go's strict base64 decoder still ignores CR/LF. Native URL_SAFE_NO_PAD
	// rejects those payload bytes, so do not normalize them into a valid expiry.
	if strings.ContainsAny(parts[1], "\r\n") {
		return nil, errors.New("native token metadata alphabet invalid")
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || !utf8.Valid(payload) {
		return nil, errors.New("native token metadata encoding invalid")
	}
	return payload, nil
}

// StandardJwtClaims serde ignores unknown fields, including repeated unknown
// keys, while a duplicate/mistyped known exp field makes parsing fail. Applying
// the configured-DTO duplicate policy here could falsely enter refresh fallback.
func codexConfiguredJWTExpiry(token string) (int64, bool) {
	payload, err := codexConfiguredJWTPayload(token)
	if err != nil {
		return 0, false
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return 0, false
	}
	seen, present := false, false
	var expires int64
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return 0, false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return 0, false
		}
		if key == "exp" {
			if seen {
				return 0, false
			}
			seen = true
			if !bytes.Equal(value, []byte("null")) {
				if json.Unmarshal(value, &expires) != nil {
					return 0, false
				}
				present = true
			}
		}
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') || decoder.Decode(new(json.RawMessage)) != io.EOF {
		return 0, false
	}
	return expires, present
}
