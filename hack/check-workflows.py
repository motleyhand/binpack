#!/usr/bin/env python3
"""Check the GitHub workflows are well-formed and mutually consistent.

Four things. The first two were learned the hard way in one sitting.

They must parse. A malformed workflow is not a failing job — it is a run with
*no* jobs, reported as a bare failure with no log to read, which is a
confusing way to discover a stray heredoc.

And release.yaml must be able to run `make check`, which it does on every tag.
It runs only on tags, so a tool missing from it is invisible until somebody
tags, and a tag cannot be taken back. Comparing the setup actions rather than a
hardcoded list keeps this true as the Makefile changes.

The two later ones close holes in the first two rather than adding a new
subject. Comparing setup actions with the version discarded says nothing about
whether they *agree*, and release.yaml's own comment declares that they must.
And a permission granted at workflow scope is held by every job in the file,
including the ones that never use it — which is a property of where the block
is written, not of what any job does, so it can only be checked here.

Only the two-workflow half of the linter pin is here. The Makefile and
CONTRIBUTING.md name it too, and internal/cli's TestTheLinterVersionAgreesEverywhere
holds all four together; this script cannot reach outside .github/workflows
without becoming something else.
"""

import pathlib
import re
import shutil
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
WORKFLOWS = ROOT / ".github/workflows"


def setup_actions(text: str) -> set[str]:
    return {a.split("@")[0] for a in re.findall(r"uses: ([\w\-./]+@v[\w.]+)", text)}


# The action and the version it installs, from the block that installs it.
# Both matter and they are different numbers: `@v9` is the action, `v2.12.2` is
# the linter, and only the second decides which rules run.
LINTER = re.compile(
    r"uses: (golangci/golangci-lint-action@[\w.]+)\s*\n\s*with:\s*\n\s*version: (v[\d.]+)"
)


def linters_agree() -> list[str]:
    """The two workflows install the same linter.

    release.yaml states this three lines above the step it governs — "a release
    that fails on a lint rule the pull request never ran is a release blocked by
    a disagreement between two workflows" — and nothing enforced it. Probed
    before this existed: ci.yaml at v2.13.0 against release.yaml at v2.12.2 exits
    0, and @v10 against @v9 exits 0 too, because setup_actions() splits the
    version off and throws it away. Only deleting the step outright failed.

    Dependabot cannot introduce the drift: it updates `uses:` refs, not `with:`
    inputs, and it updates both files in one pull request. A human editing one
    file can, and that is the whole trigger.
    """
    pins = {}
    for name in ("ci.yaml", "release.yaml"):
        match = LINTER.search((WORKFLOWS / name).read_text())
        pins[name] = match.groups() if match else None

    missing = [name for name, pin in pins.items() if pin is None]
    if missing:
        return [
            f"{', '.join(sorted(missing))} does not install golangci-lint with a pinned "
            "version; both workflows run the same lint and must install the same linter"
        ]

    if pins["ci.yaml"] != pins["release.yaml"]:
        return [
            "the two workflows install different linters: "
            f"ci.yaml {' '.join(pins['ci.yaml'])}, release.yaml {' '.join(pins['release.yaml'])}",
            "a tag would then fail on a rule the pull request never ran",
        ]
    return []


# A `permissions:` block at column 0 — workflow scope. Anything indented
# belongs to a job and is out of scope for this check by construction.
WORKFLOW_SCOPE_PERMISSIONS = re.compile(
    r"(?m)^permissions:[ \t]*(?:(?P<inline>\S.*)|\n(?P<block>(?:[ \t]+\S.*\n|[ \t]*\n)*))"
)


def permissions_are_scoped() -> list[str]:
    """No workflow grants a write to every job it happens to contain.

    A workflow-scope block is inherited, so the token handed to a job is
    decided by where the block is written rather than by what the job does.
    release.yaml's `chart` job held `contents: write` that way — it checks out,
    packages a chart and pushes it to a registry, and never writes to the
    repository at all.

    The narrowing is smaller than it looks and worth stating honestly: `chart`
    is `needs: release`, and `release` legitimately holds `contents: write`
    while running seven third-party actions. What per-job scoping removes is
    the marginal surface of the one action that runs only in `chart`.

    Every workflow must also declare permissions *somewhere*, so that deleting
    the block is a failure rather than a silent fall back to whatever the
    repository default happens to be.
    """
    problems = []
    for path in sorted(WORKFLOWS.glob("*.yaml")):
        text = path.read_text()
        if not re.search(r"(?m)^\s*permissions:", text):
            problems.append(
                f"{path.name} declares no permissions, so its token is whatever the "
                "repository default is rather than what the workflow needs"
            )
            continue

        match = WORKFLOW_SCOPE_PERMISSIONS.search(text)
        if match and "write" in (match.group("inline") or match.group("block") or ""):
            problems.append(
                f"{path.name} grants a write permission at workflow scope, which every "
                "job in it inherits; declare it on the job that needs it"
            )
    return problems


def parses() -> list[str]:
    """Every workflow parses, and defines the jobs it means to.

    Tries PyYAML, then ruby, and fails if it finds neither rather than
    reporting success it did not establish. A check that quietly skips itself
    is worse than no check: it is a green tick for work that never happened.
    """
    paths = sorted(WORKFLOWS.glob("*.yaml"))

    try:
        import yaml
    except ImportError:
        pass
    else:
        problems = []
        for path in paths:
            try:
                document = yaml.safe_load(path.read_text())
            except yaml.YAMLError as err:
                problems.append(f"{path.name} does not parse: {err}")
                continue
            if not document.get("jobs"):
                problems.append(f"{path.name} defines no jobs")
        return problems

    # Ruby ships with macOS and with the GitHub runners, and its YAML is the
    # same libyaml GitHub Actions itself parses with.
    if shutil.which("ruby"):
        problems = []
        for path in paths:
            script = (
                f'require "yaml"; d = YAML.load_file({str(path)!r}); '
                'abort("no jobs") if d["jobs"].nil? || d["jobs"].empty?'
            )
            result = subprocess.run(
                ["ruby", "-e", script], capture_output=True, text=True, check=False
            )
            if result.returncode != 0:
                detail = (result.stderr or result.stdout).strip().splitlines()
                problems.append(f"{path.name} does not parse: {detail[0] if detail else 'unknown'}")
        return problems

    return ["no YAML parser available (install PyYAML or ruby); refusing to report a check that did not run"]


def release_can_verify() -> list[str]:
    ci = (WORKFLOWS / "ci.yaml").read_text()
    release = (WORKFLOWS / "release.yaml").read_text()

    # The command, not the words: "make check" appears in the comments around
    # it too, so a substring search reports success for a workflow that only
    # talks about running it.
    if not re.search(r"^\s*run: make check\s*$", release, re.MULTILINE):
        return ["release.yaml no longer runs `make check`; this check needs rethinking"]

    # Everything the build job installs before running the same command.
    #
    # Named explicitly rather than taken as "the first job": `on:` has keys at
    # the same indent as a job, so splitting on the shape of a job header finds
    # `push:` first and silently compares the wrong block — which it did, and
    # the check went green while missing the very thing it was written for.
    if "\n  build:\n" not in ci:
        return ["ci.yaml has no build job; this check needs rethinking"]
    after_build = ci.split("\n  build:\n", 1)[1]
    build = re.split(r"\n  [a-z-]+:\n", after_build, maxsplit=1)[0]

    # The job that runs `make check`, not the whole file. release.yaml also has
    # a chart job, which sets up helm and runs on its own runner — so taking
    # the union let one job's tools stand in for another's, and the moment the
    # tests started needing helm this check went green against a release that
    # would have failed at its verify step.
    if "\n  release:\n" not in release:
        return ["release.yaml has no release job; this check needs rethinking"]
    after_release = release.split("\n  release:\n", 1)[1]
    job = re.split(r"\n  [a-z-]+:\n", after_release, maxsplit=1)[0]

    # Only the steps that run *before* the verification, because a setup step
    # is available to a command only if it has already run. Compared against
    # the whole job, moving `azure/setup-helm` below `run: make check` left
    # this green and the tag failing at its verify step — the same masking as
    # the job-vs-file one above, one level down.
    verify = re.search(r"^\s*run: make check\s*$", job, re.MULTILINE)
    if not verify:
        return ["release.yaml runs `make check` outside its release job; this check "
                "compares the wrong block and needs rethinking"]
    before = job[: verify.start()]

    missing = setup_actions(build) - setup_actions(before)
    if missing:
        return [
            "release.yaml's release job runs `make check` but does not set up: "
            + ", ".join(sorted(missing)),
            "a tag would fail at the verify step, before publishing anything",
        ]
    return []


def main() -> int:
    problems = parses() + release_can_verify() + linters_agree() + permissions_are_scoped()
    for problem in problems:
        print(problem)
    if problems:
        return 1
    print(
        "workflows parse, the release job provisions what the build job does, "
        "both install the same linter, and no write is granted workflow-wide"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
