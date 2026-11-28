def owner_proof(kind, proof, binary, digest, cwd, args, tagged, binding=None):
    if kind == "foreground":
        return foreground_qualification(proof, binding)
    if kind == "namespace":
        return namespace_qualification(proof, binding)
    if kind == "job":
        required = {*part.JOB_TESTS, part.JOB_TESTS[1]+"/real-native-typed-gc-and-creator"}
        return (proof.get("fullSuite") is True and proof.get("success") is True
                and proof.get("originalWaitKnown") is True and proof.get("originalExitCode")==0
                and proof.get("settlementComplete") is True and not proof.get("custodyRetained")
                and proof.get("streamsJoined") is True and proof.get("controlWriterJoined") is True
                and proof.get("stdoutEOF") is True and proof.get("stderrEOF") is True
                and proof.get("binaryUnchanged") is True and set(proof.get("passed",[]))==required)
    return (kind == "contained" and proof.get("schema")=="literal-contained-test-run-v1"
            and proof.get("binary")==str(binary) and proof.get("binary_sha256")==digest
            and proof.get("source_dir")==str(cwd) and proof.get("args")==args
            and proof.get("root_wait_known") is True and proof.get("root_exit_code")==0
            and proof.get("settlement_complete") is True and proof.get("custody_retained") is False
            and proof.get("binary_unchanged") is True and proof.get("binary_after_measured") is True
            and not proof.get("failure"))

def native_settled(kind, proof):
    if kind == "job":
        return (proof.get("originalWaitKnown") is True and proof.get("settlementComplete") is True
                and proof.get("streamsJoined") is True and proof.get("controlWriterJoined") is True
                and proof.get("stdoutEOF") is True and proof.get("stderrEOF") is True
                and proof.get("custodyRetained",False) is False)
    return (kind in ("namespace", "contained")
            and proof.get("root_wait_known") is True and proof.get("settlement_complete") is True
            and proof.get("custody_retained") is False
            and (kind!="namespace" or (proof.get("native_complete") is True and proof.get("io_complete") is True)))

def finalize_owner_capture(run, command, kind, proof_path, receipt_key, binary, digest, cwd, args, tagged, *, owner_binding=None, owner_receiver=None):
    def receive_completion(row):
        if kind in ("namespace", "foreground"):
            if owner_receiver is None:
                raise RuntimeError("typed owner receiving requires its prebound original reader")
            proof=owner_receiver.read_original_result()
        else:
            proof=json.loads(proof_path.read_text(encoding="utf-8"))
        record={"receipt":str(proof_path),"proof":proof}
        if receipt_key=="owner":
            record["kind"]=kind
        run.receipt[receipt_key]=record
        if kind not in ("namespace", "foreground") and not native_settled(kind, proof):
            raise RuntimeError("original native or IO settlement is unverified; captures retained unmeasured")
        if kind=="job":
            packet_path=proof_path.parent/"packet.json"
            packet=json.loads(packet_path.read_text(encoding="utf-8"))
            bound=(proof.get("schema")=="job-fixture-controller-result-v1" and proof.get("fullSuite") is True
                and packet.get("schema")=="job-fixture-controller-packet-v1" and packet.get("fullSuite") is True
                and packet.get("args")==args and packet.get("binary")==str(binary)
                and packet.get("sourceDir")==str(cwd) and packet.get("binarySha256")==digest
                and sha(packet_path)==proof.get("bind",{}).get("packet"))
        elif kind=="namespace":
            terminal=validate_namespace_terminal(proof,owner_binding)
            if terminal=="prechild-refusal":
                if type(row.get("exit_code")) is not int or row["exit_code"]==0:
                    raise RuntimeError("namespace admission refusal requires its original nonzero exit")
                run.receipt["namespace_prechild_refusal"]=True
            bound=True
        elif kind=="foreground":
            terminal=validate_foreground_terminal(proof,owner_binding)
            if terminal=="no-child":
                if type(row.get("exit_code")) is not int or row["exit_code"]==0:
                    raise RuntimeError("foreground creation failure requires its original nonzero exit")
                run.receipt["foreground_no_child"]=True
            bound=True
        elif kind=="contained":
            bound=(proof.get("schema")=="literal-contained-test-run-v1"
                and proof.get("binary")==str(binary) and proof.get("binary_sha256")==digest
                and proof.get("source_dir")==str(cwd) and proof.get("args")==args
                and proof.get("inventory") is (receipt_key=="inventory_owner"))
        else:
            raise RuntimeError("unknown original owner kind")
        if not bound:
            raise RuntimeError("original owner receipt does not bind this exact invocation; captures retained unmeasured")
        run.receipt["native_custody_settled"]=True
        return proof
    run.finalize_deferred(command,receive_completion,decode=False)
    run.receipt[receipt_key]["sha256"]=(owner_receiver.original_result_sha256()
                                       if kind in ("namespace", "foreground") else sha(proof_path))
    return command["completion_receiving"]
