package api

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
)

type preparedSerenaVCSInfo struct {
	VCS               string  `json:"vcs"`
	CommitID          string  `json:"commit_id"`
	RequestedRevision *string `json:"requested_revision,omitempty"`
	GitLFS            *bool   `json:"git_lfs,omitempty"`
}
type preparedSerenaArchiveInfo struct {
	Hash   *string            `json:"hash,omitempty"`
	Hashes *map[string]string `json:"hashes,omitempty"`
}
type preparedSerenaDirectoryInfo struct {
	Editable *bool `json:"editable,omitempty"`
}
type preparedSerenaDirectURL struct {
	URL           string                       `json:"url"`
	VCSInfo       *preparedSerenaVCSInfo       `json:"vcs_info,omitempty"`
	ArchiveInfo   *preparedSerenaArchiveInfo   `json:"archive_info,omitempty"`
	DirectoryInfo *preparedSerenaDirectoryInfo `json:"dir_info,omitempty"`
	Subdirectory  *string                      `json:"subdirectory,omitempty"`
}

// The bounded fixed codec rejects duplicate keys before struct decoding can
// replace a field. This is also applied to the entire collector frame.
func rejectSerenaJSONDuplicates(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return errors.New("DIRECT_URL_JSON")
		}
		token, err := d.Token()
		if err != nil {
			return errors.New("DIRECT_URL_JSON")
		}
		delimiter, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				token, err = d.Token()
				if err != nil {
					return errors.New("DIRECT_URL_JSON")
				}
				key, ok := token.(string)
				if !ok || seen[key] {
					return errors.New("DIRECT_URL_JSON")
				}
				seen[key] = true
				if err = value(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return errors.New("DIRECT_URL_JSON")
			}
		case '[':
			for d.More() {
				if err = value(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return errors.New("DIRECT_URL_JSON")
			}
		default:
			return errors.New("DIRECT_URL_JSON")
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("DIRECT_URL_JSON")
	}
	return nil
}
func directURLObject(raw []byte, allowed ...string) (map[string]json.RawMessage, error) {
	if err := rejectSerenaJSONDuplicates(raw); err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return nil, errors.New("DIRECT_URL_JSON")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, errors.New("DIRECT_URL_JSON")
	}
	permit := map[string]bool{}
	for _, key := range allowed {
		permit[key] = true
	}
	for key, value := range obj {
		if !permit[key] {
			return nil, errors.New("DIRECT_URL_UNSUPPORTED_FIELD")
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, errors.New("DIRECT_URL_VALUE")
		}
	}
	return obj, nil
}
func (d *preparedSerenaDirectURL) UnmarshalJSON(raw []byte) error {
	*d = preparedSerenaDirectURL{}
	obj, err := directURLObject(raw, "url", "vcs_info", "archive_info", "dir_info", "subdirectory")
	if err != nil {
		return err
	}
	uri, ok := obj["url"]
	if !ok || json.Unmarshal(uri, &d.URL) != nil || d.URL == "" {
		return errors.New("DIRECT_URL_VALUE")
	}
	parsed, err := url.Parse(d.URL)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.ContainsAny(d.URL, "\x00\r\n") {
		return errors.New("DIRECT_URL_VALUE")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "file" {
		return errors.New("DIRECT_URL_VALUE")
	}
	if parsed.Scheme == "https" && parsed.Host == "" {
		return errors.New("DIRECT_URL_VALUE")
	}
	if parsed.Scheme == "file" && (parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/")) {
		return errors.New("DIRECT_URL_VALUE")
	}
	count := 0
	if info, present := obj["vcs_info"]; present {
		count++
		fields, e := directURLObject(info, "vcs", "commit_id", "requested_revision", "git_lfs")
		if e != nil {
			return e
		}
		if fields["vcs"] == nil || fields["commit_id"] == nil {
			return errors.New("DIRECT_URL_VALUE")
		}
		var v preparedSerenaVCSInfo
		if json.Unmarshal(info, &v) != nil || v.CommitID == "" || strings.ContainsAny(v.CommitID, "\x00\r\n") {
			return errors.New("DIRECT_URL_VALUE")
		}
		switch v.VCS {
		case "git", "hg":
			if len(v.CommitID) != 40 {
				return errors.New("DIRECT_URL_VALUE")
			}
			if _, e = hex.DecodeString(v.CommitID); e != nil {
				return errors.New("DIRECT_URL_VALUE")
			}
		case "svn":
			if _, e = strconv.ParseUint(v.CommitID, 10, 64); e != nil {
				return errors.New("DIRECT_URL_VALUE")
			}
		case "bzr":
		default:
			return errors.New("DIRECT_URL_VALUE")
		}
		if v.RequestedRevision != nil && (*v.RequestedRevision == "" || strings.ContainsAny(*v.RequestedRevision, "\x00\r\n")) {
			return errors.New("DIRECT_URL_VALUE")
		}
		if v.GitLFS != nil && v.VCS != "git" {
			return errors.New("DIRECT_URL_VALUE")
		}
		d.VCSInfo = &v
	}
	if info, present := obj["archive_info"]; present {
		count++
		if _, e := directURLObject(info, "hash", "hashes"); e != nil {
			return e
		}
		var v preparedSerenaArchiveInfo
		if json.Unmarshal(info, &v) != nil {
			return errors.New("DIRECT_URL_VALUE")
		}
		var algorithm, digest string
		if v.Hash != nil {
			var found bool
			algorithm, digest, found = strings.Cut(*v.Hash, "=")
			if !found {
				return errors.New("DIRECT_URL_VALUE")
			}
			algorithm, digest, err = normalizeSerenaArchiveHash(algorithm, digest)
			if err != nil {
				return err
			}
			normalized := algorithm + "=" + digest
			v.Hash = &normalized
		}
		if v.Hashes != nil {
			normalized := map[string]string{}
			for a, h := range *v.Hashes {
				a, h, err = normalizeSerenaArchiveHash(a, h)
				if err != nil {
					return err
				}
				if _, duplicate := normalized[a]; duplicate {
					return errors.New("DIRECT_URL_HASH_CONFLICT")
				}
				normalized[a] = h
			}
			if v.Hash != nil && normalized[algorithm] != digest {
				return errors.New("DIRECT_URL_HASH_CONFLICT")
			}
			v.Hashes = &normalized
		}
		d.ArchiveInfo = &v
	}
	if info, present := obj["dir_info"]; present {
		count++
		if _, e := directURLObject(info, "editable"); e != nil {
			return e
		}
		var v preparedSerenaDirectoryInfo
		if json.Unmarshal(info, &v) != nil || parsed.Scheme != "file" {
			return errors.New("DIRECT_URL_VALUE")
		}
		d.DirectoryInfo = &v
	}
	if count != 1 {
		return errors.New("DIRECT_URL_ORIGIN")
	}
	if raw, present := obj["subdirectory"]; present {
		var value string
		if json.Unmarshal(raw, &value) != nil || !validSerenaSubdirectory(value) {
			return errors.New("DIRECT_URL_VALUE")
		}
		d.Subdirectory = &value
	}
	return nil
}
func normalizeSerenaArchiveHash(algorithm, digest string) (string, string, error) {
	algorithm = strings.ToLower(algorithm)
	digest = strings.ToLower(digest)
	if algorithm == "" || digest == "" {
		return "", "", errors.New("DIRECT_URL_VALUE")
	}
	for _, r := range algorithm {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return "", "", errors.New("DIRECT_URL_VALUE")
		}
	}
	for _, r := range digest {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return "", "", errors.New("DIRECT_URL_VALUE")
		}
	}
	return algorithm, digest, nil
}
func validSerenaSubdirectory(value string) bool {
	if value == "" || strings.ContainsAny(value, ":\x00\r\n") || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "\\") {
		return false
	}
	for _, r := range value {
		if r < 32 || r == 127 {
			return false
		}
	}
	normalized := strings.ReplaceAll(value, "\\", "/")
	for _, part := range strings.Split(normalized, "/") {
		if part == ".." {
			return false
		}
	}
	return !path.IsAbs(normalized)
}

// Required presence is verified independently of the original collector.
func (d *preparedSerenaDistribution) UnmarshalJSON(raw []byte) error {
	if err := rejectSerenaJSONDuplicates(raw); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return errors.New("DIRECT_URL_JSON")
	}
	present, exists := fields["direct_url_present"]
	direct, hasDirect := fields["direct_url"]
	if !exists || !hasDirect || bytes.Equal(bytes.TrimSpace(present), []byte("null")) {
		return errors.New("DIRECT_URL_PRESENCE")
	}
	var flag bool
	if err := json.Unmarshal(present, &flag); err != nil {
		return errors.New("DIRECT_URL_PRESENCE")
	}
	if (!flag && !bytes.Equal(bytes.TrimSpace(direct), []byte("null"))) || (flag && bytes.Equal(bytes.TrimSpace(direct), []byte("null"))) {
		return errors.New("DIRECT_URL_PRESENCE")
	}
	type wire preparedSerenaDistribution
	var value wire
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	*d = preparedSerenaDistribution(value)
	return nil
}

type serenaResolutionOriginV2 struct {
	URL          string          `json:"url"`
	Origin       string          `json:"origin"`
	Subdirectory *string         `json:"subdirectory"`
	Info         json.RawMessage `json:"info"`
}
type serenaResolutionVCSInfoV2 struct {
	VCS               string  `json:"vcs"`
	CommitID          string  `json:"commit_id"`
	RequestedRevision *string `json:"requested_revision"`
	GitLFS            *bool   `json:"git_lfs"`
}
type serenaResolutionArchiveInfoV2 struct {
	Hash   *string            `json:"hash"`
	Hashes *map[string]string `json:"hashes"`
}
type serenaResolutionDirectoryInfoV2 struct {
	Editable *bool `json:"editable"`
}
type serenaResolutionDistributionV2 struct {
	Name    string                    `json:"name"`
	Version string                    `json:"version"`
	Present bool                      `json:"direct_url_present"`
	Direct  *serenaResolutionOriginV2 `json:"direct_url"`
}
type serenaResolutionProjectionV2 struct {
	Schema        string                           `json:"schema"`
	Version       int                              `json:"version"`
	Distributions []serenaResolutionDistributionV2 `json:"distributions"`
}

func serializeSerenaResolutionV2(distributions []preparedSerenaDistribution) ([]byte, error) {
	projection := serenaResolutionProjectionV2{Schema: "serena-installed-resolution-v2", Version: 2, Distributions: []serenaResolutionDistributionV2{}}
	for _, d := range distributions {
		if d.DirectURLPresent == nil {
			return nil, errors.New("DIRECT_URL_PRESENCE")
		}
		row := serenaResolutionDistributionV2{Name: strings.ToLower(strings.ReplaceAll(d.Name, "_", "-")), Version: d.Version, Present: *d.DirectURLPresent}
		if row.Present {
			if d.DirectURL == nil {
				return nil, errors.New("DIRECT_URL_PRESENCE")
			}
			direct := d.DirectURL
			origin := &serenaResolutionOriginV2{URL: direct.URL, Subdirectory: direct.Subdirectory}
			var err error
			switch {
			case direct.VCSInfo != nil:
				v := direct.VCSInfo
				origin.Origin = "vcs"
				origin.Info, err = json.Marshal(serenaResolutionVCSInfoV2{v.VCS, v.CommitID, v.RequestedRevision, v.GitLFS})
			case direct.ArchiveInfo != nil:
				v := direct.ArchiveInfo
				origin.Origin = "archive"
				origin.Info, err = json.Marshal(serenaResolutionArchiveInfoV2{v.Hash, v.Hashes})
			case direct.DirectoryInfo != nil:
				v := direct.DirectoryInfo
				origin.Origin = "directory"
				origin.Info, err = json.Marshal(serenaResolutionDirectoryInfoV2{v.Editable})
			default:
				return nil, errors.New("DIRECT_URL_ORIGIN")
			}
			if err != nil {
				return nil, err
			}
			row.Direct = origin
		} else if d.DirectURL != nil {
			return nil, errors.New("DIRECT_URL_PRESENCE")
		}
		projection.Distributions = append(projection.Distributions, row)
	}
	sort.Slice(projection.Distributions, func(i, j int) bool { return projection.Distributions[i].Name < projection.Distributions[j].Name })
	return json.Marshal(projection)
}
