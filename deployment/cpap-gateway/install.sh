#!/usr/bin/env bash
# One-time console installer for the CLIProxyAPI release-pull updater.
#
# It installs only the updater program, a fixed-argument wrapper and the
# cpap-deploy systemd service/timer. It never edits the application unit,
# config.yaml, auth material or logs, and it never restarts the application.
#
# Production use (as root):
#   ./install.sh
#   ./install.sh --enable
#
# The updater stays disabled unless --enable is passed, and --enable is only
# honoured after every check and the release preview have succeeded.

set -Eeuo pipefail

PROG=${0##*/}
MARKER='# Managed by CLIProxyAPI deployment/cpap-gateway/install.sh - do not edit.'

# Production paths. The internal test seam below prepends TEST_ROOT to every
# path; production always uses these literal values.
APP_DIR=/root/cliproxyapi
BINARY=$APP_DIR/cli-proxy-api
CONFIG=$APP_DIR/config.yaml
AUTH_DIR=/root/.cli-proxy-api
APP_UNIT=cliproxyapi.service
PORT=8317
STATE_DIR=/root/cliproxyapi.deploy
REPO=SCNU-PIONEER/CLIProxyAPI
KEEP_BACKUPS=5
LIB_DIR=/usr/local/lib/cpap-deploy
UPDATER=$LIB_DIR/cpap_deploy.py
OWNER_MARKER=$LIB_DIR/.installer-managed
BIN_DIR=/usr/local/bin
WRAPPER=$BIN_DIR/cpap-deploy
UNIT_DIR=/etc/systemd/system
SERVICE_UNIT=$UNIT_DIR/cpap-deploy.service
TIMER_UNIT=$UNIT_DIR/cpap-deploy.timer
TIMER_NAME=cpap-deploy.timer

ENABLE=0
BASELINE_VERSION=''
BASELINE_SET=0
TEST_MODE=0
TEST_ROOT=''
TMP_FILE=''
TMP_FILES=()

die() {
    printf '%s: error: %s\n' "$PROG" "$*" >&2
    exit 1
}

usage_error() {
    printf '%s: error: %s\n' "$PROG" "$*" >&2
    printf 'Try %s --help for usage.\n' "$PROG" >&2
    exit 2
}

warn() {
    printf '%s: warning: %s\n' "$PROG" "$*" >&2
}

note() {
    printf '==> %s\n' "$*"
}

usage() {
    cat <<'EOF'
Usage: sudo ./install.sh [--enable] [--baseline-version vX.Y.Z]

Install or upgrade the CLIProxyAPI release-pull updater on this server.
The updater is installed disabled unless --enable is given.

Options:
  --enable                  After all checks and a successful --dry-run,
                            enable and start cpap-deploy.timer.
  --baseline-version vX.Y.Z Record this running version when no baseline
                            state exists yet. Use it if the live binary
                            cannot report its own version.
  -h, --help                Show this help.

The installer only manages:
  /usr/local/lib/cpap-deploy/cpap_deploy.py
  /usr/local/bin/cpap-deploy
  /etc/systemd/system/cpap-deploy.service
  /etc/systemd/system/cpap-deploy.timer

It never modifies the application unit, config.yaml, auth material, logs or
the live binary. The timer runs the updater with --latest; this installer
never runs --latest itself.
EOF
}

cleanup() {
    local file
    for file in "${TMP_FILES[@]:-}"; do
        if [[ -n "$file" ]]; then
            rm -f -- "$file"
        fi
    done
}
trap cleanup EXIT

parse_args() {
    while (($#)); do
        case "$1" in
            --enable)
                ENABLE=1
                ;;
            --baseline-version)
                (($# >= 2)) || usage_error "--baseline-version requires a value"
                BASELINE_VERSION=$2
                BASELINE_SET=1
                shift
                ;;
            --baseline-version=*)
                BASELINE_VERSION=${1#*=}
                BASELINE_SET=1
                ;;
            -h|--help)
                usage
                exit 0
                ;;
            --)
                shift
                break
                ;;
            *)
                usage_error "unknown argument: $1"
                ;;
        esac
        shift
    done
    if (($#)); then
        usage_error "unexpected argument: $1"
    fi
    if ((BASELINE_SET)); then
        if [[ -z "$BASELINE_VERSION" || ! "$BASELINE_VERSION" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
            usage_error "--baseline-version must look like vX.Y.Z (for example v8.0.38)"
        fi
    fi
}

prefix_path() {
    printf '%s%s' "$TEST_ROOT" "$1"
}

setup_test_paths() {
    TEST_MODE=${CPAP_DEPLOY_TEST_MODE:-0}
    TEST_ROOT=${CPAP_DEPLOY_TEST_ROOT:-}

    if [[ "$TEST_MODE" != 0 && "$TEST_MODE" != 1 ]]; then
        die "CPAP_DEPLOY_TEST_MODE must be 0 or 1"
    fi

    if [[ "$TEST_MODE" == 0 ]]; then
        if [[ -n "$TEST_ROOT" ]]; then
            die "CPAP_DEPLOY_TEST_ROOT is only supported with CPAP_DEPLOY_TEST_MODE=1"
        fi
        return 0
    fi

    [[ -n "$TEST_ROOT" ]] || die "CPAP_DEPLOY_TEST_MODE=1 requires CPAP_DEPLOY_TEST_ROOT"
    case "$TEST_ROOT" in
        /*) ;;
        [A-Za-z]:/*) ;;
        *) die "CPAP_DEPLOY_TEST_ROOT must be an absolute path" ;;
    esac
    [[ "$TEST_ROOT" != "/" ]] || die "CPAP_DEPLOY_TEST_ROOT must not be /"
    TEST_ROOT=${TEST_ROOT%/}
    case "$TEST_ROOT" in
        *"'"*|*$'\n'*|*$'\r'*) die "CPAP_DEPLOY_TEST_ROOT contains an unsupported character" ;;
    esac

    APP_DIR=$(prefix_path /root/cliproxyapi)
    BINARY=$APP_DIR/cli-proxy-api
    CONFIG=$APP_DIR/config.yaml
    AUTH_DIR=$(prefix_path /root/.cli-proxy-api)
    STATE_DIR=$(prefix_path /root/cliproxyapi.deploy)
    LIB_DIR=$(prefix_path /usr/local/lib/cpap-deploy)
    UPDATER=$LIB_DIR/cpap_deploy.py
    OWNER_MARKER=$LIB_DIR/.installer-managed
    BIN_DIR=$(prefix_path /usr/local/bin)
    WRAPPER=$BIN_DIR/cpap-deploy
    UNIT_DIR=$(prefix_path /etc/systemd/system)
    SERVICE_UNIT=$UNIT_DIR/cpap-deploy.service
    TIMER_UNIT=$UNIT_DIR/cpap-deploy.timer

    warn "internal test mode: all managed paths are prefixed with $TEST_ROOT"
}

check_privileges() {
    if [[ "$TEST_MODE" == 0 ]]; then
        [[ "$(id -u)" -eq 0 ]] || die "run this installer as root (sudo $PROG ...)"
        [[ -d /run/systemd/system ]] || die "systemd does not appear to be running (/run/systemd/system is missing)"
    fi
}

check_tools() {
    local tool
    for tool in python3 systemctl install mktemp mkdir mv chmod chown stat grep cmp dirname cat rm id; do
        command -v "$tool" >/dev/null 2>&1 || die "required command not found: $tool"
    done

    PYTHON=$(command -v python3) || die "python3 was not found in PATH"
    case "$PYTHON" in
        *$'\n'*|*$'\r'*) die "python3 path contains a newline" ;;
        *[[:space:]]*) die "python3 path contains whitespace: $PYTHON" ;;
        *"'"*) die "python3 path contains a single quote: $PYTHON" ;;
    esac
    if [[ "$TEST_MODE" == 0 ]]; then
        [[ "$PYTHON" == /* ]] || die "python3 must resolve to an absolute path: $PYTHON"
    fi

    if ! "$PYTHON" -c 'import sys; sys.exit(0 if sys.version_info >= (3, 8) else 1)'; then
        die "Python 3.8 or newer is required: $PYTHON"
    fi
}

check_sources() {
    SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P) ||
        die "cannot resolve the installer directory"
    SRC_UPDATER=$SCRIPT_DIR/cpap_deploy.py
    SRC_SERVICE=$SCRIPT_DIR/cpap-deploy.service
    SRC_TIMER=$SCRIPT_DIR/cpap-deploy.timer

    local file
    for file in "$SRC_UPDATER" "$SRC_SERVICE" "$SRC_TIMER"; do
        [[ -f "$file" && -r "$file" ]] || die "missing or unreadable source file: $file"
    done

    grep -qF -- "$MARKER" "$SRC_SERVICE" ||
        die "refusing to use a source service file without the managed marker"
    grep -qF -- "$MARKER" "$SRC_TIMER" ||
        die "refusing to use a source timer file without the managed marker"
    grep -qxF 'ExecStart=/usr/local/bin/cpap-deploy --latest' "$SRC_SERVICE" ||
        die "source service file does not run the expected wrapper command"
    grep -qxF 'Unit=cpap-deploy.service' "$SRC_TIMER" ||
        die "source timer file does not point at cpap-deploy.service"
}

assert_real_dir() {
    local path=$1 required=$2
    if [[ -L "$path" ]]; then
        die "refusing to follow symlink: $path"
    fi
    if [[ -e "$path" ]]; then
        [[ -d "$path" ]] || die "not a directory: $path"
    elif [[ "$required" == "yes" ]]; then
        die "required directory is missing: $path"
    fi
}

assert_root_dir_safe() {
    local path=$1 mode owner
    [[ "$TEST_MODE" == 0 ]] || return 0
    owner=$(stat -c '%u' "$path") || die "cannot stat $path"
    mode=$(stat -c '%a' "$path") || die "cannot stat $path"
    [[ "$owner" == "0" ]] || die "$path is not owned by root"
    (( (8#$mode & 022) == 0 )) || die "$path is group/world writable"
}

assert_replacable_file() {
    local path=$1
    if [[ -L "$path" ]]; then
        die "refusing to follow symlink: $path"
    fi
    if [[ -e "$path" ]]; then
        [[ -f "$path" ]] || die "refusing to replace non-regular file: $path"
        [[ -r "$path" ]] || die "cannot read existing file: $path"
    fi
}

check_targets() {
    assert_real_dir "$(dirname -- "$LIB_DIR")" no
    assert_real_dir "$LIB_DIR" no
    assert_real_dir "$BIN_DIR" yes
    assert_real_dir "$UNIT_DIR" yes
    assert_real_dir "$STATE_DIR" no

    if [[ "$TEST_MODE" == 0 ]]; then
        assert_root_dir_safe "$BIN_DIR"
        assert_root_dir_safe "$UNIT_DIR"
        if [[ -d "$LIB_DIR" ]]; then
            assert_root_dir_safe "$LIB_DIR"
        fi
    fi

    if [[ -e "$STATE_DIR" ]]; then
        [[ -d "$STATE_DIR" ]] || die "state path is not a directory: $STATE_DIR"
        if [[ "$TEST_MODE" == 0 ]]; then
            local state_mode state_owner
            state_owner=$(stat -c '%u' "$STATE_DIR") || die "cannot stat $STATE_DIR"
            state_mode=$(stat -c '%a' "$STATE_DIR") || die "cannot stat $STATE_DIR"
            [[ "$state_owner" == "0" ]] || die "state directory is not owned by root: $STATE_DIR"
            (( (8#$state_mode & 077) == 0 )) ||
                die "state directory is group/world accessible; run: chmod 0700 $STATE_DIR"
        fi
    fi

    assert_replacable_file "$UPDATER"
    assert_replacable_file "$OWNER_MARKER"
    assert_replacable_file "$WRAPPER"
    assert_replacable_file "$SERVICE_UNIT"
    assert_replacable_file "$TIMER_UNIT"

    if [[ -e "$WRAPPER" ]] && ! grep -qF -- "$MARKER" "$WRAPPER"; then
        die "refusing to overwrite an unmanaged wrapper: $WRAPPER"
    fi
    if [[ -e "$SERVICE_UNIT" ]] && ! grep -qF -- "$MARKER" "$SERVICE_UNIT"; then
        die "refusing to overwrite an unmanaged unit: $SERVICE_UNIT"
    fi
    if [[ -e "$TIMER_UNIT" ]] && ! grep -qF -- "$MARKER" "$TIMER_UNIT"; then
        die "refusing to overwrite an unmanaged unit: $TIMER_UNIT"
    fi
    if [[ -e "$OWNER_MARKER" ]] && ! grep -qF -- "$MARKER" "$OWNER_MARKER"; then
        die "refusing to use an unexpected ownership marker: $OWNER_MARKER"
    fi

    if [[ -e "$UPDATER" ]] && ! cmp -s -- "$SRC_UPDATER" "$UPDATER"; then
        if [[ ! -e "$OWNER_MARKER" ]]; then
            die "refusing to overwrite an unmanaged updater: $UPDATER"
        fi
    fi

    if [[ "$TEST_MODE" == 0 ]]; then
        local vendor
        for vendor in /usr/lib/systemd/system /lib/systemd/system; do
            if [[ -e "$vendor/cpap-deploy.service" || -e "$vendor/cpap-deploy.timer" ]]; then
                die "a package-provided cpap-deploy unit already exists under $vendor"
            fi
        done
    fi
}

make_temp_for() {
    local destination=$1
    TMP_FILE=$(mktemp "${destination}.tmp.XXXXXX") ||
        die "cannot create a temporary file next to $destination"
    TMP_FILES+=("$TMP_FILE")
}

install_file() {
    local source=$1 destination=$2 mode=$3
    local tmp

    make_temp_for "$destination"
    tmp=$TMP_FILE
    if ! install -m "$mode" -- "$source" "$tmp"; then
        rm -f -- "$tmp"
        die "cannot stage $destination"
    fi
    if [[ "$TEST_MODE" == 0 ]]; then
        if ! chown root:root -- "$tmp"; then
            rm -f -- "$tmp"
            die "cannot set root ownership on $destination"
        fi
    fi
    if ! mv -f -- "$tmp" "$destination"; then
        rm -f -- "$tmp"
        die "cannot install $destination"
    fi
}

write_text_file() {
    local destination=$1 mode=$2
    shift 2
    local tmp

    make_temp_for "$destination"
    tmp=$TMP_FILE
    if ! printf '%s\n' "$@" >"$tmp"; then
        rm -f -- "$tmp"
        die "cannot stage $destination"
    fi
    if ! chmod "$mode" -- "$tmp"; then
        rm -f -- "$tmp"
        die "cannot set mode on $destination"
    fi
    if [[ "$TEST_MODE" == 0 ]]; then
        if ! chown root:root -- "$tmp"; then
            rm -f -- "$tmp"
            die "cannot set root ownership on $destination"
        fi
    fi
    if ! mv -f -- "$tmp" "$destination"; then
        rm -f -- "$tmp"
        die "cannot install $destination"
    fi
}

make_dir() {
    local path=$1 mode=$2
    if [[ "$TEST_MODE" == 0 ]]; then
        install -d -m "$mode" -o root -g root -- "$path" || die "cannot create $path"
    else
        mkdir -p -- "$path" || die "cannot create $path"
        chmod "$mode" -- "$path" || die "cannot set mode on $path"
    fi
}

shquote() {
    printf "'%s'" "$1"
}

write_wrapper() {
    local tmp argument

    make_temp_for "$WRAPPER"
    tmp=$TMP_FILE
    if ! {
        printf '%s\n' '#!/bin/sh'
        printf '%s\n' "$MARKER"
        printf '%s\n' '# Fixed server paths. Extra arguments are appended unchanged.'
        printf '%s\n' 'set -eu'
        printf 'exec %s %s' "$(shquote "$PYTHON")" "$(shquote "$UPDATER")"
        for argument in "${UPDATER_ARGS[@]}"; do
            printf ' \\\n  %s' "$(shquote "$argument")"
        done
        printf ' \\\n  "$@"\n'
    } >"$tmp"; then
        rm -f -- "$tmp"
        die "cannot generate $WRAPPER"
    fi
    if ! chmod 0755 -- "$tmp"; then
        rm -f -- "$tmp"
        die "cannot make $WRAPPER executable"
    fi
    if [[ "$TEST_MODE" == 0 ]]; then
        if ! chown root:root -- "$tmp"; then
            rm -f -- "$tmp"
            die "cannot set root ownership on $WRAPPER"
        fi
    fi
    if ! mv -f -- "$tmp" "$WRAPPER"; then
        rm -f -- "$tmp"
        die "cannot install $WRAPPER"
    fi
}

build_updater_args() {
    UPDATER_ARGS=(
        --binary "$BINARY"
        --app-dir "$APP_DIR"
        --config "$CONFIG"
        --auth-dir "$AUTH_DIR"
        --unit "$APP_UNIT"
        --port "$PORT"
        --state-dir "$STATE_DIR"
        --repo "$REPO"
        --keep-backups "$KEEP_BACKUPS"
    )
}

assert_unit_check_output() {
    local label=$1 output=$2 bad
    bad=$(grep -E '^(FAIL|WARN)[[:space:]]+.*(ExecStart|WorkingDirectory)' <<<"$output" || true)
    if [[ -n "$bad" ]]; then
        printf '%s\n' "$bad" >&2
        die "$label: unit path verification did not pass"
    fi
}

run_source_check() {
    local output rc
    note "checking the source updater against the live server"
    if output=$("$PYTHON" "$SRC_UPDATER" "${UPDATER_ARGS[@]}" --check 2>&1); then
        rc=0
    else
        rc=$?
    fi
    printf '%s\n' "$output"
    ((rc == 0)) || die "the source updater's --check failed with exit code $rc; nothing was changed"
    assert_unit_check_output "source --check" "$output"
}

run_installed_check() {
    local output rc
    note "verifying the installed wrapper"
    if output=$("$WRAPPER" --check 2>&1); then
        rc=0
    else
        rc=$?
    fi
    printf '%s\n' "$output"
    ((rc == 0)) || die "the installed updater's --check failed with exit code $rc; the timer was not enabled"
    assert_unit_check_output "installed --check" "$output"
}

record_baseline() {
    local args=(--baseline)
    if [[ -n "$BASELINE_VERSION" ]]; then
        args+=(--baseline-version "$BASELINE_VERSION")
    fi
    note "recording the baseline (an existing baseline and journal are preserved)"
    if ! "$WRAPPER" "${args[@]}"; then
        die "baseline failed; rerun with --baseline-version vX.Y.Z if the running binary cannot report its version"
    fi
}

preview_release() {
    local rc
    PREVIEW_OK=0
    note "previewing the newest release (no files are changed)"
    if "$WRAPPER" --dry-run; then
        PREVIEW_OK=1
        return 0
    else
        rc=$?
    fi
    if ((ENABLE)); then
        die "the release preview failed with exit code $rc; refusing to enable $TIMER_NAME"
    fi
    warn "the release preview failed with exit code $rc; the updater remains disabled or in its previous timer state"
}

install_files() {
    note "installing updater files"
    if [[ ! -d "$LIB_DIR" ]]; then
        make_dir "$LIB_DIR" 0755
    fi
    install_file "$SRC_UPDATER" "$UPDATER" 0644
    write_text_file "$OWNER_MARKER" 0644 "$MARKER"
    write_wrapper
    install_file "$SRC_SERVICE" "$SERVICE_UNIT" 0644
    install_file "$SRC_TIMER" "$TIMER_UNIT" 0644

    if [[ ! -d "$STATE_DIR" ]]; then
        make_dir "$STATE_DIR" 0700
    fi
    if ! systemctl daemon-reload; then
        die "systemctl daemon-reload failed"
    fi
}

timer_enabled() {
    systemctl is-enabled --quiet "$TIMER_NAME" >/dev/null 2>&1
}

enable_timer() {
    local were_enabled=0
    if timer_enabled; then
        were_enabled=1
    fi
    if ! systemctl enable --now "$TIMER_NAME"; then
        if ((were_enabled == 0)); then
            systemctl disable --now "$TIMER_NAME" >/dev/null 2>&1 || true
        fi
        die "systemctl enable --now $TIMER_NAME failed"
    fi
    if ! systemctl is-enabled --quiet "$TIMER_NAME"; then
        if ((were_enabled == 0)); then
            systemctl disable --now "$TIMER_NAME" >/dev/null 2>&1 || true
        fi
        die "$TIMER_NAME is not enabled after systemctl enable"
    fi
    if ! systemctl is-active --quiet "$TIMER_NAME"; then
        if ((were_enabled == 0)); then
            systemctl disable --now "$TIMER_NAME" >/dev/null 2>&1 || true
        fi
        die "$TIMER_NAME is not active after systemctl enable --now"
    fi
    note "$TIMER_NAME is enabled and active; the first check runs five minutes after activation"
}

report_disabled() {
    if timer_enabled; then
        note "$TIMER_NAME was already enabled and was left untouched"
    else
        note "$TIMER_NAME is not enabled; rerun with --enable after reviewing the preview"
    fi
}

print_summary() {
    printf '\n'
    printf 'Installed files:\n'
    printf '  %s\n' "$UPDATER"
    printf '  %s\n' "$WRAPPER"
    printf '  %s\n' "$SERVICE_UNIT"
    printf '  %s\n' "$TIMER_UNIT"
    printf '\n'
    if ((ENABLE)); then
        printf 'The timer can be inspected with:\n'
        printf '  systemctl status %s\n' "$TIMER_NAME"
        printf '  journalctl -u cpap-deploy.service\n'
    else
        if timer_enabled; then
            printf 'The updater is installed and %s is already enabled; its state was left untouched.\n' "$TIMER_NAME"
            printf 'Inspect it with:\n'
            printf '  systemctl status %s\n' "$TIMER_NAME"
        else
            printf 'The updater is installed but not enabled. To enable it:\n'
            printf '  sudo %s --enable\n' "$PROG"
        fi
    fi
}

main() {
    parse_args "$@"
    setup_test_paths
    check_tools
    check_privileges
    check_sources
    check_targets
    build_updater_args
    run_source_check
    install_files

    if ! record_baseline; then
        exit 1
    fi
    run_installed_check
    preview_release

    if ((ENABLE)); then
        ((PREVIEW_OK)) || die "refusing to enable $TIMER_NAME without a successful preview"
        enable_timer
    else
        report_disabled
    fi
    print_summary
}

main "$@"
