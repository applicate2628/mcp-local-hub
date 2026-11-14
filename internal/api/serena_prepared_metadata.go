package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/pelletier/go-toml/v2"
)

// The qualified environment interpreter runs only this stdlib collector. It
// does not import Serena or its dependencies. Protocol qualification is a
// separate actual MCP initialize/initialized/tools-list operation.
const preparedSerenaMetadataCollector = `import hashlib, importlib.metadata as m, json, os, platform, sys, sysconfig
def unique_object(pairs):
    result={}
    for key,value in pairs:
        if key in result: raise RuntimeError('DIRECT_URL_JSON duplicate field')
        result[key]=value
    return result
def reject_constant(value):
    raise RuntimeError('DIRECT_URL_JSON non-finite constant')
rows=[]
for d in m.distributions():
    files=[]
    for f in sorted(d.files or [], key=str):
        p=os.path.abspath(d.locate_file(f))
        if not os.path.isfile(p): raise RuntimeError('missing distribution file')
        with open(p,'rb') as h: digest=hashlib.file_digest(h,'sha256').hexdigest()
        files.append({'path':p,'sha256':digest})
    direct=d.read_text('direct_url.json')
    present=direct is not None
    provenance=json.loads(direct,object_pairs_hook=unique_object,parse_constant=reject_constant) if present else None
    if present and not isinstance(provenance,dict): raise RuntimeError('DIRECT_URL_JSON object required')
    rows.append({'name':d.metadata['Name'],'version':d.version,'direct_url_present':present,'direct_url':provenance,'files':files,'entrypoints':[{'name':e.name,'group':e.group,'value':e.value} for e in d.entry_points]})
rows.sort(key=lambda r:r['name'].lower())
print(json.dumps({'version':2,'executable':os.path.abspath(sys.executable),'base_executable':os.path.abspath(sys._base_executable),'prefix':os.path.abspath(sys.prefix),'python_version':platform.python_version(),'platform':sys.platform,'machine':platform.machine(),'soabi':sysconfig.get_config_var('SOABI') or '', 'distributions':rows},sort_keys=True,separators=(',',':')))
`

type preparedSerenaMetadataFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type preparedSerenaMetadataEntrypoint struct {
	Name  string `json:"name"`
	Group string `json:"group"`
	Value string `json:"value"`
}
type preparedSerenaDistribution struct {
	Name             string                             `json:"name"`
	Version          string                             `json:"version"`
	DirectURL        *preparedSerenaDirectURL           `json:"direct_url"`
	DirectURLPresent *bool                              `json:"direct_url_present"`
	Files            []preparedSerenaMetadataFile       `json:"files"`
	Entrypoints      []preparedSerenaMetadataEntrypoint `json:"entrypoints"`
}
type preparedSerenaMetadata struct {
	Version        int                          `json:"version"`
	Executable     string                       `json:"executable"`
	BaseExecutable string                       `json:"base_executable"`
	Prefix         string                       `json:"prefix"`
	PythonVersion  string                       `json:"python_version"`
	Platform       string                       `json:"platform"`
	Machine        string                       `json:"machine"`
	SOABI          string                       `json:"soabi"`
	Distributions  []preparedSerenaDistribution `json:"distributions"`
}
type preparedSerenaUVEntrypoint struct {
	Name        string `toml:"name"`
	InstallPath string `toml:"install-path"`
	From        string `toml:"from"`
}
type preparedSerenaUVReceipt struct {
	Tool struct {
		Requirements                []map[string]any             `toml:"requirements"`
		Constraints                 []map[string]any             `toml:"constraints"`
		Overrides                   []map[string]any             `toml:"overrides"`
		BuildConstraintDependencies []map[string]any             `toml:"build-constraint-dependencies"`
		Entrypoints                 []preparedSerenaUVEntrypoint `toml:"entrypoints"`
		Python                      string                       `toml:"python"`
		Options                     map[string]any               `toml:"options"`
	} `toml:"tool"`
}

// ToolWire carries the exact Git revision in a rev query, a fragment, or
// agreeing carriers. Preserve the original receipt rather than remarshal it.
func parsePreparedSerenaUVReceipt(raw []byte, request PreparedSerenaRequest, generation string) (preparedSerenaUVReceipt, string, error) {
	var receipt preparedSerenaUVReceipt
	if len(raw) == 0 || len(raw) > 1<<20 {
		return receipt, "", errors.New("SERENA_PREPARED_UV_RECEIPT_SIZE")
	}
	if err := toml.Unmarshal(raw, &receipt); err != nil {
		return receipt, "", err
	}
	if len(receipt.Tool.Requirements) != 1 {
		return receipt, "", errors.New("SERENA_PREPARED_UV_REQUIREMENT_CARDINALITY")
	}
	requirement := receipt.Tool.Requirements[0]
	name, nameOK := requirement["name"].(string)
	git, gitOK := requirement["git"].(string)
	u, err := url.Parse(git)
	expected, expectedErr := url.Parse(strings.TrimSuffix(strings.TrimPrefix(request.SourceRequirement, "git+"), "@"+request.SourceCommit))
	if !nameOK || name != request.Package || !gitOK || err != nil || expectedErr != nil || u.User != nil || u.Scheme != "https" || u.Opaque != "" || u.Host != expected.Host || u.Path != expected.Path || u.RawPath != "" || expected.Scheme != "https" || expected.User != nil || expected.RawQuery != "" || expected.Fragment != "" {
		return receipt, "", errors.New("SERENA_PREPARED_UV_SOURCE_MISMATCH")
	}
	query, queryErr := url.ParseQuery(u.RawQuery)
	if queryErr != nil || (u.ForceQuery && u.RawQuery == "") {
		return receipt, "", errors.New("SERENA_PREPARED_UV_SOURCE_POLICY_MISMATCH")
	}
	for key, values := range query {
		if key != "rev" || len(values) != 1 || values[0] != request.SourceCommit {
			return receipt, "", errors.New("SERENA_PREPARED_UV_SOURCE_POLICY_MISMATCH")
		}
	}
	// A syntactically present empty fragment is not a missing carrier.
	fragmentPresent := strings.Contains(git, "#")
	if (fragmentPresent && u.Fragment != request.SourceCommit) || (!fragmentPresent && len(query["rev"]) == 0) {
		return receipt, "", errors.New("SERENA_PREPARED_UV_SOURCE_MISMATCH")
	}
	if len(requirement) != 2 {
		return receipt, "", errors.New("SERENA_PREPARED_UV_SOURCE_POLICY_MISMATCH")
	}
	if len(receipt.Tool.Entrypoints) == 0 {
		return receipt, "", errors.New("SERENA_PREPARED_UV_ENTRYPOINT_MISMATCH")
	}
	names, paths := map[string]bool{}, map[string]bool{}
	selectedPath := ""
	for _, entry := range receipt.Tool.Entrypoints {
		nameKey := preparedMetadataIdentityKey(entry.Name)
		path := filepath.FromSlash(entry.InstallPath)
		pathKey := preparedMetadataIdentityKey(path)
		if entry.Name == "" || strings.ContainsAny(entry.Name, `\/:`+"\x00") || names[nameKey] || paths[pathKey] || (entry.From != "" && entry.From != request.Package) {
			return receipt, "", errors.New("SERENA_PREPARED_UV_ENTRYPOINT_MISMATCH")
		}
		names[nameKey], paths[pathKey] = true, true
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || !preparedSerenaPathInside(generation, path) {
			return receipt, "", errors.New("SERENA_PREPARED_UV_ENTRYPOINT_OUTSIDE_GENERATION")
		}
		if path != filepath.Join(generation, "bin", entry.Name+".exe") {
			return receipt, "", errors.New("SERENA_PREPARED_UV_ENTRYPOINT_TOOL_BIN_MISMATCH")
		}
		if entry.Name == "serena" {
			selectedPath = path
		}
	}
	if selectedPath == "" {
		return receipt, "", errors.New("SERENA_PREPARED_UV_ENTRYPOINT_MISMATCH")
	}
	return receipt, selectedPath, nil
}

func preparedSerenaPathInside(root, path string) bool {
	if !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func parsePreparedSerenaMetadata(raw []byte, request PreparedSerenaRequest, environment string) (preparedSerenaMetadata, string, error) {
	var result preparedSerenaMetadata
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) == 0 || len(raw) > 64<<20 {
		return result, "", errors.New("SERENA_PREPARED_METADATA_SIZE")
	}
	if err := rejectSerenaJSONDuplicates(raw); err != nil {
		return result, "", err
	}
	if err := decoder.Decode(&result); err != nil {
		return result, "", err
	}
	if result.Version != 2 || result.Prefix != environment || !preparedSerenaPathInside(environment, result.Executable) || result.Platform != "win32" || result.Machine == "" || result.PythonVersion == "" || result.SOABI == "" || len(result.Distributions) == 0 {
		return result, "", errors.New("SERENA_PREPARED_METADATA_IDENTITY")
	}
	names := map[string]bool{}
	files := map[string]string{}
	foundSource := false
	for _, d := range result.Distributions {
		name := strings.ToLower(strings.ReplaceAll(d.Name, "_", "-"))
		if name == "" || d.Version == "" || names[name] || len(d.Files) == 0 {
			return result, "", errors.New("SERENA_PREPARED_DISTRIBUTION_INVALID")
		}
		names[name] = true
		for _, file := range d.Files {
			if !preparedSerenaPathInside(environment, file.Path) || !preparedSerenaDigest(file.SHA256) {
				return result, "", errors.New("SERENA_PREPARED_DISTRIBUTION_FILE_INVALID")
			}
			if old, ok := files[file.Path]; ok && old != file.SHA256 {
				return result, "", errors.New("SERENA_PREPARED_DISTRIBUTION_FILE_CONFLICT")
			}
			files[file.Path] = file.SHA256
		}
		if name == request.Package {
			if d.DirectURLPresent == nil || !*d.DirectURLPresent || d.DirectURL == nil || d.DirectURL.VCSInfo == nil || d.DirectURL.VCSInfo.VCS != "git" || d.DirectURL.VCSInfo.CommitID != request.SourceCommit {
				return result, "", errors.New("SERENA_PREPARED_DISTRIBUTION_SOURCE_INVALID")
			}
			expected := strings.TrimSuffix(strings.TrimPrefix(request.SourceRequirement, "git+"), "@"+request.SourceCommit)
			if d.DirectURL.URL != expected {
				return result, "", errors.New("SERENA_PREPARED_DISTRIBUTION_SOURCE_INVALID")
			}
			if d.DirectURL.Subdirectory != nil || (d.DirectURL.VCSInfo.RequestedRevision != nil && *d.DirectURL.VCSInfo.RequestedRevision != request.SourceCommit) {
				return result, "", errors.New("SERENA_PREPARED_DISTRIBUTION_SOURCE_INVALID")
			}
			for _, entry := range d.Entrypoints {
				if entry.Name == "serena" && entry.Group == "console_scripts" && entry.Value != "" {
					foundSource = true
				}
			}
		}
	}
	if !foundSource {
		return result, "", errors.New("SERENA_PREPARED_DISTRIBUTION_ENTRYPOINT_MISSING")
	}
	// Resolution freezes actual named versions/provenance, not requested ranges
	// from uv's requirements array. Files are covered separately by inventory.
	encoded, err := serializeSerenaResolutionV2(result.Distributions)
	if err != nil {
		return result, "", err
	}
	return result, preparedSerenaHash(encoded), nil
}

// The same Unicode simple-fold equivalence used by strings.EqualFold. This
// alias veto never replaces the original spelling or native file qualification.
func preparedMetadataIdentityKey(value string) string {
	return strings.Map(func(r rune) rune {
		smallest := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < smallest {
				smallest = next
			}
		}
		return smallest
	}, value)
}

type preparedSerenaExportAssociation struct {
	Receipt preparedSerenaUVEntrypoint
	Member  preparedSerenaMetadataFile
}

// Associate the complete root console/gui declaration set before selecting
// the manifest's console launcher. Legacy missing-From needs unique ownership
// in the collected distributions, not an executable basename inference.
func bindPreparedSerenaReceiptAssociations(receipt preparedSerenaUVReceipt, metadata preparedSerenaMetadata, request PreparedSerenaRequest, environment, selected string) ([]preparedSerenaExportAssociation, error) {
	var root *preparedSerenaDistribution
	owners := map[string]int{}
	declarationOwners := map[string]int{}
	for i := range metadata.Distributions {
		d := &metadata.Distributions[i]
		if strings.ToLower(strings.ReplaceAll(d.Name, "_", "-")) == request.Package {
			if root != nil {
				return nil, errors.New("SERENA_PREPARED_EXPORT_ROOT_DUPLICATE")
			}
			root = d
		}
		for _, f := range d.Files {
			owners[preparedMetadataIdentityKey(f.Path)]++
		}
		for _, e := range d.Entrypoints {
			if e.Group == "console_scripts" || e.Group == "gui_scripts" {
				declarationOwners[preparedMetadataIdentityKey(e.Name)]++
			}
		}
	}
	if root == nil {
		return nil, errors.New("SERENA_PREPARED_EXPORT_ROOT_MISSING")
	}
	declarations := map[string]preparedSerenaMetadataEntrypoint{}
	selectedCount := 0
	for _, e := range root.Entrypoints {
		if e.Group != "console_scripts" && e.Group != "gui_scripts" {
			continue
		}
		key := preparedMetadataIdentityKey(e.Name)
		if e.Name == "" || e.Value == "" || declarations[key].Name != "" {
			return nil, errors.New("SERENA_PREPARED_EXPORT_DECLARATION_DUPLICATE")
		}
		declarations[key] = e
		if e.Name == selected && e.Group == "console_scripts" {
			selectedCount++
		}
	}
	if selected == "" || selectedCount != 1 || len(declarations) != len(receipt.Tool.Entrypoints) {
		return nil, errors.New("SERENA_PREPARED_EXPORT_SET_MISMATCH")
	}
	members := map[string][]preparedSerenaMetadataFile{}
	for _, f := range root.Files {
		key := preparedMetadataIdentityKey(f.Path)
		members[key] = append(members[key], f)
	}
	result := make([]preparedSerenaExportAssociation, 0, len(declarations))
	used := map[string]bool{}
	for _, e := range receipt.Tool.Entrypoints {
		key := preparedMetadataIdentityKey(e.Name)
		d, ok := declarations[key]
		if !ok || d.Name != e.Name || used[key] || (e.From != "" && e.From != request.Package) || (e.From == "" && declarationOwners[key] != 1) {
			return nil, errors.New("SERENA_PREPARED_EXPORT_OWNER_MISMATCH")
		}
		used[key] = true
		memberPath := filepath.Join(environment, "Scripts", d.Name+".exe")
		memberKey := preparedMetadataIdentityKey(memberPath)
		m := members[memberKey]
		if len(m) != 1 || owners[memberKey] != 1 || m[0].Path != memberPath {
			return nil, errors.New("SERENA_PREPARED_EXPORT_ROOT_MEMBER_UNQUALIFIED")
		}
		result = append(result, preparedSerenaExportAssociation{Receipt: e, Member: m[0]})
	}
	return result, nil
}

// Native identities are supplied only by the original no-follow file owner.
// One member may be a regular copy or the same object as its own tool export;
// distinct declared exports may never alias the same native object.
func bindPreparedSerenaExportIdentity(used map[string]string, name string, member, exported PreparedSerenaFile) error {
	if !preparedSerenaFile(member) || !preparedSerenaFile(exported) || member.SHA256 != exported.SHA256 {
		return errors.New("SERENA_PREPARED_EXPORT_FILE_MISMATCH")
	}
	for _, f := range []PreparedSerenaFile{member, exported} {
		key := f.Device + "\x00" + f.FileID
		if owner, ok := used[key]; ok && owner != name {
			return errors.New("SERENA_PREPARED_EXPORT_NATIVE_ALIAS")
		}
		used[key] = name
	}
	return nil
}
