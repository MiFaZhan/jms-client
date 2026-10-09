#!/usr/bin/env bash
# Assemble the MCPB bundle for jms-client.
#
# MCPB's platform_overrides distinguishes operating systems but not CPU
# architectures, so one bundle carries a binary per architecture and
# mcpb/launcher.sh (or launcher.cmd) picks the right one at run time. That
# is why this script produces six binaries and one archive.
#
# Usage:
#   scripts/build-mcpb.sh              # build binaries, then the bundle
#   scripts/build-mcpb.sh --use-dist   # reuse binaries already in dist/
#
# Output: dist/jms-client.mcpb, plus its SHA-256 on stdout. The hash is
# what mcpb/server.json must carry for the MCP Registry to accept the
# package.

set -euo pipefail

cd "$(dirname "$0")/.."
root=$(pwd)

# The version comes from the tag being released, so the manifest, the
# registry record and the binaries cannot disagree. RELEASE_VERSION lets a
# dry run name a version without a tag existing yet.
version="${RELEASE_VERSION:-}"
if [ -z "$version" ]; then
    version=$(git describe --tags --abbrev=0 2>/dev/null | sed 's/^v//' || true)
fi
if [ -z "$version" ]; then
    version=$(grep -oE '"version": *"[^"]+"' mcpb/manifest.json | head -1 | sed 's/.*"\([^"]*\)"$/\1/')
    echo "build-mcpb: no git tag found, using manifest version $version" >&2
fi

use_dist=false
[ "${1:-}" = "--use-dist" ] && use_dist=true

# Target list: goos/goarch pairs, and the file name inside server/.
# The names are what launcher.sh and launcher.cmd look for, so the two must
# be changed together.
targets=(
    "darwin/arm64:jms-darwin-aarch64"
    "darwin/amd64:jms-darwin-x86_64"
    "linux/arm64:jms-linux-aarch64"
    "linux/amd64:jms-linux-x86_64"
    "windows/arm64:jms-win32-aarch64.exe"
    "windows/amd64:jms-win32-x86_64.exe"
)

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

# Stamp the version before anything is copied into the bundle. The manifest is
# packed into the archive, so stamping it after the copy would leave the
# bundle declaring the previous version — which is exactly what shipped as
# v0.1.2.
stamp_manifest() {
    python - "$version" <<'PY'
import re, sys

version = sys.argv[1]
path = "mcpb/manifest.json"
with open(path, encoding="utf-8", newline="") as fh:
    text = fh.read()
updated, n = re.subn(r'("version":\s*")[^"]+(")', rf'\g<1>{version}\g<2>', text, count=1)
if n != 1:
    sys.exit(f"expected exactly one version field in {path}, found {n}")
# newline="" keeps the file's existing line endings: Python's default text
# mode would rewrite every \n as \r\n on Windows, which is how an earlier
# version of this script turned a file CRLF and made it inconsistent with
# the rest of the tree.
with open(path, "w", encoding="utf-8", newline="") as fh:
    fh.write(updated)
print(f"build-mcpb: stamped version {version} into {path}", file=sys.stderr)
PY
}

# Only a real release rewrites the tracked manifests; a dry run must leave the
# tree clean so it can be repeated.
if [ "${WRITE_SERVER_JSON:-0}" = "1" ]; then
    stamp_manifest
fi

mkdir -p "$stage/server"
cp mcpb/manifest.json "$stage/manifest.json"
cp mcpb/launcher.sh "$stage/launcher.sh"
cp mcpb/launcher.cmd "$stage/launcher.cmd"
chmod +x "$stage/launcher.sh"

for entry in "${targets[@]}"; do
    target=${entry%%:*}
    name=${entry##*:}
    goos=${target%%/*}
    goarch=${target##*/}

    if $use_dist; then
        # GoReleaser writes <project>_<os>_<arch>/jms[.exe] under dist/.
        src=$(find dist -type f \( -name 'jms' -o -name 'jms.exe' \) \
            -path "*${goos}_${goarch}*" 2>/dev/null | head -1)
        if [ -z "$src" ]; then
            echo "build-mcpb: no dist binary for $goos/$goarch" >&2
            exit 1
        fi
        cp "$src" "$stage/server/$name"
    else
        echo "build-mcpb: building $goos/$goarch" >&2
        CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build \
            -trimpath \
            -ldflags "-s -w -X github.com/MiFaZhan/jms-client/internal/cli.Version=v${version}" \
            -o "$stage/server/$name" ./cmd/jms
    fi
    chmod +x "$stage/server/$name"
done

mkdir -p dist
out="$root/dist/jms-client.mcpb"
rm -f "$out"

# Zip via Python rather than the zip(1) CLI: it is present everywhere this
# script runs, and it lets the archive carry forward-slash paths and the
# executable bit, which a Windows archiver would otherwise drop.
python - "$stage" "$out" <<'PY'
import os, sys, zipfile

stage, out = sys.argv[1], sys.argv[2]


def is_executable(rel: str) -> bool:
    """Decide the executable bit from the path, not from os.stat.

    On Windows NTFS there is no executable bit: os.stat reports 0o666 for
    every file, and chmod is a no-op, so a stat-based check silently
    produces a bundle whose launcher cannot be run on macOS or Linux. The
    decision is therefore made from what the file is.
    """
    name = rel.rsplit("/", 1)[-1]
    if name.endswith(".sh"):
        return True
    # The bundled server binaries: the Windows ones end in .exe, the rest
    # are named jms-<platform>-<arch>.
    return name.startswith("jms-") and not name.endswith(".cmd")


# Fixed timestamp for every entry, so the same inputs always produce the
# same bytes and therefore the same SHA-256. Without this the archive
# carries each file's mtime, and the hash in server.json would have to be
# recomputed after every rebuild — which makes "build, record the hash,
# upload" fail the moment anything is retried. 1980-01-01 is the earliest
# value the ZIP format can represent.
EPOCH = (1980, 1, 1, 0, 0, 0)

with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
    for dirpath, dirnames, filenames in os.walk(stage):
        # Sort both, so the archive entry order does not depend on the
        # filesystem's directory order.
        dirnames.sort()
        for fn in sorted(filenames):
            full = os.path.join(dirpath, fn)
            rel = os.path.relpath(full, stage).replace(os.sep, "/")
            info = zipfile.ZipInfo(rel, date_time=EPOCH)
            info.compress_type = zipfile.ZIP_DEFLATED
            # The mode lives in the high 16 bits of external_attr. Write it
            # once: setting the field and then OR-ing into it produced two
            # disagreeing writes in an earlier version.
            perms = 0o755 if is_executable(rel) else 0o644
            info.external_attr = (0o100000 | perms) << 16
            with open(full, "rb") as fh, z.open(info, "w") as dest:
                dest.write(fh.read())
print(f"build-mcpb: wrote {out}")
PY

size=$(du -h "$out" | cut -f1)
echo "build-mcpb: bundle size $size" >&2

# The MCP Registry requires fileSha256 in server.json; print it so the
# release step can paste it in, and offer to write it in place so a
# release does not depend on a human copying 64 hex digits correctly.
sha=$(python -c "
import hashlib,sys
print(hashlib.sha256(open(sys.argv[1],'rb').read()).hexdigest())
" "$out")

# WRITE_SERVER_JSON alone decides this. An earlier version also required
# $1 != "--use-dist", but release.sh passes --use-dist in both its dry-run
# and its real path (it distinguishes them with WRITE_SERVER_JSON), so that
# clause made the rewrite unreachable and every release would have shipped a
# server.json whose fileSha256 named the previous bundle.
#
# The version and the download URL are rewritten together with the hash.
# They used to be bumped by hand in a separate commit, which is a step a
# release can silently skip: v0.1.2 shipped a bundle whose manifest still
# said 0.1.1 and whose registry record still pointed at the v0.1.1 download,
# so the new release was unreachable through the registry. The manifest was
# already stamped above, before it was copied into the bundle.
if [ "${WRITE_SERVER_JSON:-0}" = "1" ]; then
    python - "$sha" "$version" <<'PY'
import re, sys

sha, version = sys.argv[1], sys.argv[2]

path = "mcpb/server.json"
with open(path, encoding="utf-8", newline="") as fh:
    text = fh.read()
# Rewrite just the hash field, so comments, key order and formatting that a
# human chose survive.
updated, n = re.subn(r'("fileSha256":\s*")[0-9a-f]{64}(")', rf'\g<1>{sha}\g<2>', text)
if n != 1:
    sys.exit(f"expected exactly one fileSha256 field in {path}, found {n}")
text = updated
# The registry record must name this version and this bundle, or a client
# that installs it downloads the previous release.
updated, n = re.subn(r'("version":\s*")[^"]+(")', rf'\g<1>{version}\g<2>', text, count=1)
if n != 1:
    sys.exit(f"expected exactly one version field in {path}, found {n}")
text = updated
updated, n = re.subn(
    r'(releases/download/)v[0-9][^"/]*(/jms-client\.mcpb)',
    rf'\g<1>v{version}\g<2>', text)
if n != 1:
    sys.exit(f"expected exactly one bundle URL in {path}, found {n}")
# newline="" keeps the file's existing line endings: Python's default text
# mode would rewrite every \n as \r\n on Windows, which is how an earlier
# version of this script turned the file CRLF and made it inconsistent with
# the rest of the tree.
with open(path, "w", encoding="utf-8", newline="") as fh:
    fh.write(updated)
print(f"build-mcpb: stamped version {version} and the new hash into mcpb/manifest.json and mcpb/server.json", file=sys.stderr)
PY
fi

echo "$sha"
