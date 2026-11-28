"""Reconstruct three complete closed execution JSON files as DATA.

The first renderer must already have reconstructed the preserved plans and
projections. This helper restores only repeated input maps and checks each
complete original byte count and SHA256 before creating any output.
"""
import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath

def require(ok,message):
    if not ok: raise ValueError(message)
def sha(raw): return hashlib.sha256(raw).hexdigest()
def relative(value):
    path=PurePosixPath(value)
    require(isinstance(value,str) and value and "\\" not in value and ":" not in value
        and not path.is_absolute() and ".." not in path.parts and "." not in path.parts
        and str(path)==value,"plain relative role path required")
    return Path(*path.parts)
def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--data-root",required=True,type=Path)
    parser.add_argument("--output",required=True,type=Path)
    parser.add_argument("--index-sha256",required=True)
    args=parser.parse_args()
    capsule=Path(__file__).resolve().parent
    raw=(capsule/"execution-reconstruction-index.json").read_bytes()
    require(sha(raw)==args.index_sha256,"reconstruction index changed")
    index=json.loads(raw)
    require(index["schema"]=="closed-execution-data-reconstruction-index-v1"
        and len(index["projections"])==3,"fixed three closed preparation phases required")
    prepared=[];seen=set()
    for item in index["projections"]:
        projection_raw=(args.data_root/relative(item["projection_role_path"])).read_bytes()
        require(sha(projection_raw)==item["projection_sha256"],"original projection bytes differ")
        projection=json.loads(projection_raw)
        require(projection["schema"]=="closed-execution-plan-map-projection-v1"
            and projection["original_execution"]==item["original_execution"]
            and projection["original_plan"]==item["original_plan"]
            and projection["serialization"]=={"ensure_ascii":False,"indent":2,"trailing_newline":True},
            "original reconstruction contract differs")
        plan_raw=(args.data_root/relative(item["original_plan"]["role_path"])).read_bytes()
        require(sha(plan_raw)==item["original_plan"]["sha256"],"original plan bytes differ")
        inputs=json.loads(plan_raw)["input_files"]
        require(len(inputs)==projection["input_map_entries"],"original map count differs")
        execution=projection["execution"]
        for step in execution["step_inputs"]:
            for key in ("before","after"):
                require(step[key]=={"preserved_input_map":"original_plan.input_files"},
                    "unexpected omitted content")
                step[key]=inputs
        rebuilt=(json.dumps(execution,ensure_ascii=False,indent=2)+"\n").encode("utf-8")
        expected=item["original_execution"]
        require(len(rebuilt)==expected["bytes"] and sha(rebuilt)==expected["sha256"],
            "complete original execution bytes were not reconstructed")
        destination=relative(expected["role_path"])
        require(destination not in seen,"duplicate execution destination")
        seen.add(destination);prepared.append((destination,rebuilt,expected))
    require(not args.output.exists() and not args.output.is_symlink(),"new absent DATA output required")
    args.output.mkdir(parents=False,exist_ok=False)
    results=[]
    for relative_path,rebuilt,expected in prepared:
        path=args.output/relative_path
        path.parent.mkdir(parents=True,exist_ok=True)
        with path.open("xb") as stream: stream.write(rebuilt)
        require(path.read_bytes()==rebuilt,"written execution bytes differ")
        results.append(expected)
    receipt={"schema":"complete-closed-execution-data-reconstruction-v1","status":"EXACT_DATA_RECONSTRUCTED",
        "executions":results,"sources_imported_or_executed":False,"native_authority_acquired":False,
        "gate_statuses_reclassified":False,"qualification_inherited":False}
    with (args.output/"reconstruction-receipt.json").open("xb") as stream:
        stream.write((json.dumps(receipt,indent=2)+"\n").encode("utf-8"))
    print(json.dumps({"status":receipt["status"],"executions":len(results),"sources_executed":False}))
if __name__=="__main__":
    main()
