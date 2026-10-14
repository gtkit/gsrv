#!/usr/bin/env bash
# 只读契约检查：在自建临时目录生成，比较完整pb集合，不修改工作区。
set -euo pipefail
cd "$(dirname "$0")/.."
task_tmp=$(mktemp -d "${TMPDIR:-/tmp}/gsrv-proto-check.XXXXXX")
trap 'rm -rf "${task_tmp:?}"' EXIT
cp buf.yaml buf.gen.yaml buf.lock "$task_tmp/"
cp -R proto "$task_tmp/"
(
  cd "$task_tmp"
  buf lint
  buf generate
)
if ! diff -ru pb "$task_tmp/pb"; then
  echo 'proto/pb 不一致，请运行 make gen 后复核生成产物。' >&2
  exit 1
fi
echo '✓ proto/pb 产物一致（临时生成，工作区未修改）'
