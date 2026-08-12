#!/bin/bash
# Cleanly removes the mirrord LaunchAgent, binary, and socket. TCC grants
# can then be removed in System Settings > Privacy & Security if desired.
set -euo pipefail

LABEL=com.baribarigood.manzanasd.mirrord
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"

launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
rm -f "$PLIST" "$HOME/bin/mirrord" "$HOME/.manzanasd/mirrord.sock" \
  "$HOME/Library/Logs/mirrord.err.log" "$HOME/Library/Logs/mirrord.out.log"

echo "mirrord uninstalled (LaunchAgent unloaded, binary and socket removed)"
