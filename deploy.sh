#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Independent local-bundle UI; installation is delegated to install.sh.
set -uo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
BUNDLE_DIR="$SCRIPT_DIR"
INSTALL_ROOT=/etc/xboard-node
BACKUP_ROOT=/var/backups/xboard-node
NODE_PATH=/usr/local/bin/xboard-node
CLI_PATH=/usr/local/bin/xbctl
XB_PATH=/usr/local/bin/xb
UNIT_PATH=/etc/systemd/system/xboard-node.service
SERVICE=xboard-node.service
LOCK_PATH=/run/lock/xboard-node-deploy.lock
TOKEN_DIR=""
TOKEN_PATH=""
BACKUP_LAST=""
NODE_FILE=""
CLI_FILE=""
ARCH=""

say() { printf '%s\n' "$*"; }
fail() { printf '错误：%s\n' "$*" >&2; return 1; }
confirm() {
    local answer
    read -r -p "$* [y/N]: " answer || return 1
    [[ "$answer" == y || "$answer" == Y ]]
}
ask() {
    local variable=$1 label=$2 fallback=${3:-} answer
    read -r -p "$label${fallback:+ [$fallback]}: " answer || return 1
    printf -v "$variable" '%s' "${answer:-$fallback}"
}
need() {
    local command
    for command in "$@"; do command -v "$command" >/dev/null 2>&1 || { fail "缺少 $command，请使用依赖安装菜单。"; return 1; }; done
}
root_only() { [[ "$(id -u)" == 0 ]] || { fail '请使用 sudo bash deploy.sh 或 root 用户。'; return 1; }; }
linux_only() { [[ "$(uname -s)" == Linux ]] || { fail '部署操作仅适用于 Linux。'; return 1; }; }
systemd_only() {
    need systemctl || return 1
    [[ -d /run/systemd/system ]] || { fail '需要运行中的 systemd。'; return 1; }
}
architecture() {
    case "$(uname -m)" in
        x86_64|amd64) ARCH=amd64 ;;
        aarch64|arm64) ARCH=arm64 ;;
        *) fail '仅支持 Linux amd64 / arm64。'; return 1 ;;
    esac
}
cleanup_token() {
    if [[ -n "$TOKEN_DIR" && "$TOKEN_PATH" == "$TOKEN_DIR/token" ]]; then
        rm -f -- "$TOKEN_PATH"
        rmdir -- "$TOKEN_DIR" 2>/dev/null || true
    fi
    TOKEN_DIR=""; TOKEN_PATH=""
}
read_token() {
    local secret
    cleanup_token
    IFS= read -r -s -p '通信密钥（输入隐藏）: ' secret || return 1
    printf '\n'
    [[ -n "$secret" && "$secret" != *$'\r'* ]] || { fail '密钥不能为空或包含换行。'; return 1; }
    TOKEN_DIR=$(mktemp -d) || return 1
    chmod 700 "$TOKEN_DIR" || return 1
    TOKEN_PATH="$TOKEN_DIR/token"
    (umask 077; printf '%s' "$secret" > "$TOKEN_PATH") || return 1
    unset secret
}
with_lock() (
    root_only || exit 1
    need flock || exit 1
    exec 9>"$LOCK_PATH" || exit 1
    flock -n 9 || { fail '另一项部署/恢复操作正在运行。'; exit 1; }
    "$@"
)

validate_elf() {
    local path=$1 magic endian machine
    magic=$(od -An -tx1 -N4 -- "$path" | tr -d ' \n') || return 1
    endian=$(od -An -tu1 -j5 -N1 -- "$path" | tr -d ' \n') || return 1
    machine=$(od -An -tu2 -j18 -N2 -- "$path" | tr -d ' \n') || return 1
    [[ "$magic" == 7f454c46 && "$endian" == 1 ]] || { fail "不是受支持的 Linux ELF 文件：$(basename -- "$path")"; return 1; }
    case "$ARCH:$machine" in amd64:62|arm64:183) ;; *) fail "程序架构与服务器 $ARCH 不匹配。"; return 1 ;; esac
}
verify_manifest() {
    local directory=$1 line path
    [[ -f "$directory/SHA256SUMS" ]] || { fail '缺少 SHA256SUMS，请使用 build.bat 生成完整部署包。'; return 1; }
    while IFS= read -r line || [[ -n "$line" ]]; do
        [[ "$line" =~ ^[0-9a-fA-F]{64}\ \ .+ ]] || { fail '校验清单格式无效。'; return 1; }
        path=${line:66}
        case "$path" in /*|*\\*|../*|*/../*|*/..|..) fail '校验清单包含目录外路径。'; return 1 ;; esac
        [[ -f "$directory/$path" && ! -L "$directory/$path" ]] || { fail "清单文件不存在或为符号链接：$path"; return 1; }
        [[ "$(realpath -e -- "$directory/$path")" == "$directory/"* ]] || { fail '清单文件越出部署目录。'; return 1; }
    done < "$directory/SHA256SUMS"
    (cd -- "$directory" && sha256sum --strict -c SHA256SUMS) || { fail '文件校验失败，尚未修改服务。'; return 1; }
}
verify_bundle() {
    need sha256sum od realpath || return 1
    architecture || return 1
    BUNDLE_DIR=$(realpath -e -- "$BUNDLE_DIR") || return 1
    NODE_FILE="$BUNDLE_DIR/xboard-node"
    CLI_FILE="$BUNDLE_DIR/xbctl"
    [[ -f "$NODE_FILE" ]] || NODE_FILE="$BUNDLE_DIR/xboard-node-linux-$ARCH"
    [[ -f "$CLI_FILE" ]] || CLI_FILE="$BUNDLE_DIR/xbctl-linux-$ARCH"
    [[ -f "$NODE_FILE" && -f "$CLI_FILE" && -f "$BUNDLE_DIR/install.sh" ]] || { fail '目录内需要 xboard-node、xbctl、install.sh 和 SHA256SUMS。'; return 1; }
    verify_manifest "$BUNDLE_DIR" || return 1
    local name
    for name in "$(basename -- "$NODE_FILE")" "$(basename -- "$CLI_FILE")" install.sh deploy.sh; do
        grep -Fqx -- "$(sha256sum -- "$BUNDLE_DIR/$name" | cut -d' ' -f1)  $name" "$BUNDLE_DIR/SHA256SUMS" || { fail "清单未覆盖 $name。"; return 1; }
    done
    validate_elf "$NODE_FILE" && validate_elf "$CLI_FILE" || return 1
    chmod 755 "$NODE_FILE" "$CLI_FILE" || return 1
    "$NODE_FILE" -v && "$CLI_FILE" version
}
choose_bundle() {
    ask BUNDLE_DIR '本地部署包目录（解压后的目录）' "$BUNDLE_DIR" || return 1
    verify_bundle
}

snapshot() {
    need tar sha256sum realpath || return 1
    install -d -m 700 "$BACKUP_ROOT" || return 1
    local pending final active=0 enabled=0
    pending=$(mktemp -d "$BACKUP_ROOT/.pending-XXXXXXXX") || return 1
    install -d -m 700 "$pending/bin" "$pending/systemd" || return 1
    systemctl is-active --quiet "$SERVICE" && active=1
    systemctl is-enabled --quiet "$SERVICE" && enabled=1
    printf 'ACTIVE=%s\nENABLED=%s\n' "$active" "$enabled" > "$pending/state" || return 1
    if [[ -d "$INSTALL_ROOT" ]]; then
        tar -C "$INSTALL_ROOT" --exclude=./backups -cf "$pending/config.tar" . || return 1
    fi
    [[ ! -f "$NODE_PATH" ]] || cp -p -- "$NODE_PATH" "$pending/bin/xboard-node" || return 1
    [[ ! -f "$CLI_PATH" ]] || cp -p -- "$CLI_PATH" "$pending/bin/xbctl" || return 1
    if management_shortcut_owned; then cp -p -- "$XB_PATH" "$pending/bin/xb" || return 1; fi
    [[ ! -f "$UNIT_PATH" ]] || cp -p -- "$UNIT_PATH" "$pending/systemd/xboard-node.service" || return 1
    printf 'xboard-node-menu-backup-v1\n' > "$pending/COMPLETE" || return 1
    (cd -- "$pending" && find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS) || return 1
    final="$BACKUP_ROOT/menu-$(date -u +%Y%m%dT%H%M%SZ)-${pending##*.pending-}"
    mv -- "$pending" "$final" || return 1
    BACKUP_LAST="$final"
    say "备份已保存：$final"
}
backup_resolve() {
    local requested=$1 resolved base
    base=$(realpath -e -- "$BACKUP_ROOT") || return 1
    resolved=$(realpath -e -- "$BACKUP_ROOT/$requested") || return 1
    [[ "$requested" == menu-* && "$requested" != */* && "$resolved" == "$base/"* && -f "$resolved/COMPLETE" ]] || { fail '请选择备份目录中的完整快照名称。'; return 1; }
    [[ "$(<"$resolved/COMPLETE")" == xboard-node-menu-backup-v1 ]] || return 1
    (cd -- "$resolved" && sha256sum --strict -c SHA256SUMS) || { fail '备份校验失败。'; return 1; }
    RESOLVED_BACKUP="$resolved"
}
state_value() {
    local file=$1 key=$2 value
    value=$(sed -n "s/^${key}=//p" "$file")
    [[ "$value" == 0 || "$value" == 1 ]] || return 1
    printf '%s' "$value"
}
wait_running() {
    local i
    for i in 1 2 3; do
        sleep 1
        systemctl is-active --quiet "$SERVICE" || return 1
    done
}
restore_files() {
    local source=$1 previous=$2 entry name active enabled
    active=$(state_value "$source/state" ACTIVE) || return 1
    enabled=$(state_value "$source/state" ENABLED) || return 1
    if [[ -f "$source/config.tar" ]]; then
        tar -tf "$source/config.tar" >/dev/null || return 1
        # Reject traversal before stopping the current service.
        while IFS= read -r entry; do
            case "$entry" in /*|../*|*/../*|*/..) fail '备份归档路径无效。'; return 1 ;; esac
        done < <(tar -tf "$source/config.tar")
    fi
    systemctl stop "$SERVICE" >/dev/null 2>&1 || { [[ ! -f "$UNIT_PATH" ]] || return 1; }
    install -d -m 700 "$INSTALL_ROOT" "$previous" || return 1
    # Retain the old files instead of recursively deleting configuration.
    while IFS= read -r -d '' entry; do
        name=${entry##*/}
        [[ "$name" != backups ]] || continue
        mv -- "$entry" "$previous/$name" || return 1
    done < <(find "$INSTALL_ROOT" -mindepth 1 -maxdepth 1 -print0)
    if [[ -f "$source/config.tar" ]]; then
        tar --no-same-owner -xf "$source/config.tar" -C "$INSTALL_ROOT" || return 1
    fi
    if [[ -f "$source/bin/xboard-node" ]]; then
        install -m 755 "$source/bin/xboard-node" "$NODE_PATH.new" && mv -f -- "$NODE_PATH.new" "$NODE_PATH" || return 1
    else rm -f -- "$NODE_PATH" || return 1; fi
    if [[ -f "$source/bin/xbctl" ]]; then
        install -m 755 "$source/bin/xbctl" "$CLI_PATH.new" && mv -f -- "$CLI_PATH.new" "$CLI_PATH" || return 1
    else rm -f -- "$CLI_PATH" || return 1; fi
    if [[ -f "$source/bin/xb" ]]; then
        if [[ -e "$XB_PATH" || -L "$XB_PATH" ]] && ! management_shortcut_owned; then
            fail 'xb 命令已被其他程序占用，无法恢复。'; return 1
        fi
        install -m 755 "$source/bin/xb" "$XB_PATH" || return 1
    elif management_shortcut_owned; then
        rm -f -- "$XB_PATH" || return 1
    fi
    if [[ -f "$source/systemd/xboard-node.service" ]]; then
        install -m 644 "$source/systemd/xboard-node.service" "$UNIT_PATH" || return 1
    else
        systemctl disable "$SERVICE" >/dev/null 2>&1 || true
        rm -f -- "$UNIT_PATH" || return 1
    fi
    systemctl daemon-reload || return 1
    if [[ -f "$UNIT_PATH" ]]; then
        if [[ "$enabled" == 1 ]]; then systemctl enable "$SERVICE" || return 1
        else systemctl disable "$SERVICE" || return 1; fi
        if [[ "$active" == 1 ]]; then
            systemctl reset-failed "$SERVICE" >/dev/null 2>&1 || true
            systemctl start "$SERVICE" && wait_running || return 1
        fi
    fi
}
restore_snapshot() {
    local requested=$1 selected rescue
    backup_resolve "$requested" || return 1
    selected="$RESOLVED_BACKUP"
    confirm "恢复 $requested 的程序、配置和服务状态？当前状态会先备份。" || return 0
    snapshot || return 1
    rescue="$BACKUP_LAST"
    if restore_files "$selected" "$rescue/pre-restore-files"; then
        say '恢复完成。'
    else
        say '恢复失败，尝试恢复操作前的快照。'
        restore_files "$rescue" "$rescue/failed-restore-files" || { fail "自动恢复未成功，请保留 $rescue 并检查日志。"; return 1; }
        return 1
    fi
}
select_backup() {
    local selected
    say "快照目录：$BACKUP_ROOT"
    if [[ -d "$BACKUP_ROOT" ]]; then
        find "$BACKUP_ROOT" -mindepth 1 -maxdepth 1 -type d -name 'menu-*' -printf '%f\n' | sort -r
    fi
    ask selected '输入要恢复的完整快照名称' || return 1
    [[ -n "$selected" ]] || return 0
    with_lock restore_snapshot "$selected"
}

read_binding() {
    local choice
    ask choice '模式：1=节点，2=机器' 1 || return 1
    case "$choice" in 1) MODE=node ;; 2) MODE=machine ;; *) fail '模式无效。'; return 1 ;; esac
    ask PANEL_URL '面板地址（https://...）' || return 1
    [[ "$PANEL_URL" =~ ^https?://[^[:space:]@?#]+$ ]] || { fail '地址需为 http(s) URL，且不能包含用户名、查询参数或片段。'; return 1; }
    if [[ "$PANEL_URL" == http://* ]]; then confirm 'HTTP 会明文传输通信信息，仍使用该地址？' || return 1; fi
    ask TARGET_ID '节点 ID / 机器 ID' || return 1
    [[ "$TARGET_ID" =~ ^[1-9][0-9]*$ ]] || { fail 'ID 必须是正整数。'; return 1; }
    ask KERNEL '核心：xray / singbox' xray || return 1
    [[ "$KERNEL" == xray || "$KERNEL" == singbox ]] || { fail '核心名称无效。'; return 1; }
    NODE_TYPE=vless
    if [[ "$MODE" == node ]]; then ask NODE_TYPE '节点协议' vless || return 1; fi
    [[ "$NODE_TYPE" =~ ^[a-z0-9_-]+$ ]] || { fail '协议名称无效。'; return 1; }
    read_token
}
install_local() {
    local paused="" candidate result=0
    need curl tar flock || return 1
    verify_bundle || return 1
    say "部署模式=$MODE，目标 ID=$TARGET_ID，核心=$KERNEL；密钥不会显示。"
    confirm '确认安装或添加该实例？' || return 0
    for candidate in V2bX.service v2bx.service; do
        if systemctl is-active --quiet "$candidate"; then
            if confirm "检测到 $candidate 正在运行，是否临时停止以避免端口冲突？"; then
                systemctl stop "$candidate" || return 1
                paused="$candidate"
            fi
            break
        fi
    done
    snapshot || { [[ -z "$paused" ]] || systemctl start "$paused"; return 1; }
    local -a options=(install --mode "$MODE" --panel "$PANEL_URL" --token-file "$TOKEN_PATH" --kernel "$KERNEL"
        --binary "$NODE_FILE" --xbctl-binary "$CLI_FILE" --skip-dependencies)
    if [[ "$MODE" == node ]]; then options+=(--node-id "$TARGET_ID" --node-type "$NODE_TYPE")
    else options+=(--machine-id "$TARGET_ID"); fi
    bash "$BUNDLE_DIR/install.sh" "${options[@]}" || result=$?
    if [[ "$result" == 0 ]]; then
        if [[ -n "$paused" ]] && confirm "新服务已启动，是否取消 $paused 的开机自启？"; then systemctl disable "$paused" || return 1; fi
        say "配置：$INSTALL_ROOT/config.yml；日志：journalctl -u $SERVICE -f"
        say '管理菜单：xb'
    elif [[ -n "$paused" ]]; then
        systemctl start "$paused" || fail "请手动恢复 $paused。"
    fi
    return "$result"
}
install_wizard() {
    choose_bundle && read_binding || { cleanup_token; return 1; }
    with_lock install_local
    local result=$?
    cleanup_token
    return "$result"
}
management_shortcut_owned() {
    [[ -f "$XB_PATH" && ! -L "$XB_PATH" ]] &&
        grep -Fqx '# Xboard-Node management shortcut' "$XB_PATH"
}

online_upgrade() {
    local helper="$INSTALL_ROOT/online-install.sh" version=latest
    if [[ "${1:-}" == --version && $# == 2 ]]; then version=$2
    elif [[ $# != 0 ]]; then fail '用法：xb update [--version VERSION]'; return 1; fi
    [[ "$version" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$ ]] || { fail '版本号无效。'; return 1; }
    [[ -f "$helper" ]] || { fail '在线更新入口缺失，请先用新版完整部署包升级一次。'; return 1; }
    # The downloaded bundle acquires the deployment lock when it is applied.
    # Do not hold that same lock while launching this nested deployment.
    bash "$helper" --version "$version" upgrade
}

upgrade_local() {
    [[ -f "$INSTALL_ROOT/config.yml" && -f "$UNIT_PATH" ]] || { fail '请先安装，升级不会创建新配置。'; return 1; }
    need curl || return 1
    verify_bundle || return 1
    confirm '使用此部署包升级程序并重启？现有配置将保留。' || return 0
    snapshot || return 1
    bash "$BUNDLE_DIR/install.sh" upgrade --binary "$NODE_FILE" --xbctl-binary "$CLI_FILE" --skip-dependencies || return 1
    say '更新完成，输入 xb 可打开管理菜单。'
}
binding_add() {
    [[ -x "$CLI_PATH" ]] || { fail '请先安装。'; return 1; }
    read_binding || { cleanup_token; return 1; }
    local action=add-node result
    [[ "$MODE" != machine ]] || action=add-machine
    local -a options=(bind "$action" --panel-url "$PANEL_URL" --token-file "$TOKEN_PATH" --kernel "$KERNEL")
    if [[ "$MODE" == node ]]; then options+=(--node-id "$TARGET_ID" --node-type "$NODE_TYPE")
    else options+=(--machine-id "$TARGET_ID"); fi
    snapshot && "$CLI_PATH" "${options[@]}"
    result=$?
    cleanup_token
    return "$result"
}
bindings_menu() {
    local choice panel id kind
    [[ -x "$CLI_PATH" ]] || { fail '请先安装。'; return 1; }
    "$CLI_PATH" list || return 1
    ask choice '实例：1=添加，2=移除节点，3=移除机器，0=返回' 0 || return 1
    case "$choice" in
        1) with_lock binding_add ;;
        2|3)
            ask panel '面板地址' && ask id '目标 ID' || return 1
            [[ "$id" =~ ^[1-9][0-9]*$ ]] || return 1
            confirm "移除目标 $id 并重启服务？" || return 0
            if [[ "$choice" == 2 ]]; then kind=node; else kind=machine; fi
            with_lock remove_binding "$kind" "$panel" "$id"
            ;;
        0) return 0 ;;
        *) fail '选项无效。' ;;
    esac
}
remove_binding() {
    snapshot || return 1
    "$CLI_PATH" bind "remove-$1" --panel "$2" "--$1-id" "$3"
}
show_status() {
    say "系统：$(uname -sr) / $(uname -m)"
    [[ ! -x "$NODE_PATH" ]] || "$NODE_PATH" -v
    systemctl status "$SERVICE" --no-pager || true
    [[ ! -x "$CLI_PATH" ]] || "$CLI_PATH" list || true
}
service_action() {
    local action=$1
    case "$action" in start|stop|restart|enable|disable) ;; *) return 1 ;; esac
    if [[ "$action" == stop || "$action" == restart ]]; then confirm '这会中断当前节点连接，继续？' || return 0; fi
    systemctl "$action" "$SERVICE"
}
edit_config() {
    [[ -f "$INSTALL_ROOT/config.yml" ]] || { fail '配置尚不存在。'; return 1; }
    local editor
    for editor in nano vi; do command -v "$editor" >/dev/null 2>&1 && break; done
    need "$editor" || return 1
    snapshot || return 1
    "$editor" "$INSTALL_ROOT/config.yml" || return 1
    chmod 600 "$INSTALL_ROOT/config.yml" || return 1
    say '已保存。请确认配置正确，再从菜单重启；需要回退时可恢复刚才的快照。'
}
dependencies() {
    need apt-get || return 1
    confirm '通过 apt 安装 curl、CA 证书、util-linux 和 unzip？' || return 0
    apt-get update && apt-get install -y curl ca-certificates util-linux unzip
}
diagnose() {
    show_status
    if command -v ss >/dev/null 2>&1; then ss -ltnp | head -25; fi
    if [[ -f "$INSTALL_ROOT/config.yml" ]]; then stat -c '配置权限：%a %n' "$INSTALL_ROOT/config.yml"; fi
    say '默认转发 auto；周期计费可切换 compatible。嗅探默认为 off。'
    say '程序运行不代表面板认证和代理流量已验证，请同时检查面板在线状态。'
}
uninstall_local() {
    local purge=${1:-0} installer="$INSTALL_ROOT/install.sh"
    [[ -f "$installer" ]] || installer="$BUNDLE_DIR/install.sh"
    [[ -f "$installer" ]] || { fail '找不到安装器。'; return 1; }
    confirm '卸载程序和服务？将先保存完整备份。' || return 0
    snapshot || return 1
    local -a options=(uninstall --yes --skip-dependencies)
    if [[ "$purge" == 1 ]]; then
        local answer
        read -r -p '同时删除配置与证书请输入 DELETE: ' answer || return 1
        [[ "$answer" == DELETE ]] || { say '已取消。'; return 0; }
        options+=(--purge)
    fi
    bash "$installer" "${options[@]}"
}
menu() {
    local choice log_choice
    while true; do
        printf '\n%s\n' '========== Xboard-Node 部署管理 =========='
        say ' 1. 本地安装 / 部署向导（节点或机器模式）'
        say ' 2. 在线更新至最新正式版（保留配置）'
        say ' 3. 查看状态 / 版本 / 实例'
        say ' 4. 启动服务      5. 停止服务      6. 重启服务'
        say ' 7. 查看日志      8. 编辑配置      9. 实例管理'
        say '10. 创建备份     11. 恢复备份     12. 环境诊断'
        say '13. 安装依赖     14. 卸载保留配置 15. 完整卸载'
        say '16. 开机自启     17. 取消自启     18. 本地包升级'
        say ' 0. 退出'
        read -r -p '请选择: ' choice || return 0
        case "$choice" in
            0) return 0 ;;
            1) install_wizard || say '部署未完成，请检查上方信息。' ;;
            2) online_upgrade || say '更新未完成，请检查上方信息。' ;;
            3) show_status ;;
            4) with_lock service_action start || true ;;
            5) with_lock service_action stop || true ;;
            6) with_lock service_action restart || true ;;
            7)
                ask log_choice '日志：1=最近 80 行，2=实时跟踪（Ctrl+C 返回）' 1 || continue
                if [[ "$log_choice" == 2 ]]; then journalctl -u "$SERVICE" -f || true
                else journalctl -u "$SERVICE" -n 80 --no-pager || true; fi
                ;;
            8) with_lock edit_config || true ;;
            9) bindings_menu || true ;;
            10) with_lock snapshot || true ;;
            11) select_backup || true ;;
            12) diagnose ;;
            13) with_lock dependencies || true ;;
            14) with_lock uninstall_local 0 || true ;;
            15) with_lock uninstall_local 1 || true ;;
            16) with_lock service_action enable || true ;;
            17) with_lock service_action disable || true ;;
            18) choose_bundle && with_lock upgrade_local || say '升级未完成，请检查上方信息。' ;;
            *) say '请选择菜单中的编号。' ;;
        esac
    done
}
usage() {
    cat <<'HELP'
Usage: sudo bash deploy.sh [--bundle-dir DIR] [COMMAND]
Shortcut: xb (menu), xb update [--version TAG], xb logs, xb uninstall
  menu (default), install, upgrade, status, check, start, stop, restart,
  logs, backup, restore BACKUP_NAME, bindings, edit, deps, uninstall, purge
Local bundles must include both binaries, install.sh, deploy.sh and SHA256SUMS.
Snapshots are retained under /var/backups/xboard-node, including after purge.
HELP
}
main() {
    local command=menu
    if [[ "${1:-}" == --bundle-dir ]]; then
        [[ $# -ge 2 ]] || { usage; return 1; }
        BUNDLE_DIR=$2; shift 2
    fi
    command=${1:-menu}
    if [[ $# -gt 0 ]]; then shift; fi
    case "$command" in help|-h|--help) usage; return 0 ;; esac
    linux_only && systemd_only || return 1
    if [[ "$command" != status && "$command" != check ]]; then root_only || return 1; fi
    trap cleanup_token EXIT
    trap 'cleanup_token; printf "\n已中断当前操作。\n"' INT
    trap 'cleanup_token; exit 143' TERM
    case "$command" in
        menu) menu ;;
        install) install_wizard ;;
        upgrade) verify_bundle && with_lock upgrade_local ;;
        online-upgrade|update) online_upgrade "$@" ;;
        status) show_status ;;
        check) diagnose ;;
        start|stop|restart) with_lock service_action "$command" ;;
        logs|log)
            if [[ "${1:-}" == -f || "${1:-}" == --follow ]]; then journalctl -u "$SERVICE" -n 80 -f
            else journalctl -u "$SERVICE" -n 80 --no-pager; fi
            ;;
        backup) with_lock snapshot ;;
        restore)
            [[ $# == 1 ]] || { usage; return 1; }
            with_lock restore_snapshot "$1"
            ;;
        bindings) bindings_menu ;;
        edit) with_lock edit_config ;;
        deps) with_lock dependencies ;;
        uninstall) with_lock uninstall_local 0 ;;
        purge) with_lock uninstall_local 1 ;;
        *) usage; return 1 ;;
    esac
}
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
