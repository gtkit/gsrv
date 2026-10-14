#!/usr/bin/env bash
# 只读取当前源码与.example；可显式提供一个本地服务副本用于部署标识冲突核对。
set -euo pipefail
cd "$(dirname "$0")/.."
if [ "$#" -gt 1 ]; then
  echo 'usage: bash scripts/check-copy.sh [service-checkout]' >&2
  exit 2
fi
GOWORK=off go run ./cmd/server -check-copy -compare-service "${1-}"
