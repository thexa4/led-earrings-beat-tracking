#!/usr/bin/env python3
"""Does keeping the reference and the spread in Q8 unstick the selector?

The shipped cascade is

    cent    += (c - cent + 512) >> 10          centroid, low-passed
    gap      = cent - centAvg                  how far from its own reference
    centGapQ8 += ((gap<<8) - centGapQ8 + 512) >> 10     the judged quantity
    centDev  += (|gap| - centDev + 512) >> 10  the ruler
    centAvg  += (gap + 512) >> 10              the reference

and the pattern is PatternIndexGap(centGapQ8 >> 8, centDev). Only one of the four
running states carries fraction bits. That is not an oversight - the comment on
centGapQ8 explains that a plain integer EMA at shift 10 adds nothing at all while the
difference to be caught is under 512, so the state stops wherever it happened to stop
moving. The same comment applies verbatim to centAvg and centDev, and neither was
converted.

Recorded on the 25 minute capture, they show exactly that signature. centDev's median
is 512 - not approximately 512, but 512, the value an EMA deadbands at. And the ratio
the colour is chosen from, gap measured in units of the spread, is not a distribution at
all: device L sits at -78 with a 2-98 percentile range of 16 counts, device R at +139
with a range of 1. Two devices sitting on two different constants, chosen by wherever
their own reference happened to freeze, is the stuck-on-one-colour complaint, and it is
why the outer colours appear 2% of the time.

So this runs the same cascade with the reference and the ruler in Q8 like the judged
quantity already is, and reports both side by side on identical recorded band values.
The current cascade is simulated first and checked against the device, so a fix that
scores well is measured on a model known to describe the machine, not on a model that
merely disagrees with it in a flattering direction.

  python3 tools/q8fix.py DIR
"""
import csv
import os
import sys

import numpy as np

NUM_PATTERNS = 4
CUT_Q8 = 216
MIN_DEV = 8
WARMUP = 500
SHIFT = 10
SCALE = 4
FPS = 62.5
SHIPPED = (-7, -5, -3, -1, 1, 3, 5, 7)
CANDS = {
    "shipped ramp": SHIPPED,
    "b45-b01": (-5, -5, 0, 0, 5, 5, 0, 0),
    "b4-b0": (-7, 0, 0, 0, 7, 0, 0, 0),
    "b456-b01": (-5, -5, 0, 0, 4, 4, 2, 0),
    # b0 against b4 and b5 together: the pair's two most-shared shapes are band 0
    # (above 4 kHz) and band 4/5 (400 Hz - 1 kHz), and within one unit those move
    # oppositely, so differencing them roughly doubles the shared part instead of
    # cancelling it.
    "b0-45": (-7, 0, 0, 0, 4, 3, 0, 0),
}


def index_gap(gap, dev, cut=CUT_Q8):
    if dev < MIN_DEV:
        return NUM_PATTERNS // 2
    lim = 2 * dev
    d = max(-lim, min(lim, gap))
    z = int(d * 256 / dev)
    p = 0
    for i in range(NUM_PATTERNS - 1):
        if z > (2 * i - (NUM_PATTERNS - 2)) * cut // 2:
            p = i + 1
    return p


def run(bands, w, q8=False, cut=CUT_Q8, cshift=SHIFT, rshift=None, dshift=None,
        gshift=None, half=1, ruler="mad"):
    """The cascade. q8=False is the shipped arithmetic; q8=True keeps the reference
    and the ruler in the same Q8 fixed point the judged quantity already uses.

    `half` is the rounding term added before the shift. The firmware adds
    1 << (shift-1), which rounds up; for a state that is meant to sit still that is a
    constant upward bias of half a quantum on every frame it is able to move at all.
    Both are offered because the frozen states make the rounding visible, and it is
    cheaper to measure than to argue about.
    """
    # The reference and the ruler must be at least as slow as the low-pass they are
    # taken from, or they track the signal and the deviation is always near zero.
    rshift = SHIFT if rshift is None else rshift
    dshift = SHIFT if dshift is None else dshift
    gshift = SHIFT if gshift is None else gshift
    w = np.asarray(w, dtype=np.int64)
    cs = (bands.astype(np.int64) @ w) * SCALE
    n = len(cs)
    pat = np.zeros(n, dtype=np.int64)
    zout = np.zeros(n, dtype=np.int64)
    dv = np.zeros(n, dtype=np.int64)
    clamped = np.zeros(n, dtype=bool)
    zraw = np.zeros(n, dtype=np.int64)
    cent = cav = cdev = gq = cavq = cdevq = 0
    upq = dnq = 0
    twosided = ruler == "two"
    if twosided:
        upq = max(0, cs[0]) << 8
        dnq = max(0, -cs[0]) << 8
    rnd = 1 << (cshift - 1)
    for i in range(n):
        if i == 0:
            cent = cav = cavq = cs[i]
            cav = cs[i]
            cdev = 0
            cdevq = 0
            gq = 0
            if q8:
                cavq = cs[i] << 8
                cent = cs[i] << 8
        elif q8:
            # The centroid needs fraction bits too. A plain integer EMA at shift 13
            # cannot move unless the input differs by more than 8192, so at any slow
            # setting it stops dead: measured at 131 s and 262 s every cut produced
            # one pattern on both units, which is the accumulator frozen, not the
            # music being steady. The shipped comment converts only the judged
            # quantity; the same argument covers all four states.
            cent += ((cs[i] << 8) - cent + rnd) >> cshift
            gap = (cent >> 8) - (cavq >> 8)
            gq += ((gap << 8) - gq + rnd) >> gshift
            a = -gap if gap < 0 else gap
            if twosided:
                # One-sided spread per direction. A symmetric ruler plus symmetric
                # cuts assumes the deviation's middle is its average; measured on
                # t1 its median is about 50 counts under the average, so the same
                # lopsided occupancy shows up on both units however the cuts move -
                # 44% in the first colour and 11% in the last at the 66 s
                # low-pass, on data where the pair agrees 76% of the time.
                # Measuring how far each side normally swings puts the cut at equal
                # probability on both sides with one shared constant.
                hi, lo = (gap, 0) if gap >= 0 else (0, -gap)
                upq += ((hi << 8) - upq + rnd) >> dshift
                dnq += ((lo << 8) - dnq + rnd) >> dshift
            else:
                cdevq += ((a << 8) - cdevq + rnd) >> dshift
            # Increment form, matching the shipped `cav += (gap + 512) >> 10`. The
            # reference is an EMA written as an accumulation of the difference, so its
            # Q8 version accumulates the difference in Q8. Writing the more familiar
            # EMA-toward-target form here instead, ((gap<<8) - cavq) >> shift, makes
            # the reference chase the gap rather than the centroid: the reference runs
            # to zero, every gap becomes the whole centroid, and the ruler inflates to
            # fourteen times its size while the ratio pins at one value. That is a
            # silent failure - the pattern column still looks plausible.
            cavq += ((gap << 8) + rnd) >> rshift
        else:
            cent += (cs[i] - cent + rnd) >> cshift
            gap = cent - cav
            gq += ((gap << 8) - gq + rnd) >> gshift
            a = -gap if gap < 0 else gap
            cdev += (a - cdev + rnd) >> dshift
            cav += (gap + rnd) >> rshift
        dv[i] = (cdevq >> 8) if q8 else cdev
        if i < WARMUP:
            pat[i] = NUM_PATTERNS // 2
            zout[i] = 0
        else:
            dev = dv[i]
            if twosided:
                dev = (upq >> 8) if (gq >> 8) >= 0 else (dnq >> 8)
                dv[i] = dev
            g = gq >> 8
            lim = 2 * dev if dev else 0
            clamped[i] = dev > 0 and abs(g) >= lim
            zraw[i] = int(g * 256 / dev) if dev else 0
            zout[i] = int(max(-lim, min(lim, g)) * 256 / dev) if dev else 0
            pat[i] = index_gap(g, dev, cut)
    return pat, zout, dv, clamped, zraw


def load(path):
    t, b = [], []
    with open(path) as fh:
        for r in csv.DictReader(fh):
            t.append(float(r["t"]))
            b.append([float(r[f"b{i}"]) for i in range(8)])
    t = np.asarray(t)
    o = np.argsort(t)
    return t[o], np.asarray(b, dtype=np.float64)[o]


def score(a, b):
    po = float(np.mean(a == b))
    pe = sum(np.mean(a == k) * np.mean(b == k) for k in range(NUM_PATTERNS))
    kap = (po - pe) / (1 - pe) if pe < 1 else 0.0
    mo = min(min(float(np.mean(a == k)), float(np.mean(b == k)))
             for k in range(NUM_PATTERNS))
    hd = []
    for p in (a, b):
        e = np.concatenate(([0], np.flatnonzero(np.diff(p) != 0) + 1, [len(p)]))
        hd.append(np.mean(np.diff(e)) / FPS)
    return po, kap, mo, hd


def main():
    d = sys.argv[1]
    data = {}
    for side in ("L", "R"):
        p = os.path.join(d, f"state_{side}.csv")
        if not os.path.exists(p):
            raise SystemExit(f"{p}: missing")
        data[side] = load(p)
    (tl, bl), (tr, br) = data["L"], data["R"]
    lo, hi = max(tl[0], tr[0]), min(tl[-1], tr[-1])
    tg = np.arange(lo, hi, 1.0 / FPS)
    GL = bl[np.clip(np.searchsorted(tl, tg), 0, len(tl) - 1)]
    GR = br[np.clip(np.searchsorted(tr, tg), 0, len(tr) - 1)]
    n = min(len(GL), len(GR))
    GL, GR = GL[:n], GR[:n]
    warm = int(120 * FPS)
    print(f"{d}: {(n-warm)/FPS/60:.1f} min scored, first 2 min dropped")

    # The shipped arithmetic has to reproduce the device before any comparison means
    # anything.
    for nm, G in (("L", GL), ("R", GR)):
        p0 = run(G, SHIPPED, q8=False)[0]
        rec = np.array([int(float(r["pat"])) for r in
                        csv.DictReader(open(os.path.join(
                            d, f"state_{nm}.csv")))])
        rr = rec[np.clip(np.searchsorted(
            tl if nm == "L" else tr, tg), 0, len(rec) - 1)][:n]
        print(f"  {nm}: shipped arithmetic reproduces the device on "
              f"{np.sum(p0 == rr) / n * 100:.1f}% of all {n} frames")

    # Where the quartiles of z actually sit is the only honest way to choose the cut
    # width, because the cut is what assigns occupancy: the firmware puts the outer
    # boundaries at +-cut and the middle one at 0, so a quarter of the frames above
    # +cut means cut is the 75th percentile. Reading it off the distribution also
    # shows whether one number can serve both units, which it cannot if their quartiles
    # differ - and then no fixed constant evens the colours and the normalisation,
    # not the cut, is what needs changing.
    print("\n  z quartiles per unit, Q8 units of the spread:")
    for wname in ("b4-b0", "b45-b01", "b0-45"):
        for q8 in (False, True):
            out = []
            for G in (GL, GR):
                z = run(G, CANDS[wname], q8=q8)[1]
                out.append(np.percentile(z[warm:], [25, 50, 75]))
            print(f"    {wname:<10} {'Q8' if q8 else 'shipped':<8} "
                  f"L {out[0][0]:+6.0f} {out[0][1]:+6.0f} {out[0][2]:+6.0f}   "
                  f"R {out[1][0]:+6.0f} {out[1][1]:+6.0f} {out[1][2]:+6.0f}")

    print(f"\n  {'weights':<14} {'arith':<8} {'lp s':>5} {'cut':>4} {'agree':>7} "
          f"{'kappa':>7} {'minocc':>7} {'clamp%':>8}  occupancy L then R")
    print("  ruler mad = mean deviation of the gap;  two = its one-sided spread "
          "per direction, which re-centres the cuts on the median")
    q8done = False
    for wname, wv in CANDS.items():
        if wname not in ("shipped ramp", "b4-b0", "b45-b01", "b0-45"):
            continue
        for q8 in (False, True) if wname == "shipped ramp" else (True,):
            cuts = ((60, 110, 216) if wname == "shipped ramp"
                    else (110, 150, 190, 216, 245, 280))
            # The single-scalar rule puts four ordered labels on one number, which
            # forces the cut width to trade agreement against occupancy: measured on
            # t1, b4-b0 gives 62% agreement at 6% occupancy or 45% at 17%. The other
            # knob nobody has swept is the time constant. bandfit's +0.66 kappa came
            # from a 150 s low-pass; the firmware low-passes at 16 s. At 16 s the
            # pair's agreement on any single frame is mostly noise, and smoothing
            # after that cannot recover what was never there.
            lps = ((10,) if wname == "shipped ramp" else
                   (10, 11, 12, 13))
            for lp, cut, ruler in [(a, b, r) for a in lps for b in cuts
                                   for r in ("mad", "two")]:
                cs = rs = ds = gs = lp
                # A slow reference has to settle before its deviation means
                # anything, so the scored window grows with the time constant
                # rather than staying at a fixed 2 minutes and quietly scoring an
                # unsettled reference.
                w2 = max(warm, int(2.5 * (1 << lp)))
                pl, zl, dl, kl = run(GL, wv, q8=q8, cut=cut, cshift=cs,
                                     rshift=rs, dshift=ds, gshift=gs,
                                     ruler=ruler)[:4]
                pr, zr, dr, kr = run(GR, wv, q8=q8, cut=cut, cshift=cs,
                                     rshift=rs, dshift=ds, gshift=gs,
                                     ruler=ruler)[:4]
                if not q8 and wname == "shipped ramp" and cut == 216 and not q8done:
                    # Cross-check against selsim, which reimplements the same cascade
                    # independently. Two tools agreeing is not proof, but two tools
                    # disagreeing about the same arithmetic always means one of them
                    # is wrong, and finding that out here is much cheaper than after
                    # compiling it.
                    sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
                    import selsim
                    for nm, G, pmine in (("L", GL, pl), ("R", GR, pr)):
                        _, _, sd, sg, = selsim.cascade(
                            G, SHIPPED, SHIFT, SHIFT, SHIFT, SHIFT)
                        sp = selsim.bucket(sg, sd)
                        agree = float(np.mean(sp == pmine)) * 100
                        print(f"  cross-check {nm}: q8fix vs selsim pattern columns "
                              f"agree on {agree:.2f}% of frames"
                              + ("" if agree > 99.9 else "   <- DISAGREE, one is wrong"))
                    q8done = True
                po, kap, mo, hd = score(pl[w2:], pr[w2:])
                spl = np.percentile(zl[w2:], [2, 98])
                spr = np.percentile(zr[warm:], [2, 98])
                flag = "" if mo >= 0.18 else "  <- colour under-used"
                occ = [int(np.mean(pl[w2:] == k) * 100) for k in range(4)]
                occ += [int(np.mean(pr[w2:] == k) * 100) for k in range(4)]
                ch = [np.sum(np.diff(p[w2:]) != 0) / ((n - w2) / FPS / 60)
                      for p in (pl, pr)]
                print(f"  {wname:<14} {'Q8' if q8 else 'shipped':<8} "
                      f"{2**lp/62.5:5.0f} {ruler:<4} {cut:4d} {po*100:6.1f}% "
                      f"{kap:+7.3f} {mo*100:6.1f}% {kl[w2:].mean()*100:4.0f}/"
                      f"{kr[w2:].mean()*100:<3.0f}" +
                      "".join(f"{v:3d}/" for v in occ) + f"{flag}")
    print("\n  z spread is the 2-98 percentile range of the number the colour is chosen")
    print("  from. A narrow range means the device is not choosing between colours, it")
    print("  is sitting at one constant and occasionally crossing a boundary.")


if __name__ == "__main__":
    main()
