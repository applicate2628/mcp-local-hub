package apitest

import (
	"errors"
	"fmt"
	"os"
	"time"
)

const testMainCleanupAttempts = 20

// RemoveTestMainRoot removes one exact caller-owned test root and verifies its
// absence. Windows can briefly report a non-empty directory while a just-ended
// worker settles its final file operation, so retries are bounded and failure
// stays visible to TestMain instead of being discarded before os.Exit.
func RemoveTestMainRoot(root string) error {
	return removeTestMainRootWith(
		root,
		testMainCleanupAttempts,
		os.RemoveAll,
		os.Stat,
		func() { time.Sleep(10 * time.Millisecond) },
	)
}

func removeTestMainRootWith(
	root string,
	attempts int,
	removeAll func(string) error,
	stat func(string) (os.FileInfo, error),
	wait func(),
) error {
	if root == "" || attempts <= 0 || removeAll == nil || stat == nil || wait == nil {
		return errors.New("invalid exact test-root cleanup")
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		removeErr := removeAll(root)
		_, statErr := stat(root)
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		if removeErr != nil {
			lastErr = removeErr
		} else if statErr != nil {
			lastErr = statErr
		} else {
			lastErr = errors.New("exact test root still exists")
		}
		if attempt < attempts {
			wait()
		}
	}
	return fmt.Errorf("remove exact test root %q after %d attempts: %w", root, attempts, lastErr)
}
