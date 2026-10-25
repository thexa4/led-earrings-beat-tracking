#!/usr/bin/env python3
"""What is actually in the dump? Fine-resolution spectrum of the two streams.

tonescore.py established that both earrings carry a signal that is strongly
correlated with each other and completely uncorrelated with the tones being
played. This asks what that signal is, because the candidates have different
shapes:

  acoustic room noise (rain, amp hiss)  - smooth spectrum, no lines
  SWD polling artefacts                 - lines at the poll rate and its
                                          harmonics (~9 Hz here), present in
                                          both devices because one script polls
                                          both on one schedule
  LED PWM / bitbang crosstalk           - lines at the drive frequency and its
          \                                 harmonics, in the kHz range
  ADC/oversampler noise                 - flat or rising, uncorrelated between
                                          devices (so not this - L and R agree)

8 s windows give 0.125 Hz resolution, enough to see a poll-rate comb. The last
section prints the waveform of one gap for a moment: a wander is noise, a strict
periodic ladder is machine-made.

Usage: tools/whatnoise.py [hears-left.wav hears-right.wav]
"""

import argparse
import sys
import wave

import numpy as np

sys.path.insert(0, __file__.rsplit("/", 1)[0])


def read_wav(path):
    with wave.open(path) as w:
        x = np.frombuffer(w.readframes(w.getnframes()), dtype=np.int16)
    return x.astype(np.float64), w.getframerate()


def spectrum(x, fs, win_s=8.0):
    """Averaged magnitude spectrum, dB, with 1/win_s Hz resolution."""
    n = int(win_s * fs)
    acc = None
    w = np.hanning(n)
    for s in range(0, len(x) - n, n):
        X = np.abs(np.fft.rfft((x[s:s + n] - x[s:s + n].mean()) * w))
        acc = X if acc is None else acc + X
    if acc is None:
        acc = np.abs(np.fft.rfft((x - x.mean()) * np.hanning(len(x))))
    acc /= max(len(x) // n, 1)
    return np.fft.rfftfreq(n, 1 / fs), 20 * np.log10(acc + 1e-9)


def peaks(fr, dbv, fmax, n=8, prominence=6.0):
    """Local maxima worth naming: above the local median by `prominence` dB."""
    m = fr <= fmax
    f, d = fr[m], dbv[m]
    base = np.median(d)
    out = []
    for i in range(2, len(d) - 2):
        if d[i] == max(d[i - 2:i + 3]) and d[i] > base + prominence:
            if out and f[i] - out[-1][0] < 1.0 / (fr[1] - fr[0]) * 2:
                if d[i] > out[-1][1]:
                    out[-1] = (f[i], d[i])
                continue
            out.append((f[i], d[i]))
    return sorted(out, key=lambda t: -t[1])[:n]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("files", nargs="*",
                    default=["/tmp/psync/hears/hears-left.wav",
                             "/tmp/psync/hears/hears-right.wav"])
    ap.add_argument("--names", default="L,R")
    a = ap.parse_args()
    names = a.names.split(",")

    for nm, p in zip(names, a.files):
        x, fs = read_wav(p)
        fr, dbv = spectrum(x, fs)
        print(f"\n=== {nm}: {len(x) / fs:.1f} s @ {fs} Hz")
        print("  strongest narrow features below 2 kHz "
              "(Hz, dB over the median of that span):")
        for f, d in peaks(fr, dbv, 2000):
            base = np.median(dbv[(fr >= f - 200) & (fr <= f + 200)])
            print(f"    {f:8.2f} Hz   +{d - base:5.1f} dB")
        # Energy by decade, to see the shape without any line structure.
        print("  band energy (dB re. max band):")
        edges = [0, 20, 50, 100, 200, 500, 1000, 2000, 4000]
        e = []
        for lo, hi in zip(edges, edges[1:]):
            m = (fr >= lo) & (fr < hi)
            e.append(10 * np.log10(np.sum(10 ** (dbv[m] / 10)) + 1e-12))
        mx = max(e)
        for (lo, hi), v in zip(zip(edges, edges[1:]), e):
            print(f"    {lo:5.0f}-{hi:<5.0f} {v - mx:7.1f}")

    # Waveform of one silent gap: shape tells a wander from a machine ladder.
    x, fs = read_wav(a.files[0])
    s = int((0.5 + 2 * 3 + 2.3) * fs)  # mid-gap
    seg = x[s:s + int(0.4 * fs)]
    print(f"\n{names[0]} waveform, 400 ms of a silent gap, every "
          f"{len(seg) // 60} samples:")
    step = max(len(seg) // 60, 1)
    lo, hi = seg.min(), seg.max()
    for v in seg[::step]:
        k = int((v - lo) / max(hi - lo, 1) * 50)
        print(f"  {v:7.0f} |" + "#" * k)
    d = np.diff(seg)
    print(f"  consecutive-sample deltas: mean {d.mean():.1f}, "
          f"std {d.std():.1f}, largest |delta| {np.abs(d).max():.0f}")


if __name__ == "__main__":
    main()
