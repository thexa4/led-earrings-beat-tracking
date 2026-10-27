#!/usr/bin/env python3
"""Do the two earrings' features agree before any selector is applied?

Pattern agreement is a function of one number per device per frame, so asking a
selector to fix a pair whose features disagree is asking it to invent information.
This measures the features directly and nothing else: the smoothed band spectrum the
DSP selects from, and the centroid derived from it, both at the same room moment.

Pairing is by the host timestamp. paircapture runs both pollers in one process and
stamps one shared clock, so equal t is the same instant in the room - which is what
makes a correlation here meaningful. It is NOT the device frame counter: that is
each device's own time and matching on it is circular (BENCH.md section 3).

Read the per-band matrix before the headline. Two microphones in the same room
hearing the same music have band envelopes that move together with different
amplitudes, so a per-band correlation near zero means the two are not looking at a
common signal; a set of bands that correlate with each other far better than they
correlate across devices means the array is carrying one shared thing that is not
the music.

  python3 tools/featurecorr.py DIR
"""
import csv
import os
import sys

import numpy as np

# syncdsp/consts.go: the zero-sum ramp the centroid is built from, and its headroom
# scale. Mirrored as literals so a source change cannot silently redefine this.
WEIGHTS = np.array([-7, -5, -3, -1, 1, 3, 5, 7], dtype=np.float64)
SCALE = 4.0
NB = 8


def load(path):
    rows = []
    with open(path) as fh:
        for r in csv.DictReader(fh):
            try:
                rows.append([float(r["t"]), float(r["cent"])] +
                            [float(r[f"b{i}"]) for i in range(NB)])
            except (KeyError, ValueError):
                continue
    a = np.asarray(rows, dtype=np.float64)
    order = np.argsort(a[:, 0])
    return a[order]


def main():
    d = sys.argv[1]
    data = {}
    for side in ("L", "R"):
        for cand in (f"state_{side}.csv", f"state-{side}.csv"):
            p = os.path.join(d, cand)
            if os.path.exists(p):
                data[side] = load(p)
                break
        else:
            raise SystemExit(f"{d}: no state file for {side}")
    L, R = data["L"], data["R"]

    lo = max(L[0, 0], R[0, 0])
    hi = min(L[-1, 0], R[-1, 0])
    keep = (L[:, 0] >= lo) & (L[:, 0] <= hi)
    L = L[keep]
    # Nearest row of R for each row of L, in host time.
    idx = np.clip(np.searchsorted(R[:, 0], L[:, 0]), 0, len(R) - 1)
    low = np.clip(idx - 1, 0, len(R) - 1)
    pick = np.where(np.abs(R[idx, 0] - L[:, 0]) <=
                    np.abs(R[low, 0] - L[:, 0]), idx, low)
    near = np.abs(R[pick, 0] - L[:, 0])
    good = near < 0.5
    L, Rm = L[good], R[pick][good]
    print(f"{d}: {len(L)} matched pairs over {(hi-lo)/60:.1f} min, "
          f"median |dt| {np.median(near[good])*1000:.0f} ms")

    def corr(a, b):
        if a.std() < 1e-9 or b.std() < 1e-9:
            return 0.0
        return float(np.corrcoef(a, b)[0, 1])

    sl, sr = L[:, 2:], Rm[:, 2:]
    print(f"\n  per-band envelope correlation (smoothed, Q8 log2):")
    line = []
    for i in range(NB):
        line.append(f"b{i} {corr(sl[:, i], sr[:, i]):+.2f}")
    print("    " + "  ".join(line))

    cl = L[:, 1]
    cr = Rm[:, 1]
    print(f"\n  centroid (device accumulator)   r = {corr(cl, cr):+.3f}")

    # The instantaneous centroid, recomputed from the bands. The device's `cent` is a
    # 16 s EMA of this, which correlates almost anything: two slow EMA of two
    # independent noises still track their own means. The per-frame value cannot hide
    # behind that, so this is the honest feature.
    inst_l = SCALE * (sl @ WEIGHTS)
    inst_r = SCALE * (sr @ WEIGHTS)
    print(f"  centroid per frame (from bands) r = {corr(inst_l, inst_r):+.3f}")

    # Level: the AGC plus smoothing should make this nearly constant. If it moves,
    # and moves together, that shared part is the one thing a zero-sum centroid
    # deliberately throws away - worth knowing before concluding the pair is
    # disagreeing about timbre.
    tl, tr = sl.mean(1), sr.mean(1)
    print(f"  total level (mean band)         r = {corr(tl, tr):+.3f}   "
          f"L sd {tl.std():.0f}  R sd {tr.std():.0f}")

    # The centroid is already level-invariant by construction - the weights sum to
    # zero, so adding the same amount to every band leaves it unchanged. Nothing has
    # to be removed to see the tilt, and a correlation already ignores the mean, so
    # the number above is the tilt. What is worth reporting instead is how much of
    # each device's centroid is explained by its own level: if most of it is, the
    # feature is carrying loudness the weights were chosen to discard.
    def explained(y, x):
        if y.std() < 1e-9 or x.std() < 1e-9:
            return 0.0
        return corr(y, x) ** 2
    print(f"  centroid vs own level: L explains "
          f"{explained(inst_l, tl)*100:.0f}%  R explains "
          f"{explained(inst_r, tr)*100:.0f}%")

    # Within-device control: how correlated are the bands with EACH OTHER? If b0..b7
    # all move together inside one device, that device is not resolving a spectrum.
    print("\n  within-device band correlation (control):")
    for nm, s in (("L", sl), ("R", sr)):
        c = np.corrcoef(s.T)
        off = c[~np.eye(NB, dtype=bool)]
        print(f"    {nm}: mean off-diagonal {off.mean():+.2f}  "
              f"min {off.min():+.2f}  max {off.max():+.2f}")

    print("\n  reading it: cross-band r near zero means no common signal; cross-band r")
    print("  high but within-device higher still means an array moving as one thing.")


if __name__ == "__main__":
    main()
