# deploy/Dockerfile — the Wasi server image (wasi-server-plan §14: distroless,
# static, non-root, read-only rootfs except /data).
#
# Build from the repo root: `docker build -f deploy/Dockerfile .` (this is
# exactly what deploy/compose.dev.yml's `wasi` service does). Only cmd/wasi
# needs to build for this image to be useful — `serve` and the other
# subcommands are filled in by later waves (implementation-plan.md), so right
# now the container starts and exits with "not implemented yet"; that's
# expected until Wave 2B/3B land, not a bug in this Dockerfile.

FROM golang:1.26-alpine AS build
WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO_ENABLED=0 for a fully static binary — distroless/static has no libc to
# link against, and a static binary is also what makes the image portable
# across the arm64/amd64 split without per-arch libc concerns.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wasi ./cmd/wasi

# An empty /data owned by the runtime uid, staged here so the next stage can
# COPY it with that ownership. See the VOLUME comment below for why this
# matters — distroless has no shell, so a RUN mkdir/chown in the final stage
# is not available.
RUN mkdir -p /out/data

# distroless/static-debian12:nonroot: no shell, no package manager, nothing
# but the binary's own dynamic-linker-free runtime needs — and it already
# runs as a non-root, unprivileged user (65532:65532).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/wasi /usr/local/bin/wasi

# /data is the only writable path at runtime (§3, §14): ayllu.toml,
# state.json, kipu/, guardians.toml all live there. /config (wasi.toml,
# TLS material) is bind-mounted read-only by the operator; everything else
# in the image is whatever distroless/static ships, which Wasi never writes
# to. The read-only-rootfs enforcement itself is a deploy-time flag
# (`docker run --read-only` / compose's `read_only: true`), not something a
# Dockerfile can declare — deploy/compose.dev.yml sets it there.
#
# /data must exist in the image, owned by the runtime uid, BEFORE the VOLUME
# declaration. Docker seeds a fresh named volume from the image's contents at
# that path, ownership included; with no such path in the image the volume is
# created root-owned and the non-root process cannot write to it — Wasi fails
# at startup with "permission denied" creating its first atomic temp file.
# Running as root would "fix" it by giving up §14.
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME ["/data"]

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/wasi"]
CMD ["serve"]
