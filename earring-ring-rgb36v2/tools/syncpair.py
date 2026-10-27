#!/usr/bin/env python3
"""Score a tools/syncpoll.py capture: health, lock, and L/R agreement on the
shared host clock (never by frame counter, BENCH.md §3).

  tools/syncpair.py DIR [--skip 20]

The last line is where the left earring's beats fall in the right one's beat
grid, 8 bins, bin 0 = together: a pile at bin 4 is one earring on the kick
and the other on the off-beat, bins 2/6 usually a 3/2 or 3/4 period on one
side, and a flat histogram two grids drifting through each other.

Beat times: each poll gives t_mid - beatAge/frame_rate, which is the end of the
frame the beat fired in plus the unknown time since that frame ended (< 1
frame); the earliest estimate per beat count is kept.
"""
import argparse

import numpy as np


def load(path):
    lines = open(path).read().split("\n")
    head = lines[0].split(",")
    rows = [l.split(",") for l in lines[1:] if l.count(",") == len(head) - 1]
    a = np.array(rows, dtype=float)
    return {k: a[:, i] for i, k in enumerate(head)}


def beats(d, rate):
    t = (d["t0"] + d["t1"]) / 2 - d["beatage"] / rate
    ok = d["beatage"] < 255
    out = {}
    for b, tb in zip(d["beats"][ok], t[ok]):
        out[b] = min(out.get(b, 1e18), tb)
    bs = np.array(sorted(out))
    return np.array([out[b] for b in bs])


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("dir")
    ap.add_argument("--skip", type=float, default=20, help="seconds of warm-up excluded")
    a = ap.parse_args()
    D = {s: load(f"{a.dir}/{s}.csv") for s in "LR"}
    t_start = max(D[s]["t0"][0] for s in "LR") + a.skip
    t_end = min(D[s]["t1"][-1] for s in "LR")
    info = {}
    for s, d in D.items():
        tm = (d["t0"] + d["t1"]) / 2
        lat = (d["t1"] - d["t0"]) * 1e3
        fr = d["frame"]
        resets = int(np.sum(np.diff(fr) < 0))
        rate = np.polyfit(tm, fr, 1)[0]
        w = (tm >= t_start) & (tm <= t_end)
        locked = (d["flags"][w].astype(int) & 2) > 0
        silent = (d["flags"][w].astype(int) & 4) > 0
        bms = d["beatms"][w][locked]
        bt = beats(d, rate)
        bt = bt[(bt >= t_start) & (bt <= t_end)]
        ibi = np.diff(bt) * 1e3
        info[s] = dict(tm=tm, d=d, w=w, bt=bt, rate=rate)
        print(f"{s}: {len(fr)} polls, read {np.median(lat):.1f} ms median / {np.percentile(lat, 99):.0f} p99, "
              f"frame rate {rate:.2f}/s, frame resets {resets}, magic ok {np.all(d['magic'] == 0x434e5953)}")
        print(f"   locked {locked.mean():.0%}  silent {silent.mean():.0%}  beats {len(bt)}  "
              f"beatMs median {np.median(bms) if len(bms) else 0:.0f} "
              f"(BPM {60000 / np.median(bms) if len(bms) else 0:.1f})  "
              f"IBI median {np.median(ibi):.0f} ms")
        vals, cnt = np.unique(bms, return_counts=True)
        top = sorted(zip(cnt, vals), reverse=True)[:4]
        print("   beatMs mix:", ", ".join(f"{v:.0f}ms {c / len(bms):.0%}" for c, v in top))
        print(f"   level mean {d['level'][w].mean():.0f}  bass {d['bass'][w].mean():.0f}  "
              f"mid {d['mid'][w].mean():.0f}  high {d['high'][w].mean():.0f}  "
              f"sections {int(d['sections'][w][-1] - d['sections'][w][0])}")

    # Same-moment comparisons: R's value at each L poll (nearest earlier R poll).
    L, R = info["L"], info["R"]
    w = L["w"]
    tl = L["tm"][w]
    j = np.clip(np.searchsorted(R["tm"], tl) - 1, 0, len(R["tm"]) - 1)
    dl, dr = L["d"], R["d"]
    pat = np.mean(dl["pattern"][w] == dr["pattern"][j])
    lk = ((dl["flags"][w].astype(int) & 2) > 0) & ((dr["flags"][j].astype(int) & 2) > 0)
    per_eq = np.mean(np.abs(dl["beatms"][w][lk] - dr["beatms"][j][lk]) <= 16) if lk.any() else 0
    print(f"\npair: pattern same {pat:.1%}, both locked {lk.mean():.0%}, "
          f"beat period within 1 frame {per_eq:.0%} of both-locked time")
    for k in ("level", "bass", "mid", "high"):
        c = np.corrcoef(dl[k][w], dr[k][j])[0, 1]
        print(f"   {k} corr {c:.2f}", end="")
    print()

    # Beat coincidence: each L beat against the nearest R beat.
    bl, br = L["bt"], R["bt"]
    def coinc(bl, br):
        k = np.clip(np.searchsorted(br, bl), 1, len(br) - 1)
        dt = np.minimum(np.abs(bl - br[k - 1]), np.abs(bl - br[k])) * 1e3
        return dt
    dt = coinc(bl, br)
    rng = np.random.default_rng(1)
    chance32 = np.mean([np.mean(coinc(bl, br + rng.uniform(0, 2)) < 32) for _ in range(20)])
    print(f"beats L {len(bl)} R {len(br)}: |dt| median {np.median(dt):.0f} ms, "
          f"<32 ms {np.mean(dt < 32):.0%} (chance {chance32:.0%}), <70 ms {np.mean(dt < 70):.0%}")
    # Per minute, so a settle problem shows apart from a steady one.
    edges = np.arange(t_start, t_end + 1, 60)
    print("   per minute <32 ms:", " ".join(
        f"{np.mean(dt[(bl >= e) & (bl < e + 60)] < 32):.0%}" for e in edges[:-1]
        if np.any((bl >= e) & (bl < e + 60))))
    k = np.clip(np.searchsorted(br, bl), 1, len(br) - 1)
    ph = (bl - br[k - 1]) / np.diff(br)[k - 1] % 1
    print("   L beats in the R grid (bin 0 = together):",
          np.histogram((ph * 8 + 0.5) % 8, bins=8, range=(0, 8))[0])


if __name__ == "__main__":
    main()
