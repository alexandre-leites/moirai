# Build context is the repository root: the API module resolves the generated
# contracts through `replace github.com/loop-engineering/contracts => ../gen/go`.
#
# The builder always runs on the native build platform and cross-compiles to the
# requested target, so a linux/arm64 image costs no QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine@sha256:56961d79ea8129efddcc0b8643fd8a5416b4e6228cfd477e3fd61deb2672c587 AS build
# GOTOOLCHAIN=local keeps the build from fetching another toolchain. CGO_ENABLED=0
# is already the effective default here (the image ships no C toolchain); stating
# it means a base image that gains one cannot silently start producing a
# dynamically linked binary that the scratch-thin runtime could not run.
ENV GOTOOLCHAIN=local \
    CGO_ENABLED=0
WORKDIR /build
# Module metadata and the replaced local module first: this layer and the module
# download it feeds are reused by every build that changes only service source.
COPY gen/go/ ./gen/go/
COPY api/go.mod api/go.sum ./api/
WORKDIR /build/api
RUN go mod download
COPY api/ ./
ARG TARGETOS
ARG TARGETARCH
RUN GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath -ldflags='-s -w' -o /api ./cmd/api

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
RUN addgroup -S -g 65532 loop \
    && adduser -S -u 65532 -G loop loop
COPY --from=build /api /api
EXPOSE 8080
USER 65532:65532
# Compose overrides this with its own probe. It exists so that `docker run` of a
# published image reports health without the caller restating the check.
HEALTHCHECK --interval=15s --timeout=5s --start-period=15s --retries=5 \
    CMD wget --no-verbose --tries=1 --spider http://127.0.0.1:8080/ready || exit 1
ENTRYPOINT ["/api"]
