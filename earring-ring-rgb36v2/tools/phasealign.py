#!/usr/bin/env python3
"""Do the two beat trackers share a PHASE, not just a tempo?

Both units independently report the same tempo - median period 24 frames, 156 BPM, on
every capture tried - while nothing spectral correlates between them. A shared tempo is
the only confirmed shared quantity, so it is the only candidate input left for a
selector. But tempo alone is not enough: two metronomes can run at the same rate from
different starting points, and a pattern that advances on a beat would then advance at
the right speed and the wrong moment forever. Whether that is the situation is a
measurement, not a guess.

The test is circular statistics, not correlation. Wrapping each device's beat times
modulo a beat period gives a phase in [0,1). The period has to come from somewhere, and
using each device's own estimate over a long window is a trap: the estimate is an
integer number of frames, so a 24-frame period carries +-2% quantisation error, which
over fifty beats smears the phase across the whole cycle even for a tracker that is
perfectly locked. Two defences follow. The period is the pooled median over the whole
run, which is a measured property of the music rather than a parameter tuned per device,
and the windows are short - 20 s, about fifty beats - so accumulated error stays small.

The number to read is the paired resultant length R, and it has to be read against the
shuffled control printed next to it, because R depends on how many beats are in the
window. Two trackers locked to the same musical grid with a constant offset give an R
near 1 and a phase difference that is the same number in every window. Two trackers at
the same rate but free-running give an R at the control's level, and a phase difference
that is a different number every window - which is not solvable by any per-device rule,
because the offset is different every time you look.

  python3 tools/phasealign.py DIR
"""
import csv
import os
import sys

import numpy as np

WINDOW = 20.0


def read_rows(path):
    with open(path) as fh:
        rows = [p for p in csv.reader(fh) if p]
    if len(rows) < 3:
        return []
    header = rows[0]
    if len(header) != len(rows[2]):
        raise SystemExit(f"{path}: header {len(header)} names vs "
                         f"{len(rows[2])} data fields")
    return [dict(zip(header, p)) for p in rows[1:] if p and p[0] == "T"]


def load(path):
    t, ev, per = [], [], []
    for r in read_rows(path):
        try:
            tv = float(r["t"])
        except ValueError:
            continue
        p = int(float(r["period"] or 0))
        for k in ("onset", "beat", "bar"):
            if r[k] == "True":
                t.append(tv)
                ev.append(k)
                per.append(p)
    t = np.asarray(t)
    o = np.argsort(t)
    return t[o], [ev[i] for i in o], np.asarray(per)[o]


def resultant(times, period, lo, hi):
    """Circular concentration of `times` in [lo,hi) wrapped modulo `period`."""
    m = (times >= lo) & (times < hi)
    x = times[m]
    if len(x) < 6 or period <= 0:
        return np.nan, 0, np.nan
    ang = 2 * np.pi * (x / period)
    return float(np.abs(np.mean(np.exp(1j * ang)))), len(x), \
        float((np.angle(np.mean(np.exp(1j * ang))) / (2 * np.pi)) % 1.0)


def main():
    d = sys.argv[1]
    data = {}
    for side in ("L", "R"):
        for c in (f"{side}.csv", f"spec_{side}.csv"):
            p = os.path.join(d, c)
            if os.path.exists(p):
                data[side] = load(p)
                break
        else:
            raise SystemExit(f"{d}: no telemetry for {side}")
    (tl, el, pl), (tr, er, pr) = data["L"], data["R"]
    # numpy arrays, so that `el == "beat"` is a mask. On a Python list it is a single
    # comparison returning False, and tl[False] selects an empty axis, which reported
    # nan for every within-device concentration.
    el, er = np.asarray(el), np.asarray(er)
    lo, hi = max(tl[0], tr[0]) + 30.0, min(tl[-1], tr[-1])
    print(f"{d}: {hi-lo:.0f} s analysed (first 30 s dropped)")
    for nm, e, t in (("L", el, tl), ("R", er, tr)):
        print(f"  {nm}: {sum(1 for x in e if x=='onset')} onsets, "
              f"{sum(1 for x in e if x=='beat')} beats, "
              f"{sum(1 for x in e if x=='bar')} bars")

    rng = np.random.default_rng(7)
    # One pooled period for the whole run. Per-window, per-device periods carry the
    # integer-frame quantisation of a 24-frame estimate, which alone smears phase over
    # the cycle within fifty beats and would report a locked pair as free-running.
    ok_l = pl[(pl > 8) & (pl < 120)]
    ok_r = pr[(pr > 8) & (pr < 120)]
    if len(ok_l) == 0 or len(ok_r) == 0:
        raise SystemExit("no usable beat period recorded on one or both sides")
    pooled = int(np.median(np.concatenate([ok_l, ok_r])))
    P = pooled / 62.5
    print(f"\n  pooled beat period {pooled} frames = {P*1000:.0f} ms "
          f"({60.0/P:.1f} BPM); L median {int(np.median(ok_l))}, "
          f"R median {int(np.median(ok_r))}")
    print(f"\n  {'win':>8}  {'R_L':>5} {'R_R':>5} {'phase d':>8} {'R_pair':>7} "
          f"{'shuffle':>8} {'rand':>6}  pairs")
    tot, sh, npair, dps = [], [], [], []
    for a in np.arange(lo, hi - WINDOW, WINDOW):
        b = a + WINDOW
        m1 = (tl >= a) & (tl < b) & (el == "beat")
        m2 = (tr >= a) & (tr < b) & (er == "beat")
        if m1.sum() < 6 or m2.sum() < 6:
            continue
        sl, _, _ = resultant(tl[el == "beat"], P, a, b)
        sr, _, _ = resultant(tr[er == "beat"], P, a, b)
        ph_l = (tl[m1] / P) % 1.0
        ph_r = (tr[m2] / P) % 1.0
        # Every pairing, which is what a listener cannot distinguish from a single
        # offset if an offset exists.
        dd = (ph_l[:, None] - ph_r[None, :]) % 1.0
        ang = 2 * np.pi * dd.ravel()
        rp = float(np.abs(np.mean(np.exp(1j * ang))))
        # NOT a permutation. The quantity being measured is the multiset of all
        # pairwise differences, which is invariant under reordering either set, so a
        # shuffled-order control equals the measurement by construction and can never
        # disagree with it. The control has to break the pairing, which independent
        # uniform phases do.
        shf = float(np.mean([
            np.abs(np.mean(np.exp(1j * 2 * np.pi *
                                  ((rng.uniform(0, 1, ph_r.shape)[None, :]
                                    - ph_l[:, None]) % 1.0).ravel())))
            for _ in range(8)]))
        exp_rand = 1.0 / np.sqrt(len(ang))
        dp = (np.angle(np.mean(np.exp(1j * ang))) / (2 * np.pi)) % 1.0
        tot.append(rp)
        sh.append(shf)
        npair.append(exp_rand)
        dps.append(dp)
        print(f"  {a:6.0f}-{b:.0f}  {sl:5.2f} {sr:5.2f} "
              f"{dp:8.3f} {rp:7.3f} {shf:8.3f} {exp_rand:6.3f}  {m1.sum()*m2.sum()}")
    if tot:
        print(f"\n  mean paired resultant {np.mean(tot):.3f}   shuffled {np.mean(sh):.3f}"
              f"   random floor {np.mean(npair):.3f}")
        print(f"  phase difference across windows: "
              f"{', '.join(f'{v:.2f}' for v in dps)}")
        print("  A paired resultant near the random floor means the two beat trackers")
        print("  run at the same rate from unrelated points, so no per-device rule can")
        print("  key off a beat; a near-1 resultant with the same phase difference in")
        print("  every window is a constant offset, and a selector can be made")
        print("  invariant to that.")


if __name__ == "__main__":
    main()
