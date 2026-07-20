# Combined OSS image for pulsys: ships both the proxy (pulsys) and the
# admin/DB CLI (pulsys-db) on PATH, with no entrypoint wrapper. This is the
# image the Helm chart and the release pipeline target
# (ghcr.io/pulsys-io/pulsys). For the zero-config local dev stack, see the
# multi-stage build in ../../docker/Dockerfile used by compose.yaml.
FROM golang:1.26-bookworm@sha256:1ecb7edf62a0408027bd5729dfd6b1b8766e578e8df93995b225dfd0944eb651 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/pulsys ./cmd/pulsys && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pulsys-db ./cmd/pulsys-db

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
RUN apk add --no-cache ca-certificates && \
    adduser -D -H -u 65532 -h /var/lib/pulsys nonroot && \
    mkdir -p /var/lib/pulsys/cache && chown 65532:65532 /var/lib/pulsys/cache
COPY --from=build /out/pulsys /usr/local/bin/pulsys
COPY --from=build /out/pulsys-db /usr/local/bin/pulsys-db
USER 65532:65532
EXPOSE 8080 6060
ENTRYPOINT ["pulsys"]
