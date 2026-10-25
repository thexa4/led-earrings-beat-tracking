#!/usr/bin/env python3
"""One shared scalar, cut into k equal-frequency buckets.

Why argmax loses, and what replaces it
--------------------------------------
Picking the band with the largest deviation agrees well between devices (the
deviation correlates at 0.8-0.9 with 2-4 s of smoothing) but the colour it selects
is badly uneven: on EDM the bass band is almost always the most anomalous, so one
colour owns two thirds of the time and another never appears. Normalising each
band's variance evens the colours out and destroys the agreement, because it
amplifies exactly the bands that correlate least. Both halves of that result are
properties of argmax, not of the signal.

argmax also throws away most of the information: it keeps the identity of the
winner and discards the magnitudes of the other seven, so a close call and a
landslide look the same. A single scalar keeps everything, and because it is a
weighted sum of eight measurements it averages their noise down -- the same reason
averaging sensors helps.

So: reduce the smoothed, AGC-cancelled, tilt-corrected spectrum to one number, then
cut that number into k buckets placed at equal *frequencies* of the device's own
distribution. Equal-frequency cuts make the colours share the time by construction,
which is a requirement here and not something to hope a threshold achieves. The
agreement then depends only on how well the scalar correlates, which is measurable
directly and is much higher than any argmax.

Weights
-------
index   spectral centroid: brightness, the physically meaningful axis of music.
        Signed so a larger value means more treble-weighted.
flat    sum of deviations: "how anomalous is the spectrum overall".
pc      the direction along which the two devices agree most, found from the data.
        Reported as the ceiling. It is fitted on the capture being scored, so its
        number is optimistic; treat it as an upper bound, not a candidate, unless a
        third capture confirms it.
top4    index restricted to the four best-correlating bands.

Thresholds are expressed in units of each device's own spread, so mic sensitivity,
distance and AGC operating point drop out.

  python3 tools/sharedscalar.py DIR1 [DIR2 ...] [--k 4] [--taus 2,4,8]
"""
import argparse
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
    s = lambda x: np.stack([x[f"snap{k}"] / 256.0 for k in range(NB)], axis=1)
    yL, yR = s(d[0]), np.stack([np.interp(tL, tR, d[1][f"snap{k}"] / 256.0)
                                for k in range(NB)], axis=1)
    yL, yR = yL[keep], yR[keep]
    # AGC gain is one constant across all bands in the log domain, so subtracting
    # the across-band mean removes it exactly, with no model of the AGC.
    return yL - yL.mean(axis=1, keepdims=True), \
        yR - yR.mean(axis=1, keepdims=True), tL[keep]


def scalarize(s, w):
    """Project a band matrix onto one direction."""
    return s @ w


def buckets(z, k):
    """Colour index from equal-frequency cuts on the device's own distribution.

    Cut points are the device's own quantiles, which is non-causal; the units are
    its own standard deviation, so the causal version only has to estimate that
    spread slowly and can reuse the thresholds. Both are reported.
    """
    q = np.quantile(z, np.linspace(0, 1, k + 1)[1:-1])
    return np.clip(np.searchsorted(q, z), 0, k - 1)


def zbuckets(z, k):
    zs = (z - z.mean()) / max(z.std(), 1e-9)
    # Equal-width cuts on a standardised variable are equal-frequency only if the
    # variable is Gaussian; the occupancy columns show whether it is.
    e = np.linspace(-0.6745, 0.6745, k - 1) if k > 1 else []
    return np.clip(np.searchsorted(e, zs), 0, k - 1)


def score(cL, cR, k, dt):
    agree = float((cL == cR).mean())
    pL = np.array([(cL == c).mean() for c in range(k)])
    pR = np.array([(cR == c).mean() for c in range(k)])
    ch = float((pL * pR).sum())
    kap = (agree - ch) / (1 - ch) if ch < 1 else 0.0
    n = int((cL[1:] != cL[:-1]).sum())
    # Occupancy imbalance: 0 is perfectly even, 1 is all one colour.
    imb = 1 - (np.append(pL, pR) * 0.5).sum() / (1 / k) if k else 0
    imb = float(max(np.abs(pL - 1 / k).sum(), np.abs(pR - 1 / k).sum()) / 2)
    return agree, kap, pL, pR, 60 * n / dt, imb


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("dirs", nargs="+")
    ap.add_argument("--k", type=int, default=4)
    ap.add_argument("--taus", default="1,2,4,8")
    a = ap.parse_args()
    taus = [float(t) for t in a.taus.split(",")]

    for d in a.dirs:
        sL, sR, t = load(d)
        dt = t[-1] - t[0]
        print(f"\n########## {d}  ({len(t)} frames, {dt:.0f} s), k={a.k}")
        # Per-band correlation, needed to build the restricted and fitted weights.
        per = {}
        for tau in taus:
            aL = ema(sL, tau) - ema(sL, tau).mean(axis=0, keepdims=True)
            aR = ema(sR, tau) - ema(sR, tau).mean(axis=0, keepdims=True)
            per[tau] = (aL, aR, np.array([np.corrcoef(aL[:, j], aR[:, j])[0, 1]
                                          for j in range(NB)]))
        windex = np.arange(NB, dtype=float) - (NB - 1) / 2  # brightness axis
        print(f"  {'tau':>5} {'weight':>7} {'r(scalar)':>10} {'agree':>7} "
              f"{'kappa':>7} {'chg/min':>8} {'imb':>5}  occupancy L / R")
        for tau in taus:
            aL, aR, rb = per[tau]
            best4 = np.argsort(rb)[::-1][:4]
            # Fitted direction: first cross-covariance component, by power iteration.
            C = (aL.T @ aR) / len(aL)
            w = np.ones(NB)
            for _ in range(40):
                w = C @ w
                w /= max(np.abs(w).max(), 1e-12)
            weights = {
                "index": windex,
                "flat": np.ones(NB),
                "top4": np.where(np.isin(np.arange(NB), best4), windex, 0.0),
                "pc": w,
            }
            for nm, w in weights.items():
                zl = scalarize(aL, w)
                zr = scalarize(aR, w)
                r = np.corrcoef(zl, zr)[0, 1]
                for tag, fn in (("q", buckets), ("z", zbuckets)):
                    cL = fn(zl, a.k)
                    cR = fn(zr, a.k)
                    ag, ka, pL, pR, chg, imb = score(cL, cR, a.k, dt)
                    print(f"  {tau:5.1f} {nm + tag:>7} {r:+10.3f} "
                          f"{100 * ag:6.1f}% {ka:+7.3f} {chg:8.1f} {imb:5.2f}  "
                          f"{'/'.join(f'{v:.2f}' for v in pL)} / "
                          f"{'/'.join(f'{v:.2f}' for v in pR)}")
    print("\nimb is occupancy imbalance: 0 = colours share the time evenly, "
          "1 = stuck.\nLook for a row with kappa high AND imb low. r(scalar) is "
          "the ceiling for\nthat row's agreement; if r is high but kappa is not, "
          "the buckets are misplaced.")


main() if __name__ == "__main__" else None
