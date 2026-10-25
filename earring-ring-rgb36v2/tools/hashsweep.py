#!/usr/bin/env python3
"""Podcast-wide sweep of the hash field thresholds.

Seds the constants in syncdsp/consts.go, runs TestPodcastHash over the whole
podcast (PODCAST env, default /tmp/psync/full.wav), records the HEADLINE
usable-bits + p95 for every device model, restores consts.go at the end.

Usage:
  hashsweep.py one NAME VALUE            # single value, print line
  hashsweep.py sweep NAME v1,v2,v3       # 1-D sweep
  hashsweep.py grid                       # predefined small grid (1-D first)

Names: HotFloorQ8 ShapeLoQ8 ShapeHiQ8 TemporalDeadband
"""
import re
import subprocess
import sys
import os

HERE = os.path.dirname(os.path.abspath(__file__))
PKG = os.path.join(HERE, "..")
CONSTS = os.path.join(PKG, "syncdsp", "consts.go")
ORIG = open(CONSTS).read()
POD = os.environ.get("PODCAST", "/tmp/psync/full.wav")


def set_const(text, name, value):
    # Replace the whole RHS expression (e.g. "3 << 8" or "128"), keeping any
    # trailing comment. Non-greedy up to a comment or end of line.
    pat = re.compile(rf"^(\s*{name}\s*=\s*)(.*?)(\s*//.*)?$", re.M)
    out, n = pat.subn(lambda m: m.group(1) + str(value) + (m.group(3) or ""), text)
    assert n == 1, f"{name}: {n} matches"
    return out


def run():
    env = dict(os.environ, PODCAST=POD)
    r = subprocess.run(
        ["go", "test", "-count=1", "-run", "TestPodcastHash", "-v", "./syncdsp/"],
        cwd=PKG, env=env, capture_output=True, text=True, timeout=600)
    out = r.stdout + r.stderr
    hl = re.search(r"HEADLINE usable-bits\(worst=gain\+tilt\)=([\d.]+)\s+p95=(\d+)", out)
    per = dict(re.findall(r"(gain-6dB|hf-tilt|gain\+tilt|quiet|bench)\s+usable=([\d.]+)", out))
    modal = re.search(r"modal [0-9a-f]{8} = (\d+)%", out)
    if not hl:
        print(out[-2000:])
        raise SystemExit("no HEADLINE - build or test failure")
    return float(hl.group(1)), int(hl.group(2)), per, int(modal.group(1) if modal else -1)


def trial(*pairs):
    # pairs: NAME VALUE, NAME VALUE, ...  (sys.argv[2:])
    text = ORIG
    label = " "
    for i in range(0, len(pairs), 2):
        text = set_const(text, pairs[i], pairs[i + 1])
        label += f"{pairs[i]}={pairs[i + 1]} "
    open(CONSTS, "w").write(text)
    mi, p95, per, modal = run()
    print(f"{label:<44} usable={mi:.2f} p95={p95} modal={modal}%  "
          + " ".join(f"{k}:{v}" for k, v in sorted(per.items())), flush=True)
    return mi, p95, modal


def main():
    try:
        cmd = sys.argv[1]
        if cmd == "one":
            trial(*sys.argv[2:])
        elif cmd == "sweep":
            # sweep NAME v1,v2,... [BASE name=val ...]
            name, vals = sys.argv[2], sys.argv[3].split(",")
            base = sys.argv[4:]
            assert len(base) % 2 == 0
            for v in vals:
                trial(name, v, *base)
    finally:
        open(CONSTS, "w").write(ORIG)  # noqa: restore always


if __name__ == "__main__":
    main()
