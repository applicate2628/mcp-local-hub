//go:build windows

package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validOptionsForTest() options {
	return options{
		checkout:    "D:\\work\\checkout",
		commit:      strings.Repeat("a", 40),
		goPath:      "D:\\tools\\go.exe",
		goRoot:      "D:\\tools\\go-root",
		moduleCache: "D:\\cache\\modules",
		root:        "D:\\work\\qualification",
		timeout:     time.Minute,
	}
}

func TestQualificationBuilderRejectsAmbiguousOrOverlappingInputs(t *testing.T) {
	good := validOptionsForTest()
	if err := validateOptions(good, 0); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*options){
		"unbounded deadline":       func(o *options) { o.timeout = 0 },
		"excessive deadline":       func(o *options) { o.timeout = 11 * time.Minute },
		"noncanonical commit":      func(o *options) { o.commit = strings.Repeat("A", 40) },
		"relative checkout":        func(o *options) { o.checkout = "checkout" },
		"device root":              func(o *options) { o.root = "\\\\?\\D:\\work\\qualification" },
		"UNC root":                 func(o *options) { o.root = "\\\\server\\share\\qualification" },
		"alternate data stream":    func(o *options) { o.goPath += ":stream" },
		"checkout output":          func(o *options) { o.root = filepath.Join(o.checkout, "build") },
		"cache output":             func(o *options) { o.root = filepath.Join(o.moduleCache, "build") },
		"output includes checkout": func(o *options) { o.root = "D:\\work" },
	} {
		t.Run(name, func(t *testing.T) {
			o := good
			mutate(&o)
			if err := validateOptions(o, 0); err == nil {
				t.Fatal("ambiguous or overlapping input accepted")
			}
		})
	}
	if err := validateOptions(good, 1); err == nil {
		t.Fatal("unbound positional input accepted")
	}
}

func TestQualificationBuildEnvironmentCannotInheritSourceOrTagOverrides(t *testing.T) {
	t.Setenv("GOFLAGS", "-tags=foreign -overlay=D:\\foreign.json")
	t.Setenv("GOROOT", "D:\\foreign-toolchain")
	t.Setenv("GOCACHEPROG", "D:\\foreign-cache.exe")
	t.Setenv("GO111MODULE", "off")
	t.Setenv("GOEXPERIMENT", "foreign")
	t.Setenv("GOAMD64", "v3")
	t.Setenv("GOFIPS140", "latest")
	t.Setenv("GOPROXY", "https://foreign.invalid,direct")
	t.Setenv("GOPRIVATE", "*")
	t.Setenv("GONOPROXY", "*")
	t.Setenv("GONOSUMDB", "*")
	t.Setenv("APPDATA", "C:\\foreign-config")
	t.Setenv("LOCALAPPDATA", "C:\\foreign-cache")
	t.Setenv("GOWORK", "D:\\foreign.work")
	t.Setenv("GOOS", "linux")
	t.Setenv("CGO_ENABLED", "1")
	t.Setenv("MCPHUB_TEST_CHILD_EXIT_CODE", "0")
	o := validOptionsForTest()
	got := map[string]string{}
	for _, entry := range buildEnvironment(o) {
		key, value, _ := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		if strings.HasPrefix(key, "MCPHUB_") {
			t.Fatalf("inherited helper sentinel %s", key)
		}
		if _, ok := got[key]; ok && key != "" {
			t.Fatalf("duplicate environment key %q", key)
		}
		got[key] = value
	}
	for key, want := range map[string]string{
		"GOFLAGS": "", "GOWORK": "off", "GOENV": "off", "GOTOOLCHAIN": "local",
		"GOPROXY": "file:///D:/cache/modules/cache/download", "GOSUMDB": "off", "GOVCS": "*:off", "CGO_ENABLED": "0",
		"GOOS": "windows", "GOARCH": "amd64", "GOMODCACHE": o.moduleCache,
		"GOROOT": o.goRoot, "GOCACHEPROG": "", "GO111MODULE": "on", "GOEXPERIMENT": "",
		"GOAMD64": "v1", "GOFIPS140": "off",
		"GOPRIVATE": "", "GONOPROXY": "none", "GONOSUMDB": "none",
		"APPDATA": filepath.Join(o.root, "appdata"), "LOCALAPPDATA": filepath.Join(o.root, "localappdata"),
		"GOCACHE": filepath.Join(o.root, "gocache"), "GOTMPDIR": filepath.Join(o.root, "temp"),
	} {
		if got[key] != want {
			t.Errorf("%s=%q, want %q", key, got[key], want)
		}
	}
	if os.Getenv("MCPHUB_TEST_CHILD_EXIT_CODE") != "0" {
		t.Fatal("child environment selection mutated the parent")
	}
}

func TestQualificationBuildEnvironmentKeepsProxyPathLiteral(t *testing.T) {
	o := validOptionsForTest()
	o.moduleCache = `D:\cache space,$literal%value#fragment\modules`
	var proxy string
	for _, entry := range buildEnvironment(o) {
		if value, ok := strings.CutPrefix(entry, "GOPROXY="); ok {
			proxy = value
		}
	}
	if strings.ContainsAny(proxy, ",|") {
		t.Fatalf("module path introduced a proxy fallback: %q", proxy)
	}
	parsed, err := url.Parse(proxy)
	if err != nil {
		t.Fatal(err)
	}
	want := "/" + filepath.ToSlash(filepath.Join(o.moduleCache, "cache", "download"))
	if parsed.Scheme != "file" || parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != want {
		t.Fatalf("local proxy changed its path or transport: %q, parsed %#v, want %q", proxy, parsed, want)
	}
}
