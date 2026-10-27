#!/usr/bin/env python3
"""Find a band weighting the two units agree on, and validate it on held-out audio.

The shipped selector weights the eight bands by {-7,-5,-3,-1,+1,+3,+5,+7}: a graded
ramp from treble to bass, so that the sum is zero and overall loudness cannot move it.
Measured per band on the 25 minute capture, with each band expressed as its deviation
from that unit's own average - level removed, which is the same thing the ramp is for -
the correlation between the two units is:

    b0 >4k     +0.90      b4 630-1k   +0.80
    b1 2.5-4k  +0.70      b5 400-630  +0.84
    b2 1.6-2.5k +0.07     b6 250-400  +0.69
    b3 1-1.6k  +0.07      b7 <250     -0.42

Two of those bands carry nothing, one carries the shared signal inverted, and the shipped
weights put the largest negative weight on the best band and the largest positive weight
on the inverted one. The graded ramp was chosen to be monotone in frequency, which is a
reasonable guess about timbre and turns out to be a poor guess about what two separate
microphones in the same room actually agree on.

So this searches weights instead of assuming them, and the only thing it is allowed to
report is a number from audio the weights did not see. A weighting fitted to maximise
agreement on a recording will always agree well on that recording; with eight free
parameters and 7500 frames the fitted correlation is inflated by a few points at least,
and the whole purpose of the split is to catch the case where it is inflated by a lot.
Every candidate is therefore scored as:

  fit    Spearman on the first half of the recording
  test   the same weights, unchanged, on the second half
  agree  four-way agreement using per-device quartiles on the second half

Candidates are hand-written contrasts rather than a fitted optimum, because a hand
candidate that survives hold-out is explainable and can be defended when it is compiled
into firmware, while a fitted eight-vector that survives is just a number. The fitted
vector is still printed, and its test score is the honest read on how much of the
ranking is fitting noise.

  python3 tools/bandfit.py DIR
"""
import csv
import os
import sys

import numpy as np

NP = 4
GRID = 5.0
WARMUP = 60.0
NAMES = [">4k", "2.5-4k", "1.6-2.5k", "1-1.6k", "630-1k", "400-630", "250-400",
         "<250"]

# Hand-written contrasts. Each is a weighting over the eight bands; sign convention
# matches the firmware, where band 0 is above 4 kHz and band 7 below 250 Hz.
CANDIDATES = {
    "shipped ramp": (-7, -5, -3, -1, 1, 3, 5, 7),
    "shipped reversed": (7, 5, 3, 1, -1, -3, -5, -7),
    "shipped ramp scaled": (-4, -3, 0, 0, 0, 0, 0, 4),
    "treble-bass, ends": (-1, 0, 0, 0, 0, 0, 0, 1),
    "treble-mid, no b7": (-1, 0, 0, 0, 0, 0, 1, 0),
    "drop b2 b3 only": (-7, -5, 0, 0, 1, 3, 5, 7),
    "mid emphasis": (0, 0, -1, -1, 1, 1, 0, 0),
    # Around the winner of the first pass: bands 0,1 against bands 4,5, ignoring the
    # two transition bands, band 6, and the <250 Hz band whose shared content is
    # inverted. Graded rather than flat in case the ordering within a group matters.
    "0-4": (-1, 0, 0, 0, 1, 0, 0, 0),
    "01-45": (-1, -1, 0, 0, 1, 1, 0, 0),
    "01-456": (-1, -1, 0, 0, 1, 1, 1, 0),
    "012-456": (-1, -1, -1, 0, 1, 1, 1, 0),
    "01-56": (-1, -1, 0, 0, 0, 1, 1, 0),
    "0123-4567": (-1, -1, -1, -1, 1, 1, 1, 1),
    "01-45 graded": (-3, -1, 0, 0, 1, 3, 0, 0),
    "01-45 skew treble": (-4, -2, 0, 0, 1, 1, 0, 0),
    "01-45 skew mid": (-1, -1, 0, 0, 2, 4, 0, 0),
    "0-45": (-2, 0, 0, 0, 1, 1, 0, 0),
    "01-45 minus b7": (-1, -1, 0, 0, 1, 1, 0, -1),
}


def load(path):
    t, b = [], []
    with open(path) as fh:
        for r in csv.DictReader(fh):
            try:
                t.append(float(r["t"]))
                b.append([float(r[f"b{i}"]) for i in range(8)])
            except (KeyError, ValueError):
                continue
    t = np.asarray(t)
    o = np.argsort(t)
    return t[o], np.asarray(b, dtype=np.float64)[o]


def grid(t, x, lo, hi):
    tg = np.arange(lo, hi, 1.0 / GRID)
    return x[np.clip(np.searchsorted(t, tg), 0, len(t) - 1)]


def lowpass(x, sec):
    """Boxcar along time, for 1-D or for a (frames, bands) array.

    Written with a cumulative sum rather than np.convolve because np.convolve takes
    1-D input and raises "object too deep" on the band matrix, which is the usual case
    here.
    """
    k = max(1, int(round(sec * GRID)))
    if k == 1:
        return x
    axis = 0
    c = np.cumsum(np.insert(x, 0, 0.0, axis=axis), axis=axis)
    y = (c[k:] - c[:-k]) / k
    pad = x.shape[0] - y.shape[0]
    head = np.take(y, np.zeros(pad // 2, dtype=int), axis=axis)
    tail = np.take(y, np.full(pad - pad // 2, y.shape[0] - 1, dtype=int), axis=axis)
    return np.concatenate([head, y, tail], axis=axis)


def rank(x):
    return np.argsort(np.argsort(x)).astype(float)


def spearman(a, b):
    if np.std(a) < 1e-12 or np.std(b) < 1e-12:
        return 0.0
    return float(np.corrcoef(rank(a), rank(b))[0, 1])


def quartile_agree(x, y):
    cx = np.quantile(x, [(i + 1) / NP for i in range(NP - 1)])
    cy = np.quantile(y, [(i + 1) / NP for i in range(NP - 1)])
    px, py = np.searchsorted(cx, x), np.searchsorted(cy, y)
    po = float(np.mean(px == py))
    pe = sum(np.mean(px == k) * np.mean(py == k) for k in range(NP))
    mo = min(min(float(np.mean(px == k)), float(np.mean(py == k)))
             for k in range(NP))
    return po, ((po - pe) / (1 - pe) if pe < 1 else 0.0), mo


def main():
    d = sys.argv[1]
    data = {}
    for side in ("L", "R"):
        for c in (f"state_{side}.csv", f"state-{side}.csv"):
            if os.path.exists(os.path.join(d, c)):
                data[side] = load(os.path.join(d, c))
                break
        else:
            raise SystemExit(f"{d}: no state for {side}")
    (tl, bl), (tr, br) = data["L"], data["R"]
    lo, hi = max(tl[0], tr[0]), min(tl[-1], tr[-1])
    GL, GR = grid(tl, bl, lo, hi), grid(tr, br, lo, hi)
    n = min(len(GL), len(GR))
    GL, GR = GL[:n], GR[:n]
    w = int(WARMUP * GRID)
    GL, GR = GL[w:], GR[w:]
    half = len(GL) // 2
    print(f"{d}: {(len(GL))/GRID/60:.1f} min; fit on the first "
          f"{half/GRID/60:.1f} min, test on the rest")

    # Level removed per unit, so a permanently louder earring cannot be the reason a
    # weighting scores well.
    def shape(g):
        return g - g.mean(1, keepdims=True)

    def hold(x, cuts_lo, cuts_hi):
        c = np.quantile(x, [0.25, 0.5, 0.75])
        p = np.searchsorted(c, x)
        e = np.concatenate(([0], np.flatnonzero(np.diff(p) != 0) + 1, [len(p)]))
        return np.mean(np.diff(e)) / GRID

    for sec in (8.0, 20.0, 45.0, 90.0, 150.0, 240.0):
        SL = lowpass(shape(GL), sec).T
        SR = lowpass(shape(GR), sec).T
        s1, s2 = slice(0, half), slice(half, None)
        print(f"\n  low-passed {sec:g} s   (shape: each band minus its own unit's mean)")
        print(f"  {'candidate':<20} {'rho_s A':>8} {'rho_s B':>8} {'both':>7} "
              f"{'agree':>7} {'kappa':>7} {'minocc':>7} {'hold s':>10}")
        rows = []
        for nm, wv in CANDIDATES.items():
            wv = np.asarray(wv, dtype=np.float64)
            x = SL.T @ wv
            y = SR.T @ wv
            # Both halves reported separately, and the split swapped: a weighting that
            # only works forward in time is following something about the playlist,
            # not something about the two microphones.
            a = spearman(x[s1], y[s1])
            b = spearman(x[s2], y[s2])
            po, kap, mo = quartile_agree(x[s2], y[s2])
            hl, hr = hold(x[s2], 0, 0), hold(y[s2], 0, 0)
            rows.append((min(a, b), nm, a, b, po, kap, mo))
            print(f"  {nm:<20} {a:+8.3f} {b:+8.3f} "
                  f"{spearman(x, y):+7.3f} {po*100:6.1f}% {kap:+7.3f} "
                  f"{mo*100:6.1f}% {hl:4.0f}/{hr:<5.0f}")
        rows.sort(reverse=True)
        print(f"  best worst-half rho_s: {rows[0][1]}  "
              f"(A {rows[0][2]:+.3f}, B {rows[0][3]:+.3f}, agree {rows[0][4]*100:.1f}%)")


if __name__ == "__main__":
    main()
