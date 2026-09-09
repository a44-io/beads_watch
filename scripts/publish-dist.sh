#!/usr/bin/env bash
# Build the tailnet distribution directory that setup.sh consumes, and
# optionally rsync it to the fileserver. An operator action, not a CI gate.
#
#   scripts/publish-dist.sh                        # build ./dist/ only
#   scripts/publish-dist.sh --url https://bw.dev.a44.io
#                                                  # …and stamp that URL into the
#                                                  # served setup.sh, so a bare
#                                                  # `curl …/setup.sh | bash` needs
#                                                  # no BW_DIST_URL
#   scripts/publish-dist.sh --url https://bw.dev.a44.io --push pi:/srv/ice/.bw/
#                                                  # …and rsync dist/ there
#   scripts/publish-dist.sh --tag v0.2.0 --push-tag --url … --push …
#                                                  # …and cut an annotated tag
#
# Options:
#   --out DIR        output directory (default: <repo>/dist)
#   --url URL        public base URL to stamp into setup.sh's DEFAULT_DIST_URL
#   --push DEST      rsync -a --delete the dist dir to DEST (host:/path)
#   --tag NAME       create an annotated git tag at HEAD, recorded in the manifest
#   --push-tag       also `git push origin NAME` (needs --tag; off by default,
#                    because publishing artifacts should not silently write to
#                    a remote)
#   --targets LIST   comma-separated GOOS/GOARCH pairs
#                    (default: linux/amd64,linux/arm64)
#   --no-binary      skip prebuilt binaries entirely (consumers build from source)
#   --allow-dirty    publish from a dirty tree anyway (the bundle is HEAD, so
#                    uncommitted changes are silently NOT what gets served,
#                    which is why this needs saying out loud)
#
# What lands in the dist dir:
#   setup.sh                          the guided installer (URL-stamped if --url)
#   beads_watch.bundle                `git bundle` of the branch. A clone from it
#                                     keeps full history, so a consumer's source
#                                     build can still stamp its own commit.
#   bin/beads_watch-<os>-<arch>.tar.gz  prebuilt, CGO_ENABLED=0 (static, so it
#                                     does not inherit this box's glibc)
#   manifest.json                     commit, tag, and sha256 for everything above
#
# Exit codes: 0 published · 1 a step failed · 2 misuse.

set -euo pipefail
umask 022

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO=$(dirname "$HERE")

OUT="$REPO/dist"
URL=""
PUSH=""
TAG=""
PUSH_TAG=0
TARGETS="linux/amd64,linux/arm64"
WITH_BINARY=1
ALLOW_DIRTY=0

die() {
  printf 'publish-dist: %s\n' "$1" >&2
  exit "${2:-1}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --out) [[ -n "${2:-}" ]] || die '--out needs a directory' 2; OUT="$2"; shift 2 ;;
    --url) [[ -n "${2:-}" ]] || die '--url needs a URL' 2; URL="${2%/}"; shift 2 ;;
    --push) [[ -n "${2:-}" ]] || die '--push needs an rsync destination' 2; PUSH="$2"; shift 2 ;;
    --tag) [[ -n "${2:-}" ]] || die '--tag needs a name' 2; TAG="$2"; shift 2 ;;
    --push-tag) PUSH_TAG=1; shift ;;
    --targets) [[ -n "${2:-}" ]] || die '--targets needs a list' 2; TARGETS="$2"; shift 2 ;;
    --no-binary) WITH_BINARY=0; shift ;;
    --allow-dirty) ALLOW_DIRTY=1; shift ;;
    -h | --help) sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument '$1'" 2 ;;
  esac
done

[[ "$PUSH_TAG" -eq 1 && -z "$TAG" ]] && die '--push-tag needs --tag' 2

command -v git >/dev/null || die 'git is required'
command -v python3 >/dev/null || die 'python3 is required (writes manifest.json)'
[[ "$WITH_BINARY" -eq 1 ]] && { command -v go >/dev/null || die 'go is required for prebuilt binaries (or pass --no-binary)'; }

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

COMMIT=$(git -C "$REPO" rev-parse HEAD) || die 'cannot resolve HEAD'
STATUS=$(git -C "$REPO" status --porcelain --untracked-files=normal)
if [[ -n "$STATUS" && "$ALLOW_DIRTY" -eq 0 ]]; then
  die "working tree is dirty. The bundle serves HEAD ($COMMIT), not these changes.
  Commit them, or pass --allow-dirty to publish HEAD anyway." 1
fi
BRANCH=$(git -C "$REPO" rev-parse --abbrev-ref HEAD)
[[ "$BRANCH" == main ]] || echo "publish-dist: NOTE publishing branch '$BRANCH', not main" >&2

# ── 0. Tag ──────────────────────────────────────────────────────────────────
# Before anything is built, so a name collision fails the run early rather than
# after a full cross-compile.
if [[ -n "$TAG" ]]; then
  if git -C "$REPO" rev-parse -q --verify "refs/tags/$TAG" >/dev/null; then
    existing=$(git -C "$REPO" rev-list -n1 "$TAG")
    [[ "$existing" == "$COMMIT" ]] ||
      die "tag $TAG already exists and points at ${existing:0:12}, not HEAD (${COMMIT:0:12})"
    echo "publish-dist: tag $TAG already at HEAD, reusing"
  else
    git -C "$REPO" tag -a "$TAG" -m "beads_watch $TAG" || die "could not create tag $TAG"
    echo "publish-dist: tagged ${COMMIT:0:12} as $TAG"
  fi
  if [[ "$PUSH_TAG" -eq 1 ]]; then
    git -C "$REPO" push origin "$TAG" || die "could not push tag $TAG"
    echo "publish-dist: pushed tag $TAG to origin"
  fi
fi

mkdir -p "$OUT" "$OUT/bin"

# ── 1. Source bundle ────────────────────────────────────────────────────────
echo "publish-dist: bundling $BRANCH @ ${COMMIT:0:12}"
# HEAD must be in the bundle: without it a `git clone` of the bundle cannot pick
# a default branch and leaves an EMPTY tree on an unborn branch.
git -C "$REPO" bundle create "$OUT/beads_watch.bundle" HEAD "$BRANCH" 2>/dev/null ||
  die 'git bundle failed'
git bundle verify "$OUT/beads_watch.bundle" >/dev/null || die 'bundle failed verification'

# ── 2. Prebuilt binaries ────────────────────────────────────────────────────
# CGO_ENABLED=0 matters twice: it makes the binary static, so it does not carry
# this box's glibc version to a consumer, and it is what lets one box publish
# for the whole fleet.
BIN_ENTRIES=()
if [[ "$WITH_BINARY" -eq 1 ]]; then
  BUILT_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  LDFLAGS="-s -w"
  LDFLAGS+=" -X beads_watch/internal/server.Commit=$COMMIT"
  LDFLAGS+=" -X beads_watch/internal/server.BuiltAt=$BUILT_AT"

  IFS=',' read -r -a target_list <<<"$TARGETS"
  for target in "${target_list[@]}"; do
    target="${target// /}"
    [[ -n "$target" ]] || continue
    goos="${target%%/*}"
    goarch="${target##*/}"
    [[ "$goos" != "$target" && -n "$goarch" ]] || die "bad target '$target' (want GOOS/GOARCH)" 2

    stage=$(mktemp -d)
    echo "publish-dist: building $goos/$goarch"
    if ! CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
      go build -trimpath -ldflags "$LDFLAGS" -o "$stage/beads_watch" "$REPO"; then
      rm -rf "$stage"
      die "go build failed for $goos/$goarch"
    fi

    tarball="$OUT/bin/beads_watch-$goos-$goarch.tar.gz"
    tar -czf "$tarball" -C "$stage" beads_watch
    rm -rf "$stage"

    BIN_ENTRIES+=("{\"target\": \"$goos-$goarch\", \"goos\": \"$goos\", \"goarch\": \"$goarch\", \"file\": \"bin/beads_watch-$goos-$goarch.tar.gz\", \"sha256\": \"$(sha256_of "$tarball")\", \"bytes\": $(wc -c <"$tarball" | tr -d ' '), \"commit\": \"$COMMIT\"}")
    echo "publish-dist: prebuilt $goos/$goarch ($(du -h "$tarball" | cut -f1 | tr -d ' '))"
  done
fi

# ── 3. setup.sh, URL-stamped when the serving URL is known ──────────────────
cp "$REPO/setup.sh" "$OUT/setup.sh"
chmod 755 "$OUT/setup.sh"
if [[ -n "$URL" ]]; then
  # Stamp the one assignment line; the marker comment above it documents this.
  sed -i.bak "s|^DEFAULT_DIST_URL=\"\"|DEFAULT_DIST_URL=\"$URL\"|" "$OUT/setup.sh" &&
    rm -f "$OUT/setup.sh.bak"
  grep -q "DEFAULT_DIST_URL=\"$URL\"" "$OUT/setup.sh" ||
    die 'failed to stamp DEFAULT_DIST_URL into the served setup.sh'
  echo "publish-dist: stamped dist URL: $URL"
fi

# ── 4. manifest.json ────────────────────────────────────────────────────────
BUNDLE_SHA=$(sha256_of "$OUT/beads_watch.bundle")
BUNDLE_BYTES=$(wc -c <"$OUT/beads_watch.bundle" | tr -d ' ')
SETUP_SHA=$(sha256_of "$OUT/setup.sh")
GENERATED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION=$(sed -n 's/^const Version = "\(.*\)"$/\1/p' "$REPO/internal/server/server.go" | head -1)
DIRTY=false
[[ -n "$STATUS" ]] && DIRTY=true

python3 - "$OUT/manifest.json" "$GENERATED_AT" "$COMMIT" "$BRANCH" "$TAG" "$VERSION" "$DIRTY" \
  "$BUNDLE_SHA" "$BUNDLE_BYTES" "$SETUP_SHA" "${BIN_ENTRIES[@]}" <<'PYEOF'
import json, sys
(_, out, generated_at, commit, branch, tag, version, dirty,
 bundle_sha, bundle_bytes, setup_sha, *bin_entries) = sys.argv
manifest = {
    "schema": 1,
    "generated_at": generated_at,
    "commit": commit,
    "branch": branch,
    "tag": tag or None,
    "version": version,
    "dirty": dirty == "true",
    "bundle": {"file": "beads_watch.bundle", "sha256": bundle_sha, "bytes": int(bundle_bytes)},
    "setup": {"file": "setup.sh", "sha256": setup_sha},
    "binaries": [json.loads(e) for e in bin_entries if e.strip()],
}
with open(out, "w") as f:
    json.dump(manifest, f, indent=2)
    f.write("\n")
PYEOF

echo "publish-dist: wrote $OUT/manifest.json"

# ── 5. Push ─────────────────────────────────────────────────────────────────
if [[ -n "$PUSH" ]]; then
  command -v rsync >/dev/null || die 'rsync is required for --push'
  echo "publish-dist: rsync → $PUSH"
  rsync -a --delete "$OUT/" "$PUSH" || die 'rsync failed'
fi

echo ""
echo "✓ published ${COMMIT:0:12}${TAG:+ ($TAG)} to $OUT"
if [[ -n "$URL" ]]; then
  echo "  consumers run:"
  echo "    curl -fsSL \"$URL/setup.sh?\$(date +%s)\" | bash"
else
  echo "  no --url given, so the served setup.sh needs telling where it came from:"
  echo "    curl -fsSL http://<fileserver>/setup.sh | BW_DIST_URL=http://<fileserver> bash"
fi
