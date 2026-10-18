package api

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// SerenaPreparationPolicyV1 owns compatibility preferences and finite package
// limits. File discovery and actual ABI/protocol qualification remain required.
type SerenaPreparationPolicyV1 struct {
	Version             int              `json:"version"`
	UVVersion           string           `json:"uv_version"`
	InterpreterSelector string           `json:"interpreter_selector"`
	WorkLimit           time.Duration    `json:"work_limit_ns"`
	CleanupLimit        time.Duration    `json:"cleanup_limit_ns"`
	OwnedTreeLimits     SerenaTreeLimits `json:"owned_tree_limits"`
}

// These installation-policy limits are not service startup/repair limits.
// Cold-build fit is unverified until the admitted production acceptance run.
func DefaultSerenaPreparationPolicy() SerenaPreparationPolicyV1 {
	return SerenaPreparationPolicyV1{Version: 1, UVVersion: "0.9.21", InterpreterSelector: "3.13.11", WorkLimit: 300 * time.Second, CleanupLimit: 45 * time.Second, OwnedTreeLimits: DefaultSerenaTreeLimits()}
}

func resolveSerenaPreparationPolicy(override *SerenaPreparationPolicyV1) (SerenaPreparationPolicyV1, error) {
	policy := DefaultSerenaPreparationPolicy()
	if override != nil {
		policy = *override
	}
	if err := policy.Validate(); err != nil {
		return SerenaPreparationPolicyV1{}, err
	}
	return policy, nil
}

func (p SerenaPreparationPolicyV1) Validate() error {
	if p.Version != 1 || p.UVVersion != "0.9.21" || p.WorkLimit <= 0 || p.CleanupLimit <= 0 || !p.OwnedTreeLimits.valid() {
		return errors.New("SERENA_PREPARATION_POLICY_INVALID")
	}
	parts := strings.Split(p.InterpreterSelector, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return errors.New("SERENA_PREPARATION_INTERPRETER_SELECTOR_INVALID")
	}
	version := [3]int{}
	for i, component := range parts {
		v, err := strconv.Atoi(component)
		if err != nil || v < 0 || strconv.Itoa(v) != component {
			return errors.New("SERENA_PREPARATION_INTERPRETER_SELECTOR_INVALID")
		}
		version[i] = v
	}
	if version[0] != 3 || version[1] < 11 || version[1] >= 15 {
		return errors.New("SERENA_PREPARATION_INTERPRETER_SELECTOR_UNSUPPORTED")
	}
	return nil
}

func (p SerenaPreparationPolicyV1) IdentitySHA256() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return preparedSerenaHash(raw), nil
}
