#!/usr/bin/env python3
"""Do the emitted beats sit on the loud part of what the device hears?

Alignment-free: no cross-correlation with the playback file, no clock mapping,
no assumption about where in the track the capture started. For every beat the
device emitted, compare the pipeline's own loudness (`total`, log2 Q8) at the
beat against the loudness half a period later. A tracker whose grid is locked to
the kick pulses on the loud frames and scores positive; one that free-runs
anywhere scores about zero, no matter how consistent it is with itself.

Reported per capture:
  depth   mean(loud at beat) - mean(loud half a beat later), in log2 Q8 counts.
          256 = a factor of two.
  on-best the fraction of beats within +-2 frames of the loudest frame of their
          cycle; |err| its median, and "chance" the expected |err| for a grid at
          a random phase, which is P/4.
  period  the locked period, and the autocorrelation-derived period of the same
          envelope over 6 s windows, which is the tempo the grid should have.

Usage: beatphase.py CAPTURE.csv [CAPTURE ...]
"""
import sys

import numpy as np

LO, HI, WIN = 18, 49, 384


def read_csv(path):
    lines = [l.strip().split(",") for l in open(path) if l.strip()]
    head = lines[0]
    rows = [r for r in lines[1:] if len(r) == len(head)]
    out = {}
    for i, k in enumerate(head):
        col = [r[i] for r in rows]
        if col and col[0] in ("True", "False", "true", "false"):
            out[k] = np.array([v in ("True", "true") for v in col])
        else:
            v = []
            for x in col:
                try:
                    v.append(float(x))
                except ValueError:
                    v.append(0.0)
            out[k] = np.array(v)
    return out


def ac_period(e):
    """Mode of the winning lag over 6 s windows; the tempo in the envelope."""
    picks = []
    for s in range(0, len(e) - WIN, 128):
        v = e[s:s + WIN]
        v = v - v.mean()
        if (v * v).mean() <= 0:
            continue
        sc = np.array([float((v[:WIN - l] * v[l:WIN]).mean()) for l in range(LO, HI + 1)])
        sm = np.convolve(np.r_[sc[0], sc, sc[-1]], [0.25, 0.5, 0.25], "same")[1:-1]
        picks.append(LO + int(np.argmax(sm)))
    if not picks:
        return 0, 0
    picks = np.array(picks)
    return int(np.bincount(picks).argmax()), len(picks)


def main():
    print(f"{'capture':<22} {'lock':>5} {'period':>6} {'ac':>4} {'agree':>6} "
          f"{'beats':>6} {'depth':>7} {'on-best':>8} {'|err|':>6} {'chance':>7}")
    for path in sys.argv[1:]:
        d = read_csv(path)
        tot = d["total"]
        col = "bass" if "bass" in d else "onset"
        beat = d["beat"]
        lk = d["locked"]
        per = float(np.median(d["period"][lk])) if lk.any() else 0
        ap, n = ac_period(tot)
        bi = np.nonzero(beat)[0]
        if len(bi) < 10 or per <= 0:
            print(f"{path.split('/')[-1]:<22} {lk.mean()*100:4.0f}% {per:6.0f} {ap:4d} "
                  f"{'-':>6} {len(bi):6d}  (too few beats)")
            continue
        half = int(round(per / 2))
        at, err = [], []
        # Phase error per cycle: distance from the emitted beat to the loudest
        # frame of that cycle. A grid sitting on the kick scores near zero; one
        # that free-runs is uniformly distributed over +-P/2, which is what the
        # "chance" column shows.
        for f in bi:
            lo, hi = f - half, f + half
            if lo < 0 or hi >= len(tot):
                continue
            w = tot[lo:hi + 1]
            at.append(tot[f] - w.mean())
            d = int(np.argmax(w)) - half
            err.append(d)
        depth = float(np.mean(at)) if at else 0.0
        err = np.array(err)
        onb = float(np.mean(np.abs(err) <= 2)) if len(err) else 0.0
        med = float(np.median(np.abs(err))) if len(err) else 0.0
        print(f"{path.split('/')[-1]:<22} {lk.mean()*100:4.0f}% {per:6.0f} {ap:4d} "
              f"{abs(per-ap)<=2 if ap else False:>6} {len(bi):6d} {depth:7.1f} "
              f"{onb*100:7.1f}% {med:5.1f} {50/per:5.1f}%")


main()
