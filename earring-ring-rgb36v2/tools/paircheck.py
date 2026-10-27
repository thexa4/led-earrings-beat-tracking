#!/usr/bin/env python3
"""Is the bench delivering the same sound to both earrings? Check before tuning.

Pattern agreement is a pairwise number, and a pairwise number at chance has two
causes with the same symptom: both devices hear the music and measure it
differently, which is a selector problem, or the two are not hearing the same
thing, which no selector can fix. This session spent a long time tuning against
the second case, so this tool exists to make that impossible to do again.

It answers three questions, cheapest first, and each one invalidates the ones
below it if it fails:

  1. level     Do the two loudness traces move together? `total` is the frame
               loudness the pipeline itself computes. Two microphones in one
               room with one speaker in it must correlate here - not because
               the algorithm is good, but because sound pressure is the same
               physical event at both places. Near zero means the room, the
               placement, or the capture is the problem and nothing else in
               this file is worth reading.
  2. shape     Do the eight smoothed bands move together once level is removed?
               This is what a colour selector has to work with, so it is the
               number that bounds any selector, including a perfect one.
  3. truth     Does each device track the FILE that was played, scored on its
               own? The one measurement here that does not need the other
               device at all, so it can tell a deaf earring from a
               disagreeing pair. Uses filealign's split of the played file.

Alignment is by the host clock and searched over a small lag range, printed
rather than assumed: a 100 ms pairing error is invisible in a 2 s envelope but
not in a 200 ms onset, and a peak that only exists at an absurd lag is not a
peak. The slow trend removed before correlating is a quarter of the capture,
never a fixed 20 s - on a 170 s run a 20 s window throws away most of the
signal and reports a real bench as a dead one.

  python3 paircheck.py DIR [--file PLAYED.wav] [--maxlag 4]
"""
import argparse
import csv
import os
import sys

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import audiotruth  # noqa: E402  ref_bands, read_wav_pcm16, FS, FRAME
import filealign  # noqa: E402  xcorr, resample, detrend

NB = 8
GRID = 10.0
# xcorr converts FFT bin indices to seconds with this constant, and the played
# file is resampled on the same grid as the device, so both modules must agree.
filealign.GRID = GRID


def read(path, kind=None):
    """Rows from a capture file whose header may be split over two lines."""
    if not os.path.exists(path):
        return None
    with open(path) as fh:
        rd = csv.reader(fh)
        names = next(rd)
        nxt = next(rd)
        if nxt and not nxt[0]:
            names = [a if a else b for a, b in zip(names, nxt)]
            body = list(rd)
        else:
            body = [nxt] + list(rd)
    out = {}
    for i, n in enumerate(names):
        out[n] = [r[i] if len(r) > i else "" for r in body
                  if r and (kind is None or r[0] == kind)]
    return out


def nums(col, bad=("", "-1", "255", "-")):
    out = []
    for x in col:
        try:
            out.append(float(x))
        except (TypeError, ValueError):
            out.append(np.nan)
    return np.array(out)


def series(t, x):
    """Drop rows with no value, sort, and resample onto a uniform GRID."""
    if x.ndim > 1:
        m = np.isfinite(t) & np.all(np.isfinite(x), axis=1)
    else:
        m = np.isfinite(t) & np.isfinite(x)
    t, x = t[m], x[m]
    o = np.argsort(t, kind="stable")
    t, x = t[o], x[o]
    if len(t) < 20:
        return None, None
    g = np.arange(t[0], t[-1], 1.0 / GRID)
    if x.ndim == 1:
        return g, np.interp(g, t, x)
    y = np.empty((len(g), x.shape[1]))
    for b in range(x.shape[1]):
        y[:, b] = np.interp(g, t, x[:, b])
    return g, y


def r_at(a, ga, b, gb, lag):
    """Correlation of a with b resampled lagged seconds behind a's grid."""
    x = np.interp(ga, gb, b, left=np.nan, right=np.nan) if lag == 0 else \
        np.interp(ga - lag, gb, b, left=np.nan, right=np.nan)
    m = np.isfinite(x) & np.isfinite(a)
    if np.count_nonzero(m) < 50:
        return float("nan")
    return float(np.corrcoef(a[m], x[m])[0, 1])


def best_lag(a, ga, b, gb, maxlag):
    cc, lags = filealign.xcorr(a, b, maxlag)
    k = int(np.argmax(cc))
    return float(cc[k]), float(lags[k])


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("dir")
    ap.add_argument("--file", help="the wav/mp3 that was played, for the "
                                   "per-device ground truth")
    ap.add_argument("--maxlag", type=float, default=4.0)
    ap.add_argument("--bands", default="b", help="column prefix: b or snap")
    a = ap.parse_args()

    D = {}
    for s in "LR":
        ev = read(os.path.join(a.dir, f"{s}.csv"), kind="T")
        st = read(os.path.join(a.dir, f"state_{s}.csv"))
        if ev is None or st is None:
            sys.exit(f"{a.dir}: missing {s}.csv or state_{s}.csv")
        B = np.stack([nums(st[f"{a.bands}{i}"]) for i in range(NB)], axis=1)
        D[s] = dict(t=nums(st["t"]), B=B,
                    te=nums(ev["t"]), tot=nums(ev["total"]),
                    pat=nums(ev.get("pattern", [])),
                    lock=np.array([v.strip() == "True" for v in ev["locked"]]))
    dur = min(D["L"]["t"][-1] - D["L"]["t"][0],
              D["R"]["t"][-1] - D["R"]["t"][0])
    det = min(20.0, dur / 4.0)
    print(f"{a.dir}: {dur:.0f} s paired, trend removed over {det:.0f} s "
          f"(quarter of the capture)")

    for s in "LR":
        d = D[s]
        print(f"  {s}: {len(d['t'])} state rows, {len(d['te'])} event rows, "
              f"loudness p10 {np.nanpercentile(d['tot'], 10):.0f} "
              f"p50 {np.nanpercentile(d['tot'], 50):.0f} "
              f"p90 {np.nanpercentile(d['tot'], 90):.0f} "
              f"sd {np.nanstd(d['tot']):.0f}, "
              f"locked {np.mean(d['lock'])*100:.0f}%")

    # 1. loudness
    gl, vl = series(D["L"]["te"], D["L"]["tot"])
    gr, vr = series(D["R"]["te"], D["R"]["tot"])
    if gl is not None and gr is not None:
        cl, lag = best_lag(filealign.detrend(vl, det), gl,
                           filealign.detrend(vr, det), gr, a.maxlag)
        print(f"\n  1. LOUDNESS (pipeline `total`, both devices, trend removed)")
        print(f"     r at lag 0    {r_at(filealign.detrend(vl, det), gl,
                                         filealign.detrend(vr, det), gr, 0):+.3f}")
        print(f"     best r        {cl:+.3f} at lag {lag:+.2f} s")
        # `total` is the pipeline's loudness, which is measured after the AGC
        # has already flattened level, so a modest r here is expected and is NOT
        # evidence of a dead room. It is printed as context, and the verdict is
        # left to the band shapes below.
        print(f"     (context only: the AGC normalises level away, so this is "
              f"not the test of whether the room is shared)")
        loud_r = cl

    # 2. per-band and shape, at the lag the loudness found
    print(f"\n  2. BAND ENVELOPES (p.smooth, trend removed, per band)")
    bl = {}
    br = {}
    for s, box in (("L", bl), ("R", br)):
        g, y = series(D[s]["t"], D[s]["B"])
        box["g"], box["y"] = g, filealign.detrend(y, det)
    if bl["g"] is not None and br["g"] is not None:
        # one common lag for all bands, from the sum: picking a lag per band
        # would let eight noise traces each contribute their own best case
        sl = bl["y"].sum(axis=1)
        sr = br["y"].sum(axis=1)
        _, lag = best_lag(sl, bl["g"], sr, br["g"], a.maxlag)
        rs = [r_at(bl["y"][:, b], bl["g"], br["y"][:, b], br["g"], lag)
              for b in range(NB)]
        print(f"     common lag {lag:+.2f} s:  " +
              "  ".join(f"b{b} {rs[b]:+.2f}" for b in range(NB)))
        # shape = each band minus the mean of all eight: level removed per frame
        sh_l = bl["y"] - bl["y"].mean(axis=1, keepdims=True)
        sh_r = br["y"] - br["y"].mean(axis=1, keepdims=True)
        cs = [r_at(sh_l[:, b], bl["g"], sh_r[:, b], br["g"], lag)
              for b in range(NB)]
        print(f"     level removed:      " +
              "  ".join(f"b{b} {cs[b]:+.2f}" for b in range(NB)))
        ml = bl["y"].mean(axis=1)
        mr = br["y"].mean(axis=1)
        rr = r_at(ml, bl["g"], mr, br["g"], lag)
        print(f"     mean of bands (level) {rr:+.2f}")
        top = max(cs)
        print(f"     best band r {top:+.2f}: "
              f"{'a selector has something to work with' if top > 0.4 else 'NOTHING to select on - the input, not the selector, is the limit'}")
        if loud_r < 0.15 and top < 0.3:
            print("     both near zero: the two are not hearing one speaker. "
                  "Check placement, that neither unit is damped or asleep, and "
                  "that the player actually ran, before believing anything else.")

    # 3. against the played file
    if a.file:
        x = audiotruth.read_wav_pcm16(a.file)
        rb = audiotruth.ref_bands(x)
        rt = (np.arange(len(rb)) + 0.5) * audiotruth.FRAME / audiotruth.FS
        rg, rbg = filealign.resample(rt, rb)
        rshape = rbg - rbg.mean(axis=1, keepdims=True)
        print(f"\n  3. AGAINST THE FILE ({os.path.basename(a.file)}, "
              f"{len(x)/audiotruth.FS:.0f} s)")
        for s in "LR":
            g, y = series(D[s]["t"], D[s]["B"])
            if g is None:
                continue
            sh = filealign.detrend(y - y.mean(axis=1, keepdims=True), det)
            ref = filealign.detrend(rshape, det)
            # band 3 picks the offset; per-band offsets would be eight
            # independent chances to find a spurious one
            cc, lags = filealign.xcorr(sh[:, 3], ref[:, 3], a.maxlag)
            k = int(np.argmax(cc))
            lag = float(lags[k])
            rs = []
            for b in range(NB):
                c2, l2 = filealign.xcorr(sh[:, b], ref[:, b], a.maxlag)
                j = int(np.argmin(np.abs(l2 - lag)))
                rs.append(float(c2[j]))
            print(f"     {s}: shape r at its own best lag {lag:+.2f} s: " +
                  "  ".join(f"b{b} {rs[b]:+.2f}" for b in range(NB)))
            print(f"        mean {np.mean(rs):+.2f}, best band "
                  f"{max(rs):+.2f}  "
                  f"{'tracks the file' if max(rs) > 0.35 else 'does not track the file'}")
    print("""
  reading a weak result: loudness near zero means the two are not hearing one
  speaker - check that both are in the room, that neither is muted or damped,
  and that the player actually ran. Shape near zero while loudness is fine is
  the interesting case: the room is shared and the DSP is not extracting the
  same thing from it, which is where a selector can still help.""")


if __name__ == "__main__":
    main()
