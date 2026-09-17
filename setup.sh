#!/usr/bin/env bash
# beads_watch guided installer — provision a tailnet node end to end.
#
# One-liner, from the latest GitHub Release:
#   curl -fsSL https://github.com/a44-io/beads_watch/releases/latest/download/setup.sh | bash
#
# A specific release:
#   curl -fsSL https://github.com/a44-io/beads_watch/releases/latest/download/setup.sh | bash -s -- --version v1.2.0
#
# Over ssh (prompts work through the tty):
#   ssh -t box 'curl -fsSL https://github.com/a44-io/beads_watch/releases/latest/download/setup.sh | bash'
#
# Fully unattended (takes every default: install, config from discovered repos,
# units yes, start yes):
#   ssh box 'curl -fsSL https://github.com/a44-io/beads_watch/releases/latest/download/setup.sh | bash -s -- --yes'
#
# From a checkout (nothing downloaded; builds what is there):
#   git clone https://github.com/a44-io/beads_watch.git && cd beads_watch && ./setup.sh
#
# From a private fileserver (a fleet that publishes with scripts/publish-dist.sh
# --url … --push …). The served copy has its dist URL stamped in; a
# hand-downloaded copy needs telling where the artifacts live, and a plain
# directory or file:// URL works the same way for an offline install:
#   curl -fsSL https://dist.example.com/setup.sh | BW_DIST_URL=https://dist.example.com bash
#   ./setup.sh --dist-url /mnt/dist
#
# Options:
#   --prefix DIR      install the binary to DIR (default: ~/.local/bin)
#   --version TAG     install this GitHub Release instead of the latest; also
#                     BW_VERSION. Not for use with --dist-url
#   --dist-url URL    artifact base URL (http://, https://, file://, or a
#                     plain directory); also BW_DIST_URL. Overrides the
#                     GitHub Release default
#   --from-source     ignore prebuilt binaries; always build with go
#   --force           reinstall the binary even when the installed commit is
#                     current; never touches an existing config
#   --yes, -y         accept the default answer for every prompt
#   --repo NAME=PATH  serve this repo; repeatable, and suppresses discovery
#   --rewrite-config  refresh the repos list in an existing config.json from
#                     discovery (or --repo); every other key is kept, and a
#                     timestamped backup is written first. Without it an
#                     existing config is never touched.
#   --no-config       do not create or touch ~/.config/beads_watch/config.json
#   --no-units        do not install the systemd user units
#   --no-start        install the units but do not start them
#   --no-verify       skip sha256 and signature verification of downloaded
#                     artifacts
#   --require-signature
#                     refuse to install unless the release's Sigstore bundles
#                     verify; without it, no cosign on the box or an unsigned
#                     release is a warning and sha256 alone decides. Also
#                     BW_REQUIRE_SIGNATURE=1
#   --port PORT       tailnet port the daemon listens on (default: 8438)
#   --proxy-host NAME the box running the reverse proxy; its forwarded identity
#                     headers are trusted. A MagicDNS name or an ssh alias
#                     for one; also BW_PROXY_HOST. No default: without this
#                     or --trusted-proxy, trusted_proxies is [] and no
#                     forwarded identity is believed; direct callers are
#                     still identified from their own address
#   --trusted-proxy A trust forwarded identity from this IP or CIDR instead;
#                     repeatable, and skips resolving --proxy-host
#   --dry-run         print the plan; change nothing
#   --uninstall       remove binary and units (keeps config and every .beads)
#   --quiet, -q       errors only
#   --no-gum          plain ANSI output even where gum is installed
#   --help, -h        this text
#
# Exit codes: 0 success · 1 a step failed · 2 misuse (bad flag) · 3 infrastructure
# (missing required tool, unreachable dist server, checksum or signature
# mismatch).
#
# On verification: every download is checked by sha256 against manifest.json,
# and against SHA256SUMS too when the dist carries one (a GitHub Release
# does), so `sha256sum -c SHA256SUMS` by hand agrees with what this script
# checked. That is integrity: the bytes that arrived are the bytes that were
# uploaded. Who uploaded them is a second question, and a release cut by
# .github/workflows/release.yml answers it: every asset has a Sigstore bundle
# beside it, signed keyless under the workflow's own identity, so when cosign
# is on this box the manifest and the tarball are verified against that
# identity before anything is installed, and a mismatch is exit 3. Without
# cosign, or for a release cut by hand (which carries no bundles), the script
# says so and sha256 decides; --require-signature turns either into a refusal.
# --no-verify skips all of it.

set -euo pipefail
umask 022
shopt -s lastpipe 2>/dev/null || true

# ── Constants and flag defaults ─────────────────────────────────────────────

# Stamped by scripts/publish-dist.sh: the fileserver URL for a --url publish,
# the release's own download base for a --release; empty in the repo copy.
DEFAULT_DIST_URL=""

# The public channel, when nothing above or on the command line says otherwise.
RELEASE_REPO="a44-io/beads_watch"
RELEASE_BASE="https://github.com/$RELEASE_REPO/releases"

# The only signing identity a release's Sigstore bundles may carry: the
# release workflow in this repo, run from a version tag. A personal login, a
# fork's copy of the workflow, or a run from a branch all fail this check, on
# purpose. scripts/publish-dist.sh carries the same pair; they move together.
SIGN_IDENTITY_RE='^https://github\.com/a44-io/beads_watch/\.github/workflows/release\.yml@refs/tags/v[0-9]'
SIGN_ISSUER='https://token.actions.githubusercontent.com'

PREFIX="${BW_PREFIX:-$HOME/.local/bin}"
DIST_URL="${BW_DIST_URL:-}"
VERSION="${BW_VERSION:-}"
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/beads_watch"
CONFIG_FILE="$CONFIG_DIR/config.json"
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
SRC_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/beads_watch/src"
LOCK_DIR="${TMPDIR:-/tmp}/beads_watch-setup.lock.d"
BRIDGE_PORT=8438
# No default proxy box: nothing here knows what fronts this box, and a guessed
# name that happens to resolve would hand that machine the right to assert
# who every caller is. Unset means trusted_proxies stays [] and the daemon
# identifies each caller from its own address.
PROXY_HOST="${BW_PROXY_HOST:-}"
TRUSTED_PROXIES=()
BIN_NAME=beads_watch
SERVICE_UNIT=beads_watch.service
# The socket-activated bridge that used to sit between caddy and the socket.
# The daemon binds the tailnet address itself now; these names remain so an
# upgrade can retire the pair.
PROXY_SOCKET=beads_watch-proxy.socket
PROXY_SERVICE=beads_watch-proxy.service

ASSUME_YES=0
QUIET=0
NO_GUM=0
FROM_SOURCE=0
FORCE=0
NO_CHECKSUM=0
REQUIRE_SIGNATURE="${BW_REQUIRE_SIGNATURE:-0}"
SIG_STATUS="" # what the signature step concluded, for the summary box
DRY_RUN=0
DO_UNINSTALL=0
NO_CONFIG=0
REWRITE_CONFIG=0
NO_UNITS=0
NO_START=0
CLI_REPOS=()

# Set when this run actually replaced the binary or a unit file. A running
# daemon keeps its old executable mapped, so installing over it changes
# nothing until something restarts it.
BINARY_CHANGED=0
UNITS_CHANGED=0

# Where this script lives; a repo checkout if go.mod sits beside it.
HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd) || HERE=$PWD
REPO_MODE=0
[[ -f "$HERE/go.mod" && -f "$HERE/main.go" ]] && REPO_MODE=1

# ── Output stack: gum with ANSI fallback ────────────────────────────────────

HAS_GUM=0
if command -v gum &>/dev/null && [[ -t 1 ]]; then
  HAS_GUM=1
fi

use_gum() { [[ "$HAS_GUM" -eq 1 && "$NO_GUM" -eq 0 ]]; }

info() {
  [[ "$QUIET" -eq 1 ]] && return 0
  if use_gum; then gum style --foreground 39 "→ $*"; else echo -e "\033[0;34m→\033[0m $*"; fi
}
ok() {
  [[ "$QUIET" -eq 1 ]] && return 0
  if use_gum; then gum style --foreground 42 "✓ $*"; else echo -e "\033[0;32m✓\033[0m $*"; fi
}
warn() {
  [[ "$QUIET" -eq 1 ]] && return 0
  if use_gum; then gum style --foreground 214 "⚠ $*"; else echo -e "\033[1;33m⚠\033[0m $*"; fi
}
err() {
  if use_gum; then gum style --foreground 196 "✗ $*" >&2; else echo -e "\033[0;31m✗\033[0m $*" >&2; fi
}
die() { # message [exit_code]
  err "$1"
  exit "${2:-1}"
}

run_with_spinner() {
  local title="$1"; shift
  # gum spin fork/execs its argv, so a shell FUNCTION is invisible to it —
  # run functions plainly and only spinner real executables.
  if use_gum && [[ "$QUIET" -eq 0 ]] && [[ "$(type -t "$1")" != function ]]; then
    gum spin --spinner dot --title "$title" -- "$@"
  else
    info "$title"
    "$@"
  fi
}

phase() {
  [[ "$QUIET" -eq 1 ]] && return 0
  echo ""
  if use_gum; then
    gum style --foreground 212 --bold "── $* ──"
  else
    echo -e "\033[1;35m── $* ──\033[0m"
  fi
}

# draw_box COLOR line... — double-line box, ANSI-aware width.
draw_box() {
  local color="$1"; shift
  local lines=("$@") max_width=0 esc stripped len
  esc=$(printf '\033')
  local strip="s/${esc}\\[[0-9;]*m//g"
  for line in "${lines[@]}"; do
    stripped=$(printf '%b' "$line" | LC_ALL=C sed "$strip")
    len=${#stripped}
    ((len > max_width)) && max_width=$len
  done
  local inner=$((max_width + 4)) border=""
  local i
  for ((i = 0; i < inner; i++)); do border+="═"; done
  printf "\033[%sm╔%s╗\033[0m\n" "$color" "$border"
  for line in "${lines[@]}"; do
    stripped=$(printf '%b' "$line" | LC_ALL=C sed "$strip")
    len=${#stripped}
    local pad=""
    for ((i = 0; i < max_width - len; i++)); do pad+=" "; done
    printf "\033[%sm║\033[0m  %b%s  \033[%sm║\033[0m\n" "$color" "$line" "$pad" "$color"
  done
  printf "\033[%sm╚%s╝\033[0m\n" "$color" "$border"
}

# ── Interactivity: prompts must survive `curl | bash` ───────────────────────

INTERACTIVE=0
if [[ -t 0 ]]; then
  INTERACTIVE=1
elif [[ -e /dev/tty ]] && (exec </dev/tty) 2>/dev/null; then
  INTERACTIVE=1
fi

# ask_yn "question" default(y|n) → 0 yes / 1 no.
ask_yn() {
  local prompt="$1" def="${2:-y}" ans
  if [[ "$ASSUME_YES" -eq 1 || "$INTERACTIVE" -eq 0 ]]; then
    local why="--yes"
    [[ "$ASSUME_YES" -eq 1 ]] || why="non-interactive"
    if [[ "$def" == y ]]; then
      info "$prompt → yes ($why default)"
      return 0
    fi
    info "$prompt → no ($why default)"
    return 1
  fi
  if use_gum; then
    local defflag=--default=true
    [[ "$def" == y ]] || defflag=--default=false
    gum confirm "$defflag" "$prompt" </dev/tty && return 0 || return 1
  fi
  local hint="[Y/n]"
  [[ "$def" == y ]] || hint="[y/N]"
  printf '%s %s ' "$prompt" "$hint" >/dev/tty
  read -r ans </dev/tty || ans=""
  case "$ans" in
    y | Y | yes | Yes | YES) return 0 ;;
    n | N | no | No | NO) return 1 ;;
    *) [[ "$def" == y ]] && return 0 || return 1 ;;
  esac
}

usage() {
  sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//'
}

need_value() { [[ -n "${2:-}" ]] || die "$1 needs a value" 2; }

# ── Arguments ───────────────────────────────────────────────────────────────

while [[ $# -gt 0 ]]; do
  case "$1" in
    --prefix) need_value --prefix "${2:-}"; PREFIX="$2"; shift 2 ;;
    --version) need_value --version "${2:-}"; VERSION="$2"; shift 2 ;;
    --dist-url) need_value --dist-url "${2:-}"; DIST_URL="${2%/}"; shift 2 ;;
    --repo) need_value --repo "${2:-}"; CLI_REPOS+=("$2"); shift 2 ;;
    --port) need_value --port "${2:-}"; BRIDGE_PORT="$2"; shift 2 ;;
    --proxy-host) need_value --proxy-host "${2:-}"; PROXY_HOST="$2"; shift 2 ;;
    --trusted-proxy) need_value --trusted-proxy "${2:-}"; TRUSTED_PROXIES+=("$2"); shift 2 ;;
    --from-source) FROM_SOURCE=1; shift ;;
    --force) FORCE=1; shift ;;
    --yes | -y) ASSUME_YES=1; shift ;;
    --no-config) NO_CONFIG=1; shift ;;
    --rewrite-config) REWRITE_CONFIG=1; shift ;;
    --no-units) NO_UNITS=1; shift ;;
    --no-start) NO_START=1; shift ;;
    --no-verify) NO_CHECKSUM=1; shift ;;
    --require-signature) REQUIRE_SIGNATURE=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --uninstall) DO_UNINSTALL=1; shift ;;
    --quiet | -q) QUIET=1; shift ;;
    --no-gum) NO_GUM=1; shift ;;
    --help | -h) usage; exit 0 ;;
    *) die "unknown argument '$1' (try --help)" 2 ;;
  esac
done

[[ "$BRIDGE_PORT" =~ ^[0-9]+$ ]] || die "--port wants a number, got '$BRIDGE_PORT'" 2
[[ "$NO_CONFIG" -eq 1 && "$REWRITE_CONFIG" -eq 1 ]] && die "--no-config and --rewrite-config contradict each other" 2

# Where the artifacts come from, in order: an explicit --dist-url / BW_DIST_URL
# (a fleet's fileserver, a directory, a file:// URL); --version, which names a
# GitHub Release; the URL publish-dist.sh stamped into this copy; the checkout
# this script sits in; and, for a bare copy of the repo file run anywhere
# else, the latest GitHub Release. Tags are vX.Y.Z, so a bare X.Y.Z is
# forgiven.
if [[ -n "$VERSION" && "$VERSION" != v* ]]; then VERSION="v$VERSION"; fi
[[ "$NO_CHECKSUM" -eq 1 && "$REQUIRE_SIGNATURE" -eq 1 ]] &&
  die "--no-verify and --require-signature contradict each other; pass one or the other" 2
if [[ -n "$DIST_URL" ]]; then
  [[ -z "$VERSION" ]] || die "--version names a GitHub Release, and --dist-url / BW_DIST_URL ($DIST_URL) names somewhere else; pass one or the other" 2
elif [[ -n "$VERSION" ]]; then
  DIST_URL="$RELEASE_BASE/download/$VERSION"
elif [[ -n "$DEFAULT_DIST_URL" ]]; then
  DIST_URL="$DEFAULT_DIST_URL"
elif [[ "$REPO_MODE" -eq 0 ]]; then
  DIST_URL="$RELEASE_BASE/latest/download"
fi

# ── Platform ────────────────────────────────────────────────────────────────

detect_platform() {
  OS=$(uname -s | tr '[:upper:]' '[:lower:]')
  ARCH=$(uname -m)
  case "$ARCH" in
    x86_64 | amd64) GOARCH=amd64 ;;
    arm64 | aarch64) GOARCH=arm64 ;;
    *) GOARCH="" ;;
  esac
  case "$OS" in
    linux | darwin) GOOS="$OS" ;;
    *) GOOS="" ;;
  esac
  TARGET=""
  [[ -n "$GOOS" && -n "$GOARCH" ]] && TARGET="$GOOS-$GOARCH"
  if [[ "$OS" == linux ]] && grep -qi microsoft /proc/version 2>/dev/null; then
    warn "WSL detected. systemd user units may not be available; continuing as linux"
  fi
}

systemd_user_available() {
  command -v systemctl &>/dev/null || return 1
  systemctl --user show-environment &>/dev/null && return 0
  systemctl --user is-system-running &>/dev/null && return 0
  return 1
}

PROXY_ARGS=()
setup_proxy() {
  PROXY_ARGS=()
  if [[ -n "${HTTPS_PROXY:-}" ]]; then
    PROXY_ARGS=(--proxy "$HTTPS_PROXY")
    info "Using HTTPS proxy: $HTTPS_PROXY"
  elif [[ -n "${HTTP_PROXY:-}" ]]; then
    PROXY_ARGS=(--proxy "$HTTP_PROXY")
    info "Using HTTP proxy: $HTTP_PROXY"
  fi
}

sha256_of() {
  if command -v sha256sum &>/dev/null; then
    sha256sum "$1" | cut -d' ' -f1
  elif command -v shasum &>/dev/null; then
    shasum -a 256 "$1" | cut -d' ' -f1
  else
    echo ""
  fi
}

verify_checksum() { # file expected label
  local file="$1" expected="$2" label="$3" actual
  [[ "$NO_CHECKSUM" -eq 1 ]] && { warn "checksum for $label skipped (--no-verify)"; return 0; }
  [[ -n "$expected" ]] || { warn "manifest carries no sha256 for $label"; return 0; }
  actual=$(sha256_of "$file")
  if [[ -z "$actual" ]]; then
    warn "no sha256sum/shasum on this box; cannot verify $label"
    return 0
  fi
  if [[ "$actual" != "$expected" ]]; then
    err "checksum mismatch for $label"
    err "  expected: $expected"
    err "  got:      $actual"
    rm -f "$file"
    return 1
  fi
  ok "sha256 verified: $label (${actual:0:16}…)"
}

# ── Locking and cleanup ─────────────────────────────────────────────────────

acquire_lock() {
  if mkdir "$LOCK_DIR" 2>/dev/null; then
    echo $$ >"$LOCK_DIR/pid"
    return 0
  fi
  local old
  old=$(cat "$LOCK_DIR/pid" 2>/dev/null || echo "")
  if [[ -n "$old" ]] && ! kill -0 "$old" 2>/dev/null; then
    rm -rf "$LOCK_DIR"
    if mkdir "$LOCK_DIR" 2>/dev/null; then
      echo $$ >"$LOCK_DIR/pid"
      return 0
    fi
  fi
  die "another setup.sh is running (lock: $LOCK_DIR, pid ${old:-unknown})" 3
}

TMP=""
HELD_LOCK=0
cleanup() {
  [[ -n "$TMP" ]] && rm -rf "$TMP"
  [[ "$HELD_LOCK" -eq 1 ]] && rm -rf "$LOCK_DIR" 2>/dev/null
  return 0
}

# ── Preflight ───────────────────────────────────────────────────────────────

# A stock macOS has no `timeout`; homebrew coreutils installs it as `gtimeout`.
# Without either, run bare rather than skip the check.
TIMEOUT_CMD=()
if command -v timeout &>/dev/null; then
  TIMEOUT_CMD=(timeout 5)
elif command -v gtimeout &>/dev/null; then
  TIMEOUT_CMD=(gtimeout 5)
fi

installed_commit() { # of the beads_watch at $PREFIX/$BIN_NAME; empty when absent or unstamped
  local bin="$PREFIX/$BIN_NAME"
  [[ -x "$bin" ]] || return 0
  "${TIMEOUT_CMD[@]}" "$bin" --version 2>/dev/null |
    sed -n 's/^commit:[[:space:]]*\([0-9a-f]\{7,40\}\).*/\1/p' | head -1
}

preflight() {
  phase "Preflight"
  detect_platform
  info "platform: ${OS}/${ARCH}${TARGET:+ (go: $TARGET)}"

  command -v curl &>/dev/null || die 'curl is required' 3
  command -v tar &>/dev/null || die 'tar is required' 3

  local avail
  avail=$(df -Pk "$HOME" 2>/dev/null | awk 'NR==2 {print $4}')
  if [[ -n "$avail" && "$avail" -lt 51200 ]]; then
    die "less than 50MB free on $HOME" 3
  fi

  mkdir -p "$PREFIX" 2>/dev/null || die "cannot create $PREFIX" 3
  [[ -w "$PREFIX" ]] || die "$PREFIX is not writable" 3

  if ! command -v br &>/dev/null; then
    warn "br is not on PATH. The daemon execs it per request, so install beads before serving."
  else
    ok "br: $(command -v br)"
  fi

  local cur
  cur=$(installed_commit)
  if [[ -n "$cur" ]]; then
    info "installed: $PREFIX/$BIN_NAME @ ${cur:0:12}"
  elif [[ -x "$PREFIX/$BIN_NAME" ]]; then
    info "installed: $PREFIX/$BIN_NAME (unstamped source build)"
  fi
}

# ── Artifacts ───────────────────────────────────────────────────────────────

MANIFEST=""
MANIFEST_COMMIT=""
MANIFEST_TAG=""
SUMS="" # SHA256SUMS beside the manifest, when the dist has one

manifest_field() { # manifest_field '["bundle"]["sha256"]'
  python3 -c '
import json, sys
m = json.load(open(sys.argv[1]))
try:
    v = eval("m" + sys.argv[2], {"m": m})
    print(v if v is not None else "")
except Exception:
    print("")
' "$MANIFEST" "$1"
}

fetch() { # url dest
  case "$DIST_URL" in
    file://*) cp "${1#file://}" "$2" 2>/dev/null ;;
    *) curl -fsSL "${PROXY_ARGS[@]}" "$1" -o "$2" ;;
  esac
}

# The sha256 SHA256SUMS records for NAME (a bare file name, as sha256sum
# writes it), or nothing when there is no SHA256SUMS or no line for it.
sums_sha_for() {
  [[ -n "$SUMS" ]] || return 0
  awk -v f="$1" '$2 == f || $2 == "*" f { print $1; exit }' "$SUMS"
}

# The manifest already carries a sha256 for every file; SHA256SUMS is the
# same fact in the format a person checks by hand, so a file is held to both
# when both exist. verify_checksum removes the file on a mismatch.
cross_check_sums() { # file name
  local expected
  expected=$(sums_sha_for "$2")
  [[ -n "$expected" ]] || return 0
  verify_checksum "$1" "$expected" "$2 (SHA256SUMS)"
}

# Signature verification. The sha256 checks prove the file is what the
# publisher uploaded; this proves who the publisher was. A release cut by
# .github/workflows/release.yml carries <asset>.sigstore.json beside each
# asset, a cosign bundle whose certificate names that workflow at that tag,
# and only that identity passes. Three outcomes short of a pass are not a
# failure unless --require-signature: no cosign on this box, no bundle on the
# dist (a hand-cut release, or a fleet's fileserver), and --no-verify. A
# bundle that is present but does not verify is always fatal to the caller,
# because that is the one case where something is actively wrong.
verify_signature() { # file name → 0 ok/skipped, 1 mismatch (file removed)
  local file="$1" name="$2" bundle="$TMP/$2.sigstore.json"
  [[ "$NO_CHECKSUM" -eq 1 ]] && { SIG_STATUS="skipped (--no-verify)"; return 0; }
  if ! command -v cosign &>/dev/null; then
    SIG_STATUS="skipped (no cosign on this box)"
    if [[ "$REQUIRE_SIGNATURE" -eq 1 ]]; then
      die "--require-signature, but cosign is not installed here. https://docs.sigstore.dev/cosign/system_config/installation/" 3
    fi
    info "cosign not found; signature verification skipped, sha256 checks still apply"
    return 0
  fi
  if ! fetch "$DIST_URL/$name.sigstore.json" "$bundle"; then
    rm -f "$bundle"
    SIG_STATUS="unsigned dist (no bundle for $name)"
    if [[ "$REQUIRE_SIGNATURE" -eq 1 ]]; then
      die "--require-signature, but $DIST_URL carries no signature bundle for $name. A release cut by hand has none; only the release workflow signs." 3
    fi
    case "$DIST_URL" in
      "$RELEASE_BASE"/*) warn "no signature bundle for $name on this release; it was cut by hand. sha256 alone decides" ;;
      *) info "no signature bundle for $name at $DIST_URL; sha256 alone decides" ;;
    esac
    return 0
  fi
  local out
  if ! out=$(cosign verify-blob --bundle "$bundle" \
    --certificate-identity-regexp "$SIGN_IDENTITY_RE" \
    --certificate-oidc-issuer "$SIGN_ISSUER" "$file" 2>&1); then
    err "signature verification FAILED for $name"
    err "  the bundle does not match the file, or was not signed by the release workflow"
    printf '%s\n' "$out" | tail -3 | sed 's/^/    /' >&2
    rm -f "$file"
    SIG_STATUS="FAILED ($name)"
    return 1
  fi
  SIG_STATUS="verified (release workflow)"
  ok "signature verified: $name (release workflow at ${MANIFEST_TAG:-a version tag})"
}

acquire_manifest() {
  phase "Artifacts"
  if [[ "$REPO_MODE" -eq 1 && -z "$DIST_URL" ]]; then
    ok "using this checkout: $HERE"
    MANIFEST_COMMIT=$(git -C "$HERE" rev-parse HEAD 2>/dev/null || echo "")
    return 0
  fi

  [[ -n "$DIST_URL" ]] || die "not inside a beads_watch checkout and no dist URL configured.
    Pass --dist-url URL (or export BW_DIST_URL), pass --version vX.Y.Z for a
    GitHub Release, or run this script from a checkout." 3

  case "$DIST_URL" in
    http://* | https://* | file://*) ;;
    /* | ./*) DIST_URL="file://$(cd "$DIST_URL" && pwd)" ;;
    *) die "unrecognized --dist-url: $DIST_URL (want http(s)://, file://, or a directory)" 3 ;;
  esac

  command -v python3 &>/dev/null || die 'python3 is required to read manifest.json' 3

  MANIFEST="$TMP/manifest.json"
  if ! fetch "$DIST_URL/manifest.json" "$MANIFEST"; then
    case "$DIST_URL" in
      "$RELEASE_BASE"/*) die "cannot fetch $DIST_URL/manifest.json (no route to github.com, or ${VERSION:-the latest release} does not exist or is not public yet)" 3 ;;
      *) die "cannot reach $DIST_URL/manifest.json (is the fileserver up, and this box on its network?)" 3 ;;
    esac
  fi

  # Optional: a fileserver publish has no SHA256SUMS, a release always does.
  SUMS="$TMP/SHA256SUMS"
  if fetch "$DIST_URL/SHA256SUMS" "$SUMS"; then
    cross_check_sums "$MANIFEST" manifest.json || die 'manifest.json failed verification' 3
  else
    rm -f "$SUMS"
    SUMS=""
    info "no SHA256SUMS at $DIST_URL; verifying from manifest.json alone"
  fi

  MANIFEST_COMMIT=$(manifest_field '["commit"]')
  MANIFEST_TAG=$(manifest_field '["tag"]')
  local gen dirty
  gen=$(manifest_field '["generated_at"]')
  dirty=$(manifest_field '["dirty"]')
  [[ -n "$MANIFEST_COMMIT" ]] || die 'manifest.json has no commit' 3
  ok "manifest: ${MANIFEST_COMMIT:0:12}${MANIFEST_TAG:+ ($MANIFEST_TAG)} generated $gen"
  [[ "$dirty" == "True" ]] && warn "published from a DIRTY tree; it is not exactly this commit"
  # The manifest is what names every other file's sha256, so its signature is
  # the root of the chain: a signed manifest makes the tarball's sha256 check
  # a signed statement too. The tarball is verified on its own as well.
  verify_signature "$MANIFEST" manifest.json ||
    die 'manifest.json failed signature verification; nothing was installed' 3
  return 0
}

try_prebuilt() { # → 0 when a verified prebuilt landed at $TMP/beads_watch
  [[ "$FROM_SOURCE" -eq 0 ]] || return 1
  [[ -n "$TARGET" ]] || { warn "no prebuilt for ${OS}/${ARCH}; building from source"; return 1; }
  [[ -n "$MANIFEST" ]] || return 1

  local idx file sha
  idx=$(python3 -c '
import json, sys
m = json.load(open(sys.argv[1]))
for i, b in enumerate(m.get("binaries", [])):
    if b.get("target") == sys.argv[2]:
        print(i); break
else:
    print("")
' "$MANIFEST" "$TARGET")
  [[ -n "$idx" ]] || { info "no prebuilt for $TARGET in the manifest; building from source"; return 1; }

  file=$(manifest_field "[\"binaries\"][$idx][\"file\"]")
  sha=$(manifest_field "[\"binaries\"][$idx][\"sha256\"]")

  info "downloading $file"
  fetch "$DIST_URL/$file" "$TMP/bin.tar.gz" || { warn "download failed; building from source"; return 1; }
  # A mismatch between a dist and its own checksums is a broken or tampered
  # download, and the header promises exit 3 for it. Quietly building from
  # source instead would hide the one signal that matters here.
  verify_checksum "$TMP/bin.tar.gz" "$sha" "$file" ||
    die "$file failed verification; nothing was installed. Re-run to retry the download, or --from-source to build instead" 3
  cross_check_sums "$TMP/bin.tar.gz" "$(basename "$file")" ||
    die "$file disagrees with SHA256SUMS; nothing was installed. Re-run to retry the download, or --from-source to build instead" 3
  verify_signature "$TMP/bin.tar.gz" "$(basename "$file")" ||
    die "$file failed signature verification; nothing was installed. If this release was meant to be signed, do not install it; --from-source builds from the tag instead" 3

  tar -xzf "$TMP/bin.tar.gz" -C "$TMP" || { warn "extract failed; building from source"; return 1; }
  [[ -f "$TMP/$BIN_NAME" ]] || { warn "tarball had no $BIN_NAME; building from source"; return 1; }
  chmod +x "$TMP/$BIN_NAME"

  # A prebuilt that cannot run here (wrong libc, wrong arch) must fall back
  # rather than get installed and fail later as a mystery.
  if ! "$TMP/$BIN_NAME" --version >/dev/null 2>&1; then
    warn "prebuilt does not run on this box; building from source"
    rm -f "$TMP/$BIN_NAME"
    return 1
  fi
  return 0
}

build_from_source() { # leaves the binary at $TMP/beads_watch
  command -v go &>/dev/null ||
    die "no usable prebuilt and go is not installed. Install Go 1.24+ and re-run." 3

  local src
  if [[ "$REPO_MODE" -eq 1 && -z "$DIST_URL" ]]; then
    src="$HERE"
  else
    command -v git &>/dev/null || die 'git is required to build from source' 3
    rm -rf "$SRC_DIR"
    mkdir -p "$(dirname "$SRC_DIR")"

    local bundle_file
    bundle_file=$(manifest_field '["bundle"]["file"]')
    if [[ -n "$bundle_file" ]]; then
      # A fileserver publish ships the source as a git bundle of HEAD.
      local bundle sha
      bundle="$TMP/beads_watch.bundle"
      sha=$(manifest_field '["bundle"]["sha256"]')
      info "downloading source bundle"
      fetch "$DIST_URL/$bundle_file" "$bundle" ||
        die 'could not download the source bundle' 3
      verify_checksum "$bundle" "$sha" "beads_watch.bundle" || die 'bundle failed verification' 3
      git clone -q "$bundle" "$SRC_DIR" 2>/dev/null || die 'could not clone the bundle' 1
    else
      # A GitHub Release carries no bundle: the public repo is the source, at
      # the release's tag. A manifest without a tag means main.
      local ref="${MANIFEST_TAG:-main}"
      info "cloning https://github.com/$RELEASE_REPO at $ref"
      git clone -q --depth 1 --branch "$ref" "https://github.com/$RELEASE_REPO.git" "$SRC_DIR" 2>/dev/null ||
        die "could not clone https://github.com/$RELEASE_REPO at $ref (no route to github.com, or the tag is not public yet)" 3
      local got
      got=$(git -C "$SRC_DIR" rev-parse HEAD 2>/dev/null || echo "")
      [[ -z "$MANIFEST_COMMIT" || "$got" == "$MANIFEST_COMMIT" ]] ||
        warn "$ref is at ${got:0:12}, but the release was built from ${MANIFEST_COMMIT:0:12}; building what $ref has"
    fi
    src="$SRC_DIR"
    ok "source at $SRC_DIR"
  fi

  local commit built_at ldflags
  commit=$(git -C "$src" rev-parse HEAD 2>/dev/null || echo "")
  built_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  ldflags="-s -w"
  [[ -n "$commit" ]] && ldflags+=" -X beads_watch/internal/server.Commit=$commit"
  ldflags+=" -X beads_watch/internal/server.BuiltAt=$built_at"

  info "building with go (this takes a moment)"
  (cd "$src" && CGO_ENABLED=0 go build -trimpath -ldflags "$ldflags" -o "$TMP/$BIN_NAME" .) ||
    die 'go build failed' 1
}

install_binary() {
  phase "Binary"

  local want="$MANIFEST_COMMIT" cur
  cur=$(installed_commit)
  if [[ "$FORCE" -eq 0 && -n "$want" && -n "$cur" && "$cur" == "$want" ]]; then
    ok "already at ${cur:0:12}; skipping the binary (use --force to reinstall)"
    return 0
  fi

  if ! try_prebuilt; then
    build_from_source
  fi

  install -m 0755 "$TMP/$BIN_NAME" "$PREFIX/$BIN_NAME" || die "could not install to $PREFIX" 1
  BINARY_CHANGED=1
  ok "installed $("$PREFIX/$BIN_NAME" --version | head -1) → $PREFIX/$BIN_NAME"

  case ":$PATH:" in
    *:"$PREFIX":*) ;;
    *) warn "$PREFIX is not on PATH; add it to your shell rc" ;;
  esac
}

# ── Config ──────────────────────────────────────────────────────────────────

discover_repos() { # prints name<TAB>path for every .beads workspace under $HOME
  local roots=("$HOME/dev" "$HOME")
  local seen=" "
  local d repo name
  for root in "${roots[@]}"; do
    [[ -d "$root" ]] || continue
    while IFS= read -r d; do
      repo=$(dirname "$d")
      name=$(basename "$repo")
      case "$seen" in *" $repo "*) continue ;; esac
      seen+="$repo "
      printf '%s\t%s\n' "$name" "$repo"
    done < <(find "$root" -maxdepth 2 -type d -name .beads 2>/dev/null | sort)
  done
}

# The tailnet address the daemon listens on, or nothing when this box has no
# tailscale IP.
listen_addr() {
  local ip
  ip=$(tailnet_ip)
  [[ -n "$ip" ]] && printf '%s:%s' "$ip" "$BRIDGE_PORT"
  return 0
}

# The tailnet IPv4 of a box named the way people name it: a MagicDNS name, or
# an ssh alias for one (an operator who reaches the proxy box as `ssh <alias>`
# passes that alias, and its Host entry knows the real name). Empty when
# neither resolves.
proxy_host_ip() {
  local name="$1" ip="" via
  ip=$(tailscale ip -4 "$name" 2>/dev/null | head -1) || true
  if [[ -z "$ip" ]] && command -v ssh &>/dev/null; then
    via=$(ssh -G "$name" 2>/dev/null | awk '/^hostname /{print $2; exit}')
    if [[ -n "$via" && "$via" != "$name" ]]; then
      ip=$(tailscale ip -4 "$via" 2>/dev/null | head -1) || true
    fi
  fi
  printf '%s' "$ip"
}

# The proxies whose forwarded identity the daemon believes, as a JSON array in
# TRUSTED_JSON. --trusted-proxy wins outright; otherwise the reverse-proxy box
# named by --proxy-host is resolved, and TRUSTED_RESOLVED records which name
# became which address. The list is [] in two cases the messages keep apart:
# no proxy was named at all (the default; nothing to look up, and an honest
# state, since the daemon then identifies every caller from its own address),
# or one was named and did not resolve (a warning, by name). Either way
# traffic through an unlisted proxy is still served, attributed to the proxy
# machine itself, the peer the transport sees. Resolved once; sets globals
# rather than printing so it can run outside a subshell.
TRUSTED_JSON=""
TRUSTED_RESOLVED=""
resolve_trusted_proxies() {
  [[ -n "$TRUSTED_JSON" ]] && return 0
  local list=("${TRUSTED_PROXIES[@]}")
  if [[ ${#list[@]} -eq 0 && -n "$PROXY_HOST" ]] && command -v tailscale &>/dev/null; then
    local ip
    ip=$(proxy_host_ip "$PROXY_HOST")
    if [[ -n "$ip" ]]; then
      list=("$ip")
      TRUSTED_RESOLVED="$PROXY_HOST=$ip"
    fi
  fi
  if [[ ${#list[@]} -eq 0 ]]; then
    TRUSTED_JSON='[]'
    return 0
  fi
  TRUSTED_JSON=$(printf '%s\n' "${list[@]}" | python3 -c 'import json,sys; print(json.dumps([l.strip() for l in sys.stdin if l.strip()]))')
}

# Merge into an existing config, or write a new one. Every top-level key the
# operator set survives. "listen" and "trusted_proxies" are added when absent,
# because this release's daemon binds the tailnet address itself and the
# proxyd bridge is retired on the strength of that. With repos_mode=replace
# the repos list is refreshed from stdin: a path already present keeps the
# name the operator gave it, since the name is an alias and discovery only
# knows basenames, and entries no longer found are dropped by name so the loss
# is visible rather than silent. Writes DST; the caller decides whether that
# differs from what is installed.
CONFIG_MERGE_PY='
import json, sys
socket, listen, trusted, dst, existing, repos_mode = sys.argv[1:7]
cfg = {}
if existing:
    try:
        with open(existing) as f:
            cfg = json.load(f)
    except ValueError as e:
        sys.exit("%s is not valid JSON (%s); fix or remove it, then re-run" % (existing, e))
    if not isinstance(cfg, dict):
        sys.exit("%s is not a JSON object; fix or remove it, then re-run" % existing)
report = []
if "socket" not in cfg and "listen" not in cfg:
    cfg["socket"] = socket
    report.append(("key", "socket", socket))
if listen and not cfg.get("listen"):
    cfg["listen"] = listen
    report.append(("key", "listen", listen))
if "trusted_proxies" not in cfg:
    cfg["trusted_proxies"] = json.loads(trusted)
    report.append(("key", "trusted_proxies", trusted))
if repos_mode == "replace":
    old = {}
    for r in cfg.get("repos") or []:
        if isinstance(r, dict) and r.get("path"):
            old[r["path"]] = r.get("name") or ""
    repos = []
    for line in sys.stdin:
        line = line.rstrip("\n")
        if not line:
            continue
        name, _, path = line.partition("\t")
        repos.append({"name": old.get(path) or name, "path": path})
    new = {r["path"] for r in repos}
    cfg["repos"] = repos
    for path, name in old.items():
        if path not in new:
            report.append(("repo-", name, path))
    for r in repos:
        if r["path"] not in old:
            report.append(("repo+", r["name"], r["path"]))
with open(dst, "w") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")
for row in report:
    print("\t".join(row))
'

# write_config MODE (new|migrate|replace): runs the merge, leaves the file
# untouched when nothing would change, backs it up first when something
# would, and narrates every change. Repos for replace come from REPO_PAIRS.
# Returns 1 when it left the file alone.
REPO_PAIRS=()
write_config() {
  local mode="$1" existing="" tmp report
  [[ -f "$CONFIG_FILE" ]] && existing="$CONFIG_FILE"
  mkdir -p "$CONFIG_DIR"
  tmp=$(mktemp "$CONFIG_DIR/.config.json.XXXXXX") || die "cannot write in $CONFIG_DIR" 1

  local socket="${XDG_RUNTIME_DIR:-/tmp}/beads_watch.sock"
  local listen repos_mode=keep
  listen=$(listen_addr)
  resolve_trusted_proxies
  [[ "$mode" == replace || "$mode" == new ]] && repos_mode=replace
  report=$(printf '%s\n' "${REPO_PAIRS[@]}" | python3 -c "$CONFIG_MERGE_PY" \
    "$socket" "$listen" "$TRUSTED_JSON" "$tmp" "$existing" "$repos_mode") \
    || { rm -f "$tmp"; die 'could not write the config' 1; }

  # Nothing to change means nothing to write: the operator's own formatting
  # is left exactly as it was, not re-serialized.
  if [[ -n "$existing" && -z "$report" ]]; then
    rm -f "$tmp"
    return 1
  fi

  local backup=""
  if [[ -n "$existing" ]]; then
    backup="$CONFIG_FILE.bak.$(date +%Y%m%d%H%M%S)"
    cp "$existing" "$backup" || { rm -f "$tmp"; die "could not back up $CONFIG_FILE" 1; }
  fi
  chmod 0644 "$tmp"
  mv "$tmp" "$CONFIG_FILE" || die "could not write $CONFIG_FILE" 1

  case "$mode" in
    new) ok "wrote $CONFIG_FILE" ;;
    migrate) ok "updated $CONFIG_FILE for this release (backup: $backup)" ;;
    replace) ok "rewrote repos in $CONFIG_FILE (backup: $backup)" ;;
  esac
  local kind name value
  while IFS=$'\t' read -r kind name value; do
    [[ -n "$kind" ]] || continue
    case "$kind" in
      key)
        if [[ "$name" == trusted_proxies ]]; then
          if [[ "$value" != '[]' ]]; then
            info "added $name = $value${TRUSTED_RESOLVED:+ ($TRUSTED_RESOLVED)}"
          elif [[ -n "$PROXY_HOST" ]]; then
            warn "added trusted_proxies = []: could not resolve $PROXY_HOST, so no forwarded identity is trusted; traffic through that reverse proxy is attributed to the proxy machine. Pass --trusted-proxy IP to name it by address"
          else
            info "added trusted_proxies = []: no reverse proxy named, so forwarded identity is not trusted and every caller is identified from its own address. If a reverse proxy fronts this box, pass --proxy-host NAME or --trusted-proxy IP"
          fi
        else
          info "added $name = $value"
        fi ;;
      repo-) warn "dropped $name → $value (not found by discovery; still in the backup)" ;;
      repo+) info "added $name → $value" ;;
    esac
  done <<<"$report"
  return 0
}

# What this run will do to the config, in one line, shared by the plan and the
# phase so --dry-run cannot describe one thing and the run do another.
config_plan() {
  if [[ "$NO_CONFIG" -eq 1 ]]; then
    echo "skipped (--no-config)"
  elif [[ ! -f "$CONFIG_FILE" ]]; then
    echo "$CONFIG_FILE (new; repos from $([[ ${#CLI_REPOS[@]} -gt 0 ]] && echo '--repo' || echo 'discovery'))"
  elif [[ "$REWRITE_CONFIG" -eq 1 ]]; then
    echo "$CONFIG_FILE (rewrite: repos from $([[ ${#CLI_REPOS[@]} -gt 0 ]] && echo '--repo' || echo 'discovery'), other keys kept, backup first)"
  elif config_needs_migration; then
    echo "$CONFIG_FILE (exists; adds listen/trusted_proxies for this release, backup first)"
  else
    echo "$CONFIG_FILE (exists; left alone)"
  fi
}

# Whether the existing config lacks a key this release adds. Read-only.
config_needs_migration() {
  [[ -f "$CONFIG_FILE" ]] || return 1
  local listen
  listen=$(listen_addr)
  python3 -c '
import json, sys
try:
    cfg = json.load(open(sys.argv[1]))
except Exception:
    sys.exit(1)
listen = sys.argv[2]
need = (listen and not cfg.get("listen")) or "trusted_proxies" not in cfg
sys.exit(0 if need else 1)
' "$CONFIG_FILE" "$listen" 2>/dev/null
}

# The trusted_proxies the daemon will run with, as JSON: the config on disk
# when there is one (a re-run never rewrites it, so this run's flags may not
# be what applies), else what this run resolved. Read-only.
config_trusted_proxies() {
  if [[ -f "$CONFIG_FILE" ]]; then
    python3 -c '
import json, sys
try:
    print(json.dumps(json.load(open(sys.argv[1])).get("trusted_proxies") or []))
except Exception:
    sys.exit(1)
' "$CONFIG_FILE" 2>/dev/null && return 0
  fi
  resolve_trusted_proxies
  echo "$TRUSTED_JSON"
}

# Whether the config the daemon will run with has a listen address, i.e. the
# proxyd bridge can be retired. Asks the installed binary so the answer is the
# daemon's, defaults and all.
config_has_listen() {
  [[ -x "$PREFIX/$BIN_NAME" ]] || return 1
  "$PREFIX/$BIN_NAME" --print-config 2>/dev/null | python3 -c '
import json, sys
try:
    sys.exit(0 if json.load(sys.stdin).get("listen") else 1)
except Exception:
    sys.exit(1)
'
}

setup_config() {
  [[ "$NO_CONFIG" -eq 1 ]] && { info "config skipped (--no-config)"; return 0; }
  phase "Config"

  # An existing config is the operator's, and --force is about the binary.
  # Discovery only knows how to produce a repos list, so regenerating from it
  # would drop notify, allow, br_path and the rest — and the daemon would come
  # up green with the event feed silently gone. Only the explicit flag
  # rewrites repos, and even then it merges rather than replaces. What does
  # happen without it is a migration: keys this release needs are added when
  # absent, each one named, with a backup — nothing else is touched.
  if [[ -f "$CONFIG_FILE" && "$REWRITE_CONFIG" -eq 0 ]]; then
    [[ ${#CLI_REPOS[@]} -gt 0 ]] && warn "--repo ignored: the config already exists (add --rewrite-config to apply it)"
    if write_config migrate; then
      :
    else
      ok "config exists: $CONFIG_FILE (left alone; --rewrite-config to refresh repos)"
    fi
    validate_config
    return 0
  fi

  REPO_PAIRS=()
  if [[ ${#CLI_REPOS[@]} -gt 0 ]]; then
    local spec
    for spec in "${CLI_REPOS[@]}"; do
      [[ "$spec" == *=* ]] || die "--repo wants NAME=PATH, got '$spec'" 2
      REPO_PAIRS+=("${spec%%=*}	${spec#*=}")
    done
  else
    info "scanning $HOME/dev and $HOME for .beads workspaces (maxdepth 2)"
    local line
    while IFS= read -r line; do
      [[ -n "$line" ]] && REPO_PAIRS+=("$line")
    done < <(discover_repos)
  fi

  if [[ ${#REPO_PAIRS[@]} -eq 0 ]]; then
    warn "no .beads workspaces found; writing no config"
    warn "create $CONFIG_FILE by hand, or re-run with --repo name=/path"
    return 0
  fi

  info "found ${#REPO_PAIRS[@]} repo(s):"
  local p
  for p in "${REPO_PAIRS[@]}"; do
    [[ "$QUIET" -eq 1 ]] || echo "    ${p%%	*}  →  ${p#*	}"
  done
  if [[ -f "$CONFIG_FILE" ]]; then
    ask_yn "Rewrite the repos in $CONFIG_FILE to these (other keys kept, backup first)?" y \
      || { info "config left alone"; validate_config; return 0; }
    write_config replace || ok "config already matches: $CONFIG_FILE (left alone)"
  else
    ask_yn "Write $CONFIG_FILE serving these?" y || { info "config skipped"; return 0; }
    write_config new
  fi
  validate_config
}

# Validate before anything tries to start against it. A config can pass and
# still carry a repo the daemon will serve as unavailable; it says so on
# stderr, and that is worth seeing here rather than in the journal later.
validate_config() {
  [[ -f "$CONFIG_FILE" ]] || return 0
  local verdict line
  if verdict=$("$PREFIX/$BIN_NAME" --print-config 2>&1 >/dev/null); then
    ok "config validates"
    [[ -z "$verdict" ]] || while IFS= read -r line; do warn "$line"; done <<<"$verdict"
  else
    warn "config did not validate:"
    printf '%s\n' "$verdict" | head -5 >&2
  fi
}

# ── Units ───────────────────────────────────────────────────────────────────

tailnet_ip() {
  command -v tailscale &>/dev/null || return 0
  tailscale ip -4 2>/dev/null | head -1
}

# The name this node answers to. `hostname -s` is NOT it: on a box whose
# resolver maps the name to loopback, -s returns "localhost". The daemon uses
# os.Hostname(), which is plain `hostname`, so match that and lowercase it the
# way the events config does.
node_name() {
  local n
  n=$(hostname 2>/dev/null || cat /etc/hostname 2>/dev/null || echo unknown)
  n=${n%%.*}
  printf '%s' "$n" | tr '[:upper:]' '[:lower:]'
}

# The unit files, rendered to stdout. Writing goes through here and so does
# the dry-run plan, so what the plan diffs is byte-for-byte what a run writes.
render_unit() { # render_unit NAME TAILNET_IP
  local unit="$1" ip="${2:-}"
  case "$unit" in
    "$SERVICE_UNIT")
      cat <<UNIT
[Unit]
Description=beads_watch — serve br over the tailnet
Documentation=https://github.com/a44-io/beads_watch
After=network-online.target

# A config the daemon refuses (a typo'd key, every repo path missing) exits 1
# before the socket binds. Restart= below retries that; without a cap it would
# retry every 2s forever in "activating (auto-restart)", which never reaches
# "failed" and so never shows in systemctl --failed. Five tries in a minute
# still rides out a transient, then the unit fails loudly instead.
StartLimitIntervalSec=60
StartLimitBurst=5

[Service]
Type=exec
ExecStart=$PREFIX/$BIN_NAME
Restart=on-failure
RestartSec=2s

# The daemon scrubs these from br's environment anyway, but a service file that
# cannot introduce them is one less way to end up serving the wrong workspace.
UnsetEnvironment=BEADS_DIR BD_DB BD_DATABASE BD_ACTOR BR_OUTPUT_FORMAT TOON_DEFAULT_FORMAT
Environment=RUST_LOG=error

# br needs to read and write .beads/ across the home directory, so home is
# deliberately left open (ProtectHome=no is the default, spelled out so the
# choice is visible); the rest is tightened.
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=no
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictNamespaces=true
LockPersonality=true

[Install]
WantedBy=default.target
UNIT
      ;;
    *) die "render_unit: unknown unit $unit" 1 ;;
  esac
}

# Whether the retired proxyd bridge is still installed here.
proxy_bridge_present() {
  [[ -e "$UNIT_DIR/$PROXY_SOCKET" || -e "$UNIT_DIR/$PROXY_SERVICE" ]]
}

# Fingerprint the installed units, so a re-run that changes nothing does not
# restart a healthy daemon for no reason.
units_fingerprint() {
  local u out=""
  for u in "$SERVICE_UNIT" "$PROXY_SOCKET" "$PROXY_SERVICE"; do
    out+="$(cat "$UNIT_DIR/$u" 2>/dev/null || true)"
  done
  printf '%s' "$out" | cksum
}

# What this run would do to the units, in one line, for the dry-run plan: each
# unit that would be written differently from what is installed is named, and
# so is the restart that follows, since that is the question an operator asks
# before re-running on a box that is serving.
units_plan() {
  if [[ "$NO_UNITS" -eq 1 ]]; then echo "skipped (--no-units)"; return 0; fi
  if [[ "$OS" != linux ]]; then echo "skipped (systemd units are linux-only)"; return 0; fi
  if ! systemd_user_available; then echo "skipped (no systemd user session)"; return 0; fi

  local ip u pending=()
  ip=$(tailnet_ip)
  if [[ ! -e "$UNIT_DIR/$SERVICE_UNIT" ]]; then
    pending+=("$SERVICE_UNIT (new)")
  elif ! diff -q <(render_unit "$SERVICE_UNIT" "$ip") "$UNIT_DIR/$SERVICE_UNIT" >/dev/null 2>&1; then
    pending+=("$SERVICE_UNIT (changed)")
  fi
  if proxy_bridge_present; then
    # The bridge goes once the daemon has a listen address of its own; the
    # config phase adds one whenever this box has a tailnet IP.
    if [[ "$NO_START" -eq 1 ]]; then
      pending+=("proxyd bridge kept: --no-start")
    elif config_has_listen || { [[ "$NO_CONFIG" -eq 0 && -n "$ip" ]]; }; then
      pending+=("$PROXY_SOCKET + $PROXY_SERVICE (retire)")
    else
      pending+=("proxyd bridge kept: config has no listen")
    fi
  fi
  if [[ ${#pending[@]} -eq 0 ]]; then
    echo "$UNIT_DIR (all current)"
    return 0
  fi
  local restart=""
  if [[ "$NO_START" -eq 0 ]] && systemctl --user is-active --quiet "$SERVICE_UNIT" 2>/dev/null; then
    restart="; daemon restarts"
  fi
  echo "$UNIT_DIR (write: $(IFS=,; echo "${pending[*]}" | sed 's/,/, /g')$restart)"
}

setup_units() {
  [[ "$NO_UNITS" -eq 1 ]] && { info "units skipped (--no-units)"; return 0; }
  phase "Units"

  if [[ "$OS" != linux ]]; then
    warn "systemd units are linux-only; run the daemon yourself on $OS"
    return 0
  fi
  if ! systemd_user_available; then
    warn "no systemd user session; skipping units"
    return 0
  fi

  local ip
  ip=$(tailnet_ip)
  if [[ -z "$ip" ]]; then
    warn "no tailscale IPv4 on this box; the daemon will serve the unix socket only"
  else
    ok "tailnet address: $ip"
  fi

  mkdir -p "$UNIT_DIR"

  local before after
  before=$(units_fingerprint)

  # A previous manual install symlinks these into a checkout. Redirecting onto
  # a symlink writes THROUGH it and edits the repo's tracked unit files, so
  # clear the link first and write a real file in its place.
  if [[ -L "$UNIT_DIR/$SERVICE_UNIT" ]]; then
    info "replacing symlinked $SERVICE_UNIT (was → $(readlink "$UNIT_DIR/$SERVICE_UNIT"))"
    rm -f "$UNIT_DIR/$SERVICE_UNIT"
  fi

  # Drop-ins outrank the unit file. Left in place one would silently override
  # what is written below.
  local dropin="$UNIT_DIR/$SERVICE_UNIT.d"
  if [[ -d "$dropin" ]]; then
    mv "$dropin" "$dropin.bak.$(date +%Y%m%d%H%M%S)"
    warn "moved aside $SERVICE_UNIT.d (its overrides would outrank the generated unit)"
  fi

  render_unit "$SERVICE_UNIT" "$ip" >"$UNIT_DIR/$SERVICE_UNIT"
  ok "wrote $UNIT_DIR/$SERVICE_UNIT"

  after=$(units_fingerprint)
  [[ "$before" != "$after" ]] && UNITS_CHANGED=1

  systemctl --user daemon-reload

  if [[ "$NO_START" -eq 1 ]]; then
    info "units installed but not started (--no-start)"
    proxy_bridge_present && warn "the proxyd bridge is still installed; it is retired when the daemon is next restarted by this script"
    return 0
  fi
  ask_yn "Enable and start beads_watch now?" y || { info "units left disabled"; return 0; }

  systemctl --user enable "$SERVICE_UNIT" &>/dev/null ||
    warn "could not enable $SERVICE_UNIT"

  # `enable --now` starts a stopped unit but leaves a running one alone, so on
  # an upgrade it would report success while the OLD binary kept serving. Any
  # real change has to restart.
  local restarting=0
  if [[ "$BINARY_CHANGED" -eq 1 || "$UNITS_CHANGED" -eq 1 ]] &&
    systemctl --user is-active --quiet "$SERVICE_UNIT"; then
    restarting=1
    info "restarting to pick up the new $([[ "$BINARY_CHANGED" -eq 1 ]] && echo binary || echo units)"
  fi

  # The proxyd bridge used to own the tailnet port. Now that the daemon binds
  # it itself the pair has to go first, or the restart fails on the bind — and
  # it can only go when the daemon actually has a listen address, or caddy
  # would be left with nothing to reach. Retiring it counts as a unit change:
  # the daemon must restart onto the port.
  if proxy_bridge_present; then
    if config_has_listen; then
      systemctl --user disable --now "$PROXY_SOCKET" "$PROXY_SERVICE" &>/dev/null || true
      rm -f "$UNIT_DIR/$PROXY_SOCKET" "$UNIT_DIR/$PROXY_SERVICE"
      systemctl --user daemon-reload
      ok "retired the proxyd bridge; the daemon listens on the tailnet address itself"
      systemctl --user is-active --quiet "$SERVICE_UNIT" && restarting=1
    else
      warn "config has no listen address; keeping the proxyd bridge in place"
      warn "  add \"listen\": \"$ip:$BRIDGE_PORT\" to $CONFIG_FILE, or re-run without --no-config"
    fi
  fi

  # A unit that hit its start limit stays "failed" and refuses a plain start
  # until the interval passes. What is installed now is not what failed, so
  # clear the counter first; on a healthy unit this is a no-op.
  systemctl --user reset-failed "$SERVICE_UNIT" &>/dev/null || true

  if [[ "$restarting" -eq 1 ]]; then
    systemctl --user restart "$SERVICE_UNIT" ||
      warn "could not restart $SERVICE_UNIT; see: journalctl --user -u beads_watch -n 30"
  else
    systemctl --user start "$SERVICE_UNIT" ||
      warn "could not start $SERVICE_UNIT; see: journalctl --user -u beads_watch -n 30"
  fi

  if command -v loginctl &>/dev/null; then
    if [[ "$(loginctl show-user "$USER" -p Linger --value 2>/dev/null)" != yes ]]; then
      loginctl enable-linger "$USER" 2>/dev/null &&
        ok "linger enabled, so the daemon survives logout" ||
        warn "could not enable linger; the daemon will stop at logout"
    fi
  fi
}

# ── Verify ──────────────────────────────────────────────────────────────────

verify_install() {
  phase "Verify"
  local sock="${XDG_RUNTIME_DIR:-/tmp}/beads_watch.sock" body=""
  local i
  for i in 1 2 3 4 5; do
    body=$(curl -s --max-time 3 --unix-socket "$sock" http://local/v1/health 2>/dev/null) && break
    sleep 1
  done
  if [[ -z "$body" ]]; then
    warn "no answer on $sock"
    warn "  systemctl --user status beads_watch"
    warn "  journalctl --user -u beads_watch -n 30"
    return 0
  fi
  ok "health: $(printf '%s' "$body" | tr -d '\n ')"

  # The check that matters on an upgrade: a daemon keeps its old executable
  # mapped, so "installed" and "running" are different facts. Compare them.
  local want running
  want=$(installed_commit)
  running=$(printf '%s' "$body" | sed -n 's/.*"commit"[: ]*"\([0-9a-f]*\)".*/\1/p')
  if [[ -n "$want" && -n "$running" && "$want" != "$running" ]]; then
    warn "the RUNNING daemon is ${running:0:12}, but ${want:0:12} is installed"
    warn "  systemctl --user restart beads_watch"
  elif [[ -n "$want" && -z "$running" ]]; then
    warn "the running daemon reports no commit, so it predates this install"
    warn "  systemctl --user restart beads_watch"
  elif [[ -n "$running" ]]; then
    ok "running commit matches the installed binary"
  fi

  local ip
  ip=$(tailnet_ip)
  [[ -n "$ip" ]] || return 0
  local who=""
  for i in 1 2 3; do
    if who=$(curl -s --max-time 3 "http://$ip:$BRIDGE_PORT/v1/whoami" 2>/dev/null) && [[ -n "$who" ]]; then
      ok "tailnet: http://$ip:$BRIDGE_PORT reachable"
      # This call is a direct TCP peer — this very box — so the daemon should
      # have identified it by its address, not by any header. That line is
      # the identity model working, seen from the outside.
      local src
      src=$(printf '%s' "$who" | sed -n 's/.*"source"[: ]*"\([^"]*\)".*/\1/p')
      [[ -n "$src" ]] && info "identity over tcp: source=$src (a direct peer is named by its own address)"
      return 0
    fi
    sleep 1
  done
  if proxy_bridge_present; then
    warn "$ip:$BRIDGE_PORT did not answer; the proxyd bridge is still installed"
    warn "  systemctl --user status $PROXY_SOCKET"
  else
    warn "$ip:$BRIDGE_PORT did not answer"
    warn "  journalctl --user -u beads_watch -n 30   # look for a bind error; is \"listen\" set in $CONFIG_FILE?"
  fi
}

# ── Uninstall ───────────────────────────────────────────────────────────────

uninstall_all() {
  phase "Uninstall"
  local unit
  if systemd_user_available; then
    for unit in "$PROXY_SOCKET" "$PROXY_SERVICE" "$SERVICE_UNIT"; do
      systemctl --user disable --now "$unit" &>/dev/null || true
      [[ -f "$UNIT_DIR/$unit" ]] && rm -f "$UNIT_DIR/$unit" && ok "removed $unit"
    done
    systemctl --user daemon-reload || true
  fi
  if [[ -f "$PREFIX/$BIN_NAME" ]]; then
    rm -f "$PREFIX/$BIN_NAME"
    ok "removed $PREFIX/$BIN_NAME"
  fi
  [[ -d "$SRC_DIR" ]] && rm -rf "$SRC_DIR" && ok "removed $SRC_DIR"
  info "kept $CONFIG_FILE and every .beads workspace"
}

# ── Plan and summary ────────────────────────────────────────────────────────

print_plan() {
  draw_box "0;36" \
    "\033[1mbeads_watch setup — dry run\033[0m" \
    "" \
    "binary   → $PREFIX/$BIN_NAME$([[ "$FORCE" -eq 1 ]] && echo ' (--force: reinstall even if current)')" \
    "config   → $(config_plan)" \
    "units    → $(units_plan)" \
    "source   → $([[ "$REPO_MODE" -eq 1 && -z "$DIST_URL" ]] && echo "$HERE" || echo "${DIST_URL:-<none>}")" \
    "verify   → $(verify_plan)" \
    "listen   → $(listen_addr_plan)" \
    "trusts   → $(trusted_plan)"
}

verify_plan() {
  if [[ "$NO_CHECKSUM" -eq 1 ]]; then
    echo "nothing (--no-verify)"
  elif [[ "$REPO_MODE" -eq 1 && -z "$DIST_URL" ]]; then
    echo "nothing to verify; building this checkout"
  elif ! command -v cosign &>/dev/null; then
    echo "sha256 (manifest.json + SHA256SUMS); no cosign here, so no signature check$([[ "$REQUIRE_SIGNATURE" -eq 1 ]] && echo ' — --require-signature will refuse')"
  else
    echo "sha256, then Sigstore bundles against the release workflow$([[ "$REQUIRE_SIGNATURE" -eq 1 ]] && echo ' (required)' || echo ' (when present)')"
  fi
}

listen_addr_plan() {
  local l
  l=$(listen_addr)
  [[ -n "$l" ]] && echo "$l" || echo "none (no tailscale IPv4; unix socket only)"
}

trusted_plan() {
  resolve_trusted_proxies
  if [[ "$TRUSTED_JSON" != '[]' ]]; then
    echo "$TRUSTED_JSON${TRUSTED_RESOLVED:+ ($TRUSTED_RESOLVED)}"
  elif [[ -n "$PROXY_HOST" ]]; then
    echo "no proxy (could not resolve $PROXY_HOST; pass --trusted-proxy IP)"
  else
    echo "none configured (forwarded identity not trusted; --proxy-host NAME or --trusted-proxy IP to change)"
  fi
}

print_summary() {
  [[ "$QUIET" -eq 1 ]] && return 0
  local ip
  ip=$(tailnet_ip)
  local lines=("\033[1mbeads_watch is installed\033[0m" "")
  lines+=("binary   $PREFIX/$BIN_NAME")
  [[ -f "$CONFIG_FILE" ]] && lines+=("config   $CONFIG_FILE")
  [[ -n "$MANIFEST_COMMIT" ]] && lines+=("commit   ${MANIFEST_COMMIT:0:12}${MANIFEST_TAG:+ ($MANIFEST_TAG)}")
  [[ -n "$SIG_STATUS" ]] && lines+=("signed   $SIG_STATUS")
  [[ -n "$ip" ]] && lines+=("tailnet  http://$ip:$BRIDGE_PORT")
  lines+=("")
  # What the daemon trusts decides the next step: a listed proxy wants a site
  # block pointing at this box (the README's Install section has the one to
  # copy, header strips included); none means forwarded identity is off, and
  # the honest fix is in the config, since a re-run will not touch it.
  local trusted
  trusted=$(config_trusted_proxies)
  if [[ "$trusted" != '[]' ]]; then
    lines+=("Give it a name on your reverse proxy: see README → Install")
    lines+=("  site beads-$(node_name).<domain>  →  reverse_proxy ${ip:-<tailnet-ip>}:$BRIDGE_PORT  (trusts $trusted)")
  else
    lines+=("Forwarded identity is off: trusted_proxies is []. List a reverse proxy's IP there to trust it")
  fi
  lines+=("")
  lines+=("Uninstall:  setup.sh --uninstall")
  echo ""
  draw_box "0;32" "${lines[@]}"
}

# ── Main ────────────────────────────────────────────────────────────────────

main() {
  if [[ "$QUIET" -eq 0 ]]; then
    if use_gum; then
      gum style --border normal --border-foreground 39 --padding "0 1" --margin "1 0" \
        "$(gum style --foreground 42 --bold 'beads_watch installer')" \
        "$(gum style --foreground 245 'serve br over the tailnet, without ssh')"
    else
      echo ""
      echo -e "\033[1;32mbeads_watch installer\033[0m"
      echo -e "\033[0;90mserve br over the tailnet, without ssh\033[0m"
    fi
  fi

  detect_platform
  setup_proxy

  if [[ "$DRY_RUN" -eq 1 ]]; then
    print_plan
    exit 0
  fi

  acquire_lock
  HELD_LOCK=1
  TMP=$(mktemp -d)
  trap cleanup EXIT

  if [[ "$DO_UNINSTALL" -eq 1 ]]; then
    uninstall_all
    exit 0
  fi

  preflight
  acquire_manifest
  install_binary
  setup_config
  setup_units
  verify_install
  print_summary
}

main
