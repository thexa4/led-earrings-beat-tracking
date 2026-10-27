#!/usr/bin/env python3
"""Rate-limited re-decision instead of hysteresis: the fix that fits in 64 bytes.

Measured on the bench today, with the two earrings actually hearing the same
music (tools/paircheck.py on ~/psync/live1: band shape correlates +0.65..+0.87
across the two devices once level is removed, and each device tracks the played
file):

    master's per-frame argmax    agree 53%  kappa +0.30  41 changes/min  hold 0.8 s
    the same, smoothed 0.2 s     agree 88%  kappa +0.00  one band per device

So the selection is informative and the smoothing is what destroys it. Reading
the same two lines again, what kills it is not the smoothing but what smoothing
does to the MARGIN: master's hysteresis is an absolute 32<<8, the deviations
shrink as the spectrum is averaged, and once they fall below the margin no
challenger can ever win - the incumbent band is held for the rest of the song at
100% agreement, which is two earrings frozen, not two earrings agreeing. Every
earlier "long averaging gives 100% agreement" row in this project was that.

Two ways out, and only one of them costs memory:

  relative margin   challenger must lead by a FRACTION of the spread across the
                    bands, so the rule is scale-free. Costs a max and a min over
                    eight values per decision.

  rate limit        no incumbent at all. The spectrum is already a 0.5 s average
                    (SmoothShift = 5), so the argmax taken at any instant IS the
                    answer over the last half second. Simply decide less often -
                    keep the last decision until the next slot - and the flicker
                    goes away without any margin to mis-scale. Costs a counter.

The second is what master can afford, and it is also closer to what the device
already does: patterns are committed at beats and anchors, not every frame. This
measures both against the shipped rule, on real captures, with the timing the
firmware would have - the decision is taken from the average the device has at
that instant, never from a future one.

  python3 blocksel.py ~/psync/live1 [more captures...]
"""
import csv
import os
import sys

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import ema as emalib  # noqa: E402
import mastersel as ms  # noqa: E402  byte-exact spectral_devs, argmax, hysteresis

NB = 8
FPS = 62.5
NP = NB - 1  # master exposes NBands-1 patterns
MARGIN = 32 << 8


def load(path):
    t, b = [], []
    with open(path) as fh:
        for r in csv.DictReader(fh):
            try:
                t.append(float(r["t"]))
                b.append([int(float(r[f"b{i}"])) for i in range(NB)])
            except (KeyError, ValueError):
                continue
    t = np.asarray(t)
    o = np.argsort(t, kind="stable")
    return t[o], np.asarray(b, dtype=np.int64)[o]


def grid(d):
    tl, bl = load(os.path.join(d, "state_L.csv"))
    tr, br = load(os.path.join(d, "state_R.csv"))
    lo, hi = max(tl[0], tr[0]), min(tl[-1], tr[-1])
    tg = np.arange(lo, hi, 1.0 / FPS)
    pick = lambda t, b: b[np.clip(np.searchsorted(t, tg), 0, len(t) - 1)][:len(tg)]
    return pick(tl, bl), pick(tr, br)


def devs_all(G):
    """spectral_devs for every frame, vectorised, same integers as the Go."""
    b = np.arange(NB, dtype=np.int64)
    sy = G.sum(axis=1)
    sxy = (G * b).sum(axis=1)
    num = 8 * sxy - 28 * sy
    neg = num < 0
    u = np.where(neg, -num, num)
    lo = (u & 0xFFFF) * ms.Q16Q
    p = (u >> 16) * ms.Q16Q + (lo >> 16)
    slope = np.where(neg, -p - np.where(lo & 0xFFFF, 1, 0), p)
    intercept = (sy << 5) - ((slope * 7) >> 1)
    return (G << 8) - intercept[:, None] - slope[:, None] * b


def argmax_all(D):
    """Master's argmax over bands 1..NB-2, vectorised."""
    v = D[:, 1:NB - 1]
    return v.argmax(axis=1) + 1


def score(a, b, warm):
    a, b = a[warm:], b[warm:]
    if len(a) < 100:
        return (float("nan"),) * 6
    po = float(np.mean(a == b))
    pe = sum(float(np.mean(a == k)) * float(np.mean(b == k)) for k in range(NP))
    kap = (po - pe) / (1 - pe) if pe < 1 else 0.0
    seen = sorted(set(a.tolist()) | set(b.tolist()))
    # Occupancy is reported over the colours that actually appear. Counting the
    # ones that never appear would make every rule look broken - the question
    # asked here is "does any colour that shows up starve", and a rule that only
    # ever shows two colours is flagged separately by `used`.
    occ = [min(float(np.mean(a == k)), float(np.mean(b == k))) for k in seen] \
        or [0.0]
    ch = [float(np.count_nonzero(np.diff(p) != 0)) / (len(p) / FPS) * 60
          for p in (a, b)]
    hold = []
    for p in (a, b):
        e = np.concatenate(([0], np.flatnonzero(np.diff(p) != 0) + 1, [len(p)]))
        hold.append(float(np.mean(np.diff(e))) / FPS)
    used = len(set(a.tolist()) | set(b.tolist()))
    return po, kap, min(occ), ch, hold, used


def sel_hyst(D, margin, relnum=None):
    """Master's usePattern over precomputed deviations: hold the incumbent."""
    am = argmax_all(D)
    cur, out = 0, np.empty(len(am), dtype=np.int64)
    for i, c in enumerate(am):
        if i == 0:
            cur = c
        elif c != cur:
            spread = (D[i].max() - D[i].min()) if relnum else None
            need = margin if relnum is None else (relnum[0] * spread) // relnum[1]
            if D[i][c] >= D[i][cur] + need:
                cur = c
        out[i] = cur
    return out


def sel_block(D, every, relnum=None, margin=0):
    """Rate-limited re-decision: the argmax stands until the next slot.

    No incumbent, so no margin to mis-scale. `every` frames is the smallest hold
    the rule can produce; inside a slot the device simply keeps showing what it
    chose at the start of it, which is what master already does between beats.
    The optional margin still applies at the slot boundary and is what makes the
    choice sticky against a single frame's noise - but unlike sel_hyst the
    incumbent is dropped by the clock, so nothing can latch forever.
    """
    am = argmax_all(D)
    cur, out = int(am[0]), np.empty(len(am), dtype=np.int64)
    for i in range(len(am)):
        if i % every == 0:
            c = int(am[i])
            if c != cur and margin == 0 and relnum is None:
                cur = c
            elif c != cur:
                spread = int(D[i].max()) - int(D[i].min())
                need = margin if relnum is None else (relnum[0] * spread) // relnum[1]
                if int(D[i][c]) >= int(D[i][cur]) + need:
                    cur = c
        out[i] = cur
    return out


def slow_bands(G, shift):
    """A second, slower spectrum: eight accumulators, which master does not have.

    Included because smoothing measurably helps on real data (+0.31 -> +0.35 for
    two extra seconds of averaging) and the cheapest version of that is worth
    pricing against the free rate limit. On the device this is one int32 array;
    here it is the non-stalling Q8 average from ema.py.
    """
    if not shift:
        return G
    return np.stack([emalib.ema(G[:, b], shift) for b in range(NB)], axis=1)


def sel_block_mean(D, G, every, med=False):
    """Decide from the block's own average rather than from its first frame.

    The device has p.smooth - a 0.5 s EMA - and nothing else, so the first-frame
    version is the one that is free. This is the version a firmware change COULD
    not make without an extra array, included to price the difference: if it is
    small, the free form is good enough.
    """
    n = len(G)
    out = np.zeros(n, dtype=np.int64)
    cur = 0
    for s in range(0, n, every):
        blk = G[s:s + every]
        if med:
            m = np.median(blk, axis=0).astype(np.int64)
        else:
            m = blk.mean(axis=0).astype(np.int64)
        cur = int(argmax_all(devs_all(m[None, :]))[0])
        out[s:s + every] = cur
    return out


def main():
    emalib.self_test()
    caps = []
    for d in sys.argv[1:]:
        GL, GR = grid(d)
        caps.append((d, devs_all(GL), GL, devs_all(GR), GR))
        print(f"loaded {d}: {len(GL)/FPS/60:.1f} min")
    warm = int(30 * FPS)

    cands = [("shipped: hyst abs 32", lambda Dl, G: sel_hyst(Dl, MARGIN))]
    for num, den in ((1, 8), (1, 16), (1, 32)):
        cands.append((f"hyst rel {num}/{den}",
                      lambda Dl, G, n=num, d=den: sel_hyst(Dl, 0, (n, d))))
    for every in (16, 32, 62, 125, 187, 250, 375, 500):
        cands.append((f"every {every/FPS:.2f}s",
                      lambda Dl, G, e=every: sel_block(Dl, e)))
    for every in (62, 125, 250):
        cands.append((f"every {every/FPS:.2f}s rel 1/16",
                      lambda Dl, G, e=every: sel_block(Dl, e, (1, 16))))
    for every in (32, 62, 125, 250):
        cands.append((f"blockmean {every/FPS:.2f}s",
                      lambda Dl, G, e=every: sel_block_mean(Dl, G, e)))
    for every in (62, 125):
        cands.append((f"blockmed  {every/FPS:.2f}s",
                      lambda Dl, G, e=every: sel_block_mean(Dl, G, e, med=True)))
    # smoothing x rate limit. D is recomputed from the slowed spectrum, so the
    # deviations the margin is compared against scale with it - the thing the
    # absolute shipped margin cannot do.
    for sh in (4, 6):
        for every in (62, 125, 250):
            cands.append((f"slow{2**sh/FPS:.1f}s every {every/FPS:.2f}s",
                          lambda Dl, G, s=sh, e=every: sel_block(
                              devs_all(slow_bands(G, s)), e)))
        cands.append((f"slow{2**sh/FPS:.1f}s plain",
                      lambda Dl, G, s=sh: sel_block(
                          devs_all(slow_bands(G, s)), 1)))
        cands.append((f"slow{2**sh/FPS:.1f}s hyst rel 1/16",
                      lambda Dl, G, s=sh: sel_hyst(
                          devs_all(slow_bands(G, s)), 0, (1, 16))))

    hdr = "  ".join(f"{os.path.basename(c[0]):>34}" for c in caps)
    print(f"\n=== master's deviations, different commit rules "
          f"(warm {warm/FPS:.0f} s) ===")
    print(f"  {'rule':<24} {hdr}")
    print(f"  {'':<24} " + "  ".join(f"{'agree  kappa  used minocc  ch/min hold':>34}"
                                     for _ in caps))
    for name, fn in cands:
        cells = []
        for _, DL, GL, DR, GR in caps:
            a = fn(DL, GL)
            b = fn(DR, GR)
            po, kap, mo, ch, hold, used = score(a, b, warm)
            flag = ""
            if used <= 2:
                flag = " FROZEN"
            elif min(ch) < 1.0:
                flag = " slow"
            elif max(ch) > 30:
                flag = " flicker"
            cells.append(f"{po*100:5.1f}% {kap:+6.3f} {used:3d} {mo*100:5.1f}% "
                         f"{min(ch):6.1f} {min(hold):4.1f}{flag}")
        print(f"  {name:<24} " + "  ".join(cells), flush=True)


if __name__ == "__main__":
    main()
