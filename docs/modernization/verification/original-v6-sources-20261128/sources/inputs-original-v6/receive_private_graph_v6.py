"""Receive original Go declared test inputs using the preserved v7 closure predicate.
Reads only an already completed producer capture; no process or test is launched.
"""
import sys
sys.dont_write_bytecode=True
import argparse,hashlib,json,os,stat
from pathlib import Path
PACKAGE="mcp-local-hub/internal/cli"
RESOURCE="internal/cli/zz_publisher_test_versioninfo.syso"
INPUT_FIELDS=("GoFiles","CgoFiles","TestGoFiles","XTestGoFiles","CFiles","CXXFiles",
    "MFiles","HFiles","FFiles","SFiles","SwigFiles","SwigCXXFiles","SysoFiles",
    "EmbedFiles","TestEmbedFiles","XTestEmbedFiles")
def sha(path):
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream,"sha256").hexdigest()
def ordinary(path,directory=None):
    path=Path(path)
    if not path.is_absolute() or ".." in path.parts:
        raise RuntimeError("absolute clean input path required")
    for p in (*reversed(path.parents),path):
        info=p.lstat()
        if stat.S_ISLNK(info.st_mode) or getattr(info,"st_file_attributes",0)&0x400:
            raise RuntimeError("reparse input refused")
    if directory is True and not path.is_dir() or directory is False and not path.is_file():
        raise RuntimeError("input path kind differs")
    return path
def unique_object(pairs):
    value={}
    for key,item in pairs:
        if key in value: raise RuntimeError("duplicate original JSON field")
        value[key]=item
    return value
def invalid_constant(value):
    raise RuntimeError("nonfinite original JSON number")
def add_inputs(inputs,additions):
    for path,digest in additions.items():
        if path in inputs and inputs[path]!=digest: raise RuntimeError("conflicting input digest")
        inputs[path]=digest
def new_json(path,value):
    with path.open("x",encoding="utf-8",newline="\n") as stream:
        json.dump(value,stream,indent=2);stream.write("\n");stream.flush();os.fsync(stream.fileno())

def receive_graph(args):
    """Actual original Go list producer; no independently asserted source epoch."""
    source=ordinary(args.source,True)
    snapshot=ordinary(args.snapshot,False)
    if sha(snapshot)!=args.snapshot_sha256:
        raise RuntimeError("original source snapshot changed")
    table=json.loads(snapshot.read_text(encoding="utf-8-sig"),object_pairs_hook=unique_object)
    if table.get("schema")!="reused-graphify-source-snapshot-v1" or len(table.get("files",{}))!=3508:
        raise RuntimeError("complete original 3ebe source snapshot required")
    selected={name:row["sha256"] for name,row in table["files"].items()}
    resource=args.resource_sha256
    graph=ordinary(args.graph,False)
    if sha(graph)!=args.graph_sha256:
        raise RuntimeError("original Go list output changed before receiving")
    raw=graph.read_text(encoding="utf-8-sig")
    decoder = json.JSONDecoder(object_pairs_hook=unique_object, parse_constant=invalid_constant)
    offset, rows, files, objects, tool_objects, generated = 0, [], {}, {}, {}, []
    external = tuple(ordinary(os.environ[k], True) for k in ("GOROOT", "GOMODCACHE"))
    cache = ordinary(os.environ["GOCACHE"], True)
    race_object = external[0] / "src/runtime/race/internal/amd64v1/race_windows.syso"
    while offset < len(raw):
        while offset < len(raw) and raw[offset].isspace():
            offset += 1
        if offset == len(raw):
            break
        row, offset = decoder.raw_decode(raw, offset)
        if row.get("Error") or row.get("DepsErrors") or not row.get("Dir"):
            raise RuntimeError("test dependency closure is unresolved")
        directory = ordinary(row["Dir"], True)
        main = row.get("ImportPath") == PACKAGE + ".test"
        if main and (row.get("Name") != "main" or len(row.get("GoFiles", [])) != 1):
            raise RuntimeError("original generated test-main row has changed")
        observed = {}
        entries = [(field, directory / rel) for field in INPUT_FIELDS for rel in row.get(field, [])]
        module = row.get("Module")
        while module:
            if module.get("GoMod"):
                path = Path(module["GoMod"])
                entries.append(("GoMod", path))
                if path.with_name("go.sum").is_file():
                    entries.append(("GoSum", path.with_name("go.sum")))
            module = module.get("Replace")
        for field, path in entries:
            path = ordinary(path, False)
            digest = sha(path)
            if path.is_relative_to(source):
                relative = path.relative_to(source).as_posix()
                if selected.get(relative) != digest and not (
                        resource is not None and relative == RESOURCE and digest == resource):
                    raise RuntimeError("Go selected input outside original Source: " + str(path))
            elif main and field == "GoFiles" and path.is_relative_to(cache):
                generated.append({"path": str(path), "sha256": digest})
            elif not any(path.is_relative_to(root) for root in external):
                raise RuntimeError("Go selected undeclared external input: " + str(path))
            if field == "SysoFiles":
                if path.is_relative_to(source):
                    objects[str(path)] = digest
                elif path == race_object and row.get("Standard") is True and row.get("ImportPath") == "runtime/race/internal/amd64v1":
                    tool_objects[str(path)] = digest
                else:
                    raise RuntimeError("undeclared object input requires review: " + str(path))
            add_inputs(files, {str(path): digest})
            observed[str(path)] = {"sha256": digest, "field": field}
        rows.append({"import_path": row["ImportPath"], "directory": str(directory), "files": observed})
    expected_objects = {} if resource is None else {str(source / RESOURCE): resource}
    if not rows or len(generated) != 1 or objects != expected_objects or set(tool_objects) != set():
        raise RuntimeError("closure must contain one original generated main and exactly its declared derivative resource")
    result = {"schema": "private-cli-test-closure-v1", "source": str(source), "rows": rows,
              "files": files, "generated_main": generated[0], "objects": objects, "tool_objects": tool_objects,
              "scope": "Original Go test/source/embed/module/replacement inputs; generated main data, not source authority"}
    new_json(Path(args.output),result)
    return {"path":args.output,"sha256":sha(args.output),"generated_main":result["generated_main"]}


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    for key in ("source","snapshot","snapshot-sha256","graph","graph-sha256","output"):
        parser.add_argument("--"+key,required=True)
    parser.add_argument("--resource-sha256")
    args=parser.parse_args()
    print(json.dumps(receive_graph(args)))
if __name__=="__main__":
    main()
