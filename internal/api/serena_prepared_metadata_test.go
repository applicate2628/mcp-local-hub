package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPreparedUVStructuredSourceReceipt(t *testing.T) {
	r := preparedRequest()
	generation := `C:\state\runtime\generation`
	raw := []byte("[tool]\nrequirements = [{name = 'serena-agent', git = 'https://example.invalid/serena?rev=" + r.SourceCommit + "#" + r.SourceCommit + "'}]\nentrypoints = [{name = 'serena', install-path = 'C:/state/runtime/generation/bin/serena.exe', from = 'serena-agent'}]\n")
	if _, _, err := parsePreparedSerenaUVReceipt(raw, r, generation); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{
		[]byte(strings.Replace(string(raw), "{name = 'serena-agent', git = 'https://example.invalid/serena?rev="+r.SourceCommit+"#"+r.SourceCommit+"'}", "'serena-agent'", 1)),
		[]byte(strings.Replace(string(raw), "#"+r.SourceCommit, "#"+strings.Repeat("b", 40), 1)),
		[]byte(strings.Replace(string(raw), "generation/bin", "foreign/bin", 1)),
	} {
		if _, _, err := parsePreparedSerenaUVReceipt(bad, r, generation); err == nil {
			t.Fatal("invalid receipt admitted")
		}
	}
}

func TestPreparedResolvedMetadataIsNotRequestedRequirements(t *testing.T) {
	present, absent := true, false
	r := preparedRequest()
	environment := `C:\state\runtime\generation\tools\serena-agent`
	v := preparedSerenaMetadata{Version: 2, Executable: environment + `\Scripts\python.exe`, BaseExecutable: r.Interpreter.Path, Prefix: environment, PythonVersion: "3.13.11", Platform: "win32", Machine: "AMD64", SOABI: "cp313-win_amd64", Distributions: []preparedSerenaDistribution{{Name: "serena-agent", Version: "0.1", DirectURLPresent: &present, DirectURL: &preparedSerenaDirectURL{URL: "https://example.invalid/serena", VCSInfo: &preparedSerenaVCSInfo{VCS: "git", CommitID: r.SourceCommit}}, Files: []preparedSerenaMetadataFile{{Path: environment + `\Lib\site-packages\serena\cli.py`, SHA256: strings.Repeat("a", 64)}}, Entrypoints: []preparedSerenaMetadataEntrypoint{{Name: "serena", Group: "console_scripts", Value: "serena.cli:main"}}}, {Name: "dependency", Version: "2.4", DirectURLPresent: &absent, Files: []preparedSerenaMetadataFile{{Path: environment + `\Lib\site-packages\dependency.py`, SHA256: strings.Repeat("b", 64)}}}}}
	raw, _ := json.Marshal(v)
	_, resolution, err := parsePreparedSerenaMetadata(raw, r, environment)
	if err != nil || !preparedSerenaDigest(resolution) {
		t.Fatal(err)
	}
	v.Distributions[1].Version = "2.5"
	changed, _ := json.Marshal(v)
	_, other, err := parsePreparedSerenaMetadata(changed, r, environment)
	if err != nil || other == resolution {
		t.Fatal("resolved version not bound")
	}
	v.Distributions[0].DirectURL.VCSInfo.CommitID = strings.Repeat("c", 40)
	bad, _ := json.Marshal(v)
	if _, _, err = parsePreparedSerenaMetadata(bad, r, environment); err == nil {
		t.Fatal("wrong source commit admitted")
	}
}
