@echo off
rem Launcher for the jms MCP bundle on Windows.
rem
rem MCPB's platform_overrides can distinguish operating systems but not CPU
rem architectures, so the bundle ships one binary per architecture and this
rem script picks the right one at run time.
rem
rem The batch file must hand over cleanly: the MCP host talks JSON-RPC over
rem this process's stdin/stdout. `call` keeps a single console and returns
rem the child's exit code, which the host watches.

setlocal enabledelayedexpansion

set "HERE=%~dp0"
rem Strip the trailing backslash %~dp0 always carries.
if "%HERE:~-1%"=="\" set "HERE=%HERE:~0,-1%"

rem PROCESSOR_ARCHITECTURE reports the process architecture, which is what
rem matters here: a 32-bit host on 64-bit Windows reports x86 and cannot
rem load the x64 binary anyway.
set "ARCH=%PROCESSOR_ARCHITECTURE%"
if defined PROCESSOR_ARCHITEW6432 set "ARCH=%PROCESSOR_ARCHITEW6432%"

set "BIN="
if /I "%ARCH%"=="AMD64" set "BIN=%HERE%\server\jms-win32-x86_64.exe"
if /I "%ARCH%"=="ARM64" set "BIN=%HERE%\server\jms-win32-aarch64.exe"

if not defined BIN (
    echo jms: unsupported CPU architecture: %ARCH% 1>&2
    exit /b 1
)

if not exist "%BIN%" (
    echo jms: no bundled binary for win32/%ARCH% ^(looked for %BIN%^) 1>&2
    exit /b 1
)

"%BIN%" %*
exit /b %ERRORLEVEL%
