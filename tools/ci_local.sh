#!/usr/bin/env bash
# 一条命令跑完 CI 的四件事（本地可复跑，不依赖 Actions）。
set -euo pipefail
cd "$(dirname "$0")/.."

# 陷阱：本机 PATH 里可能是 Go 1.13.8（`any` 未定义），必须把选定的工具链推到 PATH 前面，
# 否则 portability_check.py 之类以子进程调 `go` 的门禁会静默用错版本。
GO=${GO:-/usr/local/go/bin/go}
if ! "$GO" version >/dev/null 2>&1; then
  GO=go
fi
export PATH="$(cd "$(dirname "$GO")" && pwd):$PATH"

echo "==> [1/5] go vet"
"$GO" vet ./...

echo "==> [2/5] go test -race"
"$GO" test -race ./...

echo "==> [3/5] 钉表（路径 + 哈希）"
python3 tools/agentkit_code_pins.py | tail -1

echo "==> [4/5] 可搬运性 J-PORT"
python3 tools/portability_check.py | tail -1

echo "==> [5/5] 覆盖率门槛 + 文档断言"
tmp=$(mktemp -t cover.XXXXXX.out)
trap 'rm -f "$tmp"' EXIT
"$GO" test -coverprofile="$tmp" ./... > /dev/null
python3 tools/coverage_gate.py "$tmp" | tail -1
python3 tools/doc_assert.py | tail -1

echo "✅ 全部门禁通过"
