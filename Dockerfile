# Build stage
# Pinned by digest (not just tag) for reproducible builds — a mutable tag can
# change under you between builds with no signal. Dependabot (.github/dependabot.yml,
# docker ecosystem) opens a PR whenever these digests move, so this doesn't go stale.
# Uses the Red Hat UBI9 go-toolset (1.26) instead of the Docker Hub
# golang image, to match the UBI-based runtime stage below. It doesn't need to be an
# exact match for go.mod's minimum `go 1.26.11` — with GOTOOLCHAIN=auto
# (see below), `go mod download`/`go build` transparently fetch and use go1.26.11
# themselves regardless of the Go version baked into this base image. This image's
# default WORKDIR is /opt/app-root/src, owned by its non-root default user (uid 1001),
# so we build there instead of /workspace.
FROM registry.access.redhat.com/ubi9/go-toolset:1.27@sha256:4053e919374d739ee6b0007e412d89256b0bd065e0bc690c2b3905bbaab6500e AS builder
WORKDIR /opt/app-root/src
# Unlike Docker Hub's golang images, Red Hat's go-toolset builds Go with
# GOTOOLCHAIN defaulting to "local" instead of upstream's "auto". Without this,
# go mod download/go build would fail when the bundled Go is older than
# go.mod's minimum `go 1.26.11`. Setting it back
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
# ubi-micro, not ubi-minimal: the manager is a static binary (CGO_ENABLED=0)
# that needs no shared libraries, and it talks only to the Kubernetes API, whose
# CA comes from the service account, so it needs no system CA bundle either.
# ubi-minimal carried packages the manager never loads (krb5-libs, sqlite-libs,
# microdnf's own stack), and their CVEs failed container-scan with no fix
# available in the UBI repos (krb5-libs, 14 HIGH, 2026-11). ubi-micro has no
# package manager, so fixes arrive when Dependabot moves this digest.
FROM registry.access.redhat.com/ubi9/ubi-micro:latest@sha256:dbd325b96bcd432dd42816d31412bdf1dc0fb973b79ed310344bc23c76c068db
WORKDIR /
COPY --from=builder /opt/app-root/src/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
