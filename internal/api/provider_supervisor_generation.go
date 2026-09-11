package api

import (
	"fmt"
	"reflect"
)

// frozenProviderAdoptDaemonWithGeneration freezes both the exact provider-owned
// daemon descriptor and the supervisor-intent generation that contained it.
// The whole-file generation is intentionally part of the settlement token: if
// any writer commits after this snapshot, a destructive cleanup must re-read and
// re-settle rather than assuming that an equal-looking row is the generation it
// already stopped.
func frozenProviderAdoptDaemonWithGeneration(rec *AdoptProvenanceRecord) (SupervisorDaemon, uint64, error) {
	if rec == nil {
		return SupervisorDaemon{}, 0, fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	intent, err := loadSupervisorOwnedIntent()
	if err != nil || intent == nil {
		return SupervisorDaemon{}, 0, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	scope, err := providerAdoptOwnershipScope(rec)
	if err != nil {
		return SupervisorDaemon{}, 0, err
	}
	var owned []SupervisorDaemon
	for _, daemon := range intent.Daemons {
		if supervisorIntentRowOwnedByScope(daemon, rec.ManifestName, scope) {
			owned = append(owned, daemon)
		}
	}
	if len(owned) != 1 {
		return SupervisorDaemon{}, 0, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	daemon := owned[0]
	if daemon.Server != rec.ManifestName || daemon.Daemon != adoptDefaultDaemonName || daemon.Port != rec.Port || daemon.ManifestHash != rec.ExpectedManifestHash {
		return SupervisorDaemon{}, 0, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	return daemon, intent.IntentGeneration, nil
}

// removeSettledProviderAdoptDaemonGeneration removes the descriptor only when
// the canonical supervisor-intent still represents the exact descriptor that
// was frozen and no writer has committed after the managed-stop write. The stop
// itself is allowed to advance the generation exactly once because
// stopAdoptOwnedDaemonSettled durably adds the user-stop override before it asks
// the supervisor for terminal settlement. Any other generation change, or a
// generation advance without that exact stop override, fails closed. This keeps
// same-content rewrites fenced while not rejecting our own stop mutation.
func removeSettledProviderAdoptDaemonGeneration(rec *AdoptProvenanceRecord, frozen SupervisorDaemon, frozenGeneration uint64) error {
	if rec == nil {
		return fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	scope, err := providerAdoptOwnershipScope(rec)
	if err != nil {
		return err
	}
	if err := MutateSupervisorIntentIfChanged(intentPath, func(intent *SupervisorIntentFile) (bool, error) {
		if intent == nil {
			return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
		}
		switch intent.IntentGeneration {
		case frozenGeneration:
			// Test seams and already-durable stops may not need another intent write.
		case frozenGeneration + 1:
			stop, ok := intent.Stops[frozen.TaskName]
			if !ok || stop.Desired != IntentDesiredStopped || stop.Reason != IntentReasonUserStop {
				return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
			}
		default:
			return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
		}
		ownedIndex := -1
		for i, daemon := range intent.Daemons {
			if !supervisorIntentRowOwnedByScope(daemon, rec.ManifestName, scope) {
				continue
			}
			if ownedIndex >= 0 {
				return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
			}
			ownedIndex = i
		}
		if ownedIndex < 0 || !reflect.DeepEqual(intent.Daemons[ownedIndex], frozen) {
			return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
		}
		intent.Daemons = append(intent.Daemons[:ownedIndex], intent.Daemons[ownedIndex+1:]...)
		return true, nil
	}); err != nil {
		return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED: late supervisor-intent cleanup: %w", err)
	}
	return nil
}
