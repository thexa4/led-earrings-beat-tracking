#!/usr/bin/env python3
"""Can colour come from loudness instead of from which band is loudest?

Why
---
Everything that picks a band has failed the same way: chance-level agreement for
every grouping, every time constant, every candidate band set. The reason is
visible in a gated-tone capture. The *total* level answers the gate with a huge
margin on both devices while the *band pattern* answers with a flat pedestal,
because a gain change and a gate click both move every band by roughly the same
amount. So the two earrings demonstrably share the envelope and demonstrably do
not share the spectral shape. Choosing a colour by argmax over bands throws away
the shared signal to keep the unshared one.

The feature to use is therefore the envelope itself: how loud this device's input
is right now relative to how loud it has been. A per-device reference removes mic
sensitivity, distance to the speaker, and the AGC's operating point, all of which
differ per device and none of which the music controls. Quantising that deviation
into k buckets then gives colours that are evenly spread by construction -- the
buckets are defined as fractions of the device's own distribution -- and which
cannot sit on one value forever, because the music's own dynamics move it.

Causality
---------
Quantiles over the whole capture are not available to a device. Two causal
stand-ins are reported:

  z-score   bucket on (e - mu) / sd with mu, sd slow running estimates. Even
            spread if the deviation is roughly symmetric.
  hysteresis-free linear: map through tanh, then equal-width buckets. Robust to
            outliers, so a single loud passage cannot pin the colour.

The non-causal own-quantile version is printed as the ceiling: if agreement is
poor even there, the envelope is not shared well enough either and this idea is
dead. If it is good, the gap between it and the causal version is the cost of
running estimate, and is worth measuring rather than guessing.

  python3 tools/levelcolor.py DIR1 [DIR2 ...] [--k 4]
"""
import argparse
import os
import sys

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import specdump  # noqa: E402

NB = 8
RATE = 16000.0 / 250  # pipeline frames per second


def load_level(dir_, tol=0.08, warmup=120):
    """log2 total input energy per device, resampled onto L's clock."""
    d = [specdump.load(os.path.join(dir_, f"spec-{s}.csv")) for s in ("L", "R")]
    tL, tR = d[0]["t"], d[1]["t"]
    idx = np.clip(np.searchsorted(tR, tL), 1, max(len(tR) - 1, 1))
    near = np.minimum(np.abs(tR[idx] - tL), np.abs(tR[idx - 1] - tL))
    keep = near <= tol
    keep[:warmup] = False

    def lvl(d):
        # sum() over bands in the linear domain, then log2: one number per frame.
        # The band set is the same on both sides, so a per-device constant gain
        # drops out when each is compared against its own reference.
        tot = sum(d[f"raw{i}"].astype(np.float64) for i in range(NB)) + 1.0
        return np.log2(tot)
    yL = lvl(d[0])[keep]
    yR = np.interp(tL[keep], tR, lvl(d[1]))
    return yL, yR, tL[keep]


def ema(y, tau_s):
    alpha = 1.0 / max(tau_s * RATE, 1.0)
    out = np.empty_like(y)
    acc = y[0]
    for i in range(len(y)):
        acc = acc + alpha * (y[i] - acc)
        out[i] = acc
    return out


def run_stats(c, rate=RATE):
    d = np.diff(np.flatnonzero(np.concatenate([[True], c[1:] != c[:-1], [True]])))
    med = float(np.median(d)) / rate if len(d) else 0.0
    ch = int((c[1:] != c[:-1]).sum())
    return med, ch


def score(cL, cR, k):
    agree = float((cL == cR).mean())
    pL = np.array([(cL == c).mean() for c in range(k)])
    pR = np.array([(cR == c).mean() for c in range(k)])
    chance = float((pL * pR).sum())
    kap = (agree - chance) / (1 - chance) if chance < 1 else 0.0
    return agree, kap, pL, pR


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("dirs", nargs="+")
    ap.add_argument("--k", type=int, default=4)
    ap.add_argument("--taus", default="0.5,1,2,4")
    a = ap.parse_args()

    taus = [float(t) for t in a.taus.split(",")]
    for d in a.dirs:
        yL, yR, t = load_level(d)
        data = (yL, yR, t)
        print(f"\n########## {d}  ({len(t)} aligned frames, "
              f"{t[-1] - t[0]:.0f} s)")
        # How shared is the envelope at all? This is the premise, so check it.
        print("  envelope correlation between devices, after removing each "
              "device's own mean:")
        for tau in taus:
            eL = ema(data[0], tau) - data[0].mean()
            eR = ema(data[1], tau) - data[1].mean()
            r = np.corrcoef(eL, eR)[0, 1]
            # Zero-lag is the only honest lag here; both traces share a clock.
            print(f"    smoothing {tau:4.1f}s: r = {r:+.3f}")
        eL = ema(data[0], 1.0) - data[0].mean()
        eR = ema(data[1], 1.0) - data[1].mean()
        print(f"  L deviation range {eL.min():+.1f}..{eL.max():+.1f} doublings, "
              f"R {eR.min():+.1f}..{eR.max():+.1f}")

        print(f"\n  colour agreement from level, k={a.k}:")
        print(f"    {'scheme':>22} {'agree':>7} {'kappa':>7} {'chg/min':>8} "
              f"{'L occ':>20} {'R occ':>20} {'dwell':>10}")
        for tau in taus:
            eL = ema(data[0], tau)
            eR = ema(data[1], tau)
            schemes = {}
            # ceiling: each device's own quantiles over the whole capture
            qL = np.quantile(eL, np.linspace(0, 1, a.k + 1)[1:-1])
            qR = np.quantile(eR, np.linspace(0, 1, a.k + 1)[1:-1])
            schemes[f"own-quantiles {tau:g}s"] = (
                np.searchsorted(qL, eL), np.searchsorted(qR, eR))
            # causal stand-in: z-score against a slow running mean and spread
            mu_l = ema(eL, 30.0)
            mu_r = ema(eR, 30.0)
            sd_l = ema(np.abs(eL - mu_l), 30.0) * 1.2533
            sd_r = ema(np.abs(eR - mu_r), 30.0) * 1.2533
            zl = (eL - mu_l) / np.maximum(sd_l, 1e-6)
            zr = (eR - mu_r) / np.maximum(sd_r, 1e-6)
            edges = np.linspace(-1.15, 1.15, a.k - 1)
            schemes[f"z-score 30s ref {tau:g}s"] = (
                np.clip(np.searchsorted(edges, zl), 0, a.k - 1),
                np.clip(np.searchsorted(edges, zr), 0, a.k - 1))
            # robust version: tanh compresses outliers so one loud passage
            # cannot own a bucket and starve the others
            tl = np.tanh(zl / 1.5)
            tr = np.tanh(zr / 1.5)
            schemes[f"tanh(z) 30s ref {tau:g}s"] = (
                np.clip(np.searchsorted(edges / 1.15, tl), 0, a.k - 1),
                np.clip(np.searchsorted(edges / 1.15, tr), 0, a.k - 1))
            for nm, (cL, cR) in schemes.items():
                ag, ka, pL, pR = score(cL, cR, a.k)
                med, ch = run_stats(cL)
                print(f"    {nm:>22} {100 * ag:6.1f}% {ka:+7.3f} "
                      f"{60 * ch / (t[-1] - t[0]):8.1f} "
                      f"{' '.join(f'{v:.2f}' for v in pL):>20} "
                      f"{' '.join(f'{v:.2f}' for v in pR):>20} "
                      f"{med:9.1f}s")

    print("\nRead: r is the premise. If the envelope correlation is high but "
          "kappa is not,")
    print("the buckets are mis-scaled, not the signal missing. Even occupancy "
          "near 1/k and")
    print("a dwell of a few seconds are the stated requirements, not decoration "
          "-- a scheme")
    print("with high agreement and one occupied bucket is the stuck case.")


main() if __name__ == "__main__" else None
