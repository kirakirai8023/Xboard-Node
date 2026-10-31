#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Standalone online entry point for the independently maintained fork.
set -Eeuo pipefail

REPOSITORY=kirakirai8023/Xboard-Node
VERSION=latest
ACTION=menu
ARCH=""
TEMP_BASE=""
WORK_DIR=""
PACKAGE_NAME=""
BUNDLE_DIR=""
SHOW_HELP=0

error() { printf '错误：%s\n' "$*" >&2; return 1; }
usage() {
    cat <<'HELP'
Usage: sudo bash online-install.sh [--version TAG] [menu|install|upgrade]
Downloads a matching deployment ZIP from GitHub Releases,
verifies it and opens the deployment menu. Default version: latest.
Supports Debian/Ubuntu with systemd, Linux amd64 and arm64.
Each release needs xboard-node-TAG-linux-ARCH.zip and its .zip.sha256 file.
HELP
}
valid_version() { [[ "$1" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$ ]]; }
parse_args() {
    local action_seen=0
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --version)
                [[ $# -ge 2 ]] || { error '--version 后需要版本号。'; return 1; }
                VERSION=$2; shift 2
                ;;
            menu|install|upgrade)
                [[ "$action_seen" == 0 ]] || { error '只能指定一个操作。'; return 1; }
                ACTION=$1; action_seen=1; shift
                ;;
            --help|-h) SHOW_HELP=1; shift ;;
            *) error "未知参数：$1"; return 1 ;;
        esac
    done
    valid_version "$VERSION" || { error '版本号包含不支持的字符。'; return 1; }
}
require_environment() {
    [[ "$(uname -s)" == Linux && "$(id -u)" == 0 ]] || { error '请在 Linux 上使用 root 或 sudo 运行。'; return 1; }
    case "$(uname -m)" in
        x86_64|amd64) ARCH=amd64 ;;
        aarch64|arm64) ARCH=arm64 ;;
        *) error '仅支持 amd64 / arm64。'; return 1 ;;
    esac
    command -v systemctl >/dev/null && [[ -d /run/systemd/system ]] || { error '需要运行中的 systemd。'; return 1; }
}
ensure_dependencies() {
    local name missing=0
    for name in curl unzip sha256sum realpath flock tar od; do
        command -v "$name" >/dev/null || missing=1
    done
    if [[ "$missing" == 1 ]]; then
        command -v apt-get >/dev/null || { error '请先安装 curl、unzip、coreutils、util-linux、tar 和 CA 证书。'; return 1; }
        printf '安装在线部署所需依赖……\n'
        apt-get update && apt-get install -y curl ca-certificates unzip coreutils util-linux tar || return 1
    fi
}
create_workspace() {
    TEMP_BASE=$(realpath -e -- "${TMPDIR:-/tmp}") || return 1
    WORK_DIR=$(mktemp -d "$TEMP_BASE/xboard-online.XXXXXXXX") || return 1
    chmod 700 "$WORK_DIR"
}
cleanup_workspace() {
    local resolved
    [[ -n "$WORK_DIR" && -d "$WORK_DIR" && ! -L "$WORK_DIR" ]] || return 0
    resolved=$(realpath -e -- "$WORK_DIR") || return 1
    [[ "$resolved" == "$WORK_DIR" ]] || return 1
    case "$resolved" in
        "$TEMP_BASE"/xboard-online.????????) rm -rf -- "$resolved" ;;
        *) error '拒绝清理生成目录以外的路径。'; return 1 ;;
    esac
    WORK_DIR=""
}
resolve_version() {
    if [[ "$VERSION" == latest ]]; then
        local effective prefix="https://github.com/$REPOSITORY/releases/tag/"
        effective=$(curl --fail --location --silent --show-error --retry 3 \
            --connect-timeout 15 --max-time 60 --proto '=https' --proto-redir '=https' \
            --output /dev/null --write-out '%{url_effective}' \
            "https://github.com/$REPOSITORY/releases/latest") || {
            error '查询最新版本失败，请检查网络或使用 --version 指定已发布版本。'; return 1;
        }
        [[ "$effective" == "$prefix"* ]] || { error '未找到正式 Release，请先发布安装包。'; return 1; }
        VERSION=${effective#"$prefix"}
    fi
    valid_version "$VERSION" || { error 'Release 标签不能用于此部署包命名格式。'; return 1; }
}
download_file() {
    curl --fail --location --show-error --retry 3 --connect-timeout 15 --max-time 600 \
        --proto '=https' --proto-redir '=https' --output "$2" "$1"
}
download_bundle() {
    PACKAGE_NAME="xboard-node-$VERSION-linux-$ARCH"
    local asset="$PACKAGE_NAME.zip" line hash base="https://github.com/$REPOSITORY/releases/download/$VERSION"
    printf '下载版本：%s；服务器架构：%s\n' "$VERSION" "$ARCH"
    download_file "$base/$asset.sha256" "$WORK_DIR/$asset.sha256" || {
        error "缺少发布附件 $asset.sha256，请先发布完整部署包及校验文件。"; return 1;
    }
    line=$(<"$WORK_DIR/$asset.sha256")
    line=${line%$'\r'}
    hash=${line:0:64}
    [[ "$hash" =~ ^[0-9a-fA-F]{64}$ && "$line" == "$hash  $asset" ]] || { error 'ZIP 校验文件格式无效。'; return 1; }
    printf '%s\n' "$line" > "$WORK_DIR/$asset.sha256" || return 1
    download_file "$base/$asset" "$WORK_DIR/$asset" || { error "下载 $asset 失败。"; return 1; }
    (cd -- "$WORK_DIR" && sha256sum --strict -c "$asset.sha256") || { error 'ZIP 校验失败，未修改服务。'; return 1; }
}
extract_bundle() {
    local archive="$WORK_DIR/$PACKAGE_NAME.zip" entry relative
    unzip -Z -1 "$archive" > "$WORK_DIR/entries" || return 1
    [[ -s "$WORK_DIR/entries" ]] || return 1
    while IFS= read -r entry; do
        [[ "$entry" == "$PACKAGE_NAME/"* ]] || { error 'ZIP 根目录与版本/架构不符。'; return 1; }
        relative=${entry#"$PACKAGE_NAME/"}
        case "$relative" in /*|*\\*|../*|*/../*|*/..|..|*$'\r'*) error 'ZIP 包含无效路径。'; return 1 ;; esac
    done < "$WORK_DIR/entries"
    unzip -Z -l "$archive" > "$WORK_DIR/attributes" || return 1
    if LC_ALL=C grep -q '^l' "$WORK_DIR/attributes"; then error 'ZIP 不得包含符号链接。'; return 1; fi
    mkdir "$WORK_DIR/extracted" || return 1
    unzip -q "$archive" -d "$WORK_DIR/extracted" || return 1
    BUNDLE_DIR="$WORK_DIR/extracted/$PACKAGE_NAME"
}
verify_bundle_files() {
    local line path hash required
    [[ -f "$BUNDLE_DIR/SHA256SUMS" && ! -L "$BUNDLE_DIR/SHA256SUMS" ]] || { error '部署包缺少 SHA256SUMS。'; return 1; }
    while IFS= read -r line || [[ -n "$line" ]]; do
        hash=${line:0:64}; path=${line:66}
        [[ "$hash" =~ ^[0-9a-fA-F]{64}$ && "$line" == "$hash  $path" && -n "$path" ]] || return 1
        case "$path" in /*|*\\*|../*|*/../*|*/..|..) error '文件清单包含无效路径。'; return 1 ;; esac
        [[ -f "$BUNDLE_DIR/$path" && ! -L "$BUNDLE_DIR/$path" ]] || return 1
        [[ "$(realpath -e -- "$BUNDLE_DIR/$path")" == "$BUNDLE_DIR/"* ]] || return 1
    done < "$BUNDLE_DIR/SHA256SUMS"
    for required in deploy.sh install.sh xboard-node xbctl; do
        [[ -f "$BUNDLE_DIR/$required" ]] || { error "部署包缺少 $required。"; return 1; }
        hash=$(sha256sum -- "$BUNDLE_DIR/$required" | cut -d' ' -f1) || return 1
        grep -Fqx -- "$hash  $required" "$BUNDLE_DIR/SHA256SUMS" || {
            error "校验清单未覆盖 $required。"; return 1;
        }
    done
    (cd -- "$BUNDLE_DIR" && sha256sum --strict -c SHA256SUMS) || { error '部署包文件校验失败。'; return 1; }
}
main() {
    parse_args "$@" || return 1
    if [[ "$SHOW_HELP" == 1 ]]; then usage; return 0; fi
    require_environment && ensure_dependencies || return 1
    umask 077
    trap cleanup_workspace EXIT
    trap 'exit 143' TERM
    create_workspace && resolve_version && download_bundle && extract_bundle && verify_bundle_files || return 1
    printf '\n文件校验通过，打开部署菜单。\n'
    bash "$BUNDLE_DIR/deploy.sh" --bundle-dir "$BUNDLE_DIR" "$ACTION"
}
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
