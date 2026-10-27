#!/usr/bin/env python3
"""Replay the selector that SHIPS - master's argmax - over recorded spectra, and
test the cheap changes that could be ported to it.

The master branch derives the pattern from a per-frame least-squares line through
the smoothed spectrum and picks the band that stands above it (spectralDevs +
PatternIndex + hysteresis). That is a different measurement from the centroid
cascade developed on the debug branch, and the two agree with each other about as
well as chance, so tuning one says nothing directly about the other. What they do
share is the input: both read p.smooth, and the capture records p.smooth as b0..b7.
So master's choice can be replayed over any capture without flashing it.

The question worth asking is not which selector is cleverer but which changes fit.
Master is at 29504 of 29568 bytes of flash - sixty-four bytes free - so an
improvement has to be measured against what it costs, and a change that needs a
histogram of counters or a hundred lines of stateful DSP is not an improvement to
master, it is a different product.

  python3 mastersel.py ~/psync/newfull [more captures...]
"""
import csv
import os
import sys

import numpy as np

NB = 8
FPS = 62.5
# Master keeps NBands-1 patterns; the debug branch has four. Occupancy is reported
# per realised pattern, and a selector that reaches three of seven is reported as
# three, because that is what a person wearing them sees.
NP = NB - 1
MARGIN = 32 << 8
Q16Q = 49939


def mul_q16(n, q=Q16Q):
    """Master's fixed-point multiply, sign handling included. For a negative
    product the floor rounds away from zero, so the low half costs an extra -1."""
    neg = n < 0
    u = -n if neg else n
    lo = (u & 0xFFFF) * q
    p = (u >> 16) * q + (lo >> 16)
    if neg:
        p = -p
        if lo & 0xFFFF:
            p -= 1
    return p


def spectral_devs(e):
    """The deviation of every band above the least-squares line through the
    spectrum, in Q8<<8. Straight from master, including the integer arithmetic.

    Checked against the Go original on four spectra, one of them a perfectly
    linear ramp, which must come out at about zero for every band. It did not on
    the first attempt: sxy weights by b, not b+1, and the missing decrement above
    turned a flat deviation of 14 into 452315. That is the difference between "no
    band stands out" and "band 0 towers", so nothing in this file is trustworthy
    until that comparison passes.
    """
    sy = 0
    sxy = 0
    for b in range(NB):
        sy += e[b]
        sxy += e[b] * b
    num = 8 * sxy - 28 * sy
    slope = mul_q16(num)
    intercept = (sy << 5) - (slope * 7 >> 1)
    return [(e[b] << 8) - intercept - slope * b for b in range(NB)]


def argmax(dev):
    best = 0
    for b in range(1, NB - 1):
        if dev[b] > dev[best]:
            best = b
    return best


def hyst(dev, cur, valid, margin=MARGIN):
    ch = argmax(dev)
    if valid and ch != cur and dev[ch] < dev[cur] + margin:
        return cur
    return ch


def hyst_rel(dev, cur, valid, num, den):
    """Hysteresis whose margin is a fraction of the spread across the bands.

    Master's margin is an absolute 32<<8, but the deviations it is compared
    against are whatever the line fit happens to produce, and their scale depends
    entirely on how peaked the spectrum is. Smooth the spectrum for longer and the
    deviations shrink; below some point no challenger can ever beat the incumbent
    by 8192 and the band latches for the rest of the song. Measured: at a 16 s
    window the selector freezes onto one colour and reports 100% agreement, which
    is two earrings both stuck, not two earrings agreeing.

    A fraction of max-min carries no units, so the same setting means the same
    thing on a dull frame and a spiky one.
    """
    ch = argmax(dev)
    if valid and ch != cur:
        spread = max(dev) - min(dev)
        if dev[ch] < dev[cur] + (num * spread) // den:
            return cur
    return ch


def score(a, b, warm):
    """Same definitions as q8fix.score, which has been checked against the device.

    An earlier version here computed its own kappa and got the expected-agreement
    term wrong - the sum of the whole outer product of the marginals rather than
    its trace, which is always 1.0 - so the denominator vanished and every row
    printed kappa +0.000, including a row that simultaneously printed 100%
    agreement. Occupancy truncated to zero the same way, int() applied to a
    fraction. Two tools scoring the same pair differently is worse than one tool
    being wrong, so this delegates rather than reimplementing.
    """
    a, b = a[warm:], b[warm:]
    po = float(np.mean(a == b))
    pe = sum(float(np.mean(a == k)) * float(np.mean(b == k)) for k in range(NP))
    kap = (po - pe) / (1 - pe) if pe < 1 else 0.0
    mo = min(min(float(np.mean(a == k)), float(np.mean(b == k)))
             for k in range(NP))
    ch = [float(np.sum(np.diff(p) != 0)) / (len(a) / FPS / 60) for p in (a, b)]
    used = len(set(a.tolist()) | set(b.tolist()))
    return po, kap, mo, ch, used


sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import ema as emalib  # noqa: E402

# The non-stalling form. An earlier version of this file used a plain rounded EMA,
# which freezes once its input settles closer than half the shift, and reported the
# result as "long smoothing makes the selector latch onto one band". The selector
# was fine; the average had stopped. See ema.py.
ema = emalib.ema


def medfilt_cols(m, k):
    """Causal k-point median down each column."""
    if k <= 1:
        return m
    h = k - 1
    pad = np.concatenate([np.repeat(m[:1], h, axis=0), m])
    w = np.lib.stride_tricks.sliding_window_view(pad, k, axis=0)
    return np.median(w, axis=2).astype(np.int64)


def run(G, mode, arg, margin=MARGIN, relmargin=None):
    """Selector over a (frames, NBands) array of smoothed spectra.

    The band average is recomputed per column, so a slow mode costs NBands passes;
    that is analysis, not firmware - the shipped equivalent would hold NBands
    accumulators, which is RAM master does not have. Worth knowing before any
    slow-window idea is priced.

    mode "ship": master exactly - deviations from the 0.5 s spectrum.
    mode "slow": master's argmax over a second, much slower band average. This is
        the portable form of the one finding that repeated across every experiment:
        the pattern wants a far longer window than the hash does. One extra array
        and one changed argument, no new mechanism.
    mode "med": master's argmax over a k-point median of the spectrum, which kills
        the single-frame spikes a longer average would have smeared instead.
    """
    n = len(G)
    out = np.zeros(n, dtype=np.int64)
    slow = None
    if mode == "slow":
        slow = np.stack([ema(G[:, b], arg) for b in range(NB)], axis=1)
    elif mode == "med":
        G = medfilt_cols(G, arg)
    cur, valid = 0, False
    for i in range(n):
        e = slow[i] if mode == "slow" else G[i]
        dev = spectral_devs(e)
        if relmargin is None:
            cur = hyst(dev, cur, valid, margin)
        else:
            cur = hyst_rel(dev, cur, valid, relmargin[0], relmargin[1])
        valid = True
        out[i] = cur
    return out


def load(path):
    t, b = [], []
    with open(path) as fh:
        for r in csv.DictReader(fh):
            t.append(float(r["t"]))
            b.append([int(float(r[f"b{i}"])) for i in range(NB)])
    t = np.asarray(t)
    o = np.argsort(t)
    return t[o], np.asarray(b, dtype=np.int64)[o]


def main():
    if len(sys.argv) < 2:
        raise SystemExit(__doc__)
    emalib.self_test()  # raises if the averaging stalls; see ema.py
    # (label, mode, ema shift or median width, absolute margin, relative margin)
    cands = [("as shipped 0.5s", "ship", 1, MARGIN, None),
             ("shipped no hysteresis", "ship", 1, 0, None),
             ("shipped rel 1/8", "ship", 1, 0, (1, 8)),
             ("shipped rel 1/16", "ship", 1, 0, (1, 16)),
             ("slow 4s abs", "slow", 6, MARGIN, None),
             ("slow 4s rel 1/8", "slow", 6, 0, (1, 8)),
             ("slow 16s abs", "slow", 10, MARGIN, None),
             ("slow 16s rel 1/8", "slow", 10, 0, (1, 8)),
             ("slow 16s rel 1/32", "slow", 10, 0, (1, 32)),
             ("slow 33s abs", "slow", 11, MARGIN, None),
             ("slow 33s rel 1/8", "slow", 11, 0, (1, 8)),
             ("slow 33s rel 1/32", "slow", 11, 0, (1, 32)),
             ("slow 66s rel 1/8", "slow", 12, 0, (1, 8)),
             ("med5 shipped", "med", 5, MARGIN, None),
             ("med15 shipped", "med", 15, MARGIN, None),
             ("med45 shipped", "med", 45, MARGIN, None),
             ("med15 rel 1/8", "med", 15, 0, (1, 8)),
             ("med45 rel 1/8", "med", 45, 0, (1, 8))]
    for d in sys.argv[1:]:
        data = {}
        for side in ("L", "R"):
            p = os.path.join(d, f"state_{side}.csv")
            if not os.path.exists(p):
                raise SystemExit(f"{p}: missing")
            data[side] = load(p)
        (tl, bl), (tr, br) = data["L"], data["R"]
        lo, hi = max(tl[0], tr[0]), min(tl[-1], tr[-1])
        tg = np.arange(lo, hi, 1.0 / FPS)
        GL = bl[np.clip(np.searchsorted(tl, tg), 0, len(tl) - 1)][:len(tg)]
        GR = br[np.clip(np.searchsorted(tr, tg), 0, len(tr) - 1)][:len(tg)]
        n = len(GL)
        warm = int(120 * FPS)
        print(f"\n=== {d}: master's selector replayed, {(n-warm)/FPS/60:.1f} min")
        print(f"  {'variant':<22} {'agree':>7} {'kappa':>7} {'used':>5} "
              f"{'minocc':>7} {'ch/min':>9}")
        for name, mode, arg, mabs, mrel in cands:
            pl = run(GL, mode, arg, mabs, mrel)
            pr = run(GR, mode, arg, mabs, mrel)
            po, kap, mo, ch, used = score(pl, pr, warm)
            # Agreement from a selector that has frozen is not agreement: two
            # devices that never change colour agree always. Say so rather than
            # letting the number stand on its own.
            flag = ""
            if used <= 2:
                flag = " <- frozen, agreement means nothing"
            elif mo < 0.12:
                flag = " <- colours unused"
            print(f"  {name:<22} {po*100:6.1f}% {kap:+7.3f} {used:3d}/{NP} "
                  f"{mo*100:6.1f}% {ch[0]:4.1f}/{ch[1]:<4.1f}{flag}", flush=True)


if __name__ == "__main__":
    main()
