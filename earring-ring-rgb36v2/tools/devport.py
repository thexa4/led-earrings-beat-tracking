#!/usr/bin/env python3
"""Threshold master's OWN line-fit residuals, which cost nothing to compute.

Every earlier port attempt here had to build its own adaptive reference, and the
reference is what made it expensive: one slow mean to remove the level, a second
to measure the spread, and then a divide to compare them. Master already has an
adaptive reference and it is free. `spectralDevs` fits a least-squares line
through the eight smoothed bands every frame and returns how far each band sits
above it:

    dev[b] = (smooth[b] << 8) - intercept - slope * b

Two things follow that the earlier tests did not exploit. First, the residuals
are centred by construction - a line fit through eight points has residuals sum
-
ming to zero - so a deviation neither grows nor shrinks with the overall level o
f
the music, which was exactly the property that absolute thresholds were missing
when they scored kappa -0.01 out of sample. Second, `slope` is a tilt estimate b
uilt
from all eight bands, so subtracting it removes the spectral slope a room or a h
ead
imparts, again without any state of our own.

So the cheapest selector that has never been tested is: pick some band
combination out of dev[], and bucket it against fixed cuts. If the contrast is a
difference of two deviations, the intercept cancels and only the slope survives;
if it is a single deviation, everything cancels with the fit. The whole rule mig
ht
be two comparisons on data the firmware already has in registers, with no new
state at all - which is the only kind of change that fits in sixty-four bytes.

Controls included on purpose: the same difference taken on the raw bands, withno
line fit, to show what the fit is actually contributing; and the same contrast p
ut
through one Q8 mean, which is four bytes of RAM, to show what smoothing adds on
top.

Thresholds are fitted as quantiles on the FIRST capture and applied unchanged to
the rest, because a rule fitted on the same data it is scored on only measures
how well it can memorise.

  python3 devport.py ~/psync/t1 ~/psync/newfull
"""
import csv
import os
import sys

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import ema as emalib  # noqa: E402

NB = 8
FPS = 62.5
Q16Q = 49939


def load(path):
    t, b = [], []
    with open(path) as fh:
        for r in csv.DictReader(fh):
            t.append(float(r["t"]))
            b.append([int(float(r[f"b{i}"])) for i in range(NB)])
    t = np.asarray(t)
    o = np.argsort(t)
    return t[o], np.asarray(b, dtype=np.int64)[o]


def grid(d):
    tl, bl = load(os.path.join(d, "state_L.csv"))
    tr, br = load(os.path.join(d, "state_R.csv"))
    lo, hi = max(tl[0], tr[0]), min(tl[-1], tr[-1])
    tg = np.arange(lo, hi, 1.0 / FPS)
    return (bl[np.clip(np.searchsorted(tl, tg), 0, len(tl) - 1)][:len(tg)],
            br[np.clip(np.searchsorted(tr, tg), 0, len(tr) - 1)][:len(tg)])


def devs_all(E):
    """master's spectralDevs, vectorised over frames, integer arithmetic.

    Same expression as the Go, including the sign handling in the multiply, whic
h
    was verified against the original byte for byte - see mastersel.py.
    """
    b = np.arange(NB, dtype=np.int64)
    sy = E.sum(axis=1)
    sxy = (E * b).sum(axis=1)
    num = 8 * sxy - 28 * sy
    neg = num < 0
    u = np.where(neg, -num, num)
    lo = (u & 0xFFFF) * Q16Q
    p = (u >> 16) * Q16Q + (lo >> 16)
    slope = np.where(neg, -p - np.where(lo & 0xFFFF, 1, 0), p)
    intercept = (sy << 5) - ((slope * 7) >> 1)
    return (E << 8) - intercept[:, None] - slope[:, None] * b


# Contrast candidates, all linear in the band values. A named function per candi-
# date so the printed row says what was measured. dev[] is (n, 8).
CANDS = {
    # single residuals: no new state, no arithmetic beyond a comparison
    "dev4": lambda D, E: D[:, 4],
    "dev5": lambda D, E: D[:, 5],
    "dev3": lambda D, E: D[:, 3],
    "dev6": lambda D, E: D[:, 6],
    "dev4-dev0": lambda D, E: D[:, 4] - D[:, 0],
    "dev5-dev0": lambda D, E: D[:, 5] - D[:, 0],
    "dev4-dev1": lambda D, E: D[:, 4] - D[:, 1],
    "dev3-dev1": lambda D, E: D[:, 3] - D[:, 1],
    "dev5-dev2": lambda D, E: D[:, 5] - D[:, 2],
    "dev6-dev1": lambda D, E: D[:, 6] - D[:, 1],
    "dev4+dev5": lambda D, E: D[:, 4] + D[:, 5],
    # control: the same contrasts with no line fit at all
    "raw s4-s0": lambda D, E: (E[:, 4] - E[:, 0]) << 8,
    "raw s45-s01": lambda D, E: ((E[:, 4] + E[:, 5]) -
                                 (E[:, 0] + E[:, 1])) << 8,
}


def q8mean(cols, shift):
    """Rounded EMA on an eight fraction bits accumulator, many columns at once.

    A plain rounded EMA freezes wherever it happens to sit once its input is
    within half a step of it, which silently invalidated two earlier rounds of
    analysis; the accumulator here carries the fraction so it cannot. See ema.py
,    whose self test is run before anything in this file prints.
    """
    r = 1 << (shift - 1)
    acc = cols[0] << 8
    out = np.empty_like(cols)
    for i in range(len(cols)):
        acc += ((cols[i] << 8) - acc + r) >> shift
        out[i] = acc >> 8
    return out


def score(a, b, warm, npat):
    a, b = a[warm:], b[warm:]
    po = float(np.mean(a == b))
    pe = sum(float(np.mean(a == k)) * float(np.mean(b == k)) for k in range(npat))
    kap = (po - pe) / (1 - pe) if pe < 1 else 0.0
    occ = [float(np.mean(a == k)) for k in range(npat)] + \
          [float(np.mean(b == k)) for k in range(npat)]
    ch = []
    for p in (a, b):
        ch.append(float(np.count_nonzero(np.diff(p) != 0)) / len(p) * FPS * 60)
    hold = []
    for p in (a, b):
        e = np.concatenate(([0], np.flatnonzero(np.diff(p) != 0) + 1, [len(p)]))
        hold.append(float(np.mean(np.diff(e))) / FPS)
    return po, kap, min(occ), min(ch), min(hold)


def main():
    emalib.self_test()
    caps = []
    for d in sys.argv[1:]:
        GL, GR = grid(d)
        caps.append((d, devs_all(GL), GL, devs_all(GR), GR))
        print(f"loaded {d}: {len(GL)/FPS/60:.1f} min")
    warm = int(120 * FPS)
    # Smoothing applied to the contrast before bucketing. None is genuinely free
    # in master; the others cost one accumulator each.
    smoothings = [("raw", None), ("1s", 62), ("4s", 250), ("16s", 1000)]
    rows = []
    for name, fn in CANDS.items():
        zl = [fn(c[1], c[2]) for c in caps]
        zr = [fn(c[3], c[4]) for c in caps]
        for sname, shift in smoothings:
            if shift is None:
                al, ar = zl, zr
            else:
                # Same time constant for every capture, computed once per capture
                # pair rather than once per candidate to keep the loop count down
                al = [q8mean(z, int(round(np.log2(shift)))) for z in zl]
                ar = [q8mean(z, int(round(np.log2(shift)))) for z in zr]
            base = np.concatenate([al[0], ar[0]])
            for npat in (3, 4, 5, 7):
                qs = [100.0 * i / npat for i in range(1, npat)]
                cuts = [int(v) for v in np.percentile(base, qs)]
                if len(set(cuts)) != len(cuts):
                    continue
                cells = []
                kmin = 1.0
                for k in range(len(caps)):
                    a = np.zeros_like(al[k])
                    b = np.zeros_like(ar[k])
                    for cut in cuts:
                        a += al[k] > cut
                        b += ar[k] > cut
                    po, kap, mo, ch, hold = score(a, b, warm, npat)
                    kmin = min(kmin, kap)
                    cells.append(f"{po*100:5.1f}% {kap:+6.3f} {mo*100:4.1f}% "
                                 f"{ch:5.1f} h{hold:4.1f}")
                rows.append((kmin, name, sname, npat, cells))
    hdr = "  ".join(f"{os.path.basename(c[0]):>32}" for c in caps)
    print(f"\n=== thresholds on master's free line-fit residuals "
          f"(cuts from {os.path.basename(caps[0][0])} only) ===")
    print(f"  {'worst':>6} {'contrast':>12} {'sm':>4} {'npat':>4}  {hdr}")
    for kmin, name, sname, npat, cells in sorted(rows, reverse=True):
        print(f"  {kmin:+6.3f} {name:>12} {sname:>4} {npat:>4}  " +
              "  ".join(cells), flush=True)
    print("  (minocc = smallest share of any colour, ch/min = changes per\n"
          "   minute on the quieter device, h = its mean hold in seconds)")


if __name__ == "__main__":
    main()
