#!/usr/bin/env bash
# beads_watch guided installer — provision a tailnet node end to end.
#
# One-liner from the tailnet fileserver (the ?cachebuster matters; dufs and
# caddy will both happily hand you a stale copy otherwise):
#   curl -fsSL "https://bw.dev.a44.io/setup.sh?$(date +%s)" | bash
#
# The served copy has its dist URL stamped in by scripts/publish-dist.sh. A
# hand-downloaded copy needs telling where the artifacts live:
#   curl -fsSL https://bw.dev.a44.io/setup.sh | BW_DIST_URL=https://bw.dev.a44.io bash
#
# Over ssh (prompts work through the tty):
#   ssh -t box 'curl -fsSL "https://bw.dev.a44.io/setup.sh?$(date +%s)" | bash'
#
# Fully unattended (takes every default: install, config from discovered repos,
# units yes, start yes):
#   ssh box 'curl -fsSL "https://bw.dev.a44.io/setup.sh?$(date +%s)" | bash -s -- --yes'
#
# From a checkout (no fileserver needed):
#   git clone https://github.com/a44-io/beads_watch.git && cd beads_watch && ./setup.sh
#
# Options:
#   --prefix DIR      install the binary to DIR (default: ~/.local/bin)
#   --dist-url URL    artifact base URL (http://, https://, file://, or a
#                     plain directory); also BW_DIST_URL
#   --from-source     ignore prebuilt binaries; always build with go
#   --force           reinstall even when the installed commit is current
#   --yes, -y         accept the default answer for every prompt
#   --repo NAME=PATH  serve this repo; repeatable, and suppresses discovery
#   --no-config       do not create or touch ~/.config/beads_watch/config.json
#   --no-units        do not install the systemd user units
#   --no-start        install the units but do not start them
#   --no-verify       skip sha256 verification of downloaded artifacts
#   --port PORT       tailnet bridge port (default: 8438)
#   --dry-run         print the plan; change nothing
#   --uninstall       remove binary and units (keeps config and every .beads)
#   --quiet, -q       errors only
#   --no-gum          plain ANSI output even where gum is installed
#   --help, -h        this text
#
# Exit codes: 0 success · 1 a step failed · 2 misuse (bad flag) · 3 infrastructure
# (missing required tool, unreachable dist server, checksum mismatch).
#
# On signatures: artifacts are private and served only inside the tailnet, with
# sha256 in manifest.json. There is no Sigstore bundle because there is no
# public release pipeline to sign against. The tailnet ACL is the authenticity
# boundary; the manifest is the integrity check.

set -euo pipefail
umask 022
shopt -s lastpipe 2>/dev/null || true

# ── Constants and flag defaults ─────────────────────────────────────────────

# Stamped by scripts/publish-dist.sh --url; empty in the repo copy.
DEFAULT_DIST_URL=""

PREFIX="${BW_PREFIX:-$HOME/.local/bin}"
DIST_URL="${BW_DIST_URL:-$DEFAULT_DIST_URL}"
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/beads_watch"
CONFIG_FILE="$CONFIG_DIR/config.json"
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
SRC_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/beads_watch/src"
LOCK_DIR="${TMPDIR:-/tmp}/beads_watch-setup.lock.d"
BRIDGE_PORT=8438
BIN_NAME=beads_watch
SERVICE_UNIT=beads_watch.service
PROXY_SOCKET=beads_watch-proxy.socket
PROXY_SERVICE=beads_watch-proxy.service

ASSUME_YES=0
QUIET=0
NO_GUM=0
FROM_SOURCE=0
FORCE=0
NO_CHECKSUM=0
DRY_RUN=0
DO_UNINSTALL=0
NO_CONFIG=0
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
    --dist-url) need_value --dist-url "${2:-}"; DIST_URL="${2%/}"; shift 2 ;;
    --repo) need_value --repo "${2:-}"; CLI_REPOS+=("$2"); shift 2 ;;
    --port) need_value --port "${2:-}"; BRIDGE_PORT="$2"; shift 2 ;;
    --from-source) FROM_SOURCE=1; shift ;;
    --force) FORCE=1; shift ;;
    --yes | -y) ASSUME_YES=1; shift ;;
    --no-config) NO_CONFIG=1; shift ;;
    --no-units) NO_UNITS=1; shift ;;
    --no-start) NO_START=1; shift ;;
    --no-verify) NO_CHECKSUM=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --uninstall) DO_UNINSTALL=1; shift ;;
    --quiet | -q) QUIET=1; shift ;;
    --no-gum) NO_GUM=1; shift ;;
    --help | -h) usage; exit 0 ;;
    *) die "unknown argument '$1' (try --help)" 2 ;;
  esac
done

[[ "$BRIDGE_PORT" =~ ^[0-9]+$ ]] || die "--port wants a number, got '$BRIDGE_PORT'" 2

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
    file://*) cp "${1#file://}" "$2" ;;
    *) curl -fsSL "${PROXY_ARGS[@]}" "$1" -o "$2" ;;
  esac
}

acquire_manifest() {
  phase "Artifacts"
  if [[ "$REPO_MODE" -eq 1 && -z "$DIST_URL" ]]; then
    ok "using this checkout: $HERE"
    MANIFEST_COMMIT=$(git -C "$HERE" rev-parse HEAD 2>/dev/null || echo "")
    return 0
  fi

  [[ -n "$DIST_URL" ]] || die "not inside a beads_watch checkout and no dist URL configured.
    Pass --dist-url https://bw.dev.a44.io (or export BW_DIST_URL), or run this
    script from a checkout." 3

  case "$DIST_URL" in
    http://* | https://* | file://*) ;;
    /* | ./*) DIST_URL="file://$(cd "$DIST_URL" && pwd)" ;;
    *) die "unrecognized --dist-url: $DIST_URL (want http(s)://, file://, or a directory)" 3 ;;
  esac

  command -v python3 &>/dev/null || die 'python3 is required to read manifest.json' 3

  MANIFEST="$TMP/manifest.json"
  fetch "$DIST_URL/manifest.json" "$MANIFEST" ||
    die "cannot reach $DIST_URL/manifest.json (is the tailnet up?)" 3

  MANIFEST_COMMIT=$(manifest_field '["commit"]')
  MANIFEST_TAG=$(manifest_field '["tag"]')
  local gen dirty
  gen=$(manifest_field '["generated_at"]')
  dirty=$(manifest_field '["dirty"]')
  [[ -n "$MANIFEST_COMMIT" ]] || die 'manifest.json has no commit' 3
  ok "manifest: ${MANIFEST_COMMIT:0:12}${MANIFEST_TAG:+ ($MANIFEST_TAG)} generated $gen"
  [[ "$dirty" == "True" ]] && warn "published from a DIRTY tree; it is not exactly this commit"
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
  verify_checksum "$TMP/bin.tar.gz" "$sha" "$file" || return 1

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
    local bundle sha
    bundle="$TMP/beads_watch.bundle"
    sha=$(manifest_field '["bundle"]["sha256"]')
    info "downloading source bundle"
    fetch "$DIST_URL/$(manifest_field '["bundle"]["file"]')" "$bundle" ||
      die 'could not download the source bundle' 3
    verify_checksum "$bundle" "$sha" "beads_watch.bundle" || die 'bundle failed verification' 3
    command -v git &>/dev/null || die 'git is required to build from the bundle' 3

    rm -rf "$SRC_DIR"
    mkdir -p "$(dirname "$SRC_DIR")"
    git clone -q "$bundle" "$SRC_DIR" 2>/dev/null || die 'could not clone the bundle' 1
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

setup_config() {
  [[ "$NO_CONFIG" -eq 1 ]] && { info "config skipped (--no-config)"; return 0; }
  phase "Config"

  if [[ -f "$CONFIG_FILE" && "$FORCE" -eq 0 ]]; then
    ok "config exists: $CONFIG_FILE (left alone; --force to rewrite)"
    return 0
  fi

  local pairs=()
  if [[ ${#CLI_REPOS[@]} -gt 0 ]]; then
    local spec
    for spec in "${CLI_REPOS[@]}"; do
      [[ "$spec" == *=* ]] || die "--repo wants NAME=PATH, got '$spec'" 2
      pairs+=("${spec%%=*}	${spec#*=}")
    done
  else
    info "scanning $HOME/dev and $HOME for .beads workspaces (maxdepth 2)"
    local line
    while IFS= read -r line; do
      [[ -n "$line" ]] && pairs+=("$line")
    done < <(discover_repos)
  fi

  if [[ ${#pairs[@]} -eq 0 ]]; then
    warn "no .beads workspaces found; writing no config"
    warn "create $CONFIG_FILE by hand, or re-run with --repo name=/path"
    return 0
  fi

  info "found ${#pairs[@]} repo(s):"
  local p
  for p in "${pairs[@]}"; do
    [[ "$QUIET" -eq 1 ]] || echo "    ${p%%	*}  →  ${p#*	}"
  done
  ask_yn "Write $CONFIG_FILE serving these?" y || { info "config skipped"; return 0; }

  mkdir -p "$CONFIG_DIR"
  [[ -f "$CONFIG_FILE" ]] && cp "$CONFIG_FILE" "$CONFIG_FILE.bak.$(date +%Y%m%d%H%M%S)"

  local socket="${XDG_RUNTIME_DIR:-/tmp}/beads_watch.sock"
  printf '%s\n' "${pairs[@]}" | python3 -c '
import json, sys
repos = []
for line in sys.stdin:
    line = line.rstrip("\n")
    if not line:
        continue
    name, _, path = line.partition("\t")
    repos.append({"name": name, "path": path})
json.dump({"socket": sys.argv[1], "repos": repos}, open(sys.argv[2], "w"), indent=2)
open(sys.argv[2], "a").write("\n")
' "$socket" "$CONFIG_FILE" || die 'could not write the config' 1
  ok "wrote $CONFIG_FILE"

  # Validate before anything tries to start against it.
  if "$PREFIX/$BIN_NAME" --print-config >/dev/null 2>&1; then
    ok "config validates"
  else
    warn "config did not validate:"
    "$PREFIX/$BIN_NAME" --print-config 2>&1 | head -5 >&2 || true
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
    warn "no tailscale IPv4 on this box; installing the daemon unit but not the bridge"
  else
    ok "tailnet address: $ip"
  fi

  mkdir -p "$UNIT_DIR"

  # Fingerprint the units before touching them, so a re-run that changes
  # nothing does not restart a healthy daemon for no reason.
  units_fingerprint() {
    local u out=""
    for u in "$SERVICE_UNIT" "$PROXY_SOCKET" "$PROXY_SERVICE"; do
      out+="$(cat "$UNIT_DIR/$u" 2>/dev/null || true)"
    done
    printf '%s' "$out" | cksum
  }
  local before after
  before=$(units_fingerprint)

  # A previous manual install symlinks these into a checkout. Redirecting onto
  # a symlink writes THROUGH it and edits the repo's tracked unit files, so
  # clear the link first and write a real file in its place.
  local unit
  for unit in "$SERVICE_UNIT" "$PROXY_SOCKET" "$PROXY_SERVICE"; do
    if [[ -L "$UNIT_DIR/$unit" ]]; then
      info "replacing symlinked $unit (was → $(readlink "$UNIT_DIR/$unit"))"
      rm -f "$UNIT_DIR/$unit"
    fi
  done

  # Drop-ins outrank the unit file. The one this installer's predecessor needed
  # exists to patch ListenStream onto a unit that hardcoded another box's IP,
  # which is exactly what generating the unit per box makes unnecessary. Left
  # in place it would silently override the address written below, so --port
  # would not do what it says.
  local dropin
  for unit in "$SERVICE_UNIT" "$PROXY_SOCKET" "$PROXY_SERVICE"; do
    dropin="$UNIT_DIR/$unit.d"
    if [[ -d "$dropin" ]]; then
      mv "$dropin" "$dropin.bak.$(date +%Y%m%d%H%M%S)"
      warn "moved aside $unit.d (its overrides would outrank the generated unit)"
    fi
  done

  cat >"$UNIT_DIR/$SERVICE_UNIT" <<UNIT
[Unit]
Description=beads_watch — serve br over the tailnet
Documentation=https://github.com/a44-io/beads_watch
After=network-online.target

[Service]
Type=exec
ExecStart=$PREFIX/$BIN_NAME
Restart=on-failure
RestartSec=2s

# The daemon scrubs these from br's environment anyway, but a service file that
# cannot introduce them is one less way to end up serving the wrong workspace.
UnsetEnvironment=BEADS_DIR BD_DB BD_DATABASE BD_ACTOR BR_OUTPUT_FORMAT TOON_DEFAULT_FORMAT
Environment=RUST_LOG=error

# br needs to read and write .beads/ across the home directory, so the
# filesystem stays writable; the rest is tightened.
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-write
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictNamespaces=true
LockPersonality=true

[Install]
WantedBy=default.target
UNIT
  ok "wrote $UNIT_DIR/$SERVICE_UNIT"

  if [[ -n "$ip" ]]; then
    cat >"$UNIT_DIR/$PROXY_SOCKET" <<UNIT
[Unit]
Description=beads_watch TCP bridge — tailnet :$BRIDGE_PORT → unix socket
Documentation=https://github.com/a44-io/beads_watch

[Socket]
# Bind only the tailscale IP: tailnet peers (i.e. caddy on pi) can reach it,
# the LAN cannot. FreeBind lets the socket exist before tailscaled brings the
# address up at login.
ListenStream=$ip:$BRIDGE_PORT
FreeBind=true

[Install]
WantedBy=sockets.target
UNIT

    cat >"$UNIT_DIR/$PROXY_SERVICE" <<UNIT
[Unit]
Description=beads_watch TCP bridge — proxy to the unix socket
Documentation=https://github.com/a44-io/beads_watch
Requires=$SERVICE_UNIT
After=$SERVICE_UNIT

[Service]
ExecStart=/usr/lib/systemd/systemd-socket-proxyd %t/beads_watch.sock
PrivateTmp=true
NoNewPrivileges=true
UNIT
    ok "wrote the bridge units ($ip:$BRIDGE_PORT)"
  fi

  after=$(units_fingerprint)
  [[ "$before" != "$after" ]] && UNITS_CHANGED=1

  systemctl --user daemon-reload

  if [[ "$NO_START" -eq 1 ]]; then
    info "units installed but not started (--no-start)"
    return 0
  fi
  ask_yn "Enable and start beads_watch now?" y || { info "units left disabled"; return 0; }

  systemctl --user enable "$SERVICE_UNIT" &>/dev/null ||
    warn "could not enable $SERVICE_UNIT"
  [[ -n "$ip" ]] && { systemctl --user enable "$PROXY_SOCKET" &>/dev/null || warn "could not enable $PROXY_SOCKET"; }

  # `enable --now` starts a stopped unit but leaves a running one alone, so on
  # an upgrade it would report success while the OLD binary kept serving. Any
  # real change has to restart.
  local restarting=0
  if [[ "$BINARY_CHANGED" -eq 1 || "$UNITS_CHANGED" -eq 1 ]] &&
    systemctl --user is-active --quiet "$SERVICE_UNIT"; then
    restarting=1
    info "restarting to pick up the new $([[ "$BINARY_CHANGED" -eq 1 ]] && echo binary || echo units)"
  fi

  # A .socket refuses to start while the service it activates is still running
  # ("Socket service ... already active, refusing"), so restarting the socket
  # on its own takes the bridge down and leaves it down. The pair has to come
  # down together and go back up socket first, with the proxy service left for
  # the socket to trigger on the next connection.
  if [[ "$restarting" -eq 1 && -n "$ip" ]]; then
    systemctl --user stop "$PROXY_SOCKET" "$PROXY_SERVICE" &>/dev/null || true
  fi

  if [[ "$restarting" -eq 1 ]]; then
    systemctl --user restart "$SERVICE_UNIT" ||
      warn "could not restart $SERVICE_UNIT; see: journalctl --user -u beads_watch -n 30"
  else
    systemctl --user start "$SERVICE_UNIT" ||
      warn "could not start $SERVICE_UNIT; see: journalctl --user -u beads_watch -n 30"
  fi

  if [[ -n "$ip" ]]; then
    systemctl --user start "$PROXY_SOCKET" || warn "could not start $PROXY_SOCKET"
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
  for i in 1 2 3; do
    if curl -s --max-time 3 "http://$ip:$BRIDGE_PORT/v1/health" >/dev/null 2>&1; then
      ok "bridge: http://$ip:$BRIDGE_PORT reachable"
      return 0
    fi
    sleep 1
  done
  warn "bridge on $ip:$BRIDGE_PORT did not answer"
  warn "  systemctl --user status $PROXY_SOCKET"
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
    "binary   → $PREFIX/$BIN_NAME" \
    "config   → $([[ "$NO_CONFIG" -eq 1 ]] && echo 'skipped' || echo "$CONFIG_FILE")" \
    "units    → $([[ "$NO_UNITS" -eq 1 ]] && echo 'skipped' || echo "$UNIT_DIR")" \
    "source   → $([[ "$REPO_MODE" -eq 1 && -z "$DIST_URL" ]] && echo "$HERE" || echo "${DIST_URL:-<none>}")" \
    "bridge   → $(tailnet_ip):$BRIDGE_PORT"
}

print_summary() {
  [[ "$QUIET" -eq 1 ]] && return 0
  local ip
  ip=$(tailnet_ip)
  local lines=("\033[1mbeads_watch is installed\033[0m" "")
  lines+=("binary   $PREFIX/$BIN_NAME")
  [[ -f "$CONFIG_FILE" ]] && lines+=("config   $CONFIG_FILE")
  [[ -n "$MANIFEST_COMMIT" ]] && lines+=("commit   ${MANIFEST_COMMIT:0:12}${MANIFEST_TAG:+ ($MANIFEST_TAG)}")
  [[ -n "$ip" ]] && lines+=("bridge   http://$ip:$BRIDGE_PORT")
  lines+=("")
  lines+=("Name it from pi so it gets a URL:")
  lines+=("  ssh pi caddy/expose beads-$(node_name) $ip:$BRIDGE_PORT")
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
