# UAT — ci-minimum-pipeline

## 1 · 验收操作（人类可复跑）

```bash
export PATH=/usr/local/go/bin:$PATH     # 本机默认 go 是 1.13.8，会假失败
bash tools/ci_local.sh                   # 一条命令 = CI 全部四类检查
```

期望末行：`✅ 全部门禁通过`

## 2 · AC 逐条结论

| AC | 结论 | 证据 |
|---|---|---|
| AC-1 CI 三步骤存在 | 通过 | `grep -c` = 4 |
| AC-2 低覆盖率被拒 | 通过 | 负例 exit=1 |
| AC-3 当前覆盖率过门槛 | 通过 | exit=0 |
| AC-4 文档不一致被拒 | 通过 | 注入「待审」→ exit=1 |
| AC-5 nightly 含漏洞扫描 | 通过 | `grep -c govulncheck` = 4 |
| AC-6 本地一键复跑 | 通过 | exit=0 |
| AC-7 零新增依赖 | 通过 | go.mod/go.sum 0 行改动 |
| AC-8 全量回归 | 通过 | 18 包 OK · 589 测试 · 缺口=0 |

## 3 · 已知限制

| 限制 | 说明 |
|---|---|
| GitHub Actions 未实跑 | 本地无 Actions 运行器；workflow 已按 `go-version-file` 写法降低风险，首次推送后需人工看一次绿 |
| `-race` 本地耗时 | 全量约 20s（18 包并行），CI 上可接受 |
| govulncheck 需联网 | 离线环境 nightly 会失败；这是有意的（漏洞库必须新鲜） |
| 钉表重钉 | 任何 `.go` 改动后必须 `python3 tools/agentkit_code_pins.py --freeze`，否则 CI 红——这是设计意图，不是缺陷 |
