#!/usr/bin/env python3
"""Is there a rhythm in the dump at all? Per-second band energies, no assumptions.

The tone fixture is 2 s of tone then 1 s of silence, five times, so any file that
really heard the speaker has a 3 s strobe in its band energies. This prints that
directly - no coherent projection, no clock fit, no reference window - because
every smarter measurement so far has returned a number that was constant across
the five tones, which is the signature of a signal that is not there.

  python3 tools/rhythm.py /tmp/psync/hears/hears-raw-left.wav
  python3 tools/rhythm.py a.wav b.wav --names L R
"""

import argparse
import wave

import numpy as np

BANDS = [(0, 125), (125, 250), (250, 500), (500, 1000), (1000, 2000), (2000, 3900)]


def load(path):
    with wave.open(path, "rb") as wf:
        n = wf.getnframes()
        x = np.frombuffer(wf.readframes(n), dtype=np.int16).astype(np.float64)
        return x, wf.getframerate()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("files", nargs="+")
    ap.add_argument("--names", default="")
    a = ap.parse_args()
    names = a.names.split() if a.names else [f.rsplit("/", 1)[-1] for f in a.files]

    for path, name in zip(a.files, names):
        x, fs = load(path)
        print(f"\n{name} ({path}): {len(x) / fs:.1f} s  "
              f"unique values={len(np.unique(x))}  "
              f"min={x.min():.0f} max={x.max():.0f} rms={np.sqrt((x ** 2).mean()):.0f}")
        # 0.5 s windows: 2 s on / 1 s off must be visible in them.
        w = fs // 2
        print("  band energy (dB re 1 code^2) in 0.5 s windows, 2 s apart:")
        print("      band" + "".join(f"{k * 2:>7}" for k in range(26)))
        for lo, hi in BANDS:
            out = []
            for k in range(0, len(x) - w, w * 4):  # every 2 s
                seg = x[k:k + w]
                f = np.abs(np.fft.rfft(seg * np.hanning(w))) ** 2
                fr = np.fft.rfftfreq(w, 1 / fs)
                m = (fr >= lo) & (fr < hi)
                out.append(10 * np.log10(max(f[m].sum(), 1)))
            print(f"  {lo:>5}-{hi:<5}" + "".join(f"{v:7.1f}" for v in out[:26]))
        print("  (columns are 2 s apart, starting at file time 0; the fixture is")
        print("   1 s lead, then 2 s tone / 1 s gap x5 at 150/400/1k/2.5k/5k Hz)")


main()
