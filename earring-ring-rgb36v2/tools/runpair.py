#!/usr/bin/env python3
"""Score how much two or more *device* captures of the same audio agree.

Each run is a telemetry CSV from tools/telem.py (see tools/capture2.py). Runs
start at different instants, so the first job is finding the offset. The offset
is chosen by exact hash agreement rather than by loudness: the AGC makes the
level identical by construction, so only the content can identify "the same
moment".

Metrics, all over frames where both runs are locked:

  hash=    exact 29-bit hash equality - the section descriptor both earrings
           would compare, so this is the sync-relevant number.
  ham      Hamming distance between hashes; ham95 is the 95th percentile and
           <=3 is the fraction inside DAccept, the tolerance the device itself
           uses when it asks "is this the same section?".
  pattern  equality of the animation pattern index, which is what the user
           actually sees.
  beat@0   F1 of the beat-boundary events: precision and recall against the
           other run's beats, at zero frame tolerance.
  beat+-2  the same with a +-2 frame tolerance. The tracker's PLL corrects phase
           by period>>2 per onset, so two devices locked to the same tempo still
           emit boundaries a couple of frames apart; demanding exact agreement
           measures the PLL, not the sync.
  per      the locked beat period of each run.

Usage: runpair.py a.csv b.csv [c.csv ...] [--lag N] [--profile]
"""
import sys

import numpy as np

sys.path.insert(0, __file__.rsplit("/", 1)[0])
import idealmatch2 as im

HAM = None


def load(path):
    d = {k: v for k, v in im.load(path).items() if isinstance(v, np.ndarray)}
    # idealmatch2 returns flags as floats when a column is partially empty; the
    # bitwise ops below need real bools.
    for k in ("locked", "beat", "onset", "anchor", "silent", "pchange", "pushed", "mark"):
        if k in d:
            d[k] = np.asarray(d[k]).astype(bool)
    d["hash"] = d["hash"].astype(np.uint32)
    return d


def hamming(a, b):
    x = (a ^ b).astype(np.uint32)
    x = x & 0x1FFFFFFF
    return np.unpackbits(x.view(np.uint8).reshape(-1, 4), axis=1, bitorder="little").sum(axis=1)


def _win(na, nb, d):
    """Overlap window for b shifted by d frames."""
    return max(0, d), min(nb, na + d)


def align(a, b, lag):
    """Best integer offset (frames of b to shift) and the score achieved."""
    na, nb = len(a["seq"]), len(b["seq"])
    best = (0, -1, 0)
    for d in range(-lag, lag + 1):
        s, e = _win(na, nb, d)
        n = e - s
        if n < 500:
            continue
        m = a["locked"][s - d:e - d] & b["locked"][s:e]
        if m.sum() < 300:
            continue
        same = float(np.mean(a["hash"][s - d:e - d][m] == b["hash"][s:e][m]))
        if same > best[1]:
            best = (d, same, n)
    return best


def compare(a, b, d):
    """Metrics for b shifted by d frames relative to a."""
    s, e = _win(len(a["seq"]), len(b["seq"]), d)
    A, B = slice(s - d, e - d), slice(s, e)
    m = a["locked"][A] & b["locked"][B]
    out = {"n": int(m.sum()), "period": (np.median(a["period"][A][m]), np.median(b["period"][B][m]))}
    if m.sum() < 50:
        return out
    h = hamming(a["hash"][A][m], b["hash"][B][m])
    out["hash"] = float(np.mean(h == 0))
    out["ham"] = float(h.mean())
    out["ham95"] = float(np.percentile(h, 95))
    out["ham_le3"] = float(np.mean(h <= 3))
    out["pat"] = float(np.mean(a["pat"][A][m] == b["pat"][B][m]))
    for tol, name in ((0, "beat0"), (2, "beat2")):
        out[name] = beat_tol(a["beat"][A][m], b["beat"][B][m], tol)
    return out


def beat_tol(x, y, tol):
    """F1 between two boolean beat trains, hits allowed within +-tol frames."""
    ix, iy = np.nonzero(x)[0], np.nonzero(y)[0]
    if len(ix) == 0 or len(iy) == 0:
        return 0.0
    hit = 0
    for k in ix:
        if np.any(np.abs(iy - k) <= tol):
            hit += 1
    prec = hit / len(ix)
    hit2 = 0
    for k in iy:
        if np.any(np.abs(ix - k) <= tol):
            hit2 += 1
    rec = hit2 / len(iy)
    if prec + rec == 0:
        return 0.0
    return 2 * prec * rec / (prec + rec)


def shift(r, d, n):
    """Run r re-indexed onto a's frame grid, truncated to n frames."""
    out = {}
    for k, v in r.items():
        w = np.zeros(n, dtype=v.dtype)
        if d >= 0:
            m = min(len(v) - d, n)
            if m > 0:
                w[:m] = v[d:d + m]
        else:
            m = min(len(v), n + d)
            if m > 0:
                w[-d:-d + m] = v[:m]
        out[k] = w
    return out


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    lag = 400
    for a in sys.argv[1:]:
        if a.startswith("--lag"):
            lag = int(a.split("=")[1])
    profile = "--profile" in sys.argv
    runs = [load(p) for p in args]
    for p, r in zip(args, runs):
        print(f"{p}: {len(r['seq'])} frames, lock {r['locked'].mean()*100:.0f}%, "
              f"period {int(np.median(r['period'][r['locked']]) if r['locked'].any() else 0)}, "
              f"onsets {int(r['onset'].sum())}, anchors {int(r['anchor'].sum())}, "
              f"{len(set(r['hash'][r['locked']].tolist()))} hashes")
    print()
    print("pairwise (offset chosen by exact-hash agreement):")
    print(f"{'pair':<28} {'lag':>5} {'n':>6} {'hash=':>7} {'ham':>6} {'ham95':>6} {'<=3':>7} "
          f"{'pattern':>8} {'beat@0':>7} {'beat+-2':>8} {'per':>7}")
    # Offsets are always relative to run 0; the pairwise numbers below use the
    # offset between the two runs in the pair, which is a difference of those.
    offs = {0: 0}
    for j in range(1, len(runs)):
        offs[j], _, _ = align(runs[0], runs[j], lag)
    for i in range(len(runs)):
        for j in range(i + 1, len(runs)):
            d = offs[j] - offs[i]
            c = compare(runs[i], runs[j], d)
            print(f"{args[i].split('/')[-1] + '~' + args[j].split('/')[-1]:<28} {d:>5} {c['n']:>6} "
                  f"{c.get('hash', 0)*100:6.1f}% {c.get('ham', 0):6.2f} {c.get('ham95', 0):6.0f} "
                  f"{c.get('ham_le3', 0)*100:6.1f}% {c.get('pat', 0)*100:7.1f}% "
                  f"{c.get('beat0', 0)*100:6.1f}% {c.get('beat2', 0)*100:7.1f}% "
                  f"{int(c['period'][0])}/{int(c['period'][1])}")
    if not profile or len(runs) < 2:
        return
    # Everything onto run 0's grid so the per-second table below compares the
    # same instant across runs.
    n = min(len(r["seq"]) - (0 if i == 0 else offs.get(i, 0)) for i, r in enumerate(runs))
    offs.setdefault(0, 0)
    S = [shift(r, offs[i], n) for i, r in enumerate(runs)]
    valid = np.ones(n, bool)
    for s in S:
        valid &= s["seq"] != 0
    print("\nper-second (all pairs averaged): hash= / ham<=3 / pattern / beat+-2 / onsets-per-sec")
    for b0 in range(0, n, 64):
        sl = slice(b0, min(b0 + 64, n))
        v = valid[sl]
        if v.sum() < 32:
            continue
        h = ham_ = pat = 0.0
        npair = 0
        for i in range(len(S)):
            for j in range(i + 1, len(S)):
                hh = hamming(S[i]["hash"][sl][v], S[j]["hash"][sl][v])
                h += float((hh == 0).mean())
                ham_ += float((hh <= 3).mean())
                pat += float((S[i]["pat"][sl][v] == S[j]["pat"][sl][v]).mean())
                npair += 1
        h /= npair
        ham_ /= npair
        pat /= npair
        f = 0.0
        for i in range(len(S)):
            for j in range(len(S)):
                if i == j:
                    continue
                ix = np.nonzero(S[i]["beat"][sl])[0]
                iy = np.nonzero(S[j]["beat"][sl])[0]
                if len(ix) == 0:
                    continue
                f += sum(1 for k in ix if len(iy) and np.min(np.abs(iy - k)) <= 2) / len(ix)
        f /= len(S) * (len(S) - 1)
        ons = np.mean([S[i]["onset"][sl][v].sum() for i in range(len(S))])
        bar = "#" * int(round(h * 20))
        print(f"  {b0/64:5.1f}s {h*100:6.1f}% {ham_*100:6.1f}% {pat*100:7.1f}% {f*100:7.1f}% "
              f"on={ons:4.1f} {bar}")


if __name__ == "__main__":
    main()
