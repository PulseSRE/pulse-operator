# Build stage
# Pinned by digest (not just tag) for reproducible builds — a mutable tag can
# change under you between builds with no signal. Dependabot (.github/dependabot.yml,
# docker ecosystem) opens a PR whenever these digests move, so this doesn't go stale.
FROM golang:1.26@sha256:0d1d3a794be25f809dd2cb3160d8c73276c4056a9f8242a138e908ddeee7b6b6 AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o manager cmd/main.go

# Runtime stage
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest@sha256:692953368d8e630f40a3c0a6135163f8824fdafc26e0400b9a6c8d7fac850366
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
