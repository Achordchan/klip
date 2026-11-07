#!/bin/bash
# Builds Klip.app for macOS and its zip, Klip-<version>-mac-<arm64|x64>.zip.
# Signed ad hoc only (no Developer ID): macOS asks the user to allow it once
# (System Settings > Privacy & Security > Open Anyway). No auto-update on macOS:
# the update window points at the zip of the newest release.
#
# usage: tools/package_mac.sh <version> [arm64|amd64] [out-dir]
#   arch defaults to this machine's, out-dir to dist/.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VER="${1:?usage: tools/package_mac.sh <version> [arm64|amd64] [out-dir]}"
ARCH="${2:-$(uname -m | sed 's/x86_64/amd64/')}"
case "$ARCH" in
  arm64) SUFFIX=arm64 ;;
  amd64) SUFFIX=x64 ;;
  *) echo "arch must be arm64 or amd64, not $ARCH" >&2; exit 2 ;;
esac
OUT="${3:-$ROOT/dist}"
mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"
# Each arch in a folder of its own: two builds into one out-dir do not meet.
STAGE="$OUT/Klip-$VER-mac-$SUFFIX"
APP="$STAGE/Klip.app"
rm -rf "$STAGE"; mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

(cd "$ROOT/app" && CGO_ENABLED=0 GOOS=darwin GOARCH="$ARCH" go build -trimpath \
  -ldflags "-s -w -X klip/internal/version.Version=$VER" -o "$APP/Contents/MacOS/Klip" ./cmd/klip)

# SQLite library MyGo's plugin loads; found next to the executable.
LIB="$(cd "$ROOT/app" && go list -m -f '{{.Dir}}' github.com/egoist/mygo)/plugins/sqlite/mygo-plugin.json"
read -r NAME URL SHA < <(python3 - "$LIB" "$ARCH" <<'PY'
import json,sys
m=json.load(open(sys.argv[1])); k="darwin-"+sys.argv[2]
for l in m["libraries"]:
    if l["name"]=="mygo-sqlite3":
        f=l["files"][k]; print(f["name"],f["url"],f["sha256"])
PY
)
CACHE="$HOME/Library/Caches/mygo/natives/$SHA/$NAME"
[ -f "$CACHE" ] || { mkdir -p "$(dirname "$CACHE")"; curl -fsSL "$URL" -o "$CACHE"; }
[ "$(shasum -a 256 "$CACHE" | cut -d' ' -f1)" = "$SHA" ] || { echo "sha256 mismatch for $NAME"; exit 1; }
cp "$CACHE" "$APP/Contents/MacOS/$NAME"

# Icon.
IS="$(mktemp -d)/Klip.iconset"; mkdir -p "$IS"
for s in 16 32 128 256 512; do
  sips -z $s $s "$ROOT/app/cmd/klip/icon.png" --out "$IS/icon_${s}x${s}.png" >/dev/null
  sips -z $((s*2)) $((s*2)) "$ROOT/app/cmd/klip/icon.png" --out "$IS/icon_${s}x${s}@2x.png" >/dev/null
done
iconutil -c icns "$IS" -o "$APP/Contents/Resources/Klip.icns"

cat > "$APP/Contents/Info.plist" <<PL
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleName</key><string>Klip</string>
<key>CFBundleDisplayName</key><string>Klip</string>
<key>CFBundleIdentifier</key><string>com.achordchan.klip</string>
<key>CFBundleExecutable</key><string>Klip</string>
<key>CFBundleIconFile</key><string>Klip</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>$VER</string>
<key>CFBundleVersion</key><string>$VER</string>
<key>LSMinimumSystemVersion</key><string>12.0</string>
<key>LSUIElement</key><true/>
<key>NSHighResolutionCapable</key><true/>
</dict></plist>
PL

# An ad hoc signature over the whole bundle: an Apple silicon Mac runs no
# unsigned code, and the library must be sealed in with the program.
codesign --force --deep --sign - "$APP"
codesign --verify --deep --strict "$APP"
ZIP="$OUT/Klip-$VER-mac-$SUFFIX.zip"
rm -f "$ZIP"
(cd "$STAGE" && ditto -c -k --keepParent Klip.app "$ZIP")
echo "$ZIP"
