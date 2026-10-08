#!/usr/bin/env bash
# Orchestrate a jms-client release.
#
# The release spans several systems that do not know about each other:
# GoReleaser builds the archives, a separate script packs the MCPB bundle,
# GitHub hosts the release, the Go module proxy indexes the tag, the MCP
# Registry holds a record pointing at the bundle, and Scoop reads a
# manifest from another repository. This script runs them in the order
# that keeps the artifact hashes consistent, and verifies each step
# instead of assuming it worked.
#
# Usage:
#   scripts/release.sh v0.1.1            # build, publish, verify
#   scripts/release.sh v0.1.1 --dry-run  # build and verify locally only
#
# Exit status is non-zero on the first failed verification, because a
# release that half-succeeded is worse than one that did not start.

set -euo pipefail

cd "$(dirname "$0")/.."
root=$(pwd)

tag=${1:-}
dry_run=false
[ "${2:-}" = "--dry-run" ] && dry_run=true

if [ -z "$tag" ]; then
    echo "usage: scripts/release.sh <tag> [--dry-run]" >&2
    exit 2
fi
case $tag in
    v*) ;;
    *) echo "release: the tag must start with 'v' (got $tag)" >&2; exit 2 ;;
esac
version=${tag#v}

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------
#
# Every check here has cost a real release. They run before anything is
# published so a mistake is cheap to fix.

step() { printf '\n==> %s\n' "$1"; }
fail() { printf 'release: %s\n' "$1" >&2; exit 1; }

step "Preflight"

# 1. Branch. Committing or tagging on someone else's branch is how a
#    release ends up containing work nobody reviewed.
branch=$(git rev-parse --abbrev-ref HEAD)
if [ "$branch" != "main" ]; then
    fail "on branch '$branch', not 'main'. A release is cut from main."
fi

# 2. Clean tree. GoReleaser refuses a dirty tree anyway, but the message
#    here says which files.
if [ -n "$(git status --porcelain)" ]; then
    git status --short >&2
    fail "the working tree is dirty; commit or stash before releasing"
fi

# 3. The tag must not already exist, locally or remotely.
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
    fail "tag $tag already exists locally"
fi
if git ls-remote --exit-code --tags origin "$tag" >/dev/null 2>&1; then
    fail "tag $tag already exists on origin"
fi

# 4. Local main must match origin/main, so the release contains what was
#    actually pushed and reviewed.
git fetch --quiet origin main
if [ "$(git rev-parse HEAD)" != "$(git rev-parse origin/main)" ]; then
    fail "HEAD and origin/main differ; push or pull first"
fi

# 5. Line endings. A CRLF shell script ships a launcher that cannot start
#    on macOS or Linux, and the MCPB bundle packs these files verbatim.
step "Checking line endings"
bad_eol=0
for f in $(git ls-files '*.sh' '*.yaml' '*.yml' '*.json'); do
    if grep -qU $'\r' "$f" 2>/dev/null; then
        echo "  CRLF: $f" >&2
        bad_eol=1
    fi
done
[ "$bad_eol" -eq 0 ] || fail "the files above have CRLF endings; .gitattributes pins them to LF"

# 6. Required tools.
command -v goreleaser >/dev/null 2>&1 || fail "goreleaser is not on PATH"
command -v gh >/dev/null 2>&1 || fail "gh is not on PATH"
command -v python >/dev/null 2>&1 || fail "python is not on PATH"

echo "  tag=$tag  version=$version  branch=$branch  dry_run=$dry_run"

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

step "Running the test suite"
# GoReleaser runs this too; doing it first means a red test fails before a
# 24 MB bundle is assembled.
go test ./...

if $dry_run; then
    step "Building (dry run, no publish)"
    goreleaser release --clean --snapshot --skip=publish --skip=validate
else
    step "Tagging $tag"
    git tag -a "$tag" -m "Release $tag"
    git push origin "$tag"

    step "Building and publishing with GoReleaser"
    # One invocation, not several: the archives carry file mtimes, so a
    # second build produces different bytes and every hash recorded in the
    # first run stops matching. The Scoop manifest is generated from the
    # same run for exactly this reason.
    GITHUB_TOKEN=$(gh auth token) goreleaser release --clean
fi

# ---------------------------------------------------------------------------
# MCPB bundle
# ---------------------------------------------------------------------------
#
# Packed from the binaries GoReleaser just built, so the bundle and the
# archives contain the same bytes. GoReleaser cannot do this itself: its
# mcp publisher accepts npm, pypi, nuget and oci only, and the bundle is an
# mcpb package.

step "Packing the MCPB bundle"
# WRITE_SERVER_JSON rewrites the fileSha256 in mcpb/server.json, which only
# makes sense once the bundle is really going to be uploaded.
if $dry_run; then
    bundle_sha=$(WRITE_SERVER_JSON=0 bash scripts/build-mcpb.sh --use-dist 2>/dev/null | tail -1)
else
    bundle_sha=$(WRITE_SERVER_JSON=1 bash scripts/build-mcpb.sh --use-dist 2>/dev/null | tail -1)
fi
echo "  bundle sha256: $bundle_sha"

# ---------------------------------------------------------------------------
# Publish and verify
# ---------------------------------------------------------------------------

if $dry_run; then
    step "Dry run complete"
    echo "  artifacts in dist/, nothing was published"
    echo "  server.json was left untouched"
    exit 0
fi

step "Attaching the bundle to the release"
# --clobber so a retried release replaces rather than fails. The bundle is
# uploaded after the archives because the release is a draft until a human
# publishes it, which gives a chance to confirm the hashes first.
gh release upload "$tag" dist/jms-client.mcpb --clobber

step "Verifying the release assets"
gh release view "$tag" --json assets --jq '.assets[].name' | sort

step "Verifying the MCP Registry record"
# The registry stores the hash of the bundle. If it disagrees with what was
# uploaded, every client that installs the bundle will reject it, so this
# is checked rather than assumed.
sleep 5
reg_sha=$(curl -sL "https://registry.modelcontextprotocol.io/v0.1/servers?search=io.github.MiFaZhan/jms-client" \
    | python -c "
import json,sys
try:
    d = json.load(sys.stdin)
except Exception:
    print('')
    raise SystemExit
for s in d.get('servers', []):
    sv = s.get('server', {})
    for p in sv.get('packages') or []:
        print(p.get('fileSha256',''))
" | head -1)

if [ -z "$reg_sha" ]; then
    echo "  the registry has no record yet; publish mcpb/server.json with mcp-publisher" >&2
elif [ "$reg_sha" = "$bundle_sha" ]; then
    echo "  registry sha matches the uploaded bundle"
else
    fail "registry sha ($reg_sha) does not match the bundle ($bundle_sha); run 'mcp-publisher publish' from mcpb/"
fi

step "Verifying the Go module proxy"
# `go install ...@latest` only works once the proxy has indexed the tag.
for _ in 1 2 3 4 5 6; do
    if curl -sf -o /dev/null "https://proxy.golang.org/github.com/!mi!fa!zhan/jms-client/@v/$tag.info"; then
        echo "  proxy has indexed $tag"
        break
    fi
    sleep 5
done

step "Release $tag complete"
cat <<EOF
  Next, if this is the first release or the bundle changed:
    cd mcpb && mcp-publisher publish

  Scoop picks the new version up from the bucket on the next
  \`scoop update\`; the manifest was committed by GoReleaser.
EOF
