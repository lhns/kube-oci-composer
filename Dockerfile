# Build stage. Dependencies are downloaded in their own layer so a source-only change does not
# re-resolve the module graph.
FROM golang:1.26 AS builder

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
      -o manager ./cmd/oci-composer

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
