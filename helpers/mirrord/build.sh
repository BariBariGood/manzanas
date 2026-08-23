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
  -framework AppKit -framework CoreGraphics -framework ScreenCaptureKit \
  -framework Vision -framework ImageIO \
  -o "$OUT" \
  "$SRC_DIR"/Sources/*.swift

# Sign with a stable identity + identifier so TCC grants survive rebuilds:
# macOS keys grants to the code-signing designated requirement. An ad-hoc
# signature keys to this exact build's cdhash, losing the grants on every
# swap. MIRRORD_SIGN_IDENTITY overrides; "-" forces ad-hoc.
IDENTIFIER=com.baribarigood.manzanasd.mirrord
IDENTITY="${MIRRORD_SIGN_IDENTITY:-}"
if [ -z "$IDENTITY" ]; then
  IDS="$(security find-identity -v -p codesigning 2>/dev/null || true)"
  for PREFIX in "Developer ID Application:" "Apple Development:" "Apple Distribution:"; do
    IDENTITY="$(printf '%s\n' "$IDS" | sed -n "s/.*\"\(${PREFIX}[^\"]*\)\".*/\1/p" | head -n1)"
    [ -n "$IDENTITY" ] && break
  done
fi
if [ -z "$IDENTITY" ]; then
  echo "error: no stable codesign identity found — refusing to sign ad-hoc." >&2
  echo "An ad-hoc signature keys TCC grants to this build's cdhash, so every" >&2
  echo "rebuild loses them. Unlock your signing keychain (e.g." >&2
  echo "  security unlock-keychain dev-sign.keychain" >&2
  echo ") or set MIRRORD_SIGN_IDENTITY to an identity." >&2
  echo "To sign ad-hoc anyway, set MIRRORD_SIGN_IDENTITY=- explicitly." >&2
  exit 1
fi
if [ "$IDENTITY" = "-" ]; then
  echo "WARNING: signing ad-hoc (MIRRORD_SIGN_IDENTITY=-); TCC grants will NOT survive the next rebuild." >&2
fi
codesign -f -s "$IDENTITY" --identifier "$IDENTIFIER" "$OUT"

echo "built $OUT (signed: $IDENTITY, identifier $IDENTIFIER)"
