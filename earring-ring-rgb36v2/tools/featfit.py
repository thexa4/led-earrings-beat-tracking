#!/usr/bin/env python3
"""Search for a pattern feature that two independent earrings agree on.

The shipped rule divides the centroid's deviation by its own exponential spread and
compares that against fixed cuts. It scores ~chance on the present bench. This sweeps
alternative features and alternative normalisations, and scores each the way the user
asked for: pair agreement, and how evenly the four colours are used.

The normalisation matters more than the feature. Two earrings at different spots in a
room have different steady-state spectra - distance rolls off the top end, a corner
booms the bottom - so their features sit at different offsets with different gains. A
fixed cut compares those two different numbers directly and one unit is permanently
tilted. Dividing by that device's OWN spread removes the gain; subtracting its own
centre removes the offset. The two extremes here are deliberately both shown:

  fixed   raw feature against cuts fitted on the pooled data (what ships today: an
          offset between the units is a permanent disagreement)
  own     each device quantile-cut on its own distribution

The `own` rows use quartiles of the WHOLE recording, which is not causal - no device
can see the future. Treat them as the ceiling that a running estimate is chasing, and
read the causal rows against them, not against `fixed`.

Scored at zero lag only for the verdict. A lag sweep is printed for the best few
because a feature that only agrees after sliding one device by seconds is not a
feature two earrings can share.

  python3 tools/featfit.py DIR
"""
import csv
import os
import sys

import numpy as np

WEIGHTS = np.array([-7, -5, -3, -1, 1, 3, 5, 7], dtype=np.float64)
NB = 8
NP = 4
GRID = 20.0  # Hz; polls arrive around 9 Hz so this interpolates, never decimates


def load(path):
    t, bands = [], []
    with open(path) as fh:
        for r in csv.DictReader(fh):
            try:
                t.append(float(r["t"]))
                bands.append([float(r[f"b{i}"]) for i in range(NB)])
            except (KeyError, ValueError):
                continue
    t = np.asarray(t)
    o = np.argsort(t)
    return t[o], np.asarray(bands, dtype=np.float64)[o]


def grid_series(t, x, lo, hi):
    """Nearest-hold onto a uniform grid. Holds the last polled value, which is what
    the device would have kept choosing between polls."""
    tg = np.arange(lo, hi, 1.0 / GRID)
    idx = np.clip(np.searchsorted(t, tg), 0, len(t) - 1)
    return tg, x[idx]


def smooth(x, seconds):
    n = max(1, int(round(seconds * GRID)))
    if n == 1:
        return x
    k = np.ones(n) / n
    return np.convolve(x, k, mode="same")


def quantile_cut(x, causal):
    """Map to 0..NP-1. causal=True uses a running median and interquartile range
    estimated from a trailing window, so it could run on a device."""
    if not causal:
        cuts = np.quantile(x, [(i + 1) / NP for i in range(NP - 1)])
        p = np.zeros(len(x), dtype=np.int64)
        for c in cuts:
            p += (x > c).astype(np.int64)
        return p
    n = int(round(30.0 * GRID))  # 30 s trailing window
    p = np.zeros(len(x), dtype=np.int64)
    step = max(1, n // 8)
    med = np.empty(len(x))
    iqr = np.empty(len(x))
    for i in range(len(x)):
        w = x[max(0, i - n):i + 1]
        if i % step == 0 or i == len(x) - 1:
            med[i] = np.median(w)
            q1, q3 = np.percentile(w, [25, 75])
            iqr[i] = q3 - q1
        else:
            med[i] = med[i - 1]
            iqr[i] = iqr[i - 1]
    s = (x - med) / np.maximum(iqr, 1e-9)
    for q in (-0.43, 0.0, 0.43):  # standard-normal quartiles of a z-like variable
        p += (s > q).astype(np.int64)
    return p


def agree(a, b):
    po = float(np.mean(a == b))
    pe = sum(np.mean(a == k) * np.mean(b == k) for k in range(NP))
    kap = (po - pe) / (1 - pe) if pe < 1 else 0.0
    occ = min(float(np.mean(a == k)) for k in range(NP))
    occ2 = min(float(np.mean(b == k)) for k in range(NP))
    return po, kap, min(occ, occ2)


def hold_seconds(p):
    """Mean run length, which is what makes a colour readable at all."""
    if len(p) < 2:
        return 0.0
    ch = np.flatnonzero(np.diff(p) != 0)
    edges = np.concatenate(([0], ch + 1, [len(p)]))
    return float(np.mean(np.diff(edges))) / GRID


def best_lag(a, b, maxsec):
    n = int(maxsec * GRID)
    best, bl = -1.0, 0
    for lag in range(-n, n + 1, 2):
        if lag >= 0:
            x, y = a[lag:], b[:len(b) - lag]
        else:
            x, y = a[:lag], b[-lag:]
        if len(x) < 200:
            continue
        m = float(np.mean(x == y))
        if m > best:
            best, bl = m, lag
    return best, bl / GRID


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
    _, gl = grid_series(tl, bl, lo, hi)
    _, gr = grid_series(tr, br, lo, hi)
    n = min(len(gl), len(gr))
    gl, gr = gl[:n], gr[:n]
    print(f"{d}: {n} grid frames at {GRID:.0f} Hz over {(hi-lo)/60:.1f} min")

    def feats(bands):
        f = {}
        f["centroid"] = bands @ WEIGHTS
        f["b7-b0"] = bands[:, 7] - bands[:, 0]
        f["lowhigh"] = bands[:, 6:8].mean(1) - bands[:, 0:2].mean(1)
        f["bass-air"] = bands[:, 7] - bands[:, 0:2].mean(1)
        f["mid-tilt"] = bands[:, 4:6].mean(1) - bands[:, 2:4].mean(1)
        for s in (0.25, 0.5, 1.0, 2.0, 4.0):
            f[f"centroid@{s}s"] = smooth(bands @ WEIGHTS, s)
            f[f"lowhigh@{s}s"] = smooth(bands[:, 6:8].mean(1)
                                        - bands[:, 0:2].mean(1), s)
        return f

    Fl, Fr = feats(gl), feats(gr)
    shared = [k for k in Fl if k in Fr]
    print(f"\n  {'feature':<16} {'norm':<7} {'agree':>6} {'kappa':>7} "
          f"{'minocc':>7} {'hold s':>7}  {'lag':>6}")
    rows = []
    for k in shared:
        x, y = Fl[k], Fr[k]
        for norm in ("fixed", "own", "causal"):
            if norm == "fixed":
                pooled = np.concatenate([x, y])
                cuts = np.quantile(pooled, [(i + 1) / NP for i in range(NP - 1)])
                pa = sum((x > c).astype(np.int64) for c in cuts)
                pb = sum((y > c).astype(np.int64) for c in cuts)
            elif norm == "own":
                pa = quantile_cut(x, False)
                pb = quantile_cut(y, False)
            else:
                pa = quantile_cut(x, True)
                pb = quantile_cut(y, True)
            po, kap, mo = agree(pa, pb)
            h = 0.5 * (hold_seconds(pa) + hold_seconds(pb))
            rows.append((k, norm, po, kap, mo, h))
    rows.sort(key=lambda r: -r[2])

    def cut(k, norm):
        x, y = Fl[k], Fr[k]
        if norm == "fixed":
            cuts = np.quantile(np.concatenate([x, y]),
                               [(i + 1) / NP for i in range(NP - 1)])
            return (sum((x > c).astype(np.int64) for c in cuts),
                    sum((y > c).astype(np.int64) for c in cuts))
        return quantile_cut(x, norm == "causal"), quantile_cut(y, norm == "causal")

    # Only the top few get the lag sweep: it is a few hundred full-array compares,
    # and its purpose is to expose a candidate that works only by sliding one device
    # through time, which is not something two earrings can do.
    labels = {}
    for k, norm, po, kap, mo, h in rows[:6]:
        pa, pb = cut(k, norm)
        b, ls = best_lag(pa, pb, 3.0)
        labels[(k, norm)] = f"{b*100:.0f}% @{ls:+.1f}s"
    for k, norm, po, kap, mo, h in rows:
        print(f"  {k:<16} {norm:<7} {po*100:5.1f}% {kap:+7.3f} {mo*100:6.1f}% "
              f"{h:7.2f}  {labels.get((k, norm), ''):>12}")
    print("\n  minocc = least-used colour across both units; below ~10% a colour is")
    print("  effectively missing. hold s = how long a colour sits, needs >= 1 s to")
    print("  be readable by a person.")


if __name__ == "__main__":
    main()
