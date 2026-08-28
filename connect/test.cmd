@echo off
setlocal

pushd "%~dp0"
if errorlevel 1 (
    echo FAIL: unable to open connect directory 1>&2
    exit /b 1
)

go run ./cmd/test
set "exit_code=%ERRORLEVEL%"

popd
exit /b %exit_code%
