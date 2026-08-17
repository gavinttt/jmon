#!/bin/bash
set -e
DIR="$(cd "$(dirname "$0")" && pwd)"
chmod +x "$DIR/jmon"
echo "Installing jmon to /usr/local/bin..."
if [ -w /usr/local/bin ]; then
  cp "$DIR/jmon" /usr/local/bin/jmon
else
  sudo cp "$DIR/jmon" /usr/local/bin/jmon
fi
echo "Done! Run 'jmon' to start."
