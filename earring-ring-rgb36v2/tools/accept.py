#!/usr/bin/env python3
"""Final acceptance table for selector candidates, scored on every capture.

Selection has to be made on one dataset and confirmed on another, because the two
have disagreed before in this project's history: a 25 minute capture ranked a
33 s time constant well above 16 s and the hour-long capture ranked it lower. Both
can be true at once - the ranking can be consistent while the level is not - so
this prints the pair side by side and sorts by the WORSE of the two, which is the
only honest way to choose a constant that gets compiled.

The four numbers are the user's requirements, not four views of one number:

  agree   fraction of frames showing the same colour ("agree most of the time")
  kappa   the same, minus the agreement the colour mix gives by luck. Raw
          agreement is gameable by using fewer colours - a rule pinned to one
          colour scores 100% - so a candidate that wins on agree alone has not
          necessarily done anything.
  minocc  the rarest colour's share, worst across both devices. The requirement
          is that the earrings are not stuck on one pattern, which is a statement
          about the least-used colour, not about the average.
  ch/min  how often the colour changes. Agreement with no movement is a frozen
          pair, which reads as broken even though it scores well, and a colour
          that changes twice an hour is stuck for anything a person can see.

  python3 tools/accept.py DIR [DIR ...]
"""
import csv
import os
import sys

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import q8fix

FPS = 62.5
NP = q8fix.NUM_PATTERNS

# The candidates still in contention, plus the configuration that ships today so
# the gain is always visible rather than asserted.
CANDS = {
    "old shipped (ramp,16s,c216)": dict(w=q8fix.SHIPPED, q8=False, lp=10,
                                        cut=216, ruler="mad"),
    "b4-b0   33s c150 (flashed)": dict(w=(-7, 0, 0, 0, 7, 0, 0, 0), q8=True,
                                       lp=11, cut=150, ruler="mad"),
    "b45-b01 66s c150": dict(w=(-5, -5, 0, 0, 5, 5, 0, 0), q8=True, lp=12,
                             cut=150, ruler="mad"),
    "b45-b01 66s c190": dict(w=(-5, -5, 0, 0, 5, 5, 0, 0), q8=True, lp=12,
                             cut=190, ruler="mad"),
    "b45-b01 33s c150": dict(w=(-5, -5, 0, 0, 5, 5, 0, 0), q8=True, lp=11,
                             cut=150, ruler="mad"),
    "b0-45   66s c150": dict(w=(-7, 0, 0, 0, 4, 3, 0, 0), q8=True, lp=12,
                             cut=150, ruler="mad"),
    "b4-b0   66s c150": dict(w=(-7, 0, 0, 0, 7, 0, 0, 0), q8=True, lp=12,
                             cut=150, ruler="mad"),
}


def load(path):
    t, b = [], []
    for r in csv.DictReader(open(path)):
        t.append(float(r["t"]))
        b.append([float(r[f"b{i}"]) for i in range(8)])
    t = np.asarray(t)
    o = np.argsort(t)
    return t[o], np.asarray(b, dtype=np.float64)[o]


def grid(d):
    ts, bs = zip(*(load(os.path.join(d, f"state_{s}.csv")) for s in ("L", "R")))
    lo, hi = max(x[0] for x in ts), min(x[-1] for x in ts)
    tg = np.arange(lo, hi, 1.0 / FPS)
    return [b[np.clip(np.searchsorted(t, tg), 0, len(t) - 1)][:len(tg)]
            for t, b in zip(ts, bs)]


def stats(a, b, warm):
    a, b = a[warm:], b[warm:]
    po = float(np.mean(a == b))
    pe = sum(np.mean(a == k) * np.mean(b == k) for k in range(NP))
    kap = (po - pe) / (1 - pe) if pe < 1 else 0.0
    mo = min(min(float(np.mean(a == k)), float(np.mean(b == k)))
             for k in range(NP))
    ch = [float(np.sum(np.diff(p) != 0)) / (len(p) / FPS / 60) for p in (a, b)]
    hold = []
    for p in (a, b):
        e = np.concatenate(([0], np.flatnonzero(np.diff(p) != 0) + 1, [len(p)]))
        hold.append(float(np.mean(np.diff(e))) / FPS)
    return po, kap, mo, ch, hold


def main():
    dirs = [d for d in sys.argv[1:] if os.path.isdir(d)]
    if not dirs:
        raise SystemExit(__doc__)
    data = {}
    for d in dirs:
        try:
            data[d] = grid(d)
        except SystemExit:
            print(f"  skipping {d}: no state csv")
    print(f"  {'candidate':<30}" +
          "".join(f"{os.path.basename(d) + ': agr kappa occ ch/min':>34}"
                  for d in dirs) + "   worst")
    scored = []
    for name, c in CANDS.items():
        cells, worst = [], 1e9
        for d in dirs:
            GL, GR = data[d]
            s = c["lp"]
            args = dict(cshift=s, rshift=s, dshift=s, gshift=s,
                        cut=c["cut"], q8=c["q8"], ruler=c["ruler"])
            pl = q8fix.run(GL, c["w"], **args)[0]
            pr = q8fix.run(GR, c["w"], **args)[0]
            # Score from 3 time constants in: a reference seeded at boot needs
            # that long to stop being a description of whatever the room was doing
            # when the earring was switched on.
            w = int(max(120.0, 3.0 * (1 << s) / FPS)) * FPS
            w = int(min(w, len(pl) * 0.4))
            po, kap, mo, ch, hold = stats(pl, pr, w)
            worst = min(worst, kap)
            if mo < 0.12:
                # A colour under 12% fails the requirement outright, on any one
                # capture. It is disqualified rather than averaged away: blending
                # occupancy into the sort key lets a strong kappa buy past a
                # missing colour, which is the exact failure being screened for.
                worst = -1.0
            cells.append(f"{po*100:6.1f}% {kap:+6.3f} {mo*100:5.1f}% "
                         f"{ch[0]:4.1f}/{ch[1]:<4.1f}")
        scored.append((worst, name, cells))
    for worst, name, cells in sorted(scored, reverse=True):
        print(f"  {name:<30}" + "".join(f"{c:>34}" for c in cells) +
              f"  {worst:+.3f}")
    print("\n  sorted by the WORST kappa seen on any capture; a candidate leaving a")
    print("  colour under 12% anywhere is listed last regardless of its numbers.")


if __name__ == "__main__":
    main()
