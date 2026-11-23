package binaryadmission

import (
	"bytes"
	"errors"
	"io"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

const RuntimeInventorySchemaV1 = "hub-runtime-inventory-v1"
const RuntimeReleaseSchemaV1 = "hub-runtime-release-v1"

type RuntimeArchiveMapping struct {
	Source      string `yaml:"source" json:"source"`
	Destination string `yaml:"destination" json:"destination"`
}
type RuntimeArchiveSource struct {
	ArchiveURL    string                  `yaml:"archive_url" json:"archive_url"`
	ArchiveSHA256 string                  `yaml:"archive_sha256" json:"archive_sha256"`
	ArchiveSize   int64                   `yaml:"archive_size" json:"archive_size"`
	ArchiveFormat string                  `yaml:"archive_format" json:"archive_format"`
	Layout        []RuntimeArchiveMapping `yaml:"layout" json:"layout"`
}
type RuntimeOriginalFile struct {
	SourcePath string `yaml:"source_path"`
	Path       string `yaml:"path"`
	SHA256     string `yaml:"sha256"`
}
type RuntimeOriginalArtifact struct {
	ArchiveURL    string `yaml:"archive_url"`
	ArchiveSHA256 string `yaml:"archive_sha256"`
}
type RuntimeReleaseDescriptorV1 struct {
	Schema               string `yaml:"schema"`
	Component            string `yaml:"component"`
	Version              string `yaml:"version"`
	Release              string `yaml:"release"`
	Target               string `yaml:"target"`
	RuntimeArchiveSource `yaml:",inline"`
	OriginalArtifacts    []RuntimeOriginalArtifact `yaml:"original_artifacts"`
	OriginalFiles        []RuntimeOriginalFile     `yaml:"original_files"`
	Executable           string                    `yaml:"executable"`
	Implementation       string                    `yaml:"implementation,omitempty"`
	Family               string                    `yaml:"family,omitempty"`
	ABI                  string                    `yaml:"abi,omitempty"`
	FreeThreaded         *bool                     `yaml:"free_threaded,omitempty"`
	NativeRequirements   []string                  `yaml:"native_requirements"`
	Licenses             []string                  `yaml:"licenses"`
}
type RuntimeObjectV1 struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Size   *int64 `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	// The portable Windows subset is readonly/hidden/system. Directory kind
	// is above; archive bits, ACLs, timestamps and file IDs are not transported.
	Attributes uint32 `json:"attributes"`
}
type RuntimeInventoryV1 struct {
	Schema  string            `json:"schema"`
	Target  string            `json:"target"`
	Objects []RuntimeObjectV1 `json:"objects"`
}
func ParseRuntimeReleaseDescriptor(raw []byte) (r RuntimeReleaseDescriptorV1, err error) {
	d := yaml.NewDecoder(bytes.NewReader(raw))
	d.KnownFields(true)
	if err = d.Decode(&r); err != nil {
		return r, err
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return r, errors.New("RUNTIME_DESCRIPTOR_MULTIPLE_DOCUMENTS")
	}
	if r.Schema != RuntimeReleaseSchemaV1 || (r.Component != "python" && r.Component != "uv") || r.Target != "win32-x64" || r.Version == "" || r.Release == "" || !RuntimeRelativePath(r.Executable) || len(r.NativeRequirements) == 0 || len(r.Licenses) == 0 {
		return r, errors.New("RUNTIME_DESCRIPTOR_INVALID")
	}
	if r.Component == "python" && (r.Implementation != "cpython" || r.Family != "3.13" || !strings.HasPrefix(r.Version, "3.13.") || r.ABI != "cp313" || r.FreeThreaded == nil || *r.FreeThreaded) {
		return r, errors.New("RUNTIME_PYTHON_ABI_INVALID")
	}
	if r.Component == "uv" && (r.Implementation != "" || r.Family != "" || r.ABI != "" || r.FreeThreaded != nil) {
		return r, errors.New("RUNTIME_UV_DESCRIPTOR_INVALID")
	}
	for _, a := range []RuntimeArchiveSource{r.RuntimeArchiveSource} {
		if !strings.HasPrefix(a.ArchiveURL, "https://github.com/astral-sh/") || !validLowerHex(a.ArchiveSHA256, 64, 64) || a.ArchiveSize <= 0 || (a.ArchiveFormat != "tar.gz" && a.ArchiveFormat != "zip") || len(a.Layout) == 0 {
			return r, errors.New("RUNTIME_ARCHIVE_INPUT_INVALID")
		}
		for _, m := range a.Layout {
			if !RuntimeRelativePath(m.Source) || m.Destination != "" && !RuntimeRelativePath(m.Destination) {
				return r, errors.New("RUNTIME_ARCHIVE_LAYOUT_INVALID")
			}
		}
	}
	if len(r.OriginalArtifacts) == 0 || len(r.OriginalFiles) == 0 {
		return r, errors.New("RUNTIME_ORIGINAL_NOTICES_MISSING")
	}
	for _, a := range r.OriginalArtifacts {
		if !strings.HasPrefix(a.ArchiveURL, "https://github.com/astral-sh/") || !validLowerHex(a.ArchiveSHA256, 64, 64) {
			return r, errors.New("RUNTIME_ORIGINAL_PROVENANCE_INVALID")
		}
	}
	for _, f := range r.OriginalFiles {
		if !RuntimeRelativePath(f.SourcePath) || !RuntimeRelativePath(f.Path) || !validLowerHex(f.SHA256, 64, 64) {
			return r, errors.New("RUNTIME_ORIGINAL_FILE_INVALID")
		}
	}
	seen := map[string]bool{}
	for _, name := range r.Licenses {
		if !RuntimeRelativePath(name) || seen[strings.ToLower(name)] {
			return r, errors.New("RUNTIME_LICENSE_PATH_INVALID")
		}
		seen[strings.ToLower(name)] = true
	}
	return r, nil
}
func ParseRuntimeInventory(raw []byte) (v RuntimeInventoryV1, err error) {
	if err = decodeCanonicalRuntime(raw, &v); err != nil {
		return v, err
	}
	if v.Schema != RuntimeInventorySchemaV1 || v.Target != "win32-x64" || len(v.Objects) == 0 {
		return v, errors.New("RUNTIME_INVENTORY_INVALID")
	}
	seen := map[string]string{}
	prior := ""
	for _, o := range v.Objects {
		if !RuntimeRelativePath(o.Path) || prior != "" && o.Path <= prior || seen[strings.ToLower(o.Path)] != "" || o.Attributes & ^uint32(7) != 0 {
			return v, errors.New("RUNTIME_INVENTORY_PATH_INVALID")
		}
		if o.Kind == "file" {
			if o.Size == nil || *o.Size < 0 || !validLowerHex(o.SHA256, 64, 64) {
				return v, errors.New("RUNTIME_INVENTORY_FILE_INVALID")
			}
		} else if o.Kind != "directory" || o.Size != nil || o.SHA256 != "" {
			return v, errors.New("RUNTIME_INVENTORY_OBJECT_UNSUPPORTED")
		}
		if parent := path.Dir(o.Path); parent != "." && seen[strings.ToLower(parent)] != "directory" {
			return v, errors.New("RUNTIME_INVENTORY_PARENT_MISSING")
		}
		seen[strings.ToLower(o.Path)] = o.Kind
		prior = o.Path
	}
	return v, nil
}
