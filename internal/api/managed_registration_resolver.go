package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"mcp-local-hub/internal/clients"
)

func (r *ManagedRegistrationReconciler) entryHasManagedRelationship(intent ManagedRegistrationEntryIntent) bool {
	if intent.EntryName == intent.Server {
		return true
	}
	if byEntry := r.options.ExistingManagedRelationships[intent.ClientID]; byEntry != nil && byEntry[intent.EntryName] == intent.Server {
		return true
	}
	return r.options.CanonicalAliases[intent.EntryName] == intent.Server
}

func (r *ManagedRegistrationReconciler) entryHasLegacyManagedRelationship(intent ManagedRegistrationEntryIntent) bool {
	if byEntry := r.options.LegacyManagedRelationships[intent.ClientID]; byEntry != nil {
		return byEntry[intent.EntryName] == intent.Server
	}
	return false
}

func resolveManagedRegistrationTarget(intent ManagedRegistrationEntryIntent, targets []ManagedRegistrationTarget) (ManagedRegistrationTarget, error) {
	var desired []ManagedRegistrationTarget
	for _, target := range targets {
		if target.Desired && target.Server == intent.Server {
			desired = append(desired, target)
		}
	}
	if len(desired) == 0 {
		return ManagedRegistrationTarget{}, newManagedRegistrationFailure(ManagedRegistrationFailureTargetAmbiguous, nil)
	}
	var ready []ManagedRegistrationTarget
	for _, target := range desired {
		if target.Ready && target.Live {
			ready = append(ready, target)
		}
	}
	if len(ready) == 0 {
		return ManagedRegistrationTarget{}, newManagedRegistrationFailure(ManagedRegistrationFailureTargetUnready, nil)
	}
	selected := ready[0]
	for _, target := range ready[1:] {
		if managedRegistrationTargetKey(target) != managedRegistrationTargetKey(selected) {
			return ManagedRegistrationTarget{}, newManagedRegistrationFailure(ManagedRegistrationFailureTargetAmbiguous, nil)
		}
	}
	return selected, nil
}

func managedRegistrationTargetKey(target ManagedRegistrationTarget) string {
	return strings.Join([]string{
		target.TaskKey,
		target.Server,
		target.Daemon,
		strconv.Itoa(target.Port),
		target.Path,
		target.TargetIdentitySHA256.String(),
	}, "\x00")
}

func managedRegistrationTargetGroupFingerprint(target ManagedRegistrationTarget) (clients.ManagedRegistrationFingerprint, error) {
	if !managedRegistrationSafeIdentifier(target.Server) || !managedRegistrationSafeIdentifier(target.Daemon) || !managedRegistrationFingerprintValid(target.TargetIdentitySHA256) {
		return clients.ManagedRegistrationFingerprint{}, newManagedRegistrationFailure(ManagedRegistrationFailureFingerprintInvalid, nil)
	}
	hash := sha256.New()
	for _, part := range []string{"managed-registration-target-group/v1", target.Server, target.Daemon, target.TargetIdentitySHA256.String()} {
		_, _ = fmt.Fprintf(hash, "%d:%s", len(part), part)
	}
	var fingerprint clients.ManagedRegistrationFingerprint
	if err := json.Unmarshal([]byte(`"`+hex.EncodeToString(hash.Sum(nil))+`"`), &fingerprint); err != nil {
		return clients.ManagedRegistrationFingerprint{}, newManagedRegistrationFailure(ManagedRegistrationFailureFingerprintInvalid, err)
	}
	return fingerprint, nil
}

func managedRegistrationDesiredEntry(intent ManagedRegistrationEntryIntent, target ManagedRegistrationTarget) (clients.MCPEntry, error) {
	if intent.Desired.Name != "" {
		desired := intent.Desired
		desired.Headers = cloneManagedRegistrationStringMap(desired.Headers)
		desired.Env = cloneManagedRegistrationStringMap(desired.Env)
		if desired.Raw != nil || desired.Name != intent.EntryName || desired.Disabled != (intent.EnabledState == clients.ManagedRegistrationDisabled) {
			return clients.MCPEntry{}, newManagedRegistrationFailure(ManagedRegistrationFailureDesiredEntryInvalid, nil)
		}
		relay := desired.RelayURL != ""
		fingerprint, err := clients.ManagedRegistrationTargetFingerprint(desired, relay)
		if err != nil || fingerprint != target.TargetIdentitySHA256 {
			return clients.MCPEntry{}, newManagedRegistrationFailure(ManagedRegistrationFailureDesiredEntryInvalid, err)
		}
		return desired, nil
	}
	disabled := intent.EnabledState == clients.ManagedRegistrationDisabled
	if intent.EnabledState != clients.ManagedRegistrationEnabled && !disabled {
		return clients.MCPEntry{}, newManagedRegistrationFailure(ManagedRegistrationFailureDesiredEntryInvalid, nil)
	}
	if intent.Transport == clients.ManagedRegistrationTransportStdioRelay || intent.Transport == "stdio-relay" {
		if !filepath.IsAbs(intent.RelayExecutable) || filepath.Clean(intent.RelayExecutable) != intent.RelayExecutable {
			return clients.MCPEntry{}, newManagedRegistrationFailure(ManagedRegistrationFailureRelayExecutableUnavailable, nil)
		}
		info, err := os.Lstat(intent.RelayExecutable)
		if err != nil || !info.Mode().IsRegular() {
			return clients.MCPEntry{}, newManagedRegistrationFailure(ManagedRegistrationFailureRelayExecutableUnavailable, err)
		}
		return clients.MCPEntry{
			Name:         intent.EntryName,
			RelayExePath: intent.RelayExecutable,
			RelayURL:     fmt.Sprintf("http://127.0.0.1:%d%s", target.Port, target.Path),
			Disabled:     disabled,
		}, nil
	}
	if target.Port < 1 || target.Port > 65535 || !strings.HasPrefix(target.Path, "/") || strings.ContainsAny(target.Path, "?#") {
		return clients.MCPEntry{}, newManagedRegistrationFailure(ManagedRegistrationFailureDesiredEntryInvalid, nil)
	}
	parsedPath, err := url.PathUnescape(target.Path)
	if err != nil || (&url.URL{Path: parsedPath, RawPath: target.Path}).EscapedPath() != target.Path {
		return clients.MCPEntry{}, newManagedRegistrationFailure(ManagedRegistrationFailureDesiredEntryInvalid, err)
	}
	return clients.MCPEntry{
		Name:     intent.EntryName,
		URL:      fmt.Sprintf("http://127.0.0.1:%d%s", target.Port, target.Path),
		Disabled: disabled,
	}, nil
}

func managedRegistrationFingerprintValid(value clients.ManagedRegistrationFingerprint) bool {
	raw := value.String()
	if len(raw) != 64 {
		return false
	}
	for _, char := range raw {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validateManagedRegistrationSnapshot(snapshot clients.ManagedRegistrationEntrySnapshot) error {
	if snapshot.ClientID == "" {
		return newManagedRegistrationFailure(ManagedRegistrationFailureSafeProjectionFailed, nil)
	}
	if !snapshot.Exists {
		if snapshot.EnabledState != clients.ManagedRegistrationEnabledAbsent || snapshot.Transport != clients.ManagedRegistrationTransportAbsent || snapshot.IdentityState != clients.ManagedRegistrationIdentityAbsent {
			return newManagedRegistrationFailure(ManagedRegistrationFailureDesiredEntryInvalid, nil)
		}
		for _, fingerprint := range []clients.ManagedRegistrationFingerprint{snapshot.ConfigSHA256, snapshot.EntrySHA256, snapshot.ConfigPathSHA256, snapshot.EntryKeySHA256} {
			if !managedRegistrationFingerprintValid(fingerprint) {
				return newManagedRegistrationFailure(ManagedRegistrationFailureFingerprintInvalid, nil)
			}
		}
		return nil
	}
	if snapshot.EnabledState != clients.ManagedRegistrationEnabled && snapshot.EnabledState != clients.ManagedRegistrationDisabled {
		return newManagedRegistrationFailure(ManagedRegistrationFailureDesiredEntryInvalid, nil)
	}
	if snapshot.IdentityState == clients.ManagedRegistrationIdentityUnavailable || snapshot.IdentityState == clients.ManagedRegistrationIdentityOpaque {
		return newManagedRegistrationFailure(ManagedRegistrationFailureIdentityUnavailable, nil)
	}
	for _, fingerprint := range []clients.ManagedRegistrationFingerprint{
		snapshot.ConfigSHA256,
		snapshot.EntrySHA256,
		snapshot.ConfigPathSHA256,
		snapshot.EntryKeySHA256,
	} {
		if !managedRegistrationFingerprintValid(fingerprint) {
			return newManagedRegistrationFailure(ManagedRegistrationFailureFingerprintInvalid, nil)
		}
	}
	return nil
}

func sameManagedRegistrationSnapshotIdentity(left, right clients.ManagedRegistrationEntrySnapshot) bool {
	return left.ClientID == right.ClientID &&
		left.ConfigSHA256 == right.ConfigSHA256 &&
		left.EntrySHA256 == right.EntrySHA256 &&
		left.ConfigPathSHA256 == right.ConfigPathSHA256 &&
		left.EntryKeySHA256 == right.EntryKeySHA256 &&
		left.TargetIdentitySHA256 == right.TargetIdentitySHA256
}
