#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
PROTO_DIR="${PROJECT_ROOT}/proto"
PROTO_FILE="${PROTO_DIR}/middleware.proto"

if [[ ! -f "${PROTO_FILE}" ]]; then
  echo "Proto file not found: ${PROTO_FILE}" >&2
  exit 1
fi

echo "Generating Go gRPC stubs from ${PROTO_FILE}..."

cd "${PROTO_DIR}"

# Check if protoc is available
if ! command -v protoc >/dev/null 2>&1; then
  echo "Error: protoc not found. Please install Protocol Buffers compiler." >&2
  echo "Download from: https://github.com/protocolbuffers/protobuf/releases" >&2
  exit 1
fi

# Check if protoc-gen-go is available
if ! command -v protoc-gen-go >/dev/null 2>&1; then
  echo "Error: protoc-gen-go not found. Installing..." >&2
  go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
  if [[ $? -ne 0 ]]; then
    echo "Failed to install protoc-gen-go" >&2
    exit 1
  fi
fi

# Check if protoc-gen-go-grpc is available
if ! command -v protoc-gen-go-grpc >/dev/null 2>&1; then
  echo "Error: protoc-gen-go-grpc not found. Installing..." >&2
  go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
  if [[ $? -ne 0 ]]; then
    echo "Failed to install protoc-gen-go-grpc" >&2
    exit 1
  fi
fi

# Generate Go stubs
protoc --go_out=. --go_opt=paths=source_relative \
       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
       middleware.proto

if [[ $? -ne 0 ]]; then
  echo "Failed to generate Go gRPC stubs" >&2
  exit 1
fi

echo "Successfully generated Go gRPC stubs in ${PROTO_DIR}"
