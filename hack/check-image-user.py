#!/usr/bin/env python3
"""Assert the image declares a numeric user, and the chart agrees.

A kubelet enforcing runAsNonRoot decides whether the image's user is root
before starting the container, and it cannot resolve a name against an image it
has not run. So `USER nonroot` — which distroless offers and which reads
perfectly well — fails at runtime with:

    container has runAsNonRoot and image has non-numeric user (nonroot),
    cannot verify user is non-root

Nothing else catches this. The image builds, `helm lint` passes, the manifests
validate against the API schema, and the pod fails only once a kubelet with
that guard tries to start it.
"""

import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent


def image_user() -> str | None:
    for line in (ROOT / "Dockerfile").read_text().splitlines():
        if line.startswith("USER "):
            return line.removeprefix("USER ").strip()
    return None


def main() -> int:
    problems = []

    # Numeric *and* non-zero, which are two requirements and not one. A named
    # user cannot be verified; uid 0 verifies fine and is root. Checking only
    # the first would be a check reporting a property it never established.
    user = image_user()
    if user is None:
        problems.append("Dockerfile sets no USER, so the image runs as root")
    elif not re.fullmatch(r"\d+(:\d+)?", user):
        problems.append(
            f"Dockerfile sets USER {user!r}, which a kubelet cannot verify against "
            "runAsNonRoot; use the numeric uid"
        )
    elif int(user.split(":")[0]) == 0:
        problems.append(f"Dockerfile sets USER {user!r}, which is root")

    values = (ROOT / "charts/binpack/values.yaml").read_text()
    if "runAsNonRoot: true" in values:
        match = re.search(r"^\s+runAsUser: (\d+)", values, re.M)
        if match is None:
            problems.append(
                "the chart sets runAsNonRoot without a numeric runAsUser, so it depends "
                "entirely on the image declaring one"
            )
        elif int(match.group(1)) == 0:
            problems.append("the chart sets runAsUser: 0, which is root and contradicts runAsNonRoot")

    for problem in problems:
        print(problem)
    if problems:
        return 1

    print(f"the image declares USER {user}, and the chart sets a non-root numeric runAsUser")
    return 0


if __name__ == "__main__":
    sys.exit(main())
