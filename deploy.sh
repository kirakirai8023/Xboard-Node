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
CERT_PROFILE_PATH=""
TOPOLOGY=direct
CDN_KIND=""
INSPECTION_FILE=""
CERT_NEEDED=0
REPORT_NODES=0
declare -a LISTENER_PORTS=()
declare -a LISTENER_PROTOCOLS=()
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
        rm -f -- "$TOKEN_PATH" "$TOKEN_DIR/dns-token" "$TOKEN_DIR/certificate.yml" "$TOKEN_DIR/inspection.tsv"
        rmdir -- "$TOKEN_DIR" 2>/dev/null || true
    fi
    TOKEN_DIR=""; TOKEN_PATH=""; CERT_PROFILE_PATH=""; INSPECTION_FILE=""
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
installation_state() {
    if [[ -f "$INSTALL_ROOT/config.yml" && -f "$UNIT_PATH" && -x "$NODE_PATH" && -x "$CLI_PATH" ]]; then
        printf installed
    elif [[ -f "$INSTALL_ROOT/config.yml" || -f "$UNIT_PATH" || -e "$NODE_PATH" || -e "$CLI_PATH" ]]; then
        printf partial
    else printf fresh; fi
}
read_topology() {
    local choice
    say '连接方式：1=服务器直连（含 Reality / TLS / UDP 协议）'
    say '          2=Cloudflare HTTPS 直接回源到核心'
    say '          3=Cloudflare → Nginx/Caddy → 核心'
    say '          4=其他 HTTP CDN / Cloudflare 自定义回源端口'
    say '          5=Nginx/Caddy 反代（不经过 CDN）'
    ask choice '选择连接方式' 1 || return 1
    CDN_KIND=""
    case "$choice" in
        1) TOPOLOGY=direct ;;
        2) TOPOLOGY=cloudflare; CDN_KIND=cloudflare ;;
        3) TOPOLOGY=proxy; CDN_KIND=cloudflare ;;
        4) TOPOLOGY=cdn; CDN_KIND=custom ;;
        5) TOPOLOGY=proxy ;;
        *) fail '连接方式无效。'; return 1 ;;
    esac
}
inspect_binding() {
    local kind id protocol network tls port listen domain path cert_mode panel_cert needs_cert users rest
    local -a options=(deploy inspect --panel-url "$PANEL_URL" --token-file "$TOKEN_PATH"
        --mode "$MODE" --kernel "$KERNEL" --topology "$TOPOLOGY" --output tsv)
    if [[ "$MODE" == machine ]]; then options+=(--machine-id "$TARGET_ID")
    else options+=(--node-id "$TARGET_ID" --node-type "$NODE_TYPE"); fi
    if [[ -f "$INSTALL_ROOT/config.yml" ]]; then
        options+=(--existing-config "$INSTALL_ROOT/config.yml" --credentials-in "$INSTALL_ROOT/credentials.env")
    fi
    INSPECTION_FILE="$TOKEN_DIR/inspection.tsv"
    say '正在读取面板实际配置和用户列表……'
    (umask 077; "$CLI_FILE" "${options[@]}" > "$INSPECTION_FILE") || {
        fail '面板预检未通过，请按上方具体原因修正后重试。'; return 1;
    }
    CERT_NEEDED=0; REPORT_NODES=0; NODE_DOMAIN=""; NODE_NETWORK=""; NODE_PATH_VALUE=""
    LISTENER_PORTS=(); LISTENER_PROTOCOLS=()
    while IFS=$'\t' read -r kind id protocol network tls port listen domain path cert_mode panel_cert needs_cert users rest; do
        case "$kind" in
            NODES) REPORT_NODES=$id ;;
            NODE)
                say "节点 $id：$protocol / $network，TLS=$tls，监听 $listen:$port，用户数=$users"
                say "  Host/SNI=$domain；路径=$path；证书=$cert_mode；面板管理证书=$panel_cert"
                LISTENER_PORTS+=("$port"); LISTENER_PROTOCOLS+=("$protocol")
                if [[ "$needs_cert" == true && "$cert_mode" == none ]]; then CERT_NEEDED=$((CERT_NEEDED+1)); fi
                NODE_DOMAIN=$domain; NODE_NETWORK=$network; NODE_PATH_VALUE=$path
                ;;
            WARNING) say "提示：$id" ;;
        esac
    done < "$INSPECTION_FILE"
    [[ "$REPORT_NODES" =~ ^[0-9]+$ ]] || return 1
    if [[ "$REPORT_NODES" == 0 ]]; then
        confirm '当前机器没有分配节点，仍安装等待面板后续下发？' || return 1
    fi
    if [[ "$MODE" == machine && "$REPORT_NODES" -gt 1 && "$CERT_NEEDED" -gt 0 ]]; then
        fail '多节点机器需要在面板为每个 TLS 节点设置证书策略（cert_config），以免共用错误域名。设置后重新运行向导。'; return 1
    fi
}
port_listeners() {
    local port=$1 socket=${2:-tcp}
    if [[ "$socket" == udp ]]; then ss -H -lunp "sport = :$port"
    else ss -H -ltnp "sport = :$port"; fi
}
check_listener_ports() {
    need ss || return 1
    local index socket listeners
    for index in "${!LISTENER_PORTS[@]}"; do
        socket=tcp
        case "${LISTENER_PROTOCOLS[index]}" in hysteria|hysteria2|tuic) socket=udp ;; esac
        listeners=$(port_listeners "${LISTENER_PORTS[index]}" "$socket") || return 1
        if [[ -n "$listeners" ]]; then
            say "端口 ${LISTENER_PORTS[index]}/$socket 已有监听："
            say "$listeners"
            confirm '确认这些监听属于将被替换的实例或 V2bX，继续？（向导不会停止 Nginx/Caddy 等其他程序）' || return 1
        fi
    done
}
read_certificate() {
    [[ "$CERT_NEEDED" -gt 0 ]] || return 0
    local choice domain cert_file key_file email secret listeners
    say '该节点尚未配置可用证书。选择以下方式后，将由本机管理此实例的证书（cert.source=local）。'
    say '本实例的证书将使用本地设置，面板的 cert_config 不再覆盖它；传输和 TLS 开关仍由面板管理。'
    say '证书来源：1=已有 PEM 文件（含 Cloudflare Origin CA），2=Cloudflare DNS 自动申请并续期'
    say '          3=HTTP 自动申请并续期（公网 80 端口）'
    ask choice '选择证书来源' 1 || return 1
    [[ "$NODE_DOMAIN" != - ]] || NODE_DOMAIN=""
    ask domain '源站证书覆盖的域名（不含 https:// 或路径）' "$NODE_DOMAIN" || return 1
    local -a options=(deploy cert --source local --domain "$domain" --output "$TOKEN_DIR/certificate.yml")
    case "$choice" in
        1)
            ask cert_file '证书 PEM 的绝对路径' || return 1
            ask key_file '私钥 PEM 的绝对路径' || return 1
            [[ "$cert_file" == /* && "$key_file" == /* ]] || { fail '请使用绝对路径，保证服务重启后仍能读取。'; return 1; }
            options+=(--mode file --cert-file "$cert_file" --key-file "$key_file")
            ;;
        2)
            ask email 'ACME 邮箱（可留空）' || return 1
            say 'Cloudflare API Token 需要该域名所在 Zone 的 DNS Edit 和 Zone Read 权限。'
            IFS= read -r -s -p 'Cloudflare API Token（输入隐藏）: ' secret || return 1
            printf '\n'
            [[ -n "$secret" && "$secret" != *$'\r'* ]] || return 1
            (umask 077; printf '%s' "$secret" > "$TOKEN_DIR/dns-token") || return 1
            unset secret
            options+=(--mode dns --email "$email" --dns-token-file "$TOKEN_DIR/dns-token")
            ;;
        3)
            listeners=$(port_listeners 80 tcp) || return 1
            [[ -z "$listeners" ]] || { say "$listeners"; fail '80 端口已被占用，请选 DNS 自动申请或已有证书。'; return 1; }
            say 'HTTP-01 需要公网 80 端口可达，且 /.well-known/acme-challenge/ 能到达本机；CDN/WAF/反代规则也需放行。'
            confirm '已确认域名解析、路由和 80 端口可达？' || return 1
            ask email 'ACME 邮箱（可留空）' || return 1
            options+=(--mode http --email "$email" --http-port 80)
            ;;
        *) fail '证书选项无效。'; return 1 ;;
    esac
    "$CLI_FILE" "${options[@]}" || return 1
    CERT_PROFILE_PATH="$TOKEN_DIR/certificate.yml"
    say '证书策略已验证；自动申请会在服务启动时进行。DNS Token 仅写入受限配置文件。'
}
topology_guidance() {
    if [[ "$CDN_KIND" == cloudflare ]]; then
        say 'Cloudflare：域名开启代理（橙云），SSL/TLS 设为 Full (strict)。'
        say '源站可用公共 CA 或 Cloudflare Origin CA 证书；客户端 Host/SNI 和证书域名需匹配。'
        say '同一条经 Cloudflare 终止 TLS 的链路使用 TLS，不能同时使用 Reality。'
    fi
    if [[ "$TOPOLOGY" != direct ]]; then
        say '面板与客户端的传输、Host、路径需一致；XHTTP 的 flow 留空，CDN 场景可先使用 packet-up。'
        say '请给节点路径配置缓存绕过，并检查 WAF/访问挑战规则；gRPC 传输需开启 CDN 的 gRPC 支持。'
    fi
    if [[ "$TOPOLOGY" == proxy ]]; then
        say 'Nginx/Caddy 负责外部 HTTPS；面板应填写核心实际监听的内部端口和 TLS 模式。'
        say '建议核心监听 127.0.0.1 的独立端口；反代需保留路径、Host 并关闭请求/响应缓冲。'
        say '向导会保留现有 Nginx/Caddy，请先自行配置对应站点和源站证书。'
    elif [[ "$CDN_KIND" == custom ]]; then
        say '请确认 CDN 支持所选 HTTP 传输和回源端口；自定义回源端口需另配 Origin Rule。'
    fi
}
prepare_deployment() {
    if ! command -v ss >/dev/null 2>&1; then
        say '需要 iproute2 提供的 ss 来检查端口占用。'
        need apt-get || return 1
        confirm '安装缺少的 iproute2 依赖后继续？' || return 1
        apt-get update && apt-get install -y iproute2 || return 1
    fi
    read_topology && inspect_binding && check_listener_ports && read_certificate || return 1
    topology_guidance
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
    if [[ -n "$CERT_PROFILE_PATH" ]]; then options+=(--cert-profile "$CERT_PROFILE_PATH"); fi
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
    if [[ ! -f "$BUNDLE_DIR/SHA256SUMS" && -f "$INSTALL_ROOT/online-install.sh" ]]; then
        say '管理目录不保存发行包。可以在线下载完整部署包后添加或重新配置实例。'
        if confirm '在线下载最新正式版并进入部署向导？'; then
            bash "$INSTALL_ROOT/online-install.sh" install
            return $?
        fi
    fi
    choose_bundle && read_binding && prepare_deployment || { cleanup_token; return 1; }
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
    [[ -f "$INSTALL_ROOT/config.yml" ]] || { fail '没有现有配置，请使用首次安装向导。'; return 1; }
    need curl || return 1
    verify_bundle || return 1
    confirm '使用此部署包升级程序并重启？现有配置将保留。' || return 0
    snapshot || return 1
    bash "$BUNDLE_DIR/install.sh" upgrade --binary "$NODE_FILE" --xbctl-binary "$CLI_FILE" --skip-dependencies || return 1
    say '更新完成，输入 xb 可打开管理菜单。'
}
upgrade_entry() {
    if [[ ! -f "$INSTALL_ROOT/config.yml" ]]; then
        say '未找到现有配置，当前服务器需要首次安装。'
        confirm '改为进入首次部署向导？' || return 1
        install_wizard
    else
        if [[ "$(installation_state)" == partial ]]; then say '检测到安装不完整，将保留配置并修复程序和服务。'; fi
        verify_bundle && with_lock upgrade_local
    fi
}
binding_add() {
    [[ -x "$CLI_PATH" ]] || { fail '请先安装。'; return 1; }
    CLI_FILE="$CLI_PATH"
    read_binding && prepare_deployment || { cleanup_token; return 1; }
    local action=add-node result
    [[ "$MODE" != machine ]] || action=add-machine
    local -a options=(bind "$action" --panel-url "$PANEL_URL" --token-file "$TOKEN_PATH" --kernel "$KERNEL")
    if [[ "$MODE" == node ]]; then options+=(--node-id "$TARGET_ID" --node-type "$NODE_TYPE")
    else options+=(--machine-id "$TARGET_ID"); fi
    if [[ -n "$CERT_PROFILE_PATH" ]]; then options+=(--cert-profile "$CERT_PROFILE_PATH"); fi
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
    if ! service_unit_ready; then
        if [[ "$UNIT_LOAD_STATE" == masked ]]; then
            fail '服务被 systemd 屏蔽（masked），请先检查屏蔽原因。'; return 1
        fi
        if [[ "$action" == stop || "$action" == disable ]]; then
            fail '服务尚未注册，无需执行此操作。'; return 1
        fi
        repair_service_unit || return 1
        if [[ "$action" == start || "$action" == restart ]]; then return 0; fi
    fi
    if [[ "$action" == stop || "$action" == restart ]]; then confirm '这会中断当前节点连接，继续？' || return 0; fi
    systemctl "$action" "$SERVICE"
}
service_unit_ready() {
    UNIT_LOAD_STATE=$(systemctl show --property=LoadState --value "$SERVICE" 2>/dev/null) || true
    if [[ "$UNIT_LOAD_STATE" != loaded && "$UNIT_LOAD_STATE" != masked && -f "$UNIT_PATH" && ! -L "$UNIT_PATH" ]]; then
        systemctl daemon-reload || return 1
        UNIT_LOAD_STATE=$(systemctl show --property=LoadState --value "$SERVICE" 2>/dev/null) || true
    fi
    [[ "$UNIT_LOAD_STATE" == loaded ]]
}
repair_service_unit() {
    local candidate installer=""
    [[ -f "$INSTALL_ROOT/config.yml" && -x "$NODE_PATH" ]] || {
        fail '程序或配置缺失，请使用菜单 1 完成安装；已有配置也可以通过菜单 2 在线升级修复。'; return 1;
    }
    for candidate in "$BUNDLE_DIR/install.sh" "$INSTALL_ROOT/install.sh"; do
        if [[ -f "$candidate" ]] && bash "$candidate" help | grep -Eq '^[[:space:]]+repair[[:space:]]'; then
            installer=$candidate; break
        fi
    done
    [[ -n "$installer" ]] || { fail '当前安装器不支持服务修复，请先通过在线升级获取新版管理脚本。'; return 1; }
    say '将重建 xboard-node.service 并启动现有程序，保留配置、密钥和开机自启设置。'
    confirm '修复服务并启动？' || return 1
    snapshot || return 1
    bash "$installer" repair --skip-dependencies || return 1
    say '服务修复完成。可以使用菜单 3 查看状态，菜单 7 查看日志。'
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
    confirm '通过 apt 安装 curl、CA 证书、util-linux、unzip 和 iproute2？' || return 0
    apt-get update && apt-get install -y curl ca-certificates util-linux unzip iproute2
}
diagnose() {
    show_status
    if command -v ss >/dev/null 2>&1; then ss -ltnp | head -25; fi
    if [[ -f "$INSTALL_ROOT/config.yml" ]]; then stat -c '配置权限：%a %n' "$INSTALL_ROOT/config.yml"; fi
    say '默认转发 auto；周期计费可切换 compatible。嗅探默认为 off。'
    say '程序运行不代表面板认证和代理流量已验证，请同时检查面板在线状态。'
}
panel_diagnose() {
    [[ -x "$CLI_PATH" && -f "$INSTALL_ROOT/config.yml" ]] || { fail '请先安装实例。'; return 1; }
    read_topology || return 1
    "$CLI_PATH" deploy inspect --config "$INSTALL_ROOT/config.yml" --credentials-in "$INSTALL_ROOT/credentials.env" --topology "$TOPOLOGY" || return 1
    topology_guidance
    say '预检验证配置与面板认证；公网 CDN、TLS 握手和真实代理流量仍需用客户端测试。'
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
        say "安装状态：$(installation_state)；配置目录：$INSTALL_ROOT"
        say ' 1. 本地安装 / 部署向导（节点或机器模式）'
        say ' 2. 在线更新至最新正式版（保留配置）'
        say ' 3. 查看状态 / 版本 / 实例'
        say ' 4. 启动服务      5. 停止服务      6. 重启服务'
        say ' 7. 查看日志      8. 编辑配置      9. 实例管理'
        say '10. 创建备份     11. 恢复备份     12. 环境诊断'
        say '13. 安装依赖     14. 卸载保留配置 15. 完整卸载'
        say '16. 开机自启     17. 取消自启     18. 本地包升级'
        say '19. 面板 / TLS / CDN 配置预检'
        say '20. 修复服务注册并启动（保留配置和程序）'
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
            18) choose_bundle && upgrade_entry || say '升级未完成，请检查上方信息。' ;;
            19) panel_diagnose || say '预检未通过，请检查上方信息。' ;;
            20) with_lock repair_service_unit || say '修复未完成，请检查上方信息。' ;;
            *) say '请选择菜单中的编号。' ;;
        esac
    done
}
usage() {
    cat <<'HELP'
Usage: sudo bash deploy.sh [--bundle-dir DIR] [COMMAND]
Shortcut: xb (menu), xb update [--version TAG], xb logs, xb uninstall
  menu (default), install, upgrade, status, check, start, stop, restart,
  logs, backup, restore BACKUP_NAME, bindings, edit, deps, preflight, repair, uninstall, purge
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
        upgrade) upgrade_entry ;;
        online-upgrade|update) online_upgrade "$@" ;;
        status) show_status ;;
        check) diagnose ;;
        preflight) panel_diagnose ;;
        repair) with_lock repair_service_unit ;;
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
