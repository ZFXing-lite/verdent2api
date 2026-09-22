#!/usr/bin/env bash
# 构建 + vet + test。用法：./build.sh [output_dir]
set -euo pipefail
cd "$(dirname "$0")"

OUT="${1:-./bin}"
mkdir -p "$OUT"

echo "==> go vet"
go vet ./...

echo "==> go test"
go test ./...

echo "==> build"
go build -trimpath -ldflags "-s -w" -o "$OUT/verdent-server" ./cmd/server
go build -trimpath -ldflags "-s -w" -o "$OUT/verdent-login" ./cmd/login

echo "==> done"
ls -la "$OUT"
