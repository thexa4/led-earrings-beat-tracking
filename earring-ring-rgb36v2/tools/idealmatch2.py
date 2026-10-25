#!/usr/bin/env python3
"""Score three device runs of one sample against each other and the ideal.

	idealmatch2.py IDEAL.CSV RUN1.csv RUN2.csv RUN3.csv [--wav SEG.WAV]

Alignment: each run's capture starts with 2 s of silence, so the mic tap
(capture2.py's kind=M rows) shows a floor then a jump: that puts playback
start within one poll (~4 frames). The offset is then refined on the ±12
frame window with three independent metrics, printed side by side so the
reader can see whether they agree:

  onset  - fraction of ideal onsets with a device onset within 1 frame
  total  - correlation of the AGC frame-loudness traces
  hash   - exact 24-bit hash agreement

The onset metric is used (it is the sharpest on real music; the hash is
degenerate on speech). Everything after alignment is reported in music
frames, with a per-second profile so the weak musical parts are visible.
"""
import csv
import json
import os
import sys

import numpy as np

FPS = 64.0
TOL = 1          # event matching tolerance, frames


def load(path):
    with open(path) as f:
        all_rows = list(csv.DictReader(f))
    rows = [r for r in all_rows if r.get("kind", "T") == "T"]
    mic = None
    m = [r for r in all_rows if r.get("kind") == "M"]
    if m:
        mic = dict(t=np.array([float(r["t"]) for r in m]),
                   seq=np.array([int(r["seq"]) for r in m]),
                   rms=np.log1p(np.array([float(r["rms"]) for r in m])))

    def col(k, cast=int):
        return np.array([cast(r[k]) for r in rows])
    def b(k):
        return np.array([r[k] == "True" for r in rows])
    seq = col("frame") if "frame" in rows[0] else col("seq")
    return dict(
        seq=seq,
        tot=col("total").astype(np.float64),
        pat=col("pattern").astype(float),
        hash=col("hash", lambda h: int(h, 16) & 0x3FFFFFFF).astype(np.uint32),
        locked=b("locked").astype(float), beat=b("beat").astype(float),
        onset=b("onset").astype(float), silent=b("silent").astype(float),
        anchor=b("anchor").astype(float), pchange=b("pchange").astype(float),
        votes=col("votes").astype(float), period=col("period").astype(float),
        mic=mic)


def popcount(a):
    return np.unpackbits(a.astype(np.uint32).view(np.uint8).reshape(len(a), 4),
                         axis=1).sum(axis=1)


def edge_offset(run):
    """Device seq at playback start, from the silence->music edge in the mic
    tap. Returns (b, t_start, floor, music_rms) or None."""
    t, ss, rs = run["mic"]["t"], run["mic"]["seq"], run["mic"]["rms"]
    if len(rs) < 50:
        return None
    dt = max(np.median(np.diff(t)), 1e-3)
    floor = float(np.expm1(np.median(rs[: int(1.0 / dt)])))
    thr = np.log1p(max(6 * floor, floor + 15))
    loud = rs > thr
    for i in range(len(t) - 3):
        if loud[i:i + 3].all():
            if np.median(rs[i:]) < thr:
                return None
            return (float(np.interp(t[i], t, ss)), float(t[i]), floor,
                    float(np.expm1(np.median(rs[i:i + 60]))))
    return None


def sample_on(run, s, keys):
    """Values at device seq s (nan where the trace has a gap)."""
    pos = np.clip(np.searchsorted(run["seq"], s), 0, len(run["seq"]) - 1)
    hit = run["seq"][pos] == s
    out = {}
    for k in keys:
        v = run[k][pos].astype(float)
        v[~hit] = np.nan
        out[k] = v
    return out


def event_hits(a_times, b_times, tol=TOL):
    """Greedy one-to-one match count of two sorted event lists."""
    a, bb = np.sort(a_times), np.sort(b_times)
    if len(a) == 0 or len(bb) == 0:
        return 0
    used = np.zeros(len(bb), bool)
    n = 0
    for x in a:
        d = np.abs(bb - x).astype(float)
        d[used] = 1e18
        j = int(np.argmin(d))
        if d[j] <= tol:
            used[j] = True
            n += 1
    return n


def jaccard(a, b, tol=TOL):
    inter = event_hits(a, b, tol)
    den = len(a) + len(b) - inter
    return inter / den if den else 1.0


def refine(run, ideal, b0, ml, halfwin=12):
    """Score each candidate offset by onset hits, total correlation and hash
    agreement. Returns dict metric -> (best_offset, best, runner_up)."""
    m = np.arange(ml)
    on_i = np.nonzero(ideal["onset"] > 0)[0]
    grids = b0 + np.arange(-halfwin, halfwin + 1)
    ion = np.zeros(len(grids))
    itot = np.zeros(len(grids))
    ihash = np.zeros(len(grids))
    for k, g in enumerate(grids):
        s = sample_on(run, np.round(m + g), ("onset", "tot", "hash"))
        ok = ~np.isnan(s["hash"])
        od = np.nonzero(ok & (s["onset"] > 0))[0]
        ion[k] = event_hits(on_i, od) / max(len(on_i), 1)
        ot = s["tot"][ok]
        it = ideal["tot"][:ml][ok]
        if len(ot) > 100 and ot.std() > 0 and it.std() > 0:
            itot[k] = float(np.corrcoef(ot, it)[0, 1])
        ihash[k] = float(np.mean(s["hash"][ok] == ideal["hash"][:ml][ok]))
    def best(arr):
        j = int(np.argmax(arr))
        o = np.argsort(arr)[::-1]
        return float(grids[j]), float(arr[j]), float(arr[o[1]])
    return dict(onset=best(ion), total=best(itot), hash=best(ihash),
                grid=grids, ion=ion, itot=itot, ihash=ihash)


def align_map(run, b, ml):
    m = np.arange(ml)
    return sample_on(run, np.round(m + b),
                     ("pat", "hash", "locked", "beat", "onset", "tot",
                      "votes", "period"))


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    wav = None
    for a in sys.argv[1:]:
        if a.startswith("--wav"):
            wav = a.split("=", 1)[1] if "=" in a else None
    ideal_path, run_paths = args[0], args[1:]
    ideal = load(ideal_path)
    ml = len(ideal["seq"])
    runs = [load(p) for p in run_paths]
    print(f"ideal: {ml} frames ({ml/FPS:.0f} s), "
          f"{int((ideal['onset'] > 0).sum())} onsets, "
          f"{int((ideal['beat'] > 0).sum())} beats, "
          f"period {np.median(ideal['period'][ideal['period'] > 0]):.0f}")

    aligns = []
    print("\nalignment (mic silence edge, then refine on +-12 frames):")
    for p, run in zip(run_paths, runs):
        e = edge_offset(run)
        if e is None:
            print(f"  {os.path.basename(p)}: audio edge never seen at the mic")
            raise SystemExit(1)
        b0, t_start, floor, mus = e
        r = refine(run, ideal, b0, ml)
        bo, ho, no = r["onset"]
        bt, ht, nt = r["total"]
        bh, hh, nh = r["hash"]
        agree = "ok" if abs(bo - bt) <= 2 and abs(bo - bh) <= 2 else "MIXED"
        print(f"  {os.path.basename(p):14s} edge={t_start:5.2f}s "
              f"floor={floor:.0f} music={mus:.0f} | onset {100*ho:3.0f}%@{bo-b0:+3.0f} "
              f"(next {100*no:3.0f}) total {100*ht:3.0f}%@{bt-b0:+3.0f} "
              f"hash {100*hh:3.0f}%@{bh-b0:+3.0f} -> {agree}")
        aligns.append((float(bo), r))

    aligned = [align_map(run, b, ml) for run, (b, _r) in zip(runs, aligns)]

    print("\nper run vs ideal:")
    for p, mm in zip(run_paths, aligned):
        ok = ~np.isnan(mm["hash"])
        ex = float(np.mean(mm["hash"][ok] == ideal["hash"][:ml][ok]))
        hm = float(popcount(mm["hash"][ok].astype(np.uint32)
                            ^ ideal["hash"][:ml][ok]).mean())
        pat = float(np.mean(mm["pat"][ok] == ideal["pat"][:ml][ok]))
        bj = jaccard(np.nonzero(~np.isnan(mm["beat"]) & (mm["beat"] > 0))[0],
                     np.nonzero(ideal["beat"][:ml] > 0)[0])
        oj = jaccard(np.nonzero(~np.isnan(mm["onset"]) & (mm["onset"] > 0))[0],
                     np.nonzero(ideal["onset"][:ml] > 0)[0])
        lk = ~np.isnan(mm["locked"]) & (mm["locked"] > 0)
        per = mm["period"][lk & ~np.isnan(mm["period"])]
        print(f"  {os.path.basename(p):14s} hash {100*ex:5.1f}% (ham {hm:.2f}) "
              f"pattern {100*pat:5.1f}% beat-J {100*bj:5.1f}% onset-J {100*oj:5.1f}% "
              f"locked {100*lk.mean():5.1f}% "
              f"period {np.median(per) if len(per) else 0:.0f}"
              f" (ideal {np.median(ideal['period'][ideal['period']>0]):.0f})")

    print("\npairwise run agreement:")
    pw = []
    n = len(aligned)
    for i in range(n):
        for j in range(i + 1, n):
            both = ~np.isnan(aligned[i]["hash"]) & ~np.isnan(aligned[j]["hash"])
            hi = aligned[i]["hash"][both].astype(np.uint32)
            hj = aligned[j]["hash"][both].astype(np.uint32)
            ex = float(np.mean(hi == hj))
            hm = float(popcount(hi ^ hj).mean())
            pat = float(np.mean(aligned[i]["pat"][both] == aligned[j]["pat"][both]))
            bj = jaccard(np.nonzero(~np.isnan(aligned[i]["beat"]) & (aligned[i]["beat"] > 0))[0],
                         np.nonzero(~np.isnan(aligned[j]["beat"]) & (aligned[j]["beat"] > 0))[0])
            oj = jaccard(np.nonzero(~np.isnan(aligned[i]["onset"]) & (aligned[i]["onset"] > 0))[0],
                         np.nonzero(~np.isnan(aligned[j]["onset"]) & (aligned[j]["onset"] > 0))[0])
            print(f"  r{i+1}-r{j+1}: hash {100*ex:5.1f}% (ham {hm:.2f}) "
                  f"pattern {100*pat:5.1f}% beat-J {100*bj:5.1f}% onset-J {100*oj:5.1f}%")
            pw.append(dict(pair=f"r{i+1}-r{j+1}", hash=ex, ham=hm, pattern=pat,
                           beat=bj, onset=oj))

    # per-second profile in music time
    common = np.ones(ml, bool)
    for mm in aligned:
        common &= ~np.isnan(mm["hash"])
    P = np.stack([mm["pat"] for mm in aligned])
    H = np.stack([mm["hash"] for mm in aligned])
    allpat = np.all(P == P[0:1], axis=0) & common
    allhash = np.all(H == H[0:1], axis=0) & common
    vsid = (H[0] == ideal["hash"][:ml]) & common
    vsidp = (P[0] == ideal["pat"][:ml]) & common
    # music features per second from the ideal trace
    loud = np.array([np.nanmean(mm["tot"][int(s*FPS):int((s+1)*FPS)])
                     for s in range(int(ml // FPS))])
    print("\nper music second (s: run-agree pat/hash | vs-ideal hash/pat | ideal total):")
    prof = []
    for sec in range(int(ml // FPS)):
        sl = slice(int(sec * FPS), int((sec + 1) * FPS))
        ns = common[sl].sum()
        if ns < 48:
            continue
        prof.append(dict(sec=sec,
                         pat=float(100 * allpat[sl].sum() / ns),
                         hash=float(100 * allhash[sl].sum() / ns),
                         vs_hash=float(100 * vsid[sl].sum() / ns),
                         vs_pat=float(100 * vsidp[sl].sum() / ns),
                         loud=float(loud[sec])))
    for i in range(0, len(prof), 10):
        print("  " + " ".join(f"{d['sec']:2d}s:{d['pat']:3.0f}/{d['hash']:3.0f}"
                              f"|{d['vs_hash']:3.0f}/{d['vs_pat']:3.0f}"
                              f"|{d['loud']/1000:4.1f}k" for d in prof[i:i + 10]))
    worst = sorted(prof, key=lambda d: d["pat"] + d["hash"])[:8]
    print("worst seconds (run agreement): " +
          ", ".join(f"{d['sec']}s {d['pat']:.0f}/{d['hash']:.0f}" for d in worst))

    out = dict(ideal=ideal_path, runs=run_paths,
               align_offsets=[b for b, _ in aligns], pairwise=pw, profile=prof)
    dst = os.environ.get("OUT_JSON")
    if dst:
        with open(dst, "w") as f:
            json.dump(out, f, indent=1)


if __name__ == "__main__":
    main()
