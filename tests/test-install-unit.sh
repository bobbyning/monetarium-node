#!/usr/bin/env bash
# Unit tests for install.sh.
#
# These source the REAL install.sh (with MONETARIUM_INSTALL_FUNCTIONS_ONLY=1
# so main() does not run) and exercise the actual functions, so the tests
# can't silently drift from the code like hand-copied duplicates do.
#
# Covered:
#   - sha256_of / lc (checksum helpers)
#   - check_selinux
#   - RPC credential reuse (ensure_rpc_credentials / rpc_credential_from_conf)
#   - write_configs (credential reuse + mining carry-over on re-runs)
#   - latest_release_asset (GitHub digest + URL parsing, via a mock curl)
#   - download_binary (fresh install, skip-when-current, self-update,
#     checksum-mismatch abort, no-digest and unreachable-API fallbacks)
#   - restart_monetarium_services (systemctl + launchctl paths)
#   - update mode: detect_install_mode matrix (fresh/update/repair/ambiguous),
#     run_update binary swaps + restart matrix (changed only, active only,
#     stale inode), staged abort, config/wallet preservation and the
#     "never run the install-time steps" contract
#   - install_staged_binaries (atomic swap + --version smoke test)
#   - configure_mining_and_voting generate/miningaddr guards, including the
#     re-run behaviour (carried-over mining config is preserved instead of
#     silently forcing generate=false)
#   - show_configuration_summary
#
# Run: bash tests/test-install-unit.sh
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALL_SCRIPT="$REPO_ROOT/install.sh"

PASS=0
FAIL=0

# --------------------------------------------------------------------------
# Isolation: point every install.sh path at a throwaway temp tree and
# source the real script before setting anything up.
# --------------------------------------------------------------------------
TEST_TMP="$(mktemp -d)"
export MONETARIUM_HOME="$TEST_TMP/monetarium-home"   # DATA_DIR / NODE_CONF / ...
export MONETARIUM_CTL_HOME="$TEST_TMP/ctl-home"
export MONETARIUM_INSTALL_FUNCTIONS_ONLY=1           # do NOT run main()

# shellcheck disable=SC1090
source "$INSTALL_SCRIPT"
set +e                                             # install.sh sets -e; tests need it off

unset MONETARIUM_INSTALL_FUNCTIONS_ONLY

# The script invokes sudo for mkdir/mv so it works documentedly on a fresh
# box; in tests we pass through to the real command (no privilege needed).
sudo() { command "$@"; }

MOCK_STATE="$TEST_TMP/mock-state"     # release JSON + fixture bodies
MOCK_BIN="$TEST_TMP/mock-bin"         # curl / systemctl / launchctl shims
MOCK_CTL="$TEST_TMP/mock-ctl-bin"     # monetarium-ctl shim
INSTALL_TEST_DIR="$TEST_TMP/install-bin"
MOCK_CTL_TRACKER="$TEST_TMP/mock-ctl-tracker"   # getnewaddress counter, shared across mock calls
MOCK_CTL_LOG="$TEST_TMP/mock-ctl.log"           # every mock invocation, for assertions
mkdir -p "$MOCK_STATE" "$MOCK_BIN" "$MOCK_CTL" "$INSTALL_TEST_DIR"

SYSTEMCTL_LOG="$TEST_TMP/systemctl.log"
LAUNCHCTL_LOG="$TEST_TMP/launchctl.log"

# --------------------------------------------------------------------------
# Harness helpers
# --------------------------------------------------------------------------
check() {
    local desc="$1"; shift
    local out rc
    # Capture instead of discarding: a bare "FAIL" with no output is
    # undebuggable from a CI log, which is exactly what platform-specific
    # failures need.
    out="$("$@" 2>&1)"; rc=$?
    if [[ $rc -eq 0 ]]; then
        echo "  PASS  $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL  $desc"
        if [[ -n "$out" ]]; then
            printf '%s\n' "$out" | sed 's/^/        /'
        else
            echo "        (no output; exit $rc)"
        fi
        FAIL=$((FAIL + 1))
    fi
}

expect_fail() {
    local desc="$1"; shift
    local out
    out="$("$@" 2>&1)"
    local rc=$?
    if [[ $rc -ne 0 ]]; then
        echo "  PASS  $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL  $desc (expected non-zero exit)"
        FAIL=$((FAIL + 1))
    fi
}

digest_of() {
    local file="$1"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$file" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$file" | awk '{print $1}'
    else
        return 1
    fi
}

# --------------------------------------------------------------------------
# Mock curl: deterministic, digest-aware. The test controls the "release"
# per repo via $MOCK_STATE/<repo>.json and stages the bytes each asset
# serves at $MOCK_STATE/<asset-name>.
# --------------------------------------------------------------------------
cat > "$MOCK_BIN/curl" <<'MOCKEOF'
#!/usr/bin/env bash
set -euo pipefail
url=""
outfile=""
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
    case "${args[$i]}" in
        -o) outfile="${args[$((i + 1))]}"; i=$((i + 1)) ;;
        http*) url="${args[$i]}" ;;
    esac
done
case "$url" in
    *"/releases/latest"*)
        [[ "${MOCK_CURL_API_FAIL:-0}" != "1" ]] || exit 1
        repo="$(printf '%s' "$url" | sed -E 's#.*/repos/[^/]+/([^/]+)/releases/latest#\1#')"
        [[ "$repo" != "${MOCK_CURL_REPO_FAIL:-}" ]] || exit 1
        cat "$MOCK_STATE/$repo.json"
        exit 0
        ;;
    *"fixtures.test"*)
        [[ -n "$outfile" ]] || { echo "mock curl: no -o output file" >&2; exit 1; }
        base="$(basename "$url")"
        echo "$base" >> "$MOCK_STATE/downloads.log"
        cp "$MOCK_STATE/$base" "$outfile"
        exit 0
        ;;
    *)
        echo "mock curl: unhandled URL: $url" >&2
        exit 1
        ;;
esac
MOCKEOF
chmod +x "$MOCK_BIN/curl"

# Write a one-asset release for $repo. $fixture's bytes are what the asset
# serves and what gets published as its sha256 digest.
json_for() {
    local repo="$1" fixture="$2"
    local name="${3:-${repo}-linux-amd64}"
    local digest
    digest="$(digest_of "$fixture")"
    cp "$fixture" "$MOCK_STATE/$name"
    cat > "$MOCK_STATE/$repo.json" <<EOF
{ "assets": [ { "name": "$name", "digest": "sha256:$digest", "browser_download_url": "https://fixtures.test/$name" } ] }
EOF
}

# Same, but no published digest (simulates a very old upload).
json_for_no_digest() {
    local repo="$1" fixture="$2"
    local name="${3:-${repo}-linux-amd64}"
    cp "$fixture" "$MOCK_STATE/$name"
    cat > "$MOCK_STATE/$repo.json" <<EOF
{ "assets": [ { "name": "$name", "browser_download_url": "https://fixtures.test/$name" } ] }
EOF
}

# --------------------------------------------------------------------------
# Mock systemctl / launchctl — log invocation and let tests steer the
# is-active result per service.
# --------------------------------------------------------------------------
cat > "$MOCK_BIN/systemctl" <<'MOCKEOF'
#!/usr/bin/env bash
{ echo "systemctl $*" >> "$SYSTEMCTL_LOG"; } 2>/dev/null || true
case "${1:-}" in
    is-active)
        case "$*" in
            *monetarium-node*)   exit "${NODE_ACTIVE:-0}" ;;
            *monetarium-wallet*) exit "${WALLET_ACTIVE:-0}" ;;
            *) exit 1 ;;
        esac
        ;;
    show)
        # `systemctl show -p MainPID --value <svc>`
        case "$*" in
            *monetarium-node*)   echo "${NODE_PID:-0}" ;;
            *monetarium-wallet*) echo "${WALLET_PID:-0}" ;;
            *) echo "0" ;;
        esac
        ;;
esac
exit 0
MOCKEOF
chmod +x "$MOCK_BIN/systemctl"

cat > "$MOCK_BIN/launchctl" <<'MOCKEOF'
#!/usr/bin/env bash
{ echo "launchctl $*" >> "$LAUNCHCTL_LOG"; } 2>/dev/null || true
# `launchctl print` exits 0 for any LOADED job, running or not, and reports
# liveness only through the `state` line. Mirror that: LAUNCHD_STATE selects
# the state (default running), LAUNCHD_LOADED=0 makes the job unknown.
if [[ "${1:-}" == "print" ]]; then
    [[ "${LAUNCHD_LOADED:-1}" == "1" ]] || exit 113
    echo "gui/501/com.monetarium.node = {"
    echo "	state = ${LAUNCHD_STATE:-running}"
    echo "}"
    exit 0
fi
exit 0
MOCKEOF
chmod +x "$MOCK_BIN/launchctl"

# --------------------------------------------------------------------------
# Mock monetarium-ctl: getinfo always ok, getnewaddress returns addresses
# controlled by GETNEWADDR_COUNT (0 = always empty). The counter is a STABLE
# tracker path exported by the harness (NOT $$, which is this mock process's
# own PID and resets on every call), so successive invocations really count.
# Every invocation is appended to MOCK_CTL_LOG so tests can assert what ran.
# --------------------------------------------------------------------------
cat > "$MOCK_CTL/monetarium-ctl" <<'MOCKCTL'
#!/usr/bin/env bash
{ echo "$*" >> "${MOCK_CTL_LOG:-/dev/null}"; } 2>/dev/null || true
case "$*" in
    *getinfo*) exit 0 ;;
    *getnewaddress*)
        count="${GETNEWADDR_COUNT:-1}"
        tracker="${MOCK_CTL_TRACKER:-${TMPDIR:-/tmp}/.mock_ctl_$$}"
        current=$(cat "$tracker" 2>/dev/null || echo 0)
        current=$((current + 1))
        echo "$current" > "$tracker"
        if [[ "$current" -le "$count" ]]; then
            echo "TTestAddr${current}"
        fi
        exit 0
        ;;
    *setvotefeeconsolidationaddress*) exit 0 ;;
    *) exit 1 ;;
esac
MOCKCTL
chmod +x "$MOCK_CTL/monetarium-ctl"

export SYSTEMCTL_LOG LAUNCHCTL_LOG MOCK_STATE MOCK_CTL MOCK_BIN INSTALL_TEST_DIR MOCK_CTL_TRACKER MOCK_CTL_LOG
export PATH="$MOCK_BIN:$PATH"

# --------------------------------------------------------------------------
# check_selinux() tests
# --------------------------------------------------------------------------
echo "============================================"
echo "  check_selinux() tests"
echo "============================================"

SELINUX_TEST_DIR="$TEST_TMP/selinux-mocks"
mkdir -p "$SELINUX_TEST_DIR"
SELINUX_LOG="$TEST_TMP/selinux-log"

_selinux_test() {
    local mock_path="$1"
    local fake_uname="$2"
    rm -f "$SELINUX_LOG"
    (
        set +e
        export PATH="$mock_path":$PATH
        uname() { echo "$fake_uname"; }
        export -f uname
        check_selinux > "$SELINUX_LOG" 2>&1
    )
    _selinux_rc=$?
    _selinux_out=""
    [[ -f "$SELINUX_LOG" ]] && _selinux_out="$(cat "$SELINUX_LOG")"
}

cat > "$SELINUX_TEST_DIR/getenforce" <<'EOF'
#!/bin/bash
echo "Enforcing"
EOF
chmod +x "$SELINUX_TEST_DIR/getenforce"

_selinux_test "$SELINUX_TEST_DIR" "Linux"
check "enforcing → exit 1" bash -c "test '$_selinux_rc' = '1'"
echo "$_selinux_out" | grep -q "ENFORCING" \
    && { echo "  PASS  enforcing → output mentions ENFORCING"; PASS=$((PASS + 1)); } \
    || { echo "  FAIL  enforcing → output mentions ENFORCING"; FAIL=$((FAIL + 1)); }

cat > "$SELINUX_TEST_DIR/getenforce" <<'EOF'
#!/bin/bash
echo "Permissive"
EOF
chmod +x "$SELINUX_TEST_DIR/getenforce"

_selinux_test "$SELINUX_TEST_DIR" "Linux"
check "permissive → exit 0" bash -c "test '$_selinux_rc' = '0'"
echo "$_selinux_out" | grep -qi "permissive" \
    && { echo "  PASS  permissive → output mentions permissive"; PASS=$((PASS + 1)); } \
    || { echo "  FAIL  permissive → output mentions permissive"; FAIL=$((FAIL + 1)); }

cat > "$SELINUX_TEST_DIR/getenforce" <<'EOF'
#!/bin/bash
echo "Disabled"
EOF
chmod +x "$SELINUX_TEST_DIR/getenforce"

_selinux_test "$SELINUX_TEST_DIR" "Linux"
check "disabled → exit 0" bash -c "test '$_selinux_rc' = '0'"
check "disabled → no output" bash -c "test -z '$_selinux_out'"

rm -f "$SELINUX_TEST_DIR/getenforce"

_selinux_test "$SELINUX_TEST_DIR" "Linux"
check "no getenforce → exit 0" bash -c "test '$_selinux_rc' = '0'"
check "no getenforce → no output" bash -c "test -z '$_selinux_out'"

cat > "$SELINUX_TEST_DIR/getenforce" <<'EOF'
#!/bin/bash
echo "Enforcing"
EOF
chmod +x "$SELINUX_TEST_DIR/getenforce"

_selinux_test "$SELINUX_TEST_DIR" "Darwin"
check "macOS → no-op, exit 0" bash -c "test '$_selinux_rc' = '0'"

# --------------------------------------------------------------------------
# sha256_of / lc helpers
# --------------------------------------------------------------------------
echo ""
echo "============================================"
echo "  sha256_of / lc tests"
echo "============================================"

run_sha256_test() {
    printf 'hash me' > "$TEST_TMP/hashme"
    local out
    out="$(sha256_of "$TEST_TMP/hashme" | awk '{print $1}')"
    [[ "$out" =~ ^[0-9a-f]{64}$ ]]
}
check "sha256_of returns lowercase hex" run_sha256_test

run_lc_test() {
    [[ "$(lc 'ABCDEF0123456789')" == "abcdef0123456789" ]] && [[ "$(lc 'abcdef')" == "abcdef" ]]
}
check "lc lowercases uppercase hex" run_lc_test

# --------------------------------------------------------------------------
# RPC credential reuse (ensure_rpc_credentials / rpc_credential_from_conf)
# --------------------------------------------------------------------------
echo ""
echo "============================================"
echo "  RPC credential reuse tests"
echo "============================================"

reset_creds() {
    RPC_USER="monetarium"
    RPC_PASS=""
    rm -rf "$MONETARIUM_HOME"
}

write_prev_node_conf() {
    # $1 = rpcuser, $2 = rpcpass, $3 = optional generate=true+miningaddr marker
    mkdir -p "$MONETARIUM_HOME"
    cat > "$NODE_CONF" <<EOF
; previous install
rpcuser=$1
rpcpass=$2
addpeer=176.113.164.216:9508
EOF
    if [[ "$3" == "true" ]]; then
        {
            echo ""
            echo "; Mining configuration — carried over from previous install by install.sh"
            echo "generate=true"
            echo "miningaddr=TPrevAddr"
        } >> "$NODE_CONF"
    fi
}

run_creds_fresh_test() {
    reset_creds
    ensure_rpc_credentials
    [[ -n "$RPC_PASS" ]] && [[ "$RPC_PASS" =~ ^[a-zA-Z0-9]+$ ]] && [[ "${#RPC_PASS}" -le 32 ]]
}
check "no existing config → fresh creds generated" run_creds_fresh_test

run_creds_reuse_test() {
    reset_creds
    write_prev_node_conf "prevuser" "prevsecret" false
    ensure_rpc_credentials
    [[ "$RPC_USER" == "prevuser" ]] && [[ "$RPC_PASS" == "prevsecret" ]]
}
check "re-run → reuses existing rpcuser/rpcpass" run_creds_reuse_test

run_creds_reuse_no_user_test() {
    reset_creds
    write_prev_node_conf "prevuser" "prevsecret" false
    grep -v "^rpcuser=" "$NODE_CONF" > "$TEST_TMP/no-user.conf" && mv "$TEST_TMP/no-user.conf" "$NODE_CONF"
    ensure_rpc_credentials
}
expect_fail "conf without rpcuser → dies (no silent desync to default user)" run_creds_reuse_no_user_test

run_creds_empty_pass_test() {
    reset_creds
    mkdir -p "$MONETARIUM_HOME"
    printf 'rpcuser=prevuser\nrpcpass=\n' > "$NODE_CONF"
    ensure_rpc_credentials
}
expect_fail "conf with empty rpcpass → dies (no silent half-regeneration)" run_creds_empty_pass_test

run_creds_missing_pass_die_test() {
    reset_creds
    mkdir -p "$MONETARIUM_HOME"
    printf 'rpcuser=prevuser\n' > "$NODE_CONF"
    local out rc
    out="$(ensure_rpc_credentials 2>&1)"
    rc=$?
    [[ $rc -ne 0 ]] && echo "$out" | grep -q "usable rpcpass"
}
check "conf with missing rpcpass → dies with guidance" run_creds_missing_pass_die_test

run_creds_tail_wins_test() {
    reset_creds
    mkdir -p "$MONETARIUM_HOME"
    printf 'rpcuser=prevuser\nrpcpass=oldpass\nrpcpass=newpass\n' > "$NODE_CONF"
    ensure_rpc_credentials
    [[ "$RPC_PASS" == "newpass" ]]
}
check "duplicate keys → last line wins (tail -n1)" run_creds_tail_wins_test

run_creds_maintains_value_test() {
    reset_creds
    RPC_PASS="preset"
    mkdir -p "$MONETARIUM_HOME"
    ensure_rpc_credentials
    [[ "$RPC_PASS" == "preset" ]]
}
check "existing RPC_PASS with no config → kept (not overwritten)" run_creds_maintains_value_test

# --------------------------------------------------------------------------
# write_configs — reuses creds + carries over mining config across re-runs
# --------------------------------------------------------------------------
echo ""
echo "============================================"
echo "  write_configs tests"
echo "============================================"

run_wc_reuse_carryover_test() {
    reset_creds
    write_prev_node_conf "prevuser" "prevsecret" true
    WALLET_PASSPHRASE="test-secret" TICKETS_ENABLED=false VOTING_ENABLED=false
    write_configs
    grep -q "^rpcuser=prevuser$" "$NODE_CONF" \
        && grep -q "^rpcpass=prevsecret$" "$NODE_CONF" \
        && grep -q "^generate=true$" "$NODE_CONF" \
        && grep -q "^miningaddr=TPrevAddr$" "$NODE_CONF" \
        && grep -q "^username=prevuser$" "$WALLET_CONF" \
        && grep -q "^password=prevsecret$" "$WALLET_CONF" \
        && grep -q "^rpcpass=prevsecret$" "$CTL_CONF" \
        && [[ "$RPC_PASS" == "prevsecret" ]]
}
check "re-run → creds reused in node/wallet/ctl + mining config carried over" run_wc_reuse_carryover_test

run_wc_fresh_test() {
    reset_creds
    WALLET_PASSPHRASE="test-secret" TICKETS_ENABLED=false VOTING_ENABLED=false
    write_configs
    grep -q "^rpcuser=monetarium$" "$NODE_CONF" \
        && ! grep -q "^generate=" "$NODE_CONF" \
        && ! grep -q "^miningaddr=" "$NODE_CONF" \
        && grep -q "^pass=test-secret$" "$WALLET_CONF" \
        && grep -q "^rpcpass=${RPC_PASS}$" "$NODE_CONF"
}
check "fresh run → new creds, no carry-over, existing passphrase preserved" run_wc_fresh_test

run_wc_mining_false_carried_test() {
    reset_creds
    mkdir -p "$MONETARIUM_HOME"
    cat > "$NODE_CONF" <<EOF
rpcuser=prevuser
rpcpass=prevsecret
; Mining configuration — added post-install by install.sh
generate=false
EOF
    WALLET_PASSPHRASE="test-secret" TICKETS_ENABLED=false VOTING_ENABLED=false
    write_configs
    grep -q "^generate=false$" "$NODE_CONF"
}
check "re-run → generate=false carried over too" run_wc_mining_false_carried_test

# --------------------------------------------------------------------------
# latest_release_asset — GitHub digest + URL parsing (mock curl)
# --------------------------------------------------------------------------
echo ""
echo "============================================"
echo "  latest_release_asset tests"
echo "============================================"

_asset_json_4platforms() {
    local repo="$1"
    cat > "$MOCK_STATE/$repo.json" <<EOF
{ "assets": [
  { "name": "$repo-linux-amd64",
    "digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "browser_download_url": "https://fixtures.test/$repo-linux-amd64" },
  { "name": "$repo-linux-arm64",
    "digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    "browser_download_url": "https://fixtures.test/$repo-linux-arm64" },
  { "name": "$repo-darwin-amd64",
    "digest": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
    "browser_download_url": "https://fixtures.test/$repo-darwin-amd64" },
  { "name": "$repo-darwin-arm64",
    "digest": "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
    "browser_download_url": "https://fixtures.test/$repo-darwin-arm64" }
] }
EOF
}

run_asset_pick_test() {
    _asset_json_4platforms "monetarium-node"
    local out expected
    out="$(latest_release_asset "monetarium-node" "linux-amd64")"
    expected=$'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\thttps://fixtures.test/monetarium-node-linux-amd64'
    [[ "$out" == "$expected" ]]
}
check "picks matching platform asset + its digest" run_asset_pick_test

run_asset_darwin_test() {
    _asset_json_4platforms "monetarium-wallet"
    local out
    out="$(latest_release_asset "monetarium-wallet" "darwin-arm64")"
    [[ "$out" == $'sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd\thttps://fixtures.test/monetarium-wallet-darwin-arm64' ]]
}
check "darwin-arm64 → correct asset chosen" run_asset_darwin_test

run_asset_no_match_test() {
    _asset_json_4platforms "monetarium-node"
    latest_release_asset "monetarium-node" "freebsd-amd64" 2>/dev/null
}
expect_fail "no matching asset → dies" run_asset_no_match_test

run_asset_first_wins_test() {
    cat > "$MOCK_STATE/monetarium-node.json" <<EOF
{ "assets": [
  { "name": "monetarium-node-linux-amd64",
    "digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "browser_download_url": "https://fixtures.test/first-linux-amd64" },
  { "name": "second-linux-amd64",
    "digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    "browser_download_url": "https://fixtures.test/second-linux-amd64" }
] }
EOF
    local out
    out="$(latest_release_asset "monetarium-node" "linux-amd64")"
    [[ "$out" == "$(printf 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\t%s\n' 'https://fixtures.test/first-linux-amd64')" ]]
}
check "multiple matches → head -n1 wins" run_asset_first_wins_test

run_asset_no_digest_test() {
    cat > "$MOCK_STATE/monetarium-node.json" <<'EOF'
{ "assets": [ { "name": "monetarium-node-linux-amd64",
  "browser_download_url": "https://fixtures.test/monetarium-node-linux-amd64" } ] }
EOF
    local out
    out="$(latest_release_asset "monetarium-node" "linux-amd64")"
    [[ "$out" == "$(printf 'sha256:\t%s\n' 'https://fixtures.test/monetarium-node-linux-amd64')" ]]
}
check "asset without digest → empty digest published" run_asset_no_digest_test

run_asset_api_fail_test() {
    _asset_json_4platforms "monetarium-node"
    export MOCK_CURL_API_FAIL=1
    latest_release_asset "monetarium-node" "linux-amd64" 2>/dev/null
    local rc=$?
    unset MOCK_CURL_API_FAIL
    return $rc
}
expect_fail "unreachable API → returns non-zero, no output" run_asset_api_fail_test

# Regression: "digest" emitted AFTER "browser_download_url" inside the asset
# object must still resolve to THIS asset's digest (the parser must scope to
# the matched asset's own JSON object, not assume GitHub's field ordering).
run_asset_digest_after_url_test() {
    cat > "$MOCK_STATE/monetarium-node.json" <<'EOF'
{ "assets": [
  { "name": "monetarium-node-darwin-amd64",
    "digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
    "browser_download_url": "https://fixtures.test/monetarium-node-darwin-amd64" },
  { "name": "monetarium-node-linux-amd64",
    "browser_download_url": "https://fixtures.test/monetarium-node-linux-amd64",
    "digest": "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" }
] }
EOF
    local out
    out="$(latest_release_asset "monetarium-node" "linux-amd64")"
    [[ "$out" == "$(printf 'sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee\t%s\n' 'https://fixtures.test/monetarium-node-linux-amd64')" ]]
}
check "digest AFTER download_url → scoped to asset's own object" run_asset_digest_after_url_test

# --------------------------------------------------------------------------
# download_binary — fresh install / skip / self-update / mismatch abort
# --------------------------------------------------------------------------
echo ""
echo "============================================"
echo "  download_binary tests"
echo "============================================"

printf '#!/usr/bin/env bash\necho "monetarium-node v1"\n' > "$MOCK_STATE/blob-v1"
printf '#!/usr/bin/env bash\necho "monetarium-node v2"\n' > "$MOCK_STATE/blob-v2"

reset_download() {
    BINARIES_REPLACED=false
    NODE_REPLACED=false
    WALLET_REPLACED=false
    rm -rf "$INSTALL_TEST_DIR"
    mkdir -p "$INSTALL_TEST_DIR"
    INSTALL_DIR="$INSTALL_TEST_DIR"
    rm -f "$MOCK_STATE/downloads.log"
}

run_download_fresh_test() {
    reset_download
    json_for "monetarium-node" "$MOCK_STATE/blob-v1"
    PATH="$MOCK_BIN:$PATH" download_binary "monetarium-node" "monetarium-node" "linux-amd64"
    [[ -x "$INSTALL_DIR/monetarium-node" ]] \
        && cmp -s "$INSTALL_DIR/monetarium-node" "$MOCK_STATE/blob-v1" \
        && [[ "$(wc -l < "$MOCK_STATE/downloads.log")" -eq 1 ]] \
        && [[ "$BINARIES_REPLACED" == "false" ]]
}
check "fresh install → download + verify checksum + install" run_download_fresh_test

run_download_skip_current_test() {
    reset_download
    json_for "monetarium-node" "$MOCK_STATE/blob-v1"
    PATH="$MOCK_BIN:$PATH" download_binary "monetarium-node" "monetarium-node" "linux-amd64"
    json_for "monetarium-node" "$MOCK_STATE/blob-v1"
    PATH="$MOCK_BIN:$PATH" download_binary "monetarium-node" "monetarium-node" "linux-amd64"
    [[ "$(wc -l < "$MOCK_STATE/downloads.log")" -eq 1 ]] \
        && [[ "$BINARIES_REPLACED" == "false" ]]
}
check "re-run with current binary → skipped, no re-download" run_download_skip_current_test

run_download_update_test() {
    reset_download
    json_for "monetarium-node" "$MOCK_STATE/blob-v1"
    PATH="$MOCK_BIN:$PATH" download_binary "monetarium-node" "monetarium-node" "linux-amd64"
    json_for "monetarium-node" "$MOCK_STATE/blob-v2"
    BINARIES_REPLACED=false
    PATH="$MOCK_BIN:$PATH" download_binary "monetarium-node" "monetarium-node" "linux-amd64"
    cmp -s "$INSTALL_DIR/monetarium-node" "$MOCK_STATE/blob-v2" \
        && [[ "$(wc -l < "$MOCK_STATE/downloads.log")" -eq 2 ]] \
        && [[ "$BINARIES_REPLACED" == "true" ]]
}
check "upstream digest changed → binary self-updated + restart flagged" run_download_update_test

run_download_no_digest_keep_test() {
    reset_download
    json_for "monetarium-node" "$MOCK_STATE/blob-v1"      # digest published
    PATH="$MOCK_BIN:$PATH" download_binary "monetarium-node" "monetarium-node" "linux-amd64"
    BINARIES_REPLACED=false
    NODE_REPLACED=false
    json_for_no_digest "monetarium-node" "$MOCK_STATE/blob-v2"   # release drops the checksum
    PATH="$MOCK_BIN:$PATH" download_binary "monetarium-node" "monetarium-node" "linux-amd64"
    cmp -s "$INSTALL_DIR/monetarium-node" "$MOCK_STATE/blob-v1" \
        && [[ "$(wc -l < "$MOCK_STATE/downloads.log")" -eq 1 ]]
}
check "re-run, no published digest → keeps installed binary" run_download_no_digest_keep_test

run_download_no_digest_fresh_test() {
    reset_download
    json_for_no_digest "monetarium-node" "$MOCK_STATE/blob-v1"
    local out rc
    out="$(PATH="$MOCK_BIN:$PATH" download_binary "monetarium-node" "monetarium-node" "linux-amd64" 2>&1)"
    rc=$?
    # No published checksum must NOT hard-refuse a fresh install (that would
    # break every install against releases that omit one) — it warns and
    # installs, relying on transport TLS like the pre-checksum installer.
    [[ $rc -eq 0 ]] \
        && echo "$out" | grep -q "No sha256 checksum published" \
        && [[ -x "$INSTALL_DIR/monetarium-node" ]]
}
check "fresh install, no digest → warns and installs unverified" run_download_no_digest_fresh_test

run_download_mismatch_test() {
    reset_download
    json_for "monetarium-node" "$MOCK_STATE/blob-v1"    # digest = hash(v1)
    cp "$MOCK_STATE/blob-v2" "$MOCK_STATE/monetarium-node-linux-amd64"  # wire serves v2
    local out rc
    out="$(PATH="$MOCK_BIN:$PATH" download_binary "monetarium-node" "monetarium-node" "linux-amd64" 2>&1)"
    rc=$?
    [[ $rc -ne 0 ]] && echo "$out" | grep -q "Checksum mismatch" && [[ ! -f "$INSTALL_DIR/monetarium-node" ]]
}
check "checksum mismatch → aborts, binary not installed" run_download_mismatch_test

# Archive-asset fixture: serves a real .tar.gz whose published digest is of the
# tarball bytes (never the extracted binary). Covers Fix 4: archive assets must
# re-download every run instead of falsely claiming "already current".
archive_fixture() {
    local repo="$1" bin_name="$2" blob="$3"
    local name="${repo}-linux-amd64.tar.gz"
    local src="$TEST_TMP/archive-src"
    rm -rf "$src"; mkdir -p "$src"
    cp "$blob" "$src/$bin_name"
    tar -C "$src" -czf "$MOCK_STATE/$name" "$bin_name"
    local digest
    digest="$(digest_of "$MOCK_STATE/$name")"
    cat > "$MOCK_STATE/$repo.json" <<EOF
{ "assets": [ { "name": "$name", "digest": "sha256:$digest", "browser_download_url": "https://fixtures.test/$name" } ] }
EOF
}

run_download_archive_rerun_test() {
    reset_download
    archive_fixture "monetarium-node" "monetarium-node" "$MOCK_STATE/blob-v1"
    PATH="$MOCK_BIN:$PATH" download_binary "monetarium-node" "monetarium-node" "linux-amd64"
    BINARIES_REPLACED=false
    NODE_REPLACED=false
    archive_fixture "monetarium-node" "monetarium-node" "$MOCK_STATE/blob-v1"
    PATH="$MOCK_BIN:$PATH" download_binary "monetarium-node" "monetarium-node" "linux-amd64"
    cmp -s "$INSTALL_DIR/monetarium-node" "$MOCK_STATE/blob-v1" \
        && [[ "$(wc -l < "$MOCK_STATE/downloads.log")" -eq 2 ]] \
        && [[ "$BINARIES_REPLACED" == "true" ]]
}
check "archive asset re-run → downloaded + verified again (no false no-op)" run_download_archive_rerun_test

run_download_no_asset_die_test() {
    reset_download
    json_for "monetarium-node" "$MOCK_STATE/blob-v1"
    PATH="$MOCK_BIN:$PATH" download_binary "monetarium-node" "monetarium-node" "linux-amd64"
    BINARIES_REPLACED=false
    NODE_REPLACED=false
    local out rc
    out="$(PATH="$MOCK_BIN:$PATH" download_binary "monetarium-node" "monetarium-node" "freebsd-amd64" 2>&1)"
    rc=$?
    [[ $rc -ne 0 ]] && echo "$out" | grep -q "No release asset" && [[ "$BINARIES_REPLACED" == "false" ]]
}
check "no matching asset → dies even with a binary installed (not treated as outage)" run_download_no_asset_die_test

run_download_curl_fail_cleanup_test() {
    reset_download
    local before after
    before="$(ls -d "${TMPDIR:-/tmp}"/tmp.* 2>/dev/null | wc -l)"
    cat > "$MOCK_STATE/monetarium-node.json" <<'EOF'
{ "assets": [ { "name": "monetarium-node-linux-amd64",
  "digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "browser_download_url": "https://unhandled.invalid/monetarium-node-linux-amd64" } ] }
EOF
    local out rc
    out="$(PATH="$MOCK_BIN:$PATH" download_binary "monetarium-node" "monetarium-node" "linux-amd64" 2>&1)"
    rc=$?
    after="$(ls -d "${TMPDIR:-/tmp}"/tmp.* 2>/dev/null | wc -l)"
    [[ $rc -ne 0 ]] && echo "$out" | grep -q "Failed to download" && [[ "$after" -eq "$before" ]]
}
check "curl failure → dies with message, tmpdir cleaned up" run_download_curl_fail_cleanup_test

run_download_api_fail_with_binary_test() {
    reset_download
    json_for "monetarium-node" "$MOCK_STATE/blob-v1"
    PATH="$MOCK_BIN:$PATH" download_binary "monetarium-node" "monetarium-node" "linux-amd64"
    export MOCK_CURL_API_FAIL=1
    BINARIES_REPLACED=false
    PATH="$MOCK_BIN:$PATH" download_binary "monetarium-node" "monetarium-node" "linux-amd64"
    local rc=$?
    unset MOCK_CURL_API_FAIL
    [[ $rc -eq 0 ]] && [[ "$(wc -l < "$MOCK_STATE/downloads.log")" -eq 1 ]] && [[ "$BINARIES_REPLACED" == "false" ]]
}
check "unreachable API with binary installed → keep, no error" run_download_api_fail_with_binary_test

run_download_api_fail_no_binary_test() {
    reset_download
    export MOCK_CURL_API_FAIL=1
    local out rc
    out="$(PATH="$MOCK_BIN:$PATH" download_binary "monetarium-node" "monetarium-node" "linux-amd64" 2>&1)"
    rc=$?
    unset MOCK_CURL_API_FAIL
    [[ $rc -ne 0 ]] && echo "$out" | grep -q "Could not fetch latest release info"
}
check "unreachable API with no binary → dies" run_download_api_fail_no_binary_test

# --------------------------------------------------------------------------
# restart_monetarium_services
# --------------------------------------------------------------------------
echo ""
echo "============================================"
echo "  restart_monetarium_services tests"
echo "============================================"

run_restart_linux_active_test() {
    : > "$SYSTEMCTL_LOG"
    (
        export SYSTEMCTL_LOG
        uname() { echo "Linux"; }
        export -f uname
        export NODE_ACTIVE=0 WALLET_ACTIVE=0
        restart_monetarium_services monetarium-node monetarium-wallet
    )
    grep -q "systemctl restart monetarium-node" "$SYSTEMCTL_LOG" \
        && grep -q "systemctl restart monetarium-wallet" "$SYSTEMCTL_LOG"
}
check "Linux, services active → restarts node + wallet" run_restart_linux_active_test

run_restart_only_changed_test() {
    : > "$SYSTEMCTL_LOG"
    (
        export SYSTEMCTL_LOG
        uname() { echo "Linux"; }
        export -f uname
        export NODE_ACTIVE=0 WALLET_ACTIVE=0
        restart_monetarium_services monetarium-node
    )
    grep -q "systemctl restart monetarium-node" "$SYSTEMCTL_LOG" \
        && ! grep -q "systemctl restart monetarium-wallet" "$SYSTEMCTL_LOG"
}
check "Linux, only node replaced → only node restarted, not wallet" run_restart_only_changed_test

run_restart_no_args_test() {
    : > "$SYSTEMCTL_LOG"
    (
        export SYSTEMCTL_LOG
        uname() { echo "Linux"; }
        export -f uname
        export NODE_ACTIVE=0 WALLET_ACTIVE=0
        restart_monetarium_services
    )
    ! grep -q "restart" "$SYSTEMCTL_LOG"
}
check "restart with no replaced services → no-op" run_restart_no_args_test

run_restart_linux_inactive_test() {
    : > "$SYSTEMCTL_LOG"
    (
        export SYSTEMCTL_LOG
        uname() { echo "Linux"; }
        export -f uname
        export NODE_ACTIVE=1 WALLET_ACTIVE=1
        restart_monetarium_services monetarium-node monetarium-wallet
    )
    ! grep -q "restart" "$SYSTEMCTL_LOG"
}
check "Linux, services inactive → no restart attempted" run_restart_linux_inactive_test

run_restart_darwin_test() {
    : > "$LAUNCHCTL_LOG"
    (
        export LAUNCHCTL_LOG
        uname() { echo "Darwin"; }
        export -f uname
        restart_monetarium_services monetarium-node monetarium-wallet
    )
    grep -q "kickstart" "$LAUNCHCTL_LOG" \
        && grep -q "com.monetarium.node" "$LAUNCHCTL_LOG" \
        && grep -q "com.monetarium.wallet" "$LAUNCHCTL_LOG"
}
check "Darwin → launchctl kickstart node + wallet" run_restart_darwin_test

run_restart_darwin_only_changed_test() {
    : > "$LAUNCHCTL_LOG"
    (
        export LAUNCHCTL_LOG
        uname() { echo "Darwin"; }
        export -f uname
        restart_monetarium_services monetarium-wallet
    )
    grep -q "com.monetarium.wallet" "$LAUNCHCTL_LOG" \
        && ! grep -q "com.monetarium.node" "$LAUNCHCTL_LOG"
}
check "Darwin → only replaced binary's service kickstarted" run_restart_darwin_only_changed_test

run_service_is_active_darwin_test() {
    (
        set +e
        export LAUNCHCTL_LOG
        uname() { echo Darwin; }
        export -f uname
        LAUNCHD_STATE=running service_is_active monetarium-node || return 1
        # Loaded but not running: `launchctl print` still exits 0, so the exit
        # status alone would wrongly report the service as up.
        LAUNCHD_STATE="not running" service_is_active monetarium-node && return 1
        LAUNCHD_LOADED=0 service_is_active monetarium-node && return 1
        return 0
    )
}
check "Darwin: service_is_active reads the job state, not the exit code" run_service_is_active_darwin_test

run_wait_service_active_dies_test() {
    (
        set +e
        export LAUNCHCTL_LOG
        uname() { echo Darwin; }
        export -f uname
        SERVICE_SETTLE_SECS=2
        # Up at the first poll, then dies: the settle window must catch it.
        local calls=0
        service_is_active() {
            calls=$((calls + 1))
            (( calls <= 1 ))
        }
        ! wait_service_active monetarium-node
    )
}
check "wait_service_active: a service that dies after starting is a failure" run_wait_service_active_dies_test

run_wait_service_active_stays_up_test() {
    (
        set +e
        SERVICE_SETTLE_SECS=2
        service_is_active() { return 0; }
        wait_service_active monetarium-node
    )
}
check "wait_service_active: a service that stays up succeeds" run_wait_service_active_stays_up_test

run_wait_service_active_never_starts_test() {
    (
        set +e
        SERVICE_SETTLE_SECS=1
        service_is_active() { return 1; }
        ! wait_service_active monetarium-node
    )
}
check "wait_service_active: a service that never starts fails" run_wait_service_active_never_starts_test

# --------------------------------------------------------------------------
# generate/miningaddr guard tests (configure_mining_and_voting)
# --------------------------------------------------------------------------
echo ""
echo "============================================"
echo "  generate/miningaddr guard tests"
echo "============================================"

fresh_node_conf() {
    cat > "$NODE_CONF" <<'EOF'
; monetarium.conf - test
rpcuser=monetarium
rpcpass=testpass123
addpeer=176.113.164.216:9508
EOF
}

prev_mining_node_conf() {
    cat > "$NODE_CONF" <<'EOF'
; monetarium.conf - previous install, mining enabled
rpcuser=monetarium
rpcpass=testpass123
; Mining configuration — carried over from previous install by install.sh
generate=true
miningaddr=TPrevAddr
EOF
}

# Run configure_mining_and_voting with a mock wallet RPC, overriding has_tty
# and making sleep a no-op so the tests are fast. $1 = "tty" or "notty".
run_configure_mining() {
    local tty_mode="$1"
    rm -f "$MOCK_CTL_TRACKER"
    (
        set +e
        sleep() { :; }
        PATH="$MOCK_CTL:$MOCK_BIN:$PATH"
        show_configuration_summary() { :; }
        if [[ "$tty_mode" == "tty" ]]; then
            has_tty() { return 0; }
        else
            has_tty() { return 1; }
        fi
        configure_mining_and_voting
    )
}

generate_count() {
    grep -c "^generate=" "$NODE_CONF" || true
}

# --- no TTY (non-interactive) ------------------------------------------------
run_no_tty_fresh_test() {
    fresh_node_conf
    MINING_ENABLED=true
    MINING_CORES="2"
    MINING_ADDR=""
    CONSOLIDATION_ADDR=""
    export GETNEWADDR_COUNT=1
    run_configure_mining notty
    grep -q "^generate=false$" "$NODE_CONF"
}
check "no TTY, fresh → generate=false appended" run_no_tty_fresh_test

run_no_tty_rerun_test() {
    prev_mining_node_conf
    MINING_ENABLED=true
    MINING_CORES="2"
    MINING_ADDR=""
    CONSOLIDATION_ADDR=""
    run_configure_mining notty
    [[ "$(generate_count)" -eq 1 ]] \
        && grep -q "^generate=true$" "$NODE_CONF" \
        && ! grep -q "^generate=false$" "$NODE_CONF" \
        && grep -q "^miningaddr=TPrevAddr$" "$NODE_CONF"
}
check "no TTY, re-run → carried-over mining config preserved" run_no_tty_rerun_test

# --- RPC timeout (wallet never ready) ----------------------------------------
run_rpc_timeout_fresh_test() {
    fresh_node_conf
    MINING_ENABLED=true
    MINING_CORES="2"
    MINING_ADDR=""
    CONSOLIDATION_ADDR=""
    # Mock: getinfo always fails → triggers timeout path
    cat > "$MOCK_CTL/monetarium-ctl" <<'MOCKEOF'
#!/usr/bin/env bash
case "$*" in
    *getinfo*) exit 1 ;;
    *) exit 1 ;;
esac
MOCKEOF
    chmod +x "$MOCK_CTL/monetarium-ctl"
    export RPC_TIMEOUT_THRESHOLD=2
    run_configure_mining tty
    grep -q "^generate=false$" "$NODE_CONF"
}
check "RPC timeout, fresh → generate=false appended" run_rpc_timeout_fresh_test

run_rpc_timeout_rerun_test() {
    prev_mining_node_conf
    MINING_ENABLED=true
    MINING_CORES="2"
    MINING_ADDR=""
    CONSOLIDATION_ADDR=""
    export RPC_TIMEOUT_THRESHOLD=2
    run_configure_mining tty
    [[ "$(generate_count)" -eq 1 ]] \
        && grep -q "^generate=true$" "$NODE_CONF" \
        && ! grep -q "^generate=false$" "$NODE_CONF" \
        && grep -q "^miningaddr=TPrevAddr$" "$NODE_CONF"
}
check "RPC timeout, re-run → keeps carried-over mining, no generate=false" run_rpc_timeout_rerun_test

# Restore the full mock
cat > "$MOCK_CTL/monetarium-ctl" <<'MOCKCTL'
#!/usr/bin/env bash
{ echo "$*" >> "${MOCK_CTL_LOG:-/dev/null}"; } 2>/dev/null || true
case "$*" in
    *getinfo*) exit 0 ;;
    *getnewaddress*)
        count="${GETNEWADDR_COUNT:-1}"
        tracker="${MOCK_CTL_TRACKER:-${TMPDIR:-/tmp}/.mock_ctl_$$}"
        current=$(cat "$tracker" 2>/dev/null || echo 0)
        current=$((current + 1))
        echo "$current" > "$tracker"
        if [[ "$current" -le "$count" ]]; then
            echo "TTestAddr${current}"
        fi
        exit 0
        ;;
    *setvotefeeconsolidationaddress*) exit 0 ;;
    *) exit 1 ;;
esac
MOCKCTL
chmod +x "$MOCK_CTL/monetarium-ctl"

# --- RPC ready -----------------------------------------------------------------
run_rpc_ready_enabled_test() {
    fresh_node_conf
    MINING_ENABLED=true
    MINING_CORES="2"
    MINING_ADDR=""
    CONSOLIDATION_ADDR=""
    export GETNEWADDR_COUNT=1
    run_configure_mining tty
    [[ "$(generate_count)" -eq 1 ]] \
        && grep -q "^generate=true$" "$NODE_CONF" \
        && grep -q "^miningaddr=TTestAddr1$" "$NODE_CONF"
}
check "RPC ready + mining enabled → generate=true + address" run_rpc_ready_enabled_test

run_rpc_ready_rerun_test() {
    prev_mining_node_conf
    MINING_ENABLED=true
    MINING_CORES="2"
    MINING_ADDR=""
    CONSOLIDATION_ADDR=""
    export GETNEWADDR_COUNT=1
    run_configure_mining tty
    [[ "$(generate_count)" -eq 1 ]] \
        && grep -q "^generate=true$" "$NODE_CONF" \
        && grep -q "^miningaddr=TTestAddr1$" "$NODE_CONF" \
        && ! grep -q "TPrevAddr" "$NODE_CONF"
}
check "RPC ready, re-run → old mining lines stripped, one fresh set written" run_rpc_ready_rerun_test

run_mining_disabled_test() {
    fresh_node_conf
    MINING_ENABLED=false
    MINING_CORES=""
    MINING_ADDR=""
    CONSOLIDATION_ADDR=""
    export GETNEWADDR_COUNT=1
    run_configure_mining tty
    grep -q "^generate=false$" "$NODE_CONF"
}
check "mining not requested → generate=false" run_mining_disabled_test

run_empty_addr_test() {
    fresh_node_conf
    MINING_ENABLED=true
    MINING_CORES="2"
    MINING_ADDR=""
    CONSOLIDATION_ADDR=""
    export GETNEWADDR_COUNT=0
    run_configure_mining tty
    grep -q "^generate=false$" "$NODE_CONF" && ! grep -q "^miningaddr=" "$NODE_CONF"
}
check "empty getnewaddress → generate=false, no miningaddr" run_empty_addr_test

run_two_addrs_test() {
    fresh_node_conf
    MINING_ENABLED=true
    MINING_CORES="2"
    MINING_ADDR=""
    CONSOLIDATION_ADDR=""
    rm -f "$MOCK_CTL_LOG"
    export GETNEWADDR_COUNT=2
    run_configure_mining tty
    grep -q "^generate=true$" "$NODE_CONF" \
        && grep -q "^miningaddr=TTestAddr1$" "$NODE_CONF" \
        && grep -q "setvotefeeconsolidationaddress default TTestAddr2" "$MOCK_CTL_LOG"
}
check "two getnewaddr calls → 2nd DISTINCT addr + setvotefee invoked" run_two_addrs_test

# --------------------------------------------------------------------------
# show_configuration_summary (real function, controlled globals)
# --------------------------------------------------------------------------
echo ""
echo "============================================"
echo "  show_configuration_summary tests"
echo "============================================"

run_summary_no_addr_test() {
    (
        set +e
        fresh_node_conf
        MINING_ENABLED=true
        MINING_ADDR=""
        MINING_CORES="2"
        TICKETS_ENABLED=false
        VOTING_ENABLED=false
        CONSOLIDATION_ADDR=""
        output=$(show_configuration_summary 2>&1)
        echo "$output" | grep -q "requested but disabled"
    )
}
check "mining requested but no addr → 'requested but disabled'" run_summary_no_addr_test

run_summary_kept_prev_test() {
    (
        set +e
        prev_mining_node_conf
        MINING_ENABLED=true
        MINING_ADDR=""
        MINING_CORES="2"
        TICKETS_ENABLED=false
        VOTING_ENABLED=false
        CONSOLIDATION_ADDR=""
        output=$(show_configuration_summary 2>&1)
        echo "$output" | grep -q "unchanged from previous run" \
            && ! echo "$output" | grep -q "requested but disabled"
    )
}
check "re-run keeps prev mining config → summary says unchanged, not disabled" run_summary_kept_prev_test

run_summary_with_addr_test() {
    (
        set +e
        MINING_ENABLED=true
        MINING_ADDR="TTestAddr"
        MINING_CORES="2"
        TICKETS_ENABLED=false
        VOTING_ENABLED=false
        CONSOLIDATION_ADDR=""
        output=$(show_configuration_summary 2>&1)
        echo "$output" | grep -q "enabled" && echo "$output" | grep -q "TTestAddr"
    )
}
check "mining + addr → 'enabled' with address" run_summary_with_addr_test

run_summary_not_requested_test() {
    (
        set +e
        fresh_node_conf
        MINING_ENABLED=false
        MINING_ADDR=""
        MINING_CORES=""
        TICKETS_ENABLED=false
        VOTING_ENABLED=false
        CONSOLIDATION_ADDR=""
        output=$(show_configuration_summary 2>&1)
        echo "$output" | grep -q "disabled" && ! echo "$output" | grep -q "requested but disabled" && ! echo "$output" | grep -q "unchanged from previous run"
    )
}
check "mining not requested → plain 'disabled'" run_summary_not_requested_test

# --------------------------------------------------------------------------
# Update mode: detection + binary-only update
# --------------------------------------------------------------------------
echo ""
echo "============================================"
echo "  update mode detection tests"
echo "============================================"

# Every detection/update test runs in a subshell: install.sh globals such as
# IS_UPDATE_MODE, INSTALL_DIR and SYSTEMD_UNIT_DIR stay isolated per case.
reset_detect_env() {
    rm -rf "$MONETARIUM_HOME" "$TEST_TMP/systemd" "$TEST_TMP/runhome" "$INSTALL_TEST_DIR"
    mkdir -p "$MONETARIUM_HOME" "$TEST_TMP/systemd" "$TEST_TMP/runhome" "$INSTALL_TEST_DIR"
    INSTALL_DIR="$INSTALL_TEST_DIR"
    SYSTEMD_UNIT_DIR="$TEST_TMP/systemd"
    RUN_HOME="$TEST_TMP/runhome"
    IS_UPDATE_MODE=false
}

make_configs() {
    printf 'node-conf\n' > "$NODE_CONF"
    printf 'wallet-conf\n' > "$WALLET_CONF"
}

make_wallet_db() {
    mkdir -p "$(wallet_data_dir)/mainnet"
    printf 'wallet-db\n' > "$(wallet_data_dir)/mainnet/wallet.db"
}

make_service_unit() {
    printf 'unit\n' > "$SYSTEMD_UNIT_DIR/monetarium-node.service"
}

make_binaries() {
    local b
    for b in monetarium-node monetarium-wallet monetarium-ctl; do
        printf '#!/usr/bin/env bash\necho old\n' > "$INSTALL_DIR/$b"
        chmod +x "$INSTALL_DIR/$b"
    done
}

run_detect_fresh_test() {
    (
        set +e
        reset_detect_env
        uname() { echo Linux; }
        detect_install_mode
        [[ "$IS_UPDATE_MODE" == "false" ]]
    )
}
check "detect: nothing installed → install mode" run_detect_fresh_test

run_detect_configs_test() {
    (
        set +e
        reset_detect_env
        make_configs
        uname() { echo Linux; }
        detect_install_mode
        [[ "$IS_UPDATE_MODE" == "true" ]]
    )
}
check "detect: configs present, binaries missing → update/repair" run_detect_configs_test

run_detect_configs_and_unit_test() {
    (
        set +e
        reset_detect_env
        make_configs
        make_service_unit
        uname() { echo Linux; }
        detect_install_mode
        [[ "$IS_UPDATE_MODE" == "true" ]]
    )
}
check "detect: configs + service unit → update" run_detect_configs_and_unit_test

run_detect_unit_only_test() {
    (
        set +e
        reset_detect_env
        make_service_unit
        uname() { echo Linux; }
        has_tty() { return 1; }
        local out rc
        out="$(detect_install_mode 2>&1)"; rc=$?
        # A unit without configs is an existing installation, not a fresh one:
        # a full install would rewrite the unit with the current user.
        [[ $rc -ne 0 ]] && echo "$out" | grep -q "Cannot choose between install and update"
    )
}
check "detect: service unit without configs → asks (no TTY aborts)" run_detect_unit_only_test

run_detect_node_conf_only_test() {
    (
        set +e
        reset_detect_env
        printf 'node-conf\n' > "$NODE_CONF"
        uname() { echo Linux; }
        has_tty() { return 1; }
        local out rc
        out="$(detect_install_mode 2>&1)"; rc=$?
        # A lone node config must never be silently reinstalled: write_configs
        # would rewrite it and drop hand-edited settings.
        [[ $rc -ne 0 ]] && echo "$out" | grep -q "Cannot choose between install and update"
    )
}
check "detect: node config only → asks (no TTY aborts)" run_detect_node_conf_only_test

run_detect_node_conf_and_unit_test() {
    (
        set +e
        reset_detect_env
        printf 'node-conf\n' > "$NODE_CONF"
        make_service_unit
        uname() { echo Linux; }
        has_tty() { return 1; }
        local out rc
        out="$(detect_install_mode 2>&1)"; rc=$?
        [[ $rc -ne 0 ]] && echo "$out" | grep -q "Cannot choose between install and update"
    )
}
check "detect: node config + unit, wallet config missing → asks" run_detect_node_conf_and_unit_test

run_detect_wallet_conf_only_test() {
    (
        set +e
        reset_detect_env
        printf 'wallet-conf\n' > "$WALLET_CONF"
        uname() { echo Linux; }
        has_tty() { return 1; }
        local out rc
        out="$(detect_install_mode 2>&1)"; rc=$?
        [[ $rc -ne 0 ]] && echo "$out" | grep -q "Cannot choose between install and update"
    )
}
check "detect: wallet config only → asks (no TTY aborts)" run_detect_wallet_conf_only_test

run_detect_manifest_only_test() {
    (
        set +e
        reset_detect_env
        printf 'manifest\n' > "$MANIFEST"
        uname() { echo Linux; }
        has_tty() { return 1; }
        local out rc
        out="$(detect_install_mode 2>&1)"; rc=$?
        [[ $rc -ne 0 ]] && echo "$out" | grep -q "Cannot choose between install and update"
    )
}
check "detect: manifest only → asks (no TTY aborts)" run_detect_manifest_only_test

run_install_refuses_sudo_test() {
    (
        set +e
        # Simulate a sudo invocation: the runtime user differs from the
        # invoking one. A fresh install must refuse rather than create the
        # data directory as root inside the user's home.
        RUN_USER="someone-else"
        local out rc
        out="$(run_install "linux-amd64" 2>&1)"; rc=$?
        [[ $rc -ne 0 ]] && echo "$out" | grep -q "without sudo"
    )
}
check "install: a fresh install through sudo is refused" run_install_refuses_sudo_test

run_detect_service_and_wallet_no_configs_test() {
    (
        set +e
        reset_detect_env
        make_service_unit
        # Pin the platform BEFORE creating the wallet DB: wallet_data_dir()
        # calls uname, so on macOS the DB would land in a different path than
        # the one detect_install_mode() later checks.
        uname() { echo Linux; }
        make_wallet_db
        has_tty() { return 1; }
        local out rc
        out="$(detect_install_mode 2>&1)"; rc=$?
        [[ $rc -ne 0 ]] && echo "$out" | grep -q "Cannot choose between install and update"
    )
}
check "detect: service + wallet DB, no configs → asks (no TTY aborts)" run_detect_service_and_wallet_no_configs_test

run_detect_binaries_only_no_tty_test() {
    (
        set +e
        reset_detect_env
        make_binaries
        uname() { echo Linux; }
        has_tty() { return 1; }
        local out rc
        out="$(detect_install_mode 2>&1)"; rc=$?
        [[ $rc -ne 0 ]] && echo "$out" | grep -q "Cannot choose between install and update"
    )
}
check "detect: binaries only, no TTY → aborts instead of guessing" run_detect_binaries_only_no_tty_test

# Fake read_tty: stores the chosen answer in the variable name passed last.
fake_read_choice() {
    local __last
    for __last in "$@"; do :; done
    printf -v "$__last" '%s' "${UPDATE_CHOICE:-}"
}

run_detect_binaries_only_choose_update_test() {
    (
        set +e
        reset_detect_env
        make_binaries
        uname() { echo Linux; }
        has_tty() { return 0; }
        read_tty() { fake_read_choice "$@"; }
        UPDATE_CHOICE=1
        detect_install_mode
        [[ "$IS_UPDATE_MODE" == "true" ]]
    )
}
check "detect: binaries only + choice 1 → update" run_detect_binaries_only_choose_update_test

run_detect_binaries_only_choose_install_test() {
    (
        set +e
        reset_detect_env
        make_binaries
        uname() { echo Linux; }
        has_tty() { return 0; }
        read_tty() { fake_read_choice "$@"; }
        UPDATE_CHOICE=2
        detect_install_mode
        [[ "$IS_UPDATE_MODE" == "false" ]]
    )
}
check "detect: binaries only + choice 2 → full install" run_detect_binaries_only_choose_install_test

# --------------------------------------------------------------------------
# run_update: binary swaps + restart matrix
# --------------------------------------------------------------------------
echo ""
echo "============================================"
echo "  run_update tests"
echo "============================================"

reset_update_env() {
    rm -rf "$INSTALL_TEST_DIR" "$MONETARIUM_HOME" "$TEST_TMP/systemd" "$TEST_TMP/runhome"
    mkdir -p "$INSTALL_TEST_DIR" "$MONETARIUM_HOME" "$TEST_TMP/systemd" "$TEST_TMP/runhome"
    INSTALL_DIR="$INSTALL_TEST_DIR"
    SYSTEMD_UNIT_DIR="$TEST_TMP/systemd"
    RUN_HOME="$TEST_TMP/runhome"
    IS_UPDATE_MODE=true
    NODE_REPLACED=false
    WALLET_REPLACED=false
    BINARIES_REPLACED=false
    : > "$SYSTEMCTL_LOG"
    : > "$LAUNCHCTL_LOG"
    rm -f "$MOCK_STATE/downloads.log"
}

seed_installed() {  # $1 = blob for all three binaries
    local b
    for b in monetarium-node monetarium-wallet monetarium-ctl; do
        cp "$1" "$INSTALL_DIR/$b"
        chmod +x "$INSTALL_DIR/$b"
    done
}

publish_all() {  # $1 node blob, $2 wallet blob, $3 ctl blob
    json_for "monetarium-node"   "$1"
    json_for "monetarium-wallet" "$2"
    json_for "monetarium-ctl"    "$3"
}

run_update_nothing_changed_test() {
    (
        set +e
        reset_update_env
        seed_installed "$MOCK_STATE/blob-v1"
        publish_all "$MOCK_STATE/blob-v1" "$MOCK_STATE/blob-v1" "$MOCK_STATE/blob-v1"
        uname() { echo Linux; }
        export NODE_ACTIVE=0 WALLET_ACTIVE=0
        local out rc
        out="$(run_update "linux-amd64" 2>&1)"; rc=$?
        [[ $rc -eq 0 ]] \
            && echo "$out" | grep -q "already current" \
            && ! grep -q "restart" "$SYSTEMCTL_LOG"
    )
}
check "update: everything current → no restart, exit 0" run_update_nothing_changed_test

run_update_node_changed_test() {
    (
        set +e
        reset_update_env
        seed_installed "$MOCK_STATE/blob-v1"
        publish_all "$MOCK_STATE/blob-v2" "$MOCK_STATE/blob-v1" "$MOCK_STATE/blob-v1"
        uname() { echo Linux; }
        export NODE_ACTIVE=0 WALLET_ACTIVE=0
        run_update "linux-amd64" >/dev/null 2>&1
        cmp -s "$INSTALL_DIR/monetarium-node" "$MOCK_STATE/blob-v2" \
            && cmp -s "$INSTALL_DIR/monetarium-wallet" "$MOCK_STATE/blob-v1" \
            && grep -q "restart monetarium-node" "$SYSTEMCTL_LOG" \
            && ! grep -q "restart monetarium-wallet" "$SYSTEMCTL_LOG"
    )
}
check "update: node changed → node replaced + restarted, wallet untouched" run_update_node_changed_test

run_update_wallet_changed_test() {
    (
        set +e
        reset_update_env
        seed_installed "$MOCK_STATE/blob-v1"
        publish_all "$MOCK_STATE/blob-v1" "$MOCK_STATE/blob-v2" "$MOCK_STATE/blob-v1"
        uname() { echo Linux; }
        export NODE_ACTIVE=0 WALLET_ACTIVE=0
        run_update "linux-amd64" >/dev/null 2>&1
        grep -q "restart monetarium-wallet" "$SYSTEMCTL_LOG" \
            && ! grep -q "restart monetarium-node" "$SYSTEMCTL_LOG"
    )
}
check "update: wallet changed → wallet restarted, node not" run_update_wallet_changed_test

run_update_both_changed_order_test() {
    (
        set +e
        reset_update_env
        seed_installed "$MOCK_STATE/blob-v1"
        publish_all "$MOCK_STATE/blob-v2" "$MOCK_STATE/blob-v2" "$MOCK_STATE/blob-v2"
        uname() { echo Linux; }
        export NODE_ACTIVE=0 WALLET_ACTIVE=0
        run_update "linux-amd64" >/dev/null 2>&1
        local node_line wallet_line
        node_line="$(grep -n "restart monetarium-node" "$SYSTEMCTL_LOG" | cut -d: -f1 | head -n1)"
        wallet_line="$(grep -n "restart monetarium-wallet" "$SYSTEMCTL_LOG" | cut -d: -f1 | head -n1)"
        [[ -n "$node_line" && -n "$wallet_line" && "$node_line" -lt "$wallet_line" ]]
    )
}
check "update: both changed → node restarted before wallet" run_update_both_changed_order_test

run_update_stopped_service_test() {
    (
        set +e
        reset_update_env
        seed_installed "$MOCK_STATE/blob-v1"
        publish_all "$MOCK_STATE/blob-v2" "$MOCK_STATE/blob-v2" "$MOCK_STATE/blob-v2"
        uname() { echo Linux; }
        export NODE_ACTIVE=1 WALLET_ACTIVE=1   # 1 = inactive in the mock
        run_update "linux-amd64" >/dev/null 2>&1
        cmp -s "$INSTALL_DIR/monetarium-node" "$MOCK_STATE/blob-v2" \
            && ! grep -q "restart" "$SYSTEMCTL_LOG"
    )
}
check "update: service stopped before update → binary replaced, not started" run_update_stopped_service_test

run_update_stale_process_test() {
    (
        set +e
        reset_update_env
        seed_installed "$MOCK_STATE/blob-v1"
        publish_all "$MOCK_STATE/blob-v1" "$MOCK_STATE/blob-v1" "$MOCK_STATE/blob-v1"
        # Pin the platform BEFORE run_update: it calls service_is_active, which
        # branches on uname. On macOS the launchctl branch would be taken and
        # the restart would never reach the systemctl log.
        uname() { echo Linux; }
        export NODE_ACTIVE=0 WALLET_ACTIVE=1   # wallet inactive: only node considered
        # Current on disk, but the process predates the binary on disk.
        service_main_pid() { echo 4242; }
        process_start_epoch() { echo 1000; }
        file_mtime() { echo 2000; }
        run_update "linux-amd64" >/dev/null 2>&1
        grep -q "restart monetarium-node" "$SYSTEMCTL_LOG"
    )
}
check "update: stale process on current binary → restarted" run_update_stale_process_test

run_service_runs_current_binary_test() {
    (
        set +e
        service_main_pid() { echo 4242; }
        printf 'bin\n' > "$TEST_TMP/runsbin"
        process_start_epoch() { echo 2000; }
        file_mtime() { echo 1000; }
        service_runs_current_binary monetarium-node "$TEST_TMP/runsbin" || return 1
        process_start_epoch() { echo 1000; }
        file_mtime() { echo 2000; }
        ! service_runs_current_binary monetarium-node "$TEST_TMP/runsbin"
    )
}
check "service_runs_current_binary: compares process start vs binary mtime" run_service_runs_current_binary_test

run_update_preserves_state_test() {
    (
        set +e
        reset_update_env
        seed_installed "$MOCK_STATE/blob-v1"
        make_configs
        # Pin the platform before creating the wallet DB: wallet_data_dir()
        # calls uname, so the DB must land where run_update() looks for it.
        uname() { echo Linux; }
        make_wallet_db
        make_service_unit
        local unit="$SYSTEMD_UNIT_DIR/monetarium-node.service"
        local node_conf_hash wallet_conf_hash db_hash unit_hash
        node_conf_hash="$(digest_of "$NODE_CONF")"
        wallet_conf_hash="$(digest_of "$WALLET_CONF")"
        db_hash="$(digest_of "$(wallet_data_dir)/mainnet/wallet.db")"
        unit_hash="$(digest_of "$unit")"
        publish_all "$MOCK_STATE/blob-v2" "$MOCK_STATE/blob-v2" "$MOCK_STATE/blob-v2"
        uname() { echo Linux; }
        export NODE_ACTIVE=0 WALLET_ACTIVE=0
        run_update "linux-amd64" >/dev/null 2>&1
        [[ "$(digest_of "$NODE_CONF")" == "$node_conf_hash" ]] \
            && [[ "$(digest_of "$WALLET_CONF")" == "$wallet_conf_hash" ]] \
            && [[ "$(digest_of "$(wallet_data_dir)/mainnet/wallet.db")" == "$db_hash" ]] \
            && [[ "$(digest_of "$unit")" == "$unit_hash" ]] \
            && cmp -s "$INSTALL_DIR/monetarium-node" "$MOCK_STATE/blob-v2"
    )
}
check "update: configs, wallet DB and service unit byte-identical afterwards" run_update_preserves_state_test

run_update_never_installs_or_configures_test() {
    (
        set +e
        reset_update_env
        seed_installed "$MOCK_STATE/blob-v1"
        publish_all "$MOCK_STATE/blob-v2" "$MOCK_STATE/blob-v2" "$MOCK_STATE/blob-v2"
        local called="$TEST_TMP/called.log"
        rm -f "$called"
        local fn
        for fn in prompt_passphrase prompt_mining prompt_ticket_buyer write_configs \
                  create_wallet configure_mining_and_voting install_systemd_service \
                  install_launchd_service write_manifest print_warning check_selinux; do
            eval "${fn}() { echo '${fn}' >> \"\$called\"; }"
        done
        uname() { echo Linux; }
        export NODE_ACTIVE=0 WALLET_ACTIVE=0
        run_update "linux-amd64" >/dev/null 2>&1
        [[ ! -s "$called" ]]
    )
}
check "update: never prompts, writes configs, wallet, units or manifest" run_update_never_installs_or_configures_test

run_update_aborts_before_install_test() {
    (
        set +e
        reset_update_env
        seed_installed "$MOCK_STATE/blob-v1"
        publish_all "$MOCK_STATE/blob-v2" "$MOCK_STATE/blob-v2" "$MOCK_STATE/blob-v2"
        uname() { echo Linux; }
        export NODE_ACTIVE=0 WALLET_ACTIVE=0
        export MOCK_CURL_REPO_FAIL="monetarium-wallet"
        local out rc
        out="$(run_update "linux-amd64" 2>&1)"; rc=$?
        unset MOCK_CURL_REPO_FAIL
        # node was staged first, but the wallet failure aborted before any swap.
        [[ $rc -ne 0 ]] \
            && echo "$out" | grep -q "Could not reach GitHub" \
            && cmp -s "$INSTALL_DIR/monetarium-node" "$MOCK_STATE/blob-v1" \
            && ! grep -q "restart" "$SYSTEMCTL_LOG"
    )
}
check "update: one repo unreachable → abort, nothing installed or restarted" run_update_aborts_before_install_test

run_update_unreachable_api_test() {
    (
        set +e
        reset_download
        json_for "monetarium-node" "$MOCK_STATE/blob-v1"
        PATH="$MOCK_BIN:$PATH" download_binary "monetarium-node" "monetarium-node" "linux-amd64"
        IS_UPDATE_MODE=true
        export MOCK_CURL_API_FAIL=1
        local out rc
        out="$(PATH="$MOCK_BIN:$PATH" download_binary "monetarium-node" "monetarium-node" "linux-amd64" 2>&1)"; rc=$?
        unset MOCK_CURL_API_FAIL
        [[ $rc -ne 0 ]] && echo "$out" | grep -q "Aborting the update"
    )
}
check "update: GitHub unreachable → exits with error (does not silently skip)" run_update_unreachable_api_test

run_install_staged_binaries_test() {
    (
        set +e
        rm -rf "$TEST_TMP/stage"
        mkdir -p "$TEST_TMP/stage"
        printf '#!/usr/bin/env bash\necho staged\n' > "$TEST_TMP/stage/monetarium-node"
        chmod +x "$TEST_TMP/stage/monetarium-node"
        STAGING_DIR="$TEST_TMP/stage"
        INSTALL_DIR="$INSTALL_TEST_DIR"
        install_staged_binaries >/dev/null 2>&1
        cmp -s "$INSTALL_DIR/monetarium-node" "$STAGING_DIR/monetarium-node" \
            && [[ -z "$(find "$INSTALL_DIR" -name '.*.new.*' -print -quit)" ]]
    )
}
check "install_staged_binaries: swaps atomically, no temp file left" run_install_staged_binaries_test

run_install_staged_binaries_smoke_fail_test() {
    (
        set +e
        reset_update_env
        printf '#!/usr/bin/env bash\necho old\n' > "$INSTALL_DIR/monetarium-node"
        chmod +x "$INSTALL_DIR/monetarium-node"
        rm -rf "$TEST_TMP/stage"
        mkdir -p "$TEST_TMP/stage"
        printf '#!/usr/bin/env bash\nexit 1\n' > "$TEST_TMP/stage/monetarium-node"
        chmod +x "$TEST_TMP/stage/monetarium-node"
        STAGING_DIR="$TEST_TMP/stage"
        local out rc
        out="$(install_staged_binaries 2>&1)"; rc=$?
        [[ $rc -ne 0 ]] \
            && echo "$out" | grep -q "smoke test" \
            && grep -q "echo old" "$INSTALL_DIR/monetarium-node" \
            && [[ -z "$(find "$INSTALL_DIR" -name '.*.new.*' -print -quit)" ]]
    )
}
check "install_staged_binaries: failing --version aborts, old binary kept" run_install_staged_binaries_smoke_fail_test

run_install_staged_binaries_write_fail_test() {
    (
        set +e
        reset_update_env
        printf '#!/usr/bin/env bash\necho old\n' > "$INSTALL_DIR/monetarium-node"
        chmod +x "$INSTALL_DIR/monetarium-node"
        rm -rf "$TEST_TMP/stage"
        mkdir -p "$TEST_TMP/stage"
        printf '#!/usr/bin/env bash\necho new\n' > "$TEST_TMP/stage/monetarium-node"
        chmod +x "$TEST_TMP/stage/monetarium-node"
        STAGING_DIR="$TEST_TMP/stage"
        # Fail only the copy. This is a function-level failure, so it works
        # whether the suite runs as a normal user or as root.
        sudo() { [[ "$1" != "cp" ]] || return 1; command "$@"; }
        local out rc
        out="$(install_staged_binaries 2>&1)"; rc=$?
        [[ $rc -ne 0 ]] \
            && echo "$out" | grep -q "Could not write" \
            && grep -q "echo old" "$INSTALL_DIR/monetarium-node" \
            && [[ -z "$(find "$INSTALL_DIR" -name '.*.new.*' -print -quit)" ]]
    )
}
check "install_staged_binaries: copy failure aborts and leaves no temp" run_install_staged_binaries_write_fail_test

run_process_start_epoch_test() {
    local now epoch
    now="$(date +%s)"
    epoch="$(process_start_epoch $$)"
    [[ -n "$epoch" ]] && (( epoch <= now )) && (( epoch >= now - 300 ))
}
check "process_start_epoch: parses the current process start time" run_process_start_epoch_test

run_home_for_user_test() {
    local user home
    user="$(id -un)"
    home="$(_home_for_user "$user")"
    [[ -n "$home" && -d "$home" ]]
}
check "_home_for_user: resolves an existing home directory" run_home_for_user_test

# --------------------------------------------------------------------------
# Cleanup
# --------------------------------------------------------------------------
rm -f "$MOCK_CTL_TRACKER"
rm -rf "$TEST_TMP"

echo ""
echo "== Results: $PASS passed, $FAIL failed =="
[[ "$FAIL" -eq 0 ]]