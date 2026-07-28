FROM golang:1.25-alpine AS build
WORKDIR /build
COPY api/go.mod api/go.sum ./api/
COPY gen/go/ ./gen/go/
COPY api/ ./api/
WORKDIR /build/api
RUN go build -o /api ./cmd/api

FROM alpine:3.20
RUN addgroup -S -g 65532 loop \
    && adduser -S -u 65532 -G loop loop
COPY --from=build /api /api
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/api"]
