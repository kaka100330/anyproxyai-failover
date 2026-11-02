# AnyProxyAI standalone server image.
#
# Build context must contain:
#   anyproxyai-server      linux/amd64 static binary (CGO_ENABLED=0)
#   web/                   web frontend build (npm run build:web)
#   ca-certificates.crt    CA bundle for outbound HTTPS
#
# Use scripts/build-server.sh to assemble this context.
FROM scratch

WORKDIR /app

COPY anyproxyai-server /app/anyproxyai-server
COPY web /app/web
COPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

ENV ANYPROXY_HOST=0.0.0.0 \
    ANYPROXY_PORT=3000 \
    ANYPROXY_DB=/data/routes.db \
    ANYPROXY_WEB_ROOT=/app/web

VOLUME ["/data"]
EXPOSE 3000

ENTRYPOINT ["/app/anyproxyai-server"]
