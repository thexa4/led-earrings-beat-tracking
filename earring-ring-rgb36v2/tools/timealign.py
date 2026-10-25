#!/usr/bin/env python3
"""Score two captures aligned by WALL CLOCK instead of frame index.

The two earrings run on independent HSI16 oscillators: measured 63.10 fps
(left) vs 63.30 fps (right), 0.3% apart. Over a 100 s capture their frame
counters drift 20 frames apart, so any fixed frame-index lag scores part of
the run against a different moment of the music - which shows up as
"disagreement" in every field. fieldwise.py/runpair.py use one global lag.

Both pollers timestamp every telemetry row with the same host clock, so the
honest alignment is by t: for each frame of A, take B's frame nearest in time.

  tools/timealign.py A.csv B.csv [...]

Prints per-field agreement (identity/shape/hot/temporal), the pattern
(the color the user sees), the beat F1 at +-2 frames, and the drift.
"""
import csv
import sys

import numpy as np

IDENT_MASK = 0x3FFFFF00


def load(path):
    rows = [r for r in csv.DictReader(open(path)) if r["kind"] == "T"]
    g = lambda k: [r[k] for r in rows]  # noqa: E731
    return dict(
        t=np.array([float(v) for v in g("t")]),
        hash=np.array([int(v, 16) if v else 0 for v in g("hash")], dtype=np.uint64),
        pat=np.array([int(v) for v in g("pattern")]),
        beat=np.array([v == "True" for v in g("beat")]),
        locked=np.array([v == "True" for v in g("locked")]),
        seq=np.array([int(v) for v in g("seq")]),
    )


def popcount(a):
    c = np.zeros(a.shape, dtype=np.int32)
    for b in range(32):
        c += ((a >> b) & 1).astype(np.int32)
    return c


def field(hash_, shift, width):
    return (hash_ >> shift) & ((1 << width) - 1)


def beat_f1(ta, tb, tol_s):
    """F1 between two beat-event time sequences with a +-tol_s second
    tolerance. Wall-clock based: the two devices' frame counters drift, so
    frame indices are not comparable across runs."""
    ea = np.sort(ta)
    eb = np.sort(tb)
    if len(ea) == 0 or len(eb) == 0:
        return 0.0
    p = np.mean([np.min(np.abs(eb - x)) <= tol_s for x in ea])
    r = np.mean([np.min(np.abs(ea - x)) <= tol_s for x in eb])
    return 0.0 if p + r == 0 else 2 * p * r / (p + r)


def fit(d):
    """Robust linear fit t = a + b*seq over the unwrapped sequence. The t
    stamps are poll-batch times (a whole batch shares one stamp), so a
    single stamp is coarse - the fit over thousands of rows is not. Returns
    (a, b) with b = seconds per frame."""
    s = d["seq"].astype(np.float64)
    t = d["t"]
    keep = np.diff(s, prepend=s[0] - 1) > 0  # drop ring wraps
    s, t = s[keep], t[keep]
    # least squares, then one rejection pass
    for _ in range(2):
        b, a = np.polyfit(s, t, 1)
        r = t - (a + b * s)
        sd = 1.4826 * np.median(np.abs(r - np.median(r))) + 1e-9
        m = np.abs(r - np.median(r)) < 4 * sd
        s, t = s[m], t[m]
    return a, b


def score(A, B):
    # Time-align through each device's own clock fit: A frame seqA happened
    # at t = aA + bA*seqA; the frame of B at that wall time is
    # seqB = (t - aB)/bB.
    aA, bA = fit(A)
    aB, bB = fit(B)
    tA = aA + bA * A["seq"]
    seqB = (tA - aB) / bB  # device-B frame counter at A's wall time
    # Rows are not contiguous (the poller can skip), so look up by seq value.
    bs = B["seq"]
    ins = np.clip(np.searchsorted(bs, seqB), 1, len(bs) - 1)
    lo, hi = bs[ins - 1], bs[ins]
    idx = np.where(np.abs(lo - seqB) <= np.abs(hi - seqB), ins - 1, ins)
    ok = (np.abs(bs[idx] - seqB) < 1.0) & A["locked"] & B["locked"][idx]
    ha, hb = A["hash"][ok], B["hash"][idx[ok]]
    n = int(ok.sum())
    if n < 100:
        print("  (too few jointly locked frames)")
        return
    d = popcount(ha ^ hb)
    ident = popcount((ha ^ hb) & IDENT_MASK)
    shape = popcount(field(ha ^ hb, 8, 14))
    hot = popcount(field(ha ^ hb, 22, 8))
    temp = popcount(field(ha ^ hb, 0, 8))

    def line(name, x):
        print(f"    {name:>9}: exact={100*np.mean(x==0):5.1f}%  "
              f"<=1={100*np.mean(x<=1):5.1f}%  <=3={100*np.mean(x<=3):5.1f}%  "
              f"mean={x.mean():.2f}")

    print(f"  n={n} locked frames, rates L={1/bA:.2f} fps R={1/bB:.2f} fps "
          f"(drift {bA/bB-1:+.2%} = {(A['t'][-1])*(1/bA-1/bB):+.0f} frames over the run)")
    line("identity", ident)
    line("shape", shape)
    line("hot", hot)
    line("temporal", temp)
    pat = float(np.mean(A["pat"][ok] == B["pat"][idx[ok]]))
    print(f"    pattern  : {100*pat:.1f}%  "
          f"(A sees {len(np.unique(A['pat'][ok]))} values, "
          f"B {len(np.unique(B['pat'][idx[ok]]))})")
    tol = 2 * (bA + bB) / 2  # +-2 frames, in seconds
    print("    beat+-2  : "
          f"{100*beat_f1(tA[A['beat'] & A['locked']], aB + bB * B['seq'][B['beat'] & B['locked']], tol):.1f}%")


def main():
    paths = sys.argv[1:]
    if len(paths) < 2:
        print(__doc__)
        sys.exit(2)
    runs = {p: load(p) for p in paths}
    names = list(runs)
    for i in range(len(names)):
        for j in range(i + 1, len(names)):
            print(f"===== {names[i]} ~ {names[j]}")
            score(runs[names[i]], runs[names[j]])


if __name__ == "__main__":
    main()
