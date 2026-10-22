#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""钉定集 == agentkit 代码全量（用户裁决 2026-10-16：「铆钉的代码是 agentkit，这是唯一事实」）。

为什么需要这个工具：钉定是这套门禁的事实基线，但它过去只被 `verify_closure` 当作
"报告引用的工件是否都在"来核，**没人反过来问过"框架的代码是不是全被钉住了"**。
漏钉一个 .go 文件 = 那个文件可以随便改而门禁不红 ⇒ "唯一事实源"就成了空话。

口径（按用户裁决）：
  · 被钉的**必须**是代码：`pkg/agentkit/**/*.go`（实现 + 测试）一个不能少，也不许有幽灵条目。
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

# 布局自识别：本文件恒位于 <框架根>/tools/ ⇒ 框架根 = 上溯两层；仓库根 = 再向
# 上找第一个含 go.mod 的目录。宿主布局与独立布局（框架即仓库根）共用一份代码。
KIT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def _find_repo_root(start):
    d = start
    while True:
        if os.path.isfile(os.path.join(d, "go.mod")):
            return d
        parent = os.path.dirname(d)
        if parent == d:
            raise SystemExit("从 %s 上溯不到含 go.mod 的仓库根" % start)
        d = parent


ROOT = _find_repo_root(KIT)
KITREL = os.path.relpath(KIT, ROOT).replace(os.sep, "/")
if KITREL == ".":
    KITREL = ""

# disk_go() 写、main() 读：被豁免的接入方接线文件（避免静默吞掉，必须打印出来）。
DISK_SKIPPED = []
if not os.path.isfile(os.path.join(ROOT, "go.mod")):
    sys.stderr.write("ROOT 推导错误：%s 下没有 go.mod\n" % ROOT)
    raise SystemExit(2)
# 钉定表位置：宿主侧在规范目录下；框架被单独取走时（无规范目录）落在仓库根。
# 也可用环境变量 AGENTKIT_PINS 显式指定。
def _pins_path():
    env = os.environ.get("AGENTKIT_PINS")
    if env:
        return env
    spec = os.path.join(ROOT, ".specs", "agent-engine", "GATE-PINS.md5")
    return spec if os.path.isfile(spec) else os.path.join(ROOT, "GATE-PINS.md5")


PINS = _pins_path()


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
            if path.endswith(".go") and (not KITREL or path.startswith(KITREL + "/")) \
                    and not is_consumer_wiring(path):
                got.add(path)
    if total == 0:
        sys.stderr.write("钉定文件一行都没解析出来 ⇒ 格式漂移，判据不可信\n")
        raise SystemExit(2)
    return got


def is_consumer_wiring(rel):
    """`cmd/**` 下的 `package main` 属**接入方接线**，不是框架冻结实现面。

    与 `guard_test.go` 的 `guardIsConsumerWiring` 同一口径：独立布局下框架根==仓库根，
    样例程序就落在 `cmd/` 里，不豁免等于把「一个示例」钉进框架实现基线（改样例会让
    框架门禁变红，这是错位的因果）。宿主布局里 kit 是 `pkg/agentkit/` 子树，`cmd/` 本来
    就在前缀过滤之外 ⇒ 本函数恒不命中，行为不变。
    有牙齿：`cmd/` 下**非** main 包不豁免（防止有人往 cmd/ 里塞框架代码绕过钉表）。
    """
    parts = rel.split("/")
    if len(parts) < 3 or parts[0] != "cmd":
        return False
    try:
        with io.open(os.path.join(ROOT, rel), encoding="utf-8") as f:
            for line in f:
                text = line.strip()
                if not text or text.startswith("//"):
                    continue
                return text.startswith("package main")
    except (IOError, OSError):
        return False
    return False


def disk_go():
    """磁盘上的 agentkit .go 路径集合（实现 + 测试；接入方接线 main 除外）。"""
    out = set()
    skipped = []
    for dirpath, dirs, files in os.walk(KIT):
        dirs[:] = [d for d in dirs if d != ".git"]   # 独立布局：框架根==仓库根
        for fn in files:
            if not fn.endswith(".go"):
                continue
            rel = os.path.relpath(os.path.join(dirpath, fn), ROOT)
            if is_consumer_wiring(rel):
                skipped.append(rel)
                continue
            out.add(rel)
    DISK_SKIPPED[:] = skipped
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
    for dirpath, dirs, files in os.walk(KIT):
        dirs[:] = [d for d in dirs if d != ".git"]   # 独立布局：框架根==仓库根
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
    if DISK_SKIPPED:
        print("  接入方接线不钉（cmd/ 下 package main，与守卫同口径）：%s" % ", ".join(sorted(DISK_SKIPPED)))
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
