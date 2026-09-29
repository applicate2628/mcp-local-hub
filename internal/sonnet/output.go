package sonnet

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

const (
	outputFileLimit = 1024
	outputByteLimit = int64(2 << 30)
)

type OutputFile struct {
	Path   string
	Size   int64
	SHA256 string
	State  string
}
type SonnetOutputManifestV1 struct {
	Files      []OutputFile
	Reportable bool
	FailureID  string
}

// SonnetOutputResultV1 is the sole public projection of a complete private
// output inventory. Baseline inputs remain counted but never appear in Outputs.
type SonnetOutputResultV1 struct {
	SchemaVersion          string                `json:"schema_version"`
	InternalManifestSHA256 string                `json:"internal_manifest_sha256"`
	Counts                 SonnetOutputCounts    `json:"counts"`
	Outputs                []SonnetOutputFile    `json:"outputs"`
	Anomalies              []SonnetOutputAnomaly `json:"anomalies"`
}

type SonnetOutputCounts struct {
	Unchanged  int `json:"unchanged"`
	Created    int `json:"created"`
	Modified   int `json:"modified"`
	Removed    int `json:"removed"`
	Unreadable int `json:"unreadable"`
}

type SonnetOutputFile struct {
	Path        string `json:"path"`
	Disposition string `json:"disposition"`
	SizeBytes   int64  `json:"size_bytes"`
	SHA256      string `json:"sha256"`
}

type SonnetOutputAnomaly struct {
	Path        string `json:"path,omitempty"`
	Disposition string `json:"disposition"`
}

func projectOutputManifest(in SonnetOutputManifestV1) (SonnetOutputResultV1, error) {
	if !in.Reportable || in.FailureID != "" {
		return SonnetOutputResultV1{}, errors.New("SONNET_OUTPUT_UNATTRIBUTABLE")
	}
	body, err := json.Marshal(in)
	if err != nil {
		return SonnetOutputResultV1{}, err
	}
	sum := sha256.Sum256(body)
	out := SonnetOutputResultV1{SchemaVersion: "sonnet-output-result-v1", InternalManifestSHA256: fmt.Sprintf("%x", sum[:]), Outputs: []SonnetOutputFile{}, Anomalies: []SonnetOutputAnomaly{}}
	for _, file := range in.Files {
		switch file.State {
		case "unchanged":
			out.Counts.Unchanged++
		case "created":
			out.Counts.Created++
			out.Outputs = append(out.Outputs, SonnetOutputFile{Path: file.Path, Disposition: file.State, SizeBytes: file.Size, SHA256: file.SHA256})
		case "modified":
			out.Counts.Modified++
			out.Outputs = append(out.Outputs, SonnetOutputFile{Path: file.Path, Disposition: file.State, SizeBytes: file.Size, SHA256: file.SHA256})
		case "removed":
			out.Counts.Removed++
			out.Anomalies = append(out.Anomalies, SonnetOutputAnomaly{Path: file.Path, Disposition: file.State})
		default:
			out.Counts.Unreadable++
			out.Anomalies = append(out.Anomalies, SonnetOutputAnomaly{Path: file.Path, Disposition: file.State})
		}
	}
	return out, nil
}

func snapshotOutput(root string) (map[string]OutputFile, error) {
	m := map[string]OutputFile{}
	var total int64
	e := filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if p == root {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("SONNET_OUTPUT_UNATTRIBUTABLE")
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("SONNET_OUTPUT_UNATTRIBUTABLE")
		}
		i, e := d.Info()
		if e != nil {
			return e
		}
		total += i.Size()
		if len(m) >= outputFileLimit || total > outputByteLimit {
			return fmt.Errorf("SONNET_OUTPUT_UNATTRIBUTABLE")
		}
		f, e := os.Open(p)
		if e != nil {
			return e
		}
		h := sha256.New()
		_, e = io.Copy(h, f)
		f.Close()
		if e != nil {
			return e
		}
		r, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		m[filepath.ToSlash(r)] = OutputFile{Path: filepath.ToSlash(r), Size: i.Size(), SHA256: fmt.Sprintf("%x", h.Sum(nil))}
		return nil
	})
	return m, e
}
func compareOutput(root string, b map[string]OutputFile) (SonnetOutputManifestV1, error) {
	a, e := snapshotOutput(root)
	if e != nil {
		return SonnetOutputManifestV1{FailureID: "SONNET_OUTPUT_UNATTRIBUTABLE"}, e
	}
	o := SonnetOutputManifestV1{Reportable: true}
	for p, v := range b {
		x, ok := a[p]
		if !ok {
			v.State = "removed"
			o.Files = append(o.Files, v)
			o.Reportable = false
			o.FailureID = "SONNET_OUTPUT_UNATTRIBUTABLE"
		} else {
			if x.SHA256 == v.SHA256 {
				x.State = "unchanged"
			} else {
				x.State = "modified"
			}
			o.Files = append(o.Files, x)
			delete(a, p)
		}
	}
	for _, x := range a {
		x.State = "created"
		o.Files = append(o.Files, x)
	}
	sort.Slice(o.Files, func(i, j int) bool { return o.Files[i].Path < o.Files[j].Path })
	return o, nil
}
