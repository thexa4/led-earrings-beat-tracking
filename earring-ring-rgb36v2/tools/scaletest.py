#!/usr/bin/env python3
"""At what timescale, if any, do the two earrings see the same thing?

Two facts set this up. The band envelopes barely correlate between the units, while
inside ONE unit all eight bands correlate with each other at +0.93. Bands that move
together and not in shape is the signature of level motion - and level is what the AGC
and the zero-sum centroid weights are built to throw away. So the shape signal the
pattern is chosen from is small, and the question is not which clever combination of
bands recovers it but whether one exists at any smoothing time constant at all.

Sweeping the timescale is the cheapest way to find out, because it also separates two
failures that look alike:

  a feature whose correlation rises with smoothing and then falls had real content at
    that scale, buried under noise;
  a feature that never rises has no shared content at any scale, and no selector
    applied to it can make a pair agree - which retunes the question from "find better
    cuts" to "find a different input".

Reported per feature: Pearson r at each smoothing window, plus the same after removing
each device's own slow drift, which is the difference between "they hear the same
music" and "both instruments wander". A negative r at short windows that turns
positive at long ones is normal and means the noise is white and the music is slow.

  python3 tools/scaletest.py DIR
"""
import csv
import os
import sys

import numpy as np

WEIGHTS = np.array([-7, -5, -3, -1, 1, 3, 5, 7], dtype=np.float64)
NB = 8
GRID = 10.0
# Half-band edges in the order syncdsp numbers them: b0 is above 4 kHz, b7 below 250.
NAMES = [">4k", "2.5-4k", "1.6-2.5k", "1-1.6k", "630-1k", "400-630", "250-400", "<250"]


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


def on_grid(t, x, lo, hi):
    tg = np.arange(lo, hi, 1.0 / GRID)
    idx = np.clip(np.searchsorted(t, tg), 0, len(t) - 1)
    return x[idx]


def movmean(x, sec):
    n = max(1, int(round(sec * GRID)))
    if n == 1:
        return x
    k = np.ones(n) / n
    # 'valid' then pad so the length is stable without the edge effects that
    # mode='same' introduces at both ends of the recording.
    y = np.convolve(x, k, mode="valid")
    pad = len(x) - len(y)
    return np.concatenate([np.full(pad // 2, y[0]), y,
                           np.full(pad - pad // 2, y[-1])])


def r(a, b):
    if a.std() < 1e-12 or b.std() < 1e-12:
        return 0.0
    return float(np.corrcoef(a, b)[0, 1])


def detrend(x, sec):
    """Remove a slow moving average: what remains is the part a stationary
    microphone-position difference cannot contribute."""
    return x - movmean(x, sec)


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
    gl, gr = on_grid(tl, bl, lo, hi), on_grid(tr, br, lo, hi)
    n = min(len(gl), len(gr))
    gl, gr = gl[:n], gr[:n]
    # Drop the first 30 s: the AGC and every EMA are still converging there and an
    # unsettled start correlates with nothing but itself.
    warm = int(30 * GRID)
    gl, gr = gl[warm:], gr[warm:]
    print(f"{d}: {len(gl)} grid frames at {GRID:.0f} Hz over "
          f"{len(gl)/GRID/60:.1f} min (first 30 s dropped)")

    scalars = {
        "centroid": gl @ WEIGHTS,
        "b7-b0": gl[:, 7] - gl[:, 0],
        "lowhigh": gl[:, 6:8].mean(1) - gl[:, 0:2].mean(1),
    }
    scalars_r = {
        "centroid": gr @ WEIGHTS,
        "b7-b0": gr[:, 7] - gr[:, 0],
        "lowhigh": gr[:, 6:8].mean(1) - gr[:, 0:2].mean(1),
    }
    scales = [0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64]
    print(f"\n  cross-device r of each feature, low-passed at:"
          f"\n  {'feature':<11}" + "".join(f"{s:>7}{'s':<1}" for s in scales))
    for k in scalars:
        row = "".join(f"{r(movmean(scalars[k], s), movmean(scalars_r[k], s)):+7.2f} "
                      for s in scales)
        print(f"  {k:<11}{row}")
    for i in range(NB):
        row = "".join(f"{r(movmean(gl[:, i], s), movmean(gr[:, i], s)):+7.2f} "
                      for s in scales)
        print(f"  b{i} {NAMES[i]:<8}{row}")

    # Level: the thing that is deliberately discarded, shown for contrast. If the two
    # units' overall level correlates better than any shape feature, they are hearing
    # the music's loudness and nothing else reaches the pattern.
    lv_l, lv_r = gl.mean(1), gr.mean(1)
    row = "".join(f"{r(movmean(lv_l, s), movmean(lv_r, s)):+7.2f} " for s in scales)
    print(f"  {'LEVEL':<11}{row}   (mean of all 8 bands; NOT used by the pattern)")

    # Remove each device's own slow drift so a fixed position/response difference
    # cannot be mistaken for shared content, then re-measure the best feature.
    print("\n  after removing each device's own drift over 60 s:")
    for k in scalars:
        x, y = detrend(scalars[k], 60.0), detrend(scalars_r[k], 60.0)
        row = "".join(f"{r(movmean(x, s), movmean(y, s)):+7.2f} " for s in scales)
        print(f"  {k:<11}{row}")

    # How much shape is there at all? Within one device, compare a band's deviation
    # from that device's own mean band level against the level's own motion. If the
    # shape term is a small fraction of the level term there is no cross-device shape
    # correlation to find, and that is the end of the feature search rather than a
    # tuning problem.
    print("\n  within one device: level motion vs shape motion (Q8 counts)")
    for nm, g in (("L", gl), ("R", gr)):
        lvl = g.mean(1)
        shape = g - lvl[:, None]
        print(f"    {nm}: level sd {lvl.std():7.0f}   per-band shape sd " +
              " ".join(f"{shape[:, i].std():5.0f}" for i in range(NB)) +
              f"\n       shape/level ratio {shape.std()/max(lvl.std(), 1e-9):.3f} "
              f"(real spectral shape motion is ~0.3-1; ~0.05 means the eight bands "
              f"are one level signal)")


if __name__ == "__main__":
    main()
