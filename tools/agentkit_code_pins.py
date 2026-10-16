#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""钉定集 == agentkit 代码全量（用户裁决 2026-10-16：「铆钉的代码是 agentkit，这是唯一事实」）。

为什么需要这个工具：钉定是这套门禁的事实基线，但它过去只被 `verify_closure` 当作
"报告引用的工件是否都在"来核，**没人反过来问过"框架的代码是不是全被钉住了"**。
漏钉一个 .go 文件 = 那个文件可以随便改而门禁不红 ⇒ "唯一事实源"就成了空话。

口径（按用户裁决）：
  · 被钉的**必须**是代码：`**/*.go`（实现 + 测试）一个不能少，也不许有幽灵条目。
  · 文档（README / docs/*.md / 原型 HTML）是**派生物**，刻意不钉 —— 它们是代码的下游，
    钉它们只会制造"文档改了要重钉"的假信号，并把事实源次序颠倒。本工具把未钉文档
    列成信息行，**不计入缺口**。

退出码：0 = 完全一致；1 = 有缺口（打印具体文件）；2 = 量具本身不可用（钉定文件缺失/格式漂移）。
判据性接入：`gate_report.py` 的 `agentkit-code-pins` 检查（要求 rc==0 且输出以 `缺口=0` 结尾）。
"""
import io
import os
import subprocess
import sys

# 本文件位于 tools/ ⇒ 上溯 4 层到仓库根
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if not os.path.isfile(os.path.join(ROOT, "go.mod")):
    sys.stderr.write("ROOT 推导错误：%s 下没有 go.mod\n" % ROOT)
    raise SystemExit(2)
PINS = os.path.join(ROOT, ".specs", "agent-engine", "GATE-PINS.md5")
KIT = os.path.join(ROOT)


def pinned_go():
    """钉定文件里的 agentkit .go 路径集合。"""
    if not os.path.isfile(PINS):
        sys.stderr.write("钉定文件不存在：%s\n" % PINS)
        raise SystemExit(2)
    got = set()
    total = 0
    with io.open(PINS, encoding="utf-8") as f:
        for line in f:
            parts = line.split()
            if len(parts) != 2:
                continue          # 注释/空行容忍，路径行必须是 "hash path"
            total += 1
            path = parts[1].replace("./", "")
            if path.startswith("") and path.endswith(".go"):
                got.add(path)
    if total == 0:
        sys.stderr.write("钉定文件一行都没解析出来 ⇒ 格式漂移，判据不可信\n")
        raise SystemExit(2)
    return got


def disk_go():
    """磁盘上的 agentkit .go 路径集合（实现 + 测试，不排除任何 .go）。"""
    out = set()
    for dirpath, _dirs, files in os.walk(KIT):
        for fn in files:
            if fn.endswith(".go"):
                out.add(os.path.relpath(os.path.join(dirpath, fn), ROOT))
    if not out:
        sys.stderr.write("没扫到任何 agentkit .go 文件 ⇒ 目录搬了？判据不可信\n")
        raise SystemExit(2)
    return out


def disk_docs():
    """磁盘上未钉的 agentkit 非代码文件（信息用，不算缺口）。"""
    pinned_any = set()
    with io.open(PINS, encoding="utf-8") as f:
        for line in f:
            parts = line.split()
            if len(parts) == 2:
                pinned_any.add(parts[1].replace("./", ""))
    miss = set()
    for dirpath, _dirs, files in os.walk(KIT):
        for fn in files:
            if fn.endswith(".go"):
                continue
            rel = os.path.relpath(os.path.join(dirpath, fn), ROOT)
            if rel not in pinned_any:
                miss.add(rel)
    return miss


def main():
    pin, disk = pinned_go(), disk_go()
    unpinned = sorted(disk - pin)     # 代码存在但没钉 ⇒ 可被偷改而门禁不红
    ghost = sorted(pin - disk)        # 钉了但文件不存在 ⇒ 钉定集里有幽灵
    gap = len(unpinned) + len(ghost)
    print("agentkit .go：磁盘 %d / 已钉 %d" % (len(disk), len(pin)))
    for x in unpinned:
        print("  未钉定的代码（必须 --freeze 或补钉）：%s" % x)
    for x in ghost:
        print("  钉定里存在但磁盘无此文件（漂移）：%s" % x)
    docs = sorted(disk_docs())
    if docs:
        print("  派生物不钉（按裁决，非缺陷）：%s" % ", ".join(docs))
    print("缺口=%d" % gap)
    return 0 if gap == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
