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

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/binpack"]
CMD ["run"]
