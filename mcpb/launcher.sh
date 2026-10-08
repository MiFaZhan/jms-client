#!/bin/sh
# Launcher for the jms MCP bundle on macOS and Linux.
#
# MCPB's platform_overrides can distinguish operating systems but not CPU
# architectures (the v0.3 and v0.4 manifest schemas have no architecture
# field), so the bundle ships one binary per architecture and this script
# picks the right one at run time.
#
# It must exec, not fork: the MCP host talks JSON-RPC over this process's
# stdin/stdout and watches its exit status, so an intermediate shell that
# stayed alive would break both.

set -eu

# Resolve this script's directory, following symlinks, so the bundle works
# however the host invokes it. POSIX sh has no readlink -f on macOS, hence
# the manual loop.
script_path=$0
while [ -L "$script_path" ]; do
    link_target=$(readlink "$script_path")
    case $link_target in
        /*) script_path=$link_target ;;
        *) script_path=$(dirname "$script_path")/$link_target ;;
    esac
done
here=$(cd "$(dirname "$script_path")" && pwd)

# uname -s / -m give the portable spelling on both macOS and Linux.
os=$(uname -s)
machine=$(uname -m)

case $os in
    Darwin) platform=darwin ;;
    Linux) platform=linux ;;
    *)
        echo "jms: unsupported operating system: $os" >&2
        exit 1
        ;;
esac

case $machine in
    x86_64|amd64) arch=x86_64 ;;
    arm64|aarch64) arch=aarch64 ;;
    *)
        echo "jms: unsupported CPU architecture: $machine" >&2
        exit 1
        ;;
esac

binary="$here/server/jms-$platform-$arch"
if [ ! -x "$binary" ]; then
    echo "jms: no bundled binary for $platform/$arch (looked for $binary)" >&2
    exit 1
fi

# A quarantined or unzipped bundle can lose the executable bit; the host
# unpacks the archive, so restore it rather than failing with EACCES.
[ -x "$binary" ] || chmod +x "$binary" 2>/dev/null || true

exec "$binary" "$@"
