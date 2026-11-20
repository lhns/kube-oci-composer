# The compiler runs natively ($BUILDPLATFORM) and cross-compiles, so no QEMU is needed (the
# runtime stage has no RUN). The golang tag is go.mod's toolchain version; CI fails on a minor
# mismatch, and the local setup-go action makes CI test with this exact Go.
FROM --platform=$BUILDPLATFORM golang:1.27.4@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS builder

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

# No defaults: a default overrides the per-platform value buildx sets, which once built every
# platform for amd64. The :? guard fails a build that sets none (no BuildKit).
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
# CMD selects which binary this image carries: oci-composer or oci-builder, one Dockerfile for both.
ARG CMD=oci-composer

RUN : "${TARGETOS:?}" "${TARGETARCH:?}" && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}" \
      -o /workspace/manager ./cmd/${CMD}

FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931

WORKDIR /
COPY --from=builder /workspace/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
