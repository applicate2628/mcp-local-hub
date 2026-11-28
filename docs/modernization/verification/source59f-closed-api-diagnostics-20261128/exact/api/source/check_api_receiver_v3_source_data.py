import ast,difflib,hashlib,json,os
from pathlib import Path
root=Path(__file__).absolute().parent
before=(root/"receive_closed_api59f_v2.py").read_bytes()
after=(root/"receive_closed_api59f_v3.py").read_bytes()
assert hashlib.sha256(before).hexdigest()=="9ffd6631bfd2d6cb274634906ad754c3957a8f9e770e593b0c7395a4bd6bdf9f"
ast.parse(after,filename="receive_closed_api59f_v3.py")
patch="".join(difflib.unified_diff(before.decode().splitlines(True),after.decode().splitlines(True),fromfile="receive_closed_api59f_v2.py",tofile="receive_closed_api59f_v3.py")).encode()
with (root/"receiver-v3-vs-v2.patch").open("xb") as stream:
    stream.write(patch);stream.flush();os.fsync(stream.fileno())
print(json.dumps({"receiver":{"bytes":len(after),"lines":len(after.splitlines()),"sha256":hashlib.sha256(after).hexdigest()},"patch":{"bytes":len(patch),"sha256":hashlib.sha256(patch).hexdigest()},"static_ast_only":True,"receiver_executed":False},indent=2))
