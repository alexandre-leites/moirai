# uv only ever runs on the build platform: it turns the committed uv.lock into a
# hash-pinned requirements file, which is architecture-independent because the
# lock is universal and carries every platform's wheel hashes.
FROM --platform=$BUILDPLATFORM ghcr.io/astral-sh/uv:0.12.0@sha256:606e70c71c852d03f611b1e56a195d08648507018a7057fab82c4974c4eae105 AS uv

FROM --platform=$BUILDPLATFORM python:3.14-slim@sha256:cea0e6040540fb2b965b6e7fb5ffa00871e632eef63719f0ea54bca189ce14a6 AS requirements
COPY --from=uv /uv /usr/local/bin/uv
WORKDIR /build
COPY pyproject.toml uv.lock ./
# --locked, not --frozen: --frozen only means "do not re-lock", and exports a
# stale lock happily -- a dependency added to pyproject.toml without running
# `uv lock` would simply be missing from the image. --locked asserts the lock
# still matches pyproject.toml and exits non-zero when it does not.
RUN uv export --locked --no-dev --no-emit-project --format requirements.txt -o /requirements.txt

FROM python:3.14-slim@sha256:cea0e6040540fb2b965b6e7fb5ffa00871e632eef63719f0ea54bca189ce14a6
ARG GH_CLI_VERSION=2.63.2
ARG MOIRAI_BUILD_VERSION
ENV MOIRAI_BUILD_VERSION=${MOIRAI_BUILD_VERSION}
RUN test -n "${MOIRAI_BUILD_VERSION}"
WORKDIR /app
RUN addgroup --system --gid 65532 loop \
    && adduser --system --uid 65532 --gid 65532 loop \
    && mkdir -p /app /data \
    && chown -R loop:loop /app /data
RUN apt-get update \
    && apt-get install --no-install-recommends -y git ca-certificates curl gosu \
    && arch="$(dpkg --print-architecture)" \
    && curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_CLI_VERSION}/gh_${GH_CLI_VERSION}_linux_${arch}.tar.gz" -o /tmp/gh.tar.gz \
    && tar -xzf /tmp/gh.tar.gz -C /tmp \
    && install -m 0755 "/tmp/gh_${GH_CLI_VERSION}_linux_${arch}/bin/gh" /usr/local/bin/gh \
    && rm -rf /tmp/gh.tar.gz "/tmp/gh_${GH_CLI_VERSION}_linux_${arch}" \
    && apt-get purge -y curl \
    && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*
# Dependencies come from the lock, not from a fresh resolution of the version
# ranges in pyproject.toml: the image then contains exactly the closure CI audits
# with pip-audit, and rebuilding an old commit reproduces its dependency set.
# The install is also its own layer, so editing orchestrator source no longer
# reinstalls every dependency.
#
# --no-compile because this layer is built for both architectures and the arm64
# one runs under QEMU. Writing .pyc for every module in the closure is pure
# CPython execution, which is the worst case under emulation: it measured 696
# seconds of a 920-second build. Python compiles each module on first import
# instead, natively, once per container -- a startup cost a long-running server
# pays once, in exchange for the build not paying it on every architecture.
COPY --from=requirements /requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir --no-compile --require-hashes --requirement /tmp/requirements.txt \
    && rm -f /tmp/requirements.txt \
    && rm -rf /root/.cache
COPY pyproject.toml ./
COPY migrations ./migrations
COPY src ./src
RUN pip install --no-cache-dir --no-compile --no-deps . \
    && rm -rf /root/.cache
COPY docker-entrypoint.sh /docker-entrypoint.sh
RUN chmod 0755 /docker-entrypoint.sh
ENV PYTHONPATH=/app/src
EXPOSE 50051
# Compose overrides this with its own probe. It exists so that `docker run` of a
# published image reports health without the caller restating the check.
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=5 \
    CMD ["python", "-c", "import json,sys,time; d=json.load(open('/tmp/loop-orchestrator-health.json')); sys.exit(0 if d.get('db_ok') and time.time()-d.get('updated_at',0)<45 else 1)"]
ENTRYPOINT ["/docker-entrypoint.sh"]
