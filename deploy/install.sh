#!/bin/bash
# Install manzanasd as a per-user launchd agent on macOS.
#
# Usage:
#   ./install.sh [--binary PATH] [--port PORT]
#
#   --binary PATH  path to the manzanasd binary to install
#                  (default: ./manzanasd next to this script, then ../bin/manzanasd)
#   --port PORT    port the daemon listens on (default: 7433)
#
# Installs the binary to ~/bin/manzanasd, writes the LaunchAgent plist,
# loads it, sets up log rotation into ~/.manzanasd/logs/, and health-checks
# GET /v0/healthz on the chosen port.
set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "install.sh: launchd install only works on macOS" >&2
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LABEL="com.baribarigood.manzanasd"
PORT=7433
BINARY=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --binary) [[ $# -ge 2 ]] || { echo "--binary requires a PATH" >&2; exit 2; }; BINARY="$2"; shift 2 ;;
    --port)   [[ $# -ge 2 ]] || { echo "--port requires a PORT" >&2; exit 2; }; PORT="$2"; shift 2 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

if [[ -z "$BINARY" ]]; then
  for cand in "$SCRIPT_DIR/manzanasd" "$SCRIPT_DIR/../bin/manzanasd" "$SCRIPT_DIR/../manzanasd"; do
    if [[ -x "$cand" ]]; then BINARY="$cand"; break; fi
  done
fi
if [[ -z "$BINARY" || ! -x "$BINARY" ]]; then
  echo "install.sh: manzanasd binary not found; pass --binary PATH" >&2
  exit 1
fi

PLIST_TEMPLATE="$SCRIPT_DIR/$LABEL.plist"
if [[ ! -f "$PLIST_TEMPLATE" ]]; then
  echo "install.sh: plist template not found at $PLIST_TEMPLATE" >&2
  exit 1
fi

AGENT_DIR="$HOME/Library/LaunchAgents"
PLIST_DST="$AGENT_DIR/$LABEL.plist"
LOG_DIR="$HOME/.manzanasd/logs"
GUI_TARGET="gui/$(id -u)"

echo "==> installing binary to ~/bin/manzanasd"
mkdir -p "$HOME/bin" "$LOG_DIR" "$AGENT_DIR"
install -m 0755 "$BINARY" "$HOME/bin/manzanasd"

echo "==> writing LaunchAgent $PLIST_DST (port $PORT)"
sed -e "s|__HOME__|$HOME|g" -e "s|__PORT__|$PORT|g" "$PLIST_TEMPLATE" > "$PLIST_DST"

# Log rotation: a daily per-user LaunchAgent copy-truncates logs >10MB in
# place. Truncation (unlike newsyslog's rename) is safe while launchd holds
# the log fds — the daemon opens them O_APPEND, so writes continue into the
# truncated file. No sudo required.
ROTATE_LABEL="$LABEL.logrotate"
ROTATE_PLIST="$AGENT_DIR/$ROTATE_LABEL.plist"
ROTATE_SCRIPT="$HOME/.manzanasd/rotate-logs.sh"
echo "==> installing daily log rotation LaunchAgent ($ROTATE_LABEL)"
cat > "$ROTATE_SCRIPT" <<ROTEOF
#!/bin/bash
# Copy-truncate manzanasd logs >10MB, keeping one .0.gz generation each.
set -u
for f in "$LOG_DIR"/manzanasd.out.log "$LOG_DIR"/manzanasd.err.log; do
  [ -f "\$f" ] || continue
  if [ "\$(stat -f %z "\$f")" -gt \$((10 * 1024 * 1024)) ]; then
    cp "\$f" "\$f.0" && : > "\$f" && gzip -f "\$f.0"
  fi
done
ROTEOF
chmod 0755 "$ROTATE_SCRIPT"
cat > "$ROTATE_PLIST" <<PLEOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$ROTATE_LABEL</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/bash</string>
		<string>$ROTATE_SCRIPT</string>
	</array>
	<key>StartCalendarInterval</key>
	<dict>
		<key>Hour</key>
		<integer>3</integer>
		<key>Minute</key>
		<integer>0</integer>
	</dict>
</dict>
</plist>
PLEOF
launchctl bootout "$GUI_TARGET/$ROTATE_LABEL" 2>/dev/null || true
launchctl bootstrap "$GUI_TARGET" "$ROTATE_PLIST" \
  || echo "install.sh: warning: could not load $ROTATE_LABEL (log rotation disabled)" >&2

echo "==> (re)loading LaunchAgent"
launchctl bootout "$GUI_TARGET/$LABEL" 2>/dev/null || true
# bootout returns before the job is fully gone; bootstrap can transiently
# fail with "5: Input/output error". Retry a few times.
for _ in 1 2 3 4 5; do
  if launchctl bootstrap "$GUI_TARGET" "$PLIST_DST" 2>/dev/null; then break; fi
  sleep 1
done
launchctl print "$GUI_TARGET/$LABEL" >/dev/null 2>&1 || {
  echo "install.sh: could not load $LABEL" >&2; exit 1; }
launchctl kickstart "$GUI_TARGET/$LABEL" || true

echo "==> health-checking http://localhost:$PORT/v0/healthz"
for i in $(seq 1 30); do
  if curl -fsS "http://localhost:$PORT/v0/healthz" >/dev/null 2>&1; then
    echo "==> manzanasd is healthy: $(curl -fsS "http://localhost:$PORT/v0/healthz")"
    echo "==> installed. logs: $LOG_DIR"
    case ":$PATH:" in
      *":$HOME/bin:"*) ;;
      *) echo "==> note: ~/bin is not on your PATH; run ~/bin/manzanasd directly or add: export PATH=\"\$HOME/bin:\$PATH\"" ;;
    esac
    exit 0
  fi
  sleep 1
done

echo "install.sh: daemon did not become healthy after 30s; check $LOG_DIR/manzanasd.err.log" >&2
exit 1
