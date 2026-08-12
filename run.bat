@echo off
REM MonsterMQ Edge run script (Go broker).
REM Usage: run.bat [SCRIPT_OPTIONS] [-- BROKER_OPTIONS]
REM
REM Script options (before --):
REM   -b,  -build       Build the binary (CGO_ENABLED=0) before starting
REM   -c,  -compile     Type-check / compile all packages (no binary output)
REM   -n,  -norun       Don't start the broker (combine with -b/-c for build-only)
REM   -nk, -nokill      Don't kill any already-running monstermq-edge first
REM   -h,  -help        Show this help
REM
REM Broker options (after --):
REM   Anything after -- is passed straight to the binary.
REM   See .\bin\monstermq-edge -h for available flags (-config, -log-level, -version).
REM
REM Examples:
REM   run.bat                       Start with config.yaml (or config.yaml.example)
REM   run.bat -b                    Build, then start
REM   run.bat -b -n                 Build only
REM   run.bat -b -- -config foo.yaml

setlocal enabledelayedexpansion

cd /d "%~dp0"

set "BIN=bin\monstermq-edge.exe"
set "BUILD=false"
set "COMPILE=false"
set "NORUN=false"
set "NOKILL=false"
set "SAW_SEP=false"
set "BROKER_ARGS="
set "HAS_CONFIG=false"

:parse_args
if "%~1"=="" goto :done_parse
if "!SAW_SEP!"=="true" (
    set "BROKER_ARGS=!BROKER_ARGS! %1"
    shift
    goto :parse_args
)
if "%~1"=="-b"         (set "BUILD=true"   & shift & goto :parse_args)
if "%~1"=="-build"     (set "BUILD=true"   & shift & goto :parse_args)
if "%~1"=="-c"         (set "COMPILE=true" & shift & goto :parse_args)
if "%~1"=="-compile"   (set "COMPILE=true" & shift & goto :parse_args)
if "%~1"=="-n"         (set "NORUN=true"   & shift & goto :parse_args)
if "%~1"=="-norun"     (set "NORUN=true"   & shift & goto :parse_args)
if "%~1"=="-nk"        (set "NOKILL=true"  & shift & goto :parse_args)
if "%~1"=="-nokill"    (set "NOKILL=true"  & shift & goto :parse_args)
if "%~1"=="-h"         goto :show_help
if "%~1"=="-help"      goto :show_help
if "%~1"=="--help"     goto :show_help
if "%~1"=="--"         (set "SAW_SEP=true" & shift & goto :parse_args)
set "BROKER_ARGS=!BROKER_ARGS! %1"
shift
goto :parse_args
:done_parse

if "!BUILD!"=="true" (
    echo Building monstermq-edge...
    if not exist bin mkdir bin
    set "CGO_ENABLED=0"
    set /p VERSION=<version.txt
    go build -trimpath -ldflags="-s -w -X monstermq.io/edge/internal/version.Version=!VERSION!" -o "!BIN!" .\cmd\monstermq-edge
    if !errorlevel! neq 0 (
        echo Build failed
        exit /b 1
    )
    echo Built: !BIN!
)

if "!COMPILE!"=="true" if "!BUILD!"=="false" (
    echo Compiling all packages...
    go build ./...
    if !errorlevel! neq 0 (
        echo Compilation failed
        exit /b 1
    )
)

if "!NORUN!"=="true" exit /b 0

if not exist "!BIN!" (
    echo Binary !BIN! not found. Run with -b first. 1>&2
    exit /b 1
)

if "!NOKILL!"=="false" (
    tasklist /FI "IMAGENAME eq monstermq-edge.exe" 2>nul | find /I "monstermq-edge.exe" >nul 2>&1
    if !errorlevel! equ 0 (
        echo Killing existing instances of monstermq-edge...
        taskkill /IM monstermq-edge.exe /F >nul 2>&1
        timeout /t 2 /nobreak >nul
    )
)

REM Pick a config: explicit -config wins; otherwise prefer config.yaml then the example.
echo !BROKER_ARGS! | find "-config" >nul 2>&1
if !errorlevel! neq 0 (
    if exist "config.yaml" (
        set "BROKER_ARGS=-config config.yaml !BROKER_ARGS!"
    ) else if exist "config.yaml.example" (
        set "BROKER_ARGS=-config config.yaml.example !BROKER_ARGS!"
    )
)

echo Starting: !BIN!!BROKER_ARGS!
"!BIN!" !BROKER_ARGS!
exit /b !errorlevel!

:show_help
echo MonsterMQ Edge run script (Go broker).
echo Usage: run.bat [SCRIPT_OPTIONS] [-- BROKER_OPTIONS]
echo.
echo Script options ^(before --^):
echo   -b,  -build       Build the binary ^(CGO_ENABLED=0^) before starting
echo   -c,  -compile     Type-check / compile all packages ^(no binary output^)
echo   -n,  -norun       Don't start the broker ^(combine with -b/-c for build-only^)
echo   -nk, -nokill      Don't kill any already-running monstermq-edge first
echo   -h,  -help        Show this help
echo.
echo Broker options ^(after --^):
echo   Anything after -- is passed straight to the binary.
echo   See .\bin\monstermq-edge -h for available flags ^(-config, -log-level, -version^).
echo.
echo Examples:
echo   run.bat                       Start with config.yaml ^(or config.yaml.example^)
echo   run.bat -b                    Build, then start
echo   run.bat -b -n                 Build only
echo   run.bat -b -- -config foo.yaml
exit /b 0

