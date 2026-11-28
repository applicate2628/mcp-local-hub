import hashlib,json
from pathlib import Path
root=Path(__file__).absolute().parent
rows=[]
for name in ("main_windows.go","README.md"):
    raw=(root/name).read_bytes()
    rows.append({"path":str(root/name),"bytes":len(raw),"lines":len(raw.splitlines()),"sha256":hashlib.sha256(raw).hexdigest()})
print(json.dumps({"files":rows,"data_hash_only":True,"go_compiled":False,"probe_executed":False},indent=2))
