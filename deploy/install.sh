#!/usr/bin/env bash
# Installs token-usage as a per-user launchd agent that starts at login,
# listens on the Tailscale IP + localhost, and restarts if it dies.
set -euo pipefail

cd "$(dirname "$0")/.."
LABEL="com.kmccarp.token-usage"
BIN_DIR="${BIN_DIR:-$HOME/bin}"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOG_DIR="$HOME/Library/Logs"
PORT="${PORT:-8787}"

mkdir -p "$BIN_DIR" "$HOME/Library/LaunchAgents" "$LOG_DIR"
if [ -x ./token-usage ]; then
  cp ./token-usage "$BIN_DIR/token-usage.new"
else
  go build -ldflags "-s -w -X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)" -o "$BIN_DIR/token-usage.new" ./cmd/token-usage
fi
mv -f "$BIN_DIR/token-usage.new" "$BIN_DIR/token-usage"

cat > "$PLIST" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$BIN_DIR/token-usage</string>
    <string>-listen</string><string>tailscale,localhost</string>
    <string>-port</string><string>$PORT</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/Applications/Tailscale.app/Contents/MacOS</string>
    <key>HOME</key><string>$HOME</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>StandardOutPath</key><string>$LOG_DIR/token-usage.log</string>
  <key>StandardErrorPath</key><string>$LOG_DIR/token-usage.log</string>
</dict>
</plist>
PLIST

launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST"
launchctl kickstart -k "gui/$(id -u)/$LABEL"
sleep 2
echo "installed $LABEL; log: $LOG_DIR/token-usage.log"
tail -n 5 "$LOG_DIR/token-usage.log" || true
