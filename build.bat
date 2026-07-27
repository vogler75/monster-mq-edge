@echo off
REM build.bat - Build wrapper for MonsterMQ Edge (Windows).
REM
REM Windows counterpart of build.sh. build.sh delegates to the Makefile, but make
REM is not normally available on Windows, so the native build invokes go build
REM directly with the same flags as the Makefile's "build" target.
REM
REM Usage:
REM   build.bat        Build the native binary for the current machine (default)
REM   build.bat --deb  Build Debian packages for all target architectures
REM   build.bat -h     Show this help
REM
REM The --deb path shells out to scripts\build-deb.sh and therefore needs a
REM working bash (WSL or Git Bash) plus the usual Debian packaging tools.

setlocal enabledelayedexpansion

cd /d "%~dp0"

set "BIN=bin\monstermq-edge.exe"
set "PKG=.\cmd\monstermq-edge"
set "BUILD_DEB=false"

:parse_args
if "%~1"=="" goto :done_parse
if "%~1"=="--deb"  (set "BUILD_DEB=true" & shift & goto :parse_args)
if "%~1"=="-deb"   (set "BUILD_DEB=true" & shift & goto :parse_args)
if "%~1"=="-h"     goto :show_help
if "%~1"=="-help"  goto :show_help
if "%~1"=="--help" goto :show_help
echo Unknown option: %~1 1>&2
goto :bad_option
:done_parse

if "!BUILD_DEB!"=="true" goto :build_deb

REM --- Native binary for this machine ----------------------------------------
where go >nul 2>&1
if !errorlevel! neq 0 (
    echo go was not found on PATH. 1>&2
    exit /b 1
)

if not exist bin mkdir bin
set /p VERSION=<version.txt

echo Building native binary for the current machine...
set "CGO_ENABLED=0"
go build -trimpath -ldflags="-s -w -X monstermq.io/edge/internal/version.Version=!VERSION!" -o "!BIN!" "!PKG!"
if !errorlevel! neq 0 (
    echo Build failed 1>&2
    exit /b 1
)
echo Native binary built at: !BIN!
exit /b 0

REM --- Debian packages for all target architectures --------------------------
:build_deb
REM Prefer Git Bash. The bash.exe in System32 is only the WSL launcher, which
REM fails outright when no distribution is installed.
set "BASH_EXE="
if exist "%ProgramFiles%\Git\bin\bash.exe" set "BASH_EXE=%ProgramFiles%\Git\bin\bash.exe"
if not defined BASH_EXE (
    for /f "delims=" %%B in ('where bash 2^>nul') do if not defined BASH_EXE set "BASH_EXE=%%B"
)
if not defined BASH_EXE (
    echo --deb needs bash to run scripts\build-deb.sh, but none was found. 1>&2
    exit /b 1
)

REM build-deb.sh assembles the .deb with tar and ar; Git Bash ships tar but no ar.
"!BASH_EXE!" -c "command -v ar >/dev/null 2>&1" >nul 2>&1
if !errorlevel! neq 0 (
    echo Cannot build .deb packages here. 1>&2
    echo   bash: !BASH_EXE! 1>&2
    echo scripts\build-deb.sh assembles the package with tar and ar, and ar 1>&2
    echo ^(binutils^) is not available to that shell. Run build.sh --deb under 1>&2
    echo WSL, Linux or macOS instead. 1>&2
    exit /b 1
)

echo Building Debian packages for all target architectures...
for %%A in (arm64 armhf amd64) do (
    echo   -^> %%A
    "!BASH_EXE!" ./scripts/build-deb.sh --arch %%A
    if !errorlevel! neq 0 (
        echo Debian package build failed for %%A 1>&2
        exit /b 1
    )
)
echo Debian packages built.
exit /b 0

:show_help
call :usage
exit /b 0

:bad_option
call :usage
exit /b 1

:usage
echo Usage: build.bat [options]
echo.
echo Options:
echo   ^(no options^)     Build the native binary for the current machine ^(default^)
echo   --deb, -deb      Build Debian packages for all target architectures ^(arm64, armhf, amd64^)
echo   -h, --help       Show this help message
echo.
goto :eof
