package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

type isolatedStdioID struct {
	value  string
	string bool
}

func stdioPrivateID(raw json.RawMessage, maxBytes int) (isolatedStdioID, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > maxBytes {
		return isolatedStdioID{}, false
	}
	if raw[0] == '"' {
		var value string
		err := json.Unmarshal(raw, &value)
		return isolatedStdioID{value: value, string: true}, err == nil
	}
	for i, b := range raw {
		if (b < '0' || b > '9') && !(i == 0 && b == '-') {
			return isolatedStdioID{}, false
		}
	}
	if bytes.Equal(raw, []byte("-0")) {
		return isolatedStdioID{value: "0"}, true
	}
	return isolatedStdioID{value: string(raw)}, true
}

// A row stays until the session retires. A cancel-first fence is never evicted
// or replaced with a later request carrying the same typed ID.
type isolatedStdioOperation struct {
	childID                      int64
	seen, canceled, handled      bool
	writeAdmitted, writeReceived bool
	written                      int
	writeError                   error
	cancelSignal                 chan struct{}
}

func (s *isolatedStdioSession) operationLocked(id isolatedStdioID) *isolatedStdioOperation {
	if op := s.requests[id]; op != nil {
		return op
	}
	if len(s.requests) == s.owner.limits.MaxRequestIDs {
		s.retireLocked(time.Now())
		return nil
	}
	op := &isolatedStdioOperation{cancelSignal: make(chan struct{})}
	s.requests[id] = op
	return op
}

func (s *isolatedStdioSession) beginPOST(w http.ResponseWriter, r *http.Request, post stdioPOST) (*isolatedStdioOperation, bool) {
	raw, hasID := post.msg["id"]
	if string(post.msg["jsonrpc"]) != `"2.0"` || post.method == "" {
		http.Error(w, "invalid JSON-RPC message", http.StatusBadRequest)
		return nil, false
	}
	if post.method == "notifications/cancelled" {
		if hasID {
			w.WriteHeader(http.StatusAccepted)
		} else {
			s.cancelPOST(w, r, post.msg["params"])
		}
		return nil, false
	}
	if !hasID || post.method == "initialize" {
		return nil, true
	}
	id, ok := stdioPrivateID(raw, s.owner.limits.MaxIDBytes)
	if !ok {
		http.Error(w, "invalid request id", http.StatusBadRequest)
		return nil, false
	}
	p := s.owner
	p.mu.Lock()
	if !s.activeLocked() || !p.allowed(r, s.role) {
		p.mu.Unlock()
		http.Error(w, "unknown session id", http.StatusNotFound)
		return nil, false
	}
	isInitializeID := id == s.initializeID
	var op *isolatedStdioOperation
	if !isInitializeID {
		op = s.operationLocked(id)
	}
	switch {
	case isInitializeID || op != nil && op.seen:
		p.mu.Unlock()
		http.Error(w, "request id already used", http.StatusBadRequest)
	case op == nil:
		p.mu.Unlock()
		http.Error(w, "unknown session id", http.StatusNotFound)
	default:
		op.seen = true
		canceled := op.canceled
		p.mu.Unlock()
		if canceled {
			http.Error(w, "request canceled", http.StatusRequestTimeout)
			return nil, false
		}
		return op, true
	}
	return nil, false
}

func (s *isolatedStdioSession) finishOperation(op *isolatedStdioOperation) {
	s.owner.mu.Lock()
	op.handled = true
	s.owner.mu.Unlock()
}

func (s *isolatedStdioSession) cancelPOST(w http.ResponseWriter, r *http.Request, raw json.RawMessage) {
	var params struct {
		ID json.RawMessage `json:"requestId"`
	}
	p := s.owner
	if err := json.Unmarshal(raw, &params); err != nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	id, ok := stdioPrivateID(params.ID, p.limits.MaxIDBytes)
	if !ok {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var childID int64
	p.mu.Lock()
	if s.activeLocked() && p.allowed(r, s.role) && id != s.initializeID {
		op := s.operationLocked(id)
		if op != nil && !op.canceled {
			op.canceled = true
			close(op.cancelSignal)
			if op.writeAdmitted && (!op.handled || !op.writeReceived) {
				childID = op.childID
			}
		}
	}
	retired := !s.stopDeadline.IsZero()
	p.mu.Unlock()
	if retired {
		http.Error(w, "unknown session id", http.StatusNotFound)
		return
	}
	if childID != 0 {
		// Forward only the trusted rewritten ID. The upstream reason may
		// contain arbitrary MIME/UI/user text and is deliberately omitted.
		body, err := json.Marshal(struct {
			JSONRPC string           `json:"jsonrpc"`
			Method  string           `json:"method"`
			Params  map[string]int64 `json:"params"`
		}{"2.0", "notifications/cancelled", map[string]int64{"requestId": childID}})
		if err == nil {
			_ = s.deliver(r, body, nil, 0)
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

var (
	errStdioSIDRetired = errors.New("private stdio session retired")
	errStdioCanceled   = errors.New("private stdio request canceled")
)

func (s *isolatedStdioSession) writePOST(w http.ResponseWriter, r *http.Request, body []byte, op *isolatedStdioOperation, childID int64) bool {
	err := s.deliver(r, body, op, childID)
	switch {
	case err == nil:
		return true
	case errors.Is(err, errStdioSIDRetired):
		http.Error(w, "unknown session id", http.StatusNotFound)
	case errors.Is(err, errStdioCanceled):
		http.Error(w, "request canceled", http.StatusRequestTimeout)
	default:
		http.Error(w, "provider input failed", http.StatusBadGateway)
	}
	return false
}

// Serialize on the actual child writer. The gate is checked after waiting
// for the writer and immediately before admitting it to the retained stdin.
func (s *isolatedStdioSession) deliver(r *http.Request, body []byte, op *isolatedStdioOperation, childID int64) error {
	ctx := r.Context()
	var canceled <-chan struct{}
	if op != nil {
		canceled = op.cancelSignal
	}
	select {
	case s.written <- struct{}{}:
	case <-ctx.Done():
		return errStdioCanceled
	case <-canceled:
		return errStdioCanceled
	case <-s.revoked:
		return errStdioSIDRetired
	}
	p := s.owner
	p.mu.Lock()
	var denied error
	input, _ := s.host.stdin.(*stdioOriginalInput)
	switch {
	case !s.activeLocked() || !p.allowed(r, s.role):
		denied = errStdioSIDRetired
	case ctx.Err() != nil || op != nil && op.canceled:
		denied = errStdioCanceled
	case input == nil || !input.admitWrite():
		denied = io.ErrClosedPipe
	}
	if denied != nil {
		p.mu.Unlock()
		<-s.written
		return denied
	}
	if op != nil {
		op.childID, op.writeAdmitted = childID, true
	}
	p.mu.Unlock()

	// Closing the request only stops observation of a committed write.
	// A genuinely stuck write uses the independently qualified liveness
	// budget and retires only this session through the original Job.
	received := make(chan error, 1)
	writeReceived := make(chan struct{})
	timer := time.AfterFunc(p.limits.Operation, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		select {
		case <-writeReceived:
		default:
			s.retireLocked(time.Now())
		}
	})
	go func() {
		defer input.writers.Done()
		line := stdioLine(body)
		n, err := input.file.file.Write(line)
		if err == nil && n != len(line) {
			err = io.ErrShortWrite
		}
		p.mu.Lock()
		if op != nil {
			op.written, op.writeError, op.writeReceived = n, err, true
		}
		close(writeReceived)
		if err != nil {
			s.retireLocked(time.Now())
		}
		p.mu.Unlock()
		timer.Stop()
		received <- err
		<-s.written
	}()
	select {
	case err := <-received:
		return err
	case <-ctx.Done():
		return errStdioCanceled
	case <-s.revoked:
		return errStdioSIDRetired
	}
}

func (s *isolatedStdioSession) resultAllowed(r *http.Request) bool {
	p := s.owner
	p.mu.Lock()
	defer p.mu.Unlock()
	return r.Context().Err() == nil && s.activeLocked() && p.allowed(r, s.role)
}

func sanitizeStdioPrivateResponse(method string, body []byte) []byte {
	var msg map[string]json.RawMessage
	if json.Unmarshal(body, &msg) != nil {
		return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"provider response invalid"}}`)
	}
	if raw := msg["error"]; len(raw) != 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return privateErrorResponse(msg["id"])
	}
	if method == "tools/call" {
		var result struct {
			IsError bool `json:"isError"`
		}
		if json.Unmarshal(msg["result"], &result) == nil && result.IsError {
			out, _ := json.Marshal(map[string]json.RawMessage{
				"jsonrpc": json.RawMessage(`"2.0"`),
				"id":      msg["id"],
				"result":  json.RawMessage(`{"isError":true,"content":[{"type":"text","text":"provider operation failed"}]}`),
			})
			return out
		}
	}
	return body
}

func privateErrorResponse(id json.RawMessage) []byte {
	out, _ := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   json.RawMessage `json:"error"`
	}{"2.0", id, json.RawMessage(`{"code":-32603,"message":"provider operation failed"}`)})
	return out
}
