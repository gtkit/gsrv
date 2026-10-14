#!/usr/bin/env bash
# 在当前 Go module 检查现代语法迁移；只读代码，临时 diff 在退出时清理。
set -euo pipefail
result=$(mktemp)
trap 'rm -f "$result"' EXIT
fix_status=0
go fix -diff ./... > "$result" || fix_status=$?
if [ "$fix_status" -ne 0 ] || [ -s "$result" ]; then
  cat "$result"
  echo 'go fix 检查未通过；请处理上面的诊断或现代写法差异后重跑。' >&2
  exit 1
fi
