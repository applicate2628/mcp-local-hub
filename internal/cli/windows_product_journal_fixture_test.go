package cli

import "context"

// This is explicit policy for existing synthetic/native test fixtures only.
// It is never consulted by a production resolver or operation.
func productJournalFixtureAllowance() WindowsProductJournalAllowance {
	a, err := NewWindowsProductJournalAllowance(32 << 20)
	if err != nil {
		panic(err)
	}
	return a
}

func productJournalFixtureContext() context.Context { return context.Background() }
