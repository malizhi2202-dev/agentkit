#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""J-PORT：仓外陌生 module 取走实验。

为什么值得做成判据而不是一次性手工活：`pkg/agentkit` 的全部卖点是"别的程序拿去就能搭"，
而"拿得走"这件事最容易在重构里悄悄破 —— 有人在框架包里 import 了宿主包（编译在仓内仍然过）、
或者某个测试依赖仓内文件路径、或者样例模板引用了改过的前缀。这些在 `go test ./pkg/agentkit/...`
里**一个都不会红**，只有在 module 名被换掉的临时仓里才暴露。故本轮起它是一条判据性检查。

四条红线（脚本自己也要守）：
  * **不联网**：`GOPROXY=off`。联网就等于承认"取走还要拉依赖"。
  * **不改宿主仓库**：`GOFLAGS=-mod=readonly`，全部工作在临时目录里，`go.mod`/`go.sum` 只复制不编辑。
  * **不许静默通过**：任何一步失败 ⇒ 退出码非 0；`go` 不在位也**判红**（门禁前置 `verify_bins()` 本来就要求 go
    在位，这里若给一条"跳过即绿"的路，就是把红检查洗成绿的自授权通道，见 LESSONS 既有教训 规则 4）。
  * **样例必须真跑**：`go run` 的样例自己带十段逐行自校验（失败会返回非 0），所以这一步同时是"接口即工具 +
    语义驱动闭环"的端到端判据，不只是编译检查。
"""

import os
import re
import shutil
import subprocess
import sys
import tempfile

# 布局自识别：本文件恒位于 <框架根>/tools/ ⇒ 框架根 = 上溯两层；仓库根 = 再向
# 上找第一个含 go.mod 的目录。宿主布局（框架在 <repo>/pkg/agentkit）与独立布局
# （框架即仓库根）共用一份代码：层数写死过一次，代价是取走实验先假绿后假红。
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
# KITREL：框架在仓库中的相对路径；独立布局下为 ""（框架根 == 仓库根）。
KITREL = os.path.relpath(KIT, ROOT).replace(os.sep, "/")
if KITREL == ".":
    KITREL = ""
SAMPLE_LAYOUT = "pkg/agentkit"   # 样例模板作者所假设的布局（宿主布局）
KIT_LABEL = KITREL if KITREL else os.path.basename(KIT)   # 人话里的框架位置，按布局给
def _module_prefix():
    """宿主 module 前缀从 go.mod 现读，不硬编码任何一家宿主（通用包要求）。"""
    with open(os.path.join(ROOT, "go.mod"), encoding="utf-8") as f:
        for line in f:
            if line.startswith("module "):
                return line.split()[1].strip()
    raise SystemExit("go.mod 无 module 声明，取走实验无法进行")

HOST_PREFIX = _module_prefix()   # 宿主 module 前缀（现读）
FOREIGN_PREFIX = "example.org/otherapp"                        # 冒充"另一个项目的 module"
# 框架的 import 前缀：宿主布局 = <module>/pkg/agentkit；独立布局 = <module>。
SRC_IMP = HOST_PREFIX + ("/" + KITREL if KITREL else "")
DST_IMP = FOREIGN_PREFIX + ("/" + KITREL if KITREL else "")
SAMPLE = os.path.join(KIT, "samples", "agentkit-demo.go.txt")
KITSRC = KIT

# ROOT 推导错（多/少一层）必须拒绝运行：否则 copytree 到错目录，判据会假绿
if not (os.path.isfile(os.path.join(ROOT, "go.mod")) and os.path.isdir(KITSRC)):
    print("❌ ROOT 推导错误（%s）：应含 go.mod，且框架根 %s 存在" % (ROOT, KITSRC))
    raise SystemExit(2)

# 样例里每一段闭环各自打印一行 ✅ 判据。**逐段列名**而不是"数够 N 个就行"：
# 阈值式断言抓不住"少跑了一段但总数凑够"，也抓不住重复打印同一行来凑数（既有教训 规则 4 同源问题）。
REQUIRED_MARKERS = [
    "接口/工具/权限同源",          # ① 一张表两用
    "黑名单在投递前生效",          # ② 审计出域前必过 Masker
    "HTTP 接口被 agent 通过语义层调用",  # ③ 接口即工具
    "语义层只选择与装配",          # ⑤ 语义 → 计划
    "裁剪只问 tools 那一条谓词",   # ⑥ 场景按角色裁
    "场景表不抄角色",              # ⑥ 形状削减
    "剧本层：预演零写入",          # ⑦ 剧本
    "模型配置层：三级顺序唯一",    # ⑧ 模型配置
    "Stop 即优雅关闭",
    "会话层：装配是纯函数",  # ⑩ 会话装配            # ④ 停止即收口
]

# 陌生 module 内必须真跑的四步（少一步就是"只编译就宣布可移植"）。
GO_STEPS = ["go build ./...", "go vet ./...", "go test -count=1 ./...", "go test -race -count=1 ./..."]

QUOTED_HOST_IMPORT = re.compile(r'"' + re.escape(HOST_PREFIX) + r'[^"]*"')


def selftest():
    """判据自身的防伪，不依赖 go（放进步骤 0 与 --selftest）。"""
    hits = QUOTED_HOST_IMPORT.findall('import "x\n\t"%s/宿主既有实现"' % HOST_PREFIX)
    if not hits:
        print("  ❌ 自我防伪失败：正则抓不到引号形态的宿主 import")
        return 1
    prose = "// 不得 import `%s/...` 包（设计文档章节）" % HOST_PREFIX
    if QUOTED_HOST_IMPORT.findall(prose):
        print("  ❌ 自我防伪失败：把注释里的禁令原文误判成 import（会误伤三处包文档）")
        return 1
    if not os.path.isfile(SAMPLE):
        print("  ❌ 自我防伪失败：样例模板缺失 %s" % SAMPLE)
        return 1
    text = open(SAMPLE, encoding="utf-8").read()
    missing = [m for m in REQUIRED_MARKERS if m not in text]
    if missing:
        print("  ❌ 自我防伪失败：样例里没有这些判据行（某段被删了还在要求输出）：%s" % "、".join(missing))
        return 1
    print("  ✅ 自我防伪通过（正则抓得住真耦合、不误伤注释；%d 段判据行都在样例里）" % len(REQUIRED_MARKERS))
    return 0


def step(no, name):
    print("  [%s] %s" % (no, name))


def run(cmd, cwd, env):
    p = subprocess.Popen(cmd, cwd=cwd, env=env, stdout=subprocess.PIPE,
                         stderr=subprocess.STDOUT)
    out = p.communicate()[0].decode("utf-8", "replace")
    return p.returncode, out


def fail(msg, out=""):
    print("  ❌ %s" % msg)
    tail = "\n".join(out.strip().splitlines()[-25:])
    if tail:
        print("     ── 输出尾部 ──")
        for line in tail.splitlines():
            print("     %s" % line[:170])
    sys.exit(1)


def main():
    if "--selftest" in sys.argv:
        sys.exit(selftest())
    if selftest() != 0:
        fail("判据自身防伪未通过")
    go = shutil.which("go")
    if not go:
        # 判红而不是跳过：见文件头第三条红线。
        fail("PATH 里找不到 go ⇒ 无法验证可移植性（本检查不许静默通过）")

    probe = subprocess.Popen([go, "version"], stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    print("  工具链：%s" % probe.communicate()[0].decode("utf-8", "replace").strip())

    tmp = tempfile.mkdtemp(prefix="agentkit-portable-")
    try:
        dst = os.path.join(tmp, FOREIGN_PREFIX.split("/")[1])
        if KITREL:            # 宿主布局：先建 dst，框架复制进 dst/pkg/agentkit
            os.makedirs(dst)
        # 独立布局：框架根 == 仓库根，dst 交给 copytree 建（预建会 FileExistsError）
        step("1/5", "复制框架包与 go.mod/go.sum 到临时 module（只复制，不编辑宿主文件）")
        kitdst = os.path.join(dst, KITREL) if KITREL else dst
        shutil.copytree(KITSRC, kitdst, ignore=shutil.ignore_patterns(".git"))
        for f in ("go.mod", "go.sum"):
            src = os.path.join(ROOT, f)
            if not os.path.isfile(src):
                fail("缺少 %s，无法构造等价 module" % f)
            shutil.copy2(src, os.path.join(dst, f))

        step("1.5/5", "随包工件齐全（框架不只是 .go：文档、样例、判据工具要能一起取走）")
        need = [("README.md",), ("docs", "architecture.md"), ("docs", "design.md"), ("docs", "guide.md"),
                ("samples", "agentkit-demo.go.txt"),
                ("tools", "portability_check.py"), ("tools", "agentkit_code_pins.py")]
        lack = ["/".join(x) for x in need if not os.path.isfile(os.path.join(kitdst, *x))]
        if lack:
            fail("搬走的框架缺工件（文档与判据工具必须随包）：%s" % ", ".join(lack))
        print("       ✅ 随包工件 %d 个全部到位（README + docs/ + samples/ + tools/）" % len(need))

        step("2/5", "改写 module 前缀（模拟别的程序 import 路径）")
        mod = os.path.join(dst, "go.mod")
        lines = open(mod, encoding="utf-8").read().splitlines(True)
        if not lines or not lines[0].startswith("module "):
            fail("go.mod 第一行不是 module 声明：%r" % (lines[0] if lines else ""))
        lines[0] = "module %s\n" % FOREIGN_PREFIX
        open(mod, "w", encoding="utf-8").write("".join(lines))
        rewritten = 0
        for dirpath, _dirs, files in os.walk(kitdst):
            for f in files:
                if not f.endswith(".go"):
                    continue
                p = os.path.join(dirpath, f)
                data = open(p, encoding="utf-8").read()
                new = data.replace(SRC_IMP, DST_IMP)
                if new != data:
                    open(p, "w", encoding="utf-8").write(new)
                    rewritten += 1
        if rewritten == 0:
            fail("一个 import 都没改写 ⇒ 复制过来的包根本不引用 agentkit（改错目录或前缀漂移）")
        print("       改写 %d 个 .go 文件" % rewritten)
        # 残留判据盯的是**引号形态**的宿主路径（= import 或字符串常量）。
        # 不能要求"零文本出现"：`guard_test.go`、`agent/result.go`、`model/protocol.go` 的包文档里
        # 就用反引号写着这条禁令本身（"不得 import <宿主 module 前缀>/…"），那种出现恰恰是**正确**的。
        leftover = []
        for dirpath, _dirs, files in os.walk(kitdst):
            for f in files:
                if not f.endswith(".go"):
                    continue
                p = os.path.join(dirpath, f)
                for m in QUOTED_HOST_IMPORT.findall(open(p, encoding="utf-8").read()):
                    leftover.append("%s: %s" % (os.path.relpath(p, dst), m[:70]))
        if leftover:
            fail("仍有 %d 处引号形态的宿主 import（真耦合，编译期就出域）：%s"
                 % (len(leftover), "; ".join(leftover[:5])))
        print("       ✅ 无引号形态宿主 import（注释里的禁令原文不算）")

        step("3/5", "放入样例 main（真实消费者的形态，含十段自校验）")
        if not os.path.isfile(SAMPLE):
            fail("样例模板缺失：%s" % SAMPLE)
        if HOST_PREFIX in open(SAMPLE, encoding="utf-8").read():
            fail("样例模板里还写着宿主前缀 ⇒ 它在冒充陌生 module 这点是假的")
        os.makedirs(os.path.join(dst, "cmd", "agentkit-demo"))
        sample_txt = open(SAMPLE, encoding="utf-8").read()
        if KITREL != SAMPLE_LAYOUT:   # 独立布局：样例里的 <foreign>/pkg/agentkit 段换算成实际布局
            sample_txt = sample_txt.replace(FOREIGN_PREFIX + "/" + SAMPLE_LAYOUT, DST_IMP)
        open(os.path.join(dst, "cmd", "agentkit-demo", "main.go"), "w", encoding="utf-8").write(sample_txt)

        env = dict(os.environ)
        env["GOPROXY"] = "off"           # 不联网
        env["GOFLAGS"] = "-mod=readonly"  # 不许改 go.mod/go.sum

        step("4/5", "build + vet + test + test -race（陌生 module 内）")
        done = []
        for cmd in (["go", "build", "./..."],
                    ["go", "vet", "./..."],
                    ["go", "test", "-count=1", "./..."],
                    ["go", "test", "-race", "-count=1", "./..."]):
            rc, out = run(cmd, dst, env)
            if rc != 0:
                fail("陌生 module 内 `%s` 失败" % " ".join(cmd), out)
            done.append(" ".join(cmd))
            print("       ✅ %s" % " ".join(cmd))
        if done != GO_STEPS:
            fail("go 步骤与清单不一致（少一步就是「只编译就宣布可移植」）：实得 %s" % done)

        step("5/5", "go run 样例（端到端：接口即工具、语义驱动、场景/剧本/模型配置十段自校验）")
        rc, out = run(["go", "run", "./cmd/agentkit-demo"], dst, env)
        if rc != 0:
            fail("样例运行失败（exit=%d）" % rc, out)
        missing = [m for m in REQUIRED_MARKERS if m not in out]
        if missing:
            fail("样例缺 %d/%d 段判据行：%s" % (len(missing), len(REQUIRED_MARKERS), "、".join(missing)), out)
        for bad in ("panic", "goroutine"):
            if re.search(r"\b%s\b" % bad, out):
                fail("样例输出里出现 %r" % bad, out)
        print("       ✅ 样例 %d 段判据行逐段命中" % len(REQUIRED_MARKERS))
        print("  ✅ J-PORT 通过：%s 可在陌生 module 内构建、测试、并跑出可运行的样例" % KIT_LABEL)
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    main()
