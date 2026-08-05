#!/bin/bash
# Builds the simbridge warm-action helper on a Mac.
#
# simbridge links against the FBSimulatorControl frameworks that ship with
# an AXe install (github.com/cameroncooke/AXe, MIT). Point AXE_FRAMEWORKS
# at the Frameworks directory of an AXe release (default: ~/axe/Frameworks,
# the fleet-standard install location).
#
# Usage:
#   ./build.sh [output-path]      # default: ./simbridge
#   AXE_FRAMEWORKS=/path/to/Frameworks ./build.sh ~/bin/simbridge
#
# SIMBRIDGE_RPATH overrides the runtime search path baked into the binary
# (default: the AXE_FRAMEWORKS dir it was built against). Use an
# @executable_path-relative rpath to make one binary deployable across
# hosts with different home directories.
#
# If the local Swift compiler does not match the one that produced the
# prebuilt frameworks, the build fails (swiftinterface fallback cannot
# parse). Rebuild toolchain-matched frameworks with ./build-frameworks.sh
# and point AXE_FRAMEWORKS at its output.
#
# Prebuilt binaries: CI cannot build this (needs macOS + Xcode + the AXe
# frameworks), so release binaries are built on a fleet Mac with this
# script and attached to GitHub releases as simbridge-darwin-{amd64,arm64}.
set -euo pipefail

FRAMEWORKS="${AXE_FRAMEWORKS:-$HOME/axe/Frameworks}"
RPATH="${SIMBRIDGE_RPATH:-$FRAMEWORKS}"
OUT="${1:-./simbridge}"
SRC_DIR="$(cd "$(dirname "$0")" && pwd)"

if [ ! -d "$FRAMEWORKS/FBSimulatorControl.framework" ]; then
  echo "error: FBSimulatorControl.framework not found in $FRAMEWORKS" >&2
  echo "install AXe (brew install cameroncooke/axe/axe or a GitHub release)" >&2
  echo "and/or set AXE_FRAMEWORKS to its Frameworks directory" >&2
  exit 1
fi

# The frameworks' Swift interfaces import private CoreSimulator/SimulatorKit
# modules; their compile-only module maps come from facebook/idb's
# PrivateHeaders (MIT), exactly as AXe's own build does.
# Cached outside the repo so builds never dirty the working tree; pinned
# so the headers are reproducible across machines.
IDB_REF="${IDB_REF:-e6c6d5dd9afc532718a8cdd1a31f05fffb517757}"
IDB_CHECKOUT="${IDB_CHECKOUT_DIR:-$HOME/.cache/manzanasd/idb-${IDB_REF:0:12}}"
if [ ! -d "$IDB_CHECKOUT/PrivateHeaders" ]; then
  echo "fetching facebook/idb@$IDB_REF PrivateHeaders into $IDB_CHECKOUT ..."
  git init -q "$IDB_CHECKOUT"
  git -C "$IDB_CHECKOUT" fetch -q --depth 1 https://github.com/facebook/idb "$IDB_REF"
  git -C "$IDB_CHECKOUT" checkout -q FETCH_HEAD
fi

PH="$IDB_CHECKOUT/PrivateHeaders"
HEADER_FLAGS=()
for d in "$PH" "$PH/AccessibilityPlatformTranslation" "$PH/AXRuntime" \
         "$PH/CoreSimDeviceIO" "$PH/CoreSimulator" "$PH/CoreSimulatorUtilities" \
         "$PH/SimulatorKit"; do
  HEADER_FLAGS+=("-I$d")
done

swiftc -O -parse-as-library \
  -F "$FRAMEWORKS" \
  "${HEADER_FLAGS[@]}" \
  -framework FBControlCore \
  -framework FBSimulatorControl \
  -Xlinker -rpath -Xlinker "$RPATH" \
  -o "$OUT" \
  "$SRC_DIR/Simbridge.swift"

echo "built $OUT (frameworks: $FRAMEWORKS, rpath: $RPATH)"
