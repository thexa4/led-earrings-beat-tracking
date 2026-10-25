#!/usr/bin/env python3
"""Search smoothing scale and band weights for the shared scalar, then cross-validate.

Selection rule
--------------
The colour is a bucket of one scalar: a weighted sum of the smoothed, AGC-cancelled,
tilt-corrected band spectrum. Two knobs, one objective.

The objective is chance-corrected agreement (kappa), not raw agreement. Raw
agreement can be gamed by using fewer effective colours -- a scheme that sits on one
colour scores 100% -- and kappa subtracts the agreement the colour mix would produce
by accident, so a degenerate answer cannot win. Occupancy is printed as well because
kappa alone does not forbid a scheme that uses three of four colours.

Cross-validation is the point of passing more than one capture. Weights fitted on
one capture and scored on the same capture will always look better than they are;
the fitted-once/scored-on-the-other rows are the only ones that predict a change to
the firmware. If a weighting's apparent advantage does not survive the transfer, it
was fitting that capture's noise, and the simpler weighting is the one to ship.

  python3 tools/scalarfit.py FITDIR EVALDIR [MORE ...] [--k 4] [--taus 4,8,16,32]
"""
import argparse
import itertools
import os
import sys

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import specdump  # noqa: E402

NB = 8
RATE = 16000.0 / 250


def ema(y, tau):
    a = 1.0 / max(tau * RATE, 1.0)
    out = np.empty_like(y)
    acc = y[0].astype(np.float64)
    for i in range(len(y)):
        acc = acc + a * (y[i] - acc)
        out[i] = acc
    return out


def load(dir_, tol=0.08, warmup=120):
    d = [specdump.load(os.path.join(dir_, f"spec-{s}.csv")) for s in ("L", "R")]
    tL, tR = d[0]["t"], d[1]["t"]
    i = np.clip(np.searchsorted(tR, tL), 1, max(len(tR) - 1, 1))
    keep = np.minimum(np.abs(tR[i] - tL), np.abs(tR[i - 1] - tL)) <= tol
    keep[:warmup] = False
    y = [np.stack([x[f"snap{k}"] / 256.0 for k in range(NB)], axis=1) for x in d]
    yR = np.stack([np.interp(tL, tR, d[1][f"snap{k}"] / 256.0)
                   for k in range(NB)], axis=1)
    yL, yR = y[0][keep], yR[keep]
    # AGC gain is one constant across bands in the log domain: this difference is
    # exactly gain-free, and subtracting each band's own average removes the
    # per-device tilt that otherwise makes one band win forever.
    f = lambda v: v - v.mean(axis=1, keepdims=True)
    return f(yL), f(yR), tL[keep]


def prepared(sL, sR, tau):
    aL = ema(sL, tau)
    aR = ema(sR, tau)
    return aL - aL.mean(axis=0, keepdims=True), aR - aR.mean(axis=0, keepdims=True)


def cca(aL, aR):
    """Direction maximising correlation of the two projections.

    Weighted by each band's agreement so a band that measures noise cannot pull
    the sum around: this is the generalised-eigenproblem for the cross-covariance,
    solved by iteration rather than by inverting a possibly-singular within-device
    covariance.
    """
    CL = (aL.T @ aL) / len(aL)
    CR = (aR.T @ aR) / len(aR)
    C = (aL.T @ aR) / len(aR)
    w = np.ones(NB)
    for _ in range(200):
        v = C @ w
        v = 0.5 * (v / np.sqrt(np.maximum(np.diag(CL), 1e-9))
                   + (C.T @ (v / np.sqrt(np.maximum(np.diag(CR), 1e-9)))))
        n = np.abs(v).max()
        if n < 1e-12:
            break
        w = v / n
    return w


def bucketise(z, q, margin=0.0):
    """Bucket index, optionally with a margin so a colour must be clearly left.

    Without a margin a value sitting on an edge flickers between two colours on
    frame-to-frame noise, and because the two devices have independent noise they
    flicker out of step. Requiring the value to clear the edge by a margin removes
    that class of disagreement. The margin is in the scalar's own units.

    This is deliberately stateful, which is the opposite of the earlier finding on
    the band argmax, where hysteresis froze an early wrong choice in place. The
    difference is that the state here is anchored to a slowly-moving scalar that
    both devices measure consistently, so a device that falls behind re-converges
    when the scalar moves rather than latching. Whether that reasoning survives the
    data is exactly what the margin sweep reports: if margin 0 wins, ship it
    stateless.
    """
    k = len(q) + 1
    c = np.empty(len(z), dtype=int)
    cur = int(np.clip(np.searchsorted(q, z[0]), 0, k - 1))
    hi = np.append(q, np.inf)
    lo = np.append(-np.inf, q)
    for i, v in enumerate(z):
        if v >= hi[cur] + margin:
            cur = min(cur + 1, k - 1)
        elif v < lo[cur] - margin:
            cur = max(cur - 1, 0)
        c[i] = cur
    return c


def agree(zL, zR, k, margin=0.0):
    """Colour agreement when both sides cut at equal frequencies of their own mix."""
    qL = np.quantile(zL, np.linspace(0, 1, k + 1)[1:-1])
    qR = np.quantile(zR, np.linspace(0, 1, k + 1)[1:-1])
    cL = bucketise(zL, qL, margin)
    cR = bucketise(zR, qR, margin)
    a = float((cL == cR).mean())
    pL = np.array([(cL == c).mean() for c in range(k)])
    pR = np.array([(cR == c).mean() for c in range(k)])
    ch = float((pL * pR).sum())
    kap = (a - ch) / (1 - ch) if ch < 1 else 0.0
    chg = 60 * int((cL[1:] != cL[:-1]).sum())
    return a, kap, pL, chg


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("dirs", nargs="+", help="first is the fit capture")
    ap.add_argument("--k", type=int, default=4)
    ap.add_argument("--taus", default="4,8,16,32")
    ap.add_argument("--minr", type=float, default=0.4,
                    help="drop bands correlating below this before weighting")
    ap.add_argument("--margins", default="0,0.25,0.5",
                    help="hysteresis margin, in scalar standard deviations")
    a = ap.parse_args()
    taus = [float(t) for t in a.taus.split(",")]
    margins = [float(m) for m in a.margins.split(",")]
    fit, evs = a.dirs[0], a.dirs[1:]

    print(f"fit: {fit}   eval: {', '.join(evs) if evs else '(none)'}\n")
    print(f"  {'tau':>5} {'weight':>10} {'bands':>12} {'r':>6} "
          f"{'agree':>7} {'kappa':>7} {'chg/m':>6}  occupancy")
    best = {}
    insample = {}
    for tau in taus:
        sL, sR, t = load(fit)
        aL, aR = prepared(sL, sR, tau)
        rb = np.array([np.corrcoef(aL[:, j], aR[:, j])[0, 1] for j in range(NB)])
        keepb = np.where(rb >= a.minr)[0]
        if len(keepb) < 2:
            keepb = np.argsort(rb)[::-1][:2]
        mask = np.zeros(NB)
        mask[keepb] = 1.0
        cand = {}
        idx = np.arange(NB) - (NB - 1) / 2
        cand["index"] = idx * mask
        cand["flat"] = mask
        w = cca(aL, aR) * mask
        cand["cca"] = w
        # Per-band agreement weighting: trust each band in proportion to how
        # reproducibly both devices measure it.
        cand["agreeW"] = idx * np.clip(rb, 0, None) * mask
        for nm, w in cand.items():
            if np.abs(w).max() < 1e-9:
                continue
            zl = aL @ w
            zr = aR @ w
            r = np.corrcoef(zl, zr)[0, 1]
            sd = 0.5 * (zl.std() + zr.std())
            for m in margins:
                ag, ka, occ, chg = agree(zl, zr, a.k, m * sd)
                print(f"  {tau:5.1f} {nm + f'/m{m:g}':>10} {str(list(keepb)):>12} "
                      f"{r:+6.2f} {100 * ag:6.1f}% {ka:+7.3f} "
                      f"{chg / max(t[-1] - t[0], 1e-9):6.1f} "
                      f" {'/'.join(f'{v:.2f}' for v in occ)}")
                best[(tau, nm, m)] = w.copy()
                insample.setdefault((tau, nm, m), {})["k"] = ka

    if not evs:
        print("\nPass a second capture: the numbers above are in-sample.")
        return
    print("\nCross-validated -- weights from the fit capture, scored on each "
          "eval capture:")
    print(f"  {'tau':>5} {'weight':>12} {'r eval':>7} "
          f"{'agree':>7} {'kappa':>7} {'in-samp':>8} {'gain':>7}  occupancy")
    rows = []
    cache = {}
    for (tau, nm, m), w in best.items():
        for ev in evs:
            if (ev, tau) not in cache:
                sL, sR, t = load(ev)
                cache[(ev, tau)] = prepared(sL, sR, tau) + (t,)
            aL, aR, t = cache[(ev, tau)]
            zl, zr = aL @ w, aR @ w
            re = np.corrcoef(zl, zr)[0, 1]
            ag, ka, occ, chg = agree(zl, zr, a.k, m * 0.5 * (zl.std() + zr.std()))
            rows.append((ka, tau, f"{nm}/m{m:g}", re, ag, insample[(tau, nm, m)]["k"],
                         occ, chg / max(t[-1] - t[0], 1e-9)))
    for ka, tau, nm, re, ag, ka0, occ, cpmin in sorted(rows, reverse=True)[:20]:
        print(f"  {tau:5.1f} {nm:>12} {re:+7.2f} {100 * ag:6.1f}% "
              f"{ka:+7.3f} {ka0:+8.3f} {ka - ka0:+7.3f}  "
              f"{'/'.join(f'{v:.2f}' for v in occ)}  {cpmin:.1f} chg/min")
    print("\nThe top row's kappa, not the fit capture's, is what the firmware "
          "would get.")
    print("If a weight's eval kappa is far below its in-sample kappa, prefer the "
          "row above it with fewer moving parts.")


main() if __name__ == "__main__" else None
