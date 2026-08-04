# syntax=docker/dockerfile:1
# The Moirai CI runner image: the GitHub Actions self-hosted runner for the
# `self-hosted`/`linux` pool, with the whole pipeline toolchain baked in.
#
# How it is run: the autoscaler project (docs/runner-image-contract) polls the
# GitHub APIs, decides how many runners are needed, and starts one container per
# job. Each container is `AutoRemove`, gets the contract environment variables
# below, and runs exactly one job before exiting. This image implements that
# contract:
#   - non-root `runner` user (uid/gid 1001) that owns /runner and the work dir
#   - entrypoint registers with --ephemeral and runs a single job, then exits
#   - never starts its own dockerd: in wrapped mode the autoscaler's wrapper
#     owns the daemon; this image ships the docker CLI + dockerd so workflow
#     jobs can use Docker (project must set docker_dind)
#   - self-contained: no volumes, nothing mounted or injected beyond env
#
# The pool used to run the myoung34 runner and mount the host's docker socket,
# so compose-smoke built the four product images directly on the 40 GB host
# disk, which ran out of space mid-build ("no space left on device"). Each job
# now builds inside its own fresh, disposable dockerd, so nothing accumulates on
# the host and the cache dies with the container. The "Required Tooling"
# comment that used to describe what the host had to have installed by hand is
# now real and versioned: git, make, gcc, curl, bash, docker, compose, Go, Node,
# sqlc, buf, gh, govulncheck and trivy are all pinned below.
#
# The image is multi-architecture (linux/amd64, linux/arm64). Download URLs
# and checksums are per-architecture, so everything below switches on
# TARGETARCH rather than relying on QEMU to run a foreign build.

FROM ubuntu:22.04@sha256:3b06811b2afd352be909dd088a004166d665dc76d38b13eada33522a9d915c6f

ARG TARGETARCH

# Every tool version the pipeline relies on, in one place. Bump these and bump
# infra/ci-runner/VERSION, then rebuild -- the pool only ever runs what is baked
# here, so a toolchain change is a deliberate, reviewed act.
ARG RUNNER_VERSION=2.336.0
ARG GO_VERSION=1.25.12
ARG NODE_VERSION=v26.6.0
ARG NPM_VERSION=12.0.2
ARG GH_VERSION=2.63.2
ARG SQLC_VERSION=v1.29.0
ARG BUF_VERSION=v1.50.0
ARG GOVULNCHECK_VERSION=v1.6.0
ARG TRIVY_VERSION=v0.73.0
# Docker CE engine/CLI and friends, pinned to the exact apt versions published
# for jammy. docker-ce 29.x depends on containerd >= 2.0, hence the explicit
# containerd pin.
ARG DOCKER_VERSION=5:29.7.1-1~ubuntu.22.04~jammy
ARG CONTAINERD_VERSION=2.2.6-1~ubuntu.22.04~jammy
ARG BUILDX_VERSION=0.36.0-1~ubuntu.22.04~jammy
ARG COMPOSE_VERSION=5.4.0-1~ubuntu.22.04~jammy

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ENV DEBIAN_FRONTEND=noninteractive \
    PATH="/usr/local/go/bin:/usr/local/node/bin:${PATH}" \
    GOTOOLCHAIN=local \
    GOFLAGS=-mod=mod

# Base apt packages. build-essential is required for `go test -race` (cgo), git
# and make for the build targets, and libicu70 + liblttng-ust are the GitHub
# runner's documented runtime dependencies on Ubuntu 22.04. sudo, passwd and
# util-linux are contract requirements: the runner user gets passwordless sudo,
# the wrapped-mode wrapper needs usermod (passwd) to join the docker group, and
# the drop-back to the non-root user needs su/setpriv (util-linux).
RUN set -eux \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        gnupg \
        lsb-release \
        software-properties-common \
        git \
        make \
        build-essential \
        bash \
        tini \
        jq \
        unzip \
        xz-utils \
        sudo \
        passwd \
        util-linux \
        libicu70 \
        liblttng-ust \
    && rm -rf /var/lib/apt/lists/*

# Docker engine, CLI, compose plugin and buildx, pinned, from Docker's apt repo.
# The deb install creates the `docker` group the wrapped-mode wrapper adds the
# runner user to (usermod -aG docker) so jobs can use the CLI without sudo.
RUN set -eux \
    && install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
        | gpg --dearmor -o /etc/apt/keyrings/docker.gpg \
    && chmod a+r /etc/apt/keyrings/docker.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "${VERSION_CODENAME}") stable" \
        > /etc/apt/sources.list.d/docker.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        docker-ce="${DOCKER_VERSION}" \
        docker-ce-cli="${DOCKER_VERSION}" \
        containerd.io="${CONTAINERD_VERSION}" \
        docker-buildx-plugin="${BUILDX_VERSION}" \
        docker-compose-plugin="${COMPOSE_VERSION}" \
    && rm -rf /var/lib/apt/lists/*

# The GitHub Actions runner. The tar self-extracts the .NET-based Listener that
# config.sh and run.sh drive; it self-updates at runtime, so this pin is a
# floor, not a ceiling. installdependencies.sh installs the runner's runtime
# dependencies (including su/setpriv via util-linux) and the `runner` user's
# sudo-less shell support.
RUN set -eux \
    && case "${TARGETARCH}" in \
        amd64) RUNNER_ARCH=x64 ;; \
        arm64) RUNNER_ARCH=arm64 ;; \
        *) echo "unsupported TARGETARCH ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && mkdir -p /runner \
    && curl -fsSL "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz" \
        -o /tmp/runner.tar.gz \
    && tar -xzf /tmp/runner.tar.gz -C /runner \
    && rm -f /tmp/runner.tar.gz \
    && /runner/bin/installdependencies.sh >/dev/null 2>&1 || true

# Non-root runner user (uid/gid 1001) and the writable directories. The GitHub
# runner must run as this user (contract); config.sh writes its state under
# /runner and job checkouts land in RUNNER_WORKDIR, so both are chowned here.
# Passwordless sudo covers pre-scripts and workflows that legitimately run sudo.
RUN set -eux \
    && useradd --create-home --shell /bin/bash --uid 1001 --gid 1001 runner \
    && echo "runner ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/runner \
    && chmod 0440 /etc/sudoers.d/runner \
    && mkdir -p /runner/_work \
    && chown -R runner:runner /runner

# /opt, /tmp and /runner must be fully accessible to root and to the `runner`
# user: pre-scripts and workflow jobs routinely install into or write under
# these trees, and the runner writes its config state under /runner. The runner
# user owns them outright (root is uid 0 and is unaffected by ownership), and
# a+rwX makes everything world-searchable/writable so files created later by
# root remain reachable through the writable directories. /tmp keeps its sticky
# bit (chmod only touches r/w/X bits).
RUN set -eux \
    && mkdir -p /opt /tmp \
    && chown -R runner:runner /opt /tmp /runner \
    && chmod -R a+rwX /opt /tmp /runner

# Go 1.25.12. The same version ci.yml pins for `setup-go`, so baking it keeps
# CI behaviour identical; the toolchain stays in the image because the test,
# vet and build targets run `go` directly.
RUN set -eux \
    && case "${TARGETARCH}" in \
        amd64) GO_ARCH=amd64; GO_SUM=234828b7a89e0e303d2556310ee549fbcf253d28de937bac3da13d6294262ac1 ;; \
        arm64) GO_ARCH=arm64; GO_SUM=8b5884aef89600aef5b0b051fb971f11f49bb996521e911f30f02a66884f7bd2 ;; \
        *) echo "unsupported TARGETARCH ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${GO_ARCH}.tar.gz" \
        -o /tmp/go.tar.gz \
    && echo "${GO_SUM}  /tmp/go.tar.gz" | sha256sum -c - \
    && tar -C /usr/local -xzf /tmp/go.tar.gz \
    && rm -f /tmp/go.tar.gz

# Node 26 + npm. The tarball bundles npm 11; the project declares npm 12 as its
# package manager (ci.yml NPM_VERSION), so that is what gets baked.
RUN set -eux \
    && case "${TARGETARCH}" in \
        amd64) NODE_ARCH=x64 ;; \
        arm64) NODE_ARCH=arm64 ;; \
        *) echo "unsupported TARGETARCH ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && curl -fsSL "https://nodejs.org/dist/${NODE_VERSION}/node-${NODE_VERSION}-linux-${NODE_ARCH}.tar.xz" \
        -o /tmp/node.tar.xz \
    && tar -C /usr/local -xJf /tmp/node.tar.xz \
    && mv "/usr/local/node-${NODE_VERSION}-linux-${NODE_ARCH}" /usr/local/node \
    && rm -f /tmp/node.tar.xz \
    && npm install --global --no-fund --no-audit "npm@${NPM_VERSION}" \
    && npm cache clean --force

# GitHub CLI (same pin as the orchestrator image), sqlc (same pin as the
# Makefile), buf (same pin as the Makefile) and trivy. Single static binaries
# installed to /usr/local/bin.
RUN set -eux \
    && case "${TARGETARCH}" in \
        amd64) GH_ARCH=amd64; SQLC_ARCH=amd64; BUF_ARCH=x86_64; TRIVY_ARCH=64bit ;; \
        arm64) GH_ARCH=arm64; SQLC_ARCH=arm64; BUF_ARCH=aarch64; TRIVY_ARCH=ARM64 ;; \
        *) echo "unsupported TARGETARCH ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${GH_ARCH}.tar.gz" \
        -o /tmp/gh.tar.gz \
    && tar -xzf /tmp/gh.tar.gz -C /tmp \
    && install -m 0755 "/tmp/gh_${GH_VERSION}_linux_${GH_ARCH}/bin/gh" /usr/local/bin/gh \
    && rm -rf "/tmp/gh.tar.gz" "/tmp/gh_${GH_VERSION}_linux_${GH_ARCH}" \
    && curl -fsSL "https://github.com/sqlc-dev/sqlc/releases/download/${SQLC_VERSION}/sqlc_${SQLC_VERSION#v}_linux_${SQLC_ARCH}.tar.gz" \
        -o /tmp/sqlc.tar.gz \
    && tar -xzf /tmp/sqlc.tar.gz -C /tmp \
    && install -m 0755 /tmp/sqlc /usr/local/bin/sqlc \
    && rm -rf /tmp/sqlc.tar.gz \
    && curl -fsSL "https://github.com/bufbuild/buf/releases/download/${BUF_VERSION}/buf-Linux-${BUF_ARCH}" \
        -o /usr/local/bin/buf \
    && chmod 0755 /usr/local/bin/buf \
    && curl -fsSL "https://github.com/aquasecurity/trivy/releases/download/v${TRIVY_VERSION}/trivy_${TRIVY_VERSION}_Linux-${TRIVY_ARCH}.tar.gz" \
        -o /tmp/trivy.tar.gz \
    && tar -xzf /tmp/trivy.tar.gz -C /tmp \
    && install -m 0755 /tmp/trivy /usr/local/bin/trivy \
    && rm -rf /tmp/trivy.tar.gz /tmp/trivy

# govulncheck is a Go module, so it is compiled from source with the baked
# toolchain and then the module cache is dropped -- the image carries the binary,
# not a fresh download of the world.
RUN set -eux \
    && go install "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}" \
    && install -m 0755 "/root/go/bin/govulncheck" /usr/local/bin/govulncheck \
    && rm -rf /root/go /root/.cache/go-build

# The contract entrypoint (non-root: register --ephemeral, run one job, exit)
# and a root wrapper for manual DinD runs that mirrors the autoscaler's
# wrapped-mode boot (dockerd, then drop to the runner user). The autoscaler
# never uses the wrapper -- it installs its own -- so it is a local-testing
# convenience only.
COPY entrypoint.sh /usr/local/bin/ci-runner-entrypoint
COPY wrapper.sh /usr/local/bin/ci-runner-wrapper
RUN chmod 0755 /usr/local/bin/ci-runner-entrypoint /usr/local/bin/ci-runner-wrapper

ENV DOCKER_DATA_ROOT=/var/lib/docker \
    RUNNER_WORKDIR=/runner/_work

# Both halves of the image must be alive: the dockerd a wrapped-mode job needs
# (only required when a socket is present; plain-mode jobs have none) and the
# runner listener. The healthcheck runs as the `runner` user.
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=5 \
    CMD bash -c 'pgrep -f Runner.Listener >/dev/null && { [ ! -e /var/run/docker.sock ] || docker info >/dev/null 2>&1; }'

# The container runs the contract entrypoint as the non-root `runner` user. In
# wrapped mode the autoscaler overrides USER/ENTRYPOINT to run its own root
# wrapper first and execs this entrypoint afterwards.
USER runner
ENTRYPOINT ["/usr/local/bin/ci-runner-entrypoint"]
