#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SOURCE_VERSION="$(tr -d '\r\n' < "$ROOT_DIR/VERSION")"
SOURCE_NUMBER="${SOURCE_VERSION%%-*}"
SOURCE_CODENAME="${SOURCE_VERSION#*-}"
VERSION="${1:-$SOURCE_NUMBER}"
CODENAME="${2:-$SOURCE_CODENAME}"
if [[ "$VERSION-$CODENAME" != "$SOURCE_VERSION" ]]; then
  echo "ERROR: build version $VERSION-$CODENAME does not match VERSION ($SOURCE_VERSION)"
  exit 1
fi
command -v node >/dev/null 2>&1 || { echo "ERROR: node not found"; exit 1; }
node "$ROOT_DIR/scripts/check-version-consistency.mjs" --mode dev
OUT_DIR="${CODEW_RELEASE_OUT_DIR:-$ROOT_DIR/dist-release}"
PKG_NAME="codew-v${SOURCE_VERSION}.zip"
PYTHON_BIN="${PYTHON3:-python3}"
TARGET_GOOS=linux
TARGET_GOARCH=amd64

command -v go >/dev/null 2>&1 || { echo "ERROR: go not found"; exit 1; }
command -v npm >/dev/null 2>&1 || { echo "ERROR: npm not found"; exit 1; }
if [[ ! -x "$PYTHON_BIN" ]] && ! command -v "$PYTHON_BIN" >/dev/null 2>&1; then
  echo "ERROR: python3 not found"
  exit 1
fi

mkdir -- "$OUT_DIR" || { echo "ERROR: use a new output directory; existing candidates are not overwritten." >&2; exit 1; }
OUT_DIR="$(cd "$OUT_DIR" && pwd)"
PKG_PATH="$OUT_DIR/$PKG_NAME"
STAGE_DIR="$OUT_DIR/staging"
PKG_ROOT="$STAGE_DIR/${PKG_NAME%.zip}"
mkdir -p "$PKG_ROOT/bin"

# Stage only release inputs. In particular, never copy the source bin/ or frontend/dist/.
"$PYTHON_BIN" -I - "$ROOT_DIR" "$PKG_ROOT" <<'PY_STAGE'
import shutil
import sys
from pathlib import Path

source, target = (Path(arg).resolve() for arg in sys.argv[1:])

def copy_file(path):
    if not path.is_file() or path.resolve() != path:
        raise SystemExit(f"ERROR: expected a regular release input: {path.relative_to(source)}")
    destination = target / path.relative_to(source)
    destination.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(path, destination)

for name in (
    ".env.example", "CHANGELOG.md", "LICENSE_STRATEGY.md", "README.md", "VERSION",
    "install.sh", "docker-compose.yml", "example.version.json",
    "backend/Dockerfile", "backend/go.mod", "agent/go.mod",
    "deploy/install.sh", "deploy/agent-install.sh", "deploy/uninstall.sh",
    "scripts/codew", "scripts/codew-netopt",
    "frontend/.dockerignore", "frontend/Dockerfile", "frontend/index.html",
    "frontend/nginx.conf", "frontend/package.json", "frontend/package-lock.json",
    "frontend/tsconfig.json", "frontend/vite.config.ts",
):
    copy_file(source / name)

for module in ("backend", "agent"):
    lock = source / module / "go.sum"
    if lock.exists() or lock.is_symlink():
        copy_file(lock)

for directory, suffixes in {
    "backend/cmd": {".go"}, "backend/internal": {".go"}, "agent/cmd": {".go"},
    "database": {".sql"}, "docs": {".md"}, "frontend/src": {".ts", ".vue", ".css"},
}.items():
    for path in sorted((source / directory).rglob("*")):
        if path.suffix in suffixes:
            copy_file(path)
PY_STAGE

echo "[1/5] Building backend API binary"
(cd "$PKG_ROOT/backend" && go test ./... &&
  CGO_ENABLED=0 GOOS="$TARGET_GOOS" GOARCH="$TARGET_GOARCH" go build -trimpath -ldflags='-s -w' -o "$PKG_ROOT/bin/codew-api-linux-amd64" ./cmd/server)

echo "[2/5] Building agent binary"
if [[ "$(go env GOOS)" == "linux" ]]; then
  (cd "$PKG_ROOT/agent" && go test ./...)
else
  echo "Non-Linux host: Agent runtime tests require Linux; validating with a Linux amd64 cross-build."
fi
(cd "$PKG_ROOT/agent" &&
  CGO_ENABLED=0 GOOS="$TARGET_GOOS" GOARCH="$TARGET_GOARCH" go build -trimpath -ldflags='-s -w' -o "$PKG_ROOT/bin/codew-agent-linux-amd64" ./cmd/agent)

echo "[3/5] Building frontend dist"
(cd "$PKG_ROOT/frontend" && npm ci --no-audit --no-fund --progress=false && MSYS2_ENV_CONV_EXCL="${MSYS2_ENV_CONV_EXCL:+$MSYS2_ENV_CONV_EXCL;}VITE_BASE_PATH" VITE_BASE_PATH=/ npm run build)

echo "[4/5] Packaging fast release"
test -f "$PKG_ROOT/bin/codew-api-linux-amd64"
test -f "$PKG_ROOT/bin/codew-agent-linux-amd64"
test -f "$PKG_ROOT/frontend/dist/index.html"
cat > "$PKG_ROOT/PACKAGE-MANIFEST.json" <<JSON_PACKAGE
{
  "version": "${VERSION}",
  "codename": "${CODENAME}",
  "latest": "${SOURCE_VERSION}",
  "package": "${PKG_NAME}",
  "note": "Release SHA256 is published outside the archive. This package intentionally omits version.json to avoid a self-referential hash."
}
JSON_PACKAGE

"$PYTHON_BIN" -I - "$STAGE_DIR" "${PKG_NAME%.zip}" "$PKG_PATH" <<'PYZIP'
import os
import stat
import sys
import zipfile

base, root_name, out = sys.argv[1], sys.argv[2], sys.argv[3]
root = os.path.join(base, root_name)
with zipfile.ZipFile(out, 'x', compression=zipfile.ZIP_DEFLATED, compresslevel=9) as zf:
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = sorted(name for name in dirnames if name != 'node_modules')
        for filename in sorted(filenames):
            if filename.endswith('.tsbuildinfo'):
                continue
            full = os.path.join(dirpath, filename)
            if not stat.S_ISREG(os.lstat(full).st_mode):
                raise SystemExit('ERROR: package input is not a regular file')
            rel = os.path.relpath(full, base).replace(os.sep, '/')
            info = zipfile.ZipInfo.from_file(full, rel)
            info.create_system = 3
            if rel.endswith(('.sh', '/scripts/codew', '/scripts/codew-netopt')) or '/bin/' in rel:
                info.external_attr = (stat.S_IFREG | 0o755) << 16
            else:
                info.external_attr = (stat.S_IFREG | 0o644) << 16
            with open(full, 'rb') as source:
                zf.writestr(info, source.read(), compress_type=zipfile.ZIP_DEFLATED, compresslevel=9)
PYZIP

SHA256="$(sha256sum "$PKG_PATH" | awk '{print $1}')"
printf '%s  %s\n' "$SHA256" "$PKG_NAME" > "$OUT_DIR/SHA256SUMS"
cat > "$OUT_DIR/version.fast.json" <<JSON
{
  "latest": "${SOURCE_VERSION}",
  "version": "${VERSION}",
  "codename": "${CODENAME}",
  "package": "${PKG_NAME}",
  "download_url": "https://releases.invalid/releases/download/v${VERSION}/${PKG_NAME}",
  "sha256": "${SHA256}",
  "changelog": [
    "Local Codew candidate. Not a published update channel."
  ]
}
JSON
node "$ROOT_DIR/scripts/check-version-consistency.mjs" --mode release \
  --manifest "$OUT_DIR/version.fast.json" --package "$PKG_PATH"
PYTHON3="$PYTHON_BIN" node "$ROOT_DIR/scripts/test-fast-release-package.mjs" "$PKG_PATH"

echo "[5/5] Done"
echo "Package: $PKG_PATH"
echo "SHA256:  $SHA256"
echo "Local candidate manifest (not a live download URL): $OUT_DIR/version.fast.json"
