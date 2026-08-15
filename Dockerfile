# Consumes the binary GoReleaser has already built, rather than compiling here.
# That keeps one build path for the release archives and the image, so the
# container ships exactly the binary that was tested and checksummed.
#
# distroless/static:nonroot has no shell, no package manager and no libc: the
# attack surface of a process that drains nodes should be the process itself.
FROM gcr.io/distroless/static:nonroot

COPY binpack /usr/local/bin/binpack

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/binpack"]
CMD ["run"]
