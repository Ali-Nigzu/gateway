#!/bin/sh
set -eu
umask 077

target=${1:-}
case "$target" in
  darwin-arm64) expected_os=Darwin; goos=darwin; goarch=arm64; cgo=1 ;;
  darwin-amd64) expected_os=Darwin; goos=darwin; goarch=amd64; cgo=1 ;;
  linux-amd64) expected_os=Linux; goos=linux; goarch=amd64; cgo=0 ;;
  *) echo "usage: ./build.sh {darwin-arm64|darwin-amd64|linux-amd64}" >&2; exit 2 ;;
esac

root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
expected_version=1.0
test "$(uname -s)" = "$expected_os" || { echo "$target requires a native $expected_os build host" >&2; exit 1; }
test "$(go env GOHOSTOS)" = "$goos" && test "$(go env GOHOSTARCH)" = "$goarch" || {
  echo "$target requires a native $goos/$goarch Go environment" >&2
  exit 1
}
test "$(go env GOVERSION)" = "go1.25.12" || {
  echo "production releases require the pinned Go 1.25.12 toolchain" >&2
  exit 1
}
test -s "$root/bootstrap_sa.json" || { echo "Missing gateway/bootstrap_sa.json" >&2; exit 1; }
test -s "$root/ffmpeg" || { echo "Missing gateway/ffmpeg" >&2; exit 1; }
description=$(file -b "$root/ffmpeg")
case "$target:$description" in
  darwin-arm64:*Mach-O*arm64*executable*) ;;
  darwin-amd64:*Mach-O*x86_64*executable*) ;;
  linux-amd64:*ELF*64-bit*LSB*executable*x86-64*) ;;
  *) echo "gateway/ffmpeg does not match $target" >&2; exit 1 ;;
esac

mkdir -p "$root/dist"
temporary="$root/dist/$target.building"
checksum_file="$root/dist/SHA256SUMS"
checksum_temporary="$checksum_file.building"
trap 'rm -f "$temporary" "$checksum_temporary"' EXIT HUP INT TERM
rm -f "$temporary" "$checksum_temporary"
cd "$root"
CAMOS_VERIFY_BOOTSTRAP_CREDENTIAL="$root/bootstrap_sa.json" \
  go test -count=1 -run '^TestReleaseBootstrapInput$' .
go test -count=1 ./...
go vet ./...
CGO_ENABLED=$cgo GOOS=$goos GOARCH=$goarch go build -tags production -trimpath -o "$temporary" .
CAMOS_VERIFY_RELEASE_ARTIFACT="$temporary" \
CAMOS_VERIFY_RELEASE_TARGET="$target" \
CAMOS_VERIFY_RELEASE_VERSION="$expected_version" \
  go test -count=1 -run '^TestReleaseBuildArtifact$' .
mv -f "$temporary" "$root/dist/$target"

windows_hash=
darwin_arm_hash=
darwin_amd_hash=
linux_hash=
if test -f "$checksum_file"; then
  if grep -Ev '^[0-9a-fA-F]{64}  (windows-amd64\.exe|darwin-arm64|darwin-amd64|linux-amd64)$' "$checksum_file" | grep -q .; then
    echo "dist/SHA256SUMS contains a malformed or unknown entry" >&2
    exit 1
  fi
  while read -r checksum filename extra; do
    test -z "${extra:-}" || { echo "dist/SHA256SUMS contains a malformed entry" >&2; exit 1; }
    checksum=$(printf '%s' "$checksum" | tr 'A-F' 'a-f')
    case "$filename" in
      windows-amd64.exe) test -z "$windows_hash" || { echo "duplicate checksum for $filename" >&2; exit 1; }; windows_hash=$checksum ;;
      darwin-arm64) test -z "$darwin_arm_hash" || { echo "duplicate checksum for $filename" >&2; exit 1; }; darwin_arm_hash=$checksum ;;
      darwin-amd64) test -z "$darwin_amd_hash" || { echo "duplicate checksum for $filename" >&2; exit 1; }; darwin_amd_hash=$checksum ;;
      linux-amd64) test -z "$linux_hash" || { echo "duplicate checksum for $filename" >&2; exit 1; }; linux_hash=$checksum ;;
    esac
  done < "$checksum_file"
fi

artifact_hash() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

test ! -f "$root/dist/windows-amd64.exe" || windows_hash=$(artifact_hash "$root/dist/windows-amd64.exe")
test ! -f "$root/dist/darwin-arm64" || darwin_arm_hash=$(artifact_hash "$root/dist/darwin-arm64")
test ! -f "$root/dist/darwin-amd64" || darwin_amd_hash=$(artifact_hash "$root/dist/darwin-amd64")
test ! -f "$root/dist/linux-amd64" || linux_hash=$(artifact_hash "$root/dist/linux-amd64")

{
  test -z "$windows_hash" || printf '%s  %s\n' "$windows_hash" windows-amd64.exe
  test -z "$darwin_arm_hash" || printf '%s  %s\n' "$darwin_arm_hash" darwin-arm64
  test -z "$darwin_amd_hash" || printf '%s  %s\n' "$darwin_amd_hash" darwin-amd64
  test -z "$linux_hash" || printf '%s  %s\n' "$linux_hash" linux-amd64
} > "$checksum_temporary"
mv -f "$checksum_temporary" "$checksum_file"

echo "Built and statically verified dist/$target (BuildVersion 1.0)"
