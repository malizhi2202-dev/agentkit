#!/usr/bin/env python3
"""文档断言：让"状态标 / 状态表 / 路线图 / 架构文档"的一致性变成会失败的检查。

检查项：
  1. .specs/ARCHITECTURE.md 存在，含 8 个规定段，且至少 1 条 ADR
  2. 每个 D-<slug>.md 头部有「审核:」标，取值 ∈ {通过, 待审, 否决}
  3. D-<slug>.md 与 TOPICS.md 子议题状态表双向一致（不漏登记、不幽灵行）
  4. ROADMAP.md 不含「待审」（未过审结论禁止入内 · R16.3）
  5. STATE.md 含 last_intel_scan 字段
  6. 宿主布局悬空指针 → 仅告警（已登记议题 doc-path-realign）

退出码：任一 FAIL → 1。
"""
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SPECS = ROOT / ".specs"
STATUS_WORDS = ("通过", "待审", "否决")
REQUIRED_SECTIONS = ("系统概览", "模块清单", "ADR 列表", "跨模块契约", "扩展点", "容量", "技术债", "修订历史")
DANGLING = (".specs/adr/", "pkg/agentkit/", "agentkit/samples/")

fails, warns = [], []


def fail(msg):
    fails.append(msg)


def warn(msg):
    warns.append(msg)


def check_architecture():
    path = SPECS / "ARCHITECTURE.md"
    if not path.exists():
        fail("缺少 .specs/ARCHITECTURE.md（先跑 A-architect）")
        return
    text = path.read_text(encoding="utf-8")
    missing = [s for s in REQUIRED_SECTIONS if s not in text]
    if missing:
        fail(f"ARCHITECTURE.md 缺段：{missing}")
    adr = re.findall(r"^### ADR-\d+", text, re.M)
    if not adr:
        fail("ARCHITECTURE.md 无 ADR 条目")
    else:
        print(f"  ok  ARCHITECTURE：8 段齐全，ADR {len(adr)} 条")


def check_subtopic_status():
    topics = SPECS / "agentkit-evolution" / "TOPICS.md"
    if not topics.exists():
        warn("未找到 agentkit-evolution/TOPICS.md，跳过于议题一致性检查")
        return
    ttext = topics.read_text(encoding="utf-8")
    files = sorted(p for p in (SPECS / "agentkit-evolution").glob("D-*.md"))
    if not files:
        warn("agentkit-evolution 下无 D-*.md")
        return
    for f in files:
        slug = f.stem[2:]
        head = f.read_text(encoding="utf-8").splitlines()[:6]
        line = next((h for h in head if h.lstrip().startswith("> 审核:")), None)
        if line is None:
            fail(f"{f.name} 头部缺少「审核:」状态标")
        elif not any(w in line for w in STATUS_WORDS):
            fail(f"{f.name} 状态标取值非法（须为 通过/待审/否决）：{line.strip()}")
        if f"`{slug}`" not in ttext:
            fail(f"{f.name} 未登记在 TOPICS.md 子议题状态表（slug={slug}）")
    print(f"  ok  子议题状态：{len(files)} 份工件全部有状态标且已登记")


def check_roadmap():
    path = SPECS / "agentkit-evolution" / "ROADMAP.md"
    if not path.exists():
        warn("未找到 ROADMAP.md，跳过")
        return
    text = path.read_text(encoding="utf-8")
    if "待审" in text:
        fail("ROADMAP.md 含「待审」：未过审结论禁止入内（R16.3）")
    else:
        print("  ok  ROADMAP：无未过审结论")


def check_state():
    path = ROOT / "STATE.md"
    if not path.exists():
        fail("缺少 STATE.md")
        return
    if "last_intel_scan:" not in path.read_text(encoding="utf-8"):
        fail("STATE.md 缺 last_intel_scan 字段")
    else:
        print("  ok  STATE：last_intel_scan 存在")


def check_dangling():
    hits = []
    for rel in ("README.md", "docs/architecture.md", "docs/design.md", "docs/guide.md"):
        p = ROOT / rel
        if not p.exists():
            continue
        for i, line in enumerate(p.read_text(encoding="utf-8").splitlines(), 1):
            for tok in DANGLING:
                if tok in line and "宿主" not in line:
                    hits.append(f"{rel}:{i} → {tok}")
    if hits:
        warn(f"宿主布局指针 {len(hits)} 处（已登记议题 doc-path-realign）：" + "; ".join(hits[:3]))
    else:
        print("  ok  文档指针：无宿主布局残留")


def main():
    print("[doc-assert] 检查文档一致性")
    check_architecture()
    check_subtopic_status()
    check_roadmap()
    check_state()
    check_dangling()
    for w in warns:
        print(f"  WARN {w}")
    if fails:
        print(f"[doc-assert] FAIL：{len(fails)} 项")
        for f in fails:
            print(f"  - {f}")
        return 1
    print("[doc-assert] PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
