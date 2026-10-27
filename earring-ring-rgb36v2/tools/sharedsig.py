#!/usr/bin/env python3
"""What do the two earrings actually agree on? Split the shared signal in two.

Two earrings wear the same song but hear it through different heads in different
places, so almost nothing in their spectra matches. The one thing they truly
share is the MUSIC ITSELF: the beat arrives at both ears at the same instant, a
drop happens at the same second for both. A rule whose output depends on WHEN
can agree where a rule built out of spectral shape cannot, and this matters for
master specifically because master's rule is entirely spectral - argmax over the
bands that stand above a line fit - and measures at chance.

Two files per device are used, both produced by the debug-branch tap:

  L.csv / R.csv    per-frame events: hash, onset, beat, bar, anchor, pchange,
                   locked, plus the pattern shown, beat phase and period.
  state_*.csv      per-frame bands b0..b7, which is also master's input.

What is measured:

  temporal  onset / beat / bar / anchor coincidence, tolerance +-1, +-3, +-8
            frames (16, 48, 128 ms), scored against the union of the two event
            sets: if one device misses half the beats that IS a disagreement for
            a rule keyed to them, however neatly the survivors line up. The
            signed skew is printed too, because a rule that COUNTS events is
            destroyed by a systematic one-beat offset that a coincidence rate
            hides completely.
  phase     the two beat-phase counters, in fractions of a beat, while both are
            locked. Same reason: a shared clock running at the right speed but
            reading a different number is not a shared clock.
  spectral  correlation of the two band vectors and the agreement of the band
            argmax. This is the ceiling on any rule master could build out of
            what it already computes, no matter how clever.

  python3 sharedsig.py ~/psync/t1 ~/psync/newfull
"""
import csv
import os
import sys

import numpy as np

NB = 8
FPS = 62.5
FRAME = 1.0 / FPS


def rows(path, kind=None):
    """Read a telemetry CSV whose header may be split over two lines.

    L.csv names fourteen columns and continues with thirteen more on a second
    line whose first field is empty; state_*.csv has an ordinary one-line
    header. Joining them is harmless either way.
    """
    with open(path) as fh:
        rd = csv.reader(fh)
        names = next(rd)
        nxt = next(rd)
        if nxt and not nxt[0]:
            names = [a if a else b for a, b in zip(names, nxt)]
            body = list(rd)
        else:
            body = [nxt] + list(rd)
    body = [r for r in body if r and (kind is None or r[0] == kind)]
    return {n: [r[i] if len(r) > i else "" for r in body]
            for i, n in enumerate(names)}


def truth(col):
    return np.array([str(v).strip() in ("True", "true", "1") for v in col])


def num(col):
    out = []
    for x in col:
        try:
            out.append(int(float(x)))
        except (TypeError, ValueError):
            out.append(-1)
    return np.array(out, dtype=np.int64)


def times(col):
    out = []
    for x in col:
        try:
            out.append(float(x))
        except (TypeError, ValueError):
            out.append(np.nan)
    return np.array(out)


def finite(t):
    m = np.isfinite(t)
    return t[m], m


def grid(tl, tr):
    """Nearest-frame alignment of two streams onto one 62.5 Hz grid."""
    lo = max(tl[0], tr[0])
    hi = min(tl[-1], tr[-1])
    tg = np.arange(lo, hi, FRAME)
    il = np.clip(np.searchsorted(tl, tg), 0, len(tl) - 1)
    ir = np.clip(np.searchsorted(tr, tg), 0, len(tr) - 1)
    ok = (np.abs(tl[il] - tg) <= 1.5 * FRAME) & \
         (np.abs(tr[ir] - tg) <= 1.5 * FRAME)
    return il, ir, ok, tg


def coinc(fl, fr, ok, tol):
    """Jitter-tolerant event agreement against the union, plus signed skew."""
    el = np.flatnonzero(fl & ok)
    er = np.flatnonzero(fr & ok)
    if len(el) == 0 or len(er) == 0:
        return float("nan"), float("nan")
    dm = el[:, None] - er[None, :]
    dist = np.abs(dm).min(axis=1)
    back = np.abs(dm).min(axis=0)
    rate = (np.count_nonzero(dist <= tol) + np.count_nonzero(back <= tol)) / \
        (len(el) + len(er))
    j = np.abs(dm).argmin(axis=1)
    m = dist <= tol
    skew = float(np.median(dm[np.arange(len(el)), j][m])) if np.any(m) else -1.0
    return rate, skew


def load(path):
    ev, st = {}, {}
    for s in "LR":
        d = rows(os.path.join(path, f"{s}.csv"), kind="T")
        t = times(d.get("t", []))
        keep = np.isfinite(t)
        t = t[keep]
        o = np.argsort(t, kind="stable")
        ev[s] = (t[o], {k: np.array(v)[keep][o] for k, v in d.items()})
        d = rows(os.path.join(path, f"state_{s}.csv"))
        t = times(d.get("t", []))
        keep = np.isfinite(t)
        t = t[keep]
        o = np.argsort(t, kind="stable")
        st[s] = (t[o], {k: np.array(v)[keep][o] for k, v in d.items()})
    return ev, st


def pct(n, d):
    return 100.0 * n / d if d else float("nan")


def main():
    for d in sys.argv[1:]:
        ev, st = load(d)
        tl = ev["L"][0]
        tr = ev["R"][0]
        il, ir, ok, tg = grid(tl, tr)
        print(f"\n=== {d}: {len(tg)/FPS/60:.1f} min, "
              f"{pct(np.count_nonzero(ok), len(tg)):.0f}% of grid frames paired")

        eL, eR = ev["L"][1], ev["R"][1]
        for name in ("onset", "beat", "bar", "anchor", "pchange"):
            if name not in eL or name not in eR:
                continue
            fl = truth(eL[name])[il]
            fr = truth(eR[name])[ir]
            cells = []
            for tol in (1, 3, 8):
                r, skew = coinc(fl, fr, ok, tol)
                cells.append(f"+-{tol}f {r*100:5.1f}%")
            _, skew = coinc(fl, fr, ok, 3)
            print(f"  {name:>8} L{np.count_nonzero(fl & ok):7d} "
                  f"R{np.count_nonzero(fr & ok):7d}  " + "  ".join(cells) +
                  f"   skew {skew*FRAME*1000:+6.0f} ms")

        pl, pr = num(eL.get("phase", []))[il], num(eR.get("phase", []))[ir]
        per = num(eL.get("period", []))[il]
        lkL = truth(eL.get("locked", []))[il]
        lkR = truth(eR.get("locked", []))[ir]
        lk = lkL & lkR & ok
        print(f"  beat lock: both {pct(np.count_nonzero(lk), len(ok)):5.1f}%  "
              f"L only {pct(np.count_nonzero(lkL & ~lkR & ok), len(ok)):5.1f}%  "
              f"R only {pct(np.count_nonzero(~lkL & lkR & ok), len(ok)):5.1f}%")
        good = lk & (pl >= 0) & (pr >= 0) & (per > 0)
        if np.count_nonzero(good) > 200:
            f = (pl[good] % per[good]) / per[good] - \
                (pr[good] % per[good]) / per[good]
            f = np.abs((f + 0.5) % 1.0 - 0.5)
            print("  phase offset, both locked: " +
                  "  ".join(f"p{p} {np.percentile(f, p)*100:3.0f}% beat"
                            for p in (50, 75, 90)))

        if "pattern" in eL and "pattern" in eR:
            al, ar = num(eL["pattern"])[il], num(eR["pattern"])[ir]
            v = ok & (al >= 0) & (ar >= 0)
            a, b = al[v], ar[v]
            npat = int(max(a.max(), b.max())) + 1
            po = float(np.mean(a == b))
            pe = sum(float(np.mean(a == k)) * float(np.mean(b == k))
                     for k in range(npat))
            k = (po - pe) / (1 - pe) if pe < 1 else 0.0
            print(f"  pattern shown: {npat} seen  agreement {po*100:5.1f}%  "
                  f"kappa {k:+.3f}")
            for nm, p in (("L", a), ("R", b)):
                print("     occ " + nm + " " +
                      " ".join(f"{np.mean(p == i)*100:4.1f}"
                               for i in range(npat)))

        sl, sL = st["L"]
        sr, sR = st["R"]
        if "b0" not in sL or "b0" not in sR:
            print("  (no b0..b7 in this capture: spectral ceiling skipped)")
            continue
        jl, jr, jog, _ = grid(sl, sr)
        Bl = np.array([[int(float(x)) for x in r]
                       for r in zip(*[sL[f"b{i}"] for i in range(NB)])],
                      dtype=np.float64)[jl]
        Br = np.array([[int(float(x)) for x in r]
                       for r in zip(*[sR[f"b{i}"] for i in range(NB)])],
                      dtype=np.float64)[jr]
        v = jog & (Bl.sum(axis=1) > 0) & (Br.sum(axis=1) > 0)
        cl = Bl[v] - Bl[v].mean(axis=1, keepdims=True)
        cr = Br[v] - Br[v].mean(axis=1, keepdims=True)
        den = np.sqrt((cl ** 2).sum(axis=1) * (cr ** 2).sum(axis=1)) + 1e-9
        c = (cl * cr).sum(axis=1) / den
        print(f"  band shape corr(L,R): mean {c.mean():+.3f}  "
              f"p50 {np.percentile(c,50):+.3f}  p10 {np.percentile(c,10):+.3f}")
        al = cl[:, 1:NB - 1].argmax(axis=1)
        ar = cr[:, 1:NB - 1].argmax(axis=1)
        po = float(np.mean(al == ar))
        pe = sum(float(np.mean(al == k)) * float(np.mean(ar == k))
                 for k in range(NB - 2))
        print(f"  band argmax: agreement {po*100:5.1f}%  kappa "
              f"{(po-pe)/(1-pe) if pe < 1 else 0:+.3f}  "
              f"chance {pct(1, NB-2):4.1f}%  <- ceiling for any rule that asks "
              "which band stands out")


if __name__ == "__main__":
    main()
