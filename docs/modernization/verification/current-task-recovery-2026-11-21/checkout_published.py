"""Read the exact published child commit and select it locally; no branch integration.

Requires publication-input.json with schema=1, base, commit, tree, parents,
checkout_runner_sha256, execution_runner_sha256 and source_manifest_sha256.
This script is preparation only until separately invoked in the approved root.
Its fetch is an explicit read from the fixed repository, never a GitHub write.
"""
import sys
sys.dont_write_bytecode = True

import json
from pathlib import Path
import re

from check_recovery import BASE, ROOT, SOURCE, Runner, sha

REMOTE = "https://github.com/applicate2628/mcp-local-hub.git"


def select_published(runner):
    raw = (ROOT / "publication-input.json").read_bytes()
    publication = json.loads(raw)
    commit, tree = publication.get("commit"), publication.get("tree")
    if publication.get("schema") != 1 or publication.get("base") != BASE or publication.get("parents") != [BASE] or not all(isinstance(v, str) and re.fullmatch(r"[0-9a-f]{40}", v) for v in (commit, tree)):
        raise RuntimeError("Unexpected publication ancestry or object identities")
    wrapper_sha = sha(Path(__file__).read_bytes())
    if publication.get("checkout_runner_sha256") != wrapper_sha or publication.get("execution_runner_sha256") != runner.receipt["runner_sha256"] or publication.get("source_manifest_sha256") != runner.receipt["manifest_sha256"]:
        raise RuntimeError("Publication runner or source-manifest pin mismatch")
    runner.receipt.update(
        checkout_runner_sha256=wrapper_sha, publication_input_sha256=sha(raw),
        expected_commit=commit, expected_tree=tree, remote_read_url=REMOTE,
    )
    runner.receipt["source_before"] = runner.admit("candidate")
    runner.git_run("stage-exact-files", ["add", "--", *runner.paths])
    if runner.git_at(SOURCE, "write-tree") != tree:
        raise RuntimeError("Staged candidate tree differs from the published tree")
    runner.git_run("fetch-exact-commit", ["fetch", "--no-tags", REMOTE, commit])
    if runner.git_at(SOURCE, "show", "-s", "--format=%P", commit).split() != [BASE] or runner.git_at(SOURCE, "rev-parse", commit + "^{tree}") != tree:
        raise RuntimeError("Fetched publication has unexpected ancestry or tree")
    if set(runner.git_at(SOURCE, "diff", "--name-only", BASE, commit).splitlines()) != set(runner.paths):
        raise RuntimeError("Published commit changes unexpected paths")
    runner.git_run("checkout-detached", ["checkout", "--detach", commit])
    after = runner.admit("published", commit)
    if after["tree"] != tree:
        raise RuntimeError("Published checkout tree differs")
    runner.receipt.update(
        source_after=after, source_head=after["head"], source_tree=after["tree"],
        working_status=after["status"], completed=True, state="complete",
    )


def main():
    if len(sys.argv) != 1:
        raise SystemExit("No arguments; use the reviewed publication-input.json")
    runner = Runner("published-checkout")
    failure = None
    try:
        runner.begin()
        select_published(runner)
    except BaseException as error:
        failure = error
    finally:
        runner.finish(failure)
    if failure is not None or not runner.receipt["completed"]:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
