@echo off
setlocal enabledelayedexpansion

set "SCRIPT_DIR=%~dp0"
rem Remove trailing backslash if present
if "%SCRIPT_DIR:~-1%"=="\" set "SCRIPT_DIR=%SCRIPT_DIR:~0,-1%"
set "PROJECT_ROOT=%SCRIPT_DIR%\.."
for %%i in ("%PROJECT_ROOT%") do set "PROJECT_ROOT=%%~fi"
set "PROTO_DIR=%PROJECT_ROOT%\proto"
set "PROTO_FILE=%PROTO_DIR%\middleware.proto"

if not exist "%PROTO_FILE%" (
  echo Proto file not found: %PROTO_FILE%
  exit /b 1
)

echo Generating Go gRPC stubs from %PROTO_FILE%...

pushd "%PROTO_DIR%"

rem Check if protoc is available
where protoc >nul 2>nul
if errorlevel 1 (
  echo Error: protoc not found. Please install Protocol Buffers compiler.
  echo Download from: https://github.com/protocolbuffers/protobuf/releases
  popd
  exit /b 1
)

rem Check if protoc-gen-go is available
where protoc-gen-go >nul 2>nul
if errorlevel 1 (
  echo Error: protoc-gen-go not found. Installing...
  go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
  if errorlevel 1 (
    echo Failed to install protoc-gen-go
    popd
    exit /b 1
  )
)

rem Check if protoc-gen-go-grpc is available
where protoc-gen-go-grpc >nul 2>nul
if errorlevel 1 (
  echo Error: protoc-gen-go-grpc not found. Installing...
  go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
  if errorlevel 1 (
    echo Failed to install protoc-gen-go-grpc
    popd
    exit /b 1
  )
)

rem Generate Go stubs
protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative middleware.proto
if errorlevel 1 (
  echo Failed to generate Go gRPC stubs
  popd
  exit /b 1
)

popd

echo Successfully generated Go gRPC stubs in %PROTO_DIR%
endlocal
