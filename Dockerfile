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
# Compile every command once, with no stamp ARGs in scope: an ARG is in the cache key of every RUN
# after it, and VERSION/COMMIT/BUILD_DATE change on every build. Both images share this layer.
# PER-REPO: oci-composer has two commands; both are compiled here and stamped below.
RUN : "${TARGETOS:?}" "${TARGETARCH:?}" && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" ./cmd/...

# Only package main is relinked here (-X reaches it alone); the compiled dependencies come from the
# build cache of the RUN above.
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}" \
      -o /out/ ./cmd/...

# PER-REPO: distroless nonroot: no shell, package manager or libc. This sits in the supply chain, so
# the smaller its attack surface the better; its writable paths are volumes, so the root
# filesystem stays read-only.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5

# PER-REPO: CMD selects the binary. One Dockerfile for both, since they share every layer up to this
# point; ADR 0004's two Deployments are about RBAC and blast radius, not the build recipe.
ARG CMD=oci-composer

WORKDIR /
COPY --from=builder /out/${CMD} /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
