package process

// JobOptions selects the Windows Job Object lifecycle policy. Each enabled
// option is required: NewJob returns an error instead of silently weakening a
// requested policy when the operating system rejects configuration.
//
// Non-Windows implementations retain the same API as a no-op portability seam;
// their process-lifecycle mechanisms are owned separately.
type JobOptions struct {
	KillOnClose bool
	BreakawayOK bool
}
