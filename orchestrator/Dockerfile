FROM python:3.12-slim
ARG GH_CLI_VERSION=2.63.2
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
COPY pyproject.toml ./
COPY migrations ./migrations
COPY src ./src
RUN pip install --no-cache-dir . \
    && rm -rf /root/.cache
COPY docker-entrypoint.sh /docker-entrypoint.sh
RUN chmod 0755 /docker-entrypoint.sh
ENV PYTHONPATH=/app/src
EXPOSE 50051
ENTRYPOINT ["/docker-entrypoint.sh"]
