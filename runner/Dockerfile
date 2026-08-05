# Build context is the repository root: the runner module resolves the generated
# contracts through `replace github.com/alexandre-leites/moirai/contracts => ../gen/go`.
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
ARG MOIRAI_BUILD_VERSION
RUN GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath -ldflags="-s -w -X main.version=${MOIRAI_BUILD_VERSION}" -o /runner ./cmd/runner

FROM node:26-alpine@sha256:233761595746769ebfdb6090f44fc7cdf818ae0ce62d2b37e0367723b9823e36
ARG OPENCODE_VERSION=1.18.7
# `npm install --global opencode-ai` keeps every optional platform package it
# downloaded (four of them on linux/amd64, ~520 MB) plus a ~425 MB npm cache,
# even though its postinstall has already hardlinked the one binary it needs
# into bin/opencode.exe. Dropping both cuts the published image from ~2.0 GB to
# ~0.6 GB per architecture. `opencode --version` runs after the deletion, so a
# future opencode layout that stops hardlinking fails the build here instead of
# shipping a runner whose agent binary is missing.
# openssh-client because git shells out to `ssh` for an ssh:// or git@ remote.
# Without it a project with an SSH remote fails the clone with
# "ssh: not found" -- after the key has been resolved and written, so
# everything upstream looks like it worked. git itself does not pull it in.
RUN apk add --no-cache git openssh-client ca-certificates su-exec \
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
# The image's own account of the toolchain it offers the agent, at the
# conventional path every execution image answers on. The runner splices it into
# the agent's prompt, so the agent is told what it has instead of finding out by
# running a command that fails -- and a failed probe costs a whole attempt out
# of a budget of two.
#
# `toolchain --verify` runs it against this filesystem and fails the build if it
# does not match, in both directions: a declared tool that is missing, or a
# declared absence that someone has since installed. A declaration nobody checks
# rots into a confident lie, which is worse for the agent than no declaration.
COPY runner/toolchain.json /etc/moirai/toolchain.json
RUN chmod 0755 /docker-entrypoint.sh \
    && chmod 0644 /etc/moirai/toolchain.json \
    && /runner toolchain --verify
ENV HOME=/home/loop
VOLUME ["/data"]
# Compose overrides this with its own probe. It exists so that `docker run` of a
# published image reports health without the caller restating the check.
HEALTHCHECK --interval=15s --timeout=10s --start-period=30s --retries=5 \
    CMD ["/runner", "live"]
ENTRYPOINT ["/docker-entrypoint.sh"]
