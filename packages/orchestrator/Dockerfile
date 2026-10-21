ARG GOLANG_VERSION=1.26.8
ARG DEBIAN_RUNTIME_TAG=bookworm-20260824-slim

# The orchestrator binary is built with CGO and dynamically links against
# glibc, so the build image's glibc must be <= the host's glibc (forward
# compatibility only). The host runs Ubuntu 24.04 (glibc 2.39), so bookworm
# (glibc 2.36) is safe; do NOT bump to trixie (glibc 2.41) without also
# upgrading the host, or the raw binary will fail with "GLIBC_2.4x not found".
ARG DEBIAN_VERSION=bookworm

FROM golang:${GOLANG_VERSION}-${DEBIAN_VERSION} AS builder
# Fall through to a retry on any module proxy error; see
# .github/actions/go-setup-cache/action.yml for the mechanism.
ENV GOPROXY=https://proxy.golang.org|https://proxy.golang.org:443|https://proxy.golang.org/|direct
ARG TARGETARCH

# Cached golang dependencies
WORKDIR /build/shared
COPY ./shared/go.mod ./shared/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

WORKDIR /build/clickhouse
COPY ./clickhouse/go.mod ./clickhouse/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

WORKDIR /build/orchestrator
COPY ./orchestrator/go.mod ./orchestrator/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Copy source code
WORKDIR /build

COPY ./shared/pkg ./shared/pkg
COPY ./clickhouse/pkg ./clickhouse/pkg

COPY ./orchestrator/pkg ./orchestrator/pkg
COPY ./orchestrator/cmd ./orchestrator/cmd
COPY ./orchestrator/main.go ./orchestrator/main.go
COPY ./orchestrator/Makefile ./orchestrator/Makefile

WORKDIR /build/orchestrator

ARG COMMIT_SHA
ARG VERSION
# Use Docker's TARGETARCH (set by --platform) so GOARCH matches the container platform.
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/go/pkg/mod make build-local COMMIT_SHA=${COMMIT_SHA} VERSION=${VERSION} BUILD_ARCH=${TARGETARCH}

FROM scratch AS artifacts

COPY --from=builder /build/orchestrator/bin/clean-nfs-cache .
COPY --from=builder /build/orchestrator/bin/orchestrator .

# Klex host-binary bundle. Keep `artifacts` and the default runtime image
# unchanged for upstream callers. Publication selects this target explicitly.
FROM builder AS klex-release

RUN test -n "$VERSION" \
    && printf '%s\n' "$COMMIT_SHA" | grep -Eq '^[a-f0-9]{40}$' \
    && go version -m bin/orchestrator | grep -F -- "-X=main.commitSHA=$COMMIT_SHA" \
    && go version -m bin/orchestrator | grep -F -- "-X=github.com/e2b-dev/infra/packages/orchestrator/pkg/version.Version=$VERSION" \
    && ldd bin/orchestrator > /tmp/orchestrator-ldd \
    && ! grep -q 'not found' /tmp/orchestrator-ldd \
    && cd bin \
    && sha256sum orchestrator clean-nfs-cache > SHA256SUMS \
    && printf '{"schema_version":1,"version":"%s","commit":"%s","os":"linux","arch":"%s"}\n' "$VERSION" "$COMMIT_SHA" "$TARGETARCH" > release.json

FROM artifacts AS klex-artifacts

ARG COMMIT_SHA
ARG VERSION
LABEL org.opencontainers.image.source="https://github.com/stagewise-io/e2b-runtime" \
      org.opencontainers.image.revision="${COMMIT_SHA}" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=klex-release /build/orchestrator/bin/SHA256SUMS .
COPY --from=klex-release /build/orchestrator/bin/release.json .

FROM debian:${DEBIAN_RUNTIME_TAG} AS runtime

ARG COMMIT_SHA

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        bash \
        util-linux \
        iproute2 \
        iptables \
        rsync \
        e2fsprogs \
        systemd \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /build/orchestrator/bin/orchestrator /usr/local/bin/orchestrator

ENTRYPOINT ["/usr/local/bin/orchestrator"]
