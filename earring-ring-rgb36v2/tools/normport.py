#!/usr/bin/env python3
"""The debug branch's rule, written so it costs no divide - and therefore might fit
in master's sixty-four spare bytes.

The debug selector computes a ratio, deviation over spread, and compares it against
fixed cuts. On a Cortex-M0+ a divide is a software routine: __aeabi_idiv costs on
the order of a hundred bytes before the caller, which alone is more than master has
left. But the divide is unnecessary. Comparing

    d * 256 / dev > cut

against a constant `cut` is the same as comparing

    d * 256 > cut * dev

when dev is positive, and dev is a spread so it always is. One multiply per
threshold instead of a divide per frame: the ruler becomes affordable, and the only
new state is the spread itself, one int32.

So the candidate port is four quantities:

    c   = (smooth[4] - smooth[0]) << 3     gain-invariant, no new state
    ref = slow mean of c                   one accumulator
    dev = slow mean of |c - ref|           one accumulator
    pattern = number of cuts the product d*256 exceeds

eight bytes of RAM, one subtract and shift, two rounded shift-adds, and a short
comparison chain. Both accumulators carry eight fraction bits: a plain rounded EMA
freezes once its input settles within half the shift, which is what invalidated two
earlier rounds of analysis here - see ema.py, whose self-test runs before anything
in this file prints.

Thresholds are fitted on the FIRST capture and applied unchanged to the rest.

  python3 normport.py ~/psync/t1 ~/psync/newfull
"""
import csv
import os
import sys

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import ema as emalib  # noqa: E402

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


def cascade(c, shift, floor):
    """Returns d and dev, both integers, from the two-accumulator version.

    Written the way the firmware would: the mean and the spread are both rounded
    EMAs carrying eight fraction bits, so neither can stall, and d is the difference
    of the integer parts. `floor` is the smallest spread the ruler may read; without
    it a quiet passage where dev has decayed to nothing makes one frame's noise look
    like a full-scale deviation, which is the failure mode the debug branch measured
    at 52% against 63% on short captures.
    """
    n = len(c)
    d = np.zeros(n, dtype=np.int64)
    dv = np.zeros(n, dtype=np.int64)
    a = int(c[0]) << 8
    s = 0
    r = 1 << (shift - 1)
    for i, v in enumerate(c):
        a += ((int(v) << 8) - a + r) >> shift
        diff = int(v) - (a >> shift)
        ad = diff if diff >= 0 else -diff
        s += ((ad << 8) - s + r) >> shift
        dev = s >> 8
        if dev < floor:
            dev = floor
        d[i] = diff
        dv[i] = dev
    return d, dv


def bucket(d, dev, cuts):
    """Compare d*256 against cut*dev - the form that needs no divide."""
    n = len(d)
    out = np.zeros(n, dtype=np.int64)
    for i in range(n):
        lhs = int(d[i]) * 256
        k = 0
        for t in cuts:
            if lhs > t * int(dev[i]):
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
    emalib.self_test()
    caps = [(d,) + grid(d) for d in sys.argv[1:]]
    for d, GL, _ in caps:
        print(f"loaded {d}: {len(GL)/FPS/60:.1f} min")
    warm = int(120 * FPS)

    contrasts = {
        "s4-s0": lambda G: G[:, 4] - G[:, 0],
        "s45-s01": lambda G: (G[:, 4] + G[:, 5]) - (G[:, 0] + G[:, 1]),
    }
    for cname, fn in contrasts.items():
        print(f"\n=== {cname}: deviation over its own spread, no divide ===")
        print(f"  {'shift':>5} {'floor':>6} {'npat':>4} " +
              "  ".join(f"{os.path.basename(c[0]):>30}" for c in caps))
        for shift in (9, 11, 13):
            dd = [cascade(fn(GL), shift, 1) for _, GL, _ in caps]
            dr = [cascade(fn(GR), shift, 1) for _, _, GR in caps]
            # Fit the cuts as quantiles of the ratio on capture 1 only.
            z0 = (dd[0][0] * 256) // np.maximum(dd[0][1], 1)
            z0 = np.concatenate([z0, (dr[0][0] * 256) //
                                 np.maximum(dr[0][1], 1)])
            for npat in (3, 4, 5):
                qs = [100.0 * i / npat for i in range(1, npat)]
                cuts = [int(v) for v in np.percentile(z0, qs)]
                if len(set(cuts)) != len(cuts):
                    continue
                for floor in (1, 32, 64, 128):
                    cells = []
                    for k in range(len(caps)):
                        dl, vl = cascade(fn(caps[k][1]), shift, floor)
                        dr2, vr = cascade(fn(caps[k][2]), shift, floor)
                        a = bucket(dl, vl, cuts)
                        b = bucket(dr2, vr, cuts)
                        po, kap, mo, hold = score(a, b, warm, npat)
                        cells.append(f"{po*100:5.1f}% {kap:+6.3f} "
                                     f"{mo*100:4.1f}% h{min(hold):4.1f}")
                    print(f"  {2**shift/FPS:5.0f} {floor:6d} {npat:>4} " +
                          "  ".join(cells), flush=True)
        print("  (floor = smallest spread the ruler may read; cuts fitted on\n"
              "   capture 1 only; h = shorter of the two hold times, seconds)")


if __name__ == "__main__":
    main()
