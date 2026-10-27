#!/usr/bin/env python3
"""Choose the pattern by rank in a running histogram instead of by fixed cuts.

What the sweep of the last hour established, on the 25 minute capture, with the
cascade reimplemented and checked against the device at 100.00%:

  the shipped arithmetic   45.0% agree, kappa +0.11, minocc 0.9%
  all four states in Q8    45.7% agree, kappa +0.26, minocc 11.2%   (16 s)
  same, 66 s low-pass      76.6% agree, kappa +0.66, minocc  7.0%
  same, 66 s, cut 216      84.1% agree, kappa +0.76, minocc  0.0%

So the deadband fix and the smoothing scale are worth 40 agreement points, and
neither of them is the last problem: no cut gives even occupancy. Sweeping the
cut at the good smoothing scale moves between 9.5% and 11.3% minimum occupancy
and nothing better, at any cut, at any smoothing scale, with either a symmetric
ruler or a per-direction one.

That is not bad luck, it is the construction. Three fixed cuts on one number can
only give even colours if that number's quartiles are symmetric about its
average, and the measured quartiles of the judged quantity are -219 / -46 / +182:
its middle is 46 counts below its average, and its left tail is longer than its
right. A symmetric rule cannot express that, and a per-direction ruler does not
help either because the skew is in the distribution, not in the scale.

So stop asking a fixed constant to be a quartile. Keep one scalar - the ranking
of this moment's spectral tilt among recent moments - and let the thresholds be
the empirical quartiles of that scalar's own recent distribution, tracked in a
decaying histogram. Then even occupancy is not a tuning target, it is what a
quartile means, and the only thing left to earn is agreement.

This is NOT the running-median experiment that failed. That one tested a bit
against a median tracker on a 1 s signal and scored exactly chance: the bit was
noise because the signal was noise, and forcing 50/50 balance on a quantity with
no information produces a coin flip with nice statistics. Here the quantity being
ranked is the same 66 s-smoothed scalar that carries kappa +0.66, and the
histogram only moves where its cut points sit. Balance is taken from a quantity
that already has signal, which is the whole difference.

  python3 tools/qhist.py DIR
"""
import csv
import os
import sys

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import q8fix

NB = 8
FPS = 62.5
NP = q8fix.NUM_PATTERNS
# 32 bins spanning +/-1024 in units of 1/256 of the spread: the judged quantity
# is a ratio to the device's own spread, so a fixed span in that unit means the
# same thing on both units and needs no calibration.
# The rank must be taken on the ratio BEFORE PatternIndexGap clamps it to +/-2
# spreads. Ranked after the clamp it is useless and the histogram showed exactly
# that: most frames sit on the edge bin, the 25th and 75th percentile both resolve
# to that same bin, and the colours come out 33/2/1/62 however the music moves.
# So the span covers the unclamped ratio, which runs to roughly +/-16 spreads
# when the ruler is momentarily small.
NBINS = 64
SPAN = 8192
WIDTH = SPAN // NBINS


def medfilt(x, k):
    """A k-point running median, edge-padded.

    Unlike a longer average it removes single-frame spikes without smearing the
    edges of a section, so it can steady the chosen colour without slowing the
    colour down - which is the trade the smoothing scale alone is stuck with.
    Causal by construction: the window at frame i covers i-k+1..i, so it never
    reads the future, which a centred window would.
    """
    if k <= 1:
        return x.astype(np.int64)
    h = k - 1
    pad = np.concatenate([np.full(h, x[0], dtype=x.dtype), x])
    w = np.lib.stride_tricks.sliding_window_view(pad, k)
    return np.median(w, axis=1).astype(np.int64)


def binidx(v, nbins=NBINS):
    b = (int(v) + SPAN // 2) // (SPAN // nbins)
    return 0 if b < 0 else (nbins - 1 if b >= nbins else b)


def block_quantiles(hist, nbins=NBINS):
    """The quartile bin centres of a histogram, as three thresholds."""
    total = sum(hist)
    if total <= 0:
        return None
    out, acc, ti = [], 0, 0
    targets = (total // 4, total // 2, (3 * total) // 4)
    for i in range(nbins):
        acc += hist[i]
        while ti < 3 and acc >= targets[ti]:
            out.append(i * (SPAN // nbins) - SPAN // 2 + (SPAN // nbins) // 2)
            ti += 1
        if ti >= 3:
            break
    while len(out) < 3:
        out.append(out[-1] if out else 0)
    return out


def rank_patterns(z, warm, window, nbins=NBINS, every=16):
    """Bucket z by the quartiles of the trailing `window` frames.

    Fixed cuts cannot give even colours - the deviation's median sits below its
    average, so three symmetric cuts cannot land on the quartiles at once. Empirical
    quartiles can, by definition.

    The window is an exact count, kept by a circular buffer of bin indices: one bin
    credited as a frame enters, one bin debited as the oldest frame leaves. That
    matters twice over. An exponentially decaying histogram was tried first and is
    wrong twice: decaying the running total once per bin instead of once per frame
    shrinks the percentile targets to nothing, and decaying a bin by `h -= h >> 12`
    stalls as soon as h falls under 4096, freezing the shape - the same integer-EMA
    deadband just fixed in the firmware, reintroduced by the tool that was meant to
    be checking it. Rebuilding once per block instead was wrong in a third way,
    quieter: no threshold exists during the first block, so every frame in it lands
    in one bucket, and on a 19 s window over a 23 min capture that is a fifth of the
    samples silently mislabelled.

    Counts are recomputed from the buffer every `every` frames rather than every
    frame: the quartiles of 4000 samples cannot move enough in 16 frames to change a
    bucket, and it keeps the loop cheap enough to run over an hour of capture.
    """
    out = np.zeros(len(z), dtype=np.int64)
    width = SPAN // nbins
    ring = np.zeros(window, dtype=np.int32)
    hist = np.zeros(nbins, dtype=np.int64)
    n_in = 0
    thr = None
    for i, v in enumerate(z):
        b = binidx(v, nbins)
        if n_in >= window:
            hist[ring[i % window]] -= 1
        else:
            n_in += 1
        ring[i % window] = b
        hist[b] += 1
        if i < warm or n_in < 64:
            out[i] = NP // 2
            continue
        if thr is None or i % every == 0:
            thr = block_quantiles(hist, nbins)
        p = 0
        for t in thr:
            if v > t:
                p += 1
        out[i] = p
    return out


def load(path):
    t, b = [], []
    with open(path) as fh:
        for r in csv.DictReader(fh):
            t.append(float(r["t"]))
            b.append([float(r[f"b{i}"]) for i in range(NB)])
    t = np.asarray(t)
    o = np.argsort(t)
    return t[o], np.asarray(b, dtype=np.float64)[o]


def row(a, b, warm, n, label, quantile=False):
    po, kap, mo, hd = q8fix.score(a[warm:], b[warm:])
    occ = [int(np.mean(a[warm:] == k) * 100) for k in range(NP)]
    occ += [int(np.mean(b[warm:] == k) * 100) for k in range(NP)]
    ch = [np.sum(np.diff(p[warm:]) != 0) / ((n - warm) / FPS / 60)
          for p in (a, b)]
    flag = "" if mo >= 0.18 else " <- under-used"
    if quantile and min(occ) < 15:
        # Quartile thresholds that do not quarter the data are not quartiles. This
        # is the check whose absence let two broken versions of this tool print
        # confident tables of nonsense.
        flag = " <- NOT QUARTILES, TOOL BUG"
    print(f"  {label:<26} {po*100:6.1f}% {kap:+7.3f} {mo*100:6.1f}% "
          f"{ch[0]:4.1f}/{ch[1]:<4.1f} " +
          "".join(f"{v:3d}/" for v in occ) + flag, flush=True)


def main():
    if len(sys.argv) < 2:
        raise SystemExit(__doc__)
    for d in sys.argv[1:]:
        data = {}
        for side in ("L", "R"):
            p = os.path.join(d, f"state_{side}.csv")
            if not os.path.exists(p):
                raise SystemExit(f"{p}: missing")
            data[side] = load(p)
        (tl, bl), (tr, br) = data["L"], data["R"]
        lo, hi = max(tl[0], tr[0]), min(tl[-1], tr[-1])
        tg = np.arange(lo, hi, 1.0 / FPS)
        GL = bl[np.clip(np.searchsorted(tl, tg), 0, len(tl) - 1)][:len(tg)]
        GR = br[np.clip(np.searchsorted(tr, tg), 0, len(tr) - 1)][:len(tg)]
        n = len(GL)
        warm = int(120 * FPS)
        print(f"\n=== {d}: {(n-warm)/FPS/60:.1f} min scored")
        print(f"  {'rule':<26} {'agree':>7} {'kappa':>7} {'minocc':>7} "
              f"{'ch/min':>9}  occupancy L then R")
        for wname in ("b4-b0", "b45-b01"):
            w = q8fix.CANDS[wname]
            for lp in (10, 11, 12):
                args = dict(q8=True, cshift=lp, rshift=lp, dshift=lp,
                            gshift=lp)
                zl, zr = (q8fix.run(G, w, **args)[4] for G in (GL, GR))
                for cut in (150,):
                    pl, pr = (q8fix.run(G, w, cut=cut, **args)[0]
                              for G in (GL, GR))
                    row(pl, pr, warm, n,
                        f"{wname} fixed c{cut} lp{2**lp//62}" if wname ==
                        "b4-b0" else f"{wname} fixed lp{2**lp//62}")
                # Quantile thresholds at three window lengths. The point of the
                # comparison is whether they buy the even colours fixed cuts cannot,
                # and what that costs in agreement.
                for wins in (2048, 6100, 12000):
                    row(rank_patterns(zl, warm, wins),
                        rank_patterns(zr, warm, wins), warm, n,
                        f"{wname} quart/{wins//62}s", quantile=True)
                # Median filter on the judged ratio, at the same time constant as
                # the average it would replace. A median kills single-frame spikes
                # without pulling section edges with them, which is the cost a
                # longer average pays.
                for k in (3, 5, 11, 31):
                    ml, mr = medfilt(zl, k), medfilt(zr, k)
                    # Bucket the filtered ratio with the same fixed cuts, so only
                    # the filter differs from the fixed row above. The ratio is
                    # already in 256ths of a spread, so a ruler of 256 makes
                    # index_gap apply the firmware's own clamp and nothing else.
                    f = lambda v: q8fix.index_gap(int(v), 256, 150)
                    row(np.array([f(v) for v in ml]),
                        np.array([f(v) for v in mr]), warm, n,
                        f"{wname} med{k} lp{2**lp//62}")


if __name__ == "__main__":
    main()
