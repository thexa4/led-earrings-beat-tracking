#!/usr/bin/env python3
"""Which deviation should the colour be read from: the raw one or the low-passed one?

The shipped selector reads PatternIndexGap(centGapQ8>>8, centDev) - the deviation
after a PatternGapShift low-pass, over the spread of the RAW deviation. That low-pass
was chosen on a host-replay score and put on the hardware, after which measured pair
agreement went from 47.4% to 27.0% on hour-long captures of the same rig. Both
quantities involved are recorded, so the two rules can be compared on real hardware
data with no reconstruction at all: cent, centAvg, centDev and centGapQ8 are all
polled every sample.

Everything here is therefore exact arithmetic on recorded values. Nothing is
resampled, interpolated or simulated. The only judgement is the pairing, which is by
host timestamp - both pollers run in one process and stamp a shared t, so equal t is
the same room moment.

  python3 tools/gapcompare.py DIR
"""
import csv
import os
import sys

import numpy as np

# Mirrors of syncdsp/consts.go. Kept as literals rather than parsed out of the source
# so that a change to the source cannot silently redefine what this script means; if
# they drift, the sanity check on the recorded pat column is what notices.
NUM_PATTERNS = 4
PATTERN_CUT_Q8 = 216
PATTERN_MIN_DEV = 8


def pattern_index_gap(gap, cent_dev):
    """PatternIndexGap, integer-for-integer."""
    gap = np.asarray(gap, dtype=np.int64)
    cent_dev = np.asarray(cent_dev, dtype=np.int64)
    pat = np.zeros(gap.shape, dtype=np.int64)
    ok = cent_dev >= PATTERN_MIN_DEV
    lim = cent_dev * 2
    d = np.clip(gap, -np.where(ok, lim, -1), np.where(ok, lim, 1))
    d = np.where(gap > lim, lim, np.where(gap < -lim, -lim, gap))
    z = d * 256 // np.where(cent_dev == 0, 1, cent_dev)
    for i in range(NUM_PATTERNS - 1):
        cut = (2 * i - (NUM_PATTERNS - 2)) * PATTERN_CUT_Q8 // 2
        pat = np.where(ok & (z > cut), i + 1, pat)
    return pat


def load(path):
    cols = {k: [] for k in ("t", "frame", "cent", "centAvg", "centDev",
                            "gapQ8", "pat", "agc")}
    with open(path) as fh:
        for row in csv.DictReader(fh):
            for k in cols:
                v = row.get(k)
                if v not in (None, ""):
                    cols[k].append(float(v))
    return {k: np.asarray(v) for k, v in cols.items()}


def at_time(src, dst_t, values):
    """Nearest-source-sample for each dst timestamp. The t column is a shared host
    clock, so this is matching room moments, not device ones."""
    idx = np.searchsorted(src["t"], dst_t)
    idx = np.clip(idx, 0, len(src["t"]) - 1)
    lo = np.clip(idx - 1, 0, len(src["t"]) - 1)
    pick = np.where(np.abs(src["t"][idx] - dst_t) <=
                    np.abs(src["t"][lo] - dst_t), idx, lo)
    # Reject matches further away than a poll gap; a silent device's rows repeat a t
    # and a long stall would otherwise pair across it.
    good = np.abs(src["t"][pick] - dst_t) < 1.0
    return values[pick], good


def score(a, b, label):
    po = float(np.mean(a == b))
    pe = sum(np.mean(a == k) * np.mean(b == k) for k in range(NUM_PATTERNS))
    kap = (po - pe) / (1 - pe) if pe < 1 else 0.0
    occ = ([round(float(np.mean(a == k)) * 100, 1) for k in range(NUM_PATTERNS)],
           [round(float(np.mean(b == k)) * 100, 1) for k in range(NUM_PATTERNS)])
    print(f"  {label:<30} agree {po*100:5.1f}%  kappa {kap:+.3f}   "
          f"occ L {occ[0]}  R {occ[1]}")
    return po, kap


def main():
    d = sys.argv[1]
    names = {}
    for side in ("L", "R"):
        for cand in (f"state_{side}.csv", f"state-{side}.csv"):
            p = os.path.join(d, cand)
            if os.path.exists(p):
                names[side] = p
                break
    if len(names) != 2:
        raise SystemExit(f"{d}: need state_L and state_R")
    L, R = load(names["L"]), load(names["R"])
    n = min(len(L["t"]), len(R["t"]))
    print(f"{d}: {len(L['t'])} / {len(R['t'])} rows over "
          f"{min(L['t'][-1], R['t'][-1]):.0f} s")

    # Intersect on wall time.
    lo = max(L["t"][0], R["t"][0])
    hi = min(L["t"][-1], R["t"][-1])
    keep = (L["t"] >= lo) & (L["t"] <= hi)
    t = L["t"][keep]

    rules = {}
    # What the device actually chose - the ground truth and the sanity check.
    rules["recorded pat (device)"] = (
        L["pat"][keep], at_time(R, t, R["pat"])[0])
    gq_l = (L["gapQ8"] // 256).astype(np.int64)
    gq_r = (R["gapQ8"] // 256).astype(np.int64)
    dev_l = L["centDev"].astype(np.int64)
    dev_r = R["centDev"].astype(np.int64)
    raw_l = (L["cent"] - L["centAvg"]).astype(np.int64)
    raw_r = (R["cent"] - R["centAvg"]).astype(np.int64)

    sh_l = pattern_index_gap(gq_l[keep], dev_l[keep])
    sh_r = pattern_index_gap(at_time(R, t, gq_r)[0], at_time(R, t, dev_r)[0])
    rw_l = pattern_index_gap(raw_l[keep], dev_l[keep])
    rw_r = pattern_index_gap(at_time(R, t, raw_r)[0], at_time(R, t, dev_r)[0])

    print("\n  rule                       result")
    # If the shipped rule does not reproduce the recorded column, the offsets or the
    # constants above are wrong and every number below is meaningless.
    same = float(np.mean(sh_l == L["pat"][keep])) * 100
    print(f"  shipped rule reproduces the recorded pat column on L: {same:.1f}%")
    same = float(np.mean(rw_l == L["pat"][keep])) * 100
    print(f"  raw-gap rule reproduces it:                           {same:.1f}%")
    print()
    score(*rules["recorded pat (device)"], "recorded pat (what shipped)")
    score(sh_l, sh_r, "smoothed gap / raw spread")
    score(rw_l, rw_r, "RAW gap / raw spread")

    # How big are the two numerators, relative to the ruler they are divided by?
    for nm, g in (("smoothed", gq_l[keep]), ("raw", raw_l[keep])):
        r = np.abs(g) / np.maximum(dev_l[keep], 1)
        print(f"\n  L |{nm} gap| / centDev  p50 {np.percentile(r,50):.2f}  "
              f"p90 {np.percentile(r,90):.2f}  "
              f"frac under 0.844 cut: {float(np.mean(r < 0.844))*100:.0f}%")
    for nm, g in (("smoothed", gq_r), ("raw", raw_r)):
        r = np.abs(g) / np.maximum(dev_r, 1)
        print(f"  R |{nm} gap| / centDev  p50 {np.percentile(r,50):.2f}  "
              f"p90 {np.percentile(r,90):.2f}  "
              f"frac under 0.844 cut: {float(np.mean(r < 0.844))*100:.0f}%")


if __name__ == "__main__":
    main()
