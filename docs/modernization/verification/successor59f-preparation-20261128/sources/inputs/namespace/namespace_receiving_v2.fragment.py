# Integration fragment: retain the existing adapter imports and flags_are_literal/native_settled helpers.
# namespace_failed_receiver is byte-identical to v6; only terminal v2 image-copy binding changes.
# No process creation, runtime import, or invocation occurs when this DATA file is prepared.

def validate_namespace_terminal(proof, binding):
    """Classify original terminal data; this function owns no process or files."""
    keys = {"request_sha256", "request_nonce", "binary", "expected_sha256", "source_dir",
            "parent", "args", "controller_argv", "test_state_path_env"}
    if (not isinstance(proof, dict) or not isinstance(binding, dict) or set(binding) != keys
            or any(not isinstance(binding.get(key), str) or not binding[key]
                   for key in ("request_sha256", "request_nonce", "binary", "expected_sha256", "source_dir", "parent"))
            or not re.fullmatch("[0-9a-f]{64}", binding.get("request_sha256", ""))
            or not re.fullmatch("[0-9a-f]{32}", binding.get("request_nonce", ""))
            or not re.fullmatch("[0-9a-f]{64}", binding.get("expected_sha256", ""))
            or type(binding.get("test_state_path_env")) is not bool
            or not isinstance(binding.get("args"), list)
            or any(not isinstance(value, str) for value in binding["args"])
            or not flags_are_literal(binding["args"])
            or not isinstance(binding.get("controller_argv"), list) or not binding["controller_argv"]
            or any(not isinstance(value, str) or not value for value in binding["controller_argv"])):
        raise RuntimeError("namespace terminal request binding is incomplete")
    if proof.get("schema") == "api-namespace-prechild-refusal-v1":
        if (set(proof) != {"schema", "binding", "contained_stage_entered", "failure"}
                or not isinstance(proof.get("binding"), dict)
                or proof.get("binding") != binding
                or type(proof["binding"].get("test_state_path_env")) is not bool
                or proof.get("contained_stage_entered") is not False
                or not isinstance(proof.get("failure"), str) or not proof["failure"]):
            raise RuntimeError("namespace pre-child refusal differs from the original request")
        return "prechild-refusal"
    if (proof.get("schema") != "api-namespace-full-suite-v2"
            or proof.get("mode") != "incoming-go-image-copy"
            or proof.get("binary") != binding["binary"]
            or proof.get("expected_sha256") != binding["expected_sha256"]
            or proof.get("binary_sha256") != binding["expected_sha256"]
            or proof.get("evidence_root") != str(Path(binding["parent"])/"r")
            or proof.get("execution_binary") != str(Path(binding["parent"])/"r"/"bin"/"api.test.exe")
            or proof.get("execution_sha256") != binding["expected_sha256"]
            or proof.get("source_dir") != binding["source_dir"] or proof.get("args") != binding["args"]
            or proof.get("compiled_tags") != (["test_state_path_env"] if binding["test_state_path_env"] else [])
            or not native_settled("namespace", proof)):
        raise RuntimeError("original namespace native/IO settlement remains unverified")
    return "suite"


def namespace_failed_receiver(argv, cwd, proof_path, binding):
    """Freeze the original ns1 request into the current guard's two-argument callback."""
    expected_argv,expected_cwd=[str(value) for value in argv],str(cwd)
    bound=json.loads(json.dumps(binding))
    proof_path=Path(proof_path)
    if (expected_argv!=bound["controller_argv"] or expected_cwd!=bound["source_dir"]
            or proof_path!=Path(bound["parent"])/"namespace-suite-receipt.json"):
        raise ValueError("namespace receiving requires the exact original controller binding")
    metadata_files=[]
    failure,received=None,None
    unread=object()
    original_result,original_bytes=unread,None
    def read_original_result():
        nonlocal failure,original_result,original_bytes
        if failure is not None:
            raise RuntimeError("original namespace reader retains its first error") from failure
        if original_result is not unread:
            return original_result
        try:
            handle=proof_path.open("rb")
            metadata_files.append(handle)
            try:
                original_bytes=handle.read((16<<20)+1)
            finally:
                handle.close()
            if not handle.closed or len(original_bytes)>16<<20:
                raise ValueError("original namespace control read/close is incomplete")
            original_result=json.loads(original_bytes)
            return original_result
        except BaseException as error:
            failure=error
            raise
    def original_result_sha256():
        read_original_result()
        return hashlib.sha256(original_bytes).hexdigest()
    def receive(row,direct_exit_code):
        nonlocal failure,received
        if failure is not None:
            raise RuntimeError("original failed namespace receiver retains its first error") from failure
        if received is not None:
            return received
        try:
            if (row.get("argv")!=expected_argv or row.get("cwd")!=expected_cwd
                    or type(row.get("original_pid")) is not int or row["original_pid"]<=0
                    or type(direct_exit_code) is not int):
                raise ValueError("late namespace receiving differs from its prebound invocation")
            proof=read_original_result()
            terminal=validate_namespace_terminal(proof,bound)
            if terminal=="prechild-refusal" and direct_exit_code==0:
                raise ValueError("original namespace refusal requires its actual nonzero exit")
            if not all(handle.closed for handle in metadata_files):
                raise ValueError("original namespace metadata handle remains open")
            received={"schema":"namespace-original-physical-closure-v1","status":"CLOSED",
                "physical_complete":True,"terminal_kind":terminal,
                "original_controller_pid":row["original_pid"],"original_direct_exit_code":direct_exit_code,
                "original_receipt":{"path":str(proof_path),"sha256":original_result_sha256()},
                "proof":proof,"metadata_handles_closed":True,"qualification_passed":False,
                "capture_access_issued":False,"source_or_cache_access_issued":False,
                "new_commands_permitted":False,"qualification_deadlines_extended":False}
            return received
        except BaseException as error:
            failure=error
            raise
    receive.read_original_result=read_original_result
    receive.original_result_sha256=original_result_sha256
    return receive

