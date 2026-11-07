#!/usr/bin/env bash
# Build the standalone Linux server bundle (frontend + Go binary).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${VERSION:-2.6.0}"
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
TARBALL="$ROOT/build/anyproxyai-server-$VERSION-linux-amd64.tar.gz"
PYTHON_BIN="$(command -v python || command -v python3 || true)"
if [ -n "$PYTHON_BIN" ]; then
  # Windows 上 tar 会丢掉执行位；用 Python 重新打包并显式设置二进制权限。
  "$PYTHON_BIN" - "$OUT" "$TARBALL" <<'PY'
import os
import sys
import tarfile


def normalize(member):
    member.uid = 0
    member.gid = 0
    member.uname = "root"
    member.gname = "root"
    if member.isdir():
        member.mode = 0o755
    elif member.name == "anyproxyai-server":
        member.mode = 0o755
    else:
        member.mode = 0o644
    return member


root, dest = sys.argv[1], sys.argv[2]
with tarfile.open(dest, "w:gz") as tar:
    for name in sorted(os.listdir(root)):
        tar.add(os.path.join(root, name), arcname=name, filter=normalize)
PY
else
  tar -C "$OUT" -czf "$TARBALL" .
fi

echo "Bundle: $TARBALL"
