import ast,difflib,hashlib,json,os
from pathlib import Path
root=Path(__file__).absolute().parent
before=(root/"receive_closed_api59f.py").read_bytes()
after=(root/"receive_closed_api59f_v2.py").read_bytes()
assert hashlib.sha256(before).hexdigest()=="08b79031a662be16a90cf80142a6b746808f6891e0655761711540d7d6204cbe"
assert after==before.replace(b'need(record["kind"]=="contained" and path==CASE/"inventory/result.json"',b'need(set(record)=={"receipt","proof","sha256"} and path==CASE/"inventory/result.json"')
ast.parse(after,filename="receive_closed_api59f_v2.py")
patch="".join(difflib.unified_diff(before.decode().splitlines(True),after.decode().splitlines(True),fromfile="receive_closed_api59f.py",tofile="receive_closed_api59f_v2.py")).encode()
with (root/"receiver-v2-vs-v1.patch").open("xb") as stream:
    stream.write(patch);stream.flush();os.fsync(stream.fileno())
print(json.dumps({"receiver":{"bytes":len(after),"lines":len(after.splitlines()),"sha256":hashlib.sha256(after).hexdigest()},"patch":{"bytes":len(patch),"sha256":hashlib.sha256(patch).hexdigest()},"static_ast_only":True,"receiver_executed":False},indent=2))
