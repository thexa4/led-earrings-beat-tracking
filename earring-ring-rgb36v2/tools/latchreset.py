#!/usr/bin/env python3
"""Does clearing the pattern latch at a shared moment fix master's selector?

The sweep in freesweep.py shows what the hysteresis actually does: with the shipped
margin each device picks a band early and keeps it for the whole capture - hold
times of 1375 s on a 23-minute recording, one pattern out of seven. Two earrings
then agree if they happened to latch onto the same band and disagree forever if they
did not, which is exactly the measured 47.9% / kappa +0.03.

That is the failure mode hysteresis was written to prevent, turned inside out.
Hysteresis is a latch, and a latch remembers. What it remembers is the device's own
history, so any early disagreement becomes permanent: the incumbent only ever moves
when a challenger wins by a fixed margin, and two devices whose spectra differ by
less than that margin sit on whatever they each grabbed first. Agreement between two
latches is not convergence - it is two independent draws that happen to match.

The interesting part is that master already has a place where the two devices are
provably synchronised: the transition anchor (pipeline.go step 8). It is a level
trigger on onset plus novelty plus above-average energy, deliberately not tied to a
beat boundary because "which frame the PLL emits a beat on is a per-device detail".
Both earrings fire it at the same musical moment. It already calls setPattern - but
it does NOT clear the latched band, so the device-specific incumbent survives the one
moment the pair is aligned, and the anchor re-selects through the same sticky rule.

Clearing the latch there costs a store instruction. This tests the idea with a
periodic reset as a proxy for the anchor, because the recorded telemetry has no
anchor column - if a reset every few seconds recovers agreement, the mechanism is
real and firing it on the actual anchor can only do better, since the real anchor is
musically placed rather than periodic.

  python3 latchreset.py ~/psync/t1 ~/psync/newfull
"""
import csv
import os
import sys

import numpy as np

NB = 8
FPS = 62.5
NP = NB - 1
Q16Q = 49939


def mul_q16(n, q=Q16Q):
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
    """Byte-exact transcription of master's spectralDevs (see mastersel.py: checked
    against the Go original, including a linear ramp that must give ~0)."""
    sy = sxy = 0
    for b in range(NB):
        sy += e[b]
        sxy += e[b] * b
    num = 8 * sxy - 28 * sy
    slope = mul_q16(num)
    intercept = (sy << 5) - (slope * 7 >> 1)
    return [(e[b] << 8) - intercept - slope * b for b in range(NB)]


def devs_all(G):
    """Deviations for every frame, computed once and reused. Without this every
    candidate in the sweep re-runs the fit, and the sweep takes ten minutes."""
    return np.array([spectral_devs(row) for row in G], dtype=np.int64)


def run(D, margin, reset, lo=1, hi=NB - 1):
    """Hysteresis with the latch dropped every `reset` frames (0 = never)."""
    n = len(D)
    out = np.zeros(n, dtype=np.int64)
    cur, valid = -1, False
    for i in range(n):
        d = D[i]
        ch = lo
        for b in range(lo + 1, hi):
            if d[b] > d[ch]:
                ch = b
        if reset and i % reset == 0:
            valid = False
        if not valid or ch == cur or d[ch] >= d[cur] + margin:
            cur = ch
        out[i] = cur
        valid = True
    return out


def score(a, b, warm):
    a, b = a[warm:], b[warm:]
    po = float(np.mean(a == b))
    pe = sum(float(np.mean(a == k)) * float(np.mean(b == k)) for k in range(NP))
    kap = (po - pe) / (1 - pe) if pe < 1 else 0.0
    occ = [float(np.mean(a == k)) for k in range(NP)] + \
          [float(np.mean(b == k)) for k in range(NP)]
    used = len(set(a.tolist()) | set(b.tolist()))
    hold = []
    for p in (a, b):
        e = np.concatenate(([0], np.flatnonzero(np.diff(p) != 0) + 1, [len(p)]))
        hold.append(float(np.mean(np.diff(e))) / FPS)
    return po, kap, used, min(occ), hold


def load(path):
    t, b = [], []
    with open(path) as fh:
        for r in csv.DictReader(fh):
            t.append(float(r["t"]))
            b.append([int(float(r[f"b{i}"])) for i in range(NB)])
    t = np.asarray(t)
    o = np.argsort(t)
    return t[o], np.asarray(b, dtype=np.int64)[o]


def grid(d):
    tl, bl = load(os.path.join(d, "state_L.csv"))
    tr, br = load(os.path.join(d, "state_R.csv"))
    lo, hi = max(tl[0], tr[0]), min(tl[-1], tr[-1])
    tg = np.arange(lo, hi, 1.0 / FPS)
    return (bl[np.clip(np.searchsorted(tl, tg), 0, len(tl) - 1)][:len(tg)],
            br[np.clip(np.searchsorted(tr, tg), 0, len(tr) - 1)][:len(tg)])


def main():
    if len(sys.argv) < 2:
        raise SystemExit(__doc__)
    caps = []
    for d in sys.argv[1:]:
        GL, GR = grid(d)
        caps.append((d, devs_all(GL), devs_all(GR)))
        print(f"loaded {d}: {len(GL)/FPS/60:.1f} min (deviations cached)")
    warm = int(120 * FPS)

    print("\n  A reset is only worth having if it changes agreement, not just the"
          " look")
    print(f"  {'margin':>7} {'reset':>7} " +
          "  ".join(f"{os.path.basename(c[0]):>34}" for c in caps))
    for margin in (0, 32 << 8, 64 << 8):
        for rs in (0, 2, 4, 8, 16, 32):
            reset = 0 if rs == 0 else int(rs * 62.5 * 4)
            cells = []
            for d, DL, DR in caps:
                a = run(DL, margin, reset)
                b = run(DR, margin, reset)
                po, kap, used, mo, hold = score(a, b, warm)
                cells.append(f"{po*100:5.1f}% {kap:+6.3f} u{used} "
                             f"h{hold[0]:5.1f}/{hold[1]:<5.1f}")
            tag = "never (shipped rule)" if reset == 0 else f"{reset/62.5:.0f} s"
            print(f"  {margin >> 8:7d} {tag:>21} " + "  ".join(cells), flush=True)

    print("\n  Control: the same rows with R shifted by 97 s. If a reset's gain"
          " survives a"
          "\n  97 s shift it is two devices resting on the same slow state, not"
          " two devices"
          "\n  tracking the same music, and it is not worth shipping.")
    for margin in (0, 32 << 8):
        for rs in (0, 4, 16):
            reset = 0 if rs == 0 else int(rs * 62.5 * 4)
            for d, DL, DR in caps:
                a = run(DL, margin, reset)
                b = run(DR, margin, reset)
                po, kap, used, mo, hold = score(
                    a, np.roll(b, int(97 * FPS)), warm)
                print(f"  CONTROL {os.path.basename(d):<10} margin={margin>>8:3d}"
                      f" reset={'never' if not reset else str(reset//250)+'s':>6}"
                      f"  {po*100:5.1f}% {kap:+6.3f}", flush=True)


if __name__ == "__main__":
    main()
