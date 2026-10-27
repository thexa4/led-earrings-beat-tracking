#!/usr/bin/env python3
"""Score candidate pattern rules by pair agreement and evenness of the four colours.

The shipped rule reads the spectral centroid, which is deliberately blind to level:
its weights sum to zero so that two microphones with different gain, distance or room
response still answer the same question about timbre. On this bench that design choice
has removed the only term the pair agrees on. Measured over the same minutes, with
each band low-passed at 8 s, every individual band correlates across the two units at
+0.13 to +0.36 and the mean of all eight - overall loudness - at +0.31, while the
zero-sum combination of the same bands sits at -0.43. Bands move together; they do not
move in shape. So the centroid is differencing two things that share a level and
little else, and the difference is mostly each device's own drift.

This tool therefore scores rules instead of features, and puts evenness next to
agreement, because a rule that reaches 90% by parking both units on one colour is
worth nothing - the complaint is specifically that the earrings "don't always get
stuck on one pattern". Three numbers per rule:

  agree   fraction of frames showing the same colour
  kappa   the same, corrected for how the colours are distributed, so an even rule and
          a lopsided one can be compared at all
  minocc  the least-used colour across both units - the variety requirement, and the
          number that killed every earlier candidate

Rules are cut per device on that device's own distribution. That is not a detail:
two earrings have different steady-state levels, so a threshold fitted on one is a
permanent bias on the other. Percentiles are taken over the whole run here, which is
not causal - read the `causal` variants for what a device could actually compute, and
treat the whole-run rows as the ceiling.

  python3 tools/rulescore.py DIR
"""
import csv
import os
import sys

import numpy as np

NP = 4
GRID = 10.0


def load(path):
    cols = ("t", "agc", "cent", "centAvg", "centDev", "gapQ8", "pat")
    d = {c: [] for c in cols}
    bands = []
    with open(path) as fh:
        for r in csv.DictReader(fh):
            try:
                row = [float(r[f"b{i}"]) for i in range(8)]
            except (KeyError, ValueError):
                continue
            bands.append(row)
            for c in cols:
                v = r.get(c, "")
                d[c].append(float(v) if v not in ("", None) else np.nan)
    t = np.asarray(d["t"])
    o = np.argsort(t)
    out = {c: np.asarray(v)[o] for c, v in d.items()}
    out["bands"] = np.asarray(bands)[o]
    return out


def grid(t, x, lo, hi):
    tg = np.arange(lo, hi, 1.0 / GRID)
    return x[np.clip(np.searchsorted(t, tg), 0, len(t) - 1)]


def lowpass(x, sec):
    n = max(1, int(round(sec * GRID)))
    if n == 1:
        return x
    k = np.ones(n) / n
    y = np.convolve(x, k, mode="valid")
    pad = len(x) - len(y)
    return np.concatenate([np.full(pad // 2, y[0]), y,
                           np.full(pad - pad // 2, y[-1])])


def cut_fixed(x, cuts):
    p = np.zeros(len(x), dtype=np.int64)
    for c in cuts:
        p += (x > c).astype(np.int64)
    return p


def cut_own(x):
    """Quantiles of the whole run: not causal, gives the ceiling."""
    return cut_fixed(x, np.quantile(x, [(i + 1) / NP for i in range(NP - 1)]))


def cut_causal(x, seconds=45.0):
    """Running quantiles from a trailing window. Recomputed every 2 s, which is what
    a device could afford; the held value is what makes it honest rather than a peek."""
    win = int(seconds * GRID)
    step = max(1, int(2.0 * GRID))
    med = np.empty(len(x))
    iqr = np.empty(len(x))
    for i in range(len(x)):
        if i % step == 0 or i == len(x) - 1:
            w = x[max(0, i - win):i + 1]
            med[i] = np.median(w)
            q1, q3 = np.percentile(w, [25, 75])
            iqr[i] = max(q3 - q1, 1e-9)
        else:
            med[i], iqr[i] = med[i - 1], iqr[i - 1]
    z = (x - med) / iqr
    # Quartiles of a roughly-normal variable in interquartile units.
    return cut_fixed(z, [-0.43, 0.0, 0.43])


def report(a, b, label):
    po = float(np.mean(a == b))
    pe = sum(np.mean(a == k) * np.mean(b == k) for k in range(NP))
    kap = (po - pe) / (1 - pe) if pe < 1 else 0.0
    mo = min(min(float(np.mean(a == k)), float(np.mean(b == k)))
             for k in range(NP))
    runs = []
    for p in (a, b):
        e = np.concatenate(([0], np.flatnonzero(np.diff(p) != 0) + 1, [len(p)]))
        runs.append(np.mean(np.diff(e)) / GRID)
    print(f"  {label:<34} agree {po*100:5.1f}%  kappa {kap:+.3f}  "
          f"minocc {mo*100:5.1f}%  hold {runs[0]:4.1f}/{runs[1]:4.1f}s")
    return po, kap, mo


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
    L, R = data["L"], data["R"]
    lo, hi = max(L["t"][0], R["t"][0]), min(L["t"][-1], R["t"][-1])
    G = {s: {k: grid(data[s]["t"], v, lo, hi)
             for k, v in data[s].items()} for s in ("L", "R")}
    n = min(len(G["L"]["t"]), len(G["R"]["t"]))
    warm = int(30 * GRID)
    for s in ("L", "R"):
        for k in G[s]:
            G[s][k] = G[s][k][warm:n]
    print(f"{d}: {n-warm} frames at {GRID:.0f} Hz over "
          f"{(n-warm)/GRID/60:.1f} min, first 30 s dropped")

    def feats(g):
        f = {}
        # Loudness, the term the centroid weights cancel out.
        f["level"] = g["bands"].mean(1)
        f["agcPeak"] = g["agc"]
        # Timbre, what ships today.
        f["centroid"] = g["bands"] @ np.array([-7, -5, -3, -1, 1, 3, 5, 7.])
        f["gapQ8"] = g["gapQ8"] / 256.0
        return f

    Fl, Fr = feats(G["L"]), feats(G["R"])
    print("\n  recorded pattern, exactly as the devices chose it:")
    report(G["L"]["pat"].round().astype(np.int64),
           G["R"]["pat"].round().astype(np.int64), "shipped (centroid on gapQ8)")

    for sec in (0.5, 2.0, 4.0, 8.0, 16.0):
        print(f"\n  low-passed {sec:g} s:")
        for k in ("level", "agcPeak", "centroid", "gapQ8"):
            x, y = lowpass(Fl[k], sec), lowpass(Fr[k], sec)
            cuts = np.quantile(np.concatenate([x, y]),
                               [(i + 1) / NP for i in range(NP - 1)])
            report(cut_fixed(x, cuts), cut_fixed(y, cuts), f"{k} shared cuts")
            report(cut_own(x), cut_own(y), f"{k} own quantiles")
            report(cut_causal(x), cut_causal(y), f"{k} own running quartiles")
    print("\n  minocc under ~10% means a colour is effectively absent, whatever the")
    print("  agreement says. hold is how long a colour sits: a person needs ~1 s.")
    print("  'own running quartiles' is the only column a device could implement.")


if __name__ == "__main__":
    main()
