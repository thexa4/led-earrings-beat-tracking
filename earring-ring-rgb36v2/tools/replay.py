#!/usr/bin/env python3
"""Replay candidate pattern selectors over a paired --state capture.

paircapture.py --state records, for each earring and every poll, the three numbers
the colour is chosen from: the spectral centroid, its own running reference, and its
mean absolute deviation. Those are the Pipeline's live state, so a capture holds the
INPUT to every candidate selector, not just the output of the one that was flashed.
That is the point: each of the last three ideas cost a flash and a bench run to
discover it was wrong, and all three are functions of the centroid and its own
statistics, which can be re-run here against the same minute of real music in two
microphones.

Only candidates of that shape can be replayed. Anything that changes `cent` itself -
a different band weighting - starts from the recorded smoothed bands instead, which
are in the same file, so a new ramp is replayable too; what cannot be replayed is
anything upstream of the band energies.

Two things have to be true before any candidate number means anything, and both are
printed:

  * the simulation has to reproduce the firmware's own centAvg and centDev from the
    recorded centroid. It is seeded from the first sample rather than from the
    device's warm state, so the check is convergence and residual size, not equality;
  * the poll rate (~5 Hz) has to be fast enough for the time constants under test.
    Every EMA here is low-pass with a time constant of seconds, so the sampled
    trajectory is a fair stand-in for the 62.5 Hz one - but that is an argument, so
    the check prints the shipped configuration's agreement next to the committed one
    measured in the same capture. If those differ, replay is not the bench.

Usage: python3 tools/replay.py DIR [--holds 1,2,4]
"""
import argparse
import csv
import math
import os
import sys

FRAME_RATE = 62.5  # analysis frames per second (Window=250 at 16 kSPS)
MIN_DEV = 8        # PatternMinDev: below this the device is not reading anything
CUTS_SHIPPED = (-0.844, 0.0, 0.844)  # PatternCutQ8 = 216


def corr(a, b):
    """Pearson r, or nan for a constant input rather than a division by zero."""
    if len(a) != len(b) or len(a) < 2:
        return float("nan")
    ma, mb = sum(a) / len(a), sum(b) / len(b)
    cov = sum((x - ma) * (y - mb) for x, y in zip(a, b))
    va = math.sqrt(sum((x - ma) ** 2 for x in a))
    vb = math.sqrt(sum((y - mb) ** 2 for y in b))
    return cov / (va * vb) if va > 0 and vb > 0 else float("nan")


def load(dirname, side):
    rows = []
    with open(os.path.join(dirname, f"state_{side}.csv")) as fh:
        for r in csv.DictReader(fh):
            rows.append((float(r["t"]), int(r["cent"]), int(r["centAvg"]),
                         int(r["centDev"]), int(r["pat"]), int(r["frame"]),
                         [int(r[f"b{i}"]) for i in range(8)], int(r["agc"])))
    rows.sort(key=lambda r: r[0])
    return rows


def track(cent, frames, ref_shift=10, dev_shift=10, seed=None, smooth=0,
          dev_raw=False, dev_frac=False):
    """Reproduce the firmware's reference and spread from the centroid alone.

    `smooth` is an extra low-pass on the deviation itself, in frame shifts, and is 0
    for the shipped configuration. It is a different thing from ref_shift: the
    reference sets what the deviation is measured against, while smoothing averages the
    deviation after it has been formed. The two are not interchangeable, and the
    spread has to be the spread of whatever is about to be divided - smoothing the gap
    and dividing by the unsmoothed spread would shrink z toward zero and collapse every
    colour into the middle bucket, which is an artifact of the emulation and not a
    result about the feature.

    The recursion is the one in pipeline.go, in integers, stepped one analysis frame
    at a time using the device's own frame counter: the gap is taken against the
    reference before either is updated, and the spread is the EMA of that same gap's
    magnitude. Frame-exact integer arithmetic is not pedantry. The firmware's update is
    `avg += (gap + 512) >> 10`, so while the gap is under 512 units the increment is
    zero and the reference simply stops following the centroid - a deadband of a
    quarter of a typical spread that a float EMA does not have, and the one mechanism
    here that can hold a colour in place for a long time on its own. Simulating the
    float version instead measured a quarter-spread disagreement with the device's own
    reference, sign following whether the centroid was rising or falling.

    `dev_frac` carries the spread's EMA in Q8 instead of whole units, which is the
    same deadband problem solved the way the gap's low-pass already solves it: the
    spread's own update is `d += (|g| - d + 512) >> 10`, so it cannot move while |g|
    is within 512 of it, and 512 is half of a typical spread. Measured on the hour,
    61% of frames on one earring and 65% on the other sit inside that band, and on a
    quiet passage the spread freezes outright - the left earring over one capture
    read exactly 512, its seed, for every single sample while its partner's spread
    moved between 1200 and 3400. The divisor is supposed to be that device's own
    yardstick; frozen, it is a constant, and a constant that happens to be smaller
    than the deviation pushes that device toward an end colour. In Q8 the deadband is
    two units of the centroid rather than five hundred and twelve. Reported here
    before it is shipped, because three plausible fixes to this codepath have already
    been falsified by measurement.

    What remains approximate is the input: the centroid is sampled at ~5 Hz and held
    across the frames in between, against a 16 s EMA.

    `seed` is the device's own (reference, spread) at the first sample, and passing it
    matters more than it looks. Cold-seeding the reference at the first centroid makes
    the simulation spend two or three time constants climbing toward wherever the
    device's reference actually was, and since that transient points the same way on
    both earrings it pushes both z trajectories to the same side and prints a flattering
    agreement that no device ever had: cold-seeded, this replay reported 85% against
    the pair's real 47%.

    Returns (gap, dev) lists.
    """
    gap = [0] * len(cent)
    dev = [0] * len(cent)
    avg = cent[0] if seed is None else seed[0]
    d = 0 if seed is None else seed[1]
    if dev_frac:
        d <<= 8
    gs = 0
    rhalf = 1 << (ref_shift - 1)
    dhalf = 1 << (dev_shift - 1)
    shalf = 1 << (smooth - 1) if smooth else 0
    for i in range(1, len(cent)):
        # Frames elapsed since the previous sample. A poll that failed and reconnected
        # can skip a lot of them; the centroid is then held at its last reading, which
        # is wrong but bounded, and capping keeps a gap from dominating the runtime.
        step = frames[i] - frames[i - 1]
        if step < 1:
            step = 1
        elif step > 64:
            step = 64
        c = cent[i]
        for _ in range(step):
            g = c - avg
            if smooth:
                gs += (g - gs + shalf) >> smooth
                # dev_raw: the ruler is the spread of the raw deviation while the
                # numerator is the smoothed one. Self-consistent on paper is not the
                # same as safe: with the ruler also smoothed, a centroid that drifts
                # steadily against a lagging reference keeps all of its drift in the
                # numerator and loses most of its variation from the ruler, so z runs to
                # the clamp and both earrings pin to an end colour.
                src = g if dev_raw else gs
                a = abs(src)
            else:
                a = abs(g)
            d += (((a << 8) - d + dhalf) if dev_frac
                  else (a - d + dhalf)) >> dev_shift
            avg += (g + rhalf) >> ref_shift
        gap[i] = gs if smooth else g
        dev[i] = d >> 8 if dev_frac else d
    return gap, dev


def zscore(gap, dev, share, divisor):
    """Scaled deviation, in units of the divisor.

    share is how much of the ruler comes from the device itself: 1 is the shipped
    behaviour (each earring divides by its own spread), 0 is a shared constant ruler.
    A gain cancels in the centroid exactly - the band weights sum to zero - so this
    is not the AGC invariance talking; it is whether a device whose spectrum barely
    changes shape gets to read its leftover drift as a colour.
    """
    out = []
    for g, d in zip(gap, dev):
        if share >= 1.0:
            r = d
        elif share <= 0.0:
            r = divisor
        else:
            r = (d ** share) * (divisor ** (1.0 - share))
        if r < 1:
            r = 1
        v = g / r
        out.append(max(-2.0, min(2.0, v)))
    return out


def bucket(z, cuts, still):
    """Pattern index, matching PatternIndex: the last cut below z, or the middle
    bucket for a device whose spectrum is not moving (NumPatterns/2)."""
    return [2 if s else sum(1 for c in cuts if v > c) for v, s in zip(z, still)]


def runs(same, dt, hold):
    hit = miss = 0
    n = len(same)
    edge = [0] + [i + 1 for i in range(n - 1) if same[i + 1] != same[i]] + [n]
    for lo, hi in zip(edge[:-1], edge[1:]):
        if (hi - lo) * dt >= hold:
            if same[lo]:
                hit += hi - lo
            else:
                miss += hi - lo
    return hit / n, miss / n


def metrics(bl, br, zl, zr, dt, holds):
    n = min(len(bl), len(br))
    bl, br, zl, zr = bl[:n], br[:n], zl[:n], zr[:n]
    same = [x == y for x, y in zip(bl, br)]
    inst = sum(same) / n
    out = {"inst": inst, "holds": {}}
    for h in holds:
        s, d = runs(same, dt, h)
        out["holds"][h] = (s, d)
    chance = sum(bl.count(i) / n * br.count(i) / n for i in range(4))
    out["kappa"] = (inst - chance) / (1 - chance) if chance < 1 else 0.0
    out["occ"] = ([100 * bl.count(i) / n for i in range(4)],
                  [100 * br.count(i) / n for i in range(4)])
    out["chg"] = (sum(1 for i in range(1, n) if bl[i] != bl[i - 1]) / (n * dt),
                  sum(1 for i in range(1, n) if br[i] != br[i - 1]) / (n * dt))
    ml = sum(zl) / n
    mr = sum(zr) / n
    cov = sum((a - ml) * (b - mr) for a, b in zip(zl, zr)) / n
    vl = math.sqrt(sum((a - ml) ** 2 for a in zl) / n)
    vr = math.sqrt(sum((b - mr) ** 2 for b in zr) / n)
    out["r"] = cov / (vl * vr) if vl > 0 and vr > 0 else float("nan")
    out["same_sign"] = sum(1 for a, b in zip(zl, zr) if (a > 0) == (b > 0)) / n
    out["min_bucket"] = min(min(o) for o in out["occ"])
    return out


def show(tag, m, holds, extra=""):
    hs = "  ".join(f"{h:g}s {100*m['holds'][h][0]:5.1f}/{100*m['holds'][h][1]:5.1f}"
                   for h in holds)
    # The committed pattern has no z behind it here, so its correlation would print
    # as a meaningless nan.
    r = "" if math.isnan(m["r"]) else f"r {m['r']:+.3f}  "
    print(f"{tag:34s} inst {100*m['inst']:5.1f}%  k {m['kappa']:+.3f}  "
          f"{r}min {m['min_bucket']:4.1f}%  "
          f"chg {m['chg'][0]:.2f}/{m['chg'][1]:.2f}  {hs}  {extra}")
    for i, o in enumerate(m["occ"]):
        print(f"{'':34s} occ {'L' if i == 0 else 'R'} " +
              " ".join(f"{v:5.1f}" for v in o))


def quantile(vals, q):
    v = sorted(vals)
    if not v:
        return 0.0
    k = max(0, min(len(v) - 1, int(q * (len(v) - 1))))
    return v[k]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("dir")
    ap.add_argument("--holds", default="1,2,4")
    ap.add_argument("--skip", type=float, default=30.0,
                    help="drop the first N seconds: the reference is still "
                         "converging and the simulation cannot know its seed")
    ap.add_argument("--profile", type=float, default=0.0,
                    help="also print a per-N-seconds profile of the pair")
    # Two ways a candidate can score well without being any good, gated explicitly
    # rather than eyeballed. minbucket stops a selector that hides one colour; minchg
    # stops one that simply stops moving, since two statues agree perfectly. The shipped
    # selector runs about 0.10 changes/s on this data, so the default admits something
    # five times slower; the sweep is worth re-running at 0.06 to see what a selector
    # that still visibly moves costs.
    ap.add_argument("--minbucket", type=float, default=5.0, metavar="%",
                    help="smallest colour occupancy allowed in either half")
    ap.add_argument("--only", default="",
                    help="comma-separated ref:spread:share:smooth:cuts specs to "
                         "print as a decision table instead of a ranking")
    ap.add_argument("--bands", type=float, default=0.0, metavar="SEC",
                    help="print a per-N-seconds per-band cross-correlation table")
    ap.add_argument("--minchg", type=float, default=0.02, metavar="PER SEC",
                    help="lowest change rate allowed for either device")
    args = ap.parse_args()
    holds = [float(x) for x in args.holds.split(",")]

    L, R = load(args.dir, "L"), load(args.dir, "R")
    # Aligned by timestamp, not by index. Each side is polled by its own thread with
    # its own clock origin, so the two files start a few hundred milliseconds apart,
    # and over an hour the two devices' own frame counters drift apart too (they run
    # from the internal oscillator, and one measured 0.7% faster than the other).
    # Index pairing would quietly compare one earring's present against the other's
    # past, which is exactly the thing this tool exists to measure.
    rt = [r[0] for r in R]
    off = rt[0] - L[0][0]
    pairs = []
    j = 0
    for lr in L:
        want = lr[0] + off
        while j < len(rt) - 1 and rt[j + 1] < want:
            j += 1
        k = j
        if k + 1 < len(rt) and abs(rt[k + 1] - want) < abs(rt[k] - want):
            k = k + 1
        if abs(rt[k] - want) <= 0.25:
            pairs.append((lr, R[k]))
    print(f"{args.dir}: {len(L)}+{len(R)} samples, clock origin differs by "
          f"{off*1000:.0f} ms, {len(pairs)} paired within 250 ms")
    L, R = [p[0] for p in pairs], [p[1] for p in pairs]
    n = len(L)
    if n < 50:
        sys.exit("too few paired samples")
    t = [r[0] for r in L]
    dt = (t[-1] - t[0]) / max(1, len(t) - 1)
    print(f"{args.dir}: {n} paired samples, {t[-1]-t[0]:.0f} s, "
          f"poll {1/dt:.1f} Hz (frame rate {FRAME_RATE} Hz)")

    # The committed answer, straight off the capture, as the reference point for
    # everything the replay says.
    pL = [r[4] for r in L]
    pR = [r[4] for r in R]
    show("committed pattern (measured)",
         metrics(pL, pR, [float("nan")] * n, [float("nan")] * n, dt, holds), holds)

    # Does the replay reproduce the device? Seeded from the first sample, so early
    # samples must be dropped; the check is whether the simulated reference tracks the
    # real one once both have settled.
    centL = [r[1] for r in L]
    centR = [r[1] for r in R]
    # The honest version of the selector, with no simulation in it at all: the
    # pattern the device's OWN live cent/centAvg/centDev would have chosen, bucketed
    # the way PatternIndex buckets it. Compare its agreement against the committed
    # row above: the difference is the commitment layer, and it is the only part of
    # the gap that is not about the feature. Compare it against the replayed row
    # below: the difference there is replay error, and it has to be small before any
    # candidate number printed here can be believed.
    zLr = [max(-2.0, min(2.0, (r[1] - r[2]) / max(r[3], 1))) for r in L]
    zRr = [max(-2.0, min(2.0, (r[1] - r[2]) / max(r[3], 1))) for r in R]
    show("selector from the device's own RAM",
         metrics(bucket(zLr, CUTS_SHIPPED, [r[3] < MIN_DEV for r in L]),
                 bucket(zRr, CUTS_SHIPPED, [r[3] < MIN_DEV for r in R]),
                 zLr, zRr, dt, holds), holds)

    print("\nmodel check (simulated centAvg/centDev against the device's own):")
    for name, rows in (("L", L), ("R", R)):
        cent = [r[1] for r in rows]
        gap, dev = track(cent, [r[5] for r in rows], seed=(rows[0][2], rows[0][3]))
        k = max(1, int(args.skip / dt))
        # The gap the simulation and the firmware agree on is cent - centAvg; the
        # firmware's own centAvg is the reference, so a real error here is a drift in
        # the simulated reference, which is what the candidates divide by. Signed,
        # because a bias the same way on both devices inflates their agreement with
        # each other, and only a signed number shows that.
        ea = [gap[i] - (rows[i][1] - rows[i][2]) for i in range(k, n)]
        ed = [dev[i] - rows[i][3] for i in range(k, n)]
        med_d = quantile([r[3] for r in rows[k:]], 0.5) or 1
        print(f"  {name}: centAvg error median {quantile(ea, 0.5):+6.0f} "
              f"({100*quantile(ea,0.5)/med_d:+.0f}% of spread) "
              f"|error| p95 {quantile([abs(v) for v in ea], 0.95):6.0f}"
              f"   centDev error median {quantile(ed, 0.5):+6.0f} "
              f"({100*quantile(ed, 0.5)/med_d:+.0f}%)")

    k0 = max(1, int(args.skip / dt))
    tt = t[k0:]
    cL, cR = centL[k0:], centR[k0:]
    fL = [r[5] for r in L][k0:]
    fR = [r[5] for r in R][k0:]
    tL = [x - t[k0] for x in tt]

    # The shipped configuration, replayed: this number and the measured one above
    # should be close, or none of the sweep below means anything.
    gL, dL = track(cL, fL, seed=(L[k0][2], L[k0][3]))
    gR, dR = track(cR, fR, seed=(R[k0][2], R[k0][3]))
    zL = zscore(gL, dL, 1.0, 0)
    zR = zscore(gR, dR, 1.0, 0)
    stillL = [d < MIN_DEV for d in dL]
    stillR = [d < MIN_DEV for d in dR]
    print(f"\nreplay z mean {sum(zL)/len(zL):+.2f} / {sum(zR)/len(zR):+.2f} "
          f"vs the device's own "
          f"{sum(zLr[k0:])/len(zLr[k0:]):+.2f} / {sum(zRr[k0:])/len(zRr[k0:]):+.2f}")

    # Where the disagreement is in the spectrum. The centroid is a weighted sum of all
    # eight bands, so a single band whose two earrings read differently - a microphone
    # with a noisy high end, a band sitting on a room null - drags the chosen colour
    # with it, and the aggregate r cannot say which. Correlated per block because a
    # correlation over the whole hour averages passages where the pair agrees with
    # passages where it does not, and it is the passages that are actionable.
    #
    # Read this alongside the profile, not instead of it: a band can correlate badly and
    # still not matter, if its weight times its variance is small next to the others.
    if args.bands > 0:
        print("\nper-band correlation between the two earrings (raw band energy, "
              "not the centroid):")
        print("  min  " + " ".join(f"{'b' + str(i):>5s}" for i in range(8)) +
              "   cent  total L  total R")
        lo = 0
        while lo < n - 10:
            hi = min(n, lo + int(args.bands / dt))
            if hi - lo < 10:
                break
            idx = range(lo, hi)
            bs = [corr([L[i][6][k] for i in idx], [R[i][6][k] for i in idx])
                  for k in range(8)]
            print(f"  {t[lo]:5.0f} " + " ".join(f"{c:+5.2f}" for c in bs) +
                  f"  {corr(zLr[lo:hi], zRr[lo:hi]):+5.2f} "
                  f"{sum(sum(L[i][6]) for i in idx)/(hi-lo):7.0f} "
                  f"{sum(sum(R[i][6]) for i in idx)/(hi-lo):7.0f}")
            lo = hi

    # Where the hour actually goes. One number for the whole capture cannot say
    # whether the pair disagrees because one earring has a permanent opinion the other
    # lacks, or because the disagreement wanders between passages - and those want
    # opposite fixes. Per minute: agreement from the devices' own state, each side's
    # mean deviation from its own reference, each side's spread, and the AGC. A row
    # where both z are large and of one sign and the agreement is low is the level
    # problem; a row where both z wander together and agreement is still low is the
    # noise problem.
    if args.profile > 0:
        print("\nper-minute profile (own RAM, no replay):")
        print("  min  agree%   z L    z R   dev L  dev R  agc L/R   "
              "|zL-zR| med")
        step = args.profile
        lo = k0
        while lo < n:
            hi = min(n, lo + int(step / dt))
            if hi - lo < 10:
                break
            a = sum(1 for i in range(lo, hi)
                    if bucket([zLr[i]], CUTS_SHIPPED, [L[i][3] < MIN_DEV])[0]
                    == bucket([zRr[i]], CUTS_SHIPPED, [R[i][3] < MIN_DEV])[0])
            mz = (sum(zLr[lo:hi]) / (hi - lo), sum(zRr[lo:hi]) / (hi - lo))
            dv = (quantile([L[i][3] for i in range(lo, hi)], 0.5),
                  quantile([R[i][3] for i in range(lo, hi)], 0.5))
            ag = (quantile([L[i][7] for i in range(lo, hi)], 0.5),
                  quantile([R[i][7] for i in range(lo, hi)], 0.5))
            off = quantile([abs(zLr[i] - zRr[i]) for i in range(lo, hi)], 0.5)
            print(f"  {t[lo]:5.0f} {100*a/(hi-lo):6.1f}  {mz[0]:+5.2f}  {mz[1]:+5.2f}"
                  f"  {dv[0]:6.0f} {dv[1]:6.0f}   {ag[0]:3.0f}/{ag[1]:<3.0f}"
                  f"   {off:5.2f}")
            lo = hi
    show("shipped selector (replayed)",
         metrics(bucket(zL, CUTS_SHIPPED, stillL),
                 bucket(zR, CUTS_SHIPPED, stillR), zL, zR, dt, holds), holds)

    # Candidate search. Every candidate is a set of constants of the shape the
    # firmware already has - the reference's time constant, the spread's, how much of
    # the divisor belongs to the device itself, and where the cuts sit - so whatever
    # wins is a rebuild rather than a redesign.
    #
    # Fitted on the first half and reported on the second, which is not a formality:
    # this file is several tracks long, and two candidates have already been watched
    # measuring well on the passage they were chosen from and failing on hardware. A
    # candidate whose halves disagree about whether it is any good is not a candidate.
    mid = (tL[0] + tL[-1]) / 2.0
    train = [i for i, x in enumerate(tL) if x < mid]
    test = [i for i, x in enumerate(tL) if x >= mid]
    print(f"\nsearch over {len(train)+len(test)} samples: fitted on the first "
          f"{mid:.0f} s, reported on the last {tL[-1]-mid:.0f} s")

    cache = {}

    def pair_for(ref, dsh, sm=0, dev_raw=False):
        key = (ref, dsh, sm, dev_raw)
        if key not in cache:
            cache[key] = (
                track(cL, fL, ref_shift=ref, dev_shift=dsh,
                      seed=(L[k0][2], L[k0][3]), smooth=sm, dev_raw=dev_raw),
                track(cR, fR, ref_shift=ref, dev_shift=dsh,
                      seed=(R[k0][2], R[k0][3]), smooth=sm, dev_raw=dev_raw))
        return cache[key]

    def cuts_for(pooled, mode):
        """Where the three cuts go. Pooled over both earrings on purpose: fitting a
        ruler per device is what put them on opposite sides of a cut, and one set of
        constants is still the same pure function of the music in both."""
        q = [quantile(pooled, x) for x in (0.25, 0.5, 0.75)]
        if mode == "shipped":
            return CUTS_SHIPPED
        if mode == "sym":
            # The only shape PatternIndex can express today: cuts sit at -k, 0, +k.
            # Fitted as the least-squares symmetric triple through the quantiles, so
            # k = half the interquartile range and the centre is forced to the
            # reference however far the data's own middle sits from it.
            k = (q[2] - q[0]) / 2.0
            return (-k, 0.0, k)
        # A free triple, which needs PatternIndex to read a table instead of deriving
        # the cuts from PatternCutQ8. Reported separately for exactly that reason: if
        # it wins, the reason is that the deviation is not centred on the reference,
        # and that is a fact about the ruler worth acting on deliberately.
        return tuple(q)

    results = []
    for ref in (7, 8, 9, 10, 11, 12):
        for dsh in (10, 11, 12, 13, 14):
            for share in (1.0, 0.5, 0.0):
                # 11 used to be missing from this list. It sits between 10, which
                # ships, and 12, which freezes one device into two buckets - and the
                # gap between a winner and a corpse is exactly where a grid must not
                # get coarse.
                for sm in (0, 6, 8, 9, 10, 11, 12):
                    for draw in (False, True):
                        (gl, dl), (gr, dr) = pair_for(ref, dsh, sm, draw)
                        div = quantile([dl[i] for i in train] + [dr[i] for i in train], 0.5)
                        zl = zscore(gl, dl, share, div)
                        zr = zscore(gr, dr, share, div)
                        no_still = [False] * len(zl)
                        sl = [d < MIN_DEV for d in dl] if share > 0 else no_still
                        sr = [d < MIN_DEV for d in dr] if share > 0 else no_still
                        pooled = [zl[i] for i in train] + [zr[i] for i in train]
                        for mode in ("shipped", "sym", "free"):
                            cuts = cuts_for(pooled, mode)
                            bl = bucket(zl, cuts, sl)
                            br = bucket(zr, cuts, sr)
                            mt = metrics([bl[i] for i in train], [br[i] for i in train],
                                         [zl[i] for i in train], [zr[i] for i in train],
                                         dt, holds)
                            mv = metrics([bl[i] for i in test], [br[i] for i in test],
                                         [zl[i] for i in test], [zr[i] for i in test],
                                         dt, holds)
                            live = min(min(mt["chg"]), min(mv["chg"])) >= args.minchg
                            even = min(mt["min_bucket"], mv["min_bucket"]) >= args.minbucket
                            # Occupancy on the held-out half, per device: a selector can
                            # average four colours over the hour and still spend the second
                            # half on two of them.
                            occl = [100 * [bl[i] for i in test].count(b) / len(test)
                                    for b in range(4)]
                            occr = [100 * [br[i] for i in test].count(b) / len(test)
                                    for b in range(4)]
                            results.append((min(mt["inst"], mv["inst"]), even, live, ref,
                                            dsh, share, mode, cuts, mt, mv, sm,
                                            occl, occr, draw))

    def row(tag, res):
        (_, even, live, ref, dsh, share, mode, cuts, mt, mv, sm,
         occl, occr, draw) = res
        h = holds[0] if holds else 2.0
        smtxt = ("  off" if not sm
                 else f"{(1<<sm)/FRAME_RATE:4.0f}s" + ("*" if draw else " "))
        print(f"{tag:26s} ref {ref} ({(1<<ref)/FRAME_RATE:3.0f}s) "
              f"spread {dsh} ({(1<<dsh)/FRAME_RATE:3.0f}s) sm{smtxt} sh {share:.1f} "
              f"cuts {mode:7s} {cuts[0]:+.2f}/{cuts[1]:+.2f}/{cuts[2]:+.2f} "
              f"| fit {100*mt['inst']:5.1f}%  test {100*mv['inst']:5.1f}%  "
              f"worst {100*res[0]:5.1f}%  k {mv['kappa']:+.3f} r {mv['r']:+.2f}  "
              f"min {min(mt['min_bucket'], mv['min_bucket']):4.1f}%  "
              f"chg {mv['chg'][0]:.2f}/{mv['chg'][1]:.2f}  "
              f"{h:g}s {100*mv['holds'][h][0]:5.1f}/{100*mv['holds'][h][1]:5.1f}  "
              f"occ " + " ".join(f"{x:4.1f}" for x in occl) + " | " +
              " ".join(f"{x:4.1f}" for x in occr) +
              f" {'even' if even else 'skew'}{'' if live else ' DEAD'}")

    ship = [r for r in results
            if r[3] == 10 and r[4] == 10 and r[5] == 1.0 and r[6] == "shipped"
            and r[10] == 0]
    print("\nreference points:")
    for r in ship:
        row("shipped", r)

    ok = [r for r in results if r[1] and r[2]]
    if args.only:
        # A decision table instead of a ranking. The ranking is dominated by clusters of
        # near-identical candidates - a sweep over five spread values mostly finds the
        # same answer five times - while the choice that actually has to be made is
        # between a handful of specific configurations, each with a different cost: one
        # new constant, one new state variable, or a cut table. Named here, side by
        # side, on both halves.
        want = set()
        for spec in args.only.split(","):
            f = spec.split(":")
            g = f + ["0"]
            want.add((int(g[0]), int(g[1]), float(g[2]), int(g[3]), g[4],
                      bool(int(g[5]))))
        print("\ndecision table:")
        for r in results:
            # r[13] is the dev_raw flag. Naming it in the spec matters: the same
            # low-pass scores several points apart depending on whether the ruler
            # is the spread of the raw deviation or of the smoothed one, and with
            # the wrong index every spec silently matched nothing.
            if (r[3], r[4], r[5], r[10], r[6], r[13]) in want:
                row(f"cand {r[3]}:{r[4]}:{r[5]}:{r[10]}:{r[6]}", r)
        return
    print(f"\n{len(ok)} of {len(results)} candidates keep every colour above "
          f"{args.minbucket:.0f}% and both devices above {args.minchg:.2f} "
          f"changes/s; ranked by the worse of the two halves:")
    # Keyed on the score alone: the tuple also carries the two metric dicts, and once
    # two candidates tie on every scalar ahead of them Python tries to order dicts.
    for r in sorted(ok, key=lambda r: r[0], reverse=True)[:18]:
        row("", r)

    # The excluded list is only interesting if it excludes something other than the
    # frozen configuration. A long enough low-pass on the deviation makes both
    # earrings sit in one bucket permanently, which scores 100% agreement and means
    # nothing at all; those are reported as a count instead of crowding out the rows
    # that show what else the constraint costs.
    bad = [r for r in results if not (r[1] and r[2])]
    frozen = [r for r in bad if min(min(r[9]["chg"]), min(r[8]["chg"])) < 0.005
              and min(min(r[9]["chg"]), min(r[8]["chg"])) >= 0.0]
    print(f"\n{len(bad)} excluded for starving a colour (min bucket <"
          f" {args.minbucket:.0f}%) or for a dead device (below "
          f"{args.minchg:.2f} changes/s); {len(frozen)} of them freeze "
          f"altogether. Best of the rest:")
    rest = sorted([r for r in bad if not (r[1] and r[2])],
                  key=lambda r: r[0], reverse=True)
    shown = 0
    for r in rest:
        if min(min(r[9]["chg"]), min(r[8]["chg"])) < 0.005:
            continue
        row("", r)
        shown += 1
        if shown == 6:
            break
    if not shown:
        print("  (everything excluded here is frozen or near-frozen)")

    # What the winner still leaves on the table, in the units the diagnosis is in.
    if ok:
        best = max(ok, key=lambda r: r[0])
        (gl, dl), (gr, dr) = pair_for(best[3], best[4], best[10])
        div = quantile([dl[i] for i in train] + [dr[i] for i in train], 0.5)
        zl = [zscore(gl, dl, best[5], div)[i] for i in test]
        zr = [zscore(gr, dr, best[5], div)[i] for i in test]
        # Over the whole capture, for the profile below: comparing a candidate's worst
        # five minutes against the measured worst five minutes only works if both cover
        # the same hour, and the held-out half is a different hour.
        zl_f = zscore(gl, dl, best[5], div)
        zr_f = zscore(gr, dr, best[5], div)
        bl = bucket(zl_f, best[7], [d < MIN_DEV for d in dl])
        br = bucket(zr_f, best[7], [d < MIN_DEV for d in dr])
        print(f"\nbest candidate on the held-out half: mean z "
              f"{sum(zl)/len(zl):+.2f} / {sum(zr)/len(zr):+.2f}, "
              f"same sign {100*sum(1 for a, b in zip(zl, zr) if (a > 0) == (b > 0))/len(zl):.0f}%")
        # The profile is the part that decides whether to believe the number. A +10
        # point mean that comes from fixing the three bad minutes is a fix; one that
        # comes from making the good minutes better is a different claim, and only the
        # first is worth a rebuild.
        cuts_b = best[7]
        print("per-minute profile, best candidate (replayed) vs measured:")
        print("  min  cand%  meas%   r      chg/s")
        step = 300.0
        lo = 0
        while lo < len(tL) - 10:
            # The trailing window is short and its means are meaningless; stop before it
            # rather than reporting a percentage of nothing.
            hi = min(len(tL), lo + int(step / dt))
            c = sum(1 for i in range(lo, hi) if bl[i] == br[i])
            m = sum(1 for i in range(lo, hi)
                    if bucket([zLr[k0 + i]], CUTS_SHIPPED, [L[k0 + i][3] < MIN_DEV])[0]
                    == bucket([zRr[k0 + i]], CUTS_SHIPPED, [R[k0 + i][3] < MIN_DEV])[0])
            zzl, zzr = zl_f[lo:hi], zr_f[lo:hi]
            ml, mr = sum(zzl) / len(zzl), sum(zzr) / len(zzr)
            cov = sum((a - ml) * (b - mr) for a, b in zip(zzl, zzr))
            vl = math.sqrt(sum((a - ml) ** 2 for a in zzl))
            vr = math.sqrt(sum((b - mr) ** 2 for b in zzr))
            ch = (sum(1 for i in range(lo + 1, hi) if bl[i] != bl[i - 1])
                  + sum(1 for i in range(lo + 1, hi) if br[i] != br[i - 1]))
            print(f"  {tL[lo]:5.0f} {100*c/(hi-lo):6.1f} {100*m/(hi-lo):6.1f} "
                  f"{cov/(vl*vr) if vl > 0 and vr > 0 else float('nan'):+6.2f} "
                  f"{ch/((hi-lo)*dt)/2:6.2f}")
            lo = hi
        # The requirement is not just agreement: a colour nobody ever picks is a bug
        # and a colour picked half the time is nearly as bad. Reported for the winner
        # on the held-out half, because that is the only half nobody fitted on.
        cuts = best[7]
        sl = [dl[i] < MIN_DEV for i in test]
        sr = [dr[i] < MIN_DEV for i in test]
        for nm, z, s in (("L", zl, sl), ("R", zr, sr)):
            b = bucket(z, cuts, s)
            print(f"  occupancy {nm}: " + " ".join(
                f"{100*b.count(i)/len(b):5.1f}" for i in range(4)) + " %")


if __name__ == "__main__":
    main()
