# The compiler runs natively ($BUILDPLATFORM) and cross-compiles, so no QEMU is needed (the
# runtime stage has no RUN). The golang tag is go.mod's toolchain version; CI fails on a minor
# mismatch, and the local setup-go action makes CI test with this exact Go.
FROM --platform=$BUILDPLATFORM golang:1.27.4@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS builder
# PER-REPO: the line above (golang:<go.mod toolchain version>@sha256:<digest>).
# PER-REPO: the Go that builds the release is an input to every composed artifact's bytes (ADR
# 0057), so a minor bump means deciding about AssemblyVersion, not just merging a Dependabot PR.

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

# PER-REPO: the source directories the build needs (traefik-reflector and ha-surge have no api/).
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
# PER-REPO: oci-composer adds `ARG CMD=oci-composer` and builds ./cmd/${CMD}.
# PER-REPO: CMD selects the binary. One Dockerfile for both, since they share every layer up to the
# compile step; ADR 0004's two Deployments are about RBAC and blast radius, not the build recipe.
# Declared after `go mod download`: an ARG is in the cache key of every RUN after it, so declared
# earlier it made each arch, binary and version download the modules again.
ARG CMD=oci-composer

RUN : "${TARGETOS:?}" "${TARGETARCH:?}" && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}" \
      -o /workspace/manager ./cmd/${CMD}

# PER-REPO: distroless nonroot: no shell, package manager or libc. This sits in the supply chain, so
# the smaller its attack surface the better; its writable paths are volumes, so the root
# filesystem stays read-only.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931

WORKDIR /
COPY --from=builder /workspace/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
