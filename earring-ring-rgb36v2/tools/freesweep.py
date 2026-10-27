#!/usr/bin/env python3
"""Sweeps the two things that can be changed in master's selector for free.

Master picks the band standing furthest above a least-squares line through the
smoothed spectrum, and holds it until another beats it by an absolute margin. Both
of those are constants:

  * which bands are candidates at all - the loop runs b = 1 .. NBands-2, so the
    interior bands only, because band 0 and the last band are the edges of the fit
    and always lean at the line. The two ends are not equally trustworthy though:
    measured across two earrings, the top band tracks at +0.90 and the bottom one
    at MINUS 0.42 - the bottom moves opposite ways on the two units, so an argmax
    allowed to land there is choosing a value the pair disagrees about by
    construction. Narrowing the loop bound is one immediate operand in the binary.
  * the hysteresis margin, 32 << 8, absolute in deviation units. Also one constant.

Everything else that improved agreement on the debug branch - the slow reference,
the spread ruler, the four Q8 accumulators - costs code master does not have, and
master has sixty-four spare bytes. So this file asks only: is there a win inside
what a constant can buy? Thresholds fitted on one capture are always scored on the
other, because every absolute-threshold candidate this session looked excellent on
the capture it was fitted on and was chance on the next one.

  python3 freesweep.py ~/psync/t1 ~/psync/newfull
"""
import csv
import os
import sys

import numpy as np

NB = 8
FPS = 62.5
NP = NB - 1
Q16Q = 49939


def mul_q16(n, q=Q16Q):
    neg = n < 0
    u = -n if neg else n
    lo = (u & 0xFFFF) * q
    p = (u >> 16) * q + (lo >> 16)
    if neg:
        p = -p
        if lo & 0xFFFF:
            p -= 1
    return p


def spectral_devs(e):
    """Byte-exact transcription of master's spectralDevs. Checked against the Go
    original on four spectra including a linear ramp, which must give ~0."""
    sy = sxy = 0
    for b in range(NB):
        sy += e[b]
        sxy += e[b] * b
    num = 8 * sxy - 28 * sy
    slope = mul_q16(num)
    intercept = (sy << 5) - (slope * 7 >> 1)
    return [(e[b] << 8) - intercept - slope * b for b in range(NB)]


def argmax(dev, lo, hi):
    """Master's pick over a candidate window. Shipped is lo=1, hi=NB-1."""
    best = lo
    for b in range(lo + 1, hi):
        if dev[b] > dev[best]:
            best = b
    return best


def run(G, lo, hi, margin):
    n = len(G)
    out = np.zeros(n, dtype=np.int64)
    cur, valid = lo, False
    for i in range(n):
        dev = spectral_devs(G[i])
        ch = argmax(dev, lo, hi)
        if valid and ch != cur and dev[ch] < dev[cur] + margin:
            out[i] = cur
        else:
            cur = ch
            out[i] = ch
        valid = True
    return out


def score(a, b, warm):
    a, b = a[warm:], b[warm:]
    po = float(np.mean(a == b))
    pe = sum(float(np.mean(a == k)) * float(np.mean(b == k)) for k in range(NP))
    kap = (po - pe) / (1 - pe) if pe < 1 else 0.0
    occ = [float(np.mean(a == k)) for k in range(NP)] + \
          [float(np.mean(b == k)) for k in range(NP)]
    used = len(set(a.tolist()) | set(b.tolist()))
    ch = [float(np.sum(np.diff(p) != 0)) / (len(a) / FPS / 60) for p in (a, b)]
    hold = []
    for p in (a, b):
        e = np.concatenate(([0], np.flatnonzero(np.diff(p) != 0) + 1, [len(p)]))
        hold.append(float(np.mean(np.diff(e))) / FPS)
    return po, kap, min(occ), used, ch, hold


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


def main():
    if len(sys.argv) < 3:
        raise SystemExit(__doc__)
    caps = [(d,) + grid(d) for d in sys.argv[1:]]
    for d, GL, _ in caps:
        print(f"loaded {d}: {len(GL)/FPS/60:.1f} min")
    warm = int(120 * FPS)

    print("\n  candidate bands  margin  " +
          "  ".join(f"{os.path.basename(c[0]):>30}" for c in caps))
    print("  " + "-" * 96)
    rows = []
    for lo, hi in ((1, NB - 1), (1, 6), (1, 5), (1, 4), (2, 6), (2, 5),
                   (1, 7), (0, 7)):
        for margin in (0, 8 << 8, 32 << 8, 64 << 8, 128 << 8):
            cells = []
            worst = 1e9
            for d, GL, GR in caps:
                a, b = run(GL, lo, hi, margin), run(GR, lo, hi, margin)
                po, kap, mo, used, ch, hold = score(a, b, warm)
                cells.append(f"{po*100:5.1f}% {kap:+6.3f} u{used} "
                             f"{mo*100:4.1f}% h{min(hold):4.1f}")
                # A frozen run is not agreement. Exclude those from the ranking
                # rather than letting 100% agreement from two stuck devices win.
                worst = min(worst, kap if used > 2 and min(hold) > 1.0 else -9)
            rows.append((worst, lo, hi, margin, cells))
    for worst, lo, hi, margin, cells in sorted(rows, reverse=True):
        ship = "  <- shipped" if (lo, hi, margin) == (1, NB - 1, 32 << 8) else ""
        print(f"  bands {lo}..{hi-1}      {margin >> 8:5d}  " +
              "  ".join(cells) + ship, flush=True)
    print("\n  margin shown in units of 256 (the Q8<<8 scale of the deviations);"
          " shipped = 32<<8 = 8192, printed as 32")
    print("  u = distinct patterns reached; a row with u<=2 or a hold under 1 s is"
          " frozen, not agreed")


if __name__ == "__main__":
    main()
