#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

APP_DIR="$ROOT_DIR/dist/RoomChorus.app"
rm -rf "$APP_DIR"
mkdir -p "$APP_DIR/Contents/MacOS" "$APP_DIR/Contents/Resources"

cp "$ROOT_DIR/Info.plist" "$APP_DIR/Contents/Info.plist"
go build -o "$APP_DIR/Contents/MacOS/roomchorus" ./cmd/roomchorus
codesign -s - --force --deep "$APP_DIR"

echo "Built $APP_DIR"
