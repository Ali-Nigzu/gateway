@echo off
setlocal

if "%~1"=="" goto usage
if not "%~2"=="" goto usage

echo(%~1| findstr /r "^[1-9][0-9]*$" >nul
if errorlevel 1 goto usage

pushd "%~dp0"
if errorlevel 1 (
    echo FAIL: unable to open connect directory 1>&2
    exit /b 1
)

go run ./cmd/commission "%~1"
set "exit_code=%ERRORLEVEL%"

popd
exit /b %exit_code%

:usage
echo FAIL: usage: commission ^<site_id^> 1>&2
exit /b 2
