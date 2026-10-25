#!/usr/bin/env python3
"""How well CAN the two earrings agree? Measure the ceiling before tuning a selector.

tools/hearing.py answers "is each earring hearing at all". That is necessary but not
sufficient: two units can each respond to the music and still not hear the *same*
thing, and no amount of cleverness in the pattern selector fixes that. This tool
quantifies the hard ceiling the acoustics impose, so a selector is never blamed for a
number physics already forbade - the mistake that consumed most of one session.

The pattern is derived from sound alone (no radio, see sync.go), so agreement is
bounded by how similar the two microphones' views of the song are. The most
position-robust such view is the total loudness envelope: sum every band and watch it
over time. Two mics anywhere in a room playing the same EDM should track that
tightly (corr well above 0.8). If the total envelope barely correlates, the bench -
not the DSP - is the problem, and per-band agreement will be no better.

Reported per capture:
  total envelope corr   sum(bands) vs time, aligned on the shared wall clock.
  per-band corr         smoothed band energy, b0 (treble) to b7 (bass).
  excitation corr       each band minus its own slow average (what the selector sees).
  argmax agreement      fraction of frames the winner agrees, for a few band sets,
                        an upper bound on the current-style k-way selector.

Captures are aligned on the `t` column, which is a single wall clock shared by both
pollers (both start near t=0 in one process) - NOT by a seq/index lag search, which
pegs at its own limit and fabricates agreement.

  tools/sync_ceiling.py DIR [DIR ...]
  tools/sync_ceiling.py --capture --seconds 40 --file TRACK --outdir DIR

A total-envelope correlation under ~0.6 means the two units are not hearing a common
signal: move them, or fix audio delivery, before reading anything else.
"""
import argparse
import os
import subprocess
import sys

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import specdump  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
RATE = 62.5  # pipeline frame rate; filters are shifts applied once per frame
NB = 8
BANDS = [">4k", "2.5-4k", "1.6-2.5k", "1-1.6k", "630-1k",
         "400-630", "250-400", "<250"]


def shift_ema(x, shift):
    """The device's own leaky average: acc += (x-acc)>>shift, rounded."""
    x = np.asarray(x, dtype=np.int64)
    out = np.empty_like(x)
    acc = x[0]
    for i, v in enumerate(x):
        acc += ((v - acc) + (1 << (shift - 1))) >> shift
        out[i] = acc
    return out


def load_side(d):
    return [d[f"snap{i}"] for i in range(NB)]


def on_common(dL, dR):
    """Resample both sides onto a shared wall-clock grid at the frame rate."""
    tL, tR = dL["t"], dR["t"]
    g = np.arange(max(tL.min(), tR.min()), min(tL.max(), tR.max()), 1 / RATE)
    A = [np.interp(g, tL, a) for a in load_side(dL)]
    B = [np.interp(g, tR, b) for b in load_side(dR)]
    return g, A, B


def corr(a, b):
    a = a - a.mean()
    b = b - b.mean()
    d = np.sqrt((a * a).sum() * (b * b).sum())
    return float((a * b).sum() / d) if d else 0.0


def best_lag(a, b, maxlag):
    """Largest total-envelope correlation over a lag sweep, and the lag that gives it.

    A single global lag only helps if the two captures share one fixed offset. The
    earrings free-run (each starts its pipeline at its own moment and counts beats on
    its own clock, see the periodicity search in decide), so a rate difference between
    the two ~16 MHz parts makes the offset walk during a capture. That shows up as a
    best-lag correlation far above zero-lag, and - decisively - as a best lag that
    differs between the first and last third. A drifting offset means the two units
    are analysing different parts of the same song, which no choice of bands fixes.
    """
    best, blag = -9.0, 0
    for lag in range(-maxlag, maxlag + 1):
        sa, sb = max(lag, 0), max(-lag, 0)
        m = min(len(a), len(b)) - max(sa, sb)
        if m < 200:
            continue
        c = corr(a[sa:sa + m], b[sb:sb + m])
        if c > best:
            best, blag = c, lag
    return best, blag


def report(path):
    dL = specdump.load(os.path.join(path, "spec-L.csv"))
    dR = specdump.load(os.path.join(path, "spec-R.csv"))
    g, A, B = on_common(dL, dR)
    n = len(g)
    print(f"\n=== {path}: {n} aligned frames ({n / RATE:.0f} s) ===")
    ta, tb = sum(A), sum(B)
    tot = corr(ta, tb)
    fast = corr(ta - shift_ema(ta, 8), tb - shift_ema(tb, 8))
    print(f"  total loudness envelope corr : {tot:+.3f}"
          f"   (fast/detrended {fast:+.3f})")
    # Does a time offset rescue it, and is that offset stable?
    bcorr, blag = best_lag(ta, tb, int(3 * RATE))
    edge = int(3 * RATE) - 1
    thirds = []
    for k in range(3):
        s = slice(k * n // 3, (k + 1) * n // 3)
        c3, l3 = best_lag(ta[s], tb[s], int(3 * RATE))
        thirds.append((l3, c3))
    print(f"  best-lag envelope corr       : {bcorr:+.3f} at lag {blag / RATE:+.2f}s"
          + (" (pegged: offset larger than sweep)" if abs(blag) >= edge else ""))
    lags = " ".join(f"{l / RATE:+.2f}s" for l, _ in thirds)
    cs = " ".join(f"{c:+.2f}" for _, c in thirds)
    print(f"  offset per third (lag/corr)  : [{lags}] / [{cs}]")
    span = (max(l for l, _ in thirds) - min(l for l, _ in thirds)) / RATE
    good = [c for _, c in thirds if c > 0.6]
    if tot < 0.6 and len(good) and span > 0.5:
        print(f"  ** the two clocks are DRIFTING apart: aligned per-third they DO share"
              f" the music\n     (corr up to {max(c for _, c in thirds):.2f}) but the"
              f" offset wanders {span:.2f}s. Not a band problem. **")
    elif tot < 0.6 and not good:
        print("  ** below 0.6 and no lag rescues it: the two units are not hearing a"
              "\n     common signal. Move them or fix audio delivery before tuning. **")
    eband = [corr(A[i], B[i]) for i in range(NB)]
    exc = []
    for i in range(NB):
        ea = A[i] - shift_ema(A[i], 6)
        eb = B[i] - shift_ema(B[i], 6)
        exc.append(corr(ea, eb))
    print("  per-band energy corr (smoothed):")
    print("    " + "  ".join(f"b{i}{BANDS[i]:>8}:{eband[i]:+.2f}"
                             for i in range(NB)))
    print("  per-band excitation corr (ts=6):")
    print("    " + "  ".join(f"b{i}{BANDS[i]:>8}:{exc[i]:+.2f}"
                             for i in range(NB)))

    # Upper bound on the current-style argmax selector, per band set.
    def exc_stack(D):
        return np.stack([D[i] - shift_ema(D[i], 6) for i in range(NB)])
    eL, eR = exc_stack(A), exc_stack(B)

    def argmax(sets):
        a = np.argmax(np.stack([eL[b] for b in sets]), 0)
        b = np.argmax(np.stack([eR[b] for b in sets]), 0)
        return float(np.mean(a == b))
    cur = argmax([2, 4, 6])
    print(f"  argmax per-frame agreement: current {{2,4,6}} = {100 * cur:.0f}%")
    for sets in ([3, 4, 5], [4, 5, 6], [3, 4, 5, 6], list(range(NB))):
        print(f"    {{{','.join(map(str, sets))}}}: {100 * argmax(sets):.0f}%"
              f"   ({len(sets)} patterns)")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("dirs", nargs="*")
    ap.add_argument("--capture", action="store_true",
                    help="run specdump first, into --outdir")
    ap.add_argument("--seconds", type=float, default=40)
    ap.add_argument("--file", default=None)
    ap.add_argument("--outdir", default="/tmp/ceiling")
    a = ap.parse_args()
    dirs = list(a.dirs)
    if a.capture:
        subprocess.run([sys.executable, os.path.join(HERE, "specdump.py"),
                        "--seconds", str(a.seconds), "--outdir", a.outdir]
                       + (["--file", a.file] if a.file else []), check=True)
        dirs.append(a.outdir)
    if not dirs:
        ap.error("give one or more capture dirs, or use --capture")
    for d in dirs:
        report(d)


if __name__ == "__main__":
    main()
