#!/bin/bash
# Uninstall the manzanasd launchd agent installed by install.sh.
#
# Usage:
#   ./uninstall.sh [--purge]
#
#   --purge  also remove ~/.manzanasd (logs and state), not just the
#            LaunchAgent and binary.
set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "uninstall.sh: launchd uninstall only works on macOS" >&2
  exit 1
fi

LABEL="com.baribarigood.manzanasd"
PLIST_DST="$HOME/Library/LaunchAgents/$LABEL.plist"
GUI_TARGET="gui/$(id -u)"
PURGE=0
[[ "${1:-}" == "--purge" ]] && PURGE=1

echo "==> unloading LaunchAgents"
launchctl bootout "$GUI_TARGET/$LABEL" 2>/dev/null || true
launchctl bootout "$GUI_TARGET/$LABEL.logrotate" 2>/dev/null || true

echo "==> removing $PLIST_DST"
rm -f "$PLIST_DST" "$HOME/Library/LaunchAgents/$LABEL.logrotate.plist"
rm -f "$HOME/.manzanasd/rotate-logs.sh"

echo "==> removing ~/bin/manzanasd"
rm -f "$HOME/bin/manzanasd"

# Clean up the newsyslog config older installs may have written.
if [[ -f /etc/newsyslog.d/manzanasd.conf ]] && sudo -n true 2>/dev/null; then
  sudo rm -f /etc/newsyslog.d/manzanasd.conf
fi

if [[ $PURGE -eq 1 ]]; then
  # ~/.manzanasd holds logs/state for any manzanasd on this Mac; don't purge
  # while a daemon process is still running and using it. bootout returns
  # before the job exits, so give the one we just stopped a moment to go.
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    pgrep -u "$(id -u)" -x manzanasd >/dev/null 2>&1 || break
    sleep 1
  done
  if pgrep -u "$(id -u)" -x manzanasd >/dev/null 2>&1; then
    echo "uninstall.sh: a manzanasd process is still running; NOT purging ~/.manzanasd" >&2
    echo "uninstall.sh: stop it first, then re-run ./uninstall.sh --purge" >&2
    exit 1
  fi
  echo "==> purging ~/.manzanasd"
  rm -rf "$HOME/.manzanasd"
fi

echo "==> uninstalled"
