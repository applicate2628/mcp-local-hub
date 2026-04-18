package perftools

import (
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDetectTools_ReportsInstalledWithVersion(t *testing.T) {
	catalog := DetectTools()

	if catalog == nil {
		t.Fatal("DetectTools returned nil")
	}
	if catalog.ClangTidy == nil || catalog.Hyperfine == nil ||
		catalog.LLVMObjdump == nil || catalog.IWYU == nil {
		t.Errorf("catalog missing entries: %+v", catalog)
	}
	// If any tool is marked Installed it must have a non-empty Version and Path.
	for name, info := range catalog.AsMap() {
		if info.Installed && info.Version == "" {
			t.Errorf("%s marked installed but Version empty: %+v", name, info)
		}
		if info.Installed && info.Path == "" {
			t.Errorf("%s marked installed but Path empty: %+v", name, info)
		}
		if !info.Installed && info.Error == "" {
			t.Errorf("%s marked NOT installed but Error empty: %+v", name, info)
		}
	}
}

func TestToolsResource_ReturnsJSONCatalog(t *testing.T) {
	srv := &PerfToolbox{tools: DetectTools()}

	req := &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{}}
	req.Params.URI = "resource://tools"

	result, err := srv.getToolsResource(t.Context(), req)
	if err != nil {
		t.Fatalf("getToolsResource: %v", err)
	}
	if len(result.Contents) == 0 {
		t.Fatal("empty Contents")
	}
	rc := result.Contents[0]
	if rc == nil || rc.Text == "" {
		t.Fatalf("Contents[0] has no text: %+v", rc)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(rc.Text), &parsed); err != nil {
		t.Fatalf("resource text is not valid JSON: %v\n%s", err, rc.Text)
	}
	for _, key := range []string{"clang-tidy", "hyperfine", "llvm-objdump", "include-what-you-use"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("catalog missing key %q in JSON: %s", key, rc.Text)
		}
	}
}
