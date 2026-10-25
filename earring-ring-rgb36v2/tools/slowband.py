#!/usr/bin/env python3
"""How much smoothing is needed before two earrings pick the same band?

The question this answers
------------------------
Instantaneous band argmax disagrees between devices at roughly chance. Two causes
look identical in that number and need opposite fixes:

  * the disagreement is *static* -- each mic has its own frequency response and
    head shadow, so one device always reads more treble. No amount of averaging
    fixes this; only per-device normalisation does.
  * the disagreement is *noise* -- both devices see the same spectral event but
    each one's estimate is noisy at short timescales. Averaging fixes this
    exactly, and the fix gets better as the window grows.

Separating them is a single sweep: average the per-band spectrum over an
increasing window and watch agreement. Rising agreement means noise-dominated and
the recipe is "average longer". Flat agreement means static bias and the recipe is
"subtract each device's own average", which is tested here too because a per-band
high-pass does that for free.

Modes
-----
hp   exc = y - EMA_tau(y)          per-band high-pass. Picks the band that is loud
     *relative to that device's own recent average in that band*, so a fixed
     mic/speaker/room tilt cancels. tau is both the smoothing and the debias scale.
lp   x  = EMA_tau(y) - mean_t(y)   per-band low-pass, statically debiased. Answers
     "which band is louder than this device usually hears", the slowest possible
     feature.
none y - mean_bands(y)             no per-band debias at all: raw spectral tilt,
     included to show what the debias is worth.
shape
     The AGC applies one gain to the whole frame, and snap is log-domain, so a
     gain change is the same constant added to every band. Subtracting the
     across-band mean therefore cancels the AGC exactly, with no model of it and
     no dependence on agcShift being readable. Then debias each band against its
     own time average to remove the static tilt, which otherwise makes the bass
     band win forever. This is the only feature that is simultaneously
     AGC-invariant, tilt-free and smoothed:
       s = y - mean_bands(y);  lp: EMA_tau(s) - mean_t(s),  hp: s - EMA_tau(s)

A warning about the agreement number by itself: argmax over the raw spectrum picks
the bass band on every frame, so it reports very high agreement with zero changes
per minute. That is two devices being equally stuck, not two devices agreeing.
Always read agreement together with chg/min and occupancy, or use kappa, which
subtracts the chance rate that a degenerate choice enjoys.

Grouping is applied after selection, so the grouping search and the time-constant
search are orthogonal knobs and are reported separately.

Cross-device time alignment comes from the shared wall clock in the capture, not
from the audio file, so it is unaffected by playback latency.

  python3 tools/slowband.py DIR1 [DIR2 ...] [--modes hp,lp,none] [--k 4]
"""
import argparse
import os
import sys

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import specdump  # noqa: E402

NB = 8
FRAME = 250      # pipeline Window, samples
FS = 16000.0
RATE = FS / FRAME  # 62.5 pipeline frames per second


def ema(y, alpha):
    """One-pole smoothing, alpha = 1/(tau in frames). Vectorised over columns."""
    out = np.empty_like(y)
    acc = y[0].astype(np.float64)
    for i in range(len(y)):
        acc = acc + alpha * (y[i] - acc)
        out[i] = acc
    return out


def load(dir_, tol=0.08, warmup=120):
    """Both sides of one capture, resampled onto L's clock, as log2 band energy."""
    d = [specdump.load(os.path.join(dir_, f"spec-{s}.csv")) for s in ("L", "R")]
    tL, tR = d[0]["t"], d[1]["t"]
    idx = np.clip(np.searchsorted(tR, tL), 1, max(len(tR) - 1, 1))
    near = np.minimum(np.abs(tR[idx] - tL), np.abs(tR[idx - 1] - tL))
    keep = near <= tol
    keep[:warmup] = False
    yL = np.stack([d[0][f"snap{i}"] for i in range(NB)], axis=1)[keep] / 256.0
    yR = np.stack([np.interp(tL[keep], tR, d[1][f"snap{i}"])
                   for i in range(NB)], axis=1)
    return yL, yR, tL[keep]


def feature(y, mode, tau_s):
    """Turn a band-energy matrix into the quantity argmax will run over."""
    if mode == "none":
        return y - y.mean(axis=1, keepdims=True)
    if mode in ("shape", "shapehp", "shapez"):
        # Cancels the AGC gain, which is one constant across all bands here.
        y = y - y.mean(axis=1, keepdims=True)
    alpha = 1.0 / max(tau_s * RATE, 1.0)
    s = ema(y, alpha)
    if mode in ("hp", "shapehp"):
        return y - s
    if mode in ("lp", "shape"):
        return s - y.mean(axis=0, keepdims=True)
    if mode == "shapez":
        # Removing each band's own mean fixes where it sits; dividing by its own
        # spread fixes how far it swings. Bands differ in both, and argmax cares
        # about the second: a band with twice the variance wins twice as often
        # even when both are equally "loud for it". Normalising is what makes the
        # colours share the time between them instead of one owning it.
        d = s - y.mean(axis=0, keepdims=True)
        sd = d.std(axis=0, keepdims=True)
        return d / np.maximum(sd, 1e-6)
    raise ValueError(mode)


def stats(fL, fR, bands, group, k):
    """Agreement, chance-corrected agreement, and how much the choice churns."""
    wL = np.argmax(fL[:, bands], axis=1)
    wR = np.argmax(fR[:, bands], axis=1)
    cL, cR = group[wL], group[wR]
    agree = float((cL == cR).mean())
    pL = np.array([(cL == c).mean() for c in range(k)])
    pR = np.array([(cR == c).mean() for c in range(k)])
    chance = float((pL * pR).sum())
    kappa = (agree - chance) / (1 - chance) if chance < 1 else 0.0
    # Changes per minute of the chosen colour: the "is it stuck / is it strobing"
    # number, which agreement alone cannot see.
    ch = int((cL[1:] != cL[:-1]).sum())
    return agree, kappa, pL, pR, ch


def run_len(c):
    """Median and 90th-percentile colour dwell time in seconds."""
    d = np.diff(np.flatnonzero(np.concatenate([[True], c[1:] != c[:-1], [True]])))
    if len(d) == 0:
        return 0.0, 0.0
    d = d / RATE if len(c) > RATE * 60 else d  # frames, then seconds below
    return float(np.median(d)), float(np.percentile(d, 90))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("dirs", nargs="+")
    ap.add_argument("--taus", default="1,2,4,8,16,32")
    ap.add_argument("--modes", default="shape,shapehp,hp,lp,none")
    ap.add_argument("--k", type=int, default=4)
    ap.add_argument("--bands", default="0-7",
                    help="bands argmax may choose: a range like 1-6, or an "
                         "explicit list like 1,2,3,4")
    a = ap.parse_args()

    taus = [float(t) for t in a.taus.split(",")]
    modes = a.modes.split(",")
    data = {}
    for d in a.dirs:
        yL, yR, t = load(d)
        data[d] = (yL, yR, t)
        print(f"{d}: {len(t)} aligned frames over {t[-1] - t[0]:.0f} s, "
              f"mean frame gap {np.median(np.diff(t)) * 1000:.0f} ms")

    if "-" in a.bands:
        lo, hi = (s.strip() for s in a.bands.split("-"))
        bands = list(range(int(lo), int(hi) + 1))
    else:
        bands = [int(s) for s in a.bands.split(",")]
    # Contiguous groupings over the *usable* bands: a colour should mean a
    # contiguous slice of the spectrum, otherwise "agreeing" says nothing about
    # sounding alike.
    groups = {}
    for k in range(1, 5):
        edges = np.array_split(np.arange(len(bands)), k)
        m = np.zeros(len(bands), int)
        for gi, e in enumerate(edges):
            m[e] = gi
        groups[k] = m

    print("\nPer-band cross-device correlation of the FEATURE (this is what "
          "argmax is trying to agree on):")
    print("  A band correlating well but rarely winning is wasted: that is what "
          "the\n  occupancy columns below show.")
    hdr = "  mode/tau  " + " ".join(f"{'b' + str(b):>6}" for b in range(NB))
    print(hdr)
    for mode in modes:
        for tau in ([0.0] if mode == "none" else taus):
            row = []
            for d in a.dirs:
                yL, yR, _ = data[d]
                fL = feature(yL, mode, tau)
                fR = feature(yR, mode, tau)
                row.append(np.array([np.corrcoef(fL[:, b], fR[:, b])[0, 1]
                                     for b in range(NB)]))
            r = np.mean(row, axis=0)
            tag = f"{mode:5s} {tau:5.1f}s"
            print(f"  {tag}  " + " ".join(f"{c:+6.2f}" for c in r)
                  + f"   | mean {r.mean():+.2f}")

    print(f"\nColour agreement with contiguous grouping, k={a.k}:")
    print(f"  {'mode/tau':>12} {'agree':>7} {'kappa':>7} "
          f"{'chg/min':>8} {'L occupancy':>26} {'R occupancy':>26}")
    for mode in modes:
        for tau in ([0.0] if mode == "none" else taus):
            ag, ka, ch, occL, occR, n = [], [], [], [], [], 0
            for d in a.dirs:
                yL, yR, t = data[d]
                fL = feature(yL, mode, tau)
                fR = feature(yR, mode, tau)
                g = groups[a.k]
                aa, kk, pL, pR, c = stats(fL, fR, bands, g, a.k)
                ag.append(aa); ka.append(kk); ch.append(c)
                occL.append(pL); occR.append(pR)
                n += t[-1] - t[0]
            occL = np.mean(occL, axis=0); occR = np.mean(occR, axis=0)
            tag = f"{mode:5s} {tau:5.1f}s"
            print(f"  {tag:>12} {100 * np.mean(ag):6.1f}% "
                  f"{np.mean(ka):+7.3f} {60 * np.sum(ch) / n:8.1f} "
                  f"{' '.join(f'{v:.2f}' for v in occL):>26} "
                  f"{' '.join(f'{v:.2f}' for v in occR):>26}")

    print("\nInterpretation:")
    print("  agreement rising with tau  -> noise-dominated: average longer.")
    print("  agreement flat in tau, low -> static per-device tilt: needs")
    print("     normalisation, and 'hp' should beat 'none' at equal smoothing.")
    print("  kappa near 0 at every tau  -> the band axis carries no shared")
    print("     information; no grouping or smoothing of it will agree.")
    print("  chg/min near 0 -> stuck; near 30+ -> strobing. The requirement is")
    print("     a few changes per minute with occupancy spread across colours.")


main() if __name__ == "__main__" else None
