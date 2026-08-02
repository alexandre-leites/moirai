# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
WORKDIR /workspace
COPY gen/go ./gen/go
COPY orchestrator/go.mod orchestrator/go.sum ./orchestrator/
WORKDIR /workspace/orchestrator
RUN go mod download
WORKDIR /workspace
COPY orchestrator/cmd ./orchestrator/cmd
COPY orchestrator/internal ./orchestrator/internal
COPY orchestrator/migrations ./orchestrator/migrations
WORKDIR /workspace/orchestrator
ARG MOIRAI_BUILD_VERSION
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/orchestrator ./cmd/orchestrator

FROM debian:bookworm-slim
ARG GH_CLI_VERSION=2.63.2
ARG MOIRAI_BUILD_VERSION
ENV MOIRAI_BUILD_VERSION=${MOIRAI_BUILD_VERSION}
WORKDIR /app
RUN apt-get update \
    && apt-get install --no-install-recommends -y ca-certificates curl git gosu \
    && arch="$(dpkg --print-architecture)" \
    && curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_CLI_VERSION}/gh_${GH_CLI_VERSION}_linux_${arch}.tar.gz" -o /tmp/gh.tar.gz \
    && tar -xzf /tmp/gh.tar.gz -C /tmp \
    && install -m 0755 "/tmp/gh_${GH_CLI_VERSION}_linux_${arch}/bin/gh" /usr/local/bin/gh \
    && rm -rf /tmp/gh.tar.gz "/tmp/gh_${GH_CLI_VERSION}_linux_${arch}" /var/lib/apt/lists/* \
    && addgroup --system --gid 65532 loop \
    && adduser --system --uid 65532 --gid 65532 loop \
    && mkdir -p /app /run/loop-secrets \
    && chown -R loop:loop /app /run/loop-secrets
COPY --from=build /out/orchestrator /app/orchestrator
COPY orchestrator/migrations /app/migrations
COPY orchestrator/docker-entrypoint.sh /docker-entrypoint.sh
RUN chmod 0755 /docker-entrypoint.sh
EXPOSE 50051
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=5 CMD ["/app/orchestrator", "healthcheck"]
ENTRYPOINT ["/docker-entrypoint.sh"]
