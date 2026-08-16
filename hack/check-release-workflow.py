#!/usr/bin/env python3
"""Assert the release workflow can actually run `make check`.

Both workflows run it, but release.yaml runs only on tags — so a tool missing
from it is invisible until somebody tags, and a tag cannot be taken back. This
is the check that turns that into a pull-request failure.

It compares the setup actions rather than the commands: what `make check` needs
is whatever the build job installs before running it, and that list changes as
the Makefile does.
"""

import pathlib
import re
import sys


def setup_actions(text: str) -> set[str]:
    return {a.split("@")[0] for a in re.findall(r"uses: ([\w\-./]+@v[\w.]+)", text)}


def main() -> int:
    root = pathlib.Path(__file__).resolve().parent.parent
    ci = (root / ".github/workflows/ci.yaml").read_text()
    release = (root / ".github/workflows/release.yaml").read_text()

    if "make check" not in release:
        print("release.yaml no longer runs `make check`; this check needs rethinking")
        return 1

    # The build job is everything before the next job in the file.
    build = ci.split("\n  chart:")[0]
    missing = setup_actions(build) - setup_actions(release)
    if missing:
        print("release.yaml runs `make check` but does not set up:", ", ".join(sorted(missing)))
        print("a tag would fail at the verify step, before publishing anything")
        return 1

    print("the release workflow provisions everything the build job does")
    return 0


if __name__ == "__main__":
    sys.exit(main())
