# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT

WORKDIR /src
RUN apk add --no-cache ca-certificates

COPY . .
RUN mkdir -p /out && \
    case "${TARGETARCH}${TARGETVARIANT}" in \
      armv6) export GOARM=6 ;; \
      armv7) export GOARM=7 ;; \
    esac && \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
      go build -trimpath -ldflags="-s -w" -o /out/monstermq-edge ./cmd/monstermq-edge

FROM alpine:3

ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt

RUN apk add --no-cache ca-certificates && \
    mkdir -p /cfg-data

COPY --from=build /out/monstermq-edge /monstermq-edge
COPY docker/config.yaml /etc/monstermq/config.yaml
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 1883 1884 8080

ENTRYPOINT ["/entrypoint.sh"]
