# docker/Dockerfile — Pulsys local dev stack (compose targets).
#
#   init      one-shot migrate + tenant + OIDC configure
#   pulsys  HF cache proxy + admin API listener
#   console   nginx + static admin SPA

FROM golang:1.26-bookworm@sha256:18aedc16aa19b3fd7ded7245fc14b109e054d65d22ed53c355c899582bbb2113 AS go-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pulsys ./cmd/pulsys && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pulsys-db ./cmd/pulsys-db

FROM node:26-bookworm@sha256:e2cd0ff87e2597f66fab50710216e2a08ad2f09bae0ca78f6b31e8c5f1a811a0 AS ui-builder
WORKDIR /app
COPY admin-ui/package.json admin-ui/package-lock.json ./
RUN npm ci
COPY admin-ui/ .
ENV PULSYS_STATIC_EXPORT=1
RUN npm run build

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS init
RUN apk add --no-cache ca-certificates curl
COPY --from=go-builder /out/pulsys-db /usr/local/bin/pulsys-db
COPY docker/scripts/init.sh /usr/local/bin/pulsys-init
RUN chmod +x /usr/local/bin/pulsys-init
ENTRYPOINT ["/usr/local/bin/pulsys-init"]

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS pulsys
RUN apk add --no-cache ca-certificates wget && \
    adduser -D -h /var/lib/pulsys pulsys && \
    mkdir -p /var/lib/pulsys/cache && \
    chown pulsys:pulsys /var/lib/pulsys/cache
COPY --from=go-builder /out/pulsys /usr/local/bin/pulsys
COPY docker/scripts/pulsys.sh /usr/local/bin/pulsys-pulsys
RUN chmod +x /usr/local/bin/pulsys-pulsys
USER pulsys
ENTRYPOINT ["/usr/local/bin/pulsys-pulsys"]

FROM nginx:1.31-alpine@sha256:54f2a904c251d5a34adf545a72d32515a15e08418dae0266e23be2e18c66fefa AS console
RUN rm -f /etc/nginx/conf.d/default.conf
COPY docker/nginx/pulsys.conf /etc/nginx/conf.d/pulsys.conf
COPY docker/nginx/pulsys_proxy_headers.inc /etc/nginx/conf.d/pulsys_proxy_headers.inc
COPY --from=ui-builder /app/out /var/www/pulsys-admin
