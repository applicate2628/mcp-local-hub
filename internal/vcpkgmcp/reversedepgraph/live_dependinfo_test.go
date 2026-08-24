package reversedepgraph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

func TestLiveDependInfoOracle(t *testing.T) {
	root := os.Getenv("VCPKG_REVERSE_DEPENDENCIES_LIVE_ROOT")
	if root == "" {
		t.Skip("set VCPKG_REVERSE_DEPENDENCIES_LIVE_ROOT for the installed-vcpkg oracle")
	}
	scratch := os.Getenv("VCPKG_REVERSE_DEPENDENCIES_LIVE_SCRATCH")
	if scratch == "" {
		t.Fatal("VCPKG_REVERSE_DEPENDENCIES_LIVE_SCRATCH is required for the live oracle")
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	before := snapshotLiveInputs(t, root)
	args := Args{
		Port: "zlib", VcpkgRoot: root,
		Triplet: "x64-windows", HostTriplet: "x64-windows",
		ScratchRoot: scratch, TimeoutMS: MaxTimeoutMS,
	}
	if err := ValidateArgs(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	requestScratch, err := os.MkdirTemp(scratch, "live-oracle-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(requestScratch)
	runner := DefaultRunner()
	plans := map[string]Plan{}
	for _, candidate := range []string{"zlib", "curl"} {
		outputs := map[string]RunOutput{}
		for _, format := range []string{"dgml", "list"} {
			commandScratch := filepath.Join(requestScratch, candidate+"-"+format)
			for _, directory := range []string{commandScratch, filepath.Join(commandScratch, "buildtrees"), filepath.Join(commandScratch, "installed"), filepath.Join(commandScratch, "downloads"), filepath.Join(commandScratch, "packages")} {
				if err := os.MkdirAll(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			command := DependInfoCommand(args, candidate, format, commandScratch)
			output := runner.Run(context.Background(), command)
			if output.ExitCode != 0 || output.Err != nil || !output.Started || !output.Reaped || output.Stdout.Truncated || output.Stderr.Truncated {
				t.Fatalf("live command failed candidate=%s format=%s output=%#v", candidate, format, output)
			}
			outputs[format] = output
		}
		plan, failure := ParseResolvedPlan(outputs["dgml"].Stdout.Data, outputs["list"].Stdout.Data, outputs["list"].Stderr.Data, args.Triplet, args.HostTriplet)
		if failure != nil {
			t.Fatalf("live format disagreement candidate=%s: %#v", candidate, failure)
		}
		plans[candidate] = plan
	}
	var curl *Node
	edgeFound := false
	for _, node := range plans["curl"].Nodes {
		if node.Name == "curl" {
			nodeCopy := node
			curl = &nodeCopy
		}
	}
	for _, edge := range plans["curl"].Edges {
		edgeFound = edgeFound || (edge.From.Name == "curl" && edge.To.Name == "zlib")
	}
	if curl == nil {
		t.Fatalf("curl node missing from live resolved plan: %#v", plans["curl"])
	}
	if !edgeFound {
		t.Fatalf("curl -> zlib missing from live resolved plan: %#v", plans["curl"].Edges)
	}
	if !containsString(curl.Features, "ssl") || len(curl.Features) == 0 {
		t.Fatalf("curl resolved feature metadata missing: %#v", curl.Features)
	}
	if err := os.RemoveAll(requestScratch); err != nil {
		t.Fatal(err)
	}
	full := Analyze(context.Background(), args, runner)
	if full.Status != "ok" || !full.Coverage.Complete || full.Coverage.PotentialCount <= 512 {
		t.Fatalf("full common-port graph did not settle through batches: status=%s reason=%s failure=%#v coverage=%#v diagnostics=%#v", full.Status, full.Reason, full.Failure, full.Coverage, full.Diagnostics)
	}
	fullCurl := false
	for _, dependent := range full.Direct {
		fullCurl = fullCurl || dependent.Node.Name == "curl"
	}
	if !fullCurl {
		t.Fatalf("full common-port graph omitted curl direct dependent: direct_count=%d", len(full.Direct))
	}
	wire, err := publicresult.MarshalIndent(full)
	if err != nil || len(wire) > publicresult.MaxEncodedBytes || !json.Valid(wire) {
		t.Fatalf("live public projection invalid: bytes=%d err=%v", len(wire), err)
	}
	if full.PublicResultRequiresProjection(publicresult.MaxEncodedBytes) && !strings.Contains(string(wire), `"result_projection"`) {
		t.Fatalf("oversized live result omitted projection metadata: bytes=%d", len(wire))
	}
	after := snapshotLiveInputs(t, root)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("vcpkg inputs changed\nbefore=%#v\nafter=%#v", before, after)
	}
	entries, err := os.ReadDir(scratch)
	if err != nil || len(entries) != 0 {
		t.Fatalf("request scratch did not clean up: entries=%v err=%v", entries, err)
	}
}

func snapshotLiveInputs(t *testing.T, root string) []string {
	t.Helper()
	paths := []string{
		vcpkgExecutable(root),
		filepath.Join(root, "ports", "zlib", "vcpkg.json"),
		filepath.Join(root, "ports", "zlib", "portfile.cmake"),
		filepath.Join(root, "ports", "curl", "vcpkg.json"),
		filepath.Join(root, "ports", "curl", "portfile.cmake"),
	}
	rows := make([]string, 0, len(paths))
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("snapshot %s: %v", path, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("snapshot stat %s: %v", path, err)
		}
		digest := sha256.Sum256(body)
		rows = append(rows, filepath.Clean(path)+"|"+info.Mode().String()+"|"+hex.EncodeToString(digest[:]))
	}
	sort.Strings(rows)
	return rows
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
