#!/usr/bin/env python3
"""Measure each device's response to a tone with a known on/off period.

Why this exists
---------------
Agreement statistics between two devices cannot say which device is at fault, and
comparing a device to the played file needs playback latency to be known, which it
is not on a networked sink. A tone gated on and off with a fixed period escapes
both problems. The response frequency is known exactly in advance, so the test is
a single Fourier bin at that frequency: no alignment, no clock assumption, and no
cross-device comparison. Silence and noise have no energy at a frequency chosen in
advance, so a bin that stands out above neighbouring bins is a real acoustic
response, and a bin that does not is the absence of one.

The same measurement gives three answers at once:

  response amplitude at the tone's gate frequency  -> is sound arriving?
  amplitude relative to neighbouring bins         -> how clean is it (SNR)?
  which band responds most                        -> is the filterbank labeled
     right end to end, including band order?

The frequency-domain test also survives the AGC. Gain control follows the envelope,
so it responds at the gate frequency too, with a lag; that inflates the response
rather than hiding it, which is the safe direction for a detection test.

Read the SNR column as the real quantity of interest. A device can pass detection
and still have so little margin that its band-to-band balance is noise, which is
invisible to a pass/fail test and fatal to anything that picks a band.

  python3 tools/toneresponse.py --capture DIR --lead SECONDS \
      [--period 10] [--cycles 7] [--bandmap 1200]
"""
import argparse
import os
import sys

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import specdump  # noqa: E402

NB = 8


def bin_amp(t, x, f, t0, t1):
    """Amplitude of x at frequency f over [t0, t1], x interpolated to a uniform grid."""
    m = (t >= t0) & (t <= t1)
    if m.sum() < 50:
        return 0.0
    tt = np.linspace(t[m][0], t[m][-1], int((t1 - t0) * 25.0))
    y = np.interp(tt, t[m], x[m])
    y = y - y.mean()
    if np.allclose(y, 0):
        return 0.0
    ph = np.exp(-2j * np.pi * f * tt)
    return float(2.0 * np.abs((y * ph).sum()) / len(y))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--capture", required=True)
    ap.add_argument("--lead", type=float, required=True,
                    help="seconds from capture start to the start of playback")
    ap.add_argument("--period", type=float, default=10.0,
                    help="gate period in seconds, on for the first half")
    ap.add_argument("--cycles", type=int, default=6,
                    help="whole gate periods to analyse")
    ap.add_argument("--skip", type=float, default=10.0,
                    help="seconds to skip after the start, for AGC settling")
    ap.add_argument("--bandmap", type=float, default=None,
                    help="tone frequency, to check which band answers")
    a = ap.parse_args()

    fgate = 1.0 / a.period
    # Control frequencies: same magnitude of signal expected from noise alone, and
    # deliberately off the gate frequency and its harmonics.
    controls = [f for f in np.arange(0.04, 0.45, 0.013)
                if min(abs(f - fgate * h) for h in (1, 2, 3)) > 0.02]

    for side in ("L", "R"):
        p = os.path.join(a.capture, f"spec-{side}.csv")
        if not os.path.exists(p):
            continue
        d = specdump.load(p)
        t = d["t"]
        t0 = a.lead + a.skip
        t1 = t0 + a.cycles * a.period
        if t1 > t[-1] - 2:
            print(f"{side}: window {t0:.0f}-{t1:.0f} s past end of capture "
                  f"({t[-1]:.0f} s)")
            continue
        bands = [d[f"snap{i}"] / 256.0 for i in range(NB)]
        # Total level in the linear domain: the acoustic drive, and the AGC's
        # response to it.
        tot = np.sum([d[f"raw{i}"] for i in range(NB)], axis=0) / 1e3

        amps = [bin_amp(t, b, fgate, t0, t1) for b in bands]
        amps += [bin_amp(t, tot.astype(float), fgate, t0, t1)]
        floors = []
        for fc in controls:
            floors.append(max(bin_amp(t, b, fc, t0, t1) for b in bands))
        floor = float(np.median(floors)) if floors else 0.0

        names = [f"b{i}" for i in range(NB)] + ["sum"]
        print(f"\n=== {side}  window {t0:.0f}-{t1:.0f} s "
              f"({a.cycles} gate periods at {fgate:.2f} Hz)")
        print("  response amplitude, doublings of band energy at the gate rate:")
        print("    " + "  ".join(f"{n:>6}" for n in names))
        print("    " + "  ".join(f"{v:6.3f}" for v in amps))
        snr = [v / floor if floor > 0 else float("inf") for v in amps]
        print("    " + "  ".join(f"{v:6.1f}" for v in snr)
              + f"   <- SNR vs noise floor {floor:.4f}")
        best = int(np.argmax(amps))
        print(f"  strongest: {names[best]} at SNR {snr[best]:.1f}")
        if a.bandmap:
            print(f"  a {a.bandmap:.0f} Hz tone should answer in one band, not all "
                  "eight; a flat answer across bands is level, not spectrum.")
        for nm, col in (("agcShift", d["agcShift"]), ("agcPeak", d["agcPeak"])):
            s = col[(t >= t0) & (t <= t1)]
            print(f"  {nm}: {np.percentile(s, 5):.0f} .. "
                  f"{np.percentile(s, 95):.0f} (median {np.median(s):.0f})")
        det = [n for n, s in zip(names, snr) if s > 4.0]
        print(f"  detected (SNR>4): {', '.join(det) if det else 'NOTHING'}")


main() if __name__ == "__main__" else None
