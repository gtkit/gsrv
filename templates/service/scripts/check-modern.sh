#!/usr/bin/env bash
# 在当前 Go module 检查现代语法迁移；只读代码，临时 diff 在退出时清理。
set -euo pipefail
result=$(mktemp)
trap 'rm -f "$result"' EXIT
go fix -diff ./... > "$result"
if [ -s "$result" ]; then
  cat "$result"
  echo '请应用上述现代 Go 写法并重新检查。' >&2
  exit 1
fi
