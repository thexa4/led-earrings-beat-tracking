#!/usr/bin/env python3
"""Compare repeated device runs of the same music segment.

Usage: runmatch.py SEG.WAV RUN1.csv RUN2.csv RUN3.csv

The device samples from its own HSI16 oscillator (+/-1%), so device frames
and music frames (250 samples @ 16 kHz = 1/64 s) drift up to ~1% apart:
~40 frames over 60 s. Each run is aligned to the music timeline by a linear
map device_seq = a * music_frame + b, grid-searched to maximize correlation
between the device frame-energy telemetry and the WAV's frame envelope.
Agreement across runs is then scored in music time (pattern, hash, lock,
onsets, beats, PLL phase), with a per-second profile that exposes which
musical parts agree worst.
"""
import csv
import sys
import wave

import numpy as np

WIN, FS = 250, 16000
FPS = FS / WIN  # music frames per second (64)


def popcount(a):
    return np.unpackbits(a.astype(np.uint32).view(np.uint8).reshape(len(a), 4),
                         axis=1).sum(axis=1)


def wav_envelope(path):
    with wave.open(path, "rb") as w:
        assert w.getnchannels() == 1 and w.getframerate() == FS
        x = np.frombuffer(w.readframes(w.getnframes()), dtype="<i2").astype(np.float64)
    n = len(x) // WIN
    rms = np.sqrt((x[:n * WIN].reshape(n, WIN) ** 2).mean(axis=1))
    return np.log2(1.0 + rms)


def load_run(path):
    cols = {k: [] for k in ("seq", "t", "total", "pattern", "hash", "locked",
                            "beat", "onset", "phase", "silent")}
    with open(path) as f:
        for r in csv.DictReader(f):
            for k in cols:
                cols[k].append(r[k])
    seq = np.array(cols["seq"], int)
    tot = np.array(cols["total"], np.int64)
    # total is stored in 16 bits; unwrap boundary crossings so the envelope
    # is continuous for correlation
    d = np.diff(tot, prepend=tot[0])
    tot -= np.cumsum(d > 32768) * 65536 + np.cumsum(d < -32768) * 65536
    return dict(
        seq=seq, t=np.array(cols["t"], float), env=tot.astype(np.float64),
        pat=np.array(cols["pattern"], float),
        hash=np.array([int(h, 16) & 0x3FFFFFFF for h in cols["hash"]], np.uint32),
        locked=np.array([v == "True" for v in cols["locked"]], float),
        beat=np.array([v == "True" for v in cols["beat"]], float),
        onset=np.array([v == "True" for v in cols["onset"]], float),
        phase=np.array(cols["phase"], float),
        silent=np.array([v == "True" for v in cols["silent"]], float))


def kick_peaks(path, thr=0.3, dist=8):
    """Reference kick times (music frames) from the WAV: LP 200 Hz envelope
    flux, peak-picked. Same method family as the ground-truth BPM script."""
    from scipy.signal import butter, sosfilt, find_peaks
    with wave.open(path, "rb") as w:
        x = np.frombuffer(w.readframes(w.getnframes()), dtype="<i2").astype(np.float64)
    sos = butter(4, 200 / (FS / 2), "lowpass", output="sos")
    x = sosfilt(sos, x)
    n = len(x) // WIN
    env = np.abs(x[:n * WIN].reshape(n, WIN)).mean(axis=1)
    flux = np.maximum(np.diff(env, prepend=env[0]), 0.0)
    flux = np.convolve(flux, np.ones(3) / 3, mode="same")
    fmax = np.quantile(flux, 0.98)
    if fmax <= 0:
        fmax = 1.0
    pk, _ = find_peaks(flux / fmax, height=thr, distance=dist)
    return pk.astype(np.float64)


def fit_alignment(runs, peaks):
    """Joint fit: device_seq = a*music_frame + b_run. All runs share one
    device boot counter AND the host playback clock, so a is common; b_run is
    the device seq of music frame 0 (ffplay start, ~1.2 s into each capture).
    Score = fraction of a run's onsets landing within +-2 music frames of a
    reference kick peak. Returns (a, [(b, score), ...])."""
    if len(peaks) < 20:
        raise SystemExit("too few reference kicks - segment too quiet?")
    onseqs = [run["seq"][run["onset"] > 0] for run in runs]
    fps_dev = (runs[0]["seq"][-1] - runs[0]["seq"][0]) / max(
        runs[0]["t"][-1] - runs[0]["t"][0], 1e-9)
    ml = float(peaks[-1] + 1)

    def best_b(onseq, a, bs):
        mf = (onseq[:, None] - bs[None, :]) / a          # (n_on, n_b)
        inr = (mf >= 0) & (mf < ml)
        idx = np.clip(np.searchsorted(peaks, mf), 0, len(peaks) - 1)
        d1 = np.abs(peaks[idx] - mf)
        d0 = np.abs(peaks[np.clip(idx - 1, 0, None)] - mf)
        dist = np.minimum(d1, d0)
        hit = (dist <= 2.0) & inr
        cov = inr.mean(axis=0)
        score = np.where(cov >= 0.7, hit.sum(axis=0) / np.maximum(inr.sum(axis=0), 1), -1)
        j = int(np.argmax(score))
        return float(score[j]), float(bs[j])

    def fit_a(a):
        tot, bs = 0.0, []
        for run, onseq in zip(runs, onseqs):
            b0 = run["seq"][0] + fps_dev * 1.2
            bs_grid = b0 + np.arange(-90, 91)
            s, b = best_b(onseq, a, bs_grid)
            if s < 0:
                return -1.0, None
            tot += s
            bs.append(b)
        return tot / len(runs), bs

    best = (-1.0, 1.0, None)
    for a in np.arange(0.980, 1.0201, 0.0002):
        s, bs = fit_a(a)
        if s > best[0]:
            best = (s, float(a), bs)
    for a in np.arange(best[1] - 0.0002, best[1] + 0.00021, 0.00005):
        s, bs = fit_a(a)
        if s > best[0]:
            best = (s, float(a), bs)
    score, a, bs = best
    if bs is None or score < 0.5:
        print(f"WARNING: weak joint fit (score {score:.2f}) - results may be misaligned")
    return a, bs, score


def to_music(run, a, b, music_len):
    """Map run onto the music grid; NaN where there is no usable device frame."""
    m = np.arange(music_len)
    s = np.round(a * m + b).astype(np.int64)
    pos = np.clip(np.searchsorted(run["seq"], s), 0, len(run["seq"]) - 1)
    hit = run["seq"][pos] == s
    usable = hit & (run["silent"][pos] == 0)
    out = {}
    for k in ("pat", "hash", "locked", "beat", "onset", "phase"):
        v = run[k][pos].astype(float)
        v[~usable] = np.nan
        out[k] = v
    return out


def jaccard(setA, setB, tol=1):
    a, b = np.array(sorted(setA)), np.array(sorted(setB))
    if len(a) + len(b) == 0:
        return 1.0
    used = np.zeros(len(b), bool)
    inter = 0
    for x in a:
        if len(b) == 0:
            break
        d = np.abs(b - x).astype(float)
        d[used] = 1e18
        j = int(np.argmin(d))
        if d[j] <= tol:
            used[j] = True
            inter += 1
    return inter / (len(a) + len(b) - inter)


def metrics(aligned, music_len, energy):
    n = len(aligned)
    common = np.ones(music_len, bool)
    for mm in aligned:
        common &= ~np.isnan(mm["pat"])
    frames = np.nonzero(common)[0]
    if len(frames) < 64:
        print("not enough comparable frames")
        return
    P = np.stack([mm["pat"][frames] for mm in aligned])
    H = np.stack([mm["hash"][frames] for mm in aligned])
    L = np.stack([mm["locked"][frames] for mm in aligned])
    B = np.stack([mm["beat"][frames] for mm in aligned])
    O = np.stack([mm["onset"][frames] for mm in aligned])
    PH = np.stack([mm["phase"][frames] for mm in aligned])

    allpat = np.all(P == P[0:1], axis=0)
    Hu = H.astype(np.uint32)  # frames were filtered to non-NaN before stacking
    allhash = np.all(Hu == Hu[0:1], axis=0)
    ham = np.mean([popcount(Hu[i] ^ Hu[j]).mean()
                   for i in range(n) for j in range(i + 1, n)])
    print(f"\nover {len(frames)} comparable music frames "
          f"({len(frames)/FPS:.0f}s):")
    print(f"pattern agree (all runs):  {100*allpat.mean():5.1f}%")
    print(f"hash exact   (all runs):   {100*allhash.mean():5.1f}%   "
          f"pairwise Hamming {ham:.2f}/30 bits")
    print(f"lock state agree (all):    {100*np.all(L == L[0:1], axis=0).mean():5.1f}%   "
          f"(all locked {100*L.all(axis=0).mean():.0f}%, "
          f"none {100*(L == 0).all(axis=0).mean():.0f}%)")
    for name, arr in (("onset", O), ("beat", B)):
        js = [jaccard(frames[arr[i] > 0], frames[arr[j] > 0])
              for i in range(n) for j in range(i + 1, n)]
        print(f"{name} agree (+-1 frame):   {100*np.mean(js):5.1f}%   "
              f"counts {[int((arr[i] > 0).sum()) for i in range(n)]}")
    both = (L > 0).all(axis=0)
    if both.sum() > 32:
        dph = []
        for i in range(n):
            for j in range(i + 1, n):
                d = np.abs(PH[i][both] - PH[j][both])
                dph.append(np.minimum(d, 128 - d).mean())
        print(f"PLL phase spread (locked): {np.mean(dph):5.1f}/127  "
              f"({np.mean(dph)/FPS*1000:.0f} ms)")

    prof = []
    for sec in range(int(np.ceil(music_len / FPS))):
        sel = (frames >= sec * FPS) & (frames < (sec + 1) * FPS)
        if sel.sum() < 32:
            continue
        prof.append((sec, 100 * allpat[sel].mean(),
                     energy[int(sec * FPS):int((sec + 1) * FPS)].mean()))
    print("\npattern agreement % per music second (energy in log2 rms):")
    for i in range(0, len(prof), 10):
        print("  " + "  ".join(f"{s:>2}s:{p:3.0f}" for s, p, e in prof[i:i + 10]))
    worst = sorted(prof, key=lambda x: x[1])[:6]
    print("worst seconds: " + "  ".join(f"{s}s={p:.0f}% (e={e:.1f})"
                                        for s, p, e in worst))
    pcts = np.array([p for _, p, _ in prof])
    en = np.array([e for _, _, e in prof])
    if len(prof) > 10 and pcts.std() > 0 and en.std() > 0:
        print(f"corr(agreement, musical energy) = {np.corrcoef(pcts, en)[0,1]:+.2f}")


def main():
    wav, runpaths = sys.argv[1], sys.argv[2:]
    peaks = kick_peaks(wav)
    with wave.open(wav, "rb") as w:
        raw = np.frombuffer(w.readframes(w.getnframes()),
                            dtype="<i2").astype(np.float64)
    music_len = len(raw) // WIN
    energy = np.log2(1 + np.sqrt(
        (raw[:music_len * WIN].reshape(music_len, WIN) ** 2).mean(axis=1)))
    print(f"segment {wav}: {music_len/FPS:.0f} s, {len(peaks)} reference kicks")
    runs = [load_run(p) for p in runpaths]
    a, bs, joint = fit_alignment(runs, peaks)
    print(f"joint fit: a={a:.5f} (device clock {100*(a-1):+.2f}% vs playback), "
          f"score={joint:.3f}")
    aligned = []
    for i, run in enumerate(runs):
        mm = to_music(run, a, bs[i], music_len)
        aligned.append(mm)
        lk = np.nonzero(~np.isnan(mm["locked"]) & (mm["locked"] > 0))[0]
        on = ~np.isnan(mm["onset"]) & (mm["onset"] > 0)
        locktxt = f"lock after {lk[0]/FPS:.1f}s" if len(lk) else "NEVER locked"
        # onset-vs-kick precision at this alignment
        mf = (run["seq"][run["onset"] > 0] - bs[i]) / a
        idx = np.clip(np.searchsorted(peaks, mf), 0, len(peaks) - 1)
        d = np.minimum(np.abs(peaks[idx] - mf),
                       np.abs(peaks[np.clip(idx - 1, 0, None)] - mf))
        hit = float(np.mean(d[np.where(mf < music_len)] <= 2.0)) if len(mf) else 0
        print(f"run {i+1}: b={bs[i]:.0f} onsets={int(on.sum())} "
              f"kick-hit={100*hit:.0f}% {locktxt} "
              f"coverage {100*np.mean(~np.isnan(mm['pat'])):.0f}%")
    metrics(aligned, music_len, energy)


if __name__ == "__main__":
    main()
