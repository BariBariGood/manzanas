#!/bin/bash
# Rebuilds the FBSimulatorControl frameworks with the LOCAL Swift toolchain
# so simbridge can compile on hosts whose compiler does not match the one
# that produced the prebuilt AXe frameworks.
#
# Background: prebuilt AXe releases embed compiled `.swiftmodule`s tied to
# the exact Swift compiler that built them. With any other compiler, swiftc
# falls back to the shipped `.swiftinterface`, which fails to parse (the
# FBSimulatorControl class shadows its own module name, so qualified
# references like FBSimulatorControl.FBSimulatorContentSizeCategory resolve
# against the class). This bit the Intel fleet boxes (Xcode 26.6 /
# Swift 6.3.3) against AXe v1.8.0 frameworks (Swift 6.3.2). Rebuilding the
# frameworks locally sidesteps both problems.
#
# Usage:
#   ./build-frameworks.sh [output-dir]
#     default output: ~/.cache/manzanasd/axe-frameworks-<swift-version>
#   then: AXE_FRAMEWORKS=<output-dir> ./build.sh ~/bin/simbridge
#
# Requires: Xcode, git, xcodegen (brew install xcodegen). Takes ~15 min.
set -euo pipefail

AXE_REF="${AXE_REF:-v1.8.0}"
SWIFT_VER="$( (swift --version 2>/dev/null || true) | sed -n 's/.*Swift version \([0-9.]*\).*/\1/p' | head -1 || true)"
OUT="${1:-$HOME/.cache/manzanasd/axe-frameworks-${SWIFT_VER:-unknown}}"
SRC="${AXE_SRC_DIR:-$HOME/.cache/manzanasd/axe-src}"

if ! command -v xcodegen >/dev/null 2>&1; then
  echo "error: xcodegen is required (brew install xcodegen)" >&2
  exit 1
fi

if [ ! -d "$SRC/.git" ]; then
  git clone -q https://github.com/cameroncooke/AXe "$SRC"
fi
git -C "$SRC" fetch -q --tags origin
git -C "$SRC" checkout -q "$AXE_REF"

(cd "$SRC" && ./scripts/build.sh setup && ./scripts/build.sh frameworks)

PRODUCTS="$SRC/build_derived_data/Build/Products/Release"
if [ ! -d "$PRODUCTS/FBSimulatorControl.framework" ]; then
  echo "error: frameworks build did not produce $PRODUCTS/FBSimulatorControl.framework" >&2
  exit 1
fi

mkdir -p "$OUT"
for fw in FBControlCore FBSimulatorControl FBDeviceControl XCTestBootstrap; do
  if [ -d "$PRODUCTS/$fw.framework" ]; then
    rm -rf "$OUT/$fw.framework"
    cp -R "$PRODUCTS/$fw.framework" "$OUT/"
  fi
done

echo "frameworks installed to $OUT"
echo "next: AXE_FRAMEWORKS=$OUT $(dirname "$0")/build.sh ~/bin/simbridge"
