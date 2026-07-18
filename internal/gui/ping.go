// internal/gui/ping.go
package gui

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

const activationNoTargetReasonHeader = "X-Mcphub-Activation-No-Target-Reason"

func registerPingRoutes(s *Server) {
	s.mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"pid":     s.cfg.PID,
			"version": s.cfg.Version,
		})
	})
	// Authorization: public by design to native loopback clients; browser
	// callers must pass the production allowed-Host and same-origin checks.
	s.mux.HandleFunc("/api/activate-window", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if s.onActivateWindow == nil {
			// No callback wired — historic behavior was 204 (the test
			// shape Server-with-defaults). Preserve it so a minimal
			// Server (no GUI app wired) still answers cleanly.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		err := s.onActivateWindow()
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, ErrActivationNoTarget):
			// Incumbent reachable but cannot focus or relaunch a
			// window. 503 + reason header + diagnostic body let the
			// second-instance handshake surface a useful message
			// instead of "activated existing mcphub gui" — which would
			// be a lie in this case. Codex bot review on PR #26 P2.
			w.Header().Set(activationNoTargetReasonHeader, string(activationNoTargetReason(err)))
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
}

const authenticatedReadinessNonceBytes = 32

// AuthenticatedReadinessIdentity is the exact child identity covered by a
// challenged standby proof. PID is diagnostic only unless the complete tuple
// and its message authentication code match the parent's retained nonce.
type AuthenticatedReadinessIdentity struct {
	HandoffID  string `json:"handoff_id"`
	Generation string `json:"generation"`
	Sequence   uint64 `json:"sequence"`
	PID        int    `json:"pid"`
	Port       int    `json:"port"`
}

// AuthenticatedReadinessProof is the standby /api/ping response. Proof is an
// HMAC-SHA256 over Challenge plus every identity field; it never contains the
// nonce itself.
type AuthenticatedReadinessProof struct {
	AuthenticatedReadinessIdentity
	Challenge string `json:"challenge"`
	Proof     []byte `json:"proof"`
}

// AuthenticatedReadinessSession owns the in-memory one-use nonce after the
// child consumed and removed its hardened nonce file.
type AuthenticatedReadinessSession struct {
	mu       sync.Mutex
	identity AuthenticatedReadinessIdentity
	nonce    []byte
}

func NewAuthenticatedReadinessSession(identity AuthenticatedReadinessIdentity, nonce []byte) (*AuthenticatedReadinessSession, error) {
	if err := validateAuthenticatedReadinessIdentity(identity); err != nil {
		return nil, err
	}
	if len(nonce) != authenticatedReadinessNonceBytes {
		return nil, fmt.Errorf("readiness nonce length = %d, want %d bytes", len(nonce), authenticatedReadinessNonceBytes)
	}
	return &AuthenticatedReadinessSession{
		identity: identity,
		nonce:    append([]byte(nil), nonce...),
	}, nil
}

func validateAuthenticatedReadinessIdentity(identity AuthenticatedReadinessIdentity) error {
	if strings.TrimSpace(identity.HandoffID) == "" || strings.TrimSpace(identity.Generation) == "" {
		return errors.New("readiness handoff id and generation are required")
	}
	if identity.Sequence == 0 {
		return errors.New("readiness sequence must be positive")
	}
	if identity.PID <= 0 {
		return errors.New("readiness PID must be positive")
	}
	if identity.Port < 1024 || identity.Port > 65535 {
		return fmt.Errorf("readiness port %d is outside [1024,65535]", identity.Port)
	}
	return nil
}

// Handler serves only challenged standby readiness on /api/ping.
// Authorization: public by design on the exclusive loopback standby listener;
// a response is useful only to a caller holding the one-use nonce.
func (s *AuthenticatedReadinessSession) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ping" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		challenge := r.URL.Query().Get("challenge")
		if challenge == "" || len(challenge) > 256 {
			http.Error(w, "valid challenge required", http.StatusBadRequest)
			return
		}
		proof, err := s.proof(challenge)
		if err != nil {
			http.Error(w, "readiness session unavailable", http.StatusGone)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(proof)
	})
}

func (s *AuthenticatedReadinessSession) proof(challenge string) (AuthenticatedReadinessProof, error) {
	if s == nil {
		return AuthenticatedReadinessProof{}, errors.New("nil readiness session")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.nonce) == 0 {
		return AuthenticatedReadinessProof{}, errors.New("readiness session closed")
	}
	proof := AuthenticatedReadinessProof{AuthenticatedReadinessIdentity: s.identity, Challenge: challenge}
	proof.Proof = authenticatedReadinessMAC(s.nonce, challenge, s.identity)
	return proof, nil
}

func (s *AuthenticatedReadinessSession) nonceCopy() ([]byte, error) {
	if s == nil {
		return nil, errors.New("nil readiness session")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.nonce) == 0 {
		return nil, errors.New("readiness session closed")
	}
	return append([]byte(nil), s.nonce...), nil
}

func authenticatedReadinessMAC(nonce []byte, challenge string, identity AuthenticatedReadinessIdentity) []byte {
	message, _ := json.Marshal(struct {
		Challenge string `json:"challenge"`
		AuthenticatedReadinessIdentity
	}{Challenge: challenge, AuthenticatedReadinessIdentity: identity})
	mac := hmac.New(sha256.New, nonce)
	_, _ = mac.Write(message)
	return mac.Sum(nil)
}

// VerifyAuthenticatedReadiness rejects a proof unless the challenge, complete
// child identity, and HMAC all match. A reused PID alone never authenticates.
func VerifyAuthenticatedReadiness(nonce []byte, challenge string, proof AuthenticatedReadinessProof, expected AuthenticatedReadinessIdentity) bool {
	if len(nonce) != authenticatedReadinessNonceBytes || challenge == "" || proof.Challenge != challenge || proof.AuthenticatedReadinessIdentity != expected {
		return false
	}
	want := authenticatedReadinessMAC(nonce, challenge, expected)
	return hmac.Equal(proof.Proof, want)
}

// Close erases the in-memory nonce once standby authentication is no longer
// needed. The hardened transport file is consumed separately by the child.
func (s *AuthenticatedReadinessSession) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	for i := range s.nonce {
		s.nonce[i] = 0
	}
	s.nonce = nil
	s.mu.Unlock()
}
