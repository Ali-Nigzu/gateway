#!/bin/sh
set -eu

target=${1:-}
case "$target" in
  darwin-arm64) expected_os=Darwin; goos=darwin; goarch=arm64; cgo=1 ;;
  darwin-amd64) expected_os=Darwin; goos=darwin; goarch=amd64; cgo=1 ;;
  linux-amd64) expected_os=Linux; goos=linux; goarch=amd64; cgo=0 ;;
  *) echo "usage: ./build.sh {darwin-arm64|darwin-amd64|linux-amd64}" >&2; exit 2 ;;
esac

root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
test "$(uname -s)" = "$expected_os" || { echo "$target requires a native $expected_os build host" >&2; exit 1; }
test -s "$root/bootstrap_sa.json" || { echo "Missing gateway/bootstrap_sa.json" >&2; exit 1; }
test -s "$root/ffmpeg" || { echo "Missing gateway/ffmpeg" >&2; exit 1; }

description=$(file -b "$root/ffmpeg")
case "$target:$description" in
  darwin-arm64:*Mach-O*arm64*) ;;
  darwin-amd64:*Mach-O*x86_64*) ;;
  linux-amd64:*ELF*x86-64*) ;;
  *) echo "gateway/ffmpeg does not match $target" >&2; exit 1 ;;
esac

mkdir -p "$root/dist"
temporary="$root/dist/$target.building"
trap 'rm -f "$temporary"' EXIT HUP INT TERM
cd "$root"
go test -count=1 ./...
CGO_ENABLED=$cgo GOOS=$goos GOARCH=$goarch go build -tags production -trimpath -o "$temporary" .
mv -f "$temporary" "$root/dist/$target"

if command -v sha256sum >/dev/null 2>&1; then
  (cd "$root/dist" && sha256sum "$target") > "$root/dist/SHA256SUMS"
else
  (cd "$root/dist" && shasum -a 256 "$target") > "$root/dist/SHA256SUMS"
fi
echo "Built dist/$target (BuildVersion 1)"
