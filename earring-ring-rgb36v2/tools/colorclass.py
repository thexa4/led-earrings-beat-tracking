#!/usr/bin/env python3
"""Search for a band-to-colour grouping that two earrings agree on.

The premise, and why it is different from retargeting PatternBands
------------------------------------------------------------------
PatternIndex is an argmax, and the documented failure of the wide version is a
ratio: the gap between the winning band and the runner-up is about half the
disagreement between two microphones hearing the same air, so which band wins is
close to a coin flip. That ratio is set by the music and the acoustics and cannot
be widened by choosing better bands - which is why picking a different candidate
set measured as a negative result.

So do not try to win the coin flip. Make it stop mattering. The pattern is only a
*colour*, and a disagreement between two neighbouring bands is a much smaller
perceptual error than a disagreement between two distant ones - so group the bands
and let the colour be a function of the group. A confusion that used to produce
two different colours now produces one. This is the same move the shipped
firmware makes implicitly when it reduces the band argmax modulo the number of
noise patterns; this tool makes it explicit and searches for the best grouping
instead of inheriting one.

What is searched
----------------
Every partition of the candidate bands into exactly k non-empty groups (S(8,4) =
1701 and S(8,5) = 1050, so this is exhaustive, not a heuristic), scored by:

  agreement  fraction of frames where both devices show the same colour
  chance     sum_i p_L[i] * p_R[i]: what two devices with the same colour
             *habits* would agree on with no shared signal at all
  kappa      (agreement - chance) / (1 - chance)

kappa is the number to optimise. A grouping that puts every band in one class
scores agreement 1.00 and chance 1.00, so kappa punishes exactly the degenerate
"stuck on one pattern" solution that raw agreement rewards, and it punishes a
grouping that only looks good because one colour dominates anyway. Occupancy is
also printed so balance is visible rather than inferred.

Overfitting is the other way to fool yourself here, so the tool refuses to report
a number from the same data it fitted on: pass two or more capture directories
and it fits on each in turn and reports the score on the others
(cross-validation). Fitting and scoring on one capture will find a grouping that
scores high on that capture alone; that number is a description of noise.

Excitation is recomputed on the host from the exported smoothed bands, applying
the pipeline's per-frame EMA once per exported row. Polling is slower than the
pipeline, so the effective time constant is longer than on the device. Every
candidate shares that distortion, so ranking candidates is fair; absolute scores
are not the device's.

  python3 tools/colorclass.py DIR [DIR...] [--k 4] [--bands 0-7]
"""
import argparse
import itertools
import os
import sys

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import specdump  # noqa: E402

NB = 8


def exc_from(snap, shift=6, warmup=120):
    """Emulate the pipeline's excitation: v - slow, slow being an EMA of v.

    Applied once per exported row rather than per pipeline frame; see the module
    docstring for why that is acceptable here.
    """
    # int64, not float: the update is an arithmetic shift, and numpy has no >>
    # on floats. The device does this in int32; magnitudes here are nowhere near
    # overflowing it.
    v = np.rint(snap).astype(np.int64)
    slow = v.copy()
    half = 1 << (shift - 1)
    out = np.zeros_like(v)
    for i in range(1, len(v)):
        slow[i] = slow[i - 1] + ((v[i] - slow[i - 1] + half) >> shift)
        out[i] = v[i] - slow[i]
    return out[warmup:], v[warmup:]


def load_pair(dir_, warmup=120, tol=0.08):
    """Load one capture (both sides), resampling R onto L's clock."""
    d = [specdump.load(os.path.join(dir_, f"spec-{s}.csv")) for s in ("L", "R")]
    tL, tR = d[0]["t"], d[1]["t"]
    sL = np.stack([d[0][f"snap{i}"] for i in range(NB)], axis=1)
    sR = np.stack([d[1][f"snap{i}"] for i in range(NB)], axis=1)
    # R interpolated onto L's timestamps: `t` is one shared wall clock in this
    # tool (both pollers run in one process), so this is honest alignment.
    out = np.zeros((len(tL), NB))
    for b in range(NB):
        out[:, b] = np.interp(tL, tR, sR[:, b])
    # Neighbour distance by bisection: an outer product of two ~7500-sample
    # clocks is 450 MB for no reason.
    idx = np.clip(np.searchsorted(tR, tL), 1, max(len(tR) - 1, 1))
    near = np.minimum(np.abs(tR[idx] - tL), np.abs(tR[idx - 1] - tL)) \
        if len(tR) > 1 else np.full(len(tL), np.inf)
    keep = near <= tol
    eL, _ = exc_from(sL, warmup=warmup)
    eR, _ = exc_from(out, warmup=warmup)
    keep = keep[warmup:]
    tt = tL[warmup:][keep]
    return eL[keep], eR[keep], tt


def argmax_on(exc, bands):
    """The winning band index, restricted to `bands`."""
    b = np.array(bands)
    return b[np.argmax(exc[:, b], axis=1)]


def score(eL, eR, bands, cls):
    """(agreement, chance, kappa, occupancy, changes per second)."""
    aL, aR = argmax_on(eL, bands), argmax_on(eR, bands)
    cL, cR = cls[aL], cls[aR]
    k = int(cls.max()) + 1
    agree = float(np.mean(cL == cR))
    n = len(cL)
    pL = np.bincount(cL, minlength=k) / n
    pR = np.bincount(cR, minlength=k) / n
    chance = float((pL * pR).sum())
    kap = (agree - chance) / (1 - chance) if chance < 1 else 0.0
    return agree, chance, kap, pL, pR


def run_length(c, dt):
    """Median and p90 of the length of a run of one colour, in seconds."""
    ch = np.flatnonzero(np.diff(c) != 0)
    edges = np.concatenate([[0], ch, [len(c) - 1]])
    ln = np.diff(edges) * dt
    if len(ln) < 2:
        return 0.0, 0.0
    return float(np.median(ln)), float(np.percentile(ln, 90))


def partitions(n, k):
    """All ways to put n items into k non-empty groups, as label lists."""
    cur = [0] * n

    def rec(i, used):
        if i == n:
            if used == k:
                yield list(cur)
            return
        for b in range(min(used + 1, k)):
            cur[i] = b
            yield from rec(i + 1, max(used, b + 1) if b == used else used)
            cur[i] = 0

    # item 0 always opens group 0; symmetry is broken by relabeling in `used`.
    cur[0] = 0
    yield from rec(1, 1)


def parse_bands(s):
    out = []
    for part in s.split(","):
        if "-" in part:
            a, b = part.split("-")
            out += list(range(int(a), int(b) + 1))
        else:
            out.append(int(part))
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("dirs", nargs="+")
    ap.add_argument("--k", type=int, default=4)
    ap.add_argument("--bands", default="0-7")
    ap.add_argument("--minocc", type=float, default=0.06,
                    help="drop groupings where any class is used less than this "
                         "on either device (a class nobody sees is not a pattern)")
    ap.add_argument("--top", type=int, default=8)
    a = ap.parse_args()
    bands = parse_bands(a.bands)

    data = {}
    for d in a.dirs:
        eL, eR, t = load_pair(d)
        dt = float(np.median(np.diff(t))) if len(t) > 2 else 1 / 26.0
        data[d] = (eL, eR, dt)
        print(f"{d}: {len(t)} aligned frames (~{t[-1] - t[0]:.0f} s), "
              f"dt={dt * 1000:.0f} ms")

    # Locality of the confusion decides whether grouping can help at all, so look
    # at it before trusting any grouping result.
    key = a.dirs[0]
    eL, eR, _ = data[key]
    aL, aR = argmax_on(eL, bands), argmax_on(eR, bands)
    C = np.zeros((8, 8), dtype=int)
    np.add.at(C, (aL, aR), 1)
    tot = C.sum()
    print(f"\nConfusion of the argmax over bands {bands} on {key} "
          f"(rows L, cols R, % of frames):")
    print("      " + "".join(f"{b:>6d}" for b in range(8)))
    for i in range(8):
        print(f"  L{i} " + "".join(f"{100 * C[i, j] / tot:6.1f}" for j in range(8)))
    adj = sum(C[i, j] for i in range(8) for j in range(8)
              if i != j and abs(i - j) == 1) / tot
    far = sum(C[i, j] for i in range(8) for j in range(8)
              if abs(i - j) > 1) / tot
    diag = np.trace(C) / tot
    print(f"  same band {100 * diag:.0f}%  |  adjacent band {100 * adj:.0f}%  "
          f"|  distant band {100 * far:.0f}%")
    print("  Grouping can only recover the adjacent part; if 'distant' dominates, "
          "no grouping of the band axis will agree.")

    # Baselines, scored on every capture.
    print(f"\nBaselines (k={a.k}):")
    # Each baseline competes over its own band set, which is part of what it is:
    # scoring argmax{2,4,6} with an argmax over all eight bands measures a
    # different selector than the one shipped.
    m3 = np.zeros(8, int)
    for pos, b in enumerate((2, 4, 6)):
        m3[b] = pos
    m5 = np.zeros(8, int)
    for b in range(1, 7):
        m5[b] = b % 5
    named = {
        "shipped: argmax{2,4,6}": ([2, 4, 6], m3),
        "master: argmax{1..6}%5": (list(range(1, 7)), m5),
        "argmax all 8 (ungrouped)": (list(range(8)), np.arange(8)),
        "adjacent pairs {01}{23}{45}{67}": (list(range(8)),
                                            np.array([0, 0, 1, 1, 2, 2, 3, 3])),
        "low/mid/high-mid/treble": (list(range(8)),
                                    np.array([1, 1, 1, 2, 2, 3, 3, 0])),
    }
    for nm, (bb, m) in named.items():
        parts = []
        for d in a.dirs:
            ag, ch, kap, pL, pR = score(data[d][0], data[d][1], bb, m)
            parts.append(f"{os.path.basename(d)}={100 * ag:5.1f}%"
                         f"(kappa {kap:4.2f})")
        print(f"  {nm:30s} {"  ".join(parts)}")

    if len(a.dirs) < 2:
        print("\nOne capture: fitted and scored on the same data, so treat the "
              "number as a lower bound on optimism. Pass a second directory.")

    # Exhaustive search, leave-one-capture-out.
    print(f"\nSearch over all partitions of {bands} into {a.k} groups:")
    cands = list(partitions(len(bands), a.k))
    print(f"  {len(cands)} groupings")
    hold = {}
    for fit_d in a.dirs:
        eL, eR, dt = data[fit_d]
        best = []
        for lab in cands:
            cls = np.zeros(8, int)
            cls[bands] = lab
            ag, ch, kap, pL, pR = score(eL, eR, bands, cls)
            if min(pL.min(), pR.min()) < a.minocc:
                continue
            best.append((kap, ag, cls))
        best.sort(key=lambda x: -x[0])
        hold[fit_d] = [b[2] for b in best[:a.top]]
        if best:
            print(f"  fitted on {os.path.basename(fit_d)}: best in-sample "
                  f"kappa {best[0][0]:.3f} agreement {100 * best[0][1]:.0f}% "
                  f"({len(best)} groupings passed the occupancy floor)")

    # Score each fitted grouping on the OTHER captures.
    print("\nCross-validated (fitted on one, scored on the other):")
    for fit_d in a.dirs:
        for cls in hold[fit_d][:5]:
            for ev_d in a.dirs:
                if ev_d == fit_d:
                    continue
                eL, eR, dt = data[ev_d]
                ag, ch, kap, pL, pR = score(eL, eR, bands, cls)
                med, p90 = run_length(cls[argmax_on(eL, bands)], dt)
                grp = "|".join(",".join(str(b) for i, b in enumerate(bands)
                                         if cls[b] == g) for g in range(a.k))
                print(f"  fit={os.path.basename(fit_d):14s} eval="
                      f"{os.path.basename(ev_d):14s} groups={{{grp:14s}}} "
                      f"agree {100 * ag:5.1f}% kappa {kap:5.3f} "
                      f"occ {np.round(pL, 2).tolist()} "
                      f"run {med:.1f}s/p90 {p90:.1f}s")


if __name__ == "__main__":
    main()
