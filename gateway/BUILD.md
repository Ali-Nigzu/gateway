# camOS Gateway production builds

Production releases are built locally. They are not built or published by GitHub Actions.

Place the private `bootstrap_sa.json` and the matching platform FFmpeg executable in this directory. These files and `dist/` are ignored by Git. Never commit or print their contents.

Build one target at a time:

```text
Windows amd64:  .\build.ps1
macOS arm64:    ./build.sh darwin-arm64
macOS amd64:    ./build.sh darwin-amd64
Linux amd64:    ./build.sh linux-amd64
```

Darwin builds require a proper macOS toolchain because the Gateway uses CGo and Apple IOKit/CoreFoundation frameworks. The `ffmpeg` input must be replaced with the correct target build before each Darwin or Linux invocation.

The only release filenames are:

```text
windows-amd64.exe
darwin-arm64
darwin-amd64
linux-amd64
```

Each binary contains BuildVersion 1, the bootstrap commissioning credential and the matching FFmpeg payload. After physical qualification, upload these exact files to Generic Artifact Registry repository `camos-gateway-prod`, package `gateway`, version `1`.
