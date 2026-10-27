#!/usr/bin/env python3
"""Can master's pattern choice be fixed inside the ~64 spare bytes of flash?

The replay in mastersel.py says master's shipped choice - the band standing
furthest above a least-squares line through the spectrum - agrees between two
earrings at kappa +0.03 over an hour, which is chance. Everything longer-windowed
either freezes onto one colour or goes negative, so "average it longer" is not
available to it either.

What is available has to be measured in bytes, not just in agreement. Master is at
29504 of 29568 flash and 7628 of 8192 RAM with the stack included, which leaves no
room for a histogram, a median buffer, or the debug branch's four-accumulator
cascade. What does fit is a single difference of two smoothed bands, which in the
log domain master already keeps is a RATIO and therefore gain-invariant with no
AGC bookkeeping at all: log a - log b = log(a/b). Both earrings apply their own
gain to p.smooth, and the gain cancels in the subtraction. That is the property the
debug branch's weight search kept converging on - b4 minus b0, or b4+b5 minus
b0+b1 - and it happens to be the cheapest thing in the whole space.

So the candidate is: one subtract, a small comparison chain, done. This tool picks
the thresholds and reports what the chain would cost.

  python3 cheapport.py ~/psync/t1 ~/psync/newfull
"""
import csv
import os
import sys

import numpy as np

FPS = 62.5
NB = 8


def load(path):
    t, b = [], []
    with open(path) as fh:
        for r in csv.DictReader(fh):
            t.append(float(r["t"]))
            b.append([int(float(r[f"b{i}"])) for i in range(NB)])
    t = np.asarray(t)
    o = np.argsort(t)
    return t[o], np.asarray(b, dtype=np.int64)[o]


def grid(tl, bl, tr, br):
    lo, hi = max(tl[0], tr[0]), min(tl[-1], tr[-1])
    tg = np.arange(lo, hi, 1.0 / FPS)
    GL = bl[np.clip(np.searchsorted(tl, tg), 0, len(tl) - 1)][:len(tg)]
    GR = br[np.clip(np.searchsorted(tr, tg), 0, len(tr) - 1)][:len(tg)]
    return GL, GR


def score(a, b, warm, npat):
    a, b = a[warm:], b[warm:]
    po = float(np.mean(a == b))
    pe = sum(float(np.mean(a == k)) * float(np.mean(b == k)) for k in range(npat))
    kap = (po - pe) / (1 - pe) if pe < 1 else 0.0
    occ = [float(np.mean(a == k)) for k in range(npat)] + \
          [float(np.mean(b == k)) for k in range(npat)]
    mo = min(occ)
    ch = [float(np.sum(np.diff(p) != 0)) / (len(a) / FPS / 60) for p in (a, b)]
    # Hold time: how long a colour stays on screen. A choice that flickers twice a
    # second is unreadable no matter what the statistics say.
    hold = []
    for p in (a, b):
        e = np.concatenate(([0], np.flatnonzero(np.diff(p) != 0) + 1, [len(p)]))
        hold.append(float(np.mean(np.diff(e))) / FPS)
    return po, kap, mo, ch, hold


def bucket(c, thr):
    """Fixed thresholds: the comparison chain a microcontroller would run."""
    out = np.zeros(len(c), dtype=np.int64)
    for i, v in enumerate(c):
        k = 0
        for t in thr:
            if v > t:
                k += 1
        out[i] = k
    return out


def main():
    if len(sys.argv) < 2:
        raise SystemExit(__doc__)
    caps = []
    for d in sys.argv[1:]:
        (tl, bl), (tr, br) = load(os.path.join(d, "state_L.csv")), \
            load(os.path.join(d, "state_R.csv"))
        GL, GR = grid(tl, bl, tr, br)
        caps.append((d, GL, GR))
        print(f"loaded {d}: {len(GL)/FPS/60:.1f} min")

    # Contrasts worth trying. All are differences of smoothed log bands, so all are
    # gain-invariant and cost one or two adds. Weighted sums with coefficients
    # other than +/-1 would need multiplies and are left out for that reason.
    contrasts = {
        "s4-s0": lambda G: G[:, 4] - G[:, 0],
        "s5-s1": lambda G: G[:, 5] - G[:, 1],
        "s45-s01": lambda G: (G[:, 4] + G[:, 5]) - (G[:, 0] + G[:, 1]),
        "s456-s012": lambda G: (G[:, 4] + G[:, 5] + G[:, 6]) -
                               (G[:, 0] + G[:, 1] + G[:, 2]),
        "s6-s0": lambda G: G[:, 6] - G[:, 0],
    }
    print("\n  contrast      p95    p99   |   distribution of the contrast in"
          " Q8-log2 units")
    for name, fn in contrasts.items():
        allc = np.concatenate([fn(GL) for _, GL, _ in caps] +
                              [fn(GR) for _, _, GR in caps])
        q = np.percentile(allc, [1, 5, 25, 50, 75, 95, 99])
        print(f"  {name:<12} {q[5]:7.0f} {q[6]:7.0f} | "
              " ".join(f"{v:7.0f}" for v in q))

    # Threshold sets. Septiles of one capture are the optimistic case - they are
    # fitted, not predicted - so every row is also scored on the OTHER capture,
    # which is what decides anything.
    for name, fn in contrasts.items():
        print(f"\n=== contrast {name} ===")
        print(f"  {'thresholds':<34} {'npat':>4} " +
              " ".join(f"{os.path.basename(d):>26}" for d, _, _ in caps))
        # Fit septiles/quartiles on the first capture only.
        c0 = np.concatenate([fn(caps[0][1]), fn(caps[0][2])])
        for label, qs in (("quartiles", (25, 50, 75)),
                          ("quintiles", (20, 40, 60, 80)),
                          ("sextiles", (100 / 6, 200 / 6, 50, 400 / 6, 500 / 6)),
                          ("septiles", (100 / 7, 200 / 7, 300 / 7, 400 / 7,
                                        500 / 7, 600 / 7))):
            thr = [int(v) for v in np.percentile(c0, qs)]
            if len(set(thr)) != len(thr):
                print(f"  {str(thr):<34}  (degenerate: thresholds collide)")
                continue
            npat = len(thr) + 1
            cells = []
            for d, GL, GR in caps:
                a = bucket(fn(GL), thr)
                b = bucket(fn(GR), thr)
                po, kap, mo, ch, hold = score(a, b, int(120 * FPS), npat)
                cells.append(f"{po*100:5.1f}% {kap:+6.3f} {mo*100:4.1f}% "
                             f"h{hold[0]:.1f}/{hold[1]:.1f}")
            print(f"  {label + ' ' + str(thr):<34} {npat:>4} " +
                  "  ".join(cells))


if __name__ == "__main__":
    main()
