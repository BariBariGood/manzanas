#!/bin/bash
# Builds the mirrord iPhone Mirroring helper on a Mac.
#
# mirrord uses only system frameworks (AppKit, CoreGraphics, Vision) —
# no third-party dependencies, so a bare swiftc invocation is enough.
# CI cannot build it (needs macOS); build it on the Mac that will run it.
#
# Usage:
#   ./build.sh [output-path]      # default: ./mirrord
set -euo pipefail

OUT="${1:-./mirrord}"
SRC_DIR="$(cd "$(dirname "$0")" && pwd)"

swiftc -O \
  -framework AppKit -framework CoreGraphics -framework Vision -framework ImageIO \
  -o "$OUT" \
  "$SRC_DIR"/Sources/*.swift

echo "built $OUT"
