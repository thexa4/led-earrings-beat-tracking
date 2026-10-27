#!/usr/bin/env python3
"""The cheapest thing that could plausibly fix master's pattern choice.

Master picks a band by argmax over deviations from a fitted line and agrees with its
partner at chance. The debug branch's version agrees at kappa +0.19, but it carries
four Q8 accumulators, a divide and a hundred and fifty lines, and master has
sixty-four spare bytes of flash.

What the debug version actually contributes, isolated by the experiments this
session, is not the centroid and not the ruler - it is the slow reference. Three
separate measurements said so:

  * a raw band difference (s4-s0), which is gain-invariant by construction, scored
    kappa +0.09 on the capture its thresholds were fitted on and -0.01 on the next
    one, with 30-65 s holds. The quantity is fine; its LEVEL is what moves with the
    music, so any fixed threshold sits somewhere useless in it.
  * averaging the spectrum longer, which does not help either - it latches.
  * the debug cascade, whose one job is to read the contrast against its own slow
    mean and then against its own spread.

So the port to test is the minimal fragment: one contrast, one slow reference of
that contrast, thresholds on the difference. Cost is one int32 of RAM, one subtract,
one rounded shift-add and a comparison chain - plausibly inside sixty-four bytes.
The ruler (normalising by spread) is deliberately left out; if the reference alone
carries the improvement, the ruler's cost is not needed, and if it does not, adding
the ruler would only buy a mechanism that has nothing to normalise.

Thresholds are fitted on the FIRST capture given and scored on all of them. That is
the whole point: every absolute-threshold candidate this session looked excellent
where it was fitted and was chance elsewhere.

  python3 hipassport.py ~/psync/t1 ~/psync/newfull
"""
import csv
import os
import sys

import numpy as np

NB = 8
FPS = 62.5


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


sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import ema as emalib  # noqa: E402

# Subtracted in the carried (Q8) domain, so the difference keeps eight fraction
# bits. Taking the integer part of the average first and subtracting would quantise
# away exactly the small deviations the high-pass is there to expose - and a plain
# rounded average additionally freezes once its input settles within half the
# shift, which is what made every row of an earlier run identical for 33 s, 131 s
# and 524 s. See ema.py.


def bucket(v, thr):
    out = np.zeros(len(v), dtype=np.int64)
    for i, x in enumerate(v):
        k = 0
        for t in thr:
            if x > t:
                k += 1
        out[i] = k
    return out


def score(a, b, warm, npat):
    a, b = a[warm:], b[warm:]
    po = float(np.mean(a == b))
    pe = sum(float(np.mean(a == k)) * float(np.mean(b == k)) for k in range(npat))
    kap = (po - pe) / (1 - pe) if pe < 1 else 0.0
    occ = [float(np.mean(a == k)) for k in range(npat)] + \
          [float(np.mean(b == k)) for k in range(npat)]
    hold = []
    for p in (a, b):
        e = np.concatenate(([0], np.flatnonzero(np.diff(p) != 0) + 1, [len(p)]))
        hold.append(float(np.mean(np.diff(e))) / FPS)
    return po, kap, min(occ), hold


def main():
    if len(sys.argv) < 3:
        raise SystemExit(__doc__)
    emalib.self_test()  # raises if the averaging stalls; see ema.py
    caps = [(d,) + grid(d) for d in sys.argv[1:]]
    for d, GL, _ in caps:
        print(f"loaded {d}: {len(GL)/FPS/60:.1f} min")
    warm = int(120 * FPS)

    contrasts = {
        "s4-s0": lambda G: G[:, 4] - G[:, 0],
        "s45-s01": lambda G: (G[:, 4] + G[:, 5]) - (G[:, 0] + G[:, 1]),
        "s5-s0": lambda G: G[:, 5] - G[:, 0],
    }
    for cname, fn in contrasts.items():
        print(f"\n=== {cname}, high-passed against its own slow mean ===")
        print(f"  {'ref':>6} {'npat':>4} " +
              "  ".join(f"{os.path.basename(c[0]):>32}" for c in caps))
        # The reference is fitted on the first capture and applied unchanged to the
        # rest, so a row that only works where it was fitted is visible as such.
        for shift in (9, 11, 13, 15):
            hl = [emalib.hipass(fn(GL), shift) for _, GL, _ in caps]
            hr = [emalib.hipass(fn(GR), shift) for _, _, GR in caps]
            ref = np.concatenate(hl[:1] + hr[:1])
            for npat in (3, 4, 7):
                qs = [100.0 * i / npat for i in range(1, npat)]
                thr = [int(v) for v in np.percentile(ref, qs)]
                if len(set(thr)) != len(thr):
                    continue
                cells = []
                for k, (d, _, _) in enumerate(caps):
                    a, b = bucket(hl[k], thr), bucket(hr[k], thr)
                    po, kap, mo, hold = score(a, b, warm, npat)
                    cells.append(f"{po*100:5.1f}% {kap:+6.3f} {mo*100:4.1f}% "
                                 f"h{min(hold):4.1f}")
            # Keep the last shift's loop variable visible per row instead: print
            # inside the shift loop.
                print(f"  {2**shift/FPS:5.0f}s {npat:>4} " + "  ".join(cells),
                      flush=True)
        print("  (ref = the slow mean the contrast is read against; thresholds"
              " fitted on\n"
              "   capture 1 only; h = shorter hold of the two, in seconds)")


if __name__ == "__main__":
    main()
