#!/usr/bin/env bash
#
# install.sh — installer for monetarium-node + monetarium-wallet + monetarium-ctl
#
# Flow:
#   1. Detect OS (Linux/macOS) and arch, download the right binaries.
#   2. Prompt for the wallet's private passphrase (hidden input).
#   3. Write that passphrase into the wallet's config file so it can
#      auto-unlock on start (needed for unattended ticket buying / voting).
#   4. Install + enable a systemd service (Linux) or launchd daemon (macOS)
#      that starts node + wallet and unlocks the wallet automatically.
#   5. Print a large, impossible-to-miss warning about what this implies.
#
# ⚠️  Read the warning at the bottom of this file (and the one the script
#     prints) before running this on a machine that holds real funds.

set -euo pipefail

# --------------------------------------------------------------------------
# Runtime user
# --------------------------------------------------------------------------
# This script normally runs as the target user and calls sudo for the steps
# that need root. When it IS run through sudo, $HOME points at root, so every
# path below would resolve under /root: detection would miss the real install
# and create a second one. Resolve the invoking user instead.
_home_for_user() {
    local user="$1"
    if command -v getent >/dev/null 2>&1; then
        getent passwd "$user" | cut -d: -f6
        return
    fi
    if command -v dscl >/dev/null 2>&1; then
        dscl . -read "/Users/$user" NFSHomeDirectory 2>/dev/null | awk '{print $2; exit}'
        return
    fi
    return 1
}

# resolve_runtime_user sets RUN_USER, RUN_UID, RUN_GID and RUN_HOME for the
# user who invoked the script, preferring SUDO_USER when run through sudo.
resolve_runtime_user() {
    RUN_USER="$(id -un)"
    RUN_UID="$(id -u)"
    RUN_GID="$(id -g)"
    RUN_HOME="${HOME:-}"
    if [[ "$RUN_UID" -eq 0 && -n "${SUDO_USER:-}" && "${SUDO_USER}" != "root" ]]; then
        local home uid gid
        home="$(_home_for_user "$SUDO_USER" || true)"
        uid="$(id -u "$SUDO_USER" 2>/dev/null || true)"
        gid="$(id -g "$SUDO_USER" 2>/dev/null || true)"
        if [[ -n "$home" && -d "$home" && -n "$uid" && "$uid" != "0" ]]; then
            RUN_USER="$SUDO_USER"
            RUN_UID="$uid"
            RUN_GID="$gid"
            RUN_HOME="$home"
        fi
    fi
}

resolve_runtime_user

# --------------------------------------------------------------------------
# Config
# --------------------------------------------------------------------------
GITHUB_ORG="monetarium"
REPO_NODE="monetarium-node"
REPO_WALLET="monetarium-wallet"
REPO_CTL="monetarium-ctl"

INSTALL_DIR="/usr/local/bin"

# Overridable so the unit tests can point service detection at a temp tree.
SYSTEMD_UNIT_DIR="${SYSTEMD_UNIT_DIR:-/etc/systemd/system}"
LAUNCHD_DIR="${LAUNCHD_DIR:-${RUN_HOME}/Library/LaunchAgents}"

detect_data_dir() {
    if [[ -n "${MONETARIUM_HOME:-}" ]]; then
        echo "$MONETARIUM_HOME"
        return
    fi
    local home="$RUN_HOME"
    case "$(uname -s)" in
        Darwin) echo "${home}/Library/Application Support/Monetarium" ;;
        *)      echo "${home}/.monetarium" ;;
    esac
}
DATA_DIR="$(detect_data_dir)"
WALLET_CONF="$DATA_DIR/monetarium-wallet.conf"
NODE_CONF="$DATA_DIR/monetarium.conf"

# ctl config — written to the ctl's own appdata dir so "monetarium-ctl"
# works without --configfile on every invocation.
# Go's dcrutil.AppDataDir("monetarium-ctl", false) returns:
#   Linux:   ~/.monetarium-ctl
#   macOS:   ~/Library/Application Support/Monetarium-ctl
detect_ctl_data_dir() {
    if [[ -n "${MONETARIUM_CTL_HOME:-}" ]]; then
        echo "$MONETARIUM_CTL_HOME"
        return
    fi
    local home="$RUN_HOME"
    case "$(uname -s)" in
        Darwin) echo "${home}/Library/Application Support/Monetarium-ctl" ;;
        *)      echo "${home}/.monetarium-ctl" ;;
    esac
}
CTL_DIR="$(detect_ctl_data_dir)"
CTL_CONF="$CTL_DIR/monetarium-ctl.conf"

MANIFEST="$DATA_DIR/install.manifest"

# Mining & ticket buyer settings (set by prompts)
MINING_ENABLED=false
MINING_CORES=""
TICKETS_ENABLED=false
TICKET_LIMIT=""
TICKET_BALANCE=""
VOTING_ENABLED=false

# Wallet addresses (populated after services start)
MINING_ADDR=""
CONSOLIDATION_ADDR=""

RPC_USER="monetarium"
RPC_PASS=""

# Set on script start (defaults). RPC credentials are reused on re-runs —
# see ensure_rpc_credentials() below.
# Exported because the test suite (tests/test-install-*.sh) sources this file
# and reads it directly; without `export`, shellcheck flags it as unused since
# main() keys restarts off NODE_REPLACED/WALLET_REPLACED instead.
export BINARIES_REPLACED=false
# Per-binary replacement flags: restart only the service(s) whose binary
# actually changed (see restart_monetarium_services in main()).
NODE_REPLACED=false
WALLET_REPLACED=false

# --------------------------------------------------------------------------
# Helpers
# --------------------------------------------------------------------------
red()    { printf '\033[1;31m%s\033[0m\n' "$*"; }
green()  { printf '\033[1;32m%s\033[0m\n' "$*"; }
info()  { printf '\033[1;34m[install]\033[0m %s\n' "$*"; }
warn()  { printf '\033[1;33m[install]\033[0m %s\n' "$*"; }
die()   { red "ERROR: $*"; exit 1; }

require_cmd() {
    command -v "$1" >/dev/null 2>&1 || die "Required command '$1' not found. Please install it and re-run."
}

# Return 0 if a SHA-256 tool is available, else die with an actionable message.
# Both checksums and the verified-download path depend on hashing, so a host
# with neither `sha256sum` (coreutils) nor `shasum` (BSD/macOS) must fail loud
# instead of falling through to a misleading "Checksum mismatch ... got .".
require_hasher() {
    command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 \
        || die "No SHA-256 tool found. Install sha256sum (coreutils) or shasum and re-run."
}

# Echo the byte offset of the LAST (literal, non-regex) occurrence of $1 in
# $2, or -1. Pure-bash so it is safe under `set -euo pipefail` (grep -bo would
# vary across grep implementations and complicate the pipefail handling).
_strrpos() {
    local needle="$1" hay="$2" off=-1 len
    len="${#2}"
    [[ -z "$needle" ]] && { printf '%s\n' -1; return; }
    while [[ "$hay" == *"$needle"* ]]; do
        local head="${hay%%"$needle"*}"
        off=$(( len - ${#hay} + ${#head} ))
        hay="${hay:$(( ${#head} + ${#needle} ))}"
    done
    printf '%s\n' "$off"
}

has_tty() {
    { exec 3</dev/tty; } 2>/dev/null && exec 3<&- && return 0
    return 1
}

# --------------------------------------------------------------------------
# 1. Detect OS / arch
# --------------------------------------------------------------------------
detect_platform() {
    local os arch
    case "$(uname -s)" in
        Linux)  os="linux" ;;
        Darwin) os="darwin" ;;
        *) die "Unsupported OS: $(uname -s). This script supports Linux and macOS only." ;;
    esac

    case "$(uname -m)" in
        x86_64|amd64)  arch="amd64" ;;
        arm64|aarch64) arch="arm64" ;;
        *) die "Unsupported architecture: $(uname -m)" ;;
    esac

    echo "${os}-${arch}"
}

# --------------------------------------------------------------------------
# 2. Download binaries (checksum-verified, self-updating on re-run)
# --------------------------------------------------------------------------
latest_release_asset() {
    # $1 = repo name, $2 = platform string (e.g. linux_amd64)
    # Prints "sha256:<hex>\t<url>" for the matching release asset (digest may
    # be empty for very old uploads).
    # Exit status:
    #   0 — asset found
    #   1 — could not reach the GitHub API (transient; caller decides)
    #   2 — API reachable, but no asset matches this platform
    local repo="$1" platform="$2"
    local api="https://api.github.com/repos/${GITHUB_ORG}/${repo}/releases/latest"

    local json
    json="$(curl -fsSL "$api" 2>/dev/null)" || return 1

    # Match the asset's "browser_download_url" FIELD token (not the bare URL) so
    # that a release `body` which repeats the download link verbatim does not
    # mis-anchor the per-asset slice. grep -o emits the whole field+value;
    # head -n1 picks the first platform match.
    local match url
    match="$(printf '%s' "$json" \
        | grep -oE "\"browser_download_url\"[[:space:]]*:[[:space:]]*\"[^\"]*${platform}[^\"]*\"" \
        | head -n1)" || true

    if [[ -z "$match" ]]; then
        echo "Could not find a release asset for ${repo} matching platform '${platform}'. Check https://github.com/${GITHUB_ORG}/${repo}/releases manually." >&2
        return 2
    fi

    # Pull just the URL value out of the matched token.
    url="$(printf '%s' "$match" \
        | sed -E 's/.*"browser_download_url"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')"

    # Scope the digest lookup to the matched asset's OWN JSON object instead
    # of relying on GitHub's (undocumented) field ordering. The old "last
    # sha256 before the URL" pairing broke whenever a neighbouring asset's
    # digest sat between this asset's fields, and `${json%%"$url"*}` broke when
    # the URL was also repeated in the release body. Bash has no JSON parser,
    # so (quote/backslash aware, unlike the naive brace counter):
    #   1. Anchor on the matched field token — the LAST occurrence, since the
    #      assets array follows the body, so the genuine field is the last copy
    #      of this literal in the document.
    #   2. The asset object's opening '{' is the innermost currently-open '{'
    #      at the anchor (top of a brace stack built scanning forward).
    #   3. The matching '}' is the forward scan that returns the stack to empty.
    #   4. grep the digest only inside that object.
    local anchor
    anchor="$(_strrpos "$match" "$json")"
    if [[ "$anchor" -lt 0 ]]; then
        echo "Could not find a release asset for ${repo} matching platform '${platform}'. Check https://github.com/${GITHUB_ORG}/${repo}/releases manually." >&2
        return 2
    fi

    local i ch in_str=0 esc=0
    local opens="" d=0   # opens = space-separated positions of unclosed '{'

    # Forward pass over [0, anchor): build the brace stack, quote/backslash
    # aware so braces inside string values are ignored.
    for ((i = 0; i < anchor; i++)); do
        ch="${json:i:1}"
        if (( in_str )); then
            if (( esc )); then esc=0
            elif [[ "$ch" == "\\" ]]; then esc=1
            elif [[ "$ch" == "\"" ]]; then in_str=0
            fi
        else
            case "$ch" in
                '"') in_str=1 ;;
                '{') opens="$opens $i" ;;
                '}')
                    [[ -n "$opens" ]] && opens="${opens% *}" || d=$((d + 1))
                    ;;
            esac
        fi
    done

    # The innermost open '{' before the anchor is the asset object's brace.
    # (opens is empty only if the anchor is not inside an object — malformed.)
    if [[ -z "$opens" ]]; then
        echo "Could not find a release asset for ${repo} matching platform '${platform}'. Check https://github.com/${GITHUB_ORG}/${repo}/releases manually." >&2
        return 2
    fi
    local open="${opens##* }"   # last token = innermost open brace position

    # Forward pass from open: find the matching '}' at the same depth, again
    # quote/backslash aware. Track depth relative to the asset object.
    in_str=0; esc=0; d=1
    local end=-1
    for ((i = open + 1; i < ${#json}; i++)); do
        ch="${json:i:1}"
        if (( in_str )); then
            if (( esc )); then esc=0
            elif [[ "$ch" == "\\" ]]; then esc=1
            elif [[ "$ch" == "\"" ]]; then in_str=0
            fi
        else
            case "$ch" in
                '"') in_str=1 ;;
                '{') d=$((d + 1)) ;;
                '}')
                    if (( d == 1 )); then end=$i; break
                    else d=$((d - 1)); fi
                    ;;
            esac
        fi
    done

    if [[ "$end" -lt 0 ]]; then
        echo "Could not find a release asset for ${repo} matching platform '${platform}'. Check https://github.com/${GITHUB_ORG}/${repo}/releases manually." >&2
        return 2
    fi

    local obj="${json:open:$((end - open + 1))}"
    local digest
    digest="$(printf '%s' "$obj" \
        | grep -oE '"digest":[[:space:]]*"sha256:[0-9a-fA-F]+"' \
        | head -n1 \
        | sed -E 's/.*"sha256:([0-9a-fA-F]+)".*/\1/')" || true

    printf 'sha256:%s\t%s\n' "$digest" "$url"
}

sha256_of() {
    # $1 = file; prints the lowercase hex SHA-256. Coreutils on Linux,
    # BSD/macOS ships shasum.
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1"
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1"
    else
        return 1
    fi
}

lc() { printf '%s' "$1" | tr 'A-F' 'a-f'; }

download_binary() {
    # $1 = repo, $2 = binary name to install, $3 = platform,
    # $4 = optional staging dir. With a staging dir the verified binary is
    # copied there instead of installed, so callers can fetch everything
    # before touching the live install.
    # Re-runs self-update: the asset's published sha256 is compared against
    # the installed binary, and the download is skipped when they match.
    # The downloaded file is hash-verified before it is ever installed.
    local repo="$1" bin_name="$2" platform="$3" stage_dir="${4:-}"
    local dest="${INSTALL_DIR}/${bin_name}"
    local had_binary=false
    [[ -f "$dest" ]] && had_binary=true

    local asset_info url digest rc is_archive
    rc=0
    asset_info="$(latest_release_asset "$repo" "$platform" 2>/dev/null)" || rc=$?

    if [[ $rc -eq 2 ]]; then
        # API reachable but this platform has no asset — a real problem,
        # not a transient outage, so refuse regardless of install state.
        die "No release asset for ${bin_name} matching platform '${platform}'. Check https://github.com/${GITHUB_ORG}/${repo}/releases manually."
    fi
    if [[ $rc -ne 0 ]]; then
        # rc == 1: GitHub unreachable.
        if $IS_UPDATE_MODE; then
            die "Could not reach GitHub to check for ${bin_name} updates. Aborting the update; nothing was changed."
        fi
        if $had_binary; then
            info "Skipping ${bin_name} - could not reach GitHub to check for updates (already installed)."
            return
        fi
        die "Could not fetch latest release info for ${repo}."
    fi

    digest="${asset_info%%$'\t'*}"
    digest="${digest#sha256:}"
    url="${asset_info#*$'\t'}"

    is_archive=false
    case "$url" in
        *.tar.gz|*.tgz|*.zip) is_archive=true ;;
    esac

    # No digest published for this asset? On a RE-RUN keep whatever is already
    # installed (we can't verify currency against a missing checksum). On a
    # FRESH install we must not hard-refuse: releases that don't publish a
    # checksum would then break *every* install against them, so warn and
    # proceed — relying on transport TLS exactly like the pre-checksum
    # installer did. Verification only kicks in when a digest IS present.
    if [[ -z "$digest" ]]; then
        if $had_binary; then
            info "Skipping ${bin_name} - release asset has no published checksum; keeping existing binary."
            return
        fi
        warn "No sha256 checksum published for ${bin_name}; installing without verification."
    fi

    # Skip-when-current only applies to raw binaries: the published digest is
    # of the exact bytes that get installed. For archive assets (.tar.gz/.tgz
    # /zip) the digest covers the ARCHIVE, which never equals the extracted
    # binary — so download + verify every run instead of pretending re-runs
    # are a no-op.
    if $had_binary && ! $is_archive; then
        local current
        current="$(sha256_of "$dest" 2>/dev/null | awk '{print $1}' || true)"
        if [[ -n "$current" ]] && [[ "$(lc "$current")" == "$(lc "$digest")" ]]; then
            info "Skipping ${bin_name} - already current at $dest"
            return
        fi
        info "Update available for ${bin_name} (installed hash differs from release asset)."
    fi

    local tmpdir archive
    tmpdir="$(mktemp -d)"
    # RETURN fires both on a normal return and on the errexit-induced return from
    # a failing `sudo mkdir/mv` (so set -e never leaks tmpdir). `die` uses
    # `exit`, which bypasses RETURN, so every die below still rm's explicitly.
    # Guard the expansion: when download_binary is itself called from inside
    # another function, the RETURN trap can run after this local has been torn
    # down, and an unbound "$tmpdir" would abort the script under `set -u`.
    trap '[[ -n "${tmpdir:-}" ]] && rm -rf "$tmpdir"' RETURN
    archive="$tmpdir/$(basename "$url")"

    info "Downloading ${bin_name} from: $url"
    if ! curl -fsSL -o "$archive" "$url"; then
        rm -rf "$tmpdir"
        die "Failed to download ${bin_name} from ${url}."
    fi

    # Integrity check: only when a digest was published. With no digest we
    # already warned and are installing on transport TLS trust alone, so
    # there is nothing to compare against.
    if [[ -n "$digest" ]]; then
        local got
        got="$(sha256_of "$archive" 2>/dev/null | awk '{print $1}' || true)"
        if [[ "$(lc "$got")" != "$(lc "$digest")" ]]; then
            rm -rf "$tmpdir"
            die "Checksum mismatch for ${bin_name}: expected sha256:${digest}, got ${got}. Refusing to install a corrupted/downloaded binary."
        fi
        info "Checksum verified for ${bin_name} (sha256:${digest})."
    fi

    case "$archive" in
        *.tar.gz|*.tgz)
            if ! tar -xzf "$archive" -C "$tmpdir"; then
                rm -rf "$tmpdir"
                die "Failed to extract downloaded archive ${archive}."
            fi
            ;;
        *.zip)
            require_cmd unzip
            if ! unzip -q "$archive" -d "$tmpdir"; then
                rm -rf "$tmpdir"
                die "Failed to extract downloaded archive ${archive}."
            fi
            ;;
        *)
            # Assume it's a raw binary
            cp "$archive" "$tmpdir/$bin_name"
            ;;
    esac

    local found
    found="$(find "$tmpdir" -type f -name "$bin_name" | head -n1)"
    [[ -n "$found" ]] || { rm -rf "$tmpdir"; die "Could not locate '${bin_name}' binary inside downloaded archive."; }

    chmod +x "$found"

    if [[ -n "$stage_dir" ]]; then
        mkdir -p "$stage_dir"
        cp "$found" "$stage_dir/$bin_name"
        chmod +x "$stage_dir/$bin_name"
        rm -rf "$tmpdir"
        info "Staged ${bin_name} for update."
        return
    fi

    sudo mkdir -p "$INSTALL_DIR"
    sudo mv "$found" "${INSTALL_DIR}/${bin_name}"
    rm -rf "$tmpdir"

    info "Installed ${bin_name} -> ${INSTALL_DIR}/${bin_name}"
    # If a service was already running, flag that it must be restarted so it
    # runs the new binary (handled in main after the services are re-installed).
    if $had_binary; then
        case "$bin_name" in
            monetarium-node)   NODE_REPLACED=true   ;;
            monetarium-wallet) WALLET_REPLACED=true ;;
        esac
        # Aggregate of the per-binary flags above; read by the test suite, not
        # by main() (which keys restarts off NODE_REPLACED/WALLET_REPLACED).
        BINARIES_REPLACED=true
    fi
}

restart_monetarium_services() {
    # $@ = service daemon labels whose binary was replaced. Restart only
    # those so they pick up the freshly-updated executable; services whose
    # binary did not change keep running untouched. Nothing to do when nothing
    # changed (fresh installs start clean; monetarium-ctl has no service).
    [[ $# -eq 0 ]] && return

    case "$(uname -s)" in
        Linux)
            if systemctl is-active --quiet monetarium-node 2>/dev/null \
                || systemctl is-active --quiet monetarium-wallet 2>/dev/null; then
                info "Binaries updated - restarting monetarium services."
                local svc
                for svc in "$@"; do
                    sudo systemctl restart "$svc" 2>/dev/null || true
                done
            fi
            ;;
        Darwin)
            local svc label
            for svc in "$@"; do
                case "$svc" in
                    monetarium-node)   label="com.monetarium.node" ;;
                    monetarium-wallet) label="com.monetarium.wallet" ;;
                    *) continue ;;
                esac
                launchctl kickstart -k "gui/${RUN_UID}/$label" 2>/dev/null || true
            done
            ;;
    esac
}

# --------------------------------------------------------------------------
# 2c. Update mode: re-running on an existing node
# --------------------------------------------------------------------------
# Re-running this script on an existing installation must behave as an
# updater, not as a reinstall. The update path only downloads the release
# binaries, swaps them into place and restarts the services whose binary
# changed. It never touches configs, the wallet, the service units or the
# manifest, and it never asks for the passphrase.

IS_UPDATE_MODE=false
STAGING_DIR=""

# service_unit_paths prints the service definition path for each service.
service_unit_paths() {
    case "$(uname -s)" in
        Linux)
            printf '%s\n' "$SYSTEMD_UNIT_DIR/monetarium-node.service" \
                          "$SYSTEMD_UNIT_DIR/monetarium-wallet.service"
            ;;
        Darwin)
            printf '%s\n' "$LAUNCHD_DIR/com.monetarium.node.plist" \
                          "$LAUNCHD_DIR/com.monetarium.wallet.plist"
            ;;
    esac
}

# service_installed reports whether at least one service unit exists.
service_installed() {
    local unit
    while IFS= read -r unit; do
        [[ -f "$unit" ]] && return 0
    done < <(service_unit_paths)
    return 1
}

# installed_binary_count prints how many release binaries exist in INSTALL_DIR.
installed_binary_count() {
    local n=0 bin
    for bin in monetarium-node monetarium-wallet monetarium-ctl; do
        [[ -f "$INSTALL_DIR/$bin" ]] && n=$((n + 1))
    done
    printf '%s\n' "$n"
}

# Map a service/binary name to its platform-specific daemon label.
service_target() {
    case "$(uname -s)" in
        Linux)  printf '%s\n' "$1" ;;
        Darwin)
            case "$1" in
                monetarium-node)   echo "com.monetarium.node" ;;
                monetarium-wallet) echo "com.monetarium.wallet" ;;
                *) return 1 ;;
            esac
            ;;
    esac
}

# service_is_active reports whether the service is running right now.
# On macOS `launchctl print` exits 0 for any LOADED job, running or not, so its
# exit status says nothing about liveness: the job's `state` line must be read.
service_is_active() {
    local svc="$1" label
    case "$(uname -s)" in
        Linux)  systemctl is-active --quiet "$svc" 2>/dev/null ;;
        Darwin)
            label="$(service_target "$svc")" || return 1
            launchctl print "gui/${RUN_UID}/${label}" 2>/dev/null \
                | grep -Eq '^[[:space:]]*state = running$'
            ;;
    esac
}

# service_main_pid prints the PID of the service's main process, if any.
service_main_pid() {
    local svc="$1"
    case "$(uname -s)" in
        Linux)  systemctl show -p MainPID --value "$svc" 2>/dev/null ;;
        Darwin) pgrep -f "${INSTALL_DIR}/${svc}( |$)" 2>/dev/null | head -n1 ;;
    esac
}

# Start time of a process as a Unix timestamp, parsed from `ps -o etime=`
# ([dd-]hh:mm:ss or mm:ss). Both GNU and BSD ps support this format.
process_start_epoch() {
    local pid="$1" etime days=0 hours=0 mins=0 secs=0
    etime="$(ps -o etime= -p "$pid" 2>/dev/null | tr -d '[:space:]')"
    [[ -n "$etime" ]] || return 1
    if [[ "$etime" == *-* ]]; then
        days="${etime%%-*}"
        etime="${etime#*-}"
    fi
    local -a parts=()
    local IFS=':'
    read -r -a parts <<< "$etime"
    unset IFS
    case "${#parts[@]}" in
        3) hours="${parts[0]}"; mins="${parts[1]}"; secs="${parts[2]}" ;;
        2) mins="${parts[0]}"; secs="${parts[1]}" ;;
        1) secs="${parts[0]}" ;;
        *) return 1 ;;
    esac
    [[ "$days$hours$mins$secs" =~ ^[0-9]+$ ]] || return 1
    printf '%s\n' "$(( $(date +%s) - (10#$days * 86400 + 10#$hours * 3600 + 10#$mins * 60 + 10#$secs) ))"
}

# file_mtime prints a file's modification time as a Unix timestamp.
file_mtime() {
    if stat -c %Y "$1" >/dev/null 2>&1; then
        stat -c %Y "$1"
    else
        stat -f %m "$1"
    fi
}

# True when the running process started at or after the on-disk binary was
# written. A previous run can die after replacing the binary but before
# restarting the service; the hash then matches and a plain "already current"
# check would leave the old inode running forever.
service_runs_current_binary() {
    local svc="$1" dest="$2" pid start mtime
    pid="$(service_main_pid "$svc")" || return 0
    [[ -n "$pid" && "$pid" != "0" ]] || return 0
    start="$(process_start_epoch "$pid")" || return 0
    mtime="$(file_mtime "$dest")" || return 0
    (( start >= mtime ))
}

# restart_service restarts one service through the platform's native tool.
restart_service() {
    local svc="$1" label
    case "$(uname -s)" in
        Linux) sudo systemctl restart "$svc" ;;
        Darwin)
            label="$(service_target "$svc")" || return 1
            if launchctl print "gui/${RUN_UID}/${label}" >/dev/null 2>&1; then
                launchctl kickstart -k "gui/${RUN_UID}/${label}"
            else
                launchctl unload "$LAUNCHD_DIR/${label}.plist" 2>/dev/null || true
                launchctl load "$LAUNCHD_DIR/${label}.plist"
            fi
            ;;
    esac
}

# wait_service_active polls until the service is active, then keeps watching
# for a settle period. A unit with Type=simple is "active" the moment the
# process starts, and Restart=on-failure brings it back to "active" between
# crashes, so a single check would report success for a node that dies a
# second later. The settle window is longer than RestartSec (5s) so a crash
# loop is caught instead of being mistaken for a healthy start.
SERVICE_SETTLE_SECS="${SERVICE_SETTLE_SECS:-8}"

wait_service_active() {
    local svc="$1" i=0
    while (( i < 20 )); do
        service_is_active "$svc" && break
        sleep 1
        i=$((i + 1))
    done
    service_is_active "$svc" || return 1

    i=0
    while (( i < SERVICE_SETTLE_SECS )); do
        sleep 1
        service_is_active "$svc" || return 1
        i=$((i + 1))
    done
    return 0
}

# Platform-appropriate command for the user to inspect a failed service.
service_log_hint() {
    local svc="$1" label
    case "$(uname -s)" in
        Linux) echo "journalctl -u ${svc} -n 50" ;;
        Darwin)
            label="$(service_target "$svc")" || label="$svc"
            echo "launchctl print gui/${RUN_UID}/${label}"
            ;;
    esac
}

# prompt_install_or_update asks the user to pick a mode and dies without a TTY.
prompt_install_or_update() {
    local reason="$1" answer
    if ! has_tty; then
        die "${reason} Cannot choose between install and update non-interactively. Run this script from a terminal, or remove the leftover files to force a clean install."
    fi
    red "$reason"
    read_tty -r -p "Install or update? [1] Update binaries only [2] Full install: " answer
    case "$answer" in
        2|[fF]|[fF][uU][lL][lL]*) IS_UPDATE_MODE=false ;;
        *)                        IS_UPDATE_MODE=true ;;
    esac
}

# Sets IS_UPDATE_MODE. A fresh install is chosen ONLY when nothing at all is
# on disk: no node config, no wallet config, no service unit, no binaries, no
# wallet database and no manifest. Anything else is an existing installation
# and must never be silently reinstalled: a full install rewrites the configs
# (dropping hand-edited settings such as proxy= and addpeer=) and rewrites the
# service units with the current user, which can move the service to another
# account.
#
# A complete config set is an update. Every other partial state asks, and the
# question fails without a terminal, so a non-interactive run never guesses.
detect_install_mode() {
    local bins confs=false node_conf=false wallet_conf=false
    local db=false svc=false manifest=false
    bins="$(installed_binary_count)"
    [[ -f "$NODE_CONF" ]] && node_conf=true
    [[ -f "$WALLET_CONF" ]] && wallet_conf=true
    $node_conf && $wallet_conf && confs=true
    [[ -f "$(wallet_data_dir)/mainnet/wallet.db" ]] && db=true
    service_installed && svc=true
    [[ -f "$MANIFEST" ]] && manifest=true

    if $confs; then
        IS_UPDATE_MODE=true
        return
    fi

    if ! $node_conf && ! $wallet_conf && ! $svc && ! $db && ! $manifest \
        && [[ "$bins" -eq 0 ]]; then
        IS_UPDATE_MODE=false
        return
    fi

    if $node_conf && ! $wallet_conf; then
        prompt_install_or_update "A node config exists but the wallet config is missing."
    elif $wallet_conf && ! $node_conf; then
        prompt_install_or_update "A wallet config exists but the node config is missing."
    elif $svc; then
        prompt_install_or_update "Existing service units found, but no config files."
    else
        prompt_install_or_update "Existing monetarium binaries or wallet data found, but no config files or services."
    fi
}

# Copy every staged binary next to its final path, smoke-test the copies and
# only then rename them into place. Writing into a sibling temp file and
# renaming is atomic on the same filesystem and never writes into the running
# executable's inode, so a live process keeps using the old file until it is
# restarted. Nothing is replaced unless every staged binary runs.
install_staged_binaries() {
    local bin tmp
    local -a copy_bins=() copy_tmps=()

    for bin in monetarium-node monetarium-wallet monetarium-ctl; do
        [[ -f "$STAGING_DIR/$bin" ]] || continue
        tmp="${INSTALL_DIR}/.${bin}.new.$$"
        sudo mkdir -p "$INSTALL_DIR"
        if ! sudo cp "$STAGING_DIR/$bin" "$tmp" || ! sudo chmod 0755 "$tmp"; then
            local cleanup
            # ${arr[@]+...} guard: macOS ships bash 3.2, where expanding an
            # empty array under `set -u` is an unbound-variable error. Without
            # it the first binary's failure dies with a confusing message
            # instead of the real one.
            for cleanup in ${copy_tmps[@]+"${copy_tmps[@]}"} "$tmp"; do
                sudo rm -f "$cleanup"
            done
            die "Could not write ${bin} into ${INSTALL_DIR}. Nothing was changed."
        fi
        copy_bins+=("$bin")
        copy_tmps+=("$tmp")
        if ! "$tmp" --version >/dev/null 2>&1; then
            local cleanup
            for cleanup in ${copy_tmps[@]+"${copy_tmps[@]}"}; do
                sudo rm -f "$cleanup"
            done
            die "Downloaded ${bin} failed its --version smoke test. Refusing to install it; nothing was changed."
        fi
    done

    local i
    for i in "${!copy_bins[@]}"; do
        sudo mv -f "${copy_tmps[$i]}" "${INSTALL_DIR}/${copy_bins[$i]}"
        info "Installed ${copy_bins[$i]} -> ${INSTALL_DIR}/${copy_bins[$i]}"
    done
}

# Re-install only the binaries whose on-disk hash differs from the release.
# All binaries are downloaded and verified in a staging directory first, so a
# failure on any one of them aborts before anything installed is touched.
run_update() {
    local platform="$1"
    local node_active=false wallet_active=false
    local node_changed=false wallet_changed=false ctl_changed=false
    local bin failed=0

    service_is_active monetarium-node && node_active=true
    service_is_active monetarium-wallet && wallet_active=true

    info "Existing installation detected - updating binaries only."
    info "Configs, wallet, service units and settings are left untouched."

    STAGING_DIR="$(mktemp -d "${TMPDIR:-/tmp}/monetarium-update.XXXXXX")" || \
        die "Could not create a staging directory."
    trap 'rm -rf "${STAGING_DIR:-}"' EXIT

    download_binary "$REPO_NODE"   "monetarium-node"   "$platform" "$STAGING_DIR"
    download_binary "$REPO_WALLET" "monetarium-wallet" "$platform" "$STAGING_DIR"
    download_binary "$REPO_CTL"    "monetarium-ctl"    "$platform" "$STAGING_DIR"

    [[ -f "$STAGING_DIR/monetarium-node" ]]   && node_changed=true
    [[ -f "$STAGING_DIR/monetarium-wallet" ]] && wallet_changed=true
    [[ -f "$STAGING_DIR/monetarium-ctl" ]]    && ctl_changed=true

    # Catch a previous interrupted update: the binary is current on disk but
    # the service is still running the replaced inode. This must run BEFORE
    # the "nothing to do" return, or that stale process is never noticed.
    if ! $node_changed && $node_active \
        && ! service_runs_current_binary monetarium-node "$INSTALL_DIR/monetarium-node"; then
        warn "monetarium-node is running an older binary than the one on disk - restarting it."
        node_changed=true
    fi
    if ! $wallet_changed && $wallet_active \
        && ! service_runs_current_binary monetarium-wallet "$INSTALL_DIR/monetarium-wallet"; then
        warn "monetarium-wallet is running an older binary than the one on disk - restarting it."
        wallet_changed=true
    fi

    if ! $node_changed && ! $wallet_changed && ! $ctl_changed; then
        green "All monetarium binaries are already current. Nothing to do."
        return 0
    fi

    # A downloaded binary that cannot even report its version is not safe to
    # install. The copies are validated in place before any rename happens, so
    # a bad download leaves the installed set untouched.
    install_staged_binaries

    # Restart only services whose binary changed AND that were running before
    # the update. A service the operator stopped stays stopped.
    if $node_changed && $node_active; then
        info "Restarting monetarium-node..."
        restart_service monetarium-node || failed=1
        if ! wait_service_active monetarium-node; then
            red "monetarium-node did not become active after the restart. Check: $(service_log_hint monetarium-node)"
            failed=1
        fi
    fi
    if $wallet_changed && $wallet_active; then
        info "Restarting monetarium-wallet..."
        restart_service monetarium-wallet || failed=1
        if ! wait_service_active monetarium-wallet; then
            red "monetarium-wallet did not become active after the restart. Check: $(service_log_hint monetarium-wallet)"
            failed=1
        fi
    fi

    for bin in monetarium-node monetarium-wallet monetarium-ctl; do
        [[ -f "$INSTALL_DIR/$bin" ]] || continue
        info "  $("$INSTALL_DIR/$bin" --version 2>/dev/null | head -n1 || true)"
    done

    if [[ "$failed" -ne 0 ]]; then
        red "Update failed: a service did not restart cleanly. Binaries were replaced and are NOT rolled back."
        return 1
    fi

    green "Update complete."
    return 0
}

# --------------------------------------------------------------------------
# 2b. Ensure INSTALL_DIR is on PATH for interactive shell use
#     (the systemd/launchd services call binaries by absolute path,
#     so this step is purely for convenient `monetarium-ctl ...` usage).
# --------------------------------------------------------------------------
path_needs_update() {
    case ":$PATH:" in
        *":$INSTALL_DIR:"*) return 1 ;;
        *) return 0 ;;
    esac
}

add_install_dir_to_path() {
    local rc_file marker="# Added by monetarium install.sh"

    case "$(basename "${SHELL:-}")" in
        zsh)  rc_file="$RUN_HOME/.zshrc" ;;
        bash) rc_file="$RUN_HOME/.bashrc" ;;
        *)    rc_file="$RUN_HOME/.profile" ;;
    esac

    if ! grep -qF "$marker" "$rc_file" 2>/dev/null; then
        {
            echo ""
            echo "$marker"
            echo "export PATH=\"$INSTALL_DIR:\$PATH\""
        } >> "$rc_file"
        info "Added $INSTALL_DIR to PATH in $rc_file. Run 'source $rc_file' or open a new terminal to use it."
    else
        info "$rc_file already updates PATH for $INSTALL_DIR."
    fi

    export PATH="$INSTALL_DIR:$PATH"
}

# --------------------------------------------------------------------------
# 3. Prompt for passphrase (hidden)
# --------------------------------------------------------------------------
# In curl | bash, stdin is a pipe — interactive commands need /dev/tty
# so the user can type. In headless CI there is no /dev/tty, so read
# from stdin (the pipe the CI feeds).
needs_tty() {
    [[ ! -t 0 ]] && has_tty
}

read_tty() {
    if needs_tty; then
        read -r "$@" < /dev/tty || true
    else
        read -r "$@" || true
    fi
}

prompt_passphrase() {
    local pass1 pass2
    while true; do
        read_tty -r -s -p "Enter wallet private passphrase: " pass1
        echo
        read_tty -r -s -p "Confirm passphrase: " pass2
        echo
        if [[ -z "$pass1" ]]; then
            red "Passphrase cannot be empty. Try again."
            continue
        fi
        if [[ "$pass1" != "$pass2" ]]; then
            red "Passphrases did not match. Try again."
            continue
        fi
        WALLET_PASSPHRASE="$pass1"
        break
    done
}

# --------------------------------------------------------------------------
# 3b. Prompt for mining
# --------------------------------------------------------------------------
prompt_mining() {
    if ! has_tty; then
        return
    fi

    local answer
    read_tty -r -p "Enable CPU mining? (y/N): " answer
    case "$answer" in
        [yY]|[yY][eE][sS]) MINING_ENABLED=true ;;
    esac

    if $MINING_ENABLED; then
        local cpu_count recommended
        cpu_count=$(nproc 2>/dev/null || sysctl -n hw.logicalcpu 2>/dev/null || echo 2)
        recommended=$((cpu_count / 2))
        [[ "$recommended" -lt 1 ]] && recommended=1

        read_tty -r -p "You have ${cpu_count} CPU core(s). How many cores to use for mining? (recommended: ${recommended}): " answer
        MINING_CORES="${answer:-$recommended}"
        while ! [[ "$MINING_CORES" =~ ^[0-9]+$ ]]; do
            read_tty -r -p "Please enter a valid number: " answer
            MINING_CORES="${answer:-$recommended}"
        done
    fi
}

# --------------------------------------------------------------------------
# 3c. Prompt for ticket buyer
# --------------------------------------------------------------------------
prompt_ticket_buyer() {
    if ! has_tty; then
        return
    fi

    local answer
    read_tty -r -p "Enable automatic ticket purchase for staking? (y/N): " answer
    case "$answer" in
        [yY]|[yY][eE][sS]) TICKETS_ENABLED=true ;;
    esac

    if $TICKETS_ENABLED; then
        read_tty -r -p "Maximum number of tickets per purchase (ticketbuyer.limit): " answer
        TICKET_LIMIT="${answer:-5}"
        while ! [[ "$TICKET_LIMIT" =~ ^[0-9]+$ ]]; do
            read_tty -r -p "Please enter a valid number: " answer
            TICKET_LIMIT="$answer"
        done

        read_tty -r -p "Minimum VAR balance to keep in wallet (ticketbuyer.balancetomaintainabsolute): " answer
        TICKET_BALANCE="${answer:-0}"
        while ! [[ "$TICKET_BALANCE" =~ ^[0-9]+(\.[0-9]+)?$ ]]; do
            read_tty -r -p "Please enter a valid number: " answer
            TICKET_BALANCE="$answer"
        done
    fi

    # Ask about auto-voting regardless of ticket buyer answer.
    read_tty -r -p "Enable automatic voting? (Y/n): " answer
    case "$answer" in
        [nN]|[nN][oO]) VOTING_ENABLED=false ;;
        *) VOTING_ENABLED=true ;;
    esac
}

# --------------------------------------------------------------------------
# 3d. Reuse RPC credentials on re-runs
# --------------------------------------------------------------------------
# On a re-run the node/wallet daemons are already running with the RPC
# credentials from the previous install. Regenerating them in write_configs()
# would put a NEW password on disk while the daemons still answer with the
# OLD one — so monetarium-ctl silently fails auth and the post-install
# wallet polling times out. Instead, adopt the existing credentials when a
# node config already exists, and only generate fresh ones on first install.
rpc_credential_from_conf() {
    # $1 = key (e.g. rpcuser), reads $NODE_CONF
    # grep exits 1 when the key is absent and 2 when $NODE_CONF is missing;
    # under `set -eo pipefail` that aborts the caller (e.g. write_configs on a
    # fresh install calls this before the config file exists). Mask the exit
    # status so a missing file/key yields empty output, not a hard abort.
    grep -E "^${1}=" "$NODE_CONF" 2>/dev/null | tail -n1 | cut -d= -f2- || true
}

ensure_rpc_credentials() {
    if [[ -f "$NODE_CONF" ]]; then
        local prev_user prev_pass
        prev_user="$(rpc_credential_from_conf rpcuser)"
        prev_pass="$(rpc_credential_from_conf rpcpass)"
        if [[ -n "$prev_pass" ]]; then
            # When a usable password exists, the rpcuser MUST come from the
            # running daemons too. Defaulting the user here would put a NEW
            # rpcuser on disk while the daemons still answer with their
            # previous one — exactly the auth desync this function exists to
            # prevent (the same trap a missing rpcpass falls into below).
            if [[ -z "$prev_user" ]]; then
                die "Found ${NODE_CONF} with rpcpass but no rpcuser. Deleting only the password would desync the daemon's user; delete it (or the previous install) and re-run to reinstall cleanly."
            fi
            RPC_USER="$prev_user"
            RPC_PASS="$prev_pass"
            info "Reusing existing RPC credentials from $NODE_CONF (re-run detected)."
            return
        fi
        # NODE_CONF exists but lacks a usable rpcpass. The message covers both
        # "rpcuser present, rpcpass missing/absent" and "neither key set" (e.g.
        # a config left behind with dcrd's commented-out RPC defaults). Our
        # installer writes rpcuser+rpcpass together, so a partial/missing pair is
        # a broken or foreign config: silently regenerating only one half would
        # desync disk vs. the running daemons. Refuse.
        die "Found ${NODE_CONF} without a usable rpcpass (and possibly without rpcuser either). Delete it (or the previous install) and re-run to reinstall cleanly."
    fi
    if [[ -z "$RPC_PASS" ]]; then
        RPC_PASS="$(head -c 24 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9' | head -c 32)"
    fi
}

# --------------------------------------------------------------------------
# 4. Write config files
# --------------------------------------------------------------------------
write_configs() {
    ensure_rpc_credentials

    mkdir -p "$DATA_DIR"
    chmod 700 "$DATA_DIR"

    # Preserve the post-install mining section across re-runs: write_configs()
    # rewrites the node config from scratch, which would otherwise wipe the
    # generate=/miningaddr= lines appended on a previous run. Carry them over.
    local prev_generate prev_miningaddr
    prev_generate="$(rpc_credential_from_conf generate)"
    prev_miningaddr="$(rpc_credential_from_conf miningaddr)"

    # Node config
    cat > "$NODE_CONF" <<EOF
; monetarium.conf - generated by install.sh on $(date -u +"%Y-%m-%dT%H:%M:%SZ")
rpcuser=${RPC_USER}
rpcpass=${RPC_PASS}
EOF

    # Re-append the previous mining settings so a re-run keeps them
    # (configure_mining_and_voting below may adjust them).
    if [[ -n "$prev_generate" || -n "$prev_miningaddr" ]]; then
        {
            echo ""
            echo "; Mining configuration — carried over from previous install by install.sh"
            [[ -n "$prev_generate" ]] && echo "generate=${prev_generate}"
            [[ -n "$prev_miningaddr" ]] && echo "miningaddr=${prev_miningaddr}"
        } >> "$NODE_CONF"
    fi

    # Wallet config — this is where the plaintext passphrase lives.
    local tb_enabled=0; $TICKETS_ENABLED && tb_enabled=1
    local vote_enabled=0; $VOTING_ENABLED && vote_enabled=1
    local tkt_limit="${TICKET_LIMIT:-1}"
    local tkt_balance="${TICKET_BALANCE:-1}"

    cat > "$WALLET_CONF" <<EOF
; monetarium-wallet.conf - generated by install.sh on $(date -u +"%Y-%m-%dT%H:%M:%SZ")
; WARNING: 'pass' below stores the wallet unlock passphrase in plaintext
; so the service can auto-unlock the wallet on boot for unattended
; ticket buying / voting. See the warning printed by install.sh.
username=${RPC_USER}
password=${RPC_PASS}
pass=${WALLET_PASSPHRASE}
enablevoting=${vote_enabled}
enableticketbuyer=${tb_enabled}
ticketbuyer.limit=${tkt_limit}
ticketbuyer.balancetomaintainabsolute=${tkt_balance}
; Gap limit for address discovery
gaplimit=20
accountgaplimit=10
EOF

    chmod 600 "$NODE_CONF" "$WALLET_CONF"
    chown "${RUN_UID}:${RUN_GID}" "$NODE_CONF" "$WALLET_CONF"

    # ctl config — so "monetarium-ctl getinfo" works without flags.
    mkdir -p "$CTL_DIR"
    cat > "$CTL_CONF" <<EOF
; monetarium-ctl.conf - generated by install.sh on $(date -u +"%Y-%m-%dT%H:%M:%SZ")
; RPC credentials matching the node config.
rpcuser=${RPC_USER}
rpcpass=${RPC_PASS}
EOF
    chmod 600 "$CTL_CONF"
    chown "${RUN_UID}:${RUN_GID}" "$CTL_CONF"

    info "Config files written to $DATA_DIR (permissions set to 600)."
}

# --------------------------------------------------------------------------
# 5. Create wallet
# --------------------------------------------------------------------------
wallet_data_dir() {
    local home="$RUN_HOME"
    case "$(uname -s)" in
        Darwin) echo "${home}/Library/Application Support/Monetarium-wallet" ;;
        Linux)  echo "${home}/.monetarium-wallet" ;;
    esac
}

create_wallet() {
    local wallet_data wallet_db
    wallet_data="$(wallet_data_dir)"
    wallet_db="${wallet_data}/mainnet/wallet.db"

    if [[ -f "$wallet_db" ]]; then
        info "Wallet database already exists at $wallet_db — skipping creation."
        return 0
    fi

    info "Creating wallet. Follow the interactive prompts."
    info "When asked, type 'yes' to use the passphrase you just entered."

    if needs_tty; then
        monetarium-wallet --create --configfile="$WALLET_CONF" < /dev/tty
    else
        monetarium-wallet --create --configfile="$WALLET_CONF"
    fi

    [[ -f "$wallet_db" ]] || \
        die "Wallet creation failed — database not found at $wallet_db"

    green "Wallet created successfully."
}

# --------------------------------------------------------------------------
# 6a. systemd service (Linux)
# --------------------------------------------------------------------------
install_systemd_service() {
    local node_unit="$SYSTEMD_UNIT_DIR/monetarium-node.service"
    local wallet_unit="$SYSTEMD_UNIT_DIR/monetarium-wallet.service"
    local svc_user="$RUN_USER"

    sudo tee "$node_unit" > /dev/null <<EOF
[Unit]
Description=Monetarium full node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${svc_user}
ExecStart=${INSTALL_DIR}/monetarium-node --configfile=${NODE_CONF}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

    sudo tee "$wallet_unit" > /dev/null <<EOF
[Unit]
Description=Monetarium wallet (auto-unlock, ticket buying / voting enabled)
After=monetarium-node.service
Requires=monetarium-node.service
PartOf=monetarium-node.service

[Service]
Type=simple
User=${svc_user}
ExecStart=${INSTALL_DIR}/monetarium-wallet --configfile=${WALLET_CONF}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

    sudo systemctl daemon-reload
    sudo systemctl enable --now monetarium-node.service
    sudo systemctl enable --now monetarium-wallet.service

    info "systemd services installed and started:"
    info "  systemctl status monetarium-node"
    info "  systemctl status monetarium-wallet"
}

# --------------------------------------------------------------------------
# 7b. launchd plist (macOS)
# --------------------------------------------------------------------------
install_launchd_service() {
    local plist_dir="$LAUNCHD_DIR"
    mkdir -p "$plist_dir"

    local node_plist="$plist_dir/com.monetarium.node.plist"
    local wallet_plist="$plist_dir/com.monetarium.wallet.plist"

    cat > "$node_plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>com.monetarium.node</string>
    <key>ProgramArguments</key>
    <array>
        <string>${INSTALL_DIR}/monetarium-node</string>
        <string>--configfile=${NODE_CONF}</string>
    </array>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
    <key>StandardOutPath</key><string>${DATA_DIR}/node.log</string>
    <key>StandardErrorPath</key><string>${DATA_DIR}/node.err.log</string>
</dict>
</plist>
EOF

    cat > "$wallet_plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>com.monetarium.wallet</string>
    <key>ProgramArguments</key>
    <array>
        <string>${INSTALL_DIR}/monetarium-wallet</string>
        <string>--configfile=${WALLET_CONF}</string>
    </array>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
    <key>StandardOutPath</key><string>${DATA_DIR}/wallet.log</string>
    <key>StandardErrorPath</key><string>${DATA_DIR}/wallet.err.log</string>
</dict>
</plist>
EOF

    chmod 600 "$node_plist" "$wallet_plist"

    launchctl unload "$node_plist" >/dev/null 2>&1 || true
    launchctl unload "$wallet_plist" >/dev/null 2>&1 || true
    launchctl load "$node_plist"
    launchctl load "$wallet_plist"

    info "launchd agents installed and loaded:"
    info "  launchctl list | grep monetarium"
}

# --------------------------------------------------------------------------
# 8. Post-install: configure mining & ticket buyer
# --------------------------------------------------------------------------
# Remove any mining section previously appended to the node config (either
# carried over by write_configs or written by an earlier run) so we never
# end up with duplicate generate=/miningaddr= keys that override each other.
strip_mining_section() {
    local tmp
    tmp="$(mktemp)"
    grep -vE '^generate=|^miningaddr=|^; Mining configuration' "$NODE_CONF" > "$tmp" 2>/dev/null || true
    mv "$tmp" "$NODE_CONF"
    chmod 600 "$NODE_CONF"
}

has_previous_mining_config() {
    grep -qE '^(generate|miningaddr)=' "$NODE_CONF" 2>/dev/null
}

# True when the node config on disk actually enables mining. Used by the
# summary so a re-run that never got a fresh address (RPC timeout) reports the
# carried-over mining config truthfully instead of claiming mining is off.
has_previous_mining_enabled() {
    grep -qE '^generate=true[[:space:]]*$' "$NODE_CONF" 2>/dev/null
}

configure_mining_and_voting() {
    # No TTY → can't get a mining address → generate must stay false.
    # Setting generate=true without miningaddr prevents the node from starting.
    if ! has_tty; then
        if ! has_previous_mining_config; then
            {
                echo ""
                echo "; Mining configuration — added post-install by install.sh"
                echo "generate=false"
            } >> "$NODE_CONF" 2>/dev/null || true
        fi
        info "Non-interactive mode — skipped wallet address polling (no TTY)."
        return
    fi

    info "Waiting for wallet RPC to become available..."

    local i=0
    until monetarium-ctl --wallet getinfo >/dev/null 2>&1; do
        i=$((i + 1))
        if [[ $i -gt "${RPC_TIMEOUT_THRESHOLD:-60}" ]]; then
            # Wallet RPC never came up — no address to key the config off.
            # On a FRESH install (no previous mining config) generate stays
            # false, because the node WILL NOT START with generate=true and
            # no miningaddr. On a RE-RUN we keep whatever mining config was
            # carried over instead of silently turning mining off.
            # Carrying over the previous mining config is only correct when the
            # user wants mining. If they explicitly declined (MINING_ENABLED
            # false) but the wallet RPC timed out, write_configs() already
            # carried over a previous generate=true — and the running node would
            # keep mining against the user's wish. Stage generate=false to match
            # the answer; the live daemon still needs a bounce to drop it.
            if has_previous_mining_config && ! $MINING_ENABLED && has_previous_mining_enabled; then
                strip_mining_section 2>/dev/null || true
                {
                    echo ""
                    echo "; Mining configuration — honoured on RPC-timeout re-run by install.sh"
                    echo "generate=false"
                } >> "$NODE_CONF" 2>/dev/null || true
                red "  Mining: your answer was 'No', but the wallet RPC timed out, so the running"
                red "  node still uses the previous install's generate=true. generate=false staged"
                red "  in ${NODE_CONF}; restart the monetarium-node service to stop mining."
            elif ! has_previous_mining_config; then
                {
                    echo ""
                    echo "; Mining configuration — added post-install by install.sh"
                    echo "generate=false"
                } >> "$NODE_CONF" 2>/dev/null || true
            fi
            info "Wallet RPC not ready after ${RPC_TIMEOUT_THRESHOLD:-60} seconds."
            info "Configure mining/ticket addresses manually:"
            info "  Edit ${NODE_CONF}: add miningaddr=<address> then set generate=true"
            show_configuration_summary
            return
        fi
        sleep 2
    done

    green "Wallet RPC ready."

    local config_changed=false

    # Get mining address first. Only enable generate if address succeeds.
    strip_mining_section
    local mining_addr
    mining_addr=$(monetarium-ctl --wallet getnewaddress 2>/dev/null || true)

    local gen_value="false"
    if $MINING_ENABLED && [[ -n "$mining_addr" ]]; then
        gen_value="true"
    fi

    {
        echo ""
        echo "; Mining configuration — added post-install by install.sh"
        echo "generate=${gen_value}"
    } >> "$NODE_CONF"
    config_changed=true
    if [[ -n "$mining_addr" ]]; then
        MINING_ADDR="$mining_addr"
        echo "miningaddr=${mining_addr}" >> "$NODE_CONF"
    fi

    # Generate consolidation address always and set it on the wallet.
    local cons_addr
    cons_addr=$(monetarium-ctl --wallet getnewaddress 2>/dev/null || true)
    if [[ -n "$cons_addr" ]]; then
        CONSOLIDATION_ADDR="$cons_addr"
        monetarium-ctl --wallet setvotefeeconsolidationaddress "default" "$cons_addr" >/dev/null 2>&1 || true
    fi

    if $config_changed; then
        info "Restarting node to apply mining configuration..."
        case "$(uname -s)" in
            Linux)
                sudo systemctl restart monetarium-node
                ;;
            Darwin)
                if launchctl kickstart -k "gui/${RUN_UID}/com.monetarium.node" 2>/dev/null; then
                    :
                else
                    launchctl unload "$LAUNCHD_DIR/com.monetarium.node.plist" 2>/dev/null || true
                    launchctl load "$LAUNCHD_DIR/com.monetarium.node.plist"
                fi
                ;;
        esac

        sleep 3
        i=0
        until monetarium-ctl getinfo >/dev/null 2>&1; do
            i=$((i + 1))
            [[ $i -gt 30 ]] && break
            sleep 2
        done

        if $MINING_ENABLED && [[ -n "$MINING_CORES" ]]; then
            monetarium-ctl setgenerate true "$MINING_CORES" 2>/dev/null || true
        fi
    fi

    show_configuration_summary
}

# --------------------------------------------------------------------------
# 8b. Configuration Summary display
# --------------------------------------------------------------------------
show_configuration_summary() {
    echo ""
    green "============================================="
    green "  Configuration Summary"
    green "============================================="
    if $MINING_ENABLED && [[ -n "$MINING_ADDR" ]]; then
        green "  Mining:             enabled (${MINING_CORES} core(s))"
        green "  Mining reward addr: ${MINING_ADDR}"
    elif has_previous_mining_enabled; then
        # A re-run whose wallet RPC never came up keeps the mining config
        # carried over from the previous install — say so truthfully rather
        # than claiming mining was disabled while the node runs with
        # generate=true.
        info "  Mining:             unchanged from previous run — verify miningaddr= manually"
    else
        if $MINING_ENABLED; then
            red "  Mining:             requested but disabled — no mining address obtained"
        else
            green "  Mining:             disabled"
        fi
    fi
    if $TICKETS_ENABLED; then
        green "  Ticket buying:      enabled"
        green "  Ticket limit:       ${TICKET_LIMIT}"
        green "  Min wallet balance: ${TICKET_BALANCE}"
    else
        green "  Ticket buying:      disabled"
    fi
    if [[ -n "$CONSOLIDATION_ADDR" ]]; then
        green "  Fee consolidation:  ${CONSOLIDATION_ADDR}"
    fi
    if $VOTING_ENABLED; then
        green "  Auto voting:        enabled"
    else
        green "  Auto voting:        disabled"
    fi
    green "============================================="
}

# --------------------------------------------------------------------------
# 9. Install manifest
# --------------------------------------------------------------------------
write_manifest() {
    local wallet_db
    wallet_db="$(wallet_data_dir)/mainnet/wallet.db"

    cat > "$MANIFEST" <<EOF
# Install manifest — files created by monetarium install.sh
# Run date: $(date -u +"%Y-%m-%dT%H:%M:%SZ")
#
# To uninstall, stop the services first:
#   Linux:  sudo systemctl disable --now monetarium-node monetarium-wallet
#   macOS:  launchctl unload ~/Library/LaunchAgents/com.monetarium.node.plist
#           launchctl unload ~/Library/LaunchAgents/com.monetarium.wallet.plist
#
# Then remove the files listed below.
# ⚠️  Back up and remove the wallet database FIRST if it holds funds!
#     The wallet seed was shown during installation — without it the
#     wallet.db is unrecoverable.

binaries:
  ${INSTALL_DIR}/monetarium-node
  ${INSTALL_DIR}/monetarium-wallet
  ${INSTALL_DIR}/monetarium-ctl

configs:
  ${NODE_CONF}
  ${WALLET_CONF}
  ${CTL_CONF}

wallet database (back up seed before removing):
  ${wallet_db}

data directories:
  ${DATA_DIR}
  ${CTL_DIR}

EOF

    {
        echo "mining:"
        # Mirror show_configuration_summary's decision tree so the manifest
        # never lies about what the node is actually running. On an RPC-timeout
        # re-run $MINING_ENABLED/$MINING_ADDR are empty even though the on-disk
        # config still has generate=true, so consult the live config in that case
        # rather than the ephemeral prompt variables.
        if $MINING_ENABLED && [[ -n "$MINING_ADDR" ]]; then
            echo "  enabled:  true"
            echo "  cores:    ${MINING_CORES}"
            echo "  address:  ${MINING_ADDR}"
        elif has_previous_mining_enabled; then
            echo "  enabled:  true (carried over — verify miningaddr= manually)"
        elif $MINING_ENABLED; then
            echo "  enabled:  false (address unavailable)"
        else
            echo "  enabled:  false"
        fi
        echo ""
        echo "ticket buyer:"
        if $TICKETS_ENABLED; then
            echo "  enabled:  true"
            echo "  limit:    ${TICKET_LIMIT:-1}"
            echo "  balance:  ${TICKET_BALANCE:-1}"
        else
            echo "  enabled:  false"
        fi
        if [[ -n "$CONSOLIDATION_ADDR" ]]; then
            echo "  fee consolidation addr:   ${CONSOLIDATION_ADDR}"
        fi
        echo ""
    } >> "$MANIFEST"

    case "$(uname -s)" in
        Linux)
            cat >> "$MANIFEST" <<EOF
systemd service units:
  /etc/systemd/system/monetarium-node.service
  /etc/systemd/system/monetarium-wallet.service
EOF
            ;;
        Darwin)
            cat >> "$MANIFEST" <<EOF
launchd plists:
  ${LAUNCHD_DIR}/com.monetarium.node.plist
  ${LAUNCHD_DIR}/com.monetarium.wallet.plist
EOF
            ;;
    esac

    # Shell rc file that was modified (if any) — detect by marker
    local rc_file marker="# Added by monetarium install.sh"
    case "$(basename "${SHELL:-}")" in
        zsh)  rc_file="$RUN_HOME/.zshrc" ;;
        bash) rc_file="$RUN_HOME/.bashrc" ;;
        *)    rc_file="$RUN_HOME/.profile" ;;
    esac
    if grep -qF "$marker" "$rc_file" 2>/dev/null; then
        echo "shell rc (PATH addition):" >> "$MANIFEST"
        echo "  ${rc_file}" >> "$MANIFEST"
    fi

    echo "" >> "$MANIFEST"
    echo "This manifest: ${MANIFEST}" >> "$MANIFEST"

    info "Install manifest written to $MANIFEST"
}

# --------------------------------------------------------------------------
# 9. SELinux check (Linux only)
# --------------------------------------------------------------------------
check_selinux() {
    [[ "$(uname -s)" != "Linux" ]] && return

    local mode
    mode="$(command -v getenforce >/dev/null && getenforce 2>/dev/null || echo "Unknown")"

    case "$mode" in
        Enforcing)
            red "=============================================================="
            red "           SELinux is ENFORCING — Installation will FAIL"
            red "=============================================================="
            red "SELinux is currently enforcing on this system. It will block:"
            red "  - Writing systemd unit files to /etc/systemd/system/"
            red "  - Services executing binaries from ${INSTALL_DIR}"
            red "  - Services accessing data directories under your home"
            red ""
            red "To proceed, set SELinux to permissive or disabled FIRST:"
            red "  Temporary (until reboot):  sudo setenforce 0"
            red "  Permanent:                 edit /etc/selinux/config, set"
            red "                             SELINUX=permissive, then reboot"
            red ""
            red "After installation you can re-enable it with 'sudo setenforce 1'"
            red "but you'll need a custom SELinux module for the monetarium"
            red "services and their data directories."
            red "=============================================================="
            die "SELinux is enforcing. Disable it before running this installer."
            ;;
        Permissive)
            info "SELinux is in permissive mode — check AVC logs if services fail to start."
            ;;
    esac
}

# --------------------------------------------------------------------------
# 10. Big warning
# --------------------------------------------------------------------------
print_warning() {
    red   "=============================================================="
    red   "                       !!!  WARNING  !!!"
    red   "=============================================================="
    red   " This script has written your wallet's PRIVATE PASSPHRASE in"
    red   " PLAINTEXT to:"
    red   "     ${WALLET_CONF}"
    red   ""
    red   " This is required so the wallet can auto-unlock and buy"
    red   " tickets / vote WITHOUT any further action from you. It also"
    red   " means:"
    red   "   - Anyone who can read that file (root, a backup, a"
    red   "     compromised process, a misconfigured permission) can"
    red   "     unlock your wallet and move your funds."
    red   "   - This machine is now effectively a HOT WALLET that stays"
    red   "     unlocked 24/7. There is no PIN, 2FA, or manual approval"
    red   "     step standing between an attacker and your coins."
    red   "   - If this server is compromised, funds can be drained"
    red   "     automatically, with no prompt and no warning."
    red   ""
    red   "   If you suspect this machine has been compromised, treat the"
    red   "   wallet as compromised: keep the seed safe, back up the wallet"
    red   "   database, re-create it from the seed, and purge the config"
    red   "   files. Do NOT just re-run this script with a new passphrase"
    red   "   — the wallet database keeps its original passphrase, so a new"
    red   "   one would only break the auto-unlock service."
    red   ""
    red   " Recommended precautions:"
    red   "   - Only put funds on this wallet that you can afford to"
    red   "     lose, sized for ticket-buying/voting purposes only."
    red   "   - Keep the bulk of your holdings in a separate, offline"
    red   "     or hardware-secured wallet, not this one."
    red   "   - Restrict SSH/root access to this machine tightly, keep"
    red   "     it patched, and monitor it actively."
    red   "=============================================================="
}

# --------------------------------------------------------------------------
# Main
# --------------------------------------------------------------------------
run_install() {
    local platform="$1"

    # A fresh install must run as the account that will own the node. Under
    # sudo the data directory would be created by root inside the user's home
    # (mode 700, so the service user cannot enter it) and the wallet would be
    # created under root's HOME while the check looks in the user's.
    if [[ "$RUN_USER" != "$(id -un)" ]]; then
        die "Run a fresh install without sudo, as the account that will own the node."
    fi

    check_selinux

    download_binary "$REPO_NODE"   "monetarium-node"   "$platform"
    download_binary "$REPO_WALLET" "monetarium-wallet" "$platform"
    download_binary "$REPO_CTL"    "monetarium-ctl"    "$platform"

    if path_needs_update; then
        add_install_dir_to_path
    fi

    prompt_passphrase

    prompt_ticket_buyer

    write_configs
    create_wallet

    case "$(uname -s)" in
        Linux)  install_systemd_service ;;
        Darwin) install_launchd_service ;;
    esac

    # Bounce only the services whose binary was replaced so they run the
    # freshly-updated executables. This runs AFTER the services are
    # (re)installed above — restarting before they are written caused a
    # double bounce on every update.
    local -a replaced_services=()
    $NODE_REPLACED   && replaced_services+=(monetarium-node)
    $WALLET_REPLACED && replaced_services+=(monetarium-wallet)
    if ((${#replaced_services[@]} > 0)); then
        restart_monetarium_services "${replaced_services[@]}"
    fi

    prompt_mining

    unset WALLET_PASSPHRASE

    configure_mining_and_voting

    green "Installation complete."
    write_manifest
    print_warning
}

main() {
    require_cmd curl
    require_cmd tar
    require_cmd sudo
    require_hasher

    local platform
    platform="$(detect_platform)"
    info "Detected platform: $platform"

    detect_install_mode
    if $IS_UPDATE_MODE; then
        # A failed update exits non-zero (errexit): no rollback, loud failure.
        run_update "$platform"
    else
        run_install "$platform"
    fi
}

# When this file is `source`d (unit tests set MONETARIUM_INSTALL_FUNCTIONS_ONLY=1)
# only the functions above are defined; invoked as a script it runs main().
if [[ -z "${MONETARIUM_INSTALL_FUNCTIONS_ONLY:-}" ]]; then
    main "$@"
fi
