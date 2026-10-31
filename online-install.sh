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
SCRIPTS_REF=""
BUNDLED_SCRIPTS=0
MANAGEMENT_DIR=""
INSTALL_ROOT=/etc/xboard-node
CLI_PATH=/usr/local/bin/xbctl
XB_PATH=/usr/local/bin/xb
LOCK_PATH=/run/lock/xboard-node-deploy.lock

error() { printf '错误：%s\n' "$*" >&2; return 1; }
usage() {
    cat <<'HELP'
Usage: sudo bash online-install.sh [--version TAG] [menu|install|upgrade|scripts-update]
Downloads a matching deployment ZIP from GitHub Releases,
verifies it and opens the deployment menu. Default version: latest.
Supports Debian/Ubuntu with systemd, Linux amd64 and arm64.
Each release needs xboard-node-TAG-linux-ARCH.zip and its .zip.sha256 file.
Management scripts are fetched from one main commit when compatible with the core.
  --scripts-ref SHA   Pin management scripts to a 40-character Git commit
  --bundled-scripts   Use only the scripts included in the verified release ZIP
  scripts-update     Update installed scripts only; no binary download or restart
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
            --scripts-ref)
                [[ $# -ge 2 && "$2" =~ ^[0-9a-f]{40}$ ]] || { error '脚本引用必须是完整 Git commit SHA。'; return 1; }
                SCRIPTS_REF=$2; shift 2
                ;;
            --bundled-scripts) BUNDLED_SCRIPTS=1; shift ;;
            menu|install|upgrade|scripts-update)
                [[ "$action_seen" == 0 ]] || { error '只能指定一个操作。'; return 1; }
                ACTION=$1; action_seen=1; shift
                ;;
            --help|-h) SHOW_HELP=1; shift ;;
            *) error "未知参数：$1"; return 1 ;;
        esac
    done
    valid_version "$VERSION" || { error '版本号包含不支持的字符。'; return 1; }
    [[ "$BUNDLED_SCRIPTS" == 0 || ( -z "$SCRIPTS_REF" && "$ACTION" != scripts-update ) ]] || {
        error '--bundled-scripts 不能与脚本更新或 --scripts-ref 同用。'; return 1;
    }
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
    local -a required=(curl realpath flock)
    [[ "$ACTION" == scripts-update ]] || required+=(unzip sha256sum tar od)
    for name in "${required[@]}"; do
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
    [[ ! -f "$WORK_DIR/KEEP" ]] || { error "恢复文件已保留：$WORK_DIR，请勿删除。"; return 1; }
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
fetch_management_scripts() {
    local response helper sha
    if [[ -z "$SCRIPTS_REF" ]]; then
        response="$WORK_DIR/scripts-ref.json"
        download_file "https://api.github.com/repos/$REPOSITORY/git/ref/heads/main" "$response" || {
            error '查询管理脚本版本失败，可重试或使用 --bundled-scripts 运行发行包菜单。'; return 1;
        }
        sha=$(grep -oE '"sha"[[:space:]]*:[[:space:]]*"[0-9a-f]{40}"' "$response" | tr -d '"[:space:]' | cut -d: -f2) || return 1
        [[ "$sha" =~ ^[0-9a-f]{40}$ ]] || { error '脚本 commit 响应无效。'; return 1; }
        SCRIPTS_REF=$sha
    fi
    MANAGEMENT_DIR="$WORK_DIR/management"
    mkdir "$MANAGEMENT_DIR" && chmod 700 "$MANAGEMENT_DIR" || return 1
    for helper in deploy.sh install.sh online-install.sh xb; do
        download_file "https://raw.githubusercontent.com/$REPOSITORY/$SCRIPTS_REF/$helper" "$MANAGEMENT_DIR/$helper" || return 1
        bash -n "$MANAGEMENT_DIR/$helper" || { error "脚本语法校验失败：$helper"; return 1; }
        chmod 755 "$MANAGEMENT_DIR/$helper" || return 1
    done
    grep -Fqx '# Xboard-Node management shortcut' "$MANAGEMENT_DIR/xb" || { error 'xb 脚本标识无效。'; return 1; }
    printf '管理脚本 commit：%s\n' "$SCRIPTS_REF"
}
scripts_compatible() {
    local core=$1 minimum index
    local -a current required
    minimum=$(sed -n 's/^# XBCTL_MIN_VERSION=//p' "$MANAGEMENT_DIR/deploy.sh") || return 1
    [[ "$core" =~ ^v?([0-9]{1,8})\.([0-9]{1,8})\.([0-9]{1,8})$ ]] || return 1
    current=("${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}")
    [[ "$minimum" =~ ^v?([0-9]{1,8})\.([0-9]{1,8})\.([0-9]{1,8})$ ]] || return 1
    required=("${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}")
    (( 10#${current[0]} == 10#${required[0]} )) || return 1
    for index in 1 2; do
        (( 10#${current[index]} >= 10#${required[index]} )) || return 1
        if (( 10#${current[index]} > 10#${required[index]} )); then return 0; fi
    done
}
apply_management_update() (
    # EXIT handlers must retain their state even after an explicit return.
    helper=""; destination=""; update_started=0; rollback_failed=0
    backup="$WORK_DIR/script-backup"; suffix=".xb-scripts-$BASHPID"
    destinations=("$INSTALL_ROOT/deploy.sh" "$INSTALL_ROOT/install.sh" "$INSTALL_ROOT/online-install.sh" "$XB_PATH")
    staged=()
    rollback_scripts() {
        local index
        trap '' INT TERM
        for destination in "${staged[@]}"; do rm -f -- "$destination" || rollback_failed=1; done
        for index in "${!destinations[@]}"; do
            destination=${destinations[index]}
            if [[ "$update_started" == 1 ]]; then
                if [[ -f "$backup/$index" ]]; then cp -p -- "$backup/$index" "$destination" || rollback_failed=1
                else rm -f -- "$destination" || rollback_failed=1; fi
            fi
        done
        if [[ "$rollback_failed" == 1 ]]; then
            error "脚本回滚未完成，恢复文件：$backup"
        else
            rm -f -- "$WORK_DIR/KEEP"
        fi
    }
    trap rollback_scripts EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    exec 9>"$LOCK_PATH" || return 1
    flock -n 9 || { error '另一项部署/恢复操作正在运行。'; return 1; }
    [[ -x "$CLI_PATH" && -d "$INSTALL_ROOT" ]] || { error '尚未安装，请先运行部署向导。'; return 1; }
    [[ ! -e "$XB_PATH" && ! -L "$XB_PATH" ]] || {
        [[ -f "$XB_PATH" && ! -L "$XB_PATH" ]] && grep -Fqx '# Xboard-Node management shortcut' "$XB_PATH" || {
            error 'xb 已被其他程序占用，拒绝覆盖。'; return 1;
        }
    }
    local core_version index=0
    core_version=$("$CLI_PATH" version | awk 'NR == 1 {print $2}') || return 1
    scripts_compatible "$core_version" || { error "新脚本与已安装核心 $core_version 不兼容，请先升级核心。"; return 1; }
    mkdir "$backup" && chmod 700 "$backup" || return 1
    for destination in "${destinations[@]}"; do
        [[ ! -L "$destination" && ( ! -e "$destination" || -f "$destination" ) ]] || {
            error "拒绝覆盖非常规文件：$destination"; return 1;
        }
        [[ ! -f "$destination" ]] || cp -p -- "$destination" "$backup/$index" || return 1
        index=$((index+1))
    done
    index=0
    for helper in deploy.sh install.sh online-install.sh xb; do
        destination=${destinations[index]}
        [[ ! -e "$destination$suffix" && ! -L "$destination$suffix" ]] || return 1
        staged+=("$destination$suffix")
        install -m 755 "$MANAGEMENT_DIR/$helper" "$destination$suffix" || return 1
        index=$((index+1))
    done
    # Retain backups even if an uncatchable termination prevents rollback.
    touch "$WORK_DIR/KEEP" || return 1
    update_started=1
    for destination in "${destinations[@]}"; do mv -f -- "$destination$suffix" "$destination" || return 1; done
    update_started=0
    printf '管理脚本已更新。核心程序、配置和服务运行状态未改动；输入 xb 打开菜单。\n'
)
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
    trap 'exit 130' INT
    create_workspace || return 1
    if [[ "$ACTION" == scripts-update ]]; then
        [[ -x "$CLI_PATH" && -d "$INSTALL_ROOT" ]] || { error '请先完成安装。'; return 1; }
        fetch_management_scripts && apply_management_update
        return $?
    fi
    resolve_version && download_bundle && extract_bundle && verify_bundle_files || return 1
    local entry="$BUNDLE_DIR/deploy.sh"
    local -a options=(--bundle-dir "$BUNDLE_DIR")
    if [[ "$BUNDLED_SCRIPTS" == 0 ]]; then
        fetch_management_scripts || return 1
        if scripts_compatible "$VERSION"; then
            entry="$MANAGEMENT_DIR/deploy.sh"
            options+=(--management-dir "$MANAGEMENT_DIR")
        else
            printf '脚本与 %s 不兼容，使用该发行包自带的管理脚本。\n' "$VERSION"
        fi
    fi
    printf '\n文件校验通过，打开部署菜单。\n'
    # Keep release files and their checksum manifest unchanged.
    bash "$entry" "${options[@]}" "$ACTION"
}
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
