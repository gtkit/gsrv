#!/usr/bin/env bash
# 沿用既有分层；公开包禁止反向依赖内部验证模块和独立模板。
set -euo pipefail
cd "$(dirname "$0")/.."
fail=0
check() {
  local desc="$1" pattern="$2"
  shift 2
  local out
  out=$(grep -rEn --include='*.go' --exclude='*_test.go' "$pattern" "$@" || true)
  if [ -n "$out" ]; then
    echo "✗ $desc:" >&2
    echo "$out" >&2
    fail=1
  fi
}
check '公开骨架禁止依赖 internal、模板或业务 SDK' \
  'github.com/gtkit/gsrv/(internal/(bootstrap|platform|module|runtime|repository)|pb)/|example.com/service|gtkit/(ocpcx|asynqx|redisx)' \
  gsrv.go internal/pkg
for service in . templates/service; do
  check 'module 禁止直接引用基础设施、repository、SDK 和熔断器' \
    'gorm\.io/|gtkit/(redisx|httpc|asynqx|ocpcx)|redis/go-redis|hibiken/asynq|internal/repository/|sony/gobreaker' \
    "$service/internal/module"
  for directory in repository pkg; do
    if [ -d "$service/internal/$directory" ]; then
      check 'repository/pkg 禁止反向依赖 module' 'internal/module/' "$service/internal/$directory"
    fi
  done
  check '业务层禁止引用 pb 和协议包' '/pb/|google.golang.org/grpc|net/http' \
    "$service/internal/module/info/application"
done
if [ "$fail" -eq 0 ]; then
  echo '✓ 架构传感器通过（根服务和模板沿用原分层）'
fi
exit "$fail"
