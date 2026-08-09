package api

// supervisorIntentLockSuffix is the single mapping from a supervisor-intent
// path to its cross-process lock leaf. The generic ledger remains the sole
// owner of poisoning, OS acquisition, and one-shot release semantics.
const supervisorIntentLockSuffix = ".lock"

func supervisorIntentLockPath(path string) string {
	return path + supervisorIntentLockSuffix
}

func lockSupervisorIntent(path string) (func() error, error) {
	return lockLeafLedgered(supervisorIntentLockPath(path))
}

func tryLockSupervisorIntent(path string) (func() error, bool, error) {
	return tryLockLeafLedgered(supervisorIntentLockPath(path))
}

func releaseSupervisorIntentAndJoin(err *error, release func() error, what string) {
	ReleaseAndJoin(err, release, what)
}

func releaseSupervisorIntentAndJoinApplied(err *error, release func() error, what string, applied bool) {
	releaseAndJoinApplied(err, release, what, applied)
}
