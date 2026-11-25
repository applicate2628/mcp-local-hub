//go:build windows

package api

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Small standard PAX bytes, also read by archive/tar as an independent oracle.
// This fixture is file DATA only: it issues no process or publisher observation.
func privateCacheSeedTarFixture(t *testing.T) ([]byte, privateSerenaCacheSeedDescriptor, privateSerenaCacheSeedInventory) {
	t.Helper()
	mtime := "1700000000123456700"
	entries := []privateSerenaCacheSeedEntry{
		{Path: "wheel", Kind: "directory", MtimeNS: mtime},
		{Path: "wheel/example.txt", Kind: "file", Bytes: 4, SHA256: preparedSerenaHash([]byte("seed")), MtimeNS: mtime},
	}
	var archive bytes.Buffer
	header := func(name string, kind byte, size, mode, seconds int64) {
		var raw [512]byte
		copy(raw[:100], name)
		putOctal := func(start, width int, value int64) { copy(raw[start:start+width], fmt.Sprintf("%0*o", width-1, value)) }
		putOctal(100, 8, mode)
		putOctal(108, 8, 0)
		putOctal(116, 8, 0)
		putOctal(124, 12, size)
		putOctal(136, 12, seconds)
		copy(raw[148:156], "        ")
		raw[156] = kind
		copy(raw[257:263], "ustar\x00")
		copy(raw[263:265], "00")
		putOctal(329, 8, 0)
		putOctal(337, 8, 0)
		var sum int
		for _, value := range raw {
			sum += int(value)
		}
		copy(raw[148:156], fmt.Sprintf("%06o\x00 ", sum))
		archive.Write(raw[:])
	}
	paxRecord := func(key, value string) string {
		body := " " + key + "=" + value + "\n"
		n := len(body) + 1
		for len(strconv.Itoa(n))+len(body) != n {
			n = len(strconv.Itoa(n)) + len(body)
		}
		return strconv.Itoa(n) + body
	}
	pad := func(n int64) { archive.Write(make([]byte, (512-n%512)%512)) }
	for _, entry := range entries {
		pax := paxRecord("path", entry.Path) + paxRecord("mtime", "1700000000.123456700")
		header("././@PaxHeader", 'x', int64(len(pax)), 0, 0)
		archive.WriteString(pax)
		pad(int64(len(pax)))
		kind, mode := byte('0'), int64(0600)
		if entry.Kind == "directory" {
			kind, mode = '5', 0700
		}
		header(entry.Path, kind, entry.Bytes, mode, 1700000000)
		if entry.Kind == "file" {
			archive.WriteString("seed")
		}
		pad(entry.Bytes)
	}
	archive.Write(make([]byte, 1024))
	raw := archive.Bytes()
	d := privateSerenaCacheSeedDescriptor{}
	d.Archive.Bytes, d.Archive.SHA256 = int64(len(raw)), preparedSerenaHash(raw)
	v := privateSerenaCacheSeedInventory{Schema: privateSerenaCacheSeedInventorySchema, RootMtimeNS: mtime, EntriesCount: 2, TotalBytes: 4, Entries: entries}
	return raw, d, v
}

func TestPrivateSerenaCacheSeedTarRequiresPhysicalClosure(t *testing.T) {
	raw, descriptor, inventory := privateCacheSeedTarFixture(t)
	oracle := tar.NewReader(bytes.NewReader(raw))
	for _, expected := range inventory.Entries {
		h, err := oracle.Next()
		if err != nil || strings.TrimSuffix(h.Name, "/") != expected.Path || strconv.FormatInt(h.ModTime.UnixNano(), 10) != expected.MtimeNS {
			t.Fatalf("independent standard tar reader rejected fixture: %+v %v", h, err)
		}
		body, err := io.ReadAll(oracle)
		if err != nil || int64(len(body)) != expected.Bytes || expected.Kind == "file" && preparedSerenaHash(body) != expected.SHA256 {
			t.Fatal("fixture data differs", err)
		}
	}
	if _, err := oracle.Next(); err != io.EOF {
		t.Fatal("fixture has extra entry", err)
	}
	consume := func(entry privateSerenaCacheSeedEntry, r io.Reader) error {
		_, err := io.Copy(io.Discard, r)
		return err
	}
	if err := readPrivateSerenaCacheTar(context.Background(), bytes.NewReader(raw), descriptor, inventory, consume); err != nil {
		t.Fatal(err)
	}
	wrongDigest := descriptor
	wrongDigest.Archive.SHA256 = strings.Repeat("0", 64)
	consumed := 0
	err := readPrivateSerenaCacheTar(context.Background(), bytes.NewReader(raw), wrongDigest, inventory, func(entry privateSerenaCacheSeedEntry, r io.Reader) error {
		if err := consume(entry, r); err != nil {
			return err
		}
		consumed++
		return nil
	})
	if consumed != len(inventory.Entries) || err == nil || !strings.Contains(err.Error(), "ARCHIVE_DIGEST") {
		t.Fatalf("valid tar must reach and fail the final pinned archive hash: entries=%d err=%v", consumed, err)
	}
	for _, tc := range []struct {
		name   string
		change func([]byte) []byte
		repin  bool
	}{
		{"header_integrity", func(b []byte) []byte { b[2000] ^= 1; return b }, false},
		{"concatenated_archive", func(b []byte) []byte { return append(b, raw...) }, true},
		{"physical_truncation", func(b []byte) []byte { return b[:len(b)-1] }, false},
		{"nonzero_tail", func(b []byte) []byte { return append(b, append([]byte{1}, make([]byte, 511)...)...) }, true},
		{"file_content", func(b []byte) []byte {
			at := bytes.Index(b, []byte("seed"))
			if at < 0 {
				t.Fatal("fixture body absent")
			}
			b[at] = 'X'
			return b
		}, true},
		{"padding", func(b []byte) []byte { at := bytes.Index(b, []byte("seed")); b[at+4] = 1; return b }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := tc.change(append([]byte(nil), raw...))
			d := descriptor
			if tc.repin {
				d.Archive.Bytes, d.Archive.SHA256 = int64(len(changed)), preparedSerenaHash(changed)
			}
			if err := readPrivateSerenaCacheTar(context.Background(), bytes.NewReader(changed), d, inventory, consume); err == nil {
				t.Fatal("invalid archive accepted")
			}
		})
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if err := readPrivateSerenaCacheTar(canceled, bytes.NewReader(raw), descriptor, inventory, func(privateSerenaCacheSeedEntry, io.Reader) error { called = true; return nil }); !errors.Is(err, context.Canceled) || called {
		t.Fatalf("cancellation entered destination: %v %v", called, err)
	}
	original := errors.New("original destination write refused")
	if err := readPrivateSerenaCacheTar(context.Background(), bytes.NewReader(raw), descriptor, inventory, func(privateSerenaCacheSeedEntry, io.Reader) error { return original }); !errors.Is(err, original) {
		t.Fatal("original write cause lost", err)
	}
	if err := readPrivateSerenaCacheTar(context.Background(), bytes.NewReader(raw), descriptor, inventory, func(privateSerenaCacheSeedEntry, io.Reader) error { return nil }); err == nil {
		t.Fatal("unconsumed file bytes accepted")
	}
}

func TestPrivateSerenaCacheSeedInventoryRejectsAliasesAndBounds(t *testing.T) {
	_, d, initial := privateCacheSeedTarFixture(t)
	decode := func(v privateSerenaCacheSeedInventory, limits SerenaTreeLimits) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		d.Manifest.Path, d.Manifest.Schema = "cache-manifest.json", v.Schema
		d.Manifest.Bytes, d.Manifest.SHA256 = int64(len(raw)), preparedSerenaHash(raw)
		d.Manifest.EntriesCount, d.Manifest.TotalBytes, d.Manifest.RootMtimeNS = v.EntriesCount, v.TotalBytes, v.RootMtimeNS
		_, err = decodePrivateSerenaCacheSeedInventory(raw, d, limits)
		return err
	}
	if err := decode(initial, DefaultSerenaTreeLimits()); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../outside", "/absolute", "wheel/../escape", "wheel/stream:ads", "wheel/trailing.", "wheel/trailing ", "wheel/NUL.txt", "wheel/COM1", "wheel/a\\b", "wheel/a\x00b", "wheel/a?b", "wheel/" + strings.Repeat("x", 256)} {
		t.Run(strconv.Quote(bad), func(t *testing.T) {
			v := initial
			v.Entries = append([]privateSerenaCacheSeedEntry(nil), initial.Entries...)
			v.Entries[1].Path = bad
			if err := decode(v, DefaultSerenaTreeLimits()); err == nil {
				t.Fatal("unsafe name accepted")
			}
		})
	}
	for _, mutate := range []func(*privateSerenaCacheSeedInventory){
		func(v *privateSerenaCacheSeedInventory) { v.Entries[1].Path = "WHEEL" },
		func(v *privateSerenaCacheSeedInventory) { v.Entries[1].Path = "missing/file" },
		func(v *privateSerenaCacheSeedInventory) { v.Entries[1].MtimeNS = "1700000000123456701" },
		func(v *privateSerenaCacheSeedInventory) { v.Entries[0].Kind = "symlink" },
		func(v *privateSerenaCacheSeedInventory) { v.Entries[0].Bytes = 1 },
		func(v *privateSerenaCacheSeedInventory) { v.Entries[1].Bytes = privateSerenaCacheSeedBytesLimit + 1 },
		func(v *privateSerenaCacheSeedInventory) { v.TotalBytes++ },
		func(v *privateSerenaCacheSeedInventory) { v.Entries[0], v.Entries[1] = v.Entries[1], v.Entries[0] },
	} {
		v := initial
		v.Entries = append([]privateSerenaCacheSeedEntry(nil), initial.Entries...)
		mutate(&v)
		if err := decode(v, DefaultSerenaTreeLimits()); err == nil {
			t.Fatal("invalid inventory accepted", v)
		}
	}
	limits := DefaultSerenaTreeLimits()
	limits.Entries = 12
	if err := decode(initial, limits); err == nil {
		t.Fatal("object plus stream budget not enforced")
	}
	for _, value := range []string{"-1", "+100", "0100", "1e9", "9223372036854775808", "100.0"} {
		if _, err := privateSerenaCacheSeedNanoseconds(value); err == nil {
			t.Fatal("ambiguous/out-of-range timestamp accepted", value)
		}
	}
	for _, raw := range []string{`{"path":"a","path":"b"}`, `{"path":"a","extra":1}`, `{"path":null}`, `{"path":"a"} {}`} {
		if _, err := privateSerenaCacheSeedObject([]byte(raw), new(struct {
			Path string `json:"path"`
		}), "path"); err == nil {
			t.Fatal("non-exact JSON accepted", raw)
		}
	}
}

func TestPrivateSerenaCacheSeedCannotSupplyPublisherOrProbes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	assembly := &privatePublisherRefusingAssembly{}
	seed := PrivateSerenaCacheSeed{DescriptorPath: `D:\private-seed\descriptor.json`, DescriptorSHA256: strings.Repeat("a", 64)}
	for _, values := range [][]PrivateSerenaCacheSeed{{seed}, {seed, seed}, {{DescriptorPath: "relative", DescriptorSHA256: seed.DescriptorSHA256}}} {
		owner, err := NewAPIWithPrivateSerenaIntegrationPublisher(ctx, assembly, values...)
		if owner != nil || err == nil {
			t.Fatal("data admitted an actual api.test as cli.test publisher", err)
		}
		assembly.assertUnused(t)
	}
	if err := validatePrivateSerenaCacheSeedBinding(SerenaPreparationInputs{}, nil); err != nil {
		t.Fatal("cold default changed", err)
	}
	private := &privateSerenaPublisherOwner{cacheSeedConfig: &seed}
	if err := validatePrivateSerenaCacheSeedBinding(SerenaPreparationInputs{}, private); err == nil {
		t.Fatal("configured seed silently fell back to cold inputs")
	}
	request := preparedRequest()
	before := request
	coldOwner := &privateSerenaPublisherOwner{}
	cold, err := coldOwner.bindCacheSeed(ctx, SerenaPreparationInputs{Request: request})
	if err != nil || cold.Request != before || cold.cacheSeed != nil || coldOwner.cacheSeed != nil {
		t.Fatal("nil seed changed request identity or acquired seed state", err)
	}
	if _, err := bindPrivateSerenaCacheSeedRequest(request, seed.DescriptorSHA256); err == nil {
		t.Fatal("synthetic request issued original probe context")
	}
	if request != before {
		t.Fatal("failed bind changed caller's request")
	}
	data := &privateSerenaCacheSeed{}
	if err := data.matchesInputs(SerenaPreparationInputs{Request: request}); err == nil {
		t.Fatal("absent actual probes accepted")
	}
	if _, err := private.bindCacheSeed(ctx, SerenaPreparationInputs{}); err == nil {
		t.Fatal("partial publisher bound seed")
	}
}
