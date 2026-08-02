package clients

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
)

func TestWithConfigLock_ReleaseFailureIsJoinedAndLaterAcquireFailsFast(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "client.json")
	lockPath := configPath + ".lock"
	unlockFailure := errors.New("injected config unlock failure")
	previous := configFlockUnlockFn
	var stranded []*flock.Flock
	configFlockUnlockFn = func(fl *flock.Flock) error {
		if fl.Path() == lockPath {
			stranded = append(stranded, fl)
			return unlockFailure
		}
		return previous(fl)
	}
	t.Cleanup(func() {
		configFlockUnlockFn = previous
		for _, fl := range stranded {
			_ = fl.Unlock()
		}
		unconfirmedConfigLockReleasesMu.Lock()
		delete(unconfirmedConfigLockReleases, lockPath)
		unconfirmedConfigLockReleasesMu.Unlock()
	})

	primary := errors.New("primary mutation failure")
	err := withConfigLock(configPath, func() error { return primary })
	if !errors.Is(err, primary) {
		t.Fatalf("withConfigLock error = %v, want primary cause", err)
	}
	if !errors.Is(err, unlockFailure) {
		t.Fatalf("withConfigLock error = %v, want unlock cause", err)
	}
	if !errors.Is(err, ErrConfigLockReleaseUnconfirmed) {
		t.Fatalf("withConfigLock error = %v, want ErrConfigLockReleaseUnconfirmed", err)
	}

	reentered := false
	err = withConfigLock(configPath, func() error {
		reentered = true
		return nil
	})
	if reentered {
		t.Fatal("later acquire entered the critical section despite an unconfirmed retained lock")
	}
	if !errors.Is(err, ErrConfigLockReleaseUnconfirmed) {
		t.Fatalf("later acquire error = %v, want ErrConfigLockReleaseUnconfirmed", err)
	}
}

func TestPruneContentFailureRemainsBestEffort(t *testing.T) {
	livePath := filepath.Join(t.TempDir(), "client.json")
	sentinelPath := livePath + originalSentinelSuffix
	if err := os.WriteFile(sentinelPath, []byte("pristine"), 0o600); err != nil {
		t.Fatal(err)
	}
	backups := []string{
		livePath + backupSuffixPrefix + "20260801-000001",
		livePath + backupSuffixPrefix + "20260801-000002",
		livePath + backupSuffixPrefix + "20260801-000003",
		livePath + backupSuffixPrefix + "20260801-000004",
	}
	for _, backup := range backups {
		if err := os.WriteFile(backup, []byte(backup), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	failPath := backups[2]
	laterPath := backups[1]
	var attempts []string
	pruneOldTimestampedWithRemove(livePath, 1, func(path string) error {
		attempts = append(attempts, path)
		if path == failPath {
			return errors.New("injected timestamped backup removal failure")
		}
		return os.Remove(path)
	})
	failIndex := -1
	laterIndex := -1
	for index, attempt := range attempts {
		if attempt == failPath {
			failIndex = index
		}
		if attempt == laterPath {
			laterIndex = index
		}
	}
	if failIndex < 0 || laterIndex <= failIndex {
		t.Fatalf("best-effort removal attempts = %v, want later eligible removal after %q failure", attempts, failPath)
	}
	if got, err := os.ReadFile(sentinelPath); err != nil || string(got) != "pristine" {
		t.Fatalf("original sentinel = %q err=%v, want preserved bytes", got, err)
	}
	if _, err := os.Stat(failPath); err != nil {
		t.Fatalf("failed eligible backup = %v, want preserved failed-removal path", err)
	}
}

func TestPruneReleaseFailureIsTyped(t *testing.T) {
	livePath := filepath.Join(t.TempDir(), "client.json")
	backupPath := livePath + backupSuffixPrefix + "20260801-000000"
	if err := os.WriteFile(backupPath, []byte("backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	lockPath := livePath + ".lock"
	releaseCause := errors.New("injected prune unlock failure")
	previous := configFlockUnlockFn
	var stranded []*flock.Flock
	configFlockUnlockFn = func(fl *flock.Flock) error {
		if fl.Path() == lockPath {
			stranded = append(stranded, fl)
			return releaseCause
		}
		return previous(fl)
	}
	t.Cleanup(func() {
		configFlockUnlockFn = previous
		for _, fl := range stranded {
			_ = fl.Unlock()
		}
		unconfirmedConfigLockReleasesMu.Lock()
		delete(unconfirmedConfigLockReleases, lockPath)
		unconfirmedConfigLockReleasesMu.Unlock()
	})

	err := PruneBackupsForBackupPath(backupPath, 1)
	if !errors.Is(err, ErrConfigLockReleaseUnconfirmed) || !errors.Is(err, releaseCause) {
		t.Fatalf("PruneBackupsForBackupPath error = %v, want lifecycle class and cause", err)
	}
	laterCandidate := livePath + backupSuffixPrefix + "20260802-000001"
	laterNewest := livePath + backupSuffixPrefix + "20260802-000002"
	if err := os.WriteFile(laterCandidate, []byte("later-candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(laterNewest, []byte("later-newest"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PruneBackupsForBackupPath(backupPath, 1); !errors.Is(err, ErrConfigLockReleaseUnconfirmed) {
		t.Fatalf("later prune error = %v, want retained-lock fail-fast", err)
	}
	if _, err := os.Stat(laterCandidate); err != nil {
		t.Fatalf("later retained-lock acquire reached content work: %v", err)
	}
}
