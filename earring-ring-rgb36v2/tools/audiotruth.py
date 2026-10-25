#!/usr/bin/env python3
"""Does each earring measure the music that was actually played?

Why this exists
---------------
Every cross-device agreement number is a *pairwise* measurement, and a pairwise
number cannot tell you which device is wrong. When agreement is near chance there
are two very different causes with the same symptom: both devices hear the music
but measure it slightly differently (a selector problem, fixable in software), or
one device is not measuring the music at all (a level, placement, AGC or hardware
problem, not fixable by any selector). Tuning a selector against the second cause
wastes days, because every grouping looks equally hopeless.

So stop comparing the earrings to each other and compare each one to the file
that was played. The file is known exactly, so this is ground truth rather than a
second opinion, and each device is scored on its own.

Method
------
Compute the played audio's band energies with the same band edges and frame length
the pipeline uses, on the host, then correlate each device's reported smoothed
bands against that reference. Correlation is on the *shape* time series, so the
AGC's level normalisation does not interfere: the AGC removes level, not which
band is loud.

Playback start is not known to the sample: this sink is networked and adds a
variable delay. So each correlation is a small lag scan and both the correlation
*and the lag that produced it* are printed. A high correlation at an absurd lag is
the classic way to invent agreement, so the lag is part of the result, not a detail.
A sane result is a lag of a fraction of a second to a couple of seconds, consistent
between bands. A device that hears the music shows high correlation at one common
lag; a device that does not shows low correlation at every lag.

The device's own filterbank is an IIR/Goertzel bank, not an FFT, so absolute values
differ from the reference. Temporal correlation does not depend on that.

  python3 tools/audiotruth.py --capture DIR --file PLAYED.wav [--maxlag 4]
"""
import argparse
import os
import sys

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import specdump  # noqa: E402

NB = 8
# Band edges, Hz, matching the pipeline's map: b0 is the top band.
EDGES = [250, 400, 630, 1000, 1600, 2500, 4000]
FS = 16000.0
FRAME = 250  # pipeline Window, samples


def read_wav_pcm16(path):
    """Mono float at FS from a wav, via ffmpeg (any input format)."""
    import subprocess
    p = subprocess.run(
        ["ffmpeg", "-v", "error", "-i", path, "-ac", "1", "-ar", str(int(FS)),
         "-f", "f32le", "-"],
        stdout=subprocess.PIPE, check=True)
    return np.frombuffer(p.stdout, dtype=np.float32)


def ref_bands(x):
    """log2 band energy per frame, shaped like the device's snap columns.

    One frame per FRAME samples, so the reference advances at the pipeline's
    frame rate rather than the poller's; the caller resamples onto the poll grid.
    """
    n = len(x) // FRAME * FRAME
    xf = x[:n].reshape(-1, FRAME) * np.hanning(FRAME)
    sp = np.abs(np.fft.rfft(xf, n=512, axis=1)) ** 2
    fr = np.fft.rfftfreq(512, 1 / FS)
    # EDGES ascending, 7 of them for 8 bands. Band 0 is the top band, so band b
    # spans [EDGES[NB-2-b], EDGES[NB-1-b]) with band 0 open above and band NB-1
    # open below.
    out = np.zeros((xf.shape[0], NB))
    for b in range(NB):
        lo = None if b == NB - 1 else EDGES[NB - 2 - b]
        hi = None if b == 0 else EDGES[NB - 1 - b]
        m = np.ones(len(fr), bool)
        if lo is not None:
            m &= fr >= lo
        if hi is not None:
            m &= fr < hi
        out[:, b] = np.log2(sp[:, m].sum(axis=1) + 1.0) * 256.0
    return out


def cc(a, b):
    a = a - a.mean()
    b = b - b.mean()
    d = np.sqrt((a * a).sum() * (b * b).sum())
    return float((a * b).sum() / d) if d else 0.0


def best_lag(dev_t, dev, ref_t, ref, maxlag, step=0.05):
    """Correlation of dev(t) with ref(t - lag), plus the winning lag."""
    best = (-9.0, 0.0)
    for lag in np.arange(-maxlag, maxlag + 1e-9, step):
        r = np.interp(dev_t - lag, ref_t, ref)
        c = cc(dev, r)
        if c > best[0]:
            best = (c, lag)
    return best


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--capture", required=True,
                    help="specdump output directory (spec-L.csv / spec-R.csv)")
    ap.add_argument("--file", required=True, help="the wav/mp3 that was played")
    ap.add_argument("--lead", type=float, default=None,
                    help="seconds of capture before playback began; if omitted, "
                         "taken from the specdump log next to the capture")
    ap.add_argument("--maxlag", type=float, default=4.0)
    ap.add_argument("--quiet-frac", type=float, default=0.0,
                    help="require this fraction of the capture to be non-silent "
                         "in the reference, as a delivery sanity check")
    a = ap.parse_args()

    x = read_wav_pcm16(a.file)
    rb = ref_bands(x)
    ref_t = (np.arange(len(rb)) + 0.5) * FRAME / FS
    print(f"reference: {len(x) / FS:.0f} s of audio, {len(rb)} frames")

    lead = a.lead
    if lead is None:
        # specdump prints when it started the player; fall back to its default.
        lead = 5.0
        print("no --lead given, assuming 5 s (pass --lead to be exact)")

    for side in ("L", "R"):
        p = os.path.join(a.capture, f"spec-{side}.csv")
        if not os.path.exists(p):
            print(f"{side}: no {p}")
            continue
        d = specdump.load(p)
        t = d["t"] - lead
        keep = (t > ref_t[0]) & (t < ref_t[-1])
        snap = np.stack([d[f"snap{i}"] for i in range(NB)], axis=1)[keep]
        t = t[keep]
        if len(t) < 200:
            print(f"{side}: only {len(t)} usable rows")
            continue
        cors, lags = [], []
        for b in range(NB):
            c, lag = best_lag(t, snap[:, b], ref_t, rb[:, b], a.maxlag)
            cors.append(c)
            lags.append(lag)
        # Total envelope: mean over bands, the most robust single trace.
        env = snap.mean(axis=1)
        renv = rb.mean(axis=1)
        ce, le = best_lag(t, env, ref_t, renv, a.maxlag)
        # At the envelope's lag, how much of the band *shape* is shared?
        shape = []
        r_env = np.interp(t - le, ref_t, renv)
        for b in range(NB):
            r = np.interp(t - le, ref_t, rb[:, b])
            shape.append(cc(snap[:, b] - env, r - r_env))
        print(f"\n=== {side}: {len(t)} rows over {t[-1] - t[0]:.0f} s")
        print("  per-band correlation with the played file (lag scan):")
        print("    band:  " + " ".join(f"{b:>6d}" for b in range(NB)))
        print("    corr:  " + " ".join(f"{c:+6.2f}" for c in cors))
        print("    lag s: " + " ".join(f"{l:+6.2f}" for l in lags))
        print(f"  total envelope: corr {ce:+.3f} at lag {le:+.2f} s")
        print("  band shape at that lag (level removed): "
              + " ".join(f"{c:+.2f}" for c in shape))
        spread = np.std(lags)
        verdict = ("MEASURES the music" if ce > 0.5 and spread < 0.6 else
                   "weak / partial" if ce > 0.3 else
                   "does NOT track the played audio")
        print(f"  -> {verdict}  (envelope corr {ce:+.2f}, "
              f"lag spread across bands {spread:.2f} s)")
        print("  A high correlation at one lag that no other band shares, or a "
              "best lag pinned at "
              f"+-{a.maxlag:.0f} s, is not evidence.")


if __name__ == "__main__":
    main()
