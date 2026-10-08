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

version=$(git describe --tags --abbrev=0 2>/dev/null | sed 's/^v//' || true)
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


with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
    for dirpath, _, filenames in os.walk(stage):
        for fn in sorted(filenames):
            full = os.path.join(dirpath, fn)
            rel = os.path.relpath(full, stage).replace(os.sep, "/")
            info = zipfile.ZipInfo.from_file(full, rel)
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

if [ "${1:-}" != "--use-dist" ] && [ "${WRITE_SERVER_JSON:-0}" = "1" ]; then
    python - "$sha" <<'PY'
import json, re, sys

sha = sys.argv[1]
path = "mcpb/server.json"
with open(path, encoding="utf-8") as fh:
    text = fh.read()
# Rewrite just the hash field, so comments, key order and formatting that a
# human chose survive.
updated, n = re.subn(r'("fileSha256":\s*")[0-9a-f]{64}(")', rf'\g<1>{sha}\g<2>', text)
if n != 1:
    sys.exit(f"expected exactly one fileSha256 field in {path}, found {n}")
with open(path, "w", encoding="utf-8") as fh:
    fh.write(updated)
print(f"build-mcpb: updated {path} with the new hash", file=sys.stderr)
PY
fi

echo "$sha"
