package api

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreparedUVStructuredSourceReceipt(t *testing.T) {
	r := preparedRequest()
	generation := `C:\state\runtime\generation`
	base := "https://example.invalid/serena"
	commit := r.SourceCommit
	for _, tc := range []struct {
		name, url string
		valid     bool
	}{
		{"query", base + "?rev=" + commit, true}, {"fragment", base + "#" + commit, true}, {"both", base + "?rev=" + commit + "#" + commit, true},
		{"missing", base, false}, {"emptyQuery", base + "?", false}, {"emptyRev", base + "?rev=", false}, {"emptyFragment", base + "?rev=" + commit + "#", false},
		{"wrongRev", base + "?rev=" + strings.Repeat("b", 40), false},
		{"differentRetainedSupplierSource", base + "?rev=5e4f5cd173c7ff3eed37ee3f1de6c266b937c5dc", false}, {"wrongFragment", base + "#" + strings.Repeat("b", 40), false}, {"conflict", base + "?rev=" + commit + "#" + strings.Repeat("b", 40), false},
		{"duplicateSame", base + "?rev=" + commit + "&rev=" + commit, false}, {"duplicateDifferent", base + "?rev=" + commit + "&rev=" + strings.Repeat("b", 40), false},
		{"malformedQuery", base + "?rev=" + commit + "&broken=%ZZ#" + commit, false}, {"semicolonQuery", base + "?rev=" + commit + ";tag=x#" + commit, false},
		{"unknownKey", base + "?rev=" + commit + "&branch=main", false}, {"wrongHost", "https://foreign.invalid/serena#" + commit, false},
		{"userinfo", "https://user@example.invalid/serena#" + commit, false}, {"port", "https://example.invalid:443/serena#" + commit, false},
		{"subdirectory", base + "/sub#" + commit, false}, {"schema", "http://example.invalid/serena#" + commit, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte("[tool]\nrequirements = [{name = 'serena-agent', git = '" + tc.url + "'}]\nentrypoints = [{name = 'serena', install-path = 'C:/state/runtime/generation/bin/serena.exe', from = 'serena-agent'}]\n")
			_, _, err := parsePreparedSerenaUVReceipt(raw, r, generation)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
	raw := "[tool]\nrequirements = [{name = 'serena-agent', git = '" + base + "?rev=" + commit + "'}]\nentrypoints = [{name = 'serena', install-path = 'C:/state/runtime/generation/bin/serena.exe', from = 'serena-agent'}]\n"
	for name, bad := range map[string]string{
		"unstructured":          strings.Replace(raw, "{name = 'serena-agent', git = '"+base+"?rev="+commit+"'}", "'serena-agent'", 1),
		"extraRequirementField": strings.Replace(raw, "git =", "branch = 'main', git =", 1),
		"outside":               strings.Replace(raw, "generation/bin", "foreign/bin", 1),
		"caseToolBin":           strings.Replace(raw, "generation/bin", "generation/Bin", 1),
		"pathAlias":             strings.Replace(raw, "bin/serena.exe", "bin/../bin/serena.exe", 1),
		"foreignOwner":          strings.Replace(raw, "from = 'serena-agent'", "from = 'foreign'", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parsePreparedSerenaUVReceipt([]byte(bad), r, generation); err == nil {
				t.Fatal("invalid receipt admitted")
			}
		})
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
	v.Distributions[0].Entrypoints = append(v.Distributions[0].Entrypoints, preparedSerenaMetadataEntrypoint{Name: "serena-agent", Group: "console_scripts", Value: "serena.cli:agent"}, preparedSerenaMetadataEntrypoint{Name: "serena-hooks", Group: "gui_scripts", Value: "serena.cli:hooks"})
	full, _ := json.Marshal(v)
	received, sameResolution, err := parsePreparedSerenaMetadata(full, r, environment)
	if err != nil || len(received.Distributions[0].Entrypoints) != 3 || sameResolution != other {
		t.Fatal("full exports or unchanged resolution projection lost", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*preparedSerenaDirectURL)
	}{
		{"commit", func(d *preparedSerenaDirectURL) { d.VCSInfo.CommitID = strings.Repeat("c", 40) }},
		{"url", func(d *preparedSerenaDirectURL) { d.URL = "https://foreign.invalid/serena" }},
		{"requestedRevision", func(d *preparedSerenaDirectURL) { s := "main"; d.VCSInfo.RequestedRevision = &s }},
		{"subdirectory", func(d *preparedSerenaDirectURL) { s := "sub"; d.Subdirectory = &s }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var copy preparedSerenaMetadata
			if err := json.Unmarshal(full, &copy); err != nil {
				t.Fatal(err)
			}
			tc.mutate(copy.Distributions[0].DirectURL)
			bad, _ := json.Marshal(copy)
			if _, _, err := parsePreparedSerenaMetadata(bad, r, environment); err == nil {
				t.Fatal("wrong installed source admitted")
			}
		})
	}
}

func preparedExportFixture(t *testing.T) (PreparedSerenaRequest, preparedSerenaUVReceipt, preparedSerenaMetadata, string) {
	t.Helper()
	r := preparedRequest()
	environment := `C:\state\runtime\generation\tools\serena-agent`
	generation := `C:\state\runtime\generation`
	var receipt preparedSerenaUVReceipt
	metadata := preparedSerenaMetadata{Distributions: []preparedSerenaDistribution{{Name: r.Package}}}
	for _, e := range []struct{ name, group string }{{"serena", "console_scripts"}, {"serena-agent", "console_scripts"}, {"serena-hooks", "gui_scripts"}} {
		receipt.Tool.Entrypoints = append(receipt.Tool.Entrypoints, preparedSerenaUVEntrypoint{Name: e.name, From: r.Package, InstallPath: filepath.Join(generation, "bin", e.name+".exe")})
		metadata.Distributions[0].Entrypoints = append(metadata.Distributions[0].Entrypoints, preparedSerenaMetadataEntrypoint{Name: e.name, Group: e.group, Value: "serena.cli:main"})
		metadata.Distributions[0].Files = append(metadata.Distributions[0].Files, preparedSerenaMetadataFile{Path: filepath.Join(environment, "Scripts", e.name+".exe"), SHA256: strings.Repeat("a", 64)})
	}
	return r, receipt, metadata, environment
}

func TestPreparedCompleteRootExports(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*preparedSerenaUVReceipt, *preparedSerenaMetadata)
		valid  bool
	}{
		{"complete", func(_ *preparedSerenaUVReceipt, _ *preparedSerenaMetadata) {}, true},
		{"legacyUnique", func(r *preparedSerenaUVReceipt, _ *preparedSerenaMetadata) {
			for i := range r.Tool.Entrypoints {
				r.Tool.Entrypoints[i].From = ""
			}
		}, true},
		{"missingReceipt", func(r *preparedSerenaUVReceipt, _ *preparedSerenaMetadata) {
			r.Tool.Entrypoints = r.Tool.Entrypoints[:2]
		}, false},
		{"missingDeclaration", func(_ *preparedSerenaUVReceipt, m *preparedSerenaMetadata) {
			m.Distributions[0].Entrypoints = m.Distributions[0].Entrypoints[:2]
		}, false},
		{"duplicateReceipt", func(r *preparedSerenaUVReceipt, _ *preparedSerenaMetadata) {
			r.Tool.Entrypoints[2] = r.Tool.Entrypoints[1]
		}, false},
		{"caseDeclaration", func(_ *preparedSerenaUVReceipt, m *preparedSerenaMetadata) {
			m.Distributions[0].Entrypoints[2].Name = "Serena"
		}, false},
		{"missingSelected", func(_ *preparedSerenaUVReceipt, m *preparedSerenaMetadata) {
			m.Distributions[0].Entrypoints[0].Group = "gui_scripts"
		}, false},
		{"foreignOwner", func(r *preparedSerenaUVReceipt, _ *preparedSerenaMetadata) { r.Tool.Entrypoints[1].From = "dependency" }, false},
		{"rootMemberMissing", func(_ *preparedSerenaUVReceipt, m *preparedSerenaMetadata) {
			m.Distributions[0].Files = m.Distributions[0].Files[:2]
		}, false},
		{"rootMemberAlias", func(_ *preparedSerenaUVReceipt, m *preparedSerenaMetadata) {
			f := m.Distributions[0].Files[0]
			f.Path = strings.Replace(f.Path, "serena.exe", "Serena.exe", 1)
			m.Distributions[0].Files = append(m.Distributions[0].Files, f)
		}, false},
		{"foreignMemberOwner", func(_ *preparedSerenaUVReceipt, m *preparedSerenaMetadata) {
			m.Distributions = append(m.Distributions, preparedSerenaDistribution{Name: "dependency", Files: []preparedSerenaMetadataFile{m.Distributions[0].Files[0]}})
		}, false},
		{"legacyAmbiguous", func(r *preparedSerenaUVReceipt, m *preparedSerenaMetadata) {
			r.Tool.Entrypoints[0].From = ""
			m.Distributions = append(m.Distributions, preparedSerenaDistribution{Name: "dependency", Entrypoints: []preparedSerenaMetadataEntrypoint{m.Distributions[0].Entrypoints[0]}})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, receipt, m, environment := preparedExportFixture(t)
			tc.mutate(&receipt, &m)
			associations, err := bindPreparedSerenaReceiptAssociations(receipt, m, r, environment, "serena")
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
			if err == nil && len(associations) != 3 {
				t.Fatal("exports truncated")
			}
		})
	}
	// Exercise the original receipt parser with all exports, including name/path aliases.
	r, receipt, _, _ := preparedExportFixture(t)
	for _, name := range []string{"valid", "caseName", "samePath", "emptyName"} {
		t.Run(name, func(t *testing.T) {
			entries := append([]preparedSerenaUVEntrypoint(nil), receipt.Tool.Entrypoints...)
			switch name {
			case "caseName":
				entries[2].Name = "Serena"
			case "samePath":
				entries[2].InstallPath = entries[0].InstallPath
			case "emptyName":
				entries[2].Name = ""
			}
			raw := "[tool]\nrequirements=[{name='serena-agent',git='https://example.invalid/serena?rev=" + r.SourceCommit + "'}]\nentrypoints=["
			for _, e := range entries {
				raw += "{name='" + e.Name + "',install-path='" + filepath.ToSlash(e.InstallPath) + "',from='" + e.From + "'},"
			}
			raw += "]\n"
			_, _, err := parsePreparedSerenaUVReceipt([]byte(raw), r, `C:\state\runtime\generation`)
			if (err == nil) != (name == "valid") {
				t.Fatalf("%v", err)
			}
		})
	}
}

func TestPreparedExportNativeIdentityVeto(t *testing.T) {
	// Synthetic identity inputs test only the association veto; they are not a
	// native owner, publication receipt, installation or runtime qualification.
	file := func(path, id string) PreparedSerenaFile {
		return PreparedSerenaFile{Path: path, SHA256: strings.Repeat("a", 64), Device: "fixture", FileID: id}
	}
	a := file(`C:\root\Scripts\serena.exe`, "member-a")
	b := file(`C:\root\bin\serena.exe`, "export-a")
	used := map[string]string{}
	if err := bindPreparedSerenaExportIdentity(used, "serena", a, b); err != nil {
		t.Fatal(err)
	}
	if err := bindPreparedSerenaExportIdentity(used, "other", file(`C:\root\Scripts\other.exe`, "member-b"), file(`C:\root\bin\other.exe`, "export-a")); err == nil {
		t.Fatal("native alias admitted")
	}
	if err := bindPreparedSerenaExportIdentity(map[string]string{}, "serena", a, file(b.Path, "member-a")); err != nil {
		t.Fatal("same object relation refused", err)
	}
	b.SHA256 = strings.Repeat("b", 64)
	if err := bindPreparedSerenaExportIdentity(map[string]string{}, "serena", a, b); err == nil {
		t.Fatal("copy byte drift admitted")
	}
	if preparedMetadataIdentityKey("SK") != preparedMetadataIdentityKey("sk") {
		t.Fatal("Unicode identity fold drift")
	}
}

func TestPreparedInstallMetadataOrderingAndRetention(t *testing.T) {
	// Static ordering guard supplements pure fixtures. No contained command,
	// installed namespace, native publisher or Product runs in this test.
	raw, err := os.ReadFile("serena_prepared_install_effects.go")
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "effects", raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	positions := map[string]token.Pos{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok {
			if _, exists := positions[id.Name]; !exists {
				positions[id.Name] = call.Pos()
			}
		}
		return true
	})
	if positions["parsePreparedSerenaMetadata"] == 0 || positions["bindPreparedSerenaReceiptAssociations"] <= positions["parsePreparedSerenaMetadata"] {
		t.Fatal("association precedes installed metadata")
	}
	body := string(raw)
	if strings.Index(body, "installed.Entrypoint = selected") < strings.Index(body, "bindPreparedSerenaExportIdentity") {
		t.Fatal("selected launcher accepted before full association")
	}
	if !strings.Contains(body, "combined.commands = append(combined.commands, metadataCommand)") || !strings.Contains(body, "combined.receiptRaw = append([]byte(nil), receiptRaw...)") {
		t.Fatal("late failure drops original outcomes")
	}
}
