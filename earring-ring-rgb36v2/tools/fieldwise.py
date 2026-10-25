#!/usr/bin/env python3
"""Field-wise hash agreement between two device captures.

runpair.py aligns by exact hash, which fails when the hash itself is the thing
in question. This aligns by cross-correlating the `total` loudness (the two
runs hear the same music, AGC-normalised, so total is a reliable landmark),
then scores agreement separately per hash field:

  identity = shape + hot bits (IdentityMask, what anchors compare)
  temporal = the 8 "rose since last push" bits (beat-phase dependent!)

plus the beat-grid phase offset between the runs. If identity agrees and
temporal does not, the temporal field is the agreement leak and the design
question is whether it earns its place.
"""
import csv
import sys

import numpy as np

IDENT_BITS = 21  # bits 8..28: shape(14) + hot(8)... read from Go: IdentityMask


def load(path):
    rows = [r for r in csv.DictReader(open(path)) if r["kind"] == "T"]
    n = len(rows)
    d = {}
    d["total"] = np.array([float(r["total"]) for r in rows])
    d["hash"] = np.array([int(r["hash"], 16) if r["hash"] else 0 for r in rows], dtype=np.uint32)
    d["beat"] = np.array([r["beat"] == "True" for r in rows])
    d["lock"] = np.array([r["locked"] == "True" for r in rows])
    d["pat"] = np.array([int(r["pattern"]) for r in rows])
    return d


def main():
    a, b = load(sys.argv[1]), load(sys.argv[2])
    # align b to a by total cross-correlation
    x = a["total"] - a["total"].mean()
    y = b["total"] - b["total"].mean()
    cc = np.correlate(x, y, "full")[len(y) - 1:]
    lag = int(np.argmax(cc))  # cc[k]=sum x[n+k]*y[n]: a[i+lag] matches b[i]
    def cut(d, s0, s1):
        return {k: v[s0:s1] for k, v in d.items()}
    if lag > 0:
        ya, yb = cut(a, lag, None), cut(b, 0, len(b["total"]) - lag)
    else:
        ya, yb = cut(a, 0, a["total"].size + lag), cut(b, -lag, None)
    m = min(ya["total"].size, yb["total"].size)
    ya = {k: v[:m] for k, v in ya.items()}
    yb = {k: v[:m] for k, v in yb.items()}
    v = ya["lock"] & yb["lock"]
    ha, hb = ya["hash"][v], yb["hash"][v]
    ident = np.uint32(0x3FFFFF00)
    temp = np.uint32(0x000000FF)
    hot = np.uint32(0x3FC00000)
    shape = np.uint32(0x003FFF00)
    for name, mask in (("identity", ident), ("  shape", shape), ("  hot  ", hot),
                       ("temporal", temp)):
        d = np.zeros(len(ha), dtype=int)
        z = (ha ^ hb) & mask
        # popcount
        while np.any(z):
            d += (z & 1).astype(int)
            z >>= 1
        print(f"{name:>9}: exact={100*(d==0).mean():5.1f}%  <=1={100*(d<=1).mean():5.1f}%  "
              f"<=3={100*(d<=3).mean():5.1f}%  mean={d.mean():.2f}  lag={lag}")
    # per-bit flip rate, grouped by field
    flip = np.zeros(30)
    for bit in range(30):
        flip[bit] = ((ha ^ hb) >> bit & 1).mean()
    def grp(lo, hi):
        return " ".join(f"{100*flip[b]:4.0f}" for b in range(lo, hi))
    print(f"  flip% temporal b0-7: {grp(0,8)}")
    print(f"  flip% shape    b8-21:{grp(8,22)}")
    print(f"  flip% hot     b22-29:{grp(22,30)}")
    # pattern + beat phase
    pa, pb = ya["pat"][v], yb["pat"][v]
    print(f"  pattern: {100*(pa==pb).mean():.1f}%")
    ba = np.nonzero(ya["beat"])[0]
    bb = np.nonzero(yb["beat"])[0]
    if len(ba) and len(bb):
        f = sum(1 for k in ba if np.min(np.abs(bb - k)) <= 2) / len(ba)
        print(f"  beat+-2: {100*f:.1f}%")
        # dominant phase offset mod period
        per = np.median(np.diff(ba))
        off = np.array([np.min(np.abs(bb - k)) for k in ba])
        print(f"  beat period ~{per:.1f}, |offset| median {np.median(off):.1f} frames")


if __name__ == "__main__":
    main()
