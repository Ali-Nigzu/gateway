param(
    [ValidateSet("windows-amd64")]
    [string]$Target = "windows-amd64"
)

$ErrorActionPreference = "Stop"
$gatewayRoot = $PSScriptRoot
$outputDirectory = Join-Path $gatewayRoot "dist"
$bootstrapPath = Join-Path $gatewayRoot "bootstrap_sa.json"
$ffmpegPath = Join-Path $gatewayRoot "ffmpeg.exe"
$outputPath = Join-Path $outputDirectory "windows-amd64.exe"
$temporaryOutput = $outputPath + ".building"

if (-not (Test-Path -LiteralPath $bootstrapPath -PathType Leaf)) { throw "Missing gateway/bootstrap_sa.json" }
if (-not (Test-Path -LiteralPath $ffmpegPath -PathType Leaf)) { throw "Missing gateway/ffmpeg.exe" }
if ((Get-Item -LiteralPath $ffmpegPath).Length -eq 0) { throw "gateway/ffmpeg.exe is empty" }

$bootstrap = Get-Content -Raw -LiteralPath $bootstrapPath | ConvertFrom-Json
if ($bootstrap.type -ne "service_account" -or $bootstrap.client_email -ne "gateway-bootstrap@camosbase.iam.gserviceaccount.com") {
    throw "bootstrap_sa.json is not the camOS bootstrap service account credential"
}

$ffmpegBytes = [System.IO.File]::ReadAllBytes($ffmpegPath)
if ($ffmpegBytes.Length -lt 64 -or $ffmpegBytes[0] -ne 0x4d -or $ffmpegBytes[1] -ne 0x5a) { throw "ffmpeg.exe is not a PE executable" }
$peOffset = [BitConverter]::ToInt32($ffmpegBytes, 0x3c)
if ($peOffset -lt 0 -or $peOffset + 6 -gt $ffmpegBytes.Length -or [BitConverter]::ToUInt16($ffmpegBytes, $peOffset + 4) -ne 0x8664) {
    throw "ffmpeg.exe is not windows/amd64"
}

if ((go env GOOS) -ne "windows" -or (go env GOARCH) -ne "amd64") { throw "windows-amd64 must be built in a native windows/amd64 Go environment" }
New-Item -ItemType Directory -Force -Path $outputDirectory | Out-Null
Remove-Item -Force -ErrorAction SilentlyContinue -LiteralPath $temporaryOutput

Push-Location $gatewayRoot
try {
    go test -count=1 ./...
    go build -tags production -trimpath -o $temporaryOutput .
    Move-Item -Force -LiteralPath $temporaryOutput -Destination $outputPath
    $hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $outputPath).Hash.ToLowerInvariant()
    Set-Content -LiteralPath (Join-Path $outputDirectory "SHA256SUMS") -Encoding ascii -NoNewline -Value "$hash  windows-amd64.exe`n"
} finally {
    Pop-Location
    Remove-Item -Force -ErrorAction SilentlyContinue -LiteralPath $temporaryOutput
}

Write-Output "Built dist/windows-amd64.exe (BuildVersion 1)"
