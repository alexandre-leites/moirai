# Build context is the repository root: the runner module resolves the generated
# contracts through `replace github.com/loop-engineering/contracts => ../gen/go`.
#
# The builder always runs on the native build platform and cross-compiles to the
# requested target, so a linux/arm64 image costs no QEMU emulation for the Go
# stage. Only the runtime stage, which installs distro and npm packages, runs
# under emulation on a foreign architecture.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build
ENV GOTOOLCHAIN=local \
    CGO_ENABLED=0
WORKDIR /build
COPY gen/go/ ./gen/go/
COPY runner/go.mod runner/go.sum ./runner/
WORKDIR /build/runner
RUN go mod download
COPY runner/ ./
ARG TARGETOS
ARG TARGETARCH
RUN GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath -ldflags='-s -w' -o /runner ./cmd/runner

FROM node:22-alpine@sha256:16e22a550f3863206a3f701448c45f7912c6896a62de43add43bb9c86130c3e2
ARG OPENCODE_VERSION=1.18.7
# `npm install --global opencode-ai` keeps every optional platform package it
# downloaded (four of them on linux/amd64, ~520 MB) plus a ~425 MB npm cache,
# even though its postinstall has already hardlinked the one binary it needs
# into bin/opencode.exe. Dropping both cuts the published image from ~2.0 GB to
# ~0.6 GB per architecture. `opencode --version` runs after the deletion, so a
# future opencode layout that stops hardlinking fails the build here instead of
# shipping a runner whose agent binary is missing.
RUN apk add --no-cache git ca-certificates su-exec \
    && npm install --global --no-fund --no-audit "opencode-ai@${OPENCODE_VERSION}" \
    && rm -rf "$(npm root -g)/opencode-ai/node_modules" \
    && npm cache clean --force \
    && rm -rf /root/.npm \
    && opencode --version \
    && addgroup -S -g 65532 loop \
    && adduser -S -D -u 65532 -h /home/loop -G loop loop \
    && mkdir -p /data \
    && chown -R loop:loop /data /home/loop
COPY --from=build /runner /runner
COPY runner/docker-entrypoint.sh /docker-entrypoint.sh
RUN chmod 0755 /docker-entrypoint.sh
ENV HOME=/home/loop
VOLUME ["/data"]
# Compose overrides this with its own probe. It exists so that `docker run` of a
# published image reports health without the caller restating the check.
HEALTHCHECK --interval=15s --timeout=10s --start-period=30s --retries=5 \
    CMD ["/runner", "live"]
ENTRYPOINT ["/docker-entrypoint.sh"]
