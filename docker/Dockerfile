# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT

WORKDIR /src
RUN apk add --no-cache ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN mkdir -p /out /empty-data && \
    case "${TARGETARCH}${TARGETVARIANT}" in \
      armv6) export GOARM=6 ;; \
      armv7) export GOARM=7 ;; \
    esac && \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
      go build -trimpath -ldflags="-s -w" -o /out/monstermq-edge ./cmd/monstermq-edge

FROM scratch

ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/monstermq-edge /monstermq-edge
COPY --from=build --chown=65532:65532 /empty-data /data
COPY docker/config.yaml /etc/monstermq/config.yaml

USER 65532:65532
VOLUME ["/data"]
EXPOSE 1883 1884 8080

ENTRYPOINT ["/monstermq-edge"]
CMD ["-config", "/etc/monstermq/config.yaml"]
