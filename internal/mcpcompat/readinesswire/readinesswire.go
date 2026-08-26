// Package readinesswire owns the bounded versioned HTTP failure envelope used
// between daemon hosts and API readiness consumers. It deliberately contains
// no server-specific compatibility policy.
package readinesswire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	MediaTypeV1  = "application/vnd.mcp-local-hub.readiness-failure+json; version=1; charset=utf-8"
	mediaBaseV1  = "application/vnd.mcp-local-hub.readiness-failure+json"
	MaxBodyBytes = 2048

	FailureBackingProtocolUnsupported = "MCP_BACKING_PROTOCOL_UNSUPPORTED"
	FailureChildProtocolMismatch      = "MCP_COMPAT_CHILD_PROTOCOL_MISMATCH"
	FailureCapabilityDrift            = "MCP_COMPAT_CAPABILITY_DRIFT"
	FailureToolCatalogDrift           = "MCP_COMPAT_TOOL_CATALOG_DRIFT"
	FailureIDCorrelationFailed        = "MCP_COMPAT_ID_CORRELATION_FAILED"
	FailureChildResponseInvalid       = "MCP_COMPAT_CHILD_RESPONSE_INVALID"
	FailureReadinessChildExited       = "MCP_READINESS_CHILD_EXITED"
	FailureReadinessHostUnavailable   = "MCP_READINESS_HOST_UNAVAILABLE"
	FailureReadinessTimeout           = "MCP_READINESS_TIMEOUT"

	// Consumer-only classifications are never accepted by WriteFailure.
	FailureWireInvalid           = "MCP_READINESS_FAILURE_WIRE_INVALID"
	FailureHTTPErrorBodyUnusable = "MCP_READINESS_HTTP_ERROR_BODY_UNUSABLE"

	StageProfile     = "profile"
	StageIdentity    = "identity"
	StageInitialize  = "initialize"
	StageInitialized = "initialized"
	StageToolsList   = "tools_list"
	StageToolCall    = "tool_call"
	StageResponse    = "response"
	StageHost        = "host"
)

const emergencyBodyV1 = "{\"schema_version\":\"mcp-readiness-failure-v1\",\"failure_id\":\"MCP_READINESS_HOST_UNAVAILABLE\",\"stage\":\"host\",\"http_status\":503,\"retryable\":true,\"manifest_name\":\"\",\"daemon_name\":\"\",\"requested_protocol\":\"\",\"negotiated_protocol\":\"\",\"supported_floor\":\"\",\"profile_id\":\"\"}\n"

type failureRule struct {
	status    int
	retryable bool
}

var failureRules = map[string]failureRule{
	FailureBackingProtocolUnsupported: {http.StatusBadGateway, false},
	FailureChildProtocolMismatch:      {http.StatusBadGateway, false},
	FailureCapabilityDrift:            {http.StatusBadGateway, false},
	FailureToolCatalogDrift:           {http.StatusBadGateway, false},
	FailureIDCorrelationFailed:        {http.StatusBadGateway, false},
	FailureChildResponseInvalid:       {http.StatusBadGateway, false},
	FailureReadinessChildExited:       {http.StatusBadGateway, true},
	FailureReadinessHostUnavailable:   {http.StatusServiceUnavailable, true},
	FailureReadinessTimeout:           {http.StatusGatewayTimeout, true},
}

var (
	identifierV1 = regexp.MustCompile(`^[A-Za-z0-9._-]{0,128}$`)
	legacyV1     = regexp.MustCompile(`^initialize negotiated unsupported protocol version \"(2024-11-05|2025-03-26|2025-06-18|2025-11-25)\"\n$`)
	protocolsV1  = map[string]struct{}{
		"2024-10-07": {},
		"2024-11-05": {},
		"2025-03-26": {},
		"2025-06-18": {},
		"2025-11-25": {},
	}
	stagesV1 = map[string]struct{}{
		StageProfile: {}, StageIdentity: {}, StageInitialize: {}, StageInitialized: {},
		StageToolsList: {}, StageToolCall: {}, StageResponse: {}, StageHost: {},
	}
)

// Failure is the typed, bounded producer and consumer projection. Empty
// identifier/revision strings mean not applicable.
type Failure struct {
	FailureID          string
	Stage              string
	HTTPStatus         int
	Retryable          bool
	ManifestName       string
	DaemonName         string
	RequestedProtocol  string
	NegotiatedProtocol string
	SupportedFloor     string
	ProfileID          string
}

type envelopeV1 struct {
	SchemaVersion      string `json:"schema_version"`
	FailureID          string `json:"failure_id"`
	Stage              string `json:"stage"`
	HTTPStatus         int    `json:"http_status"`
	Retryable          bool   `json:"retryable"`
	ManifestName       string `json:"manifest_name"`
	DaemonName         string `json:"daemon_name"`
	RequestedProtocol  string `json:"requested_protocol"`
	NegotiatedProtocol string `json:"negotiated_protocol"`
	SupportedFloor     string `json:"supported_floor"`
	ProfileID          string `json:"profile_id"`
}

func validateFailure(f Failure) error {
	rule, ok := failureRules[f.FailureID]
	if !ok {
		return fmt.Errorf("readinesswire: unregistered failure id %q", f.FailureID)
	}
	if f.HTTPStatus != rule.status || f.Retryable != rule.retryable {
		return fmt.Errorf("readinesswire: failure registry mismatch for %s", f.FailureID)
	}
	if _, ok := stagesV1[f.Stage]; !ok {
		return fmt.Errorf("readinesswire: invalid stage %q", f.Stage)
	}
	for name, value := range map[string]string{
		"manifest_name": f.ManifestName, "daemon_name": f.DaemonName, "profile_id": f.ProfileID,
	} {
		if !identifierV1.MatchString(value) {
			return fmt.Errorf("readinesswire: invalid %s", name)
		}
	}
	for name, value := range map[string]string{
		"requested_protocol":  f.RequestedProtocol,
		"negotiated_protocol": f.NegotiatedProtocol,
		"supported_floor":     f.SupportedFloor,
	} {
		if value == "" {
			continue
		}
		if _, ok := protocolsV1[value]; !ok {
			return fmt.Errorf("readinesswire: invalid %s", name)
		}
	}
	return nil
}

func envelopeFromFailure(f Failure) envelopeV1 {
	return envelopeV1{
		SchemaVersion: "mcp-readiness-failure-v1", FailureID: f.FailureID, Stage: f.Stage,
		HTTPStatus: f.HTTPStatus, Retryable: f.Retryable, ManifestName: f.ManifestName,
		DaemonName: f.DaemonName, RequestedProtocol: f.RequestedProtocol,
		NegotiatedProtocol: f.NegotiatedProtocol, SupportedFloor: f.SupportedFloor,
		ProfileID: f.ProfileID,
	}
}

func failureFromEnvelope(e envelopeV1) Failure {
	return Failure{
		FailureID: e.FailureID, Stage: e.Stage, HTTPStatus: e.HTTPStatus, Retryable: e.Retryable,
		ManifestName: e.ManifestName, DaemonName: e.DaemonName,
		RequestedProtocol: e.RequestedProtocol, NegotiatedProtocol: e.NegotiatedProtocol,
		SupportedFloor: e.SupportedFloor, ProfileID: e.ProfileID,
	}
}

// EncodeFailure returns the sole canonical v1 body form.
func EncodeFailure(f Failure) ([]byte, error) {
	if err := validateFailure(f); err != nil {
		return nil, err
	}
	body, err := json.Marshal(envelopeFromFailure(f))
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')
	if len(body) > MaxBodyBytes {
		return nil, errors.New("readinesswire: encoded body exceeds v1 limit")
	}
	return body, nil
}

// WriteFailure validates and encodes before committing headers.
func WriteFailure(w http.ResponseWriter, f Failure) error {
	body, err := EncodeFailure(f)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", MediaTypeV1)
	w.WriteHeader(f.HTTPStatus)
	_, err = w.Write(body)
	return err
}

// WriteEmergencyFailure writes the one compile-time constant fallback. It has
// no caller-controlled or free-form content.
func WriteEmergencyFailure(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", MediaTypeV1)
	w.WriteHeader(http.StatusServiceUnavailable)
	_, err := io.WriteString(w, emergencyBodyV1)
	return err
}

func invalidFailure(status int, id string) Failure {
	return Failure{FailureID: id, Stage: StageHost, HTTPStatus: status}
}

func claimsV1(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), mediaBaseV1)
}

func exactV1MediaType(contentType string) bool {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != mediaBaseV1 || len(params) != 2 {
		return false
	}
	return params["version"] == "1" && params["charset"] == "utf-8"
}

// DecodeFailureResponse is the only v1/legacy receiver. It never returns raw
// response content. Invalid claimed-v1 envelopes retain only status and class.
func DecodeFailureResponse(status int, contentType string, body io.Reader) Failure {
	isLegacy := contentType == "text/plain; charset=utf-8"
	if !isLegacy && !claimsV1(contentType) {
		return invalidFailure(status, FailureHTTPErrorBodyUnusable)
	}
	limit := int64(MaxBodyBytes)
	if isLegacy {
		limit = 256
	}
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		if claimsV1(contentType) {
			return invalidFailure(status, FailureWireInvalid)
		}
		return invalidFailure(status, FailureHTTPErrorBodyUnusable)
	}
	if isLegacy && status == http.StatusBadGateway && len(raw) <= 256 {
		match := legacyV1.FindSubmatch(raw)
		if len(match) == 2 {
			return Failure{
				FailureID: FailureBackingProtocolUnsupported, Stage: StageInitialize,
				HTTPStatus: status, NegotiatedProtocol: string(match[1]),
			}
		}
		return invalidFailure(status, FailureHTTPErrorBodyUnusable)
	}
	if !claimsV1(contentType) {
		return invalidFailure(status, FailureHTTPErrorBodyUnusable)
	}
	if len(raw) > MaxBodyBytes || !utf8.Valid(raw) || !exactV1MediaType(contentType) {
		return invalidFailure(status, FailureWireInvalid)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var env envelopeV1
	if err := decoder.Decode(&env); err != nil {
		return invalidFailure(status, FailureWireInvalid)
	}
	if decoder.Decode(new(any)) != io.EOF || env.SchemaVersion != "mcp-readiness-failure-v1" {
		return invalidFailure(status, FailureWireInvalid)
	}
	failure := failureFromEnvelope(env)
	if failure.HTTPStatus != status || validateFailure(failure) != nil {
		return invalidFailure(status, FailureWireInvalid)
	}
	canonical, err := EncodeFailure(failure)
	if err != nil || !bytes.Equal(raw, canonical) {
		return invalidFailure(status, FailureWireInvalid)
	}
	return failure
}
