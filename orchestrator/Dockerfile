FROM python:3.12-slim
WORKDIR /app
RUN addgroup --system --gid 65532 loop \
    && adduser --system --uid 65532 --gid 65532 loop \
    && mkdir -p /app /data \
    && chown -R loop:loop /app /data
COPY pyproject.toml ./
COPY migrations ./migrations
COPY src ./src
RUN pip install --no-cache-dir "psycopg[binary]>=3.2" . \
    && rm -rf /root/.cache
ENV PYTHONPATH=/app/src
EXPOSE 50051
USER 65532:65532
CMD ["python", "-m", "moirai.main"]
