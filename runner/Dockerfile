FROM golang:1.25-alpine AS build
ENV GOTOOLCHAIN=local
WORKDIR /build
COPY runner/go.mod runner/go.sum ./runner/
COPY gen/go/ ./gen/go/
COPY runner/ ./runner/
WORKDIR /build/runner
RUN go build -o /runner ./cmd/runner

FROM node:22-alpine
RUN apk add --no-cache git ca-certificates \
    && npm install --global opencode-ai@1.18.7 \
    && addgroup -S -g 65532 loop \
    && adduser -S -D -u 65532 -h /home/loop -G loop loop \
    && mkdir -p /data \
    && chown -R loop:loop /data /home/loop
COPY --from=build /runner /runner
ENV HOME=/home/loop
USER loop
VOLUME ["/data"]
ENTRYPOINT ["/runner"]
