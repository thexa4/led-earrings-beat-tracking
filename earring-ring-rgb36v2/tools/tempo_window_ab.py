#!/usr/bin/env python3
"""Tempo-window experiment: does a longer correlation window make the chosen
period robust to nuisance variation in the acoustic channel?

The dominant run-to-run disagreement measured on the device is the tempo itself
(period 26 vs 27 on the same clip, 5 of 15 run pairs). On the host the same clip
is a near-tie too (evidence 551 vs 482), and it stays a near-tie for every
capture phase and noise seed - so the estimator cannot resolve it, and any
device-side perturbation picks a side.

This script measures the resolution directly. For each window size it replays a
matrix of bench-channel variants of ONE clip (high-pass corner x noise seed, plus
capture-phase offsets) and reports which period each variant picks and by what
margin. All these files are the same music at the same tempo, so a window that
picks unanimously with a wide margin resolves the tempo; one that splits does
not. The window with the better spread wins, and the device decides after that.

  tempo_window_ab.py [WAV ...]     (default: /tmp/psync/cv_*.wav + ph*.wav)

Restores consts.go and pipeline.go afterwards. Run with no device attached work
in flight: it only rebuilds the host test binary, never flashes.
"""
import glob
import os
import re
import subprocess
import sys

DIR = "/home/max/Documents/led-earrings/debug/earring-ring-rgb36v2"
CONSTS = os.path.join(DIR, "syncdsp/consts.go")
PIPE = os.path.join(DIR, "syncdsp/pipeline.go")

# (window, envRef shift). The envelope baseline is deliberately ~2x the ring, so
# growing the ring grows the reference with it - see the comment at pushEnv.
CONFIGS = [(256, 9), (512, 10)]


def patch(window, shift):
    c = open(CONSTS).read()
    c = re.sub(r"\n(\t)TempoWindow = \d+\n", f"\n\\1TempoWindow = {window}\n", c, count=1)
    open(CONSTS, "w").write(c)
    p = open(PIPE).read()
    p = re.sub(r"p\.envRef \+= \(total - p\.envRef\) >> \d+",
               f"p.envRef += (total - p.envRef) >> {shift}", p, count=1)
    open(PIPE, "w").write(p)


def run(wav):
    env = dict(os.environ, HOSTBEAT=wav)
    out = subprocess.run(["go", "test", "-count=1", "-v", "-run", "TestReplayWav", "./syncdsp/"],
                         cwd=DIR, capture_output=True, text=True, env=env).stdout
    per = re.search(r"frames=\d+.*period=(\d+)", out)
    ev = re.search(r"ev: ((?:\d+:\d+ ?){1,3})", out)
    if not per or not ev:
        return None
    cands = [tuple(int(x) for x in c.split(":")) for c in ev.group(1).split()]
    top = dict((p, v) for p, v in cands)
    margin = 0.0
    if len(cands) > 1 and cands[0][1]:
        margin = 100.0 * (cands[0][1] - cands[1][1]) / cands[0][1]
    return int(per.group(1)), cands[0][1], margin, top.get(int(per.group(1)), 0)


def main():
    files = sys.argv[1:] or sorted(glob.glob("/tmp/psync/cv_*.wav") + glob.glob("/tmp/psync/ph*.wav"))
    orig = (open(CONSTS).read(), open(PIPE).read())
    try:
        for window, shift in CONFIGS:
            patch(window, shift)
            print(f"\n=== TempoWindow={window} envRef=>>{shift} ===")
            chosen, margins = [], []
            for f in files:
                r = run(f)
                if r is None:
                    print(f"  {os.path.basename(f):22s} FAILED")
                    continue
                per, ev, margin, evp = r
                chosen.append(per)
                margins.append(margin)
                print(f"  {os.path.basename(f):22s} period={per:<3} ev={ev:<5} "
                      f"margin={margin:5.1f}%  top3={ev} {evp}")
            uniq = sorted(set(chosen))
            import statistics
            print(f"  -> {len(files)} files: choices {uniq}  "
                  f"unanimous={len(uniq) == 1}  margin med "
                  f"{statistics.median(margins):.1f}% min {min(margins):.1f}%")
    finally:
        open(CONSTS, "w").write(orig[0])
        open(PIPE, "w").write(orig[1])
        print("\nconsts.go / pipeline.go restored")


main()
