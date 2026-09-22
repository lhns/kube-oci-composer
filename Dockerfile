# Build stage. Dependencies are downloaded in their own layer so a source-only change does not
# re-resolve the module graph.
# Pinned by digest, and CI fails if its Go minor differs from go.mod's: the Go that builds the RELEASE
# is an input to every composed artifact's bytes (ADR 0057), and a floating tag once moved it
# without CI, which used go.mod's, noticing. Dependabot proposes patch and digest bumps.
FROM golang:1.27.1@sha256:3680233e3204827fbdc66088528ae6d4b3d034f51d03a99d454f6de034888244 AS builder

# CMD selects which binary this image carries. The two are built from one Dockerfile because they
# share every layer up to the compile step; ADR 0004 wants two DEPLOYMENTS, which is about RBAC and
# blast radius, not about duplicating a build recipe.
ARG CMD=oci-composer
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# CGO off so the result is a static binary that runs on a distroless base.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}" \
      -o manager ./cmd/${CMD}

# Runtime stage.
#
# Distroless nonroot: no shell, no package manager, no libc to patch. This component sits in the
# supply chain, so the smaller its attack surface the better — and nothing in the binary needs a
# shell. The blob store and cache directories are the only writable paths it wants, and both are
# supplied as volumes, so the root filesystem can stay read-only.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /
COPY --from=builder /workspace/manager .

USER 65532:65532

ENTRYPOINT ["/manager"]
