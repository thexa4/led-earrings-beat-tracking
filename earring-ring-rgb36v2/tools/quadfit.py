#!/usr/bin/env python3
"""Two independent spectral tilts instead of one, and the four patterns are the
quadrants.

Why the one-scalar rule cannot satisfy both things the user asked for
---------------------------------------------------------------------
Every rule tried so far - the shipped ramp, b4-b0, b45-b01 - is one scalar with
three cut points, so the four colours sit in order along one line and the cut
width trades the two requirements off against each other directly. Measured on
t1 with the Q8 reference and ruler (tools/q8fix.py), for b4-b0:

  cut   agreement  kappa   minocc
   70      62.5%   +0.384    6.4%
  150      51.9%   +0.330   15.5%
  190      44.8%   +0.261   16.7%
  280      75.6%   +0.505    0.0%

Widening the cuts evens the colours out and throws away the agreement, because
the middle two colours are the "ordinary spectrum" band around the reference and
the outer two only occur on a real excursion. That is not a tuning problem, it is
the geometry of putting four ordered labels on one number: equal occupancy needs
the cuts at the quartiles of a distribution that is heavily concentrated at the
middle, so the outer labels get starved no matter where the cuts go.

Two binary features have no such problem. Take two contrasts, each a weighted sum
of bands whose weights sum to zero, so neither is moved by overall loudness, and
each compared against its own slow reference, so neither is moved by where this
earring sits in the room. Each is then + or - about half the time by
construction, and the pattern is the pair of signs. If the two are roughly
independent, all four quadrants land near 25% each AND the agreement is the
product of the two sign agreements: two features that each agree 85% of the time
give 72% four-way agreement at even occupancy, which the single scalar cannot
reach without collapsing to two colours.

The risk is that the two features are secretly one feature. If both signs are
really "how loud is it", the quadrants collapse onto the diagonal, two of them go
empty, and the scheme silently degrades to a coin flip between two colours. So
this reports the within-device dependence between the two bits alongside the
agreement, and refuses to show a row whose quadrants are lopsided without saying
so.

Selection is by hold-out: the pair is chosen on the first half of the capture and
scored on the second half, never on the half it was picked from.

  python3 tools/quadfit.py DIR [DIR...]
"""
import csv
import os
import sys

import numpy as np

NUM_PATTERNS = 4
FPS = 62.5
NB = 8
# The firmware's own smoothing constants, so a result here describes the machine.
FAST_SHIFT = 6      # ~1 s  - what the band is read as
SLOW_SHIFT = 11     # ~32 s - the reference that removes this earring's own tone
WARMUP = int(150 * FPS)


def ema(x, shift):
    """The firmware's EMA: out += (in - out + half) >> shift."""
    out = np.empty_like(x)
    acc = x[0]
    half = 1 << (shift - 1)
    for i in range(len(x)):
        acc += (x[i] - acc + half) >> shift
        out[i] = acc
    return out


def dev(bands, fast=FAST_SHIFT, slow=SLOW_SHIFT):
    """Band minus its own slow reference, per unit. Removes the steady-state
    spectrum of this earring in this spot, which is the offset no two earrings
    share. Kept in integer arithmetic matching the firmware's."""
    b = bands.astype(np.int64)
    f = np.stack([ema(b[:, i], fast) for i in range(NB)], axis=1)
    r = np.stack([ema(f[:, i].astype(np.int64), slow) for i in range(NB)], axis=1)
    return f - r


def contrast(d, i, j):
    """A zero-sum pair of bands: immune to overall level, since raising every
    band by the same amount leaves this unchanged."""
    return d[:, i] - d[:, j]


def medbit(x, step):
    """The sign of x about its own running MEDIAN, tracked a fixed step at a time.

    Comparing against a mean does not give a balanced bit. The excursion of a
    log-band away from its average is strongly skewed - a passage that drops out
    moves it tens of counts, a passage that comes in moves it a few - so the mean
    sits well above the middle of the distribution and a contrast tested against it
    comes out 62/38 or worse however the pair agrees. Measured that way, none of
    the 28 two-band contrasts on t1 was within 0.32 of 50/50, and there was nothing
    to pair.

    A tracker that moves a constant `step` toward x every frame, rather than a
    proportion of the distance, converges to the median: it drifts up while fewer
    than half the frames are above it and back down when more are. So the bit is
    balanced by construction, over whatever window the step can cross, and it is a
    compare and an add on the M0+ - cheaper than the EMA it replaces, and with no
    shift to deadband on. The time constant is len(x)*step over the range of x.
    """
    ref = x[0]
    out = np.empty(len(x), dtype=bool)
    for i in range(len(x)):
        v = x[i]
        out[i] = v > ref
        ref += step if v > ref else -step
    return out


def quadrant(ca, cb):
    return ((ca > 0).astype(np.int64) << 1) | (cb > 0).astype(np.int64)


def score(a, b, warm=0):
    a, b = a[warm:], b[warm:]
    po = float(np.mean(a == b))
    pe = sum(np.mean(a == k) * np.mean(b == k) for k in range(NUM_PATTERNS))
    kap = (po - pe) / (1 - pe) if pe < 1 else 0.0
    occ = [float(np.mean(a == k)) for k in range(NUM_PATTERNS)] + \
          [float(np.mean(b == k)) for k in range(NUM_PATTERNS)]
    mo = min(occ)
    # Mean run length, in seconds: a rule that agrees 80% of the time while
    # changing colour every frame is not something a person can see.
    hold = []
    for p in (a, b):
        e = np.concatenate(([0], np.flatnonzero(np.diff(p) != 0) + 1, [len(p)]))
        hold.append(np.mean(np.diff(e)) / FPS)
    return po, kap, mo, occ, hold


def bits_independent(pa):
    """Normalised (Cramer's V) dependence between the two bits inside one device,
    0 when the quadrants are genuinely four and 1 when they have collapsed onto a
    diagonal. Without this a collapsed pair scores as a success."""
    x = (pa >> 1) & 1
    y = pa & 1
    p = np.zeros((2, 2))
    for i in (0, 1):
        for j in (0, 1):
            p[i, j] = np.mean((x == i) & (y == j))
    px, py = p.sum(1, keepdims=True), p.sum(0, keepdims=True)
    exp = px @ py
    with np.errstate(divide="ignore", invalid="ignore"):
        chi = np.nansum(np.where(exp > 0, (p - exp) ** 2 / exp, 0.0))
    n = len(pa)
    return float(np.sqrt(max(0.0, chi / (chi + n)))) if (chi + n) > 0 else 0.0


def load(path):
    t, b = [], []
    with open(path) as fh:
        for r in csv.DictReader(fh):
            t.append(float(r["t"]))
            b.append([float(r[f"b{i}"]) for i in range(NB)])
    t = np.asarray(t)
    o = np.argsort(t)
    return t[o], np.asarray(b, dtype=np.float64)[o]


def grid(data, sides=("L", "R")):
    ts, bs = zip(*(data[s] for s in sides))
    lo, hi = max(x[0] for x in ts), min(x[-1] for x in ts)
    tg = np.arange(lo, hi, 1.0 / FPS)
    return [b[np.clip(np.searchsorted(t, tg), 0, len(t) - 1)][:len(tg)]
            for t, b in zip(ts, bs)]


def main():
    if len(sys.argv) < 2:
        raise SystemExit(__doc__)
    for d in sys.argv[1:]:
        data = {}
        for side in ("L", "R"):
            p = os.path.join(d, f"state_{side}.csv")
            if not os.path.exists(p):
                raise SystemExit(f"{p}: missing")
            data[side] = load(p)
        GL, GR = grid(data)
        n = len(GL)
        if n <= WARMUP + 100:
            raise SystemExit(f"{d}: too short to split")
        print(f"\n=== {d}: {(n-WARMUP)/FPS/60:.1f} min scored")
        DL, DR = dev(GL), dev(GR)

        # Every zero-sum 2-band contrast. The tracker step sets the reference's
        # time constant: over T frames it can travel T*step, so step 2 with the
        # ~1500-count range of a contrast tracks roughly a 2 minute median.
        names, XL, XR = [], [], []
        for i in range(NB):
            for j in range(NB):
                if i >= j:
                    continue
                names.append(f"b{j}-b{i}")   # low index is high frequency
                XL.append(contrast(DL, j, i))
                XR.append(contrast(DR, j, i))
        step = 2
        SL = np.stack([medbit(c, step) for c in XL], axis=1)
        SR = np.stack([medbit(c, step) for c in XR], axis=1)

        # Per-contrast: how often the two devices put it on the same side, and
        # how balanced that side is. A contrast that is always + scores 100% here
        # and is worthless, which is why balance is printed next to it.
        m = slice(WARMUP, n)
        agree1 = np.mean(SL[m] == SR[m], axis=0)
        bal = np.minimum(np.mean(SL[m], axis=0), np.mean(SR[m], axis=0))
        bal = np.minimum(bal, 1 - bal)
        print(f"  median tracker step {step}: best single contrasts "
              f"(sign agreement / how near 50/50):")
        order = np.argsort(-agree1)
        for k in order[:8]:
            print(f"    {names[k]:<10} agree {agree1[k]*100:5.1f}%  "
                  f"balance {bal[k]*100:5.1f}%")

        # Pick the pair on the first half, score it on the second.
        h0, h1 = WARMUP, WARMUP + (n - WARMUP) // 2
        h2 = n
        ag_a = np.mean(SL[h0:h1] == SR[h0:h1], axis=0)
        bal_a = np.minimum(np.mean(SL[h0:h1], axis=0), np.mean(SR[h0:h1], axis=0))
        bal_a = np.minimum(bal_a, 1 - bal_a)
        usable = np.where(bal_a > 0.40)[0]      # near 50/50 on the fit half
        if len(usable) < 2:
            print("  no contrast is balanced on the fit half; nothing to pick")
            return
        best = []
        for x in range(len(usable)):
            for y in range(x + 1, len(usable)):
                i, j = usable[x], usable[y]
                pa = quadrant(SL[i][h0:h1], SL[j][h0:h1])
                # Reject a pair that is really one feature: the quadrants must be
                # four quadrants on the fit half.
                occ = [np.mean(pa == k) for k in range(4)]
                if min(occ) < 0.12:
                    continue
                q = score(pa, quadrant(SR[i][h0:h1], SR[j][h0:h1]))[1]
                best.append((q, i, j))
        best.sort(reverse=True)
        if not best:
            print("  no contrast pair keeps four quadrants alive on the fit half")
            return
        print(f"  {len(usable)} usable contrasts, "
              f"{len(best)} pairs keep four quadrants alive")
        print("  top pairs, scored on the HELD-OUT half:")
        print(f"    {'pair':<24} {'fit k':>7} {'test agree':>10} {'kappa':>7} "
              f"{'minocc':>7} {'hold s':>11} {'dep':>5}")
        shown = 0
        for kfit, i, j in best:
            pl = quadrant(SL[i][h1:h2], SL[j][h1:h2])
            pr = quadrant(SR[i][h1:h2], SR[j][h1:h2])
            po, kap, mo, occ, hd = score(pl, pr)
            dep = max(bits_independent(pl), bits_independent(pr))
            lbl = f"{names[i]} x {names[j]}"
            flag = "" if mo >= 0.12 else "  <- quadrant starved"
            print(f"    {lbl:<24} {kfit:+7.3f} {po*100:9.1f}% {kap:+7.3f} "
                  f"{mo*100:6.1f}% {hd[0]:5.1f}/{hd[1]:<5.1f} {dep:5.2f}{flag}")
            shown += 1
            if shown >= 10:
                break
        # The reference a single scalar gets on the same held-out split, so the
        # gain (if any) from the second feature is visible.
        k = int(np.argmax(ag_a * (bal_a > 0.40)))
        po, kap, mo, occ, hd = score(SL[k][h1:h2], SR[k][h1:h2])
        print(f"    one feature only ({names[k]}): 2 colours, "
              f"agree {po*100:.1f}%  minocc {mo*100:.1f}%")


if __name__ == "__main__":
    main()
