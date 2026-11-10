//go:build windows

package binaryadmission

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func canonicalEntryRebuildVersionFixture(block versionInfoBlock) []byte {
	children := make([][]byte, 0, len(block.children))
	for _, child := range block.children {
		children = append(children, canonicalEntryRebuildVersionFixture(child))
	}
	return versionFixtureBlock(block.key, block.value, block.valueWords, block.blockType, children...)
}

func canonicalEntrySetFixture(t *testing.T) (WindowsCanonicalEntrySetTargetsV1, WindowsCanonicalEntryBuildBindingV1) {
	t.Helper()
	root := t.TempDir()
	targets, err := CanonicalEntryTargetsV1(filepath.Join(root, "mcphub.exe"))
	if err != nil {
		t.Fatal(err)
	}
	launcherFixture := writeWindowsPEFixture(t, 0x20b, WindowsCUISubsystem, func(raw []byte) { binary.LittleEndian.PutUint16(raw[0x80+4:0x80+6], 0x8664) })
	launcher, err := os.ReadFile(launcherFixture)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(targets.CanonicalEntryPath, launcher, 0o600); err != nil {
		t.Fatal(err)
	}
	binding, err := NewWindowsCanonicalEntryBuildBindingV1(strings.Repeat("a", 32), canonicalEntrySHA256(launcher), WindowsCanonicalEntryReverseProjectionV1)
	if err != nil {
		t.Fatal(err)
	}
	const version, commit, date = "0.4.41", "dddddddddddddddddddddddddddddddddddddddd", "2026-11-10T12:34:56Z"
	productFixture := writeWindowsPEVersionFixture(t, WindowsArtifactRoleProduct, version, commit, date, false)
	product, err := os.ReadFile(productFixture)
	if err != nil {
		t.Fatal(err)
	}
	resource := buildVersionResourceFixture(WindowsArtifactRoleProduct, version, commit, date, false)
	block, _, err := parseVersionInfoBlock(resource, 0)
	if err != nil {
		t.Fatal(err)
	}
	comments, err := EncodeWindowsCanonicalEntryCommentsV1("fixture\r\n\nmcphub-source-v1;sha256="+strings.Repeat("f", 64), binding)
	if err != nil {
		t.Fatal(err)
	}
	commentBlock, _, err := parseVersionInfoBlock(versionStringFixture("Comments", comments), 0)
	if err != nil {
		t.Fatal(err)
	}
	block.children[0].children[0].children = append(block.children[0].children[0].children, commentBlock)
	resource = canonicalEntryRebuildVersionFixture(block)
	product = append(product[:0x280:0x280], resource...)
	binary.LittleEndian.PutUint32(product[0x80+24+132:0x80+24+136], uint32(0x80+len(resource)))
	section := 0x80 + 24 + 0xf0
	binary.LittleEndian.PutUint32(product[section+8:section+12], uint32(0x80+len(resource)))
	binary.LittleEndian.PutUint32(product[section+16:section+20], uint32(0x80+len(resource)))
	binary.LittleEndian.PutUint32(product[0x264:0x268], uint32(len(resource)))
	if err = os.Mkdir(filepath.Dir(targets.ProductPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(targets.ProductPath, product, 0o600); err != nil {
		t.Fatal(err)
	}
	i := WindowsProductIdentityV1{"windows-product-identity-v1", WindowsArtifactRoleProduct, "mcphub.exe", 2, 0x8664, version, commit, date, canonicalEntrySHA256(product)}
	raw, err := json.MarshalIndent(i, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\r', '\n')
	if err = os.WriteFile(targets.IdentityPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return targets, binding
}

func closeCanonicalEntryFixtureOwner(t *testing.T, o *WindowsCanonicalEntrySetV1) {
	t.Helper()
	t.Cleanup(func() {
		if err := o.Close(); err != nil {
			t.Errorf("close original fixture owner: %v", err)
		}
	})
}

func TestOriginalCanonicalEntrySetBuildBindingAndRetainedGuards(t *testing.T) {
	targets, binding := canonicalEntrySetFixture(t)
	o, err := AdmitWindowsCanonicalEntrySetV1(context.Background(), targets.CanonicalEntryPath, binding)
	closeCanonicalEntryFixtureOwner(t, o)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := o.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ProductArtifact.Path != targets.ProductPath || snapshot.CanonicalEntry.SHA256 != binding.ExpectedCanonicalEntrySHA256() || snapshot.Machine != 0x8664 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	rawIdentity, err := os.ReadFile(targets.IdentityPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snapshot.IdentityBytes, rawIdentity) {
		t.Fatal("exact identity-v1 bytes lost")
	}
	snapshot.IdentityBytes[0] ^= 1
	fresh, err := o.Snapshot()
	if err != nil || !bytes.Equal(fresh.IdentityBytes, rawIdentity) {
		t.Fatal("detached snapshot mutated owner")
	}
	for _, path := range []string{targets.CanonicalEntryPath, targets.ProductPath, targets.IdentityPath} {
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err == nil {
			f.Close()
			t.Fatalf("write opened held original %s", path)
		}
		if err = os.Rename(path, path+".displaced"); err == nil {
			t.Fatalf("renamed held original %s", path)
		}
	}
	if err := o.VerifyUnchanged(); err != nil {
		t.Fatal(err)
	}
	if err := o.VerifyOriginalProductProcessHandle(0); err == nil {
		t.Fatal("missing native process handle accepted")
	}
	wire, err := json.Marshal(fresh)
	if err != nil {
		t.Fatal(err)
	}
	var expected WindowsCanonicalEntrySetSnapshotV1
	if err = json.Unmarshal(wire, &expected); err != nil {
		t.Fatal(err)
	}
	readback, err := AcquireWindowsCanonicalEntrySetV1(context.Background(), targets, expected)
	closeCanonicalEntryFixtureOwner(t, readback)
	if err != nil {
		t.Fatalf("journal roundtrip reader: %v", err)
	}
}

func TestOriginalCanonicalEntryRefusesSubstitutedEntryAndRetainsPartialOwner(t *testing.T) {
	targets, binding := canonicalEntrySetFixture(t)
	foreign, err := os.ReadFile(targets.CanonicalEntryPath)
	if err != nil {
		t.Fatal(err)
	}
	foreign[len(foreign)-1] ^= 1
	if err = os.WriteFile(targets.CanonicalEntryPath, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	o, err := AdmitWindowsCanonicalEntrySetV1(context.Background(), targets.CanonicalEntryPath, binding)
	if o == nil || err == nil {
		t.Fatalf("substituted valid PE3 owner=%v error=%v", o, err)
	}
	if _, err := o.Snapshot(); err == nil {
		t.Fatal("failed acquisition exposed admitted snapshot")
	}
	first := o.Close()
	second := o.Close()
	if first != second {
		t.Fatalf("close result not cached: %v / %v", first, second)
	}
	if err = os.WriteFile(targets.CanonicalEntryPath, foreign, 0o600); err != nil {
		t.Fatalf("partial owner leaked guard after Close: %v", err)
	}
}

func TestOriginalCanonicalEntryMissingIdentityClosesEveryPartialGuard(t *testing.T) {
	targets, binding := canonicalEntrySetFixture(t)
	if err := os.Remove(targets.IdentityPath); err != nil {
		t.Fatal(err)
	}
	o, err := AdmitWindowsCanonicalEntrySetV1(context.Background(), targets.CanonicalEntryPath, binding)
	if o == nil || err == nil {
		t.Fatalf("missing identity owner=%v error=%v", o, err)
	}
	if err = o.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(targets.ProductPath, targets.ProductPath+".released"); err != nil {
		t.Fatalf("partial guard not released: %v", err)
	}
}

func TestOriginalCanonicalEntrySetCompleteRelocationAndWrongExpectedGeneration(t *testing.T) {
	targets, binding := canonicalEntrySetFixture(t)
	newRoot := filepath.Join(t.TempDir(), "relocated")
	if err := os.Rename(filepath.Dir(targets.CanonicalEntryPath), newRoot); err != nil {
		t.Fatal(err)
	}
	newTargets, err := CanonicalEntryTargetsV1(filepath.Join(newRoot, "mcphub.exe"))
	if err != nil {
		t.Fatal(err)
	}
	o, err := AdmitWindowsCanonicalEntrySetV1(context.Background(), newTargets.CanonicalEntryPath, binding)
	closeCanonicalEntryFixtureOwner(t, o)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := o.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	expected.BuildID = strings.Repeat("b", 32)
	wrong, err := AcquireWindowsCanonicalEntrySetV1(context.Background(), newTargets, expected)
	closeCanonicalEntryFixtureOwner(t, wrong)
	if err == nil {
		t.Fatal("different journaled generation accepted")
	}
}

func TestCanonicalEntryStageReceivesBytesAndTransfersDispositionOnlyOnce(t *testing.T) {
	targets, binding := canonicalEntrySetFixture(t)
	o, err := AdmitWindowsCanonicalEntrySetV1(context.Background(), targets.CanonicalEntryPath, binding)
	closeCanonicalEntryFixtureOwner(t, o)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "mcphub.exe")
	staged, err := o.Stage(context.Background(), destination)
	if staged != nil {
		t.Cleanup(func() {
			if err := staged.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	actualPaths, err := staged.Targets()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(actualPaths.ProductPath) != filepath.Dir(destination) {
		t.Fatal("staging created Product directory instead of transaction")
	}
	if err := staged.VerifyUnchanged(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := staged.TakeForTransaction(context.Background(), destination+".wrong"); err == nil {
		t.Fatal("wrong destination accepted")
	}
	snapshot, paths, err := staged.TakeForTransaction(context.Background(), destination)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Product.Path != paths.ProductPath || paths != actualPaths {
		t.Fatal("Take changed acquired staged paths")
	}
	if _, _, err := staged.TakeForTransaction(context.Background(), destination); err == nil {
		t.Fatal("second Take accepted")
	}
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.CanonicalEntryPath, paths.ProductPath, paths.IdentityPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("Close after Take deleted transaction file: %v", err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCanonicalEntryStageCloseBeforeTakeCleansOnlyOriginalStages(t *testing.T) {
	targets, binding := canonicalEntrySetFixture(t)
	o, err := AdmitWindowsCanonicalEntrySetV1(context.Background(), targets.CanonicalEntryPath, binding)
	closeCanonicalEntryFixtureOwner(t, o)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "mcphub.exe")
	staged, err := o.Stage(context.Background(), destination)
	if err != nil {
		if staged != nil {
			staged.Close()
		}
		t.Fatal(err)
	}
	paths, err := staged.Targets()
	if err != nil {
		t.Fatal(err)
	}
	if err = staged.Close(); err != nil {
		t.Fatal(err)
	}
	if err = staged.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.CanonicalEntryPath, paths.ProductPath, paths.IdentityPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stage still present after Close: %s %v", path, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	partial, err := o.Stage(ctx, destination)
	if partial == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stage owner=%v error=%v", partial, err)
	}
	if err = partial.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalEntryStageOriginalMoveDeniesForeignSwapAtPromotionBarrier(t *testing.T) {
	targets, binding := canonicalEntrySetFixture(t)
	o, err := AdmitWindowsCanonicalEntrySetV1(context.Background(), targets.CanonicalEntryPath, binding)
	closeCanonicalEntryFixtureOwner(t, o)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "mcphub.exe")
	staged, err := o.Stage(context.Background(), destination)
	if err != nil {
		if staged != nil {
			staged.Close()
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := staged.Close(); err != nil {
			t.Error(err)
		}
	})
	paths, err := staged.Targets()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = staged.TakeForTransaction(context.Background(), destination); err != nil {
		t.Fatal(err)
	}
	if err = staged.VerifyUnchanged(); err != nil {
		t.Fatal(err)
	}
	// Explicit promotion barrier: try to replace the exposed name AFTER the
	// successful currentness check and BEFORE the actual native move call.
	foreignPath := filepath.Join(filepath.Dir(destination), "foreign.exe")
	foreign := []byte("unrelated occupant")
	if err = os.WriteFile(foreignPath, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(paths.CanonicalEntryPath, paths.CanonicalEntryPath+".moved"); err == nil {
		t.Fatal("foreign rename bypassed continuous original stage guard")
	}
	if err = os.Rename(foreignPath, paths.CanonicalEntryPath); err == nil {
		t.Fatal("foreign replacement bypassed continuous original stage guard")
	}
	for _, invalid := range []struct{ member, path string }{{"unknown", "safe.aside"}, {"launcher", "../escape"}} {
		result, err := staged.MoveOriginal(context.Background(), invalid.member, invalid.path)
		if err == nil || result.Attempted || result.Applied {
			t.Fatalf("invalid move result=%+v error=%v", result, err)
		}
	}
	result, err := staged.MoveOriginal(context.Background(), "launcher", "mcphub.exe")
	if err != nil || !result.Applied || !result.Readback || !result.NativeStatusKnown || result.NativeStatus != 0 {
		t.Fatalf("original move=%+v error=%v", result, err)
	}
	present, actual, actualHash, err := staged.ReadOriginal("launcher")
	if err != nil || !present || actualHash != binding.ExpectedCanonicalEntrySHA256() || bytes.Equal(actual, foreign) {
		t.Fatalf("destination is not original entry: %v", err)
	}
	back := filepath.Base(paths.CanonicalEntryPath)
	result, err = staged.MoveOriginal(context.Background(), "launcher", back)
	if err != nil || !result.Applied || !result.Readback {
		t.Fatalf("original inverse=%+v error=%v", result, err)
	}
	for _, member := range []struct{ name, path string }{{"launcher", paths.CanonicalEntryPath}, {"product", paths.ProductPath}, {"identity", paths.IdentityPath}} {
		result, err := staged.DiscardOriginal(context.Background(), member.name, filepath.Base(member.path))
		if err != nil || !result.Applied || !result.Readback {
			t.Fatalf("discard original stage=%+v error=%v", result, err)
		}
		cached, cachedErr := staged.DiscardOriginal(context.Background(), member.name, filepath.Base(member.path))
		if cachedErr != err || cached.Applied != result.Applied || cached.Readback != result.Readback {
			t.Fatal("native discard outcome was not cached")
		}
	}
	actual, err = os.ReadFile(foreignPath)
	if err != nil || !bytes.Equal(actual, foreign) {
		t.Fatalf("foreign file changed: %q %v", actual, err)
	}
}

func TestOriginalCanonicalEntryRefusesHardlinkedObjects(t *testing.T) {
	targets, binding := canonicalEntrySetFixture(t)
	if err := os.Link(targets.CanonicalEntryPath, targets.CanonicalEntryPath+".link"); err != nil {
		t.Fatal(err)
	}
	o, err := AdmitWindowsCanonicalEntrySetV1(context.Background(), targets.CanonicalEntryPath, binding)
	closeCanonicalEntryFixtureOwner(t, o)
	if o == nil || err == nil {
		t.Fatalf("multiply linked original owner=%v error=%v", o, err)
	}
}

func TestCanonicalEntryPriorOriginalMoveInverseAndCommittedRetirement(t *testing.T) {
	for _, mode := range []string{"inverse", "commit"} {
		t.Run(mode, func(t *testing.T) {
			targets, binding := canonicalEntrySetFixture(t)
			source, err := AdmitWindowsCanonicalEntrySetV1(context.Background(), targets.CanonicalEntryPath, binding)
			closeCanonicalEntryFixtureOwner(t, source)
			if err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "mcphub.exe")
			old := []byte("original canonical prior preimage")
			if err = os.WriteFile(destination, old, 0o600); err != nil {
				t.Fatal(err)
			}
			staged, err := source.Stage(context.Background(), destination)
			if staged != nil {
				t.Cleanup(func() {
					if err := staged.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			paths, err := staged.Targets()
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = staged.TakeForTransaction(context.Background(), destination); err != nil {
				t.Fatal(err)
			}
			if err = source.Close(); err != nil {
				t.Fatal(err)
			}
			prior, err := staged.AcquireOriginalPriorSet(context.Background())
			if prior != nil {
				t.Cleanup(func() {
					if err := prior.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			present, body, hash, err := prior.ReadOriginal("launcher")
			if err != nil || !present || !bytes.Equal(body, old) || hash != canonicalEntrySHA256(old) {
				t.Fatalf("prior receiving present=%t bytes=%q hash=%s error=%v", present, body, hash, err)
			}
			for _, member := range []string{"product", "identity"} {
				present, body, hash, err = prior.ReadOriginal(member)
				if err != nil || present || body != nil || hash != "" {
					t.Fatalf("prior absence %s present=%t bytes=%q hash=%s error=%v", member, present, body, hash, err)
				}
			}
			if _, err = os.Stat(filepath.Join(filepath.Dir(destination), "mcphub-product")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("prior acquisition created absent parent: %v", err)
			}
			if result, err := prior.DiscardOriginal(context.Background(), "launcher", "mcphub.exe"); err == nil || result.Attempted {
				t.Fatal("discard accepted original canonical destination")
			}
			aside := ".fixture-prior.aside"
			result, err := prior.MoveOriginal(context.Background(), "launcher", aside)
			if err != nil || !result.Applied || !result.Readback {
				t.Fatalf("prior aside=%+v error=%v", result, err)
			}
			result, err = staged.MoveOriginal(context.Background(), "launcher", "mcphub.exe")
			if err != nil || !result.Applied || !result.Readback {
				t.Fatalf("successor publication=%+v error=%v", result, err)
			}
			if mode == "inverse" {
				withdrawn := ".fixture-successor-withdrawn.aside"
				result, err = staged.MoveOriginal(context.Background(), "launcher", withdrawn)
				if err != nil || !result.Applied || !result.Readback {
					t.Fatalf("successor withdrawal=%+v error=%v", result, err)
				}
				result, err = prior.MoveOriginal(context.Background(), "launcher", "mcphub.exe")
				if err != nil || !result.Applied || !result.Readback {
					t.Fatalf("prior restoration=%+v error=%v", result, err)
				}
				result, err = staged.DiscardOriginal(context.Background(), "launcher", withdrawn)
				if err != nil || !result.Readback {
					t.Fatalf("withdrawn cleanup=%+v error=%v", result, err)
				}
				if err = prior.Close(); err != nil {
					t.Fatal(err)
				}
				actual, err := os.ReadFile(destination)
				if err != nil || !bytes.Equal(actual, old) {
					t.Fatalf("prior.Close changed restored prior: %q %v", actual, err)
				}
			} else {
				result, err = prior.DiscardOriginal(context.Background(), "launcher", aside)
				if err != nil || !result.Applied || !result.Readback {
					t.Fatalf("committed prior retirement=%+v error=%v", result, err)
				}
				if err = prior.Close(); err != nil {
					t.Fatal(err)
				}
				present, actual, actualHash, err := staged.ReadOriginal("launcher")
				if err != nil || !present || actualHash != binding.ExpectedCanonicalEntrySHA256() || canonicalEntrySHA256(actual) != actualHash {
					t.Fatalf("prior.Close changed committed successor: %v", err)
				}
			}
			// Mixed repair's unused stages require explicit post-Take disposition.
			for _, unused := range []struct{ name, path string }{{"product", paths.ProductPath}, {"identity", paths.IdentityPath}} {
				result, err = staged.DiscardOriginal(context.Background(), unused.name, filepath.Base(unused.path))
				if err != nil || !result.Readback {
					t.Fatalf("unused stage=%+v error=%v", result, err)
				}
			}
		})
	}
}

func canonicalEntryShortPathFixture(t *testing.T, path string) string {
	t.Helper()
	long, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	size, err := windows.GetShortPathName(long, nil, 0)
	if err != nil || size == 0 {
		t.Skipf("actual short-path availability unqualified: size=%d error=%v", size, err)
	}
	for {
		buffer := make([]uint16, int(size))
		n, err := windows.GetShortPathName(long, &buffer[0], size)
		if err != nil {
			t.Skipf("actual short-path availability unqualified: %v", err)
		}
		if n < size {
			return windows.UTF16ToString(buffer[:n])
		}
		if n == ^uint32(0) {
			t.Skip("short-path API representation unavailable")
		}
		size = n + 1
	}
}

func TestCanonicalEntryDiscardRefusesActualShortAliasOfCanonicalProduct(t *testing.T) {
	targets, binding := canonicalEntrySetFixture(t)
	source, err := AdmitWindowsCanonicalEntrySetV1(context.Background(), targets.CanonicalEntryPath, binding)
	closeCanonicalEntryFixtureOwner(t, source)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	destination := filepath.Join(root, "mcphub.exe")
	productDir := filepath.Join(root, "mcphub-product")
	if err = os.Mkdir(productDir, 0o700); err != nil {
		t.Fatal(err)
	}
	longInfo, err := os.Stat(productDir)
	if err != nil {
		t.Fatal(err)
	}
	shortRoot := canonicalEntryShortPathFixture(t, root)
	shortDirectory := canonicalEntryShortPathFixture(t, productDir)
	shortInfo, err := os.Stat(shortDirectory)
	if err != nil || !os.SameFile(longInfo, shortInfo) {
		t.Skipf("actual short directory alias not qualified: %v", err)
	}
	alias, err := filepath.Rel(shortRoot, shortDirectory)
	if err != nil {
		t.Fatal(err)
	}
	alias = filepath.ToSlash(alias)
	if strings.EqualFold(alias, "mcphub-product") || !publisherSourcePathValid(alias) {
		t.Skipf("filesystem supplied no distinct qualified directory short alias: %q", alias)
	}
	canonicalProduct := filepath.Join(productDir, "mcphub.exe")
	old := []byte("original Product protected through actual native alias")
	if err = os.WriteFile(canonicalProduct, old, 0o600); err != nil {
		t.Fatal(err)
	}
	staged, err := source.Stage(context.Background(), destination)
	if staged != nil {
		t.Cleanup(func() {
			if err := staged.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = source.Close(); err != nil {
		t.Fatal(err)
	}
	prior, err := staged.AcquireOriginalPriorSet(context.Background())
	if prior != nil {
		t.Cleanup(func() {
			if err := prior.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	aside := ".fixture-alias-prior.aside"
	if result, err := prior.MoveOriginal(context.Background(), "product", aside); err != nil || !result.Readback {
		t.Fatalf("move to original aside: %+v %v", result, err)
	}
	aliasProduct := alias + "/mcphub.exe"
	if result, err := prior.MoveOriginal(context.Background(), "product", aliasProduct); err != nil || !result.Readback {
		t.Fatalf("restore through actual short alias: %+v %v", result, err)
	}
	result, err := prior.DiscardOriginal(context.Background(), "product", aliasProduct)
	if err == nil || result.Attempted || result.Applied {
		t.Fatalf("canonical native object discarded through short alias: %+v %v", result, err)
	}
	actual, err := os.ReadFile(canonicalProduct)
	if err != nil || !bytes.Equal(actual, old) {
		t.Fatalf("canonical Product changed: %q %v", actual, err)
	}
	if result, err = prior.MoveOriginal(context.Background(), "product", aside); err != nil || !result.Readback {
		t.Fatalf("withdraw actual original to owned aside: %+v %v", result, err)
	}
	if result, err = prior.DiscardOriginal(context.Background(), "product", aside); err != nil || !result.Applied || !result.Readback {
		t.Fatalf("real owned aside discard refused: %+v %v", result, err)
	}
}

func TestCanonicalEntryBorrowedPriorRuntimeRolesAbsenceAndLifetime(t *testing.T) {
	for _, mode := range []string{"legacy", "complete", "absent", "partial"} {
		t.Run(mode, func(t *testing.T) {
			targets, binding := canonicalEntrySetFixture(t)
			source, err := AdmitWindowsCanonicalEntrySetV1(context.Background(), targets.CanonicalEntryPath, binding)
			closeCanonicalEntryFixtureOwner(t, source)
			if err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "mcphub.exe")
			finals, err := CanonicalEntryTargetsV1(destination)
			if err != nil {
				t.Fatal(err)
			}
			copyFile := func(from, to string) {
				body, err := os.ReadFile(from)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(to, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "legacy":
				copyFile(targets.ProductPath, destination)
			case "partial":
				copyFile(targets.CanonicalEntryPath, destination)
			case "complete":
				if err = os.Mkdir(filepath.Dir(finals.ProductPath), 0o700); err != nil {
					t.Fatal(err)
				}
				copyFile(targets.CanonicalEntryPath, destination)
				copyFile(targets.ProductPath, finals.ProductPath)
				copyFile(targets.IdentityPath, finals.IdentityPath)
			}
			staged, err := source.Stage(context.Background(), destination)
			if staged != nil {
				t.Cleanup(func() {
					if err := staged.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = source.Close(); err != nil {
				t.Fatal(err)
			}
			prior, err := staged.AcquireOriginalPriorSet(context.Background())
			if prior != nil {
				t.Cleanup(func() {
					if err := prior.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			view, err := prior.RuntimeProduct(context.Background())
			if mode == "partial" {
				if err == nil || view != nil {
					t.Fatalf("partial runtime view=%v error=%v", view, err)
				}
				return
			}
			if mode == "absent" {
				if err != nil || view != nil {
					t.Fatalf("positive absent runtime view=%v error=%v", view, err)
				}
				return
			}
			if err != nil || view == nil {
				t.Fatalf("borrowed runtime view=%v error=%v", view, err)
			}
			artifact := view.Artifact()
			wantPath := destination
			if mode == "complete" {
				wantPath = finals.ProductPath
			}
			if artifact.Path != wantPath || artifact.Role != WindowsArtifactRoleProduct || artifact.Subsystem != WindowsGUISubsystem || !validLowerHex(artifact.SHA256, 64, 64) {
				t.Fatalf("borrowed artifact=%+v", artifact)
			}
			if err = view.ValidateOriginal(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err = view.VerifyOriginalProductProcessHandle(0); err == nil {
				t.Fatal("runtime view accepted missing process handle")
			}
			if err = prior.Close(); err != nil {
				t.Fatal(err)
			}
			if err = view.ValidateOriginal(context.Background()); err == nil {
				t.Fatal("borrowed runtime survived prior Close")
			}
		})
	}
}
