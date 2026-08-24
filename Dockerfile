# Consumes the binary GoReleaser has already built, rather than compiling here.
# That keeps one build path for the release archives and the image, so the
# container ships exactly the binary that was tested and checksummed.
#
# distroless/static:nonroot has no shell, no package manager and no libc: the
# attack surface of a process that drains nodes should be the process itself.
#
# Pinned by digest with the tag kept beside it, and both halves are load-bearing.
# The digest is what makes a release rebuildable from this tree and what stops a
# rebuilt or rotated `nonroot` entering a build with no diff; the tag is what
# dependabot resolves, and what tells a reader what was pinned. A pin alone
# would freeze the base — this image is not empty, it carries a CA bundle and
# tzdata — which is why the `docker` ecosystem entry in .github/dependabot.yml
# went in with it rather than after it.
#
# The digest is the *index*, not a platform manifest: buildx selects the
# per-platform image from it. Pinning one platform's manifest instead does not
# fail — `docker buildx build --platform linux/arm64` against the amd64
# manifest emits `InvalidBaseImagePlatform` as a *warning* and produces an
# arm64 image on an amd64 base, which is a broken release nobody is told about.
# `docker buildx imagetools inspect gcr.io/distroless/static:nonroot` prints
# the index digest first, above the per-platform ones.
FROM gcr.io/distroless/static:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7

# dockers_v2 puts every platform's binary in one build context, under a
# directory named for the platform, and buildx sets TARGETPLATFORM per build.
# A bare `COPY binpack` worked with the old single-arch `dockers` block and
# fails here with "/binpack: not found", which is what a multi-platform context
# looks like when the Dockerfile still assumes a single-arch one.
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/binpack /usr/local/bin/binpack

# Numeric, not the `nonroot` name distroless also provides. A kubelet enforcing
# runAsNonRoot has to decide whether the image's user is root *before* starting
# it, and it cannot resolve a name against an image it has not run — so a named
# user fails with "has non-numeric user (nonroot), cannot verify user is
# non-root" on any cluster with that guard on. 65532 is what `nonroot` is.
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/binpack"]
CMD ["run"]
