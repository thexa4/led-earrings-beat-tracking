#!/usr/bin/env python3
"""Score device runs against the ideal (host-replayed) trace, in music time.

Usage: idealmatch.py IDEAL.CSV RUN1.csv [RUN2.csv ...]

The ideal CSV comes from tools/hosttrace (pipeline replay of the exact WAV
ffplay played); device CSVs from tools/telem.py. Each run is aligned to the
ideal frame axis by a linear map dev_seq = a*music_frame + b: coarse search
maximizing correlation of the AGC'd `total` (same quantized signal on both
sides), refined with the 30-bit hash agreement. Alignment therefore anchors
on content, not on wall-clock guesses (ffplay start delay varies per spawn).

Reports per-run vs ideal (heard-quality), pairwise run-vs-run agreement,
and a per-second profile of pattern/hash agreement to expose which musical
parts the device is worse at.
"""
import csv
import json
import os
import sys
import wave

import numpy as np

FPS = 64.0
NA = 0xFF


def wav_envelope(path, win=250):
    with wave.open(path, "rb") as w:
        x = np.frombuffer(w.readframes(w.getnframes()),
                          dtype="<i2").astype(np.float64)
    n = len(x) // win
    return np.log1p(np.sqrt((x[:n * win].reshape(n, win) ** 2).mean(axis=1)))


def popcount(a):
    return np.unpackbits(a.astype(np.uint32).view(np.uint8).reshape(len(a), 4),
                         axis=1).sum(axis=1)


def load(path, key="seq"):
    with open(path) as f:
        rows = [r for r in csv.DictReader(f) if r.get("kind", "T") == "T"]
    mic = None
    if rows and "kind" in rows[0] and rows[0]["kind"] == "T":
        with open(path) as f:
            m = [r for r in csv.DictReader(f) if r.get("kind") == "M"]
        if m:
            mic = dict(t=np.array([float(r["t"]) for r in m]),
                       seq=np.array([int(r["seq"]) for r in m]),
                       rms=np.log1p(np.array([float(r["rms"]) for r in m])))
    def col(k, cast=int):
        return np.array([cast(r[k]) for r in rows])
    b = lambda k: np.array([r[k] == "True" for r in rows])
    seq = col("frame") if "frame" in rows[0] else col("seq")
    return dict(
        seq=seq,
        tot=col("total").astype(np.int64),
        pat=col("pattern").astype(float),
        hash=col("hash", lambda h: int(h, 16) & 0x3FFFFFFF).astype(np.uint32),
        locked=b("locked").astype(float), beat=b("beat").astype(float),
        onset=b("onset").astype(float), silent=b("silent").astype(float),
        anchor=b("anchor").astype(float),
        phase=col("phase").astype(float),
        votes=col("votes").astype(float),
        mic=mic,
    )


WAV_ENV = None     # log-rms envelope of the segment WAV, 64 fps (set in main)
KICK_PEAKS = None  # reference kick times (music frames) from the WAV
IDEAL_B = None     # ideal beat flags per music frame (set in main)


def ideal_beats():
    return IDEAL_B > 0


def align_mic(run, ml):
    """Locate music frame 0 from the silence prefix of the capture WAV
    (clk*.wav = 2 s of silence, then music): the mic timeline shows a clean
    floor, then a sustained jump at playback start (spawn delay included).
    The jump edge is caught within one ~66 ms poll; the caller refines to
    the frame with a hash scan in a +-10-frame window. Returns (a, b, info)
    with a=1 (HSI16 drift over 60 s is under the +-1 event tolerance)."""
    t, ss, rs = run["mic"]["t"], run["mic"]["seq"], run["mic"]["rms"]
    if len(rs) < 50:
        return None
    # rms is stored as log1p; work the threshold in linear counts.
    floor = float(np.expm1(np.median(rs[: int(1.0 / max(np.median(np.diff(t)), 1e-3))])))
    thr = np.log1p(max(6 * floor, floor + 15))
    loud = rs > thr
    i0 = None
    for i in range(len(t) - 3):
        if loud[i:i + 3].all():
            i0 = i
            break
    if i0 is None or np.median(rs[i0:]) < thr:
        return None  # audio never audible
    t_start = float(t[i0])
    b = float(np.interp(t_start, t, ss))
    return 1.0, b, dict(t_start=t_start, floor=floor,
                        music_med=float(np.expm1(np.median(rs[i0:i0 + 60]))))


def z(x):
    return (x - x.mean()) / (x.std() + 1e-9)


def hash_grid(run, m, a, bs):
    """Vectorized over b: fraction of music frames whose mapped device frame
    exists and matches the ideal hash within Hamming 2."""
    S = np.round(a * m[:, None] + bs[None, :])                     # (M, B)
    pos = np.clip(np.searchsorted(run["seq"], S.ravel()).reshape(S.shape),
                  0, len(run["seq"]) - 1)
    ok = run["seq"][pos] == S
    h = np.zeros(S.shape, np.uint8)
    if ok.any():
        exp = np.broadcast_to(IDEAL_H[m][:, None], S.shape)[ok]
        ham = popcount(run["hash"][pos][ok] ^ exp)
        h[ok] = (ham <= 2).astype(np.uint8)
    n = ok.sum(axis=0)
    return h.sum(axis=0) / np.maximum(n, 1), n / len(m)


def align_all(runs, ml):
    """Per-run offset b (device seq of music frame 0), a=1 fixed: over 60 s the
    HSI16-vs-playback drift is <=2 frames, well inside the +-1 tolerance used
    for event metrics. Score = exact-hash-match fraction vs the ideal trace."""
    m = np.arange(ml)
    exp = IDEAL_H[m]
    bs, scores = [], []
    for run in runs:
        seq, h = run["seq"], run["hash"]
        best = (-1.0, 0.0)
        for lo in range(-4000, 12000, 2000):          # chunked b grid
            grid = run["seq"][0] + np.arange(lo, lo + 2000)
            S = m[:, None] + grid[None, :]            # (M, B) target seqs
            pos = np.clip(np.searchsorted(seq, S.ravel()).reshape(S.shape),
                          0, len(seq) - 1)
            ok = seq[pos] == S
            eq = ok & (h[pos] == exp[:, None])
            n = ok.sum(axis=0)
            sc = np.where(n >= 0.7 * len(m), eq.sum(axis=0) / np.maximum(n, 1), -1)
            j = int(np.argmax(sc))
            if sc[j] > best[0]:
                best = (float(sc[j]), float(grid[j]))
        bs.append(best[1])
        scores.append(best[0])
    return float(np.mean(scores)), 1.0, bs, scores


def jaccard(a, b, tol=1):
    a, b = np.sort(a), np.sort(b)
    if len(a) + len(b) == 0:
        return 1.0
    used = np.zeros(len(b), bool)
    inter = 0
    for x in a:
        if not len(b):
            break
        d = np.abs(b - x).astype(float)
        d[used] = 1e18
        j = int(np.argmin(d))
        if d[j] <= tol:
            used[j] = True
            inter += 1
    return inter / (len(a) + len(b) - inter)


def to_music(run, a, b, ml):
    m = np.arange(ml)
    s = np.round(a * m + b)
    pos = np.clip(np.searchsorted(run["seq"], s), 0, len(run["seq"]) - 1)
    hit = run["seq"][pos] == s
    out = {}
    for k in ("pat", "hash", "locked", "beat", "onset", "phase", "tot"):
        v = run[k][pos].astype(float)
        v[~hit] = np.nan
        out[k] = v
    return out


def align_triplet(run, ml):
    """Click-triplet alignment: the capture WAV starts with clicks at 0.5 /
    0.7 / 1.6 s and music at 2.0 s, so music frame 0 sits exactly 96 frames
    after the first click. Device onset detection adds a few frames of
    filter latency -> refine with an exact-hash scan over +-10 frames, which
    is unambiguous once the coarse position is this tight. Returns (a,b,info)
    or None when no triplet is present."""
    on = run["seq"][run["onset"] > 0]
    s0 = run["seq"][0]
    early = on[on < s0 + 4 * 64]
    for i, c1 in enumerate(early):
        c2 = early[(early > c1 + 8) & (early < c1 + 18)]
        c3 = early[(early > c1 + 50) & (early < c1 + 66)]
        if len(c2) == 0 or len(c3) == 0:
            continue
        b0 = c1 + 96
        grid = np.arange(b0 - 8, b0 + 19)
        S = np.arange(ml)[:, None] + grid[None, :]
        pos = np.clip(np.searchsorted(run["seq"], S.ravel()).reshape(S.shape),
                      0, len(run["seq"]) - 1)
        ok = run["seq"][pos] == S
        eq = ok & (run["hash"][pos] == IDEAL_H[:, None])
        n = ok.sum(axis=0)
        sc = np.where(n >= 0.5 * ml, eq.sum(axis=0) / np.maximum(n, 1), -1)
        j = int(np.argmax(sc))
        if sc[j] < 0:
            continue
        order = np.argsort(sc)[::-1]
        return 1.0, float(grid[j]), dict(
            clicks=(int(c1), int(c2[0]), int(c3[0])), lat=float(grid[j] - b0),
            match=float(sc[j]), next=float(sc[order[1]]))
    return None


def main():
    global IDEAL_H, IDEAL_T, IDEAL_B
    global WAV_ENV, KICK_PEAKS
    ideal_path, run_paths = sys.argv[1], sys.argv[2:]
    ideal = load(ideal_path)
    ml = len(ideal["seq"])
    IDEAL_H, IDEAL_T, IDEAL_B = ideal["hash"], ideal["tot"], ideal["beat"]
    wav_path = os.environ.get("SEG_WAV")
    if wav_path:
        WAV_ENV = wav_envelope(wav_path)
        from runmatch import kick_peaks
        KICK_PEAKS = kick_peaks(wav_path)
    print(f"ideal: {ml} frames ({ml/FPS:.0f} s)")

    runs = [load(p) for p in run_paths]
    if os.environ.get("CLICK"):
        aligns = []
        for p, run in zip(run_paths, runs):
            r0 = align_triplet(run, ml)
            if r0 is None:
                print(f"WARNING: {p}: no click triplet found")
                raise SystemExit(1)
            aligns.append(r0)
        print("alignment: click triplet (music frame 0 = click1 + 96), "
              "hash-refined:")
        for p, (a, b, info) in zip(run_paths, aligns):
            print(f"  {os.path.basename(p):16s} clicks@{info['clicks']} "
                  f"lat={info['lat']:+.0f}fr exact={100*info['match']:.0f}% "
                  f"(next {100*info['next']:.0f}%)")
    elif all(r["mic"] is not None for r in runs):
        aligns = []
        for run in runs:
            r0 = align_mic(run, ml)
            if r0 is None:
                print(f"WARNING: {run}: audio never audible at the mic")
                raise SystemExit(1)
            _, b0, info = r0
            # The mic edge pins b to ~+-4 frames (one poll). Refine with the
            # BEAT trains: beats are ~2/s and frame-precise, while the hash
            # is too degenerate on speech to pick a unique offset.
            info["b0"] = b0
            ib = np.nonzero(ideal_beats())[0]
            db = run["seq"][run["beat"] > 0]
            grid = np.arange(b0 - 10, b0 + 11)
            scores = []
            for g in grid:
                used = np.zeros(len(db), bool)
                hit = 0
                for x in ib + g:
                    d = np.abs(db - x).astype(float)
                    d[used] = 1e18
                    j = int(np.argmin(d))
                    if d[j] <= 2:
                        used[j] = True
                        hit += 1
                scores.append(hit / max(len(ib), 1))
            sc = np.array(scores)
            j = int(np.argmax(sc))
            order = np.argsort(sc)[::-1]
            info["refine"] = (float(sc[j]), float(grid[j]),
                              float(sc[order[1]]))
            aligns.append((1.0, float(grid[j]), info))
        print("alignment: silence-prefix edge (mic) then beat-train refine:")
        for p, (a, b, info) in zip(run_paths, aligns):
            print(f"  {os.path.basename(p):16s} audio start={info['t_start']:5.2f}s "
                  f"floor={info['floor']:.0f} music~{info['music_med']:.0f} "
                  f"beat-hit {100*info['refine'][0]:.0f}% at "
                  f"{info['refine'][1]-info['b0']:+.0f} fr (next "
                  f"{100*info['refine'][2]:.0f}%)")
        if any(info["refine"][0] - info["refine"][2] < 0.05 for _a, _b, info in aligns):
            print("  WARNING: hash-refine peak weak vs runner-up; frame "
                  "alignment uncertain for those runs")
    else:
        score, a0, bs, per = align_all(runs, ml)
        print(f"joint hash alignment: a={a0:.5f}, mean hash-match "
              f"{100*score:.1f}% (no mic rows: absolute time unreliable)")
        aligns = [(a0, b, {}) for b in bs]
    aligned = []
    for p, run, (a, b, _info) in zip(run_paths, runs, aligns):
        mm = to_music(run, a, b, ml)
        aligned.append(mm)
        ok = ~np.isnan(mm["hash"])
        ex = float(np.mean(mm["hash"][ok] == IDEAL_H[:ml][ok]))
        hm = float(popcount((mm["hash"][ok].astype(np.uint32)) ^ IDEAL_H[:ml][ok]).mean())
        pat = float(np.mean(mm["pat"][ok] == ideal["pat"][:ml][ok]))
        bj = jaccard(np.nonzero(~np.isnan(mm["beat"]) & (mm["beat"] > 0))[0],
                     np.nonzero(ideal["beat"][:ml] > 0)[0])
        lk = np.nonzero(~np.isnan(mm["locked"]) & (mm["locked"] > 0))[0]
        print(f"{os.path.basename(p):16s} b={b:.0f} | "
              f"vs ideal: hash exact {100*ex:.0f}% ham {hm:.2f}, "
              f"pattern {100*pat:.0f}%, beat-Jac {100*bj:.0f}%, locked from "
              f"{lk[0]/FPS if len(lk) else -1:.1f}s")

    n = len(aligned)
    if n > 1:
        print("\npairwise run agreement (common frames):")
        for i in range(n):
            for j in range(i + 1, n):
                both = ~np.isnan(aligned[i]["hash"]) & ~np.isnan(aligned[j]["hash"])
                if both.sum() < 64:
                    continue
                hi = aligned[i]["hash"][both].astype(np.uint32)
                hj = aligned[j]["hash"][both].astype(np.uint32)
                ex = float(np.mean(hi == hj))
                hm = float(popcount(hi ^ hj).mean())
                pat = float(np.mean(aligned[i]["pat"][both] == aligned[j]["pat"][both]))
                bi = np.nonzero(~np.isnan(aligned[i]["beat"]) & (aligned[i]["beat"] > 0))[0]
                bj = np.nonzero(~np.isnan(aligned[j]["beat"]) & (aligned[j]["beat"] > 0))[0]
                jc = jaccard(bi, bj)
                print(f"  r{i+1}-r{j+1}: hash {100*ex:5.1f}% ham {hm:.2f}, "
                      f"pattern {100*pat:5.1f}%, beat-Jac {100*jc:5.1f}%")

        # per-second profile: pattern + hash agreement (all runs) vs ideal
        common = np.ones(ml, bool)
        for mm in aligned:
            common &= ~np.isnan(mm["hash"])
        P = np.stack([mm["pat"] for mm in aligned])
        H = np.stack([mm["hash"] for mm in aligned])
        allpat = np.all(P == P[0:1], axis=0) & common
        allhash = np.all(H == H[0:1], axis=0) & common
        vsid = (H[0] == ideal["hash"][:ml]) & common
        prof = []
        for sec in range(int(ml // FPS)):
            sel = common[np.arange(ml) >= 0]
            sl = slice(sec * int(FPS), (sec + 1) * int(FPS))
            nsec = common[sl].sum()
            if nsec < 48:
                continue
            prof.append((sec,
                         100 * allpat[sl].sum() / nsec,
                         100 * allhash[sl].sum() / nsec,
                         100 * vsid[sl].sum() / nsec,
                         float(np.nanmean([mm["tot"][sl] for mm in aligned]))))
        print("\nper music second: run-agreement(pattern) / run-agreement(hash) / vs-ideal(hash) %")
        for i in range(0, len(prof), 10):
            print("  " + "  ".join(f"{s:>2}s:{p:3.0f}/{h:3.0f}/{v:3.0f}"
                                   for s, p, h, v, e in prof[i:i+10]))
        worst = sorted(prof, key=lambda x: x[1] + x[2])[:6]
        print("worst seconds (p/h): " + "  ".join(
            f"{s}s={p:.0f}/{h:.0f}" for s, p, h, v, e in worst))
        out = dict(prof=[[s, p, h, v, e] for s, p, h, v, e in prof],
                   runs=[os.path.basename(p) for p in run_paths])
        base = os.path.basename(ideal_path).replace("ideal", "score").replace(".csv", ".json")
        with open("/tmp/psync/" + base, "w") as f:
            json.dump(out, f)


if __name__ == "__main__":
    main()
