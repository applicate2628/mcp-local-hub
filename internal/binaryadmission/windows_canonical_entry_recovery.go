package binaryadmission

import (
	"context"
	"sync"
)

type WindowsCanonicalRecoverySideV1 string

const (
	WindowsCanonicalRecoveryPriorV1     WindowsCanonicalRecoverySideV1 = "prior"
	WindowsCanonicalRecoverySuccessorV1 WindowsCanonicalRecoverySideV1 = "successor"
)

type WindowsCanonicalRecoveryMemberV1 struct {
	Present        bool
	SourceRelative string
	ExpectedSHA256 string
}
type WindowsCanonicalRecoveryLocationsV1 struct {
	Launcher, Product, Identity WindowsCanonicalRecoveryMemberV1
}
type WindowsCanonicalRecoveryTargetsV1 struct{ Root, LauncherRelative, ProductRelative, IdentityRelative string }

// Source is implemented in production only by the existing private fenced
// journal receiver/classifier. It retains the actual actor, original journal
// transition and fence; public tuples/booleans do not supply that authority.
type WindowsCanonicalRecoverySourceV1 interface {
	WithOriginalRecoveryInput(context.Context, WindowsCanonicalRecoverySideV1, func(WindowsCanonicalRecoveryTargetsV1, WindowsCanonicalRecoveryLocationsV1) error) error
	ValidateOriginalRecovery(context.Context) error
}

// RecoverySet issues CURRENT reacquired native ownership of exactly classified
// objects. It does not resurrect a serialized pre-crash handle or file identity.
type WindowsCanonicalEntryRecoverySetV1 struct {
	mu         sync.Mutex
	original   *WindowsCanonicalEntryPriorSetV1
	source     WindowsCanonicalRecoverySourceV1
	side       WindowsCanonicalRecoverySideV1
	targets    WindowsCanonicalRecoveryTargetsV1
	locations  WindowsCanonicalRecoveryLocationsV1
	ready      bool
	closed     bool
	closeError error
}
