# TASK: agentkit 产品基线验收

- **Change ID**: `agentkit-framework`
- **关联**: `@.specs/agentkit-framework/REQUIREMENT.md`、`@.specs/agentkit-framework/PRODUCT-DESIGN.html`
- **无 DESIGN.md**：本次无新架构决策；`3-task` 输入按 `REQUIREMENT.md` 与 `CONTEXT.md`（`1-requirement.md` 允许无新架构时直接拆任务）

---

## 波次划分

```
Wave 1 (parallel): T01[P], T02[P]
```

> T01 改代码侧（`guard_test.go` + `GATE-PINS.md5`），T02 改文档侧（`STATE.md` / `CONTEXT.md`），两侧文件集不相交，可并行。

---

## 任务清单

```xml
<task id="T01" parallel="true" status="done">
  <name>恢复根守卫夹具的 8 处布局字面量并刷新钉表</name>
  <read_files>
    <参考边界>
    <guard_test.go（仅 TestGuard_AgentkitImportClassification 与其上方注释）>
    <tools/agentkit_code_pins.py>
    <GATE-PINS.md5>
    <说明：需要 git log -p -- guard_test.go 核对 6bbec97 之前的原意>
  </read_files>
  <write_files>
    <修改边界>
    <guard_test.go>
    <GATE-PINS.md5>
  </write_files>
  <action>
    只恢复测试夹具的路径字面量，判据函数 guardInsideFramework / guardUnderModulePrefix 一行不改：
    ① repoSubtree 由 host+"/agentkit" 改回 host+"/pkg/agentkit"（仓库布局）；
    ② 仓库布局 4 条相似前缀用例回到 host+"/pkg/agentkitfoo"、"/pkg/agentkitfoo/service"、"/pkg/agentkit_extra"、"/pkg/agentkitmodel"，注释与 reason 一并回正；
    ③ 尾斜杠用例回到 host+"/pkg/agentkit/"；带 ".." 用例回到 host+"/pkg/agentkit/../logic"；
    ④ 抽取布局最后一条（reason「抽取后：仓库布局路径已不再是框架子树」）的 path 回到 host+"/pkg/agentkit/model"；
    ⑤ 抽取布局 extractedSubtree 保持 host+"/agentkit" 不变；
    ⑥ 用例条数保持 23 条子用例（仓库布局 17 + 抽取布局 6），不增不删；
    ⑦ 改完用 md5sum 重算 guard_test.go 并同步 GATE-PINS.md5 对应行（其余行不动）。
    禁止：顺手改任何 make/断言、禁止放宽 inside 期望值、禁止删用例。
  </action>
  <verify>
    go test ./... && python3 tools/agentkit_code_pins.py && test "$(md5sum guard_test.go | awk '{print $1}')" = "$(grep ' guard_test.go$' GATE-PINS.md5 | awk '{print $1}')"
  </verify>
  <done>AC-5 与 AC-6 同时成立：go test ./... 退出码 0，钉表缺口 0 且哈希一致</done>
  <depends_on></depends_on>
  <auto>true</auto>
  <!-- 专家团投票：3/3 🤖 自动化（R13.2）：边界为纯测试夹具、verify 可机判、无破坏性变更 -->
</task>

<task id="T02" parallel="true" status="done">
  <name>四件套状态与根因描述互链一致</name>
  <read_files>
    <参考边界>
    <STATE.md>
    <.specs/CONTEXT.md>
    <.specs/agentkit-framework/PRODUCT-DESIGN.html>
    <.specs/agentkit-framework/CHANGE.md>
  </read_files>
  <write_files>
    <修改边界>
    <STATE.md>
    <.specs/CONTEXT.md>
    <.specs/agentkit-framework/PRODUCT-DESIGN.html（仅状态标与背书行，正文不动）>
  </write_files>
  <action>
    ① STATE.md：活跃 change 记为 agentkit-framework，当前阶段随产出推进（REQUIREMENT 门禁已过 → TASK 门禁已过 → DEV 执行中/完成），阻塞表移除"根守卫常驻红"并记入决策日志；
    ② CONTEXT.md §8.1：把根因描述由"两布局常量塌缩 / 1 行常量"更正为事实——6bbec97 把 /pkg/agentkit 批量替换成 /agentkit，连累 8 处夹具字面量（其中一条与自身断言矛盾），修法是恢复字面量、判定式不动；
    ③ PRODUCT-DESIGN.html 已在 2026-10-23 过审（用户原话「好的同意了」），本次只确保状态标与 §0 说明、页脚一致，不重写正文；
    ④ 三处表述必须同一口径：8 处夹具字面量、判定函数一行不改、22 条子用例不变。
  </action>
  <verify>
    bash -c 'grep -q "agentkit-framework" STATE.md && grep -q "8 处" .specs/CONTEXT.md && grep -q "状态：已过审" .specs/agentkit-framework/PRODUCT-DESIGN.html && grep -q "好的同意了" .specs/agentkit-framework/PRODUCT-DESIGN.html && ! grep -q "状态：起草中" .specs/agentkit-framework/PRODUCT-DESIGN.html && echo AC-7/AC-8 ok'
  </verify>
  <done>AC-7 与 AC-8 同时成立</done>
  <depends_on></depends_on>
  <auto>true</auto>
  <!-- 专家团投票：3/3 🤖 自动化（R13.2）：纯文档一致性，无代码与判据改动 -->
</task>
```

<task id="T03" parallel="false" status="done">
  <name>修 360px 视口横向溢出（长路径 code 不可断行）</name>
  <read_files>
    <参考边界>
    <.specs/agentkit-framework/PRODUCT-DESIGN.html（CSS 段）>
  </read_files>
  <write_files>
    <修改边界>
    <.specs/agentkit-framework/PRODUCT-DESIGN.html>
  </write_files>
  <action>
    5-test 第 4 轮用 CDP 量测发现 360px 视口 scrollWidth=377 大于 360；
    逐元素定位（含祖先 overflow-x 过滤）后确认唯一贡献者是一个不可断行的长路径行内 code（右边界 363），
    表格均在 overflow-x:auto 容器内属正常滚动。修复只加一条：
    .doc-body code 允许 overflow-wrap:anywhere（pre code 保持 normal，避免代码块被断开）。
    禁止改任何版面结构、禁止重排表格、禁止引入外部字体。
  </action>
  <verify>
    grep -q "overflow-wrap:anywhere" .specs/agentkit-framework/PRODUCT-DESIGN.html && echo T03-css-ok
    （四视口溢出量测结果见 TEST.md 第 4 轮表格：360/768/1024/1440 均 overflow=False）
  </verify>
  <done>AC-1 不回归；360/768/1024/1440 四视口 document.scrollWidth 不大于 clientWidth（溢出=false）</done>
  <depends_on>T01</depends_on>
  <auto>true</auto>
</task>

<task id="T04" parallel="false" status="done">
  <name>无障碍补强：reduced-motion 降级 + 可见焦点环</name>
  <read_files>
    <参考边界>
    <.specs/agentkit-framework/PRODUCT-DESIGN.html（CSS 段）>
    <code-kit/reference/ui-anti-patterns.md（动效类 / 无障碍快检）>
  </read_files>
  <write_files>
    <修改边界>
    <.specs/agentkit-framework/PRODUCT-DESIGN.html>
  </write_files>
  <action>
    6-review 第三轮命中两项：① 有 scroll-behavior:smooth 但无 prefers-reduced-motion 降级；
    ② 全篇无 :focus-visible 规则，键盘焦点回落到浏览器默认蓝。各补一条：
    @media (prefers-reduced-motion: reduce){html{scroll-behavior:auto}}；
    :focus-visible{outline:2px solid var(--brand);outline-offset:2px}（跟随品牌色，不用默认蓝）。
    禁止改动版面结构、颜色 token、断点。
  </action>
  <verify>
    grep -q "prefers-reduced-motion" .specs/agentkit-framework/PRODUCT-DESIGN.html && grep -q "focus-visible" .specs/agentkit-framework/PRODUCT-DESIGN.html && echo T04-ok
  </verify>
  <done>两条 CSS 生效，四视口量测仍 overflow=False（360/768/1024/1440）</done>
  <depends_on>T03</depends_on>
  <auto>true</auto>
</task>

<task id="T05" parallel="false" status="done">
  <name>夹具自检：断言两套布局常量不同值（防 6bbec97 复发）</name>
  <read_files>
    <参考边界>
    <guard_test.go（TestGuard_AgentkitImportClassification）>
    <GATE-PINS.md5>
  </read_files>
  <write_files>
    <修改边界>
    <guard_test.go>
    <GATE-PINS.md5>
  </write_files>
  <action>
    6-review 第二轮 R2（变更传播）发现：repoSubtree 与 extractedSubtree 仍可能被下一次批量替换
    再次塌缩成同值，而本用例只会以「want false got true」的形式模糊报错。
    在用例开头加 3 行自检（常量相等即 t.Fatalf 并打印值），让该回归形态一眼可读。
    只加断言，不改任何期望值、不增删子用例（RUN 行仍为 24）。
  </action>
  <verify>
    go test -count=1 -run TestGuard_AgentkitImportClassification . && gofmt -l guard_test.go && python3 tools/agentkit_code_pins.py
  </verify>
  <done>23 条子用例仍全 PASS；gofmt 无输出；钉表缺口 0 且哈希同步</done>
  <depends_on>T01</depends_on>
  <auto>true</auto>
</task>

---

## 🛡️ Task 门禁（R9 / R13 · 已执行）

🗳️ **议题 1 · 任务拆分质量**

- 🟫 **工程效能专家**: ✅ 两个 task 粒度对称（一个代码侧、一个文档侧），文件集不相交，同波次并行是最优；无空转任务。
- 🟦 **架构师**: ✅ 覆盖 CHANGE 的全部 v1 动作；「判定式一行不改」作为硬边界写进 action 与 verify，未遗漏架构约束。
- 🟩 **研发负责人**: ✅ `write_files` 精确到 4 个文件，`read_files` 指明了核对 `6bbec97` 的路径；工时各 ≤ 15 分钟。
- 🔴 **资深测试工程师**: ✅ `verify` 可执行且可机判（`go test` 退出码、钉表缺口、md5 相等、grep 命中）；`done` 直接映射 AC-5/6 与 AC-7/8。

**结果: 4/4 全票通过**

🗳️ **议题 2 · 逐 Task 自动化策略**

- **T01**：🟦 研发负责人 🤖（边界清晰、纯夹具）· 🟩 资深测试工程师 🤖（verify 可机判）· 🔴 安全审计师 🤖（不改判定式、不放宽期望值）→ **3/3 🤖 自动化**
- **T02**：🟦 研发负责人 🤖（纯文档）· 🟩 资深测试工程师 🤖（grep 可判）· 🔴 安全审计师 🤖（无敏感操作）→ **3/3 🤖 自动化**

**汇总：T01🤖 T02🤖 → ✅ 自动进入 `4-dev`**
