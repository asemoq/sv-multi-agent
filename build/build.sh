#!/usr/bin/env bash
# Cross-build the SV Multi-Agent CLI (svma) for a chosen operating system.
#
# The sv-multi-agent service and all agents run in-process; there is no Python runtime.
#
# Usage:
#   ./build.sh                       # interactive menu
#   ./build.sh -t linux/amd64        # one target
#   ./build.sh -t all                # all targets
#   ./build.sh -t all --all          # svma + sv-multi-agent + svctl
#   ./build.sh --list                # list targets
#
# Env: GO (go executable), VERSION, OUT_DIR

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
OUT_DIR="${OUT_DIR:-$REPO_ROOT/dist}"
GO_BIN="${GO:-go}"
VERSION="${VERSION:-}"

targets=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64)

usage() {
    sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'
}

TARGET=""
BUILD_ALL=0

while [ $# -gt 0 ]; do
    case "$1" in
        -t|--target)  TARGET="${2:-}"; shift 2 ;;
        -a|--all)     BUILD_ALL=1; shift ;;
        -o|--out)     OUT_DIR="${2:-}"; shift 2 ;;
        -v|--version) VERSION="${2:-}"; shift 2 ;;
        -l|--list)    printf '%s\n' "${targets[@]}"; exit 0 ;;
        -h|--help)    usage; exit 0 ;;
        *) echo "Unknown option: $1" >&2; usage; exit 2 ;;
    esac
done

if ! command -v "$GO_BIN" >/dev/null 2>&1; then
    echo "Go was not found. Install it (https://go.dev/dl/) or set GO=<path-to-go>." >&2
    exit 1
fi

if [ -z "$VERSION" ]; then
    VERSION="dev"
    if command -v git >/dev/null 2>&1; then
        VERSION="$(git -C "$REPO_ROOT" describe --tags --always 2>/dev/null || echo dev)"
    fi
fi

if [ -z "$TARGET" ]; then
    echo ""
    echo "SV Multi-Agent - cross-platform build"
    echo "-------------------------------------"
    PS3="Select target: "
    select choice in "${targets[@]}" "all" "quit"; do
        case "$choice" in
            quit|"") echo "Cancelled."; exit 0 ;;
            all) TARGET="all"; break ;;
            *) TARGET="$choice"; break ;;
        esac
    done
fi

selected=()
if [ "$TARGET" = "all" ]; then
    selected=("${targets[@]}")
else
    selected=("$TARGET")
fi

mkdir -p "$OUT_DIR"
pushd "$REPO_ROOT" >/dev/null
trap 'popd >/dev/null 2>&1 || true' EXIT

for t in "${selected[@]}"; do
    os="${t%%/*}"
    arch="${t##*/}"
    ext=""
    [ "$os" = "windows" ] && ext=".exe"

    echo "Building svma-$os-$arch$ext ..."
    GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 "$GO_BIN" build -trimpath \
        -ldflags "-s -w -X main.version=$VERSION" \
        -o "$OUT_DIR/svma-$os-$arch$ext" ./cmd/svma

    if [ "$BUILD_ALL" -eq 1 ]; then
        GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 "$GO_BIN" build -trimpath -ldflags "-s -w" \
            -o "$OUT_DIR/sv-multi-agent-$os-$arch$ext" ./cmd/sv-multi-agent
        GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 "$GO_BIN" build -trimpath -ldflags "-s -w" \
            -o "$OUT_DIR/svctl-$os-$arch$ext" ./cmd/svctl
    fi
done

echo ""
echo "Done (version $VERSION)."
echo "Binaries in: $OUT_DIR"
ls -lh "$OUT_DIR"
