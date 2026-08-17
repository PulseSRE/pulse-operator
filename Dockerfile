# Build stage
# Pinned by digest (not just tag) for reproducible builds — a mutable tag can
# change under you between builds with no signal. Dependabot (.github/dependabot.yml,
# docker ecosystem) opens a PR whenever these digests move, so this doesn't go stale.
# Uses the Red Hat UBI9 go-toolset (1.26, ships Go 1.26.5) instead of the Docker Hub
# golang image, to match the UBI-based runtime stage below. It doesn't need to be an
# exact match for go.mod's `go 1.26.0` / `toolchain go1.26.6` — with GOTOOLCHAIN=auto
# (see below), `go mod download`/`go build` transparently fetch and use go1.26.6
# themselves regardless of the Go version baked into this base image. This image's
# default WORKDIR is /opt/app-root/src, owned by its non-root default user (uid 1001),
# so we build there instead of /workspace.
FROM registry.access.redhat.com/ubi9/go-toolset:1.26@sha256:1a9bbbfa854931a97dbff276bd69dc0e32b36cb2fbce3b9813b2cf9892aa8d43 AS builder
WORKDIR /opt/app-root/src
# Unlike Docker Hub's golang images, Red Hat's go-toolset builds Go with
# GOTOOLCHAIN defaulting to "local" instead of upstream's "auto". Without this,
# go mod download/go build would silently keep using the image's bundled
# go1.26.5 instead of picking up go.mod's `toolchain go1.26.6`. Setting it back
# to "auto" restores the standard behavior of downloading/using the toolchain
# version pinned in go.mod.
ENV GOTOOLCHAIN=auto
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o manager cmd/main.go

# Runtime stage
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest@sha256:692953368d8e630f40a3c0a6135163f8824fdafc26e0400b9a6c8d7fac850366
WORKDIR /
COPY --from=builder /opt/app-root/src/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
