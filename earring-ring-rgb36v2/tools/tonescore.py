#!/usr/bin/env python3
"""Analyse a micdump capture against the tone fixture: is there real audio?

tools/micdump.py reconstructs the two microphone streams; this decides what
they mean. For every burst of /tmp/psync/tones.wav (150, 400, 1000, 2500,
5000 Hz, 2 s each, 1 s gaps, 16 s cycle) it measures the coherent tone amplitude

    amp(f) = 2 * |sum((x - mean) * hann * exp(-2j pi f t))| / sum(hann)

on 8 kSPS windows, plus the same projection one null off tone inside the silent
gaps as the noise floor. Coherent projection, not RMS: a 2 s burst of a single
sine buried in noise is invisible in RMS but stands out here, and the null
measurement is what turns "the numbers moved" into "there is signal".

Also reported, because they are what the pipeline consumes:

  SNR        tone amplitude over the in-gap noise floor, per device
  L - R      level difference between the earrings during the same burst
  spectrum   a broadband FFT of one burst, to see whether the response is the
             tone or a rising noise floor

The dump is byte-wide (micDumpShift in sync.go), so amplitudes are in dump
counts: 1 count = 2^micDumpShift = 32 raw ADC codes.

Usage: tools/tonescore.py [hears-left.wav hears-right.wav]
"""

import argparse
import os
import sys
import wave

import numpy as np

CYCLE = 16.0
TONES = [150, 400, 1000, 2500, 5000]
BURST = 2.0     # s of tone
GAP = 1.0       # s of silence after it
LEAD = 0.5      # music starts at 0.5 s in the aligned files (micdump convention)
# Since the auto-ranging dump (micDumpShift -> per-frame gain in sync.go),
# micdump writes deviations in the device's own 16-bit oversampled ADC codes, so
# a WAV sample IS an ADC code and no conversion is needed. Divide by 16 for the
# 12-bit equivalent.
ADC = 1.0


def read_wav(path):
    with wave.open(path) as w:
        n = w.getnframes()
        raw = w.readframes(n)
    x = np.frombuffer(raw, dtype=np.int16).astype(np.float64)
    if w.getnchannels() == 2:
        x = x[::2]
    return x, w.getframerate()


def coherent(x, f, fs):
    """Amplitude of a sine of frequency f in samples x (Hann-windowed)."""
    n = len(x)
    w = np.hanning(n)
    t = np.arange(n) / fs
    return 2 * abs(((x - x.mean()) * w) * np.exp(-2j * np.pi * f * t)).sum() / w.sum()


def band_spectrum(x, fs, fmax=3900):
    """Magnitude spectrum in dB, coarse log-ish bins for printing."""
    n = len(x)
    X = np.abs(np.fft.rfft((x - x.mean()) * np.hanning(n)))
    fr = np.fft.rfftfreq(n, 1 / fs)
    edges = [0, 60, 120, 250, 500, 1000, 2000, 3000, fs / 2]
    out = []
    for lo, hi in zip(edges, edges[1:]):
        m = (fr >= lo) & (fr < hi)
        out.append(20 * np.log10(X[m].max() + 1e-9))
    return [f"{int(lo)}-{int(hi) if hi < 1e4 else 'Nyq'}"
            for lo, hi in zip(edges, edges[1:])], out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("files", nargs="*",
                    default=["/tmp/psync/hears/hears-left.wav",
                             "/tmp/psync/hears/hears-right.wav"])
    ap.add_argument("--names", default="L,R")
    ap.add_argument("--window", type=float, default=0.5, help="s per snapshot")
    a = ap.parse_args()

    names = a.names.split(",")
    data = {}
    for n, p in zip(names, a.files):
        if not os.path.exists(p):
            raise SystemExit(f"missing {p}")
        data[n], fs = read_wav(p)
    fs = read_wav(a.files[0])[1]
    print(f"{fs=} Hz, " + ", ".join(
        f"{n}: {len(data[n]) / fs:.1f} s" for n in names))

    win = int(a.window * fs)
    # Mid-burst and mid-gap sample windows, across every repeat of the fixture.
    slots = []
    t = LEAD
    while t + CYCLE <= min(len(v) for v in data.values()) / fs:
        for i in range(5):
            b0 = t + i * 3 + 0.5
            slots.append((b0, i))
            slots.append((t + i * 3 + BURST + 0.25, None))
        t += CYCLE
    tone = {n: {i: [] for i in range(5)} for n in names}
    gapref = {n: {i: [] for i in range(5)} for n in names}
    floor = {n: [] for n in names}
    for start, i in slots:
        s0 = int(start * fs)
        for n in names:
            x = data[n][s0:s0 + win]
            if len(x) < win:
                continue
            if i is None:
                floor[n].append(max(coherent(x, f * 1.37, fs) for f in TONES))
                # The honest reference: the very same projection, at the very
                # same frequency, measured while nothing but room noise plays.
                # A projection at a "null" frequency measures whatever else is
                # in the room, which for a rising noise floor is not the same
                # thing at all.
                for j in range(5):
                    gapref[n][j].append(max(coherent(x, TONES[j] * k, fs)
                                            for k in np.linspace(0.97, 1.03, 25)))
            else:
                # +-3% search: the crystals differ by ~0.3% and the dump rate is
                # a fitted grid, so the tone is not exactly on frequency.
                freq = TONES[i]
                tone[n][i].append(max(coherent(x, freq * k, fs)
                                      for k in np.linspace(0.97, 1.03, 25)))

    print(f"\ncoherent amplitude at the tone frequency, ADC codes "
          "(16-bit oversampled: divide by 16 for 12-bit)")
    print(f"{'':16}" + "".join(f"{f:>10}Hz" for f in TONES))
    for n in names:
        amp = [np.median(tone[n][i]) * ADC for i in range(5)]
        ref = [np.median(gapref[n][i]) * ADC for i in range(5)]
        print(f"{n + ' during tone':16}" + "".join(f"{v:12.1f}" for v in amp))
        print(f"{n + ' in gap':16}" + "".join(f"{v:12.1f}" for v in ref))
        print(f"{n + ' rise (dB)':16}" +
              "".join(f"{20 * np.log10(a / max(r, 1e-9)):12.1f}"
                      for a, r in zip(amp, ref)))
    print(f"\n(off-tone null, for comparison: L={np.median(floor['L']) * ADC:.1f} "
          f"R={np.median(floor['R']) * ADC:.1f} - higher than the on-tone gap "
          "reading means a rising noise floor, not signal)")
    if len(names) == 2:
        l, r = names
        print(f"{'R - L (dB)':14}" + "".join(
            f"{20 * np.log10(np.median(tone[r][i]) / max(np.median(tone[l][i]), 1e-9)):12.1f}"
            for i in range(5)) +
            f"{20 * np.log10(np.median(floor[r]) / np.median(floor[l])):11.1f}")

    print(f"\nspectrum during the 1000 Hz burst (dB, max per band, 1 s window):")
    dur = int(1.0 * fs)
    b0 = int((LEAD + 2 * 3 + 0.5) * fs)
    for n in names:
        labels, vals = band_spectrum(data[n][b0:b0 + dur], fs)
        print(f"{n:14}" + "".join(f"{v:11s}" for v in labels))
        print(f"{'':14}" + "".join(f"{v:11.0f}" for v in vals))
    # Does each earring follow the stimulus? Complex coherence between two
    # free-running crystals is meaningless above a few hundred Hz (0.3% clock
    # error rotates the phase by 3 cycles/s at 1 kHz), so this compares band
    # ENVELOPES, which are insensitive to sample-clock offset. A front end that
    # is deaf, or is only picking up its own switching noise, correlates with
    # nothing; one that hears the speaker follows the burst/gap pattern.
    w = int(0.25 * fs)

    def envelope(x, lo, hi):
        n = len(x)
        X = np.fft.rfft(x * np.hanning(n))
        fr = np.fft.rfftfreq(n, 1 / fs)
        X[(fr < lo) | (fr > hi)] = 0
        env = np.abs(np.fft.irfft(X, n))
        # 0.25 s moving average: keeps the note-level shape, drops the cycle.
        k = max(int(0.05 * fs), 1)
        return np.convolve(env, np.ones(k) / k, mode="same")

    n = min(len(v) for v in data.values())
    t = np.arange(n) / fs
    on = np.zeros(n)
    for k in range(int(n / fs / CYCLE) + 1):
        for i in range(5):
            b0 = LEAD + k * CYCLE + i * 3
            on[(t >= b0) & (t < b0 + BURST)] = 1.0
    print("\ncorrelation of the band envelope with the burst/gap pattern:")
    print("        " + "".join(f"{f'{lo}-{hi}Hz':>12s}"
                               for lo, hi in zip([0, 125, 250, 500, 1000, 2000],
                                                 [125, 250, 500, 1000, 2000, 3900])))
    for name in names:
        row = []
        for lo, hi in [(0, 125), (125, 250), (250, 500), (500, 1000),
                       (1000, 2000), (2000, 3900)]:
            e = envelope(data[name][:n], lo, hi)
            e = e[:: max(w // 8, 1)]
            o = on[:: max(w // 8, 1)]
            if e.std() < 1e-9 or o.std() < 1e-9:
                row.append(0.0)
            else:
                row.append(float(np.corrcoef(e, o)[0, 1]))
        print(f"  {name:6s}" + "".join(f"{v:12.2f}" for v in row))
    if len(names) == 2:
        row = []
        for lo, hi in [(0, 125), (125, 250), (250, 500), (500, 1000),
                       (1000, 2000), (2000, 3900)]:
            e1 = envelope(data[names[0]][:n], lo, hi)[:: max(w // 8, 1)]
            e2 = envelope(data[names[1]][:n], lo, hi)[:: max(w // 8, 1)]
            row.append(float(np.corrcoef(e1, e2)[0, 1])
                       if e1.std() > 0 and e2.std() > 0 else 0.0)
        print(f"  {'L~R':6s}" + "".join(f"{v:12.2f}" for v in row))

    print("\nfull-file RMS: " + ", ".join(
        f"{nm}={np.std(data[nm]) * ADC:.0f} ADC codes"
        for nm in names))
    print("(the 5000 Hz burst folds to 3000 Hz: the dump is 8 kSPS, Nyquist 4 kHz)")


if __name__ == "__main__":
    main()
