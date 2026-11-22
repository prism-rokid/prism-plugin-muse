#!/bin/sh
set -eu
cd "$(dirname "$0")"
case "$(uname -s)" in Darwin) ;; *) echo 'Muse requires macOS' >&2; exit 1 ;; esac
output="$PWD/dist"
app="$output/Muse Web.app"
case "$(go env GOARCH)" in
    arm64) swift_arch=arm64 ;;
    amd64) swift_arch=x86_64 ;;
    *) echo 'Muse requires arm64 or amd64' >&2; exit 1 ;;
esac
mkdir -p "$app/Contents/MacOS"
cp pluginbridge-plugin.yaml "$output/"
cp webview/Info.plist "$app/Contents/"
CGO_ENABLED=0 go build -trimpath -o "$output/muse" .
xcrun swiftc -O -target "$swift_arch-apple-macos12.0" -framework AppKit -framework WebKit webview/main.swift -o "$app/Contents/MacOS/MuseWeb"
codesign --force --sign - "$app"
