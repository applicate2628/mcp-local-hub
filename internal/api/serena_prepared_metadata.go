package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"strings"

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

// ToolWire uses flattened structured requirements. The git serializer carries
// the exact commit in a URL fragment, never the input git+URL@commit spelling.
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
	if !nameOK || name != request.Package || !gitOK || err != nil || expectedErr != nil || u.User != nil || u.Scheme != "https" || u.Fragment != request.SourceCommit || u.Scheme != expected.Scheme || u.Host != expected.Host || u.Path != expected.Path {
		return receipt, "", errors.New("SERENA_PREPARED_UV_SOURCE_MISMATCH")
	}
	for key, value := range u.Query() {
		if key != "rev" || len(value) != 1 || value[0] != request.SourceCommit {
			return receipt, "", errors.New("SERENA_PREPARED_UV_SOURCE_POLICY_MISMATCH")
		}
	}
	if len(requirement) != 2 {
		return receipt, "", errors.New("SERENA_PREPARED_UV_SOURCE_POLICY_MISMATCH")
	}
	if len(receipt.Tool.Entrypoints) != 1 || receipt.Tool.Entrypoints[0].Name != "serena" || (receipt.Tool.Entrypoints[0].From != "" && receipt.Tool.Entrypoints[0].From != request.Package) {
		return receipt, "", errors.New("SERENA_PREPARED_UV_ENTRYPOINT_MISMATCH")
	}
	path := filepath.Clean(filepath.FromSlash(receipt.Tool.Entrypoints[0].InstallPath))
	if !filepath.IsAbs(path) || !preparedSerenaPathInside(generation, path) {
		return receipt, "", errors.New("SERENA_PREPARED_UV_ENTRYPOINT_OUTSIDE_GENERATION")
	}
	return receipt, path, nil
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
