param(
    [ValidateSet("windows-amd64")]
    [string]$Target = "windows-amd64"
)

$ErrorActionPreference = "Stop"
$expectedBuildVersion = "1.0"
$gatewayRoot = $PSScriptRoot
$outputDirectory = Join-Path $gatewayRoot "dist"
$bootstrapPath = Join-Path $gatewayRoot "bootstrap_sa.json"
$ffmpegPath = Join-Path $gatewayRoot "ffmpeg.exe"
$outputPath = Join-Path $outputDirectory "windows-amd64.exe"
$temporaryOutput = $outputPath + ".building"
$checksumPath = Join-Path $outputDirectory "SHA256SUMS"
$temporaryChecksumPath = $checksumPath + ".building"
$releaseTargets = @("windows-amd64.exe", "darwin-arm64", "darwin-amd64", "linux-amd64")

if (-not (Test-Path -LiteralPath $bootstrapPath -PathType Leaf)) { throw "Missing gateway/bootstrap_sa.json" }
if (-not (Test-Path -LiteralPath $ffmpegPath -PathType Leaf)) { throw "Missing gateway/ffmpeg.exe" }
if ((Get-Item -LiteralPath $ffmpegPath).Length -eq 0) { throw "gateway/ffmpeg.exe is empty" }

$ffmpegBytes = [System.IO.File]::ReadAllBytes($ffmpegPath)
if ($ffmpegBytes.Length -lt 64 -or $ffmpegBytes[0] -ne 0x4d -or $ffmpegBytes[1] -ne 0x5a) { throw "ffmpeg.exe is not a PE executable" }
$peOffset = [BitConverter]::ToInt32($ffmpegBytes, 0x3c)
if ($peOffset -lt 0 -or $peOffset + 44 -gt $ffmpegBytes.Length) { throw "ffmpeg.exe has an invalid PE header" }
if ($ffmpegBytes[$peOffset] -ne 0x50 -or
    $ffmpegBytes[$peOffset + 1] -ne 0x45 -or
    $ffmpegBytes[$peOffset + 2] -ne 0x00 -or
    $ffmpegBytes[$peOffset + 3] -ne 0x00) {
    throw "ffmpeg.exe has an invalid PE signature"
}
$optionalHeaderSize = [BitConverter]::ToUInt16($ffmpegBytes, $peOffset + 20)
if ($optionalHeaderSize -lt 20 -or $peOffset + 24 + $optionalHeaderSize -gt $ffmpegBytes.Length) {
    throw "ffmpeg.exe has an invalid PE optional header"
}
$characteristics = [BitConverter]::ToUInt16($ffmpegBytes, $peOffset + 22)
$entryPoint = [BitConverter]::ToUInt32($ffmpegBytes, $peOffset + 40)
if ([BitConverter]::ToUInt16($ffmpegBytes, $peOffset + 4) -ne 0x8664 -or
    [BitConverter]::ToUInt16($ffmpegBytes, $peOffset + 24) -ne 0x020b -or
    $entryPoint -eq 0 -or
    ($characteristics -band 0x0002) -eq 0 -or
    ($characteristics -band 0x2000) -ne 0) {
    throw "ffmpeg.exe is not an executable windows/amd64 PE image"
}

$goHostOS = (& go env GOHOSTOS)
if ($LASTEXITCODE -ne 0) { throw "go env GOHOSTOS failed" }
$goHostArch = (& go env GOHOSTARCH)
if ($LASTEXITCODE -ne 0) { throw "go env GOHOSTARCH failed" }
$goVersion = (& go env GOVERSION)
if ($LASTEXITCODE -ne 0) { throw "go env GOVERSION failed" }
if ($goHostOS -ne "windows" -or $goHostArch -ne "amd64") { throw "windows-amd64 must be built in a native windows/amd64 Go environment" }
if ($goVersion -ne "go1.25.12") { throw "production releases require the pinned Go 1.25.12 toolchain (found $goVersion)" }

New-Item -ItemType Directory -Force -Path $outputDirectory | Out-Null
Remove-Item -Force -ErrorAction SilentlyContinue -LiteralPath $temporaryOutput
Remove-Item -Force -ErrorAction SilentlyContinue -LiteralPath $temporaryChecksumPath

Push-Location $gatewayRoot
try {
    $oldBootstrapVerificationPath = $env:CAMOS_VERIFY_BOOTSTRAP_CREDENTIAL
    try {
        $env:CAMOS_VERIFY_BOOTSTRAP_CREDENTIAL = $bootstrapPath
        go test -count=1 -run '^TestReleaseBootstrapInput$' .
        if ($LASTEXITCODE -ne 0) { throw "bootstrap credential verification failed" }
    } finally {
        $env:CAMOS_VERIFY_BOOTSTRAP_CREDENTIAL = $oldBootstrapVerificationPath
    }
    go test -count=1 ./...
    if ($LASTEXITCODE -ne 0) { throw "Gateway tests failed" }
    go vet ./...
    if ($LASTEXITCODE -ne 0) { throw "Gateway vet failed" }
    go build -tags production -trimpath -o $temporaryOutput .
    if ($LASTEXITCODE -ne 0) { throw "Gateway build failed" }

    $oldVerifyPath = $env:CAMOS_VERIFY_RELEASE_ARTIFACT
    $oldVerifyTarget = $env:CAMOS_VERIFY_RELEASE_TARGET
    $oldVerifyVersion = $env:CAMOS_VERIFY_RELEASE_VERSION
    try {
        $env:CAMOS_VERIFY_RELEASE_ARTIFACT = $temporaryOutput
        $env:CAMOS_VERIFY_RELEASE_TARGET = "windows-amd64.exe"
        $env:CAMOS_VERIFY_RELEASE_VERSION = $expectedBuildVersion
        go test -count=1 -run '^TestReleaseBuildArtifact$' .
        if ($LASTEXITCODE -ne 0) { throw "static release identity verification failed" }
    } finally {
        $env:CAMOS_VERIFY_RELEASE_ARTIFACT = $oldVerifyPath
        $env:CAMOS_VERIFY_RELEASE_TARGET = $oldVerifyTarget
        $env:CAMOS_VERIFY_RELEASE_VERSION = $oldVerifyVersion
    }

    Move-Item -Force -LiteralPath $temporaryOutput -Destination $outputPath

    $checksums = [ordered]@{}
    if (Test-Path -LiteralPath $checksumPath -PathType Leaf) {
        foreach ($line in Get-Content -LiteralPath $checksumPath) {
            if ($line -notmatch '^([0-9a-fA-F]{64})  (windows-amd64\.exe|darwin-arm64|darwin-amd64|linux-amd64)$') {
                throw "dist/SHA256SUMS contains a malformed or unknown entry"
            }
            $name = $Matches[2]
            if ($checksums.Contains($name)) { throw "dist/SHA256SUMS contains duplicate $name entries" }
            $checksums[$name] = $Matches[1].ToLowerInvariant()
        }
    }
    foreach ($name in $releaseTargets) {
        $artifactPath = Join-Path $outputDirectory $name
        if (Test-Path -LiteralPath $artifactPath -PathType Leaf) {
            $checksums[$name] = (Get-FileHash -Algorithm SHA256 -LiteralPath $artifactPath).Hash.ToLowerInvariant()
        }
    }
    $checksumLines = @()
    foreach ($name in $releaseTargets) {
        if ($checksums.Contains($name)) { $checksumLines += "$($checksums[$name])  $name" }
    }
    [System.IO.File]::WriteAllText($temporaryChecksumPath, (($checksumLines -join "`n") + "`n"), [System.Text.Encoding]::ASCII)
    Move-Item -Force -LiteralPath $temporaryChecksumPath -Destination $checksumPath
} finally {
    Pop-Location
    Remove-Item -Force -ErrorAction SilentlyContinue -LiteralPath $temporaryOutput
    Remove-Item -Force -ErrorAction SilentlyContinue -LiteralPath $temporaryChecksumPath
}

Write-Output "Built and statically verified dist/windows-amd64.exe (BuildVersion 1.0)"
