package clients

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

var (
	ErrProviderCapabilityUnsupported = errors.New("E_PROVIDER_CAPABILITY_UNSUPPORTED")
	ErrProviderSourceChanged         = errors.New("E_PROVIDER_SOURCE_CHANGED")
)

type ProviderMCPTransportV1 string

const (
	ProviderMCPTransportStdio ProviderMCPTransportV1 = "stdio"
	ProviderMCPTransportHTTP  ProviderMCPTransportV1 = "http"
)

type ProviderMCPScopeV1 string

const ProviderMCPScopeUser ProviderMCPScopeV1 = "user"

type ProviderMCPPolicyStateV1 string

const (
	ProviderMCPPolicyNone            ProviderMCPPolicyStateV1 = "none"
	ProviderMCPPolicyUnrepresentable ProviderMCPPolicyStateV1 = "unrepresentable"
)

type ProviderMCPEntryV1 struct {
	ProviderClient, PluginRef, ServerName string
	Transport                             ProviderMCPTransportV1
	Command                               string
	Args                                  []string
	Env                                   map[string]string
	EnvForwardLocal                       []string
	WorkingDir                            *string
	ToolTimeoutSec                        int
	Scope                                 ProviderMCPScopeV1
	Enabled                               bool
	ReceiptFingerprint                    string
	ActivationFingerprint                 string
	ActivationEnabledPresent              bool
	ActivationEnabled                     bool
	DisabledActivationFingerprint         string
	PolicyState                           ProviderMCPPolicyStateV1
	PolicyFingerprint                     string
}

type ProviderMCPActivationCASV1 struct {
	PluginRef, ServerName                                    string
	ExpectedActivationFingerprint, ExpectedPolicyFingerprint string
	DesiredEnabledPresent                                    bool
	DesiredEnabled                                           bool
}

type ProviderMCPActivationResultV1 struct {
	PriorEnabledPresent   bool
	PriorEnabled          bool
	ActivationFingerprint string
}

type ProviderMCPSourceV1 interface {
	ListProviderMCPEntries(context.Context) ([]ProviderMCPEntryV1, error)
	CompareAndSetProviderMCPActivation(context.Context, ProviderMCPActivationCASV1) (ProviderMCPActivationResultV1, error)
}

func providerFingerprint(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func providerActivationFingerprint(raw map[string]any) (string, error) {
	value, present := raw["enabled"]
	enabled, ok := value.(bool)
	return providerFingerprint(struct {
		Present bool `json:"present"`
		Enabled bool `json:"enabled"`
		Valid   bool `json:"valid"`
	}{present, enabled, !present || ok})
}

func providerPolicyFingerprint(raw map[string]any) (string, error) {
	copy := make(map[string]any, len(raw))
	for key, value := range raw {
		if key != "enabled" {
			copy[key] = value
		}
	}
	return providerFingerprint(copy)
}

func validateProviderActivationCAS(req ProviderMCPActivationCASV1) error {
	if req.PluginRef == "" || req.ServerName == "" || req.ExpectedActivationFingerprint == "" || req.ExpectedPolicyFingerprint == "" {
		return fmt.Errorf("%w: mandatory activation identity or fingerprint is empty", ErrProviderSourceChanged)
	}
	return nil
}

func cloneProviderMCPEntries(entries []ProviderMCPEntryV1) []ProviderMCPEntryV1 {
	cloned := make([]ProviderMCPEntryV1, len(entries))
	for i, entry := range entries {
		cloned[i] = entry
		cloned[i].Args = append([]string(nil), entry.Args...)
		cloned[i].EnvForwardLocal = append([]string(nil), entry.EnvForwardLocal...)
		if entry.Env != nil {
			cloned[i].Env = make(map[string]string, len(entry.Env))
			for key, value := range entry.Env {
				cloned[i].Env[key] = value
			}
		}
		if entry.WorkingDir != nil {
			value := *entry.WorkingDir
			cloned[i].WorkingDir = &value
		}
	}
	return cloned
}
