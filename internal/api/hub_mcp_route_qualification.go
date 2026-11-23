package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

const (
	hubRouteQualificationLeaf   = "hub-mcp-route-qualification.json"
	hubRouteQualificationSchema = "hub-mcp-route-qualification-v1"
	hubRouteWitnessKey          = "route_qualification_transaction_id"
	// Two canonical rows preserve exactly the prior and proposed terminal V3 owners.
	hubRouteQualificationLimit = len(`{"schema":"hub-mcp-route-qualification-v1","qualified":[,]}`) + 2*(len(`{"transaction_id":"","successor_sha256":"","scope_sha256":"","endpoint_authority_sha256":"","endpoint_provenance_sha256":""}`)+32+4*64)
	// Four required fields and both optional fields, with maximal Windows PID,
	// port, fractional UTC timestamp, instance and transaction widths.
	hubRouteEndpointLimit = len(`{"port":65535,"instance_id":"","pid":4294967295,"started_at":"2006-01-02T15:04:05.000000001Z","reconcile_pending":false,"route_qualification_transaction_id":""}`) + 64 + 32
)

var (
	errHubRouteStateUnavailable = errors.New("protected Hub Route state unavailable")
	errHubRouteStateChanged     = errors.New("protected Hub Route state differs from the selected preimage")
	errHubRouteQualifiedAck     = errors.New("qualified Hub reconcile requires the original protected endpoint selection")
)

// HubRouteQualification carries the narrow Product facts supplied under the
// original commit-decision fence, together with the H-owned endpoint digests.
// The record alone confers no Product or fixed-listener authority.
type HubRouteQualification struct {
	TransactionID            string `json:"transaction_id"`
	SuccessorSHA256          string `json:"successor_sha256"`
	ScopeSHA256              string `json:"scope_sha256"`
	EndpointAuthoritySHA256  string `json:"endpoint_authority_sha256"`
	EndpointProvenanceSHA256 string `json:"endpoint_provenance_sha256"`
}

type hubRouteQualificationRecord struct {
	Schema    string                  `json:"schema"`
	Qualified []HubRouteQualification `json:"qualified"`
}

type hubRouteEndpoint struct {
	HubEndpoint
	TransactionID string `json:"route_qualification_transaction_id,omitempty"`
}

type hubRouteLeaf struct {
	identity ConfigParentDirectoryIdentityV1
	data     []byte
	present  bool
}

func (f hubRouteLeaf) matches(other hubRouteLeaf) bool {
	return f.present == other.present && f.identity == other.identity && bytes.Equal(f.data, other.data)
}

// HubRouteEndpointSelection retains the exact namespace, endpoint object and
// bytes. A selection permits a reconcile ACK only in the live scope that read it.
type HubRouteEndpointSelection struct {
	owner    *hubRouteStateCore
	parent   ConfigParentDirectoryIdentityV1
	leaf     hubRouteLeaf
	endpoint hubRouteEndpoint
}

func (s HubRouteEndpointSelection) Endpoint() HubEndpoint { return s.endpoint.HubEndpoint }
func (s HubRouteEndpointSelection) Witness() string       { return s.endpoint.TransactionID }

func (s HubRouteEndpointSelection) AuthoritySHA256() string {
	return hubRouteProjection("hub-mcp-endpoint-authority-v1", struct {
		Port       int    `json:"port"`
		InstanceID string `json:"instance_id"`
	}{Port: s.endpoint.Port, InstanceID: s.endpoint.InstanceID})
}

func (s HubRouteEndpointSelection) ProvenanceSHA256() string {
	endpoint := s.endpoint
	endpoint.ReconcilePending = false
	return hubRouteProjection("hub-mcp-endpoint-provenance-v1", endpoint)
}

func hubRouteProjection(version string, shape any) string {
	// Only the fixed value-only projection structs above reach this encoder.
	encoded, _ := json.Marshal(shape)
	sum := sha256.Sum256(append([]byte(version+"\x00"), encoded...))
	return hex.EncodeToString(sum[:])
}

// HubRoutePublicationOutcome is the native readback result, independent of late
// close errors. Unknown gives no authority to retry, remove or restore data.
type HubRoutePublicationOutcome string

const (
	HubRoutePublicationApplied           HubRoutePublicationOutcome = "applied"
	HubRoutePublicationPreimageConfirmed HubRoutePublicationOutcome = "preimage-confirmed"
	HubRoutePublicationUnknown           HubRoutePublicationOutcome = "unknown"
)

// HubRoutePublicationError preserves the observed effect and the actual cause.
type HubRoutePublicationError struct {
	Outcome HubRoutePublicationOutcome
	Err     error
}

func (e *HubRoutePublicationError) Error() string {
	return fmt.Sprintf("protected Hub publication %s: %v", e.Outcome, e.Err)
}
func (e *HubRoutePublicationError) Unwrap() error { return e.Err }

// HubRouteState borrows the protected native parent and original H lock. The
// caller keeps its independently acquired Product/Route authority and acquires
// the existing settings lease only inside this callback scope.
type HubRouteState struct{ core *hubRouteStateCore }

type hubRouteStateCore struct {
	mu           sync.Mutex
	active       bool
	acknowledged bool
	native       hubRouteNative
	ctx          context.Context
	parent       ConfigParentDirectoryIdentityV1
	stateDir     string
}

// ObserveHubRouteStateDirectory reads the physical identity of the strict
// namespace. The Product or reconcile owner must bind it to its own selection.
func ObserveHubRouteStateDirectory(ctx context.Context, stateDir string) (ConfigParentDirectoryIdentityV1, error) {
	return observeHubRouteStateDirectory(ctx, stateDir)
}

// WithHubRouteState reopens the exact previously identified namespace and
// holds it and hub-mcp.lock until the synchronous callback returns.
func WithHubRouteState(ctx context.Context, stateDir string, expected ConfigParentDirectoryIdentityV1, run func(*HubRouteState) error) error {
	if ctx == nil || expected.FileID == ([16]byte{}) || run == nil {
		return errHubRouteStateUnavailable
	}
	return withHubRouteState(ctx, stateDir, expected, run)
}

func (s *HubRouteState) use(run func(*hubRouteStateCore) error) error {
	if s == nil || s.core == nil {
		return errHubRouteStateUnavailable
	}
	c := s.core
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active {
		return errHubRouteStateUnavailable
	}
	if err := c.ctx.Err(); err != nil {
		return err
	}
	if err := c.native.validate(); err != nil {
		return err
	}
	return run(c)
}

func (s *HubRouteState) ReadEndpoint() (selected HubRouteEndpointSelection, err error) {
	err = s.use(func(c *hubRouteStateCore) error {
		leaf, err := c.native.read(hubMcpEndpointFileLeaf, hubRouteEndpointLimit)
		if err != nil {
			return err
		}
		if !leaf.present {
			return os.ErrNotExist
		}
		endpoint, err := decodeHubRouteEndpoint(leaf.data)
		if err != nil {
			return err
		}
		selected = HubRouteEndpointSelection{owner: c, parent: c.parent, leaf: leaf, endpoint: endpoint}
		return nil
	})
	return selected, err
}

func (s *HubRouteState) ReadQualification(transactionID string) (row HubRouteQualification, found bool, err error) {
	if !hubRouteTransactionIDValid(transactionID) {
		return row, false, errHubRouteStateUnavailable
	}
	err = s.use(func(c *hubRouteStateCore) error {
		_, record, err := c.loadQualification()
		if err != nil {
			return err
		}
		row, found = record.find(transactionID)
		return nil
	})
	return row, found, err
}

func (c *hubRouteStateCore) loadQualification() (hubRouteLeaf, hubRouteQualificationRecord, error) {
	leaf, err := c.native.read(hubRouteQualificationLeaf, hubRouteQualificationLimit)
	if err != nil || !leaf.present {
		return leaf, hubRouteQualificationRecord{}, err
	}
	record, err := decodeHubRouteQualification(leaf.data)
	return leaf, record, err
}

// StageQualification preserves only the entry selected by the actual terminal
// prior receipt, then the new entry. An invalid protected preimage is preserved.
func (s *HubRouteState) StageQualification(priorTransactionID string, selected HubRouteEndpointSelection, next HubRouteQualification) error {
	if !next.valid() || priorTransactionID != "" && (!hubRouteTransactionIDValid(priorTransactionID) || priorTransactionID == next.TransactionID) {
		return errHubRouteStateUnavailable
	}
	return s.use(func(c *hubRouteStateCore) error {
		if err := c.matchEndpoint(selected); err != nil {
			return err
		}
		if selected.AuthoritySHA256() != next.EndpointAuthoritySHA256 || selected.ProvenanceSHA256() != next.EndpointProvenanceSHA256 {
			return errHubRouteStateChanged
		}
		if err := c.native.checkAbsent(hubMcpControlTokenFileLeaf); err != nil {
			return err
		}
		leaf, old, err := c.loadQualification()
		if err != nil {
			return err
		}
		if existing, ok := old.find(next.TransactionID); ok && existing != next {
			return errHubRouteStateChanged
		}
		record := hubRouteQualificationRecord{Schema: hubRouteQualificationSchema}
		if prior, found := old.find(priorTransactionID); found {
			record.Qualified = append(record.Qualified, prior)
		}
		record.Qualified = append(record.Qualified, next)
		payload, err := json.Marshal(record)
		if err != nil || bytes.Equal(payload, leaf.data) {
			return err
		}
		return c.native.publish(c.ctx, hubRouteQualificationLeaf, leaf, payload, hubRouteQualificationLimit)
	})
}

// CheckReloadTokenAbsent proves absence in the same held namespace, with no
// token read, creation or mutation. The fixed listener owner supplies socket proof.
func (s *HubRouteState) CheckReloadTokenAbsent() error {
	return s.use(func(c *hubRouteStateCore) error { return c.native.checkAbsent(hubMcpControlTokenFileLeaf) })
}

func (c *hubRouteStateCore) matchQualification(selected HubRouteEndpointSelection, qualification HubRouteQualification) error {
	if err := c.matchEndpoint(selected); err != nil {
		return err
	}
	_, record, err := c.loadQualification()
	if err != nil {
		return err
	}
	row, found := record.find(qualification.TransactionID)
	if !found || row != qualification || selected.AuthoritySHA256() != row.EndpointAuthoritySHA256 ||
		(selected.ProvenanceSHA256() != row.EndpointProvenanceSHA256 && selected.Witness() != row.TransactionID) {
		return errHubRouteStateChanged
	}
	return nil
}

func (c *hubRouteStateCore) publishEndpoint(selected HubRouteEndpointSelection, transactionID string) (HubEndpoint, error) {
	next := selected.endpoint
	next.TransactionID = transactionID
	next.PID = os.Getpid()
	next.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	payload, err := json.Marshal(next)
	if err != nil {
		return HubEndpoint{}, err
	}
	if err := c.native.publish(c.ctx, hubMcpEndpointFileLeaf, selected.leaf, payload, hubRouteEndpointLimit); err != nil {
		return HubEndpoint{}, err
	}
	return next.HubEndpoint, nil
}

func (c *hubRouteStateCore) matchEndpoint(selected HubRouteEndpointSelection) error {
	if selected.owner == nil || selected.parent != c.parent || !selected.leaf.present {
		return errHubRouteStateChanged
	}
	current, err := c.native.read(hubMcpEndpointFileLeaf, hubRouteEndpointLimit)
	if err != nil {
		return err
	}
	if !current.matches(selected.leaf) {
		return errHubRouteStateChanged
	}
	return nil
}

func acknowledgeProtectedHubEndpoint(selected HubRouteEndpointSelection) error {
	return (&HubRouteState{core: selected.owner}).use(func(c *hubRouteStateCore) error {
		if err := c.matchEndpoint(selected); err != nil {
			return &HubRoutePublicationError{Outcome: HubRoutePublicationUnknown, Err: err}
		}
		if !selected.endpoint.ReconcilePending {
			return nil
		}
		if selected.Witness() != "" {
			_, qualifications, err := c.loadQualification()
			if err != nil {
				return err
			}
			row, found := qualifications.find(selected.Witness())
			if !found || row.EndpointAuthoritySHA256 != selected.AuthoritySHA256() {
				return errHubRouteStateChanged
			}
		}
		next := selected.endpoint
		next.ReconcilePending = false
		payload, err := json.Marshal(next)
		if err != nil {
			return err
		}
		err = c.native.publish(c.ctx, hubMcpEndpointFileLeaf, selected.leaf, payload, hubRouteEndpointLimit)
		var result *HubRoutePublicationError
		if err == nil || errors.As(err, &result) && result.Outcome == HubRoutePublicationApplied {
			c.acknowledged = true
		}
		return err
	})
}

func decodeHubRouteEndpoint(data []byte) (ep hubRouteEndpoint, err error) {
	err = decodeHubRouteObject(data, &ep, []string{"port", "instance_id", "pid", "started_at"}, "reconcile_pending", hubRouteWitnessKey)
	stamp, stampErr := time.Parse(time.RFC3339Nano, ep.StartedAt)
	if err != nil || ep.Port < 1 || ep.Port > 65535 || ep.PID < 1 || uint64(ep.PID) > uint64(^uint32(0)) ||
		!isLowerHex64(ep.InstanceID) || stampErr != nil || stamp.UTC().Format(time.RFC3339Nano) != ep.StartedAt {
		return hubRouteEndpoint{}, fmt.Errorf("invalid protected Hub endpoint: %w", errors.Join(errHubRouteStateUnavailable, err))
	}
	// An omitted witness is valid legacy provenance. An explicit empty or
	// malformed witness does not turn a qualified record into a legacy one.
	if ep.TransactionID == "" {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(data, &fields) // decodeHubRouteObject proved the shape.
		if _, exists := fields[hubRouteWitnessKey]; exists {
			return hubRouteEndpoint{}, errHubRouteStateUnavailable
		}
	} else if !hubRouteTransactionIDValid(ep.TransactionID) {
		return hubRouteEndpoint{}, errHubRouteStateUnavailable
	}
	return ep, nil
}

func decodeHubRouteQualification(data []byte) (hubRouteQualificationRecord, error) {
	var wire struct {
		Schema    string            `json:"schema"`
		Qualified []json.RawMessage `json:"qualified"`
	}
	if err := decodeHubRouteObject(data, &wire, []string{"schema", "qualified"}); err != nil || wire.Schema != hubRouteQualificationSchema || len(wire.Qualified) < 1 || len(wire.Qualified) > 2 {
		return hubRouteQualificationRecord{}, fmt.Errorf("invalid Hub Route qualification: %w", errors.Join(errHubRouteStateUnavailable, err))
	}
	result := hubRouteQualificationRecord{Schema: wire.Schema}
	for _, raw := range wire.Qualified {
		var row HubRouteQualification
		if err := decodeHubRouteObject(raw, &row, []string{"transaction_id", "successor_sha256", "scope_sha256", "endpoint_authority_sha256", "endpoint_provenance_sha256"}); err != nil || !row.valid() {
			return hubRouteQualificationRecord{}, fmt.Errorf("invalid Hub Route qualification row: %w", errors.Join(errHubRouteStateUnavailable, err))
		}
		if _, duplicate := result.find(row.TransactionID); duplicate {
			return hubRouteQualificationRecord{}, errHubRouteStateUnavailable
		}
		result.Qualified = append(result.Qualified, row)
	}
	return result, nil
}

func decodeHubRouteObject(data []byte, target any, required []string, optional ...string) error {
	if !json.Valid(data) {
		return errHubRouteStateUnavailable
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	first, err := dec.Token()
	if err != nil || first != json.Delim('{') {
		return errHubRouteStateUnavailable
	}
	seen := make(map[string]bool, len(required)+len(optional))
	for _, name := range required {
		seen[name] = false
	}
	for _, name := range optional {
		seen[name] = false
	}
	for dec.More() {
		token, err := dec.Token()
		key, stringKey := token.(string)
		visited, known := seen[key]
		if err != nil || !stringKey || !known || visited {
			return errHubRouteStateUnavailable
		}
		seen[key] = true
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return err
		}
		if bytes.Equal(value, []byte("null")) {
			return errHubRouteStateUnavailable
		}
	}
	for _, name := range required {
		if !seen[name] {
			return errHubRouteStateUnavailable
		}
	}
	return json.Unmarshal(data, target)
}

func (r hubRouteQualificationRecord) find(id string) (HubRouteQualification, bool) {
	for _, row := range r.Qualified {
		if row.TransactionID == id {
			return row, true
		}
	}
	return HubRouteQualification{}, false
}

func (r HubRouteQualification) valid() bool {
	return hubRouteTransactionIDValid(r.TransactionID) && isLowerHex64(r.SuccessorSHA256) && isLowerHex64(r.ScopeSHA256) &&
		isLowerHex64(r.EndpointAuthoritySHA256) && isLowerHex64(r.EndpointProvenanceSHA256)
}

func hubRouteTransactionIDValid(id string) bool {
	var decoded [16]byte
	if len(id) != 2*len(decoded) {
		return false
	}
	_, err := hex.Decode(decoded[:], []byte(id))
	return err == nil && hex.EncodeToString(decoded[:]) == id
}
