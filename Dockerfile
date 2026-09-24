# syntax=docker/dockerfile:1
#
# devin2proxy — OpenAI-compatible gateway over a Devin CLI credential.
#
# The binary is pure Go standard library with CGO disabled, so the runtime stage
# is a small alpine with nothing but the certificate store: the proxy makes TLS
# calls to the Devin backend, and alpine without ca-certificates would fail
# verification with an error that looks like a network problem rather than a
# missing trust anchor.
#
# Everything is configured through environment variables (see README "Container
# deployment"); no config file is read or written unless DEVIN2PROXY_API_KEY is
# absent, so the image runs correctly against a read-only root filesystem.

# ------------------------------------------------------------------- build
FROM golang:1.27-alpine AS build
WORKDIR /src

# Copy the module graph first and download nothing: there are no dependencies,
# so this layer is just the go.mod, and the source copy below invalidates the
# build cache on every code change rather than on every module change.
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal

# CGO off makes the binary static; -trimpath and -ldflags strip build paths and
# the symbol table, which is all the information an attacker gets from a pulled
# layer.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/devin2proxy ./cmd/devin2proxy

# ------------------------------------------------------------------ runtime
FROM alpine:3.20

# ca-certificates is what the proxy's own TLS calls need; curl is what the
# healthcheck dials with, because alpine's busybox wget cannot speak HTTPS at
# all and the healthcheck has to work when TLS is on.
RUN apk add --no-cache ca-certificates curl

# Run as a dedicated unprivileged user. The process writes nothing outside /data
# (and only there when the dashboard is enabled and asked to keep state), so the
# account needs a writable directory rather than a writable filesystem.
RUN addgroup -S -g 10005 devin \
    && adduser -S -D -u 10005 -G devin devin \
    && mkdir -p /data \
    && chown devin:devin /data

COPY --from=build /out/devin2proxy /usr/local/bin/devin2proxy

USER devin
# /data is the working directory, so the default -config config.json resolves to
# /data/config.json and the dashboard's state file beside it. Mount a volume here
# to persist dashboard-managed accounts and the password hash across restarts;
# without one, that state lives only as long as the container.
WORKDIR /data
VOLUME ["/data"]

# Inside the container the listen address must be every interface, not the
# loopback default: a published port is reached through the bridge address, and
# binding to 127.0.0.1 would make the container unreachable from the host. Override
# with DEVIN2PROXY_ADDR.
ENV DEVIN2PROXY_ADDR=0.0.0.0:8788
EXPOSE 8788

# /healthz answers without authentication and reports the pool and route counts,
# which is exactly what an orchestrator's liveness probe should look at. TLS is
# tried first and -sk accepts the generated self-signed certificate; the plain
# fallback keeps the check honest when DEVIN2PROXY_TLS is off.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -sfk https://127.0.0.1:8788/healthz >/dev/null 2>&1 || \
        curl -sf http://127.0.0.1:8788/healthz >/dev/null 2>&1

ENTRYPOINT ["/usr/local/bin/devin2proxy"]