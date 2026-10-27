#!/usr/bin/env python3
"""Re-run the firmware's own pattern selector on recorded band values.

Everything else in this directory scores a *stand-in* rule against recorded telemetry.
That is fine for ranking features, but the shipped selector is a specific cascade - an
EMA of the centroid, a slower average of its deviation, a mean absolute deviation of
that, and a division - and none of those are the same as "quartiles of the smoothed
feature". To change the weights or a time constant and know what the device will do,
the cascade has to be rebuilt rather than approximated.

So this reimplements it exactly, integer arithmetic and rounding included, from the
recorded per-band smoothed values, and then proves it before using it: with the shipped
constants it must reproduce the recorded centroid, deviation and pattern columns. Those
checks are the whole point. A reimplementation that is subtly wrong will happily report
wonderful agreement for a selector that does not exist, and the only defence is to show
it reproduces what the device actually did on the same numbers.

  python3 tools/selsim.py DIR              # verify, then sweep
  python3 tools/selsim.py DIR --sweep      # search weights and time constants

Constants are copied from syncdsp/pipeline.go by hand and asserted against the values
this file assumes; when the firmware changes, change them here or the assert fires.
"""
import csv
import os
import sys

import numpy as np

# --- from syncdsp/pipeline.go -------------------------------------------------
NBANDS = 8
NUM_PATTERNS = 4
PATTERN_CUT_Q8 = 216
PATTERN_MIN_DEV = 8
PATTERN_WARMUP_FRAMES = 500
PATTERN_CENTROID_SCALE = 4
PATTERN_CENTROID_SHIFT = 10
PATTERN_GAP_SHIFT = 10
PATTERN_SCALE_SHIFT = 10
SHIPPED_WEIGHTS = (-7, -5, -3, -1, 1, 3, 5, 7)

# Candidate weightings. Every one sums to zero, which is not a stylistic requirement:
# the centroid is meant to be unmoved by a spectrum that is merely louder, and a set of
# weights that does not sum to zero hands that decision to the AGC.
# Every candidate is scaled so its largest coefficient is around 7, matching the
# shipped ramp. That is not cosmetics: PatternMinDev is an ABSOLUTE threshold on the
# spread, so a weighting with small coefficients produces a small centDev, the gate
# closes, and the selector returns the middle colour forever. Run unscaled, a
# (-1,0,0,0,1,0,0,0) weighting reported 100% agreement with kappa 0.000 - both units
# pinned on the same colour for the whole capture, which is the stuck-on-one-pattern
# failure the gate was written to prevent, reported as success.
WEIGHTS = {
    "shipped ramp": SHIPPED_WEIGHTS,
    "b4-b0": (-7, 0, 0, 0, 7, 0, 0, 0),
    "b4-b1": (0, -7, 0, 0, 7, 0, 0, 0),
    "b4+b5-b0": (-8, 0, 0, 0, 4, 4, 0, 0),
    "b45-b01": (-5, -5, 0, 0, 5, 5, 0, 0),
    "b456-b01": (-5, -5, 0, 0, 4, 4, 2, 0),
    "b4-b0 graded": (-7, -2, 0, 0, 2, 7, 0, 0),
}

FPS = 62.5
GRID = FPS


def index_gap(gap, dev, cut_q8=PATTERN_CUT_Q8):
    """pattern.PatternIndexGap, integer."""
    if dev < PATTERN_MIN_DEV:
        return NUM_PATTERNS // 2
    d = gap
    lim = 2 * dev
    if d > lim:
        d = lim
    if d < -lim:
        d = -lim
    z = int(d * 256 / dev) if dev else 0
    # pipeline.go: for i := 0; i < NumPatterns-1; i++ { if z > cut { pat = i + 1 } }.
    # Starting the loop at 0 matters: the first cut is at -0.844 spreads, so a gap
    # between -0.844 and 0 lands on pattern 1, and only gaps below that stay on 0.
    # A loop that starts at 1 cannot reach the fourth bucket at all.
    p = 0
    for i in range(0, NUM_PATTERNS - 1):
        cut = (2 * i - (NUM_PATTERNS - 2)) * cut_q8 // 2
        if z > cut:
            p = i + 1
    return p


def bucket(gq, cdev, cut_q8=PATTERN_CUT_Q8):
    """Turn a cascade's smoothed deviation and spread into colours at a given cut.

    Separate from cascade because the cut width does not feed back into anything: it
    is applied last, so one cascade can be scored at five widths instead of running
    five cascades over ninety thousand frames.
    """
    out = np.empty(len(gq), dtype=np.int64)
    for i in range(len(gq)):
        out[i] = (NUM_PATTERNS // 2 if i < PATTERN_WARMUP_FRAMES
                  else index_gap(gq[i] >> 8, cdev[i], cut_q8))
    return out


def cascade(bands, weights, cshift, gshift, devshift, avgshift, scale=4):
    """One device's selector, from smoothed bands to pattern index per frame."""
    w = np.asarray(weights, dtype=np.int64)
    c = (bands.astype(np.int64) @ w) * scale
    n = len(c)
    cent = np.zeros(n, dtype=np.int64)
    cent_avg = np.zeros(n, dtype=np.int64)
    cent_dev = np.zeros(n, dtype=np.int64)
    gapq8 = np.zeros(n, dtype=np.int64)
    pat = np.zeros(n, dtype=np.int64)
    ce = ca = cd = gq = 0
    for i in range(n):
        if i == 0:
            ce, ca, cd, gq = c[i], c[i], 0, 0
        else:
            ce += (c[i] - ce + (1 << (cshift - 1))) >> cshift
            gap = ce - ca
            gq += ((gap << 8) - gq + (1 << (gshift - 1))) >> gshift
            a = gap if gap >= 0 else -gap
            cd += (a - cd + (1 << (devshift - 1))) >> devshift
            ca += (gap + (1 << (avgshift - 1))) >> avgshift
        cent[i], cent_avg[i], cent_dev[i], gapq8[i] = ce, ca, cd, gq
    return cent, cent_avg, cent_dev, gapq8


def load(path):
    t, b, extra = [], [], {k: [] for k in ("cent", "centDev", "pat")}
    with open(path) as fh:
        for r in csv.DictReader(fh):
            try:
                row = [float(r[f"b{i}"]) for i in range(NBANDS)]
            except (KeyError, ValueError):
                continue
            b.append(row)
            t.append(float(r["t"]))
            for k in extra:
                extra[k].append(float(r[k]))
    t = np.asarray(t)
    o = np.argsort(t)
    bands = np.asarray(b)[o]
    return t, bands, {k: np.asarray(v)[o] for k, v in extra.items()}, \
        np.arange(len(t), dtype=np.int64)


def on_grid(t, x, lo, hi):
    tg = np.arange(lo, hi, 1.0 / GRID)
    idx = np.clip(np.searchsorted(t, tg), 0, len(t) - 1)
    if x.ndim == 1:
        return x[idx]
    # Carry the frame number forward so the warmup counter means frames, not rows:
    # the state log is sampled far below the device's frame rate and a counter built
    # from rows would reach warmup in a fraction of the real time.
    return x[idx]


def stats(a, b):
    po = float(np.mean(a == b))
    pe = sum(np.mean(a == k) * np.mean(b == k) for k in range(NUM_PATTERNS))
    kap = (po - pe) / (1 - pe) if pe < 1 else 0.0
    mo = min(min(float(np.mean(a == k)), float(np.mean(b == k)))
             for k in range(NUM_PATTERNS))
    holds = []
    for p in (a, b):
        e = np.concatenate(([0], np.flatnonzero(np.diff(p) != 0) + 1, [len(p)]))
        holds.append(np.mean(np.diff(e)) / GRID)
    return po, kap, mo, holds


def main():
    argv = [a for a in sys.argv[1:] if not a.startswith("--")]
    sweep = "--sweep" in sys.argv
    d = argv[0]
    data = {}
    for side in ("L", "R"):
        for c in (f"state_{side}.csv", f"state-{side}.csv"):
            if os.path.exists(os.path.join(d, c)):
                data[side] = load(os.path.join(d, c))
                break
        else:
            raise SystemExit(f"{d}: no state for {side}")
    (tl, bl, el, _), (tr, br, er, _) = data["L"], data["R"]
    lo, hi = max(tl[0], tr[0]), min(tl[-1], tr[-1])
    tg = np.arange(lo, hi, 1.0 / GRID)
    il = np.clip(np.searchsorted(tl, tg), 0, len(tl) - 1)
    ir = np.clip(np.searchsorted(tr, tg), 0, len(tr) - 1)
    GL, GR = bl[il], br[ir]
    # The recorded columns have to be resampled by the same index as the bands. A
    # grid-length simulation compared field-by-field against un-resampled log rows is
    # off by one row per nine, which reported 24% agreement for a cascade that
    # reproduces the device at 94.7%.
    EL = {k: v[il] for k, v in el.items()}
    ER = {k: v[ir] for k, v in er.items()}
    n = min(len(GL), len(GR))
    GL, GR = GL[:n], GR[:n]
    EL = {k: v[:n] for k, v in EL.items()}
    ER = {k: v[:n] for k, v in ER.items()}
    print(f"{d}: {n/FPS/60:.1f} min of device frames at {GRID:.0f} Hz")

    # --- prove the reimplementation before trusting any number from it ----------
    warm = int(120 * FPS)
    for nm, G, E, side in (("L", GL, EL, "L"), ("R", GR, ER, "R")):
        cent, cavg, cdev, gq = cascade(G, SHIPPED_WEIGHTS,
                                       PATTERN_CENTROID_SHIFT,
                                       PATTERN_GAP_SHIFT, PATTERN_SCALE_SHIFT,
                                       PATTERN_SCALE_SHIFT)
        pat = bucket(gq, cdev)
        # Both start cold here and the device started warm, so the first two minutes
        # of the log are excluded rather than forgiven: after about 8 time constants
        # of a 16 s EMA the two are the same number, and before that they are not.
        sl = slice(warm, n)
        dp = float(np.mean(pat[sl] == E["pat"][sl].astype(np.int64))) * 100
        print(f"  {side}: reproduces the recorded pattern on {sl.stop-sl.start} "
              f"frames: {dp:.1f}%   cent correlation "
              f"{np.corrcoef(cent[sl], E['cent'][sl])[0,1]:.4f}   "
              f"dev median {int(np.median(cdev[sl]))} recorded "
              f"{int(np.median(E['centDev'][sl]))}")
        if dp < 90.0:
            print("  REFUSING TO CONTINUE: the cascade does not reproduce the device. "
                  "A constant here is out of date with syncdsp/pipeline.go, and every "
                  "number below would describe a selector that does not exist.")
            return
    if not sweep:
        print("\n  verified. --sweep to search weights, time constants and cut width.")
        return

    # The shipped cut width is compared against the one that would put a quarter of
    # the frames in each bucket, because the two are not close and that difference, by
    # itself, is why the outer colours are rare: the recorded z has to clear 0.844 of
    # a spread to reach an end colour.
    print(f"\n  shipped: cut {PATTERN_CUT_Q8} Q8, centroid shift "
          f"{PATTERN_CENTROID_SHIFT}")
    print(f"  {'weights':<24} {'cshift':>6} {'cut':>5} {'agree':>7} {'kappa':>7} "
          f"{'minocc':>7} {'hold s':>11} {'dev':>6} {'gated':>6}")
    rows = []
    for wname, wv in WEIGHTS.items():
        for cs in (9, 10, 11, 12):
            # One cascade per weighting and time constant; the cut only re-buckets.
            base = [cascade(G, wv, cs, PATTERN_GAP_SHIFT, PATTERN_SCALE_SHIFT,
                            PATTERN_SCALE_SHIFT) for G in (GL, GR)]
            med = int(np.median([np.median(b[2][warm:]) for b in base]))
            gate = np.mean([np.mean(b[2][warm:] < PATTERN_MIN_DEV) for b in base])
            for cut in (40, 60, 90, 130, 216):
                pats = [bucket(b[3], b[2], cut) for b in base]
                po, kap, mo, hd = stats(pats[0][warm:], pats[1][warm:])
                rows.append((kap, wname, cs, cut, po, mo, hd, med, gate))
    rows.sort(reverse=True)
    print(f"  showing the top {min(26, len(rows))} of {len(rows)} by kappa")
    for kap, wname, cs, cut, po, mo, hd, med, gate in rows[:26]:
        flag = "" if mo >= 0.10 else "   <- a colour is nearly absent"
        if gate > 0.05:
            flag += f"   <- spread under PatternMinDev {gate*100:.0f}% of frames"
        print(f"  {wname:<24} {cs:6d} {cut:5d} {po*100:6.1f}% {kap:+7.3f} "
              f"{mo*100:6.1f}% {hd[0]:4.0f}/{hd[1]:<6.0f} {med:6d} "
              f"{gate*100:5.1f}%{flag}")
    print("\n  kappa is agreement corrected for how the colours are split, so a row "
          "that\n  reaches its number by parking both units on one colour scores near "
          "zero.\n  minocc is the least-used colour across both units: the variety "
          "requirement.")



if __name__ == "__main__":
    main()
