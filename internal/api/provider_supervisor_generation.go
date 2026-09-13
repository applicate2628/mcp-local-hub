package api

import (
	"fmt"
	"reflect"
)

// providerAdoptDaemonFence freezes the exact provider-owned descriptor together
// with the whole supervisor-intent generation and the pre-stop value of this
// task's stop directive. The latter is load-bearing: a generation+1 rewrite is
// attributable to our managed-stop write only when that write actually changed
// the task's stop directive. A pre-existing stopped/user_stop entry therefore
// cannot authenticate an unrelated same-content descriptor rewrite.
type providerAdoptDaemonFence struct {
	Daemon           SupervisorDaemon
	IntentGeneration uint64
	PriorStop        DaemonIntent
	PriorStopPresent bool
}

func frozenProviderAdoptDaemonFence(rec *AdoptProvenanceRecord) (providerAdoptDaemonFence, error) {
	if rec == nil {
		return providerAdoptDaemonFence{}, fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	intent, err := loadSupervisorOwnedIntent()
	if err != nil || intent == nil || intent.IntentGeneration == 0 {
		return providerAdoptDaemonFence{}, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	scope, err := providerAdoptOwnershipScope(rec)
	if err != nil {
		return providerAdoptDaemonFence{}, err
	}
	var owned []SupervisorDaemon
	for _, daemon := range intent.Daemons {
		if supervisorIntentRowOwnedByScope(daemon, rec.ManifestName, scope) {
			owned = append(owned, daemon)
		}
	}
	if len(owned) != 1 {
		return providerAdoptDaemonFence{}, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	daemon := owned[0]
	if daemon.Server != rec.ManifestName || daemon.Daemon != adoptDefaultDaemonName || daemon.Port != rec.Port || daemon.ManifestHash != rec.ExpectedManifestHash {
		return providerAdoptDaemonFence{}, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	fence := providerAdoptDaemonFence{Daemon: daemon, IntentGeneration: intent.IntentGeneration}
	if stop, ok := intent.Stops[canonicalIntentTaskKey(daemon.TaskName)]; ok {
		fence.PriorStop = stop
		fence.PriorStopPresent = true
	}
	return fence, nil
}

// frozenProviderAdoptDaemonWithGeneration is retained for focused callers and
// tests that only need the descriptor generation snapshot. Destructive cleanup
// uses the stronger providerAdoptDaemonFence above.
func frozenProviderAdoptDaemonWithGeneration(rec *AdoptProvenanceRecord) (SupervisorDaemon, uint64, error) {
	fence, err := frozenProviderAdoptDaemonFence(rec)
	if err != nil {
		return SupervisorDaemon{}, 0, err
	}
	return fence.Daemon, fence.IntentGeneration, nil
}

func removeSettledProviderLifecycleArtifacts(intent *SupervisorIntentFile, daemon SupervisorDaemon) {
	removed := []SupervisorDaemon{daemon}
	intent.Stops = pruneStopsForRemovedSupervisorTargets(intent.Stops, removed)
	intent.LegacyStopWatermarks = pruneLegacyStopWatermarksForRemovedSupervisorTargets(intent.LegacyStopWatermarks, removed)
}

// removeSettledProviderAdoptDaemonFence removes only the descriptor generation
// whose terminal stop was just established. The stop implementation writes a
// fresh stopped/user_stop directive before asking the supervisor for terminal
// settlement, so one generation advance is accepted only when that directive
// changed relative to the frozen pre-stop snapshot. Any later write, including
// an equal-looking daemon rewrite, fails closed and leaves ownership intact for
// a fresh settlement on retry. Descriptor removal also prunes that exact task's
// stop and legacy-stop watermark in the same flocked intent mutation, matching
// the normal uninstall/decommission lifecycle contract.
func removeSettledProviderAdoptDaemonFence(rec *AdoptProvenanceRecord, fence providerAdoptDaemonFence) error {
	if rec == nil || fence.IntentGeneration == 0 {
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
		case fence.IntentGeneration:
			// Test seams and an already-durable terminal stop may not rewrite intent.
		case fence.IntentGeneration + 1:
			stop, ok := intent.Stops[canonicalIntentTaskKey(fence.Daemon.TaskName)]
			if !ok || stop.Desired != IntentDesiredStopped || stop.Reason != IntentReasonUserStop {
				return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
			}
			if fence.PriorStopPresent && reflect.DeepEqual(stop, fence.PriorStop) {
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
		if ownedIndex < 0 || !reflect.DeepEqual(intent.Daemons[ownedIndex], fence.Daemon) {
			return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
		}
		removedDaemon := intent.Daemons[ownedIndex]
		intent.Daemons = append(intent.Daemons[:ownedIndex], intent.Daemons[ownedIndex+1:]...)
		removeSettledProviderLifecycleArtifacts(intent, removedDaemon)
		return true, nil
	}); err != nil {
		return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED: settled supervisor-intent cleanup: %w", err)
	}
	return nil
}

// removeSettledProviderAdoptDaemonGeneration is the strict form used when a
// caller already owns the exact settled supervisor-intent generation. Unlike
// the fence form above it never admits an implicit generation advance.
func removeSettledProviderAdoptDaemonGeneration(rec *AdoptProvenanceRecord, frozen SupervisorDaemon, settledGeneration uint64) error {
	if rec == nil || settledGeneration == 0 {
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
		if intent == nil || intent.IntentGeneration != settledGeneration {
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
		removedDaemon := intent.Daemons[ownedIndex]
		intent.Daemons = append(intent.Daemons[:ownedIndex], intent.Daemons[ownedIndex+1:]...)
		removeSettledProviderLifecycleArtifacts(intent, removedDaemon)
		return true, nil
	}); err != nil {
		return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED: exact supervisor-intent cleanup: %w", err)
	}
	return nil
}
