# Integration fragment: requires validate_namespace_terminal from namespace_receiving_v2.fragment.py.
# Physical closure and qualification remain separate; known FAIL stays FAIL.
def namespace_qualification(proof, binding):
    if validate_namespace_terminal(proof, binding) != "suite":
        return False
    required = (
        "TestSecretsInit_RecoveryNamespaceBoundary",
        "TestSecretsInit_RecoveryNamespaceRetainedDirectory",
        "TestSecretsInit_RecoveryNamespaceFailurePaths",
        "TestSupervisorPeerStrictControlConnections",
        "TestRotateSupervisorStderrSinkIfOversize_AtBoundary",
        "TestRotateSupervisorStderrSinkIfOversize_ReplacesExistingBackup",
        "TestSupervisorStderrSinkOversize_MatchesRotationThreshold",
    )
    mandatory = proof.get("mandatory_tests", {})
    return (
        proof.get("root_exit_code") == 0
        and proof.get("observations_verified") is True
        and proof.get("mandatory_tests_passed") is True
        and proof.get("binary_unchanged") is True
        and proof.get("execution_binary_unchanged") is True
        and set(mandatory) == set(required)
        and all(mandatory[name] == {"Runs": 1, "Passes": 1, "Fails": 0, "Skips": 0}
                for name in required)
        and not proof.get("failure")
        and not proof.get("cleanup_error")
        and not proof.get("native_error")
    )
