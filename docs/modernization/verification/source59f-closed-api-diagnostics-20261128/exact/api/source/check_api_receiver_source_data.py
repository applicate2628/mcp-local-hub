import ast,hashlib,json
from pathlib import Path
root=Path(__file__).absolute().parent
path=root/"receive_closed_api59f.py"
raw=path.read_bytes()
ast.parse(raw,filename=str(path))
print(json.dumps({"path":str(path),"bytes":len(raw),"lines":len(raw.splitlines()),"sha256":hashlib.sha256(raw).hexdigest(),"static_ast_only":True,"receiver_executed":False},indent=2))
