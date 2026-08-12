#!/bin/bash
# Installs mirrord as a user LaunchAgent (GUI session). Run ON the Mac,
# as the logged-in user (not root, not over sudo).
#
#   ./install.sh [binary-path]    # default: builds ./mirrord first
#
# After install, grant TCC permissions to the *mirrord binary* in
# System Settings > Privacy & Security:
#   - Accessibility            (taps & keystrokes)
#   - Screen & System Audio Recording   (window capture)
# then verify the whole chain: ~/bin/mirrord --doctor
#
# Uninstall cleanly with ./uninstall.sh.
set -euo pipefail

SRC_DIR="$(cd "$(dirname "$0")" && pwd)"
LABEL=com.baribarigood.manzanasd.mirrord
BIN="${1:-}"

if [ -z "$BIN" ]; then
  # Build into a private mktemp dir, never a fixed world-writable path:
  # this binary receives Accessibility + Screen Recording grants.
  BUILD_DIR="$(mktemp -d)"
  trap 'rm -rf "$BUILD_DIR"' EXIT
  "$SRC_DIR/build.sh" "$BUILD_DIR/mirrord"
  BIN="$BUILD_DIR/mirrord"
fi

mkdir -p "$HOME/bin" "$HOME/.manzanasd" "$HOME/Library/Logs"
# Install to a stable path: TCC grants attach to the binary's path+signature,
# so rebuilding in place at ~/bin/mirrord keeps them.
install -m 0755 "$BIN" "$HOME/bin/mirrord"

PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
mkdir -p "$HOME/Library/LaunchAgents"
sed -e "s|__MIRRORD__|$HOME/bin/mirrord|" -e "s|__LOGDIR__|$HOME/Library/Logs|" \
  "$SRC_DIR/$LABEL.plist" > "$PLIST"

launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST"

echo "mirrord installed: $HOME/bin/mirrord, LaunchAgent $LABEL loaded"
echo "next: grant Accessibility + Screen Recording to $HOME/bin/mirrord in"
echo "System Settings > Privacy & Security, then run: $HOME/bin/mirrord --doctor"
