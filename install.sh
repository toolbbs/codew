#!/usr/bin/env bash
set -Eeuo pipefail

export DEBIAN_FRONTEND="${DEBIAN_FRONTEND:-noninteractive}"
export NEEDRESTART_MODE="${NEEDRESTART_MODE:-a}"
export NEEDRESTART_SUSPEND="${NEEDRESTART_SUSPEND:-1}"

APP_NAME="codew"
MANIFEST_URL="${CODEW_UPDATE_MANIFEST_URL:-}"
WORK_DIR="$(mktemp -d)"
START_TS="$(date +%s)"
APT_UPDATED=false

cleanup() {
  rm -rf "${WORK_DIR}"
}
trap cleanup EXIT

log() {
  printf '[%s] %s\n' "${APP_NAME}" "$*"
}

die() {
  printf '[%s] ERROR: %s\n' "${APP_NAME}" "$*" >&2
  exit 1
}

elapsed() {
  local now
  now="$(date +%s)"
  printf '%ss' "$((now - START_TS))"
}

require_root() {
  if [ "$(id -u)" -ne 0 ]; then
    die "请使用 root 用户执行，或通过 sudo 执行安装命令"
  fi
}

detect_os() {
  case "$(uname -m)" in
    x86_64|amd64) ;;
    *) die "Codew 安装包仅支持 Linux amd64" ;;
  esac
  if [ ! -r /etc/os-release ]; then
    die "无法识别系统版本，当前仅优先支持 Ubuntu 22.04 / Debian 12"
  fi
  # shellcheck disable=SC1091
  . /etc/os-release
  OS_ID="${ID:-unknown}"
  OS_VERSION="${VERSION_ID:-unknown}"
  case "${OS_ID}:${OS_VERSION}" in
    ubuntu:22.04|debian:12)
      log "系统检测通过：${PRETTY_NAME:-${OS_ID} ${OS_VERSION}}"
      ;;
    ubuntu:*|debian:*)
      log "检测到 ${PRETTY_NAME:-${OS_ID} ${OS_VERSION}}，将按兼容模式继续"
      ;;
    *)
      die "当前系统为 ${PRETTY_NAME:-${OS_ID} ${OS_VERSION}}，请优先使用 Ubuntu 22.04 / Debian 12"
      ;;
  esac
}

apt_update_once() {
  if [ "${APT_UPDATED}" != "true" ]; then
    apt-get update
    APT_UPDATED=true
  fi
}

install_dependencies() {
  local missing=()
  command -v unzip >/dev/null 2>&1 || missing+=(unzip)
  command -v curl >/dev/null 2>&1 || missing+=(curl)
  command -v tar >/dev/null 2>&1 || missing+=(tar)
  command -v gzip >/dev/null 2>&1 || missing+=(gzip)
  command -v python3 >/dev/null 2>&1 || missing+=(python3)
  if [ ! -f /etc/ssl/certs/ca-certificates.crt ]; then
    missing+=(ca-certificates)
  fi

  if [ "${#missing[@]}" -eq 0 ]; then
    log "基础依赖已存在，跳过依赖安装"
    return
  fi

  log "安装缺失依赖：${missing[*]}"
  log "首次安装会更新系统依赖索引，网络较慢时可能需要 1-3 分钟。"
  if command -v apt-get >/dev/null 2>&1; then
    apt_update_once
    DEBIAN_FRONTEND=noninteractive apt-get install -y "${missing[@]}"
    return
  fi
  if command -v dnf >/dev/null 2>&1; then
    dnf install -y "${missing[@]}"
    return
  fi
  if command -v yum >/dev/null 2>&1; then
    yum install -y "${missing[@]}"
    return
  fi
  die "未找到支持的包管理器，请手动安装 unzip curl ca-certificates tar gzip python3"
}

check_manifest() {
  python3 -I - "${MANIFEST_URL}" "$@" <<'PY_MANIFEST'
import json
import re
import sys
from urllib.parse import urlsplit

def https_url(value):
    if not isinstance(value, str) or re.search(r"[\x00-\x20\x7f\\#]", value):
        raise ValueError("invalid URL")
    url = urlsplit(value)
    if url.scheme != "https" or not url.hostname or url.username is not None:
        raise ValueError("expected HTTPS URL without credentials")
    return url

def unique_fields(fields):
    result = dict(fields)
    if len(result) != len(fields):
        raise ValueError("duplicate manifest field")
    return result

try:
    https_url(sys.argv[1])
    if len(sys.argv) > 2:
        with open(sys.argv[2], encoding="utf-8") as source:
            manifest = json.load(source, object_pairs_hook=unique_fields)
        version, codename, latest, package, download, digest = (
            manifest[key] for key in ("version", "codename", "latest", "package", "download_url", "sha256")
        )
        if not re.fullmatch(r"[0-9]+(?:\.[0-9]+){2,}", version) or not re.fullmatch(r"[a-z][a-z0-9]*(?:-[a-z0-9]+)*", codename):
            raise ValueError("invalid version or codename")
        if latest != f"{version}-{codename}" or package != f"codew-v{latest}.zip":
            raise ValueError("version does not match the Codew package")
        if not https_url(download).path.endswith(f"/releases/download/v{version}/{package}"):
            raise ValueError("download URL does not match release tag and package")
        if not re.fullmatch(r"[0-9a-fA-F]{64}", digest):
            raise ValueError("invalid SHA256")
        print(package, download, digest.lower(), sep="\n")
except (KeyError, OSError, TypeError, ValueError):
    sys.exit(1)
PY_MANIFEST
}

fetch_manifest() {
  local verified
  MANIFEST_FILE="${WORK_DIR}/version.json"
  log "读取 Codew 版本清单"
  curl -fsSL --proto '=https' --proto-redir '=https' --connect-timeout 10 --max-time 30 --max-filesize 262144 -o "${MANIFEST_FILE}" -- "${MANIFEST_URL}" \
    || die "无法下载版本清单"
  verified="$(check_manifest "${MANIFEST_FILE}")" || die "版本清单无效：请检查 Codew 包名、版本、下载地址及 SHA256"
  local -a fields
  mapfile -t fields <<< "${verified}"
  PACKAGE_NAME="${fields[0]}"
  DOWNLOAD_URL="${fields[1]}"
  SHA256="${fields[2]}"
}

download_release() {
  PACKAGE_FILE="${WORK_DIR}/${PACKAGE_NAME}"
  log "下载 Codew 包：${PACKAGE_NAME}"
  curl -fsSL --proto '=https' --proto-redir '=https' --connect-timeout 10 --max-time 300 -o "${PACKAGE_FILE}" -- "${DOWNLOAD_URL}" \
    || die "无法下载 Codew 包"

  log "校验 SHA256"
  (cd "${WORK_DIR}" && printf '%s  %s\n' "${SHA256}" "${PACKAGE_NAME}" | sha256sum -c -) \
    || die "SHA256 校验失败，已停止安装"
}

extract_release() {
  local package_root="${PACKAGE_NAME%.zip}"
  python3 -I - "${PACKAGE_FILE}" "${package_root}" <<'PY_ZIP' || die "ZIP 预检失败，未解压或部署"
import re
import stat
import sys
import zipfile

try:
    with zipfile.ZipFile(sys.argv[1]) as archive:
        root = sys.argv[2]
        seen = set()
        files = set()
        for member in archive.infolist():
            path = member.filename.removesuffix("/")
            parts = path.split("/")
            if (member.orig_filename != member.filename or re.search(r"[\x00-\x1f\x7f\\:]", path)
                    or parts[0] != root or any(part in ("", ".", "..") for part in parts)
                    or path in seen):
                raise ValueError("unsafe ZIP path")
            seen.add(path)
            expected_type = stat.S_IFDIR if member.is_dir() else stat.S_IFREG
            if stat.S_IFMT(member.external_attr >> 16) not in (0, expected_type):
                raise ValueError("unsafe ZIP member type")
            if not member.is_dir():
                if len(parts) < 2:
                    raise ValueError("expected package directory")
                files.add(path)
        required = {"deploy/install.sh", "deploy/agent-install.sh", "scripts/codew", "scripts/codew-netopt",
                    "bin/codew-api-linux-amd64", "bin/codew-agent-linux-amd64", "frontend/dist/index.html"}
        if not all(f"{root}/{path}" in files for path in required) or archive.testzip() is not None:
            raise ValueError("incomplete or corrupt Codew package")
except (OSError, RuntimeError, ValueError, zipfile.BadZipFile):
    sys.exit(1)
PY_ZIP
  local extract_dir="${WORK_DIR}/extract"
  mkdir -- "${extract_dir}"
  unzip -q "${PACKAGE_FILE}" -d "${extract_dir}" || die "ZIP 解压失败，未部署"
  RELEASE_DIR="${extract_dir}/${package_root}"
}

run_deploy_install() {
  log "执行部署脚本"
  (cd "${RELEASE_DIR}" && CODEW_UPDATE_MANIFEST_URL="${MANIFEST_URL}" bash deploy/install.sh)
}

print_finish_info() {
  log "安装完成（$(elapsed)）。运行 codew info 查看面板地址，codew doctor 检查状态。"
}

main() {
  require_root
  detect_os
  [[ -n "${MANIFEST_URL}" ]] || die "请显式设置 CODEW_UPDATE_MANIFEST_URL；不会使用旧仓库或默认清单"
  install_dependencies
  check_manifest || die "CODEW_UPDATE_MANIFEST_URL 必须是无凭据的 HTTPS 地址"
  fetch_manifest
  download_release
  extract_release
  run_deploy_install
  print_finish_info
}

main "$@"
