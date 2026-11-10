package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mcp-local-hub/internal/binaryadmission"
)

type entryCommentsRequest struct {
	existing, buildID, launcherSHA256, reverseProjection string
}

// Keep the build-tool wire exact. The binary-admission codec alone owns the
// VERSIONINFO record encoding; this decoder does not normalize metadata.
func decodeEntryCommentsRequest(raw []byte) (entryCommentsRequest, error) {
	var request entryCommentsRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return request, errors.New("entry comments request must be an object")
	}
	fields := map[string]*string{
		"existing_comments":  &request.existing,
		"build_id":           &request.buildID,
		"launcher_sha256":    &request.launcherSHA256,
		"reverse_projection": &request.reverseProjection,
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return request, err
		}
		key, ok := token.(string)
		target, known := fields[key]
		if !ok || !known || seen[key] {
			return request, errors.New("unknown or duplicate entry comments field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return request, err
		}
		if len(value) == 0 || value[0] != '"' {
			return request, errors.New("entry comments fields must be strings")
		}
		if err := json.Unmarshal(value, target); err != nil {
			return request, err
		}
		seen[key] = true
	}
	if _, err := decoder.Token(); err != nil {
		return request, err
	}
	if len(seen) != len(fields) {
		return request, errors.New("missing entry comments field")
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return request, errors.New("trailing entry comments request data")
	}
	return request, nil
}

func runEntryCommentsEncoder(path string, stdout, stderr io.Writer) int {
	if !filepath.IsAbs(path) {
		fmt.Fprintln(stderr, "entry comments request path must be absolute")
		return 2
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		fmt.Fprintln(stderr, "entry comments request is not an ordinary file")
		return 1
	}
	file, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	opened, readErr := file.Stat()
	if readErr == nil && !os.SameFile(info, opened) {
		readErr = errors.New("entry comments request identity changed")
	}
	var raw []byte
	if readErr == nil {
		raw, readErr = io.ReadAll(file)
	}
	if err := errors.Join(readErr, file.Close()); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	request, err := decodeEntryCommentsRequest(raw)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	binding, err := binaryadmission.NewWindowsCanonicalEntryBuildBindingV1(request.buildID, request.launcherSHA256, request.reverseProjection)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	comments, err := binaryadmission.EncodeWindowsCanonicalEntryCommentsV1(request.existing, binding)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(struct {
		Comments string `json:"comments"`
	}{comments}); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
