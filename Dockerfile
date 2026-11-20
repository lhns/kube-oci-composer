# Build stage. Dependencies are downloaded in their own layer so a source-only change does not
# re-resolve the module graph.
# Pinned by digest, and CI fails if its Go minor differs from go.mod's: the Go that builds the RELEASE
# is an input to every composed artifact's bytes (ADR 0057), and a floating tag once moved it
# without CI, which used go.mod's, noticing. Dependabot proposes patch and digest bumps.
# $BUILDPLATFORM: the compiler runs natively and cross-compiles, so no QEMU is needed (the runtime
# stage has no RUN).
FROM --platform=$BUILDPLATFORM golang:1.27.4@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS builder

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# CMD selects which binary this image carries. The two are built from one Dockerfile because they
# share every layer up to the compile step; ADR 0004 wants two DEPLOYMENTS, which is about RBAC and
# blast radius, not about duplicating a build recipe.
#
# Declared here, not at the top: an ARG is in the cache key of every RUN after it, so declared
# earlier they made each arch, binary and version download the modules again.
ARG CMD=oci-composer
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
ARG TARGETOS
ARG TARGETARCH

# CGO off so the result is a static binary that runs on a distroless base.
# No GOOS/GOARCH fallbacks: a silent default can put an amd64 binary in an arm64 image. The ELF
# e_machine check (readable without QEMU) fails the build if the binary is not the target's.
RUN set -eu; \
    [ -n "${TARGETOS}" ] && [ -n "${TARGETARCH}" ] || { echo "TARGETOS/TARGETARCH unset: build with BuildKit"; exit 1; }; \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}" \
      -o manager ./cmd/${CMD}; \
    case "${TARGETARCH}" in \
      amd64) want="3e 00" ;; \
      arm64) want="b7 00" ;; \
      *) echo "unsupported TARGETARCH ${TARGETARCH}"; exit 1 ;; \
    esac; \
    got=$(od -An -tx1 -j18 -N2 manager | xargs); \
    [ "$got" = "$want" ] || { echo "manager ELF e_machine is '$got', expected '$want' for ${TARGETARCH}"; exit 1; }

# Runtime stage.
#
# Distroless nonroot: no shell, no package manager, no libc to patch. This component sits in the
# supply chain, so the smaller its attack surface the better — and nothing in the binary needs a
# shell. The blob store and cache directories are the only writable paths it wants, and both are
# supplied as volumes, so the root filesystem can stay read-only.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931

WORKDIR /
COPY --from=builder /workspace/manager .

USER 65532:65532

ENTRYPOINT ["/manager"]
