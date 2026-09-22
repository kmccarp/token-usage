#!/usr/bin/env bash
set -euo pipefail
LABEL="com.kmccarp.token-usage"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
rm -f "$PLIST"
echo "removed $LABEL (binary in ~/bin and data in ~/.local/share/token-usage left in place)"
