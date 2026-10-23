# UAT: agentkit 产品基线验收

- **Change ID**: `agentkit-framework`
- **关联**: `@.specs/agentkit-framework/TEST.md` §1.2、`@.specs/agentkit-framework/REQUIREMENT.md`
- **R15.1 五层对齐**：已跑（`.specs/ALIGNMENT.md` 首轮，覆盖 `6bbec97..2049508`，无未处理 D2/D5）

---

## 1. 自动化验证（7-integration 步骤 1 · 真实输出）

```text
$ go test -count=1 ./...
ok  .  0.676s
ok  ./agent 0.099s        ok  ./audit 0.008s      ok  ./authz 0.011s
ok  ./engine 0.012s       ok  ./im 0.012s         ok  ./kit 0.030s
ok  ./kitctx 0.028s       ok  ./model 0.134s      ok  ./modelcfg 3.045s
ok  ./playbook 0.030s     ok  ./preset 0.039s     ok  ./scene 0.028s
ok  ./security 0.048s     ok  ./semantic 0.033s   ok  ./session 0.028s
ok  ./tools 0.733s        ok  ./web 0.064s
exit=0            ← 18/18 包，0 FAIL

$ go test -race -count=1 ./...
18 个包 ok，exit=0

$ go vet ./...
exit=0（零告警）

$ python3 tools/agentkit_code_pins.py
agentkit .go：磁盘 82 / 已钉 82
缺口=0

$ md5 对照（guard_test.go）
hash ok

$ gofmt -l .
（空输出 = 全部已格式化）
```

构建 / 类型检查：Go 无独立 build 步骤，`go vet` + `go test` 编译通过即等价。

---

## 2. 人工 UAT

### UAT-1 · 产品文档人审门

- **前置**: `PRODUCT-DESIGN.html` 状态为「起草中」，等待人工逐章过审
- **步骤**:
  1. 浏览器打开 `.specs/agentkit-framework/PRODUCT-DESIGN.html`（断网环境）
  2. 按左侧目录逐章阅读 §0 阅读指引 → §9 FAQ
  3. 重点核对 §1.4 边界、§6 七个原型、§7 权限矩阵三者是否自洽
- **期望**:
  - 九段齐全、无 `TODO` / `待补` 字样
  - §5.0 角色可见面与 §7 权限矩阵逐格一致
  - 原型中的角色权限表现与矩阵一致
- **实际**: **通过** —— 用户于 2026-10-23 以原话「好的同意了」背书，文档状态晋级「已过审」
- **执行人 / 时间**: 用户 / 2026-10-23

> 其余验收项（AC-1 ~ AC-8）均为机器可判，已在 `TEST.md` 与上表覆盖，不再单列 UAT。

---

## 3. 失败诊断

无失败项（自动化 5 条命令全部 exit=0；UAT-1 通过）。

---

## 4. 结论

- 自动化：✅ 全绿（18 包 / race / vet / 钉表 / 格式）
- 人工：✅ UAT-1 通过（用户背书）
- 五层对齐：✅ 无未处理 D2/D5；存量 D6 已留档议题
- **可进 ARCHIVE**
