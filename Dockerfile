# Consumes the binary GoReleaser has already built, rather than compiling here.
# That keeps one build path for the release archives and the image, so the
# container ships exactly the binary that was tested and checksummed.
#
# distroless/static:nonroot has no shell, no package manager and no libc: the
# attack surface of a process that drains nodes should be the process itself.
FROM gcr.io/distroless/static:nonroot

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
