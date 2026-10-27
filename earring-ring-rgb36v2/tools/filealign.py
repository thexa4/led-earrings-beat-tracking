#!/usr/bin/env python3
"""Score each earring against the file that was played, over the WHOLE file.

Every agreement number produced so far compares the two earrings with each other,
and a pairwise number cannot say which side is wrong. Chance agreement has two
causes with identical symptoms - both devices hear the music and measure it
differently, which is a selector problem, or one of them is not measuring the
music at all, which no selector can fix - and the tools built so far cannot tell
them apart, which is why tuning kept producing nothing.

So this compares each device with the audio file itself. The file is known exact-
ly:
decode it, run the pipeline's own band split over it, and correlate. Two devices
that both track the file well must agree with each other, and a selector cannot
repair a feature one of them does not measure. This is the cheapest way to find
out whether any amount of selector work is worth doing at all.

The playback offset is unknown - the capture tool starts a player and the sink
adds delay, and a 25-minute capture might begin anywhere in a 60-minute podcast -
so the search runs over the whole file by FFT rather than over a few seconds. A
peak at a nonsense lag is how agreement gets invented, so the lag is printed as
part of the result, and the same lag must appear for every band and for both
devices or the peak means nothing.

Two features are correlated, because the AGC makes the second one the important
one:
  envelope  mean of the eight bands: loudness over time. The AGC normalises this
            away by design, so a low value here is expected and not a fault.
  shape     each band minus its own mean over bands, per frame. The AGC divides
            all bands by roughly the same thing, so WHICH band is loud survives
            it. This is what a colour selector actually reads.

  python3 filealign.py --capture ~/psync/newfull --file ../podcast.mp3
"""
import argparse
import csv
import os
import sys

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import audiotruth  # noqa: E402  (ref_bands, read_wav_pcm16, NB, FS, FRAME)

NB = audiotruth.NB
GRID = 10.0  # Hz: fine for envelope dynamics, keeps the FFT correlation cheap


def load_state(path, col="b"):
    """Device rows: time plus the eight smoothed bands (p.smooth, Q8 log2)."""
    t, rows = [], []
    with open(path) as fh:
        for r in csv.DictReader(fh):
            try:
                v = [float(r[f"{col}{i}"]) for i in range(NB)]
                tt = float(r["t"])
            except (KeyError, ValueError):
                continue
            t.append(tt)
            rows.append(v)
    t = np.asarray(t)
    o = np.argsort(t, kind="stable")
    return t[o], np.asarray(rows, dtype=np.float64)[o]


def resample(t, x, dt=1.0 / GRID):
    g = np.arange(t[0], t[-1], dt)
    out = np.empty((len(g), x.shape[1]))
    for b in range(x.shape[1]):
        out[:, b] = np.interp(g, t, x[:, b])
    return g, out


def detrend(x, seconds=20.0):
    """Remove a slow moving mean: keeps the shape's dynamics, drops bias and any
    drift in the AGC's operating point between the two recordings.

    Works on a (frames, bands) matrix as well as a plain series, removing the
    trend of each column independently, which is the point: each band has its
    own operating point and its own drift.
    """
    n = max(3, int(seconds * GRID))
    ax = 0
    c = np.cumsum(np.concatenate([np.zeros((1,) + x.shape[1:]), x], axis=0),
                  axis=ax)
    i = np.arange(len(x))
    a = np.clip(i - n // 2, 0, len(x))
    b = np.clip(i + n // 2 + 1, 0, len(x))
    m = (c[b] - c[a]) / (b - a).reshape((-1,) + (1,) * (x.ndim - 1))
    return x - m


def xcorr(a, b, maxsec, minfrac=0.5):
    """Zero-lag-corrected normalised cross-correlation of a against b, by FFT.

    Returns (corr, lag_seconds): corr[k] is the correlation of a(t) with
    b(t - lag[k]). Positive lag means the reference leads the device, i.e. the
    device hears the file late, which is what networked playback does.

    Normalised by the OVERLAP at each lag, not by the whole length of both
    series. Without that, a long search over a long file silently divides every
    large lag by the energy of samples that were never compared, so the
    correlation decays with lag and the peak is pulled towards zero - which
    reads as "the capture starts at the start of the file" no matter where it
    actually starts.

    But an overlap of a handful of samples scores a perfect correlation on the
    same arithmetic - the first run of this tool reported r=1.000 for both
    devices at a lag of -3593 s, which is one sample of a 3600-second file - so
    lags with less than `minfrac` of the shorter series in common are zeroed.
    """
    na, nb = len(a), len(b)
    a = a - a.mean()
    b = b - b.mean()
    n = 1 << int(np.ceil(np.log2(na + nb - 1)))
    c = np.fft.irfft(np.fft.rfft(a, n) * np.conj(np.fft.rfft(b, n)), n)
    lags = np.concatenate([np.arange(-(nb - 1), 0), np.arange(0, na)])
    cc = np.concatenate([c[n - (nb - 1):], c[:na]])
    # Energy of the overlapping windows, from cumulative sums of squares. At lag
    # L, a contributes a[max(0,L) : min(na, nb+L)] and b contributes
    # b[max(0,-L) : min(nb, na-L)].
    ca = np.concatenate([[0.0], np.cumsum(a * a)])
    cb = np.concatenate([[0.0], np.cumsum(b * b)])
    a0 = np.maximum(lags, 0)
    a1 = np.minimum(na, nb + lags)
    b0 = np.maximum(-lags, 0)
    b1 = np.minimum(nb, na - lags)
    empty = (a1 - a0 < minfrac * min(na, nb)) | (b1 - b0 < minfrac * min(na, nb))
    ea = ca[np.clip(a1, 0, na)] - ca[np.clip(a0, 0, na)]
    eb = cb[np.clip(b1, 0, nb)] - cb[np.clip(b0, 0, nb)]
    d = np.sqrt(np.maximum(ea, 1e-12) * np.maximum(eb, 1e-12))
    cc = cc / d
    cc[empty] = 0.0
    m = np.abs(lags / GRID) <= maxsec
    return cc[m], lags[m] / GRID


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--capture", required=True)
    ap.add_argument("--file", required=True)
    ap.add_argument("--maxsec", type=float, default=None,
                    help="search window, seconds either side of the natural "
                         "offset; default is the whole file")
    ap.add_argument("--bands", default="b",
                    help="column prefix: b for p.smooth, snap for raw snap")
    a = ap.parse_args()

    x = audiotruth.read_wav_pcm16(a.file)
    rb = audiotruth.ref_bands(x)
    rt = (np.arange(len(rb)) + 0.5) * audiotruth.FRAME / audiotruth.FS
    rg, rbg = resample(rt, rb)
    rshape = rbg - rbg.mean(axis=1, keepdims=True)
    renv = rbg.mean(axis=1)
    print(f"reference {os.path.basename(a.file)}: {len(x)/audiotruth.FS:.0f} s, "
          f"{len(rg)} frames at {GRID:.0f} Hz")

    maxsec = a.maxsec if a.maxsec else len(x) / audiotruth.FS
    for side in "LR":
        p = os.path.join(a.capture, f"state_{side}.csv")
        if not os.path.exists(p):
            print(f"{side}: no {p}")
            continue
        t, B = load_state(p, a.bands)
        if len(t) < 100:
            print(f"{side}: only {len(t)} rows")
            continue
        g, Bg = resample(t, B)
        shape = Bg - Bg.mean(axis=1, keepdims=True)
        env = Bg.mean(axis=1)
        print(f"\n=== {side}: {len(t)} rows over {t[-1]-t[0]:.0f} s ===")
        # The envelope first: where in the file are we, and does loudness arrive?
        cc, lags = xcorr(detrend(env, 20.0), detrend(renv, 20.0), maxsec)
        k = int(np.argmax(cc))
        print(f"  envelope: best r {cc[k]:+.3f} at lag {lags[k]:+.1f} s "
              f"(r@0 {cc[np.argmin(np.abs(lags))]:+.3f})")
        best = lags[k]
        # Now every band's shape at the offset the envelope found. Reading the
        # bands at one common lag is the point: eight independent peaks at eight
        # different lags would be noise fitted eight times, not a signal.
        cc2, lags2 = xcorr(detrend(shape[:, 4], 20.0), detrend(rshape[:, 4], 20.0),
                           maxsec)
        print(f"  b4 shape: best r {cc2.max():+.3f} at lag "
              f"{lags2[int(np.argmax(cc2))]:+.1f} s")
        cors, peaks = [], []
        for b in range(NB):
            c3, l3 = xcorr(detrend(shape[:, b], 20.0), detrend(rshape[:, b], 20.0),
                           max(4.0, min(maxsec, 60.0)))
            j = np.argmax(np.abs(l3 - best) <= 2.0)
            cors.append(c3[j])
            peaks.append(l3[int(np.argmax(c3))])
        print("  shape r at the envelope's offset:")
        print("    band: " + " ".join(f"{b:+6.2f}" for b in cors))
        print("    its own best lag within +-60 s: " +
              " ".join(f"{v:+6.1f}" for v in peaks))
        good = sum(1 for c in cors if c > 0.3)
        v = ("MEASURES the file" if good >= 4 else
             "partial: a few bands track" if good else
             "does NOT track the file")
        print(f"  -> {v}: {good}/8 bands above r=0.3 at one common offset")


if __name__ == "__main__":
    main()
