package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

const privateSerenaCacheSeedSchema = "private-serena-uv-cache-seed-v1"
const privateSerenaCacheSeedInventorySchema = "private-serena-uv-cache-seed-inventory-v1"
const privateSerenaCacheSeedDescriptorLimit int64 = 64 << 10
const privateSerenaCacheSeedManifestLimit int64 = 16 << 20
const privateSerenaCacheSeedBytesLimit int64 = 1 << 30
const privateSerenaCacheSeedArchiveLimit int64 = privateSerenaCacheSeedBytesLimit + (64 << 20)
const privateSerenaCacheSeedEntriesLimit = 32768

// PrivateSerenaCacheSeed is passive input to the already admitted private test
// publisher. It cannot grant a publisher, generation, Ready or deletion right.
// The descriptor names exactly two sibling files; no provenance path is opened.
type PrivateSerenaCacheSeed struct {
	DescriptorPath   string
	DescriptorSHA256 string
}

type privateSerenaCacheSeedDescriptor struct {
	Schema string `json:"schema"`
	UV     struct {
		Version string `json:"version"`
		SHA256  string `json:"sha256"`
	} `json:"uv"`
	Python struct {
		SHA256 string               `json:"sha256"`
		ABI    serenaInterpreterABI `json:"abi"`
	} `json:"python"`
	Source struct {
		Requirement string `json:"requirement"`
		Commit      string `json:"commit"`
		Package     string `json:"package"`
	} `json:"source"`
	BuildConstraint struct {
		ContentSHA256 string `json:"content_sha256"`
		Bytes         int64  `json:"bytes"`
	} `json:"build_constraint"`
	NetworkPolicy string `json:"network_policy"`
	Provenance    struct {
		ProducerRootSHA256    string `json:"producer_root_sha256"`
		ProducerReceiptSHA256 string `json:"producer_receipt_sha256"`
		ConsumerRootSHA256    string `json:"consumer_root_sha256"`
		ConsumerReceiptSHA256 string `json:"consumer_receipt_sha256"`
		InventorySHA256       string `json:"inventory_sha256"`
		RelocationSHA256      string `json:"relocation_sha256"`
		ABIEvidenceSHA256     string `json:"abi_evidence_sha256"`
	} `json:"provenance"`
	Archive struct {
		Path   string `json:"path"`
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
		Format string `json:"format"`
	} `json:"archive"`
	Manifest struct {
		Path         string `json:"path"`
		Bytes        int64  `json:"bytes"`
		SHA256       string `json:"sha256"`
		Schema       string `json:"schema"`
		EntriesCount int    `json:"entries_count"`
		TotalBytes   int64  `json:"total_bytes"`
		RootMtimeNS  string `json:"root_mtime_ns"`
	} `json:"manifest"`
}

type privateSerenaCacheSeedEntry struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Bytes   int64  `json:"bytes"`
	SHA256  string `json:"sha256"`
	MtimeNS string `json:"mtime_ns"`
}

type privateSerenaCacheSeedInventory struct {
	Schema       string                        `json:"schema"`
	RootMtimeNS  string                        `json:"root_mtime_ns"`
	EntriesCount int                           `json:"entries_count"`
	TotalBytes   int64                         `json:"total_bytes"`
	Entries      []privateSerenaCacheSeedEntry `json:"entries"`
}

// The original metadata readers remain retained even when binding fails.
// This value is issued once by the same private publisher, never from JSON.
type privateSerenaCacheSeed struct {
	owner                      *privateSerenaPublisherOwner
	config                     PrivateSerenaCacheSeed
	descriptor                 privateSerenaCacheSeedDescriptor
	inventory                  privateSerenaCacheSeedInventory
	readers                    []*StateFileReadObservation
	parentIdentitySHA256       string
	request                    PreparedSerenaRequest
	bound, materializationUsed bool
}

func privateSerenaCacheSeedObject(raw []byte, target any, fields ...string) (map[string]json.RawMessage, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("SERENA_PRIVATE_CACHE_SEED_JSON_ENCODING")
	}
	if err := rejectSerenaJSONDuplicates(raw); err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || len(object) != len(fields) {
		return nil, errors.Join(err, errors.New("SERENA_PRIVATE_CACHE_SEED_JSON_FIELDS"))
	}
	for _, field := range fields {
		v, ok := object[field]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return nil, errors.New("SERENA_PRIVATE_CACHE_SEED_JSON_FIELDS")
		}
	}
	if target != nil {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(target); err != nil {
			return nil, err
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return nil, errors.Join(err, errors.New("SERENA_PRIVATE_CACHE_SEED_JSON_TRAILING"))
		}
	}
	return object, nil
}

func decodePrivateSerenaCacheSeedDescriptor(raw []byte) (d privateSerenaCacheSeedDescriptor, err error) {
	if int64(len(raw)) > privateSerenaCacheSeedDescriptorLimit {
		return d, errors.New("SERENA_PRIVATE_CACHE_SEED_DESCRIPTOR_LIMIT")
	}
	object, err := privateSerenaCacheSeedObject(raw, &d, "schema", "uv", "python", "source", "build_constraint", "network_policy", "provenance", "archive", "manifest")
	if err != nil {
		return d, err
	}
	for name, fields := range map[string][]string{
		"uv": {"version", "sha256"}, "python": {"sha256", "abi"},
		"source": {"requirement", "commit", "package"}, "build_constraint": {"content_sha256", "bytes"},
		"provenance": {"producer_root_sha256", "producer_receipt_sha256", "consumer_root_sha256", "consumer_receipt_sha256", "inventory_sha256", "relocation_sha256", "abi_evidence_sha256"},
		"archive":    {"path", "bytes", "sha256", "format"},
		"manifest":   {"path", "bytes", "sha256", "schema", "entries_count", "total_bytes", "root_mtime_ns"},
	} {
		if _, err = privateSerenaCacheSeedObject(object[name], nil, fields...); err != nil {
			return d, err
		}
	}
	var python map[string]json.RawMessage
	if err = json.Unmarshal(object["python"], &python); err != nil {
		return d, err
	}
	if d.Python.ABI, err = decodeSerenaInterpreterABI(python["abi"]); err != nil {
		return d, err
	}
	if d.Schema != privateSerenaCacheSeedSchema || d.NetworkPolicy != "offline" ||
		d.Archive.Path != "cache.tar" || d.Archive.Format != "pax-tar-uncompressed" ||
		d.Archive.Bytes < 1024 || d.Archive.Bytes > privateSerenaCacheSeedArchiveLimit || d.Archive.Bytes%512 != 0 ||
		d.Manifest.Path != "cache-manifest.json" || d.Manifest.Schema != privateSerenaCacheSeedInventorySchema ||
		d.Manifest.Bytes <= 0 || d.Manifest.Bytes > privateSerenaCacheSeedManifestLimit ||
		d.Manifest.EntriesCount <= 0 || d.Manifest.EntriesCount > privateSerenaCacheSeedEntriesLimit ||
		d.Manifest.TotalBytes < 0 || d.Manifest.TotalBytes > privateSerenaCacheSeedBytesLimit ||
		d.Source.Package != "serena-agent" || len(d.Source.Commit) != 40 ||
		!preparedSerenaDigest(d.Source.Commit+strings.Repeat("0", 24)) ||
		!strings.HasSuffix(d.Source.Requirement, "@"+d.Source.Commit) || d.UV.Version == "" ||
		d.BuildConstraint.Bytes != int64(len(preparedSerenaBuildConstraintContent)) ||
		d.BuildConstraint.ContentSHA256 != preparedSerenaHash([]byte(preparedSerenaBuildConstraintContent)) {
		return d, errors.New("SERENA_PRIVATE_CACHE_SEED_DESCRIPTOR_INVALID")
	}
	for _, digest := range []string{d.UV.SHA256, d.Python.SHA256, d.Archive.SHA256, d.Manifest.SHA256,
		d.Provenance.ProducerRootSHA256, d.Provenance.ProducerReceiptSHA256, d.Provenance.ConsumerRootSHA256,
		d.Provenance.ConsumerReceiptSHA256, d.Provenance.InventorySHA256, d.Provenance.RelocationSHA256, d.Provenance.ABIEvidenceSHA256} {
		if !preparedSerenaDigest(digest) {
			return d, errors.New("SERENA_PRIVATE_CACHE_SEED_DIGEST_INVALID")
		}
	}
	_, err = privateSerenaCacheSeedNanoseconds(d.Manifest.RootMtimeNS)
	return d, err
}

func privateSerenaCacheSeedNanoseconds(value string) (int64, error) {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != value || n%100 != 0 {
		return 0, errors.Join(err, errors.New("SERENA_PRIVATE_CACHE_SEED_MTIME_INVALID"))
	}
	return n, nil
}

func privateSerenaCacheSeedPath(value string) error {
	if !utf8.ValidString(value) || value == "" || len(value) > 4096 || path.Clean(value) != value || strings.HasPrefix(value, "/") {
		return errors.New("SERENA_PRIVATE_CACHE_SEED_PATH_INVALID")
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." || len(utf16.Encode([]rune(part))) > 255 ||
			strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") || strings.ContainsAny(part, "\\:<>\"|?*") {
			return errors.New("SERENA_PRIVATE_CACHE_SEED_PATH_INVALID")
		}
		for _, r := range part {
			if unicode.IsControl(r) {
				return errors.New("SERENA_PRIVATE_CACHE_SEED_PATH_INVALID")
			}
		}
		stem, _, _ := strings.Cut(strings.ToUpper(part), ".")
		if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" ||
			(len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9') {
			return errors.New("SERENA_PRIVATE_CACHE_SEED_PATH_INVALID")
		}
	}
	return nil
}

func decodePrivateSerenaCacheSeedInventory(raw []byte, d privateSerenaCacheSeedDescriptor, limits SerenaTreeLimits) (v privateSerenaCacheSeedInventory, err error) {
	if !limits.valid() || int64(len(raw)) != d.Manifest.Bytes || preparedSerenaHash(raw) != d.Manifest.SHA256 {
		return v, errors.New("SERENA_PRIVATE_CACHE_SEED_MANIFEST_DRIFT")
	}
	object, err := privateSerenaCacheSeedObject(raw, &v, "schema", "root_mtime_ns", "entries_count", "total_bytes", "entries")
	if err != nil {
		return v, err
	}
	if v.Schema != d.Manifest.Schema || v.RootMtimeNS != d.Manifest.RootMtimeNS || v.EntriesCount != d.Manifest.EntriesCount ||
		v.TotalBytes != d.Manifest.TotalBytes || len(v.Entries) != v.EntriesCount {
		return v, errors.New("SERENA_PRIVATE_CACHE_SEED_MANIFEST_DRIFT")
	}
	var entryRaw []json.RawMessage
	if err = json.Unmarshal(object["entries"], &entryRaw); err != nil {
		return v, err
	}
	directories := map[string]bool{".": true}
	seen := make(map[string]bool, len(v.Entries))
	lastChild := make(map[string]string)
	var stack []string
	var total, paths int64
	files := 0
	for i, entry := range v.Entries {
		if _, err = privateSerenaCacheSeedObject(entryRaw[i], nil, "path", "kind", "bytes", "sha256", "mtime_ns"); err != nil {
			return v, err
		}
		if err = privateSerenaCacheSeedPath(entry.Path); err != nil {
			return v, err
		}
		if _, err = privateSerenaCacheSeedNanoseconds(entry.MtimeNS); err != nil {
			return v, err
		}
		fold := strings.ToUpper(entry.Path)
		parent := path.Dir(entry.Path)
		if seen[fold] || !directories[parent] || path.Base(entry.Path) <= lastChild[parent] {
			return v, errors.New("SERENA_PRIVATE_CACHE_SEED_ORDER_OR_COLLISION")
		}
		for len(stack) > 0 && stack[len(stack)-1] != parent {
			delete(directories, stack[len(stack)-1])
			stack = stack[:len(stack)-1]
		}
		if !directories[parent] || (parent != "." && (len(stack) == 0 || stack[len(stack)-1] != parent)) {
			return v, errors.New("SERENA_PRIVATE_CACHE_SEED_DEPTH_FIRST_REQUIRED")
		}
		seen[fold] = true
		lastChild[parent] = path.Base(entry.Path)
		depth := strings.Count(entry.Path, "/") + 2 // cache is an attempt child.
		if depth > 64 || depth > limits.Depth || depth+6 > limits.OpenHandles {
			return v, errors.New("SERENA_PRIVATE_CACHE_SEED_DEPTH_HANDLE_LIMIT")
		}
		paths += int64(len(utf16.Encode([]rune("cache/"+entry.Path))) * 2)
		switch entry.Kind {
		case "directory":
			if entry.Bytes != 0 || entry.SHA256 != "" {
				return v, errors.New("SERENA_PRIVATE_CACHE_SEED_DIRECTORY_INVALID")
			}
			directories[entry.Path] = true
			stack = append(stack, entry.Path)
		case "file":
			if entry.Bytes < 0 || entry.Bytes > privateSerenaCacheSeedBytesLimit-total || !preparedSerenaDigest(entry.SHA256) {
				return v, errors.New("SERENA_PRIVATE_CACHE_SEED_FILE_INVALID")
			}
			total += entry.Bytes
			files++
		default:
			return v, errors.New("SERENA_PRIVATE_CACHE_SEED_KIND_INVALID")
		}
	}
	// Reserve at least half the existing tree capacity for the real UV attempt.
	// Entries include ordinary file streams, not merely the manifest objects.
	if total != v.TotalBytes || total > limits.LogicalBytes/2 || len(v.Entries)+files+4 > limits.Entries/2 ||
		paths > limits.EncodedPathBytes/2 || int64(len(raw)) > limits.RecordBytes/2 {
		return v, errors.New("SERENA_PRIVATE_CACHE_SEED_TREE_LIMIT")
	}
	return v, nil
}

func (o *privateSerenaPublisherOwner) bindCacheSeed(ctx context.Context, inputs SerenaPreparationInputs) (SerenaPreparationInputs, error) {
	if o == nil {
		return inputs, errors.New("SERENA_PRIVATE_CACHE_SEED_OWNER_REQUIRED")
	}
	if o.cacheSeedConfig == nil {
		return inputs, nil
	}
	if err := o.validateOrigin(ctx); err != nil {
		return inputs, err
	}
	o.activityMu.Lock()
	if o.activity != privateSerenaPreparationActivity || o.cacheSeed != nil {
		o.activityMu.Unlock()
		return inputs, errors.New("SERENA_PRIVATE_CACHE_SEED_ALREADY_BOUND")
	}
	seed := &privateSerenaCacheSeed{owner: o, config: *o.cacheSeedConfig}
	o.cacheSeed = seed // Retain partial reads and consume the one bind before IO.
	o.activityMu.Unlock()
	r := inputs.Request
	if _, err := r.preparationContext.qualification(r); err != nil {
		return inputs, err
	}
	if r.Namespace != o.rootPath || r.WorkDeadline.IsZero() || inputs.cacheSeed != nil {
		return inputs, errors.New("SERENA_PRIVATE_CACHE_SEED_REQUEST_DRIFT")
	}
	work, cancel := context.WithDeadline(ctx, r.WorkDeadline)
	defer cancel()
	read := func(file string, maximum int64, expected string) ([]byte, error) {
		raw, observation, err := ReadStateFileInodeAnchoredObserved(work, file, maximum)
		seed.readers = append(seed.readers, observation)
		if err != nil {
			return nil, err
		}
		if err = observation.Validate(work, file, maximum, raw, expected); err != nil {
			return nil, err
		}
		view := observation.Snapshot()
		if !view.ParentIdentityDefined || view.ParentIdentitySHA256 == nil {
			return nil, errors.New("SERENA_PRIVATE_CACHE_SEED_PARENT_UNQUALIFIED")
		}
		if seed.parentIdentitySHA256 == "" {
			seed.parentIdentitySHA256 = *view.ParentIdentitySHA256
		} else if seed.parentIdentitySHA256 != *view.ParentIdentitySHA256 {
			return nil, errors.New("SERENA_PRIVATE_CACHE_SEED_PARENT_DRIFT")
		}
		return raw, nil
	}
	raw, err := read(seed.config.DescriptorPath, privateSerenaCacheSeedDescriptorLimit, seed.config.DescriptorSHA256)
	if err != nil {
		return inputs, err
	}
	seed.descriptor, err = decodePrivateSerenaCacheSeedDescriptor(raw)
	if err != nil {
		return inputs, err
	}
	manifestPath := filepath.Join(filepath.Dir(seed.config.DescriptorPath), seed.descriptor.Manifest.Path)
	raw, err = read(manifestPath, privateSerenaCacheSeedManifestLimit, seed.descriptor.Manifest.SHA256)
	if err != nil {
		return inputs, err
	}
	seed.inventory, err = decodePrivateSerenaCacheSeedInventory(raw, seed.descriptor, r.OwnedTreeLimits)
	if err != nil {
		return inputs, err
	}
	if err = seed.matchesInputs(inputs); err != nil {
		return inputs, err
	}
	bound, err := bindPrivateSerenaCacheSeedRequest(r, seed.config.DescriptorSHA256)
	if err != nil {
		return inputs, err
	}
	if err = work.Err(); err != nil {
		return inputs, err
	}
	o.activityMu.Lock()
	seed.request, seed.bound = bound, true
	o.activityMu.Unlock()
	inputs.Request, inputs.cacheSeed = bound, seed
	return inputs, nil
}

func bindPrivateSerenaCacheSeedRequest(r PreparedSerenaRequest, descriptorSHA string) (PreparedSerenaRequest, error) {
	if !preparedSerenaDigest(descriptorSHA) {
		return r, errors.New("SERENA_PRIVATE_CACHE_SEED_DIGEST_INVALID")
	}
	if _, err := r.preparationContext.qualification(r); err != nil {
		return r, err
	}
	raw, err := json.Marshal(struct {
		Domain, ConsumerBuildSettingsSHA256, DescriptorSHA256 string
	}{"private-serena-cache-seed-build-settings-v1", r.BuildSettingsSHA256, descriptorSHA})
	if err != nil {
		return r, err
	}
	// Copy the original sealed context. Never mutate the still-retained old
	// request or replace its actual UV/ABI probes, source, policy or deadlines.
	contextCopy := *r.preparationContext
	r.BuildSettingsSHA256 = preparedSerenaHash(raw)
	r.preparationContext = nil
	contextCopy.request = r
	r.preparationContext = &contextCopy
	if _, err = r.preparationContext.qualification(r); err != nil {
		return r, err
	}
	_, err = r.Identity()
	return r, err
}

func (s *privateSerenaCacheSeed) matchesInputs(inputs SerenaPreparationInputs) error {
	d, r := s.descriptor, inputs.Request
	if r.preparationContext == nil || inputs.uvVersionProbe == nil || inputs.interpreterProbe == nil ||
		inputs.uvVersionProbe != r.preparationContext.uvProbe || inputs.interpreterProbe != r.preparationContext.abiProbe || inputs.source != r.preparationContext.source {
		return errors.New("SERENA_PRIVATE_CACHE_SEED_ORIGINAL_PROBES_REQUIRED")
	}
	if d.UV.SHA256 != r.UV.SHA256 || d.Python.SHA256 != r.Interpreter.SHA256 ||
		d.Source.Requirement != r.SourceRequirement || d.Source.Commit != r.SourceCommit || d.Source.Package != r.Package ||
		!inputs.execution.buildConstraint || inputs.execution.networkPolicy == nil || !inputs.execution.networkPolicy.offline ||
		inputs.execution.networkPolicy.decisionSource != "declared" {
		return errors.New("SERENA_PRIVATE_CACHE_SEED_TOOL_SOURCE_DRIFT")
	}
	version := strings.Fields(string(inputs.uvVersionProbe.Stdout))
	if len(version) < 2 || version[0] != "uv" || version[1] != d.UV.Version {
		return errors.New("SERENA_PRIVATE_CACHE_SEED_UV_VERSION_DRIFT")
	}
	actual, err := decodeSerenaInterpreterABI(inputs.interpreterProbe.Stdout)
	if err != nil {
		return err
	}
	left, _ := json.Marshal(actual)
	right, _ := json.Marshal(d.Python.ABI)
	if !bytes.Equal(left, right) {
		return errors.New("SERENA_PRIVATE_CACHE_SEED_ABI_DRIFT")
	}
	// v1 represents the accepted default-index, fixed-constraint installation.
	// A seed for a different declared index/build route needs its own contract.
	for key := range preparedSerenaEnvironmentMap(inputs.Environment) {
		if strings.HasPrefix(key, "UV_INDEX") || strings.HasPrefix(key, "UV_DEFAULT_INDEX") || strings.HasPrefix(key, "UV_EXTRA_INDEX") || strings.HasPrefix(key, "UV_CONFIG_") || strings.HasPrefix(key, "UV_CONSTRAINT") || strings.HasPrefix(key, "UV_OVERRIDE") {
			return errors.New("SERENA_PRIVATE_CACHE_SEED_BUILD_SETTINGS_DRIFT")
		}
	}
	want := []string{"tool", "install", "--python", r.Interpreter.Path, "--no-python-downloads", "--no-config", "--from", r.SourceRequirement, "--offline", r.Package}
	if len(inputs.InstallArgs) != len(want) {
		return errors.New("SERENA_PRIVATE_CACHE_SEED_BUILD_SETTINGS_DRIFT")
	}
	for i := range want {
		if inputs.InstallArgs[i] != want[i] {
			return errors.New("SERENA_PRIVATE_CACHE_SEED_BUILD_SETTINGS_DRIFT")
		}
	}
	return inputs.execution.networkPolicy.validateExecution(inputs.Environment, inputs.InstallArgs)
}

func validatePrivateSerenaCacheSeedBinding(inputs SerenaPreparationInputs, owner *privateSerenaPublisherOwner) error {
	if owner != nil {
		owner.activityMu.Lock()
		defer owner.activityMu.Unlock()
	}
	seed := inputs.cacheSeed
	if seed == nil && (owner == nil || owner.cacheSeedConfig == nil) {
		return nil
	}
	if seed == nil || owner == nil || seed.owner != owner || owner.cacheSeedConfig == nil ||
		owner.cacheSeed != seed || !seed.bound || seed.config != *owner.cacheSeedConfig || seed.request != inputs.Request {
		return errors.New("SERENA_PRIVATE_CACHE_SEED_OWNER_OR_REQUEST_DRIFT")
	}
	return nil
}
