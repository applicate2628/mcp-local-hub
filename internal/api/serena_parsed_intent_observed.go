package api

import (
	"context"
	"errors"
	"reflect"
)

type heldSerenaIntentMutation struct {
	observation *SupervisorFileMutationObservation
	input       SupervisorFileMutationPrecondition
	afterSHA    string
}

func (s *PreparedSerenaConsumerInvocation) readParsedIntentObserved(ctx context.Context, lease *StateFileObservedLease) (*SupervisorIntentFile, bool, string, error) {
	if s == nil || lease == nil || s.borrowedIntent != lease || s.intentNamespace == nil {
		return nil, false, "", errors.New("SERENA_PARSED_INTENT_OWNER_MISSING")
	}
	if err := lease.ValidateHeld(ctx, s.io.IntentPath); err != nil {
		return nil, false, "", err
	}
	if err := s.intentNamespace.ValidateOperation(ctx, s.request.OperationID, s.requestSHA); err != nil {
		return nil, false, "", err
	}
	cap := stateFileReadCapBytes(s.io.IntentPath)
	raw, reader, readErr := ReadStateFileInodeAnchoredObserved(ctx, s.io.IntentPath, cap)
	s.readers = append(s.readers, reader)
	if reader == nil {
		return nil, false, "", errors.Join(readErr, errors.New("SERENA_PARSED_INTENT_READER_MISSING"))
	}
	if readErr != nil {
		if absenceErr := reader.ValidateAbsence(ctx, s.io.IntentPath, StateFileReadLeafNotFound); absenceErr != nil {
			return nil, false, "", errors.Join(readErr, absenceErr)
		}
		return &SupervisorIntentFile{Version: 1}, false, "", nil
	}
	sha := preparedSerenaHash(raw)
	if err := reader.Validate(ctx, s.io.IntentPath, cap, raw, sha); err != nil {
		return nil, false, "", err
	}
	prior, _, err := decodeSupervisorIntentFile(s.io.IntentPath, raw)
	return prior, true, sha, err
}

func (s *PreparedSerenaConsumerInvocation) replaceParsedIntentObserved(ctx context.Context, lease *StateFileObservedLease, beforeSHA string, desired *SupervisorIntentFile) (*SupervisorFileMutationObservation, string, error) {
	return s.publishParsedIntentObserved(ctx, lease, true, beforeSHA, desired)
}

func (s *PreparedSerenaConsumerInvocation) parsedIntentPrecondition(beforeDefined bool, beforeSHA string) SupervisorFileMutationPrecondition {
	input := SupervisorFileMutationPrecondition{NamespaceSource: s.intentNamespace, Apply: true, SourceOperationID: s.request.OperationID, RequestSHA256: s.requestSHA, NamespaceSHA256: s.intentNamespace.NamespaceSHA256(), BeforeDefined: beforeDefined}
	if beforeDefined {
		input.BeforeSHA256 = &beforeSHA
	}
	return input
}

func (s *PreparedSerenaConsumerInvocation) preflightParsedIntentObserved(ctx context.Context, lease *StateFileObservedLease, beforeDefined bool, beforeSHA string, desired *SupervisorIntentFile) error {
	if s == nil || lease == nil || lease != s.borrowedIntent {
		return errors.New("SERENA_PARSED_PREFLIGHT_OWNER_MISSING")
	}
	return preflightPreparedSupervisorIntentLockHeld(ctx, s.io.IntentPath, s.parsedIntentPrecondition(beforeDefined, beforeSHA), lease, desired)
}

func (s *PreparedSerenaConsumerInvocation) publishParsedIntentObserved(ctx context.Context, lease *StateFileObservedLease, beforeDefined bool, beforeSHA string, desired *SupervisorIntentFile) (*SupervisorFileMutationObservation, string, error) {
	if s == nil || lease == nil || lease != s.borrowedIntent || desired == nil || (beforeDefined && !preparedSerenaDigest(beforeSHA)) || (!beforeDefined && beforeSHA != "") {
		return nil, "", errors.New("SERENA_PARSED_REPLACEMENT_INPUT_MISSING")
	}
	input := s.parsedIntentPrecondition(beforeDefined, beforeSHA)
	observation, err := MutateSupervisorIntentLockHeldObserved(ctx, s.io.IntentPath, input, lease, func(current *SupervisorIntentFile) (bool, error) {
		replacement := cloneSupervisorIntentFile(desired)
		replacement.IntentGeneration = current.IntentGeneration
		if reflect.DeepEqual(current, replacement) {
			return false, nil
		}
		*current = *replacement
		return true, nil
	})
	after := ""
	if observation != nil {
		snapshot := observation.Snapshot()
		if snapshot.IntendedAfterSHA256 != nil {
			after = *snapshot.IntendedAfterSHA256
		}
		s.heldMutations = append(s.heldMutations, heldSerenaIntentMutation{observation, input, after})
	}
	if err != nil {
		return observation, after, err
	}
	if observation == nil {
		return nil, "", errors.New("SERENA_PARSED_REPLACEMENT_OBSERVATION_MISSING")
	}
	return observation, after, observation.ValidateHeldProgress(ctx, input, after, lease)
}

func (s *PreparedSerenaConsumerInvocation) restoreParsedIntentAbsenceObserved(ctx context.Context, lease *StateFileObservedLease, commit *SupervisorFileMutationObservation, commitSHA string) error {
	if s == nil || lease == nil || lease != s.borrowedIntent {
		return errors.New("SERENA_PARSED_RESTORATION_OWNER_MISSING")
	}
	input := s.parsedIntentPrecondition(true, commitSHA)
	observation, err := RestoreSupervisorIntentAbsenceLockHeldObserved(ctx, s.io.IntentPath, input, lease, commit)
	if observation != nil {
		s.heldRestorations = append(s.heldRestorations, observation)
	}
	if err != nil {
		return err
	}
	return observation.ValidateHeldProgress(ctx, input, lease, commit)
}
