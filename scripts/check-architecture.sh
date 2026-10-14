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
check '公开入口及通用封装禁止依赖业务层、模板或 SDK' \
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
  for worker in "$service"/internal/module/*/worker; do
    if [ -d "$worker" ]; then
      check 'worker 只能通过 application 访问业务' 'internal/repository/|internal/module/[^/]+/transport/|gorm\.io/|net/http' "$worker"
    fi
  done
  for application in "$service"/internal/module/*/application; do
    if [ -d "$application" ]; then
      check '业务层禁止引用 pb、协议及装配层' '/pb/|google.golang.org/grpc|net/http|internal/(bootstrap|runtime)/' "$application"
    fi
  done
done
if [ "$fail" -eq 0 ]; then
  echo '✓ 架构传感器通过（根服务和模板沿用原分层）'
fi
exit "$fail"
