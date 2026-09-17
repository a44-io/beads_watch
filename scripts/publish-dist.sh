#!/usr/bin/env bash
# Build the distribution that setup.sh consumes, and publish it: as a draft
# GitHub Release (the public channel), to a private fileserver over rsync (a
# fleet's own channel), or both from one run.
#
# The public channel is normally driven by .github/workflows/release.yml, which
# runs this script with --release --sign on a version tag push. Run that way,
# every asset gets a Sigstore bundle bound to the workflow's identity, which is
# what setup.sh verifies. Run by hand, --release still works and produces an
# unsigned draft; setup.sh installs from one, but says so.
#
#   scripts/publish-dist.sh                        # build ./dist/ only
#   scripts/publish-dist.sh --tag v1.2.0 --release
#                                                  # …and upload the release
#                                                  # asset set as a DRAFT release
#                                                  # on github.com/a44-io/beads_watch;
#                                                  # publish it from the GitHub UI
#   scripts/publish-dist.sh --tag v1.2.0 --release --sign
#                                                  # …signing each asset with
#                                                  # cosign first (what the
#                                                  # workflow runs)
#   scripts/publish-dist.sh --url https://dist.example.com
#                                                  # …and stamp that URL into the
#                                                  # served setup.sh, so a bare
#                                                  # `curl …/setup.sh | bash` needs
#                                                  # no BW_DIST_URL
#   scripts/publish-dist.sh --url https://dist.example.com --push host:/srv/dist/
#                                                  # …and rsync dist/ there
#   scripts/publish-dist.sh --tag v1.2.0 --push-tag --release --url … --push …
#                                                  # tag, GitHub, and the fileserver
#                                                  # in one go
#
# Options:
#   --out DIR        output directory (default: <repo>/dist)
#   --url URL        base URL to stamp into the fileserver copy of setup.sh's
#                    DEFAULT_DIST_URL
#   --push DEST      rsync -a --delete the dist dir to DEST (host:/path);
#                    the release/ subdirectory is excluded
#   --tag NAME       create an annotated git tag at HEAD, recorded in the manifest
#   --push-tag       also `git push origin NAME` (needs --tag; off by default,
#                    because publishing artifacts should not silently write to
#                    a remote)
#   --release        also build the flat GitHub Release asset set into
#                    <out>/release/ and create a DRAFT release for --tag on
#                    a44-io/beads_watch with gh. Needs --tag vX.Y.Z. Always a
#                    draft: nothing is public until a human publishes it.
#   --sign           sign every release asset with cosign (keyless) and upload
#                    the .sigstore.json bundles beside them. Needs --release
#                    and cosign on PATH. Inside the release workflow the
#                    identity is the workflow itself, which is the only one
#                    setup.sh accepts; a signature from a browser login is a
#                    real signature under an identity setup.sh refuses.
#   --targets LIST   comma-separated GOOS/GOARCH pairs
#                    (default: linux/amd64,linux/arm64)
#   --no-binary      skip prebuilt binaries entirely (consumers build from source)
#   --allow-dirty    publish from a dirty tree anyway (the bundle is HEAD, so
#                    uncommitted changes are silently NOT what gets served,
#                    which is why this needs saying out loud)
#
# What lands in the dist dir (the fileserver layout):
#   setup.sh                          the guided installer (URL-stamped if --url)
#   beads_watch.bundle                `git bundle` of the branch. A clone from it
#                                     keeps full history, so a consumer's source
#                                     build can still stamp its own commit.
#   bin/beads_watch-<os>-<arch>.tar.gz  prebuilt, CGO_ENABLED=0 (static, so it
#                                     does not inherit this box's glibc)
#   manifest.json                     commit, tag, and sha256 for everything above
#
# What lands on a GitHub Release (--release; assets are a flat namespace):
#   setup.sh                          stamped with the release's own download
#                                     base, so the served copy is pinned to it
#   beads_watch-<os>-<arch>.tar.gz    the same prebuilts, flat names
#   SHA256SUMS                        sha256sum format, every other asset
#   manifest.json                     the same schema; binaries[].file are the
#                                     flat names and bundle is null. No git
#                                     bundle: the public repo is the clone source.
#   <asset>.sigstore.json             with --sign: one cosign bundle per asset
#                                     above (certificate, signature, and the
#                                     transparency-log entry in one file)
#   A copy of the set is left in <out>/release/.
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
RELEASE=0
SIGN=0
TARGETS="linux/amd64,linux/arm64"
WITH_BINARY=1
ALLOW_DIRTY=0

# The public channel. A release's assets download from
# $RELEASE_BASE/download/<tag>/<asset>, and the newest from
# $RELEASE_BASE/latest/download/<asset>; setup.sh derives both.
RELEASE_REPO="a44-io/beads_watch"
RELEASE_BASE="https://github.com/$RELEASE_REPO/releases"

# The identity a --sign run inside the release workflow signs under, as it
# appears in the certificate: this workflow file, in this repo, at a version
# tag. setup.sh carries the same pair and verifies against it; the two must
# move together.
SIGN_IDENTITY_RE='^https://github\.com/a44-io/beads_watch/\.github/workflows/release\.yml@refs/tags/v[0-9]'
SIGN_ISSUER='https://token.actions.githubusercontent.com'

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
    --release) RELEASE=1; shift ;;
    --sign) SIGN=1; shift ;;
    --targets) [[ -n "${2:-}" ]] || die '--targets needs a list' 2; TARGETS="$2"; shift 2 ;;
    --no-binary) WITH_BINARY=0; shift ;;
    --allow-dirty) ALLOW_DIRTY=1; shift ;;
    -h | --help) sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument '$1'" 2 ;;
  esac
done

[[ "$PUSH_TAG" -eq 1 && -z "$TAG" ]] && die '--push-tag needs --tag' 2
if [[ "$RELEASE" -eq 1 ]]; then
  # A release is a tag. The shape check is loose on purpose: vX.Y.Z plus an
  # optional pre-release suffix, so a throwaway v0.0.0-draft-test passes.
  [[ -n "$TAG" ]] || die "--release needs --tag vX.Y.Z (a release is a tag; there are no untagged public artifacts)
  usage: scripts/publish-dist.sh --tag vX.Y.Z --release" 2
  [[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] ||
    die "--release wants a vX.Y.Z tag, got '$TAG'" 2
  [[ "$WITH_BINARY" -eq 1 ]] || die '--release without prebuilt binaries makes no sense (drop --no-binary)' 2
fi
[[ "$SIGN" -eq 1 && "$RELEASE" -eq 0 ]] && die '--sign needs --release (only release assets are signed)' 2

command -v git >/dev/null || die 'git is required'
command -v python3 >/dev/null || die 'python3 is required (writes manifest.json)'
[[ "$WITH_BINARY" -eq 1 ]] && { command -v go >/dev/null || die 'go is required for prebuilt binaries (or pass --no-binary)'; }
if [[ "$RELEASE" -eq 1 ]]; then
  command -v gh >/dev/null || die 'gh is required for --release (creates the draft release)'
  gh auth status >/dev/null 2>&1 || die 'gh is not authenticated; run gh auth login (the a44-io account)'
  command -v sha256sum >/dev/null || command -v shasum >/dev/null || die 'sha256sum or shasum is required for --release (writes SHA256SUMS)'
fi
[[ "$SIGN" -eq 1 ]] && { command -v cosign >/dev/null || die 'cosign is required for --sign (https://docs.sigstore.dev/cosign/system_config/installation/)'; }

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
# A tag checkout (the release workflow's) is a detached HEAD, which
# --abbrev-ref names "HEAD"; the manifest should say which branch carries the
# commit, so fall back to the first remote branch that does.
BRANCH=$(git -C "$REPO" symbolic-ref -q --short HEAD ||
  git -C "$REPO" branch -r --contains HEAD --format='%(refname:short)' 2>/dev/null | sed 's#^origin/##' | grep -v '^HEAD$' | head -1)
BRANCH=${BRANCH:-detached}
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
# bin_entry GOOS GOARCH FILE TARBALL — one manifest.json binaries[] row. FILE is
# the path setup.sh appends to the dist URL: bin/… on the fileserver, the bare
# name on a release, where assets have no directories.
bin_entry() {
  local goos="$1" goarch="$2" file="$3" tarball="$4"
  printf '{"target": "%s-%s", "goos": "%s", "goarch": "%s", "file": "%s", "sha256": "%s", "bytes": %s, "commit": "%s"}' \
    "$goos" "$goarch" "$goos" "$goarch" "$file" "$(sha256_of "$tarball")" "$(wc -c <"$tarball" | tr -d ' ')" "$COMMIT"
}

BIN_ENTRIES=()     # fileserver layout, bin/<name>
REL_ENTRIES=()     # release layout, flat
TARBALLS=()
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

    name="beads_watch-$goos-$goarch.tar.gz"
    tarball="$OUT/bin/$name"
    tar -czf "$tarball" -C "$stage" beads_watch
    rm -rf "$stage"

    TARBALLS+=("$tarball")
    BIN_ENTRIES+=("$(bin_entry "$goos" "$goarch" "bin/$name" "$tarball")")
    REL_ENTRIES+=("$(bin_entry "$goos" "$goarch" "$name" "$tarball")")
    echo "publish-dist: prebuilt $goos/$goarch ($(du -h "$tarball" | cut -f1 | tr -d ' '))"
  done
fi

# ── 3. setup.sh, URL-stamped when the serving URL is known ──────────────────
# stage_setup DEST [URL] — copy the installer, stamping DEFAULT_DIST_URL when a
# URL is given, on the one assignment line the marker comment in setup.sh
# points at.
stage_setup() {
  local dest="$1" url="${2:-}"
  cp "$REPO/setup.sh" "$dest"
  chmod 755 "$dest"
  [[ -n "$url" ]] || return 0
  sed -i.bak "s|^DEFAULT_DIST_URL=\"\"|DEFAULT_DIST_URL=\"$url\"|" "$dest" &&
    rm -f "$dest.bak"
  grep -q "DEFAULT_DIST_URL=\"$url\"" "$dest" ||
    die "failed to stamp DEFAULT_DIST_URL into $dest"
  echo "publish-dist: stamped dist URL: $url"
}

stage_setup "$OUT/setup.sh" "$URL"

# ── 4. manifest.json ────────────────────────────────────────────────────────
GENERATED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION=$(sed -n 's/^const Version = "\(.*\)"$/\1/p' "$REPO/internal/server/server.go" | head -1)
DIRTY=false
[[ -n "$STATUS" ]] && DIRTY=true

# write_manifest OUT BUNDLE SETUP ENTRY... — BUNDLE is the bundle path, or
# empty for a release, where the manifest says bundle: null and setup.sh
# clones the public repo instead.
write_manifest() {
  local out="$1" bundle="$2" setup="$3"
  shift 3
  local bundle_sha="" bundle_bytes=0
  if [[ -n "$bundle" ]]; then
    bundle_sha=$(sha256_of "$bundle")
    bundle_bytes=$(wc -c <"$bundle" | tr -d ' ')
  fi
  python3 - "$out" "$GENERATED_AT" "$COMMIT" "$BRANCH" "$TAG" "$VERSION" "$DIRTY" \
    "$bundle_sha" "$bundle_bytes" "$(sha256_of "$setup")" "$@" <<'PYEOF'
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
    "bundle": {"file": "beads_watch.bundle", "sha256": bundle_sha, "bytes": int(bundle_bytes)} if bundle_sha else None,
    "setup": {"file": "setup.sh", "sha256": setup_sha},
    "binaries": [json.loads(e) for e in bin_entries if e.strip()],
}
with open(out, "w") as f:
    json.dump(manifest, f, indent=2)
    f.write("\n")
PYEOF
  echo "publish-dist: wrote $out"
}

write_manifest "$OUT/manifest.json" "$OUT/beads_watch.bundle" "$OUT/setup.sh" "${BIN_ENTRIES[@]}"

# ── 5. Release asset set ────────────────────────────────────────────────────
# Assets on a GitHub Release share one flat namespace, so the tarballs lose
# their bin/ prefix and the manifest names them bare. setup.sh is stamped with
# the tag's own download base, so the copy a user fetches from
# releases/latest is pinned to the release it came with rather than resolving
# "latest" a second time and racing the next publish. SHA256SUMS is what a
# public user expects to run `sha256sum -c` against; manifest.json is what
# setup.sh reads, and it covers the manifest too.
REL_DIR="$OUT/release"
if [[ "$RELEASE" -eq 1 ]]; then
  REL_URL="$RELEASE_BASE/download/$TAG"
  rm -rf "$REL_DIR"
  mkdir -p "$REL_DIR"
  for tarball in "${TARBALLS[@]}"; do
    cp "$tarball" "$REL_DIR/$(basename "$tarball")"
  done
  stage_setup "$REL_DIR/setup.sh" "$REL_URL"
  write_manifest "$REL_DIR/manifest.json" "" "$REL_DIR/setup.sh" "${REL_ENTRIES[@]}"

  REL_ASSETS=(setup.sh)
  for tarball in "${TARBALLS[@]}"; do REL_ASSETS+=("$(basename "$tarball")"); done
  REL_ASSETS+=(manifest.json)
  (
    cd "$REL_DIR" &&
      if command -v sha256sum >/dev/null; then sha256sum "${REL_ASSETS[@]}"; else shasum -a 256 "${REL_ASSETS[@]}"; fi >SHA256SUMS
  ) || die 'could not write SHA256SUMS'
  REL_ASSETS+=(SHA256SUMS)
  echo "publish-dist: release assets in $REL_DIR: ${REL_ASSETS[*]}"
  # Version is a Go const, so the binary reports it and the commit, never the
  # tag; the two are meant to agree, and a mismatch is worth a line.
  [[ "$TAG" == "v$VERSION" || "$TAG" == "v$VERSION-"* ]] ||
    echo "publish-dist: NOTE tag $TAG does not match const Version \"$VERSION\" in internal/server/server.go" >&2
fi

# ── 5b. Signatures ──────────────────────────────────────────────────────────
# One bundle per asset, SHA256SUMS and manifest.json included, so the sums a
# user checks by hand are themselves signed. Keyless: cosign trades an OIDC
# token for a minutes-long certificate, signs, and records both in the public
# transparency log; the bundle carries certificate, signature, and log entry
# together, so verification needs nothing but the bundle and the file. In the
# release workflow the token is the job's own (id-token: write) and the
# certificate names the workflow file at the tag. Outside it, cosign opens a
# browser login, and the certificate names whoever logged in: a valid
# signature under an identity setup.sh does not trust. Say so instead of
# leaving it to be discovered at install time.
SIGNED=()
if [[ "$SIGN" -eq 1 ]]; then
  if [[ -z "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" ]]; then
    echo "publish-dist: NOTE not inside GitHub Actions; the signing identity will be your login, which" >&2
    echo "publish-dist:      setup.sh refuses (it trusts only $SIGN_IDENTITY_RE). Fine for a local check." >&2
  fi
  for asset in "${REL_ASSETS[@]}"; do
    echo "publish-dist: signing $asset"
    (cd "$REL_DIR" && cosign sign-blob --yes --bundle "$asset.sigstore.json" "$asset" >/dev/null) ||
      die "cosign sign-blob failed for $asset"
    SIGNED+=("$asset.sigstore.json")
  done
  # Verify what was just signed, the way setup.sh will, before uploading it.
  # Outside the workflow the identity check is expected to fail, so it is only
  # a hard gate where the identity can be right.
  if [[ -n "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" ]]; then
    for asset in "${REL_ASSETS[@]}"; do
      (cd "$REL_DIR" && cosign verify-blob --bundle "$asset.sigstore.json" \
        --certificate-identity-regexp "$SIGN_IDENTITY_RE" \
        --certificate-oidc-issuer "$SIGN_ISSUER" "$asset" >/dev/null 2>&1) ||
        die "freshly signed $asset does not verify against $SIGN_IDENTITY_RE; is this the release workflow on a v* tag?"
    done
    echo "publish-dist: ${#SIGNED[@]} bundles verify against the release workflow identity"
  fi
fi

# ── 6. Push ─────────────────────────────────────────────────────────────────
if [[ -n "$PUSH" ]]; then
  command -v rsync >/dev/null || die 'rsync is required for --push'
  echo "publish-dist: rsync → $PUSH"
  rsync -a --delete --exclude=/release/ "$OUT/" "$PUSH" || die 'rsync failed'
fi

# ── 7. Draft release ────────────────────────────────────────────────────────
# Always a draft. Publishing is the one step a human does, from the GitHub UI,
# after looking at what was uploaded; the script never makes anything public.
# A draft creates no tag on the remote either: GitHub creates the tag when the
# draft is published, at --target. That is pinned to the built commit when
# origin already has it, so a draft published after further commits land on
# main still tags what the binaries were built from. When origin does not
# have the commit yet (a build ahead of its push), --target cannot be given and
# the tag would be created at main's head at publish time: push first, or
# pass --push-tag, which makes the tag exist before the release does.
RELEASE_URL=""
if [[ "$RELEASE" -eq 1 ]]; then
  if gh release view "$TAG" --repo "$RELEASE_REPO" >/dev/null 2>&1; then
    die "a release named $TAG already exists on $RELEASE_REPO; delete or publish that one first"
  fi
  NOTES="$REL_DIR/.notes.md"
  PREV=$(git -C "$REPO" describe --tags --abbrev=0 --exclude "$TAG" HEAD 2>/dev/null || true)
  {
    echo "beads_watch $TAG"
    echo ""
    echo "Built from \`${COMMIT:0:12}\` on \`$BRANCH\`; \`beads_watch --version\` reports \`$VERSION\` and this commit."
    echo ""
    echo "## Install"
    echo ""
    echo '```bash'
    echo "curl -fsSL $REL_URL/setup.sh | bash"
    echo '```'
    echo ""
    echo "## Verify"
    echo ""
    echo '```bash'
    echo "sha256sum -c SHA256SUMS"
    echo '```'
    if [[ "$SIGN" -eq 1 ]]; then
      echo ""
      echo "Every asset has a Sigstore bundle beside it, signed by this repository's release workflow at this tag. With [cosign](https://docs.sigstore.dev/cosign/system_config/installation/):"
      echo ""
      echo '```bash'
      echo "cosign verify-blob --bundle SHA256SUMS.sigstore.json \\"
      echo "  --certificate-identity-regexp '$SIGN_IDENTITY_RE' \\"
      echo "  --certificate-oidc-issuer $SIGN_ISSUER \\"
      echo "  SHA256SUMS"
      echo '```'
      echo ""
      echo "\`setup.sh\` runs the same check on \`manifest.json\` and the tarball it installs when cosign is on the box."
    else
      echo ""
      echo "This release was cut by hand and carries no Sigstore bundles; \`setup.sh\` installs from it on sha256 alone and says so."
    fi
    if [[ -n "$PREV" ]]; then
      echo ""
      echo "## Changes since $PREV"
      echo ""
      git -C "$REPO" log --no-merges --format='- %s' "$PREV..HEAD"
    fi
  } >"$NOTES"

  TARGET_ARGS=()
  if git -C "$REPO" branch -r --contains "$COMMIT" 2>/dev/null | grep -q .; then
    TARGET_ARGS=(--target "$COMMIT")
  else
    echo "publish-dist: NOTE ${COMMIT:0:12} is not on any remote branch yet; the draft's tag" >&2
    echo "publish-dist:      will be created at main's head when it is published. Push first." >&2
  fi

  echo "publish-dist: creating DRAFT release $TAG on $RELEASE_REPO"
  RELEASE_URL=$(cd "$REL_DIR" && gh release create "$TAG" --repo "$RELEASE_REPO" --draft \
    --title "$TAG" --notes-file "$NOTES" "${TARGET_ARGS[@]}" "${REL_ASSETS[@]}" "${SIGNED[@]}") ||
    die 'gh release create failed'
  rm -f "$NOTES"
fi

echo ""
echo "✓ published ${COMMIT:0:12}${TAG:+ ($TAG)} to $OUT"
if [[ "$RELEASE" -eq 1 ]]; then
  echo "  DRAFT release: $RELEASE_URL"
  if [[ "$SIGN" -eq 1 ]]; then
    echo "  signed: ${#SIGNED[@]} Sigstore bundles uploaded beside the assets"
  else
    echo "  UNSIGNED: no Sigstore bundles; setup.sh will install from it on sha256 alone and say so."
    echo "  Signed releases come from .github/workflows/release.yml: push the tag and let it build."
  fi
  echo "  review it there and press Publish; once it is public, consumers run:"
  echo "    curl -fsSL $RELEASE_BASE/latest/download/setup.sh | bash"
fi
if [[ -n "$URL" ]]; then
  echo "  fileserver consumers run:"
  echo "    curl -fsSL \"$URL/setup.sh?\$(date +%s)\" | bash"
elif [[ "$RELEASE" -eq 0 ]]; then
  echo "  no --url given, so the served setup.sh needs telling where it came from:"
  echo "    curl -fsSL http://<fileserver>/setup.sh | BW_DIST_URL=http://<fileserver> bash"
fi
