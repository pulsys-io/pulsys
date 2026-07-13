# docker/Dockerfile — Pulsys local dev stack (compose targets).
#
#   init      one-shot migrate + tenant + OIDC configure
#   pulsys  HF cache proxy + admin API listener
#   console   nginx + static admin SPA

FROM golang:1.25-bookworm@sha256:a9c020ee3d1508c7be5435c262434e3d3fc1d0e76a11afeb9ddae7d60bc86aa4 AS go-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pulsys ./cmd/pulsys && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pulsys-db ./cmd/pulsys-db

FROM node:20-bookworm@sha256:8f693eaa7e0a8e71560c9a82b55fd54c2ae920a2ba5d2cde28bac7d1c01c9ba5 AS ui-builder
WORKDIR /app
COPY admin-ui/package.json admin-ui/package-lock.json ./
RUN npm ci
COPY admin-ui/ .
ENV PULSYS_STATIC_EXPORT=1
RUN npm run build

FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d AS init
RUN apk add --no-cache ca-certificates curl
COPY --from=go-builder /out/pulsys-db /usr/local/bin/pulsys-db
COPY docker/scripts/init.sh /usr/local/bin/pulsys-init
RUN chmod +x /usr/local/bin/pulsys-init
ENTRYPOINT ["/usr/local/bin/pulsys-init"]

FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d AS pulsys
RUN apk add --no-cache ca-certificates wget && \
    adduser -D -h /var/lib/pulsys pulsys && \
    mkdir -p /var/lib/pulsys/cache && \
    chown pulsys:pulsys /var/lib/pulsys/cache
COPY --from=go-builder /out/pulsys /usr/local/bin/pulsys
COPY docker/scripts/pulsys.sh /usr/local/bin/pulsys-pulsys
RUN chmod +x /usr/local/bin/pulsys-pulsys
USER pulsys
ENTRYPOINT ["/usr/local/bin/pulsys-pulsys"]

FROM nginx:1.27-alpine@sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10 AS console
RUN rm -f /etc/nginx/conf.d/default.conf
COPY docker/nginx/pulsys.conf /etc/nginx/conf.d/pulsys.conf
COPY docker/nginx/pulsys_proxy_headers.inc /etc/nginx/conf.d/pulsys_proxy_headers.inc
COPY --from=ui-builder /app/out /var/www/pulsys-admin
