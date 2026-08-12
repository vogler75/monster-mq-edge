@echo off
REM build.bat - Master build script for MonsterMQ Edge (Windows).
REM
REM Usage:
REM   build.bat            Build all artifacts locally (binary, debian packages, docker image)
REM   build.bat --binary   Build native Go binary for current machine
REM   build.bat --deb      Build Debian packages for all target architectures (arm64, armhf, amd64)
REM   build.bat --docker   Build local Docker image (native platform)
REM   build.bat --publish  Trigger release publication script
REM   build.bat --clean    Clean build output directories

setlocal enabledelayedexpansion

cd /d "%~dp0"

set "BIN=bin\monstermq-edge.exe"
set "PKG=.\cmd\monstermq-edge"

set "BUILD_BINARY=false"
set "BUILD_DEB=false"
set "BUILD_DOCKER=false"
set "PUBLISH=false"
set "CLEAN=false"

:parse_args
if "%~1"=="" goto :done_parse
if /i "%~1"=="--all"     (set "BUILD_BINARY=true" & set "BUILD_DEB=true" & set "BUILD_DOCKER=true" & shift & goto :parse_args)
if /i "%~1"=="--binary"  (set "BUILD_BINARY=true" & shift & goto :parse_args)
if /i "%~1"=="--deb"     (set "BUILD_DEB=true"    & shift & goto :parse_args)
if /i "%~1"=="-deb"      (set "BUILD_DEB=true"    & shift & goto :parse_args)
if /i "%~1"=="--docker"   (set "BUILD_DOCKER=true" & shift & goto :parse_args)
if /i "%~1"=="--publish"  (set "PUBLISH=true"      & shift & goto :parse_args)
if /i "%~1"=="-p"         (set "PUBLISH=true"      & shift & goto :parse_args)
if /i "%~1"=="--clean"    (set "CLEAN=true"        & shift & goto :parse_args)
if /i "%~1"=="-h"        goto :show_help
if /i "%~1"=="-help"     goto :show_help
if /i "%~1"=="--help"    goto :show_help
echo Unknown option: %~1 1>&2
goto :bad_option

:done_parse

if "!BUILD_BINARY!"=="false" if "!BUILD_DEB!"=="false" if "!BUILD_DOCKER!"=="false" (
    set "BUILD_BINARY=true"
    set "BUILD_DEB=true"
    set "BUILD_DOCKER=true"
)

if not exist version.txt (
    echo Error: version.txt not found 1>&2
    exit /b 1
)
set /p VERSION=<version.txt

echo === MonsterMQ Edge Build Pipeline ^(v!VERSION!^) ===

if "!CLEAN!"=="true" (
    echo Cleaning build directories...
    if exist bin rmdir /S /Q bin
    if exist dist rmdir /S /Q dist
    echo Clean complete.
)

if "!BUILD_BINARY!"=="true" (
    echo [1/3] Building native Go binary...
    where go >nul 2>&1
    if !errorlevel! neq 0 (
        echo go was not found on PATH. 1>&2
        exit /b 1
    )
    if not exist bin mkdir bin
    set "CGO_ENABLED=0"
    go build -trimpath -ldflags="-s -w -X monstermq.io/edge/internal/version.Version=!VERSION!" -o "!BIN!" "!PKG!"
    if !errorlevel! neq 0 (
        echo Build failed 1>&2
        exit /b 1
    )
    echo Native binary built at: !BIN!
)

if "!BUILD_DEB!"=="true" (
    echo [2/3] Building Debian packages ^(arm64, armhf, amd64^)...
    set "BASH_EXE="
    if exist "%ProgramFiles%\Git\bin\bash.exe" set "BASH_EXE=%ProgramFiles%\Git\bin\bash.exe"
    if not defined BASH_EXE (
        for /f "delims=" %%B in ('where bash 2^>nul') do if not defined BASH_EXE set "BASH_EXE=%%B"
    )
    if not defined BASH_EXE (
        echo --deb needs bash to run scripts\build-deb.sh, but none was found. 1>&2
        echo Skipping Debian packages on this machine.
    ) else (
        "!BASH_EXE!" -c "command -v ar >/dev/null 2>&1" >nul 2>&1
        if !errorlevel! neq 0 (
            echo ar ^(binutils^) not found in bash environment. Skipping Debian packaging. 1>&2
        ) else (
            for %%A in (arm64 armhf amd64) do (
                echo   -^> %%A
                "!BASH_EXE!" ./scripts/build-deb.sh --arch %%A
                if !errorlevel! neq 0 (
                    echo Debian package build failed for %%A 1>&2
                    exit /b 1
                )
            )
            echo Debian packages built in bin/
        )
    )
)

if "!BUILD_DOCKER!"=="true" (
    echo [3/3] Building local Docker image...
    where docker >nul 2>&1
    if !errorlevel! neq 0 (
        echo docker was not found on PATH. Skipping Docker build. 1>&2
    ) else (
        docker build -t rocworks/monstermq-edge:latest -f docker/Dockerfile .
        if !errorlevel! neq 0 (
            echo Docker build failed 1>&2
            exit /b 1
        )
        echo Local Docker image built: rocworks/monstermq-edge:latest
    )
)

echo === Build Complete ===

if "!PUBLISH!"=="true" (
    echo Triggering release publication...
    if exist publish.bat (
        call publish.bat
    ) else (
        set "BASH_EXE="
        if exist "%ProgramFiles%\Git\bin\bash.exe" set "BASH_EXE=%ProgramFiles%\Git\bin\bash.exe"
        if not defined BASH_EXE (
            for /f "delims=" %%B in ('where bash 2^>nul') do if not defined BASH_EXE set "BASH_EXE=%%B"
        )
        if defined BASH_EXE (
            "!BASH_EXE!" ./publish.sh
        ) else (
            echo Cannot run publish.sh - bash not found. 1>&2
        )
    )
)

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
echo   --all            Build all artifacts ^(default^)
echo   --binary         Build native Go binary for current machine
echo   --deb            Build Debian packages ^(arm64, armhf, amd64^)
echo   --docker         Build local Docker image ^(native platform^)
echo   -p, --publish    Trigger ./publish.sh after building
echo   --clean          Clean build output directories
echo   -h, --help       Show this help message
echo.
goto :eof

