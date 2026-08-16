#!/usr/bin/env python3
"""Check the GitHub workflows are well-formed and mutually consistent.

Two things, both learned the hard way in one sitting.

They must parse. A malformed workflow is not a failing job — it is a run with
*no* jobs, reported as a bare failure with no log to read, which is a
confusing way to discover a stray heredoc.

And release.yaml must be able to run `make check`, which it does on every tag.
It runs only on tags, so a tool missing from it is invisible until somebody
tags, and a tag cannot be taken back. Comparing the setup actions rather than a
hardcoded list keeps this true as the Makefile changes.
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
    missing = setup_actions(build) - setup_actions(release)
    if missing:
        return [
            "release.yaml runs `make check` but does not set up: " + ", ".join(sorted(missing)),
            "a tag would fail at the verify step, before publishing anything",
        ]
    return []


def main() -> int:
    problems = parses() + release_can_verify()
    for problem in problems:
        print(problem)
    if problems:
        return 1
    print("workflows parse, and the release job provisions what the build job does")
    return 0


if __name__ == "__main__":
    sys.exit(main())
