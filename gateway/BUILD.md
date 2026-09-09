# camOS Gateway production release admission

camOS Gateway releases are built and admitted locally. GitHub Actions does not build or publish the appliance, and release verification never changes Postgres `desired_version`.

The release represented by this source tree has one canonical version:

```text
BuildVersion = 1.0
Artifact Registry package = gateway
Artifact Registry version = 1.0
Repository = projects/camosbase/locations/europe-west2/repositories/camos-gateway-prod
```

The CLI, static release stamp, provider version, `update.pending`, lifecycle diagnostics, and Postgres desired/reported values must all use that same string. Commit SHA and executable SHA-256 distinguish candidate builds of the same source version; no second release-version concept exists.

## Native builds

Use the exact Go 1.25.12 toolchain pinned by `go.mod`. Place the private `bootstrap_sa.json` and the matching native FFmpeg executable in this directory. Those inputs and `dist/` are ignored by Git. Never commit or print credential contents.

Build each target in a native environment:

| Release file | Required build host | Command |
| --- | --- | --- |
| `windows-amd64.exe` | Windows amd64 | `.\build.ps1` |
| `darwin-arm64` | macOS arm64 | `./build.sh darwin-arm64` |
| `darwin-amd64` | macOS amd64 | `./build.sh darwin-amd64` |
| `linux-amd64` | Linux amd64 | `./build.sh linux-amd64` |

Darwin requires CGo and the Apple IOKit/CoreFoundation frameworks. Replace `ffmpeg` with the exact native target payload before each Darwin or Linux build.

Each script:

1. rejects a non-native OS/architecture or wrong Go toolchain;
2. validates the bootstrap identity and embedded FFmpeg family/architecture;
3. runs the test suite;
4. runs `go vet ./...` as a mandatory release gate;
5. builds with `production` tags and `-trimpath`;
6. invokes the same static candidate inspector used by the updater, without executing the output;
7. verifies exact module, BuildVersion, target, executable family, Go build information, GOOS/GOARCH, and release stamp;
8. publishes the artifact only after verification; and
9. updates `dist/SHA256SUMS` without erasing valid entries for other targets.

The checksum manifest has one canonical lowercase SHA-256 entry per target in the order shown above. A build script rejects malformed, duplicate, or unknown existing entries and re-hashes every supported artifact present in `dist/`. Carry the existing admitted artifacts and manifest into the next native build host so the fourth build leaves one aggregate four-target manifest.

No `.building` file is a release artifact. A failed build or static inspection must not replace the admitted target file.

## First hardened-release boundary

Pre-hardening pilot executables do not contain the complete 1.0 static identity and one-slot recovery protocol. Do not claim that a self-update from those binaries exercised the hardened path. Establish 1.0 with a controlled manual/native installation or service replacement, preserve the existing Gateway identity directory, start the native service, and prove identity continuity plus a successful authoritative control refresh.

Only transitions originating from a confirmed hardened Gateway are covered by the new update/downgrade guarantees. This deliberate bootstrap boundary avoids depending on the updater being replaced to prove its own hardening.

## Assemble and verify the local release set

The final release directory must contain exactly:

```text
SHA256SUMS
windows-amd64.exe
darwin-arm64
darwin-amd64
linux-amd64
```

Run the read-only local admission test from `gateway/` after all four native results have been assembled.

PowerShell:

```powershell
$env:CAMOS_LOCAL_RELEASE_VERIFY = "1"
$env:CAMOS_GATEWAY_RELEASE_DIR = (Resolve-Path .\dist).Path
go test -count=1 -run '^TestLocalReleaseSet$' .
```

POSIX shell:

```sh
CAMOS_LOCAL_RELEASE_VERIFY=1 \
CAMOS_GATEWAY_RELEASE_DIR="$(pwd)/dist" \
go test -count=1 -run '^TestLocalReleaseSet$' .
```

This verifies every local SHA against `SHA256SUMS` and statically inspects all four binaries. It does not execute any candidate.

## Publish one immutable Generic Artifact Registry version

Upload the clean five-file release directory in one release operation. Generic Artifact Registry versions are immutable, so do not create version `1.0` until the complete local set has passed admission.

```sh
gcloud artifacts generic upload \
  --project=camosbase \
  --location=europe-west2 \
  --repository=camos-gateway-prod \
  --package=gateway \
  --version=1.0 \
  --source-directory=dist
```

Google's canonical file resource IDs for the executables must be:

```text
gateway:1.0:windows-amd64.exe
gateway:1.0:darwin-arm64
gateway:1.0:darwin-amd64
gateway:1.0:linux-amd64
```

The runtime validates those IDs structurally together with exact repository, package, owner, version, filename, size, and a single canonical SHA-256. See Google's [generic artifact guidance](https://cloud.google.com/artifact-registry/docs/generic) and [`gcloud artifacts generic upload`](https://cloud.google.com/sdk/gcloud/reference/artifacts/generic/upload).

## Verify the real provider contract

Use a short-lived credential with Artifact Registry read/download permission. Keep the access token out of command history and logs. With the same clean local release directory:

PowerShell:

```powershell
$env:CAMOS_AR_INTEGRATION = "1"
$env:CAMOS_AR_ACCESS_TOKEN = (gcloud auth print-access-token)
$env:CAMOS_GATEWAY_RELEASE_DIR = (Resolve-Path .\dist).Path
go test -count=1 -run '^TestRealArtifactRegistryReleaseContract$' .
Remove-Item Env:CAMOS_AR_ACCESS_TOKEN
```

POSIX shell:

```sh
export CAMOS_AR_INTEGRATION=1
export CAMOS_AR_ACCESS_TOKEN="$(gcloud auth print-access-token)"
export CAMOS_GATEWAY_RELEASE_DIR="$(pwd)/dist"
go test -count=1 -run '^TestRealArtifactRegistryReleaseContract$' .
unset CAMOS_AR_ACCESS_TOKEN
```

The opt-in test uses the runtime's real Artifact Registry list API and download URL. For all four platforms it proves exact file identity and owner, positive bounded size, one SHA-256, successful byte download, provider/local byte equality, and the complete static candidate identity. It is read-only and has no Postgres client or release-selection side effect.

Passing local and provider verification makes a release eligible for physical acceptance; it does not make it deployable. Record the Git commit, four SHA-256 values, native host/toolchain evidence, and verifier output in the release record, then complete [PRODUCTION_ACCEPTANCE.md](PRODUCTION_ACCEPTANCE.md).

Do not set production `desired_version = '1.0'` until the separate version-column migration, control-plane invariant, all-four-target physical acceptance, and explicit deployment approval are complete.
