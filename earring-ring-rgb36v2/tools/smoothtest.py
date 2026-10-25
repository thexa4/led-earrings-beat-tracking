#!/usr/bin/env python3
"""Does low-passing the deviation raise agreement between the two earrings?

They already agree on which way the spectrum is tilting (r=+0.78) and disagree about
how far that puts them from their own reference. If the disagreement is per-frame
noise rather than the music, a low-pass on the deviation - renormalised by its own
spread, so the cuts keep meaning the same thing - buys agreement without moving the
cuts. If it is the music, smoothing changes nothing and the idea is dead.

The dump samples at 10 Hz while the firmware would smooth at 62.5 Hz, so the window
here is an upper bound on the latency and a lower bound on the noise removed: read
this as go/no-go, not as a predicted score.

Superseded by replay.py, which replays the firmware's own integer recursion against a
--state capture and searches the reference and spread time constants jointly instead of
only convolving a window over the deviation. Kept because the answer it gave - a
plateau, +3 points for a 1.2 s window - is a negative result worth being able to
reproduce.
"""
import csv
import sys

import numpy as np

K = 0.844


def load(path, side):
    """Return the raw deviation (cent - centAvg) per sample; column 3 of the dump."""
    gap = []
    for row in csv.reader(open(path)):
        if not row or row[1] != side:
            continue
        gap.append(float(row[3]))
    return np.array(gap)


def mad(v):
    m = np.median(v)
    return np.median(np.abs(v - m)) or 1e-9


def bucket(z):
    return np.digitize(z, [-K, 0.0, K])


def main():
    path = sys.argv[1]
    gl, gr = load(path, "L"), load(path, "R")
    n = min(len(gl), len(gr))
    gl, gr = gl[:n], gr[:n]
    print(f"{n} samples at 10 Hz ({n/10:.0f} s)")
    print(" window   r    same-sign  agree  kappa   L occupancy %      R occupancy %")
    for w in (1, 2, 3, 5, 8, 12):
        sl = np.convolve(gl, np.ones(w) / w, mode="valid")
        sr = np.convolve(gr, np.ones(w) / w, mode="valid")
        zl, zr = sl / mad(sl), sr / mad(sr)
        m = min(len(zl), len(zr))
        zl, zr = zl[:m], zr[:m]
        bl, br = bucket(zl), bucket(zr)
        agree = float((bl == br).mean())
        chance = sum(
            (bl == i).mean() * (br == i).mean() for i in range(4)
        )
        kappa = (agree - chance) / (1 - chance) if chance < 1 else 0.0
        occ = [
            " ".join(f"{100*(bl==i).mean():4.0f}" for i in range(4)),
            " ".join(f"{100*(br==i).mean():4.0f}" for i in range(4)),
        ]
        r = float(np.corrcoef(zl, zr)[0, 1])
        same = float((np.sign(zl) == np.sign(zr)).mean())
        print(f"  {w:2d} ({w/10:.1f}s) {r:+.3f}  {100*same:5.1f}%  "
              f"{100*agree:5.1f}%  {kappa:+.3f}  {occ[0]}   {occ[1]}")


main()
