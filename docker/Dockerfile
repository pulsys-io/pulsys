# docker/Dockerfile — Pulsys local dev stack (compose targets).
#
#   init      one-shot migrate + tenant + OIDC configure
#   pulsys  HF cache proxy + admin API listener
#   console   nginx + static admin SPA

FROM golang:1.27-bookworm@sha256:484ef6066fa69acb059fdfeda7ba2b8f7391f2ef6abc6f9b8411e669ebd56466 AS go-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pulsys ./cmd/pulsys && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pulsys-db ./cmd/pulsys-db

FROM node:26-bookworm@sha256:0353e48e0e8a993db87b720c242f54b207059d1bcc0106534896e8a11054c837 AS ui-builder
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

FROM nginx:1.31-alpine@sha256:db35bfc6b2951e7f8a72db5db120288c127ffaeeb4a6d4b95a26fead017d5913 AS console
RUN rm -f /etc/nginx/conf.d/default.conf
COPY docker/nginx/pulsys.conf /etc/nginx/conf.d/pulsys.conf
COPY docker/nginx/pulsys_proxy_headers.inc /etc/nginx/conf.d/pulsys_proxy_headers.inc
COPY --from=ui-builder /app/out /var/www/pulsys-admin
