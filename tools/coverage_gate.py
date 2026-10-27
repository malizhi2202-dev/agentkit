#!/usr/bin/env python3
"""覆盖率门槛：每个有语句的包都必须 ≥ MIN，否则退出非 0。

用法：
  go test -coverprofile=/tmp/cover.out ./...
  python3 tools/coverage_gate.py /tmp/cover.out [--min 90]

为什么按包而不是按总量：总覆盖率会被大包稀释，一个小包整体裸奔也能达标。
"""
import sys
from collections import defaultdict

MIN_DEFAULT = 90.0
MODULE_PREFIX = "github.com/malizhi2202-dev/agentkit"


def parse(path):
    """返回 {包名: (语句数, 命中语句数)}；包名取模块内相对路径，根包记为 (root)。"""
    rows = defaultdict(lambda: [0, 0])
    with open(path, encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if not line or line.startswith("mode:"):
                continue
            # file:startLine.startCol,endLine.endCol numStmt count
            loc, stmts, count = line.rsplit(" ", 2)
            file_path = loc.rsplit(":", 1)[0]
            pkg = file_path.rsplit("/", 1)[0] if "/" in file_path else "(root)"
            pkg = pkg.replace(MODULE_PREFIX + "/", "")
            stmts, count = int(stmts), int(count)
            rows[pkg][0] += stmts
            rows[pkg][1] += stmts if count > 0 else 0
    return rows


def main(argv):
    if len(argv) < 2:
        print(__doc__)
        return 2
    path = argv[1]
    min_pct = MIN_DEFAULT
    if "--min" in argv:
        min_pct = float(argv[argv.index("--min") + 1])

    rows = parse(path)
    if not rows:
        print("[coverage] 输入为空，无法判定")
        return 2

    failed, skipped = [], []
    print(f"[coverage] 门槛 {min_pct:.1f}% · 包数 {len(rows)}")
    for pkg in sorted(rows):
        total, hit = rows[pkg]
        if total == 0:
            skipped.append(pkg)
            continue
        pct = hit * 100.0 / total
        flag = "OK " if pct >= min_pct else "LOW"
        print(f"  {flag} {pkg:<24} {pct:6.1f}%  ({hit}/{total})")
        if pct < min_pct:
            failed.append((pkg, pct))

    if skipped:
        print(f"[coverage] 跳过无语句包：{', '.join(sorted(skipped))}")
    if failed:
        print(f"[coverage] FAIL：{len(failed)} 个包低于门槛")
        for pkg, pct in failed:
            print(f"  - {pkg}: {pct:.1f}% < {min_pct:.1f}%")
        return 1
    print("[coverage] PASS：全部包达到门槛")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
