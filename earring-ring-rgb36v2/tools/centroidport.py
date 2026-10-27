#!/usr/bin/env python3
"""Should master's colour be the argmax of a fitted-line deviation, or a bucketed
band difference? Decided on real captures, replayed through master's own commit
path.

Why this file exists: the two branches do not use the same selector, and until
today nobody had measured the difference on data that actually contains sound
common to both earrings.

  master  spectralDevs() fits a line through the eight log-band energies and the
          colour is the band furthest above it, with hysteresis on the incumbent.
  debug   c = 7*(s4 - s7), low-passed, minus its own slow reference, divided by
          its own mean absolute deviation, then bucketed by three fixed cuts.

The debug form is a monotone scalar, so two devices that disagree differ by one
bucket; argmax is a discrete winner, so a hair of measurement noise changes the
answer completely. That is the argument on paper. On the bench, with the debug
firmware showing its own committed colour over an 11.6 minute capture of loud
EDM, the pair agreed 61.8% of the time, kappa +0.46. Replaying master's selector
and master's commit path over the same frames gives kappa +0.15.

Both are run here through the SAME commit path, from the SAME captured band
stream, with the SAME beat and anchor flags, so the only variable is the colour
function. The harness is mastership.replay; if the ported selector reproduces
the ~+0.46 the device itself showed, the harness is believed and the comparison
is worth something.

Go integer division truncates toward zero and Python's floors, so every division
here goes through godiv(). Getting that wrong silently biases every bucket edge
on negative gaps, which is half the input range.

  python3 centroidport.py ~/psync/goodA [more captures...]
"""
import os
import sys

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import blocksel as bs  # noqa: E402
import mastership as ms  # noqa: E402

NB = 8
NP_MASTER = 7
# syncdsp/consts.go on the debug branch.
W = (-7, 0, 0, 0, 7, 0, 0, 0)
CSCALE = 4
CSHIFT = 11
GSHIFT = 11
SSHIFT = 11
CUTQ8 = 150
MINDEV = 8
PATWARM = 500
MAXHOLD = 250


def godiv(a, b):
    """Go's integer division: truncation toward zero, not floor."""
    q = abs(a) // abs(b)
    return q if (a >= 0) == (b >= 0) else -q


def index_gap(gap, cent_dev, npat):
    """PatternIndexGap, one-to-one."""
    if cent_dev < MINDEV:
        return npat // 2
    lim = cent_dev * 2
    d = lim if gap > lim else (-lim if gap < -lim else gap)
    z = godiv(d * 256, cent_dev)
    pat = 0
    for i in range(npat - 1):
        cut = godiv((2 * i - (npat - 2)) * CUTQ8, 2)
        if z > cut:
            pat = i + 1
    return pat


def centroid_pat(G, npat, warm=PATWARM, cshift=CSHIFT):
    """The debug branch's running statistics, per frame."""
    n = len(G)
    out = np.zeros(n, dtype=np.int64)
    centq8 = centavgq8 = centgapq8 = 0
    centdevq8 = 0
    init = False
    for i in range(n):
        s = G[i]
        c = 0
        for b in range(NB):
            c += s[b] * W[b]
        c *= CSCALE
        if not init:
            init = True
            centq8 = centavgq8 = c << 8
            centdevq8 = centgapq8 = 0
        else:
            centq8 += ((c << 8) - centq8 + (1 << (cshift - 1))) >> cshift
            gap = (centq8 >> 8) - (centavgq8 >> 8)
            centgapq8 += ((gap << 8) - centgapq8 + (1 << (GSHIFT - 1))) >> GSHIFT
            a = abs(gap)
            centdevq8 += ((a << 8) - centdevq8 + (1 << (SSHIFT - 1))) >> SSHIFT
            centavgq8 += ((gap << 8) + (1 << (SSHIFT - 1))) >> SSHIFT
        out[i] = index_gap(centgapq8 >> 8, centdevq8 >> 8, npat) if i >= warm \
            else npat // 2
    return out


def run(E, patseq, hold=True, refractory=ms.REFRACTORY_BEATS):
    """Commit patseq through master's timing, optionally with debug's backstop."""
    n = len(patseq)
    out = np.zeros(n, dtype=np.int64)
    pushed, returned, bar = E["pushed"], E["returned"], E["bar"]
    anchor, silent, age = E["anchor"], E["silent"], E["onset_age"]
    disp, since, frames = 0, 0, 0
    for i in range(n):
        if i < ms.WARMUP or silent[i]:
            out[i] = disp
            continue
        want = int(patseq[i])
        if pushed[i]:
            if (returned[i] or bar[i]) and since >= refractory and disp != want:
                disp = want
                since = 0
            since += 1
        if anchor[i] and age[i] <= ms.ANCHOR_ONSET_MAX and disp != want:
            disp = want
            since = 0
        if hold and i >= PATWARM and frames >= MAXHOLD and disp != want:
            disp = want
            since = 0
        frames += 1
        out[i] = disp
    return out


def run_argmax(E, D, margin, hold=False, refractory=ms.REFRACTORY_BEATS):
    """master's own selector through the same timing, for the baseline row."""
    am = bs.argmax_all(D)
    n = len(D)
    out = np.zeros(n, dtype=np.int64)
    pushed, returned, bar = E["pushed"], E["returned"], E["bar"]
    anchor, silent, age = E["anchor"], E["silent"], E["onset_age"]
    disp, band, valid = 0, 0, False
    since, frames = 0, 0
    for i in range(n):
        if i < ms.WARMUP or silent[i]:
            out[i] = disp
            continue
        want = int(am[i])
        if valid and want != band and D[i][want] < D[i][band] + margin:
            want = band
        band, valid = want, True
        if pushed[i]:
            if (returned[i] or bar[i]) and since >= refractory and disp != want:
                disp = want
                since = 0
            since += 1
        if anchor[i] and age[i] <= ms.ANCHOR_ONSET_MAX and disp != want:
            disp = want
            since = 0
        if hold and i >= PATWARM and frames >= MAXHOLD and disp != want:
            disp = want
            since = 0
        frames += 1
        out[i] = disp
    return out


def main():
    caps = []
    for d in sys.argv[1:]:
        EL, ER = ms.events(os.path.join(d, "L.csv")), ms.events(os.path.join(d, "R.csv"))
        btL, bL = ms.band_grid(os.path.join(d, "state_L.csv"))
        btR, bR = ms.band_grid(os.path.join(d, "state_R.csv"))
        lo = max(EL["t"][0], ER["t"][0], btL[0], btR[0])
        hi = min(EL["t"][-1], ER["t"][-1], btL[-1], btR[-1])
        grid = np.arange(lo, hi, 1.0 / ms.FPS)
        evL, GL = ms.onto(EL["t"], EL, btL, bL, grid, 1.0 / ms.FPS)
        evR, GR = ms.onto(ER["t"], ER, btR, bR, grid, 1.0 / ms.FPS)
        caps.append((d, GL, evL, GR, evR, 1.0 / ms.FPS))
        print(f"loaded {d}: {(hi-lo)/60:.1f} min")
    warm = 60 * ms.FPS

    hdr = "  ".join(f"{'agree  kappa minocc ch/min  hold used':>38}" for _ in caps)
    print(f"\n=== same commit path, different colour function ===")
    print(f"  {'selector':<30} {hdr}")

    def show(name, fn):
        cells = []
        for d, GL, EL, GR, ER, dt in caps:
            a, b = fn(GL, EL), fn(GR, ER)
            po, kap, mo, ch, hold, used = ms.score(a, b, int(warm), dt)
            cells.append(f"{po*100:5.1f}% {kap:+6.3f} {mo*100:5.1f}% "
                         f"{ch:6.1f} {hold:5.1f} {used:3d}")
        print(f"  {name:<30} " + "  ".join(cells), flush=True)

    show("master argmax+margin 32",
         lambda G, E: run_argmax(E, bs.devs_all(np.asarray(G, np.int64)), 32 << 8))
    show("master argmax+margin 32 +hold",
         lambda G, E: run_argmax(E, bs.devs_all(np.asarray(G, np.int64)), 32 << 8,
                                 hold=True))
    show("master argmax, no margin +hold",
         lambda G, E: run_argmax(E, bs.devs_all(np.asarray(G, np.int64)), 0,
                                 hold=True))
    cache = {}
    for npat in (4, 7):
        for hold in (False, True):
            def fn(G, E, npat=npat, hold=hold):
                key = (id(G), npat)
                if key not in cache:
                    cache[key] = centroid_pat(np.asarray(G, np.int64), npat)
                return run(E, cache[key], hold=hold)
            show(f"centroid bucket NP={npat}"
                 f"{' +hold' if hold else ''}", fn)
    for cs in (9, 10, 12):
        def fn(G, E, cs=cs):
            key = (id(G), 'c', cs)
            if key not in cache:
                cache[key] = centroid_pat(np.asarray(G, np.int64), 7, cshift=cs)
            return run(E, cache[key], hold=True)
        show(f"centroid bucket NP=7 1/{2**cs} +hold", fn)

    # Occupancy, because "at least four patterns, evenly spread" is an
    # acceptance criterion that kappa cannot see: a selector that sits on one
    # colour 90% of the time can still score a positive kappa on the rare frames
    # where it happens to agree.
    print(f"\n  committed colour occupancy (L | R), as a percentage of time:")
    for name, fn in (("master argmax+margin 32",
                      lambda G, E: run_argmax(E, bs.devs_all(np.asarray(G, np.int64)), 32 << 8)),
                     ("centroid bucket NP=7",
                      lambda G, E: run(E, centroid_pat(np.asarray(G, np.int64), 7), hold=True))):
        for d, GL, EL, GR, ER, dt in caps:
            a, b = fn(GL, EL)[int(warm):], fn(GR, ER)[int(warm):]
            n = int(max(a.max(), b.max())) + 1
            oa = [float(np.mean(a == k)) * 100 for k in range(n)]
            ob = [float(np.mean(b == k)) * 100 for k in range(n)]
            print(f"    {name:<22} {os.path.basename(d):<8} L " +
                  " ".join(f"{v:4.1f}" for v in oa))
            print(f"    {'':<22} {'':<8} R " + " ".join(f"{v:4.1f}" for v in ob))

    print(f"\n  what the debug device itself showed, same captures:")
    for d, GL, EL, GR, ER, dt in caps:
        a, b = EL["pattern"], ER["pattern"]
        ok = (a >= 0) & (b >= 0)
        a, b = a[ok][int(warm):], b[ok][int(warm):]
        po = float(np.mean(a == b))
        pe = sum(float(np.mean(a == k)) * float(np.mean(b == k))
                 for k in range(int(max(a.max(), b.max())) + 1))
        ch = [float(np.count_nonzero(np.diff(p) != 0)) / (len(p) * dt) * 60
              for p in (a, b)]
        print(f"    {os.path.basename(d):<12} {po*100:5.1f}% "
              f"kappa {(po-pe)/(1-pe) if pe < 1 else 0:+.3f} "
              f"({int(max(a.max(), b.max()))+1} colours, {min(ch):.1f}/min)")


if __name__ == "__main__":
    main()
