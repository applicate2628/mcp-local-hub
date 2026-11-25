package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"math"
	"strconv"
	"strings"
)

// One deliberately narrow wire format, not a general-purpose tar extractor.
// Every raw header, PAX record, data byte and padding block enters the pinned
// archive digest. No pathname is used here; the original attempt owns writes.
type privateSerenaCacheSeedStream struct {
	ctx      context.Context
	source   io.Reader
	digest   hash.Hash
	expected int64
	read     int64
}

func (r *privateSerenaCacheSeedStream) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(buffer) == 0 {
		return 0, nil
	}
	if r.read == r.expected {
		var extra [1]byte
		n, err := r.source.Read(extra[:])
		if n != 0 {
			return 0, errors.New("SERENA_PRIVATE_CACHE_SEED_ARCHIVE_TRAILING")
		}
		if err == nil {
			return 0, io.ErrNoProgress
		}
		return 0, err
	}
	if int64(len(buffer)) > r.expected-r.read {
		buffer = buffer[:r.expected-r.read]
	}
	n, err := r.source.Read(buffer)
	if n > 0 {
		_, _ = r.digest.Write(buffer[:n])
		r.read += int64(n)
	}
	if n == 0 && err == nil {
		return 0, io.ErrNoProgress
	}
	return n, err
}

type privateSerenaCacheTarHeader struct {
	name       string
	kind       byte
	size, mode int64
	mtime      int64
}

func privateSerenaCacheTarString(raw []byte) (string, error) {
	if at := bytes.IndexByte(raw, 0); at >= 0 {
		if !privateSerenaCacheZero(raw[at:]) {
			return "", errors.New("SERENA_PRIVATE_CACHE_SEED_TAR_HEADER")
		}
		raw = raw[:at]
	}
	return string(raw), nil
}

func privateSerenaCacheTarOctal(raw []byte) (int64, error) {
	value := strings.Trim(string(raw), " \x00")
	if value == "" {
		return 0, nil
	}
	for _, digit := range value {
		if digit < '0' || digit > '7' {
			return 0, errors.New("SERENA_PRIVATE_CACHE_SEED_TAR_NUMBER")
		}
	}
	return strconv.ParseInt(value, 8, 64)
}

func privateSerenaCacheTarDecodeHeader(raw []byte) (h privateSerenaCacheTarHeader, err error) {
	if len(raw) != 512 || string(raw[257:263]) != "ustar\x00" || string(raw[263:265]) != "00" ||
		!privateSerenaCacheZero(raw[500:]) {
		return h, errors.New("SERENA_PRIVATE_CACHE_SEED_TAR_FORMAT")
	}
	var sum int64
	for i, value := range raw {
		if i >= 148 && i < 156 {
			sum += ' '
		} else {
			sum += int64(value)
		}
	}
	checksum, err := privateSerenaCacheTarOctal(raw[148:156])
	if err != nil || checksum != sum {
		return h, errors.Join(err, errors.New("SERENA_PRIVATE_CACHE_SEED_TAR_CHECKSUM"))
	}
	for _, field := range [][]byte{raw[108:116], raw[116:124], raw[329:337], raw[337:345]} {
		value, parseErr := privateSerenaCacheTarOctal(field)
		if parseErr != nil || value != 0 {
			return h, errors.Join(parseErr, errors.New("SERENA_PRIVATE_CACHE_SEED_TAR_OWNER_OR_DEVICE"))
		}
	}
	for _, field := range [][]byte{raw[157:257], raw[265:297], raw[297:329]} {
		value, parseErr := privateSerenaCacheTarString(field)
		if parseErr != nil || value != "" {
			return h, errors.Join(parseErr, errors.New("SERENA_PRIVATE_CACHE_SEED_TAR_LINK_OR_OWNER"))
		}
	}
	if h.name, err = privateSerenaCacheTarString(raw[:100]); err != nil {
		return h, err
	}
	prefix, err := privateSerenaCacheTarString(raw[345:500])
	if err != nil {
		return h, err
	}
	if prefix != "" {
		h.name = prefix + "/" + h.name
	}
	h.kind = raw[156]
	if h.mode, err = privateSerenaCacheTarOctal(raw[100:108]); err != nil {
		return h, err
	}
	if h.size, err = privateSerenaCacheTarOctal(raw[124:136]); err != nil {
		return h, err
	}
	h.mtime, err = privateSerenaCacheTarOctal(raw[136:148])
	return h, err
}

func privateSerenaCacheZero(raw []byte) bool {
	for _, value := range raw {
		if value != 0 {
			return false
		}
	}
	return true
}

func privateSerenaCacheTarPadding(reader io.Reader, size int64) error {
	var block [512]byte
	padding := (512 - size%512) % 512
	if _, err := io.ReadFull(reader, block[:padding]); err != nil {
		return err
	}
	if !privateSerenaCacheZero(block[:padding]) {
		return errors.New("SERENA_PRIVATE_CACHE_SEED_TAR_PADDING")
	}
	return nil
}

func privateSerenaCachePAX(raw []byte) (map[string]string, error) {
	result := make(map[string]string, 2)
	for len(raw) != 0 {
		space := bytes.IndexByte(raw, ' ')
		if space <= 0 || space > 6 {
			return nil, errors.New("SERENA_PRIVATE_CACHE_SEED_PAX_LENGTH")
		}
		n, err := strconv.Atoi(string(raw[:space]))
		if err != nil || strconv.Itoa(n) != string(raw[:space]) || n <= space+3 || n > len(raw) || raw[n-1] != '\n' {
			return nil, errors.New("SERENA_PRIVATE_CACHE_SEED_PAX_LENGTH")
		}
		key, value, found := strings.Cut(string(raw[space+1:n-1]), "=")
		if !found || (key != "path" && key != "mtime") || value == "" || result[key] != "" || strings.ContainsAny(value, "\x00\r\n") {
			return nil, errors.New("SERENA_PRIVATE_CACHE_SEED_PAX_FIELD")
		}
		result[key] = value
		raw = raw[n:]
	}
	if result["mtime"] == "" {
		return nil, errors.New("SERENA_PRIVATE_CACHE_SEED_PAX_MTIME_REQUIRED")
	}
	return result, nil
}

func privateSerenaCachePAXNanoseconds(value string) (int64, error) {
	whole, fraction, dot := strings.Cut(value, ".")
	seconds, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || seconds < 0 || strconv.FormatInt(seconds, 10) != whole || seconds > math.MaxInt64/1000000000 ||
		(dot && (fraction == "" || len(fraction) > 9)) {
		return 0, errors.New("SERENA_PRIVATE_CACHE_SEED_PAX_MTIME")
	}
	for _, digit := range fraction {
		if digit < '0' || digit > '9' {
			return 0, errors.New("SERENA_PRIVATE_CACHE_SEED_PAX_MTIME")
		}
	}
	part := int64(0)
	if dot {
		part, err = strconv.ParseInt(fraction+strings.Repeat("0", 9-len(fraction)), 10, 64)
		if err != nil {
			return 0, err
		}
	}
	if part > math.MaxInt64-seconds*1e9 {
		return 0, errors.New("SERENA_PRIVATE_CACHE_SEED_PAX_MTIME")
	}
	return seconds*1e9 + part, nil
}

func readPrivateSerenaCacheTar(ctx context.Context, source io.Reader, d privateSerenaCacheSeedDescriptor, inventory privateSerenaCacheSeedInventory, consume func(privateSerenaCacheSeedEntry, io.Reader) error) error {
	if ctx == nil || source == nil || consume == nil || d.Archive.Bytes < 1024 || d.Archive.Bytes > privateSerenaCacheSeedArchiveLimit || d.Archive.Bytes%512 != 0 {
		return errors.New("SERENA_PRIVATE_CACHE_SEED_TAR_INPUT")
	}
	r := &privateSerenaCacheSeedStream{ctx: ctx, source: bufio.NewReaderSize(source, 128<<10), digest: sha256.New(), expected: d.Archive.Bytes}
	var block [512]byte
	for _, entry := range inventory.Entries {
		if _, err := io.ReadFull(r, block[:]); err != nil {
			return err
		}
		paxHeader, err := privateSerenaCacheTarDecodeHeader(block[:])
		if err != nil || paxHeader.kind != 'x' || paxHeader.name != "././@PaxHeader" || paxHeader.size <= 0 || paxHeader.size > 16<<10 {
			return errors.Join(err, errors.New("SERENA_PRIVATE_CACHE_SEED_LOCAL_PAX_REQUIRED"))
		}
		paxRaw := make([]byte, int(paxHeader.size))
		if _, err = io.ReadFull(r, paxRaw); err != nil {
			return err
		}
		if err = privateSerenaCacheTarPadding(r, paxHeader.size); err != nil {
			return err
		}
		pax, err := privateSerenaCachePAX(paxRaw)
		if err != nil {
			return err
		}
		if _, err = io.ReadFull(r, block[:]); err != nil {
			return err
		}
		h, err := privateSerenaCacheTarDecodeHeader(block[:])
		if err != nil {
			return err
		}
		name := h.name
		if pax["path"] != "" {
			name = pax["path"]
		}
		expectedKind, expectedMode := byte('0'), int64(0600)
		if entry.Kind == "directory" {
			expectedKind, expectedMode = '5', 0700
			name = strings.TrimSuffix(name, "/")
		}
		mtime, err := privateSerenaCachePAXNanoseconds(pax["mtime"])
		expectedTime, timeErr := privateSerenaCacheSeedNanoseconds(entry.MtimeNS)
		if err != nil || timeErr != nil || name != entry.Path || h.kind != expectedKind || h.mode != expectedMode ||
			h.size != entry.Bytes || mtime != expectedTime || h.mtime != expectedTime/1e9 {
			return errors.Join(err, timeErr, errors.New("SERENA_PRIVATE_CACHE_SEED_TAR_ENTRY_DRIFT"))
		}
		limited := &io.LimitedReader{R: r, N: h.size}
		digest := sha256.New()
		if err = consume(entry, io.TeeReader(limited, digest)); err != nil {
			return err
		}
		if limited.N != 0 || (entry.Kind == "file" && hex.EncodeToString(digest.Sum(nil)) != entry.SHA256) {
			return errors.New("SERENA_PRIVATE_CACHE_SEED_FILE_CONTENT_DRIFT")
		}
		if err = privateSerenaCacheTarPadding(r, h.size); err != nil {
			return err
		}
	}
	// A logical tar EOF is insufficient. Receive its second zero block and
	// every physical trailing byte; concatenated archives are never accepted.
	for i := 0; i < 2; i++ {
		if _, err := io.ReadFull(r, block[:]); err != nil {
			return err
		}
		if !privateSerenaCacheZero(block[:]) {
			return errors.New("SERENA_PRIVATE_CACHE_SEED_TAR_EXTRA_MEMBER")
		}
	}
	for {
		n, err := r.Read(block[:])
		if !privateSerenaCacheZero(block[:n]) {
			return errors.New("SERENA_PRIVATE_CACHE_SEED_TAR_TRAILING")
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if r.read != d.Archive.Bytes || hex.EncodeToString(r.digest.Sum(nil)) != d.Archive.SHA256 {
		return errors.New("SERENA_PRIVATE_CACHE_SEED_ARCHIVE_DIGEST")
	}
	return ctx.Err()
}
