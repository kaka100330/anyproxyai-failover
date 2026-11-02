#!/usr/bin/env bash
# Build the standalone Linux server bundle (frontend + Go binary).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${VERSION:-2.4.0}"
OUT="$ROOT/build/server"

echo "==> Building web frontend (mode=web)"
(cd "$ROOT/frontend" && npm run build:web)

mkdir -p "$OUT/web"
rm -rf "$OUT/web"
mkdir -p "$OUT/web"
cp -r "$ROOT/frontend/dist/." "$OUT/web/"

echo "==> Building linux/amd64 server binary"
(cd "$ROOT" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags "-s -w" -o "$OUT/anyproxyai-server" ./cmd/server)

cp "$ROOT/deploy/Dockerfile" "$OUT/Dockerfile"
cp "$ROOT/deploy/docker-compose.yml" "$OUT/docker-compose.yml"

if [ -f /etc/ssl/certs/ca-certificates.crt ]; then
  cp /etc/ssl/certs/ca-certificates.crt "$OUT/ca-certificates.crt"
else
  echo "WARN: /etc/ssl/certs/ca-certificates.crt not found; copy a CA bundle before docker build" >&2
fi

echo "==> Packaging"
tar -C "$OUT" -czf "$ROOT/build/anyproxyai-server-$VERSION-linux-amd64.tar.gz" .

echo "Bundle: $ROOT/build/anyproxyai-server-$VERSION-linux-amd64.tar.gz"
